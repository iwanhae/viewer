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
	"viewer/internal/feed"
	"viewer/internal/images"
	"viewer/internal/models"
	"viewer/internal/recommend"
	"viewer/internal/web"
)

type Server struct {
	albums         *albums.Service
	feed           *feed.Service
	images         *images.Service
	recommend      *recommend.Service
	maxUploadBytes int64
}

func New(albumsService *albums.Service, feedService *feed.Service, imageService *images.Service, recommendService *recommend.Service, maxUploadBytes int64) *Server {
	return &Server{
		albums:         albumsService,
		feed:           feedService,
		images:         imageService,
		recommend:      recommendService,
		maxUploadBytes: maxUploadBytes,
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

	r.Route("/api", func(r chi.Router) {
		r.Post("/albums", s.createAlbum)
		r.Post("/albums/{albumId}/upload", s.uploadAlbumSource)
		r.Get("/albums", s.listAlbums)
		r.Get("/albums/search", s.searchAlbums)
		r.Post("/albums/{albumId}/finalize", s.finalizeAlbum)
		r.Get("/albums/{albumId}", s.getAlbum)
		r.Get("/feed", s.getFeed)
		r.Get("/image/{albumId}/{index}", s.getImage)
		r.Get("/recommendations/{albumId}/{index}", s.getRecommendations)
		r.Get("/debug/range-cache-stats", s.getRangeCacheStats)
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
		"albumId": res.AlbumID,
		"upload": map[string]any{
			"method":  "PUT",
			"url":     res.URL,
			"headers": res.Headers,
		},
	})
}

func (s *Server) uploadAlbumSource(w http.ResponseWriter, r *http.Request) {
	if s.maxUploadBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes)
	}

	albumID := chi.URLParam(r, "albumId")
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", "missing file form field")
		return
	}
	defer file.Close()

	if err := s.albums.UploadSource(r.Context(), albumID, header.Filename, file); err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "UPLOADED"})
}

func (s *Server) finalizeAlbum(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	idx, err := s.albums.Finalize(r.Context(), albumID)
	if err != nil {
		status := http.StatusInternalServerError
		code := "INDEXING_FAILED"
		if errors.Is(err, albums.ErrAlbumNotFound) || errors.Is(err, albums.ErrAlbumSourceNotFound) {
			status = http.StatusNotFound
			code = "NOT_FOUND"
		}
		if errors.Is(err, albums.ErrNoValidImages) {
			status = http.StatusUnprocessableEntity
		}
		writeError(w, r, status, code, err.Error())
		return
	}
	s.recommend.NotifyAlbumFinalized(r.Context(), albumID)

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "READY",
		"photoCount": idx.PhotoCount,
	})
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

func (s *Server) listAlbums(w http.ResponseWriter, r *http.Request) {
	albumsList, err := s.albums.ListAlbums(r.Context())
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"albums": albumsList})
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

	resp, err := s.feed.Build(
		r.Context(),
		limit,
		r.URL.Query().Get("cursor"),
		r.URL.Query().Get("seed"),
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
	result, err := s.images.GetImage(r.Context(), albumID, idx)
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

	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(result.Bytes); err != nil {
		log.Printf("write image response failed: %v", err)
	}
}

func (s *Server) getRecommendations(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) getRangeCacheStats(w http.ResponseWriter, r *http.Request) {
	stats := s.images.RangeCacheStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"fetchRequests": stats.FetchRequests,
		"fetchBytes":    stats.FetchBytes,
		"cacheHits":     stats.CacheHits,
		"cacheMisses":   stats.CacheMisses,
		"readErrors":    stats.ReadErrors,
		"loadedBytes":   stats.LoadedBytes,
	})
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

func parsePathIntParam(r *http.Request, key string) (int, error) {
	raw := chi.URLParam(r, key)
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func parseNonNegativePathIntParam(r *http.Request, key string) (int, error) {
	value, err := parsePathIntParam(r, key)
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

func Warmup(ctx context.Context, albumsService *albums.Service, recommendService *recommend.Service) {
	startedAt := time.Now()
	log.Printf("album cache warmup started")

	loadedCount := 0
	failedCount := 0
	if err := albumsService.RefreshFromStorageWithProgressAndAlbum(
		ctx,
		func(progress albums.RefreshProgress) {
			stage := "listing"
			if progress.ListingDone {
				stage = "draining"
			}
			if progress.Err != nil {
				failedCount++
				log.Printf(
					"album cache warmup stage=%s discovered=%d processed=%d succeeded=%d failed=%d status=failed key=%s err=%v",
					stage,
					progress.Discovered,
					progress.Processed,
					progress.Succeeded,
					progress.Failed,
					progress.Key,
					progress.Err,
				)
				return
			}

			loadedCount++
			log.Printf(
				"album cache warmup stage=%s discovered=%d processed=%d succeeded=%d failed=%d status=ok album_id=%s",
				stage,
				progress.Discovered,
				progress.Processed,
				progress.Succeeded,
				progress.Failed,
				progress.AlbumID,
			)
		},
		func(idx models.AlbumIndex) {
			recommendService.IngestAlbumIndex(idx)
		},
	); err != nil {
		log.Printf("album cache warmup skipped: %v", err)
		return
	}

	log.Printf(
		"album cache warmup finished loaded=%d failed=%d duration=%s",
		loadedCount,
		failedCount,
		time.Since(startedAt).Round(time.Millisecond),
	)
}
