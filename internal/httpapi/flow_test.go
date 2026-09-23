package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"viewer/internal/albums"
	"viewer/internal/catalog"
	"viewer/internal/feed"
	"viewer/internal/images"
	"viewer/internal/pipeline"
	"viewer/internal/qdrant"
	"viewer/internal/recommend"
	"viewer/internal/storage"
)

// memoryS3 is an in-memory stand-in for S3 that satisfies the store surfaces
// used by the albums service, the ingest pipeline and the image service.
type memoryS3 struct {
	mu   sync.Mutex
	data map[string][]byte
	puts []string
	// failPresignGet simulates the object store rejecting a signing request,
	// to exercise the claim handler's cleanup path.
	failPresignGet bool
}

func newMemoryS3() *memoryS3 {
	return &memoryS3{data: make(map[string][]byte)}
}

func (m *memoryS3) PresignPut(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memory://" + key, nil
}

func (m *memoryS3) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPresignGet {
		return "", errors.New("presign unavailable")
	}
	return "memory://" + key, nil
}

func (m *memoryS3) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.data[key]
	if !ok {
		return nil, "", storage.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(value)), "application/octet-stream", nil
}

func (m *memoryS3) PutObject(_ context.Context, key string, body io.Reader, _ string) error {
	value, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	m.puts = append(m.puts, key)
	return nil
}

func (m *memoryS3) HeadObject(_ context.Context, key string) (bool, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.data[key]
	if !ok {
		return false, 0, nil
	}
	return true, int64(len(value)), nil
}

func (m *memoryS3) putCountFor(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, key := range m.puts {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			count++
		}
	}
	return count
}

// stubEmbedder satisfies recommend.EmbeddingProvider without a checkpoint. The
// flow tests never embed anything: the stub only keeps the service non-nil so
// the recommendation endpoints stay reachable.
type stubEmbedder struct{}

func (stubEmbedder) Load(context.Context) error { return nil }

func (stubEmbedder) Embed(context.Context, []byte) ([]float32, error) {
	return nil, errors.New("stub embedder must not be called")
}

func (stubEmbedder) Close() error { return nil }

// fakeVectorStore is the harness's stand-in for the Qdrant client: points are
// keyed by the deterministic point IDs, search ranks honestly by cosine
// similarity with the query's album and hash excluded and one photo per album
// returned, and album points can be wiped the way a re-extraction does.
type fakeVectorStore struct {
	mu     sync.Mutex
	points map[string]qdrant.PhotoRecord
}

func newFakeVectorStore() *fakeVectorStore {
	return &fakeVectorStore{points: make(map[string]qdrant.PhotoRecord)}
}

func (f *fakeVectorStore) EnsureCollection(context.Context) error { return nil }

func (f *fakeVectorStore) UpsertPhotos(_ context.Context, records []qdrant.PhotoRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, record := range records {
		f.points[qdrant.PointID(record.AlbumID, record.Idx)] = record
	}
	return nil
}

func (f *fakeVectorStore) GroupSearch(_ context.Context, queryPointID, excludeAlbumID, excludeHash string, limit int) ([]qdrant.PhotoHit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	query, ok := f.points[queryPointID]
	if !ok {
		// The live client answers a missing query point with empty results.
		return nil, nil
	}
	type scored struct {
		record qdrant.PhotoRecord
		score  float64
	}
	var matches []scored
	for id, record := range f.points {
		if id == queryPointID || record.AlbumID == excludeAlbumID || record.Hash == excludeHash {
			continue
		}
		matches = append(matches, scored{record: record, score: cosineSimilarity(query.Vector, record.Vector)})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		if matches[i].record.AlbumID != matches[j].record.AlbumID {
			return matches[i].record.AlbumID < matches[j].record.AlbumID
		}
		return matches[i].record.Idx < matches[j].record.Idx
	})
	hits := make([]qdrant.PhotoHit, 0, limit)
	seenAlbum := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if _, dup := seenAlbum[match.record.AlbumID]; dup {
			continue
		}
		seenAlbum[match.record.AlbumID] = struct{}{}
		hits = append(hits, qdrant.PhotoHit{
			AlbumID: match.record.AlbumID,
			Idx:     match.record.Idx,
			Hash:    match.record.Hash,
			W:       match.record.W,
			H:       match.record.H,
			Score:   match.score,
		})
		if len(hits) == limit {
			break
		}
	}
	return hits, nil
}

