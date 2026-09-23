package config

import (
	"strings"
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

// TestDeploymentConstants pins the directory the image pre-creates for the
// checkpoint: the startup download fills it and a volume mount can replace it.
func TestDeploymentConstants(t *testing.T) {
	if ModelDir != "/app/siglip2" {
		t.Errorf("ModelDir=%q want /app/siglip2 (the checkpoint directory the image pre-creates)", ModelDir)
	}
}

// TestLoadModelURL covers the checkpoint mirror default and the trailing-slash
// trim, so the downloader can append a filename directly.
func TestLoadModelURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unset", raw: "", want: DefaultModelURL},
		{name: "blank", raw: "   ", want: DefaultModelURL},
		{name: "slashes only", raw: "///", want: DefaultModelURL},
		{name: "override", raw: "https://mirror.example.test/models/siglip2", want: "https://mirror.example.test/models/siglip2"},
		{name: "trailing slash", raw: "https://mirror.example.test/models/siglip2/", want: "https://mirror.example.test/models/siglip2"},
		{name: "surrounding spaces", raw: "  https://mirror.example.test/m  ", want: "https://mirror.example.test/m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("SIGLIP2_MODEL_URL", tc.raw)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.ModelURL != tc.want {
				t.Fatalf("ModelURL=%q want=%q for SIGLIP2_MODEL_URL=%q", cfg.ModelURL, tc.want, tc.raw)
			}
		})
	}
}

// TestStateDirAccessors pins the one path derived from STATE_DIR: the catalog,
// which is the only thing an operator's volume is asked to preserve.
func TestStateDirAccessors(t *testing.T) {
	cfg := Config{StateDir: "/data"}

	if got := cfg.DBPath(); got != "/data/viewer.db" {
		t.Errorf("DBPath=%q want /data/viewer.db", got)
	}
}

// TestLoadStateDir covers the STATE_DIR default, its cleaning, and the
// rejection of a relative path, which would put the catalog somewhere a volume
// mount cannot preserve.
func TestLoadStateDir(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "unset", raw: "", want: DefaultStateDir},
		{name: "blank", raw: "   ", want: DefaultStateDir},
		{name: "absolute", raw: "/data/viewer", want: "/data/viewer"},
		{name: "trailing slash", raw: "/data/viewer/", want: "/data/viewer"},
		{name: "surrounding spaces", raw: "  /data/viewer  ", want: "/data/viewer"},
		{name: "redundant separators", raw: "/data//viewer", want: "/data/viewer"},
		{name: "root", raw: "/", want: "/"},
		{name: "relative", raw: "data/viewer", wantErr: true},
		{name: "relative dot", raw: "./data", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("STATE_DIR", tc.raw)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load(STATE_DIR=%q) expected an error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.StateDir != tc.want {
				t.Fatalf("StateDir=%q want=%q for STATE_DIR=%q", cfg.StateDir, tc.want, tc.raw)
			}
		})
	}
}

// TestLoadS3UsePathStyle covers the addressing switch: path-style is the
// default, and a value that cannot be parsed fails the start rather than
// silently sending every request to a different host.
func TestLoadS3UsePathStyle(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    bool
		wantErr bool
	}{
		{name: "unset", raw: "", want: DefaultS3UsePathStyle},
		{name: "blank", raw: "   ", want: DefaultS3UsePathStyle},
		{name: "true", raw: "true", want: true},
		{name: "false", raw: "false", want: false},
		{name: "one", raw: "1", want: true},
		{name: "zero", raw: "0", want: false},
		{name: "surrounding spaces", raw: " false ", want: false},
		{name: "unparseable", raw: "yes", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("S3_USE_PATH_STYLE", tc.raw)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load(S3_USE_PATH_STYLE=%q) expected an error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.S3UsePathStyle != tc.want {
				t.Fatalf("S3UsePathStyle=%v want=%v for S3_USE_PATH_STYLE=%q", cfg.S3UsePathStyle, tc.want, tc.raw)
			}
		})
	}
}

// TestLoadNormalizesS3Prefix pins the exact string that gets prepended to every
// object key: an unset or slash-only value must stay empty (the historical flat
// layout), and a real value always ends in exactly one slash.
func TestLoadNormalizesS3Prefix(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "unset", raw: "", want: ""},
		{name: "blank", raw: "   ", want: ""},
		{name: "slash only", raw: "/", want: ""},
		{name: "slashes only", raw: "///", want: ""},
		{name: "bare name", raw: "viewer", want: "viewer/"},
		{name: "trailing slash", raw: "viewer/", want: "viewer/"},
		{name: "surrounding slashes", raw: "/viewer/", want: "viewer/"},
		{name: "surrounding spaces", raw: "  viewer  ", want: "viewer/"},
		{name: "nested", raw: "team-a/viewer", want: "team-a/viewer/"},
		{name: "nested with slashes", raw: "/team-a/viewer/", want: "team-a/viewer/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("S3_PREFIX", tc.raw)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.S3Prefix != tc.want {
				t.Fatalf("S3Prefix=%q want=%q for S3_PREFIX=%q", cfg.S3Prefix, tc.want, tc.raw)
			}
			wantDescribe := tc.want
			if wantDescribe == "" {
				wantDescribe = "(bucket root)"
			}
			if got := cfg.DescribePrefix(); got != wantDescribe {
				t.Fatalf("DescribePrefix()=%q want=%q", got, wantDescribe)
			}
		})
	}
}

