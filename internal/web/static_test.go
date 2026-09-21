package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerCachingPolicy pins the split: hashed bundle assets are immutable,
// everything the SPA entry can reach must be rechecked so a redeployed binary
// is picked up on the next navigation.
func TestHandlerCachingPolicy(t *testing.T) {
	handler := Handler()

	assets, err := fs.Glob(staticFiles, "static/assets/*")
	if err != nil {
		t.Fatalf("glob assets: %v", err)
	}
	if len(assets) == 0 {
		t.Fatalf("expected bundled assets to exist")
	}
	assetPath := "/" + strings.TrimPrefix(assets[0], "static/")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, assetPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("asset %s status=%d", assetPath, rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("asset cache-control=%q", got)
	}

	for _, path := range []string{"/", "/albums/find", "/upload"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("path %s status=%d", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
			t.Fatalf("path %s did not serve the SPA entry", path)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("path %s cache-control=%q want=no-cache", path, got)
		}
	}
}
