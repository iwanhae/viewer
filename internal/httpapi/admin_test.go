package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"viewer/internal/admin"
	"viewer/internal/catalog"
)

// stubAdminVectorCounter stands in for the Qdrant client, letting a test fix
// the "actual points" half of the drift figure.
type stubAdminVectorCounter struct{ points int }

func (c stubAdminVectorCounter) CountVectors(ctx context.Context) (int, error) {
	return c.points, nil
}

func openAdminTestCatalog(t *testing.T) *catalog.Store {
	t.Helper()
	store, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func basicAuthHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func doAdminRequest(t *testing.T, router http.Handler, method, target, authorization string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestAdminRoutesRequireBothServiceAndToken pins the disabled shape: without a
// service or without a token the /admin routes are not registered at all, and
// neither the SPA catch-all nor the SPA fallback may dress the miss up as the
// frontend's index.html.
func TestAdminRoutesRequireBothServiceAndToken(t *testing.T) {
	adminService := admin.NewService(openAdminTestCatalog(t), nil, nil)
	routers := map[string]http.Handler{
		"nil service, empty token": New(nil, nil, nil, nil, "", nil, "").Router(),
		"service but no token":     New(nil, nil, nil, nil, "", adminService, "").Router(),
		"nil service with a token": New(nil, nil, nil, nil, "", nil, "sekrit").Router(),
	}
	requests := []struct {
		method string
		target string
	}{
		{http.MethodGet, "/admin"},
		{http.MethodGet, "/admin/"},
		{http.MethodGet, "/admin/api/stats"},
		{http.MethodPost, "/admin/api/reindex"},
	}
	for name, router := range routers {
		for _, item := range requests {
			rec := doAdminRequest(t, router, item.method, item.target, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s: %s %s status=%d want=404 body=%s", name, item.method, item.target, rec.Code, rec.Body.String())
			}
			if !jsonBodyHasErrorCode(rec.Body.String(), "NOT_FOUND") {
				t.Fatalf("%s: %s %s body=%s want NOT_FOUND error code", name, item.method, item.target, rec.Body.String())
			}
		}
	}
}

func jsonBodyHasErrorCode(body, code string) bool {
	var parsed errorBody
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return false
	}
	return parsed.Error.Code == code
}

// newAdminTestRouter builds a router over a seeded catalog with the admin
// surface enabled under "sekrit", in the shape production wires it.
func newAdminTestRouter(t *testing.T, vectorPoints int) (http.Handler, *catalog.Store) {
	t.Helper()
	store := openAdminTestCatalog(t)
	seedEmbeddingFixture(t, store)
	adminService := admin.NewService(store, stubAdminVectorCounter{points: vectorPoints}, nil).WithEncodingEnabled(true)
	router := New(nil, nil, nil, nil, "worker-token", adminService, "sekrit").Router()
	return router, store
}

// TestAdminAuthChallenges covers the Basic auth contract: every wrong shape of
// credentials is the same 401 with a challenge, and only the right password
// gets through. The username is ignored by design.
func TestAdminAuthChallenges(t *testing.T) {
	router, _ := newAdminTestRouter(t, 1)

	unauthorized := []struct {
		name          string
		authorization string
	}{
		{"no header", ""},
		{"malformed base64", "Basic !!!not-base64!!!"},
		{"wrong scheme", "Bearer sekrit"},
		{"no colon in credentials", "Basic " + base64.StdEncoding.EncodeToString([]byte("sekrit"))},
		{"empty password", "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:"))},
		{"wrong password", basicAuthHeader("admin", "wrong")},
		{"extra colon is part of the password", "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:sekrit:extra"))},
	}
	for _, item := range unauthorized {
		for _, route := range []struct{ method, target string }{
			{http.MethodGet, "/admin/api/stats"},
			{http.MethodPost, "/admin/api/reindex"},
		} {
			rec := doAdminRequest(t, router, route.method, route.target, item.authorization)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: %s %s status=%d want=401 body=%s", item.name, route.method, route.target, rec.Code, rec.Body.String())
			}
			if challenge := rec.Header().Get("WWW-Authenticate"); challenge != `Basic realm="viewer admin"` {
				t.Fatalf("%s: WWW-Authenticate=%q want %q", item.name, challenge, `Basic realm="viewer admin"`)
			}
			if !jsonBodyHasErrorCode(rec.Body.String(), "UNAUTHORIZED") {
				t.Fatalf("%s: body=%s want UNAUTHORIZED error code", item.name, rec.Body.String())
			}
		}
	}

	// The correct password gets through; the username carries no meaning.
	rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/stats", basicAuthHeader("operator", "sekrit"))
	if rec.Code != http.StatusOK {
		t.Fatalf("correct password status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminPageServesEmbeddedHTML checks that the dashboard itself comes back
// as self-contained HTML once authenticated.
func TestAdminPageServesEmbeddedHTML(t *testing.T) {
	router, _ := newAdminTestRouter(t, 1)

	rec := doAdminRequest(t, router, http.MethodGet, "/admin", basicAuthHeader("operator", "sekrit"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200", rec.Code)
	}
	contentType := rec.Header().Get("Content-Type")
	if contentType != "text/html; charset=utf-8" {
		t.Fatalf("content type=%q want text/html; charset=utf-8", contentType)
	}
	body := rec.Body.String()
	for _, fragment := range []string{"Re-embed everything", "WebP encoding", "/admin/api/stats", "/admin/api/reindex"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("admin page missing %q", fragment)
		}
	}
}

// TestAdminStatsShape pins the JSON contract the page renders: camelCase keys,
// the embedding block nested as an object, and the drift derived from the two
// point counts. seedEmbeddingFixture leaves one ready blob (one ready photo
// pair), one failed and one pending; the stub reports 3 Qdrant points.
func TestAdminStatsShape(t *testing.T) {
	router, _ := newAdminTestRouter(t, 3)

	rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/stats", basicAuthHeader("operator", "sekrit"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	for _, key := range []string{"albums", "albumsByStatus", "photos", "blobs", "embedding", "encoding", "expectedPoints", "qdrantPoints", "drift", "vectorStoreEnabled", "modelEnabled"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("stats JSON missing key %q in %v", key, payload)
		}
	}
	if payload["albums"] != float64(1) || payload["photos"] != float64(3) || payload["blobs"] != float64(3) {
		t.Fatalf("library counts=%v want albums=1 photos=3 blobs=3", payload)
	}
	byStatus, ok := payload["albumsByStatus"].(map[string]any)
	if !ok || byStatus["SUCCEEDED"] != float64(1) {
		t.Fatalf("albumsByStatus=%v want {SUCCEEDED:1}", payload["albumsByStatus"])
	}
	embedding, ok := payload["embedding"].(map[string]any)
	if !ok {
		t.Fatalf("embedding=%v want an object", payload["embedding"])
	}
	wantEmbedding := map[string]any{"total": float64(3), "ready": float64(1), "failed": float64(1), "processing": float64(0), "pending": float64(1)}
	for key, want := range wantEmbedding {
		if embedding[key] != want {
			t.Fatalf("embedding.%s=%v want=%v", key, embedding[key], want)
		}
	}
	if payload["expectedPoints"] != float64(1) || payload["qdrantPoints"] != float64(3) || payload["drift"] != float64(2) {
		t.Fatalf("point counts=%v want expected=1 qdrant=3 drift=2", payload)
	}
	if payload["modelEnabled"] != false {
		t.Fatalf("modelEnabled=%v want=false", payload["modelEnabled"])
	}
	encoding, ok := payload["encoding"].(map[string]any)
	if !ok {
		t.Fatalf("encoding=%v want an object", payload["encoding"])
	}
	if encoding["enabled"] != true || encoding["candidates"] != float64(3) || encoding["pending"] != float64(3) || encoding["remainingBytes"] != float64(3) {
		t.Fatalf("encoding stats=%v want enabled with three pending one-byte candidates", encoding)
	}
	if payload["vectorStoreEnabled"] != true {
		t.Fatalf("vectorStoreEnabled=%v want=true (a vector counter is wired)", payload["vectorStoreEnabled"])
	}
}

