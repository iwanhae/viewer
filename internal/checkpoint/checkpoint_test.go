package checkpoint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// serveCheckpoint stands up a mirror with the two files under one prefix and
// counts requests, so tests can assert that a complete directory is not
// re-fetched.
func serveCheckpoint(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/siglip2/config.json":
			_, _ = w.Write([]byte(`{"model_type":"siglip"}`))
		case "/siglip2/model.safetensors":
			_, _ = w.Write([]byte("weights"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestEnsureDownloadsMissingFiles covers the cold start: an empty directory
// ends up holding both files, and a second Ensure over the finished directory
// performs no requests at all.
func TestEnsureDownloadsMissingFiles(t *testing.T) {
	srv, hits := serveCheckpoint(t)
	dir := t.TempDir()

	if err := Ensure(context.Background(), dir, srv.URL+"/siglip2/"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for name, want := range map[string]string{
		"config.json":       `{"model_type":"siglip"}`,
		"model.safetensors": "weights",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s=%q want %q", name, got, want)
		}
	}

	if err := Ensure(context.Background(), dir, srv.URL+"/siglip2/"); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if *hits != 2 {
		t.Fatalf("second Ensure re-fetched: hits=%d want 2", *hits)
	}
}

// TestEnsureFetchesOnlyTheMissingFile keeps a partially populated directory
// usable: a file that is already there - a checkpoint supplied by a mount - is
// kept, and only what is absent is downloaded.
func TestEnsureFetchesOnlyTheMissingFile(t *testing.T) {
	srv, hits := serveCheckpoint(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"mounted":true}`), 0o644); err != nil {
		t.Fatalf("seed config.json: %v", err)
	}

	if err := Ensure(context.Background(), dir, srv.URL+"/siglip2/"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if *hits != 1 {
		t.Fatalf("hits=%d want 1 (only the weights file)", *hits)
	}
	got, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if string(got) != `{"mounted":true}` {
		t.Fatalf("config.json was overwritten: %q", got)
	}
}

// TestEnsureSkipsEmptyFile treats a zero-length file as absent, the same
// incomplete state the image build used to refuse, instead of accepting it as
// a checkpoint that can never load.
func TestEnsureSkipsEmptyFile(t *testing.T) {
	srv, hits := serveCheckpoint(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model.safetensors"), nil, 0o644); err != nil {
		t.Fatalf("seed model.safetensors: %v", err)
	}

	if err := Ensure(context.Background(), dir, srv.URL+"/siglip2/"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if *hits != 2 {
		t.Fatalf("hits=%d want 2 (empty weights file is not a checkpoint)", *hits)
	}
	got, err := os.ReadFile(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		t.Fatalf("read model.safetensors: %v", err)
	}
	if string(got) != "weights" {
		t.Fatalf("model.safetensors=%q want the fetched contents", got)
	}
}

// TestEnsureFailsWithoutPartialFile pins the crash-safety contract: a failed
// download reports the status, leaves no file under the final name and no
// .part litter, so the next start retries cleanly.
func TestEnsureFailsWithoutPartialFile(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	dir := t.TempDir()

	err := Ensure(context.Background(), dir, srv.URL)
	if err == nil {
		t.Fatalf("Ensure expected an error against a 404 mirror")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed download left %d entries behind", len(entries))
	}
}
