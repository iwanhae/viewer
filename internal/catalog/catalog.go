// Package catalog persists album, photo and image-blob metadata in a local
// SQLite database. S3 only holds the binary payloads (the staged upload zip and
// content-addressed image blobs); everything else lives here. Embedding
// vectors are the one exception: since migration 0003 they live in the
// external Qdrant collection, one point per photo, and SQLite keeps only the
// per-blob embedding bookkeeping (status, error, lease).
package catalog

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"viewer/internal/models"

	_ "modernc.org/sqlite"
	// The catalog no longer stores vectors, but a database restored from a
	// pre-0003 backup still contains the blob_embeddings vec0 virtual table,
	// and migration 0003's DROP TABLE only works while the sqlite-vec module
	// is registered on the connection. Keep this import for as long as
	// migration 0002 is in the chain.
	_ "modernc.org/sqlite/vec"
)

var (
	ErrAlbumNotFound = errors.New("album not found")
	ErrPhotoNotFound = errors.New("photo not found")
	ErrBlobNotFound  = errors.New("blob not found")
)

// AlbumStatus tracks where an album is in the upload/extract pipeline.
type AlbumStatus string

const (
	AlbumStatusQueued     AlbumStatus = "QUEUED"
	AlbumStatusProcessing AlbumStatus = "PROCESSING"
	AlbumStatusReady      AlbumStatus = "SUCCEEDED"
	AlbumStatusFailed     AlbumStatus = "FAILED"
)

// EmbeddingStatus tracks the embedding state of a stored image blob.
type EmbeddingStatus string

const (
	EmbeddingStatusPending    EmbeddingStatus = "pending"
	EmbeddingStatusProcessing EmbeddingStatus = "processing"
	EmbeddingStatusReady      EmbeddingStatus = "ready"
	EmbeddingStatusFailed     EmbeddingStatus = "failed"
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
// S3 object) no matter how many albums contain them. The embedding vector
// itself lives in the Qdrant collection; the row carries only its state.
type Blob struct {
	Hash            string
	SizeBytes       int64
	ContentType     string
	SourceRestored  bool
	EmbeddingStatus EmbeddingStatus
	EmbeddingError  string
	CreatedAt       string
}

// PhotoWithBlob is a photo joined with its blob row — the shape the vector
// store sync paths need: the photo's (album, idx) identity and dimensions plus
// the blob's embedding status.
type PhotoWithBlob struct {
	Photo Photo
	Blob  Blob
}

// EmbeddingCounts summarizes embedding coverage across distinct blobs. Pending
// is derived as Total-Ready-Failed on purpose: it means "not done yet", so
// blobs a worker is currently embedding keep counting toward it. The json tags
// exist for the admin stats payload, the one place this type is serialized.
type EmbeddingCounts struct {
	Total      int `json:"total"`
	Ready      int `json:"ready"`
	Failed     int `json:"failed"`
	Processing int `json:"processing"`
	Pending    int `json:"pending"`
}

// Store is a SQLite-backed metadata catalog.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite catalog at path, applying the
// versioned schema migrations up to this build's newest schema. Migration 0003
// moves every stored vector into sink before dropping SQLite's vector storage,
// so it can be nil only when there is nothing to upload; see
// migrateVectorsToQdrant. Open must block startup until it returns: the
// migration has to finish before the server accepts traffic.
func Open(path string, sink VectorSink) (*Store, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return nil, fmt.Errorf("catalog path is required")
	}
	if dir := filepath.Dir(clean); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create catalog dir: %w", err)
		}
	}

	// WAL lets the single writer and the readers proceed without blocking
	// each other, foreign_keys enforces the photo-to-album references, and
	// synchronous(NORMAL) is the setting WAL is designed for: commits cost no
	// fsync, and the 10s busy timeout rides out the remaining lock contention
	// instead of failing a request outright.
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

	if err := runMigrations(db, sink); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate catalog: %w", err)
	}
	// A running scoped operation owns no external lease; its last committed
	// target results are durable, and uncommitted target work rolls back. Make
	// such operations claimable by the single in-process executor after restart.
	var recoverySchema int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='admin_recovery_operations'`).Scan(&recoverySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("inspect scoped admin recovery schema: %w", err)
	}
	if recoverySchema == 1 {
		if _, err := db.Exec(`UPDATE admin_recovery_operations SET status='queued', updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE status='running'`); err != nil {
			db.Close()
			return nil, fmt.Errorf("resume scoped admin recovery operations: %w", err)
		}
	}
	// Only local leases belong to this process lifetime. External workers may
	// still be working across a server restart; preserve their ownership. This is runtime hygiene
	// that runs on every open, not a migration — a crash mid-flight must not
	// need a schema change to recover from.
	startupTx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("begin stale embedding reset: %w", err)
	}
	if err := lockCatalogWriter(context.Background(), startupTx); err != nil {
		_ = startupTx.Rollback()
		db.Close()
		return nil, fmt.Errorf("lock stale embedding reset: %w", err)
	}
	now := time.Now()
	if err := recoverJobRuns(context.Background(), startupTx); err != nil {
		_ = startupTx.Rollback()
		db.Close()
		return nil, fmt.Errorf("interrupt local job runs: %w", err)
	}
	// A finalization reservation belongs to the HTTP server's result-write
	// call, not to the external worker's lease. That call died with the old
	// process. Clear only its reservation so the worker can submit again;
	// preserve its status, owner, token and lease deadline exactly as stored.
	if _, err := startupTx.Exec(`UPDATE blobs SET embedding_finalizing='' WHERE embedding_owner='external' AND embedding_finalizing!=''`); err != nil {
		_ = startupTx.Rollback()
		db.Close()
		return nil, fmt.Errorf("clear abandoned embedding result reservations: %w", err)
	}
	for {
		staleHashes, err := queryBlobHashes(context.Background(), startupTx,
			`SELECT hash FROM blobs WHERE embedding_status='processing' AND embedding_owner='local' ORDER BY hash LIMIT 500`)
		if err != nil {
			_ = startupTx.Rollback()
			db.Close()
			return nil, fmt.Errorf("read stale embedding leases: %w", err)
		}
		if len(staleHashes) == 0 {
			break
		}
		for _, hash := range staleHashes {
			if _, err := startupTx.ExecContext(context.Background(), `UPDATE blobs SET embedding_status='pending', embedding_lease_until=0,
				embedding_owner='external', embedding_token='', embedding_finalizing=''
				WHERE hash=? AND embedding_status='processing' AND embedding_owner='local'`, hash); err != nil {
				_ = startupTx.Rollback()
				db.Close()
				return nil, fmt.Errorf("reset stale embedding lease: %w", err)
			}
			if _, err := observeStage(context.Background(), startupTx, hash, "embedding", "startup_reset", string(EmbeddingStatusPending), "", now, false, false, false, "", "unknown"); err != nil {
				_ = startupTx.Rollback()
				db.Close()
				return nil, fmt.Errorf("record stale embedding reset: %w", err)
			}
		}
	}
	if err := startupTx.Commit(); err != nil {
		db.Close()
		return nil, fmt.Errorf("commit stale embedding reset: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// BackupTo writes a consistent, self-contained snapshot of the catalog to path
// using VACUUM INTO. The snapshot includes everything committed to the WAL, so
// it does not depend on a checkpoint, and taking it does not block the
// embedding workers that write concurrently. The target must not exist:
// SQLite refuses to overwrite a backup file.
func (s *Store) BackupTo(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("backup path is required")
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?1`, path); err != nil {
		return fmt.Errorf("vacuum catalog into %s: %w", path, err)
	}
	return nil
}

