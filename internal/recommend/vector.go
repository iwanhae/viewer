package recommend

import (
	"cmp"
	"context"
	"math"
	"slices"
)

type Neighbor struct {
	ImageID string
	Score   float64
}

type SearchOptions struct {
	Limit      int
	ExcludeIDs map[string]struct{}
}

type VectorIndex interface {
	Search(ctx context.Context, query []float32, options SearchOptions) ([]Neighbor, error)
	Upsert(ctx context.Context, imageIDValue string, vector []float32, modelID string) error
}

type EmbeddedVectorIndex struct {
	store *LocalStore
}

func NewEmbeddedVectorIndex(store *LocalStore) *EmbeddedVectorIndex {
	return &EmbeddedVectorIndex{store: store}
}

func (i *EmbeddedVectorIndex) Upsert(ctx context.Context, imageIDValue string, vector []float32, modelID string) error {
	_ = ctx
	return i.store.MarkDone(imageIDValue, vector, modelID)
}

func (i *EmbeddedVectorIndex) Search(ctx context.Context, query []float32, options SearchOptions) ([]Neighbor, error) {
	_ = ctx
	if options.Limit <= 0 {
		return nil, nil
	}
	queryNormed, _ := normalizeVector(query)
	if len(queryNormed) == 0 {
		return nil, nil
	}
	embeddings := i.store.EmbeddingsSnapshot()
	neighbors := make([]Neighbor, 0, len(embeddings))
	for id, emb := range embeddings {
		if options.ExcludeIDs != nil {
			if _, excluded := options.ExcludeIDs[id]; excluded {
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
	if len(neighbors) > options.Limit {
		neighbors = neighbors[:options.Limit]
	}
	return neighbors, nil
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
