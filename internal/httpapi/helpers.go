package httpapi

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details any    `json:"details,omitempty"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encode response failed: %v", err)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code string, message string) {
	writeErrorDetailed(w, r, status, code, message, nil)
}

func writeErrorDetailed(w http.ResponseWriter, r *http.Request, status int, code string, message string, details any) {
	if status >= http.StatusInternalServerError {
		reqID := "n/a"
		if r != nil {
			reqID = middleware.GetReqID(r.Context())
		}
		path := "n/a"
		query := "n/a"
		method := "n/a"
		remote := "n/a"
		if r != nil {
			if r.URL != nil {
				path = r.URL.Path
				query = r.URL.RawQuery
			}
			method = r.Method
			remote = r.RemoteAddr
		}
		log.Printf("[http] internal_error status=%d code=%s message=%q method=%s path=%s query=%q request_id=%s remote=%s", status, code, message, method, path, query, reqID, remote)
	}

	var b errorBody
	b.Error.Code = code
	b.Error.Message = message
	b.Error.Details = details
	writeJSON(w, status, b)
}
