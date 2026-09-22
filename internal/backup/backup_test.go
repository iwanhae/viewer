package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	listErr error
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

func (s *fakeStore) ListObjects(_ context.Context, prefix string) ([]storage.Object, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	objects := make([]storage.Object, 0)
	for key, obj := range s.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, storage.Object{
				Key:          key,
				LastModified: obj.lastModified,
				Size:         int64(len(obj.body)),
			})
		}
	}
	return objects, nil
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
			got, reason := shouldRestore(tc.backup, tc.localExists, tc.stamp, tc.stampSet)
			if got != tc.want {
				t.Errorf("shouldRestore=%v want %v", got, tc.want)
			}
			if reason == "" {
				t.Errorf("shouldRestore returned an empty reason")
			} else {
				t.Logf("reason: %s", reason)
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

// TestRestoreWithoutBackupObjectHandlesListingFailure pins that the forensic
// listing behind the missing-backup log line never turns "no backup" into an
// error or a restore: a listing outage must behave exactly like before.
func TestRestoreWithoutBackupObjectHandlesListingFailure(t *testing.T) {
	dir := t.TempDir()

	store := newFakeStore(map[string]fakeObject{})
	store.listErr = errors.New("list down")
	restored, err := Restore(context.Background(), store,
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

	finalizer := NewFinalizer(store, cat, dir, false)
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

	finalizer := NewFinalizer(store, cat, dir, false)
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

// seedGuardFixture opens a catalog holding one SUCCEEDED album whose zip is
// staged in the store — the minimum state a finalize would normally clean up.
func seedGuardFixture(t *testing.T, dir string) *catalog.Store {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(dir, "viewer.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { cat.Close() })
	if err := cat.CreateAlbum(context.Background(), catalog.Album{
		ID:               "album-ok",
		OriginalFilename: "ok.zip",
		Status:           catalog.AlbumStatusReady,
		SourceKey:        "uploads/album-ok.zip",
	}); err != nil {
		t.Fatalf("seed album: %v", err)
	}
	return cat
}

// TestFinalizerRefusesOverwriteWithoutStamp pins the guard's main case: a
// local database that never came from a backup (no stamp) must not replace
// the bucket's copy, because an unverified catalog over a good backup is how
// a missed restore becomes permanent metadata loss.
func TestFinalizerRefusesOverwriteWithoutStamp(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cat := seedGuardFixture(t, dir)
	store := newFakeStore(map[string]fakeObject{
		BackupObjectKey:        {body: []byte("an existing backup"), lastModified: backupModified},
		"uploads/album-ok.zip": {body: []byte("ok")},
	})

	finalizer := NewFinalizer(store, cat, dir, false)
	err := finalizer.Run(ctx)
	if err == nil {
		t.Fatalf("Finalizer.Run replaced the backup despite the missing stamp")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error should name the refusal, got %v", err)
	}
	if got := store.putCount(); got != 0 {
		t.Errorf("the refused run uploaded %d time(s), want 0", got)
	}
	if !store.has("uploads/album-ok.zip") {
		t.Errorf("zip deleted although the upload was refused")
	}
	obj, ok, err := store.StatObject(ctx, BackupObjectKey)
	if err != nil || !ok || obj.Size != int64(len("an existing backup")) {
		t.Errorf("existing backup changed: ok=%v size=%d err=%v", ok, obj.Size, err)
	}
	if _, stampSet, err := readStamp(StampPath(dir)); err != nil || stampSet {
		t.Errorf("stamp advanced despite the refusal: set=%v err=%v", stampSet, err)
	}
}

// TestFinalizerRefusesDrasticallySmallerSnapshot covers the guard's second
// condition: the stamp proves a backup lineage, but the fresh snapshot is a
// tiny fraction of the backup — the footprint of a local catalog that was
// never restored.
func TestFinalizerRefusesDrasticallySmallerSnapshot(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cat := seedGuardFixture(t, dir)
	const backupSize = 1 << 20
	store := newFakeStore(map[string]fakeObject{
		BackupObjectKey:        {body: bytes.Repeat([]byte("x"), backupSize), lastModified: backupModified},
		"uploads/album-ok.zip": {body: []byte("ok")},
	})
	// The stamp ties the local database to an older backup, so only the size
	// mismatch can refuse this run.
	if err := writeStamp(StampPath(dir), backupModified.Add(-time.Hour)); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}

	finalizer := NewFinalizer(store, cat, dir, false)
	err := finalizer.Run(ctx)
	if err == nil {
		t.Fatalf("Finalizer.Run replaced a large backup with a tiny snapshot")
	}
	if !strings.Contains(err.Error(), "drastically smaller") {
		t.Errorf("error should name the shrinkage, got %v", err)
	}
	if got := store.putCount(); got != 0 {
		t.Errorf("the refused run uploaded %d time(s), want 0", got)
	}
	if !store.has("uploads/album-ok.zip") {
		t.Errorf("zip deleted although the upload was refused")
	}
	obj, ok, err := store.StatObject(ctx, BackupObjectKey)
	if err != nil || !ok || obj.Size != backupSize {
		t.Errorf("existing backup changed: ok=%v size=%d err=%v", ok, obj.Size, err)
	}
}

// TestFinalizerAllowBackupOverwriteForcesTheUpload pins the opt-out: with the
// override set, the guarded run behaves exactly like an unguarded one.
func TestFinalizerAllowBackupOverwriteForcesTheUpload(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cat := seedGuardFixture(t, dir)
	store := newFakeStore(map[string]fakeObject{
		BackupObjectKey:        {body: bytes.Repeat([]byte("x"), 1<<20), lastModified: backupModified},
		"uploads/album-ok.zip": {body: []byte("ok")},
	})
	if err := writeStamp(StampPath(dir), backupModified.Add(-time.Hour)); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}

	finalizer := NewFinalizer(store, cat, dir, true)
	if err := finalizer.Run(ctx); err != nil {
		t.Fatalf("Finalizer.Run with the override set: %v", err)
	}
	if got := store.putCount(); got != 1 {
		t.Errorf("put count=%d want exactly 1", got)
	}
	obj, ok, err := store.StatObject(ctx, BackupObjectKey)
	if err != nil || !ok {
		t.Fatalf("backup after run: ok=%v err=%v", ok, err)
	}
	if obj.Size >= 1<<20 {
		t.Errorf("the backup was not replaced by the small snapshot (size=%d)", obj.Size)
	}
	if store.has("uploads/album-ok.zip") {
		t.Errorf("zip survived a completed finalize")
	}
	stamp, stampSet, err := readStamp(StampPath(dir))
	if err != nil || !stampSet || !stamp.Equal(obj.LastModified) {
		t.Errorf("stamp=%s set=%v err=%v, want the new backup's last-modified %s", stamp, stampSet, err, obj.LastModified)
	}
}

// TestRestoreRejectsNonSQLiteBackup pins the header check: a backup object
// that is not a SQLite database fails the restore instead of replacing the
// local catalog with garbage, and neither the catalog nor the stamp moves.
func TestRestoreRejectsNonSQLiteBackup(t *testing.T) {
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
		BackupObjectKey: {body: []byte("this is not a sqlite database at all"), lastModified: backupModified},
	})
	stampPath := filepath.Join(dir, "viewer.db.backup-stamp")
	if err := writeStamp(stampPath, backupModified.Add(-time.Hour)); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}

	restored, err := Restore(ctx, store, dbPath, stampPath)
	if err == nil {
		t.Fatalf("Restore replaced the catalog with a non-SQLite object")
	}
	if !strings.Contains(err.Error(), "SQLite") {
		t.Errorf("error should name the SQLite header, got %v", err)
	}
	if restored {
		t.Errorf("Restore reported success for a rejected backup")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("reread local catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("local database changed despite the failed restore")
	}
	stamp, stampSet, err := readStamp(stampPath)
	if err != nil || !stampSet || !stamp.Equal(backupModified.Add(-time.Hour)) {
		t.Fatalf("stamp moved despite the failed restore: set=%v stamp=%s err=%v", stampSet, stamp, err)
	}
}

