package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// migration is one forward-only step in the catalog schema's history. stmts
// run first, each through its own Exec so a failure names the exact statement;
// fn (optional) then moves data with full Go at its disposal. Everything —
// including the user_version bump — commits or rolls back as one transaction.
type migration struct {
	version int
	name    string
	stmts   []string
	fn      func(ctx context.Context, tx *sql.Tx) error
}

// runMigrations brings an opened catalog up to the newest schema this build
// knows, applying every migration newer than the file's user_version exactly
// once. It runs inside catalog.Open, so a failure aborts startup rather than
// serving from a half-migrated state.
//
// A file whose user_version is newer than this build's chain — a binary
// downgrade after a restore from a newer backup — is left untouched: schema
// changes so far have been additive, so the older binary's named queries keep
// working.
func runMigrations(db *sql.DB) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		log.Printf("catalog: schema version %d is newer than this build knows (%d migrations); leaving it alone", current, len(migrations))
		return nil
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
		log.Printf("catalog: applied migration %04d_%s", m.version, m.name)
	}
	return nil
}

func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range m.stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if m.fn != nil {
		if err := m.fn(context.Background(), tx); err != nil {
			return err
		}
	}
	// PRAGMA values cannot be bound parameters, but the version comes from our
	// own chain, never from input. The pragma is a database-header write, so
	// it commits and rolls back together with the migration's other work.
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return tx.Commit()
}
