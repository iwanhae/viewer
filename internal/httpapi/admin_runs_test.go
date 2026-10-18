package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"viewer/internal/admin"
	"viewer/internal/catalog"
	"viewer/internal/runs"
)

type waitingRunExecutor struct{ started chan struct{} }

func (e waitingRunExecutor) Execute(ctx context.Context, _ catalog.JobRun, _ catalog.JobRunItem) (runs.Outcome, error) {
	e.started <- struct{}{}
	<-ctx.Done()
	return runs.Outcome{}, ctx.Err()
}

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
func assertRunJSON(t *testing.T, obj map[string]any) {
	t.Helper()
	assertJSONKeys(t, obj, "id", "stage", "actor", "executor", "status", "cancelRequested", "createdAt", "updatedAt", "startedAt", "finishedAt", "lastError", "counts")
	counts, ok := obj["counts"].(map[string]any)
	if !ok {
		t.Fatalf("counts=%v", obj["counts"])
	}
	assertJSONKeys(t, counts, "total", "pending", "running", "succeeded", "failed", "busy", "skipped", "cancelled", "interrupted")
	for _, key := range []string{"createdAt", "updatedAt"} {
		if _, err := time.Parse(time.RFC3339Nano, obj[key].(string)); err != nil {
			t.Fatalf("%s=%v", key, obj[key])
		}
	}
}

