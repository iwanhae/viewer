package config

import (
	"strings"
	"testing"
)

func TestLoadRequiresRecommenderEndpoint(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RECOMMENDER_REQUIRED", "true")
	t.Setenv("RECOMMENDER_ENDPOINT", "")

	_, err := Load()
	if err == nil {
		t.Fatalf("Load expected error for missing RECOMMENDER_ENDPOINT")
	}
	if got := err.Error(); !strings.Contains(got, "RECOMMENDER_ENDPOINT is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadAllowsMissingRecommenderEndpointWhenNotRequired(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RECOMMENDER_REQUIRED", "false")
	t.Setenv("RECOMMENDER_ENDPOINT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.RecommenderEndpoint; got != "" {
		t.Fatalf("RecommenderEndpoint=%q want empty", got)
	}
}

func TestLoadDefaultRecommenderRequired(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RECOMMENDER_REQUIRED", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.RecommenderRequired, true; got != want {
		t.Fatalf("RecommenderRequired=%v want=%v", got, want)
	}
}

func TestLoadRecommenderRequiredFalse(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RECOMMENDER_REQUIRED", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.RecommenderRequired, false; got != want {
		t.Fatalf("RecommenderRequired=%v want=%v", got, want)
	}
}

func TestLoadRecommenderEndpoint(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RECOMMENDER_ENDPOINT", "http://127.0.0.1:8081")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.RecommenderEndpoint, "http://127.0.0.1:8081"; got != want {
		t.Fatalf("RecommenderEndpoint=%q want=%q", got, want)
	}
}

func TestLoadS3OnlyRequiresS3ValuesOnly(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "https://example.invalid")
	t.Setenv("S3_BUCKET", "viewer")
	t.Setenv("S3_ACCESS_KEY", "access")
	t.Setenv("S3_SECRET_KEY", "secret")
	t.Setenv("RECOMMENDER_REQUIRED", "true")
	t.Setenv("RECOMMENDER_ENDPOINT", "")

	cfg, err := LoadS3Only()
	if err != nil {
		t.Fatalf("LoadS3Only: %v", err)
	}
	if cfg.S3Bucket != "viewer" {
		t.Fatalf("S3Bucket=%q want viewer", cfg.S3Bucket)
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("S3_ENDPOINT", "https://example.invalid")
	t.Setenv("S3_BUCKET", "viewer")
	t.Setenv("S3_ACCESS_KEY", "access")
	t.Setenv("S3_SECRET_KEY", "secret")
	t.Setenv("RECOMMENDER_ENDPOINT", "http://127.0.0.1:18081")
}
