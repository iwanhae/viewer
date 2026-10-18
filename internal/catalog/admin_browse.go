package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AdminPhotoFilters are the fixed filters supported by the admin photo list.
// Values are always bound as SQL parameters; category expressions are owned by
// this package and shared with AdminOverview.
type AdminPhotoFilters struct {
	Query                 string
	AlbumID               string
	Hash                  string
	Encoding              string
	Embedding             string
	EncodingRaw           string
	EmbeddingRaw          string
	EncodingErrorCode     string
	EmbeddingErrorCode    string
	EncodingLeaseExpired  *bool
	EmbeddingLeaseExpired *bool
	EncodingPhase         string
	SkippedReason         string
}

// AdminBlobFilters are the fixed filters supported by the admin blob list.
type AdminBlobFilters struct {
	AlbumID               string
	Hash                  string
	Encoding              string
	Embedding             string
	EncodingRaw           string
	EmbeddingRaw          string
	EncodingErrorCode     string
	EmbeddingErrorCode    string
	EncodingLeaseExpired  *bool
	EmbeddingLeaseExpired *bool
	EncodingPhase         string
	SkippedReason         string
	Unreferenced          *bool
}

// AdminAlbumFilters are the fixed filters supported by the admin album list.
type AdminAlbumFilters struct {
	Query  string
	Status string
}

// AdminPhoto is one photo row joined with its album and content-addressed blob
// state. State fields preserve the raw catalog value alongside the overview
// category so unknown future values remain inspectable. Missing blobs are
// marked explicitly and have null size/type metadata and empty pipeline states.
type AdminPhoto struct {
	AlbumID              string            `json:"albumId"`
	AlbumName            string            `json:"albumName"`
	Index                int               `json:"idx"`
	Name                 string            `json:"name"`
	Hash                 string            `json:"hash"`
	MissingBlob          bool              `json:"missingBlob"`
	Width                int               `json:"width"`
	Height               int               `json:"height"`
	SizeBytes            *int64            `json:"sizeBytes"`
	ContentType          *string           `json:"contentType"`
	EncodingRaw          string            `json:"encodingRaw"`
	Encoding             string            `json:"encoding"`
	EncodingError        string            `json:"encodingError"`
	EncodingErrorCode    string            `json:"encodingErrorCode"`
	EmbeddingRaw         string            `json:"embeddingRaw"`
	Embedding            string            `json:"embedding"`
	EmbeddingError       string            `json:"embeddingError"`
	EmbeddingErrorCode   string            `json:"embeddingErrorCode"`
	SkippedReason        string            `json:"skippedReason"`
	EncodingObservation  *StageObservation `json:"encodingObservation,omitempty"`
	EmbeddingObservation *StageObservation `json:"embeddingObservation,omitempty"`
}

// AdminBlob is a blob plus bounded relation counts. Individual photos are
// obtained through AdminPhotos with hash set, so a shared blob can never
// produce an unbounded detail response.
type AdminBlob struct {
	Hash                 string            `json:"hash"`
	SizeBytes            int64             `json:"sizeBytes"`
	ContentType          string            `json:"contentType"`
	EncodingRaw          string            `json:"encodingRaw"`
	Encoding             string            `json:"encoding"`
	EncodingError        string            `json:"encodingError"`
	EncodingErrorCode    string            `json:"encodingErrorCode"`
	EmbeddingRaw         string            `json:"embeddingRaw"`
	Embedding            string            `json:"embedding"`
	EmbeddingError       string            `json:"embeddingError"`
	EmbeddingErrorCode   string            `json:"embeddingErrorCode"`
	SkippedReason        string            `json:"skippedReason"`
	PhotoCount           int64             `json:"photoCount"`
	AlbumCount           int64             `json:"albumCount"`
	EncodingObservation  *StageObservation `json:"encodingObservation,omitempty"`
	EmbeddingObservation *StageObservation `json:"embeddingObservation,omitempty"`
}

