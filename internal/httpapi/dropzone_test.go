package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/ingest"
	"viewer/internal/storage"
)

// ListObjects completes the store surface the upload scan needs. Real S3
// derives an object's ETag from its bytes, and the scan keys albums by
// "<etag>:<size>", so the stub does the same and identical zips resolve to one
// album here too.
func (m *memoryS3) ListObjects(_ context.Context, prefix string) ([]storage.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	objects := make([]storage.Object, 0, len(m.data))
	for key, value := range m.data {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		objects = append(objects, storage.Object{
			Key:  key,
			Size: int64(len(value)),
			ETag: fmt.Sprintf("\"%x\"", sha256.Sum256(value)),
		})
	}
	return objects, nil
}

// TestUploadDropZoneAdoptsZip is the point of scanning the upload prefix: a zip
// that appears there without going through POST /api/albums becomes an album and
// is extracted, so an operator or an external tool can drop one in while the
// server is running.
func TestUploadDropZoneAdoptsZip(t *testing.T) {
	harness := newFlowHarness(t)

	image := testPNG(t, 5, 3, 77)
	zipData := testZip(t, map[string][]byte{"only.png": image}, []string{"only.png"})
	key := "uploads/dropped-trip.zip"
	if err := harness.s3.PutObject(context.Background(), key, bytes.NewReader(zipData), "application/zip"); err != nil {
		t.Fatalf("drop zip: %v", err)
	}

	summary, err := ingest.Run(context.Background(), harness.s3, harness.albums)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if want := (ingest.Summary{Discovered: 1, Registered: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}

	album := waitForReadyAlbum(t, harness)

	// The scan is repeatable: the pipeline deletes the zip once the album is
	// extracted, so a second pass finds nothing and registers no second album.
	again, err := ingest.Run(context.Background(), harness.s3, harness.albums)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if again != (ingest.Summary{}) {
		t.Fatalf("second summary = %+v, want an empty scan", again)
	}
	ready, err := harness.catalog.ListAlbumsByStatus(context.Background(), catalog.AlbumStatusReady)
	if err != nil {
		t.Fatalf("list ready albums: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != album.ID {
		t.Fatalf("ready albums = %+v, want only %s", ready, album.ID)
	}

	// The adopted album is served like any other.
	albumRec := harness.do(t, http.MethodGet, "/api/albums/"+album.ID, nil)
	if albumRec.Code != http.StatusOK || !bytes.Contains(albumRec.Body.Bytes(), []byte("only.png")) {
		t.Fatalf("get album status=%d body=%s", albumRec.Code, albumRec.Body.String())
	}
	imgRec := harness.do(t, http.MethodGet, "/api/image/"+album.ID+"/0", nil)
	if imgRec.Code != http.StatusOK || !bytes.Equal(imgRec.Body.Bytes(), image) {
		t.Fatalf("get image status=%d (want the dropped zip's bytes)", imgRec.Code)
	}
}

// TestUploadScanRequeuesAnUnfinalizedUpload covers the recovery the scan took
// over from the old pending-enqueue pass: a client that uploaded its zip and
// never called finalize - or a server that restarted before it did - still gets
// its album extracted, because the zip is still in the prefix.
func TestUploadScanRequeuesAnUnfinalizedUpload(t *testing.T) {
	harness := newFlowHarness(t)

	zipData := testZip(t, map[string][]byte{"a.png": testPNG(t, 2, 4, 5)}, []string{"a.png"})
	albumID := harness.uploadZip(t, "unfinalized.zip", zipData)

	summary, err := ingest.Run(context.Background(), harness.s3, harness.albums)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if want := (ingest.Summary{Discovered: 1, Requeued: 1}); summary != want {
		t.Fatalf("summary = %+v, want %+v (the album is already registered, so it is only queued again)", summary, want)
	}

	album := waitForReadyAlbum(t, harness)
	if album.ID != albumID {
		t.Fatalf("ready album=%s want the album the API created (%s)", album.ID, albumID)
	}
	if album.PhotoCount != 1 || album.OriginalFilename != "unfinalized.zip" {
		t.Fatalf("unexpected album: %+v", album)
	}
}

// waitForReadyAlbum returns the single album the pipeline extracted.
func waitForReadyAlbum(t *testing.T, harness *flowHarness) catalog.Album {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		failed, err := harness.catalog.ListAlbumsByStatus(context.Background(), catalog.AlbumStatusFailed)
		if err != nil {
			t.Fatalf("list failed albums: %v", err)
		}
		if len(failed) > 0 {
			t.Fatalf("album failed: %+v", failed[0])
		}
		ready, err := harness.catalog.ListAlbumsByStatus(context.Background(), catalog.AlbumStatusReady)
		if err != nil {
			t.Fatalf("list ready albums: %v", err)
		}
		if len(ready) == 1 {
			return ready[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the dropped album to be extracted")
	return catalog.Album{}
}