// OpenReadOnly opens an existing catalog file for reading, without applying
// the schema or any migration. It is how the backup package reads a snapshot:
// a VACUUM INTO output is a complete, standalone database, and opening it
// read-only guarantees the snapshot file is never modified.
func OpenReadOnly(path string) (*Store, error) {
	clean := strings.TrimSpace(path)
	if clean == "" {
		return nil, fmt.Errorf("catalog path is required")
	}
	dsn := fmt.Sprintf("file:%s?mode=ro", clean)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open catalog read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db}, nil
}

// ChangeCount reports the connection's cumulative row-change counter
// (PRAGMA total_changes). Every INSERT, UPDATE and DELETE this process has
// run through the pooled connection bumps it, so two equal readings mean no
// write happened in between — the signal the backup finalizer uses to skip
// re-uploading an unchanged catalog. The counter lives on the single pooled
// connection, which stays open for the life of the process.
func (s *Store) ChangeCount(ctx context.Context) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&count); err != nil {
		return 0, fmt.Errorf("read catalog change count: %w", err)
	}
	return count, nil
}

// SnapshotAlbums lists the terminal-state albums (SUCCEEDED and FAILED) that
// a snapshot file records, opening it read-only. The backup finalizer takes
// its zip delete list from here, so the list describes exactly the state the
// uploaded backup covers — not whatever the live database says by delete
// time.
func (s *Store) SnapshotAlbums(ctx context.Context, path string) ([]Album, error) {
	reader, err := OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	albums := make([]Album, 0)
	for _, status := range []AlbumStatus{AlbumStatusReady, AlbumStatusFailed} {
		rows, err := reader.ListAlbumsByStatus(ctx, status)
		if err != nil {
			return nil, fmt.Errorf("list %s albums from the snapshot: %w", status, err)
		}
		albums = append(albums, rows...)
	}
	return albums, nil
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// CreateAlbum inserts a new album row. It is idempotent: an existing album with
// the same id is left untouched.
// writeAlbum validates and stamps album, inserts it, applies onConflict as the
// statement's ON CONFLICT clause, and names the operation errVerb in any error.
func (s *Store) writeAlbum(ctx context.Context, album Album, onConflict string, errVerb string) error {
	if strings.TrimSpace(album.ID) == "" {
		return fmt.Errorf("album id is required")
	}
	if album.Status == "" {
		album.Status = AlbumStatusQueued
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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`+onConflict,
		album.ID, album.OriginalFilename, album.SizeBytes, string(album.Status),
		album.SourceKey, album.PhotoCount, album.Error, album.CreatedAt, album.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("%s album %s: %w", errVerb, album.ID, err)
	}
	return nil
}

// CreateAlbum inserts a new album row. It is idempotent: an existing album with
// the same id is left untouched.
func (s *Store) CreateAlbum(ctx context.Context, album Album) error {
	return s.writeAlbum(ctx, album, `
		ON CONFLICT(id) DO NOTHING`, "create")
}

// UpsertAlbum creates the album when missing, otherwise refreshes the mutable
// upload fields while keeping existing progress untouched.
func (s *Store) UpsertAlbum(ctx context.Context, album Album) error {
	return s.writeAlbum(ctx, album, `
		ON CONFLICT(id) DO UPDATE SET
			original_filename = excluded.original_filename,
			size_bytes        = excluded.size_bytes,
			source_key        = excluded.source_key,
			updated_at        = excluded.updated_at`, "upsert")
}

func (s *Store) GetAlbum(ctx context.Context, albumID string) (*Album, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at
		FROM albums WHERE id = ?`, albumID)

	album, err := scanAlbum(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
	}
	if err != nil {
		return nil, fmt.Errorf("get album %s: %w", albumID, err)
	}
	return album, nil
}

// GetAlbumBySourceKey returns the album whose staged zip is sourceKey, which is
// how the upload scan tells an object it already knows from one to adopt.
func (s *Store) GetAlbumBySourceKey(ctx context.Context, sourceKey string) (*Album, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at
		FROM albums WHERE source_key = ? LIMIT 1`, sourceKey)

	album, err := scanAlbum(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: source_key=%s", ErrAlbumNotFound, sourceKey)
	}
	if err != nil {
		return nil, fmt.Errorf("get album by source key %s: %w", sourceKey, err)
	}
	return album, nil
}

func scanAlbum(row scanner) (*Album, error) {
	var album Album
	var status string
	if err := row.Scan(
		&album.ID, &album.OriginalFilename, &album.SizeBytes, &status, &album.SourceKey,
		&album.PhotoCount, &album.Error, &album.CreatedAt, &album.UpdatedAt,
	); err != nil {
		return nil, err
	}
	album.Status = AlbumStatus(status)
	return &album, nil
}

// ListAlbumsByStatus returns albums in the given status ordered by id.
func (s *Store) ListAlbumsByStatus(ctx context.Context, status AlbumStatus) ([]Album, error) {
	return s.queryAlbums(ctx, `SELECT id, original_filename, size_bytes, status, source_key, photo_count, error, created_at, updated_at FROM albums WHERE status = ? ORDER BY id`, string(status))
}

// AlbumSearchResult is a ready album matched by name search, joined with its
// cover photo (the image at index 0) when it has one.
type AlbumSearchResult struct {
	Album Album
	Cover *Photo
}

// SearchAlbumsByName returns ready albums whose original filename contains the
// query as a case-insensitive substring, newest first.
func (s *Store) SearchAlbumsByName(ctx context.Context, q string, limit int) ([]AlbumSearchResult, error) {
	normalized := strings.ToLower(strings.TrimSpace(q))
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.original_filename, a.size_bytes, a.status, a.source_key,
		       a.photo_count, a.error, a.created_at, a.updated_at,
		       p.idx, p.hash, p.width, p.height, p.ratio
		FROM albums a
		LEFT JOIN photos p ON p.album_id = a.id AND p.idx = 0
		WHERE a.status = ?
		  AND (? = '' OR instr(lower(a.original_filename), ?) > 0)
		ORDER BY a.created_at DESC, a.original_filename ASC, a.id ASC
		LIMIT ?`, string(AlbumStatusReady), normalized, normalized, limit)
	if err != nil {
		return nil, fmt.Errorf("search albums: %w", err)
	}
	defer rows.Close()

	results := make([]AlbumSearchResult, 0)
	for rows.Next() {
		var (
			album       Album
			status      string
			coverIdx    sql.NullInt64
			coverHash   sql.NullString
			coverWidth  sql.NullInt64
			coverHeight sql.NullInt64
			coverRatio  sql.NullFloat64
		)
		if err := rows.Scan(
			&album.ID, &album.OriginalFilename, &album.SizeBytes, &status, &album.SourceKey,
			&album.PhotoCount, &album.Error, &album.CreatedAt, &album.UpdatedAt,
			&coverIdx, &coverHash, &coverWidth, &coverHeight, &coverRatio,
		); err != nil {
			return nil, fmt.Errorf("scan album search row: %w", err)
		}
		album.Status = AlbumStatus(status)
		result := AlbumSearchResult{Album: album}
		if coverIdx.Valid {
			result.Cover = &Photo{
				AlbumID: album.ID,
				Index:   int(coverIdx.Int64),
				Hash:    coverHash.String,
				Width:   int(coverWidth.Int64),
				Height:  int(coverHeight.Int64),
				Ratio:   coverRatio.Float64,
			}
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate album search rows: %w", err)
	}
	return results, nil
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

// AlbumCover is a ready album with its cover photo (the image at index 0).
type AlbumCover struct {
	AlbumID   string
	CreatedAt string
	Cover     Photo
}

// ListReadyAlbumCovers returns every ready album together with its cover
// photo, ordered by album id. The latest feed ranks and pages over albums, so
// one photo per album is all it needs; a full photo scan here would load
// hundreds of thousands of rows on a large library every time the feed's
// snapshot expires.
func (s *Store) ListReadyAlbumCovers(ctx context.Context) ([]AlbumCover, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.created_at, p.album_id, p.idx, p.name, p.hash, p.width, p.height, p.ratio
		FROM albums a
		JOIN photos p ON p.album_id = a.id AND p.idx = 0
		WHERE a.status = ?
		ORDER BY a.id ASC`, string(AlbumStatusReady))
	if err != nil {
		return nil, fmt.Errorf("list ready album covers: %w", err)
	}
	defer rows.Close()

	covers := make([]AlbumCover, 0)
	for rows.Next() {
		var cover AlbumCover
		if err := rows.Scan(&cover.AlbumID, &cover.CreatedAt, &cover.Cover.AlbumID, &cover.Cover.Index, &cover.Cover.Name, &cover.Cover.Hash, &cover.Cover.Width, &cover.Cover.Height, &cover.Cover.Ratio); err != nil {
			return nil, fmt.Errorf("scan ready album cover: %w", err)
		}
		covers = append(covers, cover)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ready album covers: %w", err)
	}
	return covers, nil
}

// ListReadyAlbumPhotoCounts returns the id and photo count of every ready
// album that has photos, ordered by id. It feeds the random-feed sampler,
// which needs only the album pool rather than every photo row.
func (s *Store) ListReadyAlbumPhotoCounts(ctx context.Context) ([]models.AlbumPhotoCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, photo_count FROM albums
		WHERE status = ? AND photo_count > 0
		ORDER BY id ASC`, string(AlbumStatusReady))
	if err != nil {
		return nil, fmt.Errorf("list ready album photo counts: %w", err)
	}
	defer rows.Close()

	counts := make([]models.AlbumPhotoCount, 0)
	for rows.Next() {
		var count models.AlbumPhotoCount
		if err := rows.Scan(&count.AlbumID, &count.PhotoCount); err != nil {
			return nil, fmt.Errorf("scan ready album photo count: %w", err)
		}
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ready album photo counts: %w", err)
	}
	return counts, nil
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

// UpsertBlob records the source once. Later duplicate uploads leave stored
// metadata alone unless the S3 object had gone missing and was restored.
func (s *Store) UpsertBlob(ctx context.Context, blob Blob) error {
	if strings.TrimSpace(blob.Hash) == "" {
		return fmt.Errorf("blob hash is required")
	}
	if blob.ContentType == "" {
		blob.ContentType = "application/octet-stream"
	}
	now := nowRFC3339()
	encodingStatus := "pending"
	if blob.ContentType == "image/webp" {
		encodingStatus = "skipped"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return fmt.Errorf("lock blob upsert: %w", err)
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash=?)`, blob.Hash).Scan(&exists); err != nil {
		return fmt.Errorf("check blob %s: %w", blob.Hash, err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO blobs (hash, size_bytes, content_type, embedding_status, embedding_error, created_at, encoding_status)
		VALUES (?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			size_bytes=excluded.size_bytes, content_type=excluded.content_type,
			encoding_status=excluded.encoding_status, encoding_owner='external', encoding_token='', encoding_stage_key='', encoding_lease_until=0,
			encoding_error='', encoding_source_size_bytes=0
		WHERE ?`,
		blob.Hash, blob.SizeBytes, blob.ContentType, string(EmbeddingStatusPending), now, encodingStatus, blob.SourceRestored,
	)
	if err != nil {
		return fmt.Errorf("upsert blob %s: %w", blob.Hash, err)
	}
	if exists == 0 {
		for _, seed := range []struct{ stage, status string }{
			{"encoding", encodingStatus}, {"embedding", string(EmbeddingStatusPending)},
		} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO blob_stage_observations (blob_hash, stage, status, retryability)
				VALUES (?, ?, ?, 'unknown')`, blob.Hash, seed.stage, seed.status); err != nil {
				return fmt.Errorf("initialize %s observation for %s: %w", seed.stage, blob.Hash, err)
			}
		}
	} else if blob.SourceRestored {
		if _, err := observeStage(ctx, tx, blob.Hash, "encoding", "source_restored", encodingStatus, "", time.Now(), false, true, true, "", "unknown"); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit blob upsert %s: %w", blob.Hash, err)
	}
	return nil
}

func (s *Store) GetBlob(ctx context.Context, hash string) (*Blob, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT hash, size_bytes, content_type, embedding_status, embedding_error, created_at
		FROM blobs WHERE hash = ?`, hash)
	var blob Blob
	var status string
	err := row.Scan(
		&blob.Hash, &blob.SizeBytes, &blob.ContentType, &status,
		&blob.EmbeddingError, &blob.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, hash)
	}
	if err != nil {
		return nil, fmt.Errorf("get blob %s: %w", hash, err)
	}
	blob.EmbeddingStatus = EmbeddingStatus(status)
	return &blob, nil
}

