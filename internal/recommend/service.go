package recommend

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"sync"
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

	embedder EmbeddingProvider

	startOnce sync.Once
	startErr  error

	mu             sync.RWMutex
	photosByID     map[string]PhotoRecord
	embeddingsByID map[string]EmbeddingRecord
	failedByID     map[string]string
	missingByAlbum map[string]map[int]struct{}

	rngMu sync.Mutex
	rng   *rand.Rand

	albumLockMu sync.Mutex
	albumLocks  map[string]*sync.Mutex
}

func NewService(cfg cfgpkg.Config, albumsService *albums.Service, imagesService *images.Service, s3Store *storage.S3Store) (*Service, error) {
	return &Service{
		cfg:            cfg,
		albums:         albumsService,
		images:         imagesService,
		s3:             s3Store,
		embedder:       NewPythonEmbedder(cfg.RecommenderEndpoint, cfg.Siglip2ModelID, cfg.Siglip2Device, time.Duration(cfg.RecommenderTimeoutSec)*time.Second),
		photosByID:     make(map[string]PhotoRecord),
		embeddingsByID: make(map[string]EmbeddingRecord),
		failedByID:     make(map[string]string),
		missingByAlbum: make(map[string]map[int]struct{}),
		rng:            rand.New(rand.NewSource(time.Now().UnixNano())),
		albumLocks:     make(map[string]*sync.Mutex),
	}, nil
}

func (s *Service) Healthcheck(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("recommendation service is nil")
	}
	if s.embedder == nil {
		return fmt.Errorf("recommendation embedder is not initialized")
	}
	return s.embedder.Healthcheck(ctx)
}

func (s *Service) Start(ctx context.Context) error {
	s.startOnce.Do(func() {
		go func() {
			<-ctx.Done()
			_ = s.embedder.Close()
		}()

		concurrency := s.cfg.RecommenderConcurrency
		if concurrency <= 0 {
			concurrency = 1
		}
		for i := 0; i < concurrency; i++ {
			go s.workerLoop(ctx)
		}
		s.startErr = nil
	})
	return s.startErr
}

func (s *Service) IngestAlbumIndex(idx models.AlbumIndex) {
	if idx.AlbumID == "" {
		return
	}
	s.applyAlbumIndex(idx)
}

func albumIndexKey(albumID string) string {
	return fmt.Sprintf("albums/%s/index.json", albumID)
}

func embeddingIndexKey(photoIndex int) string {
	return strconv.Itoa(photoIndex)
}

func (s *Service) applyAlbumIndex(idx models.AlbumIndex) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyAlbumIndexLocked(idx)
}

func (s *Service) applyAlbumIndexLocked(idx models.AlbumIndex) {
	albumID := idx.AlbumID
	if albumID == "" {
		return
	}
	for id, photo := range s.photosByID {
		if photo.AlbumID == albumID {
			delete(s.photosByID, id)
			delete(s.embeddingsByID, id)
			delete(s.failedByID, id)
		}
	}

	missing := make(map[int]struct{})
	for _, photo := range idx.Photos {
		id := imageID(albumID, photo.I)
		rec := PhotoRecord{
			ImageID:    id,
			AlbumID:    albumID,
			PhotoIndex: photo.I,
			EntryName:  photo.Name,
			Width:      photo.W,
			Height:     photo.H,
			Ratio:      photo.Ratio,
			CreatedAt:  idx.CreatedAt,
		}
		s.photosByID[id] = rec

		emb, ok := idx.Embeddings[embeddingIndexKey(photo.I)]
		if !ok {
			missing[photo.I] = struct{}{}
			continue
		}
		switch emb.Status {
		case embeddingStatusReady:
			if len(emb.Vector) == 0 {
				missing[photo.I] = struct{}{}
				continue
			}
			normalized, norm := normalizeVector(emb.Vector)
			s.embeddingsByID[id] = EmbeddingRecord{
				ImageID:    id,
				Vector:     normalized,
				Norm:       norm,
				ModelID:    emb.Model,
				UpdatedAt:  emb.UpdatedAt,
				Dimensions: len(normalized),
			}
		case embeddingStatusFailed:
			s.failedByID[id] = emb.Error
		default:
			missing[photo.I] = struct{}{}
		}
	}
	s.missingByAlbum[albumID] = missing
}

func (s *Service) NotifyAlbumFinalized(ctx context.Context, albumID string) {
	go func() {
		if err := s.syncAlbum(ctx, albumID); err != nil {
			log.Printf("recommend: finalize sync failed album=%s err=%v", albumID, err)
		}
	}()
}

