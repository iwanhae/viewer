package catalog

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"math"

	"viewer/internal/qdrant"
)

// EmbeddingDim is the vector length every ready embedding must have. The
// dimension itself is owned by the Qdrant collection (the vec0 column that
// used to bake it into the schema was dropped in migration 0003); the catalog
// keeps the constant so recommend and the worker API wire contract have one
// place to mirror it from. Moving to a differently-sized model means pointing
// the collection at a new size and re-embedding the catalog.
const EmbeddingDim = 768

// VectorSink is the upload target migration 0003 moves the catalog's stored
// vectors to before it drops them from SQLite. It is satisfied structurally by
// *qdrant.Client. A nil sink is allowed only when there is nothing to upload.
type VectorSink interface {
	EnsureCollection(ctx context.Context) error
	UpsertPhotos(ctx context.Context, records []qdrant.PhotoRecord) error
}

// schema creates the catalog's baseline tables and indexes. Every statement is
// IF NOT EXISTS so migration 0001 can apply it to databases that already have
// some or all of it.
const schema = `
CREATE TABLE IF NOT EXISTS albums (
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
CREATE INDEX IF NOT EXISTS idx_albums_status ON albums(status);
CREATE INDEX IF NOT EXISTS idx_albums_source_key ON albums(source_key);

CREATE TABLE IF NOT EXISTS blobs (
	hash                 TEXT PRIMARY KEY,
	size_bytes           INTEGER NOT NULL,
	content_type         TEXT NOT NULL DEFAULT 'application/octet-stream',
	embedding_status     TEXT NOT NULL DEFAULT 'pending',
	embedding            BLOB,
	embedding_error      TEXT NOT NULL DEFAULT '',
	embedding_lease_until INTEGER NOT NULL DEFAULT 0,
	created_at           TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_blobs_embedding_status ON blobs(embedding_status);

CREATE TABLE IF NOT EXISTS photos (
	album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
	idx      INTEGER NOT NULL,
	name     TEXT NOT NULL,
	hash     TEXT NOT NULL,
	width    INTEGER NOT NULL,
	height   INTEGER NOT NULL,
	ratio    REAL NOT NULL,
	PRIMARY KEY (album_id, idx)
);
CREATE INDEX IF NOT EXISTS idx_photos_hash ON photos(hash);
`

// blobEmbeddingsDDL builds the vector index over blob embeddings. The table is
// keyed by the blob hash itself — no surrogate id — and cosine distance is
// computed by the index, which makes pre-normalization unnecessary: cosine is
// scale-invariant, so raw model output ranks identically to the normalized
// vectors the in-memory index used to hold.
//
// The dimension is part of the table's identity. A differently-sized embedding
// does not fit and needs a new migration, not an edit of this one.
func blobEmbeddingsDDL() string {
	return fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS blob_embeddings USING vec0(
	hash      text primary key,
	embedding float[%d] distance_metric=cosine
)`, EmbeddingDim)
}

// migrations is the append-only history of catalog schema changes. Version 1
// is the baseline: the CREATE statements plus the two data fixes that used to
// run inline on every open, all idempotent because pre-migration databases
// arrive here with their schema already partially in place. To change the
// schema, append a migration — never edit one that shipped.
var migrations = []migration{
	{
		version: 1,
		name:    "baseline",
		stmts: []string{
			schema,
			`UPDATE albums SET status = 'SUCCEEDED' WHERE status = 'READY'`,
			`UPDATE albums SET status = 'QUEUED' WHERE status = 'PENDING'`,
		},
		fn: addBlobLeaseColumnIfMissing,
	},
	{
		version: 2,
		name:    "blob_embeddings",
		stmts:   []string{blobEmbeddingsDDL()},
		fn:      backfillBlobEmbeddings,
	},
	{
		// stmts stays deliberately empty: the runner executes stmts before fn,
		// and the vector storage must only be dropped after the upload inside
		// fn has fully succeeded — not before.
		version: 3,
		name:    "qdrant_vectors",
		stmts:   nil,
		fn:      migrateVectorsToQdrant,
	},
	{
		version: 4,
		name:    "webp_encoding",
		stmts: []string{
			`ALTER TABLE blobs ADD COLUMN encoding_status TEXT NOT NULL DEFAULT 'pending'`,
			`ALTER TABLE blobs ADD COLUMN encoding_token TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE blobs ADD COLUMN encoding_lease_until INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE blobs ADD COLUMN encoding_stage_key TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE blobs ADD COLUMN encoding_error TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE blobs ADD COLUMN encoding_gate INTEGER NOT NULL DEFAULT 0`,
			`UPDATE blobs SET encoding_status = 'skipped' WHERE content_type = 'image/webp'`,
			`CREATE INDEX idx_blobs_encoding_priority ON blobs(encoding_status, size_bytes DESC, created_at, hash)`,
		},
	},
}

// addBlobLeaseColumnIfMissing adds the external-worker lease column to blobs.
// It keeps the pragma probe the inline version used instead of matching driver
// error strings: databases that already got the column from an earlier release
// must skip the ALTER, not fail on it.
func addBlobLeaseColumnIfMissing(ctx context.Context, tx *sql.Tx, _ VectorSink) error {
	var leaseColumn int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('blobs') WHERE name = 'embedding_lease_until'`,
	).Scan(&leaseColumn); err != nil {
		return fmt.Errorf("inspect blob columns: %w", err)
	}
	if leaseColumn == 0 {
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE blobs ADD COLUMN embedding_lease_until INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add blob lease column: %w", err)
		}
	}
	return nil
}

