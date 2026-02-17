package recommend

import (
	"cmp"
	"math"
	"slices"
)

type Neighbor struct {
	ImageID string
	Score   float64
}

func findNeighbors(embeddings map[string]EmbeddingRecord, query []float32, limit int, excludeIDs map[string]struct{}) []Neighbor {
	if limit <= 0 {
		return nil
	}
	queryNormed, _ := normalizeVector(query)
	if len(queryNormed) == 0 {
		return nil
	}
	neighbors := make([]Neighbor, 0, len(embeddings))
	for id, emb := range embeddings {
		if excludeIDs != nil {
			if _, excluded := excludeIDs[id]; excluded {
				continue
			}
		}
		score := cosineNormalized(queryNormed, emb.Vector)
		neighbors = append(neighbors, Neighbor{ImageID: id, Score: score})
	}
	slices.SortFunc(neighbors, func(a, b Neighbor) int {
		if diff := cmp.Compare(b.Score, a.Score); diff != 0 {
			return diff
		}
		return cmp.Compare(a.ImageID, b.ImageID)
	})
	if len(neighbors) > limit {
		neighbors = neighbors[:limit]
	}
	return neighbors
}

func normalizeVector(in []float32) ([]float32, float64) {
	if len(in) == 0 {
		return nil, 0
	}
	norm := 0.0
	for _, value := range in {
		norm += float64(value) * float64(value)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}
	out := make([]float32, len(in))
	for idx, value := range in {
		out[idx] = float32(float64(value) / norm)
	}
	return out, norm
}

func cosineNormalized(a []float32, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	length := len(a)
	if len(b) < length {
		length = len(b)
	}
	sum := 0.0
	for idx := 0; idx < length; idx++ {
		sum += float64(a[idx] * b[idx])
	}
	return sum
}
