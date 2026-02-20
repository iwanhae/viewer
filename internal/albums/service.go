package albums

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	cfgpkg "viewer/internal/config"
	"viewer/internal/models"
	"viewer/internal/storage"
)

type Service struct {
	cfg     cfgpkg.Config
	store   albumStore
	indexer *Indexer

	mu                sync.RWMutex
	albumCache        map[string]*models.AlbumIndex
	uploadHints       map[string]string
	finalizeJobs      map[string]*FinalizeState
	finalizeQueue     chan string
	finalizeStartOnce sync.Once
	onFinalizeSuccess func(models.AlbumIndex)
}

type RefreshSummary struct {
	Discovered int
	Loaded     int
	Failed     int
}

type FinalizeStatus string

const (
	FinalizeStatusQueued     FinalizeStatus = "QUEUED"
	FinalizeStatusProcessing FinalizeStatus = "PROCESSING"
	FinalizeStatusSucceeded  FinalizeStatus = "SUCCEEDED"
	FinalizeStatusFailed     FinalizeStatus = "FAILED"
)

type FinalizeState struct {
	AlbumID    string         `json:"albumId"`
	Status     FinalizeStatus `json:"status"`
	PhotoCount int            `json:"photoCount,omitempty"`
	CreatedAt  string         `json:"createdAt,omitempty"`
	Error      string         `json:"error,omitempty"`
	UpdatedAt  string         `json:"updatedAt"`
}

const defaultFinalizeQueueSize = 1024

type albumStore interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, map[string]string, error)
	HeadObject(ctx context.Context, key string) (bool, int64, error)
	GetObjectRange(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, string, error)
	PutJSON(ctx context.Context, key string, v any) error
	ReadJSON(ctx context.Context, key string, out any) error
	ForEachAlbumIndexKey(ctx context.Context, fn func(key string) error) error
}

func NewService(cfg cfgpkg.Config, store *storage.S3Store, indexer *Indexer) *Service {
	return &Service{
		cfg:           cfg,
		store:         store,
		indexer:       indexer,
		albumCache:    make(map[string]*models.AlbumIndex),
		uploadHints:   make(map[string]string),
		finalizeJobs:  make(map[string]*FinalizeState),
		finalizeQueue: make(chan string, defaultFinalizeQueueSize),
	}
}

type CreateUploadResult struct {
	AlbumID   string
	Key       string
	UploadURL string
	Headers   map[string]string
}

func sourceKey(albumID string) string {
	return fmt.Sprintf("albums/%s/source.zip", albumID)
}

func indexKey(albumID string) string {
	return fmt.Sprintf("albums/%s/index.json", albumID)
}

func (s *Service) CreateUpload(ctx context.Context, filename string, sizeBytes int64) (CreateUploadResult, error) {
	if strings.TrimSpace(filename) == "" {
		return CreateUploadResult{}, fmt.Errorf("filename is required")
	}
	if sizeBytes <= 0 {
		return CreateUploadResult{}, fmt.Errorf("sizeBytes must be > 0")
	}
	if sizeBytes > s.cfg.MaxUploadBytes {
		return CreateUploadResult{}, fmt.Errorf("sizeBytes exceeds MAX_UPLOAD_BYTES")
	}

	albumID := uuid.NewString()
	key := sourceKey(albumID)
	url, headers, err := s.store.PresignPut(ctx, key, s.cfg.PresignTTL)
	if err != nil {
		return CreateUploadResult{}, err
	}
	if headers == nil {
		headers = map[string]string{}
	}

	s.mu.Lock()
	s.uploadHints[albumID] = filename
	s.mu.Unlock()

	return CreateUploadResult{
		AlbumID:   albumID,
		Key:       key,
		UploadURL: url,
		Headers:   headers,
	}, nil
}

func (s *Service) SetFinalizeSuccessHook(fn func(models.AlbumIndex)) {
	s.mu.Lock()
	s.onFinalizeSuccess = fn
	s.mu.Unlock()
}

func (s *Service) StartFinalizeWorker(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.finalizeJobs == nil {
		s.finalizeJobs = make(map[string]*FinalizeState)
	}
	if s.finalizeQueue == nil {
		s.finalizeQueue = make(chan string, defaultFinalizeQueueSize)
	}
	queue := s.finalizeQueue
	s.mu.Unlock()

	s.finalizeStartOnce.Do(func() {
		go s.runFinalizeWorker(ctx, queue)
	})
}

func (s *Service) runFinalizeWorker(ctx context.Context, queue <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case albumID := <-queue:
			s.processFinalize(ctx, albumID)
		}
	}
}

