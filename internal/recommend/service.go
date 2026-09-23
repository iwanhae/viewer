package recommend

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/images"
)

const (
	// embeddingConcurrency is one worker by design: the GoMLX backend already
	// splits a single forward pass across every core, so extra workers only
	// queue behind it. Measured per-image latency was flat from one to four
	// concurrent images.
	embeddingConcurrency = 1

	// embeddingTimeout bounds one image's preprocessing plus inference.
	embeddingTimeout = 5 * time.Minute

	// embeddingProgressEvery is how many images the background worker embeds
	// between two progress lines. Every image logs its own completion; this
	// only controls the extra "how far along are we" line, which costs one
	// catalog count, so it stays coarse.
	embeddingProgressEvery = 25

	// defaultTopK and maxTopK bound recommendation result sizes.
	defaultTopK = 12
	maxTopK     = 48
)

// Service maps photos to the blob hashes they appear under and runs the
// background workers that compute embeddings for the catalog's pending blobs.
// The similarity index itself lives in the catalog's blob_embeddings vec0
// table: embeddings are queried through SQL and never held here. Extraction
// never embeds anything itself: the in-process workers and external workers
// leasing batches through ClaimEmbeddings are the only producers of
// embeddings, and both write through ApplyEmbeddingResults.
type Service struct {
	catalog *catalog.Store
	images  *images.Service

	// embedder is nil when the recommendation feature is switched off, which is
	// how tests run without a checkpoint.
	embedder EmbeddingProvider

	// onEmbeddingDrain, when set, runs after the background embedding queue
	// drains. The catalog backup uses it to capture embedding writes that
	// landed after the last extraction-drain backup — without it, a quiet
	// deployment could go days between backups while embeddings still land.
	onEmbeddingDrain func(ctx context.Context) error

	startOnce sync.Once

	// modelMu guards model loading and modelErr. A failed load marks the
	// embedder unusable so the ingest pipeline keeps blobs pending instead of
	// marking them failed.
	modelMu  sync.Mutex
	modelErr error

	// runMu guards the live state behind Progress: how many forward passes are
	// in flight, and how the current background drain is going. The catalog
	// stays the source of truth for the counts; these fields describe only what
	// is happening right now.
	runMu        sync.Mutex
	activeEmbeds int
	runActive    bool
	runStartedAt time.Time
	runEmbedded  int

	// mu guards the photo-ref maps that fan a neighbor blob hash out to the
	// photos (across albums) showing it.
	mu            sync.RWMutex
	photosByHash  map[string][]photoRef
	hashesByAlbum map[string]map[string]struct{}
}

// NewService builds a recommendation service. A nil embedder switches the
// feature off: the API keeps serving empty recommendation lists, no worker
// starts, and every blob stays pending. onEmbeddingDrain, when non-nil, is
// invoked on the worker goroutine after the embedding queue drains; a backup
// hook there is allowed to block, because there is no pending work to delay.
func NewService(cat *catalog.Store, imagesService *images.Service, embedder EmbeddingProvider, onEmbeddingDrain func(ctx context.Context) error) *Service {
	return &Service{
		catalog:          cat,
		images:           imagesService,
		embedder:         embedder,
		onEmbeddingDrain: onEmbeddingDrain,
		photosByHash:     make(map[string][]photoRef),
		hashesByAlbum:    make(map[string]map[string]struct{}),
	}
}

// LoadModel resolves the checkpoint and compiles the inference graph. Startup
// calls it once so a broken checkpoint is reported at boot instead of on the
// first image. It is a no-op when there is no embedder.
//
// A failed load is remembered: Enabled then reports false, so the application
// degrades to running without embeddings.
func (s *Service) LoadModel(ctx context.Context) error {
	if s == nil || s.embedder == nil {
		return nil
	}

	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	if err := s.embedder.Load(ctx); err != nil {
		s.modelErr = fmt.Errorf("load embedding model: %w", err)
		return s.modelErr
	}
	return nil
}

// Enabled reports whether embeddings can actually be computed, so the ingest
// pipeline and the background workers stay out of the way after a failed load.
func (s *Service) Enabled() bool {
	if s == nil || s.embedder == nil {
		return false
	}
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	return s.modelErr == nil
}

// close releases the embedding model.
func (s *Service) close() error {
	if s == nil || s.embedder == nil {
		return nil
	}
	return s.embedder.Close()
}

