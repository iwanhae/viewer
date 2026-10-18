package catalog

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidJobRun  = errors.New("invalid local run request")
	ErrJobRunConflict = errors.New("local run stage or idempotency conflict")
	ErrJobRunNotFound = errors.New("local run not found")
)

const JobRunPageLimit = 100

type JobRunCounts struct {
	Total       int64 `json:"total"`
	Pending     int64 `json:"pending"`
	Running     int64 `json:"running"`
	Succeeded   int64 `json:"succeeded"`
	Failed      int64 `json:"failed"`
	Busy        int64 `json:"busy"`
	Skipped     int64 `json:"skipped"`
	Cancelled   int64 `json:"cancelled"`
	Interrupted int64 `json:"interrupted"`
}

// JobRun is exclusively local. Actor is an unverified Basic-auth username, not
// an authenticated identity. Counts are derived from durable item outcomes.
type JobRun struct {
	ID              string       `json:"id"`
	Stage           string       `json:"stage"`
	Actor           string       `json:"actor"`
	Executor        string       `json:"executor"`
	Status          string       `json:"status"`
	CancelRequested bool         `json:"cancelRequested"`
	CreatedAt       string       `json:"createdAt"`
	UpdatedAt       string       `json:"updatedAt"`
	StartedAt       *string      `json:"startedAt"`
	FinishedAt      *string      `json:"finishedAt"`
	LastError       string       `json:"lastError"`
	Counts          JobRunCounts `json:"counts"`
}

type JobRunItem struct {
	Hash       string  `json:"hash"`
	Status     string  `json:"status"`
	StartedAt  *string `json:"startedAt"`
	FinishedAt *string `json:"finishedAt"`
	UpdatedAt  string  `json:"updatedAt"`
	LastError  string  `json:"lastError"`
	// ClaimToken fences bookkeeping and is reserved for an executor's exact-hash
	// local claim. It is never exposed through the Admin or external worker API.
	ClaimToken string `json:"-"`
}

type JobRunPage struct {
	Runs       []JobRun `json:"runs"`
	NextCursor *string  `json:"nextCursor"`
}

type JobRunItemPage struct {
	Run        JobRun       `json:"run"`
	Items      []JobRunItem `json:"items"`
	NextCursor *string      `json:"nextCursor"`
}

func ValidJobRunStage(stage string) bool { return stage == "embedding" || stage == "encoding" }

func ValidateJobRunRequest(stage, key string) error {
	if !ValidJobRunStage(stage) || strings.TrimSpace(key) == "" || len(key) > 200 {
		return fmt.Errorf("%w: stage must be embedding or encoding and Idempotency-Key must be 1–200 bytes", ErrInvalidJobRun)
	}
	return nil
}

const jobRunSelect = `SELECT r.id,r.stage,r.actor,r.executor,r.status,r.cancel_requested,r.created_at,r.updated_at,r.started_at,r.finished_at,r.last_error,
 COUNT(i.hash), COALESCE(SUM(i.status='pending'),0), COALESCE(SUM(i.status='running'),0),
 COALESCE(SUM(i.status='succeeded'),0), COALESCE(SUM(i.status='failed'),0), COALESCE(SUM(i.status='busy'),0),
 COALESCE(SUM(i.status='skipped'),0), COALESCE(SUM(i.status='cancelled'),0), COALESCE(SUM(i.status='interrupted'),0)
 FROM job_runs r LEFT JOIN job_run_items i ON i.run_id=r.id `

func scanJobRun(row interface{ Scan(...any) error }) (JobRun, error) {
	var r JobRun
	err := row.Scan(&r.ID, &r.Stage, &r.Actor, &r.Executor, &r.Status, &r.CancelRequested, &r.CreatedAt, &r.UpdatedAt, &r.StartedAt, &r.FinishedAt, &r.LastError,
		&r.Counts.Total, &r.Counts.Pending, &r.Counts.Running, &r.Counts.Succeeded, &r.Counts.Failed, &r.Counts.Busy, &r.Counts.Skipped, &r.Counts.Cancelled, &r.Counts.Interrupted)
	if err == sql.ErrNoRows {
		err = ErrJobRunNotFound
	}
	return r, err
}

// FindJobRunSubmission permits replay even if an executor has subsequently
// become unavailable. A key is bound to actor + stage for the run's lifetime.
func (s *Store) FindJobRunSubmission(ctx context.Context, actor, stage, key string) (JobRun, error) {
	r, err := scanJobRun(s.db.QueryRowContext(ctx, jobRunSelect+`WHERE r.actor=? AND r.idempotency_key=? GROUP BY r.id`, actor, key))
	if err == nil && r.Stage != stage {
		return JobRun{}, ErrJobRunConflict
	}
	return r, err
}

