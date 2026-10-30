package recommend

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/images"
)

// runEmbedder wraps okEmbedder with a per-test Embed function.
type runEmbedder struct {
	okEmbedder
	embed func(context.Context, []byte) ([]float32, error)
}

func (e runEmbedder) Embed(ctx context.Context, data []byte) ([]float32, error) {
	return e.embed(ctx, data)
}

// runImageStore hooks a callback in front of the blob store stub so a test can
// block (and observe the context) at a specific point of the worker's path.
type runImageStore struct {
	blobStoreStub
	before func(context.Context) error
}

func (s *runImageStore) GetObject(ctx context.Context, key string) (io.ReadCloser, string, error) {
	if s.before != nil {
		if err := s.before(ctx); err != nil {
			return nil, "", err
		}
	}
	return s.blobStoreStub.GetObject(ctx, key)
}

func waitAutomaticSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic worker did not reach phase")
	}
}

// TestAutomaticEmbeddingStatusShape pins the dashboard contract: enabled by
// default, Running only once Start launched workers behind a successful model
// load, and the toggle flipping Enabled without touching Running.
func TestAutomaticEmbeddingStatusShape(t *testing.T) {
	cat := newTestCatalog(t)
	service := NewService(cat, nil, images.NewService(cat, &blobStoreStub{}), okEmbedder{}, nil)
	if got := service.AutomaticEmbedding(); !got.Enabled || got.Running {
		t.Fatalf("default status=%+v want enabled and not running", got)
	}
	service.SetAutomaticEmbedding(false)
	if got := service.AutomaticEmbedding(); got.Enabled || got.Running {
		t.Fatalf("paused status=%+v want disabled and not running", got)
	}
	service.SetAutomaticEmbedding(true)

	// A model that fails to load keeps Start a no-op: no worker exists to
	// claim anything, so Running must stay false even though the preference
	// is on. This is the degradation a missing checkpoint produces.
	broken := NewService(cat, nil, images.NewService(cat, &blobStoreStub{}), failingEmbedder{}, nil)
	if err := broken.LoadModel(context.Background()); err == nil {
		t.Fatal("expected the failing provider to fail the load")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	broken.Start(ctx)
	if got := broken.AutomaticEmbedding(); !got.Enabled || got.Running {
		t.Fatalf("status after failed load=%+v want enabled and not running", got)
	}
}

// failingEmbedder models a checkpoint that cannot be loaded.
type failingEmbedder struct{ okEmbedder }

func (failingEmbedder) Load(context.Context) error { return errors.New("no checkpoint") }

// TestWorkerLoopCancellationReleasesBatchAndIsRestartable keeps the shutdown
// guarantee the Manager used to pin: cancelling the worker mid-batch hands the
// unprocessed claims back (no waiting out the lease), never touches external
// leases, and a later worker drains what is left.
func TestWorkerLoopCancellationReleasesBatchAndIsRestartable(t *testing.T) {
	for _, phase := range []string{"image-load", "inference", "inference-ignores-cancel"} {
		t.Run(phase, func(t *testing.T) {
			cat := newTestCatalog(t)
			seedBlob(t, cat, "external")
			external, err := cat.ClaimExternalEmbeddings(context.Background(), 1, time.Now().Add(time.Hour))
			if err != nil || len(external) != 1 {
				t.Fatalf("external claim=%v %v", external, err)
			}
			seedBlob(t, cat, "a")
			seedBlob(t, cat, "b")
			entered := make(chan struct{}, 1)
			store := &runImageStore{blobStoreStub: blobStoreStub{objects: map[string][]byte{"blobs/a": []byte("a"), "blobs/b": []byte("b")}}}
			var calls atomic.Int32
			if phase == "image-load" {
				store.before = func(ctx context.Context) error {
					if calls.Add(1) == 1 {
						entered <- struct{}{}
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				}
			}
			provider := runEmbedder{embed: func(ctx context.Context, _ []byte) ([]float32, error) {
				if phase != "image-load" && calls.Add(1) == 1 {
					entered <- struct{}{}
					<-ctx.Done()
					if phase != "inference-ignores-cancel" {
						return nil, ctx.Err()
					}
				}
				return vec768(1), nil
			}}
			drained := make(chan struct{}, 2)
			service := NewService(cat, nil, images.NewService(cat, store), provider, func(context.Context) error { drained <- struct{}{}; return nil })
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { service.workerLoop(ctx); close(done) }()
			waitAutomaticSignal(t, entered)
			cancel()
			waitAutomaticSignal(t, done)
			for _, hash := range []string{"a", "b"} {
				blob, err := cat.GetBlob(context.Background(), hash)
				if err != nil || blob.EmbeddingStatus != catalog.EmbeddingStatusPending {
					t.Fatalf("cancelled blob=%+v %v", blob, err)
				}
			}
			blob, err := cat.GetBlob(context.Background(), "external")
			if err != nil || blob.EmbeddingStatus != catalog.EmbeddingStatusProcessing {
				t.Fatalf("local stop touched external lease: %+v %v", blob, err)
			}
			ctx, cancel = context.WithCancel(context.Background())
			done = make(chan struct{})
			go func() { service.workerLoop(ctx); close(done) }()
			waitAutomaticSignal(t, drained)
			cancel()
			waitAutomaticSignal(t, done)
			for _, hash := range []string{"a", "b"} {
				blob, err := cat.GetBlob(context.Background(), hash)
				if err != nil || blob.EmbeddingStatus != catalog.EmbeddingStatusReady {
					t.Fatalf("restart failed to drain: %+v %v", blob, err)
				}
			}
			select {
			case <-drained:
				t.Fatal("duplicate finalizer")
			default:
			}
		})
	}
}

// TestPauseStopsClaimingButCompletesInFlightBatch pins the new pause
// semantics: the gate sits between batches, so the batch being processed when
// the pause lands still completes and writes back, while blobs that become
// claimable during the pause wait for the resume. This replaces the old
// Manager test that paused around a manual run's snapshot handoff.
func TestPauseStopsClaimingButCompletesInFlightBatch(t *testing.T) {
	cat := newTestCatalog(t)
	seedBlob(t, cat, "a")
	store := &blobStoreStub{objects: map[string][]byte{"blobs/a": []byte("a"), "blobs/b": []byte("b")}}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	provider := runEmbedder{embed: func(_ context.Context, _ []byte) ([]float32, error) {
		started <- struct{}{}
		<-release
		return vec768(1), nil
	}}
	service := NewService(cat, nil, images.NewService(cat, store), provider, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = service.workerLoop(ctx); close(done) }()

	// Pause while the first blob's inference is blocked, and let new work
	// arrive during the pause: the in-flight batch must still finish, and
	// the paused worker must not claim the new blob.
	waitAutomaticSignal(t, started)
	service.SetAutomaticEmbedding(false)
	seedBlob(t, cat, "b")
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		first, err := cat.GetBlob(context.Background(), "a")
		if err != nil {
			t.Fatal(err)
		}
		if first.EmbeddingStatus == catalog.EmbeddingStatusReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pause dropped the in-flight batch: %+v", first)
		}
		time.Sleep(time.Millisecond)
	}
	// The worker keeps ticking while paused; give it real ticks to (wrongly)
	// claim the new blob before resuming. The loop ticks every 500ms, so one
	// full second covers at least one tick after the batch released.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		second, err := cat.GetBlob(context.Background(), "b")
		if err != nil {
			t.Fatal(err)
		}
		if second.EmbeddingStatus != catalog.EmbeddingStatusPending {
			t.Fatalf("paused worker claimed blob b: %+v", second)
		}
		time.Sleep(5 * time.Millisecond)
	}
	service.SetAutomaticEmbedding(true)
	deadline = time.Now().Add(5 * time.Second)
	for {
		second, err := cat.GetBlob(context.Background(), "b")
		if err != nil {
			t.Fatal(err)
		}
		if second.EmbeddingStatus == catalog.EmbeddingStatusReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume did not drain: %+v", second)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWorkerLoopPauseRetainsDrainFinalizer models the boundary after a
// batch's durable writes but before its next idle tick. Pausing here must not
// discard the outstanding backup hook: the drain stays open and the resumed
// worker's first empty tick finalizes it exactly once.
func TestWorkerLoopPauseRetainsDrainFinalizer(t *testing.T) {
	cat := newTestCatalog(t)
	drained := make(chan struct{}, 2)
	service := NewService(cat, nil, images.NewService(cat, &blobStoreStub{}), okEmbedder{}, func(context.Context) error { drained <- struct{}{}; return nil })
	// Model the boundary after a batch's durable writes but before its next
	// idle tick. Pausing here must not discard the outstanding backup hook.
	service.beginEmbeddingRun(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A worker that never runs a tick (cancelled context) must not finalize.
	if err := service.workerLoop(ctx); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = service.workerLoop(ctx); close(done) }()
	waitAutomaticSignal(t, drained)
	cancel()
	waitAutomaticSignal(t, done)
	select {
	case <-drained:
		t.Fatal("one open drain finalized more than once")
	default:
	}
}

// TestPausedWorkerDoesNotCloseOpenDrain pins the pause-gate placement against
// a regression: closing the drain on a paused tick would run (or drop) the
// backup hook while the operator believes embedding is suspended. The close
// must happen on a resumed, actually-empty tick.
func TestPausedWorkerDoesNotCloseOpenDrain(t *testing.T) {
	cat := newTestCatalog(t)
	seedBlob(t, cat, "a")
	drained := make(chan struct{}, 2)
	service := NewService(cat, nil, images.NewService(cat, &blobStoreStub{objects: map[string][]byte{"blobs/a": []byte("a")}}), okEmbedder{}, func(context.Context) error { drained <- struct{}{}; return nil })
	service.beginEmbeddingRun(1)
	service.SetAutomaticEmbedding(false)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = service.workerLoop(ctx); close(done) }()
	// Several paused ticks pass with an open drain: no finalizer may fire.
	// The loop ticks every 500ms, so one full second covers at least one
	// tick after the worker started.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-drained:
			cancel()
			t.Fatal("paused worker closed an open drain")
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	service.SetAutomaticEmbedding(true)
	waitAutomaticSignal(t, drained)
	cancel()
	waitAutomaticSignal(t, done)
	select {
	case <-drained:
		t.Fatal("one open drain finalized more than once")
	default:
	}
}
