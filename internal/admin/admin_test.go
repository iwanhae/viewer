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
	if !stats.VectorStoreEnabled {
		t.Fatalf("vectorStoreEnabled=false want=true (a vector counter is wired)")
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
	if stats.VectorStoreEnabled {
		t.Fatalf("vectorStoreEnabled=true want=false (nil vector counter)")
	}
	if stats.QdrantPoints != 0 {
		t.Fatalf("qdrantPoints=%d want=0", stats.QdrantPoints)
	}
	// Drift stays 0 rather than -2: with no store there is no measurement,
	// and a fabricated negative would render the dashboard as "missing
	// points" when the truth is just "Qdrant is off".
	if stats.Drift != 0 {
		t.Fatalf("drift=%d want=0", stats.Drift)
	}
	if stats.ExpectedPoints != 2 {
		t.Fatalf("expectedPoints=%d want=2 (the catalog's real ready-pair count)", stats.ExpectedPoints)
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

// seedEmbeddingDiagnostics builds every diagnostic shape Stats reports at once:
// two processing rows (one live lease, one expired), three failed rows carrying
// two distinct error texts, and three gated pending rows sitting behind three
// different encoding states. It returns nothing — the shape itself is the
// fixture, and each test asserts the slice it cares about.
func seedEmbeddingDiagnostics(t *testing.T, store *catalog.Store) {
	t.Helper()
	ctx := context.Background()

	// Five blobs go through the claim path together: two stay processing
	// under an expired claim lease (one gets renewed into the future), three
	// take failed outcomes with their error texts.
	for _, hash := range []string{"hash-live", "hash-expired", "hash-boom-a", "hash-boom-b", "hash-disk"} {
		if err := store.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert %s: %v", hash, err)
		}
	}
	claimed, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(-time.Minute))
	if err != nil || len(claimed) != 5 {
		t.Fatalf("claim five blobs: claimed=%v err=%v", claimed, err)
	}
	renewed, err := store.RenewEmbeddingLeases(ctx, []string{"hash-live"}, time.Now().Add(5*time.Minute))
	if err != nil || len(renewed) != 1 || renewed[0] != "hash-live" {
		t.Fatalf("renew hash-live: renewed=%v err=%v", renewed, err)
	}
	applied, rejected, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-boom-a", Status: catalog.EmbeddingStatusFailed, Error: "boom"},
		{Hash: "hash-boom-b", Status: catalog.EmbeddingStatusFailed, Error: "boom"},
		{Hash: "hash-disk", Status: catalog.EmbeddingStatusFailed, Error: "disk full"},
	})
	if err != nil || len(applied) != 3 || len(rejected) != 0 {
		t.Fatalf("apply failed outcomes: applied=%v rejected=%v err=%v", applied, rejected, err)
	}

	// Three gated pending rows behind different encoding states. The big
	// sizes steer ClaimEncoding (largest first) at exactly the rows this
	// fixture needs, one claim each: a negative TTL hands back an
	// already-expired lease (the stuck shape the dashboard diagnoses), while
	// the received row needs a live lease to hand off through.
	gated := []catalog.Blob{
		{Hash: "hash-gate-received", SizeBytes: 100, ContentType: "image/jpeg", EncodingGate: true},
		{Hash: "hash-gate-leased", SizeBytes: 90, ContentType: "image/jpeg", EncodingGate: true},
		{Hash: "hash-gate-pending", SizeBytes: 80, ContentType: "image/jpeg", EncodingGate: true},
	}
	for _, blob := range gated {
		if err := store.UpsertBlob(ctx, blob); err != nil {
			t.Fatalf("upsert %s: %v", blob.Hash, err)
		}
	}
	// The received row is claimed and handed off while its lease is live
	// (MarkEncodingReceived rejects an expired one); the expired-lease row is
	// claimed last with a negative TTL, because any later ClaimEncoding call
	// would sweep that expired lease back to pending and re-claim it.
	liveJob, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(liveJob) != 1 || liveJob[0].Hash != "hash-gate-received" {
		t.Fatalf("claim live encoding: jobs=%v err=%v", liveJob, err)
	}
	if ok, err := store.MarkEncodingReceived(ctx, "hash-gate-received", liveJob[0].Token); err != nil || !ok {
		t.Fatalf("mark hash-gate-received received: ok=%v err=%v", ok, err)
	}
	expiredJob, err := store.ClaimEncoding(ctx, 1, -time.Minute)
	if err != nil || len(expiredJob) != 1 || expiredJob[0].Hash != "hash-gate-leased" {
		t.Fatalf("claim expired encoding: jobs=%v err=%v", expiredJob, err)
	}
}

