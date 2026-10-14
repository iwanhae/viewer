package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// PipelineCounts describes blobs in a catalog state and their references.
// BlobCount is unique content, PhotoCount counts photo rows, and AlbumCount
// counts distinct albums referencing those blobs (so album counts overlap
// between categories).
type PipelineCounts struct {
	BlobCount  int64 `json:"blobCount"`
	PhotoCount int64 `json:"photoCount"`
	AlbumCount int64 `json:"albumCount"`
}

// AdminOverviewSnapshot is a consistent catalog-only snapshot for the admin
// overview. Raw-state maps retain states this version does not recognize.
type AdminOverviewSnapshot struct {
	Albums            int64
	Photos            int64
	Blobs             int64
	UnreferencedBlobs int64

	EncodingCategories     map[string]PipelineCounts
	EncodingSkippedReasons map[string]PipelineCounts
	EncodingRawStates      map[string]PipelineCounts

	EmbeddingCategories map[string]PipelineCounts
	EmbeddingRawStates  map[string]PipelineCounts
}

// AdminOverview reads every field in one short SQLite read transaction. It
// deliberately performs no vector-store or recommendation-service calls.
func (s *Store) AdminOverview(ctx context.Context) (AdminOverviewSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("begin admin overview snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var snapshot AdminOverviewSnapshot
	if err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM albums),
			(SELECT COUNT(*) FROM photos),
			(SELECT COUNT(*) FROM blobs),
			(SELECT COUNT(*) FROM blobs b WHERE NOT EXISTS (
				SELECT 1 FROM photos p WHERE p.hash = b.hash
			))`).Scan(
		&snapshot.Albums, &snapshot.Photos, &snapshot.Blobs, &snapshot.UnreferencedBlobs,
	); err != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("read admin overview library counts: %w", err)
	}

	var queryErr error
	snapshot.EncodingCategories, queryErr = queryAdminPipelineCounts(ctx, tx, `CASE b.encoding_status
		WHEN 'pending' THEN 'queued'
		WHEN 'leased' THEN 'processing'
		WHEN 'received' THEN 'validating'
		WHEN 'committing' THEN 'committing'
		WHEN 'done' THEN 'completed'
		WHEN 'skipped' THEN 'skipped'
		WHEN 'failed' THEN 'failed'
		ELSE 'unknown' END`, "")
	if queryErr != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("read admin overview encoding categories: %w", queryErr)
	}
	snapshot.EncodingSkippedReasons, queryErr = queryAdminPipelineCounts(ctx, tx,
		`CASE WHEN b.content_type = 'image/webp' THEN 'webp' ELSE 'nonWebp' END`,
		`WHERE b.encoding_status = 'skipped'`)
	if queryErr != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("read admin overview skipped encoding reasons: %w", queryErr)
	}
	snapshot.EncodingRawStates, queryErr = queryAdminPipelineCounts(ctx, tx, `b.encoding_status`, "")
	if queryErr != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("read admin overview encoding raw states: %w", queryErr)
	}

	embeddingCategory := `CASE
		WHEN b.embedding_status = 'pending' AND ` + embeddingClaimGatePredicate + ` THEN 'queued'
		WHEN b.embedding_status = 'pending' THEN 'blocked'
		WHEN b.embedding_status = 'processing' THEN 'processing'
		WHEN b.embedding_status = 'ready' THEN 'completed'
		WHEN b.embedding_status = 'failed' THEN 'failed'
		ELSE 'unknown' END`
	snapshot.EmbeddingCategories, queryErr = queryAdminPipelineCounts(ctx, tx, embeddingCategory, "")
	if queryErr != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("read admin overview embedding categories: %w", queryErr)
	}
	snapshot.EmbeddingRawStates, queryErr = queryAdminPipelineCounts(ctx, tx, `b.embedding_status`, "")
	if queryErr != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("read admin overview embedding raw states: %w", queryErr)
	}

	if err := tx.Commit(); err != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("finish admin overview snapshot: %w", err)
	}
	return snapshot, nil
}

// queryAdminPipelineCounts groups a fixed, internal SQL category expression.
// Every category query uses the same left join so unreferenced blobs remain in
// blob counts while contributing zero photos and albums.
func queryAdminPipelineCounts(ctx context.Context, tx *sql.Tx, categoryExpr, where string) (map[string]PipelineCounts, error) {
	query := `SELECT ` + categoryExpr + `, COUNT(DISTINCT b.hash), COUNT(p.hash), COUNT(DISTINCT p.album_id)
		FROM blobs b LEFT JOIN photos p ON p.hash = b.hash ` + where + `
		GROUP BY ` + categoryExpr + ` ORDER BY 1`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]PipelineCounts)
	for rows.Next() {
		var category string
		var pipelineCounts PipelineCounts
		if err := rows.Scan(&category, &pipelineCounts.BlobCount, &pipelineCounts.PhotoCount, &pipelineCounts.AlbumCount); err != nil {
			return nil, fmt.Errorf("scan pipeline category: %w", err)
		}
		counts[category] = pipelineCounts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pipeline categories: %w", err)
	}
	return counts, nil
}
