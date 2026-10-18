package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"viewer/internal/catalog"
	"viewer/internal/runs"
)

func (s *Server) adminAutomaticEmbedding(w http.ResponseWriter, r *http.Request) {
	status, err := s.admin.AutomaticEmbedding()
	if err != nil {
		writeAdminRunError(w, r, err)
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
	status, err := s.admin.SetAutomaticEmbedding(r.Context(), *req.Enabled)
	if err != nil {
		writeAdminRunError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) adminCreateRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stage string `json:"stage"`
	}
	if err := decodeAdminJSONBody(w, r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	run, repeated, err := s.admin.CreateRun(r.Context(), adminActorFromRequest(r), req.Stage, strings.TrimSpace(r.Header.Get("Idempotency-Key")))
	if err != nil {
		writeAdminRunError(w, r, err)
		return
	}
	status := http.StatusAccepted
	if repeated {
		status = http.StatusOK
	}
	w.Header().Set("Location", "/admin/api/runs/"+run.ID)
	writeJSON(w, status, run)
}

func (s *Server) adminCancelRun(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if err := decodeAdminJSONBody(w, r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	run, err := s.admin.CancelRun(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeAdminRunError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func runPageQuery(w http.ResponseWriter, r *http.Request) (int, string, bool) {
	limit, err := parseOptionalIntQuery(r, "limit", 50, 1, catalog.JobRunPageLimit)
	if err != nil || !onlyQueryKeys(r.URL.Query(), "limit", "cursor") || len(r.URL.Query().Get("cursor")) > 4096 {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid run pagination")
		return 0, "", false
	}
	return limit, r.URL.Query().Get("cursor"), true
}
func (s *Server) adminRuns(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := runPageQuery(w, r)
	if !ok {
		return
	}
	page, err := s.admin.Runs(r.Context(), limit, cursor)
	if err != nil {
		writeAdminRunError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (s *Server) adminRun(w http.ResponseWriter, r *http.Request) {
	limit, cursor, ok := runPageQuery(w, r)
	if !ok {
		return
	}
	page, err := s.admin.Run(r.Context(), chi.URLParam(r, "id"), limit, cursor)
	if err != nil {
		writeAdminRunError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func writeAdminRunError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, catalog.ErrInvalidJobRun):
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, catalog.ErrJobRunConflict):
		writeError(w, r, http.StatusConflict, "CONFLICT", "a local run is already active for this stage, or the idempotency key is bound to a different stage")
	case errors.Is(err, catalog.ErrJobRunNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "local run not found")
	case errors.Is(err, runs.ErrUnavailable):
		writeError(w, r, http.StatusServiceUnavailable, "EXECUTOR_UNAVAILABLE", "no local executor is registered for this stage; install the local stage processor before starting a run")
	case errors.Is(err, runs.ErrClosed):
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "local run manager is stopping")
	case errors.Is(err, runs.ErrAutomatic):
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "automatic embedding claim release failed; retry control before starting a manual run")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "local worker control interrupted; read automatic status before retrying")
	default:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "local run request failed")
	}
}
