// Command throttle is the local CLI for defining and inspecting budgets.
//
// Budget definitions live in the ledger, so a definition declared once is used by
// every process that shares the database. That is why "define" is its own command
// rather than a set of flags repeated on every invocation: repeating the rules on
// each call is exactly how two processes end up governing the same money by
// different numbers.
//
// Where things live, what the stores are called, and which budget is the default come from
// one configuration model shared by every command below. See package config: flags override
// the file, the file overrides the defaults, and nothing is merged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	activitysqlite "github.com/scttfrdmn/throttle/activity/sqlite"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/config"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/reconcile"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version":
		// No flags, so anything after it is a mistake worth naming rather than ignoring --
		// including -h, which somebody types expecting to be told what the command does.
		if len(os.Args) > 2 {
			fmt.Fprintln(os.Stderr, "usage: throttle version")
			fmt.Fprintln(os.Stderr, "\nPrints the version of this binary, and takes no flags. A build from a")
			fmt.Fprintln(os.Stderr, "checkout reports the version being worked towards and the commit it came from.")
			if os.Args[2] == "-h" || os.Args[2] == "--help" || os.Args[2] == "help" {
				return
			}
			os.Exit(2)
		}
		fmt.Println(buildVersion())
		return
	case "init":
		err = initCmd(os.Args[2:])
	case "config":
		err = configCmd(os.Args[2:])
	case "define":
		err = defineCmd(os.Args[2:])
	case "budgets":
		err = budgetsCmd(os.Args[2:])
	case "rename":
		err = renameCmd(os.Args[2:])
	case "status":
		err = statusCmd(os.Args[2:])
	case "periods":
		err = periodsCmd(os.Args[2:])
	case "advance":
		err = advanceCmd(os.Args[2:])
	case "recover":
		err = recoverCmd(os.Args[2:])
	case "reconcile":
		err = reconcileCmd(os.Args[2:])
	case "serve":
		err = serveCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "throttle: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	// Asking for help is not a failure. flag.Parse returns ErrHelp after it has already
	// printed the usage, so the only thing left to do is exit successfully -- rather than
	// print "throttle: flag: help requested" underneath the help somebody asked for and
	// exit 1, which is the difference between a usable command and one no script can call.
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "throttle:", err)
		os.Exit(1)
	}
}

// usage is grouped by what a person is trying to do, not alphabetically.
//
// Getting started first, because that is the order a new user meets them in; then the
// day-to-day reading commands; then the repair commands, which exist for when something has
// gone wrong and are not part of normal operation.
func usage() {
	fmt.Fprintln(os.Stderr, `usage: throttle <command> [flags]

getting started
  init       write a starter configuration file
  config     check, show, diff, or apply the configuration
  define     store one budget from flags, without a configuration file

watching the money
  status     where a budget stands: spent, reserved, banked or borrowed
  budgets    list the stored budgets
  periods    list a budget's periods
  serve      run the local read-only dashboard on 127.0.0.1

changing a budget
  rename     change a budget's display name; its money and history are untouched

keeping the books straight
  advance    close periods that have ended and open the ones that follow
  recover    reclaim headroom held by reservations from crashed processes
  reconcile  repair bookkeeping a crashed process left half-finished

  version    print the version

Shared flags: -config, -db, -activity. Anything a flag can say, the configuration file can
say too; the flag wins for one command. Run "throttle <command> -h" for the rest.`)
}

