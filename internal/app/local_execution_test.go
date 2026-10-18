package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"image"
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
	"viewer/internal/runs"
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

func TestLocalEncodingBootRecoveryAndRegistrationWithoutWorkerToken(t *testing.T) {
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
	manager := newLocalRunManager(context.Background(), cat, encoder)
	defer manager.Close(context.Background())
	if _, _, err := manager.Create(context.Background(), "admin", "embedding", "not-loaded"); !errors.Is(err, runs.ErrUnavailable) {
		t.Fatalf("embedding registered before load: %v", err)
	}
	// An invalid source exercises the real local executor against the test store
	// without invoking cwebp, downloading a model, or accessing object storage.
	if err := cat.UpsertBlob(context.Background(), catalog.Blob{Hash: strings.Repeat("a", 64), SizeBytes: 0, ContentType: "image/png"}); err != nil {
		t.Fatal(err)
	}
	run, _, err := manager.Create(context.Background(), "admin", "encoding", "local")
	if err != nil || run.Executor != "local-cwebp/v1" || run.Counts.Total != 1 {
		t.Fatalf("local encoding unavailable: %+v %v", run, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := cat.JobRun(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "failed" {
			if got.Counts.Failed != 1 || got.LastError != "processing_failed" {
				t.Fatalf("real executor outcome=%+v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local executor never completed: %+v", got)
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

func TestEmbeddingWiringOnlyAfterSuccessfulLoadAndSharedLifetime(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "loaded", true: "unavailable"}[fail], func(t *testing.T) {
			cat := localTestCatalog(t)
			manager := runs.New(context.Background(), cat, nil)
			defer manager.Close(context.Background())
			provider := &localTestEmbedder{}
			if fail {
				provider.loadErr = errors.New("no checkpoint")
			}
			service := recommend.NewService(cat, nil, nil, provider, nil)
			if _, _, err := manager.Create(context.Background(), "admin", "embedding", "before-load"); !errors.Is(err, runs.ErrUnavailable) {
				t.Fatal(err)
			}
			// An Admin pause made during startup must survive registration.
			if _, err := manager.SetAutomaticEmbedding(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			loadErr := service.LoadModel(context.Background())
			err := registerEmbedding(manager, service)
			if fail {
				if loadErr == nil || !errors.Is(err, runs.ErrUnavailable) {
					t.Fatalf("failed model registered: %v %v", loadErr, err)
				}
				if _, _, err := manager.Create(context.Background(), "admin", "embedding", "after-load"); !errors.Is(err, runs.ErrUnavailable) {
					t.Fatal(err)
				}
				page, err := manager.List(context.Background(), 10, "")
				if err != nil || len(page.Runs) != 0 {
					t.Fatalf("unavailable created run: %+v %v", page, err)
				}
			} else {
				if loadErr != nil || err != nil {
					t.Fatalf("register=%v %v", loadErr, err)
				}
				status, err := manager.AutomaticEmbedding()
				if err != nil || status.Enabled || status.Running {
					t.Fatalf("startup pause lost: %+v %v", status, err)
				}
				run, _, err := manager.Create(context.Background(), "admin", "embedding", "after-load")
				if err != nil || run.Executor != "local-siglip2/v1" {
					t.Fatalf("loaded executor unavailable: %+v %v", run, err)
				}
				if _, err := manager.SetAutomaticEmbedding(context.Background(), true); err != nil {
					t.Fatal(err)
				}
				if _, err := manager.SetAutomaticEmbedding(context.Background(), false); err != nil {
					t.Fatal(err)
				}
			}
			if err := manager.Close(context.Background()); err != nil {
				t.Fatal(err)
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

type lifecycleEncodingStore struct {
	localTestStore
	source      []byte
	processing  chan context.Context
	release     chan struct{}
	processingN atomic.Int32
	puts        atomic.Int32
}

func (s *lifecycleEncodingStore) StatObject(ctx context.Context, key string) (storage.Object, bool, error) {
	if s.processingN.Add(1) == 1 {
		s.processing <- ctx
	}
	select {
	case <-ctx.Done():
		return storage.Object{}, false, ctx.Err()
	case <-s.release:
	}
	return storage.Object{Key: key, Size: int64(len(s.source)), ETag: "original"}, true, nil
}

func (s *lifecycleEncodingStore) GetObject(context.Context, string) (io.ReadCloser, string, error) {
	return io.NopCloser(bytes.NewReader(s.source)), "image/png", nil
}

func (s *lifecycleEncodingStore) PutObject(context.Context, string, io.Reader, string) error {
	s.puts.Add(1)
	return nil
}

type lifecycleExecutor func(context.Context, catalog.JobRun, catalog.JobRunItem) (runs.Outcome, error)

func (f lifecycleExecutor) Execute(ctx context.Context, run catalog.JobRun, item catalog.JobRunItem) (runs.Outcome, error) {
	return f(ctx, run, item)
}

func TestEncodingShutdownKeepsAcceptedOutputProcessorsAlive(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled", true: "completion-timeout-recovery"}[timeout], func(t *testing.T) {
			cat := localTestCatalog(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var source bytes.Buffer
			if err := png.Encode(&source, image.NewNRGBA(image.Rect(0, 0, 1, 1))); err != nil {
				t.Fatal(err)
			}
			// Padding makes the original strictly larger than the 1x1 WebP.
			source.Write(make([]byte, 200))
			encoded, err := base64.StdEncoding.DecodeString("UklGRkAAAABXRUJQVlA4IDQAAADwAQCdASoBAAEAAQAcJaACdLoB+AAETAAA/vW4f/6aR40jxpHxcP/ugT90CfugT/3NoAAA")
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(source.Bytes())
			hash := hex.EncodeToString(sum[:])
			if err := cat.UpsertBlob(ctx, catalog.Blob{Hash: hash, SizeBytes: int64(source.Len()), ContentType: "image/png"}); err != nil {
				t.Fatal(err)
			}
			store := &lifecycleEncodingStore{source: source.Bytes(), processing: make(chan context.Context, 1), release: make(chan struct{})}
			state := t.TempDir()
			encoder, err := startLocalEncoding(ctx, cat, store, state)
			if err != nil {
				t.Fatal(err)
			}
			defer encoder.Close()
			// A lifecycle seam replaces only cwebp and its 21-minute completion
			// wait. The real manager, accepted handoff, and processors run below.
			runCancelled := make(chan struct{})
			executor := lifecycleExecutor(func(work context.Context, _ catalog.JobRun, item catalog.JobRunItem) (runs.Outcome, error) {
				if _, _, err := cat.ClaimLocalEncoding(work, item.Hash, item.ClaimToken, time.Now().Add(time.Minute)); err != nil {
					return runs.Outcome{}, err
				}
				if err := encoder.ReceiveOutput(work, item.Hash, item.ClaimToken, bytes.NewReader(encoded)); err != nil {
					return runs.Outcome{}, err
				}
				<-work.Done()
				close(runCancelled)
				completion, stop := context.WithTimeout(context.WithoutCancel(work), 2*time.Second)
				defer stop()
				if timeout {
					// Deterministically expire the injected completion wait while
					// output is still received, without waiting for production TTLs.
					return runs.Outcome{Status: "failed", ErrorCode: "executor_failed"}, nil
				}
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					job, err := cat.GetEncodingJob(completion, hash)
					if err == nil && job.Status == "done" {
						return runs.Outcome{Status: "succeeded"}, nil
					}
					select {
					case <-completion.Done():
						return runs.Outcome{}, completion.Err()
					case <-ticker.C:
					}
				}
			})
			manager := runs.New(ctx, cat, map[string]runs.Registration{"encoding": {Name: "lifecycle-test", Executor: executor}})
			defer manager.Close(context.Background())
			run, _, err := manager.Create(ctx, "admin", "encoding", "shutdown")
			if err != nil {
				t.Fatal(err)
			}
			var processorCtx context.Context
			select {
			case processorCtx = <-store.processing:
			case <-time.After(5 * time.Second):
				t.Fatal("accepted output never reached its processor")
			}
			cancel()
			closed := make(chan error, 1)
			go func() { closed <- manager.Close(context.Background()) }()
			select {
			case <-runCancelled:
			case <-time.After(5 * time.Second):
				t.Fatal("root shutdown did not cancel local requests")
			}
			if processorCtx.Err() != nil {
				t.Fatalf("root shutdown stopped accepted output processing: %v", processorCtx.Err())
			}
			if !timeout {
				select {
				case err := <-closed:
					t.Fatalf("manager abandoned blocked accepted output: %v", err)
				default:
				}
				close(store.release)
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("manager shutdown stalled")
			}
			encoder.Close()
			if processorCtx.Err() == nil {
				t.Fatal("encoding Close did not stop/join its processors")
			}
			if timeout {
				job, err := cat.GetEncodingJob(context.Background(), hash)
				if err != nil || job.Status != "received" {
					t.Fatalf("forced shutdown lost accepted handoff: %+v %v", job, err)
				}
				close(store.release)
				restarted, err := startLocalEncoding(context.Background(), cat, store, state)
				if err != nil {
					t.Fatal(err)
				}
				defer restarted.Close()
				deadline := time.Now().Add(5 * time.Second)
				for {
					job, err := cat.GetEncodingJob(context.Background(), hash)
					if err == nil && job.Status == "done" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("accepted output did not recover: %+v %v", job, err)
					}
					time.Sleep(time.Millisecond)
				}
			} else {
				page, err := cat.JobRunItems(context.Background(), run.ID, 10, "")
				if err != nil || len(page.Items) != 1 || page.Items[0].Status != "succeeded" {
					t.Fatalf("shutdown lost settled run item: %+v %v", page, err)
				}
			}
			if store.puts.Load() != 1 {
				t.Fatalf("accepted output writes=%d want=1", store.puts.Load())
			}
		})
	}
}