// TestLoadDefaultsToNoS3Prefix keeps the prefix optional: an unset S3_PREFIX
// must leave the deployment storing objects at the bucket root.
func TestLoadDefaultsToNoS3Prefix(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("S3_PREFIX", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.S3Prefix != "" {
		t.Fatalf("S3Prefix=%q want empty", cfg.S3Prefix)
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("S3_ENDPOINT", "https://example.invalid")
	t.Setenv("S3_BUCKET", "viewer")
	t.Setenv("S3_ACCESS_KEY", "access")
	t.Setenv("S3_SECRET_KEY", "secret")
	t.Setenv("QDRANT_URL", "https://qdrant.example.test")
	t.Setenv("QDRANT_API_KEY", "qdrant-key")
}

// A whitespace-only token was set by an operator who meant to protect the
// worker API, so it must fail loudly instead of silently running open.
func TestLoadRejectsBlankWorkerToken(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "https://s3.example.com")
	t.Setenv("S3_BUCKET", "viewer")
	t.Setenv("S3_ACCESS_KEY", "ak")
	t.Setenv("S3_SECRET_KEY", "sk")
	t.Setenv("STATE_DIR", "/tmp/viewer-test")
	t.Setenv("EMBEDDING_WORKER_TOKEN", "   ")

	if _, err := Load(); err == nil {
		t.Fatalf("expected a blank EMBEDDING_WORKER_TOKEN to be rejected")
	}
}

// TestLoadRequiresEveryQdrantValue mirrors the S3 requirement for the Qdrant
// settings vector search depends on, and pins the message each failure names.
func TestLoadRequiresEveryQdrantValue(t *testing.T) {
	cases := []struct {
		name    string
		envKey  string
		wantErr string
	}{
		{name: "missing url", envKey: "QDRANT_URL", wantErr: "QDRANT_URL is required"},
		{name: "missing api key", envKey: "QDRANT_API_KEY", wantErr: "QDRANT_API_KEY is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tc.envKey, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("Load expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load error %q want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestLoadQdrantURL covers the base-URL trimming and the rejection of a value
// the Qdrant REST client could not append a collection path to, so a bad
// QDRANT_URL fails the start instead of the first vector search.
func TestLoadQdrantURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "https", raw: "https://qdrant.example.test", want: "https://qdrant.example.test"},
		{name: "http with port", raw: "http://qdrant:6333", want: "http://qdrant:6333"},
		{name: "path prefix kept", raw: "https://proxy.example.test/qdrant", want: "https://proxy.example.test/qdrant"},
		{name: "trailing slash", raw: "https://qdrant.example.test/", want: "https://qdrant.example.test"},
		{name: "surrounding spaces", raw: "  https://qdrant.example.test  ", want: "https://qdrant.example.test"},
		{name: "no scheme", raw: "qdrant.example.test", wantErr: true},
		{name: "unsupported scheme", raw: "ftp://qdrant.example.test", wantErr: true},
		{name: "no host", raw: "https://", wantErr: true},
		{name: "whitespace only", raw: "   ", wantErr: true},
		{name: "slashes only", raw: "///", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("QDRANT_URL", tc.raw)

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Load(QDRANT_URL=%q) expected an error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.QdrantURL != tc.want {
				t.Fatalf("QdrantURL=%q want=%q for QDRANT_URL=%q", cfg.QdrantURL, tc.want, tc.raw)
			}
		})
	}
}

// A whitespace-only QDRANT_API_KEY was set by an operator who meant to
// authenticate to Qdrant, so it must fail loudly and by name instead of
// reading like a variable that was never supplied.
func TestLoadRejectsBlankQdrantAPIKey(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("QDRANT_API_KEY", "   ")

	_, err := Load()
	if err == nil {
		t.Fatalf("expected a blank QDRANT_API_KEY to be rejected")
	}
	if !strings.Contains(err.Error(), "QDRANT_API_KEY is set but blank") {
		t.Fatalf("Load error %q want it to contain %q", err.Error(), "QDRANT_API_KEY is set but blank")
	}
}

// TestLoadQdrantHappyPath pins the fully configured boot: every required value
// present and ADMIN_TOKEN absent, which must load with AdminToken empty so the
// startup log can report the admin UI as disabled.
func TestLoadQdrantHappyPath(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ADMIN_TOKEN", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.QdrantURL != "https://qdrant.example.test" {
		t.Fatalf("QdrantURL=%q want https://qdrant.example.test", cfg.QdrantURL)
	}
	if cfg.QdrantAPIKey != "qdrant-key" {
		t.Fatalf("QdrantAPIKey=%q want qdrant-key", cfg.QdrantAPIKey)
	}
	if cfg.AdminToken != "" {
		t.Fatalf("AdminToken=%q want empty when ADMIN_TOKEN is unset", cfg.AdminToken)
	}
}

// TestLoadAdminToken covers the admin page being opt-in: an unset ADMIN_TOKEN
// (indistinguishable from blank to os.Getenv) passes through empty, a set
// value is trimmed, and a set-but-blank value is rejected rather than silently
// meaning off.
func TestLoadAdminToken(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr string
	}{
		{name: "unset", raw: "", want: ""},
		{name: "set", raw: "admin-token", want: "admin-token"},
		{name: "surrounding spaces", raw: "  admin-token  ", want: "admin-token"},
		{name: "set but blank", raw: "   ", wantErr: "ADMIN_TOKEN is set but blank"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("ADMIN_TOKEN", tc.raw)

			cfg, err := Load()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Load(ADMIN_TOKEN=%q) expected an error", tc.raw)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load error %q want it to contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.AdminToken != tc.want {
				t.Fatalf("AdminToken=%q want=%q for ADMIN_TOKEN=%q", cfg.AdminToken, tc.want, tc.raw)
			}
		})
	}
}
