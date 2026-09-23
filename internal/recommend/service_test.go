package recommend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/qdrant"
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
	store, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"), nil)
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

// newTestService builds a recommendation service over the catalog with a nil
// images service, which keeps the background workers out of the way. A nil
// embedder switches embedding off.
func newTestService(t *testing.T, cat *catalog.Store, vectors VectorStore, embedder EmbeddingProvider) *Service {
	t.Helper()
	svc := NewService(cat, vectors, nil, embedder, nil)
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

func seedReadyEmbedding(t *testing.T, svc *Service, hash string, vector []float32) {
	t.Helper()
	seedBlob(t, svc.catalog, hash)
	seedEmbeddingOutcome(t, svc, hash, catalog.EmbeddingStatusReady, vector, "")
}

func seedFailedEmbedding(t *testing.T, svc *Service, hash string, errText string) {
	t.Helper()
	seedBlob(t, svc.catalog, hash)
	seedEmbeddingOutcome(t, svc, hash, catalog.EmbeddingStatusFailed, nil, errText)
}

// seedEmbeddingOutcome pushes one terminal embedding outcome through the
// service's production write path, so the catalog bookkeeping and the vector
// store move together exactly as a real write-back would. The claim uses an
// already-expired lease and sweeps every claimable blob so the target is always
// among the claimed rows; blobs that only happened to be swept keep an expired
// lease, which reclaims exactly like pending for everyone under test.
func seedEmbeddingOutcome(t *testing.T, svc *Service, hash string, status catalog.EmbeddingStatus, vector []float32, errText string) {
	t.Helper()
	ctx := context.Background()
	claimed, err := svc.catalog.ClaimPendingEmbeddings(ctx, MaxClaimLimit, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("claim pending embeddings: %v", err)
	}
	claimable := false
	for _, blob := range claimed {
		if blob.Hash == hash {
			claimable = true
			break
		}
	}
	if !claimable {
		// Already past pending: only acceptable when a previous seed left the
		// blob in exactly the requested terminal state.
		blob, err := svc.catalog.GetBlob(ctx, hash)
		if err != nil {
			t.Fatalf("get blob %s: %v", hash, err)
		}
		if blob.EmbeddingStatus != status {
			t.Fatalf("blob %s not claimable (status %q, want %q)", hash, blob.EmbeddingStatus, status)
		}
		return
	}
	applied, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{{
		Hash:   hash,
		Status: status,
		Vector: vector,
		Error:  errText,
	}})
	if err != nil {
		t.Fatalf("apply embedding result %s: %v", hash, err)
	}
	if applied != 1 || len(rejected) != 0 {
		t.Fatalf("apply embedding result %s: applied=%d rejected=%v", hash, applied, rejected)
	}
}

// vec768 builds a full-length embedding whose first components are vals and
// whose remainder is zero, so tests can write short direction literals while
// satisfying the fixed width. Cosine similarity between two vec768 vectors
// equals the cosine of the short forms they encode.
func vec768(vals ...float32) []float32 {
	vector := make([]float32, catalog.EmbeddingDim)
	copy(vector, vals)
	return vector
}

// dimVector builds a valid-length embedding filled with one value, so tests
// can express "the same vector" without spelling out 768 floats.
func dimVector(fill float32) []float32 {
	vector := make([]float32, catalog.EmbeddingDim)
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

// ---------------------------------------------------------------------------
// fakeVectorStore
// ---------------------------------------------------------------------------

// fakeVectorStore is an honest in-memory stand-in for the Qdrant client: points
// are keyed by the same deterministic point IDs, GroupSearch really ranks by
// cosine and applies the same exclusions, and every operation is recorded so
// tests can pin the order writes happen in.
// searchCall records one SearchByVector call, so a test can pin the vector and
// the limit the service forwarded to the store.
type searchCall struct {
	vector []float32
	limit  int
}

type fakeVectorStore struct {
	mu            sync.Mutex
	points        map[string]qdrant.PhotoRecord
	ensureCalls   int
	upsertBatches [][]qdrant.PhotoRecord
	ops           []string
	searches      []searchCall
	ensureErr     error
	upsertErr     error
	groupErr      error
	searchErr     error
}

func newFakeVectorStore() *fakeVectorStore {
	return &fakeVectorStore{points: make(map[string]qdrant.PhotoRecord)}
}

func (f *fakeVectorStore) EnsureCollection(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	f.ops = append(f.ops, "ensure")
	return f.ensureErr
}

func (f *fakeVectorStore) UpsertPhotos(ctx context.Context, records []qdrant.PhotoRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, fmt.Sprintf("upsert:%d", len(records)))
	if f.upsertErr != nil {
		return f.upsertErr
	}
	copied := append([]qdrant.PhotoRecord(nil), records...)
	f.upsertBatches = append(f.upsertBatches, copied)
	for _, record := range copied {
		f.points[qdrant.PointID(record.AlbumID, record.Idx)] = record
	}
	return nil
}

