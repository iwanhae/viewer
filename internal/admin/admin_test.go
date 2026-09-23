package admin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/recommend"
)

func openTestCatalog(t *testing.T) *catalog.Store {
	t.Helper()
	store, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// fakeVectorCounter stands in for the Qdrant client, recording how often Stats
// consulted it so the tests can pin the read actually happening per call.
type fakeVectorCounter struct {
	points int
	err    error
	calls  int
}

func (f *fakeVectorCounter) CountVectors(ctx context.Context) (int, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	return f.points, nil
}

// stubEmbedder is an always-loadable provider, the only thing that flips
// recommend.Enabled (and therefore Stats.ModelEnabled) to true in tests.
type stubEmbedder struct{}

func (stubEmbedder) Load(context.Context) error                       { return nil }
func (stubEmbedder) Embed(context.Context, []byte) ([]float32, error) { return nil, nil }
func (stubEmbedder) Close() error                                     { return nil }

// seedLibrary builds two albums sharing one blob: album-a (SUCCEEDED) holds
// photos for hash-ready and hash-pending, album-b (FAILED) reuses hash-ready
// for its only photo. hash-ready goes through the public claim→apply path so
// its terminal state is the production one. The result: 2 albums, 3 photos,
// 2 unique blobs, 1 ready blob referenced by 2 photos, 1 pending blob.
func seedLibrary(t *testing.T, store *catalog.Store) {
	t.Helper()
	ctx := context.Background()

	albums := []catalog.Album{
		{ID: "album-a", OriginalFilename: "a.zip", Status: catalog.AlbumStatusReady},
		{ID: "album-b", OriginalFilename: "b.zip", Status: catalog.AlbumStatusFailed},
	}
	for _, album := range albums {
		if err := store.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("create album: %v", err)
		}
	}
	photos := []catalog.Photo{
		{AlbumID: "album-a", Index: 0, Name: "r.jpg", Hash: "hash-ready", Width: 4, Height: 4, Ratio: 1},
		{AlbumID: "album-a", Index: 1, Name: "p.jpg", Hash: "hash-pending", Width: 4, Height: 4, Ratio: 1},
		{AlbumID: "album-b", Index: 0, Name: "r.jpg", Hash: "hash-ready", Width: 4, Height: 4, Ratio: 1},
	}
	for _, photo := range photos {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-ready", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-ready: %v", err)
	}
	claimed, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("claim hash-ready: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Hash != "hash-ready" {
		t.Fatalf("claim hash-ready: got %v", claimed)
	}
	applied, rejected, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-ready", Status: catalog.EmbeddingStatusReady, Vector: make([]float32, catalog.EmbeddingDim)},
	})
	if err != nil || len(applied) != 1 || len(rejected) != 0 {
		t.Fatalf("apply hash-ready: applied=%v rejected=%v err=%v", applied, rejected, err)
	}
	// Upserted after the claim, so this one never left the pending queue.
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-pending", SizeBytes: 2}); err != nil {
		t.Fatalf("upsert hash-pending: %v", err)
	}
}

func TestStatsAggregatesCatalogAndVectorStore(t *testing.T) {
	store := openTestCatalog(t)
	seedLibrary(t, store)
	ctx := context.Background()

	// One point fewer than the two ready pairs: the wiped-collection shape,
	// where the drift must come out negative.
	counter := &fakeVectorCounter{points: 1}
	service := NewService(store, counter, nil)

	stats, err := service.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Albums != 2 {
		t.Fatalf("albums=%d want=2", stats.Albums)
	}
	if len(stats.AlbumsByStatus) != 2 || stats.AlbumsByStatus["SUCCEEDED"] != 1 || stats.AlbumsByStatus["FAILED"] != 1 {
		t.Fatalf("albumsByStatus=%v want {SUCCEEDED:1 FAILED:1}", stats.AlbumsByStatus)
	}
	if stats.Photos != 3 {
		t.Fatalf("photos=%d want=3", stats.Photos)
	}
	if stats.Blobs != 2 {
		t.Fatalf("blobs=%d want=2 (unique content)", stats.Blobs)
	}
	wantEmbedding := catalog.EmbeddingCounts{Total: 2, Ready: 1, Pending: 1}
	if stats.Embedding != wantEmbedding {
		t.Fatalf("embedding=%+v want=%+v", stats.Embedding, wantEmbedding)
	}
	if stats.ExpectedPoints != 2 {
		t.Fatalf("expectedPoints=%d want=2 (hash-ready is referenced by two photos)", stats.ExpectedPoints)
	}
	if stats.QdrantPoints != 1 {
		t.Fatalf("qdrantPoints=%d want=1", stats.QdrantPoints)
	}
	if stats.Drift != -1 {
		t.Fatalf("drift=%d want=-1 (store holds fewer points than the catalog promises)", stats.Drift)
	}
	if stats.ModelEnabled {
		t.Fatalf("modelEnabled=true want=false (no recommend service)")
	}
	if counter.calls != 1 {
		t.Fatalf("CountVectors calls=%d want=1", counter.calls)
	}
}

