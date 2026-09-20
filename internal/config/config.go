package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

const (
	// defaultEmbeddingModelID and defaultEmbeddingBackend mirror
	// vision.DefaultModelID and vision.DefaultBackend. They are duplicated as
	// literals so this package does not pull the inference stack into binaries
	// such as album-dedupe-cleaner that only need configuration;
	// TestEmbeddingDefaultsMatchVision keeps the two in step.
	defaultEmbeddingModelID = "google/siglip2-base-patch16-224"
	defaultEmbeddingBackend = "go"
)

type Config struct {
	Port                   int
	S3Endpoint             string
	S3Region               string
	S3Bucket               string
	S3AccessKey            string
	S3SecretKey            string
	S3UsePathStyle         bool
	PresignTTL             time.Duration
	MaxUploadBytes         int64
	CacheDir               string
	ZipCacheDir            string
	DBPath                 string
	IngestDeleteSource     bool
	WarmupFetchConcurrency int
	RecoTopKDefault        int
	RecoTopKMax            int

	// Embedding runs the SigLIP2 vision tower in-process through GoMLX.
	EmbeddingEnabled     bool
	EmbeddingModelID     string
	EmbeddingBackend     string
	EmbeddingCacheDir    string
	EmbeddingRequired    bool
	EmbeddingConcurrency int
	EmbeddingTimeoutSec  int
	BatchIngestEnabled   bool
}

func Load() (Config, error) {
	port := getenvInt("PORT", 8080)
	presignTTLSeconds := getenvInt("PRESIGN_TTL_SECONDS", 900)
	maxUploadBytes := getenvInt64("MAX_UPLOAD_BYTES", 1024*1024*1024)
	cacheDir := getenv("CACHE_DIR", ".cache/images")
	zipCacheDir := getenv("ZIP_CACHE_DIR", ".cache/zips")
	dbPath := getenv("DB_PATH", ".cache/viewer.db")

	cfg := Config{
		Port:                   port,
		S3Endpoint:             os.Getenv("S3_ENDPOINT"),
		S3Region:               getenv("S3_REGION", "us-east-1"),
		S3Bucket:               os.Getenv("S3_BUCKET"),
		S3AccessKey:            os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:            os.Getenv("S3_SECRET_KEY"),
		S3UsePathStyle:         getenvBool("S3_USE_PATH_STYLE", true),
		PresignTTL:             time.Duration(presignTTLSeconds) * time.Second,
		MaxUploadBytes:         maxUploadBytes,
		CacheDir:               cacheDir,
		ZipCacheDir:            zipCacheDir,
		DBPath:                 dbPath,
		IngestDeleteSource:     getenvBool("INGEST_DELETE_SOURCE", true),
		WarmupFetchConcurrency: getenvInt("WARMUP_FETCH_CONCURRENCY", 0),
		RecoTopKDefault:        getenvInt("RECO_TOPK_DEFAULT", 12),
		RecoTopKMax:            getenvInt("RECO_TOPK_MAX", 48),

		EmbeddingEnabled:     getenvBool("EMBEDDING_ENABLED", true),
		EmbeddingModelID:     getenv("EMBEDDING_MODEL_ID", defaultEmbeddingModelID),
		EmbeddingBackend:     getenv("EMBEDDING_BACKEND", defaultEmbeddingBackend),
		EmbeddingCacheDir:    os.Getenv("EMBEDDING_CACHE_DIR"),
		EmbeddingRequired:    getenvBool("EMBEDDING_REQUIRED", false),
		EmbeddingConcurrency: getenvInt("EMBEDDING_CONCURRENCY", defaultEmbeddingConcurrency()),
		EmbeddingTimeoutSec:  getenvInt("EMBEDDING_REQUEST_TIMEOUT_SECONDS", 300),
		BatchIngestEnabled:   getenvBool("BATCH_INGEST_ENABLED", true),
	}

	if cfg.S3Bucket == "" {
		return Config{}, fmt.Errorf("S3_BUCKET is required")
	}
	if cfg.S3AccessKey == "" {
		return Config{}, fmt.Errorf("S3_ACCESS_KEY is required")
	}
	if cfg.S3SecretKey == "" {
		return Config{}, fmt.Errorf("S3_SECRET_KEY is required")
	}
	if cfg.S3Endpoint == "" {
		return Config{}, fmt.Errorf("S3_ENDPOINT is required")
	}
	if cfg.WarmupFetchConcurrency < 0 {
		cfg.WarmupFetchConcurrency = 0
	}
	if cfg.RecoTopKDefault <= 0 {
		cfg.RecoTopKDefault = 12
	}
	if cfg.RecoTopKMax <= 0 {
		cfg.RecoTopKMax = 48
	}
	if cfg.EmbeddingConcurrency <= 0 {
		cfg.EmbeddingConcurrency = defaultEmbeddingConcurrency()
	}
	if cfg.EmbeddingTimeoutSec <= 0 {
		cfg.EmbeddingTimeoutSec = 300
	}
	if cfg.EmbeddingRequired && !cfg.EmbeddingEnabled {
		return Config{}, fmt.Errorf("EMBEDDING_REQUIRED is set but EMBEDDING_ENABLED is false")
	}

	return cfg, nil
}

func LoadS3Only() (Config, error) {
	cfg := Config{
		S3Endpoint:     os.Getenv("S3_ENDPOINT"),
		S3Region:       getenv("S3_REGION", "us-east-1"),
		S3Bucket:       os.Getenv("S3_BUCKET"),
		S3AccessKey:    os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:    os.Getenv("S3_SECRET_KEY"),
		S3UsePathStyle: getenvBool("S3_USE_PATH_STYLE", true),
	}

	if cfg.S3Bucket == "" {
		return Config{}, fmt.Errorf("S3_BUCKET is required")
	}
	if cfg.S3AccessKey == "" {
		return Config{}, fmt.Errorf("S3_ACCESS_KEY is required")
	}
	if cfg.S3SecretKey == "" {
		return Config{}, fmt.Errorf("S3_SECRET_KEY is required")
	}
	if cfg.S3Endpoint == "" {
		return Config{}, fmt.Errorf("S3_ENDPOINT is required")
	}

	return cfg, nil
}

func getenv(key string, fallback string) string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	return v
}

func getenvInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getenvInt64(key string, fallback int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func getenvBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func defaultEmbeddingConcurrency() int {
	// One worker is the right default: the GoMLX backend already splits a single
	// forward pass across every core, so extra workers only queue behind it.
	// Measured per-image latency was flat from 1 to 4 concurrent images.
	return 1
}
