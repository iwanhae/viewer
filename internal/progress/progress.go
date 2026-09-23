// Package progress counts the bytes of an in-flight transfer and logs its
// position and rate on a fixed interval. It backs the two long downloads a
// start performs - the embedding checkpoint fetch and the catalog backup
// restore - so both emit the same line shape and share one interval:
//
//	checkpoint: fetching model.safetensors 512.0 MiB / 1.5 GiB (33.3%) at 40.0 MiB/s
//	backup: restoring viewer.db 96.0 MiB / 1.2 GiB (7.8%) at 30.0 MiB/s
//
// A transfer shorter than one interval simply never logs; the caller's
// completion line stays the only trace, exactly as before.
package progress

import (
	"fmt"
	"io"
	"log"
	"sync/atomic"
	"time"
)

// Interval is how often an in-flight transfer reports its position and rate.
// A variable only so a test can shrink it.
var Interval = 5 * time.Second

// Reader counts the bytes that have flowed through it. io.Copy is the only
// writer of n; the reporter goroutine only reads it, so an atomic is enough.
type Reader struct {
	r io.Reader
	n atomic.Int64
}

// NewReader wraps r so every Read through it is counted.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n.Add(int64(n))
	return n, err
}

// Count reports the bytes that have moved through the reader so far. It is
// safe to call from the reporter goroutine while io.Copy is running.
func (r *Reader) Count() int64 {
	return r.n.Load()
}

// Report starts a goroutine that logs how much of the transfer has moved and
// how fast the last interval went, every Interval, and returns the function
// that stops it. prefix and verb name the caller's line ("checkpoint",
// "fetching"); name is the transferred file. total is the expected size,
// negative when unknown, in which case the line drops the total and the
// percentage.
func Report(prefix, verb, name string, count func() int64, total int64) (stop func()) {
	stopped := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(Interval)
		defer ticker.Stop()
		lastAt := time.Now()
		last := int64(0)
		for {
			select {
			case <-stopped:
				return
			case now := <-ticker.C:
				current := count()
				rate := float64(current-last) / now.Sub(lastAt).Seconds()
				lastAt, last = now, current
				if total >= 0 {
					log.Printf(
						"%s: %s %s %s / %s (%.1f%%) at %s/s",
						prefix, verb, name, FormatSize(float64(current)), FormatSize(float64(total)),
						100*float64(current)/float64(total), FormatSize(rate),
					)
					continue
				}
				log.Printf(
					"%s: %s %s %s at %s/s",
					prefix, verb, name, FormatSize(float64(current)), FormatSize(rate),
				)
			}
		}
	}()
	return func() {
		close(stopped)
		<-finished
	}
}

// FormatSize renders a byte count the way progress lines read it: binary
// units with one decimal once the value reaches them.
func FormatSize(b float64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%.0f B", b)
	}
	div, exp := float64(unit), 0
	for v := b / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", b/div, "KMGTPE"[exp])
}
