package albums

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"viewer/internal/catalog"
	cfgpkg "viewer/internal/config"
	"viewer/internal/models"
	"viewer/internal/pipeline"
)

// FinalizeState is the JSON payload returned by the finalize endpoints.
type FinalizeState struct {
	AlbumID    string              `json:"albumId"`
	Status     catalog.AlbumStatus `json:"status"`
	PhotoCount int                 `json:"photoCount,omitempty"`
	CreatedAt  string              `json:"createdAt,omitempty"`
	Error      string              `json:"error,omitempty"`
	UpdatedAt  string              `json:"updatedAt"`
}

// Enqueuer schedules an album for background extraction. Enqueue blocks while
// the worker's queue is full, so it returns an error only when ctx ended or the
// worker stopped - never because the deployment is merely busy.
type Enqueuer interface {
	Enqueue(ctx context.Context, albumID string) error
}

type albumStore interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, map[string]string, error)
	HeadObject(ctx context.Context, key string) (bool, int64, error)
}

// Service owns upload creation and finalize bookkeeping. The heavy extraction
// work lives in the pipeline worker; album metadata lives in the SQLite
// catalog.
type Service struct {
	catalog  *catalog.Store
	store    albumStore
	enqueuer Enqueuer
}

// CreateUploadResult is the presigned-upload envelope returned to clients.
type CreateUploadResult struct {
	AlbumID   string
	Key       string
	UploadURL string
	Headers   map[string]string
}

func NewService(cat *catalog.Store, store albumStore, enqueuer Enqueuer) *Service {
	return &Service{
		catalog:  cat,
		store:    store,
		enqueuer: enqueuer,
	}
}

