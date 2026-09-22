package recommend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"viewer/internal/catalog"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

// newTestCatalog opens a real SQLite catalog in a temp dir and closes it when
// the test finishes.
func newTestCatalog(t *testing.T) *catalog.Store {
	t.Helper()
	store, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	return store
}

// newTestService builds a recommendation service over the catalog. Tests that
// only exercise the in-memory index pass a nil images service and a nil
// embedder, which switches embedding off.
func newTestService(t *testing.T, cat *catalog.Store, embedder EmbeddingProvider) *Service {
	t.Helper()
	svc := NewService(cat, nil, embedder, nil)
	if svc == nil {
		t.Fatalf("new recommend service returned nil")
	}
	return svc
}

// seedAlbum creates (or refreshes) a ready album with the given photos.
func seedAlbum(t *testing.T, cat *catalog.Store, albumID string, photos ...catalog.Photo) {
	t.Helper()
	ctx := context.Background()
	if err := cat.CreateAlbum(ctx, catalog.Album{ID: albumID, OriginalFilename: albumID + ".zip"}); err != nil {
		t.Fatalf("create album %s: %v", albumID, err)
	}
	for i := range photos {
		photos[i].AlbumID = albumID
		if err := cat.InsertPhoto(ctx, photos[i]); err != nil {
			t.Fatalf("insert photo %s:%d: %v", albumID, photos[i].Index, err)
		}
	}
	if err := cat.MarkAlbumReady(ctx, albumID, len(photos)); err != nil {
		t.Fatalf("mark album %s ready: %v", albumID, err)
	}
}

// seedBlob creates a pending blob row. Identical hashes are upserted, so the
// call is idempotent.
func seedBlob(t *testing.T, cat *catalog.Store, hash string) {
	t.Helper()
	if err := cat.UpsertBlob(context.Background(), catalog.Blob{
		Hash:        hash,
		SizeBytes:   int64(len(hash)),
		ContentType: "image/jpeg",
	}); err != nil {
		t.Fatalf("upsert blob %s: %v", hash, err)
	}
}

func seedReadyEmbedding(t *testing.T, cat *catalog.Store, hash string, vector []float32) {
	t.Helper()
	seedBlob(t, cat, hash)
	if err := cat.SetBlobEmbedding(context.Background(), hash, catalog.EmbeddingStatusReady, vector, ""); err != nil {
		t.Fatalf("set ready embedding %s: %v", hash, err)
	}
}

func seedFailedEmbedding(t *testing.T, cat *catalog.Store, hash string, errText string) {
	t.Helper()
	seedBlob(t, cat, hash)
	if err := cat.SetBlobEmbedding(context.Background(), hash, catalog.EmbeddingStatusFailed, nil, errText); err != nil {
		t.Fatalf("set failed embedding %s: %v", hash, err)
	}
}

// vec768 builds a full-length embedding whose first components are vals and
// whose remainder is zero, so tests can write short direction literals while
// satisfying the catalog's float[768] column. Cosine similarity between two
// vec768 vectors equals the cosine of the short forms they encode.
func vec768(vals ...float32) []float32 {
	vector := make([]float32, EmbeddingDim)
	copy(vector, vals)
	return vector
}

// ---------------------------------------------------------------------------
// LoadAll
// ---------------------------------------------------------------------------

