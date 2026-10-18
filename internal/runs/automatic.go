package runs

import (
	"context"
	"log"
)

// AutomaticEmbeddingStatus describes the local poller, not external workers or
// model inference activity. Running includes a worker still releasing claims.
type AutomaticEmbeddingStatus struct {
	Enabled        bool `json:"enabled"`
	Running        bool `json:"running"`
	PausedByManual bool `json:"pausedByManual"`
}

type automaticExecution struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error // published by closing done
}

// Register installs an executor once. In-flight runs retain their registration.
// In particular, register embedding only after the shared model loads.
func (m *Manager) Register(stage string, registration Registration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return ErrClosed
	}
	if !validRegistration(stage, registration) {
		return ErrRegistration
	}
	if _, exists := m.executors[stage]; exists {
		return ErrRegistration
	}
	m.executors[stage] = registration
	return nil
}

// RegisterAutomaticEmbedding installs a blocking, restartable worker entry
// point. It must return only after releasing all claims and joining its work;
// cancelling it must NOT close the shared model. Pausing before registration is
// supported, including while startup is still loading the model. A cleanup error
// rejects the waiting handoff/control instead of silently taking a bad snapshot.
func (m *Manager) RegisterAutomaticEmbedding(worker func(context.Context) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return ErrClosed
	}
	if worker == nil || m.autoWorker != nil {
		return ErrRegistration
	}
	if _, ok := m.executors["embedding"]; !ok {
		return ErrUnavailable
	}
	m.autoWorker = worker
	m.startAutomaticLocked()
	return nil
}

func (m *Manager) automaticStatusLocked() AutomaticEmbeddingStatus {
	return AutomaticEmbeddingStatus{
		Enabled: m.autoEnabled, Running: m.auto != nil,
		PausedByManual: m.autoEnabled && m.manualEmbedding,
	}
}

func (m *Manager) AutomaticEmbedding() (AutomaticEmbeddingStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return AutomaticEmbeddingStatus{}, ErrClosed
	}
	return m.automaticStatusLocked(), nil
}

// SetAutomaticEmbedding changes the process-local preference. Disable joins the
// worker before returning successfully; a timed-out request leaves it disabled
// and releasing, never re-enables it. Manual runs are not cancelled.
func (m *Manager) SetAutomaticEmbedding(ctx context.Context, enabled bool) (AutomaticEmbeddingStatus, error) {
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return AutomaticEmbeddingStatus{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return AutomaticEmbeddingStatus{}, err
	}
	m.autoEnabled = enabled
	var done *automaticExecution
	if enabled {
		m.startAutomaticLocked()
	} else {
		done = m.stopAutomaticLocked()
	}
	m.mu.Unlock()
	if err := m.waitAutomatic(ctx, done); err != nil {
		return AutomaticEmbeddingStatus{}, err
	}
	return m.AutomaticEmbedding()
}

func (m *Manager) waitAutomatic(ctx context.Context, worker *automaticExecution) error {
	if worker == nil {
		return nil
	}
	select {
	case <-worker.done:
		return worker.err
	case <-m.ctx.Done():
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) stopAutomaticLocked() *automaticExecution {
	if m.auto != nil {
		m.auto.cancel()
	}
	return m.auto
}

func (m *Manager) startAutomaticLocked() {
	if m.closed || m.ctx.Err() != nil || !m.autoEnabled || m.manualEmbedding || m.autoWorker == nil || m.auto != nil {
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	worker := &automaticExecution{cancel: cancel, done: make(chan struct{})}
	m.auto = worker
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			if recover() != nil {
				log.Printf("runs: automatic embedding worker panicked")
				worker.err = ErrAutomatic
			}
			wasCancelled := ctx.Err() != nil
			cancel()
			m.mu.Lock()
			defer m.mu.Unlock()
			m.auto = nil
			close(worker.done)
			// Resume only after a requested stop. An unexpected return/panic
			// must not create an unbounded hot restart loop.
			if wasCancelled && worker.err == nil {
				m.startAutomaticLocked()
			}
		}()
		if err := m.autoWorker(ctx); err != nil {
			log.Printf("runs: automatic embedding worker failed to release cleanly")
			worker.err = ErrAutomatic
		}
	}()
}
