package recommend

import (
	"context"
	"fmt"
	"time"

	"viewer/internal/catalog"
)

func (s *Service) ClaimLocalEmbeddings(ctx context.Context, limit int) ([]catalog.LocalEmbeddingClaim, time.Time, error) {
	if s == nil || s.catalog == nil {
		return nil, time.Time{}, fmt.Errorf("catalog is not available")
	}
	noteActivity(&s.lastClaimMs)
	until := time.Now().Add(DefaultLeaseTTL)
	claims, err := s.catalog.ClaimLocalEmbeddings(ctx, clampClaimLimit(limit), until)
	return claims, until, err
}

func (s *Service) RenewLocalLeases(ctx context.Context, claims []catalog.LocalEmbeddingClaim) (time.Time, []string, error) {
	if s == nil || s.catalog == nil {
		return time.Time{}, nil, fmt.Errorf("catalog is not available")
	}
	noteActivity(&s.lastRenewMs)
	until := time.Now().Add(DefaultLeaseTTL)
	renewed, err := s.catalog.RenewLocalEmbeddingLeases(ctx, claims, until)
	return until, renewed, err
}

// ReleaseLocalClaims returns only successfully cancelled claims. A finalization
// that reserved first is allowed to finish and is not reported as cancelled.
func (s *Service) ReleaseLocalClaims(ctx context.Context, claims []catalog.LocalEmbeddingClaim) ([]string, error) {
	if s == nil || s.catalog == nil {
		return nil, fmt.Errorf("catalog is not available")
	}
	return s.catalog.ReleaseLocalEmbeddingClaims(ctx, claims)
}

func (s *Service) ApplyLocalEmbeddingResults(ctx context.Context, results []catalog.LocalEmbeddingResult) (int, []catalog.RejectedEmbedding, error) {
	plain := make([]catalog.EmbeddingResult, 0, len(results))
	tokens := make([]string, 0, len(results))
	for _, result := range results {
		plain = append(plain, result.EmbeddingResult)
		tokens = append(tokens, result.Token)
	}
	return s.applyEmbeddingResults(ctx, plain, tokens)
}