func (f *fakeVectorStore) RetrieveVectorsByHashes(_ context.Context, hashes []string) (map[string][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vectors := make(map[string][]float32, len(hashes))
	for _, record := range f.points {
		if _, seen := vectors[record.Hash]; seen {
			continue
		}
		for _, hash := range hashes {
			if hash == record.Hash {
				vectors[record.Hash] = append([]float32(nil), record.Vector...)
				break
			}
		}
	}
	return vectors, nil
}

func (f *fakeVectorStore) DeleteByAlbum(_ context.Context, albumID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, record := range f.points {
		if record.AlbumID == albumID {
			delete(f.points, id)
		}
	}
	return nil
}

func (f *fakeVectorStore) CountVectors(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.points), nil
}

func (f *fakeVectorStore) pointCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.points)
}

// cosineSimilarity is the ranking the fake promises: scale-invariant similarity
// in [-1, 1], with a zero vector defined as matching nothing.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / math.Sqrt(normA*normB)
}

type flowHarness struct {
	router   http.Handler
	albums   *albums.Service
	s3       *memoryS3
	catalog  *catalog.Store
	pipeline *pipeline.Service
	vectors  *fakeVectorStore
}

func newFlowHarness(t *testing.T) *flowHarness {
	return newFlowHarnessWithToken(t, "")
}

// newFlowHarnessWithToken builds the standard harness with a worker token set,
// so tests can exercise the worker auth path.
func newFlowHarnessWithToken(t *testing.T, workerToken string) *flowHarness {
	t.Helper()

	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = cat.Close() })

	s3 := newMemoryS3()
	zipCacheDir := t.TempDir()

	imageService := images.NewService(cat, s3)
	// A stub embedder keeps the recommendation endpoints available without a
	// checkpoint. It is never asked to embed anything: the pipeline does not
	// embed at all and the embedding workers are not started here, so blobs
	// stay pending. The in-memory vector store gives the worker write-back and
	// the recommendation search somewhere real to land.
	vectors := newFakeVectorStore()
	recommendService := recommend.NewService(cat, vectors, imageService, stubEmbedder{}, nil)
	pipelineService := pipeline.NewService(cat, s3, pipeline.Options{
		TempDir: zipCacheDir,
		OnAlbumReady: func(albumID string) {
			_ = recommendService.ReloadAlbum(context.Background(), albumID)
		},
	})
	albumService := albums.NewService(cat, s3, pipelineService)
	pipelineService.Start(context.Background())

	return &flowHarness{
		router:   New(albumService, feed.NewService(albumService), imageService, recommendService, workerToken).Router(),
		albums:   albumService,
		s3:       s3,
		catalog:  cat,
		pipeline: pipelineService,
		vectors:  vectors,
	}
}