func (f *fakeVectorStore) GroupSearch(ctx context.Context, queryPointID, excludeAlbumID, excludeHash string, limit int) ([]qdrant.PhotoHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.groupErr != nil {
		return nil, f.groupErr
	}
	query, ok := f.points[queryPointID]
	if !ok {
		// The live client answers 404 with empty results, not an error.
		return nil, nil
	}
	type scored struct {
		record qdrant.PhotoRecord
		score  float64
	}
	var matches []scored
	for id, record := range f.points {
		if id == queryPointID || record.AlbumID == excludeAlbumID || record.Hash == excludeHash {
			continue
		}
		matches = append(matches, scored{record: record, score: cosine(query.Vector, record.Vector)})
	}
	// Rank exactly like Qdrant: score descending, deterministic tie-break.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if matches[i].record.AlbumID != matches[j].record.AlbumID {
			return matches[i].record.AlbumID < matches[j].record.AlbumID
		}
		return matches[i].record.Idx < matches[j].record.Idx
	})
	// group_size=1 semantics: after ranking, only the best photo per album
	// survives, and the limit caps the number of albums.
	hits := make([]qdrant.PhotoHit, 0, limit)
	seenAlbum := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if _, dup := seenAlbum[match.record.AlbumID]; dup {
			continue
		}
		seenAlbum[match.record.AlbumID] = struct{}{}
		hits = append(hits, qdrant.PhotoHit{
			AlbumID: match.record.AlbumID,
			Idx:     match.record.Idx,
			Hash:    match.record.Hash,
			W:       match.record.W,
			H:       match.record.H,
			Score:   match.score,
		})
		if len(hits) == limit {
			break
		}
	}
	return hits, nil
}

// SearchByVector ranks every stored point against the query vector by honest
// cosine similarity. Unlike GroupSearch there is no exclusion and no album
// grouping: search must see every photo, so ties break on album then idx only
// to keep the order deterministic.
func (f *fakeVectorStore) SearchByVector(_ context.Context, vector []float32, limit int) ([]qdrant.PhotoHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = append(f.searches, searchCall{vector: append([]float32(nil), vector...), limit: limit})
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	type scored struct {
		record qdrant.PhotoRecord
		score  float64
	}
	matches := make([]scored, 0, len(f.points))
	for _, record := range f.points {
		matches = append(matches, scored{record: record, score: cosine(vector, record.Vector)})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if matches[i].record.AlbumID != matches[j].record.AlbumID {
			return matches[i].record.AlbumID < matches[j].record.AlbumID
		}
		return matches[i].record.Idx < matches[j].record.Idx
	})
	hits := make([]qdrant.PhotoHit, 0, limit)
	for _, match := range matches {
		if len(hits) == limit {
			break
		}
		hits = append(hits, qdrant.PhotoHit{
			AlbumID: match.record.AlbumID,
			Idx:     match.record.Idx,
			Hash:    match.record.Hash,
			W:       match.record.W,
			H:       match.record.H,
			Score:   match.score,
		})
	}
	return hits, nil
}

func (f *fakeVectorStore) RetrieveVectorsByHashes(ctx context.Context, hashes []string) (map[string][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, fmt.Sprintf("retrieve:%d", len(hashes)))
	vectors := make(map[string][]float32, len(hashes))
	for _, record := range f.points {
		if _, seen := vectors[record.Hash]; seen {
			continue
		}
		for _, hash := range hashes {
			if hash == record.Hash {
				vectors[record.Hash] = append([]float32(nil), record.Vector...)
				break
			}
		}
	}
	return vectors, nil
}

func (f *fakeVectorStore) DeleteByAlbum(ctx context.Context, albumID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "delete:"+albumID)
	for id, record := range f.points {
		if record.AlbumID == albumID {
			delete(f.points, id)
		}
	}
	return nil
}

func (f *fakeVectorStore) CountVectors(ctx context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.points), nil
}

// snapshot returns copies of the stored points plus the recorded op list, so
// assertions cannot race the store.
func (f *fakeVectorStore) snapshot() (map[string]qdrant.PhotoRecord, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	points := make(map[string]qdrant.PhotoRecord, len(f.points))
	for id, record := range f.points {
		points[id] = record
	}
	return points, append([]string(nil), f.ops...)
}

// resetOps drops the recorded call list, so a test can bracket one operation
// and assert on exactly its writes.
func (f *fakeVectorStore) resetOps() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = nil
}

// searchCalls returns copies of the recorded SearchByVector calls.
func (f *fakeVectorStore) searchCalls() []searchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]searchCall(nil), f.searches...)
}

// resetSearches drops the recorded search calls, so a test can bracket one
// query and assert on exactly its store call.
func (f *fakeVectorStore) resetSearches() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searches = nil
}

// cosine is the honest ranking the fake promises: scale-invariant similarity in
// [-1, 1], with a zero vector defined as matching nothing.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / math.Sqrt(normA*normB)
}

// ---------------------------------------------------------------------------
// Recommend
// ---------------------------------------------------------------------------

