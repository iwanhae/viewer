package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	_ "golang.org/x/image/webp" // register the WebP decoder for image.Decode
	_ "image/gif"               // register the GIF decoder for image.Decode
	_ "image/png"               // register the PNG decoder for image.Decode

	"golang.org/x/image/draw"

	"viewer/internal/catalog"
	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

// blobStore is the S3 surface needed to read image blobs and to hand their
// download URLs to external workers.
type blobStore interface {
	GetObject(ctx context.Context, key string) (io.ReadCloser, string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// Service resolves original-upload hashes to the current image bytes stored
// in S3 under "blobs/<hash>".
type Service struct {
	catalog *catalog.Store
	store   blobStore
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

// widthLadder lists the scaled widths the image endpoint accepts. The ladder
// is deliberately short: every entry is a compile-time choice, not config.
var widthLadder = []int{320, 640, 1024}

// WidthLadder lists the scaled widths the image endpoint accepts.
func WidthLadder() []int {
	return widthLadder
}

// IsSupportedWidth reports whether w is on the width ladder.
func IsSupportedWidth(w int) bool {
	return slices.Contains(widthLadder, w)
}

// newStream wraps fully-read blob bytes in a seekable stream. The reader stays
// a *bytes.Reader so http.ServeContent can answer range requests.
func newStream(data []byte, contentType string) *ImageStream {
	digest := sha256.Sum256(data)
	return &ImageStream{
		Content:     bytes.NewReader(data),
		SizeBytes:   int64(len(data)),
		ContentType: contentType,
		Hash:        hex.EncodeToString(digest[:]),
	}
}

// OpenImageByHash fetches a blob from S3 into memory and
// wraps it in a seekable reader.
func (s *Service) OpenImageByHash(ctx context.Context, hash string) (*ImageStream, error) {
	data, contentType, err := s.fetchBlob(ctx, hash)
	if err != nil {
		return nil, err
	}
	return newStream(data, contentType), nil
}

// OpenImageByHashScaled returns a blob scaled to fit within width, preserving
// aspect ratio. Images already no wider than the request are served with their
// original bytes; anything larger becomes a JPEG so the scaled response stays
// small. Each returned byte sequence gets its own digest validator.
func (s *Service) OpenImageByHashScaled(ctx context.Context, hash string, width int) (*ImageStream, error) {
	if !IsSupportedWidth(width) {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedWidth, width)
	}

	data, contentType, err := s.fetchBlob(ctx, hash)
	if err != nil {
		return nil, err
	}
	// Up-scaling adds nothing, so a small original passes through untouched
	// with its own content type and hash.
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("probe image %s: %w", hash, err)
	}
	if config.Width <= width {
		return newStream(data, contentType), nil
	}
	encoded, err := encodeScaledJPEG(data, width)
	if err != nil {
		return nil, err
	}
	return newStream(encoded, "image/jpeg"), nil
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

// GetImageBytes returns the raw bytes of a content-addressed blob. Embedding
// workers need the bytes in memory anyway.
func (s *Service) GetImageBytes(ctx context.Context, hash string) ([]byte, error) {
	data, _, err := s.fetchBlob(ctx, hash)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// PresignBlobURL returns a short-lived URL that downloads the blob straight
// from the bucket, so external workers can fetch image bytes without this
// process proxying them.
func (s *Service) PresignBlobURL(ctx context.Context, hash string, ttl time.Duration) (string, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return "", fmt.Errorf("blob hash is required")
	}
	url, err := s.store.PresignGet(ctx, pipeline.BlobKey(hash), ttl)
	if err != nil {
		return "", fmt.Errorf("presign blob %s: %w", hash, err)
	}
	return url, nil
}

// fetchBlob downloads a blob from S3 and decides its content type. S3 is the
// source of truth for existence and, because extraction always uploads with a
// real content type, usually for the type too; the catalog is consulted only
// when S3 advertises nothing useful.
func (s *Service) fetchBlob(ctx context.Context, hash string) ([]byte, string, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, "", fmt.Errorf("blob hash is required")
	}

	body, contentType, err := s.store.GetObject(ctx, pipeline.BlobKey(hash))
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
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return data, "image/webp", nil
	}
	if detected := http.DetectContentType(data); detected == "image/jpeg" || detected == "image/png" {
		return data, detected, nil
	}

	contentType = contentTypeOrFallback(contentType)
	if contentType == "application/octet-stream" {
		// Missing S3 metadata: recover the recorded type, but a blob the
		// catalog does not know is still servable — the bytes exist.
		if blob, err := s.catalog.GetBlob(ctx, hash); err == nil {
			contentType = contentTypeOrFallback(blob.ContentType)
		}
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
