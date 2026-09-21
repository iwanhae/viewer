// Package backup keeps a snapshot of the SQLite catalog in the bucket and
// deletes the staged zips that snapshot covers.
//
// The safety invariant: no staged zip is deleted before a backup that already
// records its album's terminal state is durable in S3. Status SUCCEEDED is
// only ever written by the pipeline's single worker, and the finalizer runs on
// that same worker when the queue drains, so nothing can change between the
// snapshot and the delete list. On startup Restore brings the bucket's
// snapshot back when it is newer than what the local database has seen, so a
// wiped state volume recovers from the bucket alone.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/storage"
)

// BackupObjectKey is the logical key the catalog snapshot is stored under.
// Like every key that crosses internal/storage, the deployment's key prefix is
// applied at the storage boundary, not here.
const BackupObjectKey = "backups/viewer.db"

// stampFilename marks which backup the local database already reflects. It is
// the restore decision's watermark: the database file's own mtime cannot be
// used, because in WAL mode commits can land in the sidecar without touching
// the main file's timestamp.
const stampFilename = "viewer.db.backup-stamp"

const backupContentType = "application/vnd.sqlite3"

// StampPath is the marker file recording which catalog backup the local
// database already reflects. It lives next to the database on the same volume.
func StampPath(stateDir string) string {
	return filepath.Join(stateDir, stampFilename)
}

// Store is the object-storage surface the backup needs.
type Store interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
	PutObject(ctx context.Context, key string, body io.Reader, contentType string) error
	StatObject(ctx context.Context, key string) (storage.Object, bool, error)
	DeleteObjects(ctx context.Context, keys []string) error
}

// Catalog is the catalog surface the backup needs: one way to snapshot the
// database file and one way to learn which staged zips a snapshot covers.
type Catalog interface {
	BackupTo(ctx context.Context, path string) error
	ListAlbumsByStatus(ctx context.Context, status catalog.AlbumStatus) ([]catalog.Album, error)
}

// Finalizer backs the catalog up once per batch of finished albums and deletes
// the staged zips the backup covers.
type Finalizer struct {
	store     Store
	catalog   Catalog
	stateDir  string
	stampPath string

	// mu keeps overlapping runs — the startup sweep and a queue drain — from
	// interleaving their steps.
	mu sync.Mutex
}

// NewFinalizer builds a finalizer that keeps its transient files in stateDir,
// the volume the catalog itself lives on.
func NewFinalizer(store Store, cat Catalog, stateDir string) *Finalizer {
	return &Finalizer{
		store:     store,
		catalog:   cat,
		stateDir:  stateDir,
		stampPath: StampPath(stateDir),
	}
}

// Run backs up the catalog and then deletes the staged zips the backup covers.
// The steps happen in this order on purpose, each only after the previous one
// is durable in S3, so a crash anywhere leaves either a leftover zip — cleaned
// up by the next run — or nothing to do. A run that starts while another is
// still going returns immediately without doing anything.
func (f *Finalizer) Run(ctx context.Context) error {
	if !f.mu.TryLock() {
		return nil
	}
	defer f.mu.Unlock()

	if err := os.MkdirAll(f.stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	snapshot, err := f.snapshot(ctx)
	if err != nil {
		return err
	}
	defer os.Remove(snapshot)

	stamp, err := f.uploadSnapshot(ctx, snapshot)
	if err != nil {
		return err
	}
	// The stamp is written before any delete: once the backup is durable, the
	// zips are already redundant, and a crash before the stamp would only cost
	// a repeated backup, never a lost one.
	if err := writeStamp(f.stampPath, stamp); err != nil {
		return err
	}

	return f.deleteStagedZips(ctx)
}

// snapshot writes a consistent copy of the catalog to a fresh file in stateDir
// and returns its path.
func (f *Finalizer) snapshot(ctx context.Context) (string, error) {
	file, err := os.CreateTemp(f.stateDir, "viewer.db.backup-*")
	if err != nil {
		return "", fmt.Errorf("create backup temp file: %w", err)
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close backup temp file: %w", err)
	}
	// VACUUM INTO refuses an existing target, and CreateTemp creates one.
	if err := os.Remove(name); err != nil {
		return "", fmt.Errorf("prepare backup temp file: %w", err)
	}
	if err := f.catalog.BackupTo(ctx, name); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// uploadSnapshot puts the snapshot in the bucket and returns the stored
// object's last-modified time, read back rather than taken from the local
// clock: the restore decision compares this timestamp against the same
// object's last-modified, so both must come from the same clock.
func (f *Finalizer) uploadSnapshot(ctx context.Context, path string) (time.Time, error) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, fmt.Errorf("open catalog snapshot %s: %w", path, err)
	}
	defer file.Close()
	if err := f.store.PutObject(ctx, BackupObjectKey, file, backupContentType); err != nil {
		return time.Time{}, fmt.Errorf("upload catalog backup: %w", err)
	}
	obj, ok, err := f.store.StatObject(ctx, BackupObjectKey)
	if err != nil {
		return time.Time{}, fmt.Errorf("stat catalog backup: %w", err)
	}
	if !ok {
		return time.Time{}, fmt.Errorf("catalog backup missing after upload: %s", BackupObjectKey)
	}
	return obj.LastModified, nil
}

