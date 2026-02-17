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
	"viewer/internal/recommend"
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
	feedService := feed.NewService(albumService)
	imageService, err := images.NewService(albumService, store, cfg.CacheDir, cfg.ZipCacheDir, cfg.RangeChunkSize)
	if err != nil {
		return err
	}

	recommendService, err := recommend.NewService(cfg, albumService, imageService, store)
	if err != nil {
		return fmt.Errorf("recommendation service init failed: %w", err)
	}
	if err := recommendService.Start(ctx); err != nil {
		return fmt.Errorf("recommendation startup failed: %w", err)
	}
	log.Printf("viewer: recommendation service enabled")

	h := httpapi.New(albumService, feedService, imageService, recommendService, cfg.MaxUploadBytes).Router()
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("viewer: starting HTTP server on %s", srv.Addr)
	log.Printf("viewer: startup warmup running in background")
	go httpapi.Warmup(ctx, albumService, recommendService)

	return srv.ListenAndServe()
}
