package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"viewer/internal/admin"
	"viewer/internal/catalog"
)

func (s *Server) adminPhotos(w http.ResponseWriter, r *http.Request) {
	query, err := adminQuery(r, "q", "albumId", "hash", "encoding", "embedding", "encodingRaw", "embeddingRaw", "skippedReason", "limit", "cursor")
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	limit, err := browseLimit(query)
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	page, err := s.admin.ListPhotos(r.Context(), admin.PhotoFilters{
		Query: query.Get("q"), AlbumID: query.Get("albumId"), Hash: query.Get("hash"),
		Encoding: query.Get("encoding"), Embedding: query.Get("embedding"),
		EncodingRaw: query.Get("encodingRaw"), EmbeddingRaw: query.Get("embeddingRaw"),
		SkippedReason: query.Get("skippedReason"),
	}, admin.BrowsePageOptions{Limit: limit, Cursor: query.Get("cursor")})
	if err != nil {
		writeBrowseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) adminPhoto(w http.ResponseWriter, r *http.Request) {
	if err := rejectQueryParams(r); err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	index, err := parseNonNegativePathIntParam(r, "idx")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid photo index")
		return
	}
	photo, err := s.admin.PhotoAt(r.Context(), chi.URLParam(r, "albumId"), index)
	if err != nil {
		writeBrowseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, photo)
}

func (s *Server) adminBlobs(w http.ResponseWriter, r *http.Request) {
	query, err := adminQuery(r, "albumId", "hash", "encoding", "embedding", "encodingRaw", "embeddingRaw", "skippedReason", "unreferenced", "limit", "cursor")
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	limit, err := browseLimit(query)
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	var unreferenced *bool
	if query.Has("unreferenced") {
		value, parseErr := strconv.ParseBool(query.Get("unreferenced"))
		if parseErr != nil || (query.Get("unreferenced") != "true" && query.Get("unreferenced") != "false") {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "unreferenced must be true or false")
			return
		}
		unreferenced = &value
	}
	page, err := s.admin.ListBlobs(r.Context(), admin.BlobFilters{
		AlbumID: query.Get("albumId"), Hash: query.Get("hash"),
		Encoding: query.Get("encoding"), Embedding: query.Get("embedding"),
		EncodingRaw: query.Get("encodingRaw"), EmbeddingRaw: query.Get("embeddingRaw"),
		SkippedReason: query.Get("skippedReason"), Unreferenced: unreferenced,
	}, admin.BrowsePageOptions{Limit: limit, Cursor: query.Get("cursor")})
	if err != nil {
		writeBrowseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) adminBlob(w http.ResponseWriter, r *http.Request) {
	if err := rejectQueryParams(r); err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	blob, err := s.admin.BlobByHash(r.Context(), chi.URLParam(r, "hash"))
	if err != nil {
		writeBrowseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, blob)
}

func (s *Server) adminBlobHistory(w http.ResponseWriter, r *http.Request) {
	query, err := adminQuery(r, "stage", "limit", "cursor")
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	limit, err := browseLimit(query)
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	page, err := s.admin.BlobHistory(r.Context(), chi.URLParam(r, "hash"), admin.HistoryOptions{
		Stage: query.Get("stage"), Limit: limit, Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeBrowseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) adminAlbums(w http.ResponseWriter, r *http.Request) {
	query, err := adminQuery(r, "q", "status", "limit", "cursor")
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	limit, err := browseLimit(query)
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	page, err := s.admin.ListAlbums(r.Context(), admin.AlbumFilters{Query: query.Get("q"), Status: query.Get("status")}, admin.BrowsePageOptions{Limit: limit, Cursor: query.Get("cursor")})
	if err != nil {
		writeBrowseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) adminAlbum(w http.ResponseWriter, r *http.Request) {
	if err := rejectQueryParams(r); err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	album, err := s.admin.AlbumByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeBrowseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, album)
}

func (s *Server) adminAlbumExtractions(w http.ResponseWriter, r *http.Request) {
	query, err := adminQuery(r, "limit", "cursor")
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	limit, err := browseLimit(query)
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	page, err := s.admin.ExtractionRuns(r.Context(), chi.URLParam(r, "id"), admin.BrowsePageOptions{Limit: limit, Cursor: query.Get("cursor")})
	if err != nil {
		writeBrowseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) adminAlbumEntries(w http.ResponseWriter, r *http.Request) {
	query, err := adminQuery(r, "runId", "outcome", "limit", "cursor")
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	limit, err := browseLimit(query)
	if err != nil {
		writeBrowseBadRequest(w, r, err)
		return
	}
	var runID int64
	if query.Has("runId") {
		runID, err = strconv.ParseInt(query.Get("runId"), 10, 64)
		if err != nil || runID <= 0 {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "runId must be a positive integer")
			return
		}
	}
	page, err := s.admin.ExtractionEntries(r.Context(), chi.URLParam(r, "id"), admin.ExtractionEntriesOptions{
		RunID: runID, Outcome: query.Get("outcome"), Limit: limit, Cursor: query.Get("cursor"),
	})
	if err != nil {
		writeBrowseError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func adminQuery(r *http.Request, allowed ...string) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid query string: %w", err)
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = true
	}
	for key, values := range query {
		if !allowedSet[key] {
			return nil, fmt.Errorf("unsupported query parameter %q", key)
		}
		if len(values) != 1 {
			return nil, fmt.Errorf("query parameter %q must appear once", key)
		}
		if key != "q" && values[0] == "" {
			return nil, fmt.Errorf("query parameter %q must not be empty", key)
		}
	}
	return query, nil
}

func rejectQueryParams(r *http.Request) error {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return fmt.Errorf("invalid query string: %w", err)
	}
	if len(query) != 0 {
		return fmt.Errorf("query parameters are not supported on this endpoint")
	}
	return nil
}

func browseLimit(query url.Values) (int, error) {
	if !query.Has("limit") {
		return admin.DefaultBrowseLimit, nil
	}
	limit, err := strconv.Atoi(strings.TrimSpace(query.Get("limit")))
	if err != nil || limit < 1 || limit > admin.MaxBrowseLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", admin.MaxBrowseLimit)
	}
	return limit, nil
}

func writeBrowseBadRequest(w http.ResponseWriter, r *http.Request, err error) {
	writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
}

func writeBrowseError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, admin.ErrInvalidBrowseRequest) {
		writeBrowseBadRequest(w, r, err)
		return
	}
	if errors.Is(err, catalog.ErrExtractionRunInProgress) {
		writeError(w, r, http.StatusConflict, "EXTRACTION_IN_PROGRESS", "Extraction is still running; entry outcomes can be browsed after it reaches a terminal status.")
		return
	}
	if errors.Is(err, catalog.ErrAlbumNotFound) || errors.Is(err, catalog.ErrPhotoNotFound) || errors.Is(err, catalog.ErrBlobNotFound) || errors.Is(err, catalog.ErrExtractionRunNotFound) {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "resource not found")
		return
	}
	writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
}
