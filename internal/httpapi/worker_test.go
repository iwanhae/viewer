package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/recommend"
)

// encodeVector is the wire counterpart of the server's decodeVector: little
// endian float32, base64.
func encodeVector(vector []float32) string {
	raw := make([]byte, len(vector)*4)
	for i, value := range vector {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(value))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func flatVector(dim int, value float32) []float32 {
	vector := make([]float32, dim)
	for i := range vector {
		vector[i] = value
	}
	return vector
}

type claimedBlobWire struct {
	Hash        string `json:"hash"`
	SizeBytes   int64  `json:"sizeBytes"`
	ContentType string `json:"contentType"`
	BlobKey     string `json:"blobKey"`
	GetURL      string `json:"getUrl"`
}

type claimWire struct {
	EmbeddingDim int               `json:"embeddingDim"`
	LeaseUntil   time.Time         `json:"leaseUntil"`
	Claimed      []claimedBlobWire `json:"claimed"`
}

type resultsWire struct {
	Updated  int `json:"updated"`
	Rejected []struct {
		Hash   string `json:"hash"`
		Reason string `json:"reason"`
	} `json:"rejected"`
}

func decodeClaim(t *testing.T, rec *httptest.ResponseRecorder) claimWire {
	t.Helper()
	var payload claimWire
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode claim response: %v body=%s", err, rec.Body.String())
	}
	return payload
}

func decodeResults(t *testing.T, rec *httptest.ResponseRecorder) resultsWire {
	t.Helper()
	var payload resultsWire
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode results response: %v body=%s", err, rec.Body.String())
	}
	return payload
}

// rejectionPairs renders a results response as sorted "hash reason" strings,
// so one hash may carry several rejections.
func rejectionPairs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	payload := decodeResults(t, rec)
	pairs := make([]string, 0, len(payload.Rejected))
	for _, item := range payload.Rejected {
		pairs = append(pairs, item.Hash+" "+item.Reason)
	}
	sort.Strings(pairs)
	return pairs
}

// uploadOnePhotoAlbum finalizes an album with a single photo and returns the
// photo, whose hash keys both the image route and the worker claims.
func uploadOnePhotoAlbum(t *testing.T, harness *flowHarness, filename string, fill uint8) catalog.Photo {
	t.Helper()
	zipData := testZip(t, map[string][]byte{"only.png": testPNG(t, 4, 2, fill)}, []string{"only.png"})
	albumID := harness.uploadZip(t, filename, zipData)
	harness.finalizeAndWait(t, albumID)
	photo, err := harness.catalog.PhotoAt(context.Background(), albumID, 0)
	if err != nil {
		t.Fatalf("photo at 0: %v", err)
	}
	return *photo
}