// backfillBlobEmbeddings loads every stored embedding into the fresh vec0
// index. Embeddings whose length does not match EmbeddingDim are skipped with
// a count in the log: they predate the write-path dimension check, cannot be
// queried by a float[768] column, and rank garbage in the old index's silent
// prefix-truncation anyway.
func backfillBlobEmbeddings(ctx context.Context, tx *sql.Tx, _ VectorSink) error {
	var skipped int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM blobs WHERE embedding IS NOT NULL AND length(embedding) != ?`,
		4*EmbeddingDim,
	).Scan(&skipped); err != nil {
		return fmt.Errorf("count malformed embeddings: %w", err)
	}
	if skipped > 0 {
		log.Printf("catalog: skipping %d blob(s) whose embedding is not %d bytes", skipped, 4*EmbeddingDim)
	}
	inserted, err := execCount(ctx, tx, `
		INSERT INTO blob_embeddings(hash, embedding)
		SELECT hash, embedding FROM blobs
		WHERE embedding IS NOT NULL AND length(embedding) = ?`,
		4*EmbeddingDim)
	if err != nil {
		return fmt.Errorf("backfill vector index: %w", err)
	}
	log.Printf("catalog: indexed %d blob embedding(s) into blob_embeddings", inserted)
	return nil
}

// execCount runs a statement and reports how many rows it affected.
func execCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count affected rows: %w", err)
	}
	return count, nil
}

// vectorUploadBatchSize is how many photo rows one upload batch carries. It
// mirrors the Qdrant client's own chunking, so one batch usually costs one
// request.
const vectorUploadBatchSize = 256

// vectorUploadProgressEvery is how many batches pass between two progress
// lines. The initial migration of a large library uploads for a while, and a
// silent upload looks like a hung start.
const vectorUploadProgressEvery = 50

// migrateVectorsToQdrant uploads every photo's embedding to the vector sink
// and only then drops all vector storage from SQLite. The whole migration —
// upload plus drops plus the user_version bump — commits or rolls back as one
// transaction, so a failed upload leaves the file exactly as it was and the
// next boot retries from scratch; the upserts are idempotent, so retrying
// converges. A nil sink is refused while any uploadable vector remains:
// destroying the only copy of the vectors with nowhere to send them would turn
// every recommendation permanently empty. A nil sink with nothing to upload is
// fine — test databases and vectorless catalogs just get the drops.
func migrateVectorsToQdrant(ctx context.Context, tx *sql.Tx, sink VectorSink) error {
	// Only photos count: a blob no photo references has no (album, idx) to
	// become a point, so its vector was unreachable by search anyway.
	var uploadable int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM photos p JOIN blobs b ON b.hash = p.hash
		WHERE b.embedding IS NOT NULL AND length(b.embedding) = ?`,
		4*EmbeddingDim).Scan(&uploadable); err != nil {
		return fmt.Errorf("count uploadable embeddings: %w", err)
	}

	if sink != nil {
		if err := sink.EnsureCollection(ctx); err != nil {
			return fmt.Errorf("ensure vector collection: %w", err)
		}
		uploaded, err := uploadVectors(ctx, tx, sink, uploadable)
		if err != nil {
			return err
		}
		log.Printf("catalog: uploaded %d photo embedding(s) to the vector store", uploaded)
	} else if uploadable > 0 {
		return fmt.Errorf("refusing to drop %d photo embedding(s): Qdrant is not configured, so there is nowhere to migrate them; start once with QDRANT_URL and QDRANT_COLLECTION set (plus QDRANT_API_KEY if the server requires it) to upload them", uploadable)
	}

	// Blobs whose stored vector does not fit the column were never queryable —
	// the v2 backfill skipped them for the same reason. They keep their
	// embedding_status='ready' rows after the column is gone and become
	// permanent "ready but never indexed" entries, which the v2 backfill set
	// the precedent for tolerating.
	var malformed int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM photos p JOIN blobs b ON b.hash = p.hash
		WHERE b.embedding IS NOT NULL AND length(b.embedding) != ?`,
		4*EmbeddingDim).Scan(&malformed); err != nil {
		return fmt.Errorf("count malformed embeddings: %w", err)
	}
	if malformed > 0 {
		log.Printf("catalog: skipping %d photo embedding(s) whose blob embedding is not %d bytes", malformed, 4*EmbeddingDim)
	}

	// Both drops run only after every uploadable vector made it to the sink.
	// The vec0 virtual table is dropped by name: the sqlite-vec module must be
	// registered on this connection for that to work, which is why the catalog
	// package keeps its blank vec import even though it no longer stores
	// vectors.
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS blob_embeddings`); err != nil {
		return fmt.Errorf("drop vector index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE blobs DROP COLUMN embedding`); err != nil {
		return fmt.Errorf("drop blob embedding column: %w", err)
	}
	return nil
}

