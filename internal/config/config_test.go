package config

import (
	"testing"

	"viewer/internal/vision"
)

func TestLoadEmbeddingDefaults(t *testing.T) {
	setRequiredEnv(t)
	// The test env sets this to false to keep tests hermetic; the default is on.
	t.Setenv("EMBEDDING_ENABLED", "")
	t.Setenv("EMBEDDING_MODEL_ID", "")
	t.Setenv("EMBEDDING_BACKEND", "")
	t.Setenv("EMBEDDING_CONCURRENCY", "")
	t.Setenv("EMBEDDING_REQUEST_TIMEOUT_SECONDS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EmbeddingEnabled {
		t.Fatalf("EmbeddingEnabled=false want=true")
	}
	if cfg.EmbeddingModelID != vision.DefaultModelID {
		t.Fatalf("EmbeddingModelID=%q want=%q", cfg.EmbeddingModelID, vision.DefaultModelID)
	}
	if cfg.EmbeddingBackend != vision.DefaultBackend {
		t.Fatalf("EmbeddingBackend=%q want=%q", cfg.EmbeddingBackend, vision.DefaultBackend)
	}
	if cfg.EmbeddingRequired {
		t.Fatalf("EmbeddingRequired=true want=false so a missing model degrades gracefully")
	}
	if want := defaultEmbeddingConcurrency(); cfg.EmbeddingConcurrency != want {
		t.Fatalf("EmbeddingConcurrency=%d want=%d", cfg.EmbeddingConcurrency, want)
	}
	if got, want := cfg.EmbeddingTimeoutSec, 300; got != want {
		t.Fatalf("EmbeddingTimeoutSec=%d want=%d", got, want)
	}
}

// TestEmbeddingDefaultsMatchVision guards the intentional duplication of the
// model id and backend defaults: config must not import the inference stack,
// because album-dedupe-cleaner only needs configuration.
func TestEmbeddingDefaultsMatchVision(t *testing.T) {
	if defaultEmbeddingModelID != vision.DefaultModelID {
		t.Errorf("defaultEmbeddingModelID=%q want %q", defaultEmbeddingModelID, vision.DefaultModelID)
	}
	if defaultEmbeddingBackend != vision.DefaultBackend {
		t.Errorf("defaultEmbeddingBackend=%q want %q", defaultEmbeddingBackend, vision.DefaultBackend)
	}
}

func TestLoadEmbeddingOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("EMBEDDING_ENABLED", "false")
	t.Setenv("EMBEDDING_MODEL_ID", "/models/siglip2-local")
	t.Setenv("EMBEDDING_BACKEND", "xla:cpu")
	t.Setenv("EMBEDDING_CACHE_DIR", "/var/cache/hf")
	t.Setenv("EMBEDDING_CONCURRENCY", "3")
	t.Setenv("EMBEDDING_REQUEST_TIMEOUT_SECONDS", "45")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbeddingEnabled {
		t.Fatalf("EmbeddingEnabled=true want=false")
	}
	if got, want := cfg.EmbeddingModelID, "/models/siglip2-local"; got != want {
		t.Fatalf("EmbeddingModelID=%q want=%q", got, want)
	}
	if got, want := cfg.EmbeddingBackend, "xla:cpu"; got != want {
		t.Fatalf("EmbeddingBackend=%q want=%q", got, want)
	}
	if got, want := cfg.EmbeddingCacheDir, "/var/cache/hf"; got != want {
		t.Fatalf("EmbeddingCacheDir=%q want=%q", got, want)
	}
	if got, want := cfg.EmbeddingConcurrency, 3; got != want {
		t.Fatalf("EmbeddingConcurrency=%d want=%d", got, want)
	}
	if got, want := cfg.EmbeddingTimeoutSec, 45; got != want {
		t.Fatalf("EmbeddingTimeoutSec=%d want=%d", got, want)
	}
}

func TestLoadRejectsRequiredButDisabledEmbedding(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("EMBEDDING_REQUIRED", "true")
	t.Setenv("EMBEDDING_ENABLED", "false")

	if _, err := Load(); err == nil {
		t.Fatalf("Load expected an error for required-but-disabled embedding")
	}
}

func TestLoadEmbeddingConcurrencyFallsBackToDefault(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("EMBEDDING_CONCURRENCY", "0")
	t.Setenv("EMBEDDING_REQUEST_TIMEOUT_SECONDS", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.EmbeddingConcurrency, defaultEmbeddingConcurrency(); got != want {
		t.Fatalf("EmbeddingConcurrency=%d want=%d", got, want)
	}
	if got, want := cfg.EmbeddingTimeoutSec, 300; got != want {
		t.Fatalf("EmbeddingTimeoutSec=%d want=%d", got, want)
	}
}

// TestDefaultEmbeddingConcurrencyIsSingle pins the measured default: the
// backend already parallelises one forward pass across all cores, so more
// workers only add queueing.
func TestDefaultEmbeddingConcurrencyIsSingle(t *testing.T) {
	if got := defaultEmbeddingConcurrency(); got != 1 {
		t.Fatalf("defaultEmbeddingConcurrency()=%d want=1", got)
	}
}

func TestLoadS3OnlyRequiresS3ValuesOnly(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "https://example.invalid")
	t.Setenv("S3_BUCKET", "viewer")
	t.Setenv("S3_ACCESS_KEY", "access")
	t.Setenv("S3_SECRET_KEY", "secret")

	cfg, err := LoadS3Only()
	if err != nil {
		t.Fatalf("LoadS3Only: %v", err)
	}
	if cfg.S3Bucket != "viewer" {
		t.Fatalf("S3Bucket=%q want viewer", cfg.S3Bucket)
	}
}

func TestLoadCatalogDefaults(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DB_PATH", "")
	t.Setenv("INGEST_DELETE_SOURCE", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.DBPath, ".cache/viewer.db"; got != want {
		t.Fatalf("DBPath=%q want=%q", got, want)
	}
	if !cfg.IngestDeleteSource {
		t.Fatalf("IngestDeleteSource=false want=true")
	}
}

func TestLoadCatalogOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DB_PATH", "/var/lib/viewer/catalog.db")
	t.Setenv("INGEST_DELETE_SOURCE", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.DBPath, "/var/lib/viewer/catalog.db"; got != want {
		t.Fatalf("DBPath=%q want=%q", got, want)
	}
	if cfg.IngestDeleteSource {
		t.Fatalf("IngestDeleteSource=true want=false")
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("S3_ENDPOINT", "https://example.invalid")
	t.Setenv("S3_BUCKET", "viewer")
	t.Setenv("S3_ACCESS_KEY", "access")
	t.Setenv("S3_SECRET_KEY", "secret")
}
