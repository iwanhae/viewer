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
	"viewer/internal/qdrant"
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

	// defaultSearchTopK and maxSearchTopK bound natural-language search result
	// sizes. They are separate from the recommendation bounds because search
	// returns every matching photo instead of one per album, so a query needs
	// a wider ceiling before it starts feeling truncated.
	defaultSearchTopK = 24
	maxSearchTopK     = 96
)

// Service runs the background workers that compute embeddings for the
// catalog's pending blobs and serves cross-album recommendations out of the
// external vector store. The service owns no vector state itself: SQLite keeps
// the bookkeeping (which blob is embedded, by which status) and the vector
// store keeps the vectors, so the two are written in that order — index first,
// bookkeeping second — by the single ApplyEmbeddingResults path. Extraction
// never embeds anything itself: the in-process workers and external workers
// leasing batches through ClaimEmbeddings are the only producers of
// embeddings, and both write through ApplyEmbeddingResults.
type Service struct {
	catalog *catalog.Store

	// vectors is nil when no vector store is wired up (a deployment without
	// QDRANT_URL, some tests). Writes then degrade to SQLite-only bookkeeping
	// and Recommend reports the store as unavailable (ErrVectorStoreUnavailable)
	// rather than answering with an empty list that looks like "no similar
	// photos".
	vectors VectorStore

	images *images.Service

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
}

