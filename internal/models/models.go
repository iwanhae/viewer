package models

type PhotoMeta struct {
	I     int     `json:"i"`
	Name  string  `json:"name"`
	W     int     `json:"w"`
	H     int     `json:"h"`
	Ratio float64 `json:"ratio"`
}

type PhotoEmbedding struct {
	Status    string    `json:"status,omitempty"`
	Vector    []float32 `json:"vector,omitempty"`
	Model     string    `json:"model,omitempty"`
	UpdatedAt string    `json:"updatedAt,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type AlbumIndex struct {
	AlbumID          string                    `json:"albumId"`
	OriginalFilename string                    `json:"originalFilename"`
	CreatedAt        string                    `json:"createdAt"`
	PhotoCount       int                       `json:"photoCount"`
	Photos           []PhotoMeta               `json:"photos"`
	Embeddings       map[string]PhotoEmbedding `json:"embeddings,omitempty"`
}

type AlbumSummary struct {
	AlbumID          string `json:"albumId"`
	OriginalFilename string `json:"originalFilename"`
	PhotoCount       int    `json:"photoCount"`
	CreatedAt        string `json:"createdAt"`
}

type AlbumIndexStatus string

const (
	AlbumIndexStatusReady   AlbumIndexStatus = "ready"
	AlbumIndexStatusPartial AlbumIndexStatus = "partial"
	AlbumIndexStatusPending AlbumIndexStatus = "pending"
	AlbumIndexStatusFailed  AlbumIndexStatus = "failed"
)

type AlbumSearchItem struct {
	AlbumID          string           `json:"albumId"`
	OriginalFilename string           `json:"originalFilename"`
	PhotoCount       int              `json:"photoCount"`
	CreatedAt        string           `json:"createdAt"`
	IndexStatus      AlbumIndexStatus `json:"indexStatus"`
	IndexedCount     int              `json:"indexedCount"`
	FailedCount      int              `json:"failedCount"`
	TotalCount       int              `json:"totalCount"`
}

type FeedItem struct {
	AlbumID string  `json:"albumId"`
	I       int     `json:"i"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Ratio   float64 `json:"ratio"`
	Src     string  `json:"src"`
}

type FeedResponse struct {
	Items      []FeedItem `json:"items"`
	NextCursor string     `json:"nextCursor,omitempty"`
}
