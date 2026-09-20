package cache

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type DiskCache struct {
	dir string
}

func NewDiskCache(dir string) (*DiskCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &DiskCache{dir: dir}, nil
}

func (c *DiskCache) pathFor(key string) string {
	h := sha1.Sum([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(h[:]))
}

// Materialize ensures the entry for key exists on disk and returns its path and
// size. An entry already on disk is reused as-is. On a miss the fetch result is
// streamed into a temporary file in the cache directory and renamed into place
// atomically, so a concurrent reader never observes a partial file and a failed
// fetch leaves nothing behind.
func (c *DiskCache) Materialize(key string, fetch func() (io.ReadCloser, error)) (string, int64, error) {
	path := c.pathFor(key)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path, info.Size(), nil
	}

	tmp, err := os.CreateTemp(c.dir, "download-*")
	if err != nil {
		return "", 0, fmt.Errorf("create cache temp file: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if committed {
			return
		}
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	body, err := fetch()
	if err != nil {
		return "", 0, err
	}
	size, copyErr := io.Copy(tmp, body)
	closeErr := body.Close()
	if copyErr != nil {
		return "", 0, fmt.Errorf("cache %s: %w", key, copyErr)
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("cache %s: %w", key, closeErr)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("cache %s: %w", key, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", 0, fmt.Errorf("cache %s: %w", key, err)
	}
	committed = true
	return path, size, nil
}
