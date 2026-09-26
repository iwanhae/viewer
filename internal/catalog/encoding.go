package catalog

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// EncodingJob is one leased conversion of a source blob. The token changes on
// every claim, preventing a worker whose lease expired from committing later.
type EncodingJob struct {
	Hash       string
	SizeBytes  int64
	Type       string
	Status     string
	Token      string
	StageKey   string
	LeaseUntil time.Time
}

// EncodingCounts summarizes the conversion queue and measured storage savings
// across unique blobs. Already-WebP objects are excluded from Candidates.
type EncodingCounts struct {
	Candidates     int64   `json:"candidates"`
	Pending        int64   `json:"pending"`
	Processing     int64   `json:"processing"`
	Converted      int64   `json:"converted"`
	NotSmaller     int64   `json:"notSmaller"`
	Failed         int64   `json:"failed"`
	AlreadyWebP    int64   `json:"alreadyWebp"`
	RemainingBytes int64   `json:"remainingBytes"`
	SourceBytes    int64   `json:"sourceBytes"`
	OutputBytes    int64   `json:"outputBytes"`
	BytesSaved     int64   `json:"bytesSaved"`
	SavedPercent   float64 `json:"savedPercent"`
}

// EncodingCounts reports queue progress and savings from completed WebP
// replacements. The counters are per unique blob, not per photo row.
func (s *Store) EncodingCounts(ctx context.Context) (EncodingCounts, error) {
	var counts EncodingCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN encoding_status='pending' AND content_type!='image/webp' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN encoding_status IN ('leased','committing') AND content_type!='image/webp' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN encoding_status='done' AND encoding_source_size_bytes>size_bytes THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN encoding_status='skipped' AND content_type!='image/webp' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN encoding_status='failed' AND content_type!='image/webp' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN encoding_status='skipped' AND content_type='image/webp' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN encoding_status IN ('pending','leased','committing') AND content_type!='image/webp' THEN size_bytes ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN encoding_status='done' AND encoding_source_size_bytes>size_bytes THEN encoding_source_size_bytes ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN encoding_status='done' AND encoding_source_size_bytes>size_bytes THEN size_bytes ELSE 0 END), 0)
		FROM blobs`).Scan(
		&counts.Pending,
		&counts.Processing,
		&counts.Converted,
		&counts.NotSmaller,
		&counts.Failed,
		&counts.AlreadyWebP,
		&counts.RemainingBytes,
		&counts.SourceBytes,
		&counts.OutputBytes,
	)
	if err != nil {
		return EncodingCounts{}, fmt.Errorf("encoding counts: %w", err)
	}
	counts.Candidates = counts.Pending + counts.Processing + counts.Converted + counts.NotSmaller + counts.Failed
	counts.BytesSaved = counts.SourceBytes - counts.OutputBytes
	if counts.SourceBytes > 0 {
		counts.SavedPercent = float64(counts.BytesSaved) * 100 / float64(counts.SourceBytes)
	}
	return counts, nil
}

func newEncodingToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// ClaimEncoding takes the largest pending source blobs first. Expired claims
// become pending before selection. SQLite's single writer serializes callers.
func (s *Store) ClaimEncoding(ctx context.Context, limit int, ttl time.Duration) ([]EncodingJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now()
	if _, err = tx.ExecContext(ctx, `UPDATE blobs SET encoding_status='pending', encoding_token='', encoding_stage_key='', encoding_lease_until=0
		WHERE encoding_status='leased' AND encoding_lease_until < ?`, now.UnixMilli()); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT hash, size_bytes, content_type FROM blobs
		WHERE encoding_status='pending' AND content_type != 'image/webp'
		ORDER BY size_bytes DESC, created_at ASC, hash ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	jobs := make([]EncodingJob, 0, limit)
	for rows.Next() {
		var job EncodingJob
		if err := rows.Scan(&job.Hash, &job.SizeBytes, &job.Type); err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	lease := now.Add(ttl)
	for i := range jobs {
		token, err := newEncodingToken()
		if err != nil {
			return nil, err
		}
		jobs[i].Token = token
		jobs[i].StageKey = fmt.Sprintf("encoding/%s_%s.webp", jobs[i].Hash, token)
		jobs[i].Status = "leased"
		jobs[i].LeaseUntil = lease
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET encoding_status='leased', encoding_token=?, encoding_stage_key=?, encoding_lease_until=? WHERE hash=?`,
			token, jobs[i].StageKey, lease.UnixMilli(), jobs[i].Hash); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) GetEncodingJob(ctx context.Context, hash string) (EncodingJob, error) {
	var job EncodingJob
	var lease int64
	err := s.db.QueryRowContext(ctx, `SELECT hash, size_bytes, content_type, encoding_status, encoding_token, encoding_stage_key, encoding_lease_until FROM blobs WHERE hash=?`, hash).
		Scan(&job.Hash, &job.SizeBytes, &job.Type, &job.Status, &job.Token, &job.StageKey, &lease)
	if err == sql.ErrNoRows {
		return job, ErrBlobNotFound
	}
	if err != nil {
		return job, err
	}
	job.LeaseUntil = time.UnixMilli(lease)
	return job, nil
}

