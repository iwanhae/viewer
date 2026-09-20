// Package catalog persists album, photo and image-blob metadata in a local
// SQLite database. S3 only holds the binary payloads (the staged upload zip and
// content-addressed image blobs); everything else lives here.
package catalog

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrAlbumNotFound = errors.New("album not found")
	ErrPhotoNotFound = errors.New("photo not found")
	ErrBlobNotFound  = errors.New("blob not found")
)

// AlbumStatus tracks where an album is in the upload/extract pipeline.
type AlbumStatus string

const (
	AlbumStatusPending    AlbumStatus = "PENDING"
	AlbumStatusQueued     AlbumStatus = "QUEUED"
	AlbumStatusProcessing AlbumStatus = "PROCESSING"
	AlbumStatusReady      AlbumStatus = "READY"
	AlbumStatusFailed     AlbumStatus = "FAILED"
)

// EmbeddingStatus tracks the embedding state of a stored image blob.
type EmbeddingStatus string

const (
	EmbeddingStatusPending EmbeddingStatus = "pending"
	EmbeddingStatusReady   EmbeddingStatus = "ready"
	EmbeddingStatusFailed  EmbeddingStatus = "failed"
)

// Album is one uploaded zip and its extraction state.
type Album struct {
	ID               string
	OriginalFilename string
	SizeBytes        int64
	Status           AlbumStatus
	SourceKey        string
	PhotoCount       int
	Error            string
	CreatedAt        string
	UpdatedAt        string
}

// Photo is one image entry extracted from an album zip. Width/Height/Ratio are
// denormalized so album reads never need to join the blobs table.
type Photo struct {
	AlbumID string
	Index   int
	Name    string
	Hash    string
	Width   int
	Height  int
	Ratio   float64
}

// Blob is one content-addressed image payload stored in S3 under
// "blobs/<hash>". Identical image bytes share a single blob row (and a single
// S3 object) no matter how many albums contain them.
type Blob struct {
	Hash               string
	SizeBytes          int64
	ContentType        string
	EmbeddingStatus    EmbeddingStatus
	Embedding          []float32
	EmbeddingError     string
	EmbeddingUpdatedAt string
	CreatedAt          string
	UpdatedAt          string
}

// PhotoWithBlob is a photo joined with its blob row, used to rebuild the
// in-memory recommendation index.
type PhotoWithBlob struct {
	Photo Photo
	Blob  Blob
}

// EmbeddingCounts summarizes embedding coverage across distinct blobs.
type EmbeddingCounts struct {
	Total   int
	Ready   int
	Failed  int
	Pending int
}

// Store is a SQLite-backed metadata catalog.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS albums (
	id                TEXT PRIMARY KEY,
	original_filename TEXT NOT NULL,
	size_bytes        INTEGER NOT NULL DEFAULT 0,
	status            TEXT NOT NULL,
	source_key        TEXT NOT NULL DEFAULT '',
	photo_count       INTEGER NOT NULL DEFAULT 0,
	error             TEXT NOT NULL DEFAULT '',
	created_at        TEXT NOT NULL,
	updated_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_albums_status ON albums(status);

