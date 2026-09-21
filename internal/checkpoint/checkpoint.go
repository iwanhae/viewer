// Package checkpoint provisions the SigLIP2 vision checkpoint on local disk.
//
// The Docker image ships without the checkpoint: a deployment either mounts a
// prepared directory at the model directory, or the viewer fetches the files
// from a plain HTTPS mirror - this deployment's public bucket, which serves the
// upstream Hugging Face files under one prefix. A download that fails is
// reported to the caller, which degrades to serving without embeddings exactly
// like a missing checkpoint always has.
package checkpoint

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// The two files a checkpoint directory must hold. They use the upstream
// repository's names, so a mirror is nothing but those files copied under one
// URL prefix.
const (
	configFile  = "config.json"
	weightsFile = "model.safetensors"
)

// progressEvery is how often an in-flight download reports its position and
// rate. A variable only so a test can shrink it; only the weights file is
// large enough for a line to ever appear at the real interval.
var progressEvery = 5 * time.Second

// Ensure makes dir hold a complete checkpoint, downloading any missing file
// from base (one file per <base>/<filename> URL). It is a no-op when dir
// already holds both files, so a mounted checkpoint or a previous download is
// never re-fetched. A file that cannot be fetched leaves no partial file
// behind: the next start tries again from scratch.
func Ensure(ctx context.Context, dir, base string) error {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return fmt.Errorf("checkpoint base URL is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create model directory %s: %w", dir, err)
	}

	for _, name := range []string{configFile, weightsFile} {
		if err := ensureFile(ctx, dir, base, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureFile downloads the one checkpoint file unless dir already holds it
// with content. An empty file is treated as absent, the same failing state the
// image build used to refuse.
func ensureFile(ctx context.Context, dir, base, name string) error {
	dst := filepath.Join(dir, name)
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return nil
	}

	url := base + "/" + name
	startedAt := time.Now()
	size, err := download(ctx, url, filepath.Join(dir, "."+name+".part"), dst)
	if err != nil {
		return err
	}
	elapsed := time.Since(startedAt)
	log.Printf(
		"checkpoint: fetched %s (%s in %s, average %s/s)",
		url, formatSize(float64(size)), elapsed.Round(time.Millisecond), formatSize(float64(size)/elapsed.Seconds()),
	)
	return nil
}

// download streams url into a sibling .part file and renames it into place, so
// a download cut short by a crash or a cancelled context leaves the final name
// absent rather than a truncated file that later starts would accept. While the
// copy runs, a helper goroutine logs position and rate every progressEvery.
func download(ctx context.Context, url, tmp, dst string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("request %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch %s: unexpected status %s", url, resp.Status)
	}

	f, err := os.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", tmp, err)
	}
	body := &countingReader{r: resp.Body}
	stopProgress := reportProgress(url, body, resp.ContentLength)
	size, copyErr := io.Copy(f, body)
	stopProgress()
	if closeErr := f.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr == nil && size == 0 {
		copyErr = fmt.Errorf("fetch %s: empty response", url)
	}
	if copyErr != nil {
		_ = os.Remove(tmp)
		return 0, copyErr
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("move %s into place: %w", dst, err)
	}
	return size, nil
}

// countingReader counts the bytes that have flowed through it. io.Copy is the
// only writer of n; the progress goroutine only reads it, so an atomic is
// enough.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// reportProgress starts a goroutine that logs how much of the file is on disk
// and how fast the last interval moved, every progressEvery, and returns the
// function that stops it. total is the server's Content-Length, negative when
// unknown, in which case the line drops the total and the percentage.
func reportProgress(url string, body *countingReader, total int64) (stop func()) {
	stopped := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(progressEvery)
		defer ticker.Stop()
		name := filepath.Base(url)
		lastAt := time.Now()
		last := int64(0)
		for {
			select {
			case <-stopped:
				return
			case now := <-ticker.C:
				current := body.n.Load()
				rate := float64(current-last) / now.Sub(lastAt).Seconds()
				lastAt, last = now, current
				if total >= 0 {
					log.Printf(
						"checkpoint: fetching %s %s / %s (%.1f%%) at %s/s",
						name, formatSize(float64(current)), formatSize(float64(total)),
						100*float64(current)/float64(total), formatSize(rate),
					)
					continue
				}
				log.Printf("checkpoint: fetching %s %s at %s/s", name, formatSize(float64(current)), formatSize(rate))
			}
		}
	}()
	return func() {
		close(stopped)
		<-finished
	}
}

// formatSize renders a byte count the way the progress lines read it: binary
// units with one decimal once the value reaches them.
func formatSize(b float64) string {
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