// TestFinalizerSkipsUnchangedCatalog pins the unchanged-skip: a run with no
// catalog writes since the last fully successful backup must not re-upload,
// while any write re-enables the backup.
func TestFinalizerSkipsUnchangedCatalog(t *testing.T) {
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
	finalizer := NewFinalizer(store, cat, dir, false)

	if err := finalizer.Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if got := store.putCount(); got != 1 {
		t.Fatalf("first run put count=%d want 1", got)
	}

	// No writes since the first backup: the second run skips the upload.
	if err := finalizer.Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if got := store.putCount(); got != 1 {
		t.Fatalf("unchanged run re-uploaded (put count=%d)", got)
	}

	// Any write re-enables the backup, even though nothing about the terminal
	// albums changed.
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID:               "album-new",
		OriginalFilename: "new.zip",
		Status:           catalog.AlbumStatusQueued,
		SourceKey:        "uploads/album-new.zip",
	}); err != nil {
		t.Fatalf("seed new album: %v", err)
	}
	if err := finalizer.Run(ctx); err != nil {
		t.Fatalf("third Run: %v", err)
	}
	if got := store.putCount(); got != 2 {
		t.Fatalf("changed run put count=%d want 2", got)
	}
}

// TestSnapshotAlbumsFreezeTheDeleteList pins the race the snapshot-based
// delete list exists for: an album that turns terminal after the snapshot was
// taken is invisible to SnapshotAlbums, so its zip survives until a later
// finalize covers it — no concurrent writer can slip between the backup and
// the delete.
func TestSnapshotAlbumsFreezeTheDeleteList(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	cat, err := catalog.Open(filepath.Join(dir, "viewer.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	for _, album := range []catalog.Album{
		{ID: "album-done", OriginalFilename: "done.zip", Status: catalog.AlbumStatusReady, SourceKey: "uploads/done.zip"},
		{ID: "album-late", OriginalFilename: "late.zip", Status: catalog.AlbumStatusQueued, SourceKey: "uploads/late.zip"},
	} {
		if err := cat.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("seed album %s: %v", album.ID, err)
		}
	}

	snapshot := filepath.Join(dir, "snapshot.db")
	if err := cat.BackupTo(ctx, snapshot); err != nil {
		t.Fatalf("backup catalog: %v", err)
	}

	// The late album finishes after the snapshot was taken.
	if err := cat.SetAlbumStatus(ctx, "album-late", catalog.AlbumStatusFailed, "boom"); err != nil {
		t.Fatalf("flip album-late: %v", err)
	}

	albums, err := cat.SnapshotAlbums(ctx, snapshot)
	if err != nil {
		t.Fatalf("SnapshotAlbums: %v", err)
	}
	got := make(map[string]bool, len(albums))
	for _, album := range albums {
		got[album.ID] = true
	}
	if !got["album-done"] {
		t.Errorf("snapshot misses the terminal album album-done: %v", got)
	}
	if got["album-late"] {
		t.Errorf("snapshot includes a status flip that happened after it was taken")
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

	finalizer := NewFinalizer(store, cat, dir, false)

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