// uploadVectors streams every photo-with-embedding pair to the sink in
// (album_id, idx) order, vectorUploadBatchSize rows at a time. Each batch is a
// fresh query whose cursor starts after the last row of the previous one, so
// the cursor never has to survive a network call — the transaction owns the
// pool's single connection, and nothing else runs before ListenAndServe, but
// holding a SQLite read open across an HTTP round trip would still be the
// wrong shape. The initial cursor (empty album, index 0) precedes every real
// row because album ids are never empty and photo indexes start at 0.
func uploadVectors(ctx context.Context, tx *sql.Tx, sink VectorSink, total int) (int, error) {
	var (
		uploaded  int
		batches   int
		lastAlbum string
		lastIdx   int
	)
	for {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.album_id, p.idx, p.hash, p.width, p.height, b.embedding
			FROM photos p JOIN blobs b ON b.hash = p.hash
			WHERE b.embedding IS NOT NULL AND length(b.embedding) = ?
			  AND (p.album_id > ? OR (p.album_id = ? AND p.idx > ?))
			ORDER BY p.album_id, p.idx
			LIMIT ?`,
			4*EmbeddingDim, lastAlbum, lastAlbum, lastIdx, vectorUploadBatchSize)
		if err != nil {
			return 0, fmt.Errorf("read embedding batch: %w", err)
		}
		records := make([]qdrant.PhotoRecord, 0, vectorUploadBatchSize)
		var batchAlbum string
		var batchIdx int
		for rows.Next() {
			var raw []byte
			var record qdrant.PhotoRecord
			if err := rows.Scan(&record.AlbumID, &record.Idx, &record.Hash, &record.W, &record.H, &raw); err != nil {
				rows.Close()
				return 0, fmt.Errorf("scan embedding batch row: %w", err)
			}
			record.Vector = decodeVectorLE(raw)
			batchAlbum = record.AlbumID
			batchIdx = record.Idx
			records = append(records, record)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, fmt.Errorf("iterate embedding batch: %w", err)
		}
		rows.Close()

		if len(records) == 0 {
			break
		}
		if err := sink.UpsertPhotos(ctx, records); err != nil {
			return 0, fmt.Errorf("upload embedding batch %d: %w", batches+1, err)
		}
		uploaded += len(records)
		batches++
		if batches%vectorUploadProgressEvery == 0 {
			log.Printf("catalog: vector upload progress %d/%d", uploaded, total)
		}
		lastAlbum = batchAlbum
		lastIdx = batchIdx
	}
	return uploaded, nil
}

// decodeVectorLE deserializes a little-endian float32 embedding, the wire
// format embeddings have always been stored and reported in.
func decodeVectorLE(raw []byte) []float32 {
	if len(raw) < 4 {
		return nil
	}
	count := len(raw) / 4
	vector := make([]float32, count)
	for i := 0; i < count; i++ {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return vector
}