// CreateUpload registers a queued album and presigns its staging object.
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
		Status:           catalog.AlbumStatusQueued,
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
// (for example by the upload scan) and queues it for extraction.
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
	if s.enqueuer == nil {
		return fmt.Errorf("finalize worker is not initialized")
	}
	return s.enqueuer.Enqueue(ctx, albumID)
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

	// A succeeded album is done and a processing album is already scheduled.
	// A queued album may be freshly registered (creation now records QUEUED) or
	// waiting for a restart, so it falls through to the enqueue path below; the
	// pipeline ignores a duplicate enqueue for an album it already holds.
	switch album.Status {
	case catalog.AlbumStatusReady, catalog.AlbumStatusProcessing:
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
	if s.enqueuer == nil {
		err := fmt.Errorf("finalize worker is not initialized")
		_ = s.catalog.SetAlbumStatus(context.Background(), albumID, catalog.AlbumStatusFailed, err.Error())
		return FinalizeState{}, err
	}
	if err := s.enqueuer.Enqueue(ctx, albumID); err != nil {
		// A caller that gave up while the worker was busy (cancelled request,
		// deadline, or a shutdown that stopped the worker) has not failed the
		// album. It is already QUEUED in the catalog and the upload scan queues
		// any staged zip that is still waiting, so it must stay QUEUED rather
		// than become a terminal FAILED a client has to retry by hand.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, pipeline.ErrStopped) {
			album.Status = catalog.AlbumStatusQueued
			album.Error = ""
			return finalizeStateFromAlbum(album), err
		}
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

// AlbumForStagedObject returns the album that already owns a staged object key.
// The source_key recorded on the album is authoritative; a row written before
// that column was populated is matched through the key its upload used, which
// SourceKey still generates. A not-found result is not an error: it is how the
// upload scan learns that an object needs an album of its own.
func (s *Service) AlbumForStagedObject(ctx context.Context, sourceKey string) (*catalog.Album, bool, error) {
	if s == nil || s.catalog == nil {
		return nil, false, nil
	}
	sourceKey = strings.TrimSpace(sourceKey)
	if sourceKey == "" {
		return nil, false, nil
	}

	album, err := s.catalog.GetAlbumBySourceKey(ctx, sourceKey)
	if err == nil {
		return album, true, nil
	}
	if !errors.Is(err, catalog.ErrAlbumNotFound) {
		return nil, false, err
	}

	id, ok := pipeline.StagedAlbumID(sourceKey)
	if !ok {
		return nil, false, nil
	}
	album, err = s.catalog.GetAlbum(ctx, id)
	if err == nil {
		return album, true, nil
	}
	if errors.Is(err, catalog.ErrAlbumNotFound) {
		return nil, false, nil
	}
	return nil, false, err
}

// EnqueueStaged schedules an album whose staged zip is still waiting, waiting
// for room while the worker's queue is full. It leaves the album status alone:
// the pipeline sets PROCESSING when it starts and the terminal status when it
// finishes, so a scan that finds an album already being extracted cannot leave
// it looking queued. A duplicate enqueue is ignored by the pipeline.
func (s *Service) EnqueueStaged(ctx context.Context, albumID string) error {
	if s == nil || s.enqueuer == nil {
		return fmt.Errorf("finalize worker is not initialized")
	}
	return s.enqueuer.Enqueue(ctx, albumID)
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

// SearchAlbumsByName matches ready albums whose filename contains the query
// (case-insensitive substring), each with its cover photo when it has one.
func (s *Service) SearchAlbumsByName(ctx context.Context, q string, limit int) ([]models.AlbumSearchItem, error) {
	if s == nil || s.catalog == nil {
		return []models.AlbumSearchItem{}, nil
	}
	results, err := s.catalog.SearchAlbumsByName(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	items := make([]models.AlbumSearchItem, 0, len(results))
	for _, res := range results {
		item := models.AlbumSearchItem{
			AlbumID:          res.Album.ID,
			OriginalFilename: res.Album.OriginalFilename,
			PhotoCount:       res.Album.PhotoCount,
			CreatedAt:        res.Album.CreatedAt,
			SizeBytes:        res.Album.SizeBytes,
		}
		if res.Cover != nil {
			item.Cover = &models.AlbumCover{
				I:     res.Cover.Index,
				Hash:  res.Cover.Hash,
				W:     res.Cover.Width,
				H:     res.Cover.Height,
				Ratio: res.Cover.Ratio,
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// ReadyAlbumPhotoCounts returns every ready album's id with its photo count,
// without loading any photo rows.
func (s *Service) ReadyAlbumPhotoCounts(ctx context.Context) ([]models.AlbumPhotoCount, error) {
	if s == nil || s.catalog == nil {
		return []models.AlbumPhotoCount{}, nil
	}
	counts, err := s.catalog.ListReadyAlbumPhotoCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]models.AlbumPhotoCount, 0, len(counts))
	for _, count := range counts {
		out = append(out, models.AlbumPhotoCount{
			AlbumID:    count.AlbumID,
			PhotoCount: count.PhotoCount,
		})
	}
	return out, nil
}

// PhotoMetaAt returns one photo of an album by index.
func (s *Service) PhotoMetaAt(ctx context.Context, albumID string, index int) (*models.PhotoMeta, error) {
	if s == nil || s.catalog == nil {
		return nil, fmt.Errorf("%w: %s:%d", ErrPhotoNotFound, albumID, index)
	}
	photo, err := s.catalog.PhotoAt(ctx, albumID, index)
	if err != nil {
		if errors.Is(err, catalog.ErrPhotoNotFound) {
			return nil, fmt.Errorf("%w: %s:%d", ErrPhotoNotFound, albumID, index)
		}
		return nil, err
	}
	meta := photoMetaFromRow(*photo)
	return &meta, nil
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
		Hash:  photo.Hash,
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
		Status:     album.Status,
		PhotoCount: album.PhotoCount,
		CreatedAt:  album.CreatedAt,
		Error:      album.Error,
		UpdatedAt:  album.UpdatedAt,
	}
}