// EmbeddingResult is one blob's embedding outcome as reported by a worker —
// either the in-process one or an external one calling the worker API. Only
// Ready and Failed are accepted here; claimable blobs are handled by claim, not
// by results.
type EmbeddingResult struct {
	Hash   string
	Status EmbeddingStatus
	Vector []float32
	Error  string
}

// Rejection reasons reported by ApplyEmbeddingResults. They are informational:
// a worker must re-claim rather than retry a rejected item.
const (
	EmbeddingRejectUnknownHash  = "unknown_hash"
	EmbeddingRejectNotClaimed   = "not_claimed"
	EmbeddingRejectInvalidState = "invalid_status"
	// EmbeddingRejectInvalidHash marks a result whose hash is blank: there is
	// no row it could name, and silently dropping the item would leave a
	// worker counting results that went nowhere.
	EmbeddingRejectInvalidHash = "invalid_hash"
	// EmbeddingRejectWrongDim marks a ready result whose vector does not fill
	// the model's fixed width. It is rejected per item rather than allowed to
	// fail the whole batch: one worker shipping a wrong-sized checkpoint must
	// not sink its peers' results. The recommend layer pre-validates the same
	// rule before uploading the vector, so this is the backstop for direct
	// catalog callers.
	EmbeddingRejectWrongDim = "wrong_dim"
	// EmbeddingRejectBadVector marks a ready result whose vector is numerically
	// unusable even at the right width: a non-finite entry or a zero norm ranks
	// as NaN and drags the whole neighbor ranking down with it. The recommend
	// layer rejects these before the catalog sees the result, so catalog itself
	// never produces this reason; the constant is here so the wire value has
	// one owner.
	EmbeddingRejectBadVector = "bad_vector"
	// EmbeddingRejectBadBase64 marks a results payload whose vector is not
	// valid base64. The worker API's decoder catches it before the catalog
	// sees the result, so catalog itself never produces this reason; the
	// constant is here so the wire value has one owner.
	EmbeddingRejectBadBase64 = "bad_base64"
)

