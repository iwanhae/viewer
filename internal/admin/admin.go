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
	Encoding       EncodingStats           `json:"encoding"`
	// The four fields below diagnose *why* the embedding counts above look
	// wrong, one layer down each:
	//   - EmbeddingLease reads the processing rows' leases: processing blobs
	//     whose lease has expired mean no worker is claiming anything, while
	//     live leases that never finish point at a stuck write-back.
	//   - PendingByEncoding splits the pending blobs by their WebP encoding
	//     state, because the claim gate requires terminal encoding — pending
	//     blobs behind an unfinished encoding are stalled by the WebP queue,
	//     not by a lack of embedding workers.
	//   - Failures groups the failed blobs by error text (top reasons only),
	//     turning "N failed" into "what to fix first".
	//   - WorkerActivity is the recommend service's last-attempt stamps at the
	//     pipeline entry points; zeros mean nobody has tried since process
	//     start. The recommend type is used directly — its JSON tags are the
	//     dashboard contract, and a re-declared copy in this package could
	//     silently drift from the stamps the service actually records.
	EmbeddingLease    catalog.EmbeddingLeaseHealth `json:"embeddingLease"`
	PendingByEncoding map[string]int64             `json:"pendingByEncoding"`
	Failures          []catalog.EmbeddingFailure   `json:"failures"`
	WorkerActivity    recommend.WorkerActivity     `json:"workerActivity"`
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

// EncodingStats combines queue and savings counts with the server-side
// feature flag. Enabled means WORKER_TOKEN is configured; it does not claim
// that an external worker process is currently connected.
type EncodingStats struct {
	Enabled bool `json:"enabled"`
	catalog.EncodingCounts
}

// Service backs the admin page. Everything it reports is read straight out of
// the catalog and the vector store on every request, so there is no state to
// go stale and nothing to refresh on deploy.
type Service struct {
	cat             *catalog.Store
	vectors         VectorCounter
	recommend       *recommend.Service
	encodingEnabled bool
}

// NewService builds the admin service. A nil vectors counter reports the
// store as disabled — VectorStoreEnabled false, with zero points and drift as
// unmeasured values, which is how a deployment without Qdrant runs — and a
// nil catalog makes every call fail, the same contract the recommend service
// applies.
func NewService(cat *catalog.Store, vectors VectorCounter, recommend *recommend.Service) *Service {
	return &Service{cat: cat, vectors: vectors, recommend: recommend}
}