func TestStatsEmbeddingDiagnostics(t *testing.T) {
	store := openTestCatalog(t)
	seedEmbeddingDiagnostics(t, store)
	ctx := context.Background()

	service := NewService(store, nil, nil)
	stats, err := service.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}

	// One live lease, one expired: the mixed picture, not the "everything
	// expired" stall signature.
	wantLease := catalog.EmbeddingLeaseHealth{Processing: 2, LeaseExpired: 1, OldestLeaseUnixMs: stats.EmbeddingLease.OldestLeaseUnixMs}
	if stats.EmbeddingLease != wantLease {
		t.Fatalf("embeddingLease=%+v want=%+v", stats.EmbeddingLease, wantLease)
	}
	if stats.EmbeddingLease.OldestLeaseUnixMs <= 0 {
		t.Fatalf("oldestLeaseUnixMs=%d want a positive timestamp (two processing rows hold leases)", stats.EmbeddingLease.OldestLeaseUnixMs)
	}

	wantBreakdown := map[string]int64{"pending": 1, "leased": 1, "received": 1}
	if len(stats.PendingByEncoding) != len(wantBreakdown) {
		t.Fatalf("pendingByEncoding=%v want=%v", stats.PendingByEncoding, wantBreakdown)
	}
	for status, want := range wantBreakdown {
		if stats.PendingByEncoding[status] != want {
			t.Fatalf("pendingByEncoding[%s]=%d want=%d", status, stats.PendingByEncoding[status], want)
		}
	}

	wantFailures := []catalog.EmbeddingFailure{
		{Error: "boom", Count: 2, SampleHash: "hash-boom-a"},
		{Error: "disk full", Count: 1, SampleHash: "hash-disk"},
	}
	if len(stats.Failures) != len(wantFailures) {
		t.Fatalf("failures=%+v want=%+v", stats.Failures, wantFailures)
	}
	for i, want := range wantFailures {
		if stats.Failures[i] != want {
			t.Fatalf("failures[%d]=%+v want=%+v", i, stats.Failures[i], want)
		}
	}

	// No recommend service wired: the activity must read as all zeros —
	// "never attempted", not a fabricated timestamp.
	if stats.WorkerActivity != (recommend.WorkerActivity{}) {
		t.Fatalf("workerActivity=%+v want zeros (nil recommend service)", stats.WorkerActivity)
	}

	// An idle recommend service records no attempts either; the field is
	// present with the same zero shape rather than being omitted.
	idle := NewService(store, nil, recommend.NewService(store, nil, nil, nil, nil))
	idleStats, err := idle.Stats(ctx)
	if err != nil {
		t.Fatalf("stats with idle recommend service: %v", err)
	}
	if idleStats.WorkerActivity != (recommend.WorkerActivity{}) {
		t.Fatalf("idle workerActivity=%+v want zeros", idleStats.WorkerActivity)
	}
}

func TestReleaseStuckEmbeddingsRespectsExpiredOnly(t *testing.T) {
	store := openTestCatalog(t)
	ctx := context.Background()

	// Two processing rows: hash-live holds a renewed live lease, hash-expired
	// keeps the already-expired claim lease.
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-live", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-live: %v", err)
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-expired", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-expired: %v", err)
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("claim both blobs: %v", err)
	}
	if renewed, err := store.RenewEmbeddingLeases(ctx, []string{"hash-live"}, time.Now().Add(5*time.Minute)); err != nil || len(renewed) != 1 {
		t.Fatalf("renew hash-live: renewed=%v err=%v", renewed, err)
	}

	service := NewService(store, nil, nil)
	result, err := service.ReleaseStuckEmbeddings(ctx, true)
	if err != nil {
		t.Fatalf("release expired only: %v", err)
	}
	// The live lease is untouched; only the expired one moves. Pending is
	// derived as Total-Ready-Failed, so the surviving processing row still
	// counts toward it.
	want := catalog.EmbeddingCounts{Total: 2, Processing: 1, Pending: 2}
	if result.Released != 1 || result.Counts != want {
		t.Fatalf("expired-only result={%d %+v} want released=1 counts=%+v", result.Released, result.Counts, want)
	}

	forced, err := service.ReleaseStuckEmbeddings(ctx, false)
	if err != nil {
		t.Fatalf("force release: %v", err)
	}
	wantAll := catalog.EmbeddingCounts{Total: 2, Pending: 2}
	if forced.Released != 1 || forced.Counts != wantAll {
		t.Fatalf("forced result={%d %+v} want released=1 counts=%+v", forced.Released, forced.Counts, wantAll)
	}

	fresh, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("fresh embedding counts: %v", err)
	}
	if fresh != wantAll {
		t.Fatalf("fresh embedding counts=%+v want=%+v", fresh, wantAll)
	}
}