// RejectedEmbedding is one result a write-back refused, with the reason.
type RejectedEmbedding struct {
	Hash   string
	Reason string
}

// ClaimPendingEmbeddings atomically moves up to limit blobs into the
// processing state and returns them. Pending blobs and processing blobs whose
// lease has already expired are both claimable, so a worker that dies mid-batch
// is not able to strand its blobs: the next claim reclaims them. Candidate
// selection, expiry observations, claim attempts, and updates share one
// serialized write transaction. Returned rows are re-sorted by created_at.
// Encoding runs independently and never affects embedding eligibility.
func (s *Store) ClaimPendingEmbeddings(ctx context.Context, limit int, leaseUntil time.Time) ([]Blob, error) {
	return s.ClaimExternalEmbeddings(ctx, limit, leaseUntil)
}

// ClaimExternalEmbeddings retains the legacy hash-only external worker contract.
func (s *Store) ClaimExternalEmbeddings(ctx context.Context, limit int, leaseUntil time.Time) ([]Blob, error) {
	claims, err := s.claimEmbeddings(ctx, limit, leaseUntil, "external")
	blobs := make([]Blob, 0, len(claims))
	for _, claim := range claims {
		blobs = append(blobs, claim.Blob)
	}
	return blobs, err
}

func (s *Store) claimEmbeddings(ctx context.Context, limit int, leaseUntil time.Time, owner string) ([]LocalEmbeddingClaim, error) {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin embedding claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return nil, fmt.Errorf("lock embedding claim: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.hash, b.size_bytes, b.content_type, b.created_at, b.embedding_status
		FROM blobs AS b
		WHERE b.embedding_finalizing='' AND (b.embedding_status = ? OR (b.embedding_status = ? AND b.embedding_lease_until < ?))
		ORDER BY b.created_at ASC LIMIT ?`,
		string(EmbeddingStatusPending), string(EmbeddingStatusProcessing), now.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("select blobs for embedding claim: %w", err)
	}
	type candidate struct {
		blob        Blob
		priorStatus EmbeddingStatus
	}
	candidates := make([]candidate, 0)
	for rows.Next() {
		var item candidate
		if err := rows.Scan(
			&item.blob.Hash, &item.blob.SizeBytes, &item.blob.ContentType, &item.blob.CreatedAt, &item.priorStatus,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan claimed blob: %w", err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate claimed blobs: %w", err)
	}
	_ = rows.Close()
	blobs := make([]LocalEmbeddingClaim, 0, len(candidates))
	for _, item := range candidates {
		if item.priorStatus == EmbeddingStatusProcessing {
			if _, err := observeStage(ctx, tx, item.blob.Hash, "embedding", "lease_expired", string(EmbeddingStatusPending), "", now, false, true, false, "", "unknown"); err != nil {
				return nil, err
			}
		}
		token := ""
		if owner == "local" {
			token = rand.Text()
		}
		update, err := tx.ExecContext(ctx, `UPDATE blobs SET embedding_status=?, embedding_lease_until=?, embedding_owner=?, embedding_token=?
			WHERE hash=? AND embedding_finalizing='' AND (embedding_status=? OR (embedding_status=? AND embedding_lease_until<?))`,
			string(EmbeddingStatusProcessing), leaseUntil.UnixMilli(), owner, token, item.blob.Hash,
			string(EmbeddingStatusPending), string(EmbeddingStatusProcessing), now.UnixMilli())
		if err != nil {
			return nil, fmt.Errorf("claim blob %s for embedding: %w", item.blob.Hash, err)
		}
		changed, err := update.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed != 1 {
			continue
		}
		if _, err := observeStage(ctx, tx, item.blob.Hash, "embedding", "claimed", string(EmbeddingStatusProcessing), "", now, true, false, false, "", "unknown"); err != nil {
			return nil, err
		}
		item.blob.EmbeddingStatus = EmbeddingStatusProcessing
		blobs = append(blobs, LocalEmbeddingClaim{Blob: item.blob, Token: token})
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].CreatedAt < blobs[j].CreatedAt })
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit embedding claim: %w", err)
	}
	return blobs, nil
}

// RenewEmbeddingLeases is the legacy external-only renewal helper. It cannot
// renew local owners. Hash-only external attempts cannot be
// distinguished after external-to-external reclaim (including for renewal).
func (s *Store) RenewEmbeddingLeases(ctx context.Context, hashes []string, leaseUntil time.Time) ([]string, error) {
	return s.updateEmbeddingLeases(ctx, hashes, func(builder *strings.Builder, args []any) []any {
		builder.WriteString(`
			UPDATE blobs SET embedding_lease_until = ?
			WHERE embedding_status = ? AND embedding_owner='external' AND hash IN (`)
		args = append(args, leaseUntil.UnixMilli(), string(EmbeddingStatusProcessing))
		return args
	})
}

// ReleaseEmbeddingClaims is the legacy external-only release helper. Local
// owners and finalizing rows are deliberately untouched.
func (s *Store) ReleaseEmbeddingClaims(ctx context.Context, hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	const chunkSize = 500
	now := time.Now()
	for start := 0; start < len(hashes); start += chunkSize {
		end := min(start+chunkSize, len(hashes))
		chunk := hashes[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
		args := []any{string(EmbeddingStatusPending), string(EmbeddingStatusProcessing)}
		for _, hash := range chunk {
			args = append(args, hash)
		}
		rows, err := tx.QueryContext(ctx, `UPDATE blobs SET embedding_status=?, embedding_lease_until=0, embedding_token=''
			WHERE embedding_status=? AND embedding_owner='external' AND embedding_finalizing='' AND hash IN (`+placeholders+`) RETURNING hash`, args...)
		if err != nil {
			return fmt.Errorf("release embedding claims: %w", err)
		}
		var changed []string
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil {
				_ = rows.Close()
				return err
			}
			changed = append(changed, hash)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		for _, hash := range changed {
			if _, err := observeStage(ctx, tx, hash, "embedding", "released", string(EmbeddingStatusPending), "", now, false, true, false, "", "unknown"); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// updateEmbeddingLeases runs the lease renewal, chunking the hash list so the
// IN clause stays far below SQLite's parameter limit. The
// statement RETURNs the hashes it actually touched, so a caller can tell a
// renewal that landed from one that raced a reclaim. Rows must be drained for
// RETURNING to run (QueryContext, not ExecContext), and the order is
// unspecified, so it is sorted for determinism.
func (s *Store) updateEmbeddingLeases(
	ctx context.Context,
	hashes []string,
	prefix func(*strings.Builder, []any) []any,
) ([]string, error) {
	renewed := make([]string, 0, len(hashes))
	const chunkSize = 500
	for start := 0; start < len(hashes); start += chunkSize {
		end := start + chunkSize
		if end > len(hashes) {
			end = len(hashes)
		}
		chunk := hashes[start:end]

		var builder strings.Builder
		args := prefix(&builder, nil)
		for i, hash := range chunk {
			if i > 0 {
				builder.WriteString(", ")
			}
			builder.WriteString("?")
			args = append(args, hash)
		}
		builder.WriteString(`) RETURNING hash`)
		rows, err := s.db.QueryContext(ctx, builder.String(), args...)
		if err != nil {
			return nil, fmt.Errorf("update embedding leases: %w", err)
		}
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan renewed lease: %w", err)
			}
			renewed = append(renewed, hash)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, fmt.Errorf("iterate renewed leases: %w", err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close renewed leases: %w", closeErr)
		}
	}
	sort.Strings(renewed)
	return renewed, nil
}

// ApplyEmbeddingResults is a bookkeeping-only legacy external helper, retained
// for catalog callers/tests without vector side effects. It cannot touch local
// owners or reservations. Callers with vector writes MUST instead use
// FinalizeExternalEmbeddingResults, which fences ownership before those writes.
// Expired external claims still accept late results; successive external
// attempts cannot be distinguished by the legacy hash-only contract.
func (s *Store) ApplyEmbeddingResults(ctx context.Context, results []EmbeddingResult) ([]string, []RejectedEmbedding, error) {
	return s.applyEmbeddingResults(ctx, results, "")
}

func (s *Store) applyEmbeddingResults(ctx context.Context, results []EmbeddingResult, reservation string) ([]string, []RejectedEmbedding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin embedding write-back: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	applied := make([]string, 0, len(results))
	rejected := make([]RejectedEmbedding, 0)
	for _, result := range results {
		hash := strings.TrimSpace(result.Hash)
		if hash == "" {
			rejected = append(rejected, RejectedEmbedding{Hash: result.Hash, Reason: EmbeddingRejectInvalidHash})
			continue
		}
		if result.Status != EmbeddingStatusReady && result.Status != EmbeddingStatusFailed {
			rejected = append(rejected, RejectedEmbedding{Hash: hash, Reason: EmbeddingRejectInvalidState})
			continue
		}
		if result.Status == EmbeddingStatusReady && len(result.Vector) != EmbeddingDim {
			rejected = append(rejected, RejectedEmbedding{Hash: hash, Reason: EmbeddingRejectWrongDim})
			continue
		}

		// A ready blob carries no error: keeping a stale message next to a
		// success would read as a contradiction everywhere the row is shown.
		errorText := ""
		if result.Status != EmbeddingStatusReady {
			errorText = truncateErrorText(result.Error)
		}

		ownership := ` AND embedding_owner='external' AND embedding_finalizing=''`
		args := []any{string(result.Status), errorText, hash, string(EmbeddingStatusProcessing)}
		if reservation != "" {
			ownership = ` AND embedding_finalizing=?`
			args = append(args, reservation)
		}
		update, err := tx.ExecContext(ctx, `
			UPDATE blobs
			SET embedding_status = ?, embedding_error = ?, embedding_lease_until = 0,
				embedding_owner='external', embedding_token='', embedding_finalizing=''
			WHERE hash = ? AND embedding_status = ?`+ownership, args...,
		)
		if err != nil {
			return nil, nil, fmt.Errorf("apply embedding result for %s: %w", hash, err)
		}
		affected, err := update.RowsAffected()
		if err != nil {
			return nil, nil, fmt.Errorf("count embedding result for %s: %w", hash, err)
		}
		if affected == 1 {
			if _, err := observeStage(ctx, tx, hash, "embedding", string(result.Status), string(result.Status), "", time.Now(), false, true, true,
				safeStageErrorCode("embedding", errorText), "unknown"); err != nil {
				return nil, nil, err
			}
			applied = append(applied, hash)
			continue
		}

		reason := EmbeddingRejectNotClaimed
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE hash = ?)`, hash).Scan(&exists); err != nil {
			return nil, nil, fmt.Errorf("probe blob %s: %w", hash, err)
		}
		if exists == 0 {
			reason = EmbeddingRejectUnknownHash
		}
		rejected = append(rejected, RejectedEmbedding{Hash: hash, Reason: reason})
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit embedding write-back: %w", err)
	}
	return applied, rejected, nil
}

