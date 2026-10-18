package runs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"viewer/internal/catalog"
)

func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker")
	}
}

func waitAutomaticStatus(t *testing.T, m *Manager, want AutomaticEmbeddingStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, err := m.AutomaticEmbedding()
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	got, _ := m.AutomaticEmbedding()
	t.Fatalf("automatic=%+v want=%+v", got, want)
}

func registerTestEmbedding(t *testing.T, m *Manager, executor executorFunc) {
	t.Helper()
	if executor == nil {
		executor = func(context.Context, catalog.JobRun, catalog.JobRunItem) (Outcome, error) {
			return Outcome{Status: "succeeded"}, nil
		}
	}
	if err := m.Register("embedding", Registration{Name: "test/v1", Executor: executor}); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticDefaultPauseBeforeRegistrationAndRestart(t *testing.T) {
	m := New(context.Background(), testCatalog(t), nil)
	defer closeManager(t, m)
	waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: true})
	if _, err := m.SetAutomaticEmbedding(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	registerTestEmbedding(t, m, nil)
	started := make(chan struct{}, 4)
	if err := m.RegisterAutomaticEmbedding(func(ctx context.Context) error { started <- struct{}{}; <-ctx.Done(); return nil }); err != nil {
		t.Fatal(err)
	}
	waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{})
	for i := 0; i < 3; i++ {
		if _, err := m.SetAutomaticEmbedding(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		awaitSignal(t, started)
		if _, err := m.SetAutomaticEmbedding(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
			t.Fatal("idempotent enable started a second worker")
		default:
		}
		status, err := m.SetAutomaticEmbedding(context.Background(), false)
		if err != nil || status != (AutomaticEmbeddingStatus{}) {
			t.Fatalf("pause did not join: %+v %v", status, err)
		}
	}
	// A new server does not inherit this process's pause preference.
	next := New(context.Background(), m.cat, nil)
	defer closeManager(t, next)
	waitAutomaticStatus(t, next, AutomaticEmbeddingStatus{Enabled: true})
}

func TestAutomaticManualHandoffSnapshotReleaseAndResume(t *testing.T) {
	for _, pause := range []bool{false, true} {
		for _, cancelRun := range []bool{false, true} {
			name := "complete"
			if cancelRun {
				name = "cancel"
			}
			if pause {
				name += "-paused"
			}
			t.Run(name, func(t *testing.T) {
				s := testCatalog(t, "a")
				m := New(context.Background(), s, nil)
				defer closeManager(t, m)
				manualStarted, manualFinish := make(chan catalog.JobRunItem, 1), make(chan struct{})
				registerTestEmbedding(t, m, func(ctx context.Context, _ catalog.JobRun, item catalog.JobRunItem) (Outcome, error) {
					claim, status, err := s.ClaimLocalEmbedding(ctx, item.Hash, item.ClaimToken, time.Now().Add(time.Minute))
					if err != nil || status != catalog.LocalEmbeddingClaimed {
						t.Errorf("manual claimed before release: %s %v", status, err)
						return Outcome{Status: "failed"}, nil
					}
					defer s.ReleaseLocalEmbeddingClaims(context.Background(), []catalog.LocalEmbeddingClaim{claim})
					manualStarted <- item
					select {
					case <-manualFinish:
						return Outcome{Status: "succeeded"}, nil
					case <-ctx.Done():
						return Outcome{Status: "cancelled"}, nil
					}
				})
				autoStarted, autoCancelled, allowRelease := make(chan struct{}, 3), make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				if err := m.RegisterAutomaticEmbedding(func(ctx context.Context) error {
					first := calls.Add(1) == 1
					var claim catalog.LocalEmbeddingClaim
					if first {
						var err error
						claim, _, err = s.ClaimLocalEmbedding(ctx, "a", "automatic-token", time.Now().Add(time.Minute))
						if err != nil {
							t.Error(err)
						}
					}
					autoStarted <- struct{}{}
					<-ctx.Done()
					if first {
						close(autoCancelled)
						<-allowRelease
						if _, err := s.ReleaseLocalEmbeddingClaims(context.Background(), []catalog.LocalEmbeddingClaim{claim}); err != nil {
							t.Error(err)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				awaitSignal(t, autoStarted)
				type result struct {
					run catalog.JobRun
					err error
				}
				created := make(chan result, 1)
				go func() {
					r, _, err := m.Create(context.Background(), "actor", "embedding", "manual")
					created <- result{r, err}
				}()
				awaitSignal(t, autoCancelled)
				waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: true, Running: true, PausedByManual: true})
				page, err := m.List(context.Background(), 10, "")
				if err != nil || len(page.Runs) != 0 {
					t.Fatalf("snapshot preceded release: %+v %v", page, err)
				}
				if _, _, err := m.Create(context.Background(), "another", "embedding", "second"); !errors.Is(err, catalog.ErrJobRunConflict) {
					t.Fatalf("handoff not exclusive: %v", err)
				}
				close(allowRelease)
				var r catalog.JobRun
				select {
				case result := <-created:
					if result.err != nil {
						t.Fatal(result.err)
					}
					r = result.run
				case <-time.After(5 * time.Second):
					t.Fatal("manual create stuck")
				}
				if r.Counts.Total != 1 {
					t.Fatalf("released target missing from snapshot: %+v", r)
				}
				awaitItem(t, manualStarted)
				if replay, repeated, err := m.Create(context.Background(), "actor", "embedding", "manual"); err != nil || !repeated || replay.ID != r.ID {
					t.Fatalf("replay=%+v %v %v", replay, repeated, err)
				}
				if pause {
					if _, err := m.SetAutomaticEmbedding(context.Background(), false); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := m.SetAutomaticEmbedding(context.Background(), true); err != nil {
						t.Fatal(err)
					}
					waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: true, PausedByManual: true})
				}
				terminal := "completed"
				if cancelRun {
					terminal = "cancelled"
					if _, err := m.Cancel(context.Background(), r.ID); err != nil {
						t.Fatal(err)
					}
				} else {
					close(manualFinish)
				}
				waitRun(t, s, r.ID, terminal)
				waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: !pause, Running: !pause})
				if !pause {
					awaitSignal(t, autoStarted)
				}
				wantCalls := int32(2)
				if pause {
					wantCalls = 1
				}
				if calls.Load() != wantCalls {
					t.Fatalf("automatic starts=%d want=%d", calls.Load(), wantCalls)
				}
			})
		}
	}
}

