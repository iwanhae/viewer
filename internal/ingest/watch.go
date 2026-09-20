package ingest

import (
	"context"
	"errors"
	"log"
	"time"
)

// scanInterval is how often the upload prefix is rescanned. One scan is a single
// listing plus one catalog lookup per waiting zip, and a zip is deleted as soon
// as it is extracted, so a quiet deployment pays almost nothing for it while a
// zip dropped into the prefix is picked up within an interval.
const scanInterval = time.Minute

// Watcher keeps the upload prefix ingested.
type Watcher struct {
	store Store
	sink  Sink

	// interval is a field so tests can drive the loop without waiting a minute.
	interval time.Duration
}

// NewWatcher builds a watcher over one object store and one album service.
func NewWatcher(store Store, sink Sink) *Watcher {
	return &Watcher{store: store, sink: sink, interval: scanInterval}
}

// Run scans once and then keeps scanning until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	for {
		w.scanOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.interval):
		}
	}
}

// scanOnce runs one scan and logs its outcome. A failed scan is reported and
// retried on the next tick: the bucket being unreachable for a moment is not a
// reason to stop watching.
func (w *Watcher) scanOnce(ctx context.Context) {
	startedAt := time.Now()
	summary, err := Run(ctx, w.store, w.sink)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("upload ingest: scan skipped: %v", err)
		return
	}
	log.Printf(
		"upload ingest: scan finished discovered=%d registered=%d requeued=%d skipped=%d errors=%d duration=%s",
		summary.Discovered, summary.Registered, summary.Requeued, summary.Skipped, summary.Errors,
		time.Since(startedAt).Round(time.Millisecond),
	)
}
