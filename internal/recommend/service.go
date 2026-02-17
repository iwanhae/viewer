package recommend

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"viewer/internal/albums"
	cfgpkg "viewer/internal/config"
	"viewer/internal/images"
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

func NewService(cfg cfgpkg.Config, albumsService *albums.Service, imagesService *images.Service, s3Store *storage.S3Store) (*Service, error) {
	store, err := OpenLocalStore(cfg.RecoDBPath)
	if err != nil {
		return nil, err
	}
	if cfg.VectorBackend != "" && cfg.VectorBackend != "embedded" {
		return nil, fmt.Errorf("unsupported VECTOR_BACKEND %q", cfg.VectorBackend)
	}
	vectors := NewEmbeddedVectorIndex(store)
	embedder := NewPythonEmbedder(cfg.RecoWorkerCmd, cfg.Siglip2ModelID, cfg.Siglip2Device)
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
		if err := s.syncAlbum(ctx, albumID); err != nil {
			log.Printf("recommend: sync album=%s failed: %v", albumID, err)
		}
	}
	_ = s.store.SetMeta("lastSyncAt", time.Now().UTC().Format(time.RFC3339))
}

func (s *Service) syncAlbum(ctx context.Context, albumID string) error {
	idx, err := s.albums.GetAlbum(ctx, albumID)
	if err != nil {
		return err
	}
	_, err = s.store.UpsertAlbum(idx)
	return err
}

func (s *Service) NotifyAlbumFinalized(ctx context.Context, albumID string) {
	go func() {
		if err := s.syncAlbum(ctx, albumID); err != nil {
			log.Printf("recommend: finalize sync failed album=%s err=%v", albumID, err)
		}
	}()
}

func (s *Service) workerLoop(ctx context.Context, workerID string) {
	idleTicker := time.NewTicker(2 * time.Second)
	defer idleTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		jobs := s.store.ClaimJobs(workerID, 1, time.Now().UTC())
		if len(jobs) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-idleTicker.C:
			}
			continue
		}
		job := jobs[0]
		if err := s.processJob(ctx, job); err != nil {
			maxRetries := s.cfg.RecoMaxRetries
			if maxRetries <= 0 {
				maxRetries = 5
			}
			_ = s.store.MarkFailed(job.ImageID, err.Error(), maxRetries)
		}
	}
}

func (s *Service) processJob(ctx context.Context, job JobRecord) error {
	albumID, idx, err := parseImageID(job.ImageID)
	if err != nil {
		return err
	}
	result, err := s.images.GetImage(ctx, albumID, idx, "viewer", 0)
	if err != nil {
		return fmt.Errorf("load image bytes: %w", err)
	}
	vector, modelID, err := s.embedder.Embed(ctx, result.Bytes)
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
		if err := s.syncAlbum(ctx, albumID); err != nil {
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
