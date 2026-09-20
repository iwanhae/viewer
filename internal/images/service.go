package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"viewer/internal/albums"
	"viewer/internal/cache"
	"viewer/internal/catalog"
	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

// blobStore is the S3 surface needed to read image blobs.
type blobStore interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
}

// Service resolves a (album, index) photo reference to the raw image bytes
// stored in S3 under its content hash.
type Service struct {
	catalog *catalog.Store
	store   blobStore
	cache   *cache.DiskCache
}

// ImageResult is raw image bytes ready to be written to an HTTP response.
type ImageResult struct {
	Bytes       []byte
	ContentType string
}

func NewService(cat *catalog.Store, store blobStore, cacheDir string) (*Service, error) {
	dc, err := cache.NewDiskCache(cacheDir)
	if err != nil {
		return nil, err
	}
	return &Service{
		catalog: cat,
		store:   store,
		cache:   dc,
	}, nil
}

// GetImage returns the image at the given index within an album.
func (s *Service) GetImage(ctx context.Context, albumID string, idx int) (ImageResult, error) {
	if strings.TrimSpace(albumID) == "" {
		return ImageResult{}, fmt.Errorf("album id is required")
	}
	if idx < 0 {
		return ImageResult{}, fmt.Errorf("%w: %d", ErrPhotoIndexOutOfRange, idx)
	}

	if _, err := s.catalog.GetAlbum(ctx, albumID); err != nil {
		if errors.Is(err, catalog.ErrAlbumNotFound) {
			return ImageResult{}, fmt.Errorf("%w: %s", albums.ErrAlbumNotFound, albumID)
		}
		return ImageResult{}, err
	}

	photo, err := s.catalog.PhotoAt(ctx, albumID, idx)
	if err != nil {
		if errors.Is(err, catalog.ErrPhotoNotFound) {
			return ImageResult{}, fmt.Errorf("%w: %d", ErrPhotoIndexOutOfRange, idx)
		}
		return ImageResult{}, err
	}
	return s.GetImageByHash(ctx, photo.Hash)
}

// GetImageByHash returns the raw bytes of a content-addressed blob, preferring
// the local disk cache.
func (s *Service) GetImageByHash(ctx context.Context, hash string) (ImageResult, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return ImageResult{}, fmt.Errorf("blob hash is required")
	}

	blob, err := s.catalog.GetBlob(ctx, hash)
	if err != nil {
		if errors.Is(err, catalog.ErrBlobNotFound) {
			return ImageResult{}, fmt.Errorf("%w: %s", ErrImageEntryNotFound, hash)
		}
		return ImageResult{}, err
	}

	if data, ok := s.cache.Get(hash); ok {
		return ImageResult{Bytes: data, ContentType: contentTypeOrFallback(blob.ContentType)}, nil
	}

	body, remoteContentType, err := s.store.GetObject(ctx, pipeline.BlobKey(hash))
	if err != nil {
		if storage.IsNotFound(err) {
			return ImageResult{}, fmt.Errorf("%w: %s", ErrImageEntryNotFound, hash)
		}
		return ImageResult{}, err
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return ImageResult{}, fmt.Errorf("read blob %s: %w", hash, err)
	}
	if err := s.cache.Set(hash, data); err != nil {
		// A cache write failure must not fail the request.
		_ = err
	}

	contentType := contentTypeOrFallback(blob.ContentType)
	if contentType == "application/octet-stream" {
		contentType = contentTypeOrFallback(remoteContentType)
	}
	return ImageResult{Bytes: data, ContentType: contentType}, nil
}

func contentTypeOrFallback(contentType string) string {
	trimmed := strings.TrimSpace(contentType)
	if trimmed == "" {
		return "application/octet-stream"
	}
	return trimmed
}