// AdminAlbum is the bounded album-list projection. PhotoCount is counted from
// photo rows rather than trusting the denormalized extraction field.
type AdminAlbum struct {
	ID               string `json:"id"`
	OriginalFilename string `json:"originalFilename"`
	Status           string `json:"status"`
	Error            string `json:"error"`
	PhotoCount       int64  `json:"photoCount"`
}

// AdminEncodingBreakdown exposes relation-aware pipeline counts scoped to one album.
type AdminEncodingBreakdown struct {
	Categories     map[string]PipelineCounts `json:"categories"`
	SkippedReasons map[string]PipelineCounts `json:"skippedReasons"`
	RawStates      map[string]PipelineCounts `json:"rawStates"`
}

// AdminEmbeddingBreakdown exposes relation-aware pipeline counts scoped to one album.
type AdminEmbeddingBreakdown struct {
	Categories map[string]PipelineCounts `json:"categories"`
	RawStates  map[string]PipelineCounts `json:"rawStates"`
}

// AdminAlbumDetail adds exact pipeline breakdowns for blobs and photo rows
// referenced by one album.
type AdminAlbumDetail struct {
	ID                 string                  `json:"id"`
	OriginalFilename   string                  `json:"originalFilename"`
	Status             string                  `json:"status"`
	Error              string                  `json:"error"`
	PhotoCount         int64                   `json:"photoCount"`
	Encoding           AdminEncodingBreakdown  `json:"encoding"`
	Embedding          AdminEmbeddingBreakdown `json:"embedding"`
	MissingBlobPhotos  int64                   `json:"missingBlobPhotos"`
	ExtractionRecorded bool                    `json:"extractionRecorded"`
	LatestExtraction   *ExtractionRunSummary   `json:"latestExtraction"`
}

