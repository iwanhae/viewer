package pipeline

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"viewer/internal/catalog"
	"viewer/internal/storage"
)

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	putKeys []string
	deleted []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string][]byte)}
}

func (f *fakeStore) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), "application/zip", nil
}

func (f *fakeStore) PutObject(_ context.Context, key string, body io.Reader, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = data
	f.putKeys = append(f.putKeys, key)
	return nil
}

func (f *fakeStore) HeadObject(_ context.Context, key string) (bool, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.objects[key]
	if !ok {
		return false, 0, nil
	}
	return true, int64(len(data)), nil
}

func (f *fakeStore) DeleteObject(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *fakeStore) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.putKeys)
}

func (f *fakeStore) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

type fakeEmbedder struct {
	mu    sync.Mutex
	calls int
	err   error
	dim   int
}

func (f *fakeEmbedder) Embed(_ context.Context, imageBytes []byte) ([]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.dim <= 0 {
		f.dim = 3
	}
	sum := sha256.Sum256(imageBytes)
	vector := make([]float32, f.dim)
	for i := range vector {
		vector[i] = float32(sum[i])
	}
	return vector, nil
}

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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

type zipEntry struct {
	name string
	data []byte
}

func buildZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, entry := range entries {
		w, err := writer.Create(entry.name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", entry.name, err)
		}
		if _, err := w.Write(entry.data); err != nil {
			t.Fatalf("write zip entry %s: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func pngBytes(t *testing.T, width int, height int, fill uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{R: fill, G: fill, B: fill, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func seedAlbum(t *testing.T, cat *catalog.Store, store *fakeStore, albumID string, filename string, zipData []byte) {
	t.Helper()
	sourceKey := SourceKey(albumID)
	if err := cat.CreateAlbum(context.Background(), catalog.Album{
		ID:               albumID,
		OriginalFilename: filename,
		SizeBytes:        int64(len(zipData)),
		Status:           catalog.AlbumStatusQueued,
		SourceKey:        sourceKey,
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}
	store.mu.Lock()
	store.objects[sourceKey] = zipData
	store.mu.Unlock()
}

func TestProcessAlbumExtractsImagesAndDedupesBlobs(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeStore()
	embedder := &fakeEmbedder{}

	imageA := pngBytes(t, 4, 2, 10)
	imageB := pngBytes(t, 2, 6, 200)
	zipData := buildZip(t,
		zipEntry{name: "notes.txt", data: []byte("ignore me")},
		zipEntry{name: "b.png", data: imageB},
		zipEntry{name: "a.png", data: imageA},
		zipEntry{name: "dup.png", data: imageA},
	)
	seedAlbum(t, cat, store, "album-a", "holiday.zip", zipData)

	svc := NewService(cat, store, embedder, Options{TempDir: t.TempDir()})
	if err := svc.ProcessAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("process album: %v", err)
	}

	album, err := cat.GetAlbum(context.Background(), "album-a")
	if err != nil {
		t.Fatalf("get album: %v", err)
	}
	if album.Status != catalog.AlbumStatusReady {
		t.Fatalf("status=%s want=%s", album.Status, catalog.AlbumStatusReady)
	}
	if album.PhotoCount != 3 {
		t.Fatalf("photo count=%d want=3", album.PhotoCount)
	}

	photos, err := cat.PhotosByAlbum(context.Background(), "album-a")
	if err != nil {
		t.Fatalf("photos: %v", err)
	}
	if len(photos) != 3 {
		t.Fatalf("photos=%d want=3", len(photos))
	}
	wantNames := []string{"a.png", "b.png", "dup.png"}
	for i, want := range wantNames {
		if photos[i].Name != want {
			t.Fatalf("photo[%d].name=%q want=%q", i, photos[i].Name, want)
		}
	}
	if photos[0].Hash != hashOf(imageA) {
		t.Fatalf("a.png hash mismatch")
	}
	if photos[2].Hash != photos[0].Hash {
		t.Fatalf("duplicate content should share a hash")
	}
	if photos[0].Width != 4 || photos[0].Height != 2 {
		t.Fatalf("unexpected a.png dimensions: %dx%d", photos[0].Width, photos[0].Height)
	}
	if photos[1].Width != 2 || photos[1].Height != 6 {
		t.Fatalf("unexpected b.png dimensions: %dx%d", photos[1].Width, photos[1].Height)
	}

	// Two distinct images, three zip entries: the blob must be stored once.
	if got := store.putCount(); got != 2 {
		t.Fatalf("put count=%d want=2 (dedupe by content hash)", got)
	}
	if !store.has(BlobKey(hashOf(imageA))) || !store.has(BlobKey(hashOf(imageB))) {
		t.Fatalf("expected content-addressed blob objects")
	}

	// The staged zip is removed after a successful extraction.
	if store.has(SourceKey("album-a")) {
		t.Fatalf("expected staged zip to be deleted")
	}

	// The duplicate blob is embedded once, and both blobs end up ready.
	for _, hash := range []string{hashOf(imageA), hashOf(imageB)} {
		blob, err := cat.GetBlob(context.Background(), hash)
		if err != nil {
			t.Fatalf("get blob %s: %v", hash, err)
		}
		if blob.EmbeddingStatus != catalog.EmbeddingStatusReady || len(blob.Embedding) == 0 {
			t.Fatalf("blob %s embedding not ready: %+v", hash, blob)
		}
		if blob.ContentType != "image/png" {
			t.Fatalf("blob %s content type=%q", hash, blob.ContentType)
		}
	}
	if got := embedder.callCount(); got != 2 {
		t.Fatalf("embed calls=%d want=2 (duplicate content embedded once)", got)
	}
}

func TestProcessAlbumCrossAlbumDedupeStoresBlobOnce(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeStore()

	shared := pngBytes(t, 3, 3, 42)
	zipA := buildZip(t, zipEntry{name: "shared.png", data: shared}, zipEntry{name: "only-a.png", data: pngBytes(t, 3, 3, 1)})
	zipB := buildZip(t, zipEntry{name: "shared.png", data: shared}, zipEntry{name: "only-b.png", data: pngBytes(t, 3, 3, 2)})
	seedAlbum(t, cat, store, "album-a", "a.zip", zipA)
	seedAlbum(t, cat, store, "album-b", "b.zip", zipB)

	svc := NewService(cat, store, nil, Options{TempDir: t.TempDir()})
	if err := svc.ProcessAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("process album-a: %v", err)
	}
	putsAfterA := store.putCount()
	if err := svc.ProcessAlbum(context.Background(), "album-b"); err != nil {
		t.Fatalf("process album-b: %v", err)
	}

	if got := store.putCount(); got != putsAfterA+1 {
		t.Fatalf("second album should only upload its new blob: puts=%d want=%d", got, putsAfterA+1)
	}

	hash := hashOf(shared)
	photosA, _ := cat.PhotosByAlbum(context.Background(), "album-a")
	photosB, _ := cat.PhotosByAlbum(context.Background(), "album-b")
	if photosA[1].Hash != hash || photosB[1].Hash != hash {
		t.Fatalf("expected both albums to reference the shared hash %s", hash)
	}
}

func TestProcessAlbumNoValidImagesMarksFailedAndKeepsSource(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeStore()
	zipData := buildZip(t, zipEntry{name: "readme.txt", data: []byte("nothing here")}, zipEntry{name: "dir/", data: nil})
	seedAlbum(t, cat, store, "album-a", "junk.zip", zipData)

	svc := NewService(cat, store, nil, Options{TempDir: t.TempDir()})
	err := svc.ProcessAlbum(context.Background(), "album-a")
	if !errors.Is(err, ErrNoValidImages) {
		t.Fatalf("expected ErrNoValidImages, got %v", err)
	}

	album, _ := cat.GetAlbum(context.Background(), "album-a")
	if album.Status != catalog.AlbumStatusFailed {
		t.Fatalf("status=%s want=%s", album.Status, catalog.AlbumStatusFailed)
	}
	if album.Error == "" {
		t.Fatalf("expected error message to be recorded")
	}
	if !store.has(SourceKey("album-a")) {
		t.Fatalf("failed extraction must keep the staged zip for retry")
	}
}

func TestProcessAlbumMissingSourceMarksFailed(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeStore()
	if err := cat.CreateAlbum(context.Background(), catalog.Album{
		ID:        "album-a",
		Status:    catalog.AlbumStatusQueued,
		SourceKey: SourceKey("album-a"),
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}

	svc := NewService(cat, store, nil, Options{TempDir: t.TempDir()})
	if err := svc.ProcessAlbum(context.Background(), "album-a"); err == nil {
		t.Fatalf("expected error for missing staged zip")
	}
	album, _ := cat.GetAlbum(context.Background(), "album-a")
	if album.Status != catalog.AlbumStatusFailed {
		t.Fatalf("status=%s want=%s", album.Status, catalog.AlbumStatusFailed)
	}
}

func TestProcessAlbumIsIdempotentOnceReady(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeStore()
	embedder := &fakeEmbedder{}
	zipData := buildZip(t, zipEntry{name: "a.png", data: pngBytes(t, 2, 2, 1)})
	seedAlbum(t, cat, store, "album-a", "a.zip", zipData)

	svc := NewService(cat, store, embedder, Options{TempDir: t.TempDir()})
	if err := svc.ProcessAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("first process: %v", err)
	}
	puts := store.putCount()
	calls := embedder.callCount()

	if err := svc.ProcessAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("second process: %v", err)
	}
	if store.putCount() != puts || embedder.callCount() != calls {
		t.Fatalf("ready album should be a no-op")
	}
}

