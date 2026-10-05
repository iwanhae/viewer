package httpapi

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"viewer/internal/admin"
)

// The handlers in this file are the operator surface: a dashboard over the
// catalog and vector-store state, and the re-embed trigger that is the
// recovery path for a lost vector store. They sit behind HTTP Basic auth with
// the deployment's ADMIN_TOKEN as the password, which is what lets the page
// work from a plain browser — the 401 challenge doubles as the login prompt —
// without any session or login plumbing of its own.

// requireAdminToken guards the admin routes with HTTP Basic auth. The username
// is ignored and the password must equal the admin token; the comparison is
// constant time so a wrong guess costs no measurably less than a right one.
// Every failure — absent header, wrong scheme, malformed base64, wrong
// password — is the same 401 with a WWW-Authenticate challenge, so an outsider
// cannot tell which part failed and a browser knows to prompt.
func (s *Server) requireAdminToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := ""
		header := r.Header.Get("Authorization")
		// The scheme is case-insensitive per RFC 9110 11.6.1, so a
		// spec-compliant client is not rejected for its choice of case.
		const scheme = "Basic "
		if len(header) > len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
			if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(scheme):])); err == nil {
				// The wire format is "user:password". Only the password
				// carries the token here, and splitting on the first colon
				// keeps a colon inside either half from moving the boundary.
				if cut := bytes.IndexByte(decoded, ':'); cut >= 0 {
					presented = string(decoded[cut+1:])
				}
			}
		}
		if subtle.ConstantTimeCompare([]byte(presented), []byte(s.adminToken)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="viewer admin"`)
			writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "admin credentials required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// adminEnabled reports whether the /admin surface is registered at all. It is
// the single place that defines "disabled", so the router's registration check
// and the SPA fallback's 404 rule cannot drift apart.
func (s *Server) adminEnabled() bool {
	return s.admin != nil && s.adminToken != ""
}

// isAdminPath reports whether path lives under the /admin surface. The SPA
// fallback uses it to keep serving the frontend's index.html for a dashboard
// this deployment does not have.
func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/")
}

// adminStats answers with one snapshot of the catalog and vector-store state.
func (s *Server) adminStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.admin.Stats(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// adminReindex triggers the full re-embed and answers with the embedding
// counts it produced, so the operator's next look at the page — or this very
// response — already shows the pending surge.
func (s *Server) adminReindex(w http.ResponseWriter, r *http.Request) {
	counts, err := s.admin.Reindex(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

// maxAdminBodyBytes bounds an admin action body. The biggest one the dashboard
// ever sends is a single boolean, so a few kilobytes is generous; the tight
// limit keeps a fat request from reaching the catalog at all.
const maxAdminBodyBytes = 4 << 10

// decodeAdminJSONBody decodes a strict JSON body into out, like jsonBody, with
// one relaxation: an empty or absent body stands for the zero-value request.
// The recovery endpoint requires an explicit target, so an empty body decodes
// to the zero target there and is rejected as unknown — but that rejection is
// the dispatch's message, not a decode failure, and future admin endpoints
// with optional bodies keep the leniency useful.
func decodeAdminJSONBody(w http.ResponseWriter, r *http.Request, out any) error {
	if r.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAdminBodyBytes))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	// Same strictness as jsonBody: unknown fields are a caller bug, and
	// trailing content is a mangled request, not data to ignore.
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

// The recovery targets of POST /admin/api/recover, one deliberate flip each.
// They are separate strings rather than flags on a shared action because the
// four differ in blast radius, not just in which rows they touch: releasing
// expired claims is the safe default, force-releasing live leases cancels
// in-flight worker work, retrying failed re-runs the model on blobs that may
// fail deterministically again, and the encoding reset leaves committing rows
// to the encoding service's own reconciliation. The confirm dialog and the
// test for each spell that out.
const (
	recoverReleaseExpiredClaims  = "release_expired_claims"
	recoverReleaseAllClaims      = "release_all_claims"
	recoverRetryFailedEmbeddings = "retry_failed_embeddings"
	recoverResetStuckEncoding    = "reset_stuck_encoding"
)

// recoverRequest is the body of POST /admin/api/recover. Exactly one target
// per request: a recovery is one narrow action whose effect the operator reads
// in the confirmation and the refreshed counts. Accepting a list of targets
// would invite a "run everything" habit the confirm dialogs exist to prevent,
// and blur which flip produced which number.
type recoverRequest struct {
	Target string `json:"target"`
}

// adminRecover dispatches one recovery target to its admin action and answers
// with that action's typed result wrapped in the target it served. An unknown
// target is a caller bug: 400, with the valid values quoted so a stale
// dashboard build can be fixed from the error alone.
func (s *Server) adminRecover(w http.ResponseWriter, r *http.Request) {
	var req recoverRequest
	if err := decodeAdminJSONBody(w, r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}

	result := admin.RecoverResult{Target: req.Target}
	switch req.Target {
	case recoverReleaseExpiredClaims:
		res, err := s.admin.ReleaseStuckEmbeddings(r.Context(), true)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		result.Release = &res
	case recoverReleaseAllClaims:
		res, err := s.admin.ReleaseStuckEmbeddings(r.Context(), false)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		result.Release = &res
	case recoverRetryFailedEmbeddings:
		res, err := s.admin.RetryFailedEmbeddings(r.Context())
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		result.Retry = &res
	case recoverResetStuckEncoding:
		res, err := s.admin.ResetStuckEncodings(r.Context())
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
			return
		}
		result.Encoding = &res
	default:
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("unknown recovery target %q (want one of: %s, %s, %s, %s)",
				req.Target, recoverReleaseExpiredClaims, recoverReleaseAllClaims,
				recoverRetryFailedEmbeddings, recoverResetStuckEncoding))
		return
	}
	writeJSON(w, http.StatusOK, result)
}
