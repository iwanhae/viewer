package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

// ImageStream is an image body backed by a file in the disk cache. The reader
// supports seeking so that http.ServeContent can answer range requests.
type ImageStream struct {
	Content     io.ReadSeeker
	SizeBytes   int64
	ContentType string
	Hash        string

	closer io.Closer
}

// Close releases the underlying cached file.
func (s *ImageStream) Close() error {
	if s == nil || s.closer == nil {
		return nil
	}
	err := s.closer.Close()
	s.closer = nil
	return err
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

// OpenImage returns the image at the given index within an album as a
// streamable result materialised in the disk cache.
func (s *Service) OpenImage(ctx context.Context, albumID string, idx int) (*ImageStream, error) {
	if strings.TrimSpace(albumID) == "" {
		return nil, fmt.Errorf("album id is required")
	}
	if idx < 0 {
		return nil, fmt.Errorf("%w: %d", ErrPhotoIndexOutOfRange, idx)
	}

	if _, err := s.catalog.GetAlbum(ctx, albumID); err != nil {
		if errors.Is(err, catalog.ErrAlbumNotFound) {
			return nil, fmt.Errorf("%w: %s", albums.ErrAlbumNotFound, albumID)
		}
		return nil, err
	}

	photo, err := s.catalog.PhotoAt(ctx, albumID, idx)
	if err != nil {
		if errors.Is(err, catalog.ErrPhotoNotFound) {
			return nil, fmt.Errorf("%w: %d", ErrPhotoIndexOutOfRange, idx)
		}
		return nil, err
	}
	return s.openImageByHash(ctx, photo.Hash)
}

// GetImageByHash returns the raw bytes of a content-addressed blob, preferring
// the local disk cache. Recommendation workers still need the bytes in memory.
func (s *Service) GetImageByHash(ctx context.Context, hash string) (ImageResult, error) {
	stream, err := s.openImageByHash(ctx, hash)
	if err != nil {
		return ImageResult{}, err
	}
	defer stream.Close()

	data, err := io.ReadAll(stream.Content)
	if err != nil {
		return ImageResult{}, fmt.Errorf("read blob %s: %w", stream.Hash, err)
	}
	return ImageResult{Bytes: data, ContentType: stream.ContentType}, nil
}

// openImageByHash materialises a content-addressed blob in the disk cache and
// returns an open handle to the cached file.
func (s *Service) openImageByHash(ctx context.Context, hash string) (*ImageStream, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, fmt.Errorf("blob hash is required")
	}

	blob, err := s.catalog.GetBlob(ctx, hash)
	if err != nil {
		if errors.Is(err, catalog.ErrBlobNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrImageEntryNotFound, hash)
		}
		return nil, err
	}

	// The content type advertised by S3 is only consulted on a cache miss,
	// which matches the previous cache-hit behaviour.
	remoteContentType := ""
	path, size, err := s.cache.Materialize(hash, func() (io.ReadCloser, error) {
		body, contentType, err := s.store.GetObject(ctx, pipeline.BlobKey(hash))
		if err != nil {
			if storage.IsNotFound(err) {
				return nil, fmt.Errorf("%w: %s", ErrImageEntryNotFound, hash)
			}
			return nil, err
		}
		remoteContentType = contentType
		return body, nil
	})
	if err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cached blob %s: %w", hash, err)
	}

	contentType := contentTypeOrFallback(blob.ContentType)
	if contentType == "application/octet-stream" {
		contentType = contentTypeOrFallback(remoteContentType)
	}
	return &ImageStream{
		Content:     file,
		SizeBytes:   size,
		ContentType: contentType,
		Hash:        hash,
		closer:      file,
	}, nil
}

func contentTypeOrFallback(contentType string) string {
	trimmed := strings.TrimSpace(contentType)
	if trimmed == "" {
		return "application/octet-stream"
	}
	return trimmed
}
