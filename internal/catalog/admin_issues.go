package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
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

// AdminDiagnosticsSnapshot is catalog-only. Every age is derived exclusively
// from recorded status_changed_at; absent or mismatched observations count as
// unknown instead of falling back to blob.created_at.
type AdminDiagnosticsSnapshot struct {
	ExpectedVectorPoints   int64
	UnreferencedBlobs      int64
	MissingBlobPhotos      int64
	EncodingExpiredLeases  int64
	EmbeddingExpiredLeases int64
	EncodingQueue          AdminQueueDiagnostics
	EmbeddingQueue         AdminQueueDiagnostics
	EmbeddingBlockedQueue  AdminQueueDiagnostics
	EncodingCommit         AdminEncodingCommitDiagnostics
}

type AdminQueueDiagnostics struct {
	Count                 int64
	OldestStatusChangedAt *string
	UnknownAgeCount       int64
}

type AdminEncodingCommitDiagnostics struct {
	CommittingCount      int64
	ReconcilingCount     int64
	OldestKnownStartedAt *string
	UnknownStartCount    int64
}

func (s *Store) AdminDiagnostics(ctx context.Context) (AdminDiagnosticsSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AdminDiagnosticsSnapshot{}, fmt.Errorf("begin admin diagnostics snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Freeze lease-expiry comparisons for both stages inside this catalog snapshot.
	now := time.Now()
	var result AdminDiagnosticsSnapshot
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM photos p JOIN blobs b ON b.hash=p.hash WHERE b.embedding_status='ready'),
		(SELECT COUNT(*) FROM blobs b WHERE NOT EXISTS (SELECT 1 FROM photos p WHERE p.hash=b.hash)),
		(SELECT COUNT(*) FROM photos p WHERE NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash=p.hash)),
		(SELECT COUNT(*) FROM blobs WHERE encoding_status='leased' AND encoding_lease_until < ?),
		(SELECT COUNT(*) FROM blobs WHERE embedding_status='processing' AND embedding_lease_until < ?)`,
		now.UnixMilli(), now.UnixMilli()).Scan(&result.ExpectedVectorPoints, &result.UnreferencedBlobs, &result.MissingBlobPhotos,
		&result.EncodingExpiredLeases, &result.EmbeddingExpiredLeases); err != nil {
		return AdminDiagnosticsSnapshot{}, fmt.Errorf("read admin diagnostics counts: %w", err)
	}
	result.EncodingQueue, err = queryAdminQueueDiagnostics(ctx, tx, "encoding", adminEncodingCategoryExpr, "queued")
	if err != nil {
		return AdminDiagnosticsSnapshot{}, err
	}
	result.EmbeddingQueue, err = queryAdminQueueDiagnostics(ctx, tx, "embedding", adminEmbeddingCategoryExpr, "queued")
	if err != nil {
		return AdminDiagnosticsSnapshot{}, err
	}
	result.EmbeddingBlockedQueue, err = queryAdminQueueDiagnostics(ctx, tx, "embedding", adminEmbeddingCategoryExpr, "blocked")
	if err != nil {
		return AdminDiagnosticsSnapshot{}, err
	}
	var started sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN o.phase='reconciling' THEN 1 ELSE 0 END), 0),
		(SELECT o2.started_at FROM blobs b2 JOIN blob_stage_observations o2 ON o2.blob_hash=b2.hash AND o2.stage='encoding'
			WHERE b2.encoding_status='committing' AND o2.started_at IS NOT NULL
			ORDER BY julianday(o2.started_at) ASC, b2.hash ASC LIMIT 1),
		COALESCE(SUM(CASE WHEN o.started_at IS NULL THEN 1 ELSE 0 END), 0)
		FROM blobs b LEFT JOIN blob_stage_observations o ON o.blob_hash=b.hash AND o.stage='encoding'
		WHERE b.encoding_status='committing'`).Scan(&result.EncodingCommit.CommittingCount,
		&result.EncodingCommit.ReconcilingCount, &started, &result.EncodingCommit.UnknownStartCount); err != nil {
		return AdminDiagnosticsSnapshot{}, fmt.Errorf("read encoding commit diagnostics: %w", err)
	}
	result.EncodingCommit.OldestKnownStartedAt = nullStringPointer(started)
	if err := tx.Commit(); err != nil {
		return AdminDiagnosticsSnapshot{}, fmt.Errorf("finish admin diagnostics snapshot: %w", err)
	}
	return result, nil
}

func queryAdminQueueDiagnostics(ctx context.Context, tx *sql.Tx, stage, categoryExpr, category string) (AdminQueueDiagnostics, error) {
	var result AdminQueueDiagnostics
	query := `SELECT COUNT(*),
		(SELECT o2.status_changed_at FROM blobs b JOIN blob_stage_observations o2 ON o2.blob_hash=b.hash AND o2.stage=?
			WHERE (` + categoryExpr + `)=? AND o2.status='pending' AND julianday(o2.status_changed_at) IS NOT NULL
			ORDER BY julianday(o2.status_changed_at) ASC, b.hash ASC LIMIT 1),
		COALESCE(SUM(CASE WHEN o.status='pending' AND julianday(o.status_changed_at) IS NOT NULL THEN 0 ELSE 1 END), 0)
		FROM blobs b LEFT JOIN blob_stage_observations o ON o.blob_hash=b.hash AND o.stage=?
		WHERE (` + categoryExpr + `)=?`
	var oldest sql.NullString
	if err := tx.QueryRowContext(ctx, query, stage, category, stage, category).Scan(&result.Count, &oldest, &result.UnknownAgeCount); err != nil {
		return AdminQueueDiagnostics{}, fmt.Errorf("read %s %s diagnostics: %w", stage, category, err)
	}
	result.OldestStatusChangedAt = nullStringPointer(oldest)
	return result, nil
}
