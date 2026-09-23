package qdrant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// testDim is a small vector size so fixtures stay readable; nothing in the
// client cares about the actual number.
const testDim = 4

// newTestClient points a client at srv with the default collection name.
func newTestClient(srv *httptest.Server, apiKey string) *Client {
	return New(srv.URL, apiKey, DefaultCollection, testDim)
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
		if req.path != "/collections/"+DefaultCollection+"/points" {
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
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+DefaultCollection+"/points/query/groups" {
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

func TestEnsureCollectionCreatesMissing(t *testing.T) {
	var createQuery string
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/"+DefaultCollection:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"Not found"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/"+DefaultCollection:
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

func TestRetrieveVectorsByHashes(t *testing.T) {
	// Vectors exist for hash-0000..hash-0250 only; the rest come back absent.
	stored := make(map[string][]float32)
	for i := 0; i <= 250; i++ {
		stored[fmt.Sprintf("hash-%04d", i)] = []float32{float32(i), float32(i), 0, 1}
	}
	type chunk struct {
		should     []string
		limit      int
		withVector bool
	}
	var requests []chunk
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+DefaultCollection+"/points/scroll" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var body struct {
			Filter struct {
				Should []struct {
					Key   string `json:"key"`
					Match struct {
						Value string `json:"value"`
					} `json:"match"`
				} `json:"should"`
			} `json:"filter"`
			WithVector bool `json:"with_vector"`
			Limit      int  `json:"limit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		hashes := make([]string, 0, len(body.Filter.Should))
		for _, c := range body.Filter.Should {
			if c.Key != "hash" {
				t.Errorf("should key = %q, want hash", c.Key)
			}
			hashes = append(hashes, c.Match.Value)
		}
		requests = append(requests, chunk{should: hashes, limit: body.Limit, withVector: body.WithVector})

		type point struct {
			Payload struct {
				Hash string `json:"hash"`
			} `json:"payload"`
			Vector []float32 `json:"vector"`
		}
		points := make([]point, 0)
		for _, h := range hashes {
			if v, ok := stored[h]; ok {
				points = append(points, point{Payload: struct {
					Hash string `json:"hash"`
				}{Hash: h}, Vector: v})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"points": points}, "status": "ok"})
	}))
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

	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2 (256 + 44)", len(requests))
	}
	if len(requests[0].should) != 256 || len(requests[1].should) != 44 {
		t.Errorf("chunk sizes = %d and %d, want 256 and 44", len(requests[0].should), len(requests[1].should))
	}
	for i, req := range requests {
		if req.limit != batchSize {
			t.Errorf("request %d limit = %d, want %d (one page must cover the chunk)", i, req.limit, batchSize)
		}
		if !req.withVector {
			t.Errorf("request %d did not ask for with_vector", i)
		}
	}
	seen := map[string]int{}
	for _, req := range requests {
		for _, h := range req.should {
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

func TestDeleteByAlbum(t *testing.T) {
	var reqBody map[string]any
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+DefaultCollection+"/points/delete" {
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

func TestCount(t *testing.T) {
	var reqBody map[string]any
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/"+DefaultCollection+"/points/count" {
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
	got, err := c.Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 42 {
		t.Errorf("Count = %d, want 42", got)
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

			c := New(srv.URL, key, DefaultCollection, testDim)
			if _, err := c.Count(context.Background()); err != nil {
				t.Fatalf("Count: %v", err)
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
	_, err := c.Count(context.Background())
	if err == nil {
		t.Fatal("Count succeeded against a 500, want error")
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
	if _, err := c.Count(ctx); err == nil {
		t.Fatal("Count succeeded on a canceled context, want error")
	}
}
