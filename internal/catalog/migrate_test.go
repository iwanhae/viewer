package catalog

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"viewer/internal/qdrant"
)

// encodeVectorLE serializes a vector the way the pre-0003 catalog stored them:
// little-endian float32. It exists only so tests can build realistic v2 files.
func encodeVectorLE(vec []float32) []byte {
	out := make([]byte, 4*len(vec))
	for i, value := range vec {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(value))
	}
	return out
}

// fakeSink records what migration 0003 sends, and can be told to fail the
// Nth upsert call (1-based; 0 never fails) or the collection setup.
type fakeSink struct {
	ensureCalls int
	ensureErr   error
	failOnCall  int
	upserts     [][]qdrant.PhotoRecord
}

func (f *fakeSink) EnsureCollection(ctx context.Context) error {
	f.ensureCalls++
	return f.ensureErr
}

func (f *fakeSink) UpsertPhotos(ctx context.Context, records []qdrant.PhotoRecord) error {
	if f.failOnCall == len(f.upserts)+1 {
		return fmt.Errorf("simulated upload failure on call %d", f.failOnCall)
	}
	f.upserts = append(f.upserts, append([]qdrant.PhotoRecord(nil), records...))
	return nil
}

func (f *fakeSink) records() []qdrant.PhotoRecord {
	var flat []qdrant.PhotoRecord
	for _, batch := range f.upserts {
		flat = append(flat, batch...)
	}
	return flat
}

// v2Blob is one blob row of a v2-shaped catalog file.
type v2Blob struct {
	hash      string
	status    string
	embedding []byte
}

