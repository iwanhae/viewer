package images

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"path/filepath"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/pipeline"
	"viewer/internal/storage"
)

type fakeBlobStore struct {
	objects map[string][]byte
	// contentTypes overrides the advertised content type per key; the default
	// is image/png, which is what extraction uploads carry.
	contentTypes map[string]string
	readErr      map[string]error
	gets         []string
	presigned    []string
}

func newFakeBlobStore() *fakeBlobStore {
	return &fakeBlobStore{
		objects:      make(map[string][]byte),
		contentTypes: make(map[string]string),
		readErr:      make(map[string]error),
	}
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
	contentType := f.contentTypes[key]
	if contentType == "" {
		contentType = "image/png"
	}
	return io.NopCloser(bytes.NewReader(data)), contentType, nil
}

func (f *fakeBlobStore) PresignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	f.presigned = append(f.presigned, key)
	return "memory://" + key + "?ttl=" + ttl.String(), nil
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
	return NewService(cat, store)
}

func readStream(t *testing.T, stream *ImageStream) []byte {
	t.Helper()
	data, err := io.ReadAll(stream.Content)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	return data
}

func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestOpenImageByHashFetchesFromStorage(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("image-bytes")
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")

	svc := newTestService(t, cat, store)
	stream, err := svc.OpenImageByHash(context.Background(), "hash-a")
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

	// There is no server-side cache: a second open of the same content hash
	// fetches from the store again. Repeat views are absorbed by the browser
	// via the blob-hash ETag instead.
	second, err := svc.OpenImageByHash(context.Background(), "hash-a")
	if err != nil {
		t.Fatalf("second open image: %v", err)
	}
	if got := readStream(t, second); string(got) != "image-bytes" {
		t.Fatalf("second bytes=%q", got)
	}
	if got := store.getCount(); got != 2 {
		t.Fatalf("expected a second S3 get, got %d", got)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second stream: %v", err)
	}
}

func TestOpenImageByHashSkipsCatalogWhenS3HasType(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("image-bytes")
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")

	svc := newTestService(t, cat, store)
	stream, err := svc.OpenImageByHash(context.Background(), "hash-a")
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer stream.Close()

	// The S3 response carries a real type, so the request makes no catalog
	// round trip at all.
	if stream.ContentType != "image/png" {
		t.Fatalf("content type=%q", stream.ContentType)
	}
	if got := store.getCount(); got != 1 {
		t.Fatalf("expected exactly one store call, got %d", got)
	}
}

