package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"viewer/internal/admin"
	"viewer/internal/images"
	"viewer/internal/recommend"
)

// runRequest issues an admin-authenticated request with the mutation headers
// the admin actions require.
func runRequest(router http.Handler, method, target, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.SetBasicAuth("operator", "secret")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Action", "1")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func jsonObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("invalid JSON %s: %v", raw, err)
	}
	return obj
}

func assertJSONKeys(t *testing.T, obj map[string]any, keys ...string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(keys)
	if !reflect.DeepEqual(got, keys) {
		t.Fatalf("JSON keys=%v want=%v", got, keys)
	}
}

// stubLocalEncoding records what the run-local trigger asked the encoding
// service to do, so the HTTP tests need no cwebp, spool, or real drain.
type stubLocalEncoding struct {
	started int
	start   bool
	err     error
}

func (s *stubLocalEncoding) StartLocalDrain() (bool, error) {
	s.started++
	return s.start, s.err
}

// newAutomaticTestRouter wires the admin surface the way production does for
// the automatic-embedding and run-local controls: a real recommendation
// service over a test catalog (its worker is never started, so Running stays
// false — exactly the pre-model-load state an operator first sees).
func newAutomaticTestRouter(t *testing.T, local admin.LocalEncodingRunner) (http.Handler, *recommend.Service) {
	t.Helper()
	cat := openAdminTestCatalog(t)
	recommender := recommend.NewService(cat, nil, nil, nil, nil)
	service := admin.NewService(cat, nil, recommender).WithLocalEncoding(local)
	return New(nil, nil, nil, nil, "", service, "secret").Router(), recommender
}

