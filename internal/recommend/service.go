package recommend

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cfgpkg "viewer/internal/config"
	"viewer/internal/images"
	"viewer/internal/models"
	"viewer/internal/storage"
)

type Service struct {
	cfg    cfgpkg.Config
	images *images.Service
	s3     *storage.S3Store

	embedder EmbeddingProvider

	startOnce sync.Once
	startErr  error

	mu                sync.RWMutex
	photosByID        map[string]PhotoRecord
	photoIDsByAlbum   map[string]map[string]struct{}
	embeddingsByID    map[string]EmbeddingRecord
	failedByID        map[string]string
	missingByAlbum    map[string]map[int]struct{}
	albumsWithMissing []string
	albumMissingPos   map[string]int

	rngMu sync.Mutex
	rng   *rand.Rand

	albumLockMu sync.Mutex
	albumLocks  map[string]*sync.Mutex

	imageLoadSem chan struct{}
}

func NewService(cfg cfgpkg.Config, imagesService *images.Service, s3Store *storage.S3Store) (*Service, error) {
	return &Service{
		cfg:               cfg,
		images:            imagesService,
		s3:                s3Store,
		embedder:          NewHTTPEmbedder(cfg.RecommenderEndpoint, time.Duration(cfg.RecommenderTimeoutSec)*time.Second),
		photosByID:        make(map[string]PhotoRecord),
		photoIDsByAlbum:   make(map[string]map[string]struct{}),
		embeddingsByID:    make(map[string]EmbeddingRecord),
		failedByID:        make(map[string]string),
		missingByAlbum:    make(map[string]map[int]struct{}),
		albumsWithMissing: make([]string, 0),
		albumMissingPos:   make(map[string]int),
		rng:               rand.New(rand.NewSource(time.Now().UnixNano())),
		albumLocks:        make(map[string]*sync.Mutex),
		imageLoadSem:      make(chan struct{}, 1),
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

func (s *Service) Enabled() bool {
	return s != nil && strings.TrimSpace(s.cfg.RecommenderEndpoint) != ""
}

func (s *Service) Start(ctx context.Context) error {
	s.startOnce.Do(func() {
		if !s.Enabled() {
			s.startErr = nil
			return
		}

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
	s.ensureIndexesLocked()

	albumID := idx.AlbumID
	if albumID == "" {
		return
	}
	if previous, ok := s.photoIDsByAlbum[albumID]; ok {
		for id := range previous {
			delete(s.photosByID, id)
			delete(s.embeddingsByID, id)
			delete(s.failedByID, id)
		}
	}
	delete(s.photoIDsByAlbum, albumID)

	missing := make(map[int]struct{})
	albumPhotoIDs := make(map[string]struct{}, len(idx.Photos))
	for _, photo := range idx.Photos {
		id := imageID(albumID, photo.I)
		albumPhotoIDs[id] = struct{}{}
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
				UpdatedAt:  emb.UpdatedAt,
				Dimensions: len(normalized),
			}
		case embeddingStatusFailed:
			s.failedByID[id] = emb.Error
		default:
			missing[photo.I] = struct{}{}
		}
	}
	s.photoIDsByAlbum[albumID] = albumPhotoIDs
	s.setMissingForAlbumLocked(albumID, missing)
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

		albumID, photos, ok := s.claimRandomMissingAlbum()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-idleTicker.C:
			}
			continue
		}

		if err := s.embedAlbumAndPersist(ctx, albumID, photos); err != nil {
			log.Printf("recommend: album embed failed album=%s claimed=%d err=%v", albumID, len(photos), err)
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

func (s *Service) claimRandomMissingAlbum() (string, []PhotoRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureIndexesLocked()

	for len(s.albumsWithMissing) > 0 {
		s.rngMu.Lock()
		albumID := s.albumsWithMissing[s.rng.Intn(len(s.albumsWithMissing))]
		s.rngMu.Unlock()

		missing := s.missingByAlbum[albumID]
		if len(missing) == 0 {
			s.removeAlbumWithMissingLocked(albumID)
			continue
		}

		indexes := make([]int, 0, len(missing))
		for idx := range missing {
			indexes = append(indexes, idx)
		}
		sort.Ints(indexes)

		photos := make([]PhotoRecord, 0, len(indexes))
		for _, photoIndex := range indexes {
			delete(missing, photoIndex)
			photo, ok := s.photosByID[imageID(albumID, photoIndex)]
			if !ok {
				continue
			}
			photos = append(photos, photo)
		}
		if len(missing) == 0 {
			s.removeAlbumWithMissingLocked(albumID)
		}
		if len(photos) == 0 {
			continue
		}
		return albumID, photos, true
	}
	return "", nil, false
}

func (s *Service) embedAlbumAndPersist(ctx context.Context, albumID string, photos []PhotoRecord) error {
	if albumID == "" || len(photos) == 0 {
		return nil
	}

	startedAt := time.Now()

	idx, err := s.readAlbumIndex(ctx, albumID)
	if err != nil {
		s.requeueMissingPhotos(albumID, photoIndexesFromRecords(photos))
		return fmt.Errorf("read album index: %w", err)
	}

	targets := make([]PhotoRecord, 0, len(photos))
	for _, photo := range photos {
		current := idx.Embeddings[embeddingIndexKey(photo.PhotoIndex)]
		if current.Status == embeddingStatusReady && len(current.Vector) > 0 {
			continue
		}
		if current.Status == embeddingStatusFailed {
			continue
		}
		targets = append(targets, photo)
	}
	if len(targets) == 0 {
		s.applyAlbumIndex(*idx)
		return nil
	}

	updates := make(map[int]models.PhotoEmbedding, len(targets))
	transientIndexes := make([]int, 0, len(targets))
	readyCount := 0
	failedCount := 0

	for _, photo := range targets {
		if err := s.acquireImageLoadSlot(ctx); err != nil {
			s.requeueMissingPhotos(albumID, photoIndexesFromRecords(targets))
			return fmt.Errorf("wait for image download slot: %w", err)
		}
		result, err := s.images.GetImageByEntry(ctx, albumID, photo.EntryName)
		s.releaseImageLoadSlot()
		if err != nil {
			failedCount++
			updates[photo.PhotoIndex] = models.PhotoEmbedding{
				Status:    embeddingStatusFailed,
				Error:     fmt.Sprintf("load image bytes: %v", err),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			continue
		}

		vector, _, err := s.embedder.Embed(ctx, result.Bytes)
		if err != nil {
			if isTransientEmbedError(err) {
				transientIndexes = append(transientIndexes, photo.PhotoIndex)
				continue
			}
			failedCount++
			updates[photo.PhotoIndex] = models.PhotoEmbedding{
				Status:    embeddingStatusFailed,
				Error:     fmt.Sprintf("embed image: %v", err),
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			}
			continue
		}

		readyCount++
		updates[photo.PhotoIndex] = models.PhotoEmbedding{
			Status:    embeddingStatusReady,
			Vector:    vector,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		}
	}

	if err := s.persistEmbeddingStatuses(ctx, albumID, updates); err != nil {
		s.requeueMissingPhotos(albumID, photoIndexesFromRecords(targets))
		return fmt.Errorf("persist album embeddings: %w", err)
	}

	s.requeueMissingPhotos(albumID, transientIndexes)

	log.Printf(
		"recommend: album embed batch album=%s claimed=%d processed=%d ready=%d failed=%d retry=%d duration=%s",
		albumID,
		len(photos),
		len(targets),
		readyCount,
		failedCount,
		len(transientIndexes),
		time.Since(startedAt).Round(time.Millisecond),
	)

	total, ready, failed, missing := s.albumEmbeddingStats(albumID)
	if missing == 0 {
		if failed == 0 {
			log.Printf("recommend: album embedded successfully album=%s total=%d ready=%d", albumID, total, ready)
		} else {
			log.Printf("recommend: album embedding complete album=%s total=%d ready=%d failed=%d", albumID, total, ready, failed)
		}
	}
	return nil
}

func (s *Service) acquireImageLoadSlot(ctx context.Context) error {
	if s.imageLoadSem == nil {
		return nil
	}
	select {
	case s.imageLoadSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) releaseImageLoadSlot() {
	if s.imageLoadSem == nil {
		return
	}
	select {
	case <-s.imageLoadSem:
	default:
	}
}

func photoIndexesFromRecords(photos []PhotoRecord) []int {
	indexes := make([]int, 0, len(photos))
	for _, photo := range photos {
		indexes = append(indexes, photo.PhotoIndex)
	}
	return indexes
}

func (s *Service) requeueMissingPhotos(albumID string, photoIndexes []int) {
	if albumID == "" || len(photoIndexes) == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureIndexesLocked()

	missing, ok := s.missingByAlbum[albumID]
	if !ok {
		missing = make(map[int]struct{})
		s.missingByAlbum[albumID] = missing
	}

	for _, photoIndex := range photoIndexes {
		imageIDValue := imageID(albumID, photoIndex)
		if _, ok := s.photosByID[imageIDValue]; !ok {
			continue
		}
		delete(s.failedByID, imageIDValue)
		missing[photoIndex] = struct{}{}
	}
	if len(missing) > 0 {
		s.addAlbumWithMissingLocked(albumID)
	} else {
		s.removeAlbumWithMissingLocked(albumID)
	}
}

func (s *Service) markFailedLocal(imageIDValue string, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureIndexesLocked()
	s.failedByID[imageIDValue] = errText
	albumID, photoIndex, err := parseImageID(imageIDValue)
	if err == nil {
		if missing, ok := s.missingByAlbum[albumID]; ok {
			delete(missing, photoIndex)
			if len(missing) == 0 {
				s.removeAlbumWithMissingLocked(albumID)
			}
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

func (s *Service) persistEmbeddingStatuses(ctx context.Context, albumID string, embeddings map[int]models.PhotoEmbedding) error {
	if len(embeddings) == 0 {
		return nil
	}

	lock := s.albumLock(albumID)
	lock.Lock()
	defer lock.Unlock()

	idx, err := s.readAlbumIndex(ctx, albumID)
	if err != nil {
		return err
	}
	for photoIndex, embedding := range embeddings {
		idx.Embeddings[embeddingIndexKey(photoIndex)] = embedding
	}
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

func (s *Service) EmbeddingProgress() EmbeddingProgress {
	if s == nil {
		return EmbeddingProgress{}
	}

	s.mu.RLock()
	total := len(s.photosByID)
	ready := len(s.embeddingsByID)
	failed := len(s.failedByID)
	pending := 0
	for _, missing := range s.missingByAlbum {
		pending += len(missing)
	}
	s.mu.RUnlock()

	processed := ready + failed
	ratio := 0.0
	if total > 0 {
		ratio = float64(ready) / float64(total)
	}

	return EmbeddingProgress{
		Total:     total,
		Ready:     ready,
		Failed:    failed,
		Pending:   pending,
		Processed: processed,
		Ratio:     ratio,
		Percent:   ratio * 100,
	}
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
			if storage.IsNotFound(err) {
				return RecommendationResponse{}, fmt.Errorf("%w: %s:%d", ErrPhotoNotFound, albumID, photoIndex)
			}
			return RecommendationResponse{}, err
		}
		s.mu.RLock()
		_, hasPhoto = s.photosByID[queryID]
		s.mu.RUnlock()
		if !hasPhoto {
			return RecommendationResponse{}, fmt.Errorf("%w: %s:%d", ErrPhotoNotFound, albumID, photoIndex)
		}
	}

	s.mu.RLock()
	if _, failed := s.failedByID[queryID]; failed {
		s.mu.RUnlock()
		return RecommendationResponse{Items: []RecommendationItem{}}, nil
	}
	queryEmbedding, ok := s.embeddingsByID[queryID]
	if !ok || len(queryEmbedding.Vector) == 0 {
		s.mu.RUnlock()
		return RecommendationResponse{Items: []RecommendationItem{}}, nil
	}

	// Recommendations are cross-album only.
	exclude := map[string]struct{}{queryID: {}}
	neighbors := findNeighbors(s.embeddingsByID, queryEmbedding.Vector, len(s.embeddingsByID), exclude)
	items := make([]RecommendationItem, 0, limit)
	seenAlbumIDs := make(map[string]struct{}, limit)
	for _, n := range neighbors {
		photo, ok := s.photosByID[n.ImageID]
		if !ok {
			continue
		}
		if photo.AlbumID == albumID {
			continue
		}
		if _, seen := seenAlbumIDs[photo.AlbumID]; seen {
			continue
		}
		seenAlbumIDs[photo.AlbumID] = struct{}{}
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

	return RecommendationResponse{Items: items}, nil
}

func (s *Service) ensureIndexesLocked() {
	if s.photosByID == nil {
		s.photosByID = make(map[string]PhotoRecord)
	}
	if s.photoIDsByAlbum == nil {
		s.photoIDsByAlbum = make(map[string]map[string]struct{})
	}
	if s.embeddingsByID == nil {
		s.embeddingsByID = make(map[string]EmbeddingRecord)
	}
	if s.failedByID == nil {
		s.failedByID = make(map[string]string)
	}
	if s.missingByAlbum == nil {
		s.missingByAlbum = make(map[string]map[int]struct{})
	}
	if s.albumsWithMissing == nil {
		s.albumsWithMissing = make([]string, 0)
	}
	if s.albumMissingPos == nil {
		s.albumMissingPos = make(map[string]int)
		if len(s.albumsWithMissing) > 0 {
			for idx, albumID := range s.albumsWithMissing {
				s.albumMissingPos[albumID] = idx
			}
		}
	}
	if len(s.albumMissingPos) == 0 {
		if len(s.albumsWithMissing) > 0 {
			for idx, albumID := range s.albumsWithMissing {
				s.albumMissingPos[albumID] = idx
			}
		} else {
			for albumID, missing := range s.missingByAlbum {
				if len(missing) == 0 {
					continue
				}
				s.albumMissingPos[albumID] = len(s.albumsWithMissing)
				s.albumsWithMissing = append(s.albumsWithMissing, albumID)
			}
		}
	}
}

func (s *Service) setMissingForAlbumLocked(albumID string, missing map[int]struct{}) {
	if missing == nil {
		missing = make(map[int]struct{})
	}
	s.missingByAlbum[albumID] = missing
	if len(missing) > 0 {
		s.addAlbumWithMissingLocked(albumID)
	} else {
		s.removeAlbumWithMissingLocked(albumID)
	}
}

func (s *Service) addAlbumWithMissingLocked(albumID string) {
	if _, ok := s.albumMissingPos[albumID]; ok {
		return
	}
	s.albumMissingPos[albumID] = len(s.albumsWithMissing)
	s.albumsWithMissing = append(s.albumsWithMissing, albumID)
}

func (s *Service) removeAlbumWithMissingLocked(albumID string) {
	pos, ok := s.albumMissingPos[albumID]
	if !ok {
		return
	}
	lastIdx := len(s.albumsWithMissing) - 1
	lastAlbumID := s.albumsWithMissing[lastIdx]
	s.albumsWithMissing[pos] = lastAlbumID
	s.albumMissingPos[lastAlbumID] = pos
	s.albumsWithMissing = s.albumsWithMissing[:lastIdx]
	delete(s.albumMissingPos, albumID)
}
