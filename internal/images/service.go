package images

import (
	"archive/zip"
	"context"
	"fmt"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"path/filepath"
	"strings"

	_ "golang.org/x/image/webp"
	"viewer/internal/albums"
	"viewer/internal/cache"
	"viewer/internal/rangecache"
	"viewer/internal/storage"
)

type Service struct {
	albums     *albums.Service
	store      *storage.S3Store
	cache      *cache.DiskCache
	rangeCache *rangecache.Manager
}

type ImageResult struct {
	Bytes       []byte
	ContentType string
}

const (
	defaultRangeChunkSize = int64(1 << 17) // 128 KiB
	rangeMaxBytes         = int64(8 << 30) // 8 GiB
)

func NewService(albumsService *albums.Service, store *storage.S3Store, cacheDir string, zipCacheDir string) (*Service, error) {
	dc, err := cache.NewDiskCache(cacheDir)
	if err != nil {
		return nil, err
	}
	rc, err := rangecache.NewManager(
		filepath.Join(zipCacheDir, "range"),
		rangecache.Config{
			ChunkSize: defaultRangeChunkSize,
			MaxBytes:  rangeMaxBytes,
			Fetch: func(ctx context.Context, key string, start int64, end int64) (io.ReadCloser, error) {
				body, _, err := store.GetObjectRange(ctx, key, start, end)
				if err != nil {
					return nil, err
				}
				return body, nil
			},
		},
	)
	if err != nil {
		return nil, err
	}

	return &Service{
		albums:     albumsService,
		store:      store,
		cache:      dc,
		rangeCache: rc,
	}, nil
}

func sourceKey(albumID string) string {
	return fmt.Sprintf("albums/%s/source.zip", albumID)
}

func (s *Service) GetImage(ctx context.Context, albumID string, idx int) (ImageResult, error) {
	album, err := s.albums.GetAlbum(ctx, albumID)
	if err != nil {
		return ImageResult{}, err
	}
	if idx < 0 || idx >= len(album.Photos) {
		return ImageResult{}, fmt.Errorf("%w: %d", ErrPhotoIndexOutOfRange, idx)
	}

	photo := album.Photos[idx]
	return s.GetImageByEntry(ctx, albumID, photo.Name)
}

func (s *Service) GetImageByEntry(ctx context.Context, albumID string, entryName string) (ImageResult, error) {
	if strings.TrimSpace(albumID) == "" {
		return ImageResult{}, fmt.Errorf("album id is required")
	}
	cleanEntryName := strings.TrimSpace(entryName)
	if cleanEntryName == "" {
		return ImageResult{}, fmt.Errorf("entry name is required")
	}

	key := sourceKey(albumID)
	exists, size, err := s.store.HeadObject(ctx, key)
	if err != nil {
		return ImageResult{}, err
	}
	if !exists || size <= 0 {
		return ImageResult{}, fmt.Errorf("%w: %s", albums.ErrAlbumSourceNotFound, albumID)
	}

	handle, err := s.rangeCache.Open(ctx, key, size)
	if err != nil {
		return ImageResult{}, fmt.Errorf("open range cache: %w", err)
	}
	defer handle.Close()

	r, err := zip.NewReader(handle, size)
	if err != nil {
		return ImageResult{}, fmt.Errorf("open zip: %w", err)
	}

	for _, f := range r.File {
		if f.Name != cleanEntryName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return ImageResult{}, fmt.Errorf("open zip entry: %w", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return ImageResult{}, fmt.Errorf("read zip entry: %w", err)
		}
		return ImageResult{
			Bytes:       data,
			ContentType: contentTypeForEntry(f.Name),
		}, nil
	}

	return ImageResult{}, fmt.Errorf("%w: %s", ErrImageEntryNotFound, cleanEntryName)
}

func contentTypeForEntry(entryName string) string {
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(entryName)))
	if ct == "" {
		return "application/octet-stream"
	}
	return ct
}
