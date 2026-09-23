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
	"viewer/internal/qdrant"
	"viewer/internal/recommend"
	"viewer/internal/storage"
	"viewer/internal/vision"
)

// embeddingLoadTimeout bounds the background startup load: fetching the
// checkpoint and compiling the inference graph. The checkpoint may need to be
// downloaded first.
const embeddingLoadTimeout = 15 * time.Minute

// backupInterval bounds how long catalog writes that no queue drain covers —
// embedding results landing hours after their albums, external embedding
// worker results — can go without a backup. The finalizer itself skips the
// upload when the catalog is unchanged since the last backup, so a quiet hour
// costs only one local snapshot attempt, not an upload.
const backupInterval = time.Hour

func Run(ctx context.Context) error {
	cfg, err := cfgpkg.Load()
	if err != nil {
		return err
	}
	log.Printf(
		"viewer: config loaded on port=%d catalog=%s s3_prefix=%s qdrant=%s",
		cfg.Port, cfg.DBPath(), cfg.DescribePrefix(), cfg.QdrantURL,
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

	// Vectors live in the external Qdrant collection, not in SQLite (since
	// migration 0003 the catalog stores only their status). The client is the
	// upload target the migration needs while it moves any still-stored vectors
	// over, so it must exist before the catalog opens.
	vectorStore := qdrant.New(cfg.QdrantURL, cfg.QdrantAPIKey, qdrant.DefaultCollection, catalog.EmbeddingDim)
	cat, err := catalog.Open(cfg.DBPath(), vectorStore)
	if err != nil {
		return fmt.Errorf("open metadata catalog: %w", err)
	}
	defer cat.Close()

	// The migration only guarantees the collection exists when it had vectors
	// to move. Ensure it on every boot so a fresh or already-migrated catalog
	// is searchable too; a mismatched collection config fails the start here
	// rather than surfacing as errors on the first search.
	if err := vectorStore.EnsureCollection(ctx); err != nil {
		return fmt.Errorf("ensure vector collection: %w", err)
	}

	// The store and the catalog are written by different paths (worker
	// write-backs, migrations, album reloads), so they can drift apart — most
	// commonly after a catalog backup was restored over a store that kept its
	// old points, or the other way around. Nothing here repairs that
	// automatically; the mismatch is logged so the operator can re-embed or
	// wipe whichever side is stale.
	if readyPairs, err := cat.ReadyPairCount(ctx); err != nil {
		log.Printf("viewer: counting ready embeddings failed: %v", err)
	} else if points, err := vectorStore.CountVectors(ctx); err != nil {
		log.Printf("viewer: counting vector store points failed: %v", err)
	} else if points != int(readyPairs) {
		log.Printf("viewer: WARNING vector store holds %d point(s) but the catalog has %d ready photo pair(s); recommendations may be incomplete until the two are re-synced", points, readyPairs)
	}

	imageService := images.NewService(cat, store)

	// The vision tower runs in-process against the checkpoint in
	// config.ModelDir. The image ships without one, so the background load
	// below fetches it from the mirror on a cold start; a deployment that
	// mounts a prepared directory there skips the download entirely.
	// Extraction and embedding are separate stages: the pipeline only makes an
	// album ready, and the recommendation service's background workers embed
	// the blobs it leaves pending. The finalizer backs the catalog up and
	// deletes the staged zips a backup covers. It runs on the extraction drain
	// below, on the embedding drain, hourly, and once at boot — the boot sweep
	// also cleans up a run that crashed between its backup and its deletes.
	finalizer := backup.NewFinalizer(store, cat, cfg.StateDir, cfg.AllowBackupOverwrite)
	recommendService := recommend.NewService(cat, vectorStore, imageService, recommend.NewVisionEmbedder(vision.Config{
		ModelID: cfgpkg.ModelDir,
	}), finalizer.Run)
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

	// The hourly backup fires no matter how quiet the queues are. It is the
	// safety net for catalog writes nothing drains behind; the finalizer's
	// unchanged-skip turns a quiet hour into a no-op.
	go func() {
		ticker := time.NewTicker(backupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := finalizer.Run(ctx); err != nil {
					log.Printf("viewer: scheduled catalog backup failed: %v", err)
				}
			}
		}
	}()

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
		recommendService.Start(ctx)
		log.Printf("viewer: embedding background workers started")
	}()

	log.Printf("viewer: startup tasks running in background")
	go func() {
		// A previous run can have crashed between the catalog backup and the
		// zip deletes. Finalizing once here refreshes the backup and clears
		// those leftovers; a scan that ran first could only skip them.
		if err := finalizer.Run(ctx); err != nil {
			log.Printf("viewer: startup catalog backup failed: %v", err)
		}

		// The upload prefix is the drop zone for the zips that did not come
		// through POST /api/albums. It scans once here and then keeps watching,
		// so a zip dropped while the server is running needs no restart.
		go ingest.NewWatcher(store, albumService).Run(ctx)

		log.Printf("viewer: startup tasks completed")
	}()

	return srv.ListenAndServe()
}
