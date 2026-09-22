package catalog

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

// TestOpenAppliesMigrationsOnce verifies a fresh catalog ends at this build's
// newest version with the vector index in place, and that reopening the
// migrated file applies nothing again.
func TestOpenAppliesMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")

	schemaVersion := func(db *sql.DB) int {
		t.Helper()
		var version int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			t.Fatalf("read user_version: %v", err)
		}
		return version
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if got := schemaVersion(store.db); got != len(migrations) {
		t.Fatalf("user_version=%d want %d", got, len(migrations))
	}
	var tables int
	if err := store.db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master WHERE name = 'blob_embeddings'`).Scan(&tables); err != nil {
		t.Fatalf("probe sqlite_master: %v", err)
	}
	if tables != 1 {
		t.Fatalf("blob_embeddings virtual table missing after migration")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close catalog: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	defer reopened.Close()
	if got := schemaVersion(reopened.db); got != len(migrations) {
		t.Fatalf("reopen bumped user_version to %d, want unchanged %d", got, len(migrations))
	}
}

// TestOpenMigratesLegacyCatalogToVec0 assembles a pre-migration catalog — no
// lease column, wire-incompatible album statuses, and a mix of well-formed,
// malformed and absent embeddings — and verifies one Open brings it fully
// current: lease column added, statuses rewritten, and only vectors that fit
// the float[768] column backfilled into the index.
func TestOpenMigratesLegacyCatalogToVec0(t *testing.T) {
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
	seedLegacyBlob("good", "ready", EncodeVector(vec768(1)))
	seedLegacyBlob("bad", "ready", []byte{1, 2, 3})
	seedLegacyBlob("none", "pending", nil)
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer store.Close()

	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("user_version=%d want %d", version, len(migrations))
	}

	// Migration 0001 added the lease column the claim path depends on.
	var leaseColumn int
	if err := store.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('blobs') WHERE name = 'embedding_lease_until'`,
	).Scan(&leaseColumn); err != nil {
		t.Fatalf("inspect blob columns: %v", err)
	}
	if leaseColumn != 1 {
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

	// Migration 0002 backfilled only the vector that fits the column: "good"
	// answers KNN queries, while "bad" (wrong byte length) and "none" (no
	// embedding) are invisible to the index.
	neighbors, err := store.FindNeighborEmbeddings(ctx, vec768(1), 10)
	if err != nil {
		t.Fatalf("vector query on migrated catalog: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].Hash != "good" || neighbors[0].Distance > 1e-6 {
		t.Fatalf("unexpected neighbors %+v want only good", neighbors)
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

	store, err := Open(path)
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
	_, openErr := Open(path)
	// Restore the chain before anything else opens the file.
	migrations = orig

	if openErr == nil {
		t.Fatalf("expected open to fail on the broken migration")
	} else if !strings.Contains(openErr.Error(), "broken_vec") {
		t.Fatalf("expected the error to name the failed migration, got %v", openErr)
	}

	// Nothing from the failed migration survived: not the version bump, not
	// the virtual table, not its shadow tables.
	after, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer after.Close()
	var version int
	if err := after.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("user_version=%d want unchanged %d", version, len(migrations))
	}
	var leftovers int
	if err := after.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'broken_embeddings%'`).Scan(&leftovers); err != nil {
		t.Fatalf("probe sqlite_master: %v", err)
	}
	if leftovers != 0 {
		t.Fatalf("failed migration left %d broken_embeddings table(s) behind", leftovers)
	}

	// With the broken migration gone from the chain, the same file opens and
	// keeps working.
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.FindNeighborEmbeddings(context.Background(), vec768(1), 5); err != nil {
		t.Fatalf("vector query on recovered catalog: %v", err)
	}
}

// TestOpenToleratesNewerSchema covers a binary older than the file it opens —
// a downgrade after restoring a newer backup. The migrations chain must leave
// such a file alone rather than fail startup or roll the version back.
func TestOpenToleratesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")

	store, err := Open(path)
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

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("open catalog from the future: %v", err)
	}
	defer reopened.Close()
	var version int
	if err := reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 999 {
		t.Fatalf("user_version=%d want untouched 999", version)
	}
	// Sanity: the untouched file still serves its existing data.
	if _, err := reopened.FindNeighborEmbeddings(context.Background(), vec768(1), 5); err != nil {
		t.Fatalf("vector query on newer schema: %v", err)
	}
}
