package recommend

// AutomaticStatus is the operator-facing shape of GET/POST
// /admin/api/automatic/embedding. It describes only the local embedding
// worker, never external workers or model inference activity.
//
// Running means the worker loop exists — Start ran after a successful model
// load — not that a batch is in flight at this instant; a loaded-but-idle
// worker is Running with nothing to embed. Enabled is the process-local
// preference the toggle flips; it is deliberately not persisted, so every
// server start re-enables automatic embedding unless the operator pauses it
// again. PausedByManual is gone: with manual runs removed there is no
// handoff that could suspend an enabled worker.
type AutomaticStatus struct {
	Enabled bool `json:"enabled"`
	Running bool `json:"running"`
}

// SetAutomaticEmbedding flips the process-local pause switch. The switch is
// checked between batches: an in-flight batch runs to completion (its claims,
// write-backs and lease releases finish normally) and no new batch is claimed
// until the flag is true again.
//
// This is deliberately weaker than the old Manager semantics, which cancelled
// the worker and joined it before returning. That join existed so a manual
// embedding run could take a finite snapshot without racing the automatic
// worker's claims; with manual runs gone there is no snapshot to protect, and
// a batch that is already claimed must either complete or release anyway —
// both of which the worker does on its own. Waiting for it would only hold
// the HTTP request open to no effect.
func (s *Service) SetAutomaticEmbedding(enabled bool) {
	if s == nil {
		return
	}
	s.automatic.Store(enabled)
}

// AutomaticEmbedding reports the current toggle and whether the worker loop
// exists. A nil service answers the zero status so dashboards can render a
// deployment without an embedding model instead of failing.
func (s *Service) AutomaticEmbedding() AutomaticStatus {
	if s == nil {
		return AutomaticStatus{}
	}
	// workersStarted is written once, inside Start's startOnce body, under
	// modelMu; reading it here under the same lock pairs the goroutines so the
	// HTTP handler never races the startup load.
	s.modelMu.Lock()
	running := s.workersStarted
	s.modelMu.Unlock()
	return AutomaticStatus{Enabled: s.automatic.Load(), Running: running}
}
