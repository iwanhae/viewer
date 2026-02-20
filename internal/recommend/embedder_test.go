package recommend

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"
)

type transientNetError struct{}

func (transientNetError) Error() string {
	return "temporary network failure"
}

func (transientNetError) Timeout() bool {
	return true
}

func (transientNetError) Temporary() bool {
	return true
}

func TestIsTransientEmbedErrorTransportError(t *testing.T) {
	err := &url.Error{Op: "GET", URL: "http://example.com", Err: transientNetError{}}
	if !isTransientEmbedError(err) {
		t.Fatalf("expected transport error to be transient")
	}
}

func TestIsTransientEmbedErrorStatusError(t *testing.T) {
	if !isTransientEmbedError(workerStatusError{status: 503, body: "busy", op: "embed"}) {
		t.Fatalf("expected 5xx worker status error to be transient")
	}
	if isTransientEmbedError(workerStatusError{status: 400, body: "bad", op: "embed"}) {
		t.Fatalf("expected 4xx worker status error to be non-transient")
	}
}

func TestIsTransientEmbedErrorRegularError(t *testing.T) {
	if isTransientEmbedError(errors.New("permanent failure")) {
		t.Fatalf("expected regular error to be non-transient")
	}
}

func TestHTTPEmbedderReturnsConfigErrorWhenEndpointMissing(t *testing.T) {
	embedder := NewHTTPEmbedder("", time.Second)

	if err := embedder.Healthcheck(context.Background()); !errors.Is(err, errRecommenderEndpointNotConfigured) {
		t.Fatalf("Healthcheck err=%v want=%v", err, errRecommenderEndpointNotConfigured)
	}

	_, err := embedder.Embed(context.Background(), []byte("image"))
	if !errors.Is(err, errRecommenderEndpointNotConfigured) {
		t.Fatalf("Embed err=%v want=%v", err, errRecommenderEndpointNotConfigured)
	}
}
