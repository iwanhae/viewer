package app

import (
	"context"
	"errors"
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
	// An invalid source exercises the real local executor without invoking
	// cwebp, downloading a model, or making an object-storage request.
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
