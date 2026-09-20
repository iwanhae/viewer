package recommend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"viewer/internal/catalog"
	cfgpkg "viewer/internal/config"
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
// only exercise the in-memory index pass a nil images service.
func newTestService(t *testing.T, cat *catalog.Store, cfg cfgpkg.Config) *Service {
	t.Helper()
	svc, err := NewService(cfg, cat, nil)
	if err != nil {
		t.Fatalf("new recommend service: %v", err)
	}
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

// ---------------------------------------------------------------------------
// LoadAll
// ---------------------------------------------------------------------------

func TestLoadAllBuildsIndexFromCatalogRows(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedReadyEmbedding(t, cat, "hash-a", []float32{3, 4}) // normalizes to [0.6, 0.8]
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

	svc := newTestService(t, cat, cfgpkg.Config{})
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// Ready blobs are normalized and indexed for similarity search.
	embedding, ok := svc.embeddingsByHash["hash-a"]
	if !ok {
		t.Fatalf("expected hash-a embedding in index")
	}
	if len(embedding) != 2 || !approxEqual(float64(embedding[0]), 0.6) || !approxEqual(float64(embedding[1]), 0.8) {
		t.Fatalf("hash-a embedding=%v want=[0.6 0.8]", embedding)
	}

	// Failed blobs carry their reason and no vector.
	if got := svc.failedByHash["hash-b"]; got != "embed image: boom" {
		t.Fatalf("hash-b failure=%q want=%q", got, "embed image: boom")
	}
	if _, ok := svc.embeddingsByHash["hash-b"]; ok {
		t.Fatalf("failed blob must not have an embedding")
	}
	// Pending blobs and blobs without photos never enter the similarity index.
	if _, ok := svc.embeddingsByHash["hash-pending"]; ok {
		t.Fatalf("pending blob must not have an embedding")
	}
	if _, ok := svc.embeddingsByHash["hash-orphan"]; ok {
		t.Fatalf("blob without photos must not be indexed")
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
	svc := newTestService(t, nil, cfgpkg.Config{})
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

	seedReadyEmbedding(t, cat, "hash-a0", []float32{1, 0})
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-a0", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, cfgpkg.Config{})
	if err := svc.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	// album-b exists in the catalog but is not in the in-memory index yet.
	seedReadyEmbedding(t, cat, "hash-b0", []float32{0, 1})
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
	if _, ok := svc.embeddingsByHash["hash-b0"]; !ok {
		t.Fatalf("ReloadAlbum did not index album-b embedding")
	}

	// Refreshing an album drops stale hashes and picks up the new photo.
	seedReadyEmbedding(t, cat, "hash-a1", []float32{0, 1})
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

	seedReadyEmbedding(t, cat, "hash-query", []float32{1, 0})
	seedReadyEmbedding(t, cat, "hash-same-album", []float32{1, 0})
	seedReadyEmbedding(t, cat, "hash-b", []float32{0.9, 0.1})
	seedReadyEmbedding(t, cat, "hash-c", []float32{0, 1})

	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-query", Width: 100, Height: 200, Ratio: 0.5},
		catalog.Photo{Index: 1, Name: "a1.jpg", Hash: "hash-same-album", Width: 100, Height: 100, Ratio: 1},
	)
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-b", Width: 10, Height: 20, Ratio: 0.5})
	seedAlbum(t, cat, "album-c", catalog.Photo{Index: 0, Name: "c0.jpg", Hash: "hash-c", Width: 30, Height: 40, Ratio: 0.75})

	svc := newTestService(t, cat, cfgpkg.Config{})
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
	seedReadyEmbedding(t, cat, "hash-query", []float32{1, 0})
	seedReadyEmbedding(t, cat, "hash-twin", []float32{1, 0})
	seedReadyEmbedding(t, cat, "hash-other", []float32{0.5, 0.5})

	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-query", Width: 1, Height: 1, Ratio: 1},
		catalog.Photo{Index: 1, Name: "a1.jpg", Hash: "hash-twin", Width: 1, Height: 1, Ratio: 1},
	)
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-other", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, cfgpkg.Config{})
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

	seedReadyEmbedding(t, cat, "hash-a0", []float32{1, 0})
	seedReadyEmbedding(t, cat, "hash-a1", []float32{0.9, 0.1})
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-a0", Width: 1, Height: 1, Ratio: 1},
		catalog.Photo{Index: 1, Name: "a1.jpg", Hash: "hash-a1", Width: 1, Height: 1, Ratio: 1},
	)

	svc := newTestService(t, cat, cfgpkg.Config{})
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
	seedReadyEmbedding(t, cat, "hash-b", []float32{1, 0})
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-pending", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-b", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, cfgpkg.Config{})
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
	seedReadyEmbedding(t, cat, "hash-b", []float32{1, 0})
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-failed", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-b", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, cfgpkg.Config{})
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

	seedReadyEmbedding(t, cat, "hash-a", []float32{1, 0})
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-a", Width: 1, Height: 1, Ratio: 1})

	svc := newTestService(t, cat, cfgpkg.Config{})
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

	seedReadyEmbedding(t, cat, "hash-a", []float32{1, 0})
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-a", Width: 1, Height: 1, Ratio: 1})

	// No LoadAll: the in-memory index is empty even though the catalog has the
	// photo. The query vector comes from the catalog fallback; there are no
	// indexed neighbors, so the result is an empty list rather than a panic.
	svc := newTestService(t, cat, cfgpkg.Config{})
	resp, err := svc.Recommend(ctx, "album-a", 0, 12)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations, got %+v", resp.Items)
	}
	// The catalog fallback populated the embedding cache.
	if _, ok := svc.embeddingsByHash["hash-a"]; !ok {
		t.Fatalf("expected catalog fallback to cache the query embedding")
	}
}

