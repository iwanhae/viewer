package recommend

import (
	"fmt"
	"strconv"
	"strings"
)

type PhotoRecord struct {
	ImageID         string  `json:"imageId"`
	AlbumID         string  `json:"albumId"`
	PhotoIndex      int     `json:"photoIndex"`
	EntryName       string  `json:"entryName"`
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	Ratio           float64 `json:"ratio"`
	CreatedAt       string  `json:"createdAt"`
	EmbeddingStatus string  `json:"embeddingStatus"`
	EmbeddingModel  string  `json:"embeddingModel,omitempty"`
	UpdatedAt       string  `json:"updatedAt"`
}

type EmbeddingRecord struct {
	ImageID    string    `json:"imageId"`
	Vector     []float32 `json:"vector"`
	Norm       float64   `json:"norm"`
	ModelID    string    `json:"modelId"`
	UpdatedAt  string    `json:"updatedAt"`
	CreatedAt  string    `json:"createdAt"`
	Dimensions int       `json:"dimensions"`
}

type JobRecord struct {
	ImageID   string `json:"imageId"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"lastError,omitempty"`
	NotBefore string `json:"notBefore"`
	UpdatedAt string `json:"updatedAt"`
	CreatedAt string `json:"createdAt"`
	RunningBy string `json:"runningBy,omitempty"`
	StartedAt string `json:"startedAt,omitempty"`
}

type AlbumSyncRecord struct {
	AlbumID          string `json:"albumId"`
	OriginalFilename string `json:"originalFilename"`
	CreatedAt        string `json:"createdAt"`
	PhotoCount       int    `json:"photoCount"`
	UpdatedAt        string `json:"updatedAt"`
}

type RecommendationItem struct {
	AlbumID string  `json:"albumId"`
	I       int     `json:"i"`
	W       int     `json:"w"`
	H       int     `json:"h"`
	Score   float64 `json:"score"`
	Src     string  `json:"src"`
}

type RecommendationResponse struct {
	Items  []RecommendationItem `json:"items"`
	Status string               `json:"status"`
}

func imageID(albumID string, photoIndex int) string {
	return fmt.Sprintf("%s:%d", albumID, photoIndex)
}

func parseImageID(value string) (string, int, error) {
	sep := strings.LastIndex(value, ":")
	if sep < 1 || sep == len(value)-1 {
		return "", 0, fmt.Errorf("invalid image id")
	}
	albumID := value[:sep]
	idx, err := strconv.Atoi(value[sep+1:])
	if err != nil {
		return "", 0, fmt.Errorf("invalid image id index")
	}
	if idx < 0 {
		return "", 0, fmt.Errorf("invalid image id index")
	}
	return albumID, idx, nil
}