func (h *flowHarness) do(t *testing.T, method string, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

func (h *flowHarness) uploadZip(t *testing.T, filename string, zipData []byte) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"filename":  filename,
		"sizeBytes": len(zipData),
	})
	if err != nil {
		t.Fatalf("marshal create album: %v", err)
	}
	rec := h.do(t, http.MethodPost, "/api/albums", payload)
	if rec.Code != http.StatusOK {
		t.Fatalf("create album status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		AlbumID   string `json:"albumId"`
		ObjectKey string `json:"objectKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create album: %v", err)
	}
	if created.ObjectKey == "" {
		t.Fatalf("expected object key in create response")
	}

	// Simulate the browser's direct-to-S3 presigned PUT.
	if err := h.s3.PutObject(context.Background(), created.ObjectKey, bytes.NewReader(zipData), "application/zip"); err != nil {
		t.Fatalf("stage zip: %v", err)
	}
	return created.AlbumID
}

func (h *flowHarness) finalizeAndWait(t *testing.T, albumID string) {
	t.Helper()
	rec := h.do(t, http.MethodPost, "/api/albums/"+albumID+"/finalize", nil)
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		t.Fatalf("finalize status=%d body=%s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		statusRec := h.do(t, http.MethodGet, "/api/albums/"+albumID+"/finalize", nil)
		if statusRec.Code != http.StatusOK {
			t.Fatalf("finalize status=%d body=%s", statusRec.Code, statusRec.Body.String())
		}
		var state struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(statusRec.Body.Bytes(), &state); err != nil {
			t.Fatalf("decode finalize state: %v", err)
		}
		switch state.Status {
		case "SUCCEEDED":
			return
		case "FAILED":
			t.Fatalf("finalize failed: %s", state.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for finalize")
}

func testPNG(t *testing.T, width int, height int, fill uint8) []byte {
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

func testZip(t *testing.T, entries map[string][]byte, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, name := range order {
		w, err := writer.Create(name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		if _, err := w.Write(entries[name]); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestUploadFinalizeServeFlow(t *testing.T) {
	harness := newFlowHarness(t)

	imageA := testPNG(t, 4, 2, 10)
	imageB := testPNG(t, 2, 6, 200)
	zipData := testZip(t,
		map[string][]byte{"001.png": imageA, "002.png": imageB},
		[]string{"002.png", "001.png"},
	)

	albumID := harness.uploadZip(t, "holiday.zip", zipData)
	harness.finalizeAndWait(t, albumID)

	// Album metadata is now served from SQLite.
	albumRec := harness.do(t, http.MethodGet, "/api/albums/"+albumID, nil)
	if albumRec.Code != http.StatusOK {
		t.Fatalf("get album status=%d body=%s", albumRec.Code, albumRec.Body.String())
	}
	var album struct {
		AlbumID          string `json:"albumId"`
		OriginalFilename string `json:"originalFilename"`
		PhotoCount       int    `json:"photoCount"`
		Photos           []struct {
			I     int     `json:"i"`
			Name  string  `json:"name"`
			W     int     `json:"w"`
			H     int     `json:"h"`
			Ratio float64 `json:"ratio"`
		} `json:"photos"`
	}
	if err := json.Unmarshal(albumRec.Body.Bytes(), &album); err != nil {
		t.Fatalf("decode album: %v", err)
	}
	if album.AlbumID != albumID || album.OriginalFilename != "holiday.zip" || album.PhotoCount != 2 {
		t.Fatalf("unexpected album: %+v", album)
	}
	if album.Photos[0].Name != "001.png" || album.Photos[1].Name != "002.png" {
		t.Fatalf("expected filename-sorted photos, got %+v", album.Photos)
	}
	if album.Photos[0].W != 4 || album.Photos[0].H != 2 {
		t.Fatalf("unexpected dimensions: %+v", album.Photos[0])
	}

	// Images are served from content-addressed blobs, keyed by hash.
	firstPhoto, err := harness.catalog.PhotoAt(context.Background(), albumID, 0)
	if err != nil {
		t.Fatalf("photo at 0: %v", err)
	}
	imgRec := harness.do(t, http.MethodGet, "/api/image/"+firstPhoto.Hash, nil)
	if imgRec.Code != http.StatusOK {
		t.Fatalf("get image status=%d body=%s", imgRec.Code, imgRec.Body.String())
	}
	if !bytes.Equal(imgRec.Body.Bytes(), imageA) {
		t.Fatalf("served image bytes do not match the zip entry")
	}
	if contentType := imgRec.Header().Get("Content-Type"); contentType != "image/png" {
		t.Fatalf("content type=%q want=image/png", contentType)
	}

	// The two distinct image blobs are stored under content-addressed keys,
	// and the staged zip stays until the backup finalizer deletes it after a
	// drain.
	if got := harness.s3.putCountFor("blobs/"); got != 2 {
		t.Fatalf("blob puts=%d want=2", got)
	}
	harness.s3.mu.Lock()
	staged := len(harness.s3.data)
	harness.s3.mu.Unlock()
	if staged != 3 {
		t.Fatalf("expected the two blobs plus the staged zip in storage, got %d objects", staged)
	}
	if _, err := harness.catalog.GetAlbum(context.Background(), albumID); err != nil {
		t.Fatalf("album should be in catalog: %v", err)
	}

	// Search and feed both see the ready album. The search response carries the
	// cover: the photo at index 0 with its denormalized dimensions.
	searchRec := harness.do(t, http.MethodGet, "/api/albums/search?q=holiday", nil)
	if searchRec.Code != http.StatusOK || !bytes.Contains(searchRec.Body.Bytes(), []byte(albumID)) {
		t.Fatalf("search did not return the album: %d %s", searchRec.Code, searchRec.Body.String())
	}
	// The cover carries the blob hash now, so the client can build image URLs
	// without an album lookup.
	if !bytes.Contains(searchRec.Body.Bytes(), []byte(`"cover":{"i":0,"hash":"`)) {
		t.Fatalf("search response missing cover hash: %s", searchRec.Body.String())
	}
	feedRec := harness.do(t, http.MethodGet, "/api/feed?limit=10", nil)
	if feedRec.Code != http.StatusOK || !bytes.Contains(feedRec.Body.Bytes(), []byte(albumID)) {
		t.Fatalf("feed did not return the album: %d %s", feedRec.Code, feedRec.Body.String())
	}
}

func TestAlbumSearchMatchesSubstringsAndValidatesLimit(t *testing.T) {
	harness := newFlowHarness(t)

	imageA := testPNG(t, 4, 2, 10)
	zipData := testZip(t, map[string][]byte{"001.png": imageA}, []string{"001.png"})
	albumID := harness.uploadZip(t, "2026-holiday.zip", zipData)
	harness.finalizeAndWait(t, albumID)

	// A mid-string fragment finds the album, not just a filename prefix.
	midRec := harness.do(t, http.MethodGet, "/api/albums/search?q=liday", nil)
	if midRec.Code != http.StatusOK || !bytes.Contains(midRec.Body.Bytes(), []byte(albumID)) {
		t.Fatalf("mid-string search did not return the album: %d %s", midRec.Code, midRec.Body.String())
	}

	missRec := harness.do(t, http.MethodGet, "/api/albums/search?q=zzzz", nil)
	if missRec.Code != http.StatusOK || !bytes.Contains(missRec.Body.Bytes(), []byte(`"albums":[]`)) {
		t.Fatalf("unexpected miss response: %d %s", missRec.Code, missRec.Body.String())
	}

	limitRec := harness.do(t, http.MethodGet, "/api/albums/search?limit=0", nil)
	if limitRec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 status=%d body=%s", limitRec.Code, limitRec.Body.String())
	}
}