// buildV2Catalog writes a database that looks exactly like migration 0003
// expects to find one: the full v2 schema — embedding column, lease column,
// vec0 index — at user_version 2, populated with the given rows. Well-formed
// embeddings are backfilled into the vec0 index the way migration 0002 would
// have, so the file is a faithful v2 snapshot, not merely a v2 version stamp.
func buildV2Catalog(t *testing.T, path string, albums []Album, blobs []v2Blob, photos []Photo) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE albums (
			id                TEXT PRIMARY KEY,
			original_filename TEXT NOT NULL,
			size_bytes        INTEGER NOT NULL DEFAULT 0,
			status            TEXT NOT NULL,
			source_key        TEXT NOT NULL DEFAULT '',
			photo_count       INTEGER NOT NULL DEFAULT 0,
			error             TEXT NOT NULL DEFAULT '',
			created_at        TEXT NOT NULL,
			updated_at        TEXT NOT NULL
		);
		CREATE TABLE blobs (
			hash                 TEXT PRIMARY KEY,
			size_bytes           INTEGER NOT NULL,
			content_type         TEXT NOT NULL DEFAULT 'application/octet-stream',
			embedding_status     TEXT NOT NULL DEFAULT 'pending',
			embedding            BLOB,
			embedding_error      TEXT NOT NULL DEFAULT '',
			embedding_lease_until INTEGER NOT NULL DEFAULT 0,
			created_at           TEXT NOT NULL
		);
		CREATE TABLE photos (
			album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
			idx      INTEGER NOT NULL,
			name     TEXT NOT NULL,
			hash     TEXT NOT NULL,
			width    INTEGER NOT NULL,
			height   INTEGER NOT NULL,
			ratio    REAL NOT NULL,
			PRIMARY KEY (album_id, idx)
		);
	` + blobEmbeddingsDDL()); err != nil {
		t.Fatalf("seed v2 schema: %v", err)
	}
	for _, album := range albums {
		if album.CreatedAt == "" {
			album.CreatedAt = "2024-01-01T00:00:00Z"
			album.UpdatedAt = album.CreatedAt
		}
		if _, err := db.Exec(`
			INSERT INTO albums (id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at)
			VALUES (?, ?, 0, ?, '', 0, '', ?, ?)`,
			album.ID, album.OriginalFilename, string(album.Status), album.CreatedAt, album.UpdatedAt); err != nil {
			t.Fatalf("seed album %s: %v", album.ID, err)
		}
	}
	for _, blob := range blobs {
		if _, err := db.Exec(`
			INSERT INTO blobs (hash, size_bytes, embedding_status, embedding, created_at)
			VALUES (?, 1, ?, ?, '2024-01-01T00:00:00Z')`,
			blob.hash, blob.status, blob.embedding); err != nil {
			t.Fatalf("seed blob %s: %v", blob.hash, err)
		}
		// Migration 0002 indexed exactly the embeddings that fit the column.
		if len(blob.embedding) == 4*EmbeddingDim {
			if _, err := db.Exec(`INSERT INTO blob_embeddings(hash, embedding) VALUES (?, ?)`, blob.hash, blob.embedding); err != nil {
				t.Fatalf("seed index for %s: %v", blob.hash, err)
			}
		}
	}
	for _, photo := range photos {
		if err := db.QueryRow(`
			INSERT INTO photos (album_id, idx, name, hash, width, height, ratio)
			VALUES (?, ?, ?, ?, ?, ?, 1)`,
			photo.AlbumID, photo.Index, photo.Name, photo.Hash, photo.Width, photo.Height).Err(); err != nil {
			t.Fatalf("seed photo %s:%d: %v", photo.AlbumID, photo.Index, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("stamp v2 version: %v", err)
	}
}

// fileVersion reads user_version straight from the file, outside any Store.
func fileVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return version
}

// tableExists probes sqlite_master for one table or virtual table.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("probe sqlite_master for %s: %v", name, err)
	}
	return n > 0
}

// columnExists probes one table's columns via the pragma.
func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
		t.Fatalf("inspect %s columns: %v", table, err)
	}
	return n > 0
}

// TestMigrationChainIsWellFormed pins the invariants every later tooling leans
// on: versions count up from 1 without gaps and names are unique, so a
// user_version unambiguously identifies which migrations a file has seen.
func TestMigrationChainIsWellFormed(t *testing.T) {
	seen := make(map[int]string, len(migrations))
	for i, m := range migrations {
		if m.version != i+1 {
			t.Fatalf("migration %d has version %d, want %d", i, m.version, i+1)
		}
		if strings.TrimSpace(m.name) == "" {
			t.Fatalf("migration %d has no name", m.version)
		}
		if other, dup := seen[m.version]; dup {
			t.Fatalf("version %d claimed by both %s and %s", m.version, other, m.name)
		}
		seen[m.version] = m.name
	}
}

// TestQdrantMigrationKeepsStmtsEmpty pins the shape migration 0003 must keep:
// the runner executes stmts BEFORE fn, and the vector storage may only be
// dropped after the upload inside fn has fully succeeded. A stmt added back
// here would destroy vectors that were never uploaded.
func TestQdrantMigrationKeepsStmtsEmpty(t *testing.T) {
	m := migrations[2]
	if m.version != 3 || m.name != "qdrant_vectors" {
		t.Fatalf("migration 3 = %d_%s, want 3_qdrant_vectors", m.version, m.name)
	}
	if len(m.stmts) != 0 {
		t.Fatalf("migration 0003 carries %d stmts; they would run before the upload fn and drop vectors prematurely", len(m.stmts))
	}
	if m.fn == nil {
		t.Fatalf("migration 0003 has no fn; the upload and the drops must live there")
	}
}

// TestOpenAppliesMigrationsOnce verifies a fresh catalog ends at this build's
// newest version with all vector storage already gone — a fresh catalog has no
// vectors, so even a nil sink must be accepted — and that reopening the
// migrated file applies nothing again.
func TestOpenAppliesMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")

	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if got := fileVersion(t, path); got != len(migrations) {
		t.Fatalf("user_version=%d want %d", got, len(migrations))
	}
	if tableExists(t, store.db, "blob_embeddings") {
		t.Fatalf("blob_embeddings virtual table must be dropped by migration 0003")
	}
	if columnExists(t, store.db, "blobs", "embedding") {
		t.Fatalf("blobs.embedding column must be dropped by migration 0003")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	defer reopened.Close()
	if got := fileVersion(t, path); got != len(migrations) {
		t.Fatalf("reopen bumped user_version to %d, want unchanged %d", got, len(migrations))
	}
}

// TestOpenMigratesV2VectorsToSink assembles a faithful v2 file — embedding
// column, populated vec0 index, photos over ready blobs — and verifies one Open
// uploads exactly the photo-attached vectors to the sink in (album, idx) order
// with the stored LE bytes decoded, and only then removes every trace of vector
// storage from SQLite.
func TestOpenMigratesV2VectorsToSink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	ctx := context.Background()

	vecA := vec768(0.25, -1, 3)
	vecB := vec768(9)
	buildV2Catalog(t, path,
		[]Album{
			{ID: "album-a", OriginalFilename: "a.zip", Status: AlbumStatusReady},
			{ID: "album-b", OriginalFilename: "b.zip", Status: AlbumStatusReady},
		},
		[]v2Blob{
			{hash: "hash-a", status: "ready", embedding: encodeVectorLE(vecA)},
			{hash: "hash-b", status: "ready", embedding: encodeVectorLE(vecB)},
			// Referenced by a photo but the wrong byte length: it was never
			// queryable, so it is skipped with a count, not uploaded.
			{hash: "hash-bad", status: "ready", embedding: []byte{1, 2, 3}},
			// No photo references this one, so its vector has no (album, idx)
			// to become a point and is left behind with the column.
			{hash: "hash-orphan", status: "ready", embedding: encodeVectorLE(vecB)},
			{hash: "hash-none", status: "pending", embedding: nil},
		},
		[]Photo{
			{AlbumID: "album-a", Index: 0, Name: "a0.png", Hash: "hash-a", Width: 640, Height: 480},
			{AlbumID: "album-a", Index: 1, Name: "a1.png", Hash: "hash-b", Width: 10, Height: 20},
			{AlbumID: "album-b", Index: 0, Name: "b0.png", Hash: "hash-a", Width: 1, Height: 1},
			{AlbumID: "album-b", Index: 1, Name: "b1.png", Hash: "hash-bad", Width: 2, Height: 2},
		})

	sink := &fakeSink{}
	store, err := Open(path, sink)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer store.Close()

	if sink.ensureCalls != 1 {
		t.Fatalf("EnsureCollection called %d times, want exactly 1", sink.ensureCalls)
	}

	// Three uploadable pairs: the orphan blob and the malformed one are out.
	records := sink.records()
	if len(records) != 3 {
		t.Fatalf("uploaded %d records, want 3: %+v", len(records), records)
	}
	want := []struct {
		album  string
		idx    int
		hash   string
		w, h   int
		vector []float32
	}{
		{"album-a", 0, "hash-a", 640, 480, vecA},
		{"album-a", 1, "hash-b", 10, 20, vecB},
		{"album-b", 0, "hash-a", 1, 1, vecA},
	}
	for i, w := range want {
		got := records[i]
		if got.AlbumID != w.album || got.Idx != w.idx || got.Hash != w.hash {
			t.Fatalf("record %d = %s:%d hash %s, want %s:%d hash %s", i, got.AlbumID, got.Idx, got.Hash, w.album, w.idx, w.hash)
		}
		if got.W != w.w || got.H != w.h {
			t.Fatalf("record %d carries %dx%d, want %dx%d", i, got.W, got.H, w.w, w.h)
		}
		if len(got.Vector) != len(w.vector) {
			t.Fatalf("record %d vector length %d, want %d", i, len(got.Vector), len(w.vector))
		}
		for j := range w.vector {
			if got.Vector[j] != w.vector[j] {
				t.Fatalf("record %d vector[%d] = %v, want %v (stored bytes must decode LE)", i, j, got.Vector[j], w.vector[j])
			}
		}
	}

	// The upload happened before the drops: both vector storages are gone now.
	if tableExists(t, store.db, "blob_embeddings") {
		t.Fatalf("blob_embeddings survived migration 0003")
	}
	if columnExists(t, store.db, "blobs", "embedding") {
		t.Fatalf("blobs.embedding survived migration 0003")
	}
	if got := fileVersion(t, path); got != len(migrations) {
		t.Fatalf("user_version=%d want %d", got, len(migrations))
	}

	// Bookkeeping survives the drop: statuses stay, so the drift check and the
	// per-album progress keep working without the vectors.
	blob, err := store.GetBlob(ctx, "hash-a")
	if err != nil || blob.EmbeddingStatus != EmbeddingStatusReady {
		t.Fatalf("blob hash-a=%+v err=%v want ready", blob, err)
	}
	// All four referenced blobs stay ready — including the malformed one, which
	// becomes a permanent "ready but never indexed" entry exactly as the v2
	// backfill's precedent set.
	if count, err := store.ReadyPairCount(ctx); err != nil || count != 4 {
		t.Fatalf("ready pairs=%d err=%v want 4", count, err)
	}
}

// TestMigrationUploadFailureRollsBackAndRetries pins the guarantee that makes
// the destructive migration safe: a failed upload rolls the whole thing back —
// version, embedding column, and vec0 index all return — and a retry with a
// working sink then converges.
func TestMigrationUploadFailureRollsBackAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	vec := vec768(1, 2)
	buildV2Catalog(t, path,
		[]Album{{ID: "album-a", OriginalFilename: "a.zip", Status: AlbumStatusReady}},
		[]v2Blob{{hash: "hash-a", status: "ready", embedding: encodeVectorLE(vec)}},
		[]Photo{{AlbumID: "album-a", Index: 0, Name: "a.png", Hash: "hash-a", Width: 1, Height: 1}})

	sink := &fakeSink{failOnCall: 1}
	if _, err := Open(path, sink); err == nil {
		t.Fatalf("expected open to fail when the upload fails")
	} else if !strings.Contains(err.Error(), "upload embedding batch") {
		t.Fatalf("expected the error to name the failed upload, got %v", err)
	}

	// The file is exactly as it was: version, column, and index all intact, so
	// the next boot can retry from scratch.
	after, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open rolled-back db: %v", err)
	}
	if got := fileVersion(t, path); got != 2 {
		t.Fatalf("user_version=%d want unchanged 2 after failed upload", got)
	}
	if !columnExists(t, after, "blobs", "embedding") {
		t.Fatalf("blobs.embedding was dropped despite the failed upload")
	}
	if !tableExists(t, after, "blob_embeddings") {
		t.Fatalf("blob_embeddings was dropped despite the failed upload")
	}
	if err := after.Close(); err != nil {
		t.Fatalf("close rolled-back db: %v", err)
	}

	// Retry with a working sink: the same file converges to v3.
	retry := &fakeSink{}
	store, err := Open(path, retry)
	if err != nil {
		t.Fatalf("retry open: %v", err)
	}
	defer store.Close()
	if got := fileVersion(t, path); got != len(migrations) {
		t.Fatalf("user_version=%d want %d after retry", got, len(migrations))
	}
	if records := retry.records(); len(records) != 1 || records[0].Hash != "hash-a" || records[0].Vector[0] != 1 {
		t.Fatalf("retry uploaded %+v, want the single hash-a vector", records)
	}
	if columnExists(t, store.db, "blobs", "embedding") {
		t.Fatalf("blobs.embedding still present after the successful retry")
	}
}

// TestOpenRefusesNilSinkWithVectors covers the guard that keeps the one
// irreplaceable copy of the vectors from being destroyed with nowhere to send
// it: a nil sink plus uploadable vectors must fail the open with the file left
// exactly as it was.
func TestOpenRefusesNilSinkWithVectors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	buildV2Catalog(t, path,
		[]Album{{ID: "album-a", OriginalFilename: "a.zip", Status: AlbumStatusReady}},
		[]v2Blob{{hash: "hash-a", status: "ready", embedding: encodeVectorLE(vec768(1))}},
		[]Photo{{AlbumID: "album-a", Index: 0, Name: "a.png", Hash: "hash-a", Width: 1, Height: 1}})

	_, err := Open(path, nil)
	if err == nil {
		t.Fatalf("expected open to refuse dropping vectors with no sink")
	} else if !strings.Contains(err.Error(), "refusing to drop") || !strings.Contains(err.Error(), "Qdrant is not configured") {
		t.Fatalf("expected the error to explain the missing Qdrant configuration, got %v", err)
	}
	if got := fileVersion(t, path); got != 2 {
		t.Fatalf("user_version=%d want unchanged 2", got)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open refused db: %v", err)
	}
	defer db.Close()
	if !columnExists(t, db, "blobs", "embedding") {
		t.Fatalf("blobs.embedding was dropped despite the refusal")
	}
}

// TestOpenNilSinkWithoutVectorsSucceeds covers the flip side of the guard: a
// catalog with nothing to upload gets the drops even with a nil sink, which is
// what keeps vectorless test databases and never-embedded catalogs opening.
func TestOpenNilSinkWithoutVectorsSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	buildV2Catalog(t, path,
		[]Album{{ID: "album-a", OriginalFilename: "a.zip", Status: AlbumStatusReady}},
		[]v2Blob{
			{hash: "hash-pending", status: "pending", embedding: nil},
			// A photo-referenced blob with a malformed stored vector is not
			// uploadable either: it was never queryable and never leaves SQLite
			// as a vector.
			{hash: "hash-bad", status: "ready", embedding: []byte{1, 2, 3}},
		},
		[]Photo{
			{AlbumID: "album-a", Index: 0, Name: "a.png", Hash: "hash-pending", Width: 1, Height: 1},
			{AlbumID: "album-a", Index: 1, Name: "b.png", Hash: "hash-bad", Width: 1, Height: 1},
		})

	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open catalog without vectors: %v", err)
	}
	defer store.Close()
	if got := fileVersion(t, path); got != len(migrations) {
		t.Fatalf("user_version=%d want %d", got, len(migrations))
	}
	if tableExists(t, store.db, "blob_embeddings") {
		t.Fatalf("blob_embeddings must be dropped even without a sink")
	}
	if columnExists(t, store.db, "blobs", "embedding") {
		t.Fatalf("blobs.embedding must be dropped even without a sink")
	}
}

// TestMigrationEnsureCollectionFailureRollsBack: a sink that cannot host the
// collection aborts before a single vector moves, and the file stays at v2.
func TestMigrationEnsureCollectionFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	buildV2Catalog(t, path,
		[]Album{{ID: "album-a", OriginalFilename: "a.zip", Status: AlbumStatusReady}},
		[]v2Blob{{hash: "hash-a", status: "ready", embedding: encodeVectorLE(vec768(1))}},
		[]Photo{{AlbumID: "album-a", Index: 0, Name: "a.png", Hash: "hash-a", Width: 1, Height: 1}})

	sink := &fakeSink{ensureErr: errors.New("qdrant unreachable")}
	if _, err := Open(path, sink); err == nil {
		t.Fatalf("expected open to fail when the collection cannot be ensured")
	}
	if sink.ensureCalls != 1 {
		t.Fatalf("EnsureCollection called %d times, want 1", sink.ensureCalls)
	}
	if got := fileVersion(t, path); got != 2 {
		t.Fatalf("user_version=%d want unchanged 2", got)
	}
}

// TestDecodeVectorLERoundTrip pins the stored-vector format migration 0003
// decodes: little-endian float32, with truncated input answered by nil rather
// than a garbage prefix.
func TestDecodeVectorLERoundTrip(t *testing.T) {
	if got := decodeVectorLE(nil); got != nil {
		t.Fatalf("expected nil for nil input, got %v", got)
	}
	if got := decodeVectorLE([]byte{1, 2, 3}); got != nil {
		t.Fatalf("expected nil for truncated input, got %v", got)
	}

	in := []float32{0, 1.5, -2.25, 1e-8}
	out := decodeVectorLE(encodeVectorLE(in))
	if len(out) != len(in) {
		t.Fatalf("length mismatch: %d vs %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("value %d mismatch: %v vs %v", i, out[i], in[i])
		}
	}
}

// TestOpenMigratesLegacyCatalogFullyCurrent assembles a pre-migration catalog —
// no lease column, wire-incompatible album statuses, and a mix of well-formed,
// malformed and absent embeddings — and verifies one Open brings it all the way
// to the current schema: lease column added, statuses rewritten, and the vector
// storage dropped (no photo rows exist, so there is nothing to upload and the
// nil sink is accepted).
func TestOpenMigratesLegacyCatalogFullyCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	ctx := context.Background()

	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE albums (
			id                TEXT PRIMARY KEY,
			original_filename TEXT NOT NULL,
			size_bytes        INTEGER NOT NULL DEFAULT 0,
			status            TEXT NOT NULL,
			source_key        TEXT NOT NULL DEFAULT '',
			photo_count       INTEGER NOT NULL DEFAULT 0,
			error             TEXT NOT NULL DEFAULT '',
			created_at        TEXT NOT NULL,
			updated_at        TEXT NOT NULL
		);
		INSERT INTO albums (id, original_filename, status, created_at, updated_at) VALUES
			('legacy-ready',  'ready.zip',   'READY',   '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z'),
			('legacy-pending','pending.zip', 'PENDING', '2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z');
		CREATE TABLE blobs (
			hash             TEXT PRIMARY KEY,
			size_bytes       INTEGER NOT NULL,
			content_type     TEXT NOT NULL DEFAULT 'application/octet-stream',
			embedding_status TEXT NOT NULL DEFAULT 'pending',
			embedding        BLOB,
			embedding_error  TEXT NOT NULL DEFAULT '',
			created_at       TEXT NOT NULL
		);`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	seedLegacyBlob := func(hash, status string, embedding any) {
		t.Helper()
		if _, err := legacy.Exec(
			`INSERT INTO blobs (hash, size_bytes, embedding_status, embedding, created_at) VALUES (?, 1, ?, ?, '2024-01-01T00:00:00Z')`,
			hash, status, embedding); err != nil {
			t.Fatalf("seed blob %s: %v", hash, err)
		}
	}
	seedLegacyBlob("good", "ready", encodeVectorLE(vec768(1)))
	seedLegacyBlob("bad", "ready", []byte{1, 2, 3})
	seedLegacyBlob("none", "pending", nil)
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer store.Close()

	if got := fileVersion(t, path); got != len(migrations) {
		t.Fatalf("user_version=%d want %d", got, len(migrations))
	}

	// Migration 0001 added the lease column the claim path depends on.
	if !columnExists(t, store.db, "blobs", "embedding_lease_until") {
		t.Fatalf("embedding_lease_until column missing after migration")
	}

	// ...and rewrote the legacy album statuses.
	ready, err := store.GetAlbum(ctx, "legacy-ready")
	if err != nil || ready.Status != AlbumStatusReady {
		t.Fatalf("legacy READY album=%+v err=%v want %s", ready, err, AlbumStatusReady)
	}
	pending, err := store.GetAlbum(ctx, "legacy-pending")
	if err != nil || pending.Status != AlbumStatusQueued {
		t.Fatalf("legacy PENDING album=%+v err=%v want %s", pending, err, AlbumStatusQueued)
	}

	// Migration 0003 dropped the vector storage the later migrations created
	// for the "good" blob: with no photos in the file there was nothing to
	// upload, and the nil sink was accepted.
	if tableExists(t, store.db, "blob_embeddings") {
		t.Fatalf("blob_embeddings survived the full migration of a vectorless file")
	}
	if columnExists(t, store.db, "blobs", "embedding") {
		t.Fatalf("blobs.embedding survived the full migration of a vectorless file")
	}

	// The malformed row was not promoted to ready either: it stays terminal
	// (unclaimable), so exactly the one pending blob is claimable.
	claimed, err := store.ClaimPendingEmbeddings(ctx, 10, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("claim pending: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Hash != "none" {
		t.Fatalf("claimed %+v want exactly none", claimed)
	}
}

