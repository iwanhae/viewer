package recommend

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
	"viewer/internal/models"
)

type LocalStore struct {
	path string
	db   *sql.DB
}

func OpenLocalStore(path string) (*LocalStore, error) {
	if path == "" {
		return nil, fmt.Errorf("store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA temp_store=MEMORY;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite pragmas: %w", err)
	}

	store := &LocalStore{path: path, db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := store.requeueRunning(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *LocalStore) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS albums_local (
			album_id TEXT PRIMARY KEY,
			original_filename TEXT NOT NULL,
			created_at TEXT NOT NULL,
			photo_count INTEGER NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS photos_local (
			image_id TEXT PRIMARY KEY,
			album_id TEXT NOT NULL,
			photo_index INTEGER NOT NULL,
			entry_name TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			ratio REAL NOT NULL,
			created_at TEXT NOT NULL,
			embedding_status TEXT NOT NULL,
			embedding_model TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			UNIQUE(album_id, photo_index)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_photos_album ON photos_local(album_id, photo_index);`,
		`CREATE TABLE IF NOT EXISTS embeddings_local (
			image_id TEXT PRIMARY KEY,
			dim INTEGER NOT NULL,
			vector_blob BLOB NOT NULL,
			norm REAL NOT NULL,
			model_id TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS ingest_jobs (
			image_id TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			not_before TEXT NOT NULL,
			running_by TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_pending ON ingest_jobs(status, not_before, attempts, updated_at);`,
		`CREATE TABLE IF NOT EXISTS store_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	return nil
}

func (s *LocalStore) requeueRunning() error {
	nowText := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`UPDATE ingest_jobs
		 SET status='pending', running_by='', started_at='', updated_at=?
		 WHERE status='running'`,
		nowText,
	)
	if err != nil {
		return fmt.Errorf("requeue running jobs: %w", err)
	}
	return nil
}

func (s *LocalStore) UpsertAlbum(idx *models.AlbumIndex) (int, error) {
	if idx == nil {
		return 0, fmt.Errorf("album is required")
	}
	nowText := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO albums_local(album_id, original_filename, created_at, photo_count, updated_at)
		 VALUES(?, ?, ?, ?, ?)
		 ON CONFLICT(album_id) DO UPDATE SET
		 	original_filename=excluded.original_filename,
		 	created_at=excluded.created_at,
		 	photo_count=excluded.photo_count,
		 	updated_at=excluded.updated_at`,
		idx.AlbumID,
		idx.OriginalFilename,
		idx.CreatedAt,
		idx.PhotoCount,
		nowText,
	); err != nil {
		return 0, fmt.Errorf("upsert album: %w", err)
	}

	enqueued := 0
	seen := make(map[string]struct{}, len(idx.Photos))
	for _, photo := range idx.Photos {
		id := imageID(idx.AlbumID, photo.I)
		seen[id] = struct{}{}
		if _, err := tx.Exec(
			`INSERT INTO photos_local(
				image_id, album_id, photo_index, entry_name, width, height, ratio, created_at, embedding_status, embedding_model, updated_at
			 ) VALUES(?, ?, ?, ?, ?, ?, ?, ?, 'pending', '', ?)
			 ON CONFLICT(image_id) DO UPDATE SET
			 	album_id=excluded.album_id,
			 	photo_index=excluded.photo_index,
			 	entry_name=excluded.entry_name,
			 	width=excluded.width,
			 	height=excluded.height,
			 	ratio=excluded.ratio,
			 	created_at=excluded.created_at,
			 	updated_at=excluded.updated_at`,
			id,
			idx.AlbumID,
			photo.I,
			photo.Name,
			photo.W,
			photo.H,
			photo.Ratio,
			idx.CreatedAt,
			nowText,
		); err != nil {
			return 0, fmt.Errorf("upsert photo: %w", err)
		}

		hasEmbedding, err := hasEmbeddingTx(tx, id)
		if err != nil {
			return 0, err
		}
		if !hasEmbedding {
			queued, err := enqueuePendingTx(tx, id, nowText)
			if err != nil {
				return 0, err
			}
			if queued {
				enqueued++
			}
		}
	}

	rows, err := tx.Query(`SELECT image_id FROM photos_local WHERE album_id=?`, idx.AlbumID)
	if err != nil {
		return 0, fmt.Errorf("list stale album photos: %w", err)
	}
	stale := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan stale photo id: %w", err)
		}
		if _, ok := seen[id]; !ok {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("iterate stale photo ids: %w", err)
	}
	rows.Close()

	for _, id := range stale {
		if _, err := tx.Exec(`DELETE FROM photos_local WHERE image_id=?`, id); err != nil {
			return 0, fmt.Errorf("delete stale photo: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM embeddings_local WHERE image_id=?`, id); err != nil {
			return 0, fmt.Errorf("delete stale embedding: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM ingest_jobs WHERE image_id=?`, id); err != nil {
			return 0, fmt.Errorf("delete stale job: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit upsert album: %w", err)
	}
	return enqueued, nil
}

func hasEmbeddingTx(tx *sql.Tx, imageIDValue string) (bool, error) {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM embeddings_local WHERE image_id=? LIMIT 1`, imageIDValue).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check embedding exists: %w", err)
	}
	return true, nil
}

func enqueuePendingTx(tx *sql.Tx, imageIDValue string, nowText string) (bool, error) {
	var status string
	var attempts int
	err := tx.QueryRow(`SELECT status, attempts FROM ingest_jobs WHERE image_id=?`, imageIDValue).Scan(&status, &attempts)
	if err == sql.ErrNoRows {
		_, execErr := tx.Exec(
			`INSERT INTO ingest_jobs(image_id, status, attempts, last_error, not_before, running_by, started_at, created_at, updated_at)
			 VALUES(?, 'pending', 0, '', ?, '', '', ?, ?)`,
			imageIDValue,
			nowText,
			nowText,
			nowText,
		)
		if execErr != nil {
			return false, fmt.Errorf("insert pending job: %w", execErr)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("load existing job: %w", err)
	}
	if status == "pending" || status == "running" {
		return false, nil
	}
	_, execErr := tx.Exec(
		`UPDATE ingest_jobs
		 SET status='pending', not_before=?, running_by='', started_at='', updated_at=?
		 WHERE image_id=?`,
		nowText,
		nowText,
		imageIDValue,
	)
	if execErr != nil {
		return false, fmt.Errorf("requeue failed job: %w", execErr)
	}
	_ = attempts
	return true, nil
}

func (s *LocalStore) EnqueueIfNeeded(albumID string, photoIndex int) error {
	id := imageID(albumID, photoIndex)
	nowText := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var one int
	err = tx.QueryRow(`SELECT 1 FROM photos_local WHERE image_id=? LIMIT 1`, id).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("photo metadata missing")
	}
	if err != nil {
		return fmt.Errorf("check photo exists: %w", err)
	}

	hasEmbedding, err := hasEmbeddingTx(tx, id)
	if err != nil {
		return err
	}
	if hasEmbedding {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit enqueue noop: %w", err)
		}
		return nil
	}
	if _, err := enqueuePendingTx(tx, id, nowText); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit enqueue: %w", err)
	}
	return nil
}

func (s *LocalStore) PendingJobsCount() int {
	return s.JobCounts().Pending
}

func (s *LocalStore) JobCounts() JobQueueCounts {
	rows, err := s.db.Query(`SELECT status, COUNT(1) FROM ingest_jobs GROUP BY status`)
	if err != nil {
		return JobQueueCounts{}
	}
	defer rows.Close()

	counts := JobQueueCounts{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			continue
		}
		counts.Total += count
		switch status {
		case "pending":
			counts.Pending = count
		case "running":
			counts.Running = count
		case "failed":
			counts.Failed = count
		}
	}
	return counts
}

func (s *LocalStore) BackfillProgress() BackfillProgress {
	return BackfillProgress{
		PhotosTotal:     s.countRows(`SELECT COUNT(1) FROM photos_local`),
		EmbeddingsTotal: s.countRows(`SELECT COUNT(1) FROM embeddings_local`),
		Queue:           s.JobCounts(),
	}
}

func (s *LocalStore) countRows(query string) int {
	var count int
	if err := s.db.QueryRow(query).Scan(&count); err != nil {
		return 0
	}
	return count
}

func (s *LocalStore) ClaimJobs(workerID string, limit int, now time.Time) []JobRecord {
	if limit <= 0 {
		return nil
	}
	nowText := now.UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		`SELECT image_id, status, attempts, last_error, not_before, updated_at, created_at, running_by, started_at
		 FROM ingest_jobs
		 WHERE status='pending' AND not_before <= ?
		 ORDER BY attempts ASC, updated_at ASC
		 LIMIT ?`,
		nowText,
		limit,
	)
	if err != nil {
		return nil
	}
	jobs := make([]JobRecord, 0, limit)
	for rows.Next() {
		var job JobRecord
		if err := rows.Scan(
			&job.ImageID,
			&job.Status,
			&job.Attempts,
			&job.LastError,
			&job.NotBefore,
			&job.UpdatedAt,
			&job.CreatedAt,
			&job.RunningBy,
			&job.StartedAt,
		); err != nil {
			rows.Close()
			return nil
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil
	}
	rows.Close()

	for idx := range jobs {
		job := jobs[idx]
		if _, err := tx.Exec(
			`UPDATE ingest_jobs SET status='running', running_by=?, started_at=?, updated_at=? WHERE image_id=?`,
			workerID,
			nowText,
			nowText,
			job.ImageID,
		); err != nil {
			return nil
		}
		job.Status = "running"
		job.RunningBy = workerID
		job.StartedAt = nowText
		job.UpdatedAt = nowText
		jobs[idx] = job
	}
	if err := tx.Commit(); err != nil {
		return nil
	}
	return jobs
}

func (s *LocalStore) MarkDone(imageIDValue string, vector []float32, modelID string) error {
	normalized, norm := normalizeVector(vector)
	blob := encodeVector(normalized)
	nowText := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var one int
	err = tx.QueryRow(`SELECT 1 FROM photos_local WHERE image_id=? LIMIT 1`, imageIDValue).Scan(&one)
	if err == sql.ErrNoRows {
		return fmt.Errorf("photo metadata missing")
	}
	if err != nil {
		return fmt.Errorf("check photo exists: %w", err)
	}

	_, err = tx.Exec(
		`INSERT INTO embeddings_local(image_id, dim, vector_blob, norm, model_id, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(image_id) DO UPDATE SET
		 	dim=excluded.dim,
		 	vector_blob=excluded.vector_blob,
		 	norm=excluded.norm,
		 	model_id=excluded.model_id,
		 	updated_at=excluded.updated_at`,
		imageIDValue,
		len(normalized),
		blob,
		norm,
		modelID,
		nowText,
		nowText,
	)
	if err != nil {
		return fmt.Errorf("upsert embedding: %w", err)
	}

	if _, err := tx.Exec(
		`UPDATE photos_local SET embedding_status='ready', embedding_model=?, updated_at=? WHERE image_id=?`,
		modelID,
		nowText,
		imageIDValue,
	); err != nil {
		return fmt.Errorf("update photo ready: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM ingest_jobs WHERE image_id=?`, imageIDValue); err != nil {
		return fmt.Errorf("delete completed job: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mark done: %w", err)
	}
	return nil
}

func (s *LocalStore) MarkFailed(imageIDValue string, errText string, maxRetries int) error {
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var attempts int
	err = tx.QueryRow(`SELECT attempts FROM ingest_jobs WHERE image_id=?`, imageIDValue).Scan(&attempts)
	if err == sql.ErrNoRows {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit mark failed noop: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load job attempts: %w", err)
	}
	attempts++

	if maxRetries <= 0 {
		maxRetries = 5
	}

	status := "pending"
	notBefore := now
	runningBy := ""
	startedAt := ""
	if attempts >= maxRetries {
		status = "failed"
		if _, err := tx.Exec(
			`UPDATE photos_local SET embedding_status='failed', updated_at=? WHERE image_id=?`,
			nowText,
			imageIDValue,
		); err != nil {
			return fmt.Errorf("update photo failed: %w", err)
		}
	} else {
		backoff := time.Duration(attempts*attempts) * time.Second
		notBefore = now.Add(backoff)
	}

	if _, err := tx.Exec(
		`UPDATE ingest_jobs
		 SET status=?, attempts=?, last_error=?, not_before=?, running_by=?, started_at=?, updated_at=?
		 WHERE image_id=?`,
		status,
		attempts,
		errText,
		notBefore.Format(time.RFC3339),
		runningBy,
		startedAt,
		nowText,
		imageIDValue,
	); err != nil {
		return fmt.Errorf("update failed job: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mark failed: %w", err)
	}
	return nil
}

func (s *LocalStore) LastError(imageIDValue string) string {
	var value string
	err := s.db.QueryRow(`SELECT last_error FROM ingest_jobs WHERE image_id=?`, imageIDValue).Scan(&value)
	if err != nil {
		return ""
	}
	return value
}

func (s *LocalStore) GetPhoto(albumID string, photoIndex int) (PhotoRecord, bool) {
	var rec PhotoRecord
	err := s.db.QueryRow(
		`SELECT image_id, album_id, photo_index, entry_name, width, height, ratio, created_at, embedding_status, embedding_model, updated_at
		 FROM photos_local WHERE album_id=? AND photo_index=?`,
		albumID,
		photoIndex,
	).Scan(
		&rec.ImageID,
		&rec.AlbumID,
		&rec.PhotoIndex,
		&rec.EntryName,
		&rec.Width,
		&rec.Height,
		&rec.Ratio,
		&rec.CreatedAt,
		&rec.EmbeddingStatus,
		&rec.EmbeddingModel,
		&rec.UpdatedAt,
	)
	if err != nil {
		return PhotoRecord{}, false
	}
	return rec, true
}

func (s *LocalStore) GetPhotoByID(id string) (PhotoRecord, bool) {
	var rec PhotoRecord
	err := s.db.QueryRow(
		`SELECT image_id, album_id, photo_index, entry_name, width, height, ratio, created_at, embedding_status, embedding_model, updated_at
		 FROM photos_local WHERE image_id=?`,
		id,
	).Scan(
		&rec.ImageID,
		&rec.AlbumID,
		&rec.PhotoIndex,
		&rec.EntryName,
		&rec.Width,
		&rec.Height,
		&rec.Ratio,
		&rec.CreatedAt,
		&rec.EmbeddingStatus,
		&rec.EmbeddingModel,
		&rec.UpdatedAt,
	)
	if err != nil {
		return PhotoRecord{}, false
	}
	return rec, true
}

func (s *LocalStore) GetEmbedding(albumID string, photoIndex int) (EmbeddingRecord, bool) {
	id := imageID(albumID, photoIndex)
	return s.GetEmbeddingByImageID(id)
}

func (s *LocalStore) GetEmbeddingByImageID(id string) (EmbeddingRecord, bool) {
	var rec EmbeddingRecord
	var blob []byte
	err := s.db.QueryRow(
		`SELECT image_id, dim, vector_blob, norm, model_id, updated_at, created_at
		 FROM embeddings_local WHERE image_id=?`,
		id,
	).Scan(
		&rec.ImageID,
		&rec.Dimensions,
		&blob,
		&rec.Norm,
		&rec.ModelID,
		&rec.UpdatedAt,
		&rec.CreatedAt,
	)
	if err != nil {
		return EmbeddingRecord{}, false
	}
	rec.Vector = decodeVector(blob)
	if rec.Dimensions == 0 {
		rec.Dimensions = len(rec.Vector)
	}
	return rec, true
}

func (s *LocalStore) PhotosSnapshot() map[string]PhotoRecord {
	rows, err := s.db.Query(`SELECT image_id, album_id, photo_index, entry_name, width, height, ratio, created_at, embedding_status, embedding_model, updated_at FROM photos_local`)
	if err != nil {
		return map[string]PhotoRecord{}
	}
	defer rows.Close()

	out := make(map[string]PhotoRecord)
	for rows.Next() {
		var rec PhotoRecord
		if err := rows.Scan(
			&rec.ImageID,
			&rec.AlbumID,
			&rec.PhotoIndex,
			&rec.EntryName,
			&rec.Width,
			&rec.Height,
			&rec.Ratio,
			&rec.CreatedAt,
			&rec.EmbeddingStatus,
			&rec.EmbeddingModel,
			&rec.UpdatedAt,
		); err != nil {
			continue
		}
		out[rec.ImageID] = rec
	}
	return out
}

func (s *LocalStore) EmbeddingsSnapshot() map[string]EmbeddingRecord {
	rows, err := s.db.Query(`SELECT image_id, dim, vector_blob, norm, model_id, updated_at, created_at FROM embeddings_local`)
	if err != nil {
		return map[string]EmbeddingRecord{}
	}
	defer rows.Close()

	out := make(map[string]EmbeddingRecord)
	for rows.Next() {
		var rec EmbeddingRecord
		var blob []byte
		if err := rows.Scan(
			&rec.ImageID,
			&rec.Dimensions,
			&blob,
			&rec.Norm,
			&rec.ModelID,
			&rec.UpdatedAt,
			&rec.CreatedAt,
		); err != nil {
			continue
		}
		rec.Vector = decodeVector(blob)
		out[rec.ImageID] = rec
	}
	return out
}

func (s *LocalStore) SetMeta(key string, value string) error {
	nowText := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.Exec(
		`INSERT INTO store_meta(key, value, updated_at)
		 VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key,
		value,
		nowText,
	)
	if err != nil {
		return fmt.Errorf("set meta: %w", err)
	}
	return nil
}

func (s *LocalStore) GetMeta(key string) string {
	var value string
	err := s.db.QueryRow(`SELECT value FROM store_meta WHERE key=?`, key).Scan(&value)
	if err != nil {
		return ""
	}
	return value
}

func encodeVector(values []float32) []byte {
	if len(values) == 0 {
		return nil
	}
	buf := make([]byte, len(values)*4)
	for i, value := range values {
		binary.LittleEndian.PutUint32(buf[i*4:(i+1)*4], math.Float32bits(value))
	}
	return buf
}

func decodeVector(blob []byte) []float32 {
	if len(blob) == 0 {
		return nil
	}
	count := len(blob) / 4
	out := make([]float32, count)
	for i := 0; i < count; i++ {
		bits := binary.LittleEndian.Uint32(blob[i*4 : (i+1)*4])
		out[i] = math.Float32frombits(bits)
	}
	return out
}
