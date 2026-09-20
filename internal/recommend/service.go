package recommend

import (
	"context"
	"errors"
	"fmt"
	"log"
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

// Service keeps an in-memory similarity index of blob embeddings and runs the
// background workers that fill in embeddings the ingest pipeline could not
// compute.
type Service struct {
	catalog *catalog.Store
	images  *images.Service

	// embedder is nil when the recommendation feature is switched off, which is
	// how tests run without a checkpoint.
	embedder EmbeddingProvider

	startOnce sync.Once
	startErr  error

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

	mu               sync.RWMutex
	photosByHash     map[string][]photoRef
	hashesByAlbum    map[string]map[string]struct{}
	embeddingsByHash map[string][]float32
	failedByHash     map[string]string
}

// NewService builds a recommendation service. A nil embedder switches the
// feature off: the API keeps serving empty recommendation lists and the ingest
// pipeline leaves every blob pending.
func NewService(cat *catalog.Store, imagesService *images.Service, embedder EmbeddingProvider) *Service {
	return &Service{
		catalog:          cat,
		images:           imagesService,
		embedder:         embedder,
		photosByHash:     make(map[string][]photoRef),
		hashesByAlbum:    make(map[string]map[string]struct{}),
		embeddingsByHash: make(map[string][]float32),
		failedByHash:     make(map[string]string),
	}
}

// ErrEmbeddingDisabled reports that embedding was switched off.
var ErrEmbeddingDisabled = errors.New("image embedding is disabled")

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

// Close releases the embedding model.
func (s *Service) Close() error {
	if s == nil || s.embedder == nil {
		return nil
	}
	return s.embedder.Close()
}

// Embed satisfies the pipeline's embedder interface.
func (s *Service) Embed(ctx context.Context, imageBytes []byte) ([]float32, error) {
	if s == nil || s.embedder == nil {
		return nil, ErrEmbeddingDisabled
	}
	embedCtx, cancel := context.WithTimeout(ctx, embeddingTimeout)
	defer cancel()
	return s.computeEmbedding(embedCtx, imageBytes)
}

// computeEmbedding runs one forward pass and keeps the live "embedding is
// happening now" state current for both the ingest pipeline and the background
// worker.
func (s *Service) computeEmbedding(ctx context.Context, imageBytes []byte) ([]float32, error) {
	s.runMu.Lock()
	s.activeEmbeds++
	s.runMu.Unlock()
	defer func() {
		s.runMu.Lock()
		s.activeEmbeds--
		s.runMu.Unlock()
	}()
	return s.embedder.Embed(ctx, imageBytes)
}

// LoadAll rebuilds the in-memory index from the SQLite catalog.
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
	s.embeddingsByHash = make(map[string][]float32)
	s.failedByHash = make(map[string]string)
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
		Ratio:   pair.Photo.Ratio,
	})
	if s.hashesByAlbum[pair.Photo.AlbumID] == nil {
		s.hashesByAlbum[pair.Photo.AlbumID] = make(map[string]struct{})
	}
	s.hashesByAlbum[pair.Photo.AlbumID][hash] = struct{}{}

	switch pair.Blob.EmbeddingStatus {
	case catalog.EmbeddingStatusReady:
		if len(pair.Blob.Embedding) > 0 {
			s.embeddingsByHash[hash] = normalizeVector(pair.Blob.Embedding)
			delete(s.failedByHash, hash)
		}
	case catalog.EmbeddingStatusFailed:
		s.failedByHash[hash] = pair.Blob.EmbeddingError
	}
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
func (s *Service) Start(ctx context.Context) error {
	s.startOnce.Do(func() {
		if !s.Enabled() {
			s.startErr = nil
			return
		}

		go func() {
			<-ctx.Done()
			_ = s.Close()
		}()

		concurrency := embeddingConcurrency
		for i := 0; i < concurrency; i++ {
			go s.workerLoop(ctx)
		}
		s.startErr = nil
	})
	return s.startErr
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
		blobs, err := s.catalog.ListBlobsAwaitingEmbedding(ctx, defaultWorkerBatchSize)
		if err != nil {
			continue
		}
		if len(blobs) == 0 {
			s.finishEmbeddingRun()
			continue
		}
		s.beginEmbeddingRun(len(blobs))
		for i, blob := range blobs {
			if ctx.Err() != nil {
				return
			}
			// A transient failure leaves the blob pending for a later retry.
			if s.embedBlob(ctx, blob.Hash) {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
			if (i+1)%embeddingProgressEvery == 0 {
				s.logEmbeddingProgress()
			}
		}
	}
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
// called on every idle tick and logs nothing unless a drain was in progress.
func (s *Service) finishEmbeddingRun() {
	s.runMu.Lock()
	if !s.runActive {
		s.runMu.Unlock()
		return
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

// AlbumEmbeddingProgress reports coverage for the distinct blobs one album
// references, which is the progress a client uploading that album cares about.
func (s *Service) AlbumEmbeddingProgress(ctx context.Context, albumID string) (EmbeddingProgress, error) {
	if s == nil || s.catalog == nil || strings.TrimSpace(albumID) == "" {
		return EmbeddingProgress{}, nil
	}
	counts, err := s.catalog.EmbeddingCountsByAlbum(ctx, albumID)
	if err != nil {
		return EmbeddingProgress{}, err
	}
	progress := s.progressFrom(counts)
	progress.Active = false
	return progress, nil
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
		Enabled: s.Enabled(),
		Active:  active,
		Total:   counts.Total,
		Ready:   counts.Ready,
		Failed:  counts.Failed,
		Pending: counts.Pending,
		Ratio:   ratio,
	}
}

// embedBlob computes and stores one blob embedding. It reports whether the
// failure was transient (the blob stays pending and is retried).
func (s *Service) embedBlob(ctx context.Context, hash string) bool {
	result, err := s.images.GetImageByHash(ctx, hash)
	if err != nil {
		log.Printf("recommend: blob=%s image load failed: %v", hash, err)
		s.markFailed(ctx, hash, fmt.Sprintf("load image bytes: %v", err))
		return false
	}

	startedAt := time.Now()
	vector, err := s.computeEmbedding(ctx, result.Bytes)
	if err != nil {
		if isTransientEmbedError(err) {
			log.Printf("recommend: blob=%s embed transient failure: %v", hash, err)
			return true
		}
		log.Printf("recommend: blob=%s embed failed: %v", hash, err)
		s.markFailed(ctx, hash, fmt.Sprintf("embed image: %v", err))
		return false
	}

	if err := s.catalog.SetBlobEmbedding(ctx, hash, catalog.EmbeddingStatusReady, vector, ""); err != nil {
		log.Printf("recommend: blob=%s persist embedding failed: %v", hash, err)
		return true
	}
	s.mu.Lock()
	s.embeddingsByHash[hash] = normalizeVector(vector)
	delete(s.failedByHash, hash)
	s.mu.Unlock()

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

func (s *Service) markFailed(ctx context.Context, hash string, errText string) {
	if s.catalog != nil {
		_ = s.catalog.SetBlobEmbedding(context.Background(), hash, catalog.EmbeddingStatusFailed, nil, errText)
	}
	s.mu.Lock()
	s.failedByHash[hash] = errText
	s.mu.Unlock()
}

// Recommend returns cross-album neighbors of a query photo.
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

	s.mu.RLock()
	neighbors := findNeighbors(s.embeddingsByHash, query, len(s.embeddingsByHash), photo.Hash)
	items := make([]RecommendationItem, 0, limit)
	seenAlbumIDs := make(map[string]struct{}, limit)
	for _, neighbor := range neighbors {
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
				W:       ref.Width,
				H:       ref.Height,
				Score:   neighbor.Score,
			})
			break
		}
		if len(items) >= limit {
			break
		}
	}
	s.mu.RUnlock()

	return RecommendationResponse{Items: items}, nil
}

// queryVector returns a normalized embedding for a hash, falling back to the
// catalog when the in-memory index has not seen it yet.
func (s *Service) queryVector(ctx context.Context, hash string) ([]float32, bool) {
	s.mu.RLock()
	if _, failed := s.failedByHash[hash]; failed {
		s.mu.RUnlock()
		return nil, true
	}
	if vector, ok := s.embeddingsByHash[hash]; ok {
		s.mu.RUnlock()
		return vector, false
	}
	s.mu.RUnlock()

	blob, err := s.catalog.GetBlob(ctx, hash)
	if err != nil {
		return nil, false
	}
	switch blob.EmbeddingStatus {
	case catalog.EmbeddingStatusFailed:
		s.mu.Lock()
		s.failedByHash[hash] = blob.EmbeddingError
		s.mu.Unlock()
		return nil, true
	case catalog.EmbeddingStatusReady:
		if len(blob.Embedding) == 0 {
			return nil, false
		}
		normalized := normalizeVector(blob.Embedding)
		s.mu.Lock()
		s.embeddingsByHash[hash] = normalized
		s.mu.Unlock()
		return normalized, false
	default:
		return nil, false
	}
}