CREATE TABLE IF NOT EXISTS blobs (
	hash                 TEXT PRIMARY KEY,
	size_bytes           INTEGER NOT NULL,
	content_type         TEXT NOT NULL DEFAULT 'application/octet-stream',
	embedding_status     TEXT NOT NULL DEFAULT 'pending',
	embedding            BLOB,
	embedding_error      TEXT NOT NULL DEFAULT '',
	embedding_updated_at TEXT NOT NULL DEFAULT '',
	created_at           TEXT NOT NULL,
	updated_at           TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_blobs_embedding_status ON blobs(embedding_status);

CREATE TABLE IF NOT EXISTS photos (
	album_id TEXT NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
	idx      INTEGER NOT NULL,
	name     TEXT NOT NULL,
	hash     TEXT NOT NULL,
	width    INTEGER NOT NULL,
	height   INTEGER NOT NULL,
	ratio    REAL NOT NULL,
	PRIMARY KEY (album_id, idx)
);
CREATE INDEX IF NOT EXISTS idx_photos_hash ON photos(hash);
`

// Open opens (creating if needed) the SQLite catalog at path.
func Open(path string) (*Store, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return nil, fmt.Errorf("catalog path is required")
	}
	if dir := filepath.Dir(clean); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create catalog dir: %w", err)
		}
	}

	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)",
		clean,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open catalog: %w", err)
	}
	// A single connection keeps writes serialized and avoids SQLITE_BUSY
	// surprises from the connection pool.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply catalog schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// CreateAlbum inserts a new album row. It is idempotent: an existing album with
// the same id is left untouched.
func (s *Store) CreateAlbum(ctx context.Context, album Album) error {
	if strings.TrimSpace(album.ID) == "" {
		return fmt.Errorf("album id is required")
	}
	if album.Status == "" {
		album.Status = AlbumStatusPending
	}
	now := nowRFC3339()
	if album.CreatedAt == "" {
		album.CreatedAt = now
	}
	if album.UpdatedAt == "" {
		album.UpdatedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO albums (id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		album.ID, album.OriginalFilename, album.SizeBytes, string(album.Status),
		album.SourceKey, album.PhotoCount, album.Error, album.CreatedAt, album.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("create album %s: %w", album.ID, err)
	}
	return nil
}

// UpsertAlbum creates the album when missing, otherwise refreshes the mutable
// upload fields while keeping existing progress untouched.
func (s *Store) UpsertAlbum(ctx context.Context, album Album) error {
	if strings.TrimSpace(album.ID) == "" {
		return fmt.Errorf("album id is required")
	}
	if album.Status == "" {
		album.Status = AlbumStatusPending
	}
	now := nowRFC3339()
	if album.CreatedAt == "" {
		album.CreatedAt = now
	}
	if album.UpdatedAt == "" {
		album.UpdatedAt = now
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO albums (id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			original_filename = excluded.original_filename,
			size_bytes        = excluded.size_bytes,
			source_key        = excluded.source_key,
			updated_at        = excluded.updated_at`,
		album.ID, album.OriginalFilename, album.SizeBytes, string(album.Status),
		album.SourceKey, album.PhotoCount, album.Error, album.CreatedAt, album.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert album %s: %w", album.ID, err)
	}
	return nil
}

func (s *Store) GetAlbum(ctx context.Context, albumID string) (*Album, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at
		FROM albums WHERE id = ?`, albumID)

	var album Album
	var status string
	err := row.Scan(
		&album.ID, &album.OriginalFilename, &album.SizeBytes, &status, &album.SourceKey,
		&album.PhotoCount, &album.Error, &album.CreatedAt, &album.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
	}
	if err != nil {
		return nil, fmt.Errorf("get album %s: %w", albumID, err)
	}
	album.Status = AlbumStatus(status)
	return &album, nil
}

// ListAlbums returns every album ordered by id.
func (s *Store) ListAlbums(ctx context.Context) ([]Album, error) {
	return s.queryAlbums(ctx, `SELECT id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at FROM albums ORDER BY id`)
}

// ListAlbumsByStatus returns albums in the given status ordered by id.
func (s *Store) ListAlbumsByStatus(ctx context.Context, status AlbumStatus) ([]Album, error) {
	return s.queryAlbums(ctx, `SELECT id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at FROM albums WHERE status = ? ORDER BY id`, string(status))
}

// SearchAlbumsByNamePrefix returns ready albums whose original filename starts
// with prefix (case-insensitive), newest first.
func (s *Store) SearchAlbumsByNamePrefix(ctx context.Context, prefix string, limit int) ([]Album, error) {
	if limit <= 0 {
		limit = 20
	}
	normalized := strings.ToLower(strings.TrimSpace(prefix))
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at
		FROM albums
		WHERE status = ?
		  AND (? = '' OR substr(lower(original_filename), 1, length(?)) = ?)
		ORDER BY created_at DESC, original_filename ASC, id ASC
		LIMIT ?`, string(AlbumStatusReady), normalized, normalized, normalized, limit)
	if err != nil {
		return nil, fmt.Errorf("search albums: %w", err)
	}
	defer rows.Close()
	return scanAlbums(rows)
}

