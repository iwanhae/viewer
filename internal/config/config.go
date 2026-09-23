// Package config holds the viewer's deployment settings.
//
// The viewer is always deployed as the Docker image, so the only values that
// differ between deployments are the object-storage credentials and addressing,
// the port the container listens on, the volume holding the SQLite catalog, and
// the optional key prefix that lets two deployments share one bucket. Everything
// else - the upload limits, the checkpoint location and the mirror it is
// fetched from - is a constant here rather than an environment variable.
package config

import (
	"fmt"
	"net/url"
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

	// PresignTTL is how long a presigned URL stays valid - both the zip upload
	// URL handed to the browser and the blob download URL handed to external
	// embedding workers on claim.
	PresignTTL = 15 * time.Minute

	// ModelDir is the local directory holding the SigLIP2 vision checkpoint.
	// The image ships it empty: at startup the viewer fetches the checkpoint
	// into it from DefaultModelURL, and a deployment can mount a prepared
	// directory there instead. It is a directory, not a Hugging Face
	// repository id, so a running container never talks to the Hub.
	ModelDir = "/app/siglip2"

	// DefaultModelURL is the HTTPS mirror the checkpoint is fetched from when
	// ModelDir does not hold one. It serves the upstream Hugging Face files
	// (config.json, model.safetensors) from this deployment's public bucket,
	// so neither the image build nor the container needs to reach Hugging Face.
	DefaultModelURL = "https://s3.iwanhae.kr/public/google/siglip2-base-patch16-224"
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
	// cannot be reconstructed from the blobs in S3. Everything else the
	// process writes - the staged zip being unpacked - goes to the OS temp
	// directory, because the bucket still holds the original.
	StateDir string

	// AllowBackupOverwrite lifts the finalizer's guard against replacing the
	// bucket's catalog backup with a local database that cannot be traced to
	// a backup (no readable stamp) or that is drastically smaller than the
	// backup it would replace. It is the explicit opt-out for the legitimate
	// versions of those states: a hand-replaced catalog, or a deliberate bulk
	// deletion that really did shrink the catalog tenfold.
	AllowBackupOverwrite bool

	// ModelURL is the base URL the checkpoint is fetched from when ModelDir
	// holds no file: one download per <ModelURL>/<filename>.
	ModelURL string

	// WorkerToken is the shared bearer token external embedding workers must
	// present on the claim/renew/results endpoints. Empty switches the worker
	// auth off, which is only sensible on a network that cannot be reached by
	// untrusted clients.
	WorkerToken string

	// QdrantURL is the REST base URL of the external Qdrant server vector
	// search talks to: the HTTP client appends a collection path to it. Load
	// trims surrounding whitespace and trailing slashes the way ModelURL is
	// trimmed, and rejects a value that is not an http or https URL with a
	// host.
	QdrantURL string

	// QdrantAPIKey is the value sent as the api-key header on every Qdrant
	// request.
	QdrantAPIKey string

	// QdrantCollection is the name of the Qdrant collection that holds the
	// per-photo embedding points: every upsert, search, count and migration
	// upload is scoped to it. It is required with no default, so nothing is
	// ever created or queried under an accidental collection name — a fallback
	// constant would silently fork the corpus between deployments that spell
	// the collection differently. The value is passed through as configured
	// with no charset validation, because the server rejects invalid
	// collection names; requiredness is the only contract here.
	QdrantCollection string

	// AdminToken is the credential the admin page requires, sent as the Basic
	// auth password. Empty leaves the admin UI disabled, and Load passes the
	// emptiness through rather than substituting a default, so the startup log
	// can report the UI as off.
	AdminToken string
}

// DBPath is the SQLite catalog inside StateDir.
func (c Config) DBPath() string { return filepath.Join(c.StateDir, "viewer.db") }