// computeEmbedding runs one forward pass and keeps the live "embedding is
// happening now" state current for the background worker. Each pass is bounded
// by embeddingTimeout; a deadline counts as transient, so the blob stays
// pending for a later retry instead of being marked failed.
func (s *Service) computeEmbedding(ctx context.Context, imageBytes []byte) ([]float32, error) {
	embedCtx, cancel := context.WithTimeout(ctx, embeddingTimeout)
	defer cancel()

	s.runMu.Lock()
	s.activeEmbeds++
	s.runMu.Unlock()
	defer func() {
		s.runMu.Lock()
		s.activeEmbeds--
		s.runMu.Unlock()
	}()
	return s.embedder.Embed(embedCtx, imageBytes)
}

// LoadAll rebuilds the photo-ref maps from the SQLite catalog. Embeddings are
// not read: they live in the catalog's vec0 index and are queried there. The
// rebuild reads its snapshot of the catalog outside the map lock, so an album
// extraction landing between the read and the swap can leave the refs stale
// until the next reload — the same transient gap as before, but it can no
// longer drop vectors, which never enter memory.
func (s *Service) LoadAll(ctx context.Context) error {
	if s == nil || s.catalog == nil {
		return nil
	}
	pairs, err := s.catalog.ListPhotoBlobPairs(ctx)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetLocked()
	for _, pair := range pairs {
		s.addPairLocked(pair)
	}
	return nil
}

// ReloadAlbum refreshes one album's photos and blob states in memory.
func (s *Service) ReloadAlbum(ctx context.Context, albumID string) error {
	if s == nil || s.catalog == nil || strings.TrimSpace(albumID) == "" {
		return nil
	}
	pairs, err := s.catalog.ListPhotoBlobPairsByAlbum(ctx, albumID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.hashesByAlbum[albumID]
	delete(s.hashesByAlbum, albumID)
	for hash := range previous {
		s.removeHashForAlbumLocked(hash, albumID)
	}
	for _, pair := range pairs {
		s.addPairLocked(pair)
	}
	return nil
}

func (s *Service) resetLocked() {
	s.photosByHash = make(map[string][]photoRef)
	s.hashesByAlbum = make(map[string]map[string]struct{})
}

func (s *Service) addPairLocked(pair catalog.PhotoWithBlob) {
	hash := pair.Photo.Hash
	if hash == "" {
		return
	}
	s.photosByHash[hash] = append(s.photosByHash[hash], photoRef{
		AlbumID: pair.Photo.AlbumID,
		Index:   pair.Photo.Index,
		Hash:    hash,
		Width:   pair.Photo.Width,
		Height:  pair.Photo.Height,
	})
	if s.hashesByAlbum[pair.Photo.AlbumID] == nil {
		s.hashesByAlbum[pair.Photo.AlbumID] = make(map[string]struct{})
	}
	s.hashesByAlbum[pair.Photo.AlbumID][hash] = struct{}{}
}

func (s *Service) removeHashForAlbumLocked(hash string, albumID string) {
	refs := s.photosByHash[hash]
	kept := refs[:0]
	for _, ref := range refs {
		if ref.AlbumID == albumID {
			continue
		}
		kept = append(kept, ref)
	}
	if len(kept) == 0 {
		delete(s.photosByHash, hash)
		return
	}
	s.photosByHash[hash] = kept
}

// Start launches the background embedding workers.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		if !s.Enabled() {
			return
		}

		go func() {
			<-ctx.Done()
			_ = s.close()
		}()

		concurrency := embeddingConcurrency
		for i := 0; i < concurrency; i++ {
			go s.workerLoop(ctx)
		}
	})
}