// defineCmd stores a budget definition.
//
// Two ways to reach it. With -id and no other budget flags, the definition comes from the
// configuration file, which is the path most people should use: the file is reviewable,
// diffable, and says the same thing every time it is read. The flags remain for a one-off
// budget and for scripting something the file does not describe.
//
// Both paths end at the same budget.Definition and the same money parser. A second way to
// express a budget would be a second place for the two to disagree.
func defineCmd(args []string) error {
	fs := flag.NewFlagSet("define", flag.ContinueOnError)
	var (
		id       = fs.String("id", "", "budget id (required)")
		parent   = fs.String("parent", "", "parent budget id; empty means a root budget")
		name     = fs.String("name", "", "display name")
		alloc    = fs.String("budget", "", "allocation per period, e.g. '$4,000'; taken from the config file if unset")
		borrow   = fs.String("borrow", "", "borrow window, e.g. 72h or 3d")
		recur    = fs.String("recur", "", "period: monthly, weekly, daily, duration, or none")
		every    = fs.String("every", "", "period length when -recur=duration, e.g. 6h")
		anchor   = fs.String("anchor", "", "first day the budget applies, e.g. 2026-09-01")
		endAt    = fs.String("end", "", "last day of the budget; required when -recur=none")
		tz       = fs.String("tz", "", "timezone in which calendar period boundaries fall")
		rollover = fs.String("rollover", "", "rollover mode: none, credit, or balance")
		capAmt   = fs.String("rollover-cap", "", "cap positive carry at this amount, e.g. '$1,000'")
		capPct   = fs.String("rollover-cap-pct", "", "cap positive carry at this percentage of the allocation")
	)
	common := addCommonFlags(fs)
	setUsage(fs, "define -id <budget> [flags]",
		"Stores one budget definition in the ledger, without a configuration file. WRITES.\n"+
			"Fields not given as flags come from the configuration file if it describes this id.\n"+
			"Most people should keep budgets in the file and use \"throttle config apply\".")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		fs.Usage()
		return errors.New("define: -id is required")
	}

	cfg, err := common.load()
	if err != nil {
		return err
	}

	// The config file supplies the definition unless a flag overrides a field. A -budget
	// with no file behind it is still the whole definition; a -budget alongside a file is an
	// override of one field of it.
	def, fromFile := cfg.Definition(*id)
	if fromFile {
		def.ID = *id
	} else {
		def = budget.Definition{ID: *id}
	}

	over := config.DefinitionOverrides{}
	for _, f := range []struct {
		name  string
		apply func()
	}{
		{"parent", func() { over.Parent = parent }},
		{"name", func() { over.Name = name }},
		{"budget", func() { over.Amount = alloc }},
		{"borrow", func() { over.Borrow = borrow }},
		{"recur", func() { over.Recur = recur }},
		{"every", func() { over.Every = every }},
		{"anchor", func() { over.Anchor = anchor }},
		{"end", func() { over.End = endAt }},
		{"tz", func() { over.Timezone = tz }},
		{"rollover", func() { over.Rollover = rollover }},
		{"rollover-cap", func() { over.CapAmount = capAmt }},
		{"rollover-cap-pct", func() { over.CapPercent = capPct }},
	} {
		setIfPassedName(fs, f.name, f.apply)
	}

	def, err = config.ApplyDefinitionOverrides(def, over)
	if err != nil {
		return err
	}
	if !fromFile && def.Allocation == 0 && over.Amount == nil {
		return fmt.Errorf("define: %q is not in the configuration file, so -budget is required "+
			"(an amount per period, e.g. -budget '$4,000')", *id)
	}

	ctx := context.Background()
	eng, store, err := openLedger(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	// Only the definition is persisted. Enforcement posture is how a given process
	// chooses to react to a budget, not an accounting fact about it, so there is no
	// posture flag here: the process doing the spending decides.
	err = eng.Register(ctx, def, engine.ModeEnforce)
	switch {
	case errors.Is(err, ledger.ErrDefinitionConflict):
		return fmt.Errorf("%w\n\nthe stored definition differs from the one given. "+
			"Inspect it with \"throttle budgets\"; changing a budget's rules is a deliberate "+
			"update, not a side effect of running a command", err)
	case err != nil:
		return err
	}

	source := "from flags"
	if fromFile {
		source = "from " + cfg.Path
	}
	summary := fmt.Sprintf("budget %q defined (%s): %s %s",
		def.ID, source, def.Allocation.CentsString(), config.DescribePeriod(def))

	// Status materializes the current period, so the definition is immediately usable and its
	// bounds are visible.
	//
	// A budget whose term has not started yet -- a grant beginning next month, which is a
	// perfectly ordinary thing to define in advance -- has no current period, and that is not
	// a failure. The definition is stored either way, so reporting an error here would say the
	// command failed about a command that succeeded.
	st, err := eng.Status(ctx, def.ID)
	switch {
	case errors.Is(err, budget.ErrNoSuchPeriod):
		fmt.Println(summary + ", " + termText(def))
		return nil
	case err != nil:
		return err
	}
	fmt.Printf("%s, current period %s → %s\n", summary,
		st.Period.Envelope.Start.Format(time.RFC3339),
		st.Period.Envelope.End.Format(time.RFC3339))
	return nil
}