// TestAdminStatsShapeWithDisabledVectorStore pins the JSON shape when no
// vector store is wired up — the deployment without Qdrant: the flag says so
// explicitly, the point figures degrade to unmeasured zeros instead of a
// fabricated drift, and expectedPoints keeps its real catalog value.
func TestAdminStatsShapeWithDisabledVectorStore(t *testing.T) {
	store := openAdminTestCatalog(t)
	seedEmbeddingFixture(t, store)
	adminService := admin.NewService(store, nil, nil)
	router := New(nil, nil, nil, nil, "", adminService, "sekrit").Router()

	rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/stats", basicAuthHeader("operator", "sekrit"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		ExpectedPoints     int64 `json:"expectedPoints"`
		QdrantPoints       int64 `json:"qdrantPoints"`
		Drift              int64 `json:"drift"`
		VectorStoreEnabled bool  `json:"vectorStoreEnabled"`
		Encoding           struct {
			Enabled bool `json:"enabled"`
		} `json:"encoding"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if payload.VectorStoreEnabled {
		t.Fatalf("vectorStoreEnabled=true want=false (nil vector store)")
	}
	if payload.QdrantPoints != 0 || payload.Drift != 0 {
		t.Fatalf("points=%+v want qdrant=0 drift=0", payload)
	}
	if payload.ExpectedPoints != 1 {
		t.Fatalf("expectedPoints=%d want=1 (the fixture's one ready photo pair)", payload.ExpectedPoints)
	}
	if payload.Encoding.Enabled {
		t.Fatal("encoding.enabled=true want=false without WORKER_TOKEN")
	}
}

// TestAdminReindexResetsThroughAPI drives the operator's recovery button end
// to end: the endpoint answers with the post-reset counts and the reset really
// landed in the catalog behind it.
func TestAdminReindexResetsThroughAPI(t *testing.T) {
	router, store := newAdminTestRouter(t, 1)
	ctx := context.Background()

	before, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("counts before reindex: %v", err)
	}
	if before.Ready != 1 || before.Failed != 1 || before.Pending != 1 {
		t.Fatalf("fixture counts=%+v want ready=1 failed=1 pending=1", before)
	}

	rec := doAdminRequest(t, router, http.MethodPost, "/admin/api/reindex", basicAuthHeader("operator", "sekrit"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var counts catalog.EmbeddingCounts
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil {
		t.Fatalf("decode reindex response: %v", err)
	}
	want := catalog.EmbeddingCounts{Total: 3, Pending: 3}
	if counts != want {
		t.Fatalf("response counts=%+v want=%+v", counts, want)
	}

	after, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("counts after reindex: %v", err)
	}
	if after != want {
		t.Fatalf("catalog counts=%+v want=%+v", after, want)
	}
	pairs, err := store.ReadyPairCount(ctx)
	if err != nil || pairs != 0 {
		t.Fatalf("ready pairs=%d err=%v want=0 after reset", pairs, err)
	}
}
