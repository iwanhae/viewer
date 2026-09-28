package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/encoding"
	"viewer/internal/storage"
)

type claimOnlyStore struct {
	presignPutCalls *int
}

func (claimOnlyStore) GetObject(context.Context, string) (io.ReadCloser, string, error) {
	return nil, "", storage.ErrObjectNotFound
}
func (claimOnlyStore) PutObjectIfMatch(_ context.Context, _ string, body io.Reader, _, _ string) error {
	_, err := io.Copy(io.Discard, body)
	return err
}
func (claimOnlyStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memory://" + key, nil
}
func (store claimOnlyStore) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	if store.presignPutCalls != nil {
		(*store.presignPutCalls)++
	}
	return "memory://" + key, nil
}
func (claimOnlyStore) StatObject(context.Context, string) (storage.Object, bool, error) {
	return storage.Object{}, false, nil
}
func (claimOnlyStore) CopyObjectIfMatch(context.Context, string, string, string, string) error {
	return nil
}
func (claimOnlyStore) DeleteObjects(context.Context, []string) error                 { return nil }
func (claimOnlyStore) ListObjects(context.Context, string) ([]storage.Object, error) { return nil, nil }

const testEncodingWebP = "UklGRkAAAABXRUJQVlA4IDQAAADwAQCdASoBAAEAAQAcJaACdLoB+AAETAAA/vW4f/6aR40jxpHxcP/ugT90CfugT/3NoAAA"

func encodingTestSourcePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	payload := append([]byte("Comment\x00"), bytes.Repeat([]byte{'a'}, 200)...)
	chunk := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(payload)))
	copy(chunk[4:8], "tEXt")
	copy(chunk[8:], payload)
	binary.BigEndian.PutUint32(chunk[len(chunk)-4:], crc32.ChecksumIEEE(chunk[4:len(chunk)-4]))
	return append(append(bytes.Clone(data[:len(data)-12]), chunk...), data[len(data)-12:]...)
}

type encodingOutputStore struct {
	claimOnlyStore
	mu          sync.Mutex
	objects     map[string][]byte
	contentType map[string]string
}

func newEncodingOutputStore() *encodingOutputStore {
	return &encodingOutputStore{
		objects:     make(map[string][]byte),
		contentType: make(map[string]string),
	}
}

func (store *encodingOutputStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	data, ok := store.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), store.contentType[key], nil
}

func (store *encodingOutputStore) PutObjectIfMatch(ctx context.Context, key string, body io.Reader, contentType, etag string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current, exists := store.objects[key]
	sum := sha256.Sum256(current)
	if !exists || hex.EncodeToString(sum[:]) != etag {
		return encoding.ErrLostLease
	}
	store.objects[key] = data
	store.contentType[key] = contentType
	return nil
}

func (store *encodingOutputStore) StatObject(_ context.Context, key string) (storage.Object, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	data, ok := store.objects[key]
	if !ok {
		return storage.Object{}, false, nil
	}
	sum := sha256.Sum256(data)
	return storage.Object{Key: key, Size: int64(len(data)), ETag: hex.EncodeToString(sum[:]), LastModified: time.Now()}, true, nil
}

type testEncodingOutputReceiver struct {
	hash  string
	token string
	body  []byte
	err   error
	calls int
}

func (receiver *testEncodingOutputReceiver) ReceiveOutput(_ context.Context, hash, token string, body io.Reader) error {
	receiver.calls++
	receiver.hash = hash
	receiver.token = token
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	receiver.body = data
	return receiver.err
}