// maxEmbeddingErrorBytes bounds the error text persisted per blob so one
// pathological worker cannot bloat the catalog with failure messages.
const maxEmbeddingErrorBytes = 512

// truncateErrorText cuts error text to maxEmbeddingErrorBytes without tearing
// a multi-byte rune in half: a torn suffix would come back out of SQLite as
// U+FFFD garbage in every API response that quotes the failure.
func truncateErrorText(text string) string {
	if len(text) <= maxEmbeddingErrorBytes {
		return text
	}
	cut := text[:maxEmbeddingErrorBytes]
	// Only the final rune can be torn by the cut, so at most utf8.UTFMax
	// retreats are ever needed.
	for i := 0; i < utf8.UTFMax && len(cut) > 0 && !utf8.ValidString(cut); i++ {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// EmbeddingCounts reports embedding coverage across distinct blobs.
func (s *Store) EmbeddingCounts(ctx context.Context) (EmbeddingCounts, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN embedding_status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN embedding_status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN embedding_status = ? THEN 1 ELSE 0 END), 0)
		FROM blobs`,
		string(EmbeddingStatusReady), string(EmbeddingStatusFailed), string(EmbeddingStatusProcessing))

	var counts EmbeddingCounts
	if err := row.Scan(&counts.Total, &counts.Ready, &counts.Failed, &counts.Processing); err != nil {
		return EmbeddingCounts{}, fmt.Errorf("embedding counts: %w", err)
	}
	counts.Pending = counts.Total - counts.Ready - counts.Failed
	return counts, nil
}

// EmbeddingCountsByAlbum reports embedding coverage across the distinct blobs
// one album references. A blob shared by several albums counts once per album,
// which is what makes the per-album progress the upload page shows meaningful.
func (s *Store) EmbeddingCountsByAlbum(ctx context.Context, albumID string) (EmbeddingCounts, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN b.embedding_status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN b.embedding_status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN b.embedding_status = ? THEN 1 ELSE 0 END), 0)
		FROM (SELECT DISTINCT hash FROM photos WHERE album_id = ?) p
		JOIN blobs b ON b.hash = p.hash`,
		string(EmbeddingStatusReady), string(EmbeddingStatusFailed), string(EmbeddingStatusProcessing), albumID)

	var counts EmbeddingCounts
	if err := row.Scan(&counts.Total, &counts.Ready, &counts.Failed, &counts.Processing); err != nil {
		return EmbeddingCounts{}, fmt.Errorf("album embedding counts: %w", err)
	}
	counts.Pending = counts.Total - counts.Ready - counts.Failed
	return counts, nil
}

