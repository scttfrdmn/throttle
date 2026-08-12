package config

import (
	"path/filepath"
	"testing"

	"github.com/scttfrdmn/throttle/money"
)

// Every test in this package describes its own environment through config.Env. None reads
// the real HOME, XDG_CONFIG_HOME, or user config directory: a test that does behaves
// differently on the author's machine than in CI, and one that writes there is a test that
// damages the machine it runs on.

// dollars and cents build expected amounts, matching the helpers the other packages' tests
// use. Both are integer arithmetic: an expected value computed through a float would be a
// test that agrees with a bug.
func dollars(d int64) money.Money { return money.Money(d) * money.PerDollar }
func cents(c int64) money.Money   { return money.Money(c) * money.PerDollar / 100 }

// testEnv is a synthetic platform rooted at dir.
func testEnv(t *testing.T, goos, dir string) Env {
	t.Helper()
	return Env{
		Home: dir,
		GOOS: goos,
		// Empty rather than os.Getenv: an inherited THROTTLE_LEDGER or XDG_DATA_HOME from
		// the developer's shell would silently change what these tests assert.
		Getenv: func(string) string { return "" },
	}
}

func TestDefaultPathsPerPlatform(t *testing.T) {
	// macOS and Linux genuinely differ, and the Linux data directory is deliberately not
	// the config directory: XDG reserves that one for configuration, and a SQLite ledger
	// under ~/.config is a database in a directory people sync and check into dotfiles.
	tests := []struct {
		name     string
		env      Env
		config   string
		ledger   string
		activity string
	}{
		{
			name:     "darwin",
			env:      Env{Home: "/u/alice", GOOS: "darwin", Getenv: func(string) string { return "" }},
			config:   "/u/alice/Library/Application Support/throttle/throttle.yaml",
			ledger:   "/u/alice/Library/Application Support/throttle/ledger.db",
			activity: "/u/alice/Library/Application Support/throttle/activity.db",
		},
		{
			name:     "linux",
			env:      Env{Home: "/u/alice", GOOS: "linux", Getenv: func(string) string { return "" }},
			config:   "/u/alice/.config/throttle/throttle.yaml",
			ledger:   "/u/alice/.local/share/throttle/ledger.db",
			activity: "/u/alice/.local/share/throttle/activity.db",
		},
		{
			name: "linux with XDG set",
			env: Env{Home: "/u/alice", GOOS: "linux", Getenv: func(k string) string {
				switch k {
				case "XDG_CONFIG_HOME":
					return "/cfg"
				case "XDG_DATA_HOME":
					return "/data"
				}
				return ""
			}},
			config:   "/cfg/throttle/throttle.yaml",
			ledger:   "/data/throttle/ledger.db",
			activity: "/data/throttle/activity.db",
		},
		{
			name: "darwin ignores XDG",
			env: Env{Home: "/u/alice", GOOS: "darwin", Getenv: func(k string) string {
				if k == "XDG_CONFIG_HOME" {
					return "/cfg"
				}
				return ""
			}},
			config:   "/u/alice/Library/Application Support/throttle/throttle.yaml",
			ledger:   "/u/alice/Library/Application Support/throttle/ledger.db",
			activity: "/u/alice/Library/Application Support/throttle/activity.db",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths, err := DefaultPaths(tt.env)
			if err != nil {
				t.Fatalf("DefaultPaths: %v", err)
			}
			if paths.Config != tt.config {
				t.Errorf("config = %q, want %q", paths.Config, tt.config)
			}
			if paths.Ledger != tt.ledger {
				t.Errorf("ledger = %q, want %q", paths.Ledger, tt.ledger)
			}
			if paths.Activity != tt.activity {
				t.Errorf("activity = %q, want %q", paths.Activity, tt.activity)
			}
		})
	}
}

func TestDefaultPathsNoHome(t *testing.T) {
	// Deliberately an error rather than a fallback to the working directory. A ledger at
	// "./ledger.db" is a budget that exists only for whoever runs throttle from that one
	// directory, and the surprise arrives later as an empty budget list.
	if _, err := DefaultPaths(Env{GOOS: "linux", Getenv: func(string) string { return "" }}); err == nil {
		t.Fatal("want an error with no home directory")
	}
}

func TestExpandPath(t *testing.T) {
	env := Env{Home: "/u/alice", GOOS: "linux", Getenv: func(string) string { return "" }}

	got, err := expandPath("~/throttle/ledger.db", env)
	if err != nil {
		t.Fatalf("expandPath: %v", err)
	}
	if want := "/u/alice/throttle/ledger.db"; got != want {
		t.Errorf("expandPath = %q, want %q", got, want)
	}

	// A tilde left literal would create a directory actually named "~", which is the kind
	// of mistake discovered months later.
	if _, err := expandPath("~bob/ledger.db", env); err == nil {
		t.Error("want an error for another user's home")
	}

	rel, err := expandPath("ledger.db", env)
	if err != nil {
		t.Fatalf("expandPath relative: %v", err)
	}
	if !filepath.IsAbs(rel) {
		t.Errorf("expandPath(%q) = %q, want an absolute path", "ledger.db", rel)
	}
}