func TestLoadAllBuildsIndexFromCatalogRows(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedReadyEmbedding(t, cat, "hash-a", vec768(3, 4))
	seedFailedEmbedding(t, cat, "hash-b", "embed image: boom")
	seedBlob(t, cat, "hash-pending")
	seedBlob(t, cat, "hash-orphan") // no photo references it

	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-a", Width: 10, Height: 20, Ratio: 0.5},
		catalog.Photo{Index: 1, Name: "a1.jpg", Hash: "hash-b", Width: 20, Height: 20, Ratio: 1},
	)
	seedAlbum(t, cat, "album-b",
		catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-pending", Width: 30, Height: 40, Ratio: 0.75},
	)

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// Ready embeddings live in the catalog's vector index, not in memory:
	// only the correctly-sized vector is queryable, while failed and pending
	// blobs never enter it.
	neighbors, err := cat.FindNeighborEmbeddings(ctx, vec768(3, 4), 10)
	if err != nil {
		t.Fatalf("FindNeighborEmbeddings: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].Hash != "hash-a" {
		t.Fatalf("neighbors=%+v want only hash-a", neighbors)
	}

	refs := svc.photosByHash["hash-a"]
	if len(refs) != 1 {
		t.Fatalf("hash-a refs=%d want=1", len(refs))
	}
	if refs[0].AlbumID != "album-a" || refs[0].Index != 0 || refs[0].Width != 10 || refs[0].Height != 20 || refs[0].Ratio != 0.5 {
		t.Fatalf("unexpected hash-a ref: %+v", refs[0])
	}
	if hashes := svc.hashesByAlbum["album-a"]; len(hashes) != 2 {
		t.Fatalf("album-a hashes=%d want=2", len(hashes))
	}
	if _, ok := svc.hashesByAlbum["album-a"]["hash-b"]; !ok {
		t.Fatalf("album-a reverse index missing failed blob hash")
	}

	// LoadAll is a full rebuild: re-running must not duplicate refs.
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("second LoadAll: %v", err)
	}
	if len(svc.photosByHash["hash-a"]) != 1 || len(svc.photosByHash["hash-pending"]) != 1 {
		t.Fatalf("LoadAll duplicated refs: hash-a=%d hash-pending=%d",
			len(svc.photosByHash["hash-a"]), len(svc.photosByHash["hash-pending"]))
	}
}

