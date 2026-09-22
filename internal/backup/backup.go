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
//
// Every restore decision and every overwrite is logged with its inputs, so a
// boot that does not restore — and a finalize that then uploads a fresh
// catalog over an existing backup — can be reconstructed from the log alone.
package backup

import (
	"bytes"
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

// backupPrefix is the folder part of BackupObjectKey. It is only listed when
// the backup key itself is missing, to tell an empty namespace apart from a
// missing object in a populated one.
var backupPrefix = BackupObjectKey[:strings.LastIndex(BackupObjectKey, "/")+1]

// shrinkWarnFactor is how many times smaller a fresh snapshot must be than
// the backup it is about to replace before the finalizer treats the upload as
// an overwrite of a catalog that was never restored. The guard refuses the
// upload unless ALLOW_BACKUP_OVERWRITE lifts it.
const shrinkWarnFactor = 10

// maxLoggedSiblings caps how many sibling keys the missing-backup log line
// includes, so a large prefix cannot flood the log.
const maxLoggedSiblings = 5

// stampFilename marks which backup the local database already reflects. It is
// the restore decision's watermark: the database file's own mtime cannot be
// used, because in WAL mode commits can land in the sidecar without touching
// the main file's timestamp.
const stampFilename = "viewer.db.backup-stamp"

// sqliteMagic is the 16-byte header every SQLite database starts with.
var sqliteMagic = []byte("SQLite format 3\x00")

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
	ListObjects(ctx context.Context, prefix string) ([]storage.Object, error)
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

	// allowOverwrite lifts the overwrite guard: a snapshot that cannot be
	// traced to the backup lineage (no readable stamp) or that is drastically
	// smaller than the backup it would replace is refused unless this is set.
	allowOverwrite bool

	// mu keeps overlapping runs — the startup sweep and a queue drain — from
	// interleaving their steps.
	mu sync.Mutex
}

// NewFinalizer builds a finalizer that keeps its transient files in stateDir,
// the volume the catalog itself lives on. allowOverwrite is the operator's
// explicit opt-in to replacing an existing backup under the conditions the
// guard would refuse; see Finalizer.Run.
func NewFinalizer(store Store, cat Catalog, stateDir string, allowOverwrite bool) *Finalizer {
	return &Finalizer{
		store:          store,
		catalog:        cat,
		stateDir:       stateDir,
		stampPath:      StampPath(stateDir),
		allowOverwrite: allowOverwrite,
	}
}

