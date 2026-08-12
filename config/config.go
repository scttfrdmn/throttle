// Package config is throttle's one configuration model.
//
// There is exactly one schema. Flags override it, three environment variables supply
// operational paths, and the precedence is fixed:
//
//	built-in defaults  <  config file  <  environment  <  CLI flag
//
// Later wins, and nothing is merged: a value comes from one place and Config records which,
// so "why is it using that ledger?" is answerable with "throttle config show" rather than
// by reading source.
//
// # Parsing has no side effects
//
// Loading configuration opens no database, materializes no period, advances nothing, and
// makes no provider call. It reads a file and returns a value. Commands invoke behavior
// explicitly, which is what keeps "throttle config check" honestly read-only.
//
// # What is not here
//
// Credentials. AWS region and credentials resolve through the AWS SDK's own
// environment/profile/role mechanisms, and throttle neither reads, stores, nor prints
// them. There are no provider API key fields, because the adapters that would use them do
// not exist and inventing their schema now would be guessing.
package config

import (
	"fmt"
	"time"

	"throttle/budget"
	"throttle/engine"
)

// SchemaVersion is the configuration schema this build understands.
//
// A file may omit it, which means 1: requiring a version line in a hand-written first
// config is friction with nothing behind it while v0.1 is unshipped. Any other value is an
// error naming what is understood, so a future format fails clearly instead of being
// half-read by an old binary. Deliberately an integer -- there is no semantic-version
// comparison to get wrong.
const SchemaVersion = 1

// Source is where a resolved value came from.
type Source string

const (
	FromDefault Source = "default"
	FromFile    Source = "config file"
	FromEnv     Source = "environment"
	FromFlag    Source = "flag"
)

// Config is the effective configuration.
type Config struct {
	// Path is the config file that was loaded, empty when running on defaults.
	Path string

	// Ledger and Activity are the two stores.
	Ledger   string
	Activity string

	// DefaultBudget is what commands use when no budget is named. Empty means a
	// command that needs one must be told.
	DefaultBudget string

	// Enforcement is the posture a spending process starts from. It is not persisted
	// with a definition -- posture is a property of the process doing the spending, not
	// an accounting fact about the budget -- so it is configuration in the literal
	// sense.
	Enforcement engine.Mode

	// Lease is how long a reservation blocks headroom before recovery can reclaim it.
	Lease time.Duration

	// Listen is the dashboard's bind address, and ActivityLimit its table size.
	Listen        string
	ActivityLimit int

	// Budgets are the definitions the file describes, in declaration order with
	// parents before children. They are candidates for the ledger, not the ledger's
	// contents: nothing here is persisted until a command says so.
	Budgets []budget.Definition

	// Origin records where each value came from, keyed by the dotted field path.
	Origin map[string]Source
}

// Defaults is the configuration with no file, no environment, and no flags.
func Defaults(env Env) (Config, error) {
	paths, err := DefaultPaths(env)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Ledger:        paths.Ledger,
		Activity:      paths.Activity,
		Enforcement:   engine.ModeEnforce,
		Lease:         engine.DefaultLease,
		Listen:        DefaultListen,
		ActivityLimit: DefaultActivityLimit,
		Origin: map[string]Source{
			"store.ledger":             FromDefault,
			"store.activity":           FromDefault,
			"defaults.enforcement":     FromDefault,
			"defaults.lease":           FromDefault,
			"dashboard.listen":         FromDefault,
			"dashboard.activity_limit": FromDefault,
		},
	}, nil
}

// DefaultListen and DefaultActivityLimit mirror the dashboard package's own defaults.
//
// Declared here rather than imported so that config does not depend on dashboard: the
// dependency would run the wrong way, since serve reads config and not the reverse. A test
// asserts the two agree, which is what keeps a copied constant from drifting.
const (
	DefaultListen        = "127.0.0.1:7654"
	DefaultActivityLimit = 100
)

// Definition finds a budget by id.
func (c Config) Definition(id string) (budget.Definition, bool) {
	for _, def := range c.Budgets {
		if def.ID == id {
			return def, true
		}
	}
	return budget.Definition{}, false
}

// source reports where a field's value came from.
func (c Config) source(path string) Source {
	if s, ok := c.Origin[path]; ok {
		return s
	}
	return FromDefault
}

// ListenFromFile reports whether the listen address came from the config file.
//
// Used by serve to say where a surprising bind address was set. A warning that says only
// "this is not loopback" leaves the reader grepping for the setting.
func (c Config) ListenFromFile() bool {
	return c.source("dashboard.listen") == FromFile
}

// note records a value's origin.
func (c *Config) note(path string, s Source) {
	if c.Origin == nil {
		c.Origin = map[string]Source{}
	}
	c.Origin[path] = s
}

// modes are the enforcement postures a config file may name.
var modes = map[string]engine.Mode{
	string(engine.ModeMonitor): engine.ModeMonitor,
	string(engine.ModeEnforce): engine.ModeEnforce,
	string(engine.ModeWait):    engine.ModeWait,
}

func parseMode(path, raw string) (engine.Mode, error) {
	if raw == "" {
		return engine.ModeEnforce, nil
	}
	m, ok := modes[raw]
	if !ok {
		return "", fieldErr(path, fmt.Sprintf(
			"unknown enforcement mode %q: use monitor, wait, or enforce", raw))
	}
	return m, nil
}

// rolloverModes are the carry policies a config file may name.
var rolloverModes = map[string]budget.RolloverMode{
	string(budget.RolloverNone):    budget.RolloverNone,
	string(budget.RolloverCredit):  budget.RolloverCredit,
	string(budget.RolloverBalance): budget.RolloverBalance,
}

func parseRolloverMode(path, raw string) (budget.RolloverMode, error) {
	if raw == "" {
		return budget.RolloverNone, nil
	}
	m, ok := rolloverModes[raw]
	if !ok {
		return "", fieldErr(path, fmt.Sprintf(
			"unknown rollover mode %q: use none, credit, or balance", raw))
	}
	return m, nil
}