// AdminPhotos reads a keyset page in (album_id, idx) order. Callers request one
// extra row to determine whether to issue a continuation cursor.
func (s *Store) AdminPhotos(ctx context.Context, filters AdminPhotoFilters, afterAlbumID string, afterIndex, limit int) ([]AdminPhoto, error) {
	if limit < 1 || limit > 101 {
		return nil, fmt.Errorf("admin photo page size must be between 1 and 101")
	}
	var where []string
	args := make([]any, 0, 12)
	if filters.Query != "" {
		where = append(where, `(instr(lower(a.original_filename), lower(?)) > 0 OR instr(lower(p.name), lower(?)) > 0)`)
		args = append(args, filters.Query, filters.Query)
	}
	if filters.AlbumID != "" {
		where = append(where, `p.album_id = ?`)
		args = append(args, filters.AlbumID)
	}
	if filters.Hash != "" {
		where = append(where, `p.hash = ?`)
		args = append(args, filters.Hash)
	}
	appendAdminStateFilters(&where, &args, filters.Encoding, filters.Embedding, filters.EncodingRaw, filters.EmbeddingRaw,
		filters.EncodingErrorCode, filters.EmbeddingErrorCode, filters.EncodingLeaseExpired, filters.EmbeddingLeaseExpired, filters.EncodingPhase, filters.SkippedReason)
	if afterAlbumID != "" {
		where = append(where, `(p.album_id > ? OR (p.album_id = ? AND p.idx > ?))`)
		args = append(args, afterAlbumID, afterAlbumID, afterIndex)
	}
	query := adminPhotoSelect + adminWhere(where) + ` ORDER BY p.album_id ASC, p.idx ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list admin photos: %w", err)
	}
	defer rows.Close()
	photos := make([]AdminPhoto, 0, limit)
	for rows.Next() {
		photo, err := scanAdminPhoto(rows)
		if err != nil {
			return nil, fmt.Errorf("scan admin photo: %w", err)
		}
		photos = append(photos, photo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin photos: %w", err)
	}
	return photos, nil
}

const adminPhotoSelect = `SELECT p.album_id, a.original_filename, p.idx, p.name, p.hash, p.width, p.height,
	b.size_bytes, b.content_type, b.hash IS NULL, COALESCE(b.encoding_status, ''),
	CASE WHEN b.hash IS NULL THEN '' ELSE ` + adminEncodingCategoryExpr + ` END, COALESCE(b.encoding_error, ''),
	CASE WHEN b.encoding_status='failed' THEN COALESCE(NULLIF(eo.error_code,''),'legacy_unknown') ELSE '' END,
	COALESCE(b.embedding_status, ''), CASE WHEN b.hash IS NULL THEN '' ELSE ` + adminEmbeddingCategoryExpr + ` END, COALESCE(b.embedding_error, ''),
	CASE WHEN b.embedding_status='failed' THEN COALESCE(NULLIF(bo.error_code,''),'legacy_unknown') ELSE '' END,
	CASE WHEN b.encoding_status = 'skipped' THEN ` + adminEncodingSkippedReasonExpr + ` ELSE '' END
	FROM photos p JOIN albums a ON a.id = p.album_id LEFT JOIN blobs b ON b.hash = p.hash
	LEFT JOIN blob_stage_observations eo ON eo.blob_hash=b.hash AND eo.stage='encoding'
	LEFT JOIN blob_stage_observations bo ON bo.blob_hash=b.hash AND bo.stage='embedding'`

func scanAdminPhoto(row interface{ Scan(...any) error }) (AdminPhoto, error) {
	var photo AdminPhoto
	err := row.Scan(
		&photo.AlbumID, &photo.AlbumName, &photo.Index, &photo.Name, &photo.Hash, &photo.Width, &photo.Height,
		&photo.SizeBytes, &photo.ContentType, &photo.MissingBlob, &photo.EncodingRaw, &photo.Encoding, &photo.EncodingError,
		&photo.EncodingErrorCode, &photo.EmbeddingRaw, &photo.Embedding, &photo.EmbeddingError,
		&photo.EmbeddingErrorCode, &photo.SkippedReason,
	)
	return photo, err
}

// AdminPhotoAt returns one photo and its current pipeline state.
func (s *Store) AdminPhotoAt(ctx context.Context, albumID string, index int) (AdminPhoto, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AdminPhoto{}, fmt.Errorf("begin admin photo detail: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, adminPhotoSelect+` WHERE p.album_id = ? AND p.idx = ?`, albumID, index)
	photo, err := scanAdminPhoto(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminPhoto{}, fmt.Errorf("%w: %s:%d", ErrPhotoNotFound, albumID, index)
	}
	if err != nil {
		return AdminPhoto{}, fmt.Errorf("get admin photo %s:%d: %w", albumID, index, err)
	}
	if !photo.MissingBlob {
		observations, err := stageObservations(ctx, tx, photo.Hash)
		if err != nil {
			return AdminPhoto{}, err
		}
		photo.EncodingObservation = observationPointer(observations["encoding"])
		photo.EmbeddingObservation = observationPointer(observations["embedding"])
	}
	if err := tx.Commit(); err != nil {
		return AdminPhoto{}, fmt.Errorf("finish admin photo detail: %w", err)
	}
	return photo, nil
}

// AdminBlobs reads a keyset page in hash order, including one bounded pair of
// indexed relation-count subqueries per returned blob.
func (s *Store) AdminBlobs(ctx context.Context, filters AdminBlobFilters, afterHash string, limit int) ([]AdminBlob, error) {
	if limit < 1 || limit > 101 {
		return nil, fmt.Errorf("admin blob page size must be between 1 and 101")
	}
	where, args := adminBlobWhere(filters, afterHash)
	query := `SELECT b.hash, b.size_bytes, b.content_type, b.encoding_status, ` + adminEncodingCategoryExpr + `, b.encoding_error,
		b.embedding_status, ` + adminEmbeddingCategoryExpr + `, b.embedding_error,
		CASE WHEN b.encoding_status='failed' THEN COALESCE(NULLIF(eo.error_code,''),'legacy_unknown') ELSE '' END,
		CASE WHEN b.embedding_status='failed' THEN COALESCE(NULLIF(bo.error_code,''),'legacy_unknown') ELSE '' END,
		CASE WHEN b.encoding_status = 'skipped' THEN ` + adminEncodingSkippedReasonExpr + ` ELSE '' END,
		(SELECT COUNT(*) FROM photos p WHERE p.hash = b.hash),
		(SELECT COUNT(DISTINCT p.album_id) FROM photos p WHERE p.hash = b.hash)
		FROM blobs b
		LEFT JOIN blob_stage_observations eo ON eo.blob_hash=b.hash AND eo.stage='encoding'
		LEFT JOIN blob_stage_observations bo ON bo.blob_hash=b.hash AND bo.stage='embedding'` + adminWhere(where) + ` ORDER BY b.hash ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list admin blobs: %w", err)
	}
	defer rows.Close()
	blobs := make([]AdminBlob, 0, limit)
	for rows.Next() {
		blob, err := scanAdminBlob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan admin blob: %w", err)
		}
		blobs = append(blobs, blob)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin blobs: %w", err)
	}
	return blobs, nil
}

