// Package admin aggregates the operational figures the admin page shows and
// implements the re-embed trigger behind them. It is deliberately thin: the
// catalog stays the source of truth for every count, and the recovery path is
// a single status flip that the existing embedding workers already know how to
// drain. The HTTP layer (internal/httpapi) calls only this package, never the
// catalog directly, mirroring how every other endpoint is wired.
package admin

import (
	"context"
	"errors"
	"fmt"

	"viewer/internal/catalog"
	"viewer/internal/recommend"
)

// VectorCounter is the read-only slice of the vector store the overview's
// health section needs. It is satisfied structurally by *qdrant.Client;
// keeping it an interface lets tests stand in a fake instead of a live Qdrant.
type VectorCounter interface {
	CountVectors(ctx context.Context) (int, error)
}

// LocalEncodingRunner is the slice of the encoding service the admin
// run-local trigger needs: start one background drain over the pending
// non-WebP blobs. *encoding.Service satisfies it structurally, and tests can
// stand in a fake — the same pattern VectorCounter uses.
type LocalEncodingRunner interface {
	StartLocalDrain() (bool, error)
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
	localEncoding   LocalEncodingRunner
}

// NewService builds the admin service. A nil vectors counter makes the
// overview's health section report the vector store as disabled rather than
// counting anything — which is how a deployment without Qdrant runs — and a
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

// WithLocalEncoding supplies the local encoding drain trigger. Like
// WithEncodingEnabled it is a builder wired before serving; a service built
// without one answers RunLocalEncoding with ErrLocalEncodingUnavailable,
// which is how a test (or a future deployment) without the encoding service
// gets an explicit 503 instead of a nil-pointer panic.
func (s *Service) WithLocalEncoding(runner LocalEncodingRunner) *Service {
	if s != nil {
		s.localEncoding = runner
	}
	return s
}

// ErrAutomaticEmbeddingUnavailable marks admin automatic-embedding requests
// made against a service with no recommendation service wired up — a shape
// only tests produce in practice, but one the HTTP layer must answer with a
// clean 503 rather than a 500.
var ErrAutomaticEmbeddingUnavailable = errors.New("automatic embedding control is unavailable")

// ErrLocalEncodingUnavailable marks the local encoding trigger when no
// encoding service is wired up. It is deliberately distinct from the drain's
// own errors (cwebp missing, service not started), which the encoding
// package already words for operators.
var ErrLocalEncodingUnavailable = errors.New("local encoding is unavailable")

// AutomaticEmbedding reports the recommendation service's pause switch and
// worker presence for the dashboard.
func (s *Service) AutomaticEmbedding() (recommend.AutomaticStatus, error) {
	if s == nil || s.recommend == nil {
		return recommend.AutomaticStatus{}, ErrAutomaticEmbeddingUnavailable
	}
	return s.recommend.AutomaticEmbedding(), nil
}

// SetAutomaticEmbedding flips the pause switch and returns the status the
// toggle produced, so the dashboard's next paint comes from the response
// itself rather than a second read that could race another toggle. The flip
// takes effect between batches; see recommend.Service.SetAutomaticEmbedding
// for why that is the whole story now that manual runs are gone.
func (s *Service) SetAutomaticEmbedding(enabled bool) (recommend.AutomaticStatus, error) {
	if s == nil || s.recommend == nil {
		return recommend.AutomaticStatus{}, ErrAutomaticEmbeddingUnavailable
	}
	s.recommend.SetAutomaticEmbedding(enabled)
	return s.recommend.AutomaticEmbedding(), nil
}

