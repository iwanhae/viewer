package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"viewer/internal/catalog"
)

const maxRecoveryBodyBytes = 4 << 20

type recoveryFilterRequest struct {
	AlbumID               string `json:"albumId,omitempty"`
	Hash                  string `json:"hash,omitempty"`
	Encoding              string `json:"encoding,omitempty"`
	Embedding             string `json:"embedding,omitempty"`
	EncodingRaw           string `json:"encodingRaw,omitempty"`
	EmbeddingRaw          string `json:"embeddingRaw,omitempty"`
	EncodingErrorCode     string `json:"encodingErrorCode,omitempty"`
	EmbeddingErrorCode    string `json:"embeddingErrorCode,omitempty"`
	EncodingLeaseExpired  *bool  `json:"encodingLeaseExpired,omitempty"`
	EmbeddingLeaseExpired *bool  `json:"embeddingLeaseExpired,omitempty"`
	EncodingPhase         string `json:"encodingPhase,omitempty"`
	SkippedReason         string `json:"skippedReason,omitempty"`
	Unreferenced          *bool  `json:"unreferenced,omitempty"`
}

type recoverySelectionRequest struct {
	Hashes []string                `json:"hashes,omitempty"`
	Photos []catalog.RecoveryPhoto `json:"photos,omitempty"`
	Albums []string                `json:"albumIds,omitempty"`
	Filter *recoveryFilterRequest  `json:"filter,omitempty"`
}

type recoveryPreviewRequest struct {
	Action    string                   `json:"action"`
	Selection recoverySelectionRequest `json:"selection"`
}

type recoveryOperationRequest struct {
	PreviewID string `json:"previewId"`
	Action    string `json:"action"`
}

func (s *Server) adminActionPreview(w http.ResponseWriter, r *http.Request) {
	var req recoveryPreviewRequest
	if err := decodeRecoveryJSONBody(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	selection := catalog.RecoverySelection{Hashes: req.Selection.Hashes, Photos: req.Selection.Photos, Albums: req.Selection.Albums}
	if req.Selection.Filter != nil {
		f := req.Selection.Filter
		selection.Filter = &catalog.AdminBlobFilters{
			AlbumID: f.AlbumID, Hash: f.Hash, Encoding: f.Encoding, Embedding: f.Embedding,
			EncodingRaw: f.EncodingRaw, EmbeddingRaw: f.EmbeddingRaw,
			EncodingErrorCode: f.EncodingErrorCode, EmbeddingErrorCode: f.EmbeddingErrorCode,
			EncodingLeaseExpired: f.EncodingLeaseExpired, EmbeddingLeaseExpired: f.EmbeddingLeaseExpired,
			EncodingPhase: f.EncodingPhase, SkippedReason: f.SkippedReason, Unreferenced: f.Unreferenced,
		}
	}
	preview, err := s.admin.CreateRecoveryPreview(r.Context(), adminActorFromRequest(r), req.Action, selection)
	if err != nil {
		writeAdminRecoveryError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, preview)
}

func (s *Server) adminCreateOperation(w http.ResponseWriter, r *http.Request) {
	var req recoveryOperationRequest
	if err := decodeRecoveryJSONBody(w, r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	payload, _ := json.Marshal(req)
	digest := sha256.Sum256(payload)
	operation, repeated, err := s.admin.CreateRecoveryOperation(r.Context(), adminActorFromRequest(r), req.PreviewID, req.Action, key, hex.EncodeToString(digest[:]))
	if err != nil {
		writeAdminRecoveryError(w, r, err)
		return
	}
	status := http.StatusAccepted
	if repeated {
		status = http.StatusOK
	}
	writeJSON(w, status, operation)
}

func (s *Server) adminOperation(w http.ResponseWriter, r *http.Request) {
	limit, err := parseOptionalIntQuery(r, "limit", 50, 1, catalog.RecoveryPageLimit)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid limit")
		return
	}
	if !onlyQueryKeys(r.URL.Query(), "limit", "cursor") {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "unknown query parameter")
		return
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 4096 {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid cursor")
		return
	}
	page, err := s.admin.RecoveryResults(r.Context(), chi.URLParam(r, "id"), limit, cursor)
	if err != nil {
		writeAdminRecoveryError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) adminOperations(w http.ResponseWriter, r *http.Request) {
	limit, err := parseOptionalIntQuery(r, "limit", 50, 1, catalog.RecoveryPageLimit)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid limit")
		return
	}
	if !onlyQueryKeys(r.URL.Query(), "limit", "cursor") {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "unknown query parameter")
		return
	}
	options := catalog.RecoveryOperationListOptions{Limit: limit}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		if len(cursor) > 4096 {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid cursor")
			return
		}
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		var parts []string
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid cursor")
			return
		}
		dec := json.NewDecoder(bytes.NewReader(decoded))
		dec.DisallowUnknownFields()
		if dec.Decode(&parts) != nil || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid cursor")
			return
		}
		var trailing any
		if dec.Decode(&trailing) != io.EOF {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid cursor")
			return
		}
		options.AfterCreated, options.AfterID = parts[0], parts[1]
	}
	page, err := s.admin.RecoveryOperations(r.Context(), options)
	if err != nil {
		writeAdminRecoveryError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func decodeRecoveryJSONBody(w http.ResponseWriter, r *http.Request, out any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return fmt.Errorf("Content-Type must be application/json")
	}
	if r.Body == nil {
		return fmt.Errorf("missing body")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRecoveryBodyBytes))
	if err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("missing body")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("invalid body: unexpected trailing content")
	}
	return nil
}

func writeAdminRecoveryError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, catalog.ErrInvalidRecoverySelection):
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	case errors.Is(err, catalog.ErrRecoveryConflict):
		writeError(w, r, http.StatusConflict, "CONFLICT", "preview, target generation, expiry, or idempotency binding changed")
	case errors.Is(err, catalog.ErrRecoveryNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "operation not found")
	default:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "scoped recovery request failed")
	}
}

func onlyQueryKeys(query url.Values, allowed ...string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range query {
		if _, ok := set[key]; !ok {
			return false
		}
		if len(query[key]) != 1 {
			return false
		}
	}
	return true
}
