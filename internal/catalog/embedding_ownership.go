package catalog

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LocalEmbeddingClaim is an in-process lease. Token is never part of the
// external worker protocol and must accompany every local ownership operation.
type LocalEmbeddingClaim struct {
	Blob
	Token string
}

type LocalEmbeddingResult struct {
	EmbeddingResult
	Token string
}

func (s *Store) ClaimLocalEmbeddings(ctx context.Context, limit int, leaseUntil time.Time) ([]LocalEmbeddingClaim, error) {
	return s.claimEmbeddings(ctx, limit, leaseUntil, "local")
}

// RenewLocalEmbeddingLeases cannot resurrect an expired lease, even if it has
// not yet been reclaimed. The returned hashes identify the leases still owned.
func (s *Store) RenewLocalEmbeddingLeases(ctx context.Context, claims []LocalEmbeddingClaim, leaseUntil time.Time) ([]string, error) {
	return s.updateLocalEmbeddingClaims(ctx, claims, leaseUntil, false)
}

// ReleaseLocalEmbeddingClaims is token-conditional cancellation. Finalization
// and release are mutually exclusive: if finalization won, that hash is NOT
// returned and its already-reserved write may complete. Callers must not report
// those hashes as cancelled. Releasing an expired but unreclaimed token is safe.
func (s *Store) ReleaseLocalEmbeddingClaims(ctx context.Context, claims []LocalEmbeddingClaim) ([]string, error) {
	return s.updateLocalEmbeddingClaims(ctx, claims, time.Time{}, true)
}

func (s *Store) updateLocalEmbeddingClaims(ctx context.Context, claims []LocalEmbeddingClaim, until time.Time, release bool) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return nil, err
	}
	now := time.Now()
	changed := make([]string, 0, len(claims))
	for _, claim := range claims {
		if claim.Token == "" {
			continue
		}
		query := `UPDATE blobs SET embedding_lease_until=? WHERE hash=? AND embedding_token=?
			AND embedding_owner='local' AND embedding_status='processing' AND embedding_finalizing='' AND embedding_lease_until>=?`
		args := []any{until.UnixMilli(), claim.Hash, claim.Token, now.UnixMilli()}
		if release {
			query = `UPDATE blobs SET embedding_status='pending', embedding_lease_until=0, embedding_owner='external', embedding_token=''
				WHERE hash=? AND embedding_token=? AND embedding_owner='local' AND embedding_status='processing' AND embedding_finalizing=''`
			args = []any{claim.Hash, claim.Token}
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n != 1 {
			continue
		}
		changed = append(changed, claim.Hash)
		if release {
			if _, err := observeStage(ctx, tx, claim.Hash, "embedding", "released", "pending", "", now, false, true, false, "", "unknown"); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return changed, nil
}

// FinalizeLocalEmbeddingResults rejects stale tokens BEFORE write is called.
// The durable reservation spans vector side effects and catalog bookkeeping,
// but no SQLite transaction or connection is held during write. Reservations
// do not expire: a stalled network call must not race a new owner. On ordinary
// failure they are cleared; after a crash Open's startup reset clears them.
// As with the existing startup reset, only one server process may open a catalog.
func (s *Store) FinalizeLocalEmbeddingResults(ctx context.Context, results []LocalEmbeddingResult, write func(context.Context, []EmbeddingResult) error) ([]string, []RejectedEmbedding, error) {
	plain := make([]EmbeddingResult, 0, len(results))
	tokens := make([]string, 0, len(results))
	for _, result := range results {
		plain = append(plain, result.EmbeddingResult)
		tokens = append(tokens, result.Token)
	}
	return s.finalizeEmbeddingResults(ctx, plain, tokens, write)
}

// FinalizeExternalEmbeddingResults preserves legacy hash-only acceptance,
// including expired leases. External attempts cannot be distinguished from
// successive external attempts; an old external result may finish a newer
// external claim. It can NEVER write over a local owner, even during upload.
func (s *Store) FinalizeExternalEmbeddingResults(ctx context.Context, results []EmbeddingResult, write func(context.Context, []EmbeddingResult) error) ([]string, []RejectedEmbedding, error) {
	return s.finalizeEmbeddingResults(ctx, results, nil, write)
}

func (s *Store) finalizeEmbeddingResults(ctx context.Context, results []EmbeddingResult, tokens []string, write func(context.Context, []EmbeddingResult) error) (applied []string, rejected []RejectedEmbedding, err error) {
	reservation := rand.Text()
	valid, rejected, err := s.reserveEmbeddingResults(ctx, results, tokens, reservation)
	if err != nil {
		return nil, nil, err
	}
	if len(valid) == 0 {
		return []string{}, rejected, nil
	}
	defer func() {
		// A cancelled request must still relinquish its reservation. Also runs
		// on panic; if cleanup fails, fail closed until startup recovery.
		_, cleanupErr := s.db.ExecContext(context.Background(), `UPDATE blobs SET embedding_finalizing='' WHERE embedding_finalizing=? AND embedding_finalizing!=''`, reservation)
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clear embedding finalization: %w", cleanupErr))
		}
	}()
	if write != nil {
		if err := write(ctx, valid); err != nil {
			return nil, rejected, err
		}
	}
	applied, moreRejected, err := s.applyEmbeddingResults(ctx, valid, reservation)
	return applied, append(rejected, moreRejected...), err
}

func (s *Store) reserveEmbeddingResults(ctx context.Context, results []EmbeddingResult, tokens []string, reservation string) ([]EmbeddingResult, []RejectedEmbedding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return nil, nil, err
	}
	valid := make([]EmbeddingResult, 0, len(results))
	rejected := make([]RejectedEmbedding, 0)
	now := time.Now().UnixMilli()
	for i, result := range results {
		result.Hash = strings.TrimSpace(result.Hash)
		reason := ""
		switch {
		case result.Hash == "":
			reason = EmbeddingRejectInvalidHash
		case result.Status != EmbeddingStatusReady && result.Status != EmbeddingStatusFailed:
			reason = EmbeddingRejectInvalidState
		case result.Status == EmbeddingStatusReady && len(result.Vector) != EmbeddingDim:
			reason = EmbeddingRejectWrongDim
		}
		if reason == "" {
			query := `UPDATE blobs SET embedding_finalizing=? WHERE hash=? AND embedding_status='processing' AND embedding_finalizing=''`
			args := []any{reservation, result.Hash}
			if tokens == nil {
				query += ` AND embedding_owner='external'`
			} else {
				query += ` AND embedding_owner='local' AND embedding_token=? AND embedding_token!='' AND embedding_lease_until>=?`
				args = append(args, tokens[i], now)
			}
			update, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return nil, nil, err
			}
			n, err := update.RowsAffected()
			if err != nil {
				return nil, nil, err
			}
			if n == 1 {
				valid = append(valid, result)
				continue
			}
			reason = EmbeddingRejectNotClaimed
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash=?)`, result.Hash).Scan(&exists); err != nil {
				return nil, nil, err
			}
			if !exists {
				reason = EmbeddingRejectUnknownHash
			}
		}
		rejected = append(rejected, RejectedEmbedding{Hash: result.Hash, Reason: reason})
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return valid, rejected, nil
}
