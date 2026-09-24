package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/encoding"
	"viewer/internal/storage"
)

type claimOnlyStore struct{}

func (claimOnlyStore) GetObject(context.Context, string) (io.ReadCloser, string, error) {
	return nil, "", storage.ErrObjectNotFound
}
func (claimOnlyStore) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memory://" + key, nil
}
func (claimOnlyStore) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
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

func TestEncodingWorkerRoutesUseSharedToken(t *testing.T) {
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.UpsertBlob(context.Background(), catalog.Blob{Hash: "source-hash", SizeBytes: 100, ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	router := New(nil, nil, nil, nil, "shared-token", nil, "").WithEncoder(encoding.New(cat, claimOnlyStore{})).Router()
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
