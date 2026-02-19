package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
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

func requestWithURLParam(key string, value string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/image/album/0", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}
