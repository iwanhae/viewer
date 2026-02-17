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
	var recommendService *recommend.Service
	if cfg.RecoEnabled {
		rec, recErr := recommend.NewService(cfg, albumService, imageService, store)
		if recErr != nil {
			log.Printf("viewer: recommendation service disabled due to startup error: %v", recErr)
		} else {
			recommendService = rec
			recommendService.Start(ctx)
			log.Printf("viewer: recommendation service enabled")
		}
	} else {
		log.Printf("viewer: recommendation service disabled by config")
	}

	h := httpapi.New(albumService, feedService, imageService, recommendService, cfg.MaxUploadBytes).Router()
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("viewer: starting HTTP server on %s", srv.Addr)
	if cfg.SkipWarmup {
		log.Printf("viewer: startup warmup skipped")
	} else {
		log.Printf("viewer: startup warmup running in background")
		go httpapi.Warmup(ctx, albumService)
	}

	return srv.ListenAndServe()
}
