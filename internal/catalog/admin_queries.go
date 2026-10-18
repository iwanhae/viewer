package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// BlobCounts describes a set of unique catalog blobs. It is used by the
// library-wide overview, where relation counts are intentionally not computed.
type BlobCounts struct {
	BlobCount int64 `json:"blobCount"`
}

// PipelineCounts describes the exact blob and relation counts for a scoped
// album drilldown. AlbumCount is distinct within each category.
type PipelineCounts struct {
	BlobCount  int64 `json:"blobCount"`
	PhotoCount int64 `json:"photoCount"`
	AlbumCount int64 `json:"albumCount"`
}

// AdminOverviewSnapshot is a consistent catalog-only snapshot for the admin
// overview. Raw-state maps retain states this version does not recognize.
type AdminOverviewSnapshot struct {
	Albums int64
	Photos int64
	Blobs  int64

	EncodingCategories     map[string]BlobCounts
	EncodingSkippedReasons map[string]BlobCounts
	EncodingRawStates      map[string]BlobCounts

	EmbeddingCategories map[string]BlobCounts
	EmbeddingRawStates  map[string]BlobCounts
}

// These expressions are the canonical admin pipeline classifications. They
// intentionally stay fixed SQL fragments: overview totals and browse filters
// must use the same definitions, and no caller-controlled SQL is interpolated.
const adminEncodingCategoryExpr = `CASE b.encoding_status
	WHEN 'pending' THEN 'queued'
	WHEN 'leased' THEN 'processing'
	WHEN 'received' THEN 'validating'
	WHEN 'committing' THEN 'committing'
	WHEN 'done' THEN 'completed'
	WHEN 'skipped' THEN 'skipped'
	WHEN 'failed' THEN 'failed'
	ELSE 'unknown' END`

const adminEncodingSkippedReasonExpr = `CASE WHEN b.content_type = 'image/webp' THEN 'webp' ELSE 'nonWebp' END`

const adminEmbeddingCategoryExpr = `CASE
	WHEN b.embedding_status = 'pending' THEN 'queued'
	WHEN b.embedding_status = 'processing' THEN 'processing'
	WHEN b.embedding_status = 'ready' THEN 'completed'
	WHEN b.embedding_status = 'failed' THEN 'failed'
	ELSE 'unknown' END`

// AdminOverview reads every field in one short SQLite read transaction. It
// deliberately performs no vector-store or recommendation-service calls.
func (s *Store) AdminOverview(ctx context.Context) (AdminOverviewSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("begin admin overview snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var snapshot AdminOverviewSnapshot
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM albums), (SELECT COUNT(*) FROM photos)`).Scan(
		&snapshot.Albums, &snapshot.Photos,
	); err != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("read admin overview library counts: %w", err)
	}

	if err := readAdminBlobBreakdown(ctx, tx, &snapshot); err != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("read admin overview blob breakdown: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("finish admin overview snapshot: %w", err)
	}
	return snapshot, nil
}

// readAdminBlobBreakdown scans the blobs table once, grouping by all overview
// classifications. It intentionally never joins photos: each blob contributes
// exactly one to each stage/raw-state breakdown regardless of its references.
func readAdminBlobBreakdown(ctx context.Context, tx *sql.Tx, snapshot *AdminOverviewSnapshot) error {
	snapshot.EncodingCategories = make(map[string]BlobCounts)
	snapshot.EncodingSkippedReasons = make(map[string]BlobCounts)
	snapshot.EncodingRawStates = make(map[string]BlobCounts)
	snapshot.EmbeddingCategories = make(map[string]BlobCounts)
	snapshot.EmbeddingRawStates = make(map[string]BlobCounts)

	query := `SELECT ` + adminEncodingCategoryExpr + `,
		CASE WHEN b.encoding_status='skipped' THEN ` + adminEncodingSkippedReasonExpr + ` ELSE '' END,
		b.encoding_status, ` + adminEmbeddingCategoryExpr + `, b.embedding_status, COUNT(*)
		FROM blobs b
		GROUP BY 1, 2, 3, 4, 5
		ORDER BY 1, 2, 3, 4, 5`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var encodingCategory, skippedReason, encodingRaw, embeddingCategory, embeddingRaw string
		var count int64
		if err := rows.Scan(&encodingCategory, &skippedReason, &encodingRaw, &embeddingCategory, &embeddingRaw, &count); err != nil {
			return fmt.Errorf("scan overview blob group: %w", err)
		}
		snapshot.Blobs += count
		snapshot.EncodingCategories[encodingCategory] = addBlobCounts(snapshot.EncodingCategories[encodingCategory], count)
		if skippedReason != "" {
			snapshot.EncodingSkippedReasons[skippedReason] = addBlobCounts(snapshot.EncodingSkippedReasons[skippedReason], count)
		}
		snapshot.EncodingRawStates[encodingRaw] = addBlobCounts(snapshot.EncodingRawStates[encodingRaw], count)
		snapshot.EmbeddingCategories[embeddingCategory] = addBlobCounts(snapshot.EmbeddingCategories[embeddingCategory], count)
		snapshot.EmbeddingRawStates[embeddingRaw] = addBlobCounts(snapshot.EmbeddingRawStates[embeddingRaw], count)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate overview blob groups: %w", err)
	}
	return nil
}

func addBlobCounts(counts BlobCounts, count int64) BlobCounts {
	counts.BlobCount += count
	return counts
}