func TestAutomaticRegistrationAndControlErrors(t *testing.T) {
	m := New(context.Background(), testCatalog(t), nil)
	worker := func(ctx context.Context) error { <-ctx.Done(); return nil }
	if err := m.RegisterAutomaticEmbedding(worker); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	for _, reg := range []Registration{{}, {Name: "test"}, {Executor: executorFunc(func(context.Context, catalog.JobRun, catalog.JobRunItem) (Outcome, error) { return Outcome{}, nil })}} {
		if err := m.Register("embedding", reg); !errors.Is(err, ErrRegistration) {
			t.Fatalf("bad registration=%v", err)
		}
	}
	if err := m.Register("unknown", Registration{}); !errors.Is(err, ErrRegistration) {
		t.Fatal(err)
	}
	registerTestEmbedding(t, m, nil)
	if err := m.Register("embedding", m.executors["embedding"]); !errors.Is(err, ErrRegistration) {
		t.Fatal(err)
	}
	if err := m.RegisterAutomaticEmbedding(nil); !errors.Is(err, ErrRegistration) {
		t.Fatal(err)
	}
	if err := m.RegisterAutomaticEmbedding(worker); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterAutomaticEmbedding(worker); !errors.Is(err, ErrRegistration) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.SetAutomaticEmbedding(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: true, Running: true})
	closeManager(t, m)
	if err := m.Register("encoding", Registration{}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := m.RegisterAutomaticEmbedding(worker); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := m.SetAutomaticEmbedding(context.Background(), true); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := m.AutomaticEmbedding(); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestAutomaticCancelledHandoffAndTimedOutPause(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause", true: "manual"}[manual], func(t *testing.T) {
			m := New(context.Background(), testCatalog(t, "a"), nil)
			defer closeManager(t, m)
			registerTestEmbedding(t, m, nil)
			started, cancelled, release := make(chan struct{}, 2), make(chan struct{}, 2), make(chan struct{})
			if err := m.RegisterAutomaticEmbedding(func(ctx context.Context) error {
				started <- struct{}{}
				<-ctx.Done()
				cancelled <- struct{}{}
				<-release
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			awaitSignal(t, started)
			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				if manual {
					_, _, err := m.Create(ctx, "actor", "embedding", "key")
					result <- err
				} else {
					_, err := m.SetAutomaticEmbedding(ctx, false)
					result <- err
				}
			}()
			awaitSignal(t, cancelled)
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled control=%v", err)
			}
			waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: manual, Running: true})
			page, err := m.List(context.Background(), 10, "")
			if err != nil || len(page.Runs) != 0 {
				t.Fatalf("abandoned handoff persisted run: %+v %v", page, err)
			}
			close(release)
			waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: manual, Running: manual})
			if manual {
				awaitSignal(t, started)
			}
		})
	}
}

