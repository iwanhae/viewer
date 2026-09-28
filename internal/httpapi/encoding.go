package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"viewer/internal/encoding"
)

// encodingOutputReceiver is implemented by the encoding service. Keeping the
// receiver as a small interface also lets the HTTP handler stay independent of
// the service's storage details.
type encodingOutputReceiver interface {
	ReceiveOutput(ctx context.Context, hash, token string, body io.Reader) error
}

type encodingClaimRequest struct {
	Limit      int    `json:"limit"`
	OutputMode string `json:"outputMode,omitempty"`
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
	var claimed []encoding.Claimed
	var err error
	switch req.OutputMode {
	case "":
		claimed, err = s.encoder.Claim(r.Context(), req.Limit) // legacy S3 PUT protocol
	case "api":
		claimed, err = s.encoder.ClaimForAPI(r.Context(), req.Limit)
	default:
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "unsupported encoding output mode")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, encodingClaimResponse{Claimed: claimed})
}

type encodingRenewRequest struct {
	Hash       string `json:"hash"`
	Token      string `json:"token"`
	OutputMode string `json:"outputMode,omitempty"`
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
	if req.OutputMode == "api" {
		until, err := s.encoder.RenewLease(r.Context(), req.Hash, req.Token)
		if errors.Is(err, encoding.ErrLostLease) {
			writeError(w, r, http.StatusConflict, "LOST_LEASE", err.Error())
			return
		}
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, struct {
			LeaseUntil time.Time `json:"leaseUntil"`
			OutputMode string    `json:"outputMode"`
		}{until, "api"})
		return
	}
	if req.OutputMode != "" {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "unsupported encoding output mode")
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
		LeaseUntil time.Time `json:"leaseUntil"`
		PutURL     string    `json:"putUrl"`
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

func (s *Server) receiveEncodingOutput(w http.ResponseWriter, r *http.Request) {
	hash := r.Header.Get("X-Encoding-Hash")
	token := r.Header.Get("X-Encoding-Token")
	if strings.TrimSpace(hash) == "" || strings.TrimSpace(token) == "" {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "encoding hash and token are required")
		return
	}
	if s.encodingOutput == nil {
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "encoding output receiver is not available")
		return
	}

	if err := s.encodingOutput.ReceiveOutput(r.Context(), hash, token, r.Body); err != nil {
		if errors.Is(err, encoding.ErrLostLease) {
			writeError(w, r, http.StatusConflict, "LOST_LEASE", err.Error())
			return
		}
		if errors.Is(err, encoding.ErrInvalidOutput) {
			writeError(w, r, http.StatusUnprocessableEntity, "INVALID_OUTPUT", err.Error())
			return
		}
		if errors.Is(err, encoding.ErrOutputTooLarge) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "OUTPUT_TOO_LARGE", err.Error())
			return
		}
		if errors.Is(err, encoding.ErrSpoolBusy) {
			writeError(w, r, http.StatusTooManyRequests, "SPOOL_BUSY", err.Error())
			return
		}
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		Status string `json:"status"`
	}{"received"})
}
