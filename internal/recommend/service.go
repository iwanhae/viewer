package recommend

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"viewer/internal/albums"
	cfgpkg "viewer/internal/config"
	"viewer/internal/images"
	"viewer/internal/models"
	"viewer/internal/storage"
)

type Service struct {
	cfg    cfgpkg.Config
	albums *albums.Service
	images *images.Service
	s3     *storage.S3Store

	store    *LocalStore
	vectors  VectorIndex
	embedder EmbeddingProvider

	once sync.Once
}

const (
	defaultRecoPipelinePrefetch = 48
	defaultRecoPipelineMaxBytes = int64(512 << 20)
)

type fetchedJob struct {
	Job       JobRecord
	ImageData []byte
	SizeBytes int64
}

func NewService(cfg cfgpkg.Config, albumsService *albums.Service, imagesService *images.Service, s3Store *storage.S3Store) (*Service, error) {
	store, err := OpenLocalStore(cfg.RecoDBPath)
	if err != nil {
		return nil, err
	}
	if cfg.VectorBackend != "" && cfg.VectorBackend != "embedded" {
		return nil, fmt.Errorf("unsupported VECTOR_BACKEND %q", cfg.VectorBackend)
	}
	vectors := NewEmbeddedVectorIndex(store)
	embedder := NewPythonEmbedder(
		cfg.RecoWorkerCmd,
		cfg.Siglip2ModelID,
		cfg.Siglip2Device,
		time.Duration(cfg.RecoWorkerTimeoutSec)*time.Second,
		cfg.RecoWorkerRestartLimit,
	)
	return &Service{
		cfg:      cfg,
		albums:   albumsService,
		images:   imagesService,
		s3:       s3Store,
		store:    store,
		vectors:  vectors,
		embedder: embedder,
	}, nil
}

func (s *Service) Start(ctx context.Context) {
	s.once.Do(func() {
		go s.syncLoop(ctx)
		concurrency := s.cfg.RecoWorkerConcurrency
		if concurrency <= 0 {
			concurrency = 1
		}
		for idx := 0; idx < concurrency; idx++ {
			workerID := fmt.Sprintf("worker-%d", idx+1)
			go s.workerLoop(ctx, workerID)
		}
	})
}

func (s *Service) syncLoop(ctx context.Context) {
	s.syncOnce(ctx)
	interval := time.Duration(s.cfg.RecoSyncIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncOnce(ctx)
		}
	}
}

func albumIDFromIndexKey(key string) string {
	if !strings.HasPrefix(key, "albums/") || !strings.HasSuffix(key, "/index.json") {
		return ""
	}
	dir := filepath.Dir(key)
	parts := strings.Split(dir, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-1]
}

func albumIndexKey(albumID string) string {
	return fmt.Sprintf("albums/%s/index.json", albumID)
}

func (s *Service) syncOnce(ctx context.Context) {
	keys, err := s.s3.ListAlbumIndexKeys(ctx)
	if err != nil {
		log.Printf("recommend: sync list keys failed: %v", err)
		return
	}
	for _, key := range keys {
		albumID := albumIDFromIndexKey(key)
		if albumID == "" {
			continue
		}
		if _, err := s.syncAlbum(ctx, albumID); err != nil {
			log.Printf("recommend: sync album=%s failed: %v", albumID, err)
		}
	}
	_ = s.store.SetMeta("lastSyncAt", time.Now().UTC().Format(time.RFC3339))
}

func (s *Service) syncAlbum(ctx context.Context, albumID string) (int, error) {
	var idx models.AlbumIndex
	if err := s.s3.ReadJSON(ctx, albumIndexKey(albumID), &idx); err != nil {
		return 0, err
	}
	if idx.AlbumID == "" {
		idx.AlbumID = albumID
	}
	return s.store.UpsertAlbum(&idx)
}

func (s *Service) NotifyAlbumFinalized(ctx context.Context, albumID string) {
	go func() {
		if _, err := s.syncAlbum(ctx, albumID); err != nil {
			log.Printf("recommend: finalize sync failed album=%s err=%v", albumID, err)
		}
	}()
}

func (s *Service) workerLoop(ctx context.Context, workerID string) {
	prefetch := s.cfg.RecoPipelinePrefetch
	if prefetch <= 0 {
		prefetch = defaultRecoPipelinePrefetch
	}
	claimBatch := prefetch / 4
	if claimBatch <= 0 {
		claimBatch = 1
	}
	if claimBatch > 16 {
		claimBatch = 16
	}

	maxRetries := s.cfg.RecoMaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}

	maxBufferedBytes := int64(s.cfg.RecoPipelineMaxBytesMB) * (1 << 20)
	if maxBufferedBytes <= 0 {
		maxBufferedBytes = defaultRecoPipelineMaxBytes
	}

	claimed := make(chan JobRecord, prefetch)
	fetched := make(chan fetchedJob, prefetch)
	var bufferedBytes atomic.Int64

	go s.claimLoop(ctx, workerID, claimBatch, claimed)
	go s.fetchLoop(ctx, claimed, fetched, &bufferedBytes, maxBufferedBytes, maxRetries)

	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-fetched:
			if !ok {
				return
			}
			if err := s.processJobBytes(ctx, item.Job, item.ImageData); err != nil {
				_ = s.store.MarkFailed(item.Job.ImageID, err.Error(), maxRetries)
			}
			if item.SizeBytes > 0 {
				bufferedBytes.Add(-item.SizeBytes)
			}
		}
	}
}

