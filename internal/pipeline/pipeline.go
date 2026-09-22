// Package pipeline turns an uploaded zip into content-addressed image blobs.
//
// A single background worker downloads one staged zip at a time and walks its
// entries sequentially. For every image entry it computes a SHA-256 hash, gets
// the image dimensions, stores the raw bytes in S3 under "blobs/<hash>", and
// records all metadata in the SQLite catalog. Because the blob key is the
// content hash, identical image bytes are stored exactly once.
//
// The staged zip itself stays in the bucket after extraction: once the worker
// drains the queue it signals OnIdle, where the backup package uploads the
// catalog and only then deletes the zips that backup covers.
//
// Embedding is deliberately not part of extraction: blobs are recorded as
// pending and the background workers in the recommend package drain them, so
// an album becomes ready and visible as soon as its images are extracted.
package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "golang.org/x/image/webp"
	"viewer/internal/catalog"
)

// ErrNoValidImages is returned when a zip contains no decodable image entries.
var ErrNoValidImages = errors.New("no valid images in zip")

// ErrStopped is returned by Enqueue once the worker has stopped, so a producer
// that raced a shutdown learns its album was not scheduled. It is not a failure
// of the album: the row is left QUEUED and the upload scan queues it again on
// the next process.
var ErrStopped = errors.New("ingest worker is stopped")

var allowedContentTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
}

const defaultQueueSize = 4096

// leftoverZipPattern names a downloaded staged zip in TempDir. It is shared by
// the download and by the startup sweep so the two cannot drift apart.
const leftoverZipPattern = "ingest-*.zip"

// Store is the S3 surface the pipeline needs.
type Store interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
	PutObject(ctx context.Context, key string, body io.Reader, contentType string) error
	HeadObject(ctx context.Context, key string) (bool, int64, error)
}

// Options configure a pipeline service.
type Options struct {
	// TempDir holds the downloaded zip while it is being unpacked. An empty
	// value means os.TempDir(). Start removes ingest-*.zip leftovers of a
	// crashed run from it before the worker comes up.
	TempDir string
	// OnAlbumReady is invoked after an album has been fully extracted.
	OnAlbumReady func(albumID string)
	// OnIdle is invoked by the worker after it released the last claimed
	// album and nothing else is queued. It runs on the worker goroutine, so
	// no album can turn SUCCEEDED while it runs — which is what lets the
	// callback snapshot the catalog and delete the staged zips that snapshot
	// covers. An error is logged and does not affect the worker.
	OnIdle func(ctx context.Context) error
}

// Service downloads and unpacks staged uploads sequentially.
type Service struct {
	catalog *catalog.Store
	store   Store
	opts    Options

	queue     chan string
	startOnce sync.Once
	stopOnce  sync.Once

	// stopped is closed when the worker returns, so a producer blocked on a full
	// queue cannot outlive the service.
	stopped chan struct{}

	mu       sync.Mutex
	inFlight map[string]struct{}
}

// NewService builds a pipeline service. It never computes embeddings: blobs
// are recorded as pending and the recommend package's background workers pick
// them up.
func NewService(cat *catalog.Store, store Store, opts Options) *Service {
	if strings.TrimSpace(opts.TempDir) == "" {
		opts.TempDir = os.TempDir()
	}
	return &Service{
		catalog:  cat,
		store:    store,
		opts:     opts,
		queue:    make(chan string, defaultQueueSize),
		stopped:  make(chan struct{}),
		inFlight: make(map[string]struct{}),
	}
}

// UploadPrefix is the object-key prefix that holds staged uploads. It is the
// only prefix the viewer accepts a zip from: a client uploads to a presigned
// key under it, and anything else that appears there is adopted by the upload
// scan.
const UploadPrefix = "uploads/"

// SourceKey is the staging object key a client uploads a zip to.
func SourceKey(albumID string) string {
	return UploadPrefix + albumID + ".zip"
}

// StagedAlbumID inverts SourceKey for the flat keys it generates. It lets a
// listed object be matched against the album row it belongs to, including rows
// written before the key was recorded on the album.
func StagedAlbumID(key string) (string, bool) {
	name, ok := strings.CutPrefix(key, UploadPrefix)
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(name, ".zip")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// BlobKey is the content-addressed S3 key for raw image bytes.
func BlobKey(hash string) string {
	return "blobs/" + hash
}

// Start launches the sequential worker once.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.startOnce.Do(func() {
		s.removeLeftoverZips()
		go func() {
			defer s.markStopped()
			s.runWorker(ctx)
		}()
	})
}

// markStopped closes the shutdown signal exactly once, when the worker returns
// because its context ended.
func (s *Service) markStopped() {
	s.stopOnce.Do(func() { close(s.stopped) })
}

