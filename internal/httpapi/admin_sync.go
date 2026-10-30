package httpapi

import (
	"net/http"
)

// adminVectorSync answers POST /admin/api/vector-sync with a strict `{}`
// JSON body (the same decode every admin action uses: JSON content type
// required, unknown fields rejected). It runs one vector-store
// reconciliation pass — diff the catalog's expected points against the
// store's actual ones and re-queue the blobs whose ready points are
// missing — and replies 200 with what the pass found and did; started=false
// means another pass was already running and this request did no work, the
// same double-click contract the local encoding drain uses. The call is
// synchronous and takes tens of seconds (typically 15-45s at ~312k photos),
// and it intentionally keeps running server-side even if the client hangs
// up: the work context is the service lifetime, not the request, so the
// completion line lands in the server log either way.
func (s *Server) adminVectorSync(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if err := decodeAdminJSONBody(w, r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	result, err := s.admin.SyncVectors(r.Context())
	if err != nil {
		writeAdminLocalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
