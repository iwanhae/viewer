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
	r.Get("/metrics", s.getMetrics)

	r.Route("/api", func(r chi.Router) {
		r.Post("/albums", s.createAlbum)
		r.Post("/albums/{albumId}/multipart/initiate", s.initiateAlbumMultipart)
		r.Post("/albums/{albumId}/multipart/part-url", s.presignAlbumMultipartPart)
		r.Post("/albums/{albumId}/multipart/complete", s.completeAlbumMultipart)
		r.Post("/albums/{albumId}/multipart/abort", s.abortAlbumMultipart)
		r.Get("/albums", s.listAlbums)
		r.Get("/albums/search", s.searchAlbums)
		r.Post("/albums/{albumId}/finalize", s.finalizeAlbum)
		r.Get("/albums/{albumId}/status", s.getAlbumStatus)
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

type initiateMultipartRequest struct {
	SizeBytes   int64  `json:"sizeBytes"`
	ContentType string `json:"contentType,omitempty"`
}

type presignMultipartPartRequest struct {
	UploadID   string `json:"uploadId"`
	PartNumber int32  `json:"partNumber"`
}

type completeMultipartRequest struct {
	UploadID string                `json:"uploadId"`
	Parts    []completePartRequest `json:"parts,omitempty"`
}

type completePartRequest struct {
	PartNumber int32  `json:"partNumber"`
	ETag       string `json:"etag"`
}

type abortMultipartRequest struct {
	UploadID string `json:"uploadId"`
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
			"strategy":      res.Strategy,
			"key":           res.Key,
			"partSizeBytes": res.PartSizeBytes,
			"maxParts":      res.MaxParts,
		},
	})
}

func (s *Server) initiateAlbumMultipart(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	var req initiateMultipartRequest
	if err := jsonBody(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	res, err := s.albums.InitiateMultipartUpload(r.Context(), albumID, req.SizeBytes, req.ContentType)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uploadId":      res.UploadID,
		"partSizeBytes": res.PartSizeBytes,
		"partCount":     res.PartCount,
	})
}

func (s *Server) presignAlbumMultipartPart(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	var req presignMultipartPartRequest
	if err := jsonBody(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	res, err := s.albums.PresignMultipartUploadPart(r.Context(), albumID, req.UploadID, req.PartNumber)
	if err != nil {
		status := http.StatusBadRequest
		code := "INVALID_REQUEST"
		if errors.Is(err, albums.ErrMultipartNotFound) {
			status = http.StatusNotFound
			code = "NOT_FOUND"
		}
		writeError(w, r, status, code, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url":     res.URL,
		"headers": res.Headers,
	})
}

func (s *Server) completeAlbumMultipart(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	var req completeMultipartRequest
	if err := jsonBody(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	parts := make([]albums.CompletePart, 0, len(req.Parts))
	for _, part := range req.Parts {
		parts = append(parts, albums.CompletePart{
			PartNumber: part.PartNumber,
			ETag:       part.ETag,
		})
	}

	if err := s.albums.CompleteMultipartUpload(r.Context(), albumID, req.UploadID, parts); err != nil {
		status := http.StatusBadRequest
		code := "INVALID_REQUEST"
		if errors.Is(err, albums.ErrMultipartNotFound) {
			status = http.StatusNotFound
			code = "NOT_FOUND"
		}
		writeError(w, r, status, code, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "UPLOADED"})
}

func (s *Server) abortAlbumMultipart(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	var req abortMultipartRequest
	if err := jsonBody(r, &req); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
		return
	}

	if err := s.albums.AbortMultipartUpload(r.Context(), albumID, req.UploadID); err != nil {
		status := http.StatusBadRequest
		code := "INVALID_REQUEST"
		if errors.Is(err, albums.ErrMultipartNotFound) {
			status = http.StatusNotFound
			code = "NOT_FOUND"
		}
		writeError(w, r, status, code, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ABORTED"})
}

func (s *Server) finalizeAlbum(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	finalizeStatus, err := s.albums.RequestFinalize(r.Context(), albumID)
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
	if s.recommend != nil {
		s.recommend.ProcessAlbumAsync(albumID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":     finalizeStatus.Status,
		"attempt":    finalizeStatus.Attempt,
		"lastError":  finalizeStatus.LastError,
		"photoCount": finalizeStatus.PhotoCount,
		"updatedAt":  finalizeStatus.UpdatedAt,
	})
}

func (s *Server) getAlbumStatus(w http.ResponseWriter, r *http.Request) {
	albumID := chi.URLParam(r, "albumId")
	status, err := s.albums.GetFinalizeStatus(r.Context(), albumID)
	if err != nil {
		httpStatus := http.StatusInternalServerError
		code := "INTERNAL"
		if errors.Is(err, albums.ErrAlbumNotFound) || errors.Is(err, albums.ErrAlbumSourceNotFound) {
			httpStatus = http.StatusNotFound
			code = "NOT_FOUND"
		}
		writeError(w, r, httpStatus, code, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     status.Status,
		"attempt":    status.Attempt,
		"lastError":  status.LastError,
		"photoCount": status.PhotoCount,
		"updatedAt":  status.UpdatedAt,
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
	_, _ = fmt.Fprintf(w, "# HELP viewer_embedding_images_processed Number of images processed by embedding workers.\n")
	_, _ = fmt.Fprintf(w, "# TYPE viewer_embedding_images_processed gauge\n")
	_, _ = fmt.Fprintf(w, "viewer_embedding_images_processed %d\n", progress.Processed)
	_, _ = fmt.Fprintf(w, "# HELP viewer_embedding_progress_ratio Ready embeddings divided by total images.\n")
	_, _ = fmt.Fprintf(w, "# TYPE viewer_embedding_progress_ratio gauge\n")
	_, _ = fmt.Fprintf(w, "viewer_embedding_progress_ratio %.6f\n", progress.Ratio)
	_, _ = fmt.Fprintf(w, "# HELP viewer_embedding_progress_percent Ready embeddings as percentage of total images.\n")
	_, _ = fmt.Fprintf(w, "# TYPE viewer_embedding_progress_percent gauge\n")
	_, _ = fmt.Fprintf(w, "viewer_embedding_progress_percent %.6f\n", progress.Percent)
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
