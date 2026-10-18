package runs

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"viewer/internal/catalog"
)

type executorFunc func(context.Context, catalog.JobRun, catalog.JobRunItem) (Outcome, error)

func (f executorFunc) Execute(ctx context.Context, r catalog.JobRun, i catalog.JobRunItem) (Outcome, error) {
	return f(ctx, r, i)
}

func testCatalog(t *testing.T, hashes ...string) *catalog.Store {
	t.Helper()
	s, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, hash := range hashes {
		if err := s.UpsertBlob(context.Background(), catalog.Blob{Hash: hash, SizeBytes: 10, ContentType: "image/jpeg"}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}
func closeManager(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func waitRun(t *testing.T, s *catalog.Store, id, status string) catalog.JobRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.JobRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status == status {
			return r
		}
		time.Sleep(time.Millisecond)
	}
	r, _ := s.JobRun(context.Background(), id)
	t.Fatalf("run never became %s: %+v", status, r)
	return r
}
func awaitItem(t *testing.T, ch <-chan catalog.JobRunItem) catalog.JobRunItem {
	t.Helper()
	select {
	case i := <-ch:
		return i
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not start")
		return catalog.JobRunItem{}
	}
}

func TestManagerUnavailableDoesNotCreateRun(t *testing.T) {
	s := testCatalog(t, "a")
	m := New(context.Background(), s, nil)
	defer closeManager(t, m)
	if _, _, err := m.Create(context.Background(), "actor", "embedding", "key"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing executor=%v", err)
	}
	page, err := m.List(context.Background(), 10, "")
	if err != nil || len(page.Runs) != 0 {
		t.Fatalf("unavailable created durable work: %+v %v", page, err)
	}
}

func TestManagerServiceContextFiniteSnapshotAndCancellation(t *testing.T) {
	s := testCatalog(t, "a", "b", "c")
	started := make(chan catalog.JobRunItem, 3)
	release := make(chan struct{})
	var calls atomic.Int32
	executor := executorFunc(func(ctx context.Context, r catalog.JobRun, i catalog.JobRunItem) (Outcome, error) {
		calls.Add(1)
		started <- i
		if i.Hash == "a" {
			select {
			case <-release:
				return Outcome{Status: "succeeded"}, nil
			case <-ctx.Done():
				return Outcome{}, ctx.Err()
			}
		}
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	})
	m := New(context.Background(), s, map[string]Registration{"embedding": {Name: "test/v1", Executor: executor}})
	defer closeManager(t, m)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	r, repeated, err := m.Create(requestCtx, "actor", "embedding", "key")
	if err != nil || repeated {
		t.Fatalf("create=%+v %v %v", r, repeated, err)
	}
	first := awaitItem(t, started)
	if first.Hash != "a" || first.ClaimToken == "" {
		t.Fatalf("first=%+v", first)
	}
	cancelRequest()
	if err := s.UpsertBlob(context.Background(), catalog.Blob{Hash: "later", SizeBytes: 10, ContentType: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if second := awaitItem(t, started); second.Hash != "b" {
		t.Fatalf("request cancellation stopped service work: %+v", second)
	}
	if replay, repeated, err := m.Create(context.Background(), "actor", "embedding", "key"); err != nil || !repeated || replay.ID != r.ID {
		t.Fatalf("replay=%+v %v %v", replay, repeated, err)
	}
	if _, err := m.Cancel(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Cancel(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	final := waitRun(t, s, r.ID, "cancelled")
	if final.Counts.Total != 3 || final.Counts.Succeeded != 1 || final.Counts.Cancelled != 2 || calls.Load() != 2 || !final.CancelRequested {
		t.Fatalf("final=%+v calls=%d", final, calls.Load())
	}
	// A replay after an executor is removed is still the original submission.
	unavailable := New(context.Background(), s, nil)
	defer closeManager(t, unavailable)
	if replay, repeated, err := unavailable.Create(context.Background(), "actor", "embedding", "key"); err != nil || !repeated || replay.ID != r.ID {
		t.Fatalf("unavailable replay=%+v %v %v", replay, repeated, err)
	}
}

func TestManagerStagesRunSimultaneously(t *testing.T) {
	s := testCatalog(t, "a")
	started := make(chan catalog.JobRunItem, 2)
	executor := executorFunc(func(ctx context.Context, _ catalog.JobRun, i catalog.JobRunItem) (Outcome, error) {
		started <- i
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	})
	m := New(context.Background(), s, map[string]Registration{"embedding": {Name: "embed/v1", Executor: executor}, "encoding": {Name: "encode/v1", Executor: executor}})
	defer closeManager(t, m)
	for _, stage := range []string{"embedding", "encoding"} {
		if _, _, err := m.Create(context.Background(), "actor", stage, stage); err != nil {
			t.Fatal(err)
		}
		awaitItem(t, started)
		if _, _, err := m.Create(context.Background(), "another", stage, "conflict"+stage); !errors.Is(err, catalog.ErrJobRunConflict) {
			t.Fatalf("stage exclusivity=%v", err)
		}
	}
}

func TestManagerCancellationPreservesInFlightSuccess(t *testing.T) {
	s := testCatalog(t, "a", "b")
	started := make(chan catalog.JobRunItem, 1)
	executor := executorFunc(func(ctx context.Context, _ catalog.JobRun, i catalog.JobRunItem) (Outcome, error) {
		started <- i
		<-ctx.Done()
		return Outcome{Status: "succeeded"}, nil
	})
	m := New(context.Background(), s, map[string]Registration{"encoding": {Name: "test/v1", Executor: executor}})
	defer closeManager(t, m)
	r, _, err := m.Create(context.Background(), "actor", "encoding", "key")
	if err != nil {
		t.Fatal(err)
	}
	awaitItem(t, started)
	if _, err := m.Cancel(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	final := waitRun(t, s, r.ID, "cancelled")
	if final.Counts.Succeeded != 1 || final.Counts.Cancelled != 1 {
		t.Fatalf("cancel rolled back success: %+v", final)
	}
}

func TestManagerShutdownInterruptsAndDoesNotResume(t *testing.T) {
	s := testCatalog(t, "a", "b")
	started := make(chan catalog.JobRunItem, 1)
	executor := executorFunc(func(ctx context.Context, _ catalog.JobRun, i catalog.JobRunItem) (Outcome, error) {
		started <- i
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	})
	m := New(context.Background(), s, map[string]Registration{"embedding": {Name: "test/v1", Executor: executor}})
	r, _, err := m.Create(context.Background(), "actor", "embedding", "key")
	if err != nil {
		t.Fatal(err)
	}
	awaitItem(t, started)
	closeManager(t, m)
	final := waitRun(t, s, r.ID, "interrupted")
	if final.Counts.Interrupted != 2 || final.CancelRequested {
		t.Fatalf("shutdown=%+v", final)
	}
	if _, _, err := m.Create(context.Background(), "actor", "embedding", "new"); !errors.Is(err, ErrClosed) {
		t.Fatalf("created after close: %v", err)
	}
	newManager := New(context.Background(), s, map[string]Registration{"embedding": {Name: "test/v1", Executor: executor}})
	closeManager(t, newManager)
	select {
	case <-started:
		t.Fatal("interrupted run resumed")
	default:
	}
}

func TestManagerExecutorOutcomesAndSafeFailure(t *testing.T) {
	for _, test := range []struct {
		name         string
		executor     executorFunc
		status, code string
		failed, busy int64
	}{
		{"error", func(context.Context, catalog.JobRun, catalog.JobRunItem) (Outcome, error) {
			return Outcome{}, errors.New("private token=secret")
		}, "failed", "executor_failed", 1, 0},
		{"panic", func(context.Context, catalog.JobRun, catalog.JobRunItem) (Outcome, error) {
			panic("private token=secret")
		}, "failed", "executor_panic", 1, 0},
		{"invalid", func(context.Context, catalog.JobRun, catalog.JobRunItem) (Outcome, error) { return Outcome{}, nil }, "failed", "invalid_executor_outcome", 1, 0},
		{"busy", func(context.Context, catalog.JobRun, catalog.JobRunItem) (Outcome, error) {
			return Outcome{Status: "busy", ErrorCode: "target_busy"}, nil
		}, "completed", "target_busy", 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := testCatalog(t, "a")
			m := New(context.Background(), s, map[string]Registration{"embedding": {Name: "test/v1", Executor: test.executor}})
			defer closeManager(t, m)
			r, _, err := m.Create(context.Background(), "actor", "embedding", "key")
			if err != nil {
				t.Fatal(err)
			}
			final := waitRun(t, s, r.ID, test.status)
			if final.LastError != test.code || final.Counts.Failed != test.failed || final.Counts.Busy != test.busy {
				t.Fatalf("final=%+v", final)
			}
		})
	}
}

func TestManagerEmptySnapshotCompletesWithoutExecutorCall(t *testing.T) {
	s := testCatalog(t)
	m := New(context.Background(), s, map[string]Registration{"embedding": {Name: "test/v1", Executor: executorFunc(func(context.Context, catalog.JobRun, catalog.JobRunItem) (Outcome, error) {
		t.Error("called executor for empty snapshot")
		return Outcome{}, nil
	})}})
	defer closeManager(t, m)
	r, _, err := m.Create(context.Background(), "actor", "embedding", "key")
	if err != nil {
		t.Fatal(err)
	}
	if final := waitRun(t, s, r.ID, "completed"); final.Counts.Total != 0 {
		t.Fatalf("empty snapshot=%+v", final)
	}
}
