package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/config"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
)

// The config commands: check, show, diff, apply, and the init that writes a first file.
//
// check, show, and diff are read-only in the strong sense. They open the ledger to compare
// what the file says against what is stored, and that comparison is a read.
//
// apply is the one mutating command, and it is explicit. A definition's identity covers its
// allocation and its period rule, so "make the ledger match the file" can change what a live
// budget is allowed to spend on the strength of somebody having edited a YAML file. That
// belongs behind a command somebody typed, never behind loading configuration (issue #21).
//
// One planner, two commands: diff plans and renders, apply plans and executes. Sharing
// config.NewPlan is what makes diff a dry run whose output describes what apply will do,
// rather than a second implementation that agrees until it does not.

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
	case "diff":
		return configDiffCmd(args[1:])
	case "apply":
		return configApplyCmd(args[1:])
	case "help", "-h", "--help":
		configUsage()
		return nil
	default:
		configUsage()
		return fmt.Errorf("config: unknown subcommand %q", args[0])
	}
}

func configUsage() {
	fmt.Fprintln(os.Stderr, `usage: throttle config <check|show|diff|apply> [flags]

  check  validate the configuration and compare it to the stored budgets
  show   print the configuration in effect and where each value came from
  diff   show what "apply" would change
  apply  store the configured budgets in the ledger

Only "apply" writes. "throttle init" creates a starter file.
Run "throttle config <subcommand> -h" for a subcommand's flags.`)
}