// RunLocalEncoding starts one background drain over every pending non-WebP
// blob, encoded locally with cwebp. started=false means a drain is already in
// flight — the caller surfaces that as information, not an error.
func (s *Service) RunLocalEncoding() (bool, error) {
	if s == nil || s.localEncoding == nil {
		return false, ErrLocalEncodingUnavailable
	}
	started, err := s.localEncoding.StartLocalDrain()
	if err != nil {
		// StartLocalDrain's failures are deployment states worded for
		// operators — cwebp missing from PATH, the service not started, an
		// unreadable queue — so the message is surfaced verbatim behind the
		// sentinel the HTTP layer maps to a 503.
		return false, fmt.Errorf("%w: %v", ErrLocalEncodingUnavailable, err)
	}
	return started, nil
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
// A non-empty hashes list narrows the release to those blobs, the scope the
// recover endpoint passes through; an empty one releases every matching row.
func (s *Service) ReleaseStuckEmbeddings(ctx context.Context, expiredOnly bool, hashes []string) (ReleaseResult, error) {
	if s == nil || s.cat == nil {
		return ReleaseResult{}, fmt.Errorf("catalog is not available")
	}
	released, err := s.cat.ReleaseStuckEmbeddingClaims(ctx, expiredOnly, hashes)
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

// RetryFailedEmbeddings flips failed embeddings back to pending and returns
// the fresh counts. It is the retry path for transient failures (a bad
// model checkpoint, an unavailable encoder); ready rows stay untouched, so no
// completed work is re-spent, and processing rows are left to their lease for
// the same reason Reindex leaves them alone. A non-empty hashes list narrows
// the retry to those blobs; an empty one retries every failed row.
func (s *Service) RetryFailedEmbeddings(ctx context.Context, hashes []string) (RetryFailedResult, error) {
	if s == nil || s.cat == nil {
		return RetryFailedResult{}, fmt.Errorf("catalog is not available")
	}
	reset, err := s.cat.ResetFailedEmbeddings(ctx, hashes)
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

// EncodingRetryResult reports how many failed rows RetryFailedEncodings moved
// back to pending, plus the fresh encoding stats — the same shape
// RetryFailedResult gives the embedding queue, so the recover endpoint's
// retry answers read alike across stages.
type EncodingRetryResult struct {
	Reset    int64         `json:"reset"`
	Encoding EncodingStats `json:"encoding"`
}

// RecoverResult is one recovery target's outcome as the dashboard receives it.
// Exactly one of the four pointers is set, matching the requested target.
// The action-specific shapes stay typed instead of collapsing into a single
// "rows moved" number: the encoding reset flips rows in two passes, and a
// flat count would hide which pass did the work.
type RecoverResult struct {
	Target        string               `json:"target"`
	Release       *ReleaseResult       `json:"release,omitempty"`
	Retry         *RetryFailedResult   `json:"retry,omitempty"`
	Encoding      *EncodingResetResult `json:"encoding,omitempty"`
	EncodingRetry *EncodingRetryResult `json:"encodingRetry,omitempty"`
}

// ResetStuckEncodings is the operator's recovery switch for the WebP queue:
// expired encoder leases and received-but-uncommitted results go back to
// pending, and the response carries the fresh encoding stats. Committing rows
// are deliberately never touched — a committing row means an S3 object
// replacement may be in flight, and only the encoding server's own Recover
// path holds the bookkeeping to reconcile that uncertainty; an operator reset
// here would turn "unknown outcome" into a certain loss. A non-empty hashes
// list narrows both passes to those blobs; an empty one sweeps the queue.
func (s *Service) ResetStuckEncodings(ctx context.Context, hashes []string) (EncodingResetResult, error) {
	if s == nil || s.cat == nil {
		return EncodingResetResult{}, fmt.Errorf("catalog is not available")
	}
	reset, err := s.cat.ResetStuckEncodings(ctx, hashes)
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

// RetryFailedEncodings flips failed WebP conversions back to pending and
// returns the fresh encoding stats. It is the encoding counterpart of
// RetryFailedEmbeddings and exists so the recover endpoint can serve the
// retry-failed shape for both pipeline stages; already-WebP rows are excluded
// by the catalog (there is nothing left to convert), and a non-empty hashes
// list narrows the retry to those blobs.
func (s *Service) RetryFailedEncodings(ctx context.Context, hashes []string) (EncodingRetryResult, error) {
	if s == nil || s.cat == nil {
		return EncodingRetryResult{}, fmt.Errorf("catalog is not available")
	}
	reset, err := s.cat.ResetFailedEncodings(ctx, hashes)
	if err != nil {
		return EncodingRetryResult{}, fmt.Errorf("reset failed encodings: %w", err)
	}
	encoding, err := s.cat.EncodingCounts(ctx)
	if err != nil {
		return EncodingRetryResult{}, fmt.Errorf("encoding counts after resetting %d rows: %w", reset, err)
	}
	return EncodingRetryResult{Reset: reset, Encoding: EncodingStats{Enabled: s.encodingEnabled, EncodingCounts: encoding}}, nil
}