func TestEncodingWorkerRoutesUseSharedToken(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.UpsertBlob(context.Background(), catalog.Blob{Hash: "source-hash", SizeBytes: 100, ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	router := New(nil, nil, nil, nil, "shared-token", nil, "").WithEncoder(encoding.New(cat, claimOnlyStore{}, t.TempDir())).Router()
	request := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/encoding/claim", bytes.NewBufferString(`{"limit":1}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := request(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without token: %d", rec.Code)
	}
	rec := request("shared-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("with token: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Claimed []encoding.Claimed `json:"claimed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Claimed) != 1 || payload.Claimed[0].Hash != "source-hash" || payload.Claimed[0].Token == "" || payload.Claimed[0].GetURL == "" || payload.Claimed[0].PutURL == "" {
		t.Fatalf("claim payload: %+v", payload)
	}
	if want := "memory://encoding/source-hash_" + payload.Claimed[0].Token + ".webp"; payload.Claimed[0].PutURL != want {
		t.Fatalf("put URL=%q want=%q", payload.Claimed[0].PutURL, want)
	}
	if rec := request("shared-token"); rec.Code != http.StatusOK {
		t.Fatalf("second claim: %d", rec.Code)
	} else {
		var next struct {
			Claimed []encoding.Claimed `json:"claimed"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &next); err != nil || len(next.Claimed) != 0 {
			t.Fatalf("duplicate lease: %+v %v", next, err)
		}
	}
}

