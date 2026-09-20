// Package config holds the viewer's deployment settings.
//
// The viewer is always deployed as the Docker image, so the only values that
// differ between deployments are the object-storage credentials and addressing,
// the port the container listens on, the volume holding the SQLite catalog, and
// the optional key prefix that lets two deployments share one bucket. Everything
// else - the cache paths, the upload limits, the checkpoint location - is a
// constant here rather than an environment variable.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultPort is the port the HTTP server binds inside the container.
	DefaultPort = 8080

	// DefaultStateDir holds the SQLite catalog unless STATE_DIR names another
	// directory. The Docker image pre-creates it, owns it as the unprivileged
	// user the container runs as, and declares it a volume.
	DefaultStateDir = "/var/lib/viewer"

	// CacheRoot holds everything the viewer can rebuild from the bucket: the
	// decoded image blobs and the zip staging area. It is deliberately not
	// configurable and deliberately not under StateDir, so the volume that
	// keeps the catalog never collects cache data.
	CacheRoot = "/tmp/viewer-cache"

	// S3Region targets the self-hosted object stores this deployment runs
	// against. They ignore the region, but the AWS SDK requires a non-empty
	// value.
	S3Region = "us-east-1"

	// DefaultS3UsePathStyle addresses the bucket as the first path segment of
	// the request (https://host/bucket/key) instead of as a subdomain of the
	// endpoint (https://bucket.host/key). Self-hosted stores expect the former,
	// and it needs no wildcard DNS.
	DefaultS3UsePathStyle = true

	// MaxUploadBytes caps the staged zip size accepted by POST /api/albums.
	MaxUploadBytes int64 = 1 << 30 // 1 GiB

	// PresignTTL is how long a presigned zip upload URL stays valid.
	PresignTTL = 15 * time.Minute

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

	// S3UsePathStyle selects how the bucket is addressed: the first path segment
	// of the request when true, a subdomain of the endpoint when false. Load
	// defaults it to DefaultS3UsePathStyle, so a Config built by hand must set
	// it explicitly rather than relying on the false zero value.
	S3UsePathStyle bool

	// StateDir holds the SQLite catalog. It is the one directory that has to
	// survive a container replacement: the album-to-photo mapping exists
	// nowhere else, and staged zips are deleted after extraction, so albums
	// cannot be reconstructed from the blobs in S3. The caches live in
	// CacheRoot instead, because they can be rebuilt.
	StateDir string
}

// DBPath is the SQLite catalog inside StateDir.
func (c Config) DBPath() string { return filepath.Join(c.StateDir, "viewer.db") }

// ImageCacheDir holds decoded image blobs. It is disposable: a miss is served
// from the blob in S3.
func ImageCacheDir() string { return filepath.Join(CacheRoot, "images") }

// ZipCacheDir stages a downloaded upload while it is unpacked. It is disposable
// for the same reason: the zip is still in the bucket until extraction
// succeeds.
func ZipCacheDir() string { return filepath.Join(CacheRoot, "zips") }

// Load reads the deployment settings from the environment and validates that
// the object store is fully configured.
func Load() (Config, error) {
	usePathStyle, err := getenvBool("S3_USE_PATH_STYLE", DefaultS3UsePathStyle)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Port:           getenvInt("PORT", DefaultPort),
		S3Endpoint:     os.Getenv("S3_ENDPOINT"),
		S3Bucket:       os.Getenv("S3_BUCKET"),
		S3AccessKey:    os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:    os.Getenv("S3_SECRET_KEY"),
		S3Prefix:       normalizePrefix(os.Getenv("S3_PREFIX")),
		S3UsePathStyle: usePathStyle,
		StateDir:       normalizeStateDir(os.Getenv("STATE_DIR")),
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
	case !filepath.IsAbs(cfg.StateDir):
		// A relative path would silently land next to the binary instead of in
		// a mounted volume, which loses the catalog on the next replacement.
		return Config{}, fmt.Errorf("STATE_DIR must be an absolute path, got %q", cfg.StateDir)
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

// getenvBool reads a boolean setting. Unlike PORT, a value it cannot parse is an
// error rather than a silent fallback: reading "yes" as true when the operator
// meant the opposite would move every request to a different host, which is far
// harder to notice than a failed start.
func getenvBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", key, raw)
	}
	return v, nil
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

// normalizeStateDir falls back to DefaultStateDir when STATE_DIR is unset or
// blank, and otherwise cleans the value so "/data/" and "/data" agree.
func normalizeStateDir(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return DefaultStateDir
	}
	return filepath.Clean(trimmed)
}
