package images

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"viewer/internal/albums"
	"viewer/internal/catalog"
	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

type fakeBlobStore struct {
	objects map[string][]byte
	readErr map[string]error
	gets    []string
}

func newFakeBlobStore() *fakeBlobStore {
	return &fakeBlobStore{objects: make(map[string][]byte), readErr: make(map[string]error)}
}

func (f *fakeBlobStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	f.gets = append(f.gets, key)
	data, ok := f.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	if err := f.readErr[key]; err != nil {
		return io.NopCloser(&failingReader{data: data, err: err}), "image/png", nil
	}
	return io.NopCloser(bytes.NewReader(data)), "image/png", nil
}

func (f *fakeBlobStore) getCount() int {
	return len(f.gets)
}

// failingReader yields its data and then fails, simulating a truncated S3
// response mid-stream.
type failingReader struct {
	data []byte
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func openTestCatalog(t *testing.T) *catalog.Store {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return cat
}

func seedPhoto(t *testing.T, cat *catalog.Store, albumID string, index int, hash string, contentType string) {
	t.Helper()
	ctx := context.Background()
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID:               albumID,
		OriginalFilename: albumID + ".zip",
		Status:           catalog.AlbumStatusReady,
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: 4, ContentType: contentType}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := cat.InsertPhoto(ctx, catalog.Photo{
		AlbumID: albumID, Index: index, Name: "photo.png", Hash: hash, Width: 2, Height: 2, Ratio: 1,
	}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}
}

func newTestService(t *testing.T, cat *catalog.Store, store blobStore) *Service {
	t.Helper()
	svc, err := NewService(cat, store, t.TempDir())
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func readStream(t *testing.T, stream *ImageStream) []byte {
	t.Helper()
	data, err := io.ReadAll(stream.Content)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return data
}

func TestOpenImageMaterialisesBlobInDiskCache(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("image-bytes")
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")

	svc := newTestService(t, cat, store)
	stream, err := svc.OpenImage(context.Background(), "album-a", 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	if got := readStream(t, stream); string(got) != "image-bytes" {
		t.Fatalf("bytes=%q", got)
	}
	if stream.ContentType != "image/png" {
		t.Fatalf("content type=%q", stream.ContentType)
	}
	if stream.SizeBytes != int64(len("image-bytes")) {
		t.Fatalf("size=%d", stream.SizeBytes)
	}
	if stream.Hash != "hash-a" {
		t.Fatalf("hash=%q", stream.Hash)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close stream: %v", err)
	}
	if got := store.getCount(); got != 1 {
		t.Fatalf("expected one S3 get, got %d", got)
	}

	// A second open of the same content hash is served from the disk cache
	// without touching the store.
	second, err := svc.OpenImage(context.Background(), "album-a", 0)
	if err != nil {
		t.Fatalf("second open image: %v", err)
	}
	if got := readStream(t, second); string(got) != "image-bytes" {
		t.Fatalf("second bytes=%q", got)
	}
	if got := store.getCount(); got != 1 {
		t.Fatalf("expected cache hit, s3 gets=%d", got)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second stream: %v", err)
	}
}

func TestOpenImageStreamSupportsSeeking(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("0123456789")
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")

	svc := newTestService(t, cat, store)
	stream, err := svc.OpenImage(context.Background(), "album-a", 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Content.Seek(2, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	rest, err := io.ReadAll(stream.Content)
	if err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if string(rest) != "23456789" {
		t.Fatalf("bytes after seek=%q", rest)
	}
}

func TestGetImageByHashReturnsBytes(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("image-bytes")
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")

	svc := newTestService(t, cat, store)
	result, err := svc.GetImageByHash(context.Background(), "hash-a")
	if err != nil {
		t.Fatalf("get image by hash: %v", err)
	}
	if string(result.Bytes) != "image-bytes" {
		t.Fatalf("bytes=%q", result.Bytes)
	}
	if result.ContentType != "image/png" {
		t.Fatalf("content type=%q", result.ContentType)
	}
}

func TestOpenImageIndexOutOfRange(t *testing.T) {
	cat := openTestCatalog(t)
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")
	svc := newTestService(t, cat, newFakeBlobStore())

	if _, err := svc.OpenImage(context.Background(), "album-a", 5); !errors.Is(err, ErrPhotoIndexOutOfRange) {
		t.Fatalf("expected ErrPhotoIndexOutOfRange, got %v", err)
	}
	if _, err := svc.OpenImage(context.Background(), "album-a", -1); !errors.Is(err, ErrPhotoIndexOutOfRange) {
		t.Fatalf("expected ErrPhotoIndexOutOfRange for negative index, got %v", err)
	}
}

func TestOpenImageUnknownAlbum(t *testing.T) {
	cat := openTestCatalog(t)
	svc := newTestService(t, cat, newFakeBlobStore())

	if _, err := svc.OpenImage(context.Background(), "missing", 0); !errors.Is(err, albums.ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
	if _, err := svc.OpenImage(context.Background(), "  ", 0); err == nil {
		t.Fatalf("expected error for empty album id")
	}
}

func TestOpenImageMissingBlobInStorage(t *testing.T) {
	cat := openTestCatalog(t)
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")
	svc := newTestService(t, cat, newFakeBlobStore())

	if _, err := svc.OpenImage(context.Background(), "album-a", 0); !errors.Is(err, ErrImageEntryNotFound) {
		t.Fatalf("expected ErrImageEntryNotFound, got %v", err)
	}
}

func TestGetImageByHashUnknownBlob(t *testing.T) {
	cat := openTestCatalog(t)
	svc := newTestService(t, cat, newFakeBlobStore())

	if _, err := svc.GetImageByHash(context.Background(), "nope"); !errors.Is(err, ErrImageEntryNotFound) {
		t.Fatalf("expected ErrImageEntryNotFound, got %v", err)
	}
	if _, err := svc.GetImageByHash(context.Background(), "  "); err == nil {
		t.Fatalf("expected error for empty hash")
	}
}

func TestOpenImageFallsBackToRemoteContentType(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("bytes")
	ctx := context.Background()
	if err := cat.CreateAlbum(ctx, catalog.Album{ID: "album-a", Status: catalog.AlbumStatusReady}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	// Blob row without a content type: the S3 response value is used.
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: "hash-a", SizeBytes: 5}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}
	if err := cat.InsertPhoto(ctx, catalog.Photo{AlbumID: "album-a", Index: 0, Name: "p.png", Hash: "hash-a", Width: 1, Height: 1, Ratio: 1}); err != nil {
		t.Fatalf("insert photo: %v", err)
	}

	svc := newTestService(t, cat, store)
	stream, err := svc.OpenImage(ctx, "album-a", 0)
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer stream.Close()
	if stream.ContentType != "image/png" {
		t.Fatalf("content type=%q want=image/png", stream.ContentType)
	}
}

func TestOpenImageFetchFailureLeavesNoCacheEntry(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("image-bytes")
	store.readErr[pipeline.BlobKey("hash-a")] = errors.New("connection reset")
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")

	cacheDir := t.TempDir()
	svc, err := NewService(cat, store, cacheDir)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	if _, err := svc.OpenImage(context.Background(), "album-a", 0); err == nil {
		t.Fatalf("expected streaming failure")
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no cache entry after a failed fetch, found %d", len(entries))
	}

	// The failed attempt must not be cached: the next open fetches again.
	if _, err := svc.OpenImage(context.Background(), "album-a", 0); err == nil {
		t.Fatalf("expected streaming failure on retry")
	}
	if got := store.getCount(); got != 2 {
		t.Fatalf("expected two S3 gets, got %d", got)
	}
}
