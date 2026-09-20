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

// embeddingLoadTimeout bounds resolving the checkpoint and compiling the
// inference graph at startup. The checkpoint may need to be downloaded first.
const embeddingLoadTimeout = 15 * time.Minute

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

	// Load the vision tower before wiring the ingest pipeline: a failed load
	// must keep the embedder out of the pipeline entirely, otherwise every
	// image would be marked as a failed embedding instead of staying pending.
	recommenderEnabled := recommendService.Enabled()
	if recommenderEnabled {
		loadCtx, cancel := context.WithTimeout(context.Background(), embeddingLoadTimeout)
		loadErr := recommendService.LoadModel(loadCtx)
		cancel()
		if loadErr != nil {
			if cfg.EmbeddingRequired {
				return fmt.Errorf("recommendation service init failed: %w", loadErr)
			}
			log.Printf("viewer: embedding model unavailable, continuing without embeddings: %v", loadErr)
		}
		// Enabled reflects a failed load, so re-read it rather than trusting cfg.
		recommenderEnabled = recommendService.Enabled()
	}
	if recommenderEnabled {
		log.Printf("viewer: recommendation service enabled (model=%s backend=%s)",
			cfg.EmbeddingModelID, cfg.EmbeddingBackend)
	} else {
		log.Printf("viewer: embeddings disabled (EMBEDDING_ENABLED=%t)", cfg.EmbeddingEnabled)
	}

	var pipelineEmbedder pipeline.Embedder
	if recommenderEnabled {
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
