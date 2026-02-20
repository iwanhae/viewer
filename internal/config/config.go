package config

import (
	"fmt"
	"os"
	"runtime"
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
	WarmupFetchConcurrency int
	RecoTopKDefault        int
	RecoTopKMax            int
	RecommenderEndpoint    string
	RecommenderRequired    bool
	RecommenderConcurrency int
	RecommenderTimeoutSec  int
}

func Load() (Config, error) {
	port := getenvInt("PORT", 8080)
	presignTTLSeconds := getenvInt("PRESIGN_TTL_SECONDS", 900)
	maxUploadBytes := getenvInt64("MAX_UPLOAD_BYTES", 1024*1024*1024)
	cacheDir := getenv("CACHE_DIR", ".cache/images")
	zipCacheDir := getenv("ZIP_CACHE_DIR", ".cache/zips")
	recommenderEndpoint := os.Getenv("RECOMMENDER_ENDPOINT")

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
		WarmupFetchConcurrency: getenvInt("WARMUP_FETCH_CONCURRENCY", 0),
		RecoTopKDefault:        getenvInt("RECO_TOPK_DEFAULT", 12),
		RecoTopKMax:            getenvInt("RECO_TOPK_MAX", 48),
		RecommenderEndpoint:    recommenderEndpoint,
		RecommenderRequired:    getenvBool("RECOMMENDER_REQUIRED", true),
		RecommenderConcurrency: getenvInt("RECOMMENDER_CONCURRENCY", defaultRecommenderConcurrency()),
		RecommenderTimeoutSec:  getenvInt("RECOMMENDER_REQUEST_TIMEOUT_SECONDS", 120),
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
	if cfg.RecommenderConcurrency <= 0 {
		cfg.RecommenderConcurrency = defaultRecommenderConcurrency()
	}
	if cfg.RecommenderTimeoutSec <= 0 {
		cfg.RecommenderTimeoutSec = 120
	}
	if cfg.RecommenderRequired && cfg.RecommenderEndpoint == "" {
		return Config{}, fmt.Errorf("RECOMMENDER_ENDPOINT is required")
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

func defaultRecommenderConcurrency() int {
	workers := runtime.GOMAXPROCS(0) * 3
	if workers < 8 {
		return 8
	}
	if workers > 64 {
		return 64
	}
	return workers
}
