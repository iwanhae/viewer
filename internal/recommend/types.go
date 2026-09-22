package recommend

import (
	"time"

	"viewer/internal/catalog"
)

// defaultWorkerBatchSize is how many blobs the in-process worker claims per
// drain of the pending queue. One image at a time keeps the single forward
// pass fed without queueing behind itself.
const defaultWorkerBatchSize = 32

// EmbeddingDim is the vector length every embedding must have. The schema owns
// this number — it is baked into the catalog's blob_embeddings vec0 column —
// and the checkpoint is pinned by DefaultModelURL to a siglip2-base model whose
// hidden size matches it. A deployment that mounts a differently-sized
// checkpoint has to move both the constant and the schema with it, and the
// external worker API enforces the same value so the index can never mix
// incompatible vectors.
const EmbeddingDim = catalog.EmbeddingDim

// DefaultLeaseTTL is how long a claim keeps a blob out of the pending queue.
// The internal worker renews it between images; external workers have the
// renew endpoint. The value comfortably covers one internal batch even when
// every image takes its full embedding timeout.
const DefaultLeaseTTL = 10 * time.Minute

const (
	// DefaultClaimLimit is the batch size an external worker gets when its
	// claim request does not name one.
	DefaultClaimLimit = 64
	// MaxClaimLimit caps both the requested claim size and the number of
	// results one write-back may carry, so a request is never unbounded.
	MaxClaimLimit = 1024
)

// Rejection reasons ApplyEmbeddingResults reports for results that fail
// validation before they ever reach the catalog. They are informational: a
// worker must fix its payload, not retry it.
const (
	RejectWrongDim  = "wrong_dim"
	RejectBadVector = "bad_vector"
)

// photoRef is one album photo that references a content-addressed blob.
type photoRef struct {
	AlbumID string
	Index   int
	Hash    string
	Width   int
	Height  int
	Ratio   float64
}

type RecommendationItem struct {
	AlbumID string  `json:"albumId"`
	I       int     `json:"i"`
	Hash    string  `json:"hash"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Score   float64 `json:"score"`
}

type RecommendationResponse struct {
	Items []RecommendationItem `json:"items"`
}

// EmbeddingProgress is the embedding coverage of a set of blobs, either the
// whole catalog or a single album's. Ratio is Ready/Total, so it reaches 1 only
// once every image has an embedding.
type EmbeddingProgress struct {
	// Enabled reports whether the model is loaded. When it is false nothing is
	// being embedded and nothing ever will be, so a client must not wait for
	// Pending to reach zero: the difference between "not embedded yet" and
	// "cannot embed" is what keeps a progress indicator honest.
	Enabled bool `json:"enabled"`
	// Active reports whether a forward pass is running right now. It is false
	// for a per-album view, which only carries counts.
	Active bool `json:"active"`
	Total  int  `json:"total"`
	Ready  int  `json:"ready"`
	Failed int  `json:"failed"`
	// Pending counts everything not ready and not failed, including the blobs
	// a worker currently holds a lease on.
	Pending int `json:"pending"`
	// Processing is the subset of Pending whose lease some worker currently
	// holds. It is observability for the external worker fleet; the progress
	// contract only ever promised Pending.
	Processing int     `json:"processing"`
	Ratio      float64 `json:"ratio"`
}