// ListPhotoBlobPairsByHashes returns the photo/blob pairs whose blob hash is
// in hashes, ordered by album and index. The recommend write-back uses it to
// turn ready hashes into the photo points the vector store needs, so an empty
// input returns an empty result without touching the database.
func (s *Store) ListPhotoBlobPairsByHashes(ctx context.Context, hashes []string) ([]PhotoWithBlob, error) {
	pairs := make([]PhotoWithBlob, 0)
	// Chunked like the lease updates so the IN list stays far below SQLite's
	// parameter limit; duplicate hashes simply come back once per referencing
	// photo row.
	const chunkSize = 500
	for start := 0; start < len(hashes); start += chunkSize {
		end := min(start+chunkSize, len(hashes))
		chunk := hashes[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
		args := make([]any, len(chunk))
		for i, hash := range chunk {
			args[i] = hash
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT p.album_id, p.idx, p.name, p.hash, p.width, p.height, p.ratio,
			       b.hash, b.size_bytes, b.content_type, b.embedding_status, b.embedding_error, b.created_at
			FROM photos p JOIN blobs b ON b.hash = p.hash
			WHERE b.hash IN (`+placeholders+`)
			ORDER BY p.album_id ASC, p.idx ASC`, args...)
		if err != nil {
			return nil, fmt.Errorf("list photo blobs by hashes: %w", err)
		}
		chunkPairs, err := scanPhotoBlobPairs(rows)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, chunkPairs...)
	}
	return pairs, nil
}

// ListPhotoBlobPairsByAlbum returns one album's photos joined with their blobs.
func (s *Store) ListPhotoBlobPairsByAlbum(ctx context.Context, albumID string) ([]PhotoWithBlob, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.album_id, p.idx, p.name, p.hash, p.width, p.height, p.ratio,
		       b.hash, b.size_bytes, b.content_type, b.embedding_status, b.embedding_error, b.created_at
		FROM photos p JOIN blobs b ON b.hash = p.hash
		WHERE p.album_id = ?
		ORDER BY p.idx ASC`, albumID)
	if err != nil {
		return nil, fmt.Errorf("list photo blobs: %w", err)
	}
	return scanPhotoBlobPairs(rows)
}

// ReadyPairCount counts the photo/blob pairs whose blob embedding is ready —
// the number of points the vector store is expected to hold, one per photo.
func (s *Store) ReadyPairCount(ctx context.Context) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM photos p JOIN blobs b ON b.hash = p.hash
		WHERE b.embedding_status = ?`, string(EmbeddingStatusReady)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count ready photo pairs: %w", err)
	}
	return count, nil
}

// AlbumCounts reports the total number of albums and how many sit in each
// pipeline status. Both figures come out of one GROUP BY so the total can
// never disagree with its own breakdown the way two separate queries could,
// with albums changing status between them.
func (s *Store) AlbumCounts(ctx context.Context) (int64, map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM albums GROUP BY status`)
	if err != nil {
		return 0, nil, fmt.Errorf("count albums by status: %w", err)
	}
	defer rows.Close()

	byStatus := make(map[string]int64)
	var total int64
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return 0, nil, fmt.Errorf("scan album status count: %w", err)
		}
		byStatus[status] = count
		total += count
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("iterate album status counts: %w", err)
	}
	return total, byStatus, nil
}

