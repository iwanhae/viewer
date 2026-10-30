package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// AdminIssueGroup is one current failure reason. FailureCount's dimension is
// explicit because stages count different entities and groups may overlap.
type AdminIssueGroup struct {
	Stage        string `json:"stage"`
	ErrorCode    string `json:"errorCode"`
	FailureCount int64  `json:"failureCount"`
	CountUnit    string `json:"countUnit"`
}

// AdminIssuesSnapshot contains a bounded reason catalog and the exact number
// of omitted groups. It reads current blob/album status and failed entries
// from only each album's latest extraction run.
type AdminIssuesSnapshot struct {
	Groups      []AdminIssueGroup
	TotalGroups int64
}

const maxAdminIssueGroups = 100

func (s *Store) AdminIssues(ctx context.Context) (AdminIssuesSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AdminIssuesSnapshot{}, fmt.Errorf("begin admin issues snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `WITH issue_groups AS (
		SELECT 'encoding' AS stage,
			CASE WHEN COALESCE(o.error_code, '') = '' THEN 'legacy_unknown' ELSE o.error_code END AS error_code,
			COUNT(*) AS failure_count, 'blobs' AS count_unit
		FROM blobs b
		LEFT JOIN blob_stage_observations o ON o.blob_hash=b.hash AND o.stage='encoding'
		WHERE b.encoding_status='failed'
		GROUP BY error_code
		UNION ALL
		SELECT 'embedding',
			CASE WHEN COALESCE(o.error_code, '') = '' THEN 'legacy_unknown' ELSE o.error_code END,
			COUNT(*), 'blobs'
		FROM blobs b
		LEFT JOIN blob_stage_observations o ON o.blob_hash=b.hash AND o.stage='embedding'
		WHERE b.embedding_status='failed'
		GROUP BY 2
		UNION ALL
		SELECT 'album_extraction', 'legacy_unknown', COUNT(*), 'albums'
		FROM albums WHERE status='FAILED'
		HAVING COUNT(*)>0
		UNION ALL
		SELECT 'extraction_entry', e.reason_code, COUNT(*), 'entries'
		FROM extraction_entry_outcomes e
		JOIN extraction_runs r ON r.id=e.run_id
		WHERE e.outcome='failed' AND r.status!='running' AND r.id=(SELECT MAX(latest.id) FROM extraction_runs latest WHERE latest.album_id=r.album_id)
		GROUP BY e.reason_code
	)
	SELECT stage, error_code, failure_count, count_unit
	FROM issue_groups
	ORDER BY failure_count DESC, stage ASC, error_code ASC LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, maxAdminIssueGroups+1)
	if err != nil {
		return AdminIssuesSnapshot{}, fmt.Errorf("read admin issue groups: %w", err)
	}
	groups := make([]AdminIssueGroup, 0, maxAdminIssueGroups+1)
	for rows.Next() {
		var group AdminIssueGroup
		if err := rows.Scan(&group.Stage, &group.ErrorCode, &group.FailureCount, &group.CountUnit); err != nil {
			_ = rows.Close()
			return AdminIssuesSnapshot{}, fmt.Errorf("scan admin issue group: %w", err)
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return AdminIssuesSnapshot{}, fmt.Errorf("iterate admin issue groups: %w", err)
	}
	if err := rows.Close(); err != nil {
		return AdminIssuesSnapshot{}, fmt.Errorf("close admin issue groups: %w", err)
	}

	var total int64
	countQuery := `SELECT
		(SELECT COUNT(*) FROM (SELECT COALESCE(NULLIF(o.error_code,''),'legacy_unknown') FROM blobs b
			LEFT JOIN blob_stage_observations o ON o.blob_hash=b.hash AND o.stage='encoding' WHERE b.encoding_status='failed' GROUP BY 1)) +
		(SELECT COUNT(*) FROM (SELECT COALESCE(NULLIF(o.error_code,''),'legacy_unknown') FROM blobs b
			LEFT JOIN blob_stage_observations o ON o.blob_hash=b.hash AND o.stage='embedding' WHERE b.embedding_status='failed' GROUP BY 1)) +
		(SELECT CASE WHEN COUNT(*)>0 THEN 1 ELSE 0 END FROM albums WHERE status='FAILED') +
		(SELECT COUNT(*) FROM (SELECT e.reason_code FROM extraction_entry_outcomes e JOIN extraction_runs r ON r.id=e.run_id
			WHERE e.outcome='failed' AND r.status!='running' AND r.id=(SELECT MAX(latest.id) FROM extraction_runs latest WHERE latest.album_id=r.album_id) GROUP BY e.reason_code))`
	if err := tx.QueryRowContext(ctx, countQuery).Scan(&total); err != nil {
		return AdminIssuesSnapshot{}, fmt.Errorf("count admin issue groups: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AdminIssuesSnapshot{}, fmt.Errorf("finish admin issues snapshot: %w", err)
	}
	if len(groups) > maxAdminIssueGroups {
		groups = groups[:maxAdminIssueGroups]
	}
	return AdminIssuesSnapshot{Groups: groups, TotalGroups: total}, nil
}

// AdminExtractionFailure is one failed entry from the latest recorded run of
// its album. Error detail is intentionally excluded; reason codes are safe.
type AdminExtractionFailure struct {
	AlbumID          string `json:"albumId"`
	OriginalFilename string `json:"originalFilename"`
	RunID            int64  `json:"runId"`
	Ordinal          int    `json:"ordinal"`
	Name             string `json:"name"`
	Candidate        bool   `json:"candidate"`
	ReasonCode       string `json:"reasonCode"`
}

func (s *Store) AdminLatestExtractionFailures(ctx context.Context, reasonCode, afterAlbumID string, afterOrdinal, limit int) ([]AdminExtractionFailure, error) {
	if limit < 1 || limit > maxAdminIssueGroups+1 {
		return nil, fmt.Errorf("admin extraction failure page size must be between 1 and %d", maxAdminIssueGroups+1)
	}
	query := `SELECT r.album_id, a.original_filename, r.id, e.ordinal, e.name, e.candidate, e.reason_code
		FROM extraction_runs r
		JOIN albums a ON a.id=r.album_id
		JOIN extraction_entry_outcomes e ON e.run_id=r.id
		WHERE r.status!='running' AND r.id=(SELECT MAX(latest.id) FROM extraction_runs latest WHERE latest.album_id=r.album_id)
		AND e.outcome='failed' AND e.reason_code=?
		AND (r.album_id>? OR (r.album_id=? AND e.ordinal>?))
		ORDER BY r.album_id ASC, e.ordinal ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, reasonCode, afterAlbumID, afterAlbumID, afterOrdinal, limit)
	if err != nil {
		return nil, fmt.Errorf("list latest extraction failures: %w", err)
	}
	defer rows.Close()
	items := make([]AdminExtractionFailure, 0, limit)
	for rows.Next() {
		var item AdminExtractionFailure
		if err := rows.Scan(&item.AlbumID, &item.OriginalFilename, &item.RunID, &item.Ordinal, &item.Name, &item.Candidate, &item.ReasonCode); err != nil {
			return nil, fmt.Errorf("scan latest extraction failure: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest extraction failures: %w", err)
	}
	return items, nil
}
