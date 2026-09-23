package recommend

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"viewer/internal/vision"
)

// EmbeddingProvider turns raw image bytes into an embedding vector.
type EmbeddingProvider interface {
	// Load resolves and prepares the provider's resources, such as downloading
	// and compiling the model checkpoint. It is safe to call more than once.
	Load(ctx context.Context) error
	// Embed computes the embedding of one encoded image.
	Embed(ctx context.Context, imageBytes []byte) ([]float32, error)
	// Close releases the resources held by the provider.
	Close() error
}

// VisionEmbedder runs the SigLIP2 vision tower in-process through GoMLX. The
// previous implementation shelled out to a Rust HTTP worker; keeping inference
// in this binary removes that dependency and the extra hop per image.
//
// The checkpoint is large, so it is loaded lazily: NewVisionEmbedder never
// touches the disk and Load performs the expensive work once, at startup.
type VisionEmbedder struct {
	config vision.Config

	// loadMu serialises loadOnce so callers that race the startup load block
	// until it finishes instead of piling up on sync.Once.
	loadMu   sync.Mutex
	loadOnce sync.Once
	model    *vision.Model
	loadErr  error
}

// NewVisionEmbedder describes an embedder without loading the checkpoint.
func NewVisionEmbedder(config vision.Config) *VisionEmbedder {
	return &VisionEmbedder{config: config}
}

// Load resolves the checkpoint and compiles the inference graph. It is safe to
// call concurrently; the result of the first call is cached.
func (v *VisionEmbedder) Load(ctx context.Context) error {
	v.loadMu.Lock()
	defer v.loadMu.Unlock()
	v.loadOnce.Do(func() {
		v.model, v.loadErr = vision.Load(ctx, v.config)
		if v.loadErr == nil {
			log.Printf("recommend: %s", v.Describe())
		}
	})
	return v.loadErr
}

// Embed preprocesses the image and runs the vision tower.
func (v *VisionEmbedder) Embed(ctx context.Context, imageBytes []byte) ([]float32, error) {
	if err := v.Load(ctx); err != nil {
		return nil, err
	}

	image, err := vision.Preprocess(imageBytes, v.model.ImageSize())
	if err != nil {
		// Double wrap: the sentinel lets callers classify a garbage upload as
		// a bad request without string matching, the text keeps the wrap the
		// ingest worker logs. Graph/embed failures below stay unclassified —
		// they are deployment states, not the upload's fault.
		return nil, fmt.Errorf("preprocess image: %w: %w", err, ErrUnreadableImage)
	}
	vector, err := v.model.Embed(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("embed image: %w", err)
	}
	return vector, nil
}

// EmbedText computes the query-side embedding of one natural-language query.
// It is the text half of the dual-tower model: the query vector lands in the
// same space as the image embeddings, which is what lets a text vector search
// the photo points directly.
func (v *VisionEmbedder) EmbedText(ctx context.Context, text string) ([]float32, error) {
	if err := v.Load(ctx); err != nil {
		return nil, err
	}

	vector, err := v.model.EmbedText(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("embed text: %w", err)
	}
	return vector, nil
}

// Close releases the compiled graph.
func (v *VisionEmbedder) Close() error {
	if v.model == nil {
		return nil
	}
	return v.model.Close()
}

// Describe returns a human readable summary of the loaded model, for logs.
func (v *VisionEmbedder) Describe() string {
	if v.model == nil {
		return "siglip2 model=<unloaded>"
	}
	return v.model.Describe()
}

// isTransientEmbedError reports whether a failed embedding is worth retrying.
//
// In-process inference has no network or remote-worker failure modes left: a
// failure means the image could not be decoded or the model could not run, and
// retrying it will not help. Context cancellation is still transient because
// the worker exits and the blob is picked up by the next run.
func isTransientEmbedError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