// TestRecommendGroupsOnePhotoPerAlbumByScore seeds the store through the real
// write-back path and pins the response shape: one hit per foreign album,
// ranked by similarity, with the photo fields and the raw score carried onto
// the wire unchanged.
func TestRecommendGroupsOnePhotoPerAlbumByScore(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-query")
	seedBlob(t, cat, "hash-twin")
	seedBlob(t, cat, "hash-close")
	seedBlob(t, cat, "hash-far")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-query", Width: 100, Height: 200, Ratio: 0.5},
		catalog.Photo{Index: 1, Name: "a1.jpg", Hash: "hash-twin", Width: 100, Height: 100, Ratio: 1},
	)
	// album-b holds two photos of the same image: both become points, but only
	// the better-scoring one may come back.
	seedAlbum(t, cat, "album-b",
		catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-close", Width: 10, Height: 20, Ratio: 0.5},
		catalog.Photo{Index: 1, Name: "b1.jpg", Hash: "hash-close", Width: 10, Height: 20, Ratio: 0.5},
	)
	seedAlbum(t, cat, "album-c", catalog.Photo{Index: 0, Name: "c0.jpg", Hash: "hash-far", Width: 30, Height: 40, Ratio: 0.75})

	seedReadyEmbedding(t, svc, "hash-query", vec768(1, 0))
	seedReadyEmbedding(t, svc, "hash-twin", vec768(1, 0))
	seedReadyEmbedding(t, svc, "hash-close", vec768(0.9, 0.1))
	seedReadyEmbedding(t, svc, "hash-far", vec768(0, 1))

	points, _ := store.snapshot()
	if len(points) != 5 {
		t.Fatalf("store holds %d points, want 5 (one per photo): %+v", len(points), points)
	}

	resp, err := svc.Recommend(ctx, "album-a", 0, 10)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items=%d want=2 (one per album): %+v", len(resp.Items), resp.Items)
	}
	if resp.Items[0].AlbumID != "album-b" || resp.Items[1].AlbumID != "album-c" {
		t.Fatalf("unexpected order: %+v", resp.Items)
	}
	if !(resp.Items[0].Score > resp.Items[1].Score) {
		t.Fatalf("expected descending scores, got %+v", resp.Items)
	}
	// Photo fields and the raw score pass straight through.
	if resp.Items[0].I != 0 || resp.Items[0].Hash != "hash-close" || resp.Items[0].W != 10 || resp.Items[0].H != 20 {
		t.Fatalf("unexpected album-b item: %+v", resp.Items[0])
	}
	if !approxEqual(resp.Items[0].Score, cosine(vec768(1, 0), vec768(0.9, 0.1))) {
		t.Fatalf("score=%v want the cosine similarity", resp.Items[0].Score)
	}
	for _, item := range resp.Items {
		if item.AlbumID == "album-a" {
			t.Fatalf("same-album item leaked: %+v", item)
		}
	}
}

func TestRecommendExcludesQueryHashAcrossAlbums(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	// hash-shared appears in three albums. Querying one of them must exclude
	// the shared hash everywhere, not just in the query album.
	seedBlob(t, cat, "hash-shared")
	seedBlob(t, cat, "hash-other")
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-shared", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b.jpg", Hash: "hash-shared", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-c", catalog.Photo{Index: 0, Name: "c.jpg", Hash: "hash-other", Width: 1, Height: 1, Ratio: 1})

	seedReadyEmbedding(t, svc, "hash-shared", vec768(1, 0))
	seedReadyEmbedding(t, svc, "hash-other", vec768(0.5, 0.5))

	resp, err := svc.Recommend(ctx, "album-a", 0, 10)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].AlbumID != "album-c" {
		t.Fatalf("expected only album-c, got %+v", resp.Items)
	}
}

func TestRecommendReturnsEmptyItemsWhenNothingIndexed(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-a")
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-a", Width: 1, Height: 1, Ratio: 1})
	// The blob is ready in the catalog but its vector never reached the store
	// (e.g. the store was wiped): the search finds no query point.
	seedReadyEmbedding(t, svc, "hash-a", vec768(1, 0))
	if err := store.DeleteByAlbum(ctx, "album-a"); err != nil {
		t.Fatalf("wipe store: %v", err)
	}

	resp, err := svc.Recommend(ctx, "album-a", 0, 12)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if resp.Items == nil || len(resp.Items) != 0 {
		t.Fatalf("expected a non-nil empty item list, got %+v", resp.Items)
	}
}

func TestRecommendReturnsEmptyItemsWhenQueryEmbeddingPending(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-pending")
	seedBlob(t, cat, "hash-b")
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-pending", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b.jpg", Hash: "hash-b", Width: 1, Height: 1, Ratio: 1})
	seedReadyEmbedding(t, svc, "hash-b", vec768(1, 0))

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
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-failed")
	seedBlob(t, cat, "hash-b")
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-failed", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b.jpg", Hash: "hash-b", Width: 1, Height: 1, Ratio: 1})
	seedReadyEmbedding(t, svc, "hash-b", vec768(1, 0))
	seedFailedEmbedding(t, svc, "hash-failed", "embed image: boom")

	resp, err := svc.Recommend(ctx, "album-a", 0, 12)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("expected no recommendations for failed query, got %+v", resp.Items)
	}
}