// termText says why a budget has no current period, in dates rather than in the engine's
// vocabulary: "no such period" is true and tells the reader nothing they can act on.
func termText(def budget.Definition) string {
	switch {
	case !def.AnchorAt.IsZero() && time.Now().Before(def.AnchorAt):
		return "not started yet: it begins " + def.AnchorAt.In(def.Location).Format("2006-01-02 15:04 MST")
	case !def.EndAt.IsZero() && !time.Now().Before(def.EndAt):
		return "its term ended " + def.EndAt.In(def.Location).Format("2006-01-02 15:04 MST")
	default:
		return "it has no period covering now"
	}
}

// setIfPassedName runs apply only if the command line actually set the named flag.
//
// The distinction matters because these flags override a config file: a flag whose default
// were treated as a value would silently outrank the file on every invocation.
func setIfPassedName(fs *flag.FlagSet, name string, apply func()) {
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			apply()
		}
	})
}

func budgetsCmd(args []string) error {
	fs := flag.NewFlagSet("budgets", flag.ContinueOnError)
	common := addCommonFlags(fs)
	setUsage(fs, "budgets [flags]",
		"Lists the stored budget definitions: allocation, period rule, rollover, borrow window.\n"+
			"Reads only.")
	if err := fs.Parse(args); err != nil {
		return err
	}
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

	defs, err := store.Definitions(ctx)
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		// The next step depends on whether there is a file to store from, so the message
		// names the one that applies rather than both.
		if len(cfg.Budgets) > 0 {
			fmt.Printf("no budgets stored yet; %s defines %d (store them with \"throttle config apply\")\n",
				cfg.Path, len(cfg.Budgets))
			return nil
		}
		fmt.Println(`no budgets stored yet (see "throttle init" or "throttle define -h")`)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPARENT\tALLOCATION\tPERIOD\tROLLOVER\tBORROW")
	for _, def := range defs {
		parent := def.ParentID
		if parent == "" {
			parent = "-"
		}
		borrow := "-"
		if def.Borrow > 0 {
			borrow = def.Borrow.String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			def.ID, parent, def.Allocation.CentsString(),
			config.DescribePeriod(def), describeRollover(def.Rollover), borrow)
	}
	return w.Flush()
}

