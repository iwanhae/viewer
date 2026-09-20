// Package pipeline turns an uploaded zip into content-addressed image blobs.
//
// A single background worker downloads one staged zip at a time and walks its
// entries sequentially. For every image entry it computes a SHA-256 hash, gets
// the image dimensions, computes an embedding, stores the raw bytes in S3 under
// "blobs/<hash>", and records all metadata in the SQLite catalog. Because the
// blob key is the content hash, identical image bytes are stored exactly once.
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

var allowedContentTypes = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
}

const defaultQueueSize = 1024

// Store is the S3 surface the pipeline needs.
type Store interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
	PutObject(ctx context.Context, key string, body io.Reader, contentType string) error
	HeadObject(ctx context.Context, key string) (bool, int64, error)
	DeleteObject(ctx context.Context, key string) error
}

// Embedder computes an embedding for raw image bytes.
type Embedder interface {
	Embed(ctx context.Context, imageBytes []byte) ([]float32, error)
}

// Options configure a pipeline service.
type Options struct {
	// DeleteSource removes the staged zip from S3 after a successful extract.
	DeleteSource bool
	// TempDir holds the downloaded zip while it is being unpacked.
	TempDir string
	// OnAlbumReady is invoked after an album has been fully extracted.
	OnAlbumReady func(albumID string)
	// QueueSize bounds the pending-album queue.
	QueueSize int
}

// Service downloads and unpacks staged uploads sequentially.
type Service struct {
	catalog  *catalog.Store
	store    Store
	embedder Embedder
	opts     Options

	queue     chan string
	startOnce sync.Once

	mu       sync.Mutex
	inFlight map[string]struct{}

	blobMu    sync.Mutex
	blobCache map[string]struct{}
}

// NewService builds a pipeline service. embedder may be nil, in which case
// images are stored without embeddings and left in the "pending" state.
func NewService(cat *catalog.Store, store Store, embedder Embedder, opts Options) *Service {
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultQueueSize
	}
	if strings.TrimSpace(opts.TempDir) == "" {
		opts.TempDir = os.TempDir()
	}
	return &Service{
		catalog:   cat,
		store:     store,
		embedder:  embedder,
		opts:      opts,
		queue:     make(chan string, opts.QueueSize),
		inFlight:  make(map[string]struct{}),
		blobCache: make(map[string]struct{}),
	}
}

// SourceKey is the staging object key a client uploads a zip to.
func SourceKey(albumID string) string {
	return fmt.Sprintf("uploads/%s/source.zip", albumID)
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
		go s.runWorker(ctx)
	})
}

func (s *Service) runWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case albumID := <-s.queue:
			err := s.ProcessAlbum(ctx, albumID)
			s.release(albumID)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("pipeline: album=%s failed: %v", albumID, err)
			}
		}
	}
}

// Enqueue schedules an album for extraction. Duplicate enqueues for an album
// that is already queued or processing are ignored.
func (s *Service) Enqueue(albumID string) error {
	albumID = strings.TrimSpace(albumID)
	if albumID == "" {
		return fmt.Errorf("album id is required")
	}
	if !s.claim(albumID) {
		return nil
	}
	select {
	case s.queue <- albumID:
		return nil
	default:
		s.release(albumID)
		return fmt.Errorf("ingest queue is full")
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

func (s *Service) release(albumID string) {
	s.mu.Lock()
	delete(s.inFlight, albumID)
	s.mu.Unlock()
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

	if s.opts.DeleteSource {
		if err := s.store.DeleteObject(ctx, sourceKey); err != nil {
			log.Printf("pipeline: album=%s extracted but staged zip delete failed key=%s err=%v", albumID, sourceKey, err)
		}
	}

	if s.opts.OnAlbumReady != nil {
		s.opts.OnAlbumReady(albumID)
	}

	log.Printf("pipeline: album=%s ready photos=%d", albumID, photoCount)
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

	tmp, err := os.CreateTemp(dir, "ingest-*.zip")
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

	created, err := s.ensureBlob(ctx, hash, data, contentType)
	if err != nil {
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

	if s.shouldEmbed(ctx, hash, created) {
		s.embed(ctx, albumID, hash, data)
	}
	return true, nil
}

// shouldEmbed avoids recomputing an embedding for a blob that already has one.
func (s *Service) shouldEmbed(ctx context.Context, hash string, created bool) bool {
	if s.embedder == nil {
		return false
	}
	if created {
		return true
	}
	blob, err := s.catalog.GetBlob(ctx, hash)
	if err != nil {
		return true
	}
	return !(blob.EmbeddingStatus == catalog.EmbeddingStatusReady && len(blob.Embedding) > 0)
}

// ensureBlob uploads the raw image bytes to S3 exactly once per content hash.
// It reports whether this call created the object.
func (s *Service) ensureBlob(ctx context.Context, hash string, data []byte, contentType string) (bool, error) {
	if s.blobKnown(hash) {
		return false, nil
	}

	key := BlobKey(hash)
	exists, size, err := s.store.HeadObject(ctx, key)
	if err != nil {
		return false, fmt.Errorf("head blob %s: %w", key, err)
	}
	created := false
	if !exists || size <= 0 {
		if err := s.store.PutObject(ctx, key, bytes.NewReader(data), contentType); err != nil {
			return false, fmt.Errorf("put blob %s: %w", key, err)
		}
		created = true
	}
	s.rememberBlob(hash)
	return created, nil
}

func (s *Service) embed(ctx context.Context, albumID string, hash string, data []byte) {
	if s.embedder == nil {
		return
	}
	vector, err := s.embedder.Embed(ctx, data)
	if err != nil {
		_ = s.catalog.SetBlobEmbedding(context.Background(), hash, catalog.EmbeddingStatusFailed, nil, err.Error())
		log.Printf("pipeline: album=%s blob=%s embed failed: %v", albumID, hash, err)
		return
	}
	if err := s.catalog.SetBlobEmbedding(ctx, hash, catalog.EmbeddingStatusReady, vector, ""); err != nil {
		log.Printf("pipeline: album=%s blob=%s persist embedding failed: %v", albumID, hash, err)
	}
}

func (s *Service) blobKnown(hash string) bool {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	_, ok := s.blobCache[hash]
	return ok
}

func (s *Service) rememberBlob(hash string) {
	s.blobMu.Lock()
	s.blobCache[hash] = struct{}{}
	s.blobMu.Unlock()
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
