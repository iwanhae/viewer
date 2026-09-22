package catalog

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// seedBenchmarkCatalog fills a store with a production-sized catalog
// (~273k photos spread over ~300 ready albums) for the feed benchmarks.
func seedBenchmarkCatalog(b *testing.B) *Store {
	b.Helper()
	ctx := context.Background()
	store, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("open catalog: %v", err)
	}
	b.Cleanup(func() { store.Close() })

	const albums = 300
	const photos = 273358

	if err := store.UpsertBlob(ctx, Blob{Hash: "hash", SizeBytes: 5}); err != nil {
		b.Fatalf("upsert blob: %v", err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatalf("begin tx: %v", err)
	}
	albumStmt, err := tx.PrepareContext(ctx, `INSERT INTO albums (id, original_filename, size_bytes, status, photo_count, created_at, updated_at) VALUES (?, ?, 0, 'SUCCEEDED', ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	if err != nil {
		b.Fatalf("prepare album stmt: %v", err)
	}
	for i := 0; i < albums; i++ {
		if _, err := albumStmt.ExecContext(ctx, fmt.Sprintf("album-%04d", i), fmt.Sprintf("album-%04d.zip", i), photos/albums); err != nil {
			b.Fatalf("insert album: %v", err)
		}
	}
	photoStmt, err := tx.PrepareContext(ctx, `INSERT INTO photos (album_id, idx, name, hash, width, height, ratio) VALUES (?, ?, ?, ?, 100, 80, 1.25)`)
	if err != nil {
		b.Fatalf("prepare photo stmt: %v", err)
	}
	for i := 0; i < photos; i++ {
		albumID := fmt.Sprintf("album-%04d", i%albums)
		if _, err := photoStmt.ExecContext(ctx, albumID, i/albums, fmt.Sprintf("p-%d.jpg", i), "hash"); err != nil {
			b.Fatalf("insert photo: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}
	return store
}

// BenchmarkRandomFeedQueries compares the two random-feed strategies against a
// production-sized catalog: the full photo scan the snapshot path paid versus
// the album-count scan plus 80 point lookups the sampler does now.
func BenchmarkRandomFeedQueries(b *testing.B) {
	store := seedBenchmarkCatalog(b)
	ctx := context.Background()

	b.Run("all-photos-scan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			photos, err := store.ListReadyAlbumPhotos(ctx)
			if err != nil {
				b.Fatalf("list ready album photos: %v", err)
			}
			if len(photos) != 273358 {
				b.Fatalf("unexpected photo count %d", len(photos))
			}
		}
	})

	b.Run("counts-plus-80-lookups", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			counts, err := store.ListReadyAlbumPhotoCounts(ctx)
			if err != nil {
				b.Fatalf("list ready album photo counts: %v", err)
			}
			for j := 0; j < 80; j++ {
				count := counts[j%len(counts)]
				if _, err := store.PhotoAt(ctx, count.AlbumID, j%count.PhotoCount); err != nil {
					b.Fatalf("photo at: %v", err)
				}
			}
		}
	})
}