// PhotoCount counts the photo rows: one per (album, idx) entry extraction
// produced, distinct from the album count and from the deduplicated blob
// count, because the same image can appear in several albums.
func (s *Store) PhotoCount(ctx context.Context) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM photos`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count photos: %w", err)
	}
	return count, nil
}

// BlobCount counts the blob rows, which is the unique-content count: identical
// image bytes share one blob no matter how many photos reference them.
func (s *Store) BlobCount(ctx context.Context) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count blobs: %w", err)
	}
	return count, nil
}

// ResetReadyEmbeddings flips every terminal embedding outcome (ready and
// failed) back to pending and returns how many rows moved. It is the recovery
// path for a lost vector store: the background and external workers pick the
// pending blobs up through the usual claim loop and rebuild the points, so no
// restart and no manual surgery is needed. Processing rows are deliberately
// excluded: a worker still holds that blob's lease and is about to write its
// result back, and flipping the row under it would race that write-back — the
// result would find a pending row, be rejected as not_claimed, and the work
// already spent on it would be thrown away only for another worker to redo it.
// Letting the in-flight batch finish costs nothing; its outcome is as good as
// a fresh one.
func (s *Store) ResetReadyEmbeddings(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin embedding reindex: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return 0, err
	}
	now := time.Now()
	var count int64
	for {
		hashes, err := queryBlobHashes(ctx, tx, `SELECT hash FROM blobs WHERE embedding_status IN (?, ?) ORDER BY hash LIMIT 500`,
			string(EmbeddingStatusReady), string(EmbeddingStatusFailed))
		if err != nil {
			return 0, fmt.Errorf("read embeddings for reindex: %w", err)
		}
		if len(hashes) == 0 {
			break
		}
		for _, hash := range hashes {
			result, err := tx.ExecContext(ctx, `UPDATE blobs SET embedding_status=?, embedding_error='', embedding_lease_until=0
				WHERE hash=? AND embedding_status IN (?, ?)`, string(EmbeddingStatusPending), hash,
				string(EmbeddingStatusReady), string(EmbeddingStatusFailed))
			if err != nil {
				return 0, err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			if changed == 1 {
				count++
				if _, err := observeStage(ctx, tx, hash, "embedding", "reindex", string(EmbeddingStatusPending), "", now, false, true, false, "", "unknown"); err != nil {
					return 0, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// blobHashScope narrows a blobs scan to an explicit hash set, the SQL half of
// the recover endpoint's optional hashes scope. An empty list means no
// narrowing at all — the whole-library behavior these recoveries have always
// had — so callers that arrive without a scope keep their existing blast
// radius. The clause is assembled from placeholders only; hash values always
// travel as bound statement arguments, never as interpolated SQL text.
func blobHashScope(hashes []string) (string, []any) {
	if len(hashes) == 0 {
		return "", nil
	}
	return " AND hash IN (" + placeholders(len(hashes)) + ")", stringArgs(hashes)
}

// ReleaseStuckEmbeddingClaims flips processing rows back to pending and
// returns how many rows moved. It is the runtime equivalent of the startup
// reset at the bottom of Open: a lease belongs to the lifetime of one process,
// so a worker that died mid-batch leaves its blobs stranded in processing until
// the next restart — this lets an operator hand them back without one. The
// error text is cleared along with the lease: a row going back into the queue
// must not carry a stale failure message next to a pending status.
// expiredOnly=true (the safe default) restricts the reset to rows whose lease
// deadline has already passed, leaving live leases alone; expiredOnly=false
// does the full startup-style reset for every processing row. Releasing a live
// lease is not fatal: the worker that held it finishes its work, but the
// write-back then finds a pending row and is rejected as not_claimed — the blob
// is simply re-claimed and re-embedded by whoever gets there next, so the only
// cost is wasted work. Only widen to expiredOnly=false once there is no worker
// left to finish the in-flight batch.
// Finalizing rows are always excluded, including with expiredOnly=false.
// A non-empty hashes list narrows the release to those blobs; an empty one
// keeps the historical whole-library sweep.
func (s *Store) ReleaseStuckEmbeddingClaims(ctx context.Context, expiredOnly bool, hashes []string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return 0, err
	}
	now := time.Now()
	event := "manual_release"
	if expiredOnly {
		event = "lease_expired"
	}
	var count int64
	scopeClause, scopeArgs := blobHashScope(hashes)
	selectQuery := `SELECT hash FROM blobs WHERE embedding_status=? AND embedding_finalizing=''` + scopeClause
	selectArgs := append([]any{string(EmbeddingStatusProcessing)}, scopeArgs...)
	updateQuery := `UPDATE blobs SET embedding_status=?, embedding_error='', embedding_lease_until=0, embedding_owner='external', embedding_token=''
		WHERE hash=? AND embedding_status=? AND embedding_finalizing=''`
	if expiredOnly {
		selectQuery += ` AND embedding_lease_until < ?`
		selectArgs = append(selectArgs, now.UnixMilli())
		updateQuery += ` AND embedding_lease_until < ?`
	}
	selectQuery += ` ORDER BY hash LIMIT 500`
	for {
		batch, err := queryBlobHashes(ctx, tx, selectQuery, selectArgs...)
		if err != nil {
			return 0, fmt.Errorf("read stuck embedding claims: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, hash := range batch {
			args := []any{string(EmbeddingStatusPending), hash, string(EmbeddingStatusProcessing)}
			if expiredOnly {
				args = append(args, now.UnixMilli())
			}
			result, err := tx.ExecContext(ctx, updateQuery, args...)
			if err != nil {
				return 0, err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			if changed == 1 {
				count++
				if _, err := observeStage(ctx, tx, hash, "embedding", event, string(EmbeddingStatusPending), "", now, false, true, false, "", "unknown"); err != nil {
					return 0, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// ResetFailedEmbeddings flips every failed embedding back to pending, clearing
// the error text and lease, and returns how many rows moved. It is the retry
// path for a blob whose embedding failed for a transient reason (a worker that
// had a bad model checkpoint, an unavailable encoder): the claim loop picks the
// pending blobs up and tries again. Ready rows must stay untouched — they are
// terminal successes whose vectors the store already holds, and re-embedding
// them would spend worker capacity reproducing work that is done — and
// processing rows are excluded for the same reason ResetReadyEmbeddings
// excludes them: a worker still holds that lease and flipping the row under it
// would race its in-flight write-back. A non-empty hashes list narrows the
// retry to those blobs; an empty one retries every failed row.
func (s *Store) ResetFailedEmbeddings(ctx context.Context, hashes []string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin failed embedding retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockCatalogWriter(ctx, tx); err != nil {
		return 0, err
	}
	now := time.Now()
	scopeClause, scopeArgs := blobHashScope(hashes)
	selectArgs := append([]any{string(EmbeddingStatusFailed)}, scopeArgs...)
	var count int64
	for {
		batch, err := queryBlobHashes(ctx, tx, `SELECT hash FROM blobs WHERE embedding_status=?`+scopeClause+` ORDER BY hash LIMIT 500`, selectArgs...)
		if err != nil {
			return 0, fmt.Errorf("read failed embeddings: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, hash := range batch {
			result, err := tx.ExecContext(ctx, `UPDATE blobs SET embedding_status=?, embedding_error='', embedding_lease_until=0
				WHERE hash=? AND embedding_status=?`, string(EmbeddingStatusPending), hash, string(EmbeddingStatusFailed))
			if err != nil {
				return 0, err
			}
			changed, err := result.RowsAffected()
			if err != nil {
				return 0, err
			}
			if changed == 1 {
				count++
				if _, err := observeStage(ctx, tx, hash, "embedding", "manual_retry", string(EmbeddingStatusPending), "", now, false, true, false, "", "unknown"); err != nil {
					return 0, err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// EmbeddingLeaseHealth is the diagnostic snapshot of the processing rows: how
// many there are, how many of those have already blown their lease deadline,
// and the earliest deadline still recorded (epoch ms; zero when there are no
// processing rows). It reads the pipeline's liveness out of one query:
//   - Processing > 0 with LeaseExpired == Processing means no worker is
//     claiming anything — every claim has expired and nobody picked the rows
//     back up, which is the "embedding queue is stalled" signal.
//   - Processing > 0 with LeaseExpired == 0 means workers hold live leases,
//     so the rows are claimed but not finishing: the write-back is likely
//     stuck upstream (the worker is alive but its result never lands).
type EmbeddingLeaseHealth struct {
	Processing        int64 `json:"processing"`
	LeaseExpired      int64 `json:"leaseExpired"`
	OldestLeaseUnixMs int64 `json:"oldestLeaseUnixMs"`
}

// EmbeddingLeaseHealth reports the processing-row lease picture described on
// EmbeddingLeaseHealth. COALESCE keeps an empty catalog scanning as zeros
// instead of NULLs, so the caller never has to special-case "no rows yet".
func (s *Store) EmbeddingLeaseHealth(ctx context.Context) (EmbeddingLeaseHealth, error) {
	var health EmbeddingLeaseHealth
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN embedding_lease_until < ? THEN 1 ELSE 0 END), 0),
			COALESCE(MIN(embedding_lease_until), 0)
		FROM blobs
		WHERE embedding_status = ?`,
		time.Now().UnixMilli(), string(EmbeddingStatusProcessing),
	).Scan(&health.Processing, &health.LeaseExpired, &health.OldestLeaseUnixMs)
	if err != nil {
		return EmbeddingLeaseHealth{}, fmt.Errorf("embedding lease health: %w", err)
	}
	return health, nil
}