// TestOpenMigrationFailureRollsBack pins the guarantee startup safety rests on:
// a migration that fails midway leaves the file byte-for-byte at its previous
// version — including the vec0 DDL, whose shadow tables must roll back with the
// transaction — so a fixed binary can simply try again.
func TestOpenMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")

	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	// Append a migration that creates a vec0 table and its shadow tables
	// before walking into invalid SQL.
	orig := migrations
	migrations = append(migrations, migration{
		version: len(migrations) + 1,
		name:    "broken_vec",
		stmts: []string{
			`CREATE VIRTUAL TABLE broken_embeddings USING vec0(hash text primary key, embedding float[4])`,
			`CREATE TABLE this_statement_is_not_sql (`,
		},
	})
	_, openErr := Open(path, nil)
	// Restore the chain before anything else opens the file.
	migrations = orig

	if openErr == nil {
		t.Fatalf("expected open to fail on the broken migration")
	} else if !strings.Contains(openErr.Error(), "broken_vec") {
		t.Fatalf("expected the error to name the failed migration, got %v", openErr)
	}

	// Nothing from the failed migration survived: not the version bump, not
	// the virtual table, not its shadow tables.
	if got := fileVersion(t, path); got != len(migrations) {
		t.Fatalf("user_version=%d want unchanged %d", got, len(migrations))
	}
	after, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	var leftovers int
	if err := after.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'broken_embeddings%'`).Scan(&leftovers); err != nil {
		after.Close()
		t.Fatalf("probe sqlite_master: %v", err)
	}
	if err := after.Close(); err != nil {
		t.Fatalf("close migrated db: %v", err)
	}
	if leftovers != 0 {
		t.Fatalf("failed migration left %d broken_embeddings table(s) behind", leftovers)
	}

	// With the broken migration gone from the chain, the same file opens and
	// keeps working.
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.EmbeddingCounts(context.Background()); err != nil {
		t.Fatalf("counts on recovered catalog: %v", err)
	}
}

// TestOpenToleratesNewerSchema covers a binary older than the file it opens —
// a downgrade after restoring a newer backup. The migrations chain must leave
// such a file alone rather than fail startup or roll the version back. (Since
// 0003 drops vector storage this only preserves the schema, not functionality:
// a downgrade below 0003 is unsupported.)
func TestOpenToleratesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")

	store, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	// Simulate a file from a future build.
	future, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open future db: %v", err)
	}
	if _, err := future.Exec(`PRAGMA user_version = 999`); err != nil {
		future.Close()
		t.Fatalf("bump user_version: %v", err)
	}
	if err := future.Close(); err != nil {
		t.Fatalf("close future db: %v", err)
	}

	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open catalog from the future: %v", err)
	}
	defer reopened.Close()
	if got := fileVersion(t, path); got != 999 {
		t.Fatalf("user_version=%d want untouched 999", got)
	}
	// Sanity: the untouched file still serves its existing data.
	if _, err := reopened.EmbeddingCounts(context.Background()); err != nil {
		t.Fatalf("counts on newer schema: %v", err)
	}
}