// CreateJobRun snapshots pending unique hashes in the same writer transaction
// as stage exclusivity and idempotency. It takes no blob lease and never appends
// later uploads. Executors MUST subsequently claim only these exact hashes with
// pending-state/token fencing, returning busy if another owner won the race.
func (s *Store) CreateJobRun(ctx context.Context, actor, stage, key, executor string) (JobRun, bool, error) {
	if err := ValidateJobRunRequest(stage, key); err != nil {
		return JobRun{}, false, err
	}
	if executor == "" {
		return JobRun{}, false, ErrInvalidJobRun
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return JobRun{}, false, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return JobRun{}, false, err
	}
	r, err := scanJobRun(tx.QueryRowContext(ctx, jobRunSelect+`WHERE r.actor=? AND r.idempotency_key=? GROUP BY r.id`, actor, key))
	if err == nil {
		if r.Stage != stage {
			return JobRun{}, false, ErrJobRunConflict
		}
		return r, true, tx.Commit()
	}
	if !errors.Is(err, ErrJobRunNotFound) {
		return JobRun{}, false, err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM job_runs WHERE stage=? AND status IN ('queued','running'))`, stage).Scan(&active); err != nil {
		return JobRun{}, false, err
	}
	if active {
		return JobRun{}, false, ErrJobRunConflict
	}
	id, now := rand.Text(), recoveryTimestamp(time.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_runs(id,stage,actor,idempotency_key,executor,status,created_at,updated_at) VALUES(?,?,?,?,?,'queued',?,?)`, id, stage, actor, key, executor, now, now); err != nil {
		return JobRun{}, false, err
	}
	predicate := `embedding_status='pending' AND embedding_finalizing=''`
	if stage == "encoding" {
		predicate = `encoding_status='pending' AND content_type!='image/webp'`
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_run_items(run_id,hash,status,updated_at) SELECT ?,hash,'pending',? FROM blobs WHERE `+predicate, id, now); err != nil {
		return JobRun{}, false, err
	}
	r, err = scanJobRun(tx.QueryRowContext(ctx, jobRunSelect+`WHERE r.id=? GROUP BY r.id`, id))
	if err != nil {
		return JobRun{}, false, err
	}
	return r, false, tx.Commit()
}

func (s *Store) JobRun(ctx context.Context, id string) (JobRun, error) {
	return scanJobRun(s.db.QueryRowContext(ctx, jobRunSelect+`WHERE r.id=? GROUP BY r.id`, id))
}

func runCursor(parts ...string) string {
	raw, _ := json.Marshal(parts)
	return base64.RawURLEncoding.EncodeToString(raw)
}
func parseRunCursor(cursor string, n int) ([]string, error) {
	if cursor == "" {
		return make([]string, n), nil
	}
	if len(cursor) > 4096 {
		return nil, ErrInvalidJobRun
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	var parts []string
	if err != nil || json.Unmarshal(raw, &parts) != nil || len(parts) != n {
		return nil, ErrInvalidJobRun
	}
	for _, p := range parts {
		if p == "" {
			return nil, ErrInvalidJobRun
		}
	}
	return parts, nil
}

func (s *Store) JobRuns(ctx context.Context, limit int, cursor string) (JobRunPage, error) {
	page := JobRunPage{Runs: []JobRun{}}
	parts, err := parseRunCursor(cursor, 2)
	if err != nil || limit < 1 || limit > JobRunPageLimit {
		return page, ErrInvalidJobRun
	}
	rows, err := s.db.QueryContext(ctx, jobRunSelect+`WHERE (?='' OR r.created_at<? OR (r.created_at=? AND r.id<?)) GROUP BY r.id ORDER BY r.created_at DESC,r.id DESC LIMIT ?`, parts[0], parts[0], parts[0], parts[1], limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanJobRun(rows)
		if err != nil {
			return page, err
		}
		page.Runs = append(page.Runs, r)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	if len(page.Runs) > limit {
		page.Runs = page.Runs[:limit]
		last := page.Runs[limit-1]
		c := runCursor(last.CreatedAt, last.ID)
		page.NextCursor = &c
	}
	return page, nil
}

func (s *Store) JobRunItems(ctx context.Context, id string, limit int, cursor string) (JobRunItemPage, error) {
	page := JobRunItemPage{Items: []JobRunItem{}}
	parts, err := parseRunCursor(cursor, 2)
	if err != nil || limit < 1 || limit > JobRunPageLimit || (cursor != "" && parts[0] != id) {
		return page, ErrInvalidJobRun
	}
	// One read transaction keeps counts and item states consistent.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	page.Run, err = scanJobRun(tx.QueryRowContext(ctx, jobRunSelect+`WHERE r.id=? GROUP BY r.id`, id))
	if err != nil {
		return page, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT hash,status,started_at,finished_at,updated_at,last_error FROM job_run_items WHERE run_id=? AND hash>? ORDER BY hash LIMIT ?`, id, parts[1], limit+1)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var i JobRunItem
		if err := rows.Scan(&i.Hash, &i.Status, &i.StartedAt, &i.FinishedAt, &i.UpdatedAt, &i.LastError); err != nil {
			rows.Close()
			return page, err
		}
		page.Items = append(page.Items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		c := runCursor(id, page.Items[limit-1].Hash)
		page.NextCursor = &c
	}
	return page, tx.Commit()
}

