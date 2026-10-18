package recommend

import (
	"context"
	"errors"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/runs"
)

// EmbeddingExecutor adapts the existing image/inference/finalization pipeline to
// finite durable runs. It owns no poller and never claims queue-wide work.
//
// Register only after model load; Manager suspends and joins RunAutomatic before
// taking a manual snapshot. Both paths share the model, never its lifetime.
type EmbeddingExecutor struct {
	service *Service
}

func NewEmbeddingExecutor(service *Service) *EmbeddingExecutor {
	return &EmbeddingExecutor{service: service}
}

var _ runs.Executor = (*EmbeddingExecutor)(nil)

func (e *EmbeddingExecutor) Execute(ctx context.Context, run catalog.JobRun, item catalog.JobRunItem) (outcome runs.Outcome, err error) {
	if ctx.Err() != nil {
		return runs.Outcome{Status: "cancelled"}, nil
	}
	if e == nil || e.service == nil || run.Stage != "embedding" || item.Hash == "" || item.ClaimToken == "" {
		return runs.Outcome{Status: "failed", ErrorCode: "executor_failed"}, nil
	}
	s := e.service
	if s.catalog == nil || s.images == nil || !s.Enabled() {
		// Deployment/model failures must not turn pending blobs into failures.
		return runs.Outcome{Status: "failed", ErrorCode: "executor_failed"}, nil
	}
	noteActivity(&s.lastClaimMs)
	claim, status, claimErr := s.catalog.ClaimLocalEmbedding(ctx, item.Hash, item.ClaimToken, time.Now().Add(DefaultLeaseTTL))
	if claimErr != nil {
		if ctx.Err() != nil {
			return runs.Outcome{Status: "cancelled"}, nil
		}
		return runs.Outcome{Status: "failed", ErrorCode: "executor_failed"}, nil
	}
	switch status {
	case catalog.LocalEmbeddingBusy:
		return runs.Outcome{Status: "busy", ErrorCode: "target_busy"}, nil
	case catalog.LocalEmbeddingMissing:
		return runs.Outcome{Status: "skipped", ErrorCode: "target_missing"}, nil
	}
	defer func() {
		if recover() != nil {
			outcome = runs.Outcome{Status: "failed", ErrorCode: "executor_panic"}
		}
		// Also release on transient failure, validation rejection, or panic.
		// This exact token cannot release a newer owner or a reserved write.
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, releaseErr := s.ReleaseLocalClaims(cleanup, []catalog.LocalEmbeddingClaim{claim}); releaseErr != nil && outcome.Status != "succeeded" {
			outcome = runs.Outcome{Status: "failed", ErrorCode: "executor_failed"}
		}
	}()
	if ctx.Err() != nil {
		return runs.Outcome{Status: "cancelled"}, nil
	}
	data, loadErr := s.images.GetImageBytes(ctx, claim.Hash)
	if ctx.Err() != nil {
		return runs.Outcome{Status: "cancelled"}, nil
	}
	result := catalog.EmbeddingResult{Hash: claim.Hash, Status: catalog.EmbeddingStatusReady}
	processErr := loadErr
	if processErr == nil {
		result.Vector, processErr = s.computeEmbedding(ctx, data)
	}
	if ctx.Err() != nil {
		return runs.Outcome{Status: "cancelled"}, nil
	}
	if processErr != nil {
		if isTransientEmbedError(processErr) {
			return runs.Outcome{Status: "failed", ErrorCode: "processing_failed"}, nil
		}
		// Preserve permanent image/inference failure classification, but never
		// copy raw provider/storage errors (paths, URLs, credentials) to Admin.
		result.Status = catalog.EmbeddingStatusFailed
		result.Error = "processing_failed"
	}
	if ctx.Err() != nil {
		return runs.Outcome{Status: "cancelled"}, nil
	}
	// Cancellation can still abort reservation. After it commits, neither the
	// vector write nor catalog bookkeeping is cancelled by the run context.
	completion, cancel := context.WithTimeout(context.WithoutCancel(ctx), embeddingTimeout)
	defer cancel()
	applied, rejected, applyErr := s.applyEmbeddingResultsWithCompletion(ctx, completion, []catalog.EmbeddingResult{result}, []string{claim.Token})
	if applied == 1 {
		if result.Status == catalog.EmbeddingStatusFailed {
			return runs.Outcome{Status: "failed", ErrorCode: "processing_failed"}, nil
		}
		return runs.Outcome{Status: "succeeded"}, nil
	}
	if applyErr != nil {
		if ctx.Err() != nil && errors.Is(applyErr, ctx.Err()) {
			return runs.Outcome{Status: "cancelled"}, nil
		}
		return runs.Outcome{Status: "failed", ErrorCode: "executor_failed"}, nil
	}
	for _, rejection := range rejected {
		switch rejection.Reason {
		case catalog.EmbeddingRejectUnknownHash:
			return runs.Outcome{Status: "skipped", ErrorCode: "target_missing"}, nil
		case catalog.EmbeddingRejectNotClaimed:
			return runs.Outcome{Status: "busy", ErrorCode: "claim_lost"}, nil
		}
	}
	return runs.Outcome{Status: "failed", ErrorCode: "processing_failed"}, nil
}
