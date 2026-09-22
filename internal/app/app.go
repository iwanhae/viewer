package app

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"viewer/internal/albums"
	"viewer/internal/backup"
	"viewer/internal/catalog"
	"viewer/internal/checkpoint"
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

// embeddingLoadTimeout bounds the background startup load: fetching the
// checkpoint and compiling the inference graph. The checkpoint may need to be
// downloaded first.
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
	if cfg.WorkerToken != "" {
		log.Printf("viewer: embedding worker API requires a bearer token")
	} else {
		log.Printf("viewer: embedding worker API is UNAUTHENTICATED (set EMBEDDING_WORKER_TOKEN to protect it)")
	}

	// The store comes up before the catalog: the bucket holds a snapshot of
	// the catalog, and a newer one replaces the local database file before
	// anything opens it.
	store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		return err
	}

	restored, err := backup.Restore(ctx, store, cfg.DBPath(), backup.StampPath(cfg.StateDir))
	if err != nil {
		return fmt.Errorf("restore catalog backup: %w", err)
	}
	if restored {
		log.Printf("viewer: serving the catalog restored from %s", backup.BackupObjectKey)
	}

	cat, err := catalog.Open(cfg.DBPath())
	if err != nil {
		return fmt.Errorf("open metadata catalog: %w", err)
	}
	defer cat.Close()

	imageService := images.NewService(cat, store)

	// The vision tower runs in-process against the checkpoint in
	// config.ModelDir. The image ships without one, so the background load
	// below fetches it from the mirror on a cold start; a deployment that
	// mounts a prepared directory there skips the download entirely.
	recommendService := recommend.NewService(cat, imageService, recommend.NewVisionEmbedder(vision.Config{
		ModelID: cfgpkg.ModelDir,
	}))

	// Extraction and embedding are separate stages: the pipeline only makes an
	// album ready, and the recommendation service's background workers embed
	// the blobs it leaves pending. When the extraction queue drains, the
	// finalizer backs the catalog up and deletes the staged zips it covers.
	finalizer := backup.NewFinalizer(store, cat, cfg.StateDir)
	pipelineService := pipeline.NewService(cat, store, pipeline.Options{
		OnAlbumReady: func(albumID string) {
			if err := recommendService.ReloadAlbum(context.Background(), albumID); err != nil {
				log.Printf("viewer: reload recommendation index for album=%s failed: %v", albumID, err)
			}
		},
		OnIdle: finalizer.Run,
	})
	albumService := albums.NewService(cat, store, pipelineService)

	feedService := feed.NewService(albumService)
	pipelineService.Start(ctx)

	h := httpapi.New(albumService, feedService, imageService, recommendService, cfg.WorkerToken).Router()
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("viewer: starting HTTP server on %s", srv.Addr)

	// The model resolves in the background so a cold start serves requests
	// while the checkpoint is still downloading: fetch it, load the vision
	// tower, and only then start the embedding workers. Until the load
	// finishes the API answers from stored embeddings and new blobs stay
	// pending - the same degradation as a missing checkpoint. The workers must
	// not start earlier: an embed attempt against an incomplete directory
	// caches the load failure permanently.
	go func() {
		loadCtx, cancel := context.WithTimeout(context.Background(), embeddingLoadTimeout)
		defer cancel()
		if err := checkpoint.Ensure(loadCtx, cfgpkg.ModelDir, cfg.ModelURL); err != nil {
			// vision.Load then fails on the incomplete directory and marks the
			// service disabled through the same path as any other missing model.
			log.Printf("viewer: checkpoint fetch from %s failed: %v", cfg.ModelURL, err)
		}
		if err := recommendService.LoadModel(loadCtx); err != nil {
			log.Printf("viewer: embedding model unavailable, continuing without embeddings: %v", err)
		}
		if !recommendService.Enabled() {
			log.Printf("viewer: embedding model unavailable; serving recommendations from stored embeddings only")
			return
		}
		log.Printf("viewer: recommendation service enabled (model=%s)", cfgpkg.ModelDir)
		if err := recommendService.Start(ctx); err != nil {
			log.Printf("viewer: embedding worker startup failed: %v", err)
			return
		}
		log.Printf("viewer: embedding background workers started")
	}()

	log.Printf("viewer: startup warmup running in background")
	go func() {
		warmupRecommendations(ctx, recommendService)

		// A previous run can have crashed between the catalog backup and the
		// zip deletes. Finalizing once here refreshes the backup and clears
		// those leftovers; a scan that ran first could only skip them.
		if err := finalizer.Run(ctx); err != nil {
			log.Printf("viewer: startup catalog backup failed: %v", err)
		}

		// The upload prefix is the drop zone for the zips that did not come
		// through POST /api/albums. It scans once here, after the index is
		// loaded, and then keeps watching, so a zip dropped while the server is
		// running needs no restart.
		go ingest.NewWatcher(store, albumService).Run(ctx)

		log.Printf("viewer: warmup completed")
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