func TestWorkerClaimLeasesBlobsAndHandsOutDownloads(t *testing.T) {
	harness := newFlowHarness(t)
	photoA := uploadOnePhotoAlbum(t, harness, "claim-a.zip", 10)
	photoB := uploadOnePhotoAlbum(t, harness, "claim-b.zip", 20)

	rec := harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeClaim(t, rec)
	if payload.EmbeddingDim != catalog.EmbeddingDim {
		t.Fatalf("embeddingDim=%d want=%d", payload.EmbeddingDim, catalog.EmbeddingDim)
	}
	if payload.LeaseUntil.IsZero() {
		t.Fatalf("claim response must carry a lease deadline")
	}
	if len(payload.Claimed) != 2 {
		t.Fatalf("claimed=%d want=2 body=%s", len(payload.Claimed), rec.Body.String())
	}
	claimed := make(map[string]claimedBlobWire, len(payload.Claimed))
	for _, blob := range payload.Claimed {
		claimed[blob.Hash] = blob
	}
	for _, photo := range []catalog.Photo{photoA, photoB} {
		blob, ok := claimed[photo.Hash]
		if !ok {
			t.Fatalf("claim is missing hash %s: %+v", photo.Hash, payload.Claimed)
		}
		if blob.BlobKey != "blobs/"+photo.Hash {
			t.Fatalf("blobKey=%q want blobs/%s", blob.BlobKey, photo.Hash)
		}
		// The memoryS3 stub presigns as "memory://<key>", so this proves the
		// URL points at the blob object.
		if blob.GetURL != "memory://blobs/"+photo.Hash {
			t.Fatalf("getUrl=%q want the presigned blob URL", blob.GetURL)
		}
		if blob.ContentType != "image/png" {
			t.Fatalf("contentType=%q want=image/png", blob.ContentType)
		}
	}

	// Both rows are leased now: a second worker gets an empty batch instead of
	// double work.
	again := decodeClaim(t, harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`)))
	if len(again.Claimed) != 0 {
		t.Fatalf("second claim returned %d blobs, want 0", len(again.Claimed))
	}

	// Pending stays derived in the catalog counts (still 2, in-flight
	// included) while processing makes the leases observable.
	counts, err := harness.catalog.EmbeddingCounts(context.Background())
	if err != nil {
		t.Fatalf("embedding counts: %v", err)
	}
	if counts.Total != 2 || counts.Pending != 2 || counts.Processing != 2 {
		t.Fatalf("counts=%+v want total=2 pending=2 processing=2", counts)
	}
}

func TestWorkerResultsRoundTripReachesSearchAndRecommendations(t *testing.T) {
	harness := newFlowHarness(t)

	// Two albums: recommendations are cross-album only, so the query photo's
	// neighbor has to live in the other one.
	first := uploadOnePhotoAlbum(t, harness, "roundtrip-a.zip", 30)
	second := uploadOnePhotoAlbum(t, harness, "roundtrip-b.zip", 31)

	claimed := decodeClaim(t, harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`)))
	if len(claimed.Claimed) != 2 {
		t.Fatalf("claimed=%d want=2", len(claimed.Claimed))
	}

	body, err := json.Marshal(map[string]any{
		"results": []map[string]string{
			{"hash": first.Hash, "status": "ready", "vectorB64": encodeVector(flatVector(catalog.EmbeddingDim, 0.5))},
			{"hash": second.Hash, "status": "ready", "vectorB64": encodeVector(flatVector(catalog.EmbeddingDim, 0.25))},
		},
	})
	if err != nil {
		t.Fatalf("marshal results: %v", err)
	}
	rec := harness.do(t, http.MethodPost, "/api/embedding/results", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("results status=%d body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeResults(t, rec)
	if payload.Updated != 2 || len(payload.Rejected) != 0 {
		t.Fatalf("results=%+v want updated=2 rejected=0", payload)
	}

	// The catalog counts flipped to fully ready, including the derived
	// pending, and nothing is leased anymore.
	counts, err := harness.catalog.EmbeddingCounts(context.Background())
	if err != nil {
		t.Fatalf("embedding counts: %v", err)
	}
	if counts.Total != 2 || counts.Ready != 2 || counts.Pending != 0 || counts.Processing != 0 {
		t.Fatalf("counts=%+v want fully ready", counts)
	}

	// The vectors are in the in-memory index: the query photo's best match is
	// the other album's photo, and the item carries its hash for the client's
	// image URLs.
	recItems := harness.do(t, http.MethodGet, "/api/recommendations/"+first.AlbumID+"/0?limit=5", nil)
	if recItems.Code != http.StatusOK {
		t.Fatalf("recommendations status=%d body=%s", recItems.Code, recItems.Body.String())
	}
	var result struct {
		Items []struct {
			Hash string `json:"hash"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recItems.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode recommendations: %v", err)
	}
	if len(result.Items) != 1 || result.Items[0].Hash != second.Hash {
		t.Fatalf("items=%+v want exactly the neighbor %s", result.Items, second.Hash)
	}
}

func TestWorkerResultsRejectsBadItemsPerHash(t *testing.T) {
	harness := newFlowHarness(t)

	// One ready album of four distinct blobs covers the payload rejections
	// (base64, dimension, non-finite values) plus one clean write-back.
	zipData := testZip(t, map[string][]byte{
		"001.png": testPNG(t, 4, 2, 1),
		"002.png": testPNG(t, 4, 2, 2),
		"003.png": testPNG(t, 4, 2, 3),
		"004.png": testPNG(t, 4, 2, 4),
	}, []string{"001.png", "002.png", "003.png", "004.png"})
	albumID := harness.uploadZip(t, "rejects.zip", zipData)
	harness.finalizeAndWait(t, albumID)

	var hashes []string
	for i := 0; i < 4; i++ {
		photo, err := harness.catalog.PhotoAt(context.Background(), albumID, i)
		if err != nil {
			t.Fatalf("photo at %d: %v", i, err)
		}
		hashes = append(hashes, photo.Hash)
	}
	claimed := decodeClaim(t, harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`)))
	if len(claimed.Claimed) != 4 {
		t.Fatalf("claimed=%d want=4", len(claimed.Claimed))
	}

	// A blob that exists but was never claimed - seeded after the claim, or the
	// worker above would have leased it too - and one that does not exist at
	// all: the catalog-side rejections.
	if err := harness.catalog.UpsertBlob(context.Background(), catalog.Blob{Hash: "blob-unclaimed", SizeBytes: 1}); err != nil {
		t.Fatalf("seed unclaimed blob: %v", err)
	}

	valid := encodeVector(flatVector(catalog.EmbeddingDim, 0.25))
	body, err := json.Marshal(map[string]any{
		"results": []map[string]string{
			// Not base64 at all.
			{"hash": hashes[0], "status": "ready", "vectorB64": "!!!not-base64!!!"},
			// Valid base64, but the wrong vector length for the model.
			{"hash": hashes[1], "status": "ready", "vectorB64": encodeVector([]float32{1, 2, 3})},
			// Right length, but a NaN would silently poison the index.
			{"hash": hashes[2], "status": "ready", "vectorB64": encodeVector(nanVector())},
			// The one clean result of the batch.
			{"hash": hashes[3], "status": "ready", "vectorB64": valid},
			// Known hash, but nobody ever claimed it.
			{"hash": "blob-unclaimed", "status": "ready", "vectorB64": valid},
			// Unknown to the catalog entirely.
			{"hash": "blob-missing", "status": "ready", "vectorB64": valid},
			// Only ready/failed are writable states.
			{"hash": hashes[0], "status": "pending", "vectorB64": valid},
		},
	})
	if err != nil {
		t.Fatalf("marshal results: %v", err)
	}
	rec := harness.do(t, http.MethodPost, "/api/embedding/results", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("results status=%d body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeResults(t, rec)
	if payload.Updated != 1 {
		t.Fatalf("updated=%d want=1 body=%s", payload.Updated, rec.Body.String())
	}
	got := rejectionPairs(t, rec)
	want := []string{
		hashes[0] + " bad_base64",
		// Still leased after its bad payload, so a non-terminal status is
		// refused rather than applied.
		hashes[0] + " invalid_status",
		hashes[1] + " wrong_dim",
		hashes[2] + " bad_vector",
		"blob-missing unknown_hash",
		"blob-unclaimed not_claimed",
	}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("got rejections %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rejection[%d]=%q want=%q (all: %v)", i, got[i], want[i], got)
		}
	}

	// Rejections are final, not retryable, but they also change nothing: the
	// refused blobs are still leased, so the only claimable blob left is the
	// seeded one that was never claimed - and the one clean result is the only
	// coverage.
	again := decodeClaim(t, harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`)))
	if len(again.Claimed) != 1 || again.Claimed[0].Hash != "blob-unclaimed" {
		t.Fatalf("re-claim=%+v want only blob-unclaimed", again.Claimed)
	}
}

func nanVector() []float32 {
	vector := flatVector(catalog.EmbeddingDim, 1)
	vector[0] = float32(math.NaN())
	return vector
}

func TestWorkerResultsRejectsUnknownFields(t *testing.T) {
	harness := newFlowHarness(t)

	// The wire contract is strict on purpose: a worker built against a newer
	// shape fails loudly instead of silently reporting nothing.
	rec := harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10,"batchName":"x"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("claim status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"code\":\"INVALID_REQUEST\"") {
		t.Fatalf("expected INVALID_REQUEST in body, got: %s", rec.Body.String())
	}
}

func TestWorkerEndpointsRequireToken(t *testing.T) {
	harness := newFlowHarnessWithToken(t, "sekrit")

	post := func(path string, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		harness.router.ServeHTTP(rec, req)
		return rec
	}

	for _, path := range []string{"/api/embedding/claim", "/api/embedding/renew", "/api/embedding/results"} {
		if rec := post(path, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s without token: status=%d want=401", path, rec.Code)
		}
		if rec := post(path, "Bearer wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s with wrong token: status=%d want=401", path, rec.Code)
		}
		rec := post(path, "Bearer sekrit")
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s with the right token must pass auth, got: %s", path, rec.Body.String())
		}
	}

	// The public read surface stays open: only the worker write path is
	// token-gated.
	if rec := harness.do(t, http.MethodGet, "/metrics", nil); rec.Code != http.StatusOK {
		t.Fatalf("public metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerRenewExtendsLease(t *testing.T) {
	harness := newFlowHarness(t)
	photo := uploadOnePhotoAlbum(t, harness, "renew.zip", 50)

	claimed := decodeClaim(t, harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`)))
	if len(claimed.Claimed) != 1 {
		t.Fatalf("claimed=%d want=1", len(claimed.Claimed))
	}

	body, err := json.Marshal(map[string]any{"hashes": []string{photo.Hash}})
	if err != nil {
		t.Fatalf("marshal renew: %v", err)
	}
	rec := harness.do(t, http.MethodPost, "/api/embedding/renew", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		LeaseUntil time.Time `json:"leaseUntil"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode renew response: %v body=%s", err, rec.Body.String())
	}
	// The lease was pushed forward from the claim's deadline, so the blob stays
	// out of the pending queue for another TTL.
	if payload.LeaseUntil.Before(claimed.LeaseUntil) {
		t.Fatalf("renewed lease %v must not precede the claim's %v", payload.LeaseUntil, claimed.LeaseUntil)
	}
	if again := decodeClaim(t, harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`))); len(again.Claimed) != 0 {
		t.Fatalf("a renewed claim must not be re-claimable, got %d blobs", len(again.Claimed))
	}
}

