package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"viewer/internal/encoding"
)

type encodingClaimRequest struct {
	Limit int `json:"limit"`
}
type encodingClaimResponse struct {
	Claimed []encoding.Claimed `json:"claimed"`
}

func (s *Server) claimEncoding(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkerBodyBytes)
	var req encodingClaimRequest
	if err := jsonBody(r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	claimed, err := s.encoder.Claim(r.Context(), req.Limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, encodingClaimResponse{Claimed: claimed})
}

type encodingRenewRequest struct {
	Hash  string `json:"hash"`
	Token string `json:"token"`
}

func (s *Server) renewEncoding(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkerBodyBytes)
	var req encodingRenewRequest
	if err := jsonBody(r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Hash) == "" || strings.TrimSpace(req.Token) == "" {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "hash and token are required")
		return
	}
	until, url, err := s.encoder.Renew(r.Context(), req.Hash, req.Token)
	if errors.Is(err, encoding.ErrLostLease) {
		writeError(w, r, http.StatusConflict, "LOST_LEASE", err.Error())
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		LeaseUntil any    `json:"leaseUntil"`
		PutURL     string `json:"putUrl"`
	}{until, url})
}

type encodingCompleteRequest struct {
	Hash    string `json:"hash"`
	Token   string `json:"token"`
	Outcome string `json:"outcome"`
	Error   string `json:"error"`
}

func (s *Server) completeEncoding(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkerBodyBytes)
	var req encodingCompleteRequest
	if err := jsonBody(r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Hash) == "" || strings.TrimSpace(req.Token) == "" {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "hash and token are required")
		return
	}
	status, err := s.encoder.Complete(r.Context(), req.Hash, req.Token, req.Outcome, req.Error)
	if errors.Is(err, encoding.ErrLostLease) {
		writeError(w, r, http.StatusConflict, "LOST_LEASE", err.Error())
		return
	}
	if errors.Is(err, encoding.ErrInvalidOutput) {
		writeError(w, r, http.StatusUnprocessableEntity, "INVALID_OUTPUT", err.Error())
		return
	}
	if errors.Is(err, encoding.ErrInvalidOutcome) {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Status string `json:"status"`
	}{status})
}