func TestReuploadingIdenticalContentReusesBlobs(t *testing.T) {
	harness := newFlowHarness(t)

	imageA := testPNG(t, 3, 3, 77)
	zipData := testZip(t, map[string][]byte{"only.png": imageA}, []string{"only.png"})

	firstAlbum := harness.uploadZip(t, "first.zip", zipData)
	harness.finalizeAndWait(t, firstAlbum)
	blobsAfterFirst := harness.s3.putCountFor("blobs/")

	secondAlbum := harness.uploadZip(t, "second.zip", zipData)
	harness.finalizeAndWait(t, secondAlbum)

	if got := harness.s3.putCountFor("blobs/"); got != blobsAfterFirst {
		t.Fatalf("identical content must not be re-uploaded: blobs=%d want=%d", got, blobsAfterFirst)
	}

	firstPhoto, err := harness.catalog.PhotoAt(context.Background(), firstAlbum, 0)
	if err != nil {
		t.Fatalf("first photo: %v", err)
	}
	secondPhoto, err := harness.catalog.PhotoAt(context.Background(), secondAlbum, 0)
	if err != nil {
		t.Fatalf("second photo: %v", err)
	}
	if firstPhoto.Hash != secondPhoto.Hash {
		t.Fatalf("expected shared content hash, got %s vs %s", firstPhoto.Hash, secondPhoto.Hash)
	}
}

func TestFinalizeUnknownAlbumReturnsNotFound(t *testing.T) {
	harness := newFlowHarness(t)
	rec := harness.do(t, http.MethodPost, "/api/albums/missing/finalize", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want=404 body=%s", rec.Code, rec.Body.String())
	}
}
