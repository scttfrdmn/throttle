package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"throttle/budget"
	"throttle/config"
)

// The config commands: check, show, and the init that writes a first file.
//
// check and show are read-only in the strong sense. check opens the ledger to compare what
// the file says against what is stored, and that comparison is a read: nothing in this file
// calls PutDefinition, UpdateDefinition, EnsurePeriod, Advance, Recover, or Reconcile.
//
// That is not squeamishness. A definition's identity covers its allocation and its period
// rule, so "make the ledger match the file" is an operation that can change what a live
// budget is allowed to spend on the strength of somebody having edited a YAML file. Whether
// throttle should offer that at all, and under what confirmation, is issue #21. Until it is
// answered, throttle reports the difference and names the command that would apply it.

func configCmd(args []string) error {
	if len(args) == 0 {
		configUsage()
		return errors.New("config: a subcommand is required")
	}
	switch args[0] {
	case "check":
		return configCheckCmd(args[1:])
	case "show":
		return configShowCmd(args[1:])
	case "help", "-h", "--help":
		configUsage()
		return nil
	default:
		configUsage()
		return fmt.Errorf("config: unknown subcommand %q", args[0])
	}
}

func configUsage() {
	fmt.Fprintln(os.Stderr, `usage: throttle config <check|show> [flags]

  check  validate the configuration and compare it to the stored budgets
  show   print the configuration in effect and where each value came from

Neither writes anything. "throttle init" creates a starter file.`)
}