func TestRecommendWithoutVectorStoreReportsUnavailable(t *testing.T) {
	cat := newTestCatalog(t)
	svc := newTestService(t, cat, nil, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-a")
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-a", Width: 1, Height: 1, Ratio: 1})

	resp, err := svc.Recommend(ctx, "album-a", 0, 12)
	if err == nil {
		t.Fatalf("expected an error without a vector store, got %+v", resp)
	}
	if !errors.Is(err, ErrVectorStoreUnavailable) {
		t.Fatalf("err=%v want ErrVectorStoreUnavailable", err)
	}
	if !strings.Contains(err.Error(), "vector store is not available") {
		t.Fatalf("err=%v want the unavailability message", err)
	}
	if resp.Items != nil {
		t.Fatalf("expected zero-value response on error, got %+v", resp)
	}
}

// TestRecommendUnknownPhotoWrapsErrPhotoNotFound also pins that the recommend
// sentinel is distinct from the catalog one: Recommend translates
// catalog.ErrPhotoNotFound into its own error.
func TestRecommendUnknownPhotoWrapsErrPhotoNotFound(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-a")
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-a", Width: 1, Height: 1, Ratio: 1})

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

func TestRecommendLimitClamping(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-query")
	seedAlbum(t, cat, "album-query", catalog.Photo{Index: 0, Name: "q.jpg", Hash: "hash-query", Width: 1, Height: 1, Ratio: 1})

	// Enough distinct target albums to exercise the maxTopK clamp.
	targets := maxTopK + 5
	for i := 0; i < targets; i++ {
		hash := fmt.Sprintf("hash-target-%02d", i)
		// Strictly decreasing similarity to the [1, 0] query.
		seedBlob(t, cat, hash)
		albumID := fmt.Sprintf("album-target-%02d", i)
		seedAlbum(t, cat, albumID, catalog.Photo{Index: 0, Name: albumID + ".jpg", Hash: hash, Width: 1, Height: 1, Ratio: 1})
		seedReadyEmbedding(t, svc, hash, vec768(1-0.001*float32(i), 0.001*float32(i)))
	}
	seedReadyEmbedding(t, svc, "hash-query", vec768(1, 0))

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
// Search
// ---------------------------------------------------------------------------

// textEmbedder is a dual-capability stub: it satisfies EmbeddingProvider so it
// can be wired into the service, and TextEmbeddingProvider so Search can reach
// the query half. Every query maps to one fixed vector, which is what makes
// the ranking assertable; the image half must never run in the search tests.
type textEmbedder struct {
	vector []float32
	err    error
}

func (e *textEmbedder) Load(context.Context) error { return nil }

func (e *textEmbedder) Embed(context.Context, []byte) ([]float32, error) {
	return nil, errors.New("image embedding must not run in the search tests")
}

func (e *textEmbedder) Close() error { return nil }

func (e *textEmbedder) EmbedText(context.Context, string) ([]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.vector, nil
}

// TestSearchRanksPhotosByCosine seeds the store directly — Search never
// touches the catalog — and pins the contract that separates search from
// Recommend: every matching photo comes back in descending cosine order,
// duplicates and all, with the photo fields and the raw score carried onto
// the wire unchanged.
func TestSearchRanksPhotosByCosine(t *testing.T) {
	store := newFakeVectorStore()
	stub := &textEmbedder{vector: vec768(1, 0)}
	svc := newTestService(t, nil, store, stub)
	ctx := context.Background()

	// vec768 encodes short directions at full width, so the cosines against the
	// [1, 0] query are, best first: 0.9998, 0.9988, 0.7071 and 0.
	if err := store.UpsertPhotos(ctx, []qdrant.PhotoRecord{
		{AlbumID: "album-a", Idx: 0, Hash: "hash-near", W: 800, H: 600, Vector: vec768(1, 0.05)},
		{AlbumID: "album-a", Idx: 1, Hash: "hash-twin", W: 800, H: 600, Vector: vec768(0.99, 0.02)},
		{AlbumID: "album-b", Idx: 0, Hash: "hash-mid", W: 800, H: 600, Vector: vec768(0.7, 0.7)},
		{AlbumID: "album-b", Idx: 1, Hash: "hash-far", W: 800, H: 600, Vector: vec768(0, 1)},
	}); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	resp, err := svc.Search(ctx, "a sunny beach", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	wantHashes := []string{"hash-twin", "hash-near", "hash-mid", "hash-far"}
	if len(resp.Items) != len(wantHashes) {
		t.Fatalf("items=%d want=%d (search returns every photo, no album grouping): %+v", len(resp.Items), len(wantHashes), resp.Items)
	}
	for i, hash := range wantHashes {
		if resp.Items[i].Hash != hash {
			t.Fatalf("item %d = %+v, want hash %s (descending cosine)", i, resp.Items[i], hash)
		}
	}
	if !approxEqual(resp.Items[0].Score, cosine(stub.vector, vec768(0.99, 0.02))) {
		t.Fatalf("score=%v want the cosine similarity", resp.Items[0].Score)
	}
	if resp.Items[0].AlbumID != "album-a" || resp.Items[0].I != 1 || resp.Items[0].W != 800 || resp.Items[0].H != 600 {
		t.Fatalf("photo fields did not carry through: %+v", resp.Items[0])
	}

	// The store was asked once, with exactly the embedded query vector.
	calls := store.searchCalls()
	if len(calls) != 1 {
		t.Fatalf("store calls=%d want=1", len(calls))
	}
	if !reflect.DeepEqual(calls[0].vector, stub.vector) || calls[0].limit != 10 {
		t.Fatalf("store call carried vector %v limit %d, want the query vector at limit 10", calls[0].vector, calls[0].limit)
	}
}

// TestSearchLimitClampsBeforeTheStoreQuery pins the clamping where it matters:
// the store is asked for exactly the clamped limit, so the clamp is observable
// in the recorded call rather than only in a trimmed response.
func TestSearchLimitClampsBeforeTheStoreQuery(t *testing.T) {
	store := newFakeVectorStore()
	svc := newTestService(t, nil, store, &textEmbedder{vector: vec768(1)})
	ctx := context.Background()

	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{name: "zero gets the default", limit: 0, want: defaultSearchTopK},
		{name: "negative gets the default", limit: -3, want: defaultSearchTopK},
		{name: "requested limit passes through", limit: 1, want: 1},
		{name: "oversized limit clamps to the max", limit: 1000, want: maxSearchTopK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store.resetSearches()
			if _, err := svc.Search(ctx, "anything", tc.limit); err != nil {
				t.Fatalf("Search: %v", err)
			}
			calls := store.searchCalls()
			if len(calls) != 1 {
				t.Fatalf("store calls=%d want=1", len(calls))
			}
			if calls[0].limit != tc.want {
				t.Fatalf("store limit=%d want=%d", calls[0].limit, tc.want)
			}
		})
	}
}

func TestSearchWithoutVectorStoreReportsUnavailable(t *testing.T) {
	svc := newTestService(t, nil, nil, &textEmbedder{vector: vec768(1)})
	resp, err := svc.Search(context.Background(), "a query", 10)
	if !errors.Is(err, ErrVectorStoreUnavailable) {
		t.Fatalf("err=%v want ErrVectorStoreUnavailable", err)
	}
	if resp.Items != nil {
		t.Fatalf("expected zero-value response on error, got %+v", resp)
	}
}

// TestSearchWithoutTextEmbeddingReportsUnavailable covers the capability
// check: gateEmbedder is an image-only provider, so the type assertion inside
// Search must fail softly with ErrTextEmbeddingUnavailable — and the store
// must never be asked for anything.
func TestSearchWithoutTextEmbeddingReportsUnavailable(t *testing.T) {
	store := newFakeVectorStore()
	svc := newTestService(t, nil, store, &gateEmbedder{started: make(chan struct{}), release: make(chan struct{})})
	resp, err := svc.Search(context.Background(), "a query", 10)
	if !errors.Is(err, ErrTextEmbeddingUnavailable) {
		t.Fatalf("err=%v want ErrTextEmbeddingUnavailable", err)
	}
	if errors.Is(err, ErrVectorStoreUnavailable) {
		t.Fatalf("err=%v must not report the store unavailable", err)
	}
	if resp.Items != nil {
		t.Fatalf("expected zero-value response on error, got %+v", resp)
	}
	if calls := store.searchCalls(); len(calls) != 0 {
		t.Fatalf("store was queried despite no text embedder: %+v", calls)
	}
}

func TestSearchRejectsBlankQuery(t *testing.T) {
	store := newFakeVectorStore()
	svc := newTestService(t, nil, store, &textEmbedder{vector: vec768(1)})
	for _, query := range []string{"", "   ", "\t\n"} {
		resp, err := svc.Search(context.Background(), query, 10)
		if !errors.Is(err, ErrEmptyQuery) {
			t.Fatalf("Search(%q) err=%v want ErrEmptyQuery", query, err)
		}
		if resp.Items != nil {
			t.Fatalf("expected zero-value response on error, got %+v", resp)
		}
	}
	if calls := store.searchCalls(); len(calls) != 0 {
		t.Fatalf("store was queried for a blank query: %+v", calls)
	}
}

// TestSearchRejectsDegenerateQueryVector applies the write path's degenerate
// rules to the read path: a NaN or all-zero text vector would rank as NaN
// against every point and drag the whole ranking down, so it is rejected
// before the store is asked.
func TestSearchRejectsDegenerateQueryVector(t *testing.T) {
	cases := []struct {
		name   string
		vector []float32
	}{
		{name: "all-zero vector", vector: vec768()},
		{name: "NaN vector", vector: nanVector()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeVectorStore()
			svc := newTestService(t, nil, store, &textEmbedder{vector: tc.vector})
			if _, err := svc.Search(context.Background(), "a query", 10); err == nil {
				t.Fatal("Search succeeded with a degenerate query vector, want an error")
			}
			if calls := store.searchCalls(); len(calls) != 0 {
				t.Fatalf("store was queried with a degenerate vector: %+v", calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// EmbeddingProgress
// ---------------------------------------------------------------------------

func TestEmbeddingProgressFromCatalogCounts(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)

	seedReadyEmbedding(t, svc, "hash-ready-1", vec768(1, 0))
	seedReadyEmbedding(t, svc, "hash-ready-2", vec768(0, 1))
	seedFailedEmbedding(t, svc, "hash-failed", "embed image: boom")
	seedBlob(t, cat, "hash-pending")

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
	svc := newTestService(t, cat, newFakeVectorStore(), nil)

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

	svc := newTestService(t, nil, nil, nil)
	got = svc.EmbeddingProgress()
	if got.Total != 0 || got.Ready != 0 || got.Failed != 0 || got.Pending != 0 || got.Ratio != 0 {
		t.Fatalf("unexpected progress for nil catalog: %+v", got)
	}
}

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
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-shared"},
		catalog.Photo{Index: 1, Name: "b.jpg", Hash: "hash-only-a"},
	)
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-shared"})
	seedBlob(t, cat, "hash-shared")
	seedBlob(t, cat, "hash-only-a")
	seedEmbeddingOutcome(t, svc, "hash-shared", catalog.EmbeddingStatusReady, dimVector(1), "")

	// The write-back created one point per photo showing the shared blob.
	points, _ := store.snapshot()
	if len(points) != 2 {
		t.Fatalf("store holds %d points, want 2: %+v", len(points), points)
	}

	// A service without a provider reports the coverage but says plainly that
	// it cannot embed anything, which is what stops a client from waiting.
	disabled := newTestService(t, cat, store, nil)
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

	enabled := newTestService(t, cat, store, &gateEmbedder{started: make(chan struct{}), release: make(chan struct{})})
	if progress := enabled.EmbeddingProgress(); !progress.Enabled || progress.Active {
		t.Fatalf("expected Enabled=true and Active=false while idle: %+v", progress)
	}
}

func TestEmbeddingProgressReportsActiveWhileEmbedding(t *testing.T) {
	cat := newTestCatalog(t)
	gate := &gateEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	svc := newTestService(t, cat, newFakeVectorStore(), gate)

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

func TestExternalWorkerClaimAndApplyRoundTrip(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})
	seedBlob(t, cat, "hash-y")
	seedAlbum(t, cat, "album-b",
		catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-y", Width: 10, Height: 20, Ratio: 0.5})
	seedReadyEmbedding(t, svc, "hash-y", dimVector(1))

	// A nil embedder means the local model never loads; external backfill must
	// work regardless.
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

	// Bad vectors are rejected before either store sees anything.
	_, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: []float32{1, 2}},
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: nanVector()},
	})
	if err != nil {
		t.Fatalf("apply bad vectors: %v", err)
	}
	if len(rejected) != 2 || rejected[0].Reason != catalog.EmbeddingRejectWrongDim || rejected[1].Reason != catalog.EmbeddingRejectBadVector {
		t.Fatalf("expected wrong_dim then bad_vector rejections, got %+v", rejected)
	}
	if points, _ := store.snapshot(); len(points) != 1 {
		t.Fatalf("rejected vectors must not reach the store, points=%+v", points)
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

	// The new point is searchable without any reload step, so the fresh photo
	// immediately recommends across albums.
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

// TestApplyEmbeddingResultsWritesQdrantBeforeSQLite pins the write order that
// makes crashes recoverable: a store failure leaves the catalog completely
// untouched — the blob keeps its processing claim so a retry can land both —
// and a success makes the point searchable before the status flips.
func TestApplyEmbeddingResultsWritesQdrantBeforeSQLite(t *testing.T) {
	cat := newTestCatalog(t)
	svc := newTestService(t, cat, newFakeVectorStore(), nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-x", Width: 1, Height: 1, Ratio: 1})
	if _, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}

	failing := newFakeVectorStore()
	failing.upsertErr = errors.New("qdrant unreachable")
	// Swap the store under the service for the failing one.
	svc.vectors = failing
	applied, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: dimVector(1)},
	})
	if err == nil {
		t.Fatalf("expected the store failure to surface, got applied=%d", applied)
	}
	if applied != 0 || rejected != nil {
		t.Fatalf("a failed store write must report nothing applied, got applied=%d rejected=%+v", applied, rejected)
	}
	// SQLite is untouched: the blob keeps its processing state and its lease.
	blob, err := cat.GetBlob(ctx, "hash-x")
	if err != nil || blob.EmbeddingStatus != catalog.EmbeddingStatusProcessing {
		t.Fatalf("blob=%+v err=%v want still processing after store failure", blob, err)
	}

	// The healthy store lands both writes, in that order.
	working := newFakeVectorStore()
	svc.vectors = working
	applied, rejected, err = svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: dimVector(1)},
	})
	if err != nil || applied != 1 || len(rejected) != 0 {
		t.Fatalf("apply: err=%v applied=%d rejected=%+v", err, applied, rejected)
	}
	points, ops := working.snapshot()
	if len(points) != 1 {
		t.Fatalf("expected one point, got %+v", points)
	}
	if len(ops) != 1 || ops[0] != "upsert:1" {
		t.Fatalf("ops=%v want a single upsert", ops)
	}
	blob, err = cat.GetBlob(ctx, "hash-x")
	if err != nil || blob.EmbeddingStatus != catalog.EmbeddingStatusReady {
		t.Fatalf("blob=%+v err=%v want ready", blob, err)
	}
}

