package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port            int
	S3Endpoint      string
	S3Region        string
	S3Bucket        string
	S3AccessKey     string
	S3SecretKey     string
	S3UsePathStyle  bool
	PresignTTL      time.Duration
	MaxUploadBytes  int64
	CacheDir        string
	ZipCacheDir     string
	FeedDefaultSize int
}

func Load() (Config, error) {
	port := getenvInt("PORT", 8080)
	presignTTLSeconds := getenvInt("PRESIGN_TTL_SECONDS", 900)
	maxUploadBytes := getenvInt64("MAX_UPLOAD_BYTES", 1024*1024*1024)
	cacheDir := getenv("CACHE_DIR", ".cache/images")
	zipCacheDir := getenv("ZIP_CACHE_DIR", ".cache/zips")

	cfg := Config{
		Port:            port,
		S3Endpoint:      os.Getenv("S3_ENDPOINT"),
		S3Region:        getenv("S3_REGION", "us-east-1"),
		S3Bucket:        os.Getenv("S3_BUCKET"),
		S3AccessKey:     os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:     os.Getenv("S3_SECRET_KEY"),
		S3UsePathStyle:  getenvBool("S3_USE_PATH_STYLE", true),
		PresignTTL:      time.Duration(presignTTLSeconds) * time.Second,
		MaxUploadBytes:  maxUploadBytes,
		CacheDir:        cacheDir,
		ZipCacheDir:     zipCacheDir,
		FeedDefaultSize: getenvInt("FEED_DEFAULT_LIMIT", 80),
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
