package config

import (
	"testing"
)

func TestLoadRequiresEveryS3Value(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{name: "missing endpoint", env: map[string]string{"S3_ENDPOINT": ""}},
		{name: "missing bucket", env: map[string]string{"S3_BUCKET": ""}},
		{name: "missing access key", env: map[string]string{"S3_ACCESS_KEY": ""}},
		{name: "missing secret key", env: map[string]string{"S3_SECRET_KEY": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			if _, err := Load(); err == nil {
				t.Fatalf("Load expected an error for %s", tc.name)
			}
		})
	}
}

func TestLoadDefaults(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Fatalf("Port=%d want=%d", cfg.Port, DefaultPort)
	}
	if cfg.S3Bucket != "viewer" {
		t.Fatalf("S3Bucket=%q want viewer", cfg.S3Bucket)
	}
}

func TestLoadPortOverrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORT", "18080")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 18080 {
		t.Fatalf("Port=%d want=18080", cfg.Port)
	}
}

// TestLoadFallsBackOnUnusablePort keeps a bad PORT from producing a server that
// cannot listen, rather than failing the whole container.
func TestLoadFallsBackOnUnusablePort(t *testing.T) {
	for _, value := range []string{"not-a-number", "0", "-1", "70000"} {
		setRequiredEnv(t)
		t.Setenv("PORT", value)

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(PORT=%q): %v", value, err)
		}
		if cfg.Port != DefaultPort {
			t.Fatalf("Port=%d want=%d for PORT=%q", cfg.Port, DefaultPort, value)
		}
	}
}

// TestDeploymentConstants pins the values that used to be environment
// variables. The Dockerfile bakes the checkpoint at ModelDir and pre-creates
// the cache directories, so these two have to stay in step with it.
func TestDeploymentConstants(t *testing.T) {
	if ModelDir != "/app/siglip2" {
		t.Errorf("ModelDir=%q want /app/siglip2 (the Dockerfile prefetch target)", ModelDir)
	}
	if DBPath != StateDir+"/viewer.db" || CacheDir != StateDir+"/images" || ZipCacheDir != StateDir+"/zips" {
		t.Errorf("state paths must live under StateDir=%q", StateDir)
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("S3_ENDPOINT", "https://example.invalid")
	t.Setenv("S3_BUCKET", "viewer")
	t.Setenv("S3_ACCESS_KEY", "access")
	t.Setenv("S3_SECRET_KEY", "secret")
}
