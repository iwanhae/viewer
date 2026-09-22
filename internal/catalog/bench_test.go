package catalog

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"testing"
)

// benchVector builds a deterministic pseudo-random direction from a seed, so
// every seeded blob ranks differently without needing real embeddings.
func benchVector(seed uint32) []float32 {
	vec := make([]float32, EmbeddingDim)
	rng := seed
	for i := range vec {
		rng = rng*1664525 + 1013904223
		vec[i] = float32(int32(rng>>9&0xFFFF)-0x8000) / float32(0x8000)
	}
	return vec
}

// benchSeedVectors fills the store with n ready blobs and indexes their
// embeddings, mirroring what the production write path produces. It runs inside
// one transaction because SQLite only becomes slow at this volume when every
// insert commits on its own.
func benchSeedVectors(b *testing.B, store *Store, n int) {
	b.Helper()

	tx, err := store.db.Begin()
	if err != nil {
		b.Fatalf("begin seed: %v", err)
	}
	blobs, err := tx.Prepare(`
		INSERT INTO blobs (hash, size_bytes, embedding_status, embedding, created_at)
		VALUES (?, 1, 'ready', ?, '2024-01-01T00:00:00Z')`)
	if err != nil {
		b.Fatalf("prepare blob insert: %v", err)
	}
	defer blobs.Close()
	index, err := tx.Prepare(`INSERT INTO blob_embeddings (hash, embedding) VALUES (?, ?)`)
	if err != nil {
		b.Fatalf("prepare index insert: %v", err)
	}
	defer index.Close()

	for i := 0; i < n; i++ {
		vec := benchVector(uint32(i) + 1)
		hash := fmt.Sprintf("bench-%08d", i)
		if _, err := blobs.Exec(hash, EncodeVector(vec)); err != nil {
			b.Fatalf("seed blob %s: %v", hash, err)
		}
		if _, err := index.Exec(hash, EncodeVector(vec)); err != nil {
			b.Fatalf("seed index %s: %v", hash, err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit seed: %v", err)
	}
}

func openBenchStore(b *testing.B) *Store {
	b.Helper()
	store, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("open catalog: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

// BenchmarkFindNeighborEmbeddings measures the production query: the vec0
// index scans everything and Go sorts what came back.
func BenchmarkFindNeighborEmbeddings(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			store := openBenchStore(b)
			benchSeedVectors(b, store, n)
			query := benchVector(0)
			ctx := context.Background()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := store.FindNeighborEmbeddings(ctx, query, MaxNeighborK); err != nil {
					b.Fatalf("find neighbors: %v", err)
				}
			}
		})
	}
}

// BenchmarkLegacyBruteForceScan reproduces the in-memory scan the recommend
// package used before the vec0 index: LoadAll decoded every blob embedding
// into a Go heap-resident slice, and each query normalized once and took the
// dot product against all of it, then sorted. Its setup cost is the point —
// that work ran on every boot and stayed resident forever.
func BenchmarkLegacyBruteForceScan(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			store := openBenchStore(b)
			benchSeedVectors(b, store, n)

			// The old LoadAll: decode every embedding out of the catalog.
			b.StopTimer()
			rows, err := store.db.Query(`SELECT hash, embedding FROM blobs WHERE embedding_status = 'ready'`)
			if err != nil {
				b.Fatalf("load embeddings: %v", err)
			}
			hashes := make([]string, 0, n)
			vectors := make([][]float32, 0, n)
			for rows.Next() {
				var hash string
				var raw []byte
				if err := rows.Scan(&hash, &raw); err != nil {
					b.Fatalf("scan embedding: %v", err)
				}
				vec := DecodeVector(raw)
				// The old index stored unit vectors, paying the normalize once
				// at load.
				var norm float64
				for _, v := range vec {
					norm += float64(v) * float64(v)
				}
				inv := 1 / float32(math.Sqrt(norm))
				for i := range vec {
					vec[i] *= inv
				}
				hashes = append(hashes, hash)
				vectors = append(vectors, vec)
			}
			if err := rows.Err(); err != nil {
				b.Fatalf("iterate embeddings: %v", err)
			}
			rows.Close()

			query := benchVector(0)
			var qnorm float64
			for _, v := range query {
				qnorm += float64(v) * float64(v)
			}
			qinv := 1 / float32(math.Sqrt(qnorm))
			normalized := make([]float32, len(query))
			for i, v := range query {
				normalized[i] = v * qinv
			}
			type neighbor struct {
				hash  string
				score float32
			}
			b.StartTimer()

			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				neighbors := make([]neighbor, 0, len(vectors))
				for j, vec := range vectors {
					var dot float32
					for d := range vec {
						dot += vec[d] * normalized[d]
					}
					neighbors = append(neighbors, neighbor{hash: hashes[j], score: dot})
				}
				sort.Slice(neighbors, func(x, y int) bool {
					if neighbors[x].score != neighbors[y].score {
						return neighbors[x].score > neighbors[y].score
					}
					return neighbors[x].hash < neighbors[y].hash
				})
				if MaxNeighborK < len(neighbors) {
					neighbors = neighbors[:MaxNeighborK]
				}
			}
		})
	}
}
