package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"viewer/internal/albums"
	"viewer/internal/catalog"
	"viewer/internal/feed"
	"viewer/internal/models"
	"viewer/internal/qdrant"
	"viewer/internal/recommend"
)

func TestParseOptionalIntQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/feed?limit=25", nil)
	limit, err := parseOptionalIntQuery(req, "limit", 80, 1, 200)
	if err != nil {
		t.Fatalf("parseOptionalIntQuery returned err: %v", err)
	}
	if limit != 25 {
		t.Fatalf("limit=%d want=25", limit)
	}

	defaultReq := httptest.NewRequest(http.MethodGet, "/api/feed", nil)
	defaultLimit, err := parseOptionalIntQuery(defaultReq, "limit", 80, 1, 200)
	if err != nil {
		t.Fatalf("parseOptionalIntQuery default returned err: %v", err)
	}
	if defaultLimit != 80 {
		t.Fatalf("default limit=%d want=80", defaultLimit)
	}

	invalidReq := httptest.NewRequest(http.MethodGet, "/api/feed?limit=0", nil)
	if _, err := parseOptionalIntQuery(invalidReq, "limit", 80, 1, 200); err == nil {
		t.Fatalf("expected error for out-of-range limit")
	}

	tooLargeReq := httptest.NewRequest(http.MethodGet, "/api/feed?limit=201", nil)
	if _, err := parseOptionalIntQuery(tooLargeReq, "limit", 80, 1, 200); err == nil {
		t.Fatalf("expected error for too large limit")
	}
}

func TestParseNonNegativePathIntParam(t *testing.T) {
	req := requestWithURLParam("index", "42")
	idx, err := parseNonNegativePathIntParam(req, "index")
	if err != nil {
		t.Fatalf("parseNonNegativePathIntParam err: %v", err)
	}
	if idx != 42 {
		t.Fatalf("idx=%d want=42", idx)
	}

	negativeReq := requestWithURLParam("index", "-1")
	if _, err := parseNonNegativePathIntParam(negativeReq, "index"); err == nil {
		t.Fatalf("expected negative index to fail")
	}
}

func TestJSONBodyRejectsTrailingContent(t *testing.T) {
	type requestBody struct {
		Name string `json:"name"`
	}

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/albums",
		bytes.NewBufferString(`{"name":"ok"}{"extra":true}`),
	)

	var body requestBody
	err := jsonBody(req, &body)
	if err == nil {
		t.Fatalf("expected trailing content to fail")
	}
}

func TestFeedEndpointRejectsInvalidMode(t *testing.T) {
	router := New(nil, testFeedService(), nil, nil, "", nil, "").Router()
	req := httptest.NewRequest(http.MethodGet, "/api/feed?mode=invalid", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"INVALID_REQUEST\"") {
		t.Fatalf("expected INVALID_REQUEST error code in body, got: %s", rec.Body.String())
	}
}

func TestFeedEndpointSupportsLatestMode(t *testing.T) {
	router := New(nil, testFeedService(), nil, nil, "", nil, "").Router()
	req := httptest.NewRequest(http.MethodGet, "/api/feed?limit=25&mode=latest", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"items\"") {
		t.Fatalf("expected items envelope in body, got: %s", rec.Body.String())
	}
}

func TestFeedEndpointLatestSupportsAfterCursor(t *testing.T) {
	router := New(nil, testFeedService(), nil, nil, "", nil, "").Router()

	firstReq := httptest.NewRequest(http.MethodGet, "/api/feed?limit=1&mode=latest", nil)
	firstRec := httptest.NewRecorder()
	router.ServeHTTP(firstRec, firstReq)
	if firstRec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", firstRec.Code, http.StatusOK, firstRec.Body.String())
	}

	var first models.FeedResponse
	if err := json.Unmarshal(firstRec.Body.Bytes(), &first); err != nil {
		t.Fatalf("unmarshal first response: %v", err)
	}
	if len(first.Items) != 0 {
		t.Fatalf("expected empty feed for empty album service, got items=%d", len(first.Items))
	}

	secondReq := httptest.NewRequest(http.MethodGet, "/api/feed?limit=1&mode=latest&after=not-a-real-cursor", nil)
	secondRec := httptest.NewRecorder()
	router.ServeHTTP(secondRec, secondReq)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", secondRec.Code, http.StatusOK, secondRec.Body.String())
	}

	var second models.FeedResponse
	if err := json.Unmarshal(secondRec.Body.Bytes(), &second); err != nil {
		t.Fatalf("unmarshal second response: %v", err)
	}
	if len(second.Items) != 0 {
		t.Fatalf("expected empty feed for empty album service, got items=%d", len(second.Items))
	}
}