func TestOpenImageByHashSupportsSeeking(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("0123456789")

	svc := newTestService(t, cat, store)
	stream, err := svc.OpenImageByHash(context.Background(), "hash-a")
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

func TestGetImageBytesReturnsBytes(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("image-bytes")
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/png")

	svc := newTestService(t, cat, store)
	data, err := svc.GetImageBytes(context.Background(), "hash-a")
	if err != nil {
		t.Fatalf("get image bytes: %v", err)
	}
	if string(data) != "image-bytes" {
		t.Fatalf("bytes=%q", data)
	}
}

func TestOpenImageByHashMissingBlob(t *testing.T) {
	cat := openTestCatalog(t)
	svc := newTestService(t, cat, newFakeBlobStore())

	if _, err := svc.OpenImageByHash(context.Background(), "nope"); !errors.Is(err, ErrImageEntryNotFound) {
		t.Fatalf("expected ErrImageEntryNotFound, got %v", err)
	}
	if _, err := svc.OpenImageByHash(context.Background(), "  "); err == nil {
		t.Fatalf("expected error for empty hash")
	}
	if _, err := svc.GetImageBytes(context.Background(), "nope"); !errors.Is(err, ErrImageEntryNotFound) {
		t.Fatalf("expected ErrImageEntryNotFound, got %v", err)
	}
}

// The catalog is only a content-type fallback: it is consulted when S3
// advertises nothing useful, and an unknown catalog row must not block a
// servable blob.
func TestFetchBlobCatalogContentTypeFallback(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("bytes")
	store.contentTypes[pipeline.BlobKey("hash-a")] = "application/octet-stream"
	seedPhoto(t, cat, "album-a", 0, "hash-a", "image/webp")
	seedPhoto(t, cat, "album-b", 0, "hash-b", "image/webp")
	store.objects[pipeline.BlobKey("hash-b")] = []byte("bytes")
	store.contentTypes[pipeline.BlobKey("hash-b")] = "application/octet-stream"

	svc := newTestService(t, cat, store)
	stream, err := svc.OpenImageByHash(context.Background(), "hash-a")
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	defer stream.Close()
	if stream.ContentType != "image/webp" {
		t.Fatalf("expected the catalog type, got %q", stream.ContentType)
	}

	// Known to S3 but not to the catalog: still servable.
	streamB, err := svc.OpenImageByHash(context.Background(), "hash-b")
	if err != nil {
		t.Fatalf("open unknown-to-catalog blob: %v", err)
	}
	defer streamB.Close()
	if got := readStream(t, streamB); string(got) != "bytes" {
		t.Fatalf("bytes=%q", got)
	}
}

func TestOpenImageByHashFetchFailurePropagatesAndRetries(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	store.objects[pipeline.BlobKey("hash-a")] = []byte("image-bytes")
	store.readErr[pipeline.BlobKey("hash-a")] = errors.New("connection reset")

	svc := newTestService(t, cat, store)

	if _, err := svc.OpenImageByHash(context.Background(), "hash-a"); err == nil {
		t.Fatalf("expected streaming failure")
	}

	// Nothing is cached, so a retry after the store recovers fetches again
	// and succeeds.
	delete(store.readErr, pipeline.BlobKey("hash-a"))
	if _, err := svc.OpenImageByHash(context.Background(), "hash-a"); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if got := store.getCount(); got != 2 {
		t.Fatalf("expected two S3 gets, got %d", got)
	}
}

func TestOpenImageByHashScaled(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	small := pngBytes(t, 13, 7)
	large := pngBytes(t, 400, 300)
	store.objects[pipeline.BlobKey("hash-small")] = small
	store.objects[pipeline.BlobKey("hash-large")] = large

	svc := newTestService(t, cat, store)
	ctx := context.Background()

	// Up-scaling adds nothing: the original passes through untouched.
	passthrough, err := svc.OpenImageByHashScaled(ctx, "hash-small", 320)
	if err != nil {
		t.Fatalf("passthrough: %v", err)
	}
	defer passthrough.Close()
	if got := readStream(t, passthrough); !bytes.Equal(got, small) {
		t.Fatalf("expected original bytes for a small image")
	}
	if passthrough.ContentType != "image/png" || passthrough.Hash != "hash-small" {
		t.Fatalf("passthrough metadata: %+v", passthrough)
	}

	// A large image becomes a scaled JPEG whose validator carries the width.
	scaled, err := svc.OpenImageByHashScaled(ctx, "hash-large", 320)
	if err != nil {
		t.Fatalf("scaled: %v", err)
	}
	defer scaled.Close()
	if scaled.ContentType != "image/jpeg" || scaled.Hash != "hash-large:w320" {
		t.Fatalf("scaled metadata: %+v", scaled)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(readStream(t, scaled)))
	if err != nil {
		t.Fatalf("decode scaled: %v", err)
	}
	if config.Width != 320 {
		t.Fatalf("scaled width=%d", config.Width)
	}

	if _, err := svc.OpenImageByHashScaled(ctx, "hash-small", 500); !errors.Is(err, ErrUnsupportedWidth) {
		t.Fatalf("expected ErrUnsupportedWidth, got %v", err)
	}
}

func TestPresignBlobURL(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeBlobStore()
	svc := newTestService(t, cat, store)

	url, err := svc.PresignBlobURL(context.Background(), "hash-a", time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if url != "memory://blobs/hash-a?ttl=1m0s" {
		t.Fatalf("unexpected url %q", url)
	}
	if got := store.presigned; len(got) != 1 || got[0] != "blobs/hash-a" {
		t.Fatalf("expected the logical blob key presigned, got %v", got)
	}

	if _, err := svc.PresignBlobURL(context.Background(), "  ", time.Minute); err == nil {
		t.Fatalf("expected error for empty hash")
	}
}
