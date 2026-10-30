package catalog

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

type LocalEncodingClaimStatus string

const (
	LocalEncodingClaimed LocalEncodingClaimStatus = "claimed"
	LocalEncodingBusy    LocalEncodingClaimStatus = "busy"
	LocalEncodingMissing LocalEncodingClaimStatus = "missing"
)

// PendingLocalEncodingHashes snapshots the exact target set a local encoding
// drain will work through: pending, non-WebP blobs in deterministic hash
// order — the same eligibility the retired run snapshot used. The list is
// finite on purpose: it is what makes a drain terminate, at the cost that
// blobs uploaded after the snapshot wait for the next trigger. Read outside
// any transaction: it is a starting point, and ClaimLocalEncoding re-checks
// each row atomically before any work happens.
func (s *Store) PendingLocalEncodingHashes(ctx context.Context) ([]string, error) {
	return queryBlobHashes(ctx, s.db, `SELECT hash FROM blobs WHERE encoding_status='pending' AND content_type!='image/webp' ORDER BY hash`)
}

// ClaimLocalEncoding claims exactly one pending non-WebP blob. In particular,
// an expired external lease is still busy: only external recovery may reclaim it.
func (s *Store) ClaimLocalEncoding(ctx context.Context, hash, token string, until time.Time) (EncodingJob, LocalEncodingClaimStatus, error) {
	if raw, err := hex.DecodeString(token); err != nil || len(raw) != 16 || !until.After(time.Now()) {
		return EncodingJob{}, "", fmt.Errorf("invalid local encoding claim")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EncodingJob{}, "", err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return EncodingJob{}, "", err
	}
	job := EncodingJob{Hash: hash}
	if err := tx.QueryRowContext(ctx, `SELECT size_bytes,content_type,encoding_status FROM blobs WHERE hash=?`, hash).Scan(&job.SizeBytes, &job.Type, &job.Status); err != nil {
		if err == sql.ErrNoRows {
			return EncodingJob{}, LocalEncodingMissing, nil
		}
		return EncodingJob{}, "", err
	}
	if job.Status != "pending" || job.Type == "image/webp" {
		return EncodingJob{}, LocalEncodingBusy, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE blobs SET encoding_status='leased',encoding_owner='local',encoding_token=?,encoding_stage_key='',encoding_lease_until=?,encoding_error=''
		WHERE hash=? AND encoding_status='pending' AND content_type!='image/webp'`, token, until.UnixMilli(), hash)
	if err != nil {
		return EncodingJob{}, "", err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return EncodingJob{}, LocalEncodingBusy, err
	}
	if _, err := observeStage(ctx, tx, hash, "encoding", "claimed", "leased", "", time.Now(), true, false, false, "", "unknown"); err != nil {
		return EncodingJob{}, "", err
	}
	job.Token, job.Owner, job.Status, job.LeaseUntil = token, "local", "leased", until
	return job, LocalEncodingClaimed, tx.Commit()
}

// ReleaseLocalEncoding cannot touch an external owner or accepted output, even
// when cancellation/restart races the handoff or the token happens to match.
func (s *Store) ReleaseLocalEncoding(ctx context.Context, hash, token string) error {
	return s.releaseEncoding(ctx, hash, token, "local")
}