func (s *Service) syncAlbum(ctx context.Context, albumID string) error {
	var idx models.AlbumIndex
	if err := s.s3.ReadJSON(ctx, albumIndexKey(albumID), &idx); err != nil {
		return err
	}
	if idx.AlbumID == "" {
		idx.AlbumID = albumID
	}
	s.applyAlbumIndex(idx)
	return nil
}

func (s *Service) workerLoop(ctx context.Context) {
	idleTicker := time.NewTicker(400 * time.Millisecond)
	defer idleTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		photo, ok := s.claimRandomMissingPhoto()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-idleTicker.C:
			}
			continue
		}

		if err := s.embedAndPersist(ctx, photo); err != nil {
			log.Printf("recommend: embed failed image=%s err=%v", photo.ImageID, err)
			if isTransientEmbedError(err) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
		}
	}
}

func (s *Service) claimRandomMissingPhoto() (PhotoRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	albumsWithMissing := make([]string, 0, len(s.missingByAlbum))
	for albumID, missing := range s.missingByAlbum {
		if len(missing) > 0 {
			albumsWithMissing = append(albumsWithMissing, albumID)
		}
	}
	if len(albumsWithMissing) == 0 {
		return PhotoRecord{}, false
	}

	s.rngMu.Lock()
	albumID := albumsWithMissing[s.rng.Intn(len(albumsWithMissing))]
	missing := s.missingByAlbum[albumID]
	indexes := make([]int, 0, len(missing))
	for idx := range missing {
		indexes = append(indexes, idx)
	}
	photoIndex := indexes[s.rng.Intn(len(indexes))]
	s.rngMu.Unlock()

	delete(missing, photoIndex)
	photo, ok := s.photosByID[imageID(albumID, photoIndex)]
	if !ok {
		return PhotoRecord{}, false
	}
	return photo, true
}

