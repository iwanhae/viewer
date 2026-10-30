package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/qdrant"
)

// stubSyncStore stands in for the Qdrant client's ID scroll. block (when
// non-nil) holds the scroll open and entered closes once the scroll was
// reached — together they let the TryLock and detached-context tests pin a
// pass mid-flight deterministically.
type stubSyncStore struct {
	ids     []string
	block   <-chan struct{}
	entered chan struct{}
}

func (s *stubSyncStore) ScrollPointIDs(ctx context.Context) ([]string, error) {
	if s.entered != nil {
		close(s.entered)
	}
	if s.block != nil {
		<-s.block
	}
	return s.ids, nil
}

// seedSyncFixture builds the lattice one reconciliation pass reasons about:
// two albums, six photos, six blobs covering every embedding status. The
// terminal outcomes go through the public claim→apply path; hash-proc stays
// claimed under a live lease (mid-embed), hash-pend is never claimed.
//
//	album-a/0 hash-ready-a  ready       (point present)
//	album-a/1 hash-missing  ready       (point missing → resets)
//	album-a/2 hash-proc     processing  (point missing → skipped)
//	album-a/3 hash-fail     failed      (point missing → counted only)
//	album-a/4 hash-pend     pending     (point missing → expected)
//	album-b/0 hash-ready-b  ready       (point present)
func seedSyncFixture(t *testing.T, store *catalog.Store) {
	t.Helper()
	ctx := context.Background()
	for _, album := range []catalog.Album{
		{ID: "album-a", OriginalFilename: "a.zip", Status: catalog.AlbumStatusReady},
		{ID: "album-b", OriginalFilename: "b.zip", Status: catalog.AlbumStatusReady},
	} {
		if err := store.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("create album: %v", err)
		}
	}
	for _, photo := range []catalog.Photo{
		{AlbumID: "album-a", Index: 0, Name: "ready-a.jpg", Hash: "hash-ready-a", Width: 4, Height: 4, Ratio: 1},
		{AlbumID: "album-a", Index: 1, Name: "missing.jpg", Hash: "hash-missing", Width: 4, Height: 4, Ratio: 1},
		{AlbumID: "album-a", Index: 2, Name: "processing.jpg", Hash: "hash-proc", Width: 4, Height: 4, Ratio: 1},
		{AlbumID: "album-a", Index: 3, Name: "failed.jpg", Hash: "hash-fail", Width: 4, Height: 4, Ratio: 1},
		{AlbumID: "album-a", Index: 4, Name: "pending.jpg", Hash: "hash-pend", Width: 4, Height: 4, Ratio: 1},
		{AlbumID: "album-b", Index: 0, Name: "ready-b.jpg", Hash: "hash-ready-b", Width: 4, Height: 4, Ratio: 1},
	} {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}
	for _, hash := range []string{"hash-ready-a", "hash-ready-b", "hash-missing", "hash-fail"} {
		if err := store.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert %s: %v", hash, err)
		}
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("claim terminal blobs: %v", err)
	}
	applied, rejected, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-ready-a", Status: catalog.EmbeddingStatusReady, Vector: make([]float32, catalog.EmbeddingDim)},
		{Hash: "hash-ready-b", Status: catalog.EmbeddingStatusReady, Vector: make([]float32, catalog.EmbeddingDim)},
		{Hash: "hash-missing", Status: catalog.EmbeddingStatusReady, Vector: make([]float32, catalog.EmbeddingDim)},
		{Hash: "hash-fail", Status: catalog.EmbeddingStatusFailed, Error: "boom"},
	})
	if err != nil || len(applied) != 4 || len(rejected) != 0 {
		t.Fatalf("apply terminal outcomes: applied=%v rejected=%v err=%v", applied, rejected, err)
	}
	// Upserted after the apply and claimed on a live lease, so hash-proc is
	// genuinely mid-embed; hash-pend is upserted last and never claimed.
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-proc", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-proc: %v", err)
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatalf("claim hash-proc: %v", err)
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-pend", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-pend: %v", err)
	}
}

