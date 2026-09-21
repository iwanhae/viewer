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
	"viewer/internal/ingest"
	"viewer/internal/pipeline"
	"viewer/internal/recommend"
	"viewer/internal/storage"
	"viewer/internal/vision"
)

// embeddingLoadTimeout bounds resolving the checkpoint and compiling the
// inference graph at startup. The checkpoint may need to be downloaded first.
const embeddingLoadTimeout = 15 * time.Minute

func Run(ctx context.Context) error {
	cfg, err := cfgpkg.Load()
	if err != nil {
		return err
	}
	log.Printf(
		"viewer: config loaded on port=%d catalog=%s s3_prefix=%s",
		cfg.Port, cfg.DBPath(), cfg.DescribePrefix(),
	)

	cat, err := catalog.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open metadata catalog: %w", err)
	}
	defer cat.Close()

	store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		return err
	}

	imageService := images.NewService(cat, store)

	// The vision tower runs in-process against the checkpoint the Docker image
	// bakes in at config.ModelDir.
	recommendService := recommend.NewService(cat, imageService, recommend.NewVisionEmbedder(vision.Config{
		ModelID: cfgpkg.ModelDir,
	}))

	// Load the vision tower before anything can request an embedding: a failed
	// load marks the service disabled, and the background workers then stay off
	// instead of failing every pending blob.
	loadCtx, cancel := context.WithTimeout(context.Background(), embeddingLoadTimeout)
	loadErr := recommendService.LoadModel(loadCtx)
	cancel()
	if loadErr != nil {
		log.Printf("viewer: embedding model unavailable, continuing without embeddings: %v", loadErr)
	}
	// Enabled reports a failed load, so ask the service rather than assuming.
	recommenderEnabled := recommendService.Enabled()
	if recommenderEnabled {
		log.Printf("viewer: recommendation service enabled (model=%s)", cfgpkg.ModelDir)
	} else {
		log.Printf("viewer: embedding model unavailable; serving recommendations from stored embeddings only")
	}

	// Extraction and embedding are separate stages: the pipeline only makes an
	// album ready, and the recommendation service's background workers embed
	// the blobs it leaves pending.
	pipelineService := pipeline.NewService(cat, store, pipeline.Options{
		OnAlbumReady: func(albumID string) {
			if err := recommendService.ReloadAlbum(context.Background(), albumID); err != nil {
				log.Printf("viewer: reload recommendation index for album=%s failed: %v", albumID, err)
			}
		},
	})
	albumService := albums.NewService(cat, store, pipelineService)

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
	go func() {
		warmupRecommendations(ctx, recommendService)

		// The upload prefix is the drop zone for the zips that did not come
		// through POST /api/albums. It scans once here, after the index is
		// loaded, and then keeps watching, so a zip dropped while the server is
		// running needs no restart.
		go ingest.NewWatcher(store, albumService).Run(ctx)

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

// warmupRecommendations rebuilds the in-memory recommendation index from the
// catalog. It runs before the upload scan on purpose: the scan queues albums,
// and refreshing one album's slice of the index after an extraction costs less
// than reloading the whole thing afterwards.
func warmupRecommendations(ctx context.Context, recommendService *recommend.Service) {
	startedAt := time.Now()
	log.Printf("catalog warmup started")
	if err := recommendService.LoadAll(ctx); err != nil {
		log.Printf("recommendation index warmup failed: %v", err)
	}
	log.Printf("catalog warmup finished duration=%s", time.Since(startedAt).Round(time.Millisecond))
}
