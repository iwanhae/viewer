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
// The dashboard's bodyless buttons (retry-failed, encoding/reset) post with no
// body at all, and a missing field must read as "operator did not ask for the
// variant", never as a 400.
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

// releaseEmbeddingsRequest is the body of POST /admin/api/embeddings/release.
// ExpiredOnly false — the zero value, and what an absent field decodes to —
// means force-release-all; the dashboard makes that a deliberate checkbox plus
// confirm rather than a default, because it is the variant that cancels live
// workers' in-flight work.
type releaseEmbeddingsRequest struct {
	ExpiredOnly bool `json:"expiredOnly"`
}

// adminReleaseEmbeddings hands stuck processing embeddings back to the pending
// queue and answers with how many moved plus the fresh embedding counts.
func (s *Server) adminReleaseEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req releaseEmbeddingsRequest
	if err := decodeAdminJSONBody(w, r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	result, err := s.admin.ReleaseStuckEmbeddings(r.Context(), req.ExpiredOnly)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// adminRetryFailed resets every failed embedding to pending and answers with
// how many moved plus the fresh embedding counts.
func (s *Server) adminRetryFailed(w http.ResponseWriter, r *http.Request) {
	result, err := s.admin.RetryFailedEmbeddings(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// adminResetEncodings returns stuck WebP encoding jobs to pending and answers
// with the two-pass reset counts plus the fresh encoding stats, so the
// response alone shows the queue the action drained.
func (s *Server) adminResetEncodings(w http.ResponseWriter, r *http.Request) {
	result, err := s.admin.ResetStuckEncodings(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
