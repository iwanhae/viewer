package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"viewer/internal/albums"
	"viewer/internal/catalog"
	"viewer/internal/feed"
	"viewer/internal/models"
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
	router := New(nil, testFeedService(), nil, nil, "").Router()
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
	router := New(nil, testFeedService(), nil, nil, "").Router()
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
	router := New(nil, testFeedService(), nil, nil, "").Router()

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
// distinguishable.
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
	if err := cat.SetBlobEmbedding(ctx, "hash-a", catalog.EmbeddingStatusReady, make([]float32, catalog.EmbeddingDim), ""); err != nil {
		t.Fatalf("set ready embedding: %v", err)
	}
	if err := cat.SetBlobEmbedding(ctx, "hash-b", catalog.EmbeddingStatusFailed, nil, "embed failed"); err != nil {
		t.Fatalf("set failed embedding: %v", err)
	}
}

func TestEmbeddingEndpointReportsCatalogCoverage(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	seedEmbeddingFixture(t, cat)

	// The provider is nil, so the counts are real but nothing can ever move
	// them: a client has to be told that instead of waiting forever.
	router := New(nil, nil, nil, recommend.NewService(cat, nil, nil, nil), "").Router()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/embedding", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var progress recommend.EmbeddingProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &progress); err != nil {
		t.Fatalf("decode progress: %v body=%s", err, rec.Body.String())
	}
	if progress.Enabled || progress.Active {
		t.Fatalf("expected Enabled=false and Active=false without a provider: %+v", progress)
	}
	if progress.Total != 3 || progress.Ready != 1 || progress.Failed != 1 || progress.Pending != 1 {
		t.Fatalf("progress=%+v want total=3 ready=1 failed=1 pending=1", progress)
	}
	if math.Abs(progress.Ratio-1.0/3.0) > 1e-6 {
		t.Fatalf("ratio=%v want %v", progress.Ratio, 1.0/3.0)
	}
}

func TestEmbeddingEndpointWithoutRecommendService(t *testing.T) {
	router := New(nil, nil, nil, nil, "").Router()
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/embedding", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("expected a disabled progress payload, got: %s", rec.Body.String())
	}
}

func TestMetricsEndpointPrometheusPayload(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer cat.Close()
	seedEmbeddingFixture(t, cat)

	recommendService := recommend.NewService(cat, nil, nil, nil)

	router := New(nil, nil, nil, recommendService, "").Router()
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
	router := New(nil, nil, nil, nil, "").Router()
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
	router := New(nil, nil, nil, nil, "").Router()
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
