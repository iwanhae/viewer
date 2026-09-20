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

type EmbeddingProgress struct {
	Total     int
	Ready     int
	Failed    int
	Pending   int
	Processed int
	Ratio     float64
	Percent   float64
}
