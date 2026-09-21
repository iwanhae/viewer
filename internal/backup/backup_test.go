package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/storage"
)

var backupModified = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// fakeObject is one stored object: the bytes the backup round-trips and the
// last-modified timestamp the restore decision compares stamps against.
type fakeObject struct {
	body         []byte
	lastModified time.Time
}

// fakeStore is an in-memory object store with just enough control hooks for
// the ordering and concurrency tests.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string]fakeObject
	putErr  error
	statErr error
	puts    int

	// putStarted is closed the first time PutObject begins and putRelease
	// gates it, so a test can hold a run mid-flight.
	putStarted chan struct{}
	putRelease chan struct{}
	startOnce  sync.Once
}

func newFakeStore(objects map[string]fakeObject) *fakeStore {
	return &fakeStore{objects: objects}
}

func (s *fakeStore) PutObject(_ context.Context, key string, body io.Reader, _ string) error {
	if s.putStarted != nil {
		s.startOnce.Do(func() { close(s.putStarted) })
		<-s.putRelease
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	if s.putErr != nil {
		return s.putErr
	}
	s.objects[key] = fakeObject{body: data, lastModified: backupModified}
	return nil
}

func (s *fakeStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	s.mu.Lock()
	obj, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(obj.body)), "application/vnd.sqlite3", nil
}

func (s *fakeStore) StatObject(_ context.Context, key string) (storage.Object, bool, error) {
	if s.statErr != nil {
		return storage.Object{}, false, s.statErr
	}
	s.mu.Lock()
	obj, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		return storage.Object{}, false, nil
	}
	return storage.Object{Key: key, LastModified: obj.lastModified, Size: int64(len(obj.body))}, true, nil
}

func (s *fakeStore) DeleteObjects(_ context.Context, keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, key := range keys {
		delete(s.objects, key)
	}
	return nil
}

func (s *fakeStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

func (s *fakeStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

func TestShouldRestore(t *testing.T) {
	stamp := backupModified.Add(-time.Hour)
	later := backupModified.Add(time.Hour)

	cases := []struct {
		name        string
		backup      *storage.Object
		localExists bool
		stamp       time.Time
		stampSet    bool
		want        bool
	}{
		{
			name:   "no backup object never restores",
			backup: nil,
			want:   false,
		},
		{
			name:        "no local database restores",
			backup:      &storage.Object{LastModified: backupModified},
			localExists: false,
			want:        true,
		},
		{
			name:        "local database without a stamp is authoritative",
			backup:      &storage.Object{LastModified: backupModified},
			localExists: true,
			want:        false,
		},
		{
			name:        "backup newer than the stamp restores",
			backup:      &storage.Object{LastModified: backupModified},
			localExists: true,
			stamp:       stamp,
			stampSet:    true,
			want:        true,
		},
		{
			name:        "backup at the stamp does not restore",
			backup:      &storage.Object{LastModified: backupModified},
			localExists: true,
			stamp:       backupModified,
			stampSet:    true,
			want:        false,
		},
		{
			name:        "backup older than the stamp does not restore",
			backup:      &storage.Object{LastModified: backupModified},
			localExists: true,
			stamp:       later,
			stampSet:    true,
			want:        false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldRestore(tc.backup, tc.localExists, tc.stamp, tc.stampSet)
			if got != tc.want {
				t.Errorf("shouldRestore=%v want %v", got, tc.want)
			}
		})
	}
}

// TestRestoreReplacesLocalCatalogAndWritesStamp walks the real path: a snapshot
// of one catalog restores over a different local database, the stamp lands on
// the backup's last-modified, and a second restore is a no-op.
func TestRestoreReplacesLocalCatalogAndWritesStamp(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	snapshotPath := filepath.Join(dir, "snapshot-source.db")
	source, err := catalog.Open(snapshotPath)
	if err != nil {
		t.Fatalf("open source catalog: %v", err)
	}
	if err := source.CreateAlbum(ctx, catalog.Album{
		ID:               "album-a",
		OriginalFilename: "a.zip",
		Status:           catalog.AlbumStatusReady,
	}); err != nil {
		t.Fatalf("seed source catalog: %v", err)
	}
	backupPath := filepath.Join(dir, "backup.db")
	if err := source.BackupTo(ctx, backupPath); err != nil {
		t.Fatalf("backup source catalog: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close source catalog: %v", err)
	}
	snapshot, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	store := newFakeStore(map[string]fakeObject{
		BackupObjectKey: {body: snapshot, lastModified: backupModified},
	})

	// The local database holds different data and an older stamp.
	dbPath := filepath.Join(dir, "viewer.db")
	local, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open local catalog: %v", err)
	}
	if err := local.CreateAlbum(ctx, catalog.Album{
		ID:               "album-b",
		OriginalFilename: "b.zip",
		Status:           catalog.AlbumStatusReady,
	}); err != nil {
		t.Fatalf("seed local catalog: %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatalf("close local catalog: %v", err)
	}
	stampPath := filepath.Join(dir, "viewer.db.backup-stamp")
	if err := writeStamp(stampPath, backupModified.Add(-time.Hour)); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}

	restored, err := Restore(ctx, store, dbPath, stampPath)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !restored {
		t.Fatalf("Restore reported false, wanted a restore")
	}

	reopened, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen restored catalog: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.GetAlbum(ctx, "album-a"); err != nil {
		t.Fatalf("restored catalog misses album-a: %v", err)
	}
	if _, err := reopened.GetAlbum(ctx, "album-b"); !errors.Is(err, catalog.ErrAlbumNotFound) {
		t.Fatalf("restored catalog still has the replaced album-b: %v", err)
	}

	stamp, stampSet, err := readStamp(stampPath)
	if err != nil || !stampSet {
		t.Fatalf("read stamp after restore: set=%v err=%v", stampSet, err)
	}
	if !stamp.Equal(backupModified) {
		t.Errorf("stamp=%s want %s", stamp, backupModified)
	}

	// The stamp now matches the backup: a second restore is a no-op.
	again, err := Restore(ctx, store, dbPath, stampPath)
	if err != nil {
		t.Fatalf("second Restore: %v", err)
	}
	if again {
		t.Errorf("second Restore restored again, want a no-op")
	}
}

