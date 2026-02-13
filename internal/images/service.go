package images

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"viewer/internal/albums"
	"viewer/internal/cache"
	"viewer/internal/storage"
)

type Service struct {
	albums   *albums.Service
	store    *storage.S3Store
	cache    *cache.DiskCache
	zipCache string
}

type ImageResult struct {
	Bytes       []byte
	ContentType string
}

func NewService(albumsService *albums.Service, store *storage.S3Store, cacheDir string, zipCacheDir string) (*Service, error) {
	dc, err := cache.NewDiskCache(cacheDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(zipCacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create zip cache dir: %w", err)
	}

	return &Service{
		albums:   albumsService,
		store:    store,
		cache:    dc,
		zipCache: zipCacheDir,
	}, nil
}

func sourceKey(albumID string) string {
	return fmt.Sprintf("albums/%s/source.zip", albumID)
}

func (s *Service) GetImage(ctx context.Context, albumID string, idx int, mode string, wallWidth int) (ImageResult, error) {
	album, err := s.albums.GetAlbum(ctx, albumID)
	if err != nil {
		return ImageResult{}, err
	}
	if idx < 0 || idx >= len(album.Photos) {
		return ImageResult{}, fmt.Errorf("photo index out of range")
	}

	photo := album.Photos[idx]
	data, contentType, err := s.readEntryBytes(ctx, albumID, photo.Name)
	if err != nil {
		return ImageResult{}, err
	}

	if mode != "wall" {
		return ImageResult{Bytes: data, ContentType: contentType}, nil
	}

	if wallWidth <= 0 {
		wallWidth = 480
	}
	if wallWidth < 64 {
		wallWidth = 64
	}
	if wallWidth > 2048 {
		wallWidth = 2048
	}

	cacheKey := strings.Join([]string{albumID, strconv.Itoa(idx), "wall", strconv.Itoa(wallWidth)}, ":")
	if cached, ok := s.cache.Get(cacheKey); ok {
		return ImageResult{Bytes: cached, ContentType: "image/jpeg"}, nil
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return ImageResult{}, fmt.Errorf("decode image: %w", err)
	}

	b := img.Bounds()
	origW := b.Dx()
	origH := b.Dy()
	if origW <= 0 || origH <= 0 {
		return ImageResult{}, fmt.Errorf("invalid image dimensions")
	}

	targetH := int(float64(origH) * float64(wallWidth) / float64(origW))
	if targetH < 1 {
		targetH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, wallWidth, targetH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)

	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 82}); err != nil {
		return ImageResult{}, fmt.Errorf("encode jpeg: %w", err)
	}
	encoded := out.Bytes()
	_ = s.cache.Set(cacheKey, encoded)

	return ImageResult{Bytes: encoded, ContentType: "image/jpeg"}, nil
}

func (s *Service) localZipPath(albumID string) string {
	return filepath.Join(s.zipCache, albumID+".zip")
}

func (s *Service) ensureZipCached(ctx context.Context, albumID string) (string, error) {
	zipPath := s.localZipPath(albumID)
	if _, err := os.Stat(zipPath); err == nil {
		return zipPath, nil
	}

	body, _, err := s.store.GetObject(ctx, sourceKey(albumID))
	if err != nil {
		return "", err
	}
	defer body.Close()

	tmp, err := os.CreateTemp(s.zipCache, albumID+"-*.zip")
	if err != nil {
		return "", fmt.Errorf("create temp zip: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("cache zip: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	if err := os.Rename(tmpPath, zipPath); err != nil {
		if _, stErr := os.Stat(zipPath); stErr == nil {
			_ = os.Remove(tmpPath)
			return zipPath, nil
		}
		return "", fmt.Errorf("move cached zip: %w", err)
	}
	return zipPath, nil
}

func (s *Service) readEntryBytes(ctx context.Context, albumID string, entryName string) ([]byte, string, error) {
	zipPath, err := s.ensureZipCached(ctx, albumID)
	if err != nil {
		return nil, "", err
	}

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, "", fmt.Errorf("open zip cache: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		if f.Name != entryName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, "", fmt.Errorf("open zip entry: %w", err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, "", fmt.Errorf("read zip entry: %w", err)
		}
		ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(f.Name)))
		if ct == "" {
			ct = "application/octet-stream"
		}
		return data, ct, nil
	}

	return nil, "", fmt.Errorf("image entry not found")
}