func TestProcessAlbumRecordsEmbedFailureButKeepsAlbumReady(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeStore()
	embedder := &fakeEmbedder{err: errors.New("worker exploded")}
	zipData := buildZip(t, zipEntry{name: "a.png", data: pngBytes(t, 2, 2, 7)})
	seedAlbum(t, cat, store, "album-a", "a.zip", zipData)

	svc := NewService(cat, store, embedder, Options{TempDir: t.TempDir()})
	if err := svc.ProcessAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("process album: %v", err)
	}

	album, _ := cat.GetAlbum(context.Background(), "album-a")
	if album.Status != catalog.AlbumStatusReady {
		t.Fatalf("status=%s want=%s", album.Status, catalog.AlbumStatusReady)
	}
	blob, err := cat.GetBlob(context.Background(), hashOf(pngBytes(t, 2, 2, 7)))
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if blob.EmbeddingStatus != catalog.EmbeddingStatusFailed {
		t.Fatalf("embedding status=%s want=%s", blob.EmbeddingStatus, catalog.EmbeddingStatusFailed)
	}
	if blob.EmbeddingError == "" {
		t.Fatalf("expected embedding error to be stored")
	}
}

func TestProcessAlbumLeavesEmbeddingPendingWithoutEmbedder(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeStore()
	zipData := buildZip(t, zipEntry{name: "a.png", data: pngBytes(t, 2, 2, 3)})
	seedAlbum(t, cat, store, "album-a", "a.zip", zipData)

	svc := NewService(cat, store, nil, Options{TempDir: t.TempDir()})
	if err := svc.ProcessAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("process album: %v", err)
	}

	blob, err := cat.GetBlob(context.Background(), hashOf(pngBytes(t, 2, 2, 3)))
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if blob.EmbeddingStatus != catalog.EmbeddingStatusPending {
		t.Fatalf("embedding status=%s want=%s", blob.EmbeddingStatus, catalog.EmbeddingStatusPending)
	}
}

