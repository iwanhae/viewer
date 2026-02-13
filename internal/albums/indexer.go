package albums

import (
	"archive/zip"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
	"viewer/internal/models"
)

var ErrNoValidImages = errors.New("no valid images in zip")

var allowedExtensions = map[string]struct{}{
	".jpg":  {},
	".jpeg": {},
	".png":  {},
	".webp": {},
}

type Indexer struct{}

func NewIndexer() *Indexer {
	return &Indexer{}
}

func (i *Indexer) BuildFromZip(zipPath string, albumID string, originalFilename string) (*models.AlbumIndex, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer r.Close()

	entries := make([]*zip.File, 0, len(r.File))
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		if _, ok := allowedExtensions[ext]; !ok {
			continue
		}
		entries = append(entries, f)
	}

	sort.Slice(entries, func(a, b int) bool {
		return strings.ToLower(entries[a].Name) < strings.ToLower(entries[b].Name)
	})

	photos := make([]models.PhotoMeta, 0, len(entries))
	for _, f := range entries {
		rc, err := f.Open()
		if err != nil {
			continue
		}
		cfg, _, err := image.DecodeConfig(rc)
		rc.Close()
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
			continue
		}
		idx := len(photos)
		photos = append(photos, models.PhotoMeta{
			I:     idx,
			Name:  f.Name,
			W:     cfg.Width,
			H:     cfg.Height,
			Ratio: float64(cfg.Width) / float64(cfg.Height),
		})
	}

	if len(photos) == 0 {
		return nil, ErrNoValidImages
	}

	return &models.AlbumIndex{
		AlbumID:          albumID,
		OriginalFilename: originalFilename,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		PhotoCount:       len(photos),
		Photos:           photos,
	}, nil
}
