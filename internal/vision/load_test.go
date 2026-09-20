package vision

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooksLikePath(t *testing.T) {
	cases := map[string]bool{
		"/models/siglip2":                 true,
		"./siglip2":                       true,
		"../siglip2":                      true,
		"~/siglip2":                       true,
		"google/siglip2-base-patch16-224": false,
		"siglip2-local":                   false,
	}
	for input, want := range cases {
		if got := looksLikePath(input); got != want {
			t.Errorf("looksLikePath(%q)=%t want=%t", input, got, want)
		}
	}
}

// TestLoadMissingLocalPathFailsWithoutHub checks that a path-shaped model id
// that does not exist is reported as a local error, not resolved as a
// HuggingFace repository id (which would attempt a network request).
func TestLoadMissingLocalPathFailsWithoutHub(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent-model")
	_, err := Load(context.Background(), Config{ModelID: missing, Backend: "go"})
	if err == nil {
		t.Fatalf("Load expected an error for a missing local model directory")
	}
	if !strings.Contains(err.Error(), "model directory") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("err=%v want a local model directory error naming %q", err, missing)
	}
}

// TestLoadRejectsFileAsModelPath checks the non-directory case.
func TestLoadRejectsFileAsModelPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "model.safetensors")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(context.Background(), Config{ModelID: file, Backend: "go"})
	if err == nil {
		t.Fatalf("Load expected an error for a file model path")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err=%v want a 'not a directory' error", err)
	}
}

// TestLoadMissingRemoteConfigFails checks the Hub branch reports the failure
// rather than panicking. It is skipped in short mode because it may fetch.
func TestLoadMissingRemoteConfigFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network-touching test in short mode")
	}
	_, err := Load(context.Background(), Config{
		ModelID:  "viewer-nonexistent-model-id-for-tests",
		Backend:  "go",
		CacheDir: t.TempDir(),
	})
	if err == nil {
		t.Fatalf("Load expected an error for an unknown repository")
	}
}