func adminBlobWhere(filters AdminBlobFilters, afterHash string) ([]string, []any) {
	var where []string
	args := make([]any, 0, 12)
	if filters.Hash != "" {
		where = append(where, `b.hash = ?`)
		args = append(args, filters.Hash)
	}
	if filters.AlbumID != "" {
		where = append(where, `EXISTS (SELECT 1 FROM photos f WHERE f.hash = b.hash AND f.album_id = ?)`)
		args = append(args, filters.AlbumID)
	}
	if filters.Unreferenced != nil {
		if *filters.Unreferenced {
			where = append(where, `NOT EXISTS (SELECT 1 FROM photos f WHERE f.hash = b.hash)`)
		} else {
			where = append(where, `EXISTS (SELECT 1 FROM photos f WHERE f.hash = b.hash)`)
		}
	}
	appendAdminStateFilters(&where, &args, filters.Encoding, filters.Embedding, filters.EncodingRaw, filters.EmbeddingRaw,
		filters.EncodingErrorCode, filters.EmbeddingErrorCode, filters.EncodingLeaseExpired, filters.EmbeddingLeaseExpired, filters.EncodingPhase, filters.SkippedReason)
	if afterHash != "" {
		where = append(where, `b.hash > ?`)
		args = append(args, afterHash)
	}
	return where, args
}

func scanAdminBlob(row interface{ Scan(...any) error }) (AdminBlob, error) {
	var blob AdminBlob
	err := row.Scan(
		&blob.Hash, &blob.SizeBytes, &blob.ContentType, &blob.EncodingRaw, &blob.Encoding, &blob.EncodingError,
		&blob.EmbeddingRaw, &blob.Embedding, &blob.EmbeddingError, &blob.EncodingErrorCode, &blob.EmbeddingErrorCode,
		&blob.SkippedReason,
		&blob.PhotoCount, &blob.AlbumCount,
	)
	return blob, err
}