// removeLeftoverZips deletes the staged-zip downloads a crashed run left
// behind. It runs before the worker starts, and the worker is the only thing
// that ever creates an ingest-*.zip, so anything matching at this point is a
// leftover by definition. Files that appear later are in-flight downloads and
// are never touched.
func (s *Service) removeLeftoverZips() {
	leftovers, err := filepath.Glob(filepath.Join(s.opts.TempDir, leftoverZipPattern))
	if err != nil {
		log.Printf("pipeline: listing leftover staged zips in %s failed: %v", s.opts.TempDir, err)
		return
	}
	removed := 0
	for _, path := range leftovers {
		if err := os.Remove(path); err != nil {
			log.Printf("pipeline: removing leftover staged zip %s failed: %v", path, err)
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Printf("pipeline: removed %d leftover staged zip(s) from %s", removed, s.opts.TempDir)
	}
}

func (s *Service) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case albumID := <-s.queue:
			err := s.ProcessAlbum(ctx, albumID)
			remaining := s.release(albumID)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("pipeline: album=%s failed: %v", albumID, err)
			}
			if remaining == 0 && s.opts.OnIdle != nil && ctx.Err() == nil {
				if err := s.opts.OnIdle(ctx); err != nil {
					log.Printf("pipeline: drain finalize failed: %v", err)
				}
			}
		}
	}
}

// Enqueue schedules an album for extraction, waiting for room when the queue is
// full. Extraction is one album at a time, so a full queue is backpressure and
// not an error: the caller blocks until a slot frees, ctx ends, or the worker
// stops. Dropping the album instead would make a transient backlog look like a
// permanent failure to every producer - the upload scan logs it and the
// finalize endpoint marks the album FAILED - even though the staged zip is
// still in the bucket and nothing is wrong with it.
//
// Duplicate enqueues for an album that is already queued or processing are
// ignored.
func (s *Service) Enqueue(ctx context.Context, albumID string) error {
	albumID = strings.TrimSpace(albumID)
	if albumID == "" {
		return fmt.Errorf("album id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.claim(albumID) {
		return nil
	}
	select {
	case s.queue <- albumID:
		return nil
	case <-ctx.Done():
		s.release(albumID)
		return ctx.Err()
	case <-s.stopped:
		s.release(albumID)
		return ErrStopped
	}
}

func (s *Service) claim(albumID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.inFlight[albumID]; exists {
		return false
	}
	s.inFlight[albumID] = struct{}{}
	return true
}

// release removes an album from the in-flight set and reports how many claims
// are left. Zero means the queue holds nothing and no album is being
// extracted. Because Enqueue claims before it sends to the channel, this is
// an exact drain test — unlike len() of the channel, which only approximates.
func (s *Service) release(albumID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, albumID)
	return len(s.inFlight)
}

// ProcessAlbum downloads the staged zip and extracts every image entry.
func (s *Service) ProcessAlbum(ctx context.Context, albumID string) error {
	if s == nil || s.catalog == nil || s.store == nil {
		return fmt.Errorf("pipeline is not initialized")
	}
	albumID = strings.TrimSpace(albumID)
	if albumID == "" {
		return fmt.Errorf("album id is required")
	}

	album, err := s.catalog.GetAlbum(ctx, albumID)
	if err != nil {
		return err
	}
	if album.Status == catalog.AlbumStatusReady {
		return nil
	}

	sourceKey := album.SourceKey
	if strings.TrimSpace(sourceKey) == "" {
		sourceKey = SourceKey(albumID)
	}

	if err := s.catalog.SetAlbumStatus(ctx, albumID, catalog.AlbumStatusProcessing, ""); err != nil {
		return err
	}

	photoCount, err := s.extract(ctx, albumID, sourceKey)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			_ = s.catalog.SetAlbumStatus(context.Background(), albumID, catalog.AlbumStatusQueued, "")
			return err
		}
		_ = s.catalog.SetAlbumStatus(context.Background(), albumID, catalog.AlbumStatusFailed, err.Error())
		return err
	}

	if err := s.catalog.MarkAlbumReady(ctx, albumID, photoCount); err != nil {
		return err
	}

	// The staged zip stays in the bucket for now: it is deleted in a batch
	// after the queue drains, once a catalog backup that covers it is durable
	// in S3 (see OnIdle).

	if s.opts.OnAlbumReady != nil {
		s.opts.OnAlbumReady(albumID)
	}

	// Extraction never waits for embeddings, so a ready album almost always
	// leaves blobs pending for the background workers. Report the split so the
	// log line says how much embedding work is still queued for this album.
	if embedder, err := s.catalog.EmbeddingCountsByAlbum(ctx, albumID); err == nil {
		log.Printf(
			"pipeline: album=%s ready photos=%d embedded=%d pending=%d failed=%d",
			albumID, photoCount, embedder.Ready, embedder.Pending, embedder.Failed,
		)
	} else {
		log.Printf("pipeline: album=%s ready photos=%d embedding count failed: %v", albumID, photoCount, err)
	}
	return nil
}

