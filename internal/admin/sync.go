package admin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/qdrant"
)

// VectorSyncStore is the slice of the vector store the reconciliation
// needs: every point ID currently stored. Satisfied structurally by
// *qdrant.Client; an interface so tests stand in a fake.
type VectorSyncStore interface {
	ScrollPointIDs(ctx context.Context) ([]string, error)
}

// ErrVectorSyncUnavailable marks sync requests made against a service with
// no vector store wired up — the HTTP layer answers 503, the same contract
// the other local-control sentinels use.
var ErrVectorSyncUnavailable = errors.New("vector sync is unavailable")

// vectorSyncTimeout caps one reconciliation pass. The pass normally takes
// tens of seconds (two full scans plus a scoped reset), so the cap is not
// meant to be reachable by a healthy pair — it bounds a wedged store so a
// stuck scroll cannot pin the single-flight lock forever.
const vectorSyncTimeout = 5 * time.Minute

// expectedPoint is one photo's reconciliation input: the blob hash to reset
// when the point turns out missing, and the embedding status that decides
// whether a point is expected to exist at all yet.
type expectedPoint struct {
	hash   string
	status catalog.EmbeddingStatus
}

// VectorSyncResult reports what one reconciliation pass found and did.
// Started=false means another sync was already running and this call did
// no work — a normal double-click outcome, not an error.
type VectorSyncResult struct {
	Started            bool                    `json:"started"`
	MissingPoints      int                     `json:"missingPoints"`      // ready photos with no stored point
	ResetBlobs         int64                   `json:"resetBlobs"`         // blobs flipped ready→pending for them
	SkippedProcessing  int                     `json:"skippedProcessing"`  // missing photos whose blob is mid-embed; the lease owns them
	FailedWithoutPoint int                     `json:"failedWithoutPoint"` // informational: failed blobs lack points too
	OrphanPoints       int                     `json:"orphanPoints"`       // stored points with no catalog photo — reported only, never deleted here
	DurationMs         int64                   `json:"durationMs"`
	Counts             catalog.EmbeddingCounts `json:"counts"`
}

// WithVectorSync supplies the vector-store reconciliation trigger. Like
// WithLocalEncoding it is a builder wired before serving; a service built
// without one answers SyncVectors with ErrVectorSyncUnavailable, which is
// how a deployment without Qdrant gets an explicit 503 instead of a
// nil-pointer panic. The ctx is the service lifetime the sync's work
// context derives from, so a client disconnect never cancels a
// half-finished diff while server shutdown still can — the encoding-drain
// pattern.
func (s *Service) WithVectorSync(ctx context.Context, store VectorSyncStore) *Service {
	if s != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		s.syncCtx = ctx
		s.vectorSync = store
	}
	return s
}

