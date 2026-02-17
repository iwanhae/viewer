package recommend

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type backfillAlbumCandidate struct {
	AlbumID   string
	CreatedAt time.Time
}

func (s *Service) SeedAllAlbums(ctx context.Context, order BackfillOrder) (BackfillSeedResult, error) {
	started := time.Now().UTC()
	result := BackfillSeedResult{
		StartedAt: started.Format(time.RFC3339),
	}

	seen := make(map[string]struct{}, 4096)
	candidates := make([]backfillAlbumCandidate, 0, 4096)
	if err := s.s3.ForEachAlbumIndexKey(ctx, func(key string) error {
		albumID := albumIDFromIndexKey(key)
		if albumID == "" {
			return nil
		}
		if _, ok := seen[albumID]; ok {
			return nil
		}
		seen[albumID] = struct{}{}

		idx, err := s.albums.GetAlbum(ctx, albumID)
		if err != nil {
			result.AlbumsFailed++
			log.Printf("recommend: backfill seed failed album=%s err=%v", albumID, err)
			return nil
		}
		candidates = append(candidates, backfillAlbumCandidate{
			AlbumID:   albumID,
			CreatedAt: parseBackfillTimestamp(idx.CreatedAt),
		})
		return nil
	}); err != nil {
		return result, err
	}

	result.AlbumsDiscovered = len(candidates)
	normalizedOrder := order
	if normalizedOrder == "" {
		normalizedOrder = BackfillOrderOldestFirst
	}
	slices.SortFunc(candidates, func(a, b backfillAlbumCandidate) int {
		if normalizedOrder == BackfillOrderNewestFirst {
			if diff := cmp.Compare(b.CreatedAt.UnixNano(), a.CreatedAt.UnixNano()); diff != 0 {
				return diff
			}
			return cmp.Compare(a.AlbumID, b.AlbumID)
		}
		if diff := cmp.Compare(a.CreatedAt.UnixNano(), b.CreatedAt.UnixNano()); diff != 0 {
			return diff
		}
		return cmp.Compare(a.AlbumID, b.AlbumID)
	})

	for idx, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		enqueued, err := s.syncAlbum(ctx, candidate.AlbumID)
		if err != nil {
			result.AlbumsFailed++
			log.Printf("recommend: backfill sync failed album=%s err=%v", candidate.AlbumID, err)
			continue
		}
		result.AlbumsSynced++
		result.PhotosEnqueued += enqueued
		if (idx+1)%100 == 0 {
			log.Printf("recommend: backfill seed progress synced=%d/%d failed=%d enqueued=%d", result.AlbumsSynced, result.AlbumsDiscovered, result.AlbumsFailed, result.PhotosEnqueued)
		}
	}

	finished := time.Now().UTC()
	result.FinishedAt = finished.Format(time.RFC3339)
	_ = s.store.SetMeta("backfill.seed.last_started_at", result.StartedAt)
	_ = s.store.SetMeta("backfill.seed.last_finished_at", result.FinishedAt)
	_ = s.store.SetMeta("backfill.seed.last_order", string(normalizedOrder))
	_ = s.store.SetMeta("backfill.seed.last_albums_discovered", strconv.Itoa(result.AlbumsDiscovered))
	_ = s.store.SetMeta("backfill.seed.last_albums_synced", strconv.Itoa(result.AlbumsSynced))
	_ = s.store.SetMeta("backfill.seed.last_albums_failed", strconv.Itoa(result.AlbumsFailed))
	_ = s.store.SetMeta("backfill.seed.last_photos_enqueued", strconv.Itoa(result.PhotosEnqueued))
	return result, nil
}

func (s *Service) RunWorkersUntilDrained(ctx context.Context, opts BackfillDrainOptions) (BackfillDrainResult, error) {
	started := time.Now().UTC()
	result := BackfillDrainResult{
		StartedAt: started.Format(time.RFC3339),
	}

	workerCount := opts.WorkerCount
	if workerCount <= 0 {
		workerCount = s.cfg.RecoWorkerConcurrency
	}
	if workerCount <= 0 {
		workerCount = 1
	}

	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}

	stableRounds := opts.StableRounds
	if stableRounds <= 0 {
		stableRounds = 3
	}

	logEvery := opts.LogEvery
	if logEvery <= 0 {
		logEvery = 10 * time.Second
	}

	maxRetries := s.cfg.RecoMaxRetries
	if maxRetries <= 0 {
		maxRetries = 5
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var processed atomic.Int64
	var workerFailures atomic.Int64

	for idx := 0; idx < workerCount; idx++ {
		workerID := fmt.Sprintf("backfill-worker-%d", idx+1)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			idleTicker := time.NewTicker(pollInterval)
			defer idleTicker.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				jobs := s.store.ClaimJobs(id, 1, time.Now().UTC())
				if len(jobs) == 0 {
					select {
					case <-runCtx.Done():
						return
					case <-idleTicker.C:
					}
					continue
				}

				job := jobs[0]
				if err := s.processJob(runCtx, job); err != nil {
					workerFailures.Add(1)
					_ = s.store.MarkFailed(job.ImageID, err.Error(), maxRetries)
					continue
				}
				processed.Add(1)
			}
		}(workerID)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	lastLogAt := time.Now()
	emptyRounds := 0

	for {
		select {
		case <-ctx.Done():
			cancel()
			wg.Wait()
			return result, ctx.Err()
		case <-ticker.C:
			progress := s.store.BackfillProgress()
			result.Processed = int(processed.Load())
			result.WorkerFailures = int(workerFailures.Load())
			result.Queue = progress.Queue
			result.PhotosTotal = progress.PhotosTotal
			result.EmbeddingsTotal = progress.EmbeddingsTotal

			if time.Since(lastLogAt) >= logEvery {
				log.Printf(
					"recommend: backfill drain progress photos=%d embeddings=%d pending=%d running=%d failed=%d processed=%d worker_failures=%d",
					result.PhotosTotal,
					result.EmbeddingsTotal,
					result.Queue.Pending,
					result.Queue.Running,
					result.Queue.Failed,
					result.Processed,
					result.WorkerFailures,
				)
				lastLogAt = time.Now()
			}

			if progress.Queue.Pending == 0 && progress.Queue.Running == 0 {
				emptyRounds++
				if emptyRounds >= stableRounds {
					cancel()
					wg.Wait()
					finalProgress := s.store.BackfillProgress()
					result.Queue = finalProgress.Queue
					result.PhotosTotal = finalProgress.PhotosTotal
					result.EmbeddingsTotal = finalProgress.EmbeddingsTotal
					result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
					_ = s.store.SetMeta("backfill.drain.last_finished_at", result.FinishedAt)
					_ = s.store.SetMeta("backfill.drain.last_processed", strconv.Itoa(result.Processed))
					_ = s.store.SetMeta("backfill.drain.last_worker_failures", strconv.Itoa(result.WorkerFailures))
					_ = s.store.SetMeta("backfill.drain.last_embeddings_total", strconv.Itoa(result.EmbeddingsTotal))
					return result, nil
				}
			} else {
				emptyRounds = 0
			}
		}
	}
}

func (s *Service) BackfillProgress() BackfillProgress {
	return s.store.BackfillProgress()
}

func parseBackfillTimestamp(raw string) time.Time {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return ts.UTC()
	}
	if ts, err := time.Parse(time.RFC3339, value); err == nil {
		return ts.UTC()
	}
	return time.Time{}
}
