package recommend

import "errors"

var ErrPhotoNotFound = errors.New("recommendation photo not found")

// ErrVectorStoreUnavailable reports that no vector store is wired up at all —
// a deployment without QDRANT_URL, or a test — as opposed to an empty result,
// which means the store answered and nothing similar exists. httpapi maps it
// to a 503 so clients see "the feature is off" rather than "no similar
// photos" or a 500.
var ErrVectorStoreUnavailable = errors.New("vector store is not available")
