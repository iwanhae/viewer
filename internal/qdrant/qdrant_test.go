package qdrant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// testDim is a small vector size so fixtures stay readable; nothing in the
// client cares about the actual number.
const testDim = 4

// testCollection is the collection name every test client is built with. The
// package deliberately exports no default: production gets the name from
// configuration, so the tests carry their own.
const testCollection = "photo_embeddings"

// newTestClient points a client at srv with the test collection name.
func newTestClient(srv *httptest.Server, apiKey string) *Client {
	return New(srv.URL, apiKey, testCollection, testDim)
}

// testRecords builds n valid records spread over a few albums.
func testRecords(n int) []PhotoRecord {
	recs := make([]PhotoRecord, 0, n)
	for i := 0; i < n; i++ {
		recs = append(recs, PhotoRecord{
			AlbumID: "album-" + strconv.Itoa(i%3),
			Idx:     i,
			Hash:    fmt.Sprintf("hash-%04d", i),
			W:       640,
			H:       480,
			Vector:  []float32{float32(i), 1, 2, 3},
		})
	}
	return recs
}

// TestPointIDStable pins the ID derivation: if it ever changes, existing
// points become unreachable and re-upserts duplicate the whole corpus.
func TestPointIDStable(t *testing.T) {
	first := PointID("album-7", 3)
	if again := PointID("album-7", 3); again != first {
		t.Fatalf("PointID not deterministic: %q vs %q", first, again)
	}
	if PointID("album-7", 4) == first {
		t.Fatal("different idx must not collide")
	}
	if PointID("album-8", 3) == first {
		t.Fatal("different album must not collide")
	}
	// Recompute the derivation by hand, exactly as PointID does: same
	// namespace, same "albumID/idx" key.
	ns := uuid.NewSHA1(uuid.NameSpaceURL, []byte("viewer/photo-embeddings"))
	want := uuid.NewSHA1(ns, []byte("album-7/3")).String()
	if first != want {
		t.Fatalf("PointID=%q want %q", first, want)
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("PointID is not a UUID: %v", err)
	}
}

func TestUpsertPhotosChunksRequests(t *testing.T) {
	type captured struct {
		method string
		path   string
		query  string
		body   map[string]json.RawMessage
	}
	var got []captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		got = append(got, captured{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: body})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"status":"completed"},"status":"ok"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	if err := c.UpsertPhotos(context.Background(), testRecords(257)); err != nil {
		t.Fatalf("UpsertPhotos: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2 (256 + 1)", len(got))
	}
	wantCounts := []int{256, 1}
	for i, req := range got {
		if req.method != http.MethodPut {
			t.Errorf("request %d method = %s, want PUT", i, req.method)
		}
		if req.path != "/collections/"+testCollection+"/points" {
			t.Errorf("request %d path = %q", i, req.path)
		}
		if req.query != "wait=true" {
			t.Errorf("request %d query = %q, want wait=true", i, req.query)
		}
		var points []map[string]json.RawMessage
		if err := json.Unmarshal(req.body["points"], &points); err != nil {
			t.Fatalf("request %d: decode points: %v", i, err)
		}
		if len(points) != wantCounts[i] {
			t.Errorf("request %d carries %d points, want %d", i, len(points), wantCounts[i])
		}
		for _, p := range points {
			if len(p) != 3 {
				t.Errorf("point has keys %v, want exactly id, vector, payload", keys(p))
			}
		}
	}

	// Spot-check one point per chunk: ID derivation, float-array vector (the
	// live Qdrant rejects base64), and the exact payload field names.
	check := func(req captured, rec PhotoRecord, wantVec []float32) {
		t.Helper()
		var points []map[string]json.RawMessage
		if err := json.Unmarshal(req.body["points"], &points); err != nil {
			t.Fatalf("decode points: %v", err)
		}
		p := points[0]
		var id string
		if err := json.Unmarshal(p["id"], &id); err != nil {
			t.Fatalf("decode id: %v", err)
		}
		if want := PointID(rec.AlbumID, rec.Idx); id != want {
			t.Errorf("point id = %q, want %q", id, want)
		}
		vecRaw := strings.TrimSpace(string(p["vector"]))
		if !strings.HasPrefix(vecRaw, "[") {
			t.Fatalf("vector = %s, want a JSON float array (a quoted string would mean base64)", vecRaw)
		}
		var vec []float32
		if err := json.Unmarshal(p["vector"], &vec); err != nil {
			t.Fatalf("decode vector: %v", err)
		}
		if !reflect.DeepEqual(vec, wantVec) {
			t.Errorf("vector = %v, want %v", vec, wantVec)
		}
		var payload map[string]any
		if err := json.Unmarshal(p["payload"], &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		wantPayload := map[string]any{
			"album_id": rec.AlbumID,
			"idx":      float64(rec.Idx),
			"hash":     rec.Hash,
			"w":        float64(rec.W),
			"h":        float64(rec.H),
		}
		if !reflect.DeepEqual(payload, wantPayload) {
			t.Errorf("payload = %v, want %v", payload, wantPayload)
		}
	}
	recs := testRecords(257)
	check(got[0], recs[0], []float32{0, 1, 2, 3})
	check(got[1], recs[256], []float32{256, 1, 2, 3})
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestUpsertPhotosRejectsBadRecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: validation must fail before any HTTP call", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	if err := c.UpsertPhotos(context.Background(), nil); err != nil {
		t.Fatalf("UpsertPhotos(nil): %v", err)
	}
	good := PhotoRecord{AlbumID: "a", Hash: "h", Vector: make([]float32, testDim)}
	cases := []struct {
		name   string
		record PhotoRecord
	}{
		{"short vector", PhotoRecord{AlbumID: "a", Hash: "h", Vector: make([]float32, testDim-1)}},
		{"nil vector", PhotoRecord{AlbumID: "a", Hash: "h"}},
		{"empty vector", PhotoRecord{AlbumID: "a", Hash: "h", Vector: []float32{}}},
		{"empty album", PhotoRecord{Hash: "h", Vector: make([]float32, testDim)}},
		{"empty hash", PhotoRecord{AlbumID: "a", Vector: make([]float32, testDim)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.UpsertPhotos(context.Background(), []PhotoRecord{good, tc.record}); err == nil {
				t.Fatal("UpsertPhotos succeeded, want a validation error")
			}
		})
	}
}

func TestUpsertPhotosFailsWhenNotCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"status":"rejected"},"status":"ok"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	err := c.UpsertPhotos(context.Background(), testRecords(1))
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("err = %v, want a failure mentioning the uncompleted status", err)
	}
}

// groupSearchFixture has a single-hit group, a two-hit group (group_size is 1
// on the wire, but flattening must cope with more), and an empty group that
// must be skipped.
const groupSearchFixture = `{"result":{"groups":[
  {"id":"al-b","hits":[{"id":"p-b2","score":0.93,"payload":{"album_id":"al-b","idx":2,"hash":"h-b2","w":640,"h":480}}]},
  {"id":"al-c","hits":[{"id":7,"score":0.81,"payload":{"album_id":"al-c","idx":0,"hash":"h-c0","w":100,"h":50}},
                       {"id":8,"score":0.5,"payload":{"album_id":"al-c","idx":1,"hash":"h-c1","w":200,"h":60}}]},
  {"id":"al-d","hits":[]}
]},"status":"ok"}`

func TestGroupSearchRequestShapeAndFlattening(t *testing.T) {
	var reqBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+testCollection+"/points/query/groups" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(groupSearchFixture))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	hits, err := c.GroupSearch(context.Background(), "11111111-2222-3333-4444-555555555555", "album-a", "hash-a9", 5)
	if err != nil {
		t.Fatalf("GroupSearch: %v", err)
	}

	if got, ok := reqBody["query"].(string); !ok || got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("query = %#v (%T), want the point ID as a plain JSON string", reqBody["query"], reqBody["query"])
	}
	filter, _ := reqBody["filter"].(map[string]any)
	mustNot, _ := filter["must_not"].([]any)
	wantConds := []struct{ key, value string }{{"album_id", "album-a"}, {"hash", "hash-a9"}}
	if len(mustNot) != len(wantConds) {
		t.Fatalf("must_not has %d conditions, want %d", len(mustNot), len(wantConds))
	}
	for i, cond := range mustNot {
		m, _ := cond.(map[string]any)
		match, _ := m["match"].(map[string]any)
		if m["key"] != wantConds[i].key || match["value"] != wantConds[i].value {
			t.Errorf("must_not[%d] = %v, want key=%s value=%s", i, m, wantConds[i].key, wantConds[i].value)
		}
	}
	if reqBody["group_by"] != "album_id" {
		t.Errorf("group_by = %v, want album_id", reqBody["group_by"])
	}
	if reqBody["limit"] != float64(5) {
		t.Errorf("limit = %v, want 5", reqBody["limit"])
	}
	if reqBody["group_size"] != float64(1) {
		t.Errorf("group_size = %v, want 1", reqBody["group_size"])
	}
	if reqBody["with_payload"] != true {
		t.Errorf("with_payload = %v, want true", reqBody["with_payload"])
	}

	want := []PhotoHit{
		{AlbumID: "al-b", Idx: 2, Hash: "h-b2", W: 640, H: 480, Score: 0.93},
		{AlbumID: "al-c", Idx: 0, Hash: "h-c0", W: 100, H: 50, Score: 0.81},
		{AlbumID: "al-c", Idx: 1, Hash: "h-c1", W: 200, H: 60, Score: 0.5},
	}
	if len(hits) != len(want) {
		t.Fatalf("hits = %+v, want %d flattened hits", hits, len(want))
	}
	for i, w := range want {
		if hits[i] != w {
			t.Errorf("hit %d = %+v, want %+v", i, hits[i], w)
		}
	}
}

