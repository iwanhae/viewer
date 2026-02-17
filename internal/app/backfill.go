package app

import (
	"context"
	"log"
	"os"
	"time"

	"viewer/internal/albums"
	cfgpkg "viewer/internal/config"
	"viewer/internal/images"
	"viewer/internal/recommend"
	"viewer/internal/storage"
)

func RunRecoBackfill(ctx context.Context) error {
	cfg, err := cfgpkg.Load()
	if err != nil {
		return err
	}

	store, err := storage.NewS3Store(ctx, cfg)
	if err != nil {
		return err
	}

	albumService := albums.NewService(cfg, store, albums.NewIndexer())
	imageService, err := images.NewService(albumService, store, cfg.CacheDir, cfg.ZipCacheDir, cfg.RangeChunkSize)
	if err != nil {
		return err
	}
	recoService, err := recommend.NewService(cfg, albumService, imageService, store)
	if err != nil {
		return err
	}

	order := recommend.ParseBackfillOrder(os.Getenv("RECO_BACKFILL_ORDER"))
	log.Printf("viewer: recommendation backfill seed started order=%s", order)
	seed, err := recoService.SeedAllAlbums(ctx, order)
	if err != nil {
		return err
	}
	log.Printf(
		"viewer: recommendation backfill seed complete discovered=%d synced=%d failed=%d enqueued=%d",
		seed.AlbumsDiscovered,
		seed.AlbumsSynced,
		seed.AlbumsFailed,
		seed.PhotosEnqueued,
	)

	drainOpts := recommend.BackfillDrainOptions{
		WorkerCount: cfg.RecoWorkerConcurrency,
		PollInterval: 2 * time.Second,
		StableRounds: 3,
		LogEvery:     10 * time.Second,
	}
	log.Printf("viewer: recommendation backfill drain started workers=%d", drainOpts.WorkerCount)
	drain, err := recoService.RunWorkersUntilDrained(ctx, drainOpts)
	if err != nil {
		return err
	}
	log.Printf(
		"viewer: recommendation backfill drain complete photos=%d embeddings=%d pending=%d running=%d failed=%d processed=%d worker_failures=%d",
		drain.PhotosTotal,
		drain.EmbeddingsTotal,
		drain.Queue.Pending,
		drain.Queue.Running,
		drain.Queue.Failed,
		drain.Processed,
		drain.WorkerFailures,
	)
	return nil
}
