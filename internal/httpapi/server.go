package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"viewer/internal/albums"
	"viewer/internal/catalog"
	"viewer/internal/feed"
	"viewer/internal/images"
	"viewer/internal/recommend"
	"viewer/internal/web"
)

type Server struct {
	albums    *albums.Service
	feed      *feed.Service
	images    *images.Service
	recommend *recommend.Service
}

func New(albumsService *albums.Service, feedService *feed.Service, imageService *images.Service, recommendService *recommend.Service) *Server {
	return &Server{
		albums:    albumsService,
		feed:      feedService,
		images:    imageService,
		recommend: recommendService,
	}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(recovererWithLog)
	r.Use(middleware.Logger)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	r.Get("/metrics", s.getMetrics)

	r.Route("/api", func(r chi.Router) {
		r.Post("/albums", s.createAlbum)
		r.Get("/albums/search", s.searchAlbums)
		r.Post("/albums/{albumId}/finalize", s.finalizeAlbum)
		r.Get("/albums/{albumId}/finalize", s.getFinalizeStatus)
		r.Get("/albums/{albumId}", s.getAlbum)
		r.Get("/embedding", s.getEmbeddingStatus)
		r.Get("/feed", s.getFeed)
		r.Get("/image/{albumId}/{index}", s.getImage)
		r.Get("/recommendations/{albumId}/{index}", s.getRecommendations)
	})

	staticHandler := web.Handler()
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "resource not found")
			return
		}
		staticHandler.ServeHTTP(w, r)
	})
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		staticHandler.ServeHTTP(w, r)
	})

	return r
}