func TestGroupSearchNotFoundMeansNoHits(t *testing.T) {
	// A missing query point or collection is 404 on this Qdrant build, and
	// callers (wipe recovery) rely on that surfacing as an empty result.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection ` + "`" + `photo_embeddings` + "`" + ` doesn't exist!"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	hits, err := c.GroupSearch(context.Background(), "pid", "album-a", "hash-a9", 5)
	if err != nil {
		t.Fatalf("GroupSearch on 404: %v", err)
	}
	if hits != nil {
		t.Fatalf("hits = %+v, want nil", hits)
	}
}

func TestGroupSearchSkipsRequestWhenLimitNotPositive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: a non-positive limit must not hit the server", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	for _, limit := range []int{0, -1} {
		hits, err := c.GroupSearch(context.Background(), "pid", "album-a", "hash-a9", limit)
		if err != nil {
			t.Fatalf("GroupSearch(limit=%d): %v", limit, err)
		}
		if hits != nil {
			t.Fatalf("GroupSearch(limit=%d) hits = %+v, want nil", limit, hits)
		}
	}
}

// TestSearchByVectorGroupedRequestShapeAndHits pins the wire body: the query
// vector goes out as a plain JSON float array on the groups endpoint, grouped
// by album with one hit per group and no filter, and the reply is flattened in
// server order.
func TestSearchByVectorGroupedRequestShapeAndHits(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+testCollection+"/points/query/groups" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		rawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(groupSearchFixture))
	}))
	defer srv.Close()

	vector := []float32{0.25, -1, 2, 3}
	c := newTestClient(srv, "")
	hits, err := c.SearchByVectorGrouped(context.Background(), vector, 7)
	if err != nil {
		t.Fatalf("SearchByVectorGrouped: %v", err)
	}

	// Inspect the raw body before decoding: a quoted "query" value would mean
	// the vector went out base64-encoded, which this Qdrant build rejects.
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &body); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	queryRaw := strings.TrimSpace(string(body["query"]))
	if !strings.HasPrefix(queryRaw, "[") {
		t.Fatalf("query = %s, want a plain JSON float array (a quoted string would mean base64)", queryRaw)
	}
	var sentVector []float32
	if err := json.Unmarshal(body["query"], &sentVector); err != nil {
		t.Fatalf("decode query vector: %v", err)
	}
	if !reflect.DeepEqual(sentVector, vector) {
		t.Errorf("query vector = %v, want %v", sentVector, vector)
	}
	if string(body["group_by"]) != `"album_id"` {
		t.Errorf("group_by = %s, want album_id", body["group_by"])
	}
	if string(body["group_size"]) != "1" {
		t.Errorf("group_size = %s, want 1", body["group_size"])
	}
	if string(body["limit"]) != "7" {
		t.Errorf("limit = %s, want 7", body["limit"])
	}
	if string(body["with_payload"]) != "true" {
		t.Errorf("with_payload = %s, want true", body["with_payload"])
	}
	if filterRaw, ok := body["filter"]; ok {
		t.Errorf("body carries a filter %s, want none", filterRaw)
	}

	want := []PhotoHit{
		{AlbumID: "al-b", Idx: 2, Hash: "h-b2", W: 640, H: 480, Score: 0.93},
		{AlbumID: "al-c", Idx: 0, Hash: "h-c0", W: 100, H: 50, Score: 0.81},
		{AlbumID: "al-c", Idx: 1, Hash: "h-c1", W: 200, H: 60, Score: 0.5},
	}
	if len(hits) != len(want) {
		t.Fatalf("hits = %+v, want %d flattened hits in server order", hits, len(want))
	}
	for i, w := range want {
		if hits[i] != w {
			t.Errorf("hit %d = %+v, want %+v", i, hits[i], w)
		}
	}
}

