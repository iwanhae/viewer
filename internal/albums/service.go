package albums

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

	mu                     sync.RWMutex
	albumCache             map[string]*models.AlbumIndex
	uploadHints            map[string]string
	albumSummariesSnapshot []models.AlbumSummary
	allAlbumsSnapshot      []*models.AlbumIndex
}

type RefreshProgress struct {
	Done        int
	Total       int
	Discovered  int
	Processed   int
	Succeeded   int
	Failed      int
	ListingDone bool
	AlbumID     string
	Key         string
	Err         error
}

type albumStore interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration) (string, map[string]string, error)
	PutObject(ctx context.Context, key string, body io.Reader, contentType string) error
	HeadObject(ctx context.Context, key string) (bool, int64, error)
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
	PutJSON(ctx context.Context, key string, v any) error
	ReadJSON(ctx context.Context, key string, out any) error
	ForEachAlbumIndexKey(ctx context.Context, fn func(key string) error) error
}

func NewService(cfg cfgpkg.Config, store *storage.S3Store, indexer *Indexer) *Service {
	return &Service{
		cfg:         cfg,
		store:       store,
		indexer:     indexer,
		albumCache:  make(map[string]*models.AlbumIndex),
		uploadHints: make(map[string]string),
	}
}

type CreateUploadResult struct {
	AlbumID string
	URL     string
	Headers map[string]string
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
	url, headers, err := s.store.PresignPut(ctx, sourceKey(albumID), s.cfg.PresignTTL)
	if err != nil {
		return CreateUploadResult{}, err
	}

	s.mu.Lock()
	s.uploadHints[albumID] = filename
	s.mu.Unlock()

	return CreateUploadResult{
		AlbumID: albumID,
		URL:     url,
		Headers: headers,
	}, nil
}

func (s *Service) UploadSource(ctx context.Context, albumID string, filename string, reader io.Reader) error {
	if strings.TrimSpace(albumID) == "" {
		return fmt.Errorf("albumId is required")
	}
	if reader == nil {
		return fmt.Errorf("file is required")
	}
	if err := s.store.PutObject(ctx, sourceKey(albumID), reader, "application/zip"); err != nil {
		return err
	}

	if strings.TrimSpace(filename) != "" {
		s.mu.Lock()
		s.uploadHints[albumID] = filename
		s.mu.Unlock()
	}
	return nil
}

func (s *Service) Finalize(ctx context.Context, albumID string) (*models.AlbumIndex, error) {
	if strings.TrimSpace(albumID) == "" {
		return nil, fmt.Errorf("albumId is required")
	}

	exists, _, err := s.store.HeadObject(ctx, sourceKey(albumID))
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", ErrAlbumSourceNotFound, albumID)
	}

	body, _, err := s.store.GetObject(ctx, sourceKey(albumID))
	if err != nil {
		return nil, err
	}
	defer body.Close()

	tmpFile, err := os.CreateTemp("", "viewer-album-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, body); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("download zip: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close temp file: %w", err)
	}

	originalFilename := "source.zip"
	s.mu.RLock()
	if hinted, ok := s.uploadHints[albumID]; ok && hinted != "" {
		originalFilename = hinted
	}
	s.mu.RUnlock()

	idx, err := s.indexer.BuildFromZip(tmpPath, albumID, originalFilename)
	if err != nil {
		return nil, err
	}

	if err := s.store.PutJSON(ctx, indexKey(albumID), idx); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.albumCache[albumID] = idx
	s.invalidateSnapshotsLocked()
	delete(s.uploadHints, albumID)
	s.mu.Unlock()

	return idx, nil
}

func (s *Service) RefreshFromStorage(ctx context.Context) error {
	return s.refreshFromStorage(ctx, nil, nil)
}

func (s *Service) RefreshFromStorageWithProgress(ctx context.Context, onProgress func(RefreshProgress)) error {
	return s.refreshFromStorage(ctx, onProgress, nil)
}

func (s *Service) RefreshFromStorageWithProgressAndAlbum(ctx context.Context, onProgress func(RefreshProgress), onAlbum func(models.AlbumIndex)) error {
	return s.refreshFromStorage(ctx, onProgress, onAlbum)
}