func TestRetryFailedEmbeddingsResetsOnlyFailed(t *testing.T) {
	store := openTestCatalog(t)
	ctx := context.Background()

	// One ready and one failed blob through the public claim→apply path; the
	// retry must move exactly the failed one.
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-ready", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-ready: %v", err)
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-failed", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-failed: %v", err)
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("claim both blobs: %v", err)
	}
	applied, rejected, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-ready", Status: catalog.EmbeddingStatusReady, Vector: make([]float32, catalog.EmbeddingDim)},
		{Hash: "hash-failed", Status: catalog.EmbeddingStatusFailed, Error: "boom"},
	})
	if err != nil || len(applied) != 2 || len(rejected) != 0 {
		t.Fatalf("apply outcomes: applied=%v rejected=%v err=%v", applied, rejected, err)
	}

	service := NewService(store, nil, nil)
	result, err := service.RetryFailedEmbeddings(ctx)
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	// The ready row is terminal success — re-embedding it would re-spend
	// worker capacity on finished work — so only the failed row moves.
	want := catalog.EmbeddingCounts{Total: 2, Ready: 1, Pending: 1}
	if result.Reset != 1 || result.Counts != want {
		t.Fatalf("result={%d %+v} want reset=1 counts=%+v", result.Reset, result.Counts, want)
	}

	fresh, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("fresh embedding counts: %v", err)
	}
	if fresh != want {
		t.Fatalf("fresh embedding counts=%+v want=%+v", fresh, want)
	}
}

func TestResetStuckEncodingsLeavesCommittingRowsAlone(t *testing.T) {
	store := openTestCatalog(t)
	ctx := context.Background()

	// Three encoding rows: an expired lease, a received result, and a
	// committing row whose S3 replacement may be in flight. The sizes steer
	// ClaimEncoding (largest first) at exactly one row per claim.
	rows := []catalog.Blob{
		{Hash: "hash-received", SizeBytes: 100, ContentType: "image/jpeg"},
		{Hash: "hash-committing", SizeBytes: 90, ContentType: "image/jpeg"},
		{Hash: "hash-leased", SizeBytes: 80, ContentType: "image/jpeg"},
	}
	for _, blob := range rows {
		if err := store.UpsertBlob(ctx, blob); err != nil {
			t.Fatalf("upsert %s: %v", blob.Hash, err)
		}
	}
	// Same ordering as the diagnostics fixture: the received row is claimed
	// and handed off on a live lease, the committing row is claimed and
	// promoted to committing, and the expired lease is claimed last — any
	// later ClaimEncoding call would sweep it back to pending.
	liveJob, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(liveJob) != 1 || liveJob[0].Hash != "hash-received" {
		t.Fatalf("claim live encoding: jobs=%v err=%v", liveJob, err)
	}
	if ok, err := store.MarkEncodingReceived(ctx, "hash-received", liveJob[0].Token); err != nil || !ok {
		t.Fatalf("mark hash-received received: ok=%v err=%v", ok, err)
	}
	committer, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(committer) != 1 || committer[0].Hash != "hash-committing" {
		t.Fatalf("claim hash-committing: jobs=%v err=%v", committer, err)
	}
	if ok, err := store.BeginEncodingCommit(ctx, "hash-committing", committer[0].Token, 80, time.Minute); err != nil || !ok {
		t.Fatalf("begin commit: ok=%v err=%v", ok, err)
	}
	if _, err := store.ClaimEncoding(ctx, 1, -time.Minute); err != nil {
		t.Fatalf("claim expired encoding: %v", err)
	}

	service := NewService(store, nil, nil).WithEncodingEnabled(true)
	result, err := service.ResetStuckEncodings(ctx)
	if err != nil {
		t.Fatalf("reset stuck encodings: %v", err)
	}
	wantCounts := catalog.EncodingResetCounts{ReleasedExpiredLeased: 1, ResetReceived: 1}
	if result.Counts != wantCounts {
		t.Fatalf("counts=%+v want=%+v", result.Counts, wantCounts)
	}
	// The two stuck rows drain back to pending; the committing row keeps
	// counting as processing because the reset never touched it.
	if !result.Encoding.Enabled || result.Encoding.Pending != 2 || result.Encoding.Processing != 1 {
		t.Fatalf("encoding=%+v want enabled with pending=2 processing=1", result.Encoding)
	}

	committing, err := store.CommittingEncodings(ctx)
	if err != nil || len(committing) != 1 || committing[0].Hash != "hash-committing" {
		t.Fatalf("committing after reset=%v err=%v want hash-committing untouched", committing, err)
	}
}

func TestActionNilCatalogErrors(t *testing.T) {
	service := NewService(nil, nil, nil)
	ctx := context.Background()
	if _, err := service.ReleaseStuckEmbeddings(ctx, true); err == nil {
		t.Fatalf("expected an error for a nil catalog")
	}
	if _, err := service.RetryFailedEmbeddings(ctx); err == nil {
		t.Fatalf("expected an error for a nil catalog")
	}
	if _, err := service.ResetStuckEncodings(ctx); err == nil {
		t.Fatalf("expected an error for a nil catalog")
	}
}