// AdminBlobByHash returns blob metadata and relation counts; references remain
// available through the paged photo list instead of being embedded here.
func (s *Store) AdminBlobByHash(ctx context.Context, hash string) (AdminBlob, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AdminBlob{}, fmt.Errorf("begin admin blob detail: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	query := `SELECT b.hash, b.size_bytes, b.content_type, b.encoding_status, ` + adminEncodingCategoryExpr + `, b.encoding_error,
		b.embedding_status, ` + adminEmbeddingCategoryExpr + `, b.embedding_error,
		CASE WHEN b.encoding_status='failed' THEN COALESCE(NULLIF(eo.error_code,''),'legacy_unknown') ELSE '' END,
		CASE WHEN b.embedding_status='failed' THEN COALESCE(NULLIF(bo.error_code,''),'legacy_unknown') ELSE '' END,
		CASE WHEN b.encoding_status = 'skipped' THEN ` + adminEncodingSkippedReasonExpr + ` ELSE '' END,
		(SELECT COUNT(*) FROM photos p WHERE p.hash = b.hash),
		(SELECT COUNT(DISTINCT p.album_id) FROM photos p WHERE p.hash = b.hash)
		FROM blobs b LEFT JOIN blob_stage_observations eo ON eo.blob_hash=b.hash AND eo.stage='encoding'
		LEFT JOIN blob_stage_observations bo ON bo.blob_hash=b.hash AND bo.stage='embedding' WHERE b.hash = ?`
	blob, err := scanAdminBlob(tx.QueryRowContext(ctx, query, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return AdminBlob{}, fmt.Errorf("%w: %s", ErrBlobNotFound, hash)
	}
	if err != nil {
		return AdminBlob{}, fmt.Errorf("get admin blob %s: %w", hash, err)
	}
	observations, err := stageObservations(ctx, tx, hash)
	if err != nil {
		return AdminBlob{}, err
	}
	blob.EncodingObservation = observationPointer(observations["encoding"])
	blob.EmbeddingObservation = observationPointer(observations["embedding"])
	if err := tx.Commit(); err != nil {
		return AdminBlob{}, fmt.Errorf("finish admin blob detail: %w", err)
	}
	return blob, nil
}

func observationPointer(observation StageObservation) *StageObservation {
	return &observation
}

// AdminAlbums reads albums in id order. The correlated photo count uses the
// photos primary key (album_id, idx), avoiding an N+1 round-trip.
func (s *Store) AdminAlbums(ctx context.Context, filters AdminAlbumFilters, afterID string, limit int) ([]AdminAlbum, error) {
	if limit < 1 || limit > 101 {
		return nil, fmt.Errorf("admin album page size must be between 1 and 101")
	}
	var where []string
	args := make([]any, 0, 4)
	if filters.Query != "" {
		where = append(where, `instr(lower(a.original_filename), lower(?)) > 0`)
		args = append(args, filters.Query)
	}
	if filters.Status != "" {
		where = append(where, `a.status = ?`)
		args = append(args, filters.Status)
	}
	if afterID != "" {
		where = append(where, `a.id > ?`)
		args = append(args, afterID)
	}
	query := `SELECT a.id, a.original_filename, a.status, a.error,
		(SELECT COUNT(*) FROM photos p WHERE p.album_id = a.id)
		FROM albums a` + adminWhere(where) + ` ORDER BY a.id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list admin albums: %w", err)
	}
	defer rows.Close()
	albums := make([]AdminAlbum, 0, limit)
	for rows.Next() {
		var album AdminAlbum
		if err := rows.Scan(&album.ID, &album.OriginalFilename, &album.Status, &album.Error, &album.PhotoCount); err != nil {
			return nil, fmt.Errorf("scan admin album: %w", err)
		}
		albums = append(albums, album)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admin albums: %w", err)
	}
	return albums, nil
}