// blobEmbeddingStatus reads one blob's embedding status through the public
// pair query, so a test asserts the catalog row itself, not just the
// response counts.
func blobEmbeddingStatus(t *testing.T, store *catalog.Store, hash string) catalog.EmbeddingStatus {
	t.Helper()
	pairs, err := store.ListPhotoBlobPairsByHashes(context.Background(), []string{hash})
	if err != nil {
		t.Fatalf("list pairs for %s: %v", hash, err)
	}
	if len(pairs) != 1 {
		t.Fatalf("pairs for %s=%d want exactly one", hash, len(pairs))
	}
	return pairs[0].Blob.EmbeddingStatus
}

// TestSyncVectorsReconcilesMissingPoints drives the full reconciliation over
// the seeded lattice: the store holds every point except the missing ready
// one, the processing one and the failed one, plus one ID no photo owns.
// Exactly the ready-missing blob flips to pending; the processing blob
// keeps its lease, the failed blob keeps its failure, the pending blob is
// not expected to have a point, and the orphan is only counted.
func TestSyncVectorsReconcilesMissingPoints(t *testing.T) {
	store := openTestCatalog(t)
	seedSyncFixture(t, store)
	scroller := &stubSyncStore{ids: []string{
		qdrant.PointID("album-a", 0),
		qdrant.PointID("album-b", 0),
		"orphan-point-no-photo-owns",
	}}
	service := NewService(store, nil, nil).WithVectorSync(context.Background(), scroller)

	result, err := service.SyncVectors(context.Background())
	if err != nil {
		t.Fatalf("sync vectors: %v", err)
	}
	if !result.Started || result.MissingPoints != 1 || result.ResetBlobs != 1 ||
		result.SkippedProcessing != 1 || result.FailedWithoutPoint != 1 || result.OrphanPoints != 1 {
		t.Fatalf("result=%+v want started with 1 missing/reset/skipped/failed/orphan each", result)
	}
	// The fresh counts: hash-missing left ready, so ready drops 3→2 and
	// pending (derived as total-ready-failed) rises 2→3.
	want := catalog.EmbeddingCounts{Total: 6, Ready: 2, Failed: 1, Processing: 1, Pending: 3}
	if result.Counts != want {
		t.Fatalf("result counts=%+v want=%+v", result.Counts, want)
	}
	if result.DurationMs < 0 {
		t.Fatalf("durationMs=%d must not be negative", result.DurationMs)
	}
	fresh, err := store.EmbeddingCounts(context.Background())
	if err != nil {
		t.Fatalf("fresh embedding counts: %v", err)
	}
	if fresh != want {
		t.Fatalf("fresh embedding counts=%+v want=%+v", fresh, want)
	}
	// The catalog rows behind the verdicts: only the ready-missing blob
	// moved; the lease, the failure and the never-claimed row survive, and
	// the orphan changed nothing anywhere (it owns no catalog row).
	if got := blobEmbeddingStatus(t, store, "hash-missing"); got != catalog.EmbeddingStatusPending {
		t.Fatalf("hash-missing status=%q want pending after the reset", got)
	}
	if got := blobEmbeddingStatus(t, store, "hash-proc"); got != catalog.EmbeddingStatusProcessing {
		t.Fatalf("hash-proc status=%q want processing kept (the lease owns it)", got)
	}
	if got := blobEmbeddingStatus(t, store, "hash-fail"); got != catalog.EmbeddingStatusFailed {
		t.Fatalf("hash-fail status=%q want failed kept (the retry path owns it)", got)
	}
	if got := blobEmbeddingStatus(t, store, "hash-pend"); got != catalog.EmbeddingStatusPending {
		t.Fatalf("hash-pend status=%q want pending kept", got)
	}
}

