package httpapi

import (
	"bytes"
	"crypto/subtle"
	"encoding/base64"
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
