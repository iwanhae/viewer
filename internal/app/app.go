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
	"viewer/internal/models"
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
	imageService, err := images.NewService(albumService, store, cfg.CacheDir, cfg.ZipCacheDir)
	if err != nil {
		return err
	}

	recommendService, err := recommend.NewService(cfg, imageService, store)
	if err != nil {
		return fmt.Errorf("recommendation service init failed: %w", err)
	}
	albumService.SetFinalizeSuccessHook(func(idx models.AlbumIndex) {
		if recommendService != nil {
			recommendService.IngestAlbumIndex(idx)
		}
	})
	albumService.StartFinalizeWorker(ctx)

	recommenderEnabled := recommendService.Enabled()
	if recommenderEnabled {
		if cfg.RecommenderRequired {
			healthcheckCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := recommendService.Healthcheck(healthcheckCtx); err != nil {
				cancel()
				return fmt.Errorf("recommendation service init failed: %w", err)
			}
			cancel()
		} else {
			healthcheckCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			if err := recommendService.Healthcheck(healthcheckCtx); err != nil {
				log.Printf("viewer: recommender unavailable at startup, continuing in degraded mode: %v", err)
			}
			cancel()
		}
		log.Printf("viewer: recommendation service enabled")
	} else {
		log.Printf("viewer: recommender disabled (RECOMMENDER_ENDPOINT is empty)")
	}

	h := httpapi.New(albumService, feedService, imageService, recommendService).Router()
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("viewer: starting HTTP server on %s", srv.Addr)
	log.Printf("viewer: startup warmup running in background")
	warmupDone := make(chan struct{})
	go func() {
		httpapi.Warmup(ctx, albumService, recommendService)
		close(warmupDone)
	}()
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-warmupDone:
		}
		if !recommenderEnabled {
			log.Printf("viewer: warmup completed; embedding workers disabled")
			return
		}
		log.Printf("viewer: warmup completed, starting embedding workers")
		if err := recommendService.Start(ctx); err != nil {
			log.Printf("viewer: embedding worker startup failed: %v", err)
			return
		}
		log.Printf("viewer: embedding background workers started")
	}()

	return srv.ListenAndServe()
}