// TestApplyEmbeddingResultsReembedOverwritesSamePoint covers the crash-window
// recovery: a write-back whose vector already reached the store — a re-embed
// after the SQLite half failed, or a duplicate report from a second worker —
// upserts the very same deterministic point IDs, so the store converges to one
// point per photo instead of duplicating them. The catalog side of a duplicate
// report is rejected as not_claimed, which is what keeps it first-wins.
func TestApplyEmbeddingResultsReembedOverwritesSamePoint(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a", catalog.Photo{Index: 0, Name: "a.jpg", Hash: "hash-x", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b.jpg", Hash: "hash-x", Width: 2, Height: 2, Ratio: 1})

	if _, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}
	applied, _, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: dimVector(1)},
	})
	if err != nil || applied != 1 {
		t.Fatalf("first apply: err=%v applied=%d", err, applied)
	}

	// A late duplicate report of the same hash (no fresh claim — the blob is
	// already terminal): the store upsert is harmless, the catalog says no.
	applied, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-x", Status: catalog.EmbeddingStatusReady, Vector: dimVector(3)},
	})
	if err != nil || applied != 0 {
		t.Fatalf("duplicate apply: err=%v applied=%d", err, applied)
	}
	if len(rejected) != 1 || rejected[0].Reason != catalog.EmbeddingRejectNotClaimed {
		t.Fatalf("rejected=%+v want one not_claimed rejection", rejected)
	}

	points, _ := store.snapshot()
	if len(points) != 2 {
		t.Fatalf("duplicate report duplicated points: %+v", points)
	}
	for _, record := range points {
		if record.Vector[0] != 3 {
			t.Fatalf("point %+v kept the stale first vector", record)
		}
	}
}

