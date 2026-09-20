package recommend

const defaultWorkerBatchSize = 32

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
	Active  bool    `json:"active"`
	Total   int     `json:"total"`
	Ready   int     `json:"ready"`
	Failed  int     `json:"failed"`
	Pending int     `json:"pending"`
	Ratio   float64 `json:"ratio"`
}
