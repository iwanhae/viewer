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

// TestDeploymentConstants pins the value the Dockerfile bakes the checkpoint
// at, which has to stay in step with it.
func TestDeploymentConstants(t *testing.T) {
	if ModelDir != "/app/siglip2" {
		t.Errorf("ModelDir=%q want /app/siglip2 (the Dockerfile prefetch target)", ModelDir)
	}
}

// TestStateDirAccessors pins the layout inside STATE_DIR. Every path lives
// directly under it, so mounting one volume keeps the catalog and both caches.
func TestStateDirAccessors(t *testing.T) {
	cfg := Config{StateDir: "/data"}

	if got := cfg.DBPath(); got != "/data/viewer.db" {
		t.Errorf("DBPath=%q want /data/viewer.db", got)
	}
	if got := cfg.CacheDir(); got != "/data/images" {
		t.Errorf("CacheDir=%q want /data/images", got)
	}
	if got := cfg.ZipCacheDir(); got != "/data/zips" {
		t.Errorf("ZipCacheDir=%q want /data/zips", got)
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
}