// WithEncodingEnabled records whether the server has WORKER_TOKEN configured
// and therefore exposes the encoding worker API. Configure it before serving.
func (s *Service) WithEncodingEnabled(enabled bool) *Service {
	if s != nil {
		s.encodingEnabled = enabled
	}
	return s
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
	encoding, err := s.cat.EncodingCounts(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("encoding counts: %w", err)
	}
	lease, err := s.cat.EmbeddingLeaseHealth(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("embedding lease health: %w", err)
	}
	pendingByEncoding, err := s.cat.PendingEncodingBreakdown(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("pending encoding breakdown: %w", err)
	}
	// Five reasons: the dashboard renders a fixed handful of rows, and a
	// failure list longer than that is noise an operator works through one
	// fix at a time rather than a page to scroll. The store clamps the limit
	// regardless, so this is a rendering choice, not a safety one.
	failures, err := s.cat.EmbeddingFailureReasons(ctx, 5)
	if err != nil {
		return Stats{}, fmt.Errorf("embedding failure reasons: %w", err)
	}

	stats := Stats{
		Albums:            albums,
		AlbumsByStatus:    byStatus,
		Photos:            photos,
		Blobs:             blobs,
		Embedding:         embedding,
		Encoding:          EncodingStats{Enabled: s.encodingEnabled, EncodingCounts: encoding},
		EmbeddingLease:    lease,
		PendingByEncoding: pendingByEncoding,
		Failures:          failures,
		ExpectedPoints:    expected,
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
	// The recommend service is nil in setups without an embedding model; its
	// WorkerActivity is nil-safe and would report zeros anyway, but the guard
	// keeps that contract visible here — zeros mean "no attempts recorded",
	// never "the model answered instantly".
	if s.recommend != nil {
		stats.WorkerActivity = s.recommend.WorkerActivity()
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

// ReleaseResult reports how many processing rows ReleaseStuckEmbeddings handed
// back to the pending queue, plus the fresh embedding counts so the caller
// sees the effect without a second stats round-trip (the same pattern Reindex
// uses).
type ReleaseResult struct {
	Released int64                   `json:"released"`
	Counts   catalog.EmbeddingCounts `json:"counts"`
}

// ReleaseStuckEmbeddings returns processing embeddings to the pending queue so
// a worker can claim them again. With expiredOnly the damage is bounded: only
// rows whose lease deadline has already passed move, which is the shape of a
// dead-worker recovery. With expiredOnly false every processing row moves,
// including live leases — that is the operator's explicit choice (the UI makes
// them confirm it), because it means a worker is still working: its write-back
// will be rejected as not_claimed and the blob re-embedded. That is harmless
// but wasteful, which is why the default is the narrow variant. Encoding rows
// are never touched here; that queue has its own reset (ResetStuckEncodings).
func (s *Service) ReleaseStuckEmbeddings(ctx context.Context, expiredOnly bool) (ReleaseResult, error) {
	if s == nil || s.cat == nil {
		return ReleaseResult{}, fmt.Errorf("catalog is not available")
	}
	released, err := s.cat.ReleaseStuckEmbeddingClaims(ctx, expiredOnly)
	if err != nil {
		return ReleaseResult{}, fmt.Errorf("release stuck embedding claims: %w", err)
	}
	counts, err := s.cat.EmbeddingCounts(ctx)
	if err != nil {
		return ReleaseResult{}, fmt.Errorf("embedding counts after releasing %d claims: %w", released, err)
	}
	return ReleaseResult{Released: released, Counts: counts}, nil
}

// RetryFailedResult reports how many failed rows RetryFailedEmbeddings moved
// back to pending, plus the fresh embedding counts.
type RetryFailedResult struct {
	Reset  int64                   `json:"reset"`
	Counts catalog.EmbeddingCounts `json:"counts"`
}

// RetryFailedEmbeddings flips every failed embedding back to pending and
// returns the fresh counts. It is the retry path for transient failures (a bad
// model checkpoint, an unavailable encoder); ready rows stay untouched, so no
// completed work is re-spent, and processing rows are left to their lease for
// the same reason Reindex leaves them alone.
func (s *Service) RetryFailedEmbeddings(ctx context.Context) (RetryFailedResult, error) {
	if s == nil || s.cat == nil {
		return RetryFailedResult{}, fmt.Errorf("catalog is not available")
	}
	reset, err := s.cat.ResetFailedEmbeddings(ctx)
	if err != nil {
		return RetryFailedResult{}, fmt.Errorf("reset failed embeddings: %w", err)
	}
	counts, err := s.cat.EmbeddingCounts(ctx)
	if err != nil {
		return RetryFailedResult{}, fmt.Errorf("embedding counts after resetting %d blobs: %w", reset, err)
	}
	return RetryFailedResult{Reset: reset, Counts: counts}, nil
}

// EncodingResetResult reports what ResetStuckEncodings' two passes moved and
// the fresh encoding stats, so the operator sees the queue they just drained
// in the same response.
type EncodingResetResult struct {
	Counts   catalog.EncodingResetCounts `json:"counts"`
	Encoding EncodingStats               `json:"encoding"`
}

// ResetStuckEncodings is the operator's recovery switch for the WebP queue:
// expired encoder leases and received-but-uncommitted results go back to
// pending, and the response carries the fresh encoding stats. Committing rows
// are deliberately never touched — a committing row means an S3 object
// replacement may be in flight, and only the encoding server's own Recover
// path holds the bookkeeping to reconcile that uncertainty; an operator reset
// here would turn "unknown outcome" into a certain loss.
func (s *Service) ResetStuckEncodings(ctx context.Context) (EncodingResetResult, error) {
	if s == nil || s.cat == nil {
		return EncodingResetResult{}, fmt.Errorf("catalog is not available")
	}
	reset, err := s.cat.ResetStuckEncodings(ctx)
	if err != nil {
		return EncodingResetResult{}, fmt.Errorf("reset stuck encodings: %w", err)
	}
	encoding, err := s.cat.EncodingCounts(ctx)
	if err != nil {
		return EncodingResetResult{}, fmt.Errorf("encoding counts after resetting %d+%d rows: %w", reset.ReleasedExpiredLeased, reset.ResetReceived, err)
	}
	return EncodingResetResult{
		Counts:   reset,
		Encoding: EncodingStats{Enabled: s.encodingEnabled, EncodingCounts: encoding},
	}, nil
}