func TestAdminAutomaticJSONValidationAuthAndCSRF(t *testing.T) {
	router, _ := newAutomaticTestRouter(t, nil)
	const path = "/admin/api/automatic/embedding"
	for _, enabled := range []bool{true, false, true} {
		method, body := "POST", `{"enabled":true}`
		if !enabled {
			body = `{"enabled":false}`
		}
		res := runRequest(router, method, path, body, "")
		if res.Code != 200 {
			t.Fatalf("control=%d %s", res.Code, res.Body.String())
		}
		obj := jsonObject(t, res.Body.Bytes())
		assertJSONKeys(t, obj, "enabled", "running")
		if obj["enabled"] != enabled || obj["running"] != false {
			t.Fatalf("status=%v", obj)
		}
		res = runRequest(router, "GET", path, "", "")
		if res.Code != 200 || jsonObject(t, res.Body.Bytes())["enabled"] != enabled {
			t.Fatalf("read=%d %s", res.Code, res.Body.String())
		}
	}
	for _, body := range []string{``, `{}`, `null`, `[]`, `{"enabled":null}`, `{"enabled":"false"}`, `{"enabled":0}`, `{"enabled":false,"extra":true}`, `{"enabled":false} {}`, `{"enabled":`} {
		res := runRequest(router, "POST", path, body, "")
		if res.Code != 400 || jsonObject(t, res.Body.Bytes())["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
			t.Errorf("invalid %q=%d %s", body, res.Code, res.Body.String())
		}
	}
	for _, method := range []string{"GET", "POST"} {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"enabled":false}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("no auth %s=%d", method, rec.Code)
		}
	}
	for _, test := range []struct {
		header, origin, contentType string
		status                      int
	}{
		{"", "", "application/json", 403},
		{"1", "https://evil.invalid", "application/json", 403},
		{"1", "", "text/plain", 400},
		{"1", "http://example.com", "application/json", 200},
	} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{"enabled":false}`))
		req.SetBasicAuth("operator", "secret")
		req.Header.Set("X-Admin-Action", test.header)
		req.Header.Set("Origin", test.origin)
		req.Header.Set("Content-Type", test.contentType)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Errorf("mutation %+v=%d %s", test, rec.Code, rec.Body.String())
		}
	}
	for _, token := range []string{"", "secret"} {
		var adminService *admin.Service
		if token == "" {
			// An explicitly nil service keeps only the token half of the pair.
			cat := openAdminTestCatalog(t)
			adminService = admin.NewService(cat, nil, recommend.NewService(cat, nil, nil, nil, nil))
		}
		disabled := New(nil, nil, nil, nil, "", adminService, token).Router()
		for _, method := range []string{"GET", "POST"} {
			res := runRequest(disabled, method, path, `{"enabled":false}`, "")
			if res.Code != 404 {
				t.Errorf("disabled admin exposed %s: %d", method, res.Code)
			}
		}
	}
}

// TestAdminAutomaticWithoutRecommendService pins the 503 shape: a deployment
// (really only a test harness) with no recommendation service wired up must
// answer UNAVAILABLE, not crash.
func TestAdminAutomaticWithoutRecommendService(t *testing.T) {
	router := New(nil, nil, nil, nil, "", admin.NewService(openAdminTestCatalog(t), nil, nil), "secret").Router()
	for _, method := range []string{"GET", "POST"} {
		res := runRequest(router, method, "/admin/api/automatic/embedding", `{"enabled":false}`, "")
		if res.Code != 503 || jsonObject(t, res.Body.Bytes())["error"].(map[string]any)["code"] != "UNAVAILABLE" {
			t.Errorf("missing recommend %s=%d %s", method, res.Code, res.Body.String())
		}
	}
}

// TestAdminAutomaticRunningReflectsStartedWorkers verifies the running half
// of the status: it flips to true only once the recommendation service's
// workers exist — here, started behind a loadable embedder the way the app
// starts them after a successful model load.
func TestAdminAutomaticRunningReflectsStartedWorkers(t *testing.T) {
	cat := openAdminTestCatalog(t)
	recommender := recommend.NewService(cat, nil, nil, stubEmbedder{}, nil)
	router := New(nil, nil, nil, nil, "", admin.NewService(cat, nil, recommender), "secret").Router()
	const path = "/admin/api/automatic/embedding"
	res := runRequest(router, "GET", path, "", "")
	obj := jsonObject(t, res.Body.Bytes())
	if res.Code != 200 || obj["enabled"] != true || obj["running"] != false {
		t.Fatalf("before start=%d %v", res.Code, obj)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recommender.Start(ctx)
	res = runRequest(router, "GET", path, "", "")
	obj = jsonObject(t, res.Body.Bytes())
	if res.Code != 200 || obj["running"] != true {
		t.Fatalf("after start=%d %v", res.Code, obj)
	}
}

func TestLocalEncodingRunLocalEndpointContract(t *testing.T) {
	local := &stubLocalEncoding{start: true}
	router, _ := newAutomaticTestRouter(t, local)
	const path = "/admin/api/encoding/run-local"

	// Strict {} body: JSON content type required, unknown fields rejected,
	// trailing content rejected. An empty body is the zero-value request,
	// the same decode every other admin action applies.
	for _, body := range []string{``, `{}`} {
		res := runRequest(router, "POST", path, body, "")
		if res.Code != 200 || jsonObject(t, res.Body.Bytes())["started"] != true {
			t.Errorf("body %q=%d %s", body, res.Code, res.Body.String())
		}
	}
	for _, body := range []string{`{"started":true}`, `{} {}`, `[]`} {
		res := runRequest(router, "POST", path, body, "")
		if res.Code != 400 || jsonObject(t, res.Body.Bytes())["error"].(map[string]any)["code"] != "INVALID_REQUEST" {
			t.Errorf("invalid body %q=%d %s", body, res.Code, res.Body.String())
		}
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
	req.SetBasicAuth("operator", "secret")
	req.Header.Set("X-Admin-Action", "1")
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("non-JSON content type=%d %s", rec.Code, rec.Body.String())
	}

	// started=false (a drain already in flight) is 200 information, not an error.
	local.start = false
	res := runRequest(router, "POST", path, `{}`, "")
	if res.Code != 200 || jsonObject(t, res.Body.Bytes())["started"] != false {
		t.Fatalf("single-flight=%d %s", res.Code, res.Body.String())
	}
	if local.started != 3 {
		t.Fatalf("drain triggered %d time(s)", local.started)
	}

	// Deployment errors (cwebp missing, service not started) surface as 503
	// with the operator-facing message; internal ones stay generic 500s.
	local.err = errors.New("cwebp is not on the server PATH")
	res = runRequest(router, "POST", path, `{}`, "")
	if res.Code != 503 || !strings.Contains(res.Body.String(), "cwebp") {
		t.Fatalf("operator error=%d %s", res.Code, res.Body.String())
	}

	// Auth and mutation guards match the rest of the admin surface.
	for _, method := range []string{"POST"} {
		req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("no auth %s=%d", method, rec.Code)
		}
		req = httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.SetBasicAuth("operator", "secret")
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("missing CSRF guard=%d", rec.Code)
		}
		req = httptest.NewRequest(method, path, strings.NewReader(`{}`))
		req.SetBasicAuth("operator", "secret")
		req.Header.Set("X-Admin-Action", "1")
		req.Header.Set("Origin", "https://evil.invalid")
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("cross-origin mutation=%d", rec.Code)
		}
	}

	// An unwired encoding service answers 503 UNAVAILABLE.
	unwired, _ := newAutomaticTestRouter(t, nil)
	res = runRequest(unwired, "POST", path, `{}`, "")
	if res.Code != 503 || jsonObject(t, res.Body.Bytes())["error"].(map[string]any)["code"] != "UNAVAILABLE" {
		t.Fatalf("unwired=%d %s", res.Code, res.Body.String())
	}

	// Without an admin token the route is absent entirely (404 JSON).
	absent := New(nil, nil, nil, nil, "", nil, "").Router()
	res = runRequest(absent, "POST", path, `{}`, "")
	if res.Code != 404 {
		t.Fatalf("disabled admin exposed run-local: %d", res.Code)
	}
}

// TestLocalEncodingWithoutTokenDoesNotExposeExternalRoutes keeps the split the
// local drain lives in: local capability needs no WORKER_TOKEN, and the
// external encoder fleet's routes stay absent without one — while legacy
// external embedding remains available without a token or model.
func TestLocalEncodingWithoutTokenDoesNotExposeExternalRoutes(t *testing.T) {
	cat := openAdminTestCatalog(t)
	recommender := recommend.NewService(cat, nil, nil, nil, nil)
	local := &stubLocalEncoding{start: true}
	router := New(nil, nil, images.NewService(cat, claimOnlyStore{}), recommender, "", admin.NewService(cat, nil, recommender).WithLocalEncoding(local), "secret").Router()
	res := runRequest(router, "POST", "/admin/api/encoding/run-local", `{}`, "")
	if res.Code != 200 || jsonObject(t, res.Body.Bytes())["started"] != true {
		t.Fatalf("local unavailable=%d %s", res.Code, res.Body.String())
	}
	for _, suffix := range []string{"claim", "renew", "complete", "output"} {
		for _, token := range []string{"", "Bearer anything"} {
			req := httptest.NewRequest("POST", "/api/encoding/"+suffix, strings.NewReader(`{}`))
			req.Header.Set("Authorization", token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != 404 {
				t.Errorf("external encoding exposed %s: %d %s", suffix, rec.Code, rec.Body.String())
			}
		}
	}
	for _, token := range []string{"", "shared"} {
		external := New(nil, nil, images.NewService(cat, claimOnlyStore{}), recommender, token, nil, "").Router()
		for _, suffix := range []string{"claim", "renew", "results"} {
			body := map[string]string{"claim": `{"limit":1}`, "renew": `{"hashes":[]}`, "results": `{"results":[]}`}[suffix]
			req := httptest.NewRequest("POST", "/api/embedding/"+suffix, strings.NewReader(body))
			rec := httptest.NewRecorder()
			external.ServeHTTP(rec, req)
			want := 200
			if token != "" {
				want = 401
			}
			if rec.Code != want {
				t.Errorf("embedding auth changed %s token=%q: %d %s", suffix, token, rec.Code, rec.Body.String())
			}
		}
	}
}

// A cancelled request context must not wedge the controls; the recommend-side
// toggle is synchronous and context-free by design.
func TestAdminAutomaticCancelledRequest(t *testing.T) {
	router, _ := newAutomaticTestRouter(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/admin/api/automatic/embedding", strings.NewReader(`{"enabled":false}`)).WithContext(ctx)
	req.SetBasicAuth("operator", "secret")
	req.Header.Set("X-Admin-Action", "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("cancelled control=%d %s", rec.Code, rec.Body.String())
	}
}
