package pipeline

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"viewer/internal/backup"
	"viewer/internal/catalog"
	cfgpkg "viewer/internal/config"
	"viewer/internal/storage"
)

// s3DeleteRequest is the <Delete> body of a batch-delete call.
type s3DeleteRequest struct {
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

// s3Stub is a minimal path-style object store: enough of the S3 API for the
// pipeline to stage, download and delete a zip and to write image blobs. It
// exists so the key-prefix test can assert on the keys that really reach the
// wire, rather than on the fake store the other pipeline tests use.
type s3Stub struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
}

func (s *s3Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"), s.bucket+"/")

	s.mu.Lock()
	defer s.mu.Unlock()

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.objects[key] = body
		w.Header().Set("ETag", `"stub"`)
	case http.MethodPost:
		// The only POST the store makes is a batch delete on the bucket root.
		body, err := io.ReadAll(r.Body)
		var batch s3DeleteRequest
		if err != nil || xml.Unmarshal(body, &batch) != nil {
			http.Error(w, "bad delete body", http.StatusBadRequest)
			return
		}
		for _, obj := range batch.Objects {
			delete(s.objects, obj.Key)
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`)
	case http.MethodGet:
		data, ok := s.objects[key]
		if !ok {
			stubNotFound(w)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	case http.MethodHead:
		data, ok := s.objects[key]
		if !ok {
			stubNotFound(w)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	case http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported method", http.StatusNotImplemented)
	}
}

func stubNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>not found</Message></Error>`)
}

func (s *s3Stub) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *s3Stub) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

// TestPipelineStoresEveryObjectUnderKeyPrefix runs the real pipeline through
// the real S3 store against a stub bucket, from extraction through the drain
// finalize. The callers only ever name logical keys, so this pins the whole
// point of S3_PREFIX: every object the deployment writes — the catalog backup
// included — and every object it deletes lives under the prefix.
func TestPipelineStoresEveryObjectUnderKeyPrefix(t *testing.T) {
	const bucket = "test-bucket"

	stub := &s3Stub{bucket: bucket, objects: make(map[string][]byte)}
	server := httptest.NewServer(stub)
	defer server.Close()

	ctx := context.Background()
	store, err := storage.NewS3Store(ctx, cfgpkg.Config{
		S3Endpoint:     server.URL,
		S3Bucket:       bucket,
		S3AccessKey:    "access",
		S3SecretKey:    "secret",
		S3Prefix:       "photos/",
		S3UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}

	cat := openTestCatalog(t)
	imageA := pngBytes(t, 4, 2, 10)
	imageB := pngBytes(t, 2, 6, 200)
	zipData := buildZip(t,
		zipEntry{name: "a.png", data: imageA},
		zipEntry{name: "b.png", data: imageB},
	)

	sourceKey := SourceKey("album-a")
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID:               "album-a",
		OriginalFilename: "holiday.zip",
		SizeBytes:        int64(len(zipData)),
		Status:           catalog.AlbumStatusQueued,
		SourceKey:        sourceKey,
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := store.PutObject(ctx, sourceKey, bytes.NewReader(zipData), "application/zip"); err != nil {
		t.Fatalf("stage zip: %v", err)
	}
	if !stub.has("photos/" + sourceKey) {
		t.Fatalf("staged zip keys=%v want photos/%s", stub.keys(), sourceKey)
	}

	// Drive the real wiring: the worker drains, OnIdle runs the finalizer, and
	// only after the backup is durable is the staged zip deleted.
	finalizer := backup.NewFinalizer(store, cat, t.TempDir())
	svc := NewService(cat, store, Options{
		TempDir: t.TempDir(),
		OnIdle:  finalizer.Run,
	})
	svc.Start(ctx)
	if err := svc.Enqueue("album-a"); err != nil {
		t.Fatalf("enqueue album: %v", err)
	}

	// The batch delete is the finalize's last step, so waiting for the zip to
	// go means everything before it — extraction, backup upload — has landed.
	deadline := time.Now().Add(2 * time.Second)
	for stub.has("photos/" + sourceKey) {
		if time.Now().After(deadline) {
			t.Fatalf("finalize never deleted the staged zip, keys=%v", stub.keys())
		}
		time.Sleep(5 * time.Millisecond)
	}

	for _, key := range []string{
		"photos/blobs/" + hashOf(imageA),
		"photos/blobs/" + hashOf(imageB),
		"photos/" + backup.BackupObjectKey,
	} {
		if !stub.has(key) {
			t.Errorf("keys=%v missing %s", stub.keys(), key)
		}
	}
	for _, key := range stub.keys() {
		if !strings.HasPrefix(key, "photos/") {
			t.Errorf("object %q was stored outside the key prefix, keys=%v", key, stub.keys())
		}
	}

	// The catalog keeps the logical key, so changing S3_PREFIX later is a
	// bucket-side move rather than a metadata migration.
	album, err := cat.GetAlbum(ctx, "album-a")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.SourceKey != sourceKey {
		t.Errorf("catalog source_key=%q want=%q", album.SourceKey, sourceKey)
	}
	if album.PhotoCount != 2 {
		t.Errorf("photo count=%d want=2", album.PhotoCount)
	}
}