func (s *Service) claimLoop(ctx context.Context, workerID string, batch int, out chan<- JobRecord) {
	defer close(out)
	if batch <= 0 {
		batch = 1
	}
	idleTicker := time.NewTicker(300 * time.Millisecond)
	defer idleTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		jobs := s.store.ClaimJobs(workerID, batch, time.Now().UTC())
		if len(jobs) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-idleTicker.C:
			}
			continue
		}

		for _, job := range jobs {
			select {
			case <-ctx.Done():
				return
			case out <- job:
			}
		}
	}
}

func (s *Service) fetchLoop(
	ctx context.Context,
	in <-chan JobRecord,
	out chan<- fetchedJob,
	bufferedBytes *atomic.Int64,
	maxBufferedBytes int64,
	maxRetries int,
) {
	defer close(out)

	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-in:
			if !ok {
				return
			}
			imageData, err := s.loadJobImage(ctx, job)
			if err != nil {
				_ = s.store.MarkFailed(job.ImageID, err.Error(), maxRetries)
				continue
			}

			sizeBytes := int64(len(imageData))
			if !reserveBufferedBytes(ctx, bufferedBytes, sizeBytes, maxBufferedBytes) {
				return
			}

			item := fetchedJob{
				Job:       job,
				ImageData: imageData,
				SizeBytes: sizeBytes,
			}
			select {
			case <-ctx.Done():
				bufferedBytes.Add(-sizeBytes)
				return
			case out <- item:
			}
		}
	}
}

func reserveBufferedBytes(ctx context.Context, buffered *atomic.Int64, sizeBytes int64, maxBufferedBytes int64) bool {
	if buffered == nil || sizeBytes <= 0 || maxBufferedBytes <= 0 {
		return true
	}
	for {
		current := buffered.Load()
		if current+sizeBytes <= maxBufferedBytes {
			if buffered.CompareAndSwap(current, current+sizeBytes) {
				return true
			}
			continue
		}

		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

func (s *Service) processJob(ctx context.Context, job JobRecord) error {
	imageData, err := s.loadJobImage(ctx, job)
	if err != nil {
		return err
	}
	return s.processJobBytes(ctx, job, imageData)
}

func (s *Service) loadJobImage(ctx context.Context, job JobRecord) ([]byte, error) {
	photo, ok := s.store.GetPhotoByID(job.ImageID)
	if !ok {
		return nil, fmt.Errorf("photo metadata missing")
	}
	result, err := s.images.GetImageByEntry(ctx, photo.AlbumID, photo.EntryName)
	if err != nil {
		return nil, fmt.Errorf("load image bytes: %w", err)
	}
	return result.Bytes, nil
}

func (s *Service) processJobBytes(ctx context.Context, job JobRecord, imageBytes []byte) error {
	if len(imageBytes) == 0 {
		return fmt.Errorf("empty image payload")
	}
	vector, modelID, err := s.embedder.Embed(ctx, imageBytes)
	if err != nil {
		return fmt.Errorf("embed image: %w", err)
	}
	if err := s.vectors.Upsert(ctx, job.ImageID, vector, modelID); err != nil {
		return fmt.Errorf("store embedding: %w", err)
	}
	return nil
}

func (s *Service) Recommend(ctx context.Context, albumID string, photoIndex int, limit int) (RecommendationResponse, error) {
	if limit <= 0 {
		limit = s.cfg.RecoTopKDefault
	}
	if limit <= 0 {
		limit = 12
	}
	if s.cfg.RecoTopKMax > 0 && limit > s.cfg.RecoTopKMax {
		limit = s.cfg.RecoTopKMax
	}

	if _, ok := s.store.GetPhoto(albumID, photoIndex); !ok {
		if _, err := s.syncAlbum(ctx, albumID); err != nil {
			return RecommendationResponse{}, err
		}
	}

	queryEmbedding, ok := s.store.GetEmbedding(albumID, photoIndex)
	if !ok || len(queryEmbedding.Vector) == 0 {
		if err := s.store.EnqueueIfNeeded(albumID, photoIndex); err != nil {
			return RecommendationResponse{Items: nil, Status: "pending"}, nil
		}
		return RecommendationResponse{Items: nil, Status: "pending"}, nil
	}

	exclude := map[string]struct{}{imageID(albumID, photoIndex): {}}
	neighbors, err := s.vectors.Search(ctx, queryEmbedding.Vector, SearchOptions{Limit: limit + 1, ExcludeIDs: exclude})
	if err != nil {
		return RecommendationResponse{}, err
	}

	items := make([]RecommendationItem, 0, limit)
	for _, n := range neighbors {
		photo, ok := s.store.GetPhotoByID(n.ImageID)
		if !ok {
			continue
		}
		items = append(items, RecommendationItem{
			AlbumID: photo.AlbumID,
			I:       photo.PhotoIndex,
			W:       photo.Width,
			H:       photo.Height,
			Score:   n.Score,
			Src:     fmt.Sprintf("/api/image/%s/%d?mode=wall&w=480", photo.AlbumID, photo.PhotoIndex),
		})
		if len(items) >= limit {
			break
		}
	}
	status := "ready"
	if len(items) == 0 {
		status = "pending"
	} else if len(items) < limit {
		status = "partial"
	}
	return RecommendationResponse{Items: items, Status: status}, nil
}