func TestLoadAllAndReloadAlbumWithoutCatalog(t *testing.T) {
	svc := newTestService(t, nil, nil)
	if err := svc.LoadAll(context.Background()); err != nil {
		t.Fatalf("LoadAll without catalog: %v", err)
	}
	if err := svc.ReloadAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("ReloadAlbum without catalog: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ReloadAlbum
// ---------------------------------------------------------------------------

func TestReloadAlbumAddsAndRefreshes(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedReadyEmbedding(t, cat, "hash-a0", vec768(1, 0))
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-a0", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// album-b exists in the catalog but is not in the in-memory index yet.
	seedReadyEmbedding(t, cat, "hash-b0", vec768(0, 1))
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-b0", Width: 2, Height: 2, Ratio: 1})
	if _, ok := svc.hashesByAlbum["album-b"]; ok {
		t.Fatalf("album-b indexed before ReloadAlbum")
	}

	if err := svc.ReloadAlbum(ctx, "album-b"); err != nil {
		t.Fatalf("ReloadAlbum(album-b): %v", err)
	}
	if _, ok := svc.hashesByAlbum["album-b"]["hash-b0"]; !ok {
		t.Fatalf("ReloadAlbum did not index album-b")
	}
	if refs := svc.photosByHash["hash-b0"]; len(refs) != 1 || refs[0].AlbumID != "album-b" {
		t.Fatalf("unexpected hash-b0 refs: %+v", refs)
	}

	// Refreshing an album drops stale hashes and picks up the new photo.
	seedReadyEmbedding(t, cat, "hash-a1", vec768(0, 1))
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a1.jpg", Hash: "hash-a1", Width: 3, Height: 3, Ratio: 1})

	if err := svc.ReloadAlbum(ctx, "album-a"); err != nil {
		t.Fatalf("ReloadAlbum(album-a): %v", err)
	}
	if _, ok := svc.hashesByAlbum["album-a"]["hash-a0"]; ok {
		t.Fatalf("stale hash-a0 still in album-a reverse index")
	}
	if _, ok := svc.photosByHash["hash-a0"]; ok {
		t.Fatalf("stale hash-a0 ref still in photos index")
	}
	if _, ok := svc.hashesByAlbum["album-a"]["hash-a1"]; !ok {
		t.Fatalf("new hash-a1 missing from album-a reverse index")
	}
	if refs := svc.photosByHash["hash-a1"]; len(refs) != 1 || refs[0].Index != 0 || refs[0].Width != 3 {
		t.Fatalf("unexpected hash-a1 refs: %+v", refs)
	}
	// Unrelated albums are untouched.
	if _, ok := svc.photosByHash["hash-b0"]; !ok {
		t.Fatalf("ReloadAlbum(album-a) dropped album-b entries")
	}

	// Blank album id is a no-op, and unknown albums do not error or panic.
	if err := svc.ReloadAlbum(ctx, "   "); err != nil {
		t.Fatalf("ReloadAlbum(blank): %v", err)
	}
	if err := svc.ReloadAlbum(ctx, "album-missing"); err != nil {
		t.Fatalf("ReloadAlbum(album-missing): %v", err)
	}
	if _, ok := svc.hashesByAlbum["album-missing"]; ok {
		t.Fatalf("unknown album must not be indexed")
	}
}

// ---------------------------------------------------------------------------
// Recommend
// ---------------------------------------------------------------------------

func TestRecommendOrdersCrossAlbumNeighborsByScore(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedReadyEmbedding(t, cat, "hash-query", vec768(1, 0))
	seedReadyEmbedding(t, cat, "hash-same-album", vec768(1, 0))
	seedReadyEmbedding(t, cat, "hash-b", vec768(0.9, 0.1))
	seedReadyEmbedding(t, cat, "hash-c", vec768(0, 1))

	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-query", Width: 100, Height: 200, Ratio: 0.5},
		catalog.Photo{Index: 1, Name: "a1.jpg", Hash: "hash-same-album", Width: 100, Height: 100, Ratio: 1},
	)
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-b", Width: 10, Height: 20, Ratio: 0.5})
	seedAlbum(t, cat, "album-c", catalog.Photo{Index: 0, Name: "c0.jpg", Hash: "hash-c", Width: 30, Height: 40, Ratio: 0.75})

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	resp, err := svc.Recommend(ctx, "album-a", 0, 10)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items=%d want=2: %+v", len(resp.Items), resp.Items)
	}
	if resp.Items[0].AlbumID != "album-b" || resp.Items[1].AlbumID != "album-c" {
		t.Fatalf("unexpected order: %+v", resp.Items)
	}
	if !(resp.Items[0].Score > resp.Items[1].Score) {
		t.Fatalf("expected descending scores, got %+v", resp.Items)
	}
	if resp.Items[0].I != 0 || resp.Items[0].W != 10 || resp.Items[0].H != 20 {
		t.Fatalf("unexpected album-b item: %+v", resp.Items[0])
	}
	for _, item := range resp.Items {
		if item.AlbumID == "album-a" {
			t.Fatalf("same-album item leaked: %+v", item)
		}
	}
}

func TestRecommendExcludesPhotosFromQueryAlbum(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	// The same-album twin is a perfect match, so it would rank first if the
	// album filter were missing.
	seedReadyEmbedding(t, cat, "hash-query", vec768(1, 0))
	seedReadyEmbedding(t, cat, "hash-twin", vec768(1, 0))
	seedReadyEmbedding(t, cat, "hash-other", vec768(0.5, 0.5))

	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-query", Width: 1, Height: 1, Ratio: 1},
		catalog.Photo{Index: 1, Name: "a1.jpg", Hash: "hash-twin", Width: 1, Height: 1, Ratio: 1},
	)
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-other", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	resp, err := svc.Recommend(ctx, "album-a", 0, 10)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items=%d want=1: %+v", len(resp.Items), resp.Items)
	}
	if resp.Items[0].AlbumID != "album-b" {
		t.Fatalf("expected only album-b, got %+v", resp.Items[0])
	}
}

