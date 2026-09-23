package httpapi

import (
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
	"viewer/internal/admin"
	"viewer/internal/albums"
	"viewer/internal/catalog"
	"viewer/internal/feed"
	"viewer/internal/images"
	"viewer/internal/recommend"
	"viewer/internal/web"
)

type Server struct {
	albums      *albums.Service
	feed        *feed.Service
	images      *images.Service
	recommend   *recommend.Service
	workerToken string
	admin       *admin.Service
	adminToken  string
}

// New wires the API together. An empty workerToken runs the external worker
// endpoints without auth, which is only sensible on a trusted network. The
// admin surface only registers when both an adminService and an adminToken are
// supplied: a dashboard that answered without its credential would be an
// unauthenticated view of the whole library, so either half missing simply
// leaves /admin a 404.
func New(albumsService *albums.Service, feedService *feed.Service, imageService *images.Service, recommendService *recommend.Service, workerToken string, adminService *admin.Service, adminToken string) *Server {
	return &Server{
		albums:      albumsService,
		feed:        feedService,
		images:      imageService,
		recommend:   recommendService,
		workerToken: workerToken,
		admin:       adminService,
		adminToken:  adminToken,
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
		r.Get("/feed", s.getFeed)
		r.Get("/image/{hash}", s.getImageByHash)
		r.Head("/image/{hash}", s.getImageByHash)
		r.Get("/photos/search", s.searchPhotos)
		r.Get("/recommendations/{albumId}/{index}", s.getRecommendations)
	})

	// The worker endpoints are the write surface for external embedding
	// fleets, so they carry their own token check.
	r.Group(func(r chi.Router) {
		r.Use(s.requireWorkerToken)
		r.Post("/api/embedding/claim", s.claimEmbeddings)
		r.Post("/api/embedding/renew", s.renewLeases)
		r.Post("/api/embedding/results", s.postEmbeddingResults)
	})

	// The admin surface is the operator's dashboard and the re-embed recovery
	// trigger. It is registered only when both the service and its token
	// exist — see New — so a deployment without ADMIN_TOKEN gets a plain 404
	// instead of an unauthenticated dashboard.
	if s.admin != nil && s.adminToken != "" {
		r.Route("/admin", func(r chi.Router) {
			r.Use(s.requireAdminToken)
			r.Get("/", admin.Page().ServeHTTP)
			r.Get("/api/stats", s.adminStats)
			r.Post("/api/reindex", s.adminReindex)
		})
	}

	staticHandler := web.Handler()
	// serveAPINotFound answers misses on the machine-facing surfaces with
	// JSON. /admin joins /api there when the admin surface is disabled: the
	// SPA fallback would otherwise serve the frontend's index.html for it,
	// which reads as "the page exists but is broken" when the truth is "this
	// deployment has no admin UI".
	serveAPINotFound := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || (!s.adminEnabled() && isAdminPath(r.URL.Path)) {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "resource not found")
			return
		}
		staticHandler.ServeHTTP(w, r)
	}
	r.NotFound(serveAPINotFound)
	// A method no registered route carries gets chi's bare 405 before NotFound
	// ever runs. For a disabled admin surface that bare 405 would advertise
	// that /admin exists under some other method, so it is folded into the
	// same 404 the missing route produces; every other path keeps the default.
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		if !s.adminEnabled() && isAdminPath(r.URL.Path) {
			serveAPINotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		if !s.adminEnabled() && isAdminPath(r.URL.Path) {
			serveAPINotFound(w, r)
			return
		}
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
		"albumId":   res.AlbumID,
		"uploadUrl": res.UploadURL,
		"objectKey": res.Key,
	})
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
	writeJSON(w, status, state)
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
	writeJSON(w, http.StatusOK, state)
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

	results, err := s.albums.SearchAlbumsByName(r.Context(), r.URL.Query().Get("q"), limit)
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
		r.Context(),
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

func (s *Server) getImageByHash(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimSpace(chi.URLParam(r, "hash"))
	if hash == "" {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "resource not found")
		return
	}
	// An absent w serves the untouched original; a w on the width ladder asks
	// for a scaled variant. Ladder membership below is the whole range check —
	// there is no separate upper bound — so only parse failures (non-numeric,
	// zero, negative) reach this first error.
	width, err := parseOptionalIntQuery(r, "w", 0, 1, 0)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid image width")
		return
	}
	if width != 0 && !images.IsSupportedWidth(width) {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", fmt.Sprintf("unsupported width %d, supported: %v", width, images.WidthLadder()))
		return
	}
	var stream *images.ImageStream
	if width == 0 {
		stream, err = s.images.OpenImageByHash(r.Context(), hash)
	} else {
		stream, err = s.images.OpenImageByHashScaled(r.Context(), hash, width)
	}
	if err != nil {
		if errors.Is(err, images.ErrImageEntryNotFound) {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", err.Error())
			return
		}
		// This is the one anonymous endpoint that takes an arbitrary key, so
		// the raw storage error (endpoints, bucket, request ids) stays in the
		// log instead of the response.
		log.Printf("image fetch failed hash=%s: %v", hash, err)
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "image is temporarily unavailable")
		return
	}
	defer stream.Close()

	// The content hash is immutable, so it is a stable validator for the
	// response; scaled variants append their width.
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
		if errors.Is(err, recommend.ErrVectorStoreUnavailable) {
			// Qdrant is not configured, which is a deployment state rather
			// than a failure: the same 503 shape as the nil-service case
			// above, so clients treat both identically.
			writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "recommendations are not available")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// searchPhotos answers GET /api/photos/search?q=...&limit=... with the photos
// whose embeddings are nearest to the natural-language query.
func (s *Server) searchPhotos(w http.ResponseWriter, r *http.Request) {
	// The endpoint stays available even when the embedding model could not be
	// loaded: the catalog simply has no embeddings to match against yet.
	if s.recommend == nil {
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "photo search is not available")
		return
	}

	limit, err := parseOptionalIntQuery(r, "limit", 24, 1, 96)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "invalid limit")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "missing query")
		return
	}
	result, err := s.recommend.Search(r.Context(), q, limit)
	if err != nil {
		if errors.Is(err, recommend.ErrEmptyQuery) {
			writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "missing query")
			return
		}
		if errors.Is(err, recommend.ErrVectorStoreUnavailable) || errors.Is(err, recommend.ErrTextEmbeddingUnavailable) {
			// Qdrant not configured, or an embedder without a text tower, are
			// deployment states rather than failures: the same 503 shape as
			// the nil-service case above, so clients treat all three alike.
			writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "photo search is not available")
			return
		}
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
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
	_, _ = fmt.Fprintf(w, "# HELP viewer_embedding_images_processing Number of images currently leased by an embedding worker.\n")
	_, _ = fmt.Fprintf(w, "# TYPE viewer_embedding_images_processing gauge\n")
	_, _ = fmt.Fprintf(w, "viewer_embedding_images_processing %d\n", progress.Processing)
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