func (s *Service) workerLoop(ctx context.Context) {
	idleTicker := time.NewTicker(500 * time.Millisecond)
	defer idleTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.finishEmbeddingRun()
			return
		case <-idleTicker.C:
		}

		if s.catalog == nil || s.images == nil || s.embedder == nil {
			continue
		}
		// The external worker API shares this exact claim path, so the two
		// fleets can never double-embed a blob.
		blobs, _, err := s.ClaimEmbeddings(ctx, defaultWorkerBatchSize)
		if err != nil {
			continue
		}
		if len(blobs) == 0 {
			// A tick that actually closes a drain is the moment the catalog has
			// gained everything this process computed: hand it to the finalize
			// hook so a backup captures the embedding writes. The hook runs on
			// this goroutine and may take a while; that is fine at a drain —
			// the queue is empty, and blobs arriving meanwhile wait one tick.
			if s.finishEmbeddingRun() && s.onEmbeddingDrain != nil {
				if err := s.onEmbeddingDrain(ctx); err != nil {
					log.Printf("recommend: embedding drain finalize failed: %v", err)
				}
			}
			continue
		}
		s.beginEmbeddingRun(len(blobs))
		for i, blob := range blobs {
			if ctx.Err() != nil {
				// Never strand the unprocessed remainder: hand their claims
				// back so the next worker picks them up without waiting for
				// the lease to expire.
				_ = s.ReleaseClaims(context.Background(), claimHashes(blobs[i:]))
				return
			}
			// A transient failure leaves the blob pending for a later retry. A
			// panic in the embedder is treated the same way: recover here so
			// the loop survives, hand this claim back, and keep the rest of
			// the batch from being stranded until the lease expires.
			transient := false
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						transient = true
						_ = s.ReleaseClaims(context.Background(), []string{blob.Hash})
						log.Printf("recommend: embed panic recovered blob=%s: %v", blob.Hash, rec)
					}
				}()
				transient = s.embedBlob(ctx, blob.Hash)
			}()
			if transient {
				select {
				case <-ctx.Done():
					_ = s.ReleaseClaims(context.Background(), claimHashes(blobs[i+1:]))
					return
				case <-time.After(2 * time.Second):
				}
			}
			// Renew the lease of everything not yet embedded so a slow batch
			// is not stolen out from under this worker.
			if rest := claimHashes(blobs[i+1:]); len(rest) > 0 {
				if _, err := s.catalog.RenewEmbeddingLeases(ctx, rest, time.Now().Add(DefaultLeaseTTL)); err != nil {
					log.Printf("recommend: lease renew failed blobs=%d: %v", len(rest), err)
				}
			}
			if (i+1)%embeddingProgressEvery == 0 {
				s.logEmbeddingProgress()
			}
		}
	}
}

// claimHashes reduces a claimed batch to the hash list the lease operations
// take.
func claimHashes(blobs []catalog.Blob) []string {
	hashes := make([]string, 0, len(blobs))
	for _, blob := range blobs {
		hashes = append(hashes, blob.Hash)
	}
	return hashes
}

// beginEmbeddingRun opens the log record for one drain of the pending queue.
// Repeated calls while a drain is already open are ignored, so a batch that is
// followed by another batch stays one run.
func (s *Service) beginEmbeddingRun(batch int) {
	s.runMu.Lock()
	if s.runActive {
		s.runMu.Unlock()
		return
	}
	s.runActive = true
	s.runStartedAt = time.Now()
	s.runEmbedded = 0
	s.runMu.Unlock()

	progress := s.EmbeddingProgress()
	log.Printf(
		"recommend: embedding run started pending=%d ready=%d total=%d batch=%d",
		progress.Pending, progress.Ready, progress.Total, batch,
	)
}

// finishEmbeddingRun closes the record opened by beginEmbeddingRun. It is
// called on every idle tick and on shutdown, and reports whether a drain was
// actually in progress and has now closed — the signal the drain hook keys on,
// so a quiet ticker never re-triggers it.
func (s *Service) finishEmbeddingRun() bool {
	s.runMu.Lock()
	if !s.runActive {
		s.runMu.Unlock()
		return false
	}
	embedded := s.runEmbedded
	startedAt := s.runStartedAt
	s.runActive = false
	s.runMu.Unlock()

	progress := s.EmbeddingProgress()
	duration := time.Since(startedAt)
	average := time.Duration(0)
	if embedded > 0 {
		average = duration / time.Duration(embedded)
	}
	log.Printf(
		"recommend: embedding run finished embedded=%d ready=%d/%d pending=%d failed=%d duration=%s avg=%s",
		embedded, progress.Ready, progress.Total, progress.Pending, progress.Failed,
		duration.Round(time.Millisecond), average.Round(time.Millisecond),
	)
	return true
}

// logEmbeddingProgress reports how far a long drain has come. The counts come
// from the catalog, so the line stays correct while the ingest pipeline is
// embedding images of its own at the same time.
func (s *Service) logEmbeddingProgress() {
	progress := s.EmbeddingProgress()
	log.Printf(
		"recommend: embedding progress ready=%d/%d pending=%d failed=%d",
		progress.Ready, progress.Total, progress.Pending, progress.Failed,
	)
}

