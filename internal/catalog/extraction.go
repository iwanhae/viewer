package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const ExtractionOutcomeBatchSize = 100

var (
	extractionURLPattern    = regexp.MustCompile(`(?i)\b(?:https?|s3)://[^\s"'<>]+`)
	extractionSecretPattern = regexp.MustCompile(`(?i)(^|[^a-z0-9])["']?([a-z0-9_.-]*(?:secret|token|api[_-]?key|access[_-]?key|client[_-]?secret|refresh[_-]?token|password|credential|signature|private[_-]?key|authorization|(?:^|[_-])key)(?:[_-][a-z0-9_.-]+)*)["']?(\s*[:=]\s*)(?:(?:bearer)\s+)?(?:"[^"]*"|'[^']*'|[^\s,;&]+)`)
	extractionBearerPattern = regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/-]+=*`)
	extractionReasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// ExtractionEntryOutcome is the immutable result for one central-directory
// entry. Ordinal, not name, identifies an entry: ZIP archives may repeat names.
type ExtractionEntryOutcome struct {
	RunID       int64  `json:"runId"`
	Ordinal     int    `json:"ordinal"`
	Name        string `json:"name"`
	Candidate   bool   `json:"candidate"`
	Outcome     string `json:"outcome"`
	ReasonCode  string `json:"reasonCode"`
	ErrorDetail string `json:"errorDetail"`
	PhotoIndex  *int   `json:"photoIndex"`
	Hash        string `json:"hash"`
}

// ExtractionRunSummary is the bounded overview of one append-only processing
// run. Count fields report persisted entry outcomes. TotalEntries and
// CandidateEntries are null unless every archive entry was recorded.
type ExtractionRunSummary struct {
	RunID                    int64   `json:"runId"`
	Status                   string  `json:"status"`
	StartedAt                string  `json:"startedAt"`
	CompletedAt              *string `json:"completedAt"`
	FailureReasonCode        string  `json:"failureReasonCode"`
	ErrorDetail              string  `json:"errorDetail"`
	EntriesComplete          bool    `json:"entriesComplete"`
	CountsProvisional        bool    `json:"countsProvisional"`
	EntriesRecorded          int64   `json:"entriesRecorded"`
	CandidateEntriesRecorded int64   `json:"candidateEntriesRecorded"`
	TotalEntries             *int64  `json:"totalEntries"`
	CandidateEntries         *int64  `json:"candidateEntries"`
	RegisteredCount          int64   `json:"registeredCount"`
	SkippedCount             int64   `json:"skippedCount"`
	FailedCount              int64   `json:"failedCount"`
}

// SafeExtractionErrorDetail applies conservative redaction and a byte bound
// before an error string is persisted or returned by the admin API. URLs are
// removed wholesale because signed URLs can contain credentials in any query
// parameter, including provider-specific names.
func SafeExtractionErrorDetail(text string) string {
	text = extractionURLPattern.ReplaceAllString(text, "[redacted URL]")
	text = extractionSecretPattern.ReplaceAllString(text, "$1$2$3[redacted]")
	text = extractionBearerPattern.ReplaceAllString(text, "Bearer [redacted]")
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	const maxDetailBytes = 256
	if len(text) > maxDetailBytes {
		text = text[:maxDetailBytes]
		for len(text) > 0 && !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text
}

func (s *Store) BeginExtractionRun(ctx context.Context, albumID string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO extraction_runs (album_id, started_at, status) VALUES (?, ?, 'running')`, albumID, nowRFC3339())
	if err != nil {
		return 0, fmt.Errorf("begin extraction run for album %s: %w", albumID, err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read extraction run id for album %s: %w", albumID, err)
	}
	return runID, nil
}