// TestApplyEmbeddingResultsSkipsOrphanHashesAndFailures: a ready hash no photo
// references has no (album, idx) to become a point — its bookkeeping still
// lands — and failed results never touch the store.
func TestApplyEmbeddingResultsSkipsOrphanHashesAndFailures(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-orphan")
	seedBlob(t, cat, "hash-doomed")
	if _, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}

	applied, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-orphan", Status: catalog.EmbeddingStatusReady, Vector: dimVector(1)},
		{Hash: "hash-doomed", Status: catalog.EmbeddingStatusFailed, Error: "decode image: boom"},
	})
	if err != nil || applied != 2 || len(rejected) != 0 {
		t.Fatalf("apply: err=%v applied=%d rejected=%+v", err, applied, rejected)
	}
	points, ops := store.snapshot()
	if len(points) != 0 || len(ops) != 0 {
		t.Fatalf("no points may exist without photos: points=%+v ops=%v", points, ops)
	}
	blob, err := cat.GetBlob(ctx, "hash-orphan")
	if err != nil || blob.EmbeddingStatus != catalog.EmbeddingStatusReady {
		t.Fatalf("orphan blob=%+v err=%v want ready bookkeeping", blob, err)
	}
}

func TestExternalWorkerReleaseAndFailedReport(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})

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
	blob, err := cat.GetBlob(ctx, "hash-x")
	if err != nil || blob.EmbeddingStatus != catalog.EmbeddingStatusFailed {
		t.Fatalf("blob=%+v err=%v want failed", blob, err)
	}
}