// EmbeddingProgress reports embedding coverage across the whole catalog
// together with whether anything is being embedded right now.
func (s *Service) EmbeddingProgress() EmbeddingProgress {
	if s == nil || s.catalog == nil {
		return EmbeddingProgress{}
	}
	counts, err := s.catalog.EmbeddingCounts(context.Background())
	if err != nil {
		log.Printf("recommend: embedding counts failed: %v", err)
		return EmbeddingProgress{}
	}
	return s.progressFrom(counts)
}

func (s *Service) progressFrom(counts catalog.EmbeddingCounts) EmbeddingProgress {
	ratio := 0.0
	if counts.Total > 0 {
		ratio = float64(counts.Ready) / float64(counts.Total)
	}

	s.runMu.Lock()
	active := s.activeEmbeds > 0
	s.runMu.Unlock()

	return EmbeddingProgress{
		Enabled:    s.Enabled(),
		Active:     active,
		Total:      counts.Total,
		Ready:      counts.Ready,
		Failed:     counts.Failed,
		Pending:    counts.Pending,
		Processing: counts.Processing,
		Ratio:      ratio,
	}
}

// ClaimEmbeddings leases up to limit pending blobs to one worker and returns
// them together with the lease deadline. It is the single entry point for the
// in-process worker loop and the external worker API alike, so the two fleets
// can never double-embed a blob. It deliberately works even when the local
// model is not loaded: external backfill must not depend on this process
// having a usable checkpoint.
func (s *Service) ClaimEmbeddings(ctx context.Context, limit int) ([]catalog.Blob, time.Time, error) {
	if s == nil || s.catalog == nil {
		return nil, time.Time{}, fmt.Errorf("catalog is not available")
	}
	leaseUntil := time.Now().Add(DefaultLeaseTTL)
	blobs, err := s.catalog.ClaimPendingEmbeddings(ctx, clampClaimLimit(limit), leaseUntil)
	if err != nil {
		return nil, time.Time{}, err
	}
	return blobs, leaseUntil, nil
}

// RenewLeases extends the lease of claimed blobs for another TTL and returns
// the deadline together with the hashes actually renewed. Only blobs still in
// the processing state are touched, so a renewal racing a reclaim is a no-op —
// and the renewed list is how a worker finds out its lease was lost before it
// wastes GPU hours on results that would be rejected anyway.
func (s *Service) RenewLeases(ctx context.Context, hashes []string) (time.Time, []string, error) {
	if s == nil || s.catalog == nil {
		return time.Time{}, nil, fmt.Errorf("catalog is not available")
	}
	leaseUntil := time.Now().Add(DefaultLeaseTTL)
	renewed, err := s.catalog.RenewEmbeddingLeases(ctx, hashes, leaseUntil)
	return leaseUntil, renewed, err
}

// ReleaseClaims hands claimed blobs back to the pending queue without an
// outcome, for transient failures and shutdowns.
func (s *Service) ReleaseClaims(ctx context.Context, hashes []string) error {
	if s == nil || s.catalog == nil {
		return fmt.Errorf("catalog is not available")
	}
	return s.catalog.ReleaseEmbeddingClaims(ctx, hashes)
}

