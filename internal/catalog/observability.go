package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// EventHistoryVisibility is how long pipeline events remain visible through
// history reads. Physical pruning is opportunistic and does not guarantee a
// maximum on-disk lifetime.
const EventHistoryVisibility = 90 * 24 * time.Hour

const maxHistoryPageSize = 100

// StageObservation is the best-known observation of a blob's current stage.
// Null timestamps are intentional for rows that predate observability or have
// not yet been claimed since the migration.
type StageObservation struct {
	StartedAt       *string `json:"startedAt"`
	FinishedAt      *string `json:"finishedAt"`
	StatusChangedAt *string `json:"statusChangedAt"`
	Status          string  `json:"status"`
	Phase           *string `json:"phase"`
	Attempts        int64   `json:"attempts"`
	ErrorCode       string  `json:"errorCode"`
	Retryability    string  `json:"retryability"`
}

// PipelineEvent is a safe, structured history record. AttemptID is the internal
// per-blob/stage claim sequence, not the worker's token. It deliberately has no
// stage key, URL, or raw error field.
type PipelineEvent struct {
	ID           int64   `json:"id"`
	Stage        string  `json:"stage"`
	Event        string  `json:"event"`
	Status       string  `json:"status"`
	Phase        *string `json:"phase"`
	AttemptID    int64   `json:"attemptId"`
	ErrorCode    string  `json:"errorCode"`
	Retryability string  `json:"retryability"`
	OccurredAt   string  `json:"occurredAt"`
}

