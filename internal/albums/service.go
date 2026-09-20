package albums

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"viewer/internal/catalog"
	cfgpkg "viewer/internal/config"
	"viewer/internal/models"
	"viewer/internal/pipeline"
)

// FinalizeStatus mirrors the album pipeline state exposed over the HTTP API.
type FinalizeStatus string

const (
	FinalizeStatusQueued     FinalizeStatus = "QUEUED"
	FinalizeStatusProcessing FinalizeStatus = "PROCESSING"
	FinalizeStatusSucceeded  FinalizeStatus = "SUCCEEDED"
	FinalizeStatusFailed     FinalizeStatus = "FAILED"
)

// FinalizeState is the JSON payload returned by the finalize endpoints.
type FinalizeState struct {
	AlbumID    string         `json:"albumId"`
	Status     FinalizeStatus `json:"status"`
	PhotoCount int            `json:"photoCount,omitempty"`
	CreatedAt  string         `json:"createdAt,omitempty"`
	Error      string         `json:"error,omitempty"`
	UpdatedAt  string         `json:"updatedAt"`
}

// Enqueuer schedules an album for background extraction.
type Enqueuer interface {
	Enqueue(albumID string) error
}

type albumStore interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, map[string]string, error)
	HeadObject(ctx context.Context, key string) (bool, int64, error)
}

// Service owns upload creation and finalize bookkeeping. The heavy extraction
// work lives in the pipeline worker; album metadata lives in the SQLite
// catalog.
type Service struct {
	catalog *catalog.Store
	store   albumStore

	mu       sync.RWMutex
	enqueuer Enqueuer
}

// CreateUploadResult is the presigned-upload envelope returned to clients.
type CreateUploadResult struct {
	AlbumID   string
	Key       string
	UploadURL string
	Headers   map[string]string
}

func NewService(cat *catalog.Store, store albumStore) *Service {
	return &Service{
		catalog: cat,
		store:   store,
	}
}

// SetEnqueuer wires the pipeline used to schedule extraction.
func (s *Service) SetEnqueuer(enqueuer Enqueuer) {
	s.mu.Lock()
	s.enqueuer = enqueuer
	s.mu.Unlock()
}

// CreateUpload registers a pending album and presigns its staging object.
func (s *Service) CreateUpload(ctx context.Context, filename string, sizeBytes int64) (CreateUploadResult, error) {
	if s == nil || s.catalog == nil {
		return CreateUploadResult{}, fmt.Errorf("album catalog is not initialized")
	}
	filename = strings.TrimSpace(filename)
	if filename == "" {
		return CreateUploadResult{}, fmt.Errorf("filename is required")
	}
	if sizeBytes <= 0 {
		return CreateUploadResult{}, fmt.Errorf("sizeBytes must be > 0")
	}
	if sizeBytes > cfgpkg.MaxUploadBytes {
		return CreateUploadResult{}, fmt.Errorf("sizeBytes exceeds the %d byte upload limit", cfgpkg.MaxUploadBytes)
	}

	albumID := uuid.NewString()
	key := pipeline.SourceKey(albumID)
	url, headers, err := s.store.PresignPut(ctx, key, cfgpkg.PresignTTL)
	if err != nil {
		return CreateUploadResult{}, err
	}
	if headers == nil {
		headers = map[string]string{}
	}

	if err := s.catalog.CreateAlbum(ctx, catalog.Album{
		ID:               albumID,
		OriginalFilename: filename,
		SizeBytes:        sizeBytes,
		Status:           catalog.AlbumStatusPending,
		SourceKey:        key,
	}); err != nil {
		return CreateUploadResult{}, err
	}

	return CreateUploadResult{
		AlbumID:   albumID,
		Key:       key,
		UploadURL: url,
		Headers:   headers,
	}, nil
}

// RegisterStagedUpload records an album whose zip was placed in S3 out of band
// (for example by the batch ingest scanner) and queues it for extraction.
func (s *Service) RegisterStagedUpload(ctx context.Context, albumID string, filename string, sizeBytes int64, sourceKey string) error {
	albumID = strings.TrimSpace(albumID)
	if albumID == "" {
		return fmt.Errorf("album id is required")
	}
	if strings.TrimSpace(sourceKey) == "" {
		sourceKey = pipeline.SourceKey(albumID)
	}
	if err := s.catalog.UpsertAlbum(ctx, catalog.Album{
		ID:               albumID,
		OriginalFilename: filename,
		SizeBytes:        sizeBytes,
		Status:           catalog.AlbumStatusQueued,
		SourceKey:        sourceKey,
	}); err != nil {
		return err
	}
	return s.enqueue(albumID)
}