func TestProcessAlbumInvokesReadyHook(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeStore()
	zipData := buildZip(t, zipEntry{name: "a.png", data: pngBytes(t, 2, 2, 9)})
	seedAlbum(t, cat, store, "album-a", "a.zip", zipData)

	var ready []string
	svc := NewService(cat, store, nil, Options{
		TempDir:      t.TempDir(),
		OnAlbumReady: func(albumID string) { ready = append(ready, albumID) },
	})
	if err := svc.ProcessAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("process album: %v", err)
	}
	if len(ready) != 1 || ready[0] != "album-a" {
		t.Fatalf("ready hook calls=%v", ready)
	}
}

func TestProcessAlbumReplacesPhotosOnReprocess(t *testing.T) {
	cat := openTestCatalog(t)
	store := newFakeStore()
	first := buildZip(t, zipEntry{name: "a.png", data: pngBytes(t, 2, 2, 1)}, zipEntry{name: "b.png", data: pngBytes(t, 2, 2, 2)})
	seedAlbum(t, cat, store, "album-a", "a.zip", first)

	svc := NewService(cat, store, nil, Options{TempDir: t.TempDir()})
	if err := svc.ProcessAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("process album: %v", err)
	}
	// Simulate a retry: force the album back to queued and stage a smaller zip.
	second := buildZip(t, zipEntry{name: "c.png", data: pngBytes(t, 5, 5, 3)})
	if err := cat.SetAlbumStatus(context.Background(), "album-a", catalog.AlbumStatusQueued, ""); err != nil {
		t.Fatalf("reset status: %v", err)
	}
	store.mu.Lock()
	store.objects[SourceKey("album-a")] = second
	delete(store.objects, BlobKey(hashOf(pngBytes(t, 2, 2, 1))))
	store.mu.Unlock()

	if err := svc.ProcessAlbum(context.Background(), "album-a"); err != nil {
		t.Fatalf("reprocess album: %v", err)
	}
	photos, err := cat.PhotosByAlbum(context.Background(), "album-a")
	if err != nil {
		t.Fatalf("photos: %v", err)
	}
	if len(photos) != 1 || photos[0].Name != "c.png" || photos[0].Index != 0 {
		t.Fatalf("expected photos to be replaced, got %+v", photos)
	}
}

