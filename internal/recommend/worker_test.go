package recommend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/images"
	"viewer/internal/storage"
	"viewer/internal/vision"
)

// blobStoreStub serves image bytes straight from memory, which keeps the
// background worker test off S3.
type blobStoreStub struct {
	objects map[string][]byte
}

func (b *blobStoreStub) GetObject(_ context.Context, key string) (io.ReadCloser, string, error) {
	data, ok := b.objects[key]
	if !ok {
		return nil, "", fmt.Errorf("%w: %s", storage.ErrObjectNotFound, key)
	}
	return io.NopCloser(bytes.NewReader(data)), "image/png", nil
}

func (b *blobStoreStub) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "memory://" + key, nil
}

// logCapture collects everything written to the standard logger while it is
// installed. The embedding progress is a log-first feature, so this is the only
// way to assert on it.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, string(p))
	return len(p), nil
}

func (c *logCapture) contains(substring string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, line := range c.lines {
		if strings.Contains(line, substring) {
			return true
		}
	}
	return false
}

// TestEmbeddingWorkerReportsProgressWithRealModel drives the background worker
// over three real images and checks what an operator watching stdout and the
// status endpoint would see. It is skipped without VISION_REFERENCE_DIR because
// it needs the real checkpoint.
func TestEmbeddingWorkerReportsProgressWithRealModel(t *testing.T) {
	dir := os.Getenv("VISION_REFERENCE_DIR")
	if dir == "" {
		t.Skip("VISION_REFERENCE_DIR is not set")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cat := newTestCatalog(t)
	if err := cat.CreateAlbum(ctx, catalog.Album{
		ID:               "album-live",
		OriginalFilename: "live.zip",
		Status:           catalog.AlbumStatusReady,
	}); err != nil {
		t.Fatalf("create album: %v", err)
	}

	store := &blobStoreStub{objects: make(map[string][]byte)}
	for i, name := range []string{"refimg_1000x300.png", "refimg_640x480.png", "refimg_13x7.png"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		store.objects["blobs/"+hash] = data
		if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(data)), ContentType: "image/png"}); err != nil {
			t.Fatalf("upsert blob: %v", err)
		}
		if err := cat.InsertPhoto(ctx, catalog.Photo{
			AlbumID: "album-live", Index: i, Name: name, Hash: hash,
		}); err != nil {
			t.Fatalf("insert photo: %v", err)
		}
	}

	imageService := images.NewService(cat, store)
	svc := NewService(cat, imageService, NewVisionEmbedder(vision.Config{ModelID: filepath.Join(dir, "model"), Backend: "go"}))
	t.Cleanup(func() { _ = svc.Close() })
	if err := svc.LoadModel(ctx); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}

	if progress := svc.EmbeddingProgress(); progress.Total != 3 || progress.Pending != 3 || !progress.Enabled {
		t.Fatalf("progress before the run=%+v want 3 pending and enabled", progress)
	}

	capture := &logCapture{}
	log.SetOutput(capture)
	defer log.SetOutput(os.Stderr)

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(10 * time.Minute)
	observedActive := false
	for {
		progress := svc.EmbeddingProgress()
		observedActive = observedActive || progress.Active
		if progress.Pending == 0 && progress.Ready == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the worker: %+v", progress)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !observedActive {
		t.Fatalf("the status endpoint never reported an in-flight embedding")
	}
	if progress := svc.EmbeddingProgress(); progress.Ratio != 1 {
		t.Fatalf("progress after the run=%+v want ratio 1", progress)
	}

	for _, want := range []string{
		"embedding run started pending=3",
		"embedded blob=",
		"embedding run finished embedded=3",
	} {
		if !capture.contains(want) {
			t.Fatalf("missing %q in the log output:\n%s", want, strings.Join(capture.lines, ""))
		}
	}
}
