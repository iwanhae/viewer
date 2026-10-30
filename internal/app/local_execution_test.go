package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/encoding"
	"viewer/internal/recommend"
	"viewer/internal/storage"
)

type localTestStore struct {
	encoding.Store
	lists atomic.Int32
	err   error
}

func (s *localTestStore) ListObjects(context.Context, string) ([]storage.Object, error) {
	s.lists.Add(1)
	return nil, s.err
}
func (*localTestStore) DeleteObjects(context.Context, []string) error { return nil }
func (*localTestStore) GetObject(context.Context, string) (io.ReadCloser, string, error) {
	return io.NopCloser(strings.NewReader("invalid source")), "image/png", nil
}

func localTestCatalog(t *testing.T) *catalog.Store {
	t.Helper()
	cat, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	return cat
}

// TestLocalEncodingBootRecoveryAndDrainWithoutWorkerToken pins the boot-time
// half of the wiring: the encoding service (and its spool recovery) starts
// without WORKER_TOKEN, and the local drain runs the real per-blob pipeline
// against the test store without cwebp, a model, or object storage. The
// invalid source makes every target fail terminally, which is the observable
// "drain really ran" signal.
func TestLocalEncodingBootRecoveryAndDrainWithoutWorkerToken(t *testing.T) {
	t.Setenv("WORKER_TOKEN", "")
	cat := localTestCatalog(t)
	state := t.TempDir()
	spool := filepath.Join(state, "encoding-spool")
	if err := os.Mkdir(spool, 0700); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(spool, "encoding-interrupted.part")
	if err := os.WriteFile(partial, []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &localTestStore{}
	encoder, err := startLocalEncoding(context.Background(), cat, store, state)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("startup did not recover spool: %v", err)
	}
	if store.lists.Load() != 1 {
		t.Fatalf("startup did not run output cleanup: %d", store.lists.Load())
	}
	// The local drain needs a cwebp binary on PATH; a harmless fake satisfies
	// the fail-fast check, and the invalid source fails before any invocation.
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "cwebp"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := cat.UpsertBlob(context.Background(), catalog.Blob{Hash: strings.Repeat("a", 64), SizeBytes: 0, ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	started, err := encoder.StartLocalDrain()
	if err != nil || !started {
		t.Fatalf("local encoding unavailable: started=%v err=%v", started, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := cat.GetEncodingJob(context.Background(), strings.Repeat("a", 64))
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local drain never completed: %+v", job)
		}
		time.Sleep(time.Millisecond)
	}
	// A second trigger after the drain finished is accepted again (the
	// single-flight guard only covers a drain that is still running).
	deadline = time.Now().Add(5 * time.Second)
	for {
		started, err := encoder.StartLocalDrain()
		if err != nil {
			t.Fatal(err)
		}
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain never became idle again")
		}
		time.Sleep(time.Millisecond)
	}
}

type localTestEmbedder struct {
	loadErr error
	closed  atomic.Int32
}

func (e *localTestEmbedder) Load(context.Context) error { return e.loadErr }
func (*localTestEmbedder) Embed(context.Context, []byte) ([]float32, error) {
	return nil, errors.New("unexpected inference")
}
func (e *localTestEmbedder) Close() error { e.closed.Add(1); return nil }

// TestEmbeddingWorkersStartOnlyAfterSuccessfulLoad pins the app wiring that
// replaced the run-manager registration: the model-load goroutine's success
// path is what starts the workers, so a failed load leaves Running false (no
// worker exists to claim anything), a successful one flips it true, and the
// operator's pause switch survives across both. The shared model is closed
// exactly once, by the application's own defer — never by the worker control.
func TestEmbeddingWorkersStartOnlyAfterSuccessfulLoadAndSharedLifetime(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "loaded", true: "unavailable"}[fail], func(t *testing.T) {
			cat := localTestCatalog(t)
			provider := &localTestEmbedder{}
			if fail {
				provider.loadErr = errors.New("no checkpoint")
			}
			service := recommend.NewService(cat, nil, nil, provider, nil)
			if status := service.AutomaticEmbedding(); !status.Enabled || status.Running {
				t.Fatalf("default status=%+v want enabled and not running", status)
			}
			// An Admin pause made during startup must survive the later
			// Start — the switch is a preference, not a worker handle.
			service.SetAutomaticEmbedding(false)

			loadErr := service.LoadModel(context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service.Start(ctx)
			if fail {
				if loadErr == nil {
					t.Fatalf("failed model reported a successful load")
				}
				status := service.AutomaticEmbedding()
				if status.Enabled || status.Running {
					t.Fatalf("failed model started workers: %+v", status)
				}
			} else {
				if loadErr != nil {
					t.Fatalf("load=%v", loadErr)
				}
				status := service.AutomaticEmbedding()
				// The workers exist even while paused — the switch gates
				// claiming, not the loop — and the startup pause survives.
				if !status.Running {
					t.Fatalf("loaded model did not start workers: %+v", status)
				}
				if status.Enabled {
					t.Fatalf("startup pause lost: %+v", status)
				}
			}
			// The toggle keeps working after Start, in both directions.
			service.SetAutomaticEmbedding(true)
			if status := service.AutomaticEmbedding(); !status.Enabled {
				t.Fatalf("resume lost: %+v", status)
			}
			service.SetAutomaticEmbedding(false)
			if status := service.AutomaticEmbedding(); status.Enabled {
				t.Fatalf("pause lost: %+v", status)
			}
			if provider.closed.Load() != 0 {
				t.Fatal("worker control/shutdown closed the shared model")
			}
			if err := service.Close(); err != nil || provider.closed.Load() != 1 {
				t.Fatalf("application model close=%v count=%d", err, provider.closed.Load())
			}
		})
	}
}