func TestRecommendReturnsEmptyItemsWhenNoCrossAlbumNeighbors(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedReadyEmbedding(t, cat, "hash-a0", vec768(1, 0))
	seedReadyEmbedding(t, cat, "hash-a1", vec768(0.9, 0.1))
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-a0", Width: 1, Height: 1, Ratio: 1},
		catalog.Photo{Index: 1, Name: "a1.jpg", Hash: "hash-a1", Width: 1, Height: 1, Ratio: 1},
	)

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	resp, err := svc.Recommend(ctx, "album-a", 0, 12)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if resp.Items == nil {
		t.Fatalf("expected non-nil empty items list")
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations, got %d: %+v", len(resp.Items), resp.Items)
	}
}

func TestRecommendReturnsEmptyItemsWhenQueryEmbeddingPending(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedBlob(t, cat, "hash-pending")
	seedReadyEmbedding(t, cat, "hash-b", vec768(1, 0))
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-pending", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-b", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	resp, err := svc.Recommend(ctx, "album-a", 0, 12)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations for pending query, got %+v", resp.Items)
	}
}

func TestRecommendReturnsEmptyItemsWhenQueryEmbeddingFailed(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedFailedEmbedding(t, cat, "hash-failed", "embed image: boom")
	seedReadyEmbedding(t, cat, "hash-b", vec768(1, 0))
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-failed", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-b", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	resp, err := svc.Recommend(ctx, "album-a", 0, 12)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations for failed query, got %+v", resp.Items)
	}
}

// TestRecommendUnknownPhotoWrapsErrPhotoNotFound also pins that the recommend
// sentinel is distinct from the catalog one: Recommend translates
// catalog.ErrPhotoNotFound into its own error.
func TestRecommendUnknownPhotoWrapsErrPhotoNotFound(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedReadyEmbedding(t, cat, "hash-a", vec768(1, 0))
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-a", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	cases := []struct {
		name    string
		albumID string
		index   int
	}{
		{name: "unknown photo index", albumID: "album-a", index: 7},
		{name: "unknown album", albumID: "album-missing", index: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := svc.Recommend(ctx, tc.albumID, tc.index, 5)
			if err == nil {
				t.Fatalf("expected error for %s:%d", tc.albumID, tc.index)
			}
			if !errors.Is(err, ErrPhotoNotFound) {
				t.Fatalf("err=%v want errors.Is(err, ErrPhotoNotFound)", err)
			}
			if errors.Is(err, catalog.ErrPhotoNotFound) {
				t.Fatalf("err=%v should not expose catalog.ErrPhotoNotFound", err)
			}
			if resp.Items != nil {
				t.Fatalf("expected zero-value response on error, got %+v", resp)
			}
		})
	}

	// The underlying catalog reports its own sentinel for the same lookup.
	if _, err := cat.PhotoAt(ctx, "album-a", 7); !errors.Is(err, catalog.ErrPhotoNotFound) {
		t.Fatalf("catalog.PhotoAt err=%v want errors.Is(err, catalog.ErrPhotoNotFound)", err)
	}
	if _, err := cat.PhotoAt(ctx, "album-missing", 0); !errors.Is(err, catalog.ErrPhotoNotFound) {
		t.Fatalf("catalog.PhotoAt unknown album err=%v want errors.Is(err, catalog.ErrPhotoNotFound)", err)
	}
}

func TestRecommendWithoutLoadedIndexReturnsEmptyNotPanic(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedReadyEmbedding(t, cat, "hash-a", vec768(1, 0))
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-a", Width: 1, Height: 1, Ratio: 1})

	// No LoadAll: the photo-ref maps are empty even though the catalog has the
	// photo, and the query blob is the only indexed vector — excluded as the
	// query itself. The result is an empty list rather than a panic.
	svc := newTestService(t, cat, nil)
	resp, err := svc.Recommend(ctx, "album-a", 0, 12)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations, got %+v", resp.Items)
	}
}

