// Command throttle is the local CLI for defining and inspecting budgets.
//
// Budget definitions live in the ledger, so a definition declared once is used by
// every process that shares the database. That is why "define" is its own command
// rather than a set of flags repeated on every invocation: repeating the rules on
// each call is exactly how two processes end up governing the same money by
// different numbers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"throttle/budget"
	"throttle/engine"
	"throttle/ledger"
	"throttle/ledger/sqlite"
	"throttle/money"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version":
		fmt.Println(version)
		return
	case "define":
		err = defineCmd(os.Args[2:])
	case "budgets":
		err = budgetsCmd(os.Args[2:])
	case "status":
		err = statusCmd(os.Args[2:])
	case "periods":
		err = periodsCmd(os.Args[2:])
	case "advance":
		err = advanceCmd(os.Args[2:])
	case "recover":
		err = recoverCmd(os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "throttle:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: throttle <command> [flags]

commands:
  define    create or verify a persistent budget definition
  budgets   list stored budget definitions
  status    show the current budget position
  periods   list a budget's materialized periods
  advance   perform due period transitions
  recover   reclaim expired reservations left by crashed processes
  version   print the version

run "throttle <command> -h" for command flags`)
}

func defaultDBPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "throttle.db"
	}
	return filepath.Join(dir, "throttle", "ledger.db")
}

// dbFlag is on every command, because every command needs the ledger.
func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", defaultDBPath(), "path to the ledger database")
}

// open opens the ledger and an engine over it. The caller closes the store.
func open(ctx context.Context, path string) (*engine.Engine, *sqlite.Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("create ledger directory: %w", err)
		}
	}
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	eng, err := engine.New(engine.Config{Ledger: store})
	if err != nil {
		store.Close()
		return nil, nil, err
	}
	return eng, store, nil
}

func defineCmd(args []string) error {
	fs := flag.NewFlagSet("define", flag.ContinueOnError)
	var (
		id       = fs.String("id", "", "budget id (required)")
		parent   = fs.String("parent", "", "parent budget id; empty means a root budget")
		name     = fs.String("name", "", "display name")
		alloc    = fs.String("budget", "", "allocation per period in dollars (required)")
		borrow   = fs.Duration("borrow", 0, "borrow window, e.g. 72h")
		recur    = fs.String("recur", "monthly", "recurrence: monthly, weekly, daily, duration, or none")
		every    = fs.Duration("every", 0, "period length when -recur=duration")
		anchor   = fs.String("anchor", "", "RFC3339 start of the first period; defaults to the start of the current month")
		endAt    = fs.String("end", "", "RFC3339 end of the whole budget; required when -recur=none")
		tz       = fs.String("tz", "UTC", "timezone in which calendar period boundaries fall")
		rollover = fs.String("rollover", "none", "rollover mode: none, credit, or balance")
		capAmt   = fs.String("rollover-cap", "", "cap positive carry at this many dollars")
		capPct   = fs.Float64("rollover-cap-pct", 0, "cap positive carry at this percentage of the allocation")
		dbPath   = dbFlag(fs)
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("define: -id is required")
	}
	if *alloc == "" {
		return errors.New("define: -budget is required")
	}

	allocation, err := money.Parse(*alloc)
	if err != nil {
		return fmt.Errorf("parse -budget: %w", err)
	}
	loc, err := time.LoadLocation(*tz)
	if err != nil {
		return fmt.Errorf("parse -tz: %w", err)
	}

	// The two cap forms are mutually exclusive by design: a cap is either an
	// amount or a proportion, and accepting both would leave the resolution order
	// as an implicit product rule.
	if *capAmt != "" && *capPct != 0 {
		return errors.New("define: -rollover-cap and -rollover-cap-pct are mutually exclusive")
	}
	policy := budget.RolloverPolicy{Mode: budget.RolloverMode(*rollover)}
	if *capAmt != "" {
		if policy.Cap, err = money.Parse(*capAmt); err != nil {
			return fmt.Errorf("parse -rollover-cap: %w", err)
		}
	}
	if *capPct != 0 {
		bp, err := basisPoints(*capPct)
		if err != nil {
			return err
		}
		policy.CapBasisPoints = bp
	}

	anchorAt := monthStart(time.Now().UTC(), loc)
	if *anchor != "" {
		if anchorAt, err = time.Parse(time.RFC3339, *anchor); err != nil {
			return fmt.Errorf("parse -anchor: %w", err)
		}
	}
	var end time.Time
	if *endAt != "" {
		if end, err = time.Parse(time.RFC3339, *endAt); err != nil {
			return fmt.Errorf("parse -end: %w", err)
		}
	}

	def := budget.Definition{
		ID:         *id,
		ParentID:   *parent,
		Name:       *name,
		Allocation: allocation,
		Borrow:     *borrow,
		Rollover:   policy,
		Recurrence: budget.Recurrence(*recur),
		Every:      *every,
		Location:   loc,
		AnchorAt:   anchorAt,
		EndAt:      end,
	}
	if err := def.Validate(); err != nil {
		return err
	}

	ctx := context.Background()
	eng, store, err := open(ctx, *dbPath)
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

	// Status materializes the current period, so the definition is immediately
	// usable and its bounds are visible.
	st, err := eng.Status(ctx, def.ID)
	if err != nil {
		return err
	}
	fmt.Printf("budget %q defined: %s per %s, period %s → %s\n",
		def.ID, allocation.CentsString(), describeRecurrence(def),
		st.Period.Envelope.Start.Format(time.RFC3339), st.Period.Envelope.End.Format(time.RFC3339))
	return nil
}

// basisPoints converts a user-facing percentage to the integer basis points the
// definition stores, refusing anything that is not exactly representable rather
// than silently rounding a cap the user typed.
func basisPoints(pct float64) (int64, error) {
	if pct < 0 {
		return 0, errors.New("define: -rollover-cap-pct cannot be negative")
	}
	scaled := pct * 100
	if math.Abs(scaled-math.Round(scaled)) > 1e-9 {
		return 0, fmt.Errorf("define: -rollover-cap-pct %g is finer than one basis point", pct)
	}
	return int64(math.Round(scaled)), nil
}

func monthStart(now time.Time, loc *time.Location) time.Time {
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
}

func describeRecurrence(def budget.Definition) string {
	switch def.Recurrence {
	case budget.RecurDuration:
		return def.Every.String()
	case budget.RecurNone:
		return "fixed term"
	default:
		return strings.TrimSuffix(string(def.Recurrence), "ly")
	}
}

func budgetsCmd(args []string) error {
	fs := flag.NewFlagSet("budgets", flag.ContinueOnError)
	dbPath := dbFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	_, store, err := open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	defs, err := store.Definitions(ctx)
	if err != nil {
		return err
	}
	if len(defs) == 0 {
		fmt.Println(`no budgets defined (see "throttle define -h")`)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPARENT\tALLOCATION\tRECURRENCE\tROLLOVER\tBORROW")
	for _, def := range defs {
		parent := def.ParentID
		if parent == "" {
			parent = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			def.ID, parent, def.Allocation.CentsString(),
			describeRecurrence(def), describeRollover(def.Rollover), def.Borrow)
	}
	return w.Flush()
}

func describeRollover(p budget.RolloverPolicy) string {
	mode := string(p.Mode)
	if mode == "" {
		mode = string(budget.RolloverNone)
	}
	switch {
	case p.Cap > 0:
		return mode + " ≤" + p.Cap.CentsString()
	case p.CapBasisPoints > 0:
		return fmt.Sprintf("%s ≤%g%%", mode, float64(p.CapBasisPoints)/100)
	default:
		return mode
	}
}

func statusCmd(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var (
		id           = fs.String("id", "", "budget id (required)")
		estimateText = fs.String("estimate", "", "also report the admission decision for this estimated cost")
		chain        = fs.Bool("chain", false, "also report every ancestor budget")
		dbPath       = dbFlag(fs)
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("status: -id is required")
	}

	ctx := context.Background()
	eng, store, err := open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	statuses := []engine.Status{}
	if *chain {
		if statuses, err = eng.StatusChain(ctx, *id); err != nil {
			return err
		}
	} else {
		st, err := eng.Status(ctx, *id)
		if err != nil {
			return err
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
		dec, err := eng.Check(ctx, *id, est)
		if err != nil {
			w.Flush()
			return err
		}
		fmt.Fprintf(w, "\nestimate\t%s\n", est.CentsString())
		fmt.Fprintf(w, "decision\t%s\n", dec.Outcome)
		if dec.BindingBudgetID != "" && dec.BindingBudgetID != *id {
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
	id := fs.String("id", "", "budget id (required)")
	dbPath := dbFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("periods: -id is required")
	}

	ctx := context.Background()
	_, store, err := open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	periods, err := store.Periods(ctx, *id)
	if err != nil {
		return err
	}
	if len(periods) == 0 {
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
	id := fs.String("id", "", "budget id; empty advances every budget")
	dbPath := dbFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	eng, store, err := open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	var changed []ledger.Period
	if *id == "" {
		changed, err = eng.AdvanceAll(ctx)
	} else {
		changed, err = eng.Advance(ctx, *id)
	}
	if err != nil {
		return err
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
	id := fs.String("id", "", "budget id; empty recovers across every budget")
	dbPath := dbFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	eng, store, err := open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	var recovered []ledger.Reservation
	if *id == "" {
		recovered, err = eng.RecoverAll(ctx)
	} else {
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