// configCheckCmd validates configuration without mutating anything.
//
// Success is concise: a line saying what parsed and, if the ledger has anything to say, what
// differs. Failure lists every problem the loader could find in one pass, because a config
// file is hand-edited and reporting one typo at a time turns five mistakes into five
// edit-run cycles.
func configCheckCmd(args []string) error {
	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	common := addCommonFlags(fs)
	quiet := fs.Bool("q", false, "print nothing on success; exit status alone reports the result")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := common.load()
	if err != nil {
		// Already a full report of every problem found. Wrapping it in "config check
		// failed" would push the useful part down a line and add nothing.
		return err
	}

	out := os.Stdout
	if *quiet {
		out = nil
	}

	if cfg.Path == "" {
		// Not an error. throttle runs on defaults, and a first-time user who has not
		// written a file yet should be told what to do rather than shown a failure.
		printf(out, "no configuration file found; running on built-in defaults\n")
		printf(out, "  looked for %s\n", defaultConfigPath())
		printf(out, "  write one with \"throttle init\"\n")
		return nil
	}

	printf(out, "%s: ok\n", cfg.Path)
	if n := len(cfg.Budgets); n > 0 {
		printf(out, "  %d budget(s): %s\n", n, strings.Join(budgetIDs(cfg.Budgets), ", "))
	}

	// The comparison against the ledger. Skipped when there is no ledger yet, because a
	// config file checked before anything has been stored is the normal first-run case and
	// "no such database" is not a configuration problem.
	if _, err := os.Stat(cfg.Ledger); errors.Is(err, os.ErrNotExist) {
		printf(out, "  no ledger at %s yet, so nothing to compare against\n", cfg.Ledger)
		return nil
	}

	ctx := context.Background()
	_, store, err := openLedger(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	stored, err := store.Definitions(ctx)
	if err != nil {
		return err
	}

	return reportDrift(out, config.Compare(cfg.Budgets, stored))
}

// reportDrift prints how the file and the ledger differ, and returns an error only for a
// difference that no command will resolve on its own.
func reportDrift(out *os.File, drifts []config.Drift) error {
	var (
		unchanged int
		newOnes   []config.Drift
		changed   []config.Drift
		renamed   []config.Drift
		unmanaged []config.Drift
	)
	for _, d := range drifts {
		switch d.Kind {
		case config.DriftUnchanged:
			unchanged++
		case config.DriftNew:
			newOnes = append(newOnes, d)
		case config.DriftChanged:
			changed = append(changed, d)
		case config.DriftRenamed:
			renamed = append(renamed, d)
		case config.DriftUnmanaged:
			unmanaged = append(unmanaged, d)
		}
	}

	if unchanged > 0 {
		printf(out, "  %d budget(s) already stored and identical\n", unchanged)
	}

	for _, d := range newOnes {
		printf(out, "  %s: not stored yet; \"throttle define\" creates it\n", d.BudgetID)
	}
	for _, d := range renamed {
		// Named but not treated as an error, because it is not a semantic difference and
		// because the current behavior is itself under question (issue #22): a definition's
		// fingerprint excludes Name, so re-storing this changes nothing at all.
		printf(out, "  %s: display name differs (file %q, stored %q); a name-only change is "+
			"not currently applied\n", d.BudgetID, d.Config.Name, d.Stored.Name)
	}
	for _, d := range unmanaged {
		// Reported, never removed. A budget absent from a file is far more often an
		// incomplete file than an instruction to discard accounting history.
		printf(out, "  %s: stored but not in this file; left alone\n", d.BudgetID)
	}

	if len(changed) == 0 {
		return nil
	}

	// A real disagreement about money. Written to stderr and returned as an error, because a
	// CI job checking its configuration should fail here: the file describes a budget the
	// ledger is not governing, and nothing will quietly reconcile the two.
	var b strings.Builder
	fmt.Fprintf(&b, "%d budget(s) differ from the stored definition:\n", len(changed))
	for _, d := range changed {
		fmt.Fprintf(&b, "  %s: %s\n", d.BudgetID, strings.Join(d.Fields, ", "))
		for _, line := range describeChange(d) {
			fmt.Fprintf(&b, "      %s\n", line)
		}
	}
	b.WriteString("\nA stored definition governs money that has already been spent against it, " +
		"so throttle\nwill not rewrite one because a file changed. Reconcile them deliberately: " +
		"edit the file\nto match, or change the budget with an explicit update.")
	return errors.New(b.String())
}

// describeChange renders the specific values that differ, because "allocation differs" does
// not tell the reader which of the two numbers is the one they meant.
func describeChange(d config.Drift) []string {
	var lines []string
	for _, f := range d.Fields {
		switch f {
		case "allocation":
			lines = append(lines, fmt.Sprintf("allocation: file %s, stored %s",
				d.Config.Allocation.CentsString(), d.Stored.Allocation.CentsString()))
		case "parent":
			lines = append(lines, fmt.Sprintf("parent: file %q, stored %q",
				d.Config.ParentID, d.Stored.ParentID))
		case "borrow":
			lines = append(lines, fmt.Sprintf("borrow: file %s, stored %s",
				d.Config.Borrow, d.Stored.Borrow))
		case "recurrence":
			lines = append(lines, fmt.Sprintf("period: file %s, stored %s",
				recurrenceText(d.Config), recurrenceText(d.Stored)))
		case "anchor":
			lines = append(lines, fmt.Sprintf("starts: file %s, stored %s",
				d.Config.AnchorAt.Format(time.RFC3339), d.Stored.AnchorAt.Format(time.RFC3339)))
		case "end":
			lines = append(lines, fmt.Sprintf("ends: file %s, stored %s",
				endText(d.Config), endText(d.Stored)))
		case "timezone":
			lines = append(lines, fmt.Sprintf("timezone: file %s, stored %s",
				tzText(d.Config), tzText(d.Stored)))
		case "rollover":
			lines = append(lines, fmt.Sprintf("rollover: file %s, stored %s",
				describeRollover(d.Config.Rollover), describeRollover(d.Stored.Rollover)))
		}
	}
	return lines
}

func recurrenceText(def budget.Definition) string {
	if def.Recurrence == budget.RecurDuration {
		return "every " + def.Every.String()
	}
	return string(def.Recurrence)
}

func endText(def budget.Definition) string {
	if def.EndAt.IsZero() {
		return "open-ended"
	}
	return def.EndAt.Format(time.RFC3339)
}

func tzText(def budget.Definition) string {
	if def.Location == nil {
		return "UTC"
	}
	return def.Location.String()
}

// configShowCmd prints the effective configuration.
func configShowCmd(args []string) error {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	common := addCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.load()
	if err != nil {
		return err
	}
	return cfg.Show(os.Stdout)
}

// initCmd writes a starter configuration file.
//
// Noninteractive by design. A prompt would make the command unusable in a Dockerfile or a
// CI step, and there is nothing here worth asking about that a flag cannot say. It creates
// no databases, no credentials, and no cloud resources: one text file, and a message naming
// what to edit.
func initCmd(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	var (
		path  = fs.String("config", "", "where to write the file; the default location is used if unset")
		force = fs.Bool("force", false, "replace an existing file")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	target := *path
	if target == "" {
		target = defaultConfigPath()
	}
	if target == "" {
		return errors.New("init: no home directory, so there is no default config location; " +
			"pass -config with a path")
	}

	now := time.Now().UTC()
	if err := config.Write(target, config.TemplateAnchor(now.Year(), int(now.Month())), *force); err != nil {
		return err
	}

	fmt.Printf("wrote %s\n\n", target)
	fmt.Println("next:")
	fmt.Println(`  1. edit the budget amount and period`)
	fmt.Println(`  2. throttle config check`)
	fmt.Println(`  3. throttle define -id default -budget '$100'   (store it in the ledger)`)
	fmt.Println(`  4. throttle serve                               (watch it on the dashboard)`)

	// No store is created here. "throttle define" creates the ledger when it first needs
	// one, which keeps init a command that writes one file and nothing else.
	return nil
}

// requireBudget resolves which budget a command should act on.
//
// The configured default is what makes "throttle status" work without arguments, which is
// the difference between a tool someone checks daily and one they check when they remember
// the flag. When there is no default either, the error says how to set one rather than
// restating that a flag is required.
func requireBudget(id string, cfg config.Config, cmd string) (string, error) {
	if id != "" {
		return id, nil
	}
	if cfg.DefaultBudget != "" {
		return cfg.DefaultBudget, nil
	}
	if cfg.Path == "" {
		return "", fmt.Errorf("%s: -id is required, or set defaults.budget in a configuration "+
			"file (\"throttle init\" writes one)", cmd)
	}
	return "", fmt.Errorf("%s: -id is required, or set defaults.budget in %s", cmd, cfg.Path)
}

// configSourceText names where configuration came from, for an error that suggests editing
// it. "your configuration file" is not an instruction anyone can follow.
func configSourceText(cfg config.Config) string {
	if cfg.Path == "" {
		return "a configuration file (\"throttle init\" writes one)"
	}
	return cfg.Path
}

// defaultConfigPath is the platform's default config file location, or "" if there is no
// home directory to put it in.
func defaultConfigPath() string {
	paths, err := config.DefaultPaths(config.OSEnv())
	if err != nil {
		return ""
	}
	return paths.Config
}

func budgetIDs(defs []budget.Definition) []string {
	ids := make([]string, 0, len(defs))
	for _, def := range defs {
		ids = append(ids, def.ID)
	}
	return ids
}

// printf writes to out unless out is nil, which is how -q suppresses success output without
// threading a boolean through every call.
func printf(out *os.File, format string, args ...any) {
	if out == nil {
		return
	}
	fmt.Fprintf(out, format, args...)
}