func (s *Service) refreshFromStorage(ctx context.Context, onProgress func(RefreshProgress), onAlbum func(models.AlbumIndex)) error {
	workerCount := warmupWorkerCount(s.cfg.WarmupFetchConcurrency)
	keyCh := make(chan string, workerCount*2)
	resultCh := make(chan RefreshProgress, workerCount*2)

	var discovered atomic.Int64
	var processed atomic.Int64
	var succeeded atomic.Int64
	var failed atomic.Int64
	var listingDone atomic.Bool

	producerDone := make(chan error, 1)
	go func() {
		defer close(keyCh)
		err := s.store.ForEachAlbumIndexKey(ctx, func(key string) error {
			select {
			case keyCh <- key:
				discovered.Add(1)
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		listingDone.Store(true)
		producerDone <- err
	}()

	var workers sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for key := range keyCh {
				progress := RefreshProgress{Key: key}
				var idx models.AlbumIndex
				if err := s.store.ReadJSON(ctx, key, &idx); err != nil {
					progress.Err = err
					resultCh <- progress
					continue
				}

				if idx.AlbumID == "" {
					parts := strings.Split(filepath.Dir(key), "/")
					if len(parts) > 0 {
						idx.AlbumID = parts[len(parts)-1]
					}
				}
				if idx.AlbumID == "" {
					progress.Err = fmt.Errorf("missing album id")
					resultCh <- progress
					continue
				}

				cached := new(models.AlbumIndex)
				*cached = idx
				s.mu.Lock()
				s.albumCache[idx.AlbumID] = cached
				s.invalidateSnapshotsLocked()
				s.mu.Unlock()
				if onAlbum != nil {
					onAlbum(idx)
				}

				progress.AlbumID = idx.AlbumID
				resultCh <- progress
			}
		}()
	}

	go func() {
		workers.Wait()
		close(resultCh)
	}()

	for progress := range resultCh {
		done := processed.Add(1)
		if progress.Err != nil {
			failed.Add(1)
		} else {
			succeeded.Add(1)
		}
		progress.Done = int(done)
		progress.Total = int(discovered.Load())
		progress.Discovered = int(discovered.Load())
		progress.Processed = int(done)
		progress.Succeeded = int(succeeded.Load())
		progress.Failed = int(failed.Load())
		progress.ListingDone = listingDone.Load()
		if onProgress != nil {
			onProgress(progress)
		}
	}

	if err := <-producerDone; err != nil {
		return err
	}
	return nil
}

func warmupWorkerCount(configured int) int {
	if configured > 0 {
		return configured
	}
	workers := runtime.GOMAXPROCS(0) * 2
	if workers < 2 {
		return 2
	}
	if workers > 32 {
		return 32
	}
	return workers
}

func mergeAlbumCaches(existing map[string]*models.AlbumIndex, scanned map[string]*models.AlbumIndex) map[string]*models.AlbumIndex {
	merged := make(map[string]*models.AlbumIndex, len(existing)+len(scanned))
	for albumID, idx := range existing {
		merged[albumID] = idx
	}
	for albumID, idx := range scanned {
		merged[albumID] = idx
	}
	return merged
}

func (s *Service) GetAlbum(ctx context.Context, albumID string) (*models.AlbumIndex, error) {
	s.mu.RLock()
	if idx, ok := s.albumCache[albumID]; ok {
		dup := *idx
		s.mu.RUnlock()
		return &dup, nil
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

	s.mu.Lock()
	s.albumCache[albumID] = &idx
	s.invalidateSnapshotsLocked()
	s.mu.Unlock()

	return &idx, nil
}

func (s *Service) ListAlbums(ctx context.Context) ([]models.AlbumSummary, error) {
	_ = ctx
	s.mu.RLock()
	if s.albumSummariesSnapshot != nil {
		out := cloneAlbumSummarySlice(s.albumSummariesSnapshot)
		s.mu.RUnlock()
		return out, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	if s.albumSummariesSnapshot == nil || s.allAlbumsSnapshot == nil {
		s.rebuildSnapshotsLocked()
	}
	out := cloneAlbumSummarySlice(s.albumSummariesSnapshot)
	s.mu.Unlock()
	return out, nil
}

func (s *Service) AllAlbums() []*models.AlbumIndex {
	s.mu.RLock()
	if s.allAlbumsSnapshot != nil {
		out := cloneAlbumIndexSlice(s.allAlbumsSnapshot)
		s.mu.RUnlock()
		return out
	}
	s.mu.RUnlock()

	s.mu.Lock()
	if s.albumSummariesSnapshot == nil || s.allAlbumsSnapshot == nil {
		s.rebuildSnapshotsLocked()
	}
	out := cloneAlbumIndexSlice(s.allAlbumsSnapshot)
	s.mu.Unlock()
	return out
}

func (s *Service) DumpAlbumJSON(albumID string) ([]byte, error) {
	s.mu.RLock()
	idx, ok := s.albumCache[albumID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAlbumNotFound, albumID)
	}
	return json.Marshal(idx)
}

func (s *Service) invalidateSnapshotsLocked() {
	s.albumSummariesSnapshot = nil
	s.allAlbumsSnapshot = nil
}

func (s *Service) rebuildSnapshotsLocked() {
	summaries := make([]models.AlbumSummary, 0, len(s.albumCache))
	albumsList := make([]*models.AlbumIndex, 0, len(s.albumCache))
	for _, idx := range s.albumCache {
		summaries = append(summaries, models.AlbumSummary{
			AlbumID:          idx.AlbumID,
			OriginalFilename: idx.OriginalFilename,
			PhotoCount:       idx.PhotoCount,
			CreatedAt:        idx.CreatedAt,
		})
		albumsList = append(albumsList, cloneAlbumIndex(idx))
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].CreatedAt > summaries[j].CreatedAt
	})
	sort.Slice(albumsList, func(i, j int) bool {
		if albumsList[i].AlbumID != albumsList[j].AlbumID {
			return albumsList[i].AlbumID < albumsList[j].AlbumID
		}
		if albumsList[i].CreatedAt != albumsList[j].CreatedAt {
			return albumsList[i].CreatedAt < albumsList[j].CreatedAt
		}
		return albumsList[i].OriginalFilename < albumsList[j].OriginalFilename
	})

	s.albumSummariesSnapshot = summaries
	s.allAlbumsSnapshot = albumsList
}

func cloneAlbumSummarySlice(in []models.AlbumSummary) []models.AlbumSummary {
	return append([]models.AlbumSummary(nil), in...)
}

func cloneAlbumIndexSlice(in []*models.AlbumIndex) []*models.AlbumIndex {
	out := make([]*models.AlbumIndex, 0, len(in))
	for _, idx := range in {
		out = append(out, cloneAlbumIndex(idx))
	}
	return out
}

func cloneAlbumIndex(idx *models.AlbumIndex) *models.AlbumIndex {
	if idx == nil {
		return nil
	}
	dup := *idx
	dup.Photos = append([]models.PhotoMeta(nil), idx.Photos...)
	return &dup
}