func TestRecommendLimitClamping(t *testing.T) {
	cat := newTestCatalog(t)
	ctx := context.Background()

	seedReadyEmbedding(t, cat, "hash-query", []float32{1, 0})
	seedAlbum(t, cat, "album-query", catalog.Photo{Index: 0, Name: "q.jpg", Hash: "hash-query", Width: 1, Height: 1, Ratio: 1})

	const targets = 13
	for i := 0; i < targets; i++ {
		hash := fmt.Sprintf("hash-target-%02d", i)
		// Strictly decreasing similarity to the [1, 0] query.
		seedReadyEmbedding(t, cat, hash, []float32{1 - 0.05*float32(i), 0.05 * float32(i)})
		albumID := fmt.Sprintf("album-target-%02d", i)
		seedAlbum(t, cat, albumID, catalog.Photo{Index: 0, Name: albumID + ".jpg", Hash: hash, Width: 1, Height: 1, Ratio: 1})
	}

	clamped := newTestService(t, cat, cfgpkg.Config{RecoTopKDefault: 2, RecoTopKMax: 3})
	if err := clamped.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll clamped: %v", err)
	}
	fallback := newTestService(t, cat, cfgpkg.Config{})
	if err := fallback.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll fallback: %v", err)
	}

	cases := []struct {
		name  string
		svc   *Service
		limit int
		want  int
	}{
		{name: "default applied when limit zero", svc: clamped, limit: 0, want: 2},
		{name: "default applied when limit negative", svc: clamped, limit: -3, want: 2},
		{name: "requested limit honored", svc: clamped, limit: 1, want: 1},
		{name: "max clamps oversized limit", svc: clamped, limit: 100, want: 3},
		{name: "hard fallback default", svc: fallback, limit: 0, want: 12},
		{name: "no max clamp when unset", svc: fallback, limit: 100, want: targets},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := tc.svc.Recommend(ctx, "album-query", 0, tc.limit)
			if err != nil {
				t.Fatalf("Recommend: %v", err)
			}
			if len(resp.Items) != tc.want {
				t.Fatalf("items=%d want=%d: %+v", len(resp.Items), tc.want, resp.Items)
			}
		})
	}

	// Results stay ordered by descending score, one item per target album.
	resp, err := clamped.Recommend(ctx, "album-query", 0, 3)
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

	seedReadyEmbedding(t, cat, "hash-ready-1", []float32{1, 0})
	seedReadyEmbedding(t, cat, "hash-ready-2", []float32{0, 1})
	seedFailedEmbedding(t, cat, "hash-failed", "embed image: boom")
	seedBlob(t, cat, "hash-pending")

	svc := newTestService(t, cat, cfgpkg.Config{})
	got := svc.EmbeddingProgress()

	if got.Total != 4 || got.Ready != 2 || got.Failed != 1 || got.Pending != 1 || got.Processed != 3 {
		t.Fatalf("unexpected embedding progress counts: %+v", got)
	}
	if !approxEqual(got.Ratio, 0.5) {
		t.Fatalf("ratio=%f want=0.5", got.Ratio)
	}
	if !approxEqual(got.Percent, 50) {
		t.Fatalf("percent=%f want=50", got.Percent)
	}
}

func TestEmbeddingProgressEmptyCatalog(t *testing.T) {
	cat := newTestCatalog(t)
	svc := newTestService(t, cat, cfgpkg.Config{})

	got := svc.EmbeddingProgress()
	if got.Total != 0 || got.Ready != 0 || got.Failed != 0 || got.Pending != 0 || got.Processed != 0 {
		t.Fatalf("unexpected counts for empty catalog: %+v", got)
	}
	if got.Ratio != 0 || got.Percent != 0 {
		t.Fatalf("unexpected ratio for empty catalog: %+v", got)
	}
}

