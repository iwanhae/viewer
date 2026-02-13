package albums

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	cfgpkg "viewer/internal/config"
	"viewer/internal/models"
	"viewer/internal/storage"
)

type Service struct {
	cfg     cfgpkg.Config
	store   *storage.S3Store
	indexer *Indexer

	mu          sync.RWMutex
	albumCache  map[string]*models.AlbumIndex
	uploadHints map[string]string
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
		return nil, fmt.Errorf("source zip not found")
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
	delete(s.uploadHints, albumID)
	s.mu.Unlock()

	return idx, nil
}

func (s *Service) RefreshFromStorage(ctx context.Context) error {
	keys, err := s.store.ListAlbumIndexKeys(ctx)
	if err != nil {
		return err
	}

	next := make(map[string]*models.AlbumIndex, len(keys))
	for _, key := range keys {
		var idx models.AlbumIndex
		if err := s.store.ReadJSON(ctx, key, &idx); err != nil {
			continue
		}
		if idx.AlbumID == "" {
			parts := strings.Split(filepath.Dir(key), "/")
			if len(parts) > 1 {
				idx.AlbumID = parts[len(parts)-1]
			}
		}
		next[idx.AlbumID] = &idx
	}

	s.mu.Lock()
	s.albumCache = next
	s.mu.Unlock()
	return nil
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
		return nil, err
	}
	if idx.AlbumID == "" {
		idx.AlbumID = albumID
	}

	s.mu.Lock()
	s.albumCache[albumID] = &idx
	s.mu.Unlock()

	return &idx, nil
}

func (s *Service) ListAlbums(ctx context.Context) ([]models.AlbumSummary, error) {
	s.mu.RLock()
	if len(s.albumCache) == 0 {
		s.mu.RUnlock()
		if err := s.RefreshFromStorage(ctx); err != nil {
			return nil, err
		}
		s.mu.RLock()
	}

	out := make([]models.AlbumSummary, 0, len(s.albumCache))
	for _, idx := range s.albumCache {
		out = append(out, models.AlbumSummary{
			AlbumID:          idx.AlbumID,
			OriginalFilename: idx.OriginalFilename,
			PhotoCount:       idx.PhotoCount,
			CreatedAt:        idx.CreatedAt,
		})
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out, nil
}

func (s *Service) AllAlbums() []*models.AlbumIndex {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*models.AlbumIndex, 0, len(s.albumCache))
	for _, idx := range s.albumCache {
		dup := *idx
		dup.Photos = append([]models.PhotoMeta(nil), idx.Photos...)
		out = append(out, &dup)
	}
	return out
}

func (s *Service) DumpAlbumJSON(albumID string) ([]byte, error) {
	s.mu.RLock()
	idx, ok := s.albumCache[albumID]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("album not cached")
	}
	return json.Marshal(idx)
}