func (s *Store) queryAlbums(ctx context.Context, query string, args ...any) ([]Album, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list albums: %w", err)
	}
	defer rows.Close()
	return scanAlbums(rows)
}

func scanAlbums(rows *sql.Rows) ([]Album, error) {
	albums := make([]Album, 0)
	for rows.Next() {
		var album Album
		var status string
		if err := rows.Scan(
			&album.ID, &album.OriginalFilename, &album.SizeBytes, &status, &album.SourceKey,
			&album.PhotoCount, &album.Error, &album.CreatedAt, &album.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan album: %w", err)
		}
		album.Status = AlbumStatus(status)
		albums = append(albums, album)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate albums: %w", err)
	}
	return albums, nil
}

// SetAlbumStatus updates the pipeline status of an album.
func (s *Store) SetAlbumStatus(ctx context.Context, albumID string, status AlbumStatus, errorText string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE albums SET status = ?, error = ?, updated_at = ? WHERE id = ?`,
		string(status), errorText, nowRFC3339(), albumID)
	if err != nil {
		return fmt.Errorf("set album %s status: %w", albumID, err)
	}
	return nil
}

// MarkAlbumReady records a successful extraction.
func (s *Store) MarkAlbumReady(ctx context.Context, albumID string, photoCount int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE albums SET status = ?, error = '', photo_count = ?, updated_at = ? WHERE id = ?`,
		string(AlbumStatusReady), photoCount, nowRFC3339(), albumID)
	if err != nil {
		return fmt.Errorf("mark album %s ready: %w", albumID, err)
	}
	return nil
}

// DeletePhotos removes every photo of an album (used before re-extraction).
func (s *Store) DeletePhotos(ctx context.Context, albumID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM photos WHERE album_id = ?`, albumID); err != nil {
		return fmt.Errorf("delete photos for album %s: %w", albumID, err)
	}
	return nil
}

// InsertPhoto records one extracted zip entry.
func (s *Store) InsertPhoto(ctx context.Context, photo Photo) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO photos (album_id, idx, name, hash, width, height, ratio)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(album_id, idx) DO UPDATE SET
			name   = excluded.name,
			hash   = excluded.hash,
			width  = excluded.width,
			height = excluded.height,
			ratio  = excluded.ratio`,
		photo.AlbumID, photo.Index, photo.Name, photo.Hash, photo.Width, photo.Height, photo.Ratio,
	)
	if err != nil {
		return fmt.Errorf("insert photo %s:%d: %w", photo.AlbumID, photo.Index, err)
	}
	return nil
}

// PhotosByAlbum returns an album's photos ordered by index.
func (s *Store) PhotosByAlbum(ctx context.Context, albumID string) ([]Photo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT album_id, idx, name, hash, width, height, ratio
		FROM photos WHERE album_id = ? ORDER BY idx`, albumID)
	if err != nil {
		return nil, fmt.Errorf("list photos for album %s: %w", albumID, err)
	}
	defer rows.Close()

	photos := make([]Photo, 0)
	for rows.Next() {
		photo, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		photos = append(photos, photo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate photos: %w", err)
	}
	return photos, nil
}

// ListReadyAlbumPhotos returns the photos of every ready album ordered by
// album id and index. It feeds the feed snapshot builder in one query.
func (s *Store) ListReadyAlbumPhotos(ctx context.Context) ([]Photo, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.album_id, p.idx, p.name, p.hash, p.width, p.height, p.ratio
		FROM photos p JOIN albums a ON a.id = p.album_id
		WHERE a.status = ?
		ORDER BY p.album_id ASC, p.idx ASC`, string(AlbumStatusReady))
	if err != nil {
		return nil, fmt.Errorf("list ready album photos: %w", err)
	}
	defer rows.Close()

	photos := make([]Photo, 0)
	for rows.Next() {
		photo, err := scanPhoto(rows)
		if err != nil {
			return nil, err
		}
		photos = append(photos, photo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ready album photos: %w", err)
	}
	return photos, nil
}