// Run backs up the catalog and then deletes the staged zips the backup covers.
// The steps happen in this order on purpose, each only after the previous one
// is durable in S3, so a crash anywhere leaves either a leftover zip — cleaned
// up by the next run — or nothing to do. A run that starts while another is
// still going returns immediately without doing anything.
func (f *Finalizer) Run(ctx context.Context) error {
	if !f.mu.TryLock() {
		log.Printf("backup: another finalize is still running; skipping this run")
		return nil
	}
	defer f.mu.Unlock()

	if err := os.MkdirAll(f.stateDir, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	// Record the pre-state before touching anything: when a wiped state volume
	// ends up uploading a fresh catalog over a large existing backup, these
	// are the lines that say exactly that happened.
	existing, hadExisting, err := f.store.StatObject(ctx, BackupObjectKey)
	if err != nil {
		log.Printf("backup: could not stat the existing catalog backup before upload: %v", err)
	}
	stampAt, stampSet, stampErr := readStamp(f.stampPath)
	if stampErr != nil {
		log.Printf("backup: local stamp %s is unreadable: %v", f.stampPath, stampErr)
	}
	log.Printf(
		"backup: finalize pre-state: existing backup=%t (size=%d, last-modified=%s), local stamp=%s",
		hadExisting, existing.Size, formatTime(existing.LastModified), formatStamp(stampAt, stampSet),
	)

	snapshot, err := f.snapshot(ctx)
	if err != nil {
		return err
	}
	defer os.Remove(snapshot)

	info, err := os.Stat(snapshot)
	if err != nil {
		return fmt.Errorf("stat catalog snapshot: %w", err)
	}
	log.Printf("backup: local catalog snapshot is %d bytes", info.Size())

	// The overwrite guard is the last line of defense between a local catalog
	// that was never restored and the bucket's good backup. Serving from a
	// stampless database may be the right call (the restore decision treats it
	// as authoritative), but uploading it would destroy the one copy the
	// bucket holds — an authoritative-looking empty catalog over a good backup
	// is how a missed restore becomes permanent metadata loss.
	if hadExisting {
		if concern := overwriteConcern(stampSet, stampErr, info.Size(), existing); concern != "" {
			if f.allowOverwrite {
				log.Printf("backup: ALLOW_BACKUP_OVERWRITE is set; overwriting anyway (%s)", concern)
			} else {
				log.Printf("backup: REFUSED to overwrite the catalog backup: %s", concern)
				log.Printf("backup: the local catalog is untouched and the bucket's backup still stands; resolve the cause (see the restore decision log above) or set ALLOW_BACKUP_OVERWRITE=1 to force this upload")
				return fmt.Errorf("backup: refusing to overwrite the catalog backup: %s", concern)
			}
		}
	}

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

// overwriteConcern names the reason a fresh snapshot must not replace the
// bucket's backup, or "" when the upload may proceed. Two conditions block it:
// a local database whose backup lineage is unknown (no readable stamp), and a
// snapshot drastically smaller than the backup — both the signature of a boot
// whose restore silently did not happen.
func overwriteConcern(stampSet bool, stampErr error, snapshotSize int64, existing storage.Object) string {
	switch {
	case stampErr != nil:
		return fmt.Sprintf("the local stamp is unreadable (%v), so the database's backup lineage is unknown", stampErr)
	case !stampSet:
		return fmt.Sprintf(
			"the local database has no stamp, so it did not come from a backup; uploading would replace the %d-byte backup (%s) with an unverified catalog",
			existing.Size, formatTime(existing.LastModified),
		)
	case snapshotSize*shrinkWarnFactor < existing.Size:
		return fmt.Sprintf(
			"the fresh snapshot is %d bytes against a %d-byte backup (%s) — drastically smaller, as if the local catalog was never restored",
			snapshotSize, existing.Size, formatTime(existing.LastModified),
		)
	default:
		return ""
	}
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
	log.Printf(
		"backup: uploaded catalog snapshot to %s: size=%d, last-modified=%s",
		BackupObjectKey, obj.Size, formatTime(obj.LastModified),
	)
	return obj.LastModified, nil
}

// deleteStagedZips removes the staged zips of every album in a terminal state:
// extraction either succeeded or failed for good, so the backup now records
// everything the zip carried. Albums still queued or processing — and zips no
// album row covers yet — stay, so nothing that could still be extracted is
// ever deleted.
func (f *Finalizer) deleteStagedZips(ctx context.Context) error {
	albums := make([]catalog.Album, 0)
	counts := make(map[catalog.AlbumStatus]int, 2)
	for _, status := range []catalog.AlbumStatus{catalog.AlbumStatusReady, catalog.AlbumStatusFailed} {
		rows, err := f.catalog.ListAlbumsByStatus(ctx, status)
		if err != nil {
			return fmt.Errorf("list %s albums: %w", status, err)
		}
		counts[status] = len(rows)
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
		log.Printf(
			"backup: no staged zips to delete (SUCCEEDED=%d, FAILED=%d albums)",
			counts[catalog.AlbumStatusReady], counts[catalog.AlbumStatusFailed],
		)
		return nil
	}
	if err := f.store.DeleteObjects(ctx, keys); err != nil {
		// A leftover zip only means this work repeats on the next run; the
		// backup covering it is already durable.
		return fmt.Errorf("delete staged zips: %w", err)
	}
	log.Printf(
		"backup: catalog uploaded to %s and %d staged zip(s) deleted (SUCCEEDED=%d, FAILED=%d albums)",
		BackupObjectKey, len(keys), counts[catalog.AlbumStatusReady], counts[catalog.AlbumStatusFailed],
	)
	return nil
}

// Restore replaces the local catalog with the bucket's snapshot when that
// snapshot is newer than the local stamp records. It reports whether it did.
// Every decision is logged with its inputs, so a boot that does not restore
// always says why.
func Restore(ctx context.Context, store Store, dbPath string, stampPath string) (bool, error) {
	obj, ok, err := store.StatObject(ctx, BackupObjectKey)
	if err != nil {
		return false, fmt.Errorf("stat catalog backup: %w", err)
	}
	if !ok {
		logMissingBackup(ctx, store)
		return false, nil
	}
	log.Printf("backup: catalog backup found: size=%d, last-modified=%s", obj.Size, formatTime(obj.LastModified))

	stamp, stampSet, err := readStamp(stampPath)
	if err != nil {
		return false, err
	}

	dbSize, dbExists := fileSize(dbPath)
	restoring, reason := shouldRestore(&obj, dbExists, stamp, stampSet)
	log.Printf(
		"backup: restore decision: %s (local db exists=%t, size=%d, stamp=%s)",
		reason, dbExists, dbSize, formatStamp(stamp, stampSet),
	)
	if !restoring {
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

// logMissingBackup explains a not-found backup object before the run moves on
// without a restore. A stat alone cannot tell "first run, nothing uploaded
// yet" from "the backup exists but is not visible here" — the latter being
// namespace drift (bucket, endpoint, key prefix) or deletion. Listing the
// backup prefix once keeps the difference in the log instead of leaving the
// boot to silently proceed toward overwriting whatever it cannot see.
func logMissingBackup(ctx context.Context, store Store) {
	siblings, err := store.ListObjects(ctx, backupPrefix)
	if err != nil {
		log.Printf("backup: no catalog backup object at %s; listing %s failed too: %v", BackupObjectKey, backupPrefix, err)
		return
	}
	if len(siblings) == 0 {
		log.Printf(
			"backup: no catalog backup object at %s and nothing under %s: first run, an empty bucket, or the wrong namespace (check bucket, endpoint, key prefix)",
			BackupObjectKey, backupPrefix,
		)
		return
	}
	sample := siblings
	if len(sample) > maxLoggedSiblings {
		sample = sample[:maxLoggedSiblings]
	}
	keys := make([]string, 0, len(sample))
	for _, sibling := range sample {
		keys = append(keys, sibling.Key)
	}
	log.Printf(
		"backup: catalog backup %s is missing while %d other object(s) exist under %s (e.g. %s): the key was deleted or the namespace changed; NOT restoring",
		BackupObjectKey, len(siblings), backupPrefix, strings.Join(keys, ", "),
	)
}

// shouldRestore is the restore decision. A database without a stamp never
// loses to the backup: that is an existing deployment (or a hand-replaced
// catalog) that has never uploaded, and its local data is authoritative. The
// returned reason is the human-readable justification for the decision log.
func shouldRestore(backupObj *storage.Object, localExists bool, stamp time.Time, stampSet bool) (bool, string) {
	if backupObj == nil {
		return false, "no backup object"
	}
	if !localExists {
		return true, "local database is missing; rebuilding it from the bucket's snapshot"
	}
	if !stampSet {
		return false, "local database exists without a stamp; treating it as authoritative (existing deployment or hand-replaced catalog)"
	}
	if !stamp.Before(backupObj.LastModified) {
		return false, fmt.Sprintf(
			"backup is not newer than the stamp (stamp=%s, backup last-modified=%s)",
			formatStamp(stamp, true), formatTime(backupObj.LastModified),
		)
	}
	return true, "backup is newer than the stamp; restoring"
}

// restoreObject downloads the snapshot and moves it over the database file, in
// an order that never leaves a half-written catalog behind.
func restoreObject(ctx context.Context, store Store, obj storage.Object, dbPath string) error {
	log.Printf("backup: downloading catalog backup %s (%d bytes) to replace %s", BackupObjectKey, obj.Size, dbPath)
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
	log.Printf("backup: downloaded %d bytes; moving the restored snapshot over %s", written, dbPath)

	// A snapshot that is not a SQLite database would replace the local catalog
	// with garbage and then advance the stamp, closing the door on every later
	// restore attempt. The magic header is the cheapest check that catches an
	// empty, truncated or wrong object; the size check above catches the rest.
	if err := verifySQLiteHeader(tmp.Name()); err != nil {
		os.Remove(tmp.Name())
		return err
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

// verifySQLiteHeader reports whether the file at path starts with the SQLite
// magic bytes.
func verifySQLiteHeader(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open restored catalog: %w", err)
	}
	defer file.Close()
	header := make([]byte, len(sqliteMagic))
	if _, err := io.ReadFull(file, header); err != nil {
		return fmt.Errorf("restored catalog is too short for a SQLite header: %w", err)
	}
	if !bytes.Equal(header, sqliteMagic) {
		return fmt.Errorf("restored catalog does not start with the SQLite magic header (found %q)", header)
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

// fileSize reports path's byte size, with exists=false when it is missing or
// a directory.
func fileSize(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0, false
	}
	return info.Size(), true
}

// formatTime renders an object timestamp for the log; a zero time means the
// field was absent from the storage response.
func formatTime(at time.Time) string {
	if at.IsZero() {
		return "<none>"
	}
	return at.UTC().Format(time.RFC3339)
}

// formatStamp renders the local stamp for the log.
func formatStamp(at time.Time, set bool) string {
	if !set {
		return "<unset>"
	}
	return at.UTC().Format(time.RFC3339Nano)
}