// ApplyEmbeddingResults validates a batch of worker outcomes and persists the
// acceptable ones in one transaction. It is the single write path for the
// in-process worker and external workers alike, and the catalog mirrors each
// landed ready vector into the vec0 index inside that same transaction — which
// is what keeps the index and the catalog in lockstep and makes results
// queryable the moment this returns.
func (s *Service) ApplyEmbeddingResults(ctx context.Context, results []catalog.EmbeddingResult) (applied int, rejected []catalog.RejectedEmbedding, err error) {
	if s == nil || s.catalog == nil {
		return 0, nil, fmt.Errorf("catalog is not available")
	}

	// Validate before the catalog sees anything: a vector of the wrong length,
	// with non-finite entries, or with zero length would poison the index —
	// cosine distance divides by the norm, so a zero vector ranks as NaN and
	// NaN drags the whole neighbor ranking down with it.
	valid := make([]catalog.EmbeddingResult, 0, len(results))
	rejected = make([]catalog.RejectedEmbedding, 0)
	for _, result := range results {
		// Key by the trimmed hash: the catalog normalizes the same way, so
		// reporting rejections under an untrimmed string would disagree with
		// the catalog's own rejected list for the same result.
		result.Hash = strings.TrimSpace(result.Hash)
		if result.Hash == "" {
			rejected = append(rejected, catalog.RejectedEmbedding{Hash: result.Hash, Reason: catalog.EmbeddingRejectInvalidHash})
			continue
		}
		if result.Status == catalog.EmbeddingStatusReady {
			if len(result.Vector) != catalog.EmbeddingDim {
				rejected = append(rejected, catalog.RejectedEmbedding{Hash: result.Hash, Reason: catalog.EmbeddingRejectWrongDim})
				continue
			}
			if !allFinite(result.Vector) || isZeroNorm(result.Vector) {
				rejected = append(rejected, catalog.RejectedEmbedding{Hash: result.Hash, Reason: catalog.EmbeddingRejectBadVector})
				continue
			}
		}
		valid = append(valid, result)
	}

	appliedHashes, catalogRejected, err := s.catalog.ApplyEmbeddingResults(ctx, valid)
	if err != nil {
		return 0, nil, err
	}
	rejected = append(rejected, catalogRejected...)
	// First-wins per hash falls out of the catalog: only the first UPDATE per
	// hash still sees the processing state, and the index row lands in that
	// same transaction.
	return len(appliedHashes), rejected, nil
}

// clampClaimLimit bounds a worker-requested claim size.
func clampClaimLimit(limit int) int {
	if limit <= 0 {
		return DefaultClaimLimit
	}
	if limit > MaxClaimLimit {
		return MaxClaimLimit
	}
	return limit
}

// allFinite reports whether every entry is a usable float32: NaN and ±Inf
// entries would corrupt similarity scores downstream.
func allFinite(vector []float32) bool {
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}

// isZeroNorm reports whether the vector's Euclidean length is zero: every
// entry is zero. Cosine distance divides by the norm, so such a vector's
// distance is NaN anywhere it is compared.
func isZeroNorm(vector []float32) bool {
	for _, value := range vector {
		if value != 0 {
			return false
		}
	}
	return true
}

// embedBlob computes and stores one blob embedding. It reports whether the
// failure was transient (the claim is released and the blob returns to the
// pending queue for an early retry).
func (s *Service) embedBlob(ctx context.Context, hash string) bool {
	result, err := s.images.GetImageByHash(ctx, hash)
	if err != nil {
		log.Printf("recommend: blob=%s image load failed: %v", hash, err)
		s.persistFailed(context.Background(), hash, fmt.Sprintf("load image bytes: %v", err))
		return false
	}

	startedAt := time.Now()
	vector, err := s.computeEmbedding(ctx, result.Bytes)
	if err != nil {
		if isTransientEmbedError(err) {
			log.Printf("recommend: blob=%s embed transient failure: %v", hash, err)
			// Hand the claim back so the retry does not wait out the lease.
			if releaseErr := s.ReleaseClaims(context.Background(), []string{hash}); releaseErr != nil {
				log.Printf("recommend: blob=%s claim release failed: %v", hash, releaseErr)
			}
			return true
		}
		log.Printf("recommend: blob=%s embed failed: %v", hash, err)
		s.persistFailed(context.Background(), hash, fmt.Sprintf("embed image: %v", err))
		return false
	}

	// Use a fresh context: the write must land even when the worker is being
	// shut down mid-batch.
	applied, rejected, err := s.ApplyEmbeddingResults(context.Background(), []catalog.EmbeddingResult{{
		Hash:   hash,
		Status: catalog.EmbeddingStatusReady,
		Vector: vector,
	}})
	if err != nil {
		log.Printf("recommend: blob=%s persist embedding failed: %v", hash, err)
		if releaseErr := s.ReleaseClaims(context.Background(), []string{hash}); releaseErr != nil {
			log.Printf("recommend: blob=%s claim release failed: %v", hash, releaseErr)
		}
		return true
	}
	if applied == 0 {
		// Another worker finished this blob between our claim and our write.
		log.Printf("recommend: blob=%s write-back rejected: %s", hash, rejectionSummary(rejected))
		return false
	}

	s.runMu.Lock()
	if s.runActive {
		s.runEmbedded++
	}
	s.runMu.Unlock()

	log.Printf(
		"recommend: embedded blob=%s bytes=%d dim=%d elapsed=%s",
		hash, len(result.Bytes), len(vector), time.Since(startedAt).Round(time.Millisecond),
	)
	return false
}

