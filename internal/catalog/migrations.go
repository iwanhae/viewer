package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// EmbeddingDim is the vector length every ready embedding must have. The
// schema owns this number — it is baked into the blob_embeddings vec0 table's
// column definition — so it lives in the catalog, and recommend plus the
// worker API wire contract mirror it. Moving to a differently-sized model
// means a new migration that rebuilds the index and re-embeds the catalog.
const EmbeddingDim = 768

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
}

// addBlobLeaseColumnIfMissing adds the external-worker lease column to blobs.
// It keeps the pragma probe the inline version used instead of matching driver
// error strings: databases that already got the column from an earlier release
// must skip the ALTER, not fail on it.
func addBlobLeaseColumnIfMissing(ctx context.Context, tx *sql.Tx) error {
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
func backfillBlobEmbeddings(ctx context.Context, tx *sql.Tx) error {
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