// EmbeddingFailure is one distinct embedding failure reason with how many
// blobs carry it and one representative hash, so an operator can jump from the
// aggregate to a concrete broken row.
type EmbeddingFailure struct {
	Error      string `json:"error"`
	Count      int64  `json:"count"`
	SampleHash string `json:"sampleHash"`
}

// maxEmbeddingFailureReasons bounds EmbeddingFailureReasons' response. The
// dashboard shows a fixed handful of rows and failure reasons are naturally
// few, so a request larger than this is almost certainly a caller bug rather
// than a genuine need to page through error messages.
const maxEmbeddingFailureReasons = 20

// EmbeddingFailureReasons summarizes the failed rows grouped by their error
// text, most common first, with the smallest hash of each group as the sample
// (MIN makes the representative deterministic across identical groups). The
// limit is clamped into [1, maxEmbeddingFailureReasons] — callers are not
// trusted to pass a sane value, and a clamped answer beats an SQL error on the
// dashboard. The result is empty (non-nil) when nothing has failed.
func (s *Store) EmbeddingFailureReasons(ctx context.Context, limit int) ([]EmbeddingFailure, error) {
	if limit < 1 {
		limit = 1
	}
	if limit > maxEmbeddingFailureReasons {
		limit = maxEmbeddingFailureReasons
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT embedding_error, COUNT(*), MIN(hash) FROM blobs
		WHERE embedding_status = ?
		GROUP BY embedding_error
		ORDER BY COUNT(*) DESC
		LIMIT ?`, string(EmbeddingStatusFailed), limit)
	if err != nil {
		return nil, fmt.Errorf("embedding failure reasons: %w", err)
	}
	defer rows.Close()

	failures := make([]EmbeddingFailure, 0)
	for rows.Next() {
		var failure EmbeddingFailure
		if err := rows.Scan(&failure.Error, &failure.Count, &failure.SampleHash); err != nil {
			return nil, fmt.Errorf("scan embedding failure reason: %w", err)
		}
		failures = append(failures, failure)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate embedding failure reasons: %w", err)
	}
	return failures, nil
}

// scanPhotoBlobPairs drains a photo/blob join query, converting the raw
// embedding status string on the way.
func scanPhotoBlobPairs(rows *sql.Rows) ([]PhotoWithBlob, error) {
	defer rows.Close()

	pairs := make([]PhotoWithBlob, 0)
	for rows.Next() {
		var pair PhotoWithBlob
		var status string
		if err := rows.Scan(
			&pair.Photo.AlbumID, &pair.Photo.Index, &pair.Photo.Name, &pair.Photo.Hash,
			&pair.Photo.Width, &pair.Photo.Height, &pair.Photo.Ratio,
			&pair.Blob.Hash, &pair.Blob.SizeBytes, &pair.Blob.ContentType, &status,
			&pair.Blob.EmbeddingError, &pair.Blob.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan photo blob: %w", err)
		}
		pair.Blob.EmbeddingStatus = EmbeddingStatus(status)
		pairs = append(pairs, pair)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate photo blobs: %w", err)
	}
	return pairs, nil
}
