package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"viewer/internal/admin"
	"viewer/internal/catalog"
)

// stubAdminVectorCounter stands in for the Qdrant client, letting a test fix
// the "actual points" half of the drift figure.
type stubAdminVectorCounter struct {
	points int
	err    error
}

func (c stubAdminVectorCounter) CountVectors(ctx context.Context) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
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
	return doAdminBodyRequest(t, router, method, target, authorization, nil)
}

// doAdminBodyRequest is doAdminRequest with a request body, for the action
// endpoints that take JSON. A nil body posts bodyless, the way the
// dashboard's buttons without options do.
func doAdminBodyRequest(t *testing.T, router http.Handler, method, target, authorization string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
		req.Header.Set("X-Admin-Action", "1")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
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
		{http.MethodGet, "/admin/assets/nonexistent.js"},
		{http.MethodGet, "/admin/photos/album-a/0"},
		{http.MethodGet, "/admin/runs"},
		{http.MethodGet, "/admin/api/overview"},
		{http.MethodGet, "/admin/api/issues"},
		{http.MethodGet, "/admin/api/extraction-failures?reasonCode=invalid_image"},
		{http.MethodGet, "/admin/api/photos"},
		{http.MethodGet, "/admin/api/photos/a/0"},
		{http.MethodGet, "/admin/api/blobs"},
		{http.MethodGet, "/admin/api/blobs/hash"},
		{http.MethodGet, "/admin/api/blobs/hash/history"},
		{http.MethodGet, "/admin/api/albums"},
		{http.MethodGet, "/admin/api/albums/a"},
		{http.MethodGet, "/admin/api/albums/a/extractions"},
		{http.MethodGet, "/admin/api/albums/a/entries"},
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
			{http.MethodGet, "/admin/api/overview"},
			{http.MethodGet, "/admin/api/photos"},
			{http.MethodGet, "/admin/api/photos/album-a/0"},
			{http.MethodGet, "/admin/api/blobs"},
			{http.MethodGet, "/admin/api/blobs/hash-a"},
			{http.MethodGet, "/admin/api/albums"},
			{http.MethodGet, "/admin/api/albums/album-a"},
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
	rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/overview", basicAuthHeader("operator", "sekrit"))
	if rec.Code != http.StatusOK {
		t.Fatalf("correct password status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminMutationCSRFProtection(t *testing.T) {
	router, _ := newAdminTestRouter(t, 1)
	auth := basicAuthHeader("operator", "sekrit")
	request := func(header, origin, fetchSite string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/recover", strings.NewReader(`{"target":"unknown"}`))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set("X-Admin-Action", header)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"missing custom header":     request("", "", ""),
		"cross origin":              request("1", "https://evil.example", "cross-site"),
		"same-site not same-origin": request("1", "", "same-site"),
	} {
		if rec.Code != http.StatusForbidden || !jsonBodyHasErrorCode(rec.Body.String(), "CSRF") {
			t.Fatalf("%s status=%d body=%s want CSRF 403", name, rec.Code, rec.Body.String())
		}
	}
	allowed := request("1", "http://example.com", "same-origin")
	if allowed.Code != http.StatusBadRequest || !jsonBodyHasErrorCode(allowed.Body.String(), "INVALID_REQUEST") {
		t.Fatalf("same-origin action status=%d body=%s want handler validation after CSRF", allowed.Code, allowed.Body.String())
	}
	proxied := httptest.NewRequest(http.MethodPost, "/admin/api/recover", strings.NewReader(`{"target":"unknown"}`))
	proxied.Host = "photos.example.com"
	proxied.Header.Set("Authorization", auth)
	proxied.Header.Set("Content-Type", "application/json")
	proxied.Header.Set("X-Admin-Action", "1")
	proxied.Header.Set("Origin", "https://photos.example.com")
	proxied.Header.Set("Sec-Fetch-Site", "same-origin")
	proxied.Header.Set("X-Forwarded-Proto", "https")
	proxiedRec := httptest.NewRecorder()
	router.ServeHTTP(proxiedRec, proxied)
	if proxiedRec.Code != http.StatusBadRequest || !jsonBodyHasErrorCode(proxiedRec.Body.String(), "INVALID_REQUEST") {
		t.Fatalf("same-origin HTTPS proxy action status=%d body=%s want handler validation after CSRF", proxiedRec.Code, proxiedRec.Body.String())
	}
	missingJSONType := httptest.NewRequest(http.MethodPost, "/admin/api/recover", strings.NewReader(`{"target":"unknown"}`))
	missingJSONType.Header.Set("Authorization", auth)
	missingJSONType.Header.Set("X-Admin-Action", "1")
	missingJSONTypeRec := httptest.NewRecorder()
	router.ServeHTTP(missingJSONTypeRec, missingJSONType)
	if missingJSONTypeRec.Code != http.StatusBadRequest || !strings.Contains(missingJSONTypeRec.Body.String(), "Content-Type must be application/json") {
		t.Fatalf("legacy recover without JSON Content-Type status=%d body=%s", missingJSONTypeRec.Code, missingJSONTypeRec.Body.String())
	}
	get := doAdminRequest(t, router, http.MethodGet, "/admin/api/overview", auth)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s; reads must not require the action header", get.Code, get.Body.String())
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
	for _, fragment := range []string{"admin-root", "/admin/assets/", "lang=\"ko\""} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("admin page missing %q", fragment)
		}
	}
	assetStart := strings.Index(body, "/admin/assets/")
	assetPath := body[assetStart:]
	if assetEnd := strings.IndexByte(assetPath, '"'); assetEnd >= 0 {
		assetPath = assetPath[:assetEnd]
	}
	assetUnauthorized := doAdminRequest(t, router, http.MethodGet, assetPath, "")
	if assetUnauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("admin asset %s status=%d want=401", assetPath, assetUnauthorized.Code)
	}
	assetAuthorized := doAdminRequest(t, router, http.MethodGet, assetPath, basicAuthHeader("operator", "sekrit"))
	if assetAuthorized.Code != http.StatusOK {
		t.Fatalf("admin asset %s status=%d want=200", assetPath, assetAuthorized.Code)
	}

	refresh := doAdminRequest(t, router, http.MethodGet, "/admin/photos/album-a/0", basicAuthHeader("operator", "sekrit"))
	if refresh.Code != http.StatusOK || !strings.Contains(refresh.Body.String(), `<div id="admin-root">`) {
		t.Fatalf("nested admin route status=%d body=%s", refresh.Code, refresh.Body.String())
	}

	for _, target := range []string{"/admin/photos/album-a/0", "/admin/blobs/hash", "/admin/albums/album-a"} {
		protected := doAdminRequest(t, router, http.MethodGet, target, "")
		if protected.Code != http.StatusUnauthorized {
			t.Errorf("nested admin route %s status=%d want=401", target, protected.Code)
		}
	}
}