func TestSearchByVectorGroupedNotFoundMeansNoHits(t *testing.T) {
	// A missing collection is 404 on this Qdrant build, and callers (wipe
	// recovery) rely on that surfacing as an empty result.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection ` + "`" + `photo_embeddings` + "`" + ` doesn't exist!"}}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	hits, err := c.SearchByVectorGrouped(context.Background(), make([]float32, testDim), 5)
	if err != nil {
		t.Fatalf("SearchByVectorGrouped on 404: %v", err)
	}
	if hits != nil {
		t.Fatalf("hits = %+v, want nil", hits)
	}
}

func TestSearchByVectorGroupedSkipsRequestWhenLimitNotPositive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: a non-positive limit must not hit the server", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	for _, limit := range []int{0, -1} {
		hits, err := c.SearchByVectorGrouped(context.Background(), make([]float32, testDim), limit)
		if err != nil {
			t.Fatalf("SearchByVectorGrouped(limit=%d): %v", limit, err)
		}
		if hits != nil {
			t.Fatalf("SearchByVectorGrouped(limit=%d) hits = %+v, want nil", limit, hits)
		}
	}
}

func TestSearchByVectorGroupedRejectsDimensionMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: a wrong-dimension vector must be rejected before any HTTP call", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	hits, err := c.SearchByVectorGrouped(context.Background(), make([]float32, testDim+1), 5)
	if err == nil {
		t.Fatalf("SearchByVectorGrouped with %d dimensions succeeded, want an error", testDim+1)
	}
	if !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("err = %v, want it to mention the dimension mismatch", err)
	}
	if hits != nil {
		t.Fatalf("hits = %+v, want nil on error", hits)
	}
}