// configDiffCmd reports what apply would do, and writes nothing.
//
// The natural dry run, which is why apply has no -dry-run of its own: one command that
// plans and renders, one that plans and executes, and no third code path claiming to
// predict the second.
func configDiffCmd(args []string) error {
	fs := flag.NewFlagSet("config diff", flag.ContinueOnError)
	common := addCommonFlags(fs)
	setUsage(fs, "config diff [flags]",
		"Shows what \"throttle config apply\" would change. Writes nothing: this is apply's dry run.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.load()
	if err != nil {
		return err
	}

	plan, closeStore, err := planFor(cfg)
	if err != nil {
		return err
	}
	defer closeStore()

	if err := plan.Render(os.Stdout); err != nil {
		return err
	}
	if plan.Mutates() {
		fmt.Println("\nnothing was written. \"throttle config apply\" makes these changes.")
	}
	// A refusal is reported as an error even here: diff is what a CI job runs, and a
	// configuration describing a change throttle will not make is a configuration problem
	// whether or not anybody has tried to apply it yet.
	if refusals := plan.Refusals(); len(refusals) > 0 {
		return refusalSummary(refusals)
	}
	return nil
}

// configApplyCmd stores the configured budgets.
func configApplyCmd(args []string) error {
	fs := flag.NewFlagSet("config apply", flag.ContinueOnError)
	common := addCommonFlags(fs)
	setUsage(fs, "config apply [flags]",
		"Stores the configured budgets in the ledger. WRITES, and it is the only config subcommand\n"+
			"that does. A budget stored but absent from the file is left alone, never removed.\n"+
			"Run \"throttle config diff\" first to see what would change.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.load()
	if err != nil {
		return err
	}
	if len(cfg.Budgets) == 0 {
		if cfg.Path == "" {
			return errors.New("config apply: no configuration file, so there are no budgets to " +
				"apply (\"throttle init\" writes one)")
		}
		return fmt.Errorf("config apply: %s defines no budgets", cfg.Path)
	}

	ctx := context.Background()
	eng, store, err := openLedger(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	stored, err := storedDefinitions(ctx, store)
	if err != nil {
		return err
	}

	plan := config.NewPlan(cfg.Budgets, stored, time.Now())
	if err := plan.Render(os.Stdout); err != nil {
		return err
	}

	res, err := config.Apply(ctx, store, plan)
	if err != nil {
		return err
	}

	fmt.Println()
	switch {
	case len(res.Created) == 0 && len(res.Updated) == 0:
		fmt.Println("nothing to apply")
	default:
		var parts []string
		if n := len(res.Created); n > 0 {
			parts = append(parts, fmt.Sprintf("%d created", n))
		}
		if n := len(res.Updated); n > 0 {
			parts = append(parts, fmt.Sprintf("%d updated", n))
		}
		if res.Unchanged > 0 {
			parts = append(parts, fmt.Sprintf("%d unchanged", res.Unchanged))
		}
		fmt.Println(strings.Join(parts, ", "))
	}

	// Apply itself materializes nothing: config.Writer has no period method, so the
	// mutating path cannot open an envelope even by mistake. What follows is this command
	// reporting where the budgets it just stored now stand, the way "define" does.
	//
	// That report is what brings a first period into being, and it is worth doing here
	// rather than leaving to whatever runs next: a user who applies a file and then starts
	// the dashboard should see their budgets, not a page saying nothing is materialized.
	// Only budgets this command created are touched -- an update deliberately leaves the
	// current period alone, and re-reading an unrelated budget's position is not this
	// command's business.
	for _, id := range res.Created {
		reportPosition(ctx, eng, id)
	}
	return nil
}

// reportPosition prints where one budget stands, and is the thing that materializes its
// first period.
//
// Nothing here is fatal. The definitions are stored -- that is what apply promised and it
// succeeded -- so a budget that cannot yet report a position, because its term begins next
// month, is a fact to state rather than a command to fail. Reporting an error after a
// successful write would tell the reader the apply did not happen.
func reportPosition(ctx context.Context, eng *engine.Engine, budgetID string) {
	st, err := eng.Status(ctx, budgetID)
	switch {
	case errors.Is(err, budget.ErrNoSuchPeriod):
		def, _, defErr := eng.Definition(ctx, budgetID)
		if defErr != nil {
			return
		}
		fmt.Printf("  %s: %s\n", budgetID, termText(def))
	case err != nil:
		// Any other read failure is the reporting step's problem, not the write's. Said
		// plainly and quietly: the budget is stored either way.
		fmt.Printf("  %s: stored; could not read its position (%v)\n", budgetID, err)
	default:
		fmt.Printf("  %s: current period %s → %s\n", budgetID,
			st.Period.Envelope.Start.Format("2006-01-02"),
			st.Period.Envelope.End.Format("2006-01-02"))
	}
}

// renameCmd changes a budget's display name.
//
// Explicit, and separate from apply, because throttle does not infer a rename from two
// definitions happening to match financially (issue #22). A name is metadata: this changes
// what a budget is called and not what it may spend, and nothing historical is rewritten
// because activity and reservations key on the durable id.
func renameCmd(args []string) error {
	fs := flag.NewFlagSet("rename", flag.ContinueOnError)
	common := addCommonFlags(fs)
	// Flags before the arguments, because that is what the flag package accepts: parsing stops
	// at the first non-flag word, so a -config after the new name would be read as a third
	// argument. Written the way it has to be typed.
	setUsage(fs, "rename [flags] <budget> <new name>",
		"Changes the display name only. WRITES, but only that: the allocation, period, history,\n"+
			"and children are untouched, and the id stays the same -- so nothing recorded against\n"+
			"this budget is rewritten or detached.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return errors.New("rename: needs a budget id and a new name")
	}
	budgetID, name := fs.Arg(0), fs.Arg(1)

	cfg, err := common.load()
	if err != nil {
		return err
	}

	ctx := context.Background()
	_, store, err := openLedger(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	before, _, err := store.Definition(ctx, budgetID)
	if err != nil {
		return missingBudget(cfg, budgetID, err)
	}
	if err := config.Rename(ctx, store, budgetID, name); err != nil {
		return err
	}

	switch {
	case before.Name == name:
		fmt.Printf("%s is already named %q\n", budgetID, name)
	case before.Name == "":
		fmt.Printf("%s is now named %q\n", budgetID, name)
	default:
		fmt.Printf("%s renamed from %q to %q\n", budgetID, before.Name, name)
	}
	return nil
}

// planFor loads the stored definitions and plans against them.
//
// Returns a closer rather than closing itself, because the caller may want the store open
// while rendering. A missing ledger is not an error: planning against nothing is how a
// first run reports that every budget is new.
func planFor(cfg config.Config) (config.Plan, func(), error) {
	noop := func() {}
	if _, err := os.Stat(cfg.Ledger); errors.Is(err, os.ErrNotExist) {
		return config.NewPlan(cfg.Budgets, nil, time.Now()), noop, nil
	}

	ctx := context.Background()
	_, store, err := openLedger(ctx, cfg)
	if err != nil {
		return config.Plan{}, noop, err
	}
	stored, err := storedDefinitions(ctx, store)
	if err != nil {
		store.Close()
		return config.Plan{}, noop, err
	}
	return config.NewPlan(cfg.Budgets, stored, time.Now()), func() { store.Close() }, nil
}

// storedDefinitions reads every stored definition with the revision that guards its update.
//
// Definitions() drops the revision, and an update without one is a last-write-wins update.
// For a value that governs money that is not a tradeoff worth making, so each definition is
// re-read for its revision.
func storedDefinitions(ctx context.Context, store *sqlite.Store) ([]config.Stored, error) {
	defs, err := store.Definitions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]config.Stored, 0, len(defs))
	for _, def := range defs {
		_, revision, err := store.Definition(ctx, def.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, config.Stored{Definition: def, Revision: revision})
	}
	return out, nil
}

func refusalSummary(refusals []config.Step) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%d budget(s) ask for a change throttle will not make:\n", len(refusals))
	for _, s := range refusals {
		fmt.Fprintf(&b, "  %s: %s\n", s.BudgetID, s.Reason)
	}
	b.WriteString("\nEdit the file to match what is stored, or make the change explicitly.")
	return errors.New(b.String())
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
	setUsage(fs, "config check [flags]",
		"Validates the configuration file and compares it to the stored budgets. Writes nothing.\n"+
			"Exits non-zero if the file will not parse, or if it describes a budget differently from\n"+
			"the way the ledger is governing it -- which makes it usable as a CI step.")
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
		printf(out, "  %s: not stored yet; \"throttle config apply\" creates it\n", d.BudgetID)
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

	// A real disagreement about money. Returned as an error, because a CI job checking its
	// configuration should fail here: the file describes a budget the ledger is not
	// governing, and nothing reconciles the two on its own.
	var b strings.Builder
	fmt.Fprintf(&b, "%d budget(s) differ from the stored definition:\n", len(changed))
	for _, d := range changed {
		fmt.Fprintf(&b, "  %s: %s\n", d.BudgetID, strings.Join(d.Fields, ", "))
		for _, line := range describeChange(d) {
			fmt.Fprintf(&b, "      %s\n", line)
		}
	}
	b.WriteString("\nA stored definition governs money that has already been spent against it, " +
		"so a file\nchanging does not rewrite one. Reconcile them deliberately: " +
		"\"throttle config diff\"\nshows what would change, and \"throttle config apply\" makes it so " +
		"for future periods.")
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
	setUsage(fs, "config show [flags]",
		"Prints the configuration in effect and where each value came from: a flag, the file, or a\n"+
			"built-in default. Writes nothing, and prints no credentials -- throttle stores none.")
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
	setUsage(fs, "init [flags]",
		"Writes a starter configuration file with one budget in it, then tells you what to edit.\n"+
			"Writes that one file and nothing else: no databases, no credentials, no cloud resources.")
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
	fmt.Println(`  2. throttle config check    (does it parse and make sense?)`)
	fmt.Println(`  3. throttle config diff     (what would be stored?)`)
	fmt.Println(`  4. throttle config apply    (store it in the ledger)`)
	fmt.Println(`  5. throttle serve           (watch it on the dashboard)`)

	// No store is created here. "config apply" creates the ledger when it first needs one,
	// which keeps init a command that writes one file and nothing else.
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

// missingBudget turns "that budget does not exist" into something the reader can act on.
//
// The engine and the ledger each have their own ErrBudgetNotFound, and each message names the
// layer that noticed: "engine: budget not found" from one command and "ledger: budget not
// found" from the next, for the same situation. Which package raised it is not the reader's
// problem, and the answer is usually one of two commands -- so it is said here, once.
//
// The configuration file is the interesting case. A budget named in the file but absent from
// the ledger is a first run, not a typo, and the difference between "you have not applied your
// config yet" and "no such budget" is the difference between a next step and a dead end.
//
// Any other error is returned untouched.
func missingBudget(cfg config.Config, budgetID string, err error) error {
	if !errors.Is(err, ledger.ErrBudgetNotFound) && !errors.Is(err, engine.ErrBudgetNotFound) {
		return err
	}
	if _, inFile := cfg.Definition(budgetID); inFile {
		return fmt.Errorf("budget %q is not stored yet, though %s defines it: "+
			"\"throttle config apply\" stores it", budgetID, cfg.Path)
	}
	if cfg.Path == "" {
		return fmt.Errorf("no budget %q is stored (\"throttle budgets\" lists the stored ones; "+
			"\"throttle init\" starts a configuration file)", budgetID)
	}
	return fmt.Errorf("no budget %q is stored, and %s does not define it "+
		"(\"throttle budgets\" lists the stored ones)", budgetID, cfg.Path)
}

// requireStoredBudget reports that a budget exists before a command acts as though it does.
//
// For the commands whose answer for an unknown budget would otherwise be indistinguishable
// from a true one: "no periods materialized yet" and "no expired reservations to recover" are
// both correct about a budget that does not exist, and both read as reassurance.
func requireStoredBudget(ctx context.Context, store *sqlite.Store, cfg config.Config, budgetID string) error {
	if _, _, err := store.Definition(ctx, budgetID); err != nil {
		return missingBudget(cfg, budgetID, err)
	}
	return nil
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