func testFeedService() *feed.Service {
	return feed.NewService(albums.NewService(nil, nil, nil))
}

// seedEmbeddingFixture creates one ready album with three distinct blobs: one
// embedded, one failed and one still pending, so every count is non-zero and
// distinguishable. The terminal outcomes go through the public claim→apply
// path: the claim marks the blobs processing (with an already-expired lease,
// so they stay reclaimable), and the apply lands both results in one
// transaction, mirroring the ready vector into the vec0 index. hash-c is never
// applied for, so it counts as pending — counts derive pending as
// total-ready-failed, which processing blobs still fall under.
func seedEmbeddingFixture(t *testing.T, cat *catalog.Store) {
	t.Helper()

	ctx := context.Background()
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID:               "album-a",
		OriginalFilename: "album-a.zip",
		Status:           catalog.AlbumStatusReady,
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	photos := []catalog.Photo{
		{AlbumID: "album-a", Index: 0, Name: "a.jpg", Hash: "hash-a", Width: 10, Height: 10, Ratio: 1},
		{AlbumID: "album-a", Index: 1, Name: "b.jpg", Hash: "hash-b", Width: 10, Height: 10, Ratio: 1},
		{AlbumID: "album-a", Index: 2, Name: "c.jpg", Hash: "hash-c", Width: 10, Height: 10, Ratio: 1},
	}
	for _, photo := range photos {
		if err := cat.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
		if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: photo.Hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert blob: %v", err)
		}
	}
	claimed, err := cat.ClaimPendingEmbeddings(ctx, 64, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("claim pending embeddings: %v", err)
	}
	claimable := make(map[string]bool, len(claimed))
	for _, blob := range claimed {
		claimable[blob.Hash] = true
	}
	if !claimable["hash-a"] || !claimable["hash-b"] {
		t.Fatalf("claim pending embeddings: got %v, want hash-a and hash-b claimable", claimable)
	}
	applied, rejected, err := cat.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: "hash-a", Status: catalog.EmbeddingStatusReady, Vector: make([]float32, catalog.EmbeddingDim)},
		{Hash: "hash-b", Status: catalog.EmbeddingStatusFailed, Error: "embed failed"},
	})
	if err != nil {
		t.Fatalf("apply embedding results: %v", err)
	}
	if len(applied) != 2 || len(rejected) != 0 {
		t.Fatalf("apply embedding results: applied=%v rejected=%v", applied, rejected)
	}
	// The claim swept hash-c along with the two targets; hand it back so the
	// fixture's "still pending" blob really is pending, not processing.
	if err := cat.ReleaseEmbeddingClaims(ctx, []string{"hash-c"}); err != nil {
		t.Fatalf("release hash-c claim: %v", err)
	}
}

func TestMetricsEndpointPrometheusPayload(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	seedEmbeddingFixture(t, cat)

	recommendService := recommend.NewService(cat, nil, nil, nil, nil)

	router := New(nil, nil, nil, recommendService, "", nil, "").Router()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	contentType := rec.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") {
		t.Fatalf("content type=%q want text/plain", contentType)
	}

	body := rec.Body.String()
	required := []string{
		"viewer_embedding_images_total",
		"viewer_embedding_images_ready",
		"viewer_embedding_images_failed",
		"viewer_embedding_images_pending",
		"viewer_embedding_images_processing",
		"viewer_embedding_progress_ratio",
	}
	for _, metricName := range required {
		if !strings.Contains(body, metricName) {
			t.Fatalf("missing metric %q in payload:\n%s", metricName, body)
		}
	}

	mustMetricIntValue(t, body, "viewer_embedding_images_total", 3)
	mustMetricIntValue(t, body, "viewer_embedding_images_ready", 1)
	mustMetricIntValue(t, body, "viewer_embedding_images_failed", 1)
	mustMetricIntValue(t, body, "viewer_embedding_images_pending", 1)
	mustMetricIntValue(t, body, "viewer_embedding_images_processing", 0)
	mustMetricFloatValue(t, body, "viewer_embedding_progress_ratio", 1.0/3.0, 1e-6)
}

