package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"viewer/internal/admin"
	"viewer/internal/catalog"
	"viewer/internal/encoding"
	"viewer/internal/images"
	"viewer/internal/recommend"
	"viewer/internal/runs"
)

func TestAdminAutomaticJSONValidationAuthAndCSRF(t *testing.T) {
	cat := openAdminTestCatalog(t)
	manager := runs.New(context.Background(), cat, nil)
	defer manager.Close(context.Background())
	service := admin.NewService(cat, nil, nil).WithRunManager(manager)
	router := New(nil, nil, nil, nil, "", service, "secret").Router()
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
		assertJSONKeys(t, obj, "enabled", "running", "pausedByManual")
		if obj["enabled"] != enabled || obj["running"] != false || obj["pausedByManual"] != false {
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
			adminService = service
		}
		disabled := New(nil, nil, nil, nil, "", adminService, token).Router()
		for _, method := range []string{"GET", "POST"} {
			res := runRequest(disabled, method, path, `{"enabled":false}`, "")
			if res.Code != 404 {
				t.Errorf("disabled admin exposed %s: %d", method, res.Code)
			}
		}
	}
	// Standard error envelopes for stopped, missing, and interrupted managers.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", path, strings.NewReader(`{"enabled":false}`)).WithContext(ctx)
	req.SetBasicAuth("operator", "secret")
	req.Header.Set("X-Admin-Action", "1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 503 || jsonObject(t, rec.Body.Bytes())["error"].(map[string]any)["code"] != "UNAVAILABLE" {
		t.Fatalf("cancelled control=%d %s", rec.Code, rec.Body.String())
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		res := runRequest(router, method, path, `{"enabled":false}`, "")
		if res.Code != 503 || jsonObject(t, res.Body.Bytes())["error"].(map[string]any)["code"] != "UNAVAILABLE" {
			t.Errorf("closed manager=%d %s", res.Code, res.Body.String())
		}
	}
	missing := New(nil, nil, nil, nil, "", admin.NewService(cat, nil, nil), "secret").Router()
	res := runRequest(missing, "GET", path, "", "")
	if res.Code != 503 || jsonObject(t, res.Body.Bytes())["error"].(map[string]any)["code"] != "EXECUTOR_UNAVAILABLE" {
		t.Fatalf("missing manager=%d %s", res.Code, res.Body.String())
	}
}

func TestAdminAutomaticManualPausedJSON(t *testing.T) {
	cat := openAdminTestCatalog(t)
	if err := cat.UpsertBlob(context.Background(), catalog.Blob{Hash: "target", SizeBytes: 10, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	manager := runs.New(context.Background(), cat, map[string]runs.Registration{"embedding": {Name: "test/v1", Executor: waitingRunExecutor{started: started}}})
	defer manager.Close(context.Background())
	service := admin.NewService(cat, nil, nil).WithRunManager(manager)
	router := New(nil, nil, nil, nil, "", service, "secret").Router()
	created := runRequest(router, "POST", "/admin/api/runs", `{"stage":"embedding"}`, "manual")
	if created.Code != 202 {
		t.Fatal(created.Body.String())
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("manual executor did not start")
	}
	res := runRequest(router, "GET", "/admin/api/automatic/embedding", "", "")
	obj := jsonObject(t, res.Body.Bytes())
	assertJSONKeys(t, obj, "enabled", "running", "pausedByManual")
	if res.Code != 200 || obj["enabled"] != true || obj["running"] != false || obj["pausedByManual"] != true {
		t.Fatalf("manual status=%d %v", res.Code, obj)
	}
	res = runRequest(router, "POST", "/admin/api/automatic/embedding", `{"enabled":false}`, "")
	if res.Code != 200 || jsonObject(t, res.Body.Bytes())["pausedByManual"] != false {
		t.Fatalf("explicit pause=%d %s", res.Code, res.Body.String())
	}
}

func TestLocalEncodingWithoutTokenDoesNotExposeExternalRoutes(t *testing.T) {
	cat := openAdminTestCatalog(t)
	encoder := encoding.New(cat, claimOnlyStore{}, t.TempDir())
	if err := encoder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	manager := runs.New(context.Background(), cat, map[string]runs.Registration{"encoding": {Name: "local-cwebp/v1", Executor: encoding.NewEncodingExecutor(encoder)}})
	defer manager.Close(context.Background())
	recommender := recommend.NewService(cat, nil, nil, nil, nil)
	router := New(nil, nil, nil, recommender, "", admin.NewService(cat, nil, recommender).WithRunManager(manager), "secret").WithEncoder(encoder).Router()
	res := runRequest(router, "POST", "/admin/api/runs", `{"stage":"encoding"}`, "local")
	if res.Code != 202 || jsonObject(t, res.Body.Bytes())["executor"] != "local-cwebp/v1" {
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
	// Legacy external embedding remains available without a token/model, even
	// when automatic local embedding is explicitly paused.
	if _, err := manager.SetAutomaticEmbedding(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "shared"} {
		external := New(nil, nil, images.NewService(cat, claimOnlyStore{}), recommender, token, nil, "").WithEncoder(encoder).Router()
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

func TestAdminAutomaticReleaseFailureUsesStandardJSON(t *testing.T) {
	cat := openAdminTestCatalog(t)
	manager := runs.New(context.Background(), cat, map[string]runs.Registration{"embedding": {Name: "test/v1", Executor: waitingRunExecutor{}}})
	defer manager.Close(context.Background())
	started := make(chan struct{})
	if err := manager.RegisterAutomaticEmbedding(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return errors.New("private-path secret-token")
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	router := New(nil, nil, nil, nil, "", admin.NewService(cat, nil, nil).WithRunManager(manager), "secret").Router()
	res := runRequest(router, "POST", "/admin/api/automatic/embedding", `{"enabled":false}`, "")
	if res.Code != 503 || jsonObject(t, res.Body.Bytes())["error"].(map[string]any)["code"] != "UNAVAILABLE" || strings.Contains(res.Body.String(), "secret-token") {
		t.Fatalf("release failure=%d %s", res.Code, res.Body.String())
	}
	status, err := manager.AutomaticEmbedding()
	if err != nil || status.Enabled || status.Running {
		t.Fatalf("failed pause reverted preference: %+v %v", status, err)
	}
}