func TestAutomaticCatalogFailureRestoresPreference(t *testing.T) {
	s := testCatalog(t)
	m := New(context.Background(), s, nil)
	defer closeManager(t, m)
	registerTestEmbedding(t, m, nil)
	// Closing during the handoff forces snapshot creation to fail after yield.
	started, cancelled, release := make(chan struct{}, 2), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	if err := m.RegisterAutomaticEmbedding(func(ctx context.Context) error {
		first := calls.Add(1) == 1
		started <- struct{}{}
		<-ctx.Done()
		if first {
			close(cancelled)
			<-release
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, started)
	result := make(chan error, 1)
	go func() { _, _, err := m.Create(context.Background(), "actor", "embedding", "key"); result <- err }()
	awaitSignal(t, cancelled)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("closed catalog accepted a run")
	}
	awaitSignal(t, started)
	waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: true, Running: true})
}

func TestAutomaticShutdownJoinsPendingHandoff(t *testing.T) {
	m := New(context.Background(), testCatalog(t, "a"), nil)
	registerTestEmbedding(t, m, nil)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	if err := m.RegisterAutomaticEmbedding(func(ctx context.Context) error { close(started); <-ctx.Done(); close(cancelled); <-release; return nil }); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, started)
	created := make(chan error, 1)
	go func() { _, _, err := m.Create(context.Background(), "actor", "embedding", "key"); created <- err }()
	awaitSignal(t, cancelled)
	closed := make(chan error, 1)
	go func() { closed <- m.Close(context.Background()) }()
	if err := <-created; !errors.Is(err, ErrClosed) {
		t.Fatalf("handoff accepted during shutdown: %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close did not join claim cleanup: %v", err)
	default:
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	page, err := m.cat.JobRuns(context.Background(), 10, "")
	if err != nil || len(page.Runs) != 0 {
		t.Fatalf("shutdown handoff persisted work: %+v %v", page, err)
	}
}

func TestAutomaticShutdownDuringManualDoesNotResume(t *testing.T) {
	s := testCatalog(t, "a")
	m := New(context.Background(), s, nil)
	started := make(chan catalog.JobRunItem, 1)
	registerTestEmbedding(t, m, func(ctx context.Context, _ catalog.JobRun, item catalog.JobRunItem) (Outcome, error) {
		started <- item
		<-ctx.Done()
		return Outcome{}, ctx.Err()
	})
	var calls atomic.Int32
	autoStarted := make(chan struct{}, 1)
	if err := m.RegisterAutomaticEmbedding(func(ctx context.Context) error { calls.Add(1); autoStarted <- struct{}{}; <-ctx.Done(); return nil }); err != nil {
		t.Fatal(err)
	}
	awaitSignal(t, autoStarted)
	run, _, err := m.Create(context.Background(), "actor", "embedding", "key")
	if err != nil {
		t.Fatal(err)
	}
	awaitItem(t, started)
	closeManager(t, m)
	waitRun(t, s, run.ID, "interrupted")
	if calls.Load() != 1 {
		t.Fatalf("automatic restarted during shutdown: %d", calls.Load())
	}
}

func TestAutomaticUnexpectedExitDoesNotSpin(t *testing.T) {
	for _, panicWorker := range []bool{false, true} {
		t.Run(map[bool]string{false: "return", true: "panic"}[panicWorker], func(t *testing.T) {
			m := New(context.Background(), testCatalog(t), nil)
			defer closeManager(t, m)
			registerTestEmbedding(t, m, nil)
			var calls atomic.Int32
			exited := make(chan struct{}, 1)
			if err := m.RegisterAutomaticEmbedding(func(context.Context) error {
				calls.Add(1)
				exited <- struct{}{}
				if panicWorker {
					panic("private details")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			awaitSignal(t, exited)
			waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: true})
			if calls.Load() != 1 {
				t.Fatalf("worker restart loop: %d", calls.Load())
			}
		})
	}
}

func TestAutomaticReleaseFailureRejectsHandoffAndPause(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause", true: "manual"}[manual], func(t *testing.T) {
			m := New(context.Background(), testCatalog(t, "a"), nil)
			defer closeManager(t, m)
			registerTestEmbedding(t, m, nil)
			started := make(chan struct{}, 2)
			var calls atomic.Int32
			if err := m.RegisterAutomaticEmbedding(func(ctx context.Context) error {
				first := calls.Add(1) == 1
				started <- struct{}{}
				<-ctx.Done()
				if first {
					return errors.New("private catalog release failure")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			awaitSignal(t, started)
			var err error
			if manual {
				_, _, err = m.Create(context.Background(), "actor", "embedding", "key")
			} else {
				_, err = m.SetAutomaticEmbedding(context.Background(), false)
			}
			if !errors.Is(err, ErrAutomatic) {
				t.Fatalf("release failure ignored: %v", err)
			}
			page, listErr := m.List(context.Background(), 10, "")
			if listErr != nil || len(page.Runs) != 0 {
				t.Fatalf("unsafe handoff persisted run: %+v %v", page, listErr)
			}
			waitAutomaticStatus(t, m, AutomaticEmbeddingStatus{Enabled: manual, Running: manual})
			if manual {
				awaitSignal(t, started)
			}
		})
	}
}