// RequestFinalize schedules extraction for an uploaded zip.
func (s *Service) RequestFinalize(ctx context.Context, albumID string) (FinalizeState, error) {
	albumID = strings.TrimSpace(albumID)
	if albumID == "" {
		return FinalizeState{}, fmt.Errorf("albumId is required")
	}

	album, err := s.catalog.GetAlbum(ctx, albumID)
	if err != nil {
		if errors.Is(err, catalog.ErrAlbumNotFound) {
			return FinalizeState{}, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
		}
		return FinalizeState{}, err
	}

	switch album.Status {
	case catalog.AlbumStatusReady:
		return finalizeStateFromAlbum(album), nil
	case catalog.AlbumStatusProcessing, catalog.AlbumStatusQueued:
		return finalizeStateFromAlbum(album), nil
	}

	sourceKey := album.SourceKey
	if strings.TrimSpace(sourceKey) == "" {
		sourceKey = pipeline.SourceKey(albumID)
	}
	exists, size, err := s.store.HeadObject(ctx, sourceKey)
	if err != nil {
		return FinalizeState{}, err
	}
	if !exists || size <= 0 {
		return FinalizeState{}, fmt.Errorf("%w: %s", ErrAlbumSourceNotFound, albumID)
	}

	if err := s.catalog.SetAlbumStatus(ctx, albumID, catalog.AlbumStatusQueued, ""); err != nil {
		return FinalizeState{}, err
	}
	if err := s.enqueue(albumID); err != nil {
		_ = s.catalog.SetAlbumStatus(context.Background(), albumID, catalog.AlbumStatusFailed, err.Error())
		return FinalizeState{}, err
	}

	album.Status = catalog.AlbumStatusQueued
	album.Error = ""
	album.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return finalizeStateFromAlbum(album), nil
}

// GetFinalizeStatus returns the current extraction state of an album.
func (s *Service) GetFinalizeStatus(ctx context.Context, albumID string) (FinalizeState, error) {
	albumID = strings.TrimSpace(albumID)
	if albumID == "" {
		return FinalizeState{}, fmt.Errorf("albumId is required")
	}
	if s == nil || s.catalog == nil {
		return FinalizeState{}, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
	}
	album, err := s.catalog.GetAlbum(ctx, albumID)
	if err != nil {
		if errors.Is(err, catalog.ErrAlbumNotFound) {
			return FinalizeState{}, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
		}
		return FinalizeState{}, err
	}
	return finalizeStateFromAlbum(album), nil
}