// TestRestoreLeavesLocalDatabaseWithoutStampBehind pins the upgrade guard: an
// existing deployment has no stamp, so its local data must survive untouched.
func TestRestoreLeavesLocalDatabaseWithoutStampBehind(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	dbPath := filepath.Join(dir, "viewer.db")
	local, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open local catalog: %v", err)
	}
	if err := local.CreateAlbum(ctx, catalog.Album{
		ID:               "album-local",
		OriginalFilename: "local.zip",
		Status:           catalog.AlbumStatusReady,
	}); err != nil {
		t.Fatalf("seed local catalog: %v", err)
	}
	if err := local.Close(); err != nil {
		t.Fatalf("close local catalog: %v", err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read local catalog: %v", err)
	}

	store := newFakeStore(map[string]fakeObject{
		BackupObjectKey: {body: []byte("backup-bytes"), lastModified: backupModified},
	})

	restored, err := Restore(ctx, store, dbPath, filepath.Join(dir, "viewer.db.backup-stamp"))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored {
		t.Fatalf("Restore replaced a stampless local database")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("reread local catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("local database changed despite no restore")
	}
}

func TestRestoreWithoutBackupObject(t *testing.T) {
	dir := t.TempDir()

	restored, err := Restore(context.Background(), newFakeStore(map[string]fakeObject{}),
		filepath.Join(dir, "viewer.db"), filepath.Join(dir, "viewer.db.backup-stamp"))
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored {
		t.Fatalf("Restore reported a restore with no backup object")
	}
}

// TestFinalizerUploadsThenDeletes walks the full finalize: the backup lands,
// the stamp records it, and exactly the terminal albums' zips go away.
func TestFinalizerUploadsThenDeletes(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cat, err := catalog.Open(filepath.Join(dir, "viewer.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	seed := []catalog.Album{
		{ID: "album-ok", OriginalFilename: "ok.zip", Status: catalog.AlbumStatusReady, SourceKey: "uploads/album-ok.zip"},
		{ID: "album-bad", OriginalFilename: "bad.zip", Status: catalog.AlbumStatusFailed, SourceKey: "uploads/album-bad.zip"},
		{ID: "album-new", OriginalFilename: "new.zip", Status: catalog.AlbumStatusQueued, SourceKey: "uploads/album-new.zip"},
	}
	for _, album := range seed {
		if err := cat.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("seed album %s: %v", album.ID, err)
		}
	}

	store := newFakeStore(map[string]fakeObject{
		"uploads/album-ok.zip":  {body: []byte("ok")},
		"uploads/album-bad.zip": {body: []byte("bad")},
		"uploads/album-new.zip": {body: []byte("new")},
	})

	finalizer := NewFinalizer(store, cat, dir)
	if err := finalizer.Run(ctx); err != nil {
		t.Fatalf("Finalizer.Run: %v", err)
	}

	obj, ok, err := store.StatObject(ctx, BackupObjectKey)
	if err != nil || !ok {
		t.Fatalf("backup object after run: ok=%v err=%v", ok, err)
	}
	if obj.Size == 0 {
		t.Fatalf("uploaded backup is empty")
	}
	stamp, stampSet, err := readStamp(StampPath(dir))
	if err != nil || !stampSet {
		t.Fatalf("stamp after run: set=%v err=%v", stampSet, err)
	}
	if !stamp.Equal(obj.LastModified) {
		t.Errorf("stamp=%s want the backup's last-modified %s", stamp, obj.LastModified)
	}

	if store.has("uploads/album-ok.zip") {
		t.Errorf("succeeded album's zip survived the finalize")
	}
	if store.has("uploads/album-bad.zip") {
		t.Errorf("failed album's zip survived the finalize")
	}
	if !store.has("uploads/album-new.zip") {
		t.Errorf("queued album's zip must stay until its own finalize")
	}
}

// TestFinalizerKeepsZipsWhenUploadFails pins the invariant: nothing is deleted
// unless the backup covering it is durable in the bucket.
func TestFinalizerKeepsZipsWhenUploadFails(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cat, err := catalog.Open(filepath.Join(dir, "viewer.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID:               "album-ok",
		OriginalFilename: "ok.zip",
		Status:           catalog.AlbumStatusReady,
		SourceKey:        "uploads/album-ok.zip",
	}); err != nil {
		t.Fatalf("seed album: %v", err)
	}

	store := newFakeStore(map[string]fakeObject{
		"uploads/album-ok.zip": {body: []byte("ok")},
	})
	store.putErr = errors.New("s3 down")

	finalizer := NewFinalizer(store, cat, dir)
	if err := finalizer.Run(ctx); err == nil {
		t.Fatalf("Finalizer.Run returned nil despite the failed upload")
	}

	if !store.has("uploads/album-ok.zip") {
		t.Fatalf("zip deleted although the backup never landed")
	}
	if _, stampSet, err := readStamp(StampPath(dir)); err != nil || stampSet {
		t.Fatalf("stamp advanced despite the failed upload: set=%v err=%v", stampSet, err)
	}
}

// TestFinalizerSkipsOverlappingRun covers the startup sweep racing a queue
// drain: the second run must return at once instead of interleaving steps.
func TestFinalizerSkipsOverlappingRun(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cat, err := catalog.Open(filepath.Join(dir, "viewer.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()

	store := newFakeStore(map[string]fakeObject{})
	store.putStarted = make(chan struct{})
	store.putRelease = make(chan struct{})

	finalizer := NewFinalizer(store, cat, dir)

	done := make(chan error, 1)
	go func() { done <- finalizer.Run(ctx) }()
	<-store.putStarted // the first run is inside its upload

	if err := finalizer.Run(ctx); err != nil {
		t.Fatalf("overlapping Run returned %v, want an immediate nil", err)
	}
	if got := store.putCount(); got != 0 {
		t.Fatalf("overlapping Run started its own upload (put count %d)", got)
	}

	close(store.putRelease)
	if err := <-done; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := store.putCount(); got != 1 {
		t.Fatalf("put count=%d want exactly 1", got)
	}
}