func TestWorkerEndpointsWithoutServices(t *testing.T) {
	router := New(nil, nil, nil, nil, "").Router()

	for _, path := range []string{"/api/embedding/claim", "/api/embedding/renew", "/api/embedding/results"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status=%d want=503 body=%s", path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "\"code\":\"UNAVAILABLE\"") {
			t.Fatalf("%s expected UNAVAILABLE in body, got: %s", path, rec.Body.String())
		}
	}
}

func TestWorkerResultsCapRejectsWholeRequest(t *testing.T) {
	harness := newFlowHarness(t)

	results := make([]map[string]string, recommend.MaxClaimLimit+1)
	for i := range results {
		results[i] = map[string]string{"hash": "hash", "status": "failed", "error": "x"}
	}
	body, err := json.Marshal(map[string]any{"results": results})
	if err != nil {
		t.Fatalf("marshal oversized batch: %v", err)
	}
	rec := harness.do(t, http.MethodPost, "/api/embedding/results", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized batch status=%d want=400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "maximum 1024") {
		t.Fatalf("expected the limit in the message, got: %s", rec.Body.String())
	}
}

func TestWorkerResultsRejectsWhitespaceHash(t *testing.T) {
	harness := newFlowHarness(t)

	// A hash of pure whitespace cannot name a blob, and it must come back as a
	// per-item rejection carrying what the worker sent, not vanish between
	// updated and rejected.
	body, err := json.Marshal(map[string]any{
		"results": []map[string]string{{"hash": "   ", "status": "ready", "vectorB64": encodeVector(flatVector(catalog.EmbeddingDim, 1))}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := harness.do(t, http.MethodPost, "/api/embedding/results", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("results status=%d body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeResults(t, rec)
	if payload.Updated != 0 || len(payload.Rejected) != 1 || payload.Rejected[0].Reason != "invalid_hash" {
		t.Fatalf("payload=%+v want one invalid_hash rejection", payload)
	}
}

func TestWorkerRenewReportsWhatRenewed(t *testing.T) {
	harness := newFlowHarness(t)
	photo := uploadOnePhotoAlbum(t, harness, "renew-report.zip", 60)

	claimed := decodeClaim(t, harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`)))
	if len(claimed.Claimed) != 1 {
		t.Fatalf("claimed=%d want=1", len(claimed.Claimed))
	}

	body, err := json.Marshal(map[string]any{"hashes": []string{photo.Hash, "ghost"}})
	if err != nil {
		t.Fatalf("marshal renew: %v", err)
	}
	rec := harness.do(t, http.MethodPost, "/api/embedding/renew", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		LeaseUntil time.Time `json:"leaseUntil"`
		Renewed    []string  `json:"renewed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode renew: %v", err)
	}
	if len(payload.Renewed) != 1 || payload.Renewed[0] != photo.Hash {
		t.Fatalf("renewed=%v want only the claimed hash: a lost lease must be visible", payload.Renewed)
	}
}

func TestWorkerClaimReleasesBatchWhenPresignFails(t *testing.T) {
	harness := newFlowHarness(t)
	uploadOnePhotoAlbum(t, harness, "presign-fail.zip", 70)

	harness.s3.mu.Lock()
	harness.s3.failPresignGet = true
	harness.s3.mu.Unlock()
	rec := harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("claim status=%d want=500 body=%s", rec.Code, rec.Body.String())
	}

	// The claimed batch was handed back, so a worker can claim again as soon
	// as signing works instead of waiting out the whole lease.
	harness.s3.mu.Lock()
	harness.s3.failPresignGet = false
	harness.s3.mu.Unlock()
	again := decodeClaim(t, harness.do(t, http.MethodPost, "/api/embedding/claim", []byte(`{"limit":10}`)))
	if len(again.Claimed) != 1 {
		t.Fatalf("after presign failure claimed=%d want=1 (batch must be released)", len(again.Claimed))
	}
}

func TestWorkerTokenSchemeIsCaseInsensitive(t *testing.T) {
	harness := newFlowHarnessWithToken(t, "sekrit")

	req := httptest.NewRequest(http.MethodPost, "/api/embedding/claim", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer sekrit")
	rec := httptest.NewRecorder()
	harness.router.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("RFC 9110 schemes are case-insensitive, got 401: %s", rec.Body.String())
	}
	if rec.Code == http.StatusBadRequest {
		return // auth passed; the empty-claim body is rejected downstream
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", rec.Code, rec.Body.String())
	}
}
