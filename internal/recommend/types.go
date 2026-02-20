package recommend

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	embeddingStatusReady  = "ready"
	embeddingStatusFailed = "failed"
)

type PhotoRecord struct {
	ImageID    string
	AlbumID    string
	PhotoIndex int
	EntryName  string
	Width      int
	Height     int
	Ratio      float64
}

type EmbeddingRecord struct {
	ImageID string
	Vector  []float32
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
