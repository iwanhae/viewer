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
	"time"
)

// The two files a checkpoint directory must hold. They use the upstream
// repository's names, so a mirror is nothing but those files copied under one
// URL prefix.
const (
	configFile  = "config.json"
	weightsFile = "model.safetensors"
)

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
	log.Printf("checkpoint: fetched %s (%d bytes in %s)", url, size, time.Since(startedAt).Round(time.Millisecond))
	return nil
}

// download streams url into a sibling .part file and renames it into place, so
// a download cut short by a crash or a cancelled context leaves the final name
// absent rather than a truncated file that later starts would accept.
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
	size, copyErr := io.Copy(f, resp.Body)
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