// This pins the additive Admin JSON contract, including nullable timestamps,
// complete outcome counts, opaque pagination, and absence of private claim tokens.
func TestAdminRunsJSONContractAndLifecycle(t *testing.T) {
	ctx := context.Background()
	cat := openAdminTestCatalog(t)
	for _, hash := range []string{"a", "b"} {
		if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: 10, ContentType: "image/jpeg"}); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan struct{}, 2)
	manager := runs.New(ctx, cat, map[string]runs.Registration{"embedding": {Name: "test-embed/v1", Executor: waitingRunExecutor{started: started}}})
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := manager.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	service := admin.NewService(cat, nil, nil).WithRunManager(manager)
	router := New(nil, nil, nil, nil, "", service, "secret").Router()
	created := runRequest(router, "POST", "/admin/api/runs", `{"stage":"embedding"}`, "submission")
	if created.Code != 202 {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	obj := jsonObject(t, created.Body.Bytes())
	assertRunJSON(t, obj)
	id := obj["id"].(string)
	if obj["status"] != "queued" || obj["executor"] != "test-embed/v1" || obj["stage"] != "embedding" || obj["actor"] != "operator" || obj["cancelRequested"] != false || obj["startedAt"] != nil || obj["finishedAt"] != nil || obj["counts"].(map[string]any)["total"] != float64(2) || created.Header().Get("Location") != "/admin/api/runs/"+id {
		t.Fatalf("creation contract=%v", obj)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("local execution did not start")
	}
	for _, test := range []struct {
		body, key string
		status    int
	}{
		{`{"stage":"embedding"}`, "submission", 200},
		{`{"stage":"encoding"}`, "submission", 409},
		{`{"stage":"embedding"}`, "another", 409},
		{`{"stage":"embedding"}`, "", 400},
		{`{"stage":"other"}`, "bad-stage", 400},
		{`{"stage":"embedding","extra":true}`, "extra", 400},
		{`{"stage":"embedding"} {}`, "trailing", 400},
		{`{"stage":"encoding"}`, "unavailable", 503},
	} {
		res := runRequest(router, "POST", "/admin/api/runs", test.body, test.key)
		if res.Code != test.status {
			t.Errorf("%s %s: %d %s", test.body, test.key, res.Code, res.Body.String())
		}
	}
	list := runRequest(router, "GET", "/admin/api/runs?limit=1", "", "")
	if list.Code != 200 {
		t.Fatal(list.Body.String())
	}
	listObj := jsonObject(t, list.Body.Bytes())
	assertJSONKeys(t, listObj, "runs", "nextCursor")
	if listObj["nextCursor"] != nil || len(listObj["runs"].([]any)) != 1 {
		t.Fatalf("list=%v", listObj)
	}
	detail := runRequest(router, "GET", "/admin/api/runs/"+id+"?limit=1", "", "")
	if detail.Code != 200 {
		t.Fatalf("detail=%d %s", detail.Code, detail.Body.String())
	}
	page := jsonObject(t, detail.Body.Bytes())
	assertJSONKeys(t, page, "run", "items", "nextCursor")
	assertRunJSON(t, page["run"].(map[string]any))
	items := page["items"].([]any)
	assertJSONKeys(t, items[0].(map[string]any), "hash", "status", "startedAt", "finishedAt", "updatedAt", "lastError")
	if items[0].(map[string]any)["hash"] != "a" || page["nextCursor"] == nil {
		t.Fatalf("items=%v", page)
	}
	next := runRequest(router, "GET", "/admin/api/runs/"+id+"?limit=1&cursor="+page["nextCursor"].(string), "", "")
	if next.Code != 200 {
		t.Fatal(next.Body.String())
	}
	nextObj := jsonObject(t, next.Body.Bytes())
	if nextObj["nextCursor"] != nil || nextObj["items"].([]any)[0].(map[string]any)["hash"] != "b" {
		t.Fatalf("next=%v", nextObj)
	}
	for i := 0; i < 2; i++ {
		res := runRequest(router, "POST", "/admin/api/runs/"+id+"/cancel", `{}`, "")
		if res.Code != 200 || jsonObject(t, res.Body.Bytes())["cancelRequested"] != true {
			t.Fatalf("cancel=%d %s", res.Code, res.Body.String())
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := runRequest(router, "GET", "/admin/api/runs/"+id, "", "")
		final := jsonObject(t, res.Body.Bytes())["run"].(map[string]any)
		if final["status"] == "cancelled" {
			if final["counts"].(map[string]any)["cancelled"] != float64(2) {
				t.Fatalf("final=%v", final)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancellation not completed=%v", final)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAdminRunsUnavailableAuthMutationAndPagination(t *testing.T) {
	cat := openAdminTestCatalog(t)
	manager := runs.New(context.Background(), cat, nil)
	defer manager.Close(context.Background())
	router := New(nil, nil, nil, nil, "", admin.NewService(cat, nil, nil).WithRunManager(manager), "secret").Router()
	res := runRequest(router, "POST", "/admin/api/runs", `{"stage":"encoding"}`, "key")
	if res.Code != 503 || jsonObject(t, res.Body.Bytes())["error"].(map[string]any)["code"] != "EXECUTOR_UNAVAILABLE" {
		t.Fatalf("unavailable=%d %s", res.Code, res.Body.String())
	}
	page, err := cat.JobRuns(context.Background(), 10, "")
	if err != nil || len(page.Runs) != 0 {
		t.Fatalf("unavailable persisted run=%+v %v", page, err)
	}
	for _, test := range []struct{ method, path string }{
		{"POST", "/admin/api/runs"}, {"GET", "/admin/api/runs"}, {"GET", "/admin/api/runs/missing"}, {"POST", "/admin/api/runs/missing/cancel"},
	} {
		req := httptest.NewRequest(test.method, test.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("unprotected %s %s: %d", test.method, test.path, rec.Code)
		}
		if test.method == "POST" {
			req = httptest.NewRequest(test.method, test.path, strings.NewReader(`{}`))
			req.SetBasicAuth("operator", "secret")
			rec = httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != 403 {
				t.Errorf("missing CSRF guard %s: %d", test.path, rec.Code)
			}
			req = httptest.NewRequest(test.method, test.path, strings.NewReader(`{}`))
			req.SetBasicAuth("operator", "secret")
			req.Header.Set("X-Admin-Action", "1")
			req.Header.Set("Origin", "https://evil.invalid")
			rec = httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != 403 {
				t.Errorf("cross-origin mutation %s: %d", test.path, rec.Code)
			}
		}
	}
	for _, path := range []string{"/admin/api/runs?limit=0", "/admin/api/runs?limit=101", "/admin/api/runs?cursor=not-json", "/admin/api/runs?limit=1&limit=2", "/admin/api/runs?unknown=true", "/admin/api/runs/missing?cursor=bad"} {
		res := runRequest(router, "GET", path, "", "")
		if res.Code != 400 {
			t.Errorf("invalid pagination %s: %d %s", path, res.Code, res.Body.String())
		}
	}
	for _, test := range []struct{ method, path string }{{"GET", "/admin/api/runs/missing"}, {"POST", "/admin/api/runs/missing/cancel"}} {
		res := runRequest(router, test.method, test.path, `{}`, "")
		if res.Code != 404 {
			t.Errorf("missing run %s: %d %s", test.path, res.Code, res.Body.String())
		}
	}
}