// A buggy worker can pad the hash with whitespace: the service trims it, so the
// store points and the catalog row land under the same clean hash.
func TestExternalWorkerAppliesTrimmedHash(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})
	seedBlob(t, cat, "hash-y")
	seedAlbum(t, cat, "album-b",
		catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-y", Width: 10, Height: 20, Ratio: 0.5})
	seedReadyEmbedding(t, svc, "hash-y", dimVector(1))

	if _, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}
	applied, rejected, err := svc.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: " hash-x ", Status: catalog.EmbeddingStatusReady, Vector: dimVector(3)},
	})
	if err != nil || applied != 1 || len(rejected) != 0 {
		t.Fatalf("apply: err=%v applied=%d rejected=%+v", err, applied, rejected)
	}

	// The store knows the trimmed hash: the cross-album neighbor query works,
	// which it would not if the padded hash had poisoned the points.
	result, err := svc.Recommend(ctx, "album-b", 0, 5)
	if err != nil {
		t.Fatalf("recommend: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Hash != "hash-x" {
		t.Fatalf("items=%+v want the trimmed-hash neighbor", result.Items)
	}
}

// The service applies the first result per hash in a batch and rejects the
// rest; the store must receive the first submission's vector, not the last.
func TestExternalWorkerDuplicateHashIsFirstWins(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})
	seedBlob(t, cat, "hash-y")
	seedAlbum(t, cat, "album-b",
		catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-y", Width: 10, Height: 20, Ratio: 0.5})
	seedReadyEmbedding(t, svc, "hash-y", dimVector(1))

	if _, _, err := svc.ClaimEmbeddings(ctx, 0); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// ready first, failed second: the catalog stores ready, so the store must
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
	svc := newTestService(t, cat, newFakeVectorStore(), nil)
	ctx := context.Background()

	seedBlob(t, cat, "hash-x")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-x", Width: 10, Height: 20, Ratio: 0.5})

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

// ---------------------------------------------------------------------------
// ReloadAlbum
// ---------------------------------------------------------------------------

// TestReloadAlbumSyncsStoreAfterReextraction covers the album-ready callback:
// re-extraction gave album-a a photo whose blob was already embedded elsewhere.
// The reload deletes the album's old points, reads back the album's ready
// pairs, retrieves their vectors from the store, and re-upserts — in that
// order — while blobs that are not ready stay pointless.
func TestReloadAlbumSyncsStoreAfterReextraction(t *testing.T) {
	cat := newTestCatalog(t)
	store := newFakeVectorStore()
	svc := newTestService(t, cat, store, nil)
	ctx := context.Background()

	// hash-b is embedded under album-b first; hash-old belonged to album-a.
	seedBlob(t, cat, "hash-old")
	seedBlob(t, cat, "hash-b")
	seedBlob(t, cat, "hash-pending")
	seedAlbum(t, cat, "album-a",
		catalog.Photo{Index: 0, Name: "a0.jpg", Hash: "hash-old", Width: 1, Height: 1, Ratio: 1})
	seedAlbum(t, cat, "album-b", catalog.Photo{Index: 0, Name: "b0.jpg", Hash: "hash-b", Width: 5, Height: 5, Ratio: 1})
	seedReadyEmbedding(t, svc, "hash-old", vec768(1, 0))
	seedReadyEmbedding(t, svc, "hash-b", vec768(0, 1))

	// Re-extraction replaces album-a's photo set: the old hash is gone, the
	// shared ready blob and a still-pending blob take its place.
	if err := cat.DeletePhotos(ctx, "album-a"); err != nil {
		t.Fatalf("delete photos: %v", err)
	}
	if err := cat.InsertPhoto(ctx, catalog.Photo{AlbumID: "album-a", Index: 0, Name: "new.jpg", Hash: "hash-b", Width: 5, Height: 5, Ratio: 1}); err != nil {
		t.Fatalf("insert new photo: %v", err)
	}
	if err := cat.InsertPhoto(ctx, catalog.Photo{AlbumID: "album-a", Index: 1, Name: "pending.jpg", Hash: "hash-pending", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert pending photo: %v", err)
	}

	store.resetOps()
	if err := svc.ReloadAlbum(ctx, "album-a"); err != nil {
		t.Fatalf("ReloadAlbum: %v", err)
	}

	// Exactly the documented order: delete, then retrieve (only the ready
	// hashes are looked up — the pending photo is filtered before the store is
	// asked), then upsert.
	_, ops := store.snapshot()
	wantOps := []string{"delete:album-a", "retrieve:1", "upsert:1"}
	if len(ops) != len(wantOps) {
		t.Fatalf("ops=%v want %v", ops, wantOps)
	}
	for i, op := range wantOps {
		if ops[i] != op {
			t.Fatalf("op %d = %q, want %q (all ops %v)", i, ops[i], op, ops)
		}
	}

	points, _ := store.snapshot()
	// album-a's stale point is gone and replaced by the shared blob's vector;
	// the pending photo has no point. album-b is untouched.
	if _, ok := points[qdrant.PointID("album-a", 0)]; !ok {
		t.Fatalf("album-a idx0 missing after reload: %+v", points)
	}
	if _, ok := points[qdrant.PointID("album-a", 1)]; ok {
		t.Fatalf("pending photo must not get a point: %+v", points)
	}
	reloaded := points[qdrant.PointID("album-a", 0)]
	if reloaded.Hash != "hash-b" || reloaded.AlbumID != "album-a" || reloaded.Idx != 0 {
		t.Fatalf("unexpected reloaded point: %+v", reloaded)
	}
	if !approxEqual(cosine(reloaded.Vector, vec768(0, 1)), 1) {
		t.Fatalf("reloaded vector is not the retrieved one: %+v", reloaded.Vector)
	}
	if _, ok := points[qdrant.PointID("album-b", 0)]; !ok {
		t.Fatalf("ReloadAlbum(album-a) must not touch album-b: %+v", points)
	}

	// A blank id and an unknown album are no-ops.
	if err := svc.ReloadAlbum(ctx, "   "); err != nil {
		t.Fatalf("ReloadAlbum(blank): %v", err)
	}
	if err := svc.ReloadAlbum(ctx, "album-missing"); err != nil {
		t.Fatalf("ReloadAlbum(album-missing): %v", err)
	}
}

// TestReloadAlbumWithoutDependenciesIsNoop: a service without a catalog or
// vector store has nothing to sync and must stay silent.
func TestReloadAlbumWithoutDependenciesIsNoop(t *testing.T) {
	svc := newTestService(t, nil, nil, nil)
	if err := svc.ReloadAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("ReloadAlbum without dependencies: %v", err)
	}
}