func TestRecommendLimitClamping(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedReadyEmbedding(t, cat, "hash-query", vec768(1, 0))
	seedAlbum(t, cat, "album-query", catalog.Photo{Index: 0, Name: "q.jpg", Hash: "hash-query", Width: 1, Height: 1, Ratio: 1})

	// Enough distinct target albums to exercise the maxTopK clamp.
	targets := maxTopK + 5
	for i := 0; i < targets; i++ {
		hash := fmt.Sprintf("hash-target-%02d", i)
		// Strictly decreasing similarity to the [1, 0] query.
		seedReadyEmbedding(t, cat, hash, vec768(1-0.001*float32(i), 0.001*float32(i)))
		albumID := fmt.Sprintf("album-target-%02d", i)
		seedAlbum(t, cat, albumID, catalog.Photo{Index: 0, Name: albumID + ".jpg", Hash: hash, Width: 1, Height: 1, Ratio: 1})
	}

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "default applied when limit zero", limit: 0, want: defaultTopK},
		{name: "default applied when limit negative", limit: -3, want: defaultTopK},
		{name: "requested limit honored", limit: 1, want: 1},
		{name: "max clamps oversized limit", limit: 1000, want: maxTopK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := svc.Recommend(ctx, "album-query", 0, tc.limit)
			if err != nil {
				t.Fatalf("Recommend: %v", err)
			}
			if len(resp.Items) != tc.want {
				t.Fatalf("items=%d want=%d: %+v", len(resp.Items), tc.want, resp.Items)
			}
		})
	}

	// Results stay ordered by descending score, one item per target album.
	resp, err := svc.Recommend(ctx, "album-query", 0, maxTopK)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	for i := 1; i < len(resp.Items); i++ {
		if resp.Items[i-1].Score < resp.Items[i].Score {
			t.Fatalf("items not sorted by descending score: %+v", resp.Items)
		}
	}
	seen := make(map[string]struct{}, len(resp.Items))
	for _, item := range resp.Items {
		if _, ok := seen[item.AlbumID]; ok {
			t.Fatalf("duplicate album %q in %+v", item.AlbumID, resp.Items)
		}
		seen[item.AlbumID] = struct{}{}
	}
}

// ---------------------------------------------------------------------------
// EmbeddingProgress
// ---------------------------------------------------------------------------

func TestEmbeddingProgressFromCatalogCounts(t *testing.T) {
	cat := newTestCatalog(t)

	seedReadyEmbedding(t, cat, "hash-ready-1", vec768(1, 0))
	seedReadyEmbedding(t, cat, "hash-ready-2", vec768(0, 1))
	seedFailedEmbedding(t, cat, "hash-failed", "embed image: boom")
	seedBlob(t, cat, "hash-pending")

	svc := newTestService(t, cat, nil)
	got := svc.EmbeddingProgress()

	if got.Total != 4 || got.Ready != 2 || got.Failed != 1 || got.Pending != 1 {
		t.Fatalf("unexpected embedding progress counts: %+v", got)
	}
	if !approxEqual(got.Ratio, 0.5) {
		t.Fatalf("ratio=%f want=0.5", got.Ratio)
	}
}

func TestEmbeddingProgressEmptyCatalog(t *testing.T) {
	cat := newTestCatalog(t)
	svc := newTestService(t, cat, nil)

	got := svc.EmbeddingProgress()
	if got.Total != 0 || got.Ready != 0 || got.Failed != 0 || got.Pending != 0 {
		t.Fatalf("unexpected counts for empty catalog: %+v", got)
	}
	if got.Ratio != 0 {
		t.Fatalf("unexpected ratio for empty catalog: %+v", got)
	}
}

