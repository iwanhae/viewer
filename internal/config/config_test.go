package config

import (
	"strings"
	"testing"
)

func TestLoadDefaultRangeChunkSize(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RANGE_CHUNK_SIZE_BYTES", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.RangeChunkSize, int64(1<<17); got != want {
		t.Fatalf("RangeChunkSize=%d want=%d", got, want)
	}
}

func TestLoadCustomRangeChunkSize(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RANGE_CHUNK_SIZE_BYTES", "262144")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.RangeChunkSize, int64(262144); got != want {
		t.Fatalf("RangeChunkSize=%d want=%d", got, want)
	}
}

func TestLoadRejectsNonPositiveRangeChunkSize(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RANGE_CHUNK_SIZE_BYTES", "0")

	_, err := Load()
	if err == nil {
		t.Fatalf("Load expected error for non-positive RANGE_CHUNK_SIZE_BYTES")
	}
	if got := err.Error(); !strings.Contains(got, "RANGE_CHUNK_SIZE_BYTES must be > 0") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadDefaultRecoWorkerCommand(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RECO_WORKER_CMD", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.RecoWorkerCmd, "RECO_WORKER_MODE=worker ./bin/reco-worker"; got != want {
		t.Fatalf("RecoWorkerCmd=%q want=%q", got, want)
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("S3_ENDPOINT", "https://example.invalid")
	t.Setenv("S3_BUCKET", "viewer")
	t.Setenv("S3_ACCESS_KEY", "access")
	t.Setenv("S3_SECRET_KEY", "secret")
}