type createAlbumRequest struct {
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"sizeBytes"`
}

func (s *Server) createAlbum(w http.ResponseWriter, r *http.Request) {
	var req createAlbumRequest
	if err := jsonBody(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	res, err := s.albums.CreateUpload(r.Context(), req.Filename, req.SizeBytes)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"albumId":       res.AlbumID,
		"uploadUrl":     res.UploadURL,
		"uploadHeaders": res.Headers,
		"objectKey":     res.Key,
	})
}

type finalizeResponse struct {
	albums.FinalizeState
	// Embedding is the album's blob coverage. It is absent when the server has
	// no recommendation service, which tells the client not to wait for
	// embeddings it will never get.
	Embedding *recommend.EmbeddingProgress `json:"embedding,omitempty"`
}

// finalizePayload adds embedding coverage to a finalize state, so a client that
// has just uploaded can tell "indexed but still embedding" apart from "indexed
// and fully embedded".
func (s *Server) finalizePayload(ctx context.Context, state albums.FinalizeState) finalizeResponse {
	payload := finalizeResponse{FinalizeState: state}
	if s.recommend == nil {
		return payload
	}
	progress, err := s.recommend.AlbumEmbeddingProgress(ctx, state.AlbumID)
	if err != nil {
		log.Printf("finalize: album=%s embedding progress failed: %v", state.AlbumID, err)
		return payload
	}
	payload.Embedding = &progress
	return payload
}

func (s *Server) finalizeAlbum(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	state, err := s.albums.RequestFinalize(r.Context(), albumID)
	if err != nil {
		status := http.StatusInternalServerError
		code := "INTERNAL"
		if errors.Is(err, albums.ErrAlbumNotFound) || errors.Is(err, albums.ErrAlbumSourceNotFound) {
			status = http.StatusNotFound
			code = "NOT_FOUND"
		}
		writeError(w, r, status, code, err.Error())
		return
	}

	status := http.StatusAccepted
	if state.Status == catalog.AlbumStatusReady {
		status = http.StatusOK
	}
	writeJSON(w, status, s.finalizePayload(r.Context(), state))
}

func (s *Server) getFinalizeStatus(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	state, err := s.albums.GetFinalizeStatus(r.Context(), albumID)
	if err != nil {
		status := http.StatusInternalServerError
		code := "INTERNAL"
		if errors.Is(err, albums.ErrAlbumNotFound) {
			status = http.StatusNotFound
			code = "NOT_FOUND"
		}
		writeError(w, r, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.finalizePayload(r.Context(), state))
}

func (s *Server) getAlbum(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	idx, err := s.albums.GetAlbum(r.Context(), albumID)
	if err != nil {
		if errors.Is(err, albums.ErrAlbumNotFound) {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "album not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, idx)
}

func (s *Server) searchAlbums(w http.ResponseWriter, r *http.Request) {
	limit, err := parseOptionalIntQuery(r, "limit", 20, 1, 100)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid limit")
		return
	}

	results, err := s.albums.SearchAlbumsByNamePrefix(r.Context(), r.URL.Query().Get("q"), limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": results})
}

func (s *Server) getFeed(w http.ResponseWriter, r *http.Request) {
	limit, err := parseOptionalIntQuery(r, "limit", 80, 1, 200)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid limit")
		return
	}
	mode, err := feed.ParseMode(r.URL.Query().Get("mode"))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid mode")
		return
	}

	resp, err := s.feed.Build(
		limit,
		r.URL.Query().Get("seed"),
		mode,
		r.URL.Query().Get("after"),
	)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getImage(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	idx, err := parseNonNegativePathIntParam(r, "index")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid image index")
		return
	}
	stream, err := s.images.OpenImage(r.Context(), albumID, idx)
	if err != nil {
		status := http.StatusInternalServerError
		code := "INTERNAL"
		if errors.Is(err, albums.ErrAlbumNotFound) ||
			errors.Is(err, albums.ErrAlbumSourceNotFound) ||
			errors.Is(err, images.ErrPhotoIndexOutOfRange) ||
			errors.Is(err, images.ErrImageEntryNotFound) {
			status = http.StatusNotFound
			code = "NOT_FOUND"
		}
		writeError(w, r, status, code, err.Error())
		return
	}
	defer stream.Close()

	// An album id plus a photo index is immutable, so the content hash of the
	// blob is a stable validator for the response.
	w.Header().Set("Content-Type", stream.ContentType)
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("ETag", `"`+stream.Hash+`"`)
	// A zero modtime keeps ServeContent from emitting a Last-Modified header;
	// it still negotiates Content-Length, Range and If-None-Match.
	recorder := &writeErrorRecorder{ResponseWriter: w}
	http.ServeContent(recorder, r, "", time.Time{}, stream.Content)
	if recorder.err != nil {
		log.Printf("write image response failed: %v", recorder.err)
	}
}

func (s *Server) getRecommendations(w http.ResponseWriter, r *http.Request) {
	// The endpoint stays available even when the embedding model could not be
	// loaded: the catalog simply has no embeddings to match against yet.
	if s.recommend == nil {
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "recommendations are not available")
		return
	}

	albumID := chi.URLParam(r, "albumId")
	idx, err := parseNonNegativePathIntParam(r, "index")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid image index")
		return
	}
	limit, err := parseOptionalIntQuery(r, "limit", 12, 1, 200)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid limit")
		return
	}
	result, err := s.recommend.Recommend(r.Context(), albumID, idx, limit)
	if err != nil {
		if errors.Is(err, recommend.ErrPhotoNotFound) {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "photo not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// getEmbeddingStatus reports embedding coverage across the whole catalog. It
// stays available without a recommendation service and simply reports zeros,
// which the clients read as "nothing is being embedded".
func (s *Server) getEmbeddingStatus(w http.ResponseWriter, r *http.Request) {
	progress := recommend.EmbeddingProgress{}
	if s.recommend != nil {
		progress = s.recommend.EmbeddingProgress()
	}
	writeJSON(w, http.StatusOK, progress)
}

func (s *Server) getMetrics(w http.ResponseWriter, r *http.Request) {
	progress := recommend.EmbeddingProgress{}
	if s.recommend != nil {
		progress = s.recommend.EmbeddingProgress()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	_, _ = fmt.Fprintf(w, "# HELP viewer_embedding_images_total Total number of images tracked by the embedding pipeline.\n")
	_, _ = fmt.Fprintf(w, "# TYPE viewer_embedding_images_total gauge\n")
	_, _ = fmt.Fprintf(w, "viewer_embedding_images_total %d\n", progress.Total)
	_, _ = fmt.Fprintf(w, "# HELP viewer_embedding_images_ready Number of images with ready embeddings.\n")
	_, _ = fmt.Fprintf(w, "# TYPE viewer_embedding_images_ready gauge\n")
	_, _ = fmt.Fprintf(w, "viewer_embedding_images_ready %d\n", progress.Ready)
	_, _ = fmt.Fprintf(w, "# HELP viewer_embedding_images_failed Number of images with failed embeddings.\n")
	_, _ = fmt.Fprintf(w, "# TYPE viewer_embedding_images_failed gauge\n")
	_, _ = fmt.Fprintf(w, "viewer_embedding_images_failed %d\n", progress.Failed)
	_, _ = fmt.Fprintf(w, "# HELP viewer_embedding_images_pending Number of images pending embedding.\n")
	_, _ = fmt.Fprintf(w, "# TYPE viewer_embedding_images_pending gauge\n")
	_, _ = fmt.Fprintf(w, "viewer_embedding_images_pending %d\n", progress.Pending)
	_, _ = fmt.Fprintf(w, "# HELP viewer_embedding_progress_ratio Ready embeddings divided by total images.\n")
	_, _ = fmt.Fprintf(w, "# TYPE viewer_embedding_progress_ratio gauge\n")
	_, _ = fmt.Fprintf(w, "viewer_embedding_progress_ratio %.6f\n", progress.Ratio)
}

func recovererWithLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				reqID := middleware.GetReqID(r.Context())
				log.Printf("[panic] request_id=%s method=%s path=%s query=%q remote=%s panic=%v stack=%s", reqID, r.Method, r.URL.Path, r.URL.RawQuery, r.RemoteAddr, rec, strings.TrimSpace(string(debug.Stack())))
				writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func jsonBody(r *http.Request, out any) error {
	if r.Body == nil {
		return fmt.Errorf("missing body")
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid body: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("invalid body: unexpected trailing content")
	}
	return nil
}

func parseNonNegativePathIntParam(r *http.Request, key string) (int, error) {
	value, err := strconv.Atoi(chi.URLParam(r, key))
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, fmt.Errorf("invalid %s: negative", key)
	}
	return value, nil
}

func parseOptionalIntQuery(r *http.Request, key string, defaultValue int, min int, max int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	if value < min {
		return 0, fmt.Errorf("invalid %s: too small", key)
	}
	if max > 0 && value > max {
		return 0, fmt.Errorf("invalid %s: too large", key)
	}
	return value, nil
}
