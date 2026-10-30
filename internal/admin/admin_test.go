package admin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"viewer/internal/catalog"
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

// fakeVectorCounter stands in for the Qdrant client, recording how often the
// service consulted it so a test can pin whether a vector-store read happened
// at all.
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

// TestOverviewHealthDegradesWithoutFailing pins the overview's graceful
// degradation contract: a wired-but-failing vector counter reports
// "unavailable" and an unwired one reports "disabled", and neither may fail
// the request or stop the catalog measurements from rendering. A healthy
// counter reports agreement against the snapshot's expected points.
func TestOverviewHealthDegradesWithoutFailing(t *testing.T) {
	store := openTestCatalog(t)
	seedLibrary(t, store)
	if err := store.InsertPhoto(context.Background(), catalog.Photo{AlbumID: "album-a", Index: 2, Name: "dangling.jpg", Hash: "missing-blob"}); err != nil {
		t.Fatalf("insert dangling photo: %v", err)
	}
	counter := &fakeVectorCounter{err: errors.New("qdrant down")}
	service := NewService(store, counter, nil)

	overview, err := service.Overview(context.Background())
	if err != nil {
		t.Fatalf("overview with unavailable vector store: %v", err)
	}
	if counter.calls != 1 {
		t.Fatalf("overview called CountVectors %d times, want one best-effort call", counter.calls)
	}
	if overview.Library.Albums != 2 || overview.Library.Photos != 4 || overview.Library.Blobs != 2 {
		t.Fatalf("overview library=%+v want albums=2 photos=4 blobs=2", overview.Library)
	}
	if overview.MeasuredAt == "" {
		t.Fatalf("overview timestamp contract=%+v", overview)
	}
	if overview.Health.VectorCount.Status != "unavailable" || overview.Health.VectorCount.ActualPoints != nil || overview.Health.VectorCount.CountAgreement != nil {
		t.Fatalf("unavailable vector count=%+v; zeros must not look measured", overview.Health.VectorCount)
	}
	if overview.Health.VectorCount.Message == "" || strings.Contains(overview.Health.VectorCount.Message, "qdrant down") {
		t.Fatalf("unavailable vector count message must be present and sanitized: %q", overview.Health.VectorCount.Message)
	}
	if !overview.Health.VectorStoreEnabled || overview.Health.ModelEnabled {
		t.Fatalf("health flags=%+v want vectorStoreEnabled=true modelEnabled=false", overview.Health)
	}
	// The catalog health numbers still come through with the vector store down.
	if overview.Health.MissingBlobPhotos != 1 || overview.Health.UnreferencedBlobs != 0 || overview.Health.VectorCount.ExpectedPoints != 2 {
		t.Fatalf("health counts=%+v want missingBlobPhotos=1 unreferencedBlobs=0 expectedPoints=2", overview.Health)
	}

	// A service without a vector counter reports "disabled" — not an error,
	// and not a zero that could be mistaken for a measured count.
	disabled := NewService(store, nil, nil)
	disabledOverview, err := disabled.Overview(context.Background())
	if err != nil {
		t.Fatalf("overview without vector store: %v", err)
	}
	if disabledOverview.Health.VectorCount.Status != "disabled" || disabledOverview.Health.VectorCount.ActualPoints != nil || disabledOverview.Health.VectorCount.CountAgreement != nil {
		t.Fatalf("disabled vector count=%+v; unmeasured fields must stay null", disabledOverview.Health.VectorCount)
	}
	if disabledOverview.Health.VectorStoreEnabled {
		t.Fatalf("disabled vector store must not report enabled: %+v", disabledOverview.Health)
	}

	// A healthy counter reports the agreement verdict against the snapshot.
	agreeing := &fakeVectorCounter{points: 2}
	available, err := NewService(store, agreeing, nil).Overview(context.Background())
	if err != nil {
		t.Fatalf("overview with healthy vector store: %v", err)
	}
	count := available.Health.VectorCount
	if count.Status != "available" || count.ActualPoints == nil || *count.ActualPoints != 2 || count.CountAgreement == nil || !*count.CountAgreement || count.MeasuredAt == nil {
		t.Fatalf("available vector count=%+v want measured agreement with expectedPoints=2", count)
	}
}