func TestEmbeddingProgressNilDependencies(t *testing.T) {
	var nilService *Service
	got := nilService.EmbeddingProgress()
	if got.Total != 0 || got.Ready != 0 || got.Failed != 0 || got.Pending != 0 || got.Ratio != 0 {
		t.Fatalf("unexpected progress for nil service: %+v", got)
	}

	svc := newTestService(t, nil, nil)
	got = svc.EmbeddingProgress()
	if got.Total != 0 || got.Ready != 0 || got.Failed != 0 || got.Pending != 0 || got.Ratio != 0 {
		t.Fatalf("unexpected progress for nil catalog: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Vector validation helpers
// ---------------------------------------------------------------------------

func TestIsZeroNorm(t *testing.T) {
	if !isZeroNorm(vec768()) {
		t.Fatalf("expected an all-zero vector to report zero norm")
	}
	if isZeroNorm(vec768(0, 1)) {
		t.Fatalf("expected a vector with a non-zero entry to report a norm")
	}
}

// gateEmbedder is a provider that stays inside Embed until the test releases
// it, which is what lets a test observe the in-flight state.
type gateEmbedder struct {
	started chan struct{}
	release chan struct{}
}

func (g *gateEmbedder) Load(context.Context) error { return nil }

func (g *gateEmbedder) Embed(ctx context.Context, _ []byte) ([]float32, error) {
	close(g.started)
	select {
	case <-g.release:
		return dimVector(1), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *gateEmbedder) Close() error { return nil }

func TestEmbeddingProgressCountsAndEnabled(t *testing.T) {
	cat := newTestCatalog(t)
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-shared"},
		catalog.Photo{Index: 1, Name: "b.jpg", Hash: "hash-only-a"},
	)
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-shared"})
	seedBlob(t, cat, "hash-shared")
	seedBlob(t, cat, "hash-only-a")

	if err := cat.SetBlobEmbedding(context.Background(), "hash-shared", catalog.EmbeddingStatusReady, dimVector(1), ""); err != nil {
		t.Fatalf("set ready embedding: %v", err)
	}

	// A service without a provider reports the coverage but says plainly that
	// it cannot embed anything, which is what stops a client from waiting.
	disabled := newTestService(t, cat, nil)
	progress := disabled.EmbeddingProgress()
	if progress.Enabled {
		t.Fatalf("expected Enabled=false without a provider: %+v", progress)
	}
	if progress.Total != 2 || progress.Ready != 1 || progress.Pending != 1 || progress.Failed != 0 {
		t.Fatalf("global progress=%+v want total=2 ready=1 pending=1 failed=0", progress)
	}
	if !approxEqual(progress.Ratio, 0.5) {
		t.Fatalf("ratio=%v want 0.5", progress.Ratio)
	}

	enabled := newTestService(t, cat, &gateEmbedder{started: make(chan struct{}), release: make(chan struct{})})
	if progress := enabled.EmbeddingProgress(); !progress.Enabled || progress.Active {
		t.Fatalf("expected Enabled=true and Active=false while idle: %+v", progress)
	}

	albumB, err := enabled.AlbumEmbeddingProgress(context.Background(), "album-b")
	if err != nil {
		t.Fatalf("album progress: %v", err)
	}
	if !albumB.Enabled || albumB.Total != 1 || albumB.Ready != 1 || albumB.Pending != 0 || !approxEqual(albumB.Ratio, 1) {
		t.Fatalf("album-b progress=%+v want enabled total=1 ready=1 pending=0 ratio=1", albumB)
	}
	if albumB.Active {
		t.Fatalf("a per-album view must not claim the worker's in-flight state: %+v", albumB)
	}
}

func TestEmbeddingProgressReportsActiveWhileEmbedding(t *testing.T) {
	cat := newTestCatalog(t)
	gate := &gateEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	svc := newTestService(t, cat, gate)

	done := make(chan error, 1)
	go func() {
		_, err := svc.computeEmbedding(context.Background(), []byte("image"))
		done <- err
	}()

	<-gate.started
	if progress := svc.EmbeddingProgress(); !progress.Active {
		t.Fatalf("expected Active while a forward pass is in flight: %+v", progress)
	}

	close(gate.release)
	if err := <-done; err != nil {
		t.Fatalf("embed: %v", err)
	}
	if progress := svc.EmbeddingProgress(); progress.Active {
		t.Fatalf("expected Active=false once the pass finished: %+v", progress)
	}
}

// ---------------------------------------------------------------------------
// External worker claim / write-back
// ---------------------------------------------------------------------------

// dimVector builds a valid-length embedding filled with one value, so tests
// can express "the same vector" without spelling out 768 floats.
func dimVector(fill float32) []float32 {
	vector := make([]float32, EmbeddingDim)
	for i := range vector {
		vector[i] = fill
	}
	return vector
}

func nanVector() []float32 {
	vector := dimVector(1)
	vector[0] = float32(math.NaN())
	return vector
}

func TestExternalWorkerClaimAndApplyRoundTrip(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})
	seedReadyEmbedding(t, cat, "hash-y", dimVector(1))
	seedAlbum(t, cat, "album-b",
		catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-y", Width: 10, Height: 20, Ratio: 0.5})

	// A nil embedder means the local model never loads; external backfill must
	// work regardless.
	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	blobs, leaseUntil, err := svc.ClaimEmbeddings(ctx, 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(blobs) != 1 || blobs[0].Hash != "hash-x" {
		t.Fatalf("expected hash-x claimed, got %+v", blobs)
	}
	if !leaseUntil.After(time.Now()) {
		t.Fatalf("expected a future lease deadline, got %s", leaseUntil)
	}

	// The claimed blob is in flight, so a second claim comes back empty and
	// the pending count still includes it.
	if again, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil || len(again) != 0 {
		t.Fatalf("expected empty second claim, got %+v err=%v", again, err)
	}
	if progress := svc.EmbeddingProgress(); progress.Pending != 1 || progress.Processing != 1 {
		t.Fatalf("expected pending=1 processing=1 while claimed, got %+v", progress)
	}

	// Bad vectors are rejected before anything is persisted.
	_, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: []float32{1, 2}},
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: nanVector()},
	})
	if err != nil {
		t.Fatalf("apply bad vectors: %v", err)
	}
	if len(rejected) != 2 || rejected[0].Reason != RejectWrongDim || rejected[1].Reason != RejectBadVector {
		t.Fatalf("expected wrong_dim then bad_vector rejections, got %+v", rejected)
	}

	applied, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: dimVector(2)},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied != 1 || len(rejected) != 0 {
		t.Fatalf("expected one applied result, got applied=%d rejected=%+v", applied, rejected)
	}

	progress := svc.EmbeddingProgress()
	if progress.Ready != 2 || progress.Pending != 0 || progress.Processing != 0 || progress.Ratio != 1 {
		t.Fatalf("expected full coverage after write-back, got %+v", progress)
	}

	// The in-memory index picked the vector up without a reload, so the new
	// photo immediately recommends across albums.
	resp, err := svc.Recommend(ctx, "album-a", 0, 5)
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].AlbumID != "album-b" || !approxEqual(resp.Items[0].Score, 1) {
		t.Fatalf("expected the cross-album neighbor, got %+v", resp.Items)
	}
	if resp.Items[0].Hash != "hash-y" {
		t.Fatalf("expected hash on the wire, got %+v", resp.Items[0])
	}
}