// EnqueuePending schedules every album that still has a staged zip waiting.
func (s *Service) EnqueuePending(ctx context.Context) (int, error) {
	s.mu.RLock()
	enqueuer := s.enqueuer
	s.mu.RUnlock()
	if enqueuer == nil {
		return 0, fmt.Errorf("finalize worker is not initialized")
	}

	total := 0
	// Collect first: processing an album mutates its status, so listing status
	// by status while enqueueing would visit the same album twice.
	byID := make(map[string]catalog.Album)
	for _, status := range []catalog.AlbumStatus{catalog.AlbumStatusPending, catalog.AlbumStatusQueued, catalog.AlbumStatusProcessing} {
		albums, err := s.catalog.ListAlbumsByStatus(ctx, status)
		if err != nil {
			return total, err
		}
		for _, album := range albums {
			byID[album.ID] = album
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, albumID := range ids {
		album := byID[albumID]
		sourceKey := album.SourceKey
		if strings.TrimSpace(sourceKey) == "" {
			sourceKey = pipeline.SourceKey(album.ID)
		}
		exists, size, err := s.store.HeadObject(ctx, sourceKey)
		if err != nil || !exists || size <= 0 {
			continue
		}
		if err := s.catalog.SetAlbumStatus(ctx, album.ID, catalog.AlbumStatusQueued, ""); err != nil {
			continue
		}
		if err := enqueuer.Enqueue(album.ID); err != nil {
			continue
		}
		total++
	}
	return total, nil
}

func (s *Service) enqueue(albumID string) error {
	s.mu.RLock()
	enqueuer := s.enqueuer
	s.mu.RUnlock()
	if enqueuer == nil {
		return fmt.Errorf("finalize worker is not initialized")
	}
	return enqueuer.Enqueue(albumID)
}

// GetAlbum returns an album and its photos in the legacy API shape.
func (s *Service) GetAlbum(ctx context.Context, albumID string) (*models.AlbumIndex, error) {
	if s == nil || s.catalog == nil {
		return nil, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
	}
	album, err := s.catalog.GetAlbum(ctx, albumID)
	if err != nil {
		if errors.Is(err, catalog.ErrAlbumNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
		}
		return nil, err
	}
	photos, err := s.catalog.PhotosByAlbum(ctx, albumID)
	if err != nil {
		return nil, err
	}
	return albumIndexFromRows(album, photos), nil
}

// SearchAlbumsByNamePrefix matches ready albums by filename prefix.
func (s *Service) SearchAlbumsByNamePrefix(ctx context.Context, q string, limit int) ([]models.AlbumSearchItem, error) {
	if s == nil || s.catalog == nil {
		return []models.AlbumSearchItem{}, nil
	}
	albumsList, err := s.catalog.SearchAlbumsByNamePrefix(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	items := make([]models.AlbumSearchItem, 0, len(albumsList))
	for _, album := range albumsList {
		items = append(items, models.AlbumSearchItem{
			AlbumID:          album.ID,
			OriginalFilename: album.OriginalFilename,
			PhotoCount:       album.PhotoCount,
			CreatedAt:        album.CreatedAt,
		})
	}
	return items, nil
}

// AllAlbums returns every ready album with its photos.
func (s *Service) AllAlbums() []*models.AlbumIndex {
	if s == nil || s.catalog == nil {
		return nil
	}
	ctx := context.Background()
	albumsList, err := s.catalog.ListAlbumsByStatus(ctx, catalog.AlbumStatusReady)
	if err != nil {
		return nil
	}
	photos, err := s.catalog.ListReadyAlbumPhotos(ctx)
	if err != nil {
		return nil
	}

	photosByAlbum := make(map[string][]models.PhotoMeta, len(albumsList))
	for _, photo := range photos {
		photosByAlbum[photo.AlbumID] = append(photosByAlbum[photo.AlbumID], photoMetaFromRow(photo))
	}

	out := make([]*models.AlbumIndex, 0, len(albumsList))
	for _, album := range albumsList {
		albumPhotos := photosByAlbum[album.ID]
		if len(albumPhotos) == 0 {
			continue
		}
		out = append(out, &models.AlbumIndex{
			AlbumID:          album.ID,
			OriginalFilename: album.OriginalFilename,
			CreatedAt:        album.CreatedAt,
			PhotoCount:       len(albumPhotos),
			Photos:           albumPhotos,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].AlbumID != out[j].AlbumID {
			return out[i].AlbumID < out[j].AlbumID
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out
}

func albumIndexFromRows(album *catalog.Album, photos []catalog.Photo) *models.AlbumIndex {
	metas := make([]models.PhotoMeta, 0, len(photos))
	for _, photo := range photos {
		metas = append(metas, photoMetaFromRow(photo))
	}
	return &models.AlbumIndex{
		AlbumID:          album.ID,
		OriginalFilename: album.OriginalFilename,
		CreatedAt:        album.CreatedAt,
		PhotoCount:       len(metas),
		Photos:           metas,
	}
}

func photoMetaFromRow(photo catalog.Photo) models.PhotoMeta {
	return models.PhotoMeta{
		I:     photo.Index,
		Name:  photo.Name,
		W:     photo.Width,
		H:     photo.Height,
		Ratio: photo.Ratio,
	}
}

func finalizeStateFromAlbum(album *catalog.Album) FinalizeState {
	if album == nil {
		return FinalizeState{}
	}
	return FinalizeState{
		AlbumID:    album.ID,
		Status:     finalizeStatusFromCatalog(album.Status),
		PhotoCount: album.PhotoCount,
		CreatedAt:  album.CreatedAt,
		Error:      album.Error,
		UpdatedAt:  album.UpdatedAt,
	}
}

func finalizeStatusFromCatalog(status catalog.AlbumStatus) FinalizeStatus {
	switch status {
	case catalog.AlbumStatusProcessing:
		return FinalizeStatusProcessing
	case catalog.AlbumStatusReady:
		return FinalizeStatusSucceeded
	case catalog.AlbumStatusFailed:
		return FinalizeStatusFailed
	default:
		return FinalizeStatusQueued
	}
}
