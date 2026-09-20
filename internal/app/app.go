package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"viewer/internal/albums"
	"viewer/internal/catalog"
	cfgpkg "viewer/internal/config"
	"viewer/internal/feed"
	"viewer/internal/httpapi"
	"viewer/internal/images"
	"viewer/internal/pipeline"
	"viewer/internal/recommend"
	"viewer/internal/storage"
)

func Run(ctx context.Context) error {
	cfg, err := cfgpkg.Load()
	if err != nil {
		return err
	}
	log.Printf("viewer: config loaded on port=%d cache_dir=%s zip_cache_dir=%s db_path=%s", cfg.Port, cfg.CacheDir, cfg.ZipCacheDir, cfg.DBPath)

	cat, err := catalog.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open metadata catalog: %w", err)
	}
	defer cat.Close()

	store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		return err
	}

	imageService, err := images.NewService(cat, store, cfg.CacheDir)
	if err != nil {
		return err
	}

	recommendService, err := recommend.NewService(cfg, cat, imageService)
	if err != nil {
		return fmt.Errorf("recommendation service init failed: %w", err)
	}

	albumService := albums.NewService(cfg, cat, store)

	var pipelineEmbedder pipeline.Embedder
	if recommendService.Enabled() {
		pipelineEmbedder = recommendService
	}
	pipelineService := pipeline.NewService(cat, store, pipelineEmbedder, pipeline.Options{
		DeleteSource: cfg.IngestDeleteSource,
		TempDir:      cfg.ZipCacheDir,
		OnAlbumReady: func(albumID string) {
			if err := recommendService.ReloadAlbum(context.Background(), albumID); err != nil {
				log.Printf("viewer: reload recommendation index for album=%s failed: %v", albumID, err)
			}
		},
	})
	albumService.SetEnqueuer(pipelineService)

	feedService := feed.NewService(albumService)
	pipelineService.Start(ctx)

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
		httpapi.Warmup(ctx, albumService, recommendService, store, cfg)
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
