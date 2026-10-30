package httpapi

import (
	"errors"
	"net/http"

	"viewer/internal/admin"
)

// The handlers in this file are the admin surface's local execution controls:
// the automatic-embedding pause switch (delegating to the recommendation
// service that hosts the worker) and the local WebP encoding drain trigger.
// They sit behind the same Basic auth and mutation guards as every other
// admin action.

func (s *Server) adminAutomaticEmbedding(w http.ResponseWriter, r *http.Request) {
	status, err := s.admin.AutomaticEmbedding()
	if err != nil {
		writeAdminLocalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) adminSetAutomaticEmbedding(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeAdminJSONBody(w, r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	if req.Enabled == nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "enabled must be a boolean")
		return
	}
	// The response carries the status the toggle produced, so the dashboard
	// paints the result of its own action rather than a racing re-read.
	status, err := s.admin.SetAutomaticEmbedding(*req.Enabled)
	if err != nil {
		writeAdminLocalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// adminRunLocalEncoding answers POST /admin/api/encoding/run-local with a
// strict `{}` JSON body (the same decode every admin action uses: JSON
// content type required, unknown fields rejected). It starts one background
// drain over the pending non-WebP blobs and replies 200 with
// {"started":bool} — started=false means a drain is already in flight, which
// is a normal outcome of a double click or a racing request, not an error.
// Failures that prevent any work (cwebp missing from PATH, the encoding
// service not started) come back as errors so the operator sees the cause.
func (s *Server) adminRunLocalEncoding(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if err := decodeAdminJSONBody(w, r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	started, err := s.admin.RunLocalEncoding()
	if err != nil {
		writeAdminLocalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"started": started})
}

// writeAdminLocalError maps the local-control error classes onto the admin
// wire contract: an unwired service is a deployment state (503 UNAVAILABLE),
// and anything else is a genuine server fault.
func writeAdminLocalError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, admin.ErrAutomaticEmbeddingUnavailable), errors.Is(err, admin.ErrLocalEncodingUnavailable):
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", err.Error())
	default:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "local execution request failed")
	}
}
