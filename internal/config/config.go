package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port                    int
	S3Endpoint              string
	S3Region                string
	S3Bucket                string
	S3AccessKey             string
	S3SecretKey             string
	S3UsePathStyle          bool
	SkipWarmup              bool
	PresignTTL              time.Duration
	MaxUploadBytes          int64
	CacheDir                string
	ZipCacheDir             string
	RangeChunkSize          int64
	FeedDefaultSize         int
	RecoEnabled             bool
	RecoDBPath              string
	RecoSyncIntervalSeconds int
	RecoTopKDefault         int
	RecoTopKMax             int
	RecoWorkerConcurrency   int
	RecoMaxRetries          int
	RecoWorkerCmd           string
	Siglip2ModelID          string
	Siglip2Device           string
	VectorBackend           string
}

func Load() (Config, error) {
	port := getenvInt("PORT", 8080)
	presignTTLSeconds := getenvInt("PRESIGN_TTL_SECONDS", 900)
	maxUploadBytes := getenvInt64("MAX_UPLOAD_BYTES", 1024*1024*1024)
	cacheDir := getenv("CACHE_DIR", ".cache/images")
	zipCacheDir := getenv("ZIP_CACHE_DIR", ".cache/zips")
	rangeChunkSize := getenvInt64("RANGE_CHUNK_SIZE_BYTES", 1<<17)
	recoDBPath := getenv("RECO_DB_PATH", ".cache/reco/reco.db")
	recoWorkerCmd := getenv("RECO_WORKER_CMD", "python3 scripts/reco_worker.py")

	cfg := Config{
		Port:                    port,
		S3Endpoint:              os.Getenv("S3_ENDPOINT"),
		S3Region:                getenv("S3_REGION", "us-east-1"),
		S3Bucket:                os.Getenv("S3_BUCKET"),
		S3AccessKey:             os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:             os.Getenv("S3_SECRET_KEY"),
		S3UsePathStyle:          getenvBool("S3_USE_PATH_STYLE", true),
		SkipWarmup:              getenvBool("SKIP_WARMUP", false),
		PresignTTL:              time.Duration(presignTTLSeconds) * time.Second,
		MaxUploadBytes:          maxUploadBytes,
		CacheDir:                cacheDir,
		ZipCacheDir:             zipCacheDir,
		RangeChunkSize:          rangeChunkSize,
		FeedDefaultSize:         getenvInt("FEED_DEFAULT_LIMIT", 80),
		RecoEnabled:             getenvBool("RECO_ENABLED", true),
		RecoDBPath:              recoDBPath,
		RecoSyncIntervalSeconds: getenvInt("RECO_SYNC_INTERVAL_SECONDS", 600),
		RecoTopKDefault:         getenvInt("RECO_TOPK_DEFAULT", 12),
		RecoTopKMax:             getenvInt("RECO_TOPK_MAX", 48),
		RecoWorkerConcurrency:   getenvInt("RECO_WORKER_CONCURRENCY", 2),
		RecoMaxRetries:          getenvInt("RECO_MAX_RETRIES", 5),
		RecoWorkerCmd:           recoWorkerCmd,
		Siglip2ModelID:          getenv("SIGLIP2_MODEL_ID", "google/siglip2-base-patch16-224"),
		Siglip2Device:           getenv("SIGLIP2_DEVICE", "cpu"),
		VectorBackend:           getenv("VECTOR_BACKEND", "embedded"),
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
	if cfg.RangeChunkSize <= 0 {
		return Config{}, fmt.Errorf("RANGE_CHUNK_SIZE_BYTES must be > 0")
	}
	if cfg.RecoTopKDefault <= 0 {
		cfg.RecoTopKDefault = 12
	}
	if cfg.RecoTopKMax <= 0 {
		cfg.RecoTopKMax = 48
	}
	if cfg.RecoWorkerConcurrency <= 0 {
		cfg.RecoWorkerConcurrency = 1
	}
	if cfg.RecoMaxRetries <= 0 {
		cfg.RecoMaxRetries = 5
	}
	if cfg.RecoSyncIntervalSeconds <= 0 {
		cfg.RecoSyncIntervalSeconds = 600
	}
	if cfg.RecoDBPath == "" {
		return Config{}, fmt.Errorf("RECO_DB_PATH is required")
	}
	if cfg.RecoWorkerCmd == "" {
		return Config{}, fmt.Errorf("RECO_WORKER_CMD is required")
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