type PipelineHistoryPage struct {
	Events     []PipelineEvent `json:"events"`
	NextCursor *string         `json:"nextCursor"`
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type sqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// lockCatalogWriter reserves SQLite's single-writer slot before a transaction
// reads rows it will later instrument. This avoids a deferred read-to-write
// lock upgrade racing another catalog process.
func lockCatalogWriter(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE blobs SET size_bytes=size_bytes WHERE 0`)
	return err
}

func queryBlobHashes(ctx context.Context, q sqlQueryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hashes := make([]string, 0, 500)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hashes, nil
}

func observationTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullablePhase(phase string) any {
	if phase == "" {
		return nil
	}
	return phase
}

// safeStageErrorCode maps only explicitly recognized, non-sensitive evidence
// to a short code. Unclassified legacy/raw error strings never enter history.
func safeStageErrorCode(stage, raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	switch stage {
	case "encoding":
		switch {
		case strings.Contains(raw, "invalid source blob for encoding"):
			return "invalid_source"
		case strings.Contains(raw, "invalid encoded output"):
			return "invalid_output"
		case strings.Contains(raw, "accepted encoding output is missing"):
			return "output_missing"
		default:
			return "legacy_unknown"
		}
	case "embedding":
		switch {
		case strings.HasPrefix(raw, "load image bytes:"):
			return "image_load_failed"
		case strings.HasPrefix(raw, "embed image:"):
			return "embedding_failed"
		default:
			return "legacy_unknown"
		}
	default:
		return "legacy_unknown"
	}
}

// observeStage writes current-stage fields and an append-only event in the
// caller's state transaction. A new claim is the only transition that
// increments attempts and replaces started_at; retry transitions deliberately
// leave the previous error code intact until a new outcome is accepted.
func observeStage(ctx context.Context, q sqlExecer, hash, stage, event, status, phase string, now time.Time, claim, finish, setError bool, errorCode, retryability string) (int64, error) {
	changed := observationTime(now)
	var (
		attempt      int64
		currentError string
	)
	var err error
	if claim {
		err = q.QueryRowContext(ctx, `UPDATE blob_stage_observations
			SET started_at=?, finished_at=NULL, status_changed_at=?, status=?, phase=?, attempts=attempts+1
			WHERE blob_hash=? AND stage=? RETURNING attempts, error_code`,
			changed, changed, status, nullablePhase(phase), hash, stage).Scan(&attempt, &currentError)
	} else {
		finishedAt := any(nil)
		if finish {
			finishedAt = changed
		}
		if setError {
			err = q.QueryRowContext(ctx, `UPDATE blob_stage_observations
				SET status_changed_at=?, status=?, phase=?, finished_at=?, error_code=?, retryability=?
				WHERE blob_hash=? AND stage=? RETURNING attempts, error_code`,
				changed, status, nullablePhase(phase), finishedAt, errorCode, retryability, hash, stage).Scan(&attempt, &currentError)
		} else {
			err = q.QueryRowContext(ctx, `UPDATE blob_stage_observations
				SET status_changed_at=?, status=?, phase=?, finished_at=?
				WHERE blob_hash=? AND stage=? RETURNING attempts, error_code`,
				changed, status, nullablePhase(phase), finishedAt, hash, stage).Scan(&attempt, &currentError)
		}
	}
	if err != nil {
		return 0, fmt.Errorf("update %s observation for %s: %w", stage, hash, err)
	}
	if errorCode == "" && !setError {
		errorCode = currentError
	}
	if retryability == "" {
		retryability = "unknown"
	}
	result, err := q.ExecContext(ctx, `INSERT INTO pipeline_events
		(blob_hash, stage, event, status, phase, attempt_id, error_code, retryability, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hash, stage, event, status, nullablePhase(phase), attempt, errorCode, retryability, changed)
	if err != nil {
		return 0, fmt.Errorf("append %s event for %s: %w", stage, hash, err)
	}
	// Opportunistically prune a bounded batch on periodic writes. Read visibility
	// is independently enforced by PipelineHistory, so idle catalogs remain safe.
	id, err := result.LastInsertId()
	if err == nil && id%100 == 0 {
		cutoff := observationTime(now.Add(-EventHistoryVisibility))
		_, err = q.ExecContext(ctx, `DELETE FROM pipeline_events WHERE id IN
			(SELECT id FROM pipeline_events WHERE occurred_at < ? ORDER BY id LIMIT 500)`, cutoff)
		if err != nil {
			return 0, fmt.Errorf("prune old pipeline events: %w", err)
		}
	}
	return attempt, nil
}

func (s *Store) StageObservations(ctx context.Context, hash string) (map[string]StageObservation, error) {
	return stageObservations(ctx, s.db, hash)
}

func stageObservations(ctx context.Context, q sqlQueryer, hash string) (map[string]StageObservation, error) {
	rows, err := q.QueryContext(ctx, `SELECT stage, started_at, finished_at, status_changed_at, status, phase, attempts, error_code, retryability
		FROM blob_stage_observations WHERE blob_hash=?`, hash)
	if err != nil {
		return nil, fmt.Errorf("read stage observations: %w", err)
	}
	defer rows.Close()
	observations := make(map[string]StageObservation, 2)
	for rows.Next() {
		var (
			stage                             string
			started, finished, changed, phase sql.NullString
			observation                       StageObservation
		)
		if err := rows.Scan(&stage, &started, &finished, &changed, &observation.Status, &phase,
			&observation.Attempts, &observation.ErrorCode, &observation.Retryability); err != nil {
			return nil, fmt.Errorf("scan stage observation: %w", err)
		}
		observation.StartedAt = nullStringPointer(started)
		observation.FinishedAt = nullStringPointer(finished)
		observation.StatusChangedAt = nullStringPointer(changed)
		observation.Phase = nullStringPointer(phase)
		observations[stage] = observation
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stage observations: %w", err)
	}
	return observations, nil
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

func (s *Store) PipelineHistory(ctx context.Context, hash, stage string, afterID int64, limit int) (PipelineHistoryPage, error) {
	if limit < 1 || limit > maxHistoryPageSize {
		return PipelineHistoryPage{}, fmt.Errorf("history page size must be between 1 and %d", maxHistoryPageSize)
	}
	if stage != "" && stage != "encoding" && stage != "embedding" {
		return PipelineHistoryPage{}, fmt.Errorf("unsupported pipeline stage %q", stage)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM blobs WHERE hash=?`, hash).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return PipelineHistoryPage{}, fmt.Errorf("%w: %s", ErrBlobNotFound, hash)
		}
		return PipelineHistoryPage{}, fmt.Errorf("check blob for pipeline history: %w", err)
	}
	query := `SELECT id, stage, event, status, phase, attempt_id, error_code, retryability, occurred_at
		FROM pipeline_events WHERE blob_hash=? AND id>? AND julianday(occurred_at)>=julianday(?)`
	args := []any{hash, afterID, observationTime(time.Now().Add(-EventHistoryVisibility))}
	if stage != "" {
		query += ` AND stage=?`
		args = append(args, stage)
	}
	query += ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return PipelineHistoryPage{}, fmt.Errorf("read pipeline history: %w", err)
	}
	defer rows.Close()
	page := PipelineHistoryPage{Events: make([]PipelineEvent, 0, limit)}
	for rows.Next() {
		var item PipelineEvent
		var phase sql.NullString
		if err := rows.Scan(&item.ID, &item.Stage, &item.Event, &item.Status, &phase, &item.AttemptID,
			&item.ErrorCode, &item.Retryability, &item.OccurredAt); err != nil {
			return PipelineHistoryPage{}, fmt.Errorf("scan pipeline event: %w", err)
		}
		item.Phase = nullStringPointer(phase)
		page.Events = append(page.Events, item)
	}
	if err := rows.Err(); err != nil {
		return PipelineHistoryPage{}, fmt.Errorf("iterate pipeline history: %w", err)
	}
	if len(page.Events) > limit {
		page.Events = page.Events[:limit]
		cursor := fmt.Sprintf("%d", page.Events[len(page.Events)-1].ID)
		page.NextCursor = &cursor
	}
	return page, nil
}
