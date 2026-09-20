package recommend

import (
	"cmp"
	"math"
	"slices"
)

// Neighbor is a content hash and its similarity to the query vector.
type Neighbor struct {
	Hash  string
	Score float64
}

func findNeighbors(embeddings map[string][]float32, query []float32, limit int, excludeHash string) []Neighbor {
	if limit <= 0 {
		return nil
	}
	queryNormed := normalizeVector(query)
	if len(queryNormed) == 0 {
		return nil
	}
	neighbors := make([]Neighbor, 0, len(embeddings))
	for hash, vector := range embeddings {
		if hash == excludeHash {
			continue
		}
		neighbors = append(neighbors, Neighbor{
			Hash:  hash,
			Score: cosineNormalized(queryNormed, vector),
		})
	}
	slices.SortFunc(neighbors, func(a, b Neighbor) int {
		if diff := cmp.Compare(b.Score, a.Score); diff != 0 {
			return diff
		}
		return cmp.Compare(a.Hash, b.Hash)
	})
	if len(neighbors) > limit {
		neighbors = neighbors[:limit]
	}
	return neighbors
}

func normalizeVector(in []float32) []float32 {
	if len(in) == 0 {
		return nil
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
	return out
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