func TestEnsureCollectionCreatesMissing(t *testing.T) {
	var createQuery string
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/"+testCollection:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"Not found"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/"+testCollection:
			createQuery = r.URL.RawQuery
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			_, _ = w.Write([]byte(`{"result":true,"status":"ok"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	if err := c.EnsureCollection(context.Background()); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	vectors, _ := createBody["vectors"].(map[string]any)
	if vectors == nil {
		t.Fatalf("create body = %v, want a vectors config", createBody)
	}
	if vectors["size"] != float64(testDim) {
		t.Errorf("size = %v, want %d", vectors["size"], testDim)
	}
	if vectors["distance"] != "Cosine" {
		t.Errorf("distance = %v, want Cosine", vectors["distance"])
	}
	if createQuery != "" {
		t.Errorf("create query = %q, want none", createQuery)
	}
}

func TestEnsureCollectionFailsWhenCreateNotOk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"Not found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":false,"status":"error"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	if err := c.EnsureCollection(context.Background()); err == nil {
		t.Fatal("EnsureCollection succeeded despite a failed create, want error")
	}
}

func TestEnsureCollectionValidatesExisting(t *testing.T) {
	cases := []struct {
		name    string
		vectors string
		wantErr string
	}{
		{"matching config", `{"size":4,"distance":"Cosine"}`, ""},
		{"wrong size", `{"size":8,"distance":"Cosine"}`, "size 8"},
		{"wrong distance", `{"size":4,"distance":"Euclid"}`, "Euclid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected %s to an existing collection", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"result":{"status":"green","config":{"params":{"vectors":%s}}},"status":"ok"}`, tc.vectors)
			}))
			defer srv.Close()

			c := newTestClient(srv, "")
			err := c.EnsureCollection(context.Background())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("EnsureCollection: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// scrollPoint is one stored point in a scroll fake. The JSON shape matches
// Qdrant's scroll reply: a point ID, the payload, and the vector.
type scrollPoint struct {
	ID      string `json:"id"`
	Payload struct {
		Hash string `json:"hash"`
	} `json:"payload"`
	Vector []float32 `json:"vector"`
}

// newScrollPoint builds a stored point with the given ID, payload hash, and
// vector.
func newScrollPoint(id, hash string, vector []float32) scrollPoint {
	var p scrollPoint
	p.ID = id
	p.Payload.Hash = hash
	p.Vector = vector
	return p
}

// scrollFakeRequest records what one scroll request asked for. NoFilter is
// true when the request carried no filter at all (the whole-collection ID
// scan). WithPayload mirrors the request's pointer: nil means the key was
// omitted, leaving Qdrant's default-true in effect.
type scrollFakeRequest struct {
	Hashes      []string
	NoFilter    bool
	WithPayload *bool
	WithVector  bool
	Limit       int
	Offset      *string
}

// scrollFake is a cursor-honoring stand-in for Qdrant's scroll endpoint. It
// holds a fixed store of points sorted by point ID (Qdrant scrolls in ID
// order), serves the subset matching the request's hash should-filter — or
// every stored point when the request carries no filter, as the ID scan does
// — at most limit points per request, resumes strictly after the request's
// offset when present, and reports next_page_offset — the last served
// point's ID — only when more matching points remain in that set.
type scrollFake struct {
	points   []scrollPoint
	requests []scrollFakeRequest
}

// newScrollFake builds a fake whose store is sorted by point ID, like the
// order a real scroll walks.
func newScrollFake(points []scrollPoint) *scrollFake {
	sort.Slice(points, func(i, j int) bool { return points[i].ID < points[j].ID })
	return &scrollFake{points: points}
}

// handler serves the fake's scroll endpoint, failing the test on anything but
// a well-formed scroll POST.
func (f *scrollFake) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+testCollection+"/points/scroll" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var body struct {
			Filter *struct {
				Should []struct {
					Key   string `json:"key"`
					Match struct {
						Value string `json:"value"`
					} `json:"match"`
				} `json:"should"`
			} `json:"filter"`
			WithPayload *bool   `json:"with_payload"`
			WithVector  bool    `json:"with_vector"`
			Limit       int     `json:"limit"`
			Offset      *string `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		wanted := make(map[string]bool)
		var hashes []string
		if body.Filter != nil {
			for _, c := range body.Filter.Should {
				if c.Key != "hash" {
					t.Errorf("should key = %q, want hash", c.Key)
				}
				wanted[c.Match.Value] = true
				hashes = append(hashes, c.Match.Value)
			}
		}
		f.requests = append(f.requests, scrollFakeRequest{
			Hashes:      hashes,
			NoFilter:    body.Filter == nil,
			WithPayload: body.WithPayload,
			WithVector:  body.WithVector,
			Limit:       body.Limit,
			Offset:      body.Offset,
		})

		// Collect every stored point matching the filter — or every point at
		// all when the request carries no filter — in ID order, resuming
		// strictly after the cursor.
		start := 0
		if body.Offset != nil {
			for start < len(f.points) && f.points[start].ID <= *body.Offset {
				start++
			}
		}
		matching := make([]scrollPoint, 0)
		for _, p := range f.points[start:] {
			if body.Filter == nil || wanted[p.Payload.Hash] {
				matching = append(matching, p)
			}
		}
		served := matching
		if len(matching) > body.Limit {
			served = matching[:body.Limit]
		}
		// Serve exactly what the request asked for, like a real Qdrant: the
		// id always, the payload unless with_payload explicitly denied it
		// (Qdrant's default is true), the vector only when with_vector.
		includePayload := body.WithPayload == nil || *body.WithPayload
		servedJSON := make([]map[string]any, len(served))
		for i, p := range served {
			point := map[string]any{"id": p.ID}
			if includePayload {
				point["payload"] = map[string]any{"hash": p.Payload.Hash}
			}
			if body.WithVector {
				point["vector"] = p.Vector
			}
			servedJSON[i] = point
		}
		result := map[string]any{"points": servedJSON}
		if len(matching) > body.Limit {
			// More matching points remain, so hand back the cursor: the ID
			// of the last point this page served.
			result["next_page_offset"] = served[len(served)-1].ID
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "status": "ok"})
	})
}

func TestRetrieveVectorsByHashes(t *testing.T) {
	// One point per hash for hash-0000..hash-0250; the remaining requested
	// hashes have no stored vector and must come back absent.
	stored := make(map[string][]float32)
	points := make([]scrollPoint, 0, 251)
	for i := 0; i <= 250; i++ {
		h := fmt.Sprintf("hash-%04d", i)
		v := []float32{float32(i), float32(i), 0, 1}
		stored[h] = v
		points = append(points, newScrollPoint(fmt.Sprintf("%08d-0000-0000-0000-000000000000", i), h, v))
	}
	fake := newScrollFake(points)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	var hashes []string
	for i := 0; i < 300; i++ {
		hashes = append(hashes, fmt.Sprintf("hash-%04d", i))
	}
	hashes = append(hashes, hashes[0], hashes[123]) // duplicates must be requested once

	c := newTestClient(srv, "")
	got, err := c.RetrieveVectorsByHashes(context.Background(), hashes)
	if err != nil {
		t.Fatalf("RetrieveVectorsByHashes: %v", err)
	}
	if len(got) != len(stored) {
		t.Fatalf("result has %d vectors, want %d", len(got), len(stored))
	}
	for h, want := range stored {
		if v := got[h]; !reflect.DeepEqual(v, want) {
			t.Errorf("%s vector = %v, want %v", h, v, want)
		}
	}
	if _, ok := got["hash-0299"]; ok {
		t.Error("hash-0299 has no stored vector but is present in the result")
	}

	// 302 inputs dedupe to 300 unique hashes chunked at batchSize; one point
	// per hash fits a single page per chunk, so no request carries a cursor.
	if len(fake.requests) != 2 {
		t.Fatalf("requests = %d, want 2 (256 + 44)", len(fake.requests))
	}
	if len(fake.requests[0].Hashes) != 256 || len(fake.requests[1].Hashes) != 44 {
		t.Errorf("chunk sizes = %d and %d, want 256 and 44", len(fake.requests[0].Hashes), len(fake.requests[1].Hashes))
	}
	seen := map[string]int{}
	for i, req := range fake.requests {
		if req.Limit != batchSize {
			t.Errorf("request %d limit = %d, want %d", i, req.Limit, batchSize)
		}
		if !req.WithVector {
			t.Errorf("request %d did not ask for with_vector", i)
		}
		if req.Offset != nil {
			t.Errorf("request %d offset = %q, want none: one point per hash fits one page", i, *req.Offset)
		}
		for _, h := range req.Hashes {
			seen[h]++
		}
	}
	if len(seen) != 300 {
		t.Fatalf("unique hashes requested = %d, want 300", len(seen))
	}
	for h, n := range seen {
		if n != 1 {
			t.Errorf("hash %s was requested %d times, want once", h, n)
		}
	}
}

// TestRetrieveVectorsByHashesPagesSharedHashes is the regression for the
// truncation bug: a hash filter matches one point per referencing photo, and
// dedup spreads one blob's photos across albums, so 40 hashes can own 520
// points — far more than one page of batchSize. The client must follow the
// cursor until the filtered set is exhausted and return every hash's vector.
func TestRetrieveVectorsByHashesPagesSharedHashes(t *testing.T) {
	const (
		numHashes     = 40
		photosPerHash = 13 // photos referencing each hash, each its own point
	)
	want := make(map[string][]float32, numHashes)
	points := make([]scrollPoint, 0, numHashes*photosPerHash)
	n := 0
	for i := 0; i < numHashes; i++ {
		h := fmt.Sprintf("hash-%04d", i)
		v := []float32{float32(i), 1, 2, 3}
		want[h] = v
		for j := 0; j < photosPerHash; j++ {
			// Consecutive sortable UUIDs keep one hash's points adjacent in
			// the ID-ordered scroll, so page one provably cannot cover every
			// hash — exactly the shape the old code truncated away.
			id := fmt.Sprintf("%08d-0000-0000-0000-000000000000", n)
			points = append(points, newScrollPoint(id, h, v))
			n++
		}
	}
	fake := newScrollFake(points)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	input := make([]string, 0, numHashes)
	for i := 0; i < numHashes; i++ {
		input = append(input, fmt.Sprintf("hash-%04d", i))
	}
	c := newTestClient(srv, "")
	got, err := c.RetrieveVectorsByHashes(context.Background(), input)
	if err != nil {
		t.Fatalf("RetrieveVectorsByHashes: %v", err)
	}

	// 520 points at limit 256 must be paged as 256 + 256 + 8: at least three
	// requests, each after the first resuming from the previous cursor.
	if len(fake.requests) < 3 {
		t.Fatalf("requests = %d, want at least 3 pages for %d points at limit %d",
			len(fake.requests), numHashes*photosPerHash, batchSize)
	}
	if fake.requests[0].Offset != nil {
		t.Errorf("first page offset = %q, want none", *fake.requests[0].Offset)
	}
	wantCursors := []string{points[255].ID, points[511].ID}
	for i, cursor := range wantCursors {
		req := fake.requests[i+1]
		if req.Offset == nil {
			t.Errorf("page %d carried no offset, want cursor %q", i+2, cursor)
			continue
		}
		if *req.Offset != cursor {
			t.Errorf("page %d offset = %q, want the previous page's cursor %q", i+2, *req.Offset, cursor)
		}
	}

	// The point of the fix: no hash may be silently dropped.
	if len(got) != numHashes {
		t.Fatalf("result has %d vectors, want all %d (silent truncation)", len(got), numHashes)
	}
	for h, v := range want {
		if got := got[h]; !reflect.DeepEqual(got, v) {
			t.Errorf("%s vector = %v, want %v", h, got, v)
		}
	}
}

// TestRetrieveVectorsByHashesStopsAtPageCap guards against a misbehaving
// server that always reports another page: the client must give up with the
// page-cap error after maxScrollPages requests instead of scrolling forever.
func TestRetrieveVectorsByHashesStopsAtPageCap(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+testCollection+"/points/scroll" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		pages++
		w.Header().Set("Content-Type", "application/json")
		// Broken server: a fresh non-null cursor every time, never a last page.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"points":           []any{},
				"next_page_offset": fmt.Sprintf("%08d-0000-0000-0000-000000000000", pages),
			},
			"status": "ok",
		})
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	got, err := c.RetrieveVectorsByHashes(context.Background(), []string{"hash-0000"})
	if err == nil {
		t.Fatal("RetrieveVectorsByHashes succeeded against an endless scroll, want the page-cap error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("did not terminate after %d pages", maxScrollPages)) {
		t.Fatalf("err = %v, want the page-cap error", err)
	}
	if pages != maxScrollPages {
		t.Errorf("server saw %d requests, want exactly %d before giving up", pages, maxScrollPages)
	}
	if len(got) != 0 {
		t.Errorf("got = %d vectors, want none on error", len(got))
	}
}

func TestRetrieveVectorsByHashesEmptyInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: empty input must not hit the server", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	got, err := c.RetrieveVectorsByHashes(context.Background(), nil)
	if err != nil {
		t.Fatalf("RetrieveVectorsByHashes: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got = %v, want an empty map", got)
	}
}

// TestScrollPointIDsPagesWholeCollection pins the ID scan's cursor chain: a
// 2500-point collection must come back as exactly three pages (1024 + 1024 +
// 452), every request filterless and asking for neither payload nor vector,
// the first carrying no offset, and each later one resuming from the previous
// page's last-point ID. All 2500 IDs come back once, in scroll order.
func TestScrollPointIDsPagesWholeCollection(t *testing.T) {
	const total = 2500
	points := make([]scrollPoint, 0, total)
	for i := 0; i < total; i++ {
		// Payload hash and vector are irrelevant to the ID scan; the ID
		// pattern keeps the store's ID order stable and inspectable.
		points = append(points, newScrollPoint(fmt.Sprintf("%08d-0000-0000-0000-000000000000", i), "hash-irrelevant", nil))
	}
	fake := newScrollFake(points)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	c := newTestClient(srv, "")
	got, err := c.ScrollPointIDs(context.Background())
	if err != nil {
		t.Fatalf("ScrollPointIDs: %v", err)
	}

	// 2500 points at limit 1024 page as 1024 + 1024 + 452: three requests.
	if len(fake.requests) != 3 {
		t.Fatalf("requests = %d, want 3 (1024 + 1024 + 452)", len(fake.requests))
	}
	if fake.requests[0].Offset != nil {
		t.Errorf("first page offset = %q, want none", *fake.requests[0].Offset)
	}
	wantCursors := []string{points[1023].ID, points[2047].ID}
	for i, cursor := range wantCursors {
		req := fake.requests[i+1]
		if req.Offset == nil {
			t.Errorf("page %d carried no offset, want cursor %q", i+2, cursor)
			continue
		}
		if *req.Offset != cursor {
			t.Errorf("page %d offset = %q, want the previous page's cursor %q", i+2, *req.Offset, cursor)
		}
	}
	for i, req := range fake.requests {
		if !req.NoFilter {
			t.Errorf("request %d carried a filter, want none: the scan covers the whole collection", i)
		}
		if req.WithPayload == nil || *req.WithPayload {
			t.Errorf("request %d with_payload = %v, want an explicit false", i, req.WithPayload)
		}
		if req.WithVector {
			t.Errorf("request %d asked for with_vector, want none", i)
		}
		if req.Limit != idScrollPageSize {
			t.Errorf("request %d limit = %d, want %d", i, req.Limit, idScrollPageSize)
		}
	}

	// Every ID exactly once, in the ID order the scroll walked: matching the
	// full sorted store element-for-element proves both at once.
	if len(got) != total {
		t.Fatalf("got %d IDs, want %d", len(got), total)
	}
	for i, id := range got {
		if want := points[i].ID; id != want {
			t.Errorf("ids[%d] = %q, want %q", i, id, want)
		}
	}
}

// TestScrollPointIDsEmptyCollection checks the empty-collection contract: a
// single request that returns no points and no cursor yields an empty,
// non-nil slice and no error.
func TestScrollPointIDsEmptyCollection(t *testing.T) {
	fake := newScrollFake(nil)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	c := newTestClient(srv, "")
	got, err := c.ScrollPointIDs(context.Background())
	if err != nil {
		t.Fatalf("ScrollPointIDs: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("got = %v, want an empty non-nil slice", got)
	}
	if len(fake.requests) != 1 {
		t.Errorf("requests = %d, want exactly 1 for an empty collection", len(fake.requests))
	}
}

// TestScrollPointIDsStopsAtPageCap guards against a misbehaving server that
// always reports another page: the client must give up with the page-cap
// error after maxScrollPages requests instead of scrolling forever.
func TestScrollPointIDsStopsAtPageCap(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+testCollection+"/points/scroll" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		pages++
		w.Header().Set("Content-Type", "application/json")
		// Broken server: a fresh non-null cursor every time, never a last page.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"points":           []any{},
				"next_page_offset": fmt.Sprintf("%08d-0000-0000-0000-000000000000", pages),
			},
			"status": "ok",
		})
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	got, err := c.ScrollPointIDs(context.Background())
	if err == nil {
		t.Fatal("ScrollPointIDs succeeded against an endless scroll, want the page-cap error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("did not terminate after %d pages", maxScrollPages)) {
		t.Fatalf("err = %v, want the page-cap error", err)
	}
	if pages != maxScrollPages {
		t.Errorf("server saw %d requests, want exactly %d before giving up", pages, maxScrollPages)
	}
	if got != nil {
		t.Errorf("got = %v IDs, want none on error", got)
	}
}

func TestScrollPointIDsFailsOnNon2xx(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	got, err := c.ScrollPointIDs(context.Background())
	if err == nil {
		t.Fatal("ScrollPointIDs succeeded against a 500, want error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention the HTTP status", err)
	}
	if got != nil {
		t.Errorf("got = %v IDs, want none on error", got)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1: a non-2xx must abort the scan", requests)
	}
}

func TestDeleteByAlbum(t *testing.T) {
	var reqBody map[string]any
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+testCollection+"/points/delete" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		query = r.URL.RawQuery
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"operation_id":1,"status":"completed"},"status":"ok"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	if err := c.DeleteByAlbum(context.Background(), "album-9"); err != nil {
		t.Fatalf("DeleteByAlbum: %v", err)
	}
	if query != "wait=true" {
		t.Errorf("query = %q, want wait=true", query)
	}
	filter, _ := reqBody["filter"].(map[string]any)
	must, _ := filter["must"].([]any)
	if len(must) != 1 {
		t.Fatalf("must has %d conditions, want 1", len(must))
	}
	cond, _ := must[0].(map[string]any)
	match, _ := cond["match"].(map[string]any)
	if cond["key"] != "album_id" || match["value"] != "album-9" {
		t.Errorf("filter = %v, want album_id match album-9", reqBody)
	}
}

func TestCountVectors(t *testing.T) {
	var reqBody map[string]any
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+testCollection+"/points/count" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		query = r.URL.RawQuery
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"count":42},"status":"ok"}`))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	got, err := c.CountVectors(context.Background())
	if err != nil {
		t.Fatalf("CountVectors: %v", err)
	}
	if got != 42 {
		t.Errorf("CountVectors = %d, want 42", got)
	}
	if reqBody["exact"] != true {
		t.Errorf("body = %v, want exact:true", reqBody)
	}
	if query != "" {
		t.Errorf("query = %q, want none", query)
	}
}