// PhotoAt returns the photo stored at index within an album.
func (s *Store) PhotoAt(ctx context.Context, albumID string, index int) (*Photo, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT album_id, idx, name, hash, width, height, ratio
		FROM photos WHERE album_id = ? AND idx = ?`, albumID, index)
	photo, err := scanPhoto(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s:%d", ErrPhotoNotFound, albumID, index)
	}
	if err != nil {
		return nil, err
	}
	return &photo, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanPhoto(row scanner) (Photo, error) {
	var photo Photo
	if err := row.Scan(
		&photo.AlbumID, &photo.Index, &photo.Name, &photo.Hash,
		&photo.Width, &photo.Height, &photo.Ratio,
	); err != nil {
		return Photo{}, err
	}
	return photo, nil
}

// UpsertBlob records the existence of a content-addressed blob. It never
// clobbers an existing embedding.
func (s *Store) UpsertBlob(ctx context.Context, blob Blob) error {
	if strings.TrimSpace(blob.Hash) == "" {
		return fmt.Errorf("blob hash is required")
	}
	if blob.ContentType == "" {
		blob.ContentType = "application/octet-stream"
	}
	now := nowRFC3339()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO blobs (hash, size_bytes, content_type, embedding_status, embedding, embedding_error, embedding_updated_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, NULL, '', '', ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			size_bytes   = excluded.size_bytes,
			content_type = excluded.content_type,
			updated_at   = excluded.updated_at`,
		blob.Hash, blob.SizeBytes, blob.ContentType, string(EmbeddingStatusPending), now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert blob %s: %w", blob.Hash, err)
	}
	return nil
}

func (s *Store) GetBlob(ctx context.Context, hash string) (*Blob, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT hash, size_bytes, content_type, embedding_status, embedding, embedding_error, embedding_updated_at, created_at, updated_at
		FROM blobs WHERE hash = ?`, hash)
	var blob Blob
	var status string
	var vector []byte
	err := row.Scan(
		&blob.Hash, &blob.SizeBytes, &blob.ContentType, &status, &vector,
		&blob.EmbeddingError, &blob.EmbeddingUpdatedAt, &blob.CreatedAt, &blob.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, hash)
	}
	if err != nil {
		return nil, fmt.Errorf("get blob %s: %w", hash, err)
	}
	blob.EmbeddingStatus = EmbeddingStatus(status)
	blob.Embedding = DecodeVector(vector)
	return &blob, nil
}

// SetBlobEmbedding stores the embedding outcome for a blob.
func (s *Store) SetBlobEmbedding(ctx context.Context, hash string, status EmbeddingStatus, vector []float32, errorText string) error {
	now := nowRFC3339()
	_, err := s.db.ExecContext(ctx, `
		UPDATE blobs
		SET embedding_status = ?, embedding = ?, embedding_error = ?, embedding_updated_at = ?, updated_at = ?
		WHERE hash = ?`,
		string(status), EncodeVector(vector), errorText, now, now, hash)
	if err != nil {
		return fmt.Errorf("set blob %s embedding: %w", hash, err)
	}
	return nil
}

// ListBlobsAwaitingEmbedding returns blobs that have not been embedded yet.
// Failed blobs are deliberately excluded so the retry worker does not spin on
// permanently unembeddable images.
func (s *Store) ListBlobsAwaitingEmbedding(ctx context.Context, limit int) ([]Blob, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT hash, size_bytes, content_type, embedding_status, embedding, embedding_error, embedding_updated_at, created_at, updated_at
		FROM blobs
		WHERE embedding_status = ?
		ORDER BY created_at ASC
		LIMIT ?`, string(EmbeddingStatusPending), limit)
	if err != nil {
		return nil, fmt.Errorf("list blobs awaiting embedding: %w", err)
	}
	defer rows.Close()

	blobs := make([]Blob, 0)
	for rows.Next() {
		var blob Blob
		var status string
		var vector []byte
		if err := rows.Scan(
			&blob.Hash, &blob.SizeBytes, &blob.ContentType, &status, &vector,
			&blob.EmbeddingError, &blob.EmbeddingUpdatedAt, &blob.CreatedAt, &blob.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan blob: %w", err)
		}
		blob.EmbeddingStatus = EmbeddingStatus(status)
		blob.Embedding = DecodeVector(vector)
		blobs = append(blobs, blob)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate blobs: %w", err)
	}
	return blobs, nil
}