func (s *Store) StartJobRun(ctx context.Context, id string) error {
	now := recoveryTimestamp(time.Now())
	_, err := s.db.ExecContext(ctx, `UPDATE job_runs SET status='running',started_at=?,updated_at=? WHERE id=? AND status='queued' AND cancel_requested=0`, now, now, id)
	return err
}

// NextJobRunItem claims bookkeeping only, not the underlying blob. The executor
// must atomically claim the exact pending hash using ClaimToken before any side
// effect. Keeping that distinction lets external workers continue untouched.
func (s *Store) NextJobRunItem(ctx context.Context, id string) (*JobRunItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return nil, err
	}
	var item JobRunItem
	err = tx.QueryRowContext(ctx, `SELECT i.hash FROM job_run_items i JOIN job_runs r ON r.id=i.run_id WHERE r.id=? AND r.status='running' AND r.cancel_requested=0 AND i.status='pending' ORDER BY i.hash LIMIT 1`, id).Scan(&item.Hash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item.Status = "running"
	// Private stage tokens also identify accepted encoder spool files. Use the
	// same safe 128-bit hex identity as encoding, without changing worker APIs.
	item.ClaimToken, err = newEncodingToken()
	if err != nil {
		return nil, err
	}
	item.UpdatedAt = recoveryTimestamp(time.Now())
	item.StartedAt = &item.UpdatedAt
	_, err = tx.ExecContext(ctx, `UPDATE job_run_items SET status='running',claim_token=?,started_at=?,updated_at=? WHERE run_id=? AND hash=? AND status='pending'`, item.ClaimToken, item.UpdatedAt, item.UpdatedAt, id, item.Hash)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE job_runs SET updated_at=? WHERE id=?`, item.UpdatedAt, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &item, nil
}

// FinishJobRunItem is token-fenced and cannot overwrite terminal outcomes.
// safeError is a public machine code, never raw processor stderr or an error.
func (s *Store) FinishJobRunItem(ctx context.Context, id string, item JobRunItem, status, safeError string) error {
	switch status {
	case "succeeded", "failed", "busy", "skipped", "cancelled":
	default:
		return ErrInvalidJobRun
	}
	safeError = jobRunSafeError(safeError)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := recoveryTimestamp(time.Now())
	result, err := tx.ExecContext(ctx, `UPDATE job_run_items SET status=?,last_error=?,updated_at=?,finished_at=? WHERE run_id=? AND hash=? AND claim_token=? AND claim_token!='' AND status='running'`, status, safeError, now, now, id, item.Hash, item.ClaimToken)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE job_runs SET updated_at=?,last_error=CASE WHEN ?!='' THEN ? ELSE last_error END WHERE id=?`, now, safeError, safeError, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Only allow known public codes: even an accidentally supplied secret or path
// must not become a durable Admin-visible error.
func jobRunSafeError(code string) string {
	switch code {
	case "", "executor_failed", "executor_panic", "invalid_executor_outcome", "target_busy", "target_missing", "claim_lost", "processing_failed", "service_stopped", "cwebp_unavailable":
		return code
	default:
		return "executor_failed"
	}
}

func (s *Store) RequestJobRunCancel(ctx context.Context, id string) (JobRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return JobRun{}, err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return JobRun{}, err
	}
	now := recoveryTimestamp(time.Now())
	if _, err := tx.ExecContext(ctx, `UPDATE job_runs SET cancel_requested=1,updated_at=? WHERE id=? AND status IN ('queued','running') AND cancel_requested=0`, now, id); err != nil {
		return JobRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE job_run_items SET status='cancelled',updated_at=?,finished_at=? WHERE run_id=? AND status='pending' AND EXISTS(SELECT 1 FROM job_runs WHERE id=? AND cancel_requested=1)`, now, now, id, id); err != nil {
		return JobRun{}, err
	}
	r, err := scanJobRun(tx.QueryRowContext(ctx, jobRunSelect+`WHERE r.id=? GROUP BY r.id`, id))
	if err != nil {
		return r, err
	}
	return r, tx.Commit()
}

// FinishJobRun closes a drained run. Cancellation is not rollback: completed
// outcomes remain intact and an in-flight processor may still have succeeded.
func (s *Store) FinishJobRun(ctx context.Context, id, terminal, safeError string) error {
	if terminal != "" && terminal != "failed" && terminal != "interrupted" {
		return ErrInvalidJobRun
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return err
	}
	r, err := scanJobRun(tx.QueryRowContext(ctx, jobRunSelect+`WHERE r.id=? GROUP BY r.id`, id))
	if err != nil {
		return err
	}
	if r.Status != "queued" && r.Status != "running" {
		return nil
	}
	if terminal == "" {
		if r.Counts.Pending+r.Counts.Running > 0 {
			return ErrJobRunConflict
		}
		terminal = "completed"
		if r.Counts.Failed > 0 {
			terminal = "failed"
		}
		if r.CancelRequested {
			terminal = "cancelled"
		}
	}
	if err := finishJobRunTx(ctx, tx, id, terminal, jobRunSafeError(safeError)); err != nil {
		return err
	}
	return tx.Commit()
}

func finishJobRunTx(ctx context.Context, tx *sql.Tx, id, status, safeError string) error {
	finishedAt := time.Now()
	now := recoveryTimestamp(finishedAt)
	// Release only matching run-owned tokens. In particular, do not reset any
	// external leases, or roll back ambiguous encoding commits/finalizations.
	embeddingHashes, err := queryBlobHashes(ctx, tx, `UPDATE blobs SET embedding_status='pending',embedding_owner='external',embedding_token='',embedding_lease_until=0
 WHERE embedding_owner='local' AND embedding_status='processing' AND embedding_finalizing='' AND EXISTS
 (SELECT 1 FROM job_run_items i JOIN job_runs r ON r.id=i.run_id WHERE r.id=? AND r.stage='embedding' AND i.hash=blobs.hash AND i.claim_token!='' AND i.claim_token=blobs.embedding_token) RETURNING hash`, id)
	if err != nil {
		return err
	}
	encodingHashes, err := queryBlobHashes(ctx, tx, `UPDATE blobs SET encoding_status='pending',encoding_owner='external',encoding_token='',encoding_lease_until=0,encoding_stage_key=''
 WHERE encoding_owner='local' AND encoding_status='leased' AND EXISTS
 (SELECT 1 FROM job_run_items i JOIN job_runs r ON r.id=i.run_id WHERE r.id=? AND r.stage='encoding' AND i.hash=blobs.hash AND i.claim_token!='' AND i.claim_token=blobs.encoding_token) RETURNING hash`, id)
	if err != nil {
		return err
	}
	for stage, hashes := range map[string][]string{"embedding": embeddingHashes, "encoding": encodingHashes} {
		for _, hash := range hashes {
			if _, err := observeStage(ctx, tx, hash, stage, "released", "pending", "", finishedAt, false, true, false, "", "unknown"); err != nil {
				return err
			}
		}
	}
	itemStatus := "interrupted"
	if status == "failed" {
		itemStatus = "failed"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE job_run_items SET status=?,updated_at=?,finished_at=?,last_error=CASE WHEN ?!='' THEN ? ELSE last_error END WHERE run_id=? AND status IN ('pending','running')`, itemStatus, now, now, safeError, safeError, id); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE job_runs SET status=?,finished_at=?,updated_at=?,last_error=CASE WHEN ?!='' THEN ? ELSE last_error END WHERE id=?`, status, now, now, safeError, safeError, id)
	return err
}

func recoverJobRuns(ctx context.Context, tx *sql.Tx) error {
	// Some migration tests intentionally open a future-version, partial schema.
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='job_runs' AND type='table')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	ids, err := queryBlobHashes(ctx, tx, `SELECT id FROM job_runs WHERE status IN ('queued','running')`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := finishJobRunTx(ctx, tx, id, "interrupted", "service_stopped"); err != nil {
			return err
		}
	}
	return nil
}