func (s *Service) processFinalize(ctx context.Context, albumID string) {
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)

	s.mu.Lock()
	state, ok := s.finalizeJobs[albumID]
	if !ok || state == nil {
		state = &FinalizeState{
			AlbumID:   albumID,
			Status:    FinalizeStatusQueued,
			UpdatedAt: updatedAt,
		}
		s.finalizeJobs[albumID] = state
	}
	if state.Status != FinalizeStatusQueued {
		s.mu.Unlock()
		return
	}
	state.Status = FinalizeStatusProcessing
	state.Error = ""
	state.UpdatedAt = updatedAt
	s.mu.Unlock()

	idx, err := s.finalizeNow(ctx, albumID)
	if err != nil {
		s.mu.Lock()
		if failed, ok := s.finalizeJobs[albumID]; ok && failed != nil {
			failed.Status = FinalizeStatusFailed
			failed.PhotoCount = 0
			failed.CreatedAt = ""
			failed.Error = err.Error()
			failed.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		s.mu.Unlock()
		return
	}

	hookAlbum := cloneAlbumIndex(idx)
	s.mu.Lock()
	s.finalizeJobs[albumID] = &FinalizeState{
		AlbumID:    albumID,
		Status:     FinalizeStatusSucceeded,
		PhotoCount: idx.PhotoCount,
		CreatedAt:  idx.CreatedAt,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	hook := s.onFinalizeSuccess
	s.mu.Unlock()

	if hook != nil && hookAlbum != nil {
		hook(*hookAlbum)
	}
}

func (s *Service) RequestFinalize(ctx context.Context, albumID string) (FinalizeState, error) {
	albumID = strings.TrimSpace(albumID)
	if albumID == "" {
		return FinalizeState{}, fmt.Errorf("albumId is required")
	}

	s.StartFinalizeWorker(context.Background())

	s.mu.RLock()
	if current, ok := s.finalizeJobs[albumID]; ok && current != nil {
		state := cloneFinalizeState(current)
		s.mu.RUnlock()
		switch state.Status {
		case FinalizeStatusQueued, FinalizeStatusProcessing, FinalizeStatusSucceeded:
			return state, nil
		}
	} else {
		s.mu.RUnlock()
	}

	if idx, err := s.GetAlbum(ctx, albumID); err == nil {
		state := FinalizeState{
			AlbumID:    albumID,
			Status:     FinalizeStatusSucceeded,
			PhotoCount: idx.PhotoCount,
			CreatedAt:  idx.CreatedAt,
			UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		}
		s.mu.Lock()
		s.finalizeJobs[albumID] = cloneFinalizeStatePtr(state)
		s.mu.Unlock()
		return state, nil
	} else if !errors.Is(err, ErrAlbumNotFound) {
		return FinalizeState{}, err
	}

	exists, size, err := s.store.HeadObject(ctx, sourceKey(albumID))
	if err != nil {
		return FinalizeState{}, err
	}
	if !exists || size <= 0 {
		return FinalizeState{}, fmt.Errorf("%w: %s", ErrAlbumSourceNotFound, albumID)
	}

	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	shouldEnqueue := false

	s.mu.Lock()
	state, ok := s.finalizeJobs[albumID]
	if !ok || state == nil {
		state = &FinalizeState{
			AlbumID:   albumID,
			Status:    FinalizeStatusQueued,
			UpdatedAt: updatedAt,
		}
		s.finalizeJobs[albumID] = state
		shouldEnqueue = true
	} else {
		switch state.Status {
		case FinalizeStatusQueued, FinalizeStatusProcessing, FinalizeStatusSucceeded:
		default:
			state.Status = FinalizeStatusQueued
			state.PhotoCount = 0
			state.CreatedAt = ""
			state.Error = ""
			state.UpdatedAt = updatedAt
			shouldEnqueue = true
		}
	}
	response := cloneFinalizeState(state)
	s.mu.Unlock()

	if shouldEnqueue {
		if err := s.enqueueFinalize(albumID); err != nil {
			s.mu.Lock()
			if failed, ok := s.finalizeJobs[albumID]; ok && failed != nil {
				failed.Status = FinalizeStatusFailed
				failed.PhotoCount = 0
				failed.CreatedAt = ""
				failed.Error = "finalize queue is full"
				failed.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			s.mu.Unlock()
			return FinalizeState{}, err
		}
	}

	return response, nil
}

func (s *Service) GetFinalizeStatus(ctx context.Context, albumID string) (FinalizeState, error) {
	albumID = strings.TrimSpace(albumID)
	if albumID == "" {
		return FinalizeState{}, fmt.Errorf("albumId is required")
	}

	s.mu.RLock()
	if state, ok := s.finalizeJobs[albumID]; ok && state != nil {
		out := cloneFinalizeState(state)
		s.mu.RUnlock()
		return out, nil
	}
	s.mu.RUnlock()

	idx, err := s.GetAlbum(ctx, albumID)
	if err != nil {
		if errors.Is(err, ErrAlbumNotFound) {
			return FinalizeState{}, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
		}
		return FinalizeState{}, err
	}

	state := FinalizeState{
		AlbumID:    albumID,
		Status:     FinalizeStatusSucceeded,
		PhotoCount: idx.PhotoCount,
		CreatedAt:  idx.CreatedAt,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	s.mu.Lock()
	if _, ok := s.finalizeJobs[albumID]; !ok {
		s.finalizeJobs[albumID] = cloneFinalizeStatePtr(state)
	}
	s.mu.Unlock()
	return state, nil
}

func (s *Service) enqueueFinalize(albumID string) error {
	if s.finalizeQueue == nil {
		return fmt.Errorf("finalize worker is not initialized")
	}
	select {
	case s.finalizeQueue <- albumID:
		return nil
	default:
		return fmt.Errorf("finalize queue is full")
	}
}

func (s *Service) Finalize(ctx context.Context, albumID string) (*models.AlbumIndex, error) {
	return s.finalizeNow(ctx, albumID)
}

func (s *Service) finalizeNow(ctx context.Context, albumID string) (*models.AlbumIndex, error) {
	if strings.TrimSpace(albumID) == "" {
		return nil, fmt.Errorf("albumId is required")
	}

	sourceObjectKey := sourceKey(albumID)
	exists, size, err := s.store.HeadObject(ctx, sourceObjectKey)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrAlbumSourceNotFound, albumID)
	}
	if size <= 0 {
		return nil, fmt.Errorf("%w: %s", ErrAlbumSourceNotFound, albumID)
	}

	originalFilename := "source.zip"
	s.mu.RLock()
	if hinted, ok := s.uploadHints[albumID]; ok && hinted != "" {
		originalFilename = hinted
	}
	s.mu.RUnlock()

	readerAt := &s3ObjectReaderAt{
		ctx:   ctx,
		store: s.store,
		key:   sourceObjectKey,
		size:  size,
	}
	idx, err := s.indexer.BuildFromZipReaderAt(readerAt, size, albumID, originalFilename)
	if err != nil {
		return nil, err
	}

	if err := s.store.PutJSON(ctx, indexKey(albumID), idx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.albumCache[albumID] = cloneAlbumIndex(idx)
	delete(s.uploadHints, albumID)
	if job, ok := s.finalizeJobs[albumID]; ok && job != nil && job.Status != FinalizeStatusSucceeded {
		job.Status = FinalizeStatusSucceeded
		job.PhotoCount = idx.PhotoCount
		job.CreatedAt = idx.CreatedAt
		job.Error = ""
		job.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	s.mu.Unlock()

	return cloneAlbumIndex(idx), nil
}

type s3ObjectReaderAt struct {
	ctx   context.Context
	store albumStore
	key   string
	size  int64
}

func (r *s3ObjectReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r == nil || r.store == nil {
		return 0, fmt.Errorf("range reader is not initialized")
	}
	if off < 0 {
		return 0, fmt.Errorf("negative offset: %d", off)
	}
	if off >= r.size {
		return 0, io.EOF
	}

	toRead := int64(len(p))
	if max := r.size - off; toRead > max {
		toRead = max
	}
	start := off
	end := off + toRead - 1
	body, _, err := r.store.GetObjectRange(r.ctx, r.key, start, end)
	if err != nil {
		return 0, err
	}
	defer body.Close()

	n, readErr := io.ReadFull(body, p[:int(toRead)])
	if readErr != nil {
		return n, fmt.Errorf("read object range %s %d-%d: %w", r.key, start, end, readErr)
	}
	if int64(n) < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

func (s *Service) RefreshFromStorage(ctx context.Context, onAlbum func(models.AlbumIndex)) (RefreshSummary, error) {
	summary := RefreshSummary{}
	err := s.store.ForEachAlbumIndexKey(ctx, func(key string) error {
		summary.Discovered++

		var idx models.AlbumIndex
		if err := s.store.ReadJSON(ctx, key, &idx); err != nil {
			summary.Failed++
			return nil
		}

		if idx.AlbumID == "" {
			parts := strings.Split(filepath.Dir(key), "/")
			if len(parts) > 0 {
				idx.AlbumID = parts[len(parts)-1]
			}
		}
		if strings.TrimSpace(idx.AlbumID) == "" {
			summary.Failed++
			return nil
		}

		cached := cloneAlbumIndex(&idx)
		s.mu.Lock()
		s.albumCache[idx.AlbumID] = cached
		s.mu.Unlock()

		if onAlbum != nil {
			onAlbum(*cloneAlbumIndex(cached))
		}
		summary.Loaded++
		return nil
	})
	if err != nil {
		return summary, err
	}
	return summary, nil
}

func (s *Service) GetAlbum(ctx context.Context, albumID string) (*models.AlbumIndex, error) {
	s.mu.RLock()
	if idx, ok := s.albumCache[albumID]; ok {
		dup := cloneAlbumIndex(idx)
		s.mu.RUnlock()
		return dup, nil
	}
	s.mu.RUnlock()

	var idx models.AlbumIndex
	if err := s.store.ReadJSON(ctx, indexKey(albumID), &idx); err != nil {
		if storage.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
		}
		return nil, err
	}
	if idx.AlbumID == "" {
		idx.AlbumID = albumID
	}

	cached := cloneAlbumIndex(&idx)
	s.mu.Lock()
	s.albumCache[albumID] = cached
	s.mu.Unlock()

	return cloneAlbumIndex(cached), nil
}

func (s *Service) SearchAlbumsByNamePrefix(ctx context.Context, q string, limit int) ([]models.AlbumSearchItem, error) {
	_ = ctx
	if limit <= 0 {
		limit = 20
	}

	normalizedQuery := strings.ToLower(strings.TrimSpace(q))

	s.mu.RLock()
	items := make([]models.AlbumSearchItem, 0, len(s.albumCache))
	for _, idx := range s.albumCache {
		if idx == nil {
			continue
		}
		if normalizedQuery != "" && !strings.HasPrefix(strings.ToLower(idx.OriginalFilename), normalizedQuery) {
			continue
		}
		items = append(items, albumSearchItemFromIndex(idx))
	}
	s.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		if items[i].OriginalFilename != items[j].OriginalFilename {
			return items[i].OriginalFilename < items[j].OriginalFilename
		}
		return items[i].AlbumID < items[j].AlbumID
	})

	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *Service) AllAlbums() []*models.AlbumIndex {
	s.mu.RLock()
	albumsList := make([]*models.AlbumIndex, 0, len(s.albumCache))
	for _, idx := range s.albumCache {
		albumsList = append(albumsList, cloneAlbumIndex(idx))
	}
	s.mu.RUnlock()

	sort.Slice(albumsList, func(i, j int) bool {
		if albumsList[i].AlbumID != albumsList[j].AlbumID {
			return albumsList[i].AlbumID < albumsList[j].AlbumID
		}
		if albumsList[i].CreatedAt != albumsList[j].CreatedAt {
			return albumsList[i].CreatedAt < albumsList[j].CreatedAt
		}
		return albumsList[i].OriginalFilename < albumsList[j].OriginalFilename
	})

	return albumsList
}

func cloneAlbumIndex(idx *models.AlbumIndex) *models.AlbumIndex {
	if idx == nil {
		return nil
	}
	dup := *idx
	dup.Photos = append([]models.PhotoMeta(nil), idx.Photos...)
	if idx.Embeddings != nil {
		dup.Embeddings = make(map[string]models.PhotoEmbedding, len(idx.Embeddings))
		for key, value := range idx.Embeddings {
			embeddingCopy := value
			if value.Vector != nil {
				embeddingCopy.Vector = append([]float32(nil), value.Vector...)
			}
			dup.Embeddings[key] = embeddingCopy
		}
	}
	return &dup
}

func cloneFinalizeState(state *FinalizeState) FinalizeState {
	if state == nil {
		return FinalizeState{}
	}
	return *state
}

func cloneFinalizeStatePtr(state FinalizeState) *FinalizeState {
	dup := state
	return &dup
}

func albumSearchItemFromIndex(idx *models.AlbumIndex) models.AlbumSearchItem {
	return models.AlbumSearchItem{
		AlbumID:          idx.AlbumID,
		OriginalFilename: idx.OriginalFilename,
		PhotoCount:       idx.PhotoCount,
		CreatedAt:        idx.CreatedAt,
	}
}
