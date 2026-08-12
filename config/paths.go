package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Where throttle keeps things, by platform convention rather than by whatever the
// working directory happens to be.
//
// macOS and Linux genuinely differ here, and one standard-library call cannot serve both.
// os.UserConfigDir returns ~/Library/Application Support on darwin, which is the right
// place for a database, and $XDG_CONFIG_HOME on Linux, which is not: XDG reserves that
// directory for configuration and puts state under $XDG_DATA_HOME. Using it for a ledger
// would put a SQLite file in a directory a user might reasonably sync, back up
// selectively, or check into a dotfiles repository.
//
// The standard library has UserConfigDir and UserCacheDir but no UserDataDir, so the data
// directory is worked out here.

// Env is the environment a Paths resolution reads.
//
// It is a parameter rather than a call to os.Getenv and os.UserHomeDir inside the resolver
// so that a test can describe a whole platform -- a macOS home, a Linux home with XDG set,
// a Linux home without it -- and never touch the real user's directories. A test that
// reads $HOME is a test that behaves differently on the author's machine than in CI.
type Env struct {
	// Home is the user's home directory.
	Home string

	// GOOS is the platform whose conventions apply. Empty means runtime.GOOS.
	GOOS string

	// Getenv reads an environment variable. Nil means os.Getenv.
	Getenv func(string) string
}

// OSEnv is the real environment.
func OSEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{Home: home, GOOS: runtime.GOOS, Getenv: os.Getenv}
}

func (e Env) goos() string {
	if e.GOOS == "" {
		return runtime.GOOS
	}
	return e.GOOS
}

func (e Env) get(key string) string {
	if e.Getenv == nil {
		return os.Getenv(key)
	}
	return e.Getenv(key)
}

// Paths are the default locations for throttle's files.
type Paths struct {
	// Config is where a configuration file is looked for when none is named.
	Config string

	// Ledger and Activity are the two SQLite stores.
	//
	// They default to the same directory because they describe the same requests:
	// separating them would mean "throttle reconcile" needed configuration before it
	// could do anything at all.
	Ledger   string
	Activity string
}

// ErrNoHome reports that no home directory could be determined.
//
// Worth its own error rather than a silent fallback to the working directory: a ledger
// written to "./ledger.db" is a budget that exists only for whoever runs throttle from
// that one directory, and the surprise arrives later, as an empty budget list.
var ErrNoHome = errors.New("config: no home directory could be determined")

const (
	// ConfigFileName is the configuration file's name in any directory.
	ConfigFileName = "throttle.yaml"

	ledgerFileName   = "ledger.db"
	activityFileName = "activity.db"

	dirName = "throttle"
)

// DefaultPaths resolves the platform's default locations.
func DefaultPaths(env Env) (Paths, error) {
	cfgDir, err := configDir(env)
	if err != nil {
		return Paths{}, err
	}
	dataDir, err := dataDir(env)
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		Config:   filepath.Join(cfgDir, ConfigFileName),
		Ledger:   filepath.Join(dataDir, ledgerFileName),
		Activity: filepath.Join(dataDir, activityFileName),
	}, nil
}

// configDir is the throttle directory under the platform's configuration root.
func configDir(env Env) (string, error) {
	if dir := env.get("XDG_CONFIG_HOME"); dir != "" && env.goos() != "darwin" {
		return filepath.Join(dir, dirName), nil
	}
	home := env.Home
	if home == "" {
		return "", ErrNoHome
	}
	if env.goos() == "darwin" {
		return filepath.Join(home, "Library", "Application Support", dirName), nil
	}
	return filepath.Join(home, ".config", dirName), nil
}

// dataDir is the throttle directory under the platform's state root.
//
// On darwin that is the same Application Support directory the config lives in, which is
// the convention there. On Linux it is deliberately not the config directory.
func dataDir(env Env) (string, error) {
	if env.goos() == "darwin" {
		return configDir(env)
	}
	if dir := env.get("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, dirName), nil
	}
	home := env.Home
	if home == "" {
		return "", ErrNoHome
	}
	return filepath.Join(home, ".local", "share", dirName), nil
}

// expandPath resolves a leading ~ and makes the path absolute.
//
// A tilde is what a person writes in a config file, and it is not a shell that reads that
// file, so nothing else would expand it. A path of "~/x" left literal would create a
// directory actually named "~" beside the config, which is the kind of mistake that is
// discovered months later.
func expandPath(p string, env Env) (string, error) {
	if p == "" {
		return "", nil
	}
	switch {
	case p == "~":
		if env.Home == "" {
			return "", ErrNoHome
		}
		return env.Home, nil
	case strings.HasPrefix(p, "~/"):
		if env.Home == "" {
			return "", ErrNoHome
		}
		p = filepath.Join(env.Home, p[2:])
	case strings.HasPrefix(p, "~"):
		// "~otheruser/..." needs a user database lookup to mean anything, and guessing
		// would produce a path that silently belongs to the wrong account.
		return "", fmt.Errorf("config: cannot expand %q: only ~/ for the current user is supported", p)
	}
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", fmt.Errorf("config: resolve %q: %w", p, err)
		}
		return abs, nil
	}
	return filepath.Clean(p), nil
}
