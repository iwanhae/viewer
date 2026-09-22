package recommend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/vision"
)

func TestIsTransientEmbedError(t *testing.T) {
	// In-process inference has no remote failure modes, so ordinary errors must
	// not be retried forever.
	if isTransientEmbedError(errors.New("decode image: unsupported format")) {
		t.Fatalf("expected a decode error to be permanent")
	}
	if isTransientEmbedError(nil) {
		t.Fatalf("expected nil to be non-transient")
	}
	// Shutdown still counts as transient: the worker exits and the next run
	// picks the blob up again.
	if !isTransientEmbedError(context.Canceled) {
		t.Fatalf("expected context.Canceled to be transient")
	}
	if !isTransientEmbedError(context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded to be transient")
	}
	if !isTransientEmbedError(errors.Join(errors.New("embed image"), context.DeadlineExceeded)) {
		t.Fatalf("expected a wrapped deadline error to be transient")
	}
}

func TestEmbeddingTimeoutIsFiveMinutes(t *testing.T) {
	if got, want := embeddingTimeout, 5*time.Minute; got != want {
		t.Fatalf("embeddingTimeout=%s want=%s", got, want)
	}
}

// TestServiceWithoutEmbedderReportsDisabled covers the switched-off feature: a
// nil provider means the service reports disabled and never starts workers, so
// blobs simply stay pending.
func TestServiceWithoutEmbedderReportsDisabled(t *testing.T) {
	cat := newTestCatalog(t)
	svc := NewService(cat, nil, nil, nil)

	if svc.embedder != nil {
		t.Fatalf("embedder=%T want nil when embedding is off", svc.embedder)
	}
	if svc.Enabled() {
		t.Fatalf("Enabled()=true want=false when embedding is off")
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestVisionEmbedderLoadValidatesCheckpoint exercises the real checkpoint when
// one is available. It is skipped without VISION_REFERENCE_DIR, mirroring the
// vision package's reference test.
func TestVisionEmbedderLoadValidatesCheckpoint(t *testing.T) {
	dir := os.Getenv("VISION_REFERENCE_DIR")
	if dir == "" {
		t.Skip("VISION_REFERENCE_DIR is not set")
	}

	embedder := NewVisionEmbedder(vision.Config{ModelID: dir + "/model", Backend: "go"})
	if got := embedder.Describe(); got != "siglip2 model=<unloaded>" {
		t.Fatalf("Describe before load=%q", got)
	}
	if err := embedder.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = embedder.Close() })
	if got := embedder.Describe(); got == "siglip2 model=<unloaded>" {
		t.Fatalf("Describe after load=%q", got)
	}

	source, err := os.ReadFile(dir + "/refimg_1000x300.png")
	if err != nil {
		t.Fatalf("read source image: %v", err)
	}
	vector, err := embedder.Embed(context.Background(), source)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got, want := len(vector), 768; got != want {
		t.Fatalf("embedding length=%d want=%d", got, want)
	}

	// Loading twice is a no-op, and the model stays usable.
	if err := embedder.Load(context.Background()); err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if _, err := embedder.Embed(context.Background(), source); err != nil {
		t.Fatalf("Embed after second Load: %v", err)
	}
}

func TestVisionEmbedderRejectsInvalidImage(t *testing.T) {
	dir := os.Getenv("VISION_REFERENCE_DIR")
	if dir == "" {
		t.Skip("VISION_REFERENCE_DIR is not set")
	}
	embedder := NewVisionEmbedder(vision.Config{ModelID: dir + "/model", Backend: "go"})
	t.Cleanup(func() { _ = embedder.Close() })

	if _, err := embedder.Embed(context.Background(), []byte("not an image")); err == nil {
		t.Fatalf("expected an error for undecodable input")
	}
}