// Load reads the deployment settings from the environment and validates that
// the object store and the Qdrant endpoint and collection are fully configured.
func Load() (Config, error) {
	usePathStyle, err := getenvBool("S3_USE_PATH_STYLE", DefaultS3UsePathStyle)
	if err != nil {
		return Config{}, err
	}
	allowBackupOverwrite, err := getenvBool("ALLOW_BACKUP_OVERWRITE", false)
	if err != nil {
		return Config{}, err
	}
	workerToken := strings.TrimSpace(os.Getenv("EMBEDDING_WORKER_TOKEN"))
	qdrantAPIKey := strings.TrimSpace(os.Getenv("QDRANT_API_KEY"))
	qdrantCollection := strings.TrimSpace(os.Getenv("QDRANT_COLLECTION"))
	adminToken := strings.TrimSpace(os.Getenv("ADMIN_TOKEN"))

	cfg := Config{
		Port:                 getenvInt("PORT", DefaultPort),
		S3Endpoint:           os.Getenv("S3_ENDPOINT"),
		S3Bucket:             os.Getenv("S3_BUCKET"),
		S3AccessKey:          os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:          os.Getenv("S3_SECRET_KEY"),
		S3Prefix:             normalizePrefix(os.Getenv("S3_PREFIX")),
		S3UsePathStyle:       usePathStyle,
		StateDir:             normalizeStateDir(os.Getenv("STATE_DIR")),
		AllowBackupOverwrite: allowBackupOverwrite,
		ModelURL:             normalizeModelURL(os.Getenv("SIGLIP2_MODEL_URL")),
		WorkerToken:          workerToken,
		QdrantURL:            normalizeQdrantURL(os.Getenv("QDRANT_URL")),
		QdrantAPIKey:         qdrantAPIKey,
		QdrantCollection:     qdrantCollection,
		AdminToken:           adminToken,
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
	case os.Getenv("EMBEDDING_WORKER_TOKEN") != "" && workerToken == "":
		// The variable was set, so the operator meant to protect the worker
		// API. A whitespace-only value would silently run it unauthenticated,
		// which is exactly the accident this check exists to prevent.
		return Config{}, fmt.Errorf("EMBEDDING_WORKER_TOKEN is set but blank")
	case cfg.QdrantURL == "":
		return Config{}, fmt.Errorf("QDRANT_URL is required")
	case !validQdrantURL(cfg.QdrantURL):
		return Config{}, fmt.Errorf("QDRANT_URL must be an http or https URL with a host, got %q", cfg.QdrantURL)
	case os.Getenv("QDRANT_API_KEY") != "" && qdrantAPIKey == "":
		// The variable was set, so the operator meant to authenticate to
		// Qdrant. This case precedes the "is required" check so the message
		// says the value was seen and rejected, not never supplied.
		return Config{}, fmt.Errorf("QDRANT_API_KEY is set but blank")
	case cfg.QdrantAPIKey == "":
		return Config{}, fmt.Errorf("QDRANT_API_KEY is required")
	case os.Getenv("QDRANT_COLLECTION") != "" && qdrantCollection == "":
		// The variable was set, so the operator meant to name the collection
		// the points live in. This case precedes the "is required" check so
		// the message says the value was seen and rejected, not never
		// supplied.
		return Config{}, fmt.Errorf("QDRANT_COLLECTION is set but blank")
	case cfg.QdrantCollection == "":
		// There is deliberately no fallback name: a default would let a
		// deployment come up creating and querying a collection the operator
		// never chose, and corpus written there would not follow the real one
		// when the setting is fixed.
		return Config{}, fmt.Errorf("QDRANT_COLLECTION is required")
	case os.Getenv("ADMIN_TOKEN") != "" && adminToken == "":
		// The variable was set, so the operator meant to protect the admin
		// page. A whitespace-only value would silently leave the admin UI
		// disabled, which is exactly the accident this check exists to
		// prevent.
		return Config{}, fmt.Errorf("ADMIN_TOKEN is set but blank")
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

// normalizeModelURL falls back to DefaultModelURL when SIGLIP2_MODEL_URL is
// unset or blank, and drops trailing slashes so the checkpoint downloader can
// append a filename directly.
func normalizeModelURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return DefaultModelURL
	}
	return trimmed
}

// normalizeQdrantURL trims the Qdrant REST base URL the same way ModelURL is
// trimmed - surrounding whitespace and trailing slashes - so the vector client
// can append a collection path directly. Unlike ModelURL there is no default:
// an empty result is rejected by the "QDRANT_URL is required" check.
func normalizeQdrantURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// validQdrantURL reports whether raw is a base URL the Qdrant REST client can
// append a collection path to: it must parse, name http or https, and carry a
// host. A value that fails any of those would otherwise surface as a request
// error on the first vector search instead of a failed start.
func validQdrantURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
