package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"viewer/internal/storage"
)

// countingStore counts listings and fails the first one, so a test can tell a
// retrying loop from one that gave up.
type countingStore struct {
	mu    sync.Mutex
	calls int
	fail  int
}

func (s *countingStore) ListObjects(context.Context, string) ([]storage.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.calls <= s.fail {
		return nil, errors.New("listing unavailable")
	}
	return nil, nil
}

func (s *countingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestWatcherKeepsScanningUntilCancelled is the difference between a one-shot
// startup scan and watching the drop zone: a zip dropped while the server is
// running must be seen on a later pass.
func TestWatcherKeepsScanningUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &countingStore{}
	watcher := &Watcher{store: store, sink: &fakeSink{}, interval: time.Millisecond}

	done := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(done)
	}()

	waitFor(t, func() bool { return store.count() >= 3 }, "three scans")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return after cancellation")
	}
}

// TestWatcherRetriesAfterAFailedScan keeps a transient S3 outage from ending the
// watch for the rest of the container's life.
func TestWatcherRetriesAfterAFailedScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := &countingStore{fail: 1}
	watcher := &Watcher{store: store, sink: &fakeSink{}, interval: time.Millisecond}

	done := make(chan struct{})
	go func() {
		watcher.Run(ctx)
		close(done)
	}()

	waitFor(t, func() bool { return store.count() >= 3 }, "a scan after the failed one")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return after cancellation")
	}
}

func waitFor(t *testing.T, ready func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