func (s *Service) embedAndPersist(ctx context.Context, photo PhotoRecord) error {
	idx, err := s.readAlbumIndex(ctx, photo.AlbumID)
	if err != nil {
		s.markFailedLocal(photo.ImageID, err.Error())
		return err
	}

	current := idx.Embeddings[embeddingIndexKey(photo.PhotoIndex)]
	if current.Status == embeddingStatusReady && len(current.Vector) > 0 {
		s.applyAlbumIndex(*idx)
		return nil
	}
	if current.Status == embeddingStatusFailed {
		s.applyAlbumIndex(*idx)
		return nil
	}

	result, err := s.images.GetImageByEntry(ctx, photo.AlbumID, photo.EntryName)
	if err != nil {
		failErr := fmt.Sprintf("load image bytes: %v", err)
		if persistErr := s.persistEmbeddingStatus(ctx, photo.AlbumID, photo.PhotoIndex, models.PhotoEmbedding{
			Status:    embeddingStatusFailed,
			Error:     failErr,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}); persistErr != nil {
			s.markFailedLocal(photo.ImageID, failErr)
			return fmt.Errorf("%s; persist failed: %w", failErr, persistErr)
		}
		return errors.New(failErr)
	}

	vector, modelID, err := s.embedder.Embed(ctx, result.Bytes)
	if err != nil {
		if isTransientEmbedError(err) {
			s.requeueMissingPhoto(photo)
			return fmt.Errorf("embed image: %w", err)
		}
		failErr := fmt.Sprintf("embed image: %v", err)
		if persistErr := s.persistEmbeddingStatus(ctx, photo.AlbumID, photo.PhotoIndex, models.PhotoEmbedding{
			Status:    embeddingStatusFailed,
			Error:     failErr,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}); persistErr != nil {
			s.markFailedLocal(photo.ImageID, failErr)
			return fmt.Errorf("%s; persist failed: %w", failErr, persistErr)
		}
		return errors.New(failErr)
	}

	entry := models.PhotoEmbedding{
		Status:    embeddingStatusReady,
		Vector:    vector,
		Model:     modelID,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.persistEmbeddingStatus(ctx, photo.AlbumID, photo.PhotoIndex, entry); err != nil {
		s.markFailedLocal(photo.ImageID, fmt.Sprintf("persist embedding: %v", err))
		return fmt.Errorf("persist embedding: %w", err)
	}
	log.Printf("recommend: embed success image=%s album=%s model=%s", photo.ImageID, photo.AlbumID, modelID)
	total, ready, failed, missing := s.albumEmbeddingStats(photo.AlbumID)
	if missing == 0 {
		if failed == 0 {
			log.Printf("recommend: album embedded successfully album=%s total=%d ready=%d", photo.AlbumID, total, ready)
		} else {
			log.Printf("recommend: album embedding complete album=%s total=%d ready=%d failed=%d", photo.AlbumID, total, ready, failed)
		}
	}
	return nil
}

func (s *Service) requeueMissingPhoto(photo PhotoRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.photosByID[photo.ImageID]; !ok {
		return
	}
	delete(s.failedByID, photo.ImageID)
	missing, ok := s.missingByAlbum[photo.AlbumID]
	if !ok {
		missing = make(map[int]struct{})
		s.missingByAlbum[photo.AlbumID] = missing
	}
	missing[photo.PhotoIndex] = struct{}{}
}

func (s *Service) markFailedLocal(imageIDValue string, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failedByID[imageIDValue] = errText
	albumID, photoIndex, err := parseImageID(imageIDValue)
	if err == nil {
		if missing, ok := s.missingByAlbum[albumID]; ok {
			delete(missing, photoIndex)
		}
	}
}

func (s *Service) readAlbumIndex(ctx context.Context, albumID string) (*models.AlbumIndex, error) {
	var idx models.AlbumIndex
	if err := s.s3.ReadJSON(ctx, albumIndexKey(albumID), &idx); err != nil {
		return nil, err
	}
	if idx.AlbumID == "" {
		idx.AlbumID = albumID
	}
	if idx.Embeddings == nil {
		idx.Embeddings = make(map[string]models.PhotoEmbedding)
	}
	return &idx, nil
}

func (s *Service) persistEmbeddingStatus(ctx context.Context, albumID string, photoIndex int, embedding models.PhotoEmbedding) error {
	lock := s.albumLock(albumID)
	lock.Lock()
	defer lock.Unlock()

	idx, err := s.readAlbumIndex(ctx, albumID)
	if err != nil {
		return err
	}
	idx.Embeddings[embeddingIndexKey(photoIndex)] = embedding
	if err := s.s3.PutJSON(ctx, albumIndexKey(albumID), idx); err != nil {
		return err
	}
	s.applyAlbumIndex(*idx)
	return nil
}

func (s *Service) albumLock(albumID string) *sync.Mutex {
	s.albumLockMu.Lock()
	defer s.albumLockMu.Unlock()
	lock, ok := s.albumLocks[albumID]
	if !ok {
		lock = &sync.Mutex{}
		s.albumLocks[albumID] = lock
	}
	return lock
}

func (s *Service) albumEmbeddingStats(albumID string) (total int, ready int, failed int, missing int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, photo := range s.photosByID {
		if photo.AlbumID != albumID {
			continue
		}
		total++
		if _, ok := s.embeddingsByID[photo.ImageID]; ok {
			ready++
		}
		if _, ok := s.failedByID[photo.ImageID]; ok {
			failed++
		}
	}
	if m, ok := s.missingByAlbum[albumID]; ok {
		missing = len(m)
	}
	return total, ready, failed, missing
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

	queryID := imageID(albumID, photoIndex)
	s.mu.RLock()
	_, hasPhoto := s.photosByID[queryID]
	s.mu.RUnlock()
	if !hasPhoto {
		if err := s.syncAlbum(ctx, albumID); err != nil {
			return RecommendationResponse{}, err
		}
	}

	s.mu.RLock()
	if _, failed := s.failedByID[queryID]; failed {
		s.mu.RUnlock()
		return RecommendationResponse{Items: nil, Status: "failed"}, nil
	}
	queryEmbedding, ok := s.embeddingsByID[queryID]
	if !ok || len(queryEmbedding.Vector) == 0 {
		s.mu.RUnlock()
		return RecommendationResponse{Items: nil, Status: "pending"}, nil
	}

	exclude := map[string]struct{}{queryID: {}}
	neighbors := findNeighbors(s.embeddingsByID, queryEmbedding.Vector, limit+1, exclude)
	items := make([]RecommendationItem, 0, limit)
	for _, n := range neighbors {
		photo, ok := s.photosByID[n.ImageID]
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
	s.mu.RUnlock()

	status := "ready"
	if len(items) == 0 {
		status = "pending"
	} else if len(items) < limit {
		status = "partial"
	}
	return RecommendationResponse{Items: items, Status: status}, nil
}
