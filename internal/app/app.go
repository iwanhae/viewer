package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"viewer/internal/albums"
	cfgpkg "viewer/internal/config"
	"viewer/internal/feed"
	"viewer/internal/httpapi"
	"viewer/internal/images"
	"viewer/internal/storage"
)

func Run(ctx context.Context) error {
	cfg, err := cfgpkg.Load()
	if err != nil {
		return err
	}
	log.Printf("viewer: config loaded on port=%d cache_dir=%s zip_cache_dir=%s", cfg.Port, cfg.CacheDir, cfg.ZipCacheDir)

	store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		return err
	}

	albumService := albums.NewService(cfg, store, albums.NewIndexer())
	httpapi.Warmup(ctx, albumService)

	feedService := feed.NewService(albumService)
	imageService, err := images.NewService(albumService, store, cfg.CacheDir, cfg.ZipCacheDir)
	if err != nil {
		return err
	}

	h := httpapi.New(albumService, feedService, imageService, cfg.MaxUploadBytes).Router()
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("viewer: starting HTTP server on %s", srv.Addr)

	return srv.ListenAndServe()
}
