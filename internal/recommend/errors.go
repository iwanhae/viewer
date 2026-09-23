package recommend

import "errors"

var ErrPhotoNotFound = errors.New("recommendation photo not found")

// ErrVectorStoreUnavailable reports that no vector store is wired up at all —
// a deployment without QDRANT_URL, or a test — as opposed to an empty result,
// which means the store answered and nothing similar exists. httpapi maps it
// to a 503 so clients see "the feature is off" rather than "no similar
// photos" or a 500.
var ErrVectorStoreUnavailable = errors.New("vector store is not available")

// ErrTextEmbeddingUnavailable reports that the wired embedder cannot embed
// text — an image-only stub, or a build where the text tower does not exist.
// It is fail-soft like ErrVectorStoreUnavailable: httpapi answers with the
// same 503 "feature is off" shape instead of a 500, because a query that
// cannot be embedded is a deployment state, not a server fault.
var ErrTextEmbeddingUnavailable = errors.New("text embedding is not available")

// ErrEmptyQuery reports that a search arrived with nothing to embed after
// trimming. It is fail-soft like the unavailability errors: httpapi maps it
// to a 400 so a blank query reads as a bad request rather than an empty
// result that would look like "nothing matches".
var ErrEmptyQuery = errors.New("query is empty")

// ErrUnreadableImage reports that an uploaded image could not be turned into
// pixels — a truncated file, a non-image payload, or an empty upload. It is
// fail-soft like ErrEmptyQuery: httpapi maps it to a 400 so a corrupt upload
// reads as a bad request rather than a server fault.
var ErrUnreadableImage = errors.New("the uploaded image could not be decoded")

// ErrImageEmbeddingUnavailable reports that the wired embedder could not embed
// an uploaded image for a reason the request did not cause — a checkpoint that
// never loaded, a graph failure, a timeout. It marks the embed step's generic
// failures so httpapi can answer with the same 503 "feature is off" shape as
// ErrVectorStoreUnavailable, because an embedder that cannot run is a
// deployment state rather than a server fault; anything else the image search
// path returns still reads as a 500.
var ErrImageEmbeddingUnavailable = errors.New("image embedding is not available")