func TestEnqueueDeduplicatesInFlightAlbums(t *testing.T) {
	svc := NewService(nil, nil, nil, Options{})
	if err := svc.Enqueue("album-a"); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := svc.Enqueue("album-a"); err != nil {
		t.Fatalf("duplicate enqueue should be ignored, got %v", err)
	}
	if err := svc.Enqueue(""); err == nil {
		t.Fatalf("expected error for empty album id")
	}
}

func TestSourceKeyAndBlobKeyLayout(t *testing.T) {
	if got := SourceKey("abc"); got != "uploads/abc.zip" {
		t.Fatalf("SourceKey=%q", got)
	}
	if got := BlobKey("deadbeef"); got != "blobs/deadbeef" {
		t.Fatalf("BlobKey=%q", got)
	}
	if UploadPrefix != "uploads/" {
		t.Fatalf("UploadPrefix=%q", UploadPrefix)
	}
}

// TestStagedAlbumIDInvertsSourceKey pins the round trip the upload scan relies
// on to match a listed object with the album row that owns it.
func TestStagedAlbumIDInvertsSourceKey(t *testing.T) {
	id, ok := StagedAlbumID(SourceKey("album-a"))
	if !ok || id != "album-a" {
		t.Fatalf("StagedAlbumID(SourceKey(album-a))=%q,%v want album-a,true", id, ok)
	}

	for _, key := range []string{"blobs/deadbeef", "uploads/", "uploads/.zip", "uploads/nested/a.zip", "uploads/a.zip.bak"} {
		if id, ok := StagedAlbumID(key); ok {
			t.Errorf("StagedAlbumID(%q)=%q,true want no match", key, id)
		}
	}
}

func TestImageEntriesFiltersAndSorts(t *testing.T) {
	zipData := buildZip(t,
		zipEntry{name: "B.PNG", data: pngBytes(t, 1, 1, 1)},
		zipEntry{name: "a.jpg", data: []byte("jpg")},
		zipEntry{name: "notes.md", data: []byte("nope")},
		zipEntry{name: "c.webp", data: []byte("webp")},
		zipEntry{name: "d.gif", data: []byte("gif")},
	)
	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	entries := imageEntries(reader.File)
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name)
	}
	want := []string{"a.jpg", "B.PNG", "c.webp"}
	if len(got) != len(want) {
		t.Fatalf("entries=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries=%v want=%v", got, want)
		}
	}
}

func TestContentTypeFor(t *testing.T) {
	cases := map[string]string{
		"a.jpg":  "image/jpeg",
		"b.JPEG": "image/jpeg",
		"c.png":  "image/png",
		"d.webp": "image/webp",
	}
	for name, want := range cases {
		if got := contentTypeFor(nil, name); got != want {
			t.Fatalf("contentTypeFor(%q)=%q want=%q", name, got, want)
		}
	}
}
