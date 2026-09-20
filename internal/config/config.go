// Package config holds the viewer's deployment settings.
//
// The viewer is always deployed as the Docker image, so the only values that
// differ between deployments are the object-storage credentials, the port the
// container listens on, and the optional key prefix that lets two deployments
// share one bucket. Everything else - the cache paths, the upload limits, the
// checkpoint location - is a constant here rather than an environment variable.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultPort is the port the HTTP server binds inside the container.
	DefaultPort = 8080

	// S3Region and S3UsePathStyle target the self-hosted, path-style object
	// stores (Garage, MinIO) this deployment runs against. Those stores ignore
	// the region, but the AWS SDK requires a non-empty value.
	S3Region       = "us-east-1"
	S3UsePathStyle = true

	// MaxUploadBytes caps the staged zip size accepted by POST /api/albums.
	MaxUploadBytes int64 = 1 << 30 // 1 GiB

	// PresignTTL is how long a presigned zip upload URL stays valid.
	PresignTTL = 15 * time.Minute

	// StateDir is the container-local directory holding the SQLite catalog and
	// the disk caches. The caches are disposable, but the catalog is not: the
	// album-to-photo mapping exists nowhere else, and staged zips are deleted
	// after extraction, so albums cannot be reconstructed from the blobs in S3.
	// Mount a host directory here to keep them across container replacements.
	StateDir = "/tmp/viewer-cache"

	// DBPath is the SQLite catalog. CacheDir holds decoded image blobs and
	// ZipCacheDir stages downloaded uploads while they are unpacked.
	DBPath      = StateDir + "/viewer.db"
	CacheDir    = StateDir + "/images"
	ZipCacheDir = StateDir + "/zips"

	// ModelDir is where the Docker image bakes the SigLIP2 vision checkpoint.
	// It is a directory, not a Hugging Face repository id, so a container never
	// needs network access to Hugging Face at startup.
	ModelDir = "/app/siglip2"
)

// Config is the set of settings a deployment supplies. It carries no derived
// or tunable state: the constants above cover everything else.
type Config struct {
	Port        int
	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string

	// S3Prefix is prepended to every object key, so several deployments can
	// share one bucket. It is normalized to either "" or a slash-terminated
	// path; an unset value keeps the flat "blobs/<hash>" layout.
	S3Prefix string
}

// Load reads the deployment settings from the environment and validates that
// the object store is fully configured.
func Load() (Config, error) {
	cfg := Config{
		Port:        getenvInt("PORT", DefaultPort),
		S3Endpoint:  os.Getenv("S3_ENDPOINT"),
		S3Bucket:    os.Getenv("S3_BUCKET"),
		S3AccessKey: os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("S3_SECRET_KEY"),
		S3Prefix:    normalizePrefix(os.Getenv("S3_PREFIX")),
	}

	switch {
	case cfg.S3Endpoint == "":
		return Config{}, fmt.Errorf("S3_ENDPOINT is required")
	case cfg.S3Bucket == "":
		return Config{}, fmt.Errorf("S3_BUCKET is required")
	case cfg.S3AccessKey == "":
		return Config{}, fmt.Errorf("S3_ACCESS_KEY is required")
	case cfg.S3SecretKey == "":
		return Config{}, fmt.Errorf("S3_SECRET_KEY is required")
	}

	return cfg, nil
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 65535 {
		return fallback
	}
	return n
}

// normalizePrefix turns an operator-supplied key prefix into the exact string
// prepended to every object key: "" for an unset or slash-only value, and a
// slash-terminated path otherwise. Accepting "viewer", "viewer/" and
// "/viewer/" alike keeps the setting forgiving about stray slashes.
func normalizePrefix(raw string) string {
	trimmed := strings.Trim(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return ""
	}
	return trimmed + "/"
}

// DescribePrefix renders the effective prefix for the startup log, so a
// misconfigured value is visible before it looks like an empty bucket.
func (c Config) DescribePrefix() string {
	if c.S3Prefix == "" {
		return "(bucket root)"
	}
	return c.S3Prefix
}