func TestAdminBrowseKeysetCursorsAreStableAndFilterBound(t *testing.T) {
	store := openTestCatalog(t)
	ctx := context.Background()
	for _, album := range []catalog.Album{
		{ID: "album-a", OriginalFilename: "a.zip", Status: catalog.AlbumStatusReady},
		{ID: "album-b", OriginalFilename: "b.zip", Status: catalog.AlbumStatusFailed},
		{ID: "album-c", OriginalFilename: "c.zip", Status: catalog.AlbumStatusReady},
	} {
		if err := store.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("create album: %v", err)
		}
	}
	for i := 0; i < 4; i++ {
		hash := fmt.Sprintf("hash-%d", i)
		if err := store.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(i + 1)}); err != nil {
			t.Fatalf("upsert blob: %v", err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := store.InsertPhoto(ctx, catalog.Photo{AlbumID: "album-a", Index: i, Name: fmt.Sprintf("a-%d.jpg", i), Hash: fmt.Sprintf("hash-%d", i)}); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}
	if err := store.InsertPhoto(ctx, catalog.Photo{AlbumID: "album-c", Index: 0, Name: "c.jpg", Hash: "hash-3"}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}
	service := NewService(store, nil, nil)

	photoPage, err := service.ListPhotos(ctx, PhotoFilters{AlbumID: "album-a"}, BrowsePageOptions{Limit: 2})
	if err != nil || len(photoPage.Photos) != 2 || photoPage.NextCursor == nil {
		t.Fatalf("first photo page=%+v err=%v", photoPage, err)
	}
	if photoPage.Photos[0].Index != 0 || photoPage.Photos[1].Index != 1 {
		t.Fatalf("first photo page order=%v want indexes 0,1", []int{photoPage.Photos[0].Index, photoPage.Photos[1].Index})
	}
	if _, err := base64.RawURLEncoding.DecodeString(*photoPage.NextCursor); err != nil {
		t.Fatalf("photo cursor is not URL-safe base64: %q (%v)", *photoPage.NextCursor, err)
	}
	secondPhotoPage, err := service.ListPhotos(ctx, PhotoFilters{AlbumID: "album-a"}, BrowsePageOptions{Limit: 2, Cursor: *photoPage.NextCursor})
	if err != nil || len(secondPhotoPage.Photos) != 1 || secondPhotoPage.NextCursor != nil || secondPhotoPage.Photos[0].Index != 2 {
		t.Fatalf("second photo page=%+v err=%v", secondPhotoPage, err)
	}
	if _, err := service.ListPhotos(ctx, PhotoFilters{AlbumID: "album-c"}, BrowsePageOptions{Limit: 2, Cursor: *photoPage.NextCursor}); !errors.Is(err, ErrInvalidBrowseRequest) {
		t.Fatalf("photo cursor accepted different filters: %v", err)
	}
	if _, err := service.ListPhotos(ctx, PhotoFilters{AlbumID: "album-a"}, BrowsePageOptions{Limit: 1, Cursor: *photoPage.NextCursor}); !errors.Is(err, ErrInvalidBrowseRequest) {
		t.Fatalf("photo cursor accepted different limit: %v", err)
	}

	albumPage, err := service.ListAlbums(ctx, AlbumFilters{Status: string(catalog.AlbumStatusReady)}, BrowsePageOptions{Limit: 1})
	if err != nil || len(albumPage.Albums) != 1 || albumPage.Albums[0].ID != "album-a" || albumPage.NextCursor == nil {
		t.Fatalf("first album page=%+v err=%v", albumPage, err)
	}
	secondAlbumPage, err := service.ListAlbums(ctx, AlbumFilters{Status: string(catalog.AlbumStatusReady)}, BrowsePageOptions{Limit: 1, Cursor: *albumPage.NextCursor})
	if err != nil || len(secondAlbumPage.Albums) != 1 || secondAlbumPage.Albums[0].ID != "album-c" || secondAlbumPage.NextCursor != nil {
		t.Fatalf("second album page=%+v err=%v", secondAlbumPage, err)
	}

	blobPage, err := service.ListBlobs(ctx, BlobFilters{}, BrowsePageOptions{Limit: 2})
	if err != nil || len(blobPage.Blobs) != 2 || blobPage.NextCursor == nil {
		t.Fatalf("first blob page=%+v err=%v", blobPage, err)
	}
	secondBlobPage, err := service.ListBlobs(ctx, BlobFilters{}, BrowsePageOptions{Limit: 2, Cursor: *blobPage.NextCursor})
	if err != nil || len(secondBlobPage.Blobs) != 2 || secondBlobPage.NextCursor != nil {
		t.Fatalf("second blob page=%+v err=%v", secondBlobPage, err)
	}
	seenHashes := map[string]bool{}
	for _, blob := range append(blobPage.Blobs, secondBlobPage.Blobs...) {
		if seenHashes[blob.Hash] {
			t.Fatalf("blob repeated across pages: %s", blob.Hash)
		}
		seenHashes[blob.Hash] = true
	}
	if len(seenHashes) != 4 {
		t.Fatalf("blob pages yielded %d unique hashes, want 4: %v", len(seenHashes), seenHashes)
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
	result, err := service.ReleaseStuckEmbeddings(ctx, true, nil)
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

	forced, err := service.ReleaseStuckEmbeddings(ctx, false, nil)
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
	result, err := service.RetryFailedEmbeddings(ctx, nil)
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
	// Steer ClaimEncoding (largest first) at exactly one row per claim: the
	// received row is claimed and handed off on a live lease, the committing
	// row is claimed and promoted to committing, and the expired lease is
	// claimed last — any later ClaimEncoding call would sweep it back to
	// pending.
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
	result, err := service.ResetStuckEncodings(ctx, nil)
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

// TestRetryFailedEncodingsResetsOnlyFailed mirrors the embedding retry test for
// the WebP queue: the failed conversion goes back to pending while a skipped
// one (a deliberate not-smaller outcome) stays terminal, and the response
// carries the fresh encoding stats.
func TestRetryFailedEncodingsResetsOnlyFailed(t *testing.T) {
	store := openTestCatalog(t)
	ctx := context.Background()

	// One failed and one not-smaller conversion through the public claim→skip
	// path; the retry must move exactly the failed one.
	for _, blob := range []catalog.Blob{
		{Hash: "hash-failed", SizeBytes: 100, ContentType: "image/jpeg"},
		{Hash: "hash-not-smaller", SizeBytes: 90, ContentType: "image/jpeg"},
	} {
		if err := store.UpsertBlob(ctx, blob); err != nil {
			t.Fatalf("upsert %s: %v", blob.Hash, err)
		}
	}
	jobs, err := store.ClaimEncoding(ctx, 2, time.Minute)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("claim encodings: jobs=%v err=%v", jobs, err)
	}
	byHash := make(map[string]catalog.EncodingJob, len(jobs))
	for _, job := range jobs {
		byHash[job.Hash] = job
	}
	if ok, err := store.SkipEncoding(ctx, "hash-failed", byHash["hash-failed"].Token, "failed", "image/jpeg", "boom", 100); err != nil || !ok {
		t.Fatalf("fail hash-failed: ok=%v err=%v", ok, err)
	}
	if ok, err := store.SkipEncoding(ctx, "hash-not-smaller", byHash["hash-not-smaller"].Token, "skipped", "image/jpeg", "", 90); err != nil || !ok {
		t.Fatalf("skip hash-not-smaller: ok=%v err=%v", ok, err)
	}

	service := NewService(store, nil, nil).WithEncodingEnabled(true)
	result, err := service.RetryFailedEncodings(ctx, nil)
	if err != nil {
		t.Fatalf("retry failed encodings: %v", err)
	}
	// Only the failed row returns to the queue; the not-smaller row is a
	// terminal decision re-encoding would only reproduce.
	if result.Reset != 1 || !result.Encoding.Enabled || result.Encoding.Pending != 1 || result.Encoding.Failed != 0 || result.Encoding.NotSmaller != 1 {
		t.Fatalf("result={%d %+v} want reset=1 enabled pending=1 failed=0 notSmaller=1", result.Reset, result.Encoding)
	}

	fresh, err := store.EncodingCounts(ctx)
	if err != nil {
		t.Fatalf("fresh encoding counts: %v", err)
	}
	if fresh.Pending != 1 || fresh.Failed != 0 || fresh.NotSmaller != 1 {
		t.Fatalf("fresh encoding counts=%+v want pending=1 failed=0 notSmaller=1", fresh)
	}
}

func TestActionNilCatalogErrors(t *testing.T) {
	service := NewService(nil, nil, nil)
	ctx := context.Background()
	if _, err := service.ReleaseStuckEmbeddings(ctx, true, nil); err == nil {
		t.Fatalf("expected an error for a nil catalog")
	}
	if _, err := service.RetryFailedEmbeddings(ctx, nil); err == nil {
		t.Fatalf("expected an error for a nil catalog")
	}
	if _, err := service.ResetStuckEncodings(ctx, nil); err == nil {
		t.Fatalf("expected an error for a nil catalog")
	}
	if _, err := service.RetryFailedEncodings(ctx, nil); err == nil {
		t.Fatalf("expected an error for a nil catalog")
	}
}
