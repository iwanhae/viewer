package images

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"strings"

	_ "image/gif"  // register the GIF decoder for image.Decode
	_ "image/png"  // register the PNG decoder for image.Decode
	_ "golang.org/x/image/webp" // register the WebP decoder for image.Decode

	"golang.org/x/image/draw"

	"viewer/internal/albums"
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
}

// ImageResult is raw image bytes ready to be written to an HTTP response.
type ImageResult struct {
	Bytes       []byte
	ContentType string
}

// ImageStream is an image body read from S3 into memory. The reader supports
// seeking so that http.ServeContent can answer range requests, and Close is
// kept so handlers can defer it exactly as they did for the file-backed stream.
type ImageStream struct {
	Content     io.ReadSeeker
	SizeBytes   int64
	ContentType string
	Hash        string
}

// Close releases the stream. The body is fully read before the stream is
// returned, so there is nothing left to release; the method stays so callers
// can defer it unconditionally.
func (s *ImageStream) Close() error {
	return nil
}

func NewService(cat *catalog.Store, store blobStore) *Service {
	return &Service{
		catalog: cat,
		store:   store,
	}
}

// OpenImage returns the image at the given index within an album, fetched from
// S3 for this request.
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

// WidthLadder lists the scaled widths the image endpoint accepts. The ladder
// is deliberately short: every entry is a compile-time choice, not config.
func WidthLadder() []int {
	return []int{320, 640, 1024}
}

// IsSupportedWidth reports whether w is on the width ladder.
func IsSupportedWidth(w int) bool {
	for _, candidate := range WidthLadder() {
		if candidate == w {
			return true
		}
	}
	return false
}

// OpenImageScaled returns the image at the given index scaled to fit within
// width, preserving aspect ratio. Images already no wider than the request are
// served with their original bytes; anything larger becomes a JPEG so the
// scaled response stays small. The content hash doubles as the response
// validator: scaled variants append their width to the blob hash.
func (s *Service) OpenImageScaled(ctx context.Context, albumID string, idx, width int) (*ImageStream, error) {
	if !IsSupportedWidth(width) {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedWidth, width)
	}
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

	data, contentType, err := s.fetchBlob(ctx, photo.Hash)
	if err != nil {
		return nil, err
	}
	// Up-scaling adds nothing, so a small original passes through untouched
	// with its own content type and hash.
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("probe image %s: %w", photo.Hash, err)
	}
	if config.Width <= width {
		return &ImageStream{
			Content:     bytes.NewReader(data),
			SizeBytes:   int64(len(data)),
			ContentType: contentType,
			Hash:        photo.Hash,
		}, nil
	}
	encoded, err := encodeScaledJPEG(data, width)
	if err != nil {
		return nil, err
	}
	return &ImageStream{
		Content:     bytes.NewReader(encoded),
		SizeBytes:   int64(len(encoded)),
		ContentType: "image/jpeg",
		Hash:        fmt.Sprintf("%s:w%d", photo.Hash, width),
	}, nil
}

// encodeScaledJPEG decodes an original image and resamples it to fit within
// width, preserving aspect ratio.
func encodeScaledJPEG(data []byte, width int) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}
	srcBounds := src.Bounds()
	dstH := srcBounds.Dy() * width / srcBounds.Dx()
	if dstH < 1 {
		dstH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, width, dstH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, srcBounds, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, fmt.Errorf("encode scaled image: %w", err)
	}
	return buf.Bytes(), nil
}

// GetImageByHash returns the raw bytes of a content-addressed blob. Embedding
// workers need the bytes in memory anyway.
func (s *Service) GetImageByHash(ctx context.Context, hash string) (ImageResult, error) {
	data, contentType, err := s.fetchBlob(ctx, hash)
	if err != nil {
		return ImageResult{}, err
	}
	return ImageResult{Bytes: data, ContentType: contentType}, nil
}

// openImageByHash fetches a content-addressed blob from S3 into memory and
// wraps it in a seekable reader.
func (s *Service) openImageByHash(ctx context.Context, hash string) (*ImageStream, error) {
	data, contentType, err := s.fetchBlob(ctx, hash)
	if err != nil {
		return nil, err
	}
	return &ImageStream{
		Content:     bytes.NewReader(data),
		SizeBytes:   int64(len(data)),
		ContentType: contentType,
		Hash:        hash,
	}, nil
}

// fetchBlob downloads a blob from S3 and decides its content type: the type
// recorded in the catalog wins, and the type advertised by S3 is the fallback.
func (s *Service) fetchBlob(ctx context.Context, hash string) ([]byte, string, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, "", fmt.Errorf("blob hash is required")
	}

	blob, err := s.catalog.GetBlob(ctx, hash)
	if err != nil {
		if errors.Is(err, catalog.ErrBlobNotFound) {
			return nil, "", fmt.Errorf("%w: %s", ErrImageEntryNotFound, hash)
		}
		return nil, "", err
	}

	body, remoteContentType, err := s.store.GetObject(ctx, pipeline.BlobKey(hash))
	if err != nil {
		if storage.IsNotFound(err) {
			return nil, "", fmt.Errorf("%w: %s", ErrImageEntryNotFound, hash)
		}
		return nil, "", err
	}
	data, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil {
		return nil, "", fmt.Errorf("read blob %s: %w", hash, readErr)
	}
	if closeErr != nil {
		return nil, "", fmt.Errorf("close blob %s: %w", hash, closeErr)
	}

	contentType := contentTypeOrFallback(blob.ContentType)
	if contentType == "application/octet-stream" {
		contentType = contentTypeOrFallback(remoteContentType)
	}
	return data, contentType, nil
}

func contentTypeOrFallback(contentType string) string {
	trimmed := strings.TrimSpace(contentType)
	if trimmed == "" {
		return "application/octet-stream"
	}
	return trimmed
}
