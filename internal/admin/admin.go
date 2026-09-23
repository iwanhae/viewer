// Package admin aggregates the operational figures the admin page shows and
// implements the re-embed trigger behind them. It is deliberately thin: the
// catalog stays the source of truth for every count, and the recovery path is
// a single status flip that the existing embedding workers already know how to
// drain. The HTTP layer (internal/httpapi) calls only this package, never the
// catalog directly, mirroring how every other endpoint is wired.
package admin

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"

	"viewer/internal/catalog"
	"viewer/internal/recommend"
)

//go:embed static/admin.html
var pageHTML []byte

// Page serves the embedded admin dashboard. It is one self-contained HTML
// file — inline CSS and JavaScript, no build step — so the admin surface adds
// nothing to the frontend toolchain and keeps working even when the SPA bundle
// is stale or broken. no-store keeps a cached page from polling an API whose
// response shape has moved on.
func Page() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(pageHTML)
	})
}

// VectorCounter is the read-only slice of the vector store the stats need.
// It is satisfied structurally by *qdrant.Client; keeping it an interface lets
// tests stand in a fake instead of a live Qdrant.
type VectorCounter interface {
	CountVectors(ctx context.Context) (int, error)
}

// Stats is one snapshot of the whole system for the admin page. The embedding
// counts are per blob — the unit the pipeline leases and tracks — while
// ExpectedPoints and QdrantPoints are per photo, the unit the vector store
// holds; the two units are never added together, only Drift compares them.
type Stats struct {
	Albums         int64                   `json:"albums"`
	AlbumsByStatus map[string]int64        `json:"albumsByStatus"`
	Photos         int64                   `json:"photos"`
	Blobs          int64                   `json:"blobs"`
	Embedding      catalog.EmbeddingCounts `json:"embedding"`
	// ExpectedPoints is how many points the vector store should hold: one per
	// photo whose blob embedding is ready. QdrantPoints is how many it
	// actually holds, and Drift is the difference — non-zero means the two
	// stores disagree, most commonly because the collection was wiped (or a
	// catalog backup was restored under it). Drift answers "which way" by its
	// sign: negative is missing points, positive is stale ones.
	ExpectedPoints int64 `json:"expectedPoints"`
	QdrantPoints   int64 `json:"qdrantPoints"`
	Drift          int64 `json:"drift"`
	// VectorStoreEnabled says a vector store is wired up at all. When it is
	// false, the point figures above are unmeasured zeros — not readings of
	// an empty store — and the dashboard must render them as disabled rather
	// than as a green "in sync".
	VectorStoreEnabled bool `json:"vectorStoreEnabled"`
	ModelEnabled       bool `json:"modelEnabled"`
}

// Service backs the admin page. Everything it reports is read straight out of
// the catalog and the vector store on every request, so there is no state to
// go stale and nothing to refresh on deploy.
type Service struct {
	cat       *catalog.Store
	vectors   VectorCounter
	recommend *recommend.Service
}

// NewService builds the admin service. A nil vectors counter reports the
// store as disabled — VectorStoreEnabled false, with zero points and drift as
// unmeasured values, which is how a deployment without Qdrant runs — and a
// nil catalog makes every call fail, the same contract the recommend service
// applies.
func NewService(cat *catalog.Store, vectors VectorCounter, recommend *recommend.Service) *Service {
	return &Service{cat: cat, vectors: vectors, recommend: recommend}
}

// Stats gathers one snapshot across the catalog and the vector store. It
// issues several independent reads without a transaction on purpose: the page
// is a dashboard, not a consistency check, and the counts are seconds apart at
// worst on a quiet catalog.
func (s *Service) Stats(ctx context.Context) (Stats, error) {
	if s == nil || s.cat == nil {
		return Stats{}, fmt.Errorf("catalog is not available")
	}

	albums, byStatus, err := s.cat.AlbumCounts(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("album counts: %w", err)
	}
	photos, err := s.cat.PhotoCount(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("photo count: %w", err)
	}
	blobs, err := s.cat.BlobCount(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("blob count: %w", err)
	}
	embedding, err := s.cat.EmbeddingCounts(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("embedding counts: %w", err)
	}
	expected, err := s.cat.ReadyPairCount(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("ready pair count: %w", err)
	}

	stats := Stats{
		Albums:         albums,
		AlbumsByStatus: byStatus,
		Photos:         photos,
		Blobs:          blobs,
		Embedding:      embedding,
		ExpectedPoints: expected,
	}
	if s.vectors != nil {
		points, err := s.vectors.CountVectors(ctx)
		if err != nil {
			return Stats{}, fmt.Errorf("count vector store points: %w", err)
		}
		stats.QdrantPoints = int64(points)
		// Drift is only meaningful against a real reading. Computed with no
		// store wired up it would always read -expected and render as a red
		// "missing everything" that is really just "Qdrant is off".
		stats.Drift = stats.QdrantPoints - stats.ExpectedPoints
		stats.VectorStoreEnabled = true
	}
	stats.ModelEnabled = s.recommend != nil && s.recommend.Enabled()
	return stats, nil
}

// Reindex flips every ready or failed blob back to pending and returns the
// fresh embedding counts, so the caller sees the pending surge immediately
// instead of having to poll stats to confirm the trigger landed. The workers
// re-embed from here on their own: the 500ms claim loop picks the reset blobs
// up without a restart, which is why this does no embedding work itself.
func (s *Service) Reindex(ctx context.Context) (catalog.EmbeddingCounts, error) {
	if s == nil || s.cat == nil {
		return catalog.EmbeddingCounts{}, fmt.Errorf("catalog is not available")
	}
	reset, err := s.cat.ResetReadyEmbeddings(ctx)
	if err != nil {
		return catalog.EmbeddingCounts{}, fmt.Errorf("reset ready embeddings: %w", err)
	}
	counts, err := s.cat.EmbeddingCounts(ctx)
	if err != nil {
		return catalog.EmbeddingCounts{}, fmt.Errorf("embedding counts after resetting %d blobs: %w", reset, err)
	}
	return counts, nil
}