func (s *Service) extract(ctx context.Context, albumID string, sourceKey string) (int, error) {
	exists, reportedSize, err := s.store.HeadObject(ctx, sourceKey)
	if err != nil {
		return 0, fmt.Errorf("head staged zip: %w", err)
	}
	if !exists || reportedSize <= 0 {
		return 0, fmt.Errorf("staged zip not found: %s", sourceKey)
	}

	zipPath, size, err := s.download(ctx, sourceKey)
	if err != nil {
		return 0, err
	}
	defer os.Remove(zipPath)

	file, err := os.Open(zipPath)
	if err != nil {
		return 0, fmt.Errorf("open downloaded zip: %w", err)
	}
	defer file.Close()

	reader, err := zip.NewReader(file, size)
	if err != nil {
		return 0, fmt.Errorf("open zip: %w", err)
	}

	entries := imageEntries(reader.File)
	log.Printf("pipeline: album=%s extracting entries=%d", albumID, len(entries))

	if err := s.catalog.DeletePhotos(ctx, albumID); err != nil {
		return 0, err
	}

	photoCount := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return photoCount, err
		}
		ok, err := s.extractEntry(ctx, albumID, photoCount, entry)
		if err != nil {
			return photoCount, err
		}
		if ok {
			photoCount++
		}
	}

	if photoCount == 0 {
		return 0, ErrNoValidImages
	}
	return photoCount, nil
}

func (s *Service) download(ctx context.Context, sourceKey string) (string, int64, error) {
	dir := s.opts.TempDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("create zip temp dir: %w", err)
	}

	body, _, err := s.store.GetObject(ctx, sourceKey)
	if err != nil {
		return "", 0, fmt.Errorf("download staged zip: %w", err)
	}
	defer body.Close()

	tmp, err := os.CreateTemp(dir, leftoverZipPattern)
	if err != nil {
		return "", 0, fmt.Errorf("create zip temp file: %w", err)
	}
	written, copyErr := io.Copy(tmp, body)
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmp.Name())
		return "", 0, fmt.Errorf("download staged zip: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmp.Name())
		return "", 0, fmt.Errorf("close zip temp file: %w", closeErr)
	}
	if written <= 0 {
		os.Remove(tmp.Name())
		return "", 0, fmt.Errorf("staged zip is empty: %s", sourceKey)
	}
	return tmp.Name(), written, nil
}

// extractEntry processes a single zip entry. It returns false when the entry is
// not a decodable image and was therefore skipped.
func (s *Service) extractEntry(ctx context.Context, albumID string, index int, entry *zip.File) (bool, error) {
	rc, err := entry.Open()
	if err != nil {
		log.Printf("pipeline: album=%s entry=%q open failed: %v", albumID, entry.Name, err)
		return false, nil
	}
	data, err := io.ReadAll(rc)
	closeErr := rc.Close()
	if err != nil {
		log.Printf("pipeline: album=%s entry=%q read failed: %v", albumID, entry.Name, err)
		return false, nil
	}
	if closeErr != nil {
		log.Printf("pipeline: album=%s entry=%q close failed: %v", albumID, entry.Name, closeErr)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		log.Printf("pipeline: album=%s entry=%q decode config failed: %v", albumID, entry.Name, err)
		return false, nil
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	contentType := contentTypeFor(data, entry.Name)

	if err := s.ensureBlob(ctx, hash, data, contentType); err != nil {
		return false, err
	}

	if err := s.catalog.UpsertBlob(ctx, catalog.Blob{
		Hash:        hash,
		SizeBytes:   int64(len(data)),
		ContentType: contentType,
	}); err != nil {
		return false, err
	}

	if err := s.catalog.InsertPhoto(ctx, catalog.Photo{
		AlbumID: albumID,
		Index:   index,
		Name:    entry.Name,
		Hash:    hash,
		Width:   cfg.Width,
		Height:  cfg.Height,
		Ratio:   float64(cfg.Width) / float64(cfg.Height),
	}); err != nil {
		return false, err
	}

	return true, nil
}

// ensureBlob uploads the raw image bytes to S3 exactly once per content hash.
// Repeated extractions of the same image reuse the stored object.
func (s *Service) ensureBlob(ctx context.Context, hash string, data []byte, contentType string) error {
	key := BlobKey(hash)
	exists, size, err := s.store.HeadObject(ctx, key)
	if err != nil {
		return fmt.Errorf("head blob %s: %w", key, err)
	}
	if !exists || size <= 0 {
		if err := s.store.PutObject(ctx, key, bytes.NewReader(data), contentType); err != nil {
			return fmt.Errorf("put blob %s: %w", key, err)
		}
	}
	return nil
}

// imageEntries returns decodable image entries sorted the same way the legacy
// indexer sorted them, so photo indexes stay stable across upgrades.
func imageEntries(files []*zip.File) []*zip.File {
	entries := make([]*zip.File, 0, len(files))
	for _, file := range files {
		if file.FileInfo().IsDir() {
			continue
		}
		if _, ok := allowedContentTypes[strings.ToLower(filepath.Ext(file.Name))]; !ok {
			continue
		}
		entries = append(entries, file)
	}
	sort.Slice(entries, func(a, b int) bool {
		return strings.ToLower(entries[a].Name) < strings.ToLower(entries[b].Name)
	})
	return entries
}

func contentTypeFor(data []byte, name string) string {
	if ct, ok := allowedContentTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return ct
	}
	return http.DetectContentType(data)
}