func TestLocalEncodingBootRecoveryFailureIsActionable(t *testing.T) {
	cat := localTestCatalog(t)
	_, err := startLocalEncoding(context.Background(), cat, &localTestStore{err: errors.New("storage unavailable")}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "start WebP encoding") {
		t.Fatalf("startup recovery error=%v", err)
	}
}

// TestEncodingServiceCloseJoinsDrainAndKeepsAcceptedOutputAlive pins the
// shutdown ordering the run manager used to own: cancelling the root context
// does not stop accepted-output processors (they run on the service
// lifetime), and Close stops the drain between blobs before the processors
// exit. The real service, accepted handoff, and processors run below; only
// cwebp is replaced by a seam.
func TestEncodingServiceCloseJoinsDrainAndKeepsAcceptedOutputAlive(t *testing.T) {
	cat := localTestCatalog(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A valid PNG source padded well past the tiny WebP, so the encoded result
	// is strictly smaller and the commit path runs for real.
	source := sourcePNGWithPadding(t, 200)
	sum := sha256.Sum256(source)
	hash := hex.EncodeToString(sum[:])
	if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(len(source)), ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	store := &lifecycleEncodingStore{source: source, processing: make(chan context.Context, 1)}
	state := t.TempDir()
	encoder, err := startLocalEncoding(ctx, cat, store, state)
	if err != nil {
		t.Fatal(err)
	}
	// The drain needs a cwebp binary on PATH. A script that cats the WebP
	// fixture exercises the REAL command runner (deadline, stdout capture)
	// without needing libwebp installed; the runCommand seam stays package-private.
	fakeBin := t.TempDir()
	fixture := filepath.Join(fakeBin, "fixture.webp")
	if err := os.WriteFile(fixture, tinyWebPFixture, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "cwebp"), []byte("#!/bin/sh\ncat '"+fixture+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	started, err := encoder.StartLocalDrain()
	if err != nil || !started {
		t.Fatalf("drain=%v err=%v", started, err)
	}
	var processorCtx context.Context
	select {
	case processorCtx = <-store.processing:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted output never reached its processor")
	}
	// Root shutdown alone must not stop the accepted output's processor: the
	// encoding service deliberately runs on a lifetime independent of it.
	cancel()
	if processorCtx.Err() != nil {
		t.Fatalf("root shutdown stopped accepted output processing: %v", processorCtx.Err())
	}
	// Close stops the drain between blobs and joins it, then stops the
	// processors; the accepted handoff is already durable, so the job lands
	// in the terminal state either way.
	closed := make(chan struct{})
	go func() { encoder.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("encoding Close stalled on the drain")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := cat.GetEncodingJob(context.Background(), hash)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("accepted output did not finish after shutdown: %+v", job)
		}
		time.Sleep(time.Millisecond)
	}
	if store.puts.Load() != 1 {
		t.Fatalf("accepted output writes=%d want=1", store.puts.Load())
	}
}

type lifecycleEncodingStore struct {
	encoding.Store
	source     []byte
	processing chan context.Context
	puts       atomic.Int32
}

func (s *lifecycleEncodingStore) ListObjects(context.Context, string) ([]storage.Object, error) {
	return nil, nil
}

func (*lifecycleEncodingStore) DeleteObjects(context.Context, []string) error { return nil }

func (s *lifecycleEncodingStore) StatObject(ctx context.Context, key string) (storage.Object, bool, error) {
	// Hand the processor's context to the test exactly once so it can assert
	// the shutdown ordering against it.
	select {
	case s.processing <- ctx:
	default:
	}
	return storage.Object{Key: key, Size: int64(len(s.source)), ETag: "original"}, true, nil
}

func (s *lifecycleEncodingStore) GetObject(context.Context, string) (io.ReadCloser, string, error) {
	return io.NopCloser(bytes.NewReader(s.source)), "image/png", nil
}

func (s *lifecycleEncodingStore) PutObject(_ context.Context, _ string, _ io.Reader, _ string) error {
	s.puts.Add(1)
	return nil
}

// writeTinyWebP fixture: the same valid 1x1 WebP bytes the encoding package's
// own tests use, emitted by the fake cwebp script the PATH installs.
var tinyWebPFixture = mustDecodeWebP("UklGRkAAAABXRUJQVlA4IDQAAADwAQCdASoBAAEAAQAcJaACdLoB+AAETAAA/vW4f/6aR40jxpHxcP/ugT90CfugT/3NoAAA")

func mustDecodeWebP(encoded string) []byte {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic(err)
	}
	return data
}

// sourcePNGWithPadding encodes a 1x1 PNG and appends enough ancillary bytes
// that the tiny WebP fixture is strictly smaller than it.
func sourcePNGWithPadding(t *testing.T, padding int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return append(buf.Bytes(), bytes.Repeat([]byte{0}, padding)...)
}
