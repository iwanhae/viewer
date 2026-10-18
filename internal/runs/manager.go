// Package runs owns finite, manual, local-only stage runs. It never polls or
// cancels external worker queues. Actual stage processors are injected.
package runs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"viewer/internal/catalog"
)

var (
	ErrUnavailable = errors.New("local stage executor unavailable; no processor is registered for this stage")
	ErrClosed      = errors.New("local run manager is stopping")
)

// Executor must claim only item.Hash, atomically from pending, using the item's
// ClaimToken as its local ownership token. A lost race is Outcome{Status:"busy"},
// never a stolen lease. It must honor ctx cancellation, release only its own
// claims, and return succeeded only after durable stage completion. Cancellation
// does not undo a completed side effect. No processor is installed by default.
type Executor interface {
	Execute(ctx context.Context, run catalog.JobRun, item catalog.JobRunItem) (Outcome, error)
}

type Outcome struct {
	Status    string // succeeded, failed, busy, skipped, or cancelled
	ErrorCode string // catalog's allowlisted, public machine code (not stderr)
}

type Registration struct {
	Name     string // stable executor/version provenance, persisted with the run
	Executor Executor
}

type Manager struct {
	cat       *catalog.Store
	ctx       context.Context
	stop      context.CancelFunc
	executors map[string]Registration
	mu        sync.Mutex
	active    map[string]context.CancelFunc
	closed    bool
	wg        sync.WaitGroup
}

// New uses the service lifetime, never an HTTP request context. Registration is
// immutable. Open has already interrupted old runs; New never resumes them.
func New(ctx context.Context, cat *catalog.Store, executors map[string]Registration) *Manager {
	ctx, stop := context.WithCancel(ctx)
	m := &Manager{cat: cat, ctx: ctx, stop: stop, executors: make(map[string]Registration), active: make(map[string]context.CancelFunc)}
	for stage, executor := range executors {
		if catalog.ValidJobRunStage(stage) && executor.Name != "" && executor.Executor != nil {
			m.executors[stage] = executor
		}
	}
	return m
}

func (m *Manager) Create(ctx context.Context, actor, stage, key string) (catalog.JobRun, bool, error) {
	if err := catalog.ValidateJobRunRequest(stage, key); err != nil {
		return catalog.JobRun{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return catalog.JobRun{}, false, ErrClosed
	}
	if r, err := m.cat.FindJobRunSubmission(ctx, actor, stage, key); !errors.Is(err, catalog.ErrJobRunNotFound) {
		return r, err == nil, err
	}
	executor, ok := m.executors[stage]
	if !ok {
		return catalog.JobRun{}, false, fmt.Errorf("%w: %s", ErrUnavailable, stage)
	}
	r, repeated, err := m.cat.CreateJobRun(ctx, actor, stage, key, executor.Name)
	if err != nil || repeated {
		return r, repeated, err
	}
	runCtx, cancel := context.WithCancel(m.ctx)
	m.active[r.ID] = cancel
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() { cancel(); m.mu.Lock(); delete(m.active, r.ID); m.mu.Unlock() }()
		m.execute(runCtx, r, executor.Executor)
	}()
	return r, false, nil
}

func (m *Manager) Cancel(ctx context.Context, id string) (catalog.JobRun, error) {
	// Persist first: NextJobRunItem observes this even before the signal arrives.
	r, err := m.cat.RequestJobRunCancel(ctx, id)
	if err != nil {
		return r, err
	}
	m.mu.Lock()
	if cancel := m.active[id]; cancel != nil {
		cancel()
	}
	m.mu.Unlock()
	return r, nil
}

func (m *Manager) execute(ctx context.Context, r catalog.JobRun, executor Executor) {
	terminal, code := "", ""
	defer func() {
		if recover() != nil {
			terminal, code = "failed", "executor_panic"
		}
		if m.ctx.Err() != nil {
			terminal, code = "interrupted", "service_stopped"
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.cat.FinishJobRun(cleanup, r.ID, terminal, code); err != nil {
			log.Printf("runs: finish run=%s failed: %v", r.ID, err)
		}
	}()
	if err := m.cat.StartJobRun(ctx, r.ID); err != nil {
		if ctx.Err() == nil {
			terminal, code = "failed", "executor_failed"
		}
		return
	}
	for ctx.Err() == nil {
		item, err := m.cat.NextJobRunItem(ctx, r.ID)
		if err != nil {
			if ctx.Err() == nil {
				terminal, code = "failed", "executor_failed"
			}
			return
		}
		if item == nil {
			return
		}
		outcome, err := executor.Execute(ctx, r, *item)
		if m.ctx.Err() != nil && (err != nil || outcome.Status == "cancelled") {
			// Leave unfinished work for interrupted recovery, not user-cancelled
			// accounting. A genuinely completed outcome is still preserved below.
			return
		}
		if err != nil {
			outcome = Outcome{Status: "failed", ErrorCode: "executor_failed"}
			if ctx.Err() != nil {
				outcome = Outcome{Status: "cancelled"}
			}
		}
		switch outcome.Status {
		case "succeeded", "failed", "busy", "skipped", "cancelled":
		default:
			outcome = Outcome{Status: "failed", ErrorCode: "invalid_executor_outcome"}
		}
		// Persist completed work even if cancellation won just after execution.
		persist, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = m.cat.FinishJobRunItem(persist, r.ID, *item, outcome.Status, outcome.ErrorCode)
		cancel()
		if err != nil {
			terminal, code = "failed", "executor_failed"
			return
		}
	}
}

func (m *Manager) List(ctx context.Context, limit int, cursor string) (catalog.JobRunPage, error) {
	return m.cat.JobRuns(ctx, limit, cursor)
}
func (m *Manager) Read(ctx context.Context, id string, limit int, cursor string) (catalog.JobRunItemPage, error) {
	return m.cat.JobRunItems(ctx, id, limit, cursor)
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.stop()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