func TestStatsPositiveDriftMeansStalePoints(t *testing.T) {
	store := openTestCatalog(t)
	seedLibrary(t, store)

	service := NewService(store, &fakeVectorCounter{points: 5}, nil)
	stats, err := service.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Drift != 3 {
		t.Fatalf("drift=%d want=+3 (store holds more points than the catalog promises)", stats.Drift)
	}
}

func TestStatsZeroDriftWhenCountsMatch(t *testing.T) {
	store := openTestCatalog(t)
	seedLibrary(t, store)

	service := NewService(store, &fakeVectorCounter{points: 2}, nil)
	stats, err := service.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Drift != 0 {
		t.Fatalf("drift=%d want=0", stats.Drift)
	}
}

func TestStatsNilVectorCounterDegradesWithoutError(t *testing.T) {
	store := openTestCatalog(t)
	seedLibrary(t, store)

	service := NewService(store, nil, nil)
	stats, err := service.Stats(context.Background())
	if err != nil {
		t.Fatalf("stats with nil vector counter: %v", err)
	}
	if stats.QdrantPoints != 0 {
		t.Fatalf("qdrantPoints=%d want=0", stats.QdrantPoints)
	}
	if stats.Drift != -2 {
		t.Fatalf("drift=%d want=-2", stats.Drift)
	}
}

func TestStatsNilCatalogErrors(t *testing.T) {
	service := NewService(nil, &fakeVectorCounter{points: 1}, nil)
	if _, err := service.Stats(context.Background()); err == nil {
		t.Fatalf("expected an error for a nil catalog")
	}
}

func TestStatsModelEnabledFollowsRecommendService(t *testing.T) {
	store := openTestCatalog(t)
	ctx := context.Background()

	disabled := NewService(store, nil, recommend.NewService(store, nil, nil, nil, nil))
	stats, err := disabled.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.ModelEnabled {
		t.Fatalf("modelEnabled=true want=false (embedder is nil)")
	}

	enabled := NewService(store, nil, recommend.NewService(store, nil, nil, stubEmbedder{}, nil))
	stats, err = enabled.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if !stats.ModelEnabled {
		t.Fatalf("modelEnabled=false want=true (embedder loaded cleanly)")
	}
}

func TestStatsVectorCounterErrorPropagates(t *testing.T) {
	store := openTestCatalog(t)
	service := NewService(store, &fakeVectorCounter{err: errors.New("qdrant down")}, nil)
	if _, err := service.Stats(context.Background()); err == nil {
		t.Fatalf("expected the vector counter error to surface")
	}
}

func TestReindexReturnsPostResetCounts(t *testing.T) {
	store := openTestCatalog(t)
	ctx := context.Background()

	// Four blobs, one per state: hash-ready goes ready through the public
	// path, hash-failed through the same path with a failure outcome,
	// hash-processing is left claimed under a live lease, hash-pending is
	// never claimed.
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-ready", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-ready: %v", err)
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-failed", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-failed: %v", err)
	}
	claimed, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("claim ready and failed: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claim ready and failed: got %v", claimed)
	}
	applied, rejected, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-ready", Status: catalog.EmbeddingStatusReady, Vector: make([]float32, catalog.EmbeddingDim)},
		{Hash: "hash-failed", Status: catalog.EmbeddingStatusFailed, Error: "boom"},
	})
	if err != nil || len(applied) != 2 || len(rejected) != 0 {
		t.Fatalf("apply ready and failed: applied=%v rejected=%v err=%v", applied, rejected, err)
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-processing", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-processing: %v", err)
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("claim hash-processing: %v", err)
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-pending", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-pending: %v", err)
	}

	service := NewService(store, nil, nil)
	counts, err := service.Reindex(ctx)
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	// Pending is derived as Total-Ready-Failed, so the blob still processing
	// under its live lease keeps counting toward it — the reset moved exactly
	// the two terminal rows, which is what the surge from 1 to 4 shows.
	want := catalog.EmbeddingCounts{Total: 4, Pending: 4, Processing: 1}
	if counts != want {
		t.Fatalf("reindex counts=%+v want=%+v", counts, want)
	}
	// The returned snapshot must match a fresh read, and the reset must have
	// landed in the catalog itself, not just in the response.
	fresh, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("fresh embedding counts: %v", err)
	}
	if fresh != want {
		t.Fatalf("fresh embedding counts=%+v want=%+v", fresh, want)
	}
}

func TestReindexNilCatalogErrors(t *testing.T) {
	service := NewService(nil, nil, nil)
	if _, err := service.Reindex(context.Background()); err == nil {
		t.Fatalf("expected an error for a nil catalog")
	}
}