func TestMetricsEndpointWithNilRecommendService(t *testing.T) {
	router := New(nil, nil, nil, nil, "", nil, "").Router()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()
	mustMetricIntValue(t, body, "viewer_embedding_images_total", 0)
	mustMetricIntValue(t, body, "viewer_embedding_images_ready", 0)
	mustMetricIntValue(t, body, "viewer_embedding_images_failed", 0)
	mustMetricIntValue(t, body, "viewer_embedding_images_pending", 0)
	mustMetricFloatValue(t, body, "viewer_embedding_progress_ratio", 0, 1e-9)
}

func TestRecommendationsEndpointWithNilRecommendService(t *testing.T) {
	router := New(nil, nil, nil, nil, "", nil, "").Router()
	req := httptest.NewRequest(http.MethodGet, "/api/recommendations/album-a/0?limit=12", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"UNAVAILABLE\"") {
		t.Fatalf("expected UNAVAILABLE error code in body, got: %s", rec.Body.String())
	}
}

// TestRecommendationsEndpointWithDisabledVectorStore covers the Qdrant-less
// deployment: a live recommend service whose vector store is nil must answer
// with the same 503 UNAVAILABLE shape as a nil service, since clients have no
// way to tell the two configurations apart and should not have to. The
// nil-store check precedes the photo lookup, so no fixture is needed.
func TestRecommendationsEndpointWithDisabledVectorStore(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	recommendService := recommend.NewService(cat, nil, nil, nil, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	req := httptest.NewRequest(http.MethodGet, "/api/recommendations/album-a/0?limit=12", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"UNAVAILABLE\"") {
		t.Fatalf("expected UNAVAILABLE error code in body, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recommendations are not available") {
		t.Fatalf("expected the disabled message in body, got: %s", rec.Body.String())
	}
}

func TestSearchEndpointWithNilRecommendService(t *testing.T) {
	router := New(nil, nil, nil, nil, "", nil, "").Router()
	req := httptest.NewRequest(http.MethodGet, "/api/photos/search?q=beach", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"UNAVAILABLE\"") {
		t.Fatalf("expected UNAVAILABLE error code in body, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "photo search is not available") {
		t.Fatalf("expected the disabled message in body, got: %s", rec.Body.String())
	}
}

// TestSearchEndpointWithDisabledVectorStore covers the Qdrant-less deployment:
// a live recommend service whose vector store is nil must answer with the same
// 503 UNAVAILABLE shape as a nil service. The nil-store check precedes the
// embedding, so no text-capable embedder or fixture is needed.
func TestSearchEndpointWithDisabledVectorStore(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	recommendService := recommend.NewService(cat, nil, nil, nil, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	req := httptest.NewRequest(http.MethodGet, "/api/photos/search?q=beach", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"UNAVAILABLE\"") {
		t.Fatalf("expected UNAVAILABLE error code in body, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "photo search is not available") {
		t.Fatalf("expected the disabled message in body, got: %s", rec.Body.String())
	}
}

// TestSearchEndpointValidatesQueryAndLimit pins the 400s the handler answers
// before the service is ever asked: a blank query and an out-of-range limit
// are bad requests, not empty results.
func TestSearchEndpointValidatesQueryAndLimit(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	recommendService := recommend.NewService(cat, nil, nil, nil, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	cases := []struct {
		name string
		url  string
	}{
		{name: "missing query parameter", url: "/api/photos/search"},
		{name: "whitespace query", url: "/api/photos/search?q=%20%20"},
		{name: "limit below the floor", url: "/api/photos/search?q=beach&limit=0"},
		{name: "limit above the ceiling", url: "/api/photos/search?q=beach&limit=97"},
		{name: "non-numeric limit", url: "/api/photos/search?q=beach&limit=lots"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "\"code\":\"INVALID_REQUEST\"") {
				t.Fatalf("expected INVALID_REQUEST error code in body, got: %s", rec.Body.String())
			}
		})
	}
}

// visionSearchEmbedder is the image-capable variant of the harness's stubs: it
// satisfies recommend.EmbeddingProvider with one fixed Embed vector, so the
// search-by-image endpoint can be driven end to end without a checkpoint. An
// optional err replaces the vector, shaped like the wraps the real embedder
// produces.
type visionSearchEmbedder struct {
	vector []float32
	err    error
}

func (e *visionSearchEmbedder) Load(context.Context) error { return nil }

func (e *visionSearchEmbedder) Embed(context.Context, []byte) ([]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.vector, nil
}

func (e *visionSearchEmbedder) Close() error { return nil }

// searchByImageBody builds a multipart body holding one file part named
// fieldName and returns it together with the Content-Type the request must
// carry, since the boundary lives there.
func searchByImageBody(t *testing.T, fieldName string, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile(fieldName, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &buf, writer.FormDataContentType()
}

// searchByImageRequest wraps a multipart body into a POST against the
// search-by-image route.
func searchByImageRequest(t *testing.T, url string, fieldName string, data []byte) *http.Request {
	t.Helper()
	body, contentType := searchByImageBody(t, fieldName, "query.png", data)
	req := httptest.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", contentType)
	return req
}

// TestSearchByImageEndpointReturnsRankedItems drives the image-search endpoint
// end to end: the upload is parsed out of the multipart body, embedded by the
// vision-capable stub, and the store ranks by cosine into the same response
// shape the text search answers with.
func TestSearchByImageEndpointReturnsRankedItems(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	seedEmbeddingFixture(t, cat)

	query := searchVector(1, 0.2)
	vectors := newFakeVectorStore()
	if err := vectors.UpsertPhotos(context.Background(), []qdrant.PhotoRecord{
		{AlbumID: "album-a", Idx: 0, Hash: "hash-a", W: 10, H: 10, Vector: searchVector(1, 0.2)},
		{AlbumID: "album-a", Idx: 1, Hash: "hash-b", W: 10, H: 10, Vector: searchVector(0, 1)},
		{AlbumID: "album-a", Idx: 2, Hash: "hash-c", W: 10, H: 10, Vector: searchVector(0.99, 0.02)},
	}); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	recommendService := recommend.NewService(cat, vectors, nil, &visionSearchEmbedder{vector: query}, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, searchByImageRequest(t, "/api/photos/search-by-image?limit=2", "image", testPNG(t, 4, 2, 10)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp recommend.RecommendationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Descending cosine against [1, 0.2]: hash-a is identical (score 1),
	// hash-c nearly parallel, hash-b nearly orthogonal and cut by the limit.
	if len(resp.Items) != 2 {
		t.Fatalf("items=%d want=2: %+v", len(resp.Items), resp.Items)
	}
	if resp.Items[0].Hash != "hash-a" || resp.Items[1].Hash != "hash-c" {
		t.Fatalf("unexpected ranking: %+v", resp.Items)
	}
	first := resp.Items[0]
	if first.AlbumID != "album-a" || first.I != 0 || first.W != 10 || first.H != 10 {
		t.Fatalf("unexpected item shape: %+v", first)
	}
	if first.Score < 0.9999 {
		t.Fatalf("score=%v want ~1 for an identical vector", first.Score)
	}
}

// TestSearchByImageEndpointRejectsMissingImage pins the 400s for an upload
// that carries no usable image part: the field absent altogether, and the
// field present but zero bytes, are both bad requests.
func TestSearchByImageEndpointRejectsMissingImage(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	recommendService := recommend.NewService(cat, newFakeVectorStore(), nil, &visionSearchEmbedder{vector: searchVector(1)}, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	cases := []struct {
		name     string
		field    string
		fromBody []byte
	}{
		{name: "no image part", field: "not-image", fromBody: testPNG(t, 2, 2, 5)},
		{name: "zero-byte image part", field: "image", fromBody: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, searchByImageRequest(t, "/api/photos/search-by-image", tc.field, tc.fromBody))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "\"code\":\"INVALID_REQUEST\"") {
				t.Fatalf("expected INVALID_REQUEST error code in body, got: %s", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "missing image") {
				t.Fatalf("expected the missing-image message in body, got: %s", rec.Body.String())
			}
		})
	}
}

// TestSearchByImageEndpointValidatesLimit pins the 400s the handler answers
// before the body is even parsed: an out-of-range limit is a bad request, not
// a clamped result.
func TestSearchByImageEndpointValidatesLimit(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	recommendService := recommend.NewService(cat, newFakeVectorStore(), nil, &visionSearchEmbedder{vector: searchVector(1)}, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	for _, limit := range []string{"0", "97", "lots"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, searchByImageRequest(t, "/api/photos/search-by-image?limit="+limit, "image", testPNG(t, 2, 2, 5)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("limit=%s status=%d want=%d body=%s", limit, rec.Code, http.StatusBadRequest, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "\"code\":\"INVALID_REQUEST\"") {
			t.Fatalf("expected INVALID_REQUEST error code in body, got: %s", rec.Body.String())
		}
	}
}

func TestSearchByImageEndpointWithNilRecommendService(t *testing.T) {
	router := New(nil, nil, nil, nil, "", nil, "").Router()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, searchByImageRequest(t, "/api/photos/search-by-image", "image", testPNG(t, 2, 2, 5)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"UNAVAILABLE\"") {
		t.Fatalf("expected UNAVAILABLE error code in body, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "photo search is not available") {
		t.Fatalf("expected the disabled message in body, got: %s", rec.Body.String())
	}
}

// TestSearchByImageEndpointWithDisabledVectorStore covers the Qdrant-less
// deployment: a live recommend service whose vector store is nil must answer
// with the same 503 UNAVAILABLE shape as a nil service.
func TestSearchByImageEndpointWithDisabledVectorStore(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	recommendService := recommend.NewService(cat, nil, nil, &visionSearchEmbedder{vector: searchVector(1)}, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, searchByImageRequest(t, "/api/photos/search-by-image", "image", testPNG(t, 2, 2, 5)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"UNAVAILABLE\"") {
		t.Fatalf("expected UNAVAILABLE error code in body, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "photo search is not available") {
		t.Fatalf("expected the disabled message in body, got: %s", rec.Body.String())
	}
}

// TestSearchByImageEndpointWithEmbedderFailure covers the "feature is off"
// side of the contract: an embedder that cannot run the vision tower is a
// deployment state, so the endpoint answers with the same 503 UNAVAILABLE
// shape as a disabled store instead of a 500.
func TestSearchByImageEndpointWithEmbedderFailure(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	stub := &visionSearchEmbedder{err: errors.New("graph exploded")}
	recommendService := recommend.NewService(cat, newFakeVectorStore(), nil, stub, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, searchByImageRequest(t, "/api/photos/search-by-image", "image", testPNG(t, 2, 2, 5)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"UNAVAILABLE\"") {
		t.Fatalf("expected UNAVAILABLE error code in body, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "photo search is not available") {
		t.Fatalf("expected the disabled message in body, got: %s", rec.Body.String())
	}
}

// TestSearchByImageEndpointWithUnreadableImage covers the 400 class: an
// embedder rejection carrying ErrUnreadableImage — the wrap the real
// preprocess path produces for a corrupt upload — reads as a bad request.
func TestSearchByImageEndpointWithUnreadableImage(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	stub := &visionSearchEmbedder{err: fmt.Errorf("preprocess image: %w", recommend.ErrUnreadableImage)}
	recommendService := recommend.NewService(cat, newFakeVectorStore(), nil, stub, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, searchByImageRequest(t, "/api/photos/search-by-image", "image", []byte("garbage bytes")))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"INVALID_REQUEST\"") {
		t.Fatalf("expected INVALID_REQUEST error code in body, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "image could not be decoded") {
		t.Fatalf("expected the unreadable message in body, got: %s", rec.Body.String())
	}
}

// TestSearchByImageEndpointRejectsOversizedUpload pins the bounded body: a
// payload past maxSearchImageBytes fails cleanly with its own 400 instead of
// being spooled out in full, and the embedder is never asked to run on it.
func TestSearchByImageEndpointRejectsOversizedUpload(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	recommendService := recommend.NewService(cat, newFakeVectorStore(), nil, &visionSearchEmbedder{vector: searchVector(1)}, nil)
	router := New(nil, nil, nil, recommendService, "", nil, "").Router()

	// The multipart framing alone pushes the total past the reader limit.
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, searchByImageRequest(t, "/api/photos/search-by-image", "image", make([]byte, maxSearchImageBytes)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"INVALID_REQUEST\"") {
		t.Fatalf("expected INVALID_REQUEST error code in body, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "image too large") {
		t.Fatalf("expected the too-large message in body, got: %s", rec.Body.String())
	}
}

func requestWithURLParam(key string, value string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/image/album/0", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func metricValue(body string, name string) (string, error) {
	targetPrefix := name + " "
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, targetPrefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, targetPrefix)), nil
		}
	}
	return "", fmt.Errorf("metric %s not found", name)
}

func mustMetricIntValue(t *testing.T, body string, name string, want int) {
	t.Helper()
	raw, err := metricValue(body, name)
	if err != nil {
		t.Fatalf("%v", err)
	}
	got, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("parse metric %s value %q as int: %v", name, raw, err)
	}
	if got != want {
		t.Fatalf("metric %s=%d want=%d", name, got, want)
	}
}

func mustMetricFloatValue(t *testing.T, body string, name string, want float64, tolerance float64) {
	t.Helper()
	raw, err := metricValue(body, name)
	if err != nil {
		t.Fatalf("%v", err)
	}
	got, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("parse metric %s value %q as float: %v", name, raw, err)
	}
	if math.Abs(got-want) > tolerance {
		t.Fatalf("metric %s=%f want=%f tolerance=%f", name, got, want, tolerance)
	}
}
