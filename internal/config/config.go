package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
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
	RangeChunkSize         int64
	WarmupFetchConcurrency int
	FeedDefaultSize        int
	RecoTopKDefault        int
	RecoTopKMax            int
	RecoWorkerConcurrency  int
	RecoWorkerCmd          string
	RecoWorkerTimeoutSec   int
	RecoWorkerRestartLimit int
	Siglip2ModelID         string
	Siglip2Device          string
}

func Load() (Config, error) {
	port := getenvInt("PORT", 8080)
	presignTTLSeconds := getenvInt("PRESIGN_TTL_SECONDS", 900)
	maxUploadBytes := getenvInt64("MAX_UPLOAD_BYTES", 1024*1024*1024)
	cacheDir := getenv("CACHE_DIR", ".cache/images")
	zipCacheDir := getenv("ZIP_CACHE_DIR", ".cache/zips")
	rangeChunkSize := getenvInt64("RANGE_CHUNK_SIZE_BYTES", 1<<17)
	recoWorkerCmd := getenv("RECO_WORKER_CMD", "RECO_WORKER_MODE=worker ./bin/reco-worker")

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
		RangeChunkSize:         rangeChunkSize,
		WarmupFetchConcurrency: getenvInt("WARMUP_FETCH_CONCURRENCY", 0),
		FeedDefaultSize:        getenvInt("FEED_DEFAULT_LIMIT", 80),
		RecoTopKDefault:        getenvInt("RECO_TOPK_DEFAULT", 12),
		RecoTopKMax:            getenvInt("RECO_TOPK_MAX", 48),
		RecoWorkerConcurrency:  getenvInt("RECO_WORKER_CONCURRENCY", 2),
		RecoWorkerCmd:          recoWorkerCmd,
		RecoWorkerTimeoutSec:   getenvInt("RECO_WORKER_REQUEST_TIMEOUT_SECONDS", 120),
		RecoWorkerRestartLimit: getenvInt("RECO_WORKER_RESTART_LIMIT", 10),
		Siglip2ModelID:         getenv("SIGLIP2_MODEL_ID", "google/siglip2-base-patch16-224"),
		Siglip2Device:          getenv("SIGLIP2_DEVICE", "cpu"),
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
	if cfg.WarmupFetchConcurrency < 0 {
		cfg.WarmupFetchConcurrency = 0
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
	if cfg.RecoWorkerTimeoutSec <= 0 {
		cfg.RecoWorkerTimeoutSec = 120
	}
	if cfg.RecoWorkerRestartLimit <= 0 {
		cfg.RecoWorkerRestartLimit = 10
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
