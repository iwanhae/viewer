package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"viewer/internal/catalog"
	"viewer/internal/config"
	"viewer/internal/pipeline"
	"viewer/internal/recommend"
)

// The handlers in this file are the write surface for external embedding
// workers: they lease pending blobs in batches, fetch the image bytes straight
// from the bucket (via the presigned URL or their own credentials), and report
// vectors back. The in-process worker shares the exact same claim and
// write-back path underneath, so the two fleets can never double-embed a blob
// or diverge from the in-memory index.

// maxWorkerBodyBytes bounds a claim or write-back request. A full batch of
// 1024 vectors is about 4.2 MB, so 16 MB leaves room without letting one
// request balloon.
const maxWorkerBodyBytes = 16 << 20

// requireWorkerToken guards the worker endpoints with the shared bearer token.
// An empty token switches the check off.
func (s *Server) requireWorkerToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.workerToken != "" {
			// The scheme is case-insensitive per RFC 9110 11.6.1, so a
			// spec-compliant client is not rejected for its choice of case.
			const scheme = "Bearer "
			header := r.Header.Get("Authorization")
			presented := ""
			if len(header) > len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
				presented = header[len(scheme):]
			}
			if subtle.ConstantTimeCompare([]byte(presented), []byte(s.workerToken)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "missing or invalid worker token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// writeBodyError answers a failed request decode. An over-large body gets its
// own status so a worker can tell "shrink the batch" from "fix the JSON".
func writeBodyError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "request body exceeds the size limit")
		return
	}
	writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
}

type claimRequest struct {
	Limit int `json:"limit"`
}

type claimedBlob struct {
	Hash        string `json:"hash"`
	SizeBytes   int64  `json:"sizeBytes"`
	ContentType string `json:"contentType"`
	// BlobKey is the logical key in the bucket ("blobs/<hash>"), for workers
	// that hold their own object-store credentials.
	BlobKey string `json:"blobKey"`
	// GetURL is a short-lived presigned download, for workers that do not.
	GetURL string `json:"getUrl"`
}

type claimResponse struct {
	// EmbeddingDim is the vector length the write-back expects.
	EmbeddingDim int           `json:"embeddingDim"`
	LeaseUntil   time.Time     `json:"leaseUntil"`
	Claimed      []claimedBlob `json:"claimed"`
}

// claimEmbeddings leases a batch of pending blobs to the calling worker. The
// lease keeps other workers away until it expires or the worker renews it; an
// expired claim is simply reclaimed by the next claim call, so a dead worker
// cannot strand blobs.
func (s *Server) claimEmbeddings(w http.ResponseWriter, r *http.Request) {
	if s.recommend == nil || s.images == nil {
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "embedding workers are not available")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkerBodyBytes)
	var req claimRequest
	if err := jsonBody(r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}

	blobs, leaseUntil, err := s.recommend.ClaimEmbeddings(r.Context(), req.Limit)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	claimed := make([]claimedBlob, 0, len(blobs))
	leased := make([]string, 0, len(blobs))
	for _, blob := range blobs {
		url, err := s.images.PresignBlobURL(r.Context(), blob.Hash, config.PresignTTL)
		if err != nil {
			// The batch is already leased; hand it back so one failed presign
			// does not strand it until the lease expires. The request context
			// may be the reason the presign failed, so the release runs on its
			// own clock.
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if relErr := s.recommend.ReleaseClaims(releaseCtx, append(leased, blob.Hash)); relErr != nil {
				log.Printf("claim: release after presign failure failed blobs=%d: %v", len(blobs), relErr)
			}
			cancel()
			writeError(w, r, http.StatusInternalServerError, "INTERNAL", "could not presign blob downloads")
			return
		}
		claimed = append(claimed, claimedBlob{
			Hash:        blob.Hash,
			SizeBytes:   blob.SizeBytes,
			ContentType: blob.ContentType,
			BlobKey:     pipeline.BlobKey(blob.Hash),
			GetURL:      url,
		})
		leased = append(leased, blob.Hash)
	}
	writeJSON(w, http.StatusOK, claimResponse{
		EmbeddingDim: catalog.EmbeddingDim,
		LeaseUntil:   leaseUntil,
		Claimed:      claimed,
	})
}

type renewRequest struct {
	Hashes []string `json:"hashes"`
}