func (s *Store) RenewEncoding(ctx context.Context, hash, token string, ttl time.Duration) (time.Time, bool, error) {
	until := time.Now().Add(ttl)
	result, err := s.db.ExecContext(ctx, `UPDATE blobs SET encoding_lease_until=?
		WHERE hash=? AND encoding_token=? AND encoding_status='leased' AND encoding_lease_until>=?`,
		until.UnixMilli(), hash, token, time.Now().UnixMilli())
	if err != nil {
		return time.Time{}, false, err
	}
	n, err := result.RowsAffected()
	return until, n == 1, err
}

// BeginEncodingCommit records the validated source size before the object
// replacement and keeps the commit leased for reconcileAfter. Recover waits
// for that lease to expire so an accepted but not-yet-visible object-store copy
// is not mistaken for a failed copy.
func (s *Store) BeginEncodingCommit(ctx context.Context, hash, token string, sourceSize int64, reconcileAfter time.Duration) (bool, error) {
	leaseUntil := time.Now().Add(reconcileAfter)
	result, err := s.db.ExecContext(ctx, `UPDATE blobs SET encoding_status='committing', encoding_source_size_bytes=?, encoding_lease_until=?
		WHERE hash=? AND encoding_token=? AND encoding_status='leased' AND encoding_lease_until>=?`,
		sourceSize, leaseUntil.UnixMilli(), hash, token, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) FinishEncoding(ctx context.Context, hash, token, status, contentType, errorText string, size int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, content_type=?, size_bytes=?, encoding_error=?, encoding_lease_until=0
		WHERE hash=? AND encoding_token=? AND encoding_status='committing'`, status, contentType, size, errorText, hash, token)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) SkipEncoding(ctx context.Context, hash, token, status, contentType, errorText string, size int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE blobs SET encoding_status=?, content_type=?, size_bytes=?, encoding_error=?, encoding_lease_until=0, encoding_source_size_bytes=0
		WHERE hash=? AND encoding_token=? AND encoding_status='leased' AND encoding_lease_until>=?`,
		status, contentType, size, errorText, hash, token, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) ReleaseEncoding(ctx context.Context, hash, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE blobs SET encoding_status='pending', encoding_lease_until=0, encoding_token='', encoding_stage_key=''
		WHERE hash=? AND encoding_token=? AND encoding_status='leased'`, hash, token)
	return err
}

func (s *Store) CommittingEncodings(ctx context.Context) ([]EncodingJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hash, size_bytes, content_type, encoding_status, encoding_token, encoding_stage_key, encoding_lease_until
		FROM blobs WHERE encoding_status='committing'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []EncodingJob
	for rows.Next() {
		var job EncodingJob
		var lease int64
		if err := rows.Scan(&job.Hash, &job.SizeBytes, &job.Type, &job.Status, &job.Token, &job.StageKey, &lease); err != nil {
			return nil, err
		}
		job.LeaseUntil = time.UnixMilli(lease)
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (s *Store) ResetCommittingEncoding(ctx context.Context, hash, token string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE blobs SET encoding_status='pending', encoding_token='', encoding_stage_key='', encoding_lease_until=0, encoding_source_size_bytes=0
		WHERE hash=? AND encoding_token=? AND encoding_status='committing'`, hash, token)
	return err
}
