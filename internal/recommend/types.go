package recommend

import (
	"context"
	"time"

	"viewer/internal/qdrant"
)

// defaultWorkerBatchSize is how many blobs the in-process worker claims per
// drain of the pending queue. One image at a time keeps the single forward
// pass fed without queueing behind itself.
const defaultWorkerBatchSize = 32

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

// VectorStore is the remote vector index recommendations and natural-language
// search queries are answered from, and embedding results are written to. It is
// satisfied structurally by *qdrant.Client. A nil store degrades the service:
// writes skip the index and go to SQLite only, and Recommend and Search report
// the store as unavailable instead of answering with results they cannot
// compute.
type VectorStore interface {
	EnsureCollection(ctx context.Context) error
	UpsertPhotos(ctx context.Context, records []qdrant.PhotoRecord) error
	GroupSearch(ctx context.Context, queryPointID, excludeAlbumID, excludeHash string, limit int) ([]qdrant.PhotoHit, error)
	SearchByVectorGrouped(ctx context.Context, vector []float32, limit int) ([]qdrant.PhotoHit, error)
	RetrieveVectorsByHashes(ctx context.Context, hashes []string) (map[string][]float32, error)
	DeleteByAlbum(ctx context.Context, albumID string) error
	CountVectors(ctx context.Context) (int, error)
}

// TextEmbeddingProvider is the text half of the dual-tower model: it turns a
// natural-language query into a vector that shares the image embedding space.
// It is a capability check, not a new dependency — the production embedder
// implements both halves, and test stubs implement only what they fake — so
// Search type-asserts it at call time instead of taking a second provider in
// the constructor.
type TextEmbeddingProvider interface {
	Load(ctx context.Context) error
	EmbedText(ctx context.Context, text string) ([]float32, error)
	Close() error
}

var _ TextEmbeddingProvider = (*VisionEmbedder)(nil)

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

// EmbeddingProgress is the embedding coverage of the whole catalog. Ratio is
// Ready/Total, so it reaches 1 only once every image has an embedding.
type EmbeddingProgress struct {
	// Enabled reports whether the model is loaded. When it is false nothing is
	// being embedded and nothing ever will be, so Pending never drains until a
	// deployment with a model picks the blobs up.
	Enabled bool `json:"enabled"`
	// Active reports whether a forward pass is running right now.
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
