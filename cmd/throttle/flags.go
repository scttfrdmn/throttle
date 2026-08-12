package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	activitysqlite "github.com/scttfrdmn/throttle/activity/sqlite"
	"github.com/scttfrdmn/throttle/config"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/report"
)

// The shared configuration layer for every command.
//
// One flag set helper, one resolution call, one precedence order. Before this existed each
// command built its own default path and parsed its own overrides, which is how a project
// ends up with a -db flag that means one thing on "serve" and another on "reconcile".
//
// Deliberately not a global options object: commonFlags is a local value a command creates,
// fills from its own flag set, and turns into the configuration it needs. Commands share the
// resolution, not the state.

// setUsage gives a command's help the same shape as "throttle -h".
//
// The flag package's default heading is "Usage of status:", which names a flag set rather than
// anything a person can type, says nothing about the arguments a command takes, and does not
// say what the command is for. One helper, so that fifteen commands cannot drift into fifteen
// shapes: a synopsis in the form somebody would type it, a sentence or two of what it does and
// whether it writes, then the flags.
func setUsage(fs *flag.FlagSet, synopsis, blurb string) {
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: throttle %s\n", synopsis)
		if blurb != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n", blurb)
		}
		fmt.Fprintln(os.Stderr)
		fs.PrintDefaults()
	}
}

// commonFlags are the flags every command has, wired to a flag set.
//
// Every one of them is an override for something the config file can say. A flag that
// corresponds to nothing in the file would be a second configuration schema.
type commonFlags struct {
	fs *flag.FlagSet

	configPath *string
	ledger     *string
	activity   *string
}

// addCommonFlags registers the flags shared by every command.
func addCommonFlags(fs *flag.FlagSet) *commonFlags {
	return &commonFlags{
		fs:         fs,
		configPath: fs.String("config", "", "configuration file; the default location is used if unset"),
		ledger:     fs.String("db", "", "ledger database; overrides the configured path"),
		activity:   fs.String("activity", "", "activity database; overrides the configured path"),
	}
}

// load resolves the configuration, letting only the flags actually passed override the file.
//
// "Actually passed" is the whole point of Visit: a flag registered with the configured value
// as its default would override the file with the file's own value, which works by accident
// until the day the file and the default disagree.
func (c *commonFlags) load(extra ...func(*config.Overrides)) (config.Config, error) {
	over := config.Overrides{}
	c.fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "config":
			over.Path = c.configPath
		case "db":
			over.Ledger = c.ledger
		case "activity":
			over.Activity = c.activity
		}
	})
	for _, fn := range extra {
		fn(&over)
	}
	return config.Load(config.OSEnv(), over)
}

// setIfPassed records a flag as an override only if the command line set it.
//
// The counterpart to load's Visit for the flags a single command adds. Passing a pointer
// unconditionally would make every command's defaults silently outrank the config file.
func setIfPassed[T any](fs *flag.FlagSet, name string, value *T, assign func(*T)) {
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			assign(value)
		}
	})
}

// openLedger opens the ledger at the configured path and an engine over it.
//
// The caller closes the store. The directory is created because a ledger path from a config
// file names a place throttle is expected to keep a database, not a place that must already
// exist.
func openLedger(ctx context.Context, cfg config.Config) (*engine.Engine, *sqlite.Store, error) {
	if dir := filepath.Dir(cfg.Ledger); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("create ledger directory %s: %w", dir, err)
		}
	}
	store, err := sqlite.Open(ctx, cfg.Ledger)
	if err != nil {
		return nil, nil, err
	}
	eng, err := engine.New(engine.Config{Ledger: store, Lease: cfg.Lease})
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return eng, store, nil
}

// openActivityIfPresent opens the activity store, or reports that there is none.
//
// A missing activity database is not created. Creating one as a side effect of looking at a
// budget would turn "no request history exists" into "no requests were recorded", and the
// second reads as a measurement.
func openActivityIfPresent(ctx context.Context, path string) (report.Activity, func(), error) {
	switch _, err := os.Stat(path); {
	case err == nil:
		store, err := activitysqlite.Open(ctx, path)
		if err != nil {
			return nil, nil, fmt.Errorf("open activity database %s: %w", path, err)
		}
		return store, func() { store.Close() }, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, func() {}, nil
	default:
		return nil, nil, fmt.Errorf("stat activity database %s: %w", path, err)
	}
}