// SyncVectors reconciles the vector store against the catalog: it walks
// every photo/blob pair, derives the point ID each photo owns, scrolls the
// store's actual point IDs, and flips exactly the blobs whose ready points
// are missing back to pending — the embedding workers then re-create the
// points on their own, which is why this does no vector work itself.
// Missing points behind processing blobs are skipped (a worker holds the
// lease and writes the point itself), failed blobs are counted
// informationally (the retry path owns them), pending blobs are not
// expected to have a point yet, and orphan points — stored IDs no catalog
// photo owns — are reported, never deleted: deciding a deletion safely
// needs the album-reload bookkeeping, which belongs to a different action.
// The pass is manual-trigger-only and single-flight: a call that arrives
// while one is running returns Started=false instead of queueing.
//
// The diff takes tens of seconds, so the work context derives from the
// service lifetime WithVectorSync captured, deliberately NOT from ctx: a
// client disconnect or a proxy timeout must not cancel the repair
// mid-pass (results are always logged server-side), while server shutdown
// still cancels between pages, and the 5-minute cap bounds a wedged store.
// ctx itself is used for nothing here; it stays in the signature so the
// handler reads like every other admin action.
func (s *Service) SyncVectors(ctx context.Context) (VectorSyncResult, error) {
	if s == nil || s.cat == nil {
		return VectorSyncResult{}, fmt.Errorf("catalog is not available")
	}
	if s.vectorSync == nil {
		return VectorSyncResult{}, ErrVectorSyncUnavailable
	}
	if !s.syncMu.TryLock() {
		return VectorSyncResult{Started: false}, nil
	}
	defer s.syncMu.Unlock()

	start := time.Now()
	workCtx, cancel := context.WithTimeout(s.syncCtx, vectorSyncTimeout)
	defer cancel()

	// One map carries the whole expected side on purpose (~312k entries at
	// production size): the orphans are simply the actual IDs it does not
	// key, and the per-status verdicts read the status field this single
	// walk already collected.
	expected := make(map[string]expectedPoint, 1<<18)
	if err := s.cat.ForEachPhotoPair(workCtx, func(pair catalog.PhotoPairState) error {
		expected[qdrant.PointID(pair.AlbumID, pair.Idx)] = expectedPoint{hash: pair.Hash, status: pair.EmbeddingStatus}
		return nil
	}); err != nil {
		return VectorSyncResult{}, fmt.Errorf("scan photo pairs: %w", err)
	}

	ids, err := s.vectorSync.ScrollPointIDs(workCtx)
	if err != nil {
		return VectorSyncResult{}, fmt.Errorf("scroll point IDs: %w", err)
	}
	actual := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		actual[id] = struct{}{}
	}

	var result VectorSyncResult
	missingHashes := make(map[string]struct{})
	for id, pair := range expected {
		if _, ok := actual[id]; ok {
			// A point existing while its blob is not ready yet is the
			// normal upload-before-flip window; present is present.
			continue
		}
		switch pair.status {
		case catalog.EmbeddingStatusReady:
			missingHashes[pair.hash] = struct{}{}
			result.MissingPoints++
		case catalog.EmbeddingStatusProcessing:
			result.SkippedProcessing++
		case catalog.EmbeddingStatusFailed:
			result.FailedWithoutPoint++
		case catalog.EmbeddingStatusPending:
			// Not expected to have a point yet.
		}
	}
	for id := range actual {
		if _, ok := expected[id]; !ok {
			result.OrphanPoints++
		}
	}

	if len(missingHashes) > 0 {
		hashes := make([]string, 0, len(missingHashes))
		for hash := range missingHashes {
			hashes = append(hashes, hash)
		}
		// Sorted for determinism: the same diff resets the same rows in
		// the same order on every run.
		sort.Strings(hashes)
		reset, err := s.cat.ResetReadyEmbeddingsByHashes(workCtx, hashes)
		if err != nil {
			return VectorSyncResult{}, fmt.Errorf("reset missing embeddings: %w", err)
		}
		result.ResetBlobs = reset
	}

	counts, countsErr := s.cat.EmbeddingCounts(workCtx)
	if countsErr != nil {
		countsErr = fmt.Errorf("embedding counts after syncing: %w", countsErr)
	} else {
		result.Counts = counts
	}
	result.Started = true
	result.DurationMs = time.Since(start).Milliseconds()
	// The definitive server-side record: the HTTP client may have hung up
	// long before this line, so the log — not the response — is what the
	// operator can always rely on. Logged even when the fresh counts read
	// failed, because the reset itself already landed.
	log.Printf(
		"admin: vector sync started=%t missingPoints=%d resetBlobs=%d skippedProcessing=%d failedWithoutPoint=%d orphanPoints=%d in %s",
		result.Started, result.MissingPoints, result.ResetBlobs, result.SkippedProcessing, result.FailedWithoutPoint, result.OrphanPoints,
		time.Since(start).Round(time.Millisecond),
	)
	if countsErr != nil {
		return VectorSyncResult{}, countsErr
	}
	return result, nil
}