type renewResponse struct {
	LeaseUntil time.Time `json:"leaseUntil"`
	// Renewed lists the hashes whose lease actually moved. Anything missing was
	// not in the processing state anymore - its lease expired and another
	// worker took it, or a result already landed - so the calling worker must
	// stop working on it: its eventual write-back would be rejected as
	// not_claimed.
	Renewed []string `json:"renewed"`
}

// renewLeases extends the calling worker's lease over a batch, for workers
// whose processing time can outgrow one TTL.
func (s *Server) renewLeases(w http.ResponseWriter, r *http.Request) {
	if s.recommend == nil {
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "embedding workers are not available")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkerBodyBytes)
	var req renewRequest
	if err := jsonBody(r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	leaseUntil, renewed, err := s.recommend.RenewLeases(r.Context(), req.Hashes)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, renewResponse{LeaseUntil: leaseUntil, Renewed: renewed})
}

type embeddingResultPayload struct {
	Hash      string `json:"hash"`
	Status    string `json:"status"`
	VectorB64 string `json:"vectorB64"`
	Error     string `json:"error"`
}

type embeddingResultsRequest struct {
	Results []embeddingResultPayload `json:"results"`
}

type rejectedEmbeddingPayload struct {
	Hash   string `json:"hash"`
	Reason string `json:"reason"`
}

type embeddingResultsResponse struct {
	Updated  int                        `json:"updated"`
	Rejected []rejectedEmbeddingPayload `json:"rejected"`
}

var (
	errBadBase64  = errors.New("vector is not valid base64")
	errBadFraming = errors.New("vector bytes do not frame float32 values")
)

func rejectionReason(err error) string {
	if errors.Is(err, errBadBase64) {
		return catalog.EmbeddingRejectBadBase64
	}
	return catalog.EmbeddingRejectBadVector
}

// decodeVector turns the base64 little-endian float32 wire format into a
// vector. Length and finiteness checks happen one layer down, where the
// expected dimension is known.
func decodeVector(encoded string) ([]float32, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errBadBase64
	}
	if len(raw)%4 != 0 {
		return nil, errBadFraming
	}
	vector := make([]float32, len(raw)/4)
	for i := range vector {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return vector, nil
}

// postEmbeddingResults records a batch of outcomes. A result only ever lands
// on a blob still in the processing state, so nothing a worker reports can
// clobber an existing embedding or a blob nobody claimed. Rejections are
// listed per item with a reason; they are final, not retryable.
func (s *Server) postEmbeddingResults(w http.ResponseWriter, r *http.Request) {
	if s.recommend == nil {
		writeError(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "embedding workers are not available")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkerBodyBytes)
	var req embeddingResultsRequest
	if err := jsonBody(r, &req); err != nil {
		writeBodyError(w, r, err)
		return
	}
	if len(req.Results) > recommend.MaxClaimLimit {
		writeError(w, r, http.StatusBadRequest, "INVALID_REQUEST",
			fmt.Sprintf("too many results in one batch, maximum %d", recommend.MaxClaimLimit))
		return
	}

	results := make([]catalog.EmbeddingResult, 0, len(req.Results))
	rejected := make([]rejectedEmbeddingPayload, 0)
	for _, item := range req.Results {
		// Report the hash exactly as the worker sent it when it is unusable,
		// so the bad entry is findable in the request that carried it.
		hash := strings.TrimSpace(item.Hash)
		if hash == "" {
			rejected = append(rejected, rejectedEmbeddingPayload{Hash: item.Hash, Reason: catalog.EmbeddingRejectInvalidHash})
			continue
		}
		status := catalog.EmbeddingStatus(item.Status)
		if status != catalog.EmbeddingStatusReady && status != catalog.EmbeddingStatusFailed {
			rejected = append(rejected, rejectedEmbeddingPayload{Hash: hash, Reason: catalog.EmbeddingRejectInvalidState})
			continue
		}
		result := catalog.EmbeddingResult{Hash: hash, Status: status, Error: item.Error}
		if status == catalog.EmbeddingStatusReady {
			vector, err := decodeVector(item.VectorB64)
			if err != nil {
				rejected = append(rejected, rejectedEmbeddingPayload{Hash: hash, Reason: rejectionReason(err)})
				continue
			}
			result.Vector = vector
		}
		results = append(results, result)
	}

	updated, applyRejected, err := s.recommend.ApplyEmbeddingResults(r.Context(), results)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	for _, item := range applyRejected {
		rejected = append(rejected, rejectedEmbeddingPayload{Hash: item.Hash, Reason: item.Reason})
	}
	writeJSON(w, http.StatusOK, embeddingResultsResponse{Updated: updated, Rejected: rejected})
}