// EmbeddingCounts reports embedding coverage across distinct blobs.
func (s *Store) EmbeddingCounts(ctx context.Context) (EmbeddingCounts, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN embedding_status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN embedding_status = ? THEN 1 ELSE 0 END), 0)
		FROM blobs`,
		string(EmbeddingStatusReady), string(EmbeddingStatusFailed))

	var counts EmbeddingCounts
	if err := row.Scan(&counts.Total, &counts.Ready, &counts.Failed); err != nil {
		return EmbeddingCounts{}, fmt.Errorf("embedding counts: %w", err)
	}
	counts.Pending = counts.Total - counts.Ready - counts.Failed
	return counts, nil
}

// ListPhotoBlobPairs returns every photo joined with its blob. It is used to
// rebuild the in-memory recommendation index at startup.
func (s *Store) ListPhotoBlobPairs(ctx context.Context) ([]PhotoWithBlob, error) {
	return s.queryPhotoBlobPairs(ctx, `
		SELECT p.album_id, p.idx, p.name, p.hash, p.width, p.height, p.ratio,
		       b.hash, b.size_bytes, b.content_type, b.embedding_status, b.embedding, b.embedding_error, b.embedding_updated_at, b.created_at, b.updated_at
		FROM photos p JOIN blobs b ON b.hash = p.hash
		ORDER BY p.album_id ASC, p.idx ASC`)
}

// ListPhotoBlobPairsByAlbum returns one album's photos joined with their blobs.
func (s *Store) ListPhotoBlobPairsByAlbum(ctx context.Context, albumID string) ([]PhotoWithBlob, error) {
	return s.queryPhotoBlobPairs(ctx, `
		SELECT p.album_id, p.idx, p.name, p.hash, p.width, p.height, p.ratio,
		       b.hash, b.size_bytes, b.content_type, b.embedding_status, b.embedding, b.embedding_error, b.embedding_updated_at, b.created_at, b.updated_at
		FROM photos p JOIN blobs b ON b.hash = p.hash
		WHERE p.album_id = ?
		ORDER BY p.idx ASC`, albumID)
}

func (s *Store) queryPhotoBlobPairs(ctx context.Context, query string, args ...any) ([]PhotoWithBlob, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list photo blobs: %w", err)
	}
	defer rows.Close()

	pairs := make([]PhotoWithBlob, 0)
	for rows.Next() {
		var pair PhotoWithBlob
		var status string
		var vector []byte
		if err := rows.Scan(
			&pair.Photo.AlbumID, &pair.Photo.Index, &pair.Photo.Name, &pair.Photo.Hash,
			&pair.Photo.Width, &pair.Photo.Height, &pair.Photo.Ratio,
			&pair.Blob.Hash, &pair.Blob.SizeBytes, &pair.Blob.ContentType, &status, &vector,
			&pair.Blob.EmbeddingError, &pair.Blob.EmbeddingUpdatedAt, &pair.Blob.CreatedAt, &pair.Blob.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan photo blob: %w", err)
		}
		pair.Blob.EmbeddingStatus = EmbeddingStatus(status)
		pair.Blob.Embedding = DecodeVector(vector)
		pairs = append(pairs, pair)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate photo blobs: %w", err)
	}
	return pairs, nil
}

// EncodeVector serializes a float32 embedding as little-endian bytes.
func EncodeVector(vector []float32) []byte {
	if len(vector) == 0 {
		return nil
	}
	out := make([]byte, 4*len(vector))
	for i, value := range vector {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(value))
	}
	return out
}

// DecodeVector deserializes a little-endian float32 embedding.
func DecodeVector(raw []byte) []float32 {
	if len(raw) < 4 {
		return nil
	}
	count := len(raw) / 4
	vector := make([]float32, count)
	for i := 0; i < count; i++ {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return vector
}
