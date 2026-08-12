package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/engine"
)

// envWith builds a test environment whose Getenv reads from a map rather than the process,
// so a THROTTLE_LEDGER left over in the developer's shell cannot change what these assert.
func envWith(t *testing.T, home string, vars map[string]string) Env {
	t.Helper()
	return Env{
		Home:   home,
		GOOS:   "linux",
		Getenv: func(k string) string { return vars[k] },
	}
}

func ptr[T any](v T) *T { return &v }

// (14) The precedence chain, one field at a time, each stage overriding the last:
//
//	built-in defaults  <  config file  <  environment  <  CLI flag
//
// The ledger path is the field every stage can set, which is what makes it the one worth
// walking the whole chain with.
func TestPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(cfgPath, []byte(`
store:
  ledger: `+filepath.Join(dir, "from-file.db")+`
`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fromFile := filepath.Join(dir, "from-file.db")
	fromEnv := filepath.Join(dir, "from-env.db")
	fromFlag := filepath.Join(dir, "from-flag.db")

	tests := []struct {
		name   string
		vars   map[string]string
		over   Overrides
		want   string
		origin Source
	}{
		{
			name:   "default",
			vars:   map[string]string{},
			want:   filepath.Join(dir, ".local", "share", "throttle", "ledger.db"),
			origin: FromDefault,
		},
		{
			name:   "file beats default",
			vars:   map[string]string{EnvConfig: cfgPath},
			want:   fromFile,
			origin: FromFile,
		},
		{
			name:   "environment beats file",
			vars:   map[string]string{EnvConfig: cfgPath, EnvLedger: fromEnv},
			want:   fromEnv,
			origin: FromEnv,
		},
		{
			name:   "flag beats environment",
			vars:   map[string]string{EnvConfig: cfgPath, EnvLedger: fromEnv},
			over:   Overrides{Ledger: &fromFlag},
			want:   fromFlag,
			origin: FromFlag,
		},
		{
			name:   "flag beats file with no environment",
			vars:   map[string]string{EnvConfig: cfgPath},
			over:   Overrides{Ledger: &fromFlag},
			want:   fromFlag,
			origin: FromFlag,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(envWith(t, dir, tt.vars), tt.over)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Ledger != tt.want {
				t.Errorf("ledger = %q, want %q", cfg.Ledger, tt.want)
			}
			// The origin is not decoration: it is what lets "config show" answer "why is it
			// using that ledger?" without anybody reading source.
			if got := cfg.source("store.ledger"); got != tt.origin {
				t.Errorf("origin = %q, want %q", got, tt.origin)
			}
		})
	}
}

// Nothing is merged. A value comes from exactly one stage, and a later stage that sets one
// field does not disturb another field's origin.
func TestPrecedenceDoesNotMerge(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(cfgPath, []byte(`
store:
  ledger: `+filepath.Join(dir, "file-ledger.db")+`
  activity: `+filepath.Join(dir, "file-activity.db")+`
defaults:
  enforcement: monitor
  lease: 5m
dashboard:
  listen: 127.0.0.1:8000
  activity_limit: 25
`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(
		envWith(t, dir, map[string]string{EnvConfig: cfgPath}),
		Overrides{Enforcement: ptr("wait"), Listen: ptr("127.0.0.1:9000")},
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Overridden.
	if cfg.Enforcement != engine.ModeWait {
		t.Errorf("enforcement = %q, want wait", cfg.Enforcement)
	}
	if cfg.Listen != "127.0.0.1:9000" {
		t.Errorf("listen = %q, want the flag's address", cfg.Listen)
	}
	// Untouched, and still attributed to the file.
	if cfg.Lease != 5*time.Minute {
		t.Errorf("lease = %v, want the file's 5m", cfg.Lease)
	}
	if cfg.ActivityLimit != 25 {
		t.Errorf("activity limit = %d, want the file's 25", cfg.ActivityLimit)
	}
	if got := cfg.source("defaults.lease"); got != FromFile {
		t.Errorf("lease origin = %q, want the config file", got)
	}
	if got := cfg.source("store.activity"); got != FromFile {
		t.Errorf("activity origin = %q, want the config file", got)
	}
}

// No configuration at all is a working configuration. "Go and try it" should not require a
// file first.
func TestLoadWithNoFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(envWith(t, dir, map[string]string{}), Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Path != "" {
		t.Errorf("path = %q, want empty with no file", cfg.Path)
	}
	if cfg.Ledger == "" {
		t.Error("no default ledger path")
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q, want the loopback default", cfg.Listen)
	}
}

// A file named explicitly and missing is an error. Falling back to defaults would run the
// wrong budget quietly, which is the failure this distinction exists to prevent.
func TestExplicitMissingFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "absent.yaml")

	t.Run("flag", func(t *testing.T) {
		_, err := Load(envWith(t, dir, map[string]string{}), Overrides{Path: &absent})
		if err == nil {
			t.Fatal("want an error for a -config file that does not exist")
		}
		if !strings.Contains(err.Error(), "no such file") {
			t.Errorf("error = %v, want it to say the file is missing", err)
		}
	})

	t.Run("environment", func(t *testing.T) {
		_, err := Load(envWith(t, dir, map[string]string{EnvConfig: absent}), Overrides{})
		if err == nil {
			t.Fatal("want an error for a THROTTLE_CONFIG file that does not exist")
		}
		if !strings.Contains(err.Error(), EnvConfig) {
			t.Errorf("error = %v, want it to name the variable that set the path", err)
		}
	})

	t.Run("default location", func(t *testing.T) {
		// The default location is a suggestion, not a requirement.
		cfg, err := Load(envWith(t, dir, map[string]string{}), Overrides{})
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Path != "" {
			t.Errorf("path = %q, want empty", cfg.Path)
		}
	})
}

// A tilde in an environment variable is expanded, because nothing else will: it is not a
// shell that reads THROTTLE_LEDGER out of a container's environment.
func TestEnvironmentPathsExpand(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(envWith(t, dir, map[string]string{EnvLedger: "~/custom/ledger.db"}), Overrides{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(dir, "custom", "ledger.db"); cfg.Ledger != want {
		t.Errorf("ledger = %q, want %q", cfg.Ledger, want)
	}
}

// Bad flag values are reported together and name the flag the reader typed, not the field
// path a config file would use.
func TestFlagOverrideErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(envWith(t, dir, map[string]string{}), Overrides{
		Enforcement:   ptr("pretend"),
		Lease:         ptr("1mo"),
		Listen:        ptr(""),
		ActivityLimit: ptr(-1),
	})
	if err == nil {
		t.Fatal("want errors for four bad overrides")
	}
	msg := err.Error()
	for _, want := range []string{"-mode", "-lease", "-listen", "-activity-limit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %s:\n%s", want, msg)
		}
	}
}