// RecordExtractionEntries appends one bounded batch in a transaction. Entry
// rows and the run's persisted counters commit together; duplicate ordinals are
// rejected rather than overwriting history.
func (s *Store) RecordExtractionEntries(ctx context.Context, runID int64, entries []ExtractionEntryOutcome) error {
	if len(entries) == 0 {
		return nil
	}
	if len(entries) > ExtractionOutcomeBatchSize {
		return fmt.Errorf("extraction outcome batch exceeds %d entries", ExtractionOutcomeBatchSize)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin extraction outcome batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var registered, skipped, failed, candidates int64
	for _, entry := range entries {
		if entry.Ordinal < 0 {
			return fmt.Errorf("invalid extraction outcome ordinal")
		}
		if entry.RunID != 0 && entry.RunID != runID {
			return fmt.Errorf("extraction outcome run id mismatch")
		}
		if entry.Outcome != "registered" && entry.Outcome != "skipped" && entry.Outcome != "failed" {
			return fmt.Errorf("invalid extraction outcome %q", entry.Outcome)
		}
		if (entry.Outcome == "registered") != (entry.PhotoIndex != nil && entry.Hash != "") {
			return fmt.Errorf("invalid photo link for extraction outcome")
		}
		if entry.Outcome != "registered" && entry.ReasonCode == "" {
			return fmt.Errorf("non-registered extraction outcome requires a reason code")
		}
		if entry.ReasonCode != "" && !extractionReasonPattern.MatchString(entry.ReasonCode) {
			return fmt.Errorf("invalid extraction reason code")
		}
		var photoIndex any
		if entry.PhotoIndex != nil {
			photoIndex = *entry.PhotoIndex
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO extraction_entry_outcomes
			(run_id, ordinal, name, candidate, outcome, reason_code, error_detail, photo_index, hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, entry.Ordinal, entry.Name, entry.Candidate, entry.Outcome, entry.ReasonCode,
			SafeExtractionErrorDetail(entry.ErrorDetail), photoIndex, entry.Hash); err != nil {
			return fmt.Errorf("append extraction outcome %d for run %d: %w", entry.Ordinal, runID, err)
		}
		if entry.Candidate {
			candidates++
		}
		switch entry.Outcome {
		case "registered":
			registered++
		case "skipped":
			skipped++
		case "failed":
			failed++
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE extraction_runs SET
		entries_recorded=entries_recorded+?, candidate_entries_recorded=candidate_entries_recorded+?,
		registered_count=registered_count+?, skipped_count=skipped_count+?, failed_count=failed_count+?
		WHERE id=? AND status='running'`, len(entries), candidates, registered, skipped, failed, runID)
	if err != nil {
		return fmt.Errorf("update extraction run counters %d: %w", runID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count extraction run update %d: %w", runID, err)
	}
	if changed != 1 {
		return fmt.Errorf("extraction run %d is not running", runID)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit extraction outcome batch for run %d: %w", runID, err)
	}
	return nil
}

func (s *Store) FinishExtractionRun(ctx context.Context, runID int64, status, reasonCode, detail string, entriesComplete bool) error {
	if status != "succeeded" && status != "failed" && status != "incomplete" {
		return fmt.Errorf("invalid terminal extraction status %q", status)
	}
	if reasonCode != "" && !extractionReasonPattern.MatchString(reasonCode) {
		return fmt.Errorf("invalid extraction failure reason code")
	}
	completed := nowRFC3339()
	result, err := s.db.ExecContext(ctx, `UPDATE extraction_runs SET completed_at=?, status=?, failure_reason_code=?, error_detail=?, entries_complete=?
		WHERE id=? AND status='running'`, completed, status, reasonCode, SafeExtractionErrorDetail(detail), entriesComplete, runID)
	if err != nil {
		return fmt.Errorf("finish extraction run %d: %w", runID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count extraction run finish %d: %w", runID, err)
	}
	if changed != 1 {
		return fmt.Errorf("extraction run %d is not running", runID)
	}
	return nil
}

const extractionRunSelect = `SELECT r.id, r.status, r.started_at, r.completed_at, r.failure_reason_code, r.error_detail,
	r.entries_complete, r.entries_recorded, r.candidate_entries_recorded, r.registered_count, r.skipped_count, r.failed_count
	FROM extraction_runs r`

func scanExtractionRun(row interface{ Scan(...any) error }) (ExtractionRunSummary, error) {
	var item ExtractionRunSummary
	var completed sql.NullString
	if err := row.Scan(&item.RunID, &item.Status, &item.StartedAt, &completed, &item.FailureReasonCode, &item.ErrorDetail,
		&item.EntriesComplete, &item.EntriesRecorded, &item.CandidateEntriesRecorded, &item.RegisteredCount, &item.SkippedCount, &item.FailedCount); err != nil {
		return ExtractionRunSummary{}, err
	}
	if completed.Valid {
		item.CompletedAt = &completed.String
	}
	item.CountsProvisional = item.Status == "running"
	if item.EntriesComplete {
		total := item.EntriesRecorded
		candidates := item.CandidateEntriesRecorded
		item.TotalEntries = &total
		item.CandidateEntries = &candidates
	}
	return item, nil
}