func TestEmbeddingProgressNilDependencies(t *testing.T) {
	var nilService *Service
	got := nilService.EmbeddingProgress()
	if got.Total != 0 || got.Ready != 0 || got.Failed != 0 || got.Pending != 0 || got.Processed != 0 || got.Ratio != 0 || got.Percent != 0 {
		t.Fatalf("unexpected progress for nil service: %+v", got)
	}

	svc := newTestService(t, nil, cfgpkg.Config{})
	got = svc.EmbeddingProgress()
	if got.Total != 0 || got.Ready != 0 || got.Failed != 0 || got.Pending != 0 || got.Processed != 0 || got.Ratio != 0 || got.Percent != 0 {
		t.Fatalf("unexpected progress for nil catalog: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// Vector helpers
// ---------------------------------------------------------------------------

func TestNormalizeVector(t *testing.T) {
	if got := normalizeVector(nil); got != nil {
		t.Fatalf("normalizeVector(nil)=%v want=nil", got)
	}
	if got := normalizeVector([]float32{}); got != nil {
		t.Fatalf("normalizeVector(empty)=%v want=nil", got)
	}

	got := normalizeVector([]float32{3, 4})
	if len(got) != 2 || !approxEqual(float64(got[0]), 0.6) || !approxEqual(float64(got[1]), 0.8) {
		t.Fatalf("normalizeVector([3 4])=%v want=[0.6 0.8]", got)
	}

	// A zero vector has no direction; the implementation substitutes norm=1 so
	// the result stays finite and keeps its length.
	zero := normalizeVector([]float32{0, 0})
	if len(zero) != 2 || zero[0] != 0 || zero[1] != 0 {
		t.Fatalf("normalizeVector([0 0])=%v want=[0 0]", zero)
	}

	input := []float32{3, 4}
	_ = normalizeVector(input)
	if input[0] != 3 || input[1] != 4 {
		t.Fatalf("normalizeVector mutated its input: %v", input)
	}
}

func TestCosineNormalized(t *testing.T) {
	if got := cosineNormalized(nil, []float32{1}); got != 0 {
		t.Fatalf("cosineNormalized(nil, x)=%f want=0", got)
	}
	if got := cosineNormalized([]float32{1}, nil); got != 0 {
		t.Fatalf("cosineNormalized(x, nil)=%f want=0", got)
	}
	if got := cosineNormalized([]float32{1, 0}, []float32{1, 0}); !approxEqual(got, 1) {
		t.Fatalf("cosineNormalized identical=%f want=1", got)
	}
	if got := cosineNormalized([]float32{1, 0}, []float32{0, 1}); !approxEqual(got, 0) {
		t.Fatalf("cosineNormalized orthogonal=%f want=0", got)
	}
	// Mismatched lengths compare only the shorter prefix.
	if got := cosineNormalized([]float32{1, 0}, []float32{1, 0, 5}); !approxEqual(got, 1) {
		t.Fatalf("cosineNormalized mismatched=%f want=1", got)
	}
	// Inputs are expected to be normalized already: this is a dot product.
	if got := cosineNormalized([]float32{2, 0}, []float32{3, 0}); !approxEqual(got, 6) {
		t.Fatalf("cosineNormalized dot product=%f want=6", got)
	}
}

func TestFindNeighborsOrdersAndTieBreaksByHash(t *testing.T) {
	embeddings := map[string][]float32{
		"hash-z": {1, 0},
		"hash-a": {1, 0},
		"hash-m": {0, 1},
	}
	query := []float32{1, 0}

	want := []Neighbor{
		{Hash: "hash-a", Score: 1},
		{Hash: "hash-z", Score: 1},
		{Hash: "hash-m", Score: 0},
	}

	// The map iteration order is random, so repeat to prove the sort is stable.
	for i := 0; i < 25; i++ {
		got := findNeighbors(embeddings, query, len(embeddings), "")
		if len(got) != len(want) {
			t.Fatalf("neighbors=%d want=%d: %+v", len(got), len(want), got)
		}
		for j := range want {
			if got[j].Hash != want[j].Hash || !approxEqual(got[j].Score, want[j].Score) {
				t.Fatalf("iteration %d neighbors[%d]=%+v want=%+v", i, j, got[j], want[j])
			}
		}
	}

	// The query is normalized before scoring, so magnitude does not matter.
	scaled := findNeighbors(embeddings, []float32{5, 0}, len(embeddings), "")
	for j := range want {
		if scaled[j].Hash != want[j].Hash {
			t.Fatalf("scaled query neighbors[%d]=%+v want=%+v", j, scaled[j], want[j])
		}
	}

	// excludeHash drops the query itself from the results.
	excluded := findNeighbors(embeddings, query, len(embeddings), "hash-a")
	if len(excluded) != 2 || excluded[0].Hash != "hash-z" || excluded[1].Hash != "hash-m" {
		t.Fatalf("excludeHash result=%+v", excluded)
	}

	// limit truncates after sorting.
	limited := findNeighbors(embeddings, query, 1, "")
	if len(limited) != 1 || limited[0].Hash != "hash-a" {
		t.Fatalf("limit=1 result=%+v", limited)
	}

	if got := findNeighbors(embeddings, query, 0, ""); got != nil {
		t.Fatalf("limit=0 result=%+v want=nil", got)
	}
	if got := findNeighbors(embeddings, nil, len(embeddings), ""); got != nil {
		t.Fatalf("empty query result=%+v want=nil", got)
	}
	if got := findNeighbors(nil, query, 10, ""); len(got) != 0 {
		t.Fatalf("empty embeddings result=%+v want empty", got)
	}
}