// deleteStagedZips removes the staged zips of every album in a terminal state:
// extraction either succeeded or failed for good, so the backup now records
// everything the zip carried. Albums still queued or processing — and zips no
// album row covers yet — stay, so nothing that could still be extracted is
// ever deleted.
func (f *Finalizer) deleteStagedZips(ctx context.Context) error {
	albums := make([]catalog.Album, 0)
	for _, status := range []catalog.AlbumStatus{catalog.AlbumStatusReady, catalog.AlbumStatusFailed} {
		rows, err := f.catalog.ListAlbumsByStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("list %s albums: %w", status, err)
		}
		albums = append(albums, rows...)
	}

	keys := make([]string, 0, len(albums))
	seen := make(map[string]struct{}, len(albums))
	for _, album := range albums {
		key := strings.TrimSpace(album.SourceKey)
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil
	}
	if err := f.store.DeleteObjects(ctx, keys); err != nil {
		// A leftover zip only means this work repeats on the next run; the
		// backup covering it is already durable.
		return fmt.Errorf("delete staged zips: %w", err)
	}
	log.Printf("backup: catalog uploaded to %s and %d staged zip(s) deleted", BackupObjectKey, len(keys))
	return nil
}

// Restore replaces the local catalog with the bucket's snapshot when that
// snapshot is newer than the local stamp records. It reports whether it did.
func Restore(ctx context.Context, store Store, dbPath string, stampPath string) (bool, error) {
	obj, ok, err := store.StatObject(ctx, BackupObjectKey)
	if err != nil {
		return false, fmt.Errorf("stat catalog backup: %w", err)
	}
	if !ok {
		return false, nil
	}

	stamp, stampSet, err := readStamp(stampPath)
	if err != nil {
		return false, err
	}
	if !shouldRestore(&obj, fileExists(dbPath), stamp, stampSet) {
		return false, nil
	}

	if err := restoreObject(ctx, store, obj, dbPath); err != nil {
		return false, err
	}
	// Written after the replace: a crash before it simply re-restores the same
	// snapshot on the next startup.
	if err := writeStamp(stampPath, obj.LastModified); err != nil {
		return false, err
	}
	return true, nil
}

// shouldRestore is the restore decision. A database without a stamp never
// loses to the backup: that is an existing deployment (or a hand-replaced
// catalog) that has never uploaded, and its local data is authoritative.
func shouldRestore(backupObj *storage.Object, localExists bool, stamp time.Time, stampSet bool) bool {
	if backupObj == nil {
		return false
	}
	if !localExists {
		return true
	}
	if !stampSet {
		return false
	}
	return stamp.Before(backupObj.LastModified)
}

// restoreObject downloads the snapshot and moves it over the database file, in
// an order that never leaves a half-written catalog behind.
func restoreObject(ctx context.Context, store Store, obj storage.Object, dbPath string) error {
	dir := filepath.Dir(dbPath)
	// catalog.Open normally creates this; the restore runs before it does.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create catalog dir: %w", err)
	}

	body, _, err := store.GetObject(ctx, BackupObjectKey)
	if err != nil {
		return fmt.Errorf("download catalog backup: %w", err)
	}
	defer body.Close()

	// The temp file lives in the target's directory so the final rename stays
	// within one filesystem.
	tmp, err := os.CreateTemp(dir, "viewer.db.restore-*")
	if err != nil {
		return fmt.Errorf("create restore temp file: %w", err)
	}
	written, copyErr := io.Copy(tmp, body)
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("download catalog backup: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("close restore temp file: %w", closeErr)
	}
	if written != obj.Size {
		os.Remove(tmp.Name())
		return fmt.Errorf("restored catalog is %d bytes, want %d", written, obj.Size)
	}

	// A WAL left behind would be recovered onto the replaced file — SQLite
	// does not verify the frames belong to this database — so both sidecars
	// must go. Removing them first risks discarding commits that lived only in
	// the old database's WAL, but discarding the old database is exactly what
	// a restore is, and the next startup retries the restore.
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, fs.ErrNotExist) {
			os.Remove(tmp.Name())
			return fmt.Errorf("remove %s: %w", sidecar, err)
		}
	}

	if err := os.Rename(tmp.Name(), dbPath); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("replace catalog with restored backup: %w", err)
	}
	return nil
}

func readStamp(path string) (time.Time, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read backup stamp: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse backup stamp %q: %w", strings.TrimSpace(string(data)), err)
	}
	return at, true, nil
}

func writeStamp(path string, at time.Time) error {
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)), 0o644); err != nil {
		return fmt.Errorf("write backup stamp: %w", err)
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