// describeRollover renders a carry policy. One implementation, in config, so that a cap
// printed by "budgets" and the same cap printed by "config show" cannot disagree -- and so
// the percentage goes through the integer formatter rather than a float.
func describeRollover(p budget.RolloverPolicy) string {
	return config.DescribeRollover(p)
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var (
		id           = fs.String("id", "", "budget id; the configured default budget if unset")
		estimateText = fs.String("estimate", "", "also report the admission decision for this estimated cost")
		chain        = fs.Bool("chain", false, "also report every ancestor budget")
	)
	common := addCommonFlags(fs)
	setUsage(fs, "status [flags]",
		"Where a budget stands now: allocation, spent, reserved, banked or borrowed, and the\n"+
			"rate that finishes the period on pace. Reads only, but opens the current period if\n"+
			"the budget's term has begun and nothing has yet.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.load()
	if err != nil {
		return err
	}
	budgetID, err := requireBudget(*id, cfg, "status")
	if err != nil {
		return err
	}

	ctx := context.Background()
	eng, store, err := openLedger(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	statuses := []engine.Status{}
	if *chain {
		if statuses, err = eng.StatusChain(ctx, budgetID); err != nil {
			return outsideTerm(ctx, store, cfg, budgetID, err)
		}
	} else {
		st, err := eng.Status(ctx, budgetID)
		if err != nil {
			return outsideTerm(ctx, store, cfg, budgetID, err)
		}
		statuses = append(statuses, st)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for i, st := range statuses {
		if i > 0 {
			fmt.Fprintln(w, "\t")
		}
		printStatus(w, st)
	}

	if *estimateText != "" {
		est, err := money.Parse(*estimateText)
		if err != nil {
			w.Flush()
			return err
		}
		dec, err := eng.Check(ctx, budgetID, est)
		if err != nil {
			w.Flush()
			return err
		}
		fmt.Fprintf(w, "\nestimate\t%s\n", est.CentsString())
		fmt.Fprintf(w, "decision\t%s\n", dec.Outcome)
		if dec.BindingBudgetID != "" && dec.BindingBudgetID != budgetID {
			fmt.Fprintf(w, "limited by\t%s\n", dec.BindingBudgetID)
		}
		if dec.Reason != "" {
			fmt.Fprintf(w, "reason\t%s\n", dec.Reason)
		}
		if dec.Outcome == budget.OutcomeWait {
			fmt.Fprintf(w, "affordable at\t%s (in %s)\n",
				dec.RetryAt.Format(time.RFC3339), dec.Wait(eng.Now()).Round(time.Second))
			fmt.Fprintf(w, "shortfall\t%s\n", dec.Shortfall.CentsString())
		}
	}
	return w.Flush()
}

// outsideTerm rewrites "no such period" into a sentence about the budget's dates.
//
// A budget defined in advance of its start date, or one whose grant has expired, has no
// current period. That is a fact about the calendar, not a fault, and the engine's own
// message -- "no such period: <timestamp> precedes the anchor <timestamp>" -- is accurate
// and useless to somebody trying to work out what to do.
//
// An unknown budget goes through missingBudget for the same reason. Any other error is passed
// through untouched.
func outsideTerm(ctx context.Context, store *sqlite.Store, cfg config.Config, budgetID string, err error) error {
	if !errors.Is(err, budget.ErrNoSuchPeriod) {
		return missingBudget(cfg, budgetID, err)
	}
	def, _, defErr := store.Definition(ctx, budgetID)
	if defErr != nil {
		return err
	}
	return fmt.Errorf("budget %q is %s, so it has no current position to report", budgetID, termText(def))
}

func printStatus(w *tabwriter.Writer, st engine.Status) {
	env := st.Period.Envelope
	s := st.Snapshot

	// No enforcement posture is printed: this process is not the one spending, so
	// its own default posture would say nothing true about how the budget is
	// governed at the call site.
	fmt.Fprintf(w, "budget\t%s\n", st.BudgetID)
	fmt.Fprintf(w, "period\t%s → %s [%s]\n",
		env.Start.Format(time.RFC3339), env.End.Format(time.RFC3339), st.Period.State)
	fmt.Fprintf(w, "allocation\t%s\n", env.Allocation.CentsString())
	if env.Carry != 0 || st.Period.Provisional() {
		note := ""
		if st.Period.Provisional() {
			// A provisional carry can still be revised upward, so a user comparing
			// two runs should know why the number moved.
			note = " (provisional: the previous period is still draining)"
		}
		fmt.Fprintf(w, "carry\t%s%s\n", env.Carry.CentsString(), note)
	}
	fmt.Fprintf(w, "spent\t%s\n", s.Spent.CentsString())
	fmt.Fprintf(w, "reserved\t%s (%d live)\n", s.Reserved.CentsString(), st.PendingCount)
	fmt.Fprintf(w, "target by now\t%s\n", s.Target.CentsString())
	fmt.Fprintf(w, "allowed by now\t%s\n", s.Allowed.CentsString())

	// The sign of the bank is the headline: ahead of pace or behind it.
	label := "banked"
	if s.Bank < 0 {
		label = "borrowed"
	}
	fmt.Fprintf(w, "%s\t%s\n", label, s.Bank.CentsString())
	fmt.Fprintf(w, "available now\t%s\n", s.AvailableNow.CentsString())
	fmt.Fprintf(w, "period remaining\t%s\n", s.PeriodRemaining.CentsString())
	if s.Overspent() {
		fmt.Fprintf(w, "status\tOVERSPENT by %s\n", (-s.PeriodRemaining).CentsString())
	}
	fmt.Fprintf(w, "sustainable rate\t%s/hour\n", s.SustainableRate.CentsString())
	fmt.Fprintf(w, "projected spend\t%s\n", st.ProjectedSpend.CentsString())
	fmt.Fprintf(w, "time remaining\t%s\n", s.TimeRemaining.Round(time.Minute))
	if st.ExpiredCount > 0 {
		fmt.Fprintf(w, "expired holds\t%d holding %s (run \"throttle recover\")\n",
			st.ExpiredCount, st.ReservedExpired.CentsString())
	}
}

func periodsCmd(args []string) error {
	fs := flag.NewFlagSet("periods", flag.ContinueOnError)
	id := fs.String("id", "", "budget id; the configured default budget if unset")
	common := addCommonFlags(fs)
	setUsage(fs, "periods [flags]",
		"Lists the periods recorded for a budget, with their carry and closing balance. A period\n"+
			"exists from the first time something asks where the budget stands, so a budget whose\n"+
			"term has not begun has none. Reads only.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.load()
	if err != nil {
		return err
	}
	budgetID, err := requireBudget(*id, cfg, "periods")
	if err != nil {
		return err
	}

	ctx := context.Background()
	_, store, err := openLedger(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	// Checked before listing, because "no periods materialized yet" is true of a budget that
	// does not exist and reads as reassurance about one that does.
	if err := requireStoredBudget(ctx, store, cfg, budgetID); err != nil {
		return err
	}

	periods, err := store.Periods(ctx, budgetID)
	if err != nil {
		return err
	}
	if len(periods) == 0 {
		// A period is materialized when something first asks where a budget stands, so an
		// empty listing is normal. It is worth saying why when the reason is the calendar
		// rather than mere inactivity.
		if def, _, err := store.Definition(ctx, budgetID); err == nil && !def.AnchorAt.IsZero() &&
			time.Now().Before(def.AnchorAt) {
			fmt.Printf("no periods yet: %q is %s\n", budgetID, termText(def))
			return nil
		}
		fmt.Println("no periods materialized yet")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SEQ\tSTART\tEND\tSTATE\tCARRY\tALLOCATION\tCLOSING")
	for _, p := range periods {
		carry := p.Envelope.Carry.CentsString()
		if p.Provisional() {
			carry += "*"
		}
		closing := "-"
		if p.State == ledger.StateClosed {
			closing = p.ClosingBalance.CentsString()
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.Seq, p.Envelope.Start.Format(time.RFC3339), p.Envelope.End.Format(time.RFC3339),
			p.State, carry, p.Envelope.Allocation.CentsString(), closing)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\n* provisional carry: the preceding period is still draining and may release money")
	return nil
}

func advanceCmd(args []string) error {
	fs := flag.NewFlagSet("advance", flag.ContinueOnError)
	// Deliberately not defaulted to the configured budget: empty already means every
	// budget, which is the safe reading for a command that closes periods. Narrowing it
	// to one budget because a file named a default would silently leave the others
	// unadvanced.
	id := fs.String("id", "", "budget id; empty advances every budget")
	common := addCommonFlags(fs)
	setUsage(fs, "advance [flags]",
		"Closes periods whose end has passed and opens the ones that follow, carrying any rollover.\n"+
			"WRITES. Nothing is spent or refunded: this is the calendar catching up.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.load()
	if err != nil {
		return err
	}

	ctx := context.Background()
	eng, store, err := openLedger(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	var changed []ledger.Period
	if *id == "" {
		changed, err = eng.AdvanceAll(ctx)
		if err != nil {
			return err
		}
	} else {
		changed, err = eng.Advance(ctx, *id)
		if err != nil {
			return missingBudget(cfg, *id, err)
		}
	}
	if len(changed) == 0 {
		fmt.Println("no period transitions due")
		return nil
	}
	for _, p := range changed {
		switch p.State {
		case ledger.StateClosed:
			fmt.Printf("%s closed with a balance of %s\n", p.ID, p.ClosingBalance.CentsString())
		default:
			fmt.Printf("%s is now %s\n", p.ID, p.State)
		}
	}
	return nil
}

func recoverCmd(args []string) error {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	// As with advance: empty means every budget, and that stays the default.
	id := fs.String("id", "", "budget id; empty recovers across every budget")
	common := addCommonFlags(fs)
	setUsage(fs, "recover [flags]",
		"Releases reservations whose lease has expired, returning the headroom they hold. WRITES.\n"+
			"For money held by a process that crashed between reserving and settling. A request still\n"+
			"running keeps its lease alive, so this does not cut one off.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.load()
	if err != nil {
		return err
	}

	ctx := context.Background()
	eng, store, err := openLedger(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	var recovered []ledger.Reservation
	if *id == "" {
		recovered, err = eng.RecoverAll(ctx)
	} else {
		// Same reasoning as periods: recovery across an unknown budget finds nothing, and
		// "no expired reservations to recover" is a reassuring way to report a typo.
		if err := requireStoredBudget(ctx, store, cfg, *id); err != nil {
			return err
		}
		recovered, err = eng.Recover(ctx, *id)
	}
	if err != nil {
		return err
	}
	if len(recovered) == 0 {
		fmt.Println("no expired reservations to recover")
		return nil
	}
	var total money.Money
	for _, r := range recovered {
		var ok bool
		if total, ok = money.Add(total, r.Amount); !ok {
			return errors.New("recovered amounts overflow")
		}
		fmt.Printf("recovered %s on %s holding %s (created %s)\n",
			r.ID, r.BudgetID, r.Amount.CentsString(), r.CreatedAt.Format(time.RFC3339))
	}
	fmt.Printf("\nreclaimed %d reservation(s) holding %s\n", len(recovered), total.CentsString())
	return nil
}

// reconcileCmd repairs bookkeeping a crashed process left half-finished.
//
// It is explicit rather than scheduled, and knowing where the stores are does not change
// that. Configuration tells reconcile which databases to open; it does not tell it to run.
// A daemon would have to decide on its own when to touch money, and an operator running a
// repair pass at start-up or after an incident is both sufficient and easier to reason
// about; -dry-run exists so that decision can be made after seeing what would change.
func reconcileCmd(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	var (
		requestID = fs.String("request", "", "reconcile a single request id; empty sweeps for stranded bookkeeping")
		dryRun    = fs.Bool("dry-run", false, "classify and report without writing anything")
		limit     = fs.Int("limit", reconcile.DefaultLimit, "maximum records to examine per store")
		verbose   = fs.Bool("v", false, "explain every record examined, not only the ones that changed")
	)
	common := addCommonFlags(fs)
	setUsage(fs, "reconcile [flags]",
		"Repairs bookkeeping a crashed process left half-finished, by comparing recorded requests\n"+
			"against the reservations they hold. WRITES unless -dry-run. A request whose cost is not\n"+
			"known yet is left encumbered rather than given an invented one.")
	if err := fs.Parse(args); err != nil {
		return err
	}
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

	// Reconciliation compares the two stores, so unlike the dashboard it cannot proceed
	// without the activity side: with nothing to compare against, every reservation would
	// look stranded. Named plainly rather than reported as a missing file, because the
	// likeliest cause is a configured path pointing somewhere nothing has written yet.
	if _, err := os.Stat(cfg.Activity); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no activity database at %s.\n"+
			"Reconciliation compares the ledger against recorded requests, so it needs both.\n"+
			"Check store.activity in %s, or pass -activity",
			cfg.Activity, configSourceText(cfg))
	}

	acts, err := activitysqlite.Open(ctx, cfg.Activity)
	if err != nil {
		return fmt.Errorf("open activity database %s: %w", cfg.Activity, err)
	}
	defer acts.Close()

	rec, err := reconcile.New(reconcile.Config{
		Ledger:   store,
		Activity: acts,
		DryRun:   *dryRun,
		Limit:    *limit,
	})
	if err != nil {
		return err
	}

	if *dryRun {
		fmt.Println("dry run: nothing will be written")
	}

	if *requestID != "" {
		res, err := rec.Reconcile(ctx, *requestID)
		if err != nil {
			return err
		}
		printReconciled(res)
		if res.Class == reconcile.ClassFailed {
			return res.Err
		}
		return nil
	}

	sum, err := rec.ReconcilePending(ctx)
	if err != nil {
		return err
	}
	for _, res := range sum.Results {
		// Consistent records are the boring majority, and printing them by default
		// would bury the three lines an operator is looking for.
		if res.Class == reconcile.ClassConsistent && !*verbose {
			continue
		}
		printReconciled(res)
	}
	if len(sum.Results) > 0 {
		fmt.Println()
	}

	// Every class is named. Folding unresolved records into a failure count would
	// make a healthy system look broken, and folding them into the repaired count
	// would claim money had been accounted for when it has not.
	fmt.Printf("scanned %d / repaired %d / consistent %d / unresolved %d / awaiting data %d / orphaned %d / failed %d\n",
		sum.Scanned, sum.Repaired, sum.Consistent, sum.Unresolved, sum.Awaiting, sum.Orphaned, sum.Failed)
	if sum.Settled != 0 || sum.Released != 0 {
		fmt.Printf("settled %s / released %s\n", sum.Settled.CentsString(), sum.Released.CentsString())
	}
	if sum.Unresolved > 0 || sum.Awaiting > 0 {
		fmt.Println("\nunresolved and awaiting records are not errors: their cost is genuinely not known yet,\n" +
			"and reconciliation will not invent one. They stay encumbered and remain settleable.")
	}
	if sum.Truncated {
		fmt.Printf("\nthe scan stopped at its limit of %d; run again to continue\n", *limit)
	}
	if sum.Failed > 0 {
		return fmt.Errorf("%d record(s) could not be reconciled", sum.Failed)
	}
	return nil
}

func printReconciled(res reconcile.Result) {
	id := res.RequestID
	if id == "" {
		id = res.ReservationID
	}
	line := fmt.Sprintf("%-22s %-22s %s", id, res.Class, res.Detail)
	if res.Money != reconcile.MoneyNone {
		line = fmt.Sprintf("%-22s %-22s %s %s; %s", id, res.Class, res.Money, res.Amount.CentsString(), res.Detail)
	}
	fmt.Println(line)
}