// AdminAlbumByID returns processing metadata and exact scoped counts for blobs
// and photos referenced by the album. All component reads share one snapshot.
func (s *Store) AdminAlbumByID(ctx context.Context, albumID string) (AdminAlbumDetail, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AdminAlbumDetail{}, fmt.Errorf("begin admin album detail: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var result AdminAlbumDetail
	err = tx.QueryRowContext(ctx, `SELECT a.id, a.original_filename, a.status, a.error,
		(SELECT COUNT(*) FROM photos p WHERE p.album_id = a.id),
		(SELECT COUNT(*) FROM photos p WHERE p.album_id = a.id AND NOT EXISTS (
			SELECT 1 FROM blobs b WHERE b.hash = p.hash
		)) FROM albums a WHERE a.id = ?`, albumID).
		Scan(&result.ID, &result.OriginalFilename, &result.Status, &result.Error, &result.PhotoCount, &result.MissingBlobPhotos)
	if errors.Is(err, sql.ErrNoRows) {
		return AdminAlbumDetail{}, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
	}
	if err != nil {
		return AdminAlbumDetail{}, fmt.Errorf("read admin album %s: %w", albumID, err)
	}
	result.Encoding, result.Embedding, err = queryAlbumPipelineBreakdowns(ctx, tx, albumID)
	if err != nil {
		return AdminAlbumDetail{}, fmt.Errorf("read album pipeline breakdowns: %w", err)
	}
	latestExtraction, err := scanExtractionRun(tx.QueryRowContext(ctx, extractionRunSelect+
		` WHERE r.id=(SELECT id FROM extraction_runs WHERE album_id=? ORDER BY id DESC LIMIT 1)`, albumID))
	if err == nil {
		result.ExtractionRecorded = true
		result.LatestExtraction = &latestExtraction
	} else if !errors.Is(err, sql.ErrNoRows) {
		return AdminAlbumDetail{}, fmt.Errorf("read latest album extraction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AdminAlbumDetail{}, fmt.Errorf("finish admin album detail: %w", err)
	}
	return result, nil
}

func queryAlbumPipelineBreakdowns(ctx context.Context, tx *sql.Tx, albumID string) (AdminEncodingBreakdown, AdminEmbeddingBreakdown, error) {
	encoding := AdminEncodingBreakdown{
		Categories: make(map[string]PipelineCounts), SkippedReasons: make(map[string]PipelineCounts), RawStates: make(map[string]PipelineCounts),
	}
	embedding := AdminEmbeddingBreakdown{
		Categories: make(map[string]PipelineCounts), RawStates: make(map[string]PipelineCounts),
	}
	query := `SELECT ` + adminEncodingCategoryExpr + `,
		CASE WHEN b.encoding_status = 'skipped' THEN ` + adminEncodingSkippedReasonExpr + ` ELSE '' END,
		b.encoding_status, ` + adminEmbeddingCategoryExpr + `, b.embedding_status,
		COUNT(DISTINCT b.hash), COUNT(p.hash), COUNT(DISTINCT p.album_id)
		FROM photos p JOIN blobs b ON b.hash = p.hash
		WHERE p.album_id = ? GROUP BY 1, 2, 3, 4, 5 ORDER BY 1, 2, 3, 4, 5`
	rows, err := tx.QueryContext(ctx, query, albumID)
	if err != nil {
		return AdminEncodingBreakdown{}, AdminEmbeddingBreakdown{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var encodingCategory, skippedReason, encodingRaw, embeddingCategory, embeddingRaw string
		var counts PipelineCounts
		if err := rows.Scan(&encodingCategory, &skippedReason, &encodingRaw, &embeddingCategory, &embeddingRaw,
			&counts.BlobCount, &counts.PhotoCount, &counts.AlbumCount); err != nil {
			return AdminEncodingBreakdown{}, AdminEmbeddingBreakdown{}, fmt.Errorf("scan album pipeline breakdown: %w", err)
		}
		// Grouping by both raw and normalized state can produce several rows
		// for one category (notably unknown); albumCount is distinct within
		// each category and therefore remains one while blob/photo counts add.
		addAlbumPipelineCounts(encoding.Categories, encodingCategory, counts)
		addAlbumPipelineCounts(encoding.RawStates, encodingRaw, counts)
		if skippedReason != "" {
			addAlbumPipelineCounts(encoding.SkippedReasons, skippedReason, counts)
		}
		addAlbumPipelineCounts(embedding.Categories, embeddingCategory, counts)
		addAlbumPipelineCounts(embedding.RawStates, embeddingRaw, counts)
	}
	if err := rows.Err(); err != nil {
		return AdminEncodingBreakdown{}, AdminEmbeddingBreakdown{}, fmt.Errorf("iterate album pipeline breakdowns: %w", err)
	}
	for _, category := range []string{"queued", "processing", "validating", "committing", "completed", "skipped", "failed", "unknown"} {
		if _, ok := encoding.Categories[category]; !ok {
			encoding.Categories[category] = PipelineCounts{}
		}
	}
	for _, reason := range []string{"webp", "nonWebp"} {
		if _, ok := encoding.SkippedReasons[reason]; !ok {
			encoding.SkippedReasons[reason] = PipelineCounts{}
		}
	}
	for _, category := range []string{"queued", "processing", "completed", "failed", "unknown"} {
		if _, ok := embedding.Categories[category]; !ok {
			embedding.Categories[category] = PipelineCounts{}
		}
	}
	return encoding, embedding, nil
}

func addAlbumPipelineCounts(destination map[string]PipelineCounts, key string, counts PipelineCounts) {
	value := destination[key]
	value.BlobCount += counts.BlobCount
	value.PhotoCount += counts.PhotoCount
	if counts.AlbumCount > 0 {
		value.AlbumCount = 1
	}
	destination[key] = value
}

func appendAdminStateFilters(where *[]string, args *[]any, encoding, embedding, encodingRaw, embeddingRaw,
	encodingErrorCode, embeddingErrorCode string, encodingLeaseExpired, embeddingLeaseExpired *bool,
	encodingPhase, skippedReason string) {
	if encoding != "" {
		*where = append(*where, `b.hash IS NOT NULL AND `+adminEncodingCategoryExpr+` = ?`)
		*args = append(*args, encoding)
	}
	if embedding != "" {
		*where = append(*where, `b.hash IS NOT NULL AND `+adminEmbeddingCategoryExpr+` = ?`)
		*args = append(*args, embedding)
	}
	if encodingRaw != "" {
		*where = append(*where, `b.encoding_status = ?`)
		*args = append(*args, encodingRaw)
	}
	if embeddingRaw != "" {
		*where = append(*where, `b.embedding_status = ?`)
		*args = append(*args, embeddingRaw)
	}
	if encodingErrorCode != "" {
		*where = append(*where, `b.encoding_status='failed' AND COALESCE(NULLIF(eo.error_code,''),'legacy_unknown') = ?`)
		*args = append(*args, encodingErrorCode)
	}
	if embeddingErrorCode != "" {
		*where = append(*where, `b.embedding_status='failed' AND COALESCE(NULLIF(bo.error_code,''),'legacy_unknown') = ?`)
		*args = append(*args, embeddingErrorCode)
	}
	if encodingLeaseExpired != nil {
		operator := `<`
		if !*encodingLeaseExpired {
			operator = `>=`
		}
		*where = append(*where, `b.encoding_status='leased' AND b.encoding_lease_until `+operator+` ?`)
		*args = append(*args, time.Now().UnixMilli())
	}
	if embeddingLeaseExpired != nil {
		operator := `<`
		if !*embeddingLeaseExpired {
			operator = `>=`
		}
		*where = append(*where, `b.embedding_status='processing' AND b.embedding_lease_until `+operator+` ?`)
		*args = append(*args, time.Now().UnixMilli())
	}
	if encodingPhase != "" {
		*where = append(*where, `b.encoding_status='committing' AND eo.phase = ?`)
		*args = append(*args, encodingPhase)
	}
	if skippedReason != "" {
		*where = append(*where, `b.encoding_status = 'skipped' AND `+adminEncodingSkippedReasonExpr+` = ?`)
		*args = append(*args, skippedReason)
	}
}

func adminWhere(conditions []string) string {
	if len(conditions) == 0 {
		return ""
	}
	return ` WHERE ` + strings.Join(conditions, ` AND `)
}