func TestAPIKeyHeaderOnlyWhenConfigured(t *testing.T) {
	for _, key := range []string{"", "sekret"} {
		t.Run("key="+strconv.Quote(key), func(t *testing.T) {
			var got http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"count":0},"status":"ok"}`))
			}))
			defer srv.Close()

			c := New(srv.URL, key, testCollection, testDim)
			if _, err := c.CountVectors(context.Background()); err != nil {
				t.Fatalf("CountVectors: %v", err)
			}
			sent, present := got["Api-Key"]
			if key == "" {
				if present {
					t.Errorf("api-key header sent without a configured key: %v", sent)
				}
				return
			}
			if !present || got.Get("api-key") != key {
				t.Errorf("api-key header = %v, want %q", sent, key)
			}
		})
	}
}

func TestNon2xxErrorCarriesStatusAndBodySnippet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		// Longer than the 512-byte cap, with a marker past it.
		_, _ = w.Write([]byte(strings.Repeat("x", 600) + "TAILMARKER"))
	}))
	defer srv.Close()

	c := newTestClient(srv, "")
	_, err := c.CountVectors(context.Background())
	if err == nil {
		t.Fatal("CountVectors succeeded against a 500, want error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention the HTTP status", err)
	}
	if !strings.Contains(err.Error(), strings.Repeat("x", 512)) {
		t.Errorf("error %q is missing the first 512 bytes of the body", err)
	}
	if strings.Contains(err.Error(), "TAILMARKER") {
		t.Errorf("error %q includes body content beyond the 512-byte cap", err)
	}
}

func TestCanceledContextAbortsRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server received a request on a canceled context")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newTestClient(srv, "")
	if _, err := c.CountVectors(ctx); err == nil {
		t.Fatal("CountVectors succeeded on a canceled context, want error")
	}
}
