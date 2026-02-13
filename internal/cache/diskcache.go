package cache

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
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

func (c *DiskCache) Get(key string) ([]byte, bool) {
	b, err := os.ReadFile(c.pathFor(key))
	if err != nil {
		return nil, false
	}
	return b, true
}

func (c *DiskCache) Set(key string, data []byte) error {
	return os.WriteFile(c.pathFor(key), data, 0o644)
}
