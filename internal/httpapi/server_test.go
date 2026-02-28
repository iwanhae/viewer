package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"viewer/internal/albums"
	cfgpkg "viewer/internal/config"
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
	router := New(nil, testFeedService(), nil, nil).Router()
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
	router := New(nil, testFeedService(), nil, nil).Router()
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
	router := New(nil, testFeedService(), nil, nil).Router()

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
	return feed.NewService(albums.NewService(cfgpkg.Config{}, nil, nil))
}

func TestMetricsEndpointPrometheusPayload(t *testing.T) {
	recommendService, err := recommend.NewService(
		cfgpkg.Config{
			RecommenderEndpoint:   "http://127.0.0.1:18081",
			RecommenderTimeoutSec: 1,
		},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("new recommend service: %v", err)
	}
	recommendService.IngestAlbumIndex(models.AlbumIndex{
		AlbumID: "album-a",
		Photos: []models.PhotoMeta{
			{I: 0, Name: "a.jpg"},
			{I: 1, Name: "b.jpg"},
			{I: 2, Name: "c.jpg"},
		},
		Embeddings: map[string]models.PhotoEmbedding{
			"0": {Status: "ready", Vector: []float32{1, 2, 3}},
			"1": {Status: "failed", Error: "embed failed"},
		},
	})

	router := New(nil, nil, nil, recommendService).Router()
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
		"viewer_embedding_images_processed",
		"viewer_embedding_progress_ratio",
		"viewer_embedding_progress_percent",
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
	mustMetricIntValue(t, body, "viewer_embedding_images_processed", 2)
	mustMetricFloatValue(t, body, "viewer_embedding_progress_ratio", 1.0/3.0, 1e-6)
	mustMetricFloatValue(t, body, "viewer_embedding_progress_percent", (100.0 / 3.0), 1e-4)
}

func TestMetricsEndpointWithNilRecommendService(t *testing.T) {
	router := New(nil, nil, nil, nil).Router()
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
	mustMetricIntValue(t, body, "viewer_embedding_images_processed", 0)
	mustMetricFloatValue(t, body, "viewer_embedding_progress_ratio", 0, 1e-9)
	mustMetricFloatValue(t, body, "viewer_embedding_progress_percent", 0, 1e-9)
}

func TestRecommendationsEndpointWithNilRecommendService(t *testing.T) {
	router := New(nil, nil, nil, nil).Router()
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
