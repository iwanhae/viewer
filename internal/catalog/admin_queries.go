package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// BlobCounts describes a set of unique catalog blobs. It is used by the
// library-wide overview, where relation counts are intentionally not computed.
type BlobCounts struct {
	BlobCount int64 `json:"blobCount"`
}

// AdminOverviewSnapshot is the consistent catalog-only snapshot behind the
// admin overview: library totals, unique-blob pipeline categories, and the
// integrity and lease figures the overview's health section renders. Everything
// is read inside one short transaction, so the page never mixes numbers from
// different catalog states.
type AdminOverviewSnapshot struct {
	Albums int64
	Photos int64
	Blobs  int64

	EncodingCategories  map[string]BlobCounts
	EmbeddingCategories map[string]BlobCounts

	// Health counts. ExpectedVectorPoints is the photo rows whose blob is
	// embedding-ready — the catalog's half of the vector-store count
	// comparison. UnreferencedBlobs and MissingBlobPhotos are the integrity
	// pair; the expired-lease counts flag rows a dead worker left behind.
	EncodingExpiredLeases  int64
	EmbeddingExpiredLeases int64
	UnreferencedBlobs      int64
	MissingBlobPhotos      int64
	ExpectedVectorPoints   int64
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

// AdminOverview reads every field in one short SQLite read transaction,
// freezing both stages' lease-expiry comparisons at one instant. It performs
// no vector-store or model reads; the admin service adds those on top, where
// their failures degrade the health section instead of the whole snapshot.
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

	if err := readAdminOverviewHealth(ctx, tx, &snapshot); err != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("read admin overview health counts: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return AdminOverviewSnapshot{}, fmt.Errorf("finish admin overview snapshot: %w", err)
	}
	return snapshot, nil
}

// readAdminBlobBreakdown scans the blobs table once, grouping by both stage
// classifications. It intentionally never joins photos: each blob contributes
// exactly one to each stage breakdown regardless of its references. The
// category CASE expressions fold unrecognized raw states into 'unknown', which
// is the only signal the dashboard keeps for them; the raw values stay
// reachable through the browse filters.
func readAdminBlobBreakdown(ctx context.Context, tx *sql.Tx, snapshot *AdminOverviewSnapshot) error {
	snapshot.EncodingCategories = make(map[string]BlobCounts)
	snapshot.EmbeddingCategories = make(map[string]BlobCounts)

	query := `SELECT ` + adminEncodingCategoryExpr + `, ` + adminEmbeddingCategoryExpr + `, COUNT(*)
		FROM blobs b
		GROUP BY 1, 2
		ORDER BY 1, 2`
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var encodingCategory, embeddingCategory string
		var count int64
		if err := rows.Scan(&encodingCategory, &embeddingCategory, &count); err != nil {
			return fmt.Errorf("scan overview blob group: %w", err)
		}
		snapshot.Blobs += count
		snapshot.EncodingCategories[encodingCategory] = addBlobCounts(snapshot.EncodingCategories[encodingCategory], count)
		snapshot.EmbeddingCategories[embeddingCategory] = addBlobCounts(snapshot.EmbeddingCategories[embeddingCategory], count)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate overview blob groups: %w", err)
	}
	return nil
}

// readAdminOverviewHealth lifts the five surviving health numbers in one
// scalar query, with the lease-expiry clock frozen to the caller's snapshot
// transaction.
func readAdminOverviewHealth(ctx context.Context, tx *sql.Tx, snapshot *AdminOverviewSnapshot) error {
	now := time.Now()
	return tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM photos p JOIN blobs b ON b.hash=p.hash WHERE b.embedding_status='ready'),
		(SELECT COUNT(*) FROM blobs b WHERE NOT EXISTS (SELECT 1 FROM photos p WHERE p.hash=b.hash)),
		(SELECT COUNT(*) FROM photos p WHERE NOT EXISTS (SELECT 1 FROM blobs b WHERE b.hash=p.hash)),
		(SELECT COUNT(*) FROM blobs WHERE encoding_status='leased' AND encoding_lease_until < ?),
		(SELECT COUNT(*) FROM blobs WHERE embedding_status='processing' AND embedding_lease_until < ?)`,
		now.UnixMilli(), now.UnixMilli()).Scan(&snapshot.ExpectedVectorPoints, &snapshot.UnreferencedBlobs, &snapshot.MissingBlobPhotos,
		&snapshot.EncodingExpiredLeases, &snapshot.EmbeddingExpiredLeases)
}

func addBlobCounts(counts BlobCounts, count int64) BlobCounts {
	counts.BlobCount += count
	return counts
}