// TestServiceDisabledAfterModelLoadFailure checks the degraded path: a failed
// load must be remembered so the ingest pipeline keeps blobs pending instead of
// marking them failed.
func TestServiceDisabledAfterModelLoadFailure(t *testing.T) {
	cat := newTestCatalog(t)
	embedder := NewVisionEmbedder(vision.Config{
		ModelID: filepath.Join(t.TempDir(), "missing-model"),
		Backend: "go",
	})
	svc := NewService(cat, nil, embedder, nil)

	if err := svc.LoadModel(context.Background()); err == nil {
		t.Fatalf("LoadModel expected an error for a missing model directory")
	}
	if svc.Enabled() {
		t.Fatalf("Enabled()=true after a failed model load")
	}
	if _, err := svc.computeEmbedding(context.Background(), []byte("image")); err == nil {
		t.Fatalf("computeEmbedding expected an error after a failed model load")
	}
	// A second load reports the same failure rather than retrying or panicking.
	if err := svc.LoadModel(context.Background()); err == nil {
		t.Fatalf("second LoadModel expected an error")
	}
}

// TestServiceEmbedsAndPersistsWithRealModel drives the whole local embedding
// path: the service's embedder, its preprocessing, and the float32 round trip
// through SQLite. It is skipped without VISION_REFERENCE_DIR because it needs
// the real checkpoint.
func TestServiceEmbedsAndPersistsWithRealModel(t *testing.T) {
	dir := os.Getenv("VISION_REFERENCE_DIR")
	if dir == "" {
		t.Skip("VISION_REFERENCE_DIR is not set")
	}
	source, err := os.ReadFile(dir + "/refimg_1000x300.png")
	if err != nil {
		t.Fatalf("read source image: %v", err)
	}

	cat := newTestCatalog(t)
	ctx := context.Background()
	svc := NewService(cat, nil, NewVisionEmbedder(vision.Config{ModelID: dir + "/model", Backend: "go"}), nil)
	if !svc.Enabled() {
		t.Fatalf("Enabled()=false want=true")
	}
	t.Cleanup(func() { _ = svc.Close() })
	if err := svc.LoadModel(ctx); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}

	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: "hash-a", SizeBytes: 1}); err != nil {
		t.Fatalf("upsert blob: %v", err)
	}

	vector, err := svc.computeEmbedding(ctx, source)
	if err != nil {
		t.Fatalf("computeEmbedding: %v", err)
	}
	if got, want := len(vector), 768; got != want {
		t.Fatalf("embedding length=%d want=%d", got, want)
	}
	if err := cat.SetBlobEmbedding(ctx, "hash-a", catalog.EmbeddingStatusReady, vector, ""); err != nil {
		t.Fatalf("persist embedding: %v", err)
	}

	blob, err := cat.GetBlob(ctx, "hash-a")
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	if blob.EmbeddingStatus != catalog.EmbeddingStatusReady {
		t.Fatalf("EmbeddingStatus=%q want=%q", blob.EmbeddingStatus, catalog.EmbeddingStatusReady)
	}
	if len(blob.Embedding) != len(vector) {
		t.Fatalf("stored embedding length=%d want=%d", len(blob.Embedding), len(vector))
	}
	for i := range vector {
		if blob.Embedding[i] != vector[i] {
			t.Fatalf("stored embedding[%d]=%v want=%v", i, blob.Embedding[i], vector[i])
		}
	}

	// The service must now serve the persisted vector from the catalog, and
	// report it as ready progress.
	loaded, failed := svc.queryVector(ctx, "hash-a")
	if failed || len(loaded) == 0 {
		t.Fatalf("queryVector failed=%t len=%d", failed, len(loaded))
	}
	counts, err := cat.EmbeddingCounts(ctx)
	if err != nil {
		t.Fatalf("EmbeddingCounts: %v", err)
	}
	if counts.Ready != 1 || counts.Pending != 0 || counts.Failed != 0 {
		t.Fatalf("counts=%+v want 1 ready and nothing else", counts)
	}
}