func TestAdminSPADirectNavigationAndReload(t *testing.T) {
	router, _ := newAdminTestRouter(t, 1)
	for _, target := range []string{"/admin/issues", "/admin/blobs/hash-a"} {
		t.Run(target, func(t *testing.T) {
			unauthorized := doAdminRequest(t, router, http.MethodGet, target, "")
			if unauthorized.Code != http.StatusUnauthorized || unauthorized.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("unprotected SPA page: status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
			}
			for range 2 { // first navigation and browser reload
				page := doAdminRequest(t, router, http.MethodGet, target, basicAuthHeader("operator", "sekrit"))
				if page.Code != http.StatusOK || page.Header().Get("Content-Type") != "text/html; charset=utf-8" || !strings.Contains(page.Body.String(), `<div id="admin-root">`) {
					t.Fatalf("SPA page: status=%d type=%s body=%s", page.Code, page.Header().Get("Content-Type"), page.Body.String())
				}
			}
		})
	}
	// The retired diagnostics and local-runs pages are gone: their old URLs
	// must not resurrect the SPA shell, and unknown admin paths stay JSON 404s.
	for _, target := range []string{"/admin/unknown", "/admin/runs", "/admin/runs/run-id", "/admin/diagnostics", "/admin/api/unknown", "/admin/api/diagnostics"} {
		page := doAdminRequest(t, router, http.MethodGet, target, basicAuthHeader("operator", "sekrit"))
		if page.Code != http.StatusNotFound || !strings.HasPrefix(page.Header().Get("Content-Type"), "application/json") || !jsonBodyHasErrorCode(page.Body.String(), "NOT_FOUND") {
			t.Fatalf("unknown path %s did not return JSON 404: status=%d body=%s", target, page.Code, page.Body.String())
		}
	}
}