func TestAPIOutputModeDoesNotPresignResultUploads(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.UpsertBlob(context.Background(), catalog.Blob{Hash: "source-hash", SizeBytes: 100, ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	putPresigns := 0
	service := encoding.New(cat, claimOnlyStore{presignPutCalls: &putPresigns}, t.TempDir())
	router := New(nil, nil, nil, nil, "shared-token", nil, "").WithEncoder(service).Router()
	post := func(path, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer shared-token")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	claim := post("/api/encoding/claim", `{"limit":1,"outputMode":"api"}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim API output mode: status=%d body=%s", claim.Code, claim.Body.String())
	}
	var claimBody struct {
		Claimed []encoding.Claimed `json:"claimed"`
	}
	if err := json.Unmarshal(claim.Body.Bytes(), &claimBody); err != nil {
		t.Fatal(err)
	}
	if len(claimBody.Claimed) != 1 || claimBody.Claimed[0].GetURL == "" || claimBody.Claimed[0].PutURL != "" {
		t.Fatalf("API-mode claim unexpectedly returned a PUT URL: %+v", claimBody.Claimed)
	}
	if putPresigns != 0 {
		t.Fatalf("API-mode claim generated %d presigned PUTs", putPresigns)
	}
	job := claimBody.Claimed[0]
	renew := post("/api/encoding/renew", `{"hash":"`+job.Hash+`","token":"`+job.Token+`","outputMode":"api"}`)
	if renew.Code != http.StatusOK {
		t.Fatalf("renew API output mode: status=%d body=%s", renew.Code, renew.Body.String())
	}
	var renewBody map[string]any
	if err := json.Unmarshal(renew.Body.Bytes(), &renewBody); err != nil {
		t.Fatal(err)
	}
	if _, hasPutURL := renewBody["putUrl"]; hasPutURL {
		t.Fatalf("API-mode renew returned legacy putUrl: %v", renewBody)
	}
	if putPresigns != 0 {
		t.Fatalf("API-mode renew generated %d presigned PUTs", putPresigns)
	}
}

func TestEncodingOutputRouteUsesWorkerAuthAndReceivesRawBody(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	api := New(nil, nil, nil, nil, "shared-token", nil, "").WithEncoder(encoding.New(cat, claimOnlyStore{}, t.TempDir()))
	receiver := &testEncodingOutputReceiver{}
	api.encodingOutput = receiver
	router := api.Router()

	request := func(auth string) *httptest.ResponseRecorder {
		body := []byte{0x00, 0xff, 'R', 'I', 'F', 'F', 0x80}
		req := httptest.NewRequest(http.MethodPost, "/api/encoding/output", bytes.NewReader(body))
		req.Header.Set("X-Encoding-Hash", "source-hash")
		req.Header.Set("X-Encoding-Token", "lease-secret")
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	if rec := request(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without worker token: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if receiver.calls != 0 {
		t.Fatalf("unauthorized request reached receiver %d times", receiver.calls)
	}

	rec := request("shared-token")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("receive output: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Status != "received" {
		t.Fatalf("response status=%q, want received", payload.Status)
	}
	if receiver.calls != 1 || receiver.hash != "source-hash" || receiver.token != "lease-secret" {
		t.Fatalf("receiver call: calls=%d hash=%q token=%q", receiver.calls, receiver.hash, receiver.token)
	}
	if want := []byte{0x00, 0xff, 'R', 'I', 'F', 'F', 0x80}; !bytes.Equal(receiver.body, want) {
		t.Fatalf("receiver body=%v, want %v", receiver.body, want)
	}
}

func TestEncodingOutputRouteAcceptsThroughEncodingService(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	source := encodingTestSourcePNG(t)
	hashBytes := sha256.Sum256(source)
	hash := hex.EncodeToString(hashBytes[:])
	store := newEncodingOutputStore()
	store.objects["blobs/"+hash] = source
	store.contentType["blobs/"+hash] = "image/png"

	service := encoding.New(cat, store, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png", EncodingGate: true}); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.Claim(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim encoding job: claimed=%+v err=%v", claimed, err)
	}

	router := New(nil, nil, nil, nil, "shared-token", nil, "").WithEncoder(service).Router()
	webp, err := base64.StdEncoding.DecodeString(testEncodingWebP)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/encoding/output", bytes.NewReader(webp))
	req.Header.Set("Authorization", "Bearer shared-token")
	req.Header.Set("X-Encoding-Hash", claimed[0].Hash)
	req.Header.Set("X-Encoding-Token", claimed[0].Token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("real service receive: status=%d body=%s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := cat.GetEncodingJob(context.Background(), hash)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("encoding output was not processed: status=%s", job.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	store.mu.Lock()
	got := bytes.Clone(store.objects["blobs/"+hash])
	store.mu.Unlock()
	if !bytes.Equal(got, webp) {
		t.Fatalf("service stored output %d bytes, want %d", len(got), len(webp))
	}
}

func TestEncodingOutputRouteValidatesHeaders(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	api := New(nil, nil, nil, nil, "shared-token", nil, "").WithEncoder(encoding.New(cat, claimOnlyStore{}, t.TempDir()))
	receiver := &testEncodingOutputReceiver{}
	api.encodingOutput = receiver
	req := httptest.NewRequest(http.MethodPost, "/api/encoding/output", bytes.NewReader([]byte("body")))
	req.Header.Set("Authorization", "Bearer shared-token")
	req.Header.Set("X-Encoding-Hash", " ")
	req.Header.Set("X-Encoding-Token", "lease-secret")
	rec := httptest.NewRecorder()
	api.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing hash: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if receiver.calls != 0 {
		t.Fatalf("invalid request reached receiver %d times", receiver.calls)
	}
}

func TestEncodingOutputRouteMapsServiceErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		want     int
		wantCode string
	}{
		{name: "lost lease", err: encoding.ErrLostLease, want: http.StatusConflict, wantCode: "LOST_LEASE"},
		{name: "invalid output", err: encoding.ErrInvalidOutput, want: http.StatusUnprocessableEntity, wantCode: "INVALID_OUTPUT"},
		{name: "output too large", err: encoding.ErrOutputTooLarge, want: http.StatusRequestEntityTooLarge, wantCode: "OUTPUT_TOO_LARGE"},
		{name: "spool busy", err: encoding.ErrSpoolBusy, want: http.StatusTooManyRequests, wantCode: "SPOOL_BUSY"},
		{name: "internal", err: errors.New("store unavailable"), want: http.StatusInternalServerError, wantCode: "INTERNAL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			api := New(nil, nil, nil, nil, "shared-token", nil, "").WithEncoder(encoding.New(cat, claimOnlyStore{}, t.TempDir()))
			api.encodingOutput = &testEncodingOutputReceiver{err: test.err}
			req := httptest.NewRequest(http.MethodPost, "/api/encoding/output", bytes.NewReader([]byte("raw output")))
			req.Header.Set("Authorization", "Bearer shared-token")
			req.Header.Set("X-Encoding-Hash", "source-hash")
			req.Header.Set("X-Encoding-Token", "lease-secret")
			rec := httptest.NewRecorder()
			api.Router().ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status=%d, want=%d body=%s", rec.Code, test.want, rec.Body.String())
			}
			var payload struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Code != test.wantCode {
				t.Fatalf("error code=%q, want=%q", payload.Error.Code, test.wantCode)
			}
		})
	}
}
