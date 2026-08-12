package config

import (
	"errors"
	"fmt"
	"os"
)

// Resolution: find the file, read it, let the environment and then flags override it.
//
// The whole precedence chain lives in Load, in order, once. Spreading it across the
// commands is how a project ends up with three configuration schemas and a flag that works
// on one subcommand and is ignored on another.

// Environment variables throttle reads.
//
// Three, all operational paths, all things a CI job or a container legitimately needs to
// redirect without writing a file. Deliberately not a mirror of the whole schema: an
// environment variable for every field is a second configuration format that nobody
// documents and that "config show" has to guess about.
//
// There is no THROTTLE_AWS_* anything. AWS configuration is the AWS SDK's business and it
// already has its own well-known variables.
const (
	// EnvConfig names the config file to load. A file named here and missing is an
	// error; the default location being empty is not.
	EnvConfig = "THROTTLE_CONFIG"

	// EnvLedger and EnvActivity redirect the two stores.
	EnvLedger   = "THROTTLE_LEDGER"
	EnvActivity = "THROTTLE_ACTIVITY"
)

// Overrides are values a command's flags supplied, or nil for the ones they did not.
//
// Pointers rather than a struct of strings with "" meaning absent, because -listen "" is
// worth an error and an omitted -listen is not. Passed by the command rather than read from
// a global flag set: commands take the configuration they need instead of sharing one
// mutable options object.
type Overrides struct {
	// Path names a config file explicitly. Missing is an error, as with EnvConfig: a
	// flag naming a file is a statement that the file matters.
	Path *string

	Ledger        *string
	Activity      *string
	Budget        *string
	Enforcement   *string
	Lease         *string
	Listen        *string
	ActivityLimit *int
}

// Load resolves the effective configuration.
//
//	built-in defaults  <  config file  <  environment  <  CLI flag
//
// Each stage overwrites whole values; nothing is merged. A ledger path comes from exactly
// one of the four, and Origin records which, so an unexpected value is explainable without
// reading source.
//
// No file anywhere is required. throttle with no configuration at all runs on defaults,
// which is what makes "go and try it" a two-command experience.
func Load(env Env, over Overrides) (Config, error) {
	path, explicit, err := findFile(env, over)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	switch {
	case path == "":
		if cfg, err = Defaults(env); err != nil {
			return Config{}, err
		}
	default:
		cfg, err = LoadFile(path, env)
		if errors.Is(err, ErrNotExist) && !explicit {
			// The default location is a suggestion, not a requirement.
			if cfg, err = Defaults(env); err != nil {
				return Config{}, err
			}
		} else if err != nil {
			return Config{}, err
		}
	}

	if err := applyEnv(&cfg, env); err != nil {
		return Config{}, err
	}
	if err := applyFlags(&cfg, env, over); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// findFile decides which config file to read, and whether its absence is an error.
func findFile(env Env, over Overrides) (path string, explicit bool, err error) {
	if over.Path != nil {
		if *over.Path == "" {
			return "", false, errors.New("-config: needs a path")
		}
		p, err := expandPath(*over.Path, env)
		if err != nil {
			return "", false, err
		}
		if _, err := os.Stat(p); err != nil {
			// Explicitly named and not there. Falling back to defaults would run the
			// wrong budget quietly, which is the failure this error exists to prevent.
			return "", false, fmt.Errorf("-config %s: %w", *over.Path, errNoFile(err))
		}
		return p, true, nil
	}

	if raw := env.get(EnvConfig); raw != "" {
		p, err := expandPath(raw, env)
		if err != nil {
			return "", false, err
		}
		if _, err := os.Stat(p); err != nil {
			return "", false, fmt.Errorf("%s=%s: %w", EnvConfig, raw, errNoFile(err))
		}
		return p, true, nil
	}

	paths, err := DefaultPaths(env)
	if err != nil {
		// No home directory is not fatal on its own: the caller may still name every
		// path by flag. It becomes fatal in Defaults, which needs somewhere to put a
		// ledger.
		return "", false, nil
	}
	return paths.Config, false, nil
}

func errNoFile(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no such file")
	}
	return err
}

// applyEnv lets the environment override the file.
func applyEnv(cfg *Config, env Env) error {
	problems := &Errors{Source: "environment"}

	for _, v := range []struct {
		key   string
		path  string
		field *string
	}{
		{EnvLedger, "store.ledger", &cfg.Ledger},
		{EnvActivity, "store.activity", &cfg.Activity},
	} {
		raw := env.get(v.key)
		if raw == "" {
			continue
		}
		p, err := expandPath(raw, env)
		if err != nil {
			problems.add(fieldErr(v.key, err.Error()))
			continue
		}
		*v.field = p
		cfg.note(v.path, FromEnv)
	}

	return problems.err()
}

// applyFlags lets a command's flags override everything.
func applyFlags(cfg *Config, env Env, over Overrides) error {
	problems := &Errors{Source: "flags"}

	if over.Ledger != nil {
		if p, err := expandPath(*over.Ledger, env); err != nil {
			problems.add(fieldErr("-db", err.Error()))
		} else {
			cfg.Ledger = p
			cfg.note("store.ledger", FromFlag)
		}
	}
	if over.Activity != nil {
		if p, err := expandPath(*over.Activity, env); err != nil {
			problems.add(fieldErr("-activity", err.Error()))
		} else {
			cfg.Activity = p
			cfg.note("store.activity", FromFlag)
		}
	}
	if over.Budget != nil && *over.Budget != "" {
		cfg.DefaultBudget = *over.Budget
		cfg.note("defaults.budget", FromFlag)
	}
	if over.Enforcement != nil {
		mode, err := parseMode("-mode", *over.Enforcement)
		if err != nil {
			problems.add(err)
		} else {
			cfg.Enforcement = mode
			cfg.note("defaults.enforcement", FromFlag)
		}
	}
	if over.Lease != nil {
		d, err := parseDuration("-lease", *over.Lease)
		if err != nil {
			problems.add(err)
		} else {
			cfg.Lease = d
			cfg.note("defaults.lease", FromFlag)
		}
	}
	if over.Listen != nil {
		if *over.Listen == "" {
			problems.add(fieldErr("-listen",
				"needs an address; an empty one binds every interface"))
		} else {
			cfg.Listen = *over.Listen
			cfg.note("dashboard.listen", FromFlag)
		}
	}
	if over.ActivityLimit != nil {
		if *over.ActivityLimit < 0 {
			problems.add(fieldErr("-activity-limit", "cannot be negative"))
		} else {
			cfg.ActivityLimit = *over.ActivityLimit
			cfg.note("dashboard.activity_limit", FromFlag)
		}
	}

	return problems.err()
}
