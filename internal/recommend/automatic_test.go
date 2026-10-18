package recommend

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/images"
	"viewer/internal/runs"
)

func waitAutomaticSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("automatic worker did not reach phase")
	}
}

func TestRunAutomaticCancellationReleasesBatchAndIsRestartable(t *testing.T) {
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
			go func() { service.RunAutomatic(ctx); close(done) }()
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
			go func() { service.RunAutomatic(ctx); close(done) }()
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

func TestManagedAutomaticYieldsToRealEmbeddingExecutor(t *testing.T) {
	cat := newTestCatalog(t)
	seedBlob(t, cat, "a")
	seedBlob(t, cat, "b")
	store := &blobStoreStub{objects: map[string][]byte{"blobs/a": []byte("a"), "blobs/b": []byte("b")}}
	started, finish := make(chan struct{}, 2), make(chan struct{})
	var calls, active, maxActive atomic.Int32
	provider := runEmbedder{embed: func(ctx context.Context, _ []byte) ([]float32, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old; old = maxActive.Load() {
			if maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		call := calls.Add(1)
		if call <= 2 {
			started <- struct{}{}
		}
		if call == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if call == 2 {
			select {
			case <-finish:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return vec768(1), nil
	}}
	service := NewService(cat, nil, images.NewService(cat, store), provider, nil)
	manager := runs.New(context.Background(), cat, map[string]runs.Registration{"embedding": {Name: "local/v1", Executor: NewEmbeddingExecutor(service)}})
	defer manager.Close(context.Background())
	if err := manager.RegisterAutomaticEmbedding(service.RunAutomatic); err != nil {
		t.Fatal(err)
	}
	waitAutomaticSignal(t, started)
	run, _, err := manager.Create(context.Background(), "admin", "embedding", "manual")
	if err != nil || run.Counts.Total != 2 {
		t.Fatalf("manual snapshot lost automatic batch: %+v %v", run, err)
	}
	waitAutomaticSignal(t, started)
	if _, err := manager.SetAutomaticEmbedding(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	close(finish)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := cat.JobRun(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == "completed" {
			if r.Counts.Succeeded != 2 || r.Counts.Busy != 0 {
				t.Fatalf("manual=%+v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manual never completed: %+v", r)
		}
		time.Sleep(time.Millisecond)
	}
	if maxActive.Load() != 1 || calls.Load() != 3 {
		t.Fatalf("inference overlap/repetition: max=%d calls=%d", maxActive.Load(), calls.Load())
	}
	status, err := manager.AutomaticEmbedding()
	if err != nil || status.Enabled || status.Running {
		t.Fatalf("manual ignored pause: %+v %v", status, err)
	}
}

func TestRunAutomaticPauseRetainsDrainFinalizer(t *testing.T) {
	cat := newTestCatalog(t)
	drained := make(chan struct{}, 2)
	service := NewService(cat, nil, images.NewService(cat, &blobStoreStub{}), okEmbedder{}, func(context.Context) error { drained <- struct{}{}; return nil })
	// Model the boundary after a batch's durable writes but before its next
	// idle tick. Pausing here must not discard the outstanding backup hook.
	service.beginEmbeddingRun(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := service.RunAutomatic(ctx); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = service.RunAutomatic(ctx); close(done) }()
	waitAutomaticSignal(t, drained)
	cancel()
	waitAutomaticSignal(t, done)
	select {
	case <-drained:
		t.Fatal("one open drain finalized more than once")
	default:
	}
}