// persistFailed records a permanent failure for a claimed blob in the catalog:
// ApplyEmbeddingResults flips the blob to the failed status, which takes it out
// of the pending queue for good and counts it as done in the progress counts.
func (s *Service) persistFailed(ctx context.Context, hash string, errText string) {
	applied, rejected, err := s.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{{
		Hash:   hash,
		Status: catalog.EmbeddingStatusFailed,
		Error:  errText,
	}})
	if err != nil {
		log.Printf("recommend: blob=%s mark failed failed: %v", hash, err)
		return
	}
	if applied == 0 {
		log.Printf("recommend: blob=%s failure report rejected: %s", hash, rejectionSummary(rejected))
	}
}

// rejectionSummary renders rejections for a log line.
func rejectionSummary(rejected []catalog.RejectedEmbedding) string {
	parts := make([]string, 0, len(rejected))
	for _, item := range rejected {
		parts = append(parts, fmt.Sprintf("%s=%s", item.Hash, item.Reason))
	}
	return strings.Join(parts, ",")
}

// Recommend returns cross-album neighbors of a query photo. Neighbors come
// from the catalog's vec0 index (distance-ascending, hash tie-broken); this
// only resolves which photos those blob hashes surface as.
func (s *Service) Recommend(ctx context.Context, albumID string, photoIndex int, limit int) (RecommendationResponse, error) {
	if limit <= 0 {
		limit = defaultTopK
	}
	if limit > maxTopK {
		limit = maxTopK
	}

	if s.catalog == nil {
		return RecommendationResponse{}, fmt.Errorf("catalog is not available")
	}
	photo, err := s.catalog.PhotoAt(ctx, albumID, photoIndex)
	if err != nil {
		if errors.Is(err, catalog.ErrPhotoNotFound) {
			return RecommendationResponse{}, fmt.Errorf("%w: %s:%d", ErrPhotoNotFound, albumID, photoIndex)
		}
		return RecommendationResponse{}, err
	}

	query, failed := s.queryVector(ctx, photo.Hash)
	if failed || len(query) == 0 {
		return RecommendationResponse{Items: []RecommendationItem{}}, nil
	}

	neighbors, err := s.catalog.FindNeighborEmbeddings(ctx, query, catalog.MaxNeighborK)
	if err != nil {
		return RecommendationResponse{}, fmt.Errorf("rank neighbors: %w", err)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]RecommendationItem, 0, limit)
	seenAlbumIDs := make(map[string]struct{}, limit)
	for _, neighbor := range neighbors {
		// Same rule the index-side exclusion used to implement: the query
		// blob never surfaces as its own neighbor, in any album.
		if neighbor.Hash == photo.Hash {
			continue
		}
		for _, ref := range s.photosByHash[neighbor.Hash] {
			// Recommendations are cross-album only.
			if ref.AlbumID == albumID {
				continue
			}
			if _, seen := seenAlbumIDs[ref.AlbumID]; seen {
				continue
			}
			seenAlbumIDs[ref.AlbumID] = struct{}{}
			items = append(items, RecommendationItem{
				AlbumID: ref.AlbumID,
				I:       ref.Index,
				Hash:    ref.Hash,
				W:       ref.Width,
				H:       ref.Height,
				Score:   1 - neighbor.Distance,
			})
			break
		}
		if len(items) >= limit {
			break
		}
	}

	return RecommendationResponse{Items: items}, nil
}

// queryVector reads the query blob's own embedding from the catalog. It
// reports failed=true for a blob whose embedding failed terminally, so such a
// query answers with an empty list instead of neighbors ranked against
// nothing.
func (s *Service) queryVector(ctx context.Context, hash string) ([]float32, bool) {
	blob, err := s.catalog.GetBlob(ctx, hash)
	if err != nil {
		return nil, false
	}
	switch blob.EmbeddingStatus {
	case catalog.EmbeddingStatusReady:
		// The dim check keeps a legacy mis-sized "ready" row (which the
		// index backfill skipped) from erroring the KNN query; it serves
		// empty items, matching the pending case.
		if len(blob.Embedding) == catalog.EmbeddingDim {
			return blob.Embedding, false
		}
		return nil, false
	case catalog.EmbeddingStatusFailed:
		return nil, true
	default:
		return nil, false
	}
}