// TestAdminOverviewRouteJSONContractAndVectorDegradation pins the overview
// endpoint's shape and its graceful-degradation contract: a failing vector
// counter turns the health section's vectorCount "unavailable" without
// failing the request or leaking the underlying error, and the catalog
// measurements stay present and exact.
func TestAdminOverviewRouteJSONContractAndVectorDegradation(t *testing.T) {
	store := openAdminTestCatalog(t)
	seedEmbeddingFixture(t, store)
	service := admin.NewService(store, stubAdminVectorCounter{err: errors.New("qdrant secret endpoint")}, nil)
	router := New(nil, nil, nil, nil, "worker-token", service, "sekrit").Router()

	rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/overview", basicAuthHeader("operator", "sekrit"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "qdrant secret endpoint") {
		t.Fatalf("overview leaked the vector-store error: %s", body)
	}
	if strings.Contains(body, "skippedReasons") || strings.Contains(body, "rawStates") || strings.Contains(body, `"units"`) {
		t.Fatalf("overview still carries the retired prose/raw-state/skipped-reason fields: %s", body)
	}
	var payload struct {
		MeasuredAt string           `json:"measuredAt"`
		Library    map[string]int64 `json:"library"`
		Encoding   struct {
			Categories map[string]map[string]int64 `json:"categories"`
		} `json:"encoding"`
		Embedding struct {
			Categories map[string]map[string]int64 `json:"categories"`
		} `json:"embedding"`
		Health struct {
			EncodingExpiredLeases  int64 `json:"encodingExpiredLeases"`
			EmbeddingExpiredLeases int64 `json:"embeddingExpiredLeases"`
			UnreferencedBlobs      int64 `json:"unreferencedBlobs"`
			MissingBlobPhotos      int64 `json:"missingBlobPhotos"`
			EncodingEnabled        bool  `json:"encodingEnabled"`
			ModelEnabled           bool  `json:"modelEnabled"`
			VectorStoreEnabled     bool  `json:"vectorStoreEnabled"`
			VectorCount            struct {
				Status         string  `json:"status"`
				MeasuredAt     *string `json:"measuredAt"`
				ExpectedPoints int64   `json:"expectedPoints"`
				ActualPoints   *int64  `json:"actualPoints"`
				CountAgreement *bool   `json:"countAgreement"`
				Message        string  `json:"message"`
			} `json:"vectorCount"`
		} `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode overview: %v body=%s", err, body)
	}
	if _, err := time.Parse(time.RFC3339Nano, payload.MeasuredAt); err != nil {
		t.Fatalf("measuredAt=%q is not RFC3339: %v", payload.MeasuredAt, err)
	}
	if payload.Library["albums"] != 1 || payload.Library["photos"] != 3 || payload.Library["blobs"] != 3 || len(payload.Library) != 3 {
		t.Fatalf("library=%v want only albums=1 photos=3 blobs=3", payload.Library)
	}
	for _, groups := range []map[string]map[string]int64{
		payload.Encoding.Categories, payload.Embedding.Categories,
	} {
		for label, counts := range groups {
			if len(counts) != 1 || counts["blobCount"] < 0 {
				t.Errorf("%s overview group has non-blob keys: %v", label, counts)
			}
		}
	}
	for _, category := range []string{"queued", "processing", "validating", "committing", "completed", "skipped", "failed", "unknown"} {
		assertOverviewJSONCounts(t, "encoding.categories."+category, payload.Encoding.Categories[category])
	}
	for _, category := range []string{"queued", "processing", "completed", "failed", "unknown"} {
		assertOverviewJSONCounts(t, "embedding.categories."+category, payload.Embedding.Categories[category])
	}
	if _, ok := payload.Embedding.Categories["blocked"]; ok {
		t.Fatal("overview still exposes the obsolete blocked embedding category")
	}

	// The health section: the seed fixture has exactly one embedding-ready
	// blob behind one photo, no unreferenced blobs or dangling photos, and no
	// expired leases; the counter fails, so the vector verdict is unavailable.
	health := payload.Health
	if health.UnreferencedBlobs != 0 || health.MissingBlobPhotos != 0 || health.EncodingExpiredLeases != 0 || health.EmbeddingExpiredLeases != 0 {
		t.Fatalf("health counts=%+v want the seed fixture's zeros", health)
	}
	if health.VectorCount.ExpectedPoints != 1 {
		t.Fatalf("expectedPoints=%d want 1 (the one ready blob's photo row)", health.VectorCount.ExpectedPoints)
	}
	if health.VectorCount.Status != "unavailable" || health.VectorCount.ActualPoints != nil || health.VectorCount.CountAgreement != nil || health.VectorCount.MeasuredAt != nil {
		t.Fatalf("unavailable vector count=%+v; unmeasured fields must stay null", health.VectorCount)
	}
	if health.VectorCount.Message == "" {
		t.Fatalf("unavailable vector count needs a sanitized message: %+v", health.VectorCount)
	}
	if !health.VectorStoreEnabled {
		t.Fatalf("a wired vector counter must report vectorStoreEnabled: %+v", health)
	}
	if health.EncodingEnabled || health.ModelEnabled {
		t.Fatalf("feature flags=%+v want encodingEnabled=false modelEnabled=false (nothing wired)", health)
	}
}

func TestAdminResourceBrowseRoutesContractAndValidation(t *testing.T) {
	store := openAdminTestCatalog(t)
	ctx := context.Background()
	for _, album := range []catalog.Album{
		{ID: "album-a", OriginalFilename: "alpha.zip", Status: catalog.AlbumStatusFailed, Error: "extract failed"},
		{ID: "album-b", OriginalFilename: "beta.zip", Status: catalog.AlbumStatusReady},
	} {
		if err := store.CreateAlbum(ctx, album); err != nil {
			t.Fatalf("create album: %v", err)
		}
	}
	blobs := []catalog.Blob{
		{Hash: "shared", SizeBytes: 31, ContentType: "image/jpeg"},
		{Hash: "blocked", SizeBytes: 32, ContentType: "image/png"},
		{Hash: "orphan", SizeBytes: 33, ContentType: "image/jpeg"},
	}
	for _, blob := range blobs {
		if err := store.UpsertBlob(ctx, blob); err != nil {
			t.Fatalf("upsert blob: %v", err)
		}
	}
	for _, photo := range []catalog.Photo{
		{AlbumID: "album-a", Index: 0, Name: "shared.jpg", Hash: "shared", Width: 4, Height: 8},
		{AlbumID: "album-a", Index: 1, Name: "blocked.jpg", Hash: "blocked", Width: 5, Height: 9},
		{AlbumID: "album-a", Index: 2, Name: `literal%_\match.jpg`, Hash: "shared", Width: 4, Height: 8},
		{AlbumID: "album-a", Index: 3, Name: "dangling.jpg", Hash: "missing-blob", Width: 6, Height: 10},
		{AlbumID: "album-b", Index: 0, Name: "same bytes.jpg", Hash: "shared", Width: 4, Height: 8},
	} {
		if err := store.InsertPhoto(ctx, photo); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}
	service := admin.NewService(store, nil, nil)
	router := New(nil, nil, nil, nil, "", service, "sekrit").Router()
	auth := basicAuthHeader("operator", "sekrit")

	unauthorized := doAdminRequest(t, router, http.MethodGet, "/admin/api/photos", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized photos status=%d want=401", unauthorized.Code)
	}

	target := "/admin/api/photos?q=" + url.QueryEscape(`%_\`)
	search := doAdminRequest(t, router, http.MethodGet, target, auth)
	if search.Code != http.StatusOK {
		t.Fatalf("literal search status=%d body=%s", search.Code, search.Body.String())
	}
	var searchBody struct {
		Photos []map[string]any `json:"photos"`
	}
	if err := json.Unmarshal(search.Body.Bytes(), &searchBody); err != nil || len(searchBody.Photos) != 1 {
		t.Fatalf("literal search payload=%s err=%v want exactly one photo", search.Body.String(), err)
	}
	photo := searchBody.Photos[0]
	for _, key := range []string{"albumId", "albumName", "idx", "name", "hash", "missingBlob", "width", "height", "sizeBytes", "contentType", "encodingRaw", "encoding", "encodingError", "embeddingRaw", "embedding", "embeddingError", "skippedReason"} {
		if _, ok := photo[key]; !ok {
			t.Errorf("photo JSON missing %q: %v", key, photo)
		}
	}
	if photo["name"] != `literal%_\match.jpg` || photo["encoding"] != "queued" {
		t.Fatalf("photo payload=%v", photo)
	}

	detail := doAdminRequest(t, router, http.MethodGet, "/admin/api/photos/album-a/1", auth)
	if detail.Code != http.StatusOK || strings.Contains(detail.Body.String(), `"blockReason"`) || !strings.Contains(detail.Body.String(), `"embedding":"queued"`) || !strings.Contains(detail.Body.String(), `"embeddingError":""`) {
		t.Fatalf("photo detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	danglingDetail := doAdminRequest(t, router, http.MethodGet, "/admin/api/photos/album-a/3", auth)
	if danglingDetail.Code != http.StatusOK || !strings.Contains(danglingDetail.Body.String(), `"missingBlob":true`) || !strings.Contains(danglingDetail.Body.String(), `"sizeBytes":null`) || !strings.Contains(danglingDetail.Body.String(), `"contentType":null`) || !strings.Contains(danglingDetail.Body.String(), `"encoding":""`) || !strings.Contains(danglingDetail.Body.String(), `"embedding":""`) {
		t.Fatalf("dangling photo detail status=%d body=%s", danglingDetail.Code, danglingDetail.Body.String())
	}
	overview := doAdminRequest(t, router, http.MethodGet, "/admin/api/overview", auth)
	if overview.Code != http.StatusOK || !strings.Contains(overview.Body.String(), `"photos":5`) || !strings.Contains(overview.Body.String(), `"unreferencedBlobs":1`) || !strings.Contains(overview.Body.String(), `"missingBlobPhotos":1`) {
		t.Fatalf("overview inventory and health contract status=%d body=%s", overview.Code, overview.Body.String())
	}
	if !strings.Contains(overview.Body.String(), `"vectorCount":{"status":"disabled"`) {
		t.Fatalf("overview without a vector store must report a disabled vectorCount: %s", overview.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/photos/album-a/99", auth); rec.Code != http.StatusNotFound {
		t.Fatalf("missing photo status=%d want=404 body=%s", rec.Code, rec.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/photos/album-a/nope", auth); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad photo index status=%d want=400", rec.Code)
	}

	blobDetail := doAdminRequest(t, router, http.MethodGet, "/admin/api/blobs/shared", auth)
	if blobDetail.Code != http.StatusOK || !strings.Contains(blobDetail.Body.String(), `"photoCount":3`) || !strings.Contains(blobDetail.Body.String(), `"albumCount":2`) {
		t.Fatalf("shared blob detail status=%d body=%s", blobDetail.Code, blobDetail.Body.String())
	}
	if strings.Contains(blobDetail.Body.String(), `"photos"`) || strings.Contains(blobDetail.Body.String(), `"albums"`) {
		t.Fatalf("blob detail must not embed unbounded references: %s", blobDetail.Body.String())
	}
	blobList := doAdminRequest(t, router, http.MethodGet, "/admin/api/blobs?unreferenced=true&embedding=queued", auth)
	if blobList.Code != http.StatusOK || !strings.Contains(blobList.Body.String(), `"hash":"orphan"`) || !strings.Contains(blobList.Body.String(), `"photoCount":0`) {
		t.Fatalf("unreferenced blob drilldown status=%d body=%s", blobList.Code, blobList.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/blobs/missing", auth); rec.Code != http.StatusNotFound {
		t.Fatalf("missing blob status=%d want=404", rec.Code)
	}

	albumList := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums?status=FAILED", auth)
	if albumList.Code != http.StatusOK || !strings.Contains(albumList.Body.String(), `"originalFilename":"alpha.zip"`) || !strings.Contains(albumList.Body.String(), `"error":"extract failed"`) || !strings.Contains(albumList.Body.String(), `"photoCount":4`) {
		t.Fatalf("album list status=%d body=%s", albumList.Code, albumList.Body.String())
	}
	albumDetail := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/album-a", auth)
	if albumDetail.Code != http.StatusOK || !strings.Contains(albumDetail.Body.String(), `"status":"FAILED"`) || !strings.Contains(albumDetail.Body.String(), `"error":"extract failed"`) || !strings.Contains(albumDetail.Body.String(), `"photoCount":4`) || !strings.Contains(albumDetail.Body.String(), `"missingBlobPhotos":1`) || !strings.Contains(albumDetail.Body.String(), `"encoding":{"categories"`) || !strings.Contains(albumDetail.Body.String(), `"embedding":{"categories"`) {
		t.Fatalf("album detail status=%d body=%s", albumDetail.Code, albumDetail.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/missing", auth); rec.Code != http.StatusNotFound {
		t.Fatalf("missing album status=%d want=404", rec.Code)
	}

	for _, target := range []string{
		"/admin/api/photos?embedding=blocked",
		"/admin/api/blobs?embedding=blocked",
		"/admin/api/photos?encoding=not-a-category",
		"/admin/api/photos?unknown=value",
		"/admin/api/photos?encoding=queued&encoding=blocked",
		"/admin/api/photos?encoding=",
		"/admin/api/photos?limit=101",
		"/admin/api/photos?cursor=not-a-cursor",
		"/admin/api/blobs?unreferenced=yes",
		"/admin/api/albums?status=READY",
	} {
		if rec := doAdminRequest(t, router, http.MethodGet, target, auth); rec.Code != http.StatusBadRequest {
			t.Errorf("bad browse request %s status=%d want=400 body=%s", target, rec.Code, rec.Body.String())
		}
	}
	malformedQuery := httptest.NewRequest(http.MethodGet, "/admin/api/photos", nil)
	malformedQuery.URL.RawQuery = "cursor=%"
	malformedQuery.Header.Set("Authorization", auth)
	malformedResponse := httptest.NewRecorder()
	router.ServeHTTP(malformedResponse, malformedQuery)
	if malformedResponse.Code != http.StatusBadRequest {
		t.Fatalf("malformed query status=%d want=400 body=%s", malformedResponse.Code, malformedResponse.Body.String())
	}
}

func TestAdminExtractionHistoryEndpointsArePagedFilteredAndReadOnly(t *testing.T) {
	store := openAdminTestCatalog(t)
	ctx := context.Background()
	for _, id := range []string{"history-album", "legacy-album"} {
		if err := store.CreateAlbum(ctx, catalog.Album{ID: id, OriginalFilename: id + ".zip", Status: catalog.AlbumStatusReady}); err != nil {
			t.Fatal(err)
		}
	}
	firstRun, err := store.BeginExtractionRun(ctx, "history-album")
	if err != nil {
		t.Fatal(err)
	}
	photoIndex := 0
	firstEntries := []catalog.ExtractionEntryOutcome{
		{Ordinal: 0, Name: "duplicate.jpg", Candidate: true, Outcome: "registered", PhotoIndex: &photoIndex, Hash: "hash-one"},
		{Ordinal: 2, Name: "duplicate.jpg", Candidate: true, Outcome: "skipped", ReasonCode: "invalid_image", ErrorDetail: "decode failed"},
		{Ordinal: 4, Name: "duplicate.jpg", Candidate: true, Outcome: "skipped", ReasonCode: "entry_read_failed", ErrorDetail: "read failed"},
		{Ordinal: 6, Name: "last.jpg", Candidate: true, Outcome: "failed", ReasonCode: "blob_storage_failed", ErrorDetail: "storage failed"},
	}
	if err := store.RecordExtractionEntries(ctx, firstRun, firstEntries); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishExtractionRun(ctx, firstRun, "failed", "entry_failed", "safe failure", true); err != nil {
		t.Fatal(err)
	}
	secondRun, err := store.BeginExtractionRun(ctx, "history-album")
	if err != nil {
		t.Fatal(err)
	}
	secondEntries := []catalog.ExtractionEntryOutcome{
		{Ordinal: 0, Name: "new.jpg", Candidate: true, Outcome: "registered", PhotoIndex: &photoIndex, Hash: "hash-two"},
		{Ordinal: 1, Name: "readme.txt", Outcome: "skipped", ReasonCode: "unsupported_extension"},
	}
	if err := store.RecordExtractionEntries(ctx, secondRun, secondEntries); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishExtractionRun(ctx, secondRun, "succeeded", "", "", true); err != nil {
		t.Fatal(err)
	}
	service := admin.NewService(store, nil, nil)
	router := New(nil, nil, nil, nil, "", service, "secret").Router()
	auth := basicAuthHeader("operator", "secret")
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/entries", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized entries status=%d body=%s", rec.Code, rec.Body.String())
	}

	// No runId explicitly selects the latest recorded run, rather than
	// pretending that an old album has zero failures.
	latest := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/entries?limit=1", auth)
	if latest.Code != http.StatusOK || !strings.Contains(latest.Body.String(), `"runId":`+strconv.FormatInt(secondRun, 10)) || !strings.Contains(latest.Body.String(), `"runSelection":"latest"`) {
		t.Fatalf("latest entries status=%d body=%s", latest.Code, latest.Body.String())
	}
	latestDetail := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album", auth)
	if latestDetail.Code != http.StatusOK || !strings.Contains(latestDetail.Body.String(), `"extractionRecorded":true`) || !strings.Contains(latestDetail.Body.String(), `"latestExtraction"`) || !strings.Contains(latestDetail.Body.String(), `"failedCount":0`) {
		t.Fatalf("album extraction summary status=%d body=%s", latestDetail.Code, latestDetail.Body.String())
	}
	legacyDetail := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/legacy-album", auth)
	if legacyDetail.Code != http.StatusOK || !strings.Contains(legacyDetail.Body.String(), `"extractionRecorded":false`) || !strings.Contains(legacyDetail.Body.String(), `"latestExtraction":null`) {
		t.Fatalf("legacy album detail status=%d body=%s", legacyDetail.Code, legacyDetail.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/legacy-album/entries", auth); rec.Code != http.StatusNotFound {
		t.Fatalf("legacy default entries status=%d want=404 body=%s", rec.Code, rec.Body.String())
	}

	// Extraction run history pages newest first; a cursor is bound to album and limit.
	extractions := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/extractions?limit=1", auth)
	if extractions.Code != http.StatusOK || !strings.Contains(extractions.Body.String(), `"runId":`+strconv.FormatInt(secondRun, 10)) || !strings.Contains(extractions.Body.String(), `"nextCursor":"`) {
		t.Fatalf("first extraction page status=%d body=%s", extractions.Code, extractions.Body.String())
	}
	var extractionsPage struct {
		NextCursor *string `json:"nextCursor"`
	}
	if err := json.Unmarshal(extractions.Body.Bytes(), &extractionsPage); err != nil || extractionsPage.NextCursor == nil {
		t.Fatalf("decode extraction cursor: page=%s err=%v", extractions.Body.String(), err)
	}
	olderRuns := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/extractions?limit=1&cursor="+url.QueryEscape(*extractionsPage.NextCursor), auth)
	if olderRuns.Code != http.StatusOK || !strings.Contains(olderRuns.Body.String(), `"runId":`+strconv.FormatInt(firstRun, 10)) {
		t.Fatalf("older extraction page status=%d body=%s", olderRuns.Code, olderRuns.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/extractions?limit=2&cursor="+url.QueryEscape(*extractionsPage.NextCursor), auth); rec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched extraction cursor status=%d want=400 body=%s", rec.Code, rec.Body.String())
	}

	filtered := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/entries?runId="+strconv.FormatInt(firstRun, 10)+"&outcome=skipped&limit=1", auth)
	if filtered.Code != http.StatusOK || !strings.Contains(filtered.Body.String(), `"ordinal":2`) || !strings.Contains(filtered.Body.String(), `"reasonCode":"invalid_image"`) || !strings.Contains(filtered.Body.String(), `"nextCursor":"`) {
		t.Fatalf("filtered entries first page status=%d body=%s", filtered.Code, filtered.Body.String())
	}
	var entriesPage struct {
		NextCursor *string `json:"nextCursor"`
	}
	if err := json.Unmarshal(filtered.Body.Bytes(), &entriesPage); err != nil || entriesPage.NextCursor == nil {
		t.Fatalf("decode entry cursor: page=%s err=%v", filtered.Body.String(), err)
	}
	nextEntry := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/entries?runId="+strconv.FormatInt(firstRun, 10)+"&outcome=skipped&limit=1&cursor="+url.QueryEscape(*entriesPage.NextCursor), auth)
	if nextEntry.Code != http.StatusOK || !strings.Contains(nextEntry.Body.String(), `"ordinal":4`) {
		t.Fatalf("filtered entries second page status=%d body=%s", nextEntry.Code, nextEntry.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/entries?runId="+strconv.FormatInt(firstRun, 10)+"&outcome=failed&limit=1&cursor="+url.QueryEscape(*entriesPage.NextCursor), auth); rec.Code != http.StatusBadRequest {
		t.Fatalf("outcome-mismatched cursor status=%d want=400 body=%s", rec.Code, rec.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/entries?outcome=bad", auth); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid outcome status=%d want=400 body=%s", rec.Code, rec.Body.String())
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/entries?runId=99999", auth); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign/missing run status=%d want=404 body=%s", rec.Code, rec.Body.String())
	}
	oldRun := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/entries?runId="+strconv.FormatInt(firstRun, 10), auth)
	if oldRun.Code != http.StatusOK || !strings.Contains(oldRun.Body.String(), `"ordinal":6`) || strings.Contains(oldRun.Body.String(), `"name":"new.jpg"`) {
		t.Fatalf("old run history changed after reprocessing: status=%d body=%s", oldRun.Code, oldRun.Body.String())
	}

	// Even already-recorded ordinals cannot be paged while a run may append
	// lower ordinals. Once terminal (including incomplete), the history is stable.
	runningRun, err := store.BeginExtractionRun(ctx, "history-album")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordExtractionEntries(ctx, runningRun, []catalog.ExtractionEntryOutcome{{Ordinal: 8, Name: "partial.jpg", Candidate: true, Outcome: "failed", ReasonCode: "invalid_image"}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"/admin/api/albums/history-album/entries",
		"/admin/api/albums/history-album/entries?runId=" + strconv.FormatInt(runningRun, 10),
	} {
		rec := doAdminRequest(t, router, http.MethodGet, target, auth)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"EXTRACTION_IN_PROGRESS"`) {
			t.Fatalf("running run entries %s status=%d want=409 body=%s", target, rec.Code, rec.Body.String())
		}
	}
	if err := store.FinishExtractionRun(ctx, runningRun, "incomplete", "outcome_recording_failed", "", false); err != nil {
		t.Fatal(err)
	}
	terminalEntries := doAdminRequest(t, router, http.MethodGet, "/admin/api/albums/history-album/entries?runId="+strconv.FormatInt(runningRun, 10), auth)
	if terminalEntries.Code != http.StatusOK || !strings.Contains(terminalEntries.Body.String(), `"ordinal":8`) {
		t.Fatalf("terminal incomplete run entries status=%d body=%s", terminalEntries.Code, terminalEntries.Body.String())
	}
}

func TestAdminBlobHistoryEndpointIsPagedAndValidatesStage(t *testing.T) {
	store := openAdminTestCatalog(t)
	ctx := context.Background()
	if err := store.CreateAlbum(ctx, catalog.Album{ID: "history-album", OriginalFilename: "history.zip", Status: catalog.AlbumStatusReady}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "history-hash", SizeBytes: 100, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertPhoto(ctx, catalog.Photo{AlbumID: "history-album", Index: 0, Name: "history.jpg", Hash: "history-hash", Width: 1, Height: 1}); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim encoding: jobs=%+v err=%v", jobs, err)
	}
	if ok, err := store.BeginEncodingCommit(ctx, jobs[0].Hash, jobs[0].Token, 100, time.Minute); err != nil || !ok {
		t.Fatalf("begin encoding: ok=%v err=%v", ok, err)
	}
	if ok, err := store.FinishEncoding(ctx, jobs[0].Hash, jobs[0].Token, "done", "image/webp", "", 50); err != nil || !ok {
		t.Fatalf("finish encoding: ok=%v err=%v", ok, err)
	}
	service := admin.NewService(store, nil, nil)
	router := New(nil, nil, nil, nil, "", service, "sekrit").Router()
	auth := basicAuthHeader("operator", "sekrit")

	detail := doAdminRequest(t, router, http.MethodGet, "/admin/api/blobs/history-hash", auth)
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"encodingObservation"`) || !strings.Contains(detail.Body.String(), `"attempts":1`) {
		t.Fatalf("blob detail observations status=%d body=%s", detail.Code, detail.Body.String())
	}
	photoDetail := doAdminRequest(t, router, http.MethodGet, "/admin/api/photos/history-album/0", auth)
	if photoDetail.Code != http.StatusOK || !strings.Contains(photoDetail.Body.String(), `"encodingObservation"`) || !strings.Contains(photoDetail.Body.String(), `"embeddingObservation"`) {
		t.Fatalf("photo detail observations status=%d body=%s", photoDetail.Code, photoDetail.Body.String())
	}
	first := doAdminRequest(t, router, http.MethodGet, "/admin/api/blobs/history-hash/history?limit=1", auth)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"event":"claimed"`) || !strings.Contains(first.Body.String(), `"nextCursor":"`) {
		t.Fatalf("first history page status=%d body=%s", first.Code, first.Body.String())
	}
	var firstPage catalog.PipelineHistoryPage
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil || firstPage.NextCursor == nil {
		t.Fatalf("decode first history page=%+v err=%v", firstPage, err)
	}
	next := doAdminRequest(t, router, http.MethodGet, "/admin/api/blobs/history-hash/history?stage=encoding&limit=1&cursor="+*firstPage.NextCursor, auth)
	if next.Code != http.StatusOK || !strings.Contains(next.Body.String(), `"event":"commit_started"`) {
		t.Fatalf("second history page status=%d body=%s", next.Code, next.Body.String())
	}
	for _, target := range []string{
		"/admin/api/blobs/history-hash/history?stage=unknown",
		"/admin/api/blobs/history-hash/history?limit=101",
		"/admin/api/blobs/history-hash/history?cursor=-1",
	} {
		if rec := doAdminRequest(t, router, http.MethodGet, target, auth); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid history request %s status=%d body=%s", target, rec.Code, rec.Body.String())
		}
	}
	if rec := doAdminRequest(t, router, http.MethodGet, "/admin/api/blobs/missing/history", auth); rec.Code != http.StatusNotFound {
		t.Fatalf("missing blob history status=%d want=404 body=%s", rec.Code, rec.Body.String())
	}
}

func assertOverviewJSONCounts(t *testing.T, name string, counts map[string]int64) {
	t.Helper()
	if len(counts) != 1 {
		t.Errorf("%s want only blobCount, got %v", name, counts)
	}
	if _, ok := counts["blobCount"]; !ok {
		t.Errorf("%s missing blobCount: %v", name, counts)
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

// TestAdminActionEndpointsRequireAuth extends the Basic auth contract to the
// three action endpoints: without credentials each one is the same 401
// challenge as the dashboard reads, never a partial action.
func TestAdminActionEndpointsRequireAuth(t *testing.T) {
	router, _ := newAdminTestRouter(t, 1)

	rec := doAdminRequest(t, router, http.MethodPost, "/admin/api/recover", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=401 body=%s", rec.Code, rec.Body.String())
	}
	if !jsonBodyHasErrorCode(rec.Body.String(), "UNAUTHORIZED") {
		t.Fatalf("body=%s want UNAUTHORIZED error code", rec.Body.String())
	}
}

// TestAdminReleaseEmbeddingsEndpoint drives the release endpoint end to end,
// including the expiredOnly switch the dashboard's checkbox feeds: true
// releases only the expired lease, false (and an empty body, which must decode
// to the zero value rather than a 400) force-releases the rest.
func TestAdminRecoverReleaseClaimsEndpoint(t *testing.T) {
	router, store := newAdminTestRouter(t, 1)
	ctx := context.Background()

	// The fixture leaves hash-c pending; sweeping it into the claim makes the
	// arithmetic below self-contained. hash-live gets its lease renewed into
	// the future, hash-expired keeps the expired claim lease.
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-live", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-live: %v", err)
	}
	if err := store.UpsertBlob(ctx, catalog.Blob{Hash: "hash-expired", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert hash-expired: %v", err)
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("claim pending blobs: %v", err)
	}
	if renewed, err := store.RenewEmbeddingLeases(ctx, []string{"hash-live"}, time.Now().Add(5*time.Minute)); err != nil || len(renewed) != 1 {
		t.Fatalf("renew hash-live: renewed=%v err=%v", renewed, err)
	}

	// The expired-claims target moves only expired leases; the live one
	// survives.
	rec := doAdminBodyRequest(t, router, http.MethodPost, "/admin/api/recover", basicAuthHeader("operator", "sekrit"), []byte(`{"target":"release_expired_claims"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("expired-only status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Target  string `json:"target"`
		Release struct {
			Released int64                   `json:"released"`
			Counts   catalog.EmbeddingCounts `json:"counts"`
		} `json:"release"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode expired-only response: %v", err)
	}
	wantCounts := catalog.EmbeddingCounts{Total: 5, Ready: 1, Failed: 1, Processing: 1, Pending: 3}
	if payload.Target != "release_expired_claims" || payload.Release.Released != 2 || payload.Release.Counts != wantCounts {
		t.Fatalf("expired-only response={%s %+v} want target=release_expired_claims released=2 counts=%+v",
			payload.Target, payload.Release, wantCounts)
	}

	// The all-claims target force-releases what is left.
	rec = doAdminBodyRequest(t, router, http.MethodPost, "/admin/api/recover", basicAuthHeader("operator", "sekrit"), []byte(`{"target":"release_all_claims"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("forced status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode forced response: %v", err)
	}
	wantCounts = catalog.EmbeddingCounts{Total: 5, Ready: 1, Failed: 1, Pending: 3}
	if payload.Target != "release_all_claims" || payload.Release.Released != 1 || payload.Release.Counts != wantCounts {
		t.Fatalf("forced response={%s %+v} want target=release_all_claims released=1 counts=%+v",
			payload.Target, payload.Release, wantCounts)
	}

	after, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("catalog counts: %v", err)
	}
	if after != wantCounts {
		t.Fatalf("catalog counts=%+v want=%+v", after, wantCounts)
	}

	// A broken body, an unknown field, an empty body (zero target), an
	// unknown target string, and a malformed hashes scope are caller bugs:
	// 400 with the shared invalid-request code. The oversized list uses short
	// placeholder entries because the count rule fires before the per-hash
	// format check — and keeps the body under the 4 KiB action limit, so the
	// count rejection itself is what gets exercised rather than the size one.
	tooManyHashes := make([]string, maxRecoverHashes+1)
	for i := range tooManyHashes {
		tooManyHashes[i] = "x"
	}
	oversized, err := json.Marshal(map[string]any{"target": recoverReleaseExpiredClaims, "hashes": tooManyHashes})
	if err != nil {
		t.Fatalf("marshal oversized hashes body: %v", err)
	}
	for name, body := range map[string]string{
		"not json":        `not json`,
		"unknown field":   `{"target":"release_all_claims","extra":1}`,
		"empty body":      ``,
		"unknown target":  `{"target":"nuke_everything"}`,
		"too many hashes": string(oversized),
		"non-hex hash":    `{"target":"release_expired_claims","hashes":["not-a-hash"]}`,
		"uppercase hash":  `{"target":"release_expired_claims","hashes":["` + strings.Repeat("A", 64) + `"]}`,
	} {
		rec := doAdminBodyRequest(t, router, http.MethodPost, "/admin/api/recover", basicAuthHeader("operator", "sekrit"), []byte(body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d want=400 body=%s", name, rec.Code, rec.Body.String())
		}
		if !jsonBodyHasErrorCode(rec.Body.String(), "INVALID_REQUEST") {
			t.Fatalf("%s: body=%s want INVALID_REQUEST error code", name, rec.Body.String())
		}
	}
}

// TestAdminRetryFailedEndpoint drives the retry-failed endpoint: a bodyless
// POST resets the fixture's one failed blob and answers with the post-reset
// counts, which must match the catalog behind it. A hashes-scoped POST then
// moves only the listed blob of two fresh failures, proving the scope reaches
// the catalog's selection.
func TestAdminRecoverRetryFailedEndpoint(t *testing.T) {
	router, store := newAdminTestRouter(t, 1)
	ctx := context.Background()

	before, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("counts before retry: %v", err)
	}
	if before.Failed != 1 {
		t.Fatalf("fixture counts=%+v want one failed blob", before)
	}

	rec := doAdminBodyRequest(t, router, http.MethodPost, "/admin/api/recover", basicAuthHeader("operator", "sekrit"), []byte(`{"target":"retry_failed_embeddings"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Target string `json:"target"`
		Retry  struct {
			Reset  int64                   `json:"reset"`
			Counts catalog.EmbeddingCounts `json:"counts"`
		} `json:"retry"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	want := catalog.EmbeddingCounts{Total: 3, Ready: 1, Pending: 2}
	if payload.Target != "retry_failed_embeddings" || payload.Retry.Reset != 1 || payload.Retry.Counts != want {
		t.Fatalf("response={%s %+v} want target=retry_failed_embeddings reset=1 counts=%+v",
			payload.Target, payload.Retry, want)
	}

	after, err := store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("counts after retry: %v", err)
	}
	if after != want {
		t.Fatalf("catalog counts=%+v want=%+v", after, want)
	}

	// A hashes scope narrows the retry to the listed blobs. The two fresh
	// failures are seeded with digest-shaped hashes — the endpoint rejects any
	// other shape — and the claim sweeps the just-reset hash-b and the pending
	// hash-c along, which is why they read as processing below.
	scopedHash := strings.Repeat("a", 64)
	otherHash := strings.Repeat("b", 64)
	for _, hash := range []string{scopedHash, otherHash} {
		if err := store.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: 1}); err != nil {
			t.Fatalf("upsert %s: %v", hash, err)
		}
	}
	if _, err := store.ClaimPendingEmbeddings(ctx, 8, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("claim pending blobs: %v", err)
	}
	if _, _, err := store.ApplyEmbeddingResults(ctx, []catalog.EmbeddingResult{
		{Hash: scopedHash, Status: catalog.EmbeddingStatusFailed, Error: "scoped failure"},
		{Hash: otherHash, Status: catalog.EmbeddingStatusFailed, Error: "outside the scope"},
	}); err != nil {
		t.Fatalf("apply failed outcomes: %v", err)
	}
	scopedBody, err := json.Marshal(map[string]any{"target": "retry_failed_embeddings", "hashes": []string{scopedHash}})
	if err != nil {
		t.Fatalf("marshal scoped retry body: %v", err)
	}
	rec = doAdminBodyRequest(t, router, http.MethodPost, "/admin/api/recover", basicAuthHeader("operator", "sekrit"), scopedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped retry status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode scoped retry response: %v", err)
	}
	// The listed failure is pending again; the unlisted one stays failed, and
	// the two swept fixture blobs count as pending (the derived field covers
	// processing rows).
	want = catalog.EmbeddingCounts{Total: 5, Ready: 1, Processing: 2, Failed: 1, Pending: 3}
	if payload.Target != "retry_failed_embeddings" || payload.Retry.Reset != 1 || payload.Retry.Counts != want {
		t.Fatalf("scoped response={%s %+v} want target=retry_failed_embeddings reset=1 counts=%+v",
			payload.Target, payload.Retry, want)
	}
	after, err = store.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("counts after scoped retry: %v", err)
	}
	if after != want {
		t.Fatalf("catalog counts=%+v want=%+v", after, want)
	}
}

// TestAdminResetEncodingsEndpoint drives the encoding reset endpoint: the two
// stuck shapes (expired lease, received result) drain back to pending while a
// committing row is left for the server-side Recover path, and the response
// carries both the reset counts and the fresh encoding stats.
func TestAdminRecoverResetEncodingEndpoint(t *testing.T) {
	router, store := newAdminTestRouter(t, 1)
	ctx := context.Background()

	// Sizes steer ClaimEncoding (largest first) at one row per claim, in the
	// order the fixture needs: received first on a live lease, committing
	// second, and the expired lease last — any later claim would sweep an
	// expired lease back to pending.
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
	liveJob, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(liveJob) != 1 || liveJob[0].Hash != "hash-received" {
		t.Fatalf("claim hash-received: jobs=%v err=%v", liveJob, err)
	}
	if ok, err := store.MarkEncodingReceived(ctx, "hash-received", liveJob[0].Token); err != nil || !ok {
		t.Fatalf("mark hash-received received: ok=%v err=%v", ok, err)
	}
	committer, err := store.ClaimEncoding(ctx, 1, time.Minute)
	if err != nil || len(committer) != 1 || committer[0].Hash != "hash-committing" {
		t.Fatalf("claim hash-committing: jobs=%v err=%v", committer, err)
	}
	if ok, err := store.BeginEncodingCommit(ctx, "hash-committing", committer[0].Token, 90, time.Minute); err != nil || !ok {
		t.Fatalf("begin commit: ok=%v err=%v", ok, err)
	}
	if _, err := store.ClaimEncoding(ctx, 1, -time.Minute); err != nil {
		t.Fatalf("claim expired lease: %v", err)
	}

	rec := doAdminBodyRequest(t, router, http.MethodPost, "/admin/api/recover", basicAuthHeader("operator", "sekrit"), []byte(`{"target":"reset_stuck_encoding"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Target   string `json:"target"`
		Encoding struct {
			Counts catalog.EncodingResetCounts `json:"counts"`
			Stats  struct {
				Enabled    bool  `json:"enabled"`
				Pending    int64 `json:"pending"`
				Processing int64 `json:"processing"`
			} `json:"encoding"`
		} `json:"encoding"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode reset response: %v", err)
	}
	if payload.Target != "reset_stuck_encoding" {
		t.Fatalf("target=%s want=reset_stuck_encoding", payload.Target)
	}
	wantCounts := catalog.EncodingResetCounts{ReleasedExpiredLeased: 1, ResetReceived: 1}
	if payload.Encoding.Counts != wantCounts {
		t.Fatalf("counts=%+v want=%+v", payload.Encoding.Counts, wantCounts)
	}
	// The fixture's three blobs are also encoding-pending, so the two reset
	// rows land on top of them: 3 + 2 pending, with committing the only
	// processing row left.
	if !payload.Encoding.Stats.Enabled || payload.Encoding.Stats.Pending != 5 || payload.Encoding.Stats.Processing != 1 {
		t.Fatalf("encoding=%+v want enabled with pending=5 processing=1 (committing untouched)", payload.Encoding.Stats)
	}

	committing, err := store.CommittingEncodings(ctx)
	if err != nil || len(committing) != 1 || committing[0].Hash != "hash-committing" {
		t.Fatalf("committing after reset=%v err=%v want hash-committing untouched", committing, err)
	}
}

// TestAdminRecoverRetryFailedEncodingsEndpoint drives the encoding retry
// target: failed non-WebP conversions drain back to pending and the response
// carries the fresh encoding stats, then a hashes-scoped request moves only
// the listed blob of two fresh failures.
func TestAdminRecoverRetryFailedEncodingsEndpoint(t *testing.T) {
	router, store := newAdminTestRouter(t, 1)
	ctx := context.Background()

	// Two digest-shaped blobs (the endpoint validates the hashes format) at
	// sizes above the fixture's, so ClaimEncoding takes them first and leaves
	// the fixture blobs untouched.
	scopedHash := strings.Repeat("a", 64)
	otherHash := strings.Repeat("b", 64)
	for _, blob := range []catalog.Blob{
		{Hash: scopedHash, SizeBytes: 100, ContentType: "image/jpeg"},
		{Hash: otherHash, SizeBytes: 90, ContentType: "image/jpeg"},
	} {
		if err := store.UpsertBlob(ctx, blob); err != nil {
			t.Fatalf("upsert %s: %v", blob.Hash, err)
		}
	}
	failBoth := func() {
		t.Helper()
		jobs, err := store.ClaimEncoding(ctx, 2, time.Minute)
		if err != nil || len(jobs) != 2 {
			t.Fatalf("claim encoding jobs: jobs=%v err=%v", jobs, err)
		}
		for _, job := range jobs {
			if ok, err := store.SkipEncoding(ctx, job.Hash, job.Token, "failed", "image/jpeg", "conversion failed", job.SizeBytes); err != nil || !ok {
				t.Fatalf("fail %s: ok=%v err=%v", job.Hash, ok, err)
			}
		}
	}
	failBoth()

	rec := doAdminBodyRequest(t, router, http.MethodPost, "/admin/api/recover", basicAuthHeader("operator", "sekrit"), []byte(`{"target":"retry_failed_encodings"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Target        string `json:"target"`
		EncodingRetry struct {
			Reset    int64 `json:"reset"`
			Encoding struct {
				Enabled bool  `json:"enabled"`
				Pending int64 `json:"pending"`
				Failed  int64 `json:"failed"`
			} `json:"encoding"`
		} `json:"encodingRetry"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	// The fixture's three blobs are encoding-pending and both failures drain
	// back onto that pile: 3 + 2 pending, nothing failed or in flight.
	if payload.Target != "retry_failed_encodings" || payload.EncodingRetry.Reset != 2 ||
		!payload.EncodingRetry.Encoding.Enabled || payload.EncodingRetry.Encoding.Pending != 5 || payload.EncodingRetry.Encoding.Failed != 0 {
		t.Fatalf("response={%s %+v} want target=retry_failed_encodings reset=2 enabled pending=5 failed=0",
			payload.Target, payload.EncodingRetry)
	}
	fresh, err := store.EncodingCounts(ctx)
	if err != nil {
		t.Fatalf("encoding counts after retry: %v", err)
	}
	if fresh.Pending != 5 || fresh.Failed != 0 {
		t.Fatalf("catalog encoding counts=%+v want pending=5 failed=0", fresh)
	}

	// The scoped variant moves only the listed blob; the other failure stays.
	failBoth()
	scopedBody, err := json.Marshal(map[string]any{"target": "retry_failed_encodings", "hashes": []string{scopedHash}})
	if err != nil {
		t.Fatalf("marshal scoped retry body: %v", err)
	}
	rec = doAdminBodyRequest(t, router, http.MethodPost, "/admin/api/recover", basicAuthHeader("operator", "sekrit"), scopedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("scoped status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode scoped retry response: %v", err)
	}
	// Three fixture blobs plus the one reset row are pending; the unlisted
	// failure is the only one left.
	if payload.Target != "retry_failed_encodings" || payload.EncodingRetry.Reset != 1 ||
		payload.EncodingRetry.Encoding.Pending != 4 || payload.EncodingRetry.Encoding.Failed != 1 {
		t.Fatalf("scoped response={%s %+v} want target=retry_failed_encodings reset=1 pending=4 failed=1",
			payload.Target, payload.EncodingRetry)
	}
	fresh, err = store.EncodingCounts(ctx)
	if err != nil {
		t.Fatalf("encoding counts after scoped retry: %v", err)
	}
	if fresh.Pending != 4 || fresh.Failed != 1 {
		t.Fatalf("catalog encoding counts=%+v want pending=4 failed=1", fresh)
	}
}