func TestExternalWorkerReleaseAndFailedReport(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	blobs, _, err := svc.ClaimEmbeddings(ctx, 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := svc.ReleaseClaims(ctx, claimHashes(blobs)); err != nil {
		t.Fatalf("release: %v", err)
	}

	// A released blob is claimable again right away, without a lease wait.
	if again, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil || len(again) != 1 {
		t.Fatalf("expected the released blob claimable again, got %d err=%v", len(again), err)
	}

	applied, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-x", Status: catalog.EmbeddingStatusFailed, Error: "decode image: boom"},
	})
	if err != nil {
		t.Fatalf("apply failed report: %v", err)
	}
	if applied != 1 || len(rejected) != 0 {
		t.Fatalf("expected the failure to land, got applied=%d rejected=%+v", applied, rejected)
	}

	// A failed blob is terminal: never claimable, and its failed status makes
	// it poison its own queries instead of ranking neighbors against nothing.
	if pending, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil || len(pending) != 0 {
		t.Fatalf("expected failed blob to stay unclaimed, got %d err=%v", len(pending), err)
	}
	if progress := svc.EmbeddingProgress(); progress.Failed != 1 || progress.Pending != 0 {
		t.Fatalf("expected failed=1 pending=0, got %+v", progress)
	}
	if _, failed := svc.queryVector(ctx, "hash-x"); !failed {
		t.Fatalf("expected the failed hash to poison queries")
	}
}