// TestSyncVectorsDedupesSharedHashes pins the dedup contract: two photos
// sharing one blob both lack their points, both count as missing, but the
// reset flips the one shared blob once — re-embedding it re-creates both
// points.
func TestSyncVectorsDedupesSharedHashes(t *testing.T) {
	store := openTestCatalog(t)
	ctx := context.Background()
	for _, album := range []catalog.Album{
		{ID: "album-a", OriginalFilename: "a.zip", Status: catalog.AlbumStatusReady},
		{ID: "album-b", OriginalFilename: "b.zip", Status: catalog.AlbumStatusReady},
	} {
		if err := store.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("create album: %v", err)
		}
	}
	for _, photo := range []catalog.Photo{
		{AlbumID: "album-a", Index: 0, Name: "shared.jpg", Hash: "hash-shared", Width: 4, Height: 4, Ratio: 1},
		{AlbumID: "album-b", Index: 0, Name: "shared.jpg", Hash: "hash-shared", Width: 4, Height: 4, Ratio: 1},
	} {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-shared", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-shared: %v", err)
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("claim hash-shared: %v", err)
	}
	if _, _, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-shared", Status: catalog.EmbeddingStatusReady, Vector: make([]float32, catalog.EmbeddingDim)},
	}); err != nil {
		t.Fatalf("apply hash-shared: %v", err)
	}
	service := NewService(store, nil, nil).WithVectorSync(ctx, &stubSyncStore{})

	result, err := service.SyncVectors(ctx)
	if err != nil {
		t.Fatalf("sync vectors: %v", err)
	}
	if !result.Started || result.MissingPoints != 2 || result.ResetBlobs != 1 ||
		result.SkippedProcessing != 0 || result.FailedWithoutPoint != 0 || result.OrphanPoints != 0 {
		t.Fatalf("result=%+v want started missingPoints=2 resetBlobs=1 and nothing else", result)
	}
	want := catalog.EmbeddingCounts{Total: 1, Pending: 1}
	if result.Counts != want {
		t.Fatalf("counts=%+v want=%+v", result.Counts, want)
	}
}

// TestSyncVectorsSingleFlight pins the TryLock contract: a pass held open
// inside the scroll makes an immediate second call a Started=false no-op
// (never an error, never a queue), and the held pass still completes with
// its own verdicts once released.
func TestSyncVectorsSingleFlight(t *testing.T) {
	store := openTestCatalog(t)
	seedSyncFixture(t, store)
	entered := make(chan struct{})
	block := make(chan struct{})
	scroller := &stubSyncStore{
		ids:     []string{qdrant.PointID("album-a", 0), qdrant.PointID("album-b", 0)},
		block:   block,
		entered: entered,
	}
	service := NewService(store, nil, nil).WithVectorSync(context.Background(), scroller)

	done := make(chan VectorSyncResult, 1)
	failed := make(chan error, 1)
	go func() {
		result, err := service.SyncVectors(context.Background())
		if err != nil {
			failed <- err
			return
		}
		done <- result
	}()
	<-entered // the first pass now holds the single-flight lock mid-scroll
	second, err := service.SyncVectors(context.Background())
	if err != nil {
		t.Fatalf("second sync while one runs: %v", err)
	}
	if second.Started {
		t.Fatalf("second sync started=%t want false while the first is running", second.Started)
	}
	close(block)
	select {
	case result := <-done:
		if !result.Started || result.MissingPoints != 1 || result.ResetBlobs != 1 ||
			result.SkippedProcessing != 1 || result.FailedWithoutPoint != 1 || result.OrphanPoints != 0 {
			t.Fatalf("first pass result=%+v want started with 1 missing/reset/skipped/failed", result)
		}
	case err := <-failed:
		t.Fatalf("first pass: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first pass never finished after the scroll was released")
	}
}

