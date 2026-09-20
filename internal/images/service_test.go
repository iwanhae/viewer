package images

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"viewer/internal/albums"
	"viewer/internal/catalog"
	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

type fakeBlobStore struct {
	objects map[string][]byte
	gets    []string
}

func newFakeBlobStore() *fakeBlobStore {
	return &fakeBlobStore{objects: make(map[string][]byte)}
}

func (f *fakeBlobStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	f.gets = append(f.gets, key)
	data, ok := f.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), "image/png", nil
}

func (f *fakeBlobStore) getCount() int {
	return len(f.gets)
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

func TestGetImageReturnsContentAddressedBlob(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("image-bytes")
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")

	svc := newTestService(t, cat, store)
	result, err := svc.GetImage(context.Background(), "album-a", 0)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}
	if string(result.Bytes) != "image-bytes" {
		t.Fatalf("bytes=%q", result.Bytes)
	}
	if result.ContentType != "image/png" {
		t.Fatalf("content type=%q", result.ContentType)
	}
	if got := store.getCount(); got != 1 {
		t.Fatalf("expected one S3 get, got %d", got)
	}

	// A second read of the same content hash must be served from the disk cache.
	if _, err := svc.GetImage(context.Background(), "album-a", 0); err != nil {
		t.Fatalf("second get image: %v", err)
	}
	if got := store.getCount(); got != 1 {
		t.Fatalf("expected cache hit, s3 gets=%d", got)
	}
}

func TestGetImageIndexOutOfRange(t *testing.T) {
	cat := openTestCatalog(t)
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")
	svc := newTestService(t, cat, newFakeBlobStore())

	if _, err := svc.GetImage(context.Background(), "album-a", 5); !errors.Is(err, ErrPhotoIndexOutOfRange) {
		t.Fatalf("expected ErrPhotoIndexOutOfRange, got %v", err)
	}
	if _, err := svc.GetImage(context.Background(), "album-a", -1); !errors.Is(err, ErrPhotoIndexOutOfRange) {
		t.Fatalf("expected ErrPhotoIndexOutOfRange for negative index, got %v", err)
	}
}

func TestGetImageUnknownAlbum(t *testing.T) {
	cat := openTestCatalog(t)
	svc := newTestService(t, cat, newFakeBlobStore())

	if _, err := svc.GetImage(context.Background(), "missing", 0); !errors.Is(err, albums.ErrAlbumNotFound) {
		t.Fatalf("expected ErrAlbumNotFound, got %v", err)
	}
	if _, err := svc.GetImage(context.Background(), "  ", 0); err == nil {
		t.Fatalf("expected error for empty album id")
	}
}

func TestGetImageMissingBlobInStorage(t *testing.T) {
	cat := openTestCatalog(t)
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")
	svc := newTestService(t, cat, newFakeBlobStore())

	if _, err := svc.GetImage(context.Background(), "album-a", 0); !errors.Is(err, ErrImageEntryNotFound) {
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

func TestGetImageFallsBackToRemoteContentType(t *testing.T) {
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
	result, err := svc.GetImage(ctx, "album-a", 0)
	if err != nil {
		t.Fatalf("get image: %v", err)
	}
	if result.ContentType != "image/png" {
		t.Fatalf("content type=%q want=image/png", result.ContentType)
	}
}