// NewService builds a recommendation service. A nil embedder switches the
// feature off: the API keeps serving empty recommendation lists, no worker
// starts, and every blob stays pending. A nil vectors store leaves the service
// usable for embedding work but without searchable recommendations.
// onEmbeddingDrain, when non-nil, is invoked on the worker goroutine after the
// embedding queue drains; a backup hook there is allowed to block, because
// there is no pending work to delay.
func NewService(cat *catalog.Store, vectors VectorStore, imagesService *images.Service, embedder EmbeddingProvider, onEmbeddingDrain func(ctx context.Context) error) *Service {
	return &Service{
		catalog:          cat,
		vectors:          vectors,
		images:           imagesService,
		embedder:         embedder,
		onEmbeddingDrain: onEmbeddingDrain,
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
// acceptable ones. It is the single write path for the in-process worker and
// external workers alike, and it writes the two stores in a fixed order:
//
//  1. The vector store first. Every accepted ready vector is turned into its
//     photo points — one per (album, idx) the hash appears under, resolved
//     from the catalog — and upserted. A store failure returns an error and
//     leaves SQLite completely untouched, so the blobs stay "processing" and
//     the worker hands its claims back for a clean retry.
//  2. SQLite bookkeeping second, in one transaction.
//
// The ordering makes both crash windows recoverable. A crash between the two
// leaves Qdrant written but SQLite still saying "processing": the claim is
// re-leased, the blob is re-embedded, and the write-back upserts the very same
// deterministic point IDs, overwriting the first copy in place. A crash inside
// the SQLite transaction loses only the bookkeeping and recovers the same way.
// Neither window can produce a duplicate or a vector the catalog does not know
// about — the reverse order could.
func (s *Service) ApplyEmbeddingResults(ctx context.Context, results []catalog.EmbeddingResult) (applied int, rejected []catalog.RejectedEmbedding, err error) {
	if s == nil || s.catalog == nil {
		return 0, nil, fmt.Errorf("catalog is not available")
	}

	// Validate before either store sees anything: a vector of the wrong length,
	// with non-finite entries, or with zero length would poison search —
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

	// Ready vectors go to the store before the catalog claims them. First wins
	// per hash, matching the catalog's own first-wins UPDATE: a duplicate
	// report of the same hash re-upserts the identical point IDs, so it is
	// harmless rather than corrupting.
	if s.vectors != nil {
		if err := s.uploadReadyVectors(ctx, valid); err != nil {
			return 0, nil, err
		}
	}

	appliedHashes, catalogRejected, err := s.catalog.ApplyEmbeddingResults(ctx, valid)
	if err != nil {
		return 0, nil, err
	}
	rejected = append(rejected, catalogRejected...)
	// First-wins per hash falls out of the catalog: only the first UPDATE per
	// hash still sees the processing state.
	return len(appliedHashes), rejected, nil
}

// uploadReadyVectors pushes every ready vector in an already-validated batch
// into the vector store, one point per photo the hash appears under. Blobs no
// photo references have no (album, idx) to become a point and are skipped —
// their bookkeeping still lands, they just never become searchable, which is
// the same treatment unreferenced blobs always had.
func (s *Service) uploadReadyVectors(ctx context.Context, valid []catalog.EmbeddingResult) error {
	vectorsByHash := make(map[string][]float32)
	readyHashes := make([]string, 0, len(valid))
	for _, result := range valid {
		if result.Status != catalog.EmbeddingStatusReady {
			continue
		}
		if _, dup := vectorsByHash[result.Hash]; dup {
			continue
		}
		vectorsByHash[result.Hash] = result.Vector
		readyHashes = append(readyHashes, result.Hash)
	}
	if len(readyHashes) == 0 {
		return nil
	}

	pairs, err := s.catalog.ListPhotoBlobPairsByHashes(ctx, readyHashes)
	if err != nil {
		return fmt.Errorf("resolve photos for %d embedded blob(s): %w", len(readyHashes), err)
	}
	records := make([]qdrant.PhotoRecord, 0, len(pairs))
	for _, pair := range pairs {
		vector, ok := vectorsByHash[pair.Photo.Hash]
		if !ok {
			continue
		}
		records = append(records, qdrant.PhotoRecord{
			AlbumID: pair.Photo.AlbumID,
			Idx:     pair.Photo.Index,
			Hash:    pair.Photo.Hash,
			W:       pair.Photo.Width,
			H:       pair.Photo.Height,
			Vector:  vector,
		})
	}
	if len(records) == 0 {
		return nil
	}
	if err := s.vectors.UpsertPhotos(ctx, records); err != nil {
		return fmt.Errorf("upload %d photo vector(s): %w", len(records), err)
	}
	return nil
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
	data, err := s.images.GetImageBytes(ctx, hash)
	if err != nil {
		log.Printf("recommend: blob=%s image load failed: %v", hash, err)
		s.persistFailed(context.Background(), hash, fmt.Sprintf("load image bytes: %v", err))
		return false
	}

	startedAt := time.Now()
	vector, err := s.computeEmbedding(ctx, data)
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
		hash, len(data), len(vector), time.Since(startedAt).Round(time.Millisecond),
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

// Recommend returns cross-album neighbors of a query photo: the vector store
// searches for points similar to the query photo's own point, already grouped
// so only the best photo per album comes back, with the query's own album and
// hash excluded on the store side. This only maps the ranked hits onto the API
// response.
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
	if s.vectors == nil {
		return RecommendationResponse{}, ErrVectorStoreUnavailable
	}

	photo, err := s.catalog.PhotoAt(ctx, albumID, photoIndex)
	if err != nil {
		if errors.Is(err, catalog.ErrPhotoNotFound) {
			return RecommendationResponse{}, fmt.Errorf("%w: %s:%d", ErrPhotoNotFound, albumID, photoIndex)
		}
		return RecommendationResponse{}, err
	}

	// A blob the embedding pipeline has not delivered for — still pending,
	// currently processing, or terminally failed — has no point to search
	// from, so such a query answers with an empty list rather than neighbors
	// ranked against nothing.
	blob, err := s.catalog.GetBlob(ctx, photo.Hash)
	if err != nil || blob.EmbeddingStatus != catalog.EmbeddingStatusReady {
		return RecommendationResponse{Items: []RecommendationItem{}}, nil
	}

	hits, err := s.vectors.GroupSearch(ctx, qdrant.PointID(albumID, photoIndex), albumID, photo.Hash, limit)
	if err != nil {
		return RecommendationResponse{}, fmt.Errorf("group search: %w", err)
	}
	if len(hits) == 0 {
		return RecommendationResponse{Items: []RecommendationItem{}}, nil
	}

	items := make([]RecommendationItem, 0, len(hits))
	for _, hit := range hits {
		items = append(items, RecommendationItem{
			AlbumID: hit.AlbumID,
			I:       hit.Idx,
			Hash:    hit.Hash,
			W:       hit.W,
			H:       hit.H,
			Score:   hit.Score,
		})
	}

	return RecommendationResponse{Items: items}, nil
}

// Search embeds a natural-language query with the SigLIP2 text tower and
// returns the nearest embedded photos, best first. Unlike Recommend there is
// no album grouping or exclusion: every matching photo comes back, so a
// picture posted in several albums simply ranks once per point it has. The
// text half of the model is found by a capability check at call time, so a
// service built with an image-only embedder reports
// ErrTextEmbeddingUnavailable rather than failing to construct at all.
func (s *Service) Search(ctx context.Context, query string, limit int) (RecommendationResponse, error) {
	if limit <= 0 {
		limit = defaultSearchTopK
	}
	if limit > maxSearchTopK {
		limit = maxSearchTopK
	}

	if s.vectors == nil {
		return RecommendationResponse{}, ErrVectorStoreUnavailable
	}
	textEmbedder, ok := s.embedder.(TextEmbeddingProvider)
	if !ok {
		return RecommendationResponse{}, ErrTextEmbeddingUnavailable
	}
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return RecommendationResponse{}, ErrEmptyQuery
	}

	vector, err := textEmbedder.EmbedText(ctx, trimmed)
	if err != nil {
		return RecommendationResponse{}, fmt.Errorf("embed query: %w", err)
	}
	// The same degenerate-vector rules as the write path: a NaN/Inf or all-zero
	// text vector would rank as NaN in the cosine distance and poison the
	// whole neighbor list, so it is rejected before the store is asked.
	if !allFinite(vector) || isZeroNorm(vector) {
		return RecommendationResponse{}, fmt.Errorf("text embedder returned a degenerate vector with %d entries", len(vector))
	}

	hits, err := s.vectors.SearchByVector(ctx, vector, limit)
	if err != nil {
		return RecommendationResponse{}, fmt.Errorf("search by vector: %w", err)
	}

	items := make([]RecommendationItem, 0, len(hits))
	for _, hit := range hits {
		items = append(items, RecommendationItem{
			AlbumID: hit.AlbumID,
			I:       hit.Idx,
			Hash:    hit.Hash,
			W:       hit.W,
			H:       hit.H,
			Score:   hit.Score,
		})
	}

	return RecommendationResponse{Items: items}, nil
}

// SearchByImage embeds an uploaded image with the vision tower and returns the
// nearest embedded photos, best first. The query vector lands in the same
// space as the stored points — it is the exact tower the ingest pipeline uses
// — and like Search there is no album grouping or exclusion: every matching
// photo comes back.
//
// The error classes mirror the wire contract. A payload that is empty or
// undecodable is the request's fault and reports ErrUnreadableImage, which
// httpapi answers with a 400. A store that is not wired up or an embedder
// that cannot run are deployment states and report ErrVectorStoreUnavailable
// or ErrImageEmbeddingUnavailable, which httpapi answers with a 503. Anything
// else is a genuine server fault.
func (s *Service) SearchByImage(ctx context.Context, imageBytes []byte, limit int) (RecommendationResponse, error) {
	if limit <= 0 {
		limit = defaultSearchTopK
	}
	if limit > maxSearchTopK {
		limit = maxSearchTopK
	}

	if s.vectors == nil {
		return RecommendationResponse{}, ErrVectorStoreUnavailable
	}
	if s.embedder == nil {
		return RecommendationResponse{}, ErrImageEmbeddingUnavailable
	}
	// An empty upload is as unreadable as a corrupt one, and it is caught here
	// so the check does not depend on the embedder noticing.
	if len(imageBytes) == 0 {
		return RecommendationResponse{}, ErrUnreadableImage
	}

	vector, err := s.embedder.Embed(ctx, imageBytes)
	if err != nil {
		if errors.Is(err, ErrUnreadableImage) {
			// Pass the classified error through unwrapped: the 400 is the
			// client's to fix, and the preprocess detail belongs on the wire.
			return RecommendationResponse{}, err
		}
		// Every other embed failure is the deployment, not the request: the
		// sentinel marks it so httpapi answers 503 instead of 500.
		return RecommendationResponse{}, fmt.Errorf("embed query image: %w: %w", err, ErrImageEmbeddingUnavailable)
	}
	// The same degenerate-vector rules as the write path: a NaN/Inf or all-zero
	// query vector would rank as NaN in the cosine distance and poison the
	// whole neighbor list, so it is rejected before the store is asked.
	if !allFinite(vector) || isZeroNorm(vector) {
		return RecommendationResponse{}, fmt.Errorf("image embedder returned a degenerate vector with %d entries", len(vector))
	}

	hits, err := s.vectors.SearchByVector(ctx, vector, limit)
	if err != nil {
		return RecommendationResponse{}, fmt.Errorf("search by vector: %w", err)
	}

	items := make([]RecommendationItem, 0, len(hits))
	for _, hit := range hits {
		items = append(items, RecommendationItem{
			AlbumID: hit.AlbumID,
			I:       hit.Idx,
			Hash:    hit.Hash,
			W:       hit.W,
			H:       hit.H,
			Score:   hit.Score,
		})
	}

	return RecommendationResponse{Items: items}, nil
}

// ReloadAlbum re-syncs one album's points in the vector store with the
// catalog's current photo rows: the album's existing points are deleted, the
// album's photos with ready embeddings are read back from the catalog, their
// vectors are retrieved from the store, and the points are written again. A
// photo whose blob is not ready — or whose vector the store does not have —
// is simply absent afterwards, which is exactly right for an album whose
// photos were replaced by a re-upload.
func (s *Service) ReloadAlbum(ctx context.Context, albumID string) error {
	if s == nil || s.catalog == nil || s.vectors == nil || strings.TrimSpace(albumID) == "" {
		return nil
	}

	// Delete first: points for (album, idx) combinations that no longer exist
	// must go even when the replacement photo set ends up empty.
	if err := s.vectors.DeleteByAlbum(ctx, albumID); err != nil {
		return fmt.Errorf("delete album %q vectors: %w", albumID, err)
	}

	pairs, err := s.catalog.ListPhotoBlobPairsByAlbum(ctx, albumID)
	if err != nil {
		return err
	}
	hashes := make([]string, 0, len(pairs))
	seen := make(map[string]struct{}, len(pairs))
	for _, pair := range pairs {
		if pair.Blob.EmbeddingStatus != catalog.EmbeddingStatusReady || pair.Photo.Hash == "" {
			continue
		}
		if _, dup := seen[pair.Photo.Hash]; dup {
			continue
		}
		seen[pair.Photo.Hash] = struct{}{}
		hashes = append(hashes, pair.Photo.Hash)
	}
	if len(hashes) == 0 {
		return nil
	}

	vectorsByHash, err := s.vectors.RetrieveVectorsByHashes(ctx, hashes)
	if err != nil {
		return fmt.Errorf("retrieve album %q vectors: %w", albumID, err)
	}
	records := make([]qdrant.PhotoRecord, 0, len(pairs))
	for _, pair := range pairs {
		vector, ok := vectorsByHash[pair.Photo.Hash]
		if !ok || len(vector) == 0 {
			continue
		}
		records = append(records, qdrant.PhotoRecord{
			AlbumID: pair.Photo.AlbumID,
			Idx:     pair.Photo.Index,
			Hash:    pair.Photo.Hash,
			W:       pair.Photo.Width,
			H:       pair.Photo.Height,
			Vector:  vector,
		})
	}
	if len(records) == 0 {
		return nil
	}
	return s.vectors.UpsertPhotos(ctx, records)
}