// A buggy worker can pad the hash with whitespace: the catalog trims it and
// applies to the clean row, so the vec0 index and the report both land under
// the same trimmed hash.
func TestExternalWorkerAppliesTrimmedHash(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})
	seedReadyEmbedding(t, cat, "hash-y", dimVector(1))
	seedAlbum(t, cat, "album-b",
		catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-y", Width: 10, Height: 20, Ratio: 0.5})

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	blobs, _, err := svc.ClaimEmbeddings(ctx, 0)
	if err != nil || len(blobs) != 1 {
		t.Fatalf("claim: %v blobs=%+v", err, blobs)
	}
	applied, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: " hash-x ", Status: catalog.EmbeddingStatusReady, Vector: dimVector(3)},
	})
	if err != nil || applied != 1 || len(rejected) != 0 {
		t.Fatalf("apply: err=%v applied=%d rejected=%+v", err, applied, rejected)
	}

	// The index knows the trimmed hash: the cross-album neighbor query works,
	// which it would not if the padded hash had poisoned the maps.
	result, err := svc.Recommend(ctx, "album-b", 0, 5)
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Hash != "hash-x" {
		t.Fatalf("items=%+v want the trimmed-hash neighbor", result.Items)
	}
}

// The catalog applies the first result per hash in a batch and rejects the
// rest; the recommendation view must follow the same side of that race, not
// the last submission.
func TestExternalWorkerDuplicateHashIsFirstWins(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})
	seedReadyEmbedding(t, cat, "hash-y", dimVector(1))
	seedAlbum(t, cat, "album-b",
		catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-y", Width: 10, Height: 20, Ratio: 0.5})

	svc := newTestService(t, cat, nil)
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if _, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// ready first, failed second: the catalog stores ready, so the index must
	// know the vector, not the failure.
	applied, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: dimVector(4)},
		{Hash: "hash-x", Status: catalog.EmbeddingStatusFailed, Error: "second submission"},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied != 1 || len(rejected) != 1 || rejected[0].Reason != catalog.EmbeddingRejectNotClaimed {
		t.Fatalf("applied=%d rejected=%+v want one apply and one not_claimed", applied, rejected)
	}
	result, err := svc.Recommend(ctx, "album-b", 0, 5)
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	if len(result.Items) != 1 {
		t.Fatalf("items=%+v want the ready neighbor despite the duplicate failed submission", result.Items)
	}
}

func TestExternalWorkerRenewReportsWhatRenewed(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})

	svc := newTestService(t, cat, nil)
	if _, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}

	leaseUntil, renewed, err := svc.RenewLeases(ctx, []string{"hash-x", "ghost"})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !leaseUntil.After(time.Now()) {
		t.Fatalf("leaseUntil=%v want a future deadline", leaseUntil)
	}
	if len(renewed) != 1 || renewed[0] != "hash-x" {
		t.Fatalf("renewed=%v want only hash-x: the ghost never had a lease", renewed)
	}
}