// TestSyncVectorsOutlivesCallerContext proves the work context is the
// service lifetime, not the caller's: the caller cancels while the pass is
// held mid-scroll, and the pass still completes — diff, reset and fresh
// counts all land.
func TestSyncVectorsOutlivesCallerContext(t *testing.T) {
	store := openTestCatalog(t)
	seedSyncFixture(t, store)
	entered := make(chan struct{})
	block := make(chan struct{})
	scroller := &stubSyncStore{
		ids:     []string{qdrant.PointID("album-a", 0), qdrant.PointID("album-b", 0)},
		block:   block,
		entered: entered,
	}
	service := NewService(store, nil, nil).WithVectorSync(context.Background(), scroller)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan VectorSyncResult, 1)
	failed := make(chan error, 1)
	go func() {
		result, err := service.SyncVectors(ctx)
		if err != nil {
			failed <- err
			return
		}
		done <- result
	}()
	<-entered
	cancel() // the client hangs up mid-pass
	close(block)
	select {
	case result := <-done:
		if !result.Started || result.MissingPoints != 1 || result.ResetBlobs != 1 {
			t.Fatalf("result=%+v want the pass to finish despite the canceled caller", result)
		}
		want := catalog.EmbeddingCounts{Total: 6, Ready: 2, Failed: 1, Processing: 1, Pending: 3}
		if result.Counts != want {
			t.Fatalf("counts=%+v want=%+v (the reset really landed)", result.Counts, want)
		}
	case err := <-failed:
		t.Fatalf("sync after caller cancel: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("sync never finished after the caller canceled")
	}
}

// TestSyncVectorsRequiresStore pins the unwired shape: a service built
// without WithVectorSync answers with the sentinel, never a panic, and a
// nil catalog keeps the shared error the other actions use.
func TestSyncVectorsRequiresStore(t *testing.T) {
	store := openTestCatalog(t)
	if _, err := NewService(store, nil, nil).SyncVectors(context.Background()); !errors.Is(err, ErrVectorSyncUnavailable) {
		t.Fatalf("err=%v want ErrVectorSyncUnavailable", err)
	}
	if _, err := NewService(nil, nil, nil).WithVectorSync(context.Background(), &stubSyncStore{}).SyncVectors(context.Background()); err == nil {
		t.Fatal("expected an error for a nil catalog")
	}
}

// TestSyncVectorsCleanPassChangesNothing pins the no-drift outcome: the
// store holding exactly the expected IDs answers zero everywhere and no
// blob status moves — including the failed and processing photos whose
// points exist (the upload-before-flip window).
func TestSyncVectorsCleanPassChangesNothing(t *testing.T) {
	store := openTestCatalog(t)
	seedSyncFixture(t, store)
	before, err := store.EmbeddingCounts(context.Background())
	if err != nil {
		t.Fatalf("counts before sync: %v", err)
	}
	scroller := &stubSyncStore{ids: []string{
		qdrant.PointID("album-a", 0),
		qdrant.PointID("album-a", 1),
		qdrant.PointID("album-a", 2),
		qdrant.PointID("album-a", 3),
		qdrant.PointID("album-a", 4),
		qdrant.PointID("album-b", 0),
	}}
	service := NewService(store, nil, nil).WithVectorSync(context.Background(), scroller)

	result, err := service.SyncVectors(context.Background())
	if err != nil {
		t.Fatalf("sync vectors: %v", err)
	}
	if !result.Started || result.MissingPoints != 0 || result.ResetBlobs != 0 ||
		result.SkippedProcessing != 0 || result.FailedWithoutPoint != 0 || result.OrphanPoints != 0 {
		t.Fatalf("result=%+v want a clean zero pass", result)
	}
	if result.Counts != before {
		t.Fatalf("counts=%+v want the unchanged %+v", result.Counts, before)
	}
	after, err := store.EmbeddingCounts(context.Background())
	if err != nil {
		t.Fatalf("fresh embedding counts: %v", err)
	}
	if after != before {
		t.Fatalf("catalog counts moved on a clean pass: %+v want %+v", after, before)
	}
	if got := blobEmbeddingStatus(t, store, "hash-missing"); got != catalog.EmbeddingStatusReady {
		t.Fatalf("hash-missing status=%q want ready untouched", got)
	}
}
