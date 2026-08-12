package report

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"throttle/activity"
	"throttle/budget"
	"throttle/ledger"
	"throttle/money"
)

// Summary is the whole read model for one budget at one instant: its position, its
// place in the hierarchy, the health of its bookkeeping, and whether the two stores
// agree about it.
type Summary struct {
	// Position is the monetary and pacing position.
	Position Position

	// Chain is the budget and its ancestors, nearest first, each with its own
	// position. A leaf with headroom can still be governed by an ancestor without
	// any, so the chain is what explains a constraint.
	Chain []Position

	// Binding names the budget in the chain with the least spendable headroom right
	// now -- the one that would refuse the next request. It is the named budget's own
	// ID when nothing above it is tighter.
	Binding string

	// Health is the state of the bookkeeping behind the figures: unresolved
	// liabilities, unknown outcomes, expired holds. Zero when everything is settled.
	Health Health

	// Activity summarizes the requests in this period, from the activity store.
	// ActivityAvailable is false when no activity store is configured or the store
	// could not be read, which is why a caller must not read an empty Activity as
	// "no requests happened".
	Activity          activity.Summary
	ActivityAvailable bool

	// ActivityError explains an activity read failure in one sentence. The rest of
	// the summary is still valid when it is set: money comes from the ledger, so a
	// broken telemetry database degrades the dashboard rather than emptying it.
	ActivityError string

	// Disagreement reports the ledger and the activity store telling different
	// stories about settled spend. The ledger is authoritative; this exists so the
	// difference is visible instead of silently resolved.
	Disagreement Disagreement
}

// Health is the state of the bookkeeping behind a position.
//
// These counts are the reason a spend total may be a floor rather than a total, so
// they belong beside it rather than on a separate page.
type Health struct {
	// Unresolved is requests that ran and incurred cost throttle could not price.
	// Their reservations stay encumbered.
	Unresolved int

	// OutcomeUnknown is requests whose outcome was never determined -- the process
	// died mid-call, or a stream ended before the provider reported usage. The hold
	// is deliberately still standing.
	OutcomeUnknown int

	// AwaitingExternal is requests whose cost arrives out of band, later, and has
	// not arrived. A hosted-runtime invocation is in this state by design, not by
	// damage, so it is counted separately from the two above.
	AwaitingExternal int

	// Encumbered is the headroom the unresolved and unknown records still hold.
	Encumbered money.Money

	// ExpiredHolds and ExpiredAmount are leases that lapsed and have not been
	// recovered.
	ExpiredHolds  int
	ExpiredAmount money.Money

	// Repaired is records carrying at least one reconciliation entry. Not a problem:
	// evidence that a crash was cleaned up.
	Repaired int

	// UnpricedDimensions names every dimension that blocked a price, so an operator
	// knows what the pricing catalog is missing.
	UnpricedDimensions []string
}

// Clean reports whether there is nothing outstanding at all.
func (h Health) Clean() bool {
	return h.Unresolved == 0 && h.OutcomeUnknown == 0 &&
		h.AwaitingExternal == 0 && h.ExpiredHolds == 0
}

// Needs reports whether any of this needs an operator's attention. An awaiting
// record does not: it is waiting for the provider, not for a human.
func (h Health) Needs() bool {
	return h.Unresolved > 0 || h.OutcomeUnknown > 0 || h.ExpiredHolds > 0
}

// Disagreement is the difference between the ledger's settled spend and the spend
// the activity store believes it recorded.
//
// The two are written by separate transactions with a paid provider call between
// them, so they can legitimately differ for a while -- and a crash can leave them
// differing indefinitely until reconciliation runs. The dashboard must not assume
// they agree, and must not recompute money from activity to make a chart tidier.
type Disagreement struct {
	// LedgerSpent is the authoritative figure.
	LedgerSpent money.Money

	// ActivitySpent is the sum of what activity records claim, which for incomplete
	// records is a floor.
	ActivitySpent money.Money

	// ActivityComplete reports whether the activity figure is a total or a floor.
	ActivityComplete bool

	// Delta is LedgerSpent-ActivitySpent, signed.
	Delta money.Money
}

// Material reports whether the two stores differ by an amount worth surfacing.
//
// An incomplete activity figure is expected to be below the ledger and is not a
// disagreement -- that is what "floor" means. Only a difference the completeness
// flag does not already explain is material.
func (d Disagreement) Material() bool {
	if d.Delta == 0 {
		return false
	}
	if !d.ActivityComplete && d.Delta > 0 {
		return false
	}
	return true
}

// Summary answers "where does this budget stand, and can I trust the number?"
//
// The ledger read comes first and independently: an activity store that is missing,
// locked, or corrupt degrades the request history without touching the monetary
// figures, because those come from the ledger.
func (r *Reporter) Summary(ctx context.Context, budgetID string) (Summary, error) {
	at := r.clock()

	chain, err := r.led.Chain(ctx, budgetID)
	if err != nil {
		return Summary{}, err
	}
	if len(chain) == 0 {
		return Summary{}, fmt.Errorf("%w: %q", ledger.ErrBudgetNotFound, budgetID)
	}

	out := Summary{Chain: make([]Position, 0, len(chain))}
	for _, def := range chain {
		pos, err := r.positionOf(ctx, def, at)
		if err != nil {
			return Summary{}, err
		}
		out.Chain = append(out.Chain, pos)
	}
	out.Position = out.Chain[0]
	out.Binding = binding(out.Chain)

	// The named budget's own period scopes the activity read. A parent's period may
	// have different bounds, and mixing them would produce a request list that does
	// not correspond to any single envelope.
	book := r.Bookkeeping(ctx, budgetID, out.Position.Period.ID)
	out.Health, out.Activity = book.Health, book.Activity
	out.ActivityAvailable, out.ActivityError = book.Available, book.Error
	out.Health.ExpiredHolds = out.Position.ExpiredHolds
	out.Health.ExpiredAmount = out.Position.ExpiredAmount

	out.Disagreement = Disagreement{
		LedgerSpent:      out.Position.Spent,
		ActivitySpent:    out.Activity.Spend,
		ActivityComplete: out.Activity.Complete,
	}
	if v, ok := money.Sub(out.Position.Spent, out.Activity.Spend); ok {
		out.Disagreement.Delta = v
	}
	if !out.ActivityAvailable {
		// Without a readable activity store there is nothing to disagree with, and
		// reporting a delta equal to the whole ledger would be alarming nonsense.
		out.Disagreement = Disagreement{LedgerSpent: out.Position.Spent, ActivityComplete: true}
	}
	return out, nil
}

// positionOf reads one budget's current period and totals.
func (r *Reporter) positionOf(ctx context.Context, def budget.Definition, at time.Time) (Position, error) {
	p, err := r.led.Period(ctx, def.PeriodID(0))
	if err == nil && !containsInstant(p, at) {
		// The seq-0 shortcut only helps a single-period budget; fall through to the
		// scan for anything else.
		p, err = r.periodContaining(ctx, def, at)
	} else if err != nil {
		p, err = r.periodContaining(ctx, def, at)
	}
	if err != nil {
		return Position{}, err
	}
	tot, err := r.led.Totals(ctx, ledger.Scope{BudgetID: def.ID, PeriodID: p.ID}, at)
	if err != nil {
		return Position{}, fmt.Errorf("report: totals for %q: %w", def.ID, err)
	}
	return position(def, p, tot, at), nil
}

// periodContaining finds the materialized period holding at, without materializing
// one.
//
// A read model must not create rows. EnsurePeriod would materialize a period as a
// side effect of somebody opening a browser tab, which is a write triggered by a
// read -- and on a shared ledger, a write triggered by a read from a process that is
// not spending any money.
//
// When no period contains at -- a budget whose first period has not started, or one
// whose term has ended -- the nearest materialized period is returned so the
// dashboard can show the envelope and say where the clock is relative to it. Its
// pacing figures are clamped by budget.Envelope.Elapsed, which is what makes a
// future or expired period render honestly rather than as a divide-by-zero.
func (r *Reporter) periodContaining(ctx context.Context, def budget.Definition, at time.Time) (ledger.Period, error) {
	periods, err := r.led.Periods(ctx, def.ID)
	if err != nil {
		return ledger.Period{}, err
	}
	if len(periods) == 0 {
		return ledger.Period{}, fmt.Errorf("%w: %q has no materialized period", ledger.ErrNoSuchPeriodRow, def.ID)
	}
	for _, p := range periods {
		if containsInstant(p, at) {
			return p, nil
		}
	}
	// Nearest by distance from the instant: the last period if the clock is past
	// everything, the first if it is before everything.
	if at.Before(periods[0].Envelope.Start) {
		return periods[0], nil
	}
	return periods[len(periods)-1], nil
}

func containsInstant(p ledger.Period, at time.Time) bool {
	if p.ID == "" {
		return false
	}
	return !at.Before(p.Envelope.Start) && at.Before(p.Envelope.End)
}

// binding names the chain member with the least spendable headroom.
//
// Ties go to the nearest budget, which is why the scan starts at the leaf and uses a
// strict comparison: when a parent and child have identical headroom, the child is
// the one the caller named and the more useful thing to report.
func binding(chain []Position) string {
	if len(chain) == 0 {
		return ""
	}
	best := chain[0]
	for _, p := range chain[1:] {
		if p.SpendableNow < best.SpendableNow {
			best = p
		}
	}
	return best.BudgetID
}

// Bookkeeping is the state of the telemetry behind a set of figures, separated from
// the figures themselves.
//
// Available is the field that keeps a dashboard honest: an empty Activity with
// Available false means throttle is not recording request history, which is a
// different fact from "no requests happened" and must not be rendered as one.
type Bookkeeping struct {
	Health   Health
	Activity activity.Summary

	// Available reports whether the activity store could be read at all.
	Available bool

	// Error explains an unavailable store in one sentence, empty when there simply
	// is no store configured.
	Error string
}

// Bookkeeping counts the outstanding telemetry for a budget in one period.
//
// It never returns an error: every failure mode here is a degradation of request
// history, not of money, and a caller that had to choose between an error page and a
// misleading empty table would be choosing between two wrong answers.
func (r *Reporter) Bookkeeping(ctx context.Context, budgetID, periodID string) Bookkeeping {
	h, sum, ok, msg := r.health(ctx, budgetID, periodID)
	return Bookkeeping{Health: h, Activity: sum, Available: ok, Error: msg}
}

// health counts the outstanding bookkeeping behind a position.
//
// Counts come from activity because that is where a request's completeness lives;
// the encumbered amount is cross-checked against the ledger's own Reserved figure by
// the caller, not silently substituted for it.
func (r *Reporter) health(ctx context.Context, budgetID, periodID string) (Health, activity.Summary, bool, string) {
	var h Health
	if r.acts == nil {
		return h, activity.Summary{Complete: true}, false, ""
	}

	records, err := r.acts.List(ctx, activity.Filter{
		BudgetID: budgetID,
		PeriodID: periodID,
	})
	if err != nil {
		// A telemetry read failure must not empty a dashboard whose money came from
		// the ledger. Report it and carry on.
		return h, activity.Summary{Complete: true}, false, err.Error()
	}

	sum := activity.Summarize(records)
	for _, rec := range records {
		switch {
		case rec.Status == activity.StatusUnresolved && awaitingExternal(rec):
			h.AwaitingExternal++
		case rec.Status == activity.StatusUnresolved:
			h.Unresolved++
		case rec.Status == activity.StatusOutstanding:
			h.OutcomeUnknown++
		}
		if len(rec.Repairs) > 0 {
			h.Repaired++
		}
	}
	h.Encumbered = sum.Encumbered
	for _, d := range sum.UnpricedDimensions {
		h.UnpricedDimensions = append(h.UnpricedDimensions, string(d))
	}
	sort.Strings(h.UnpricedDimensions)
	return h, sum, true, ""
}

// awaitingExternal reports whether an unresolved record is waiting for an
// out-of-band observation rather than for a price.
//
// The test is structural, on normalized fields: a session identity, no reconciled
// figure yet, and no known cost. There is no provider or operation conditional here,
// for the same reason the reconciler has none -- a provider name in this predicate is
// where provider knowledge would start accumulating in a provider-neutral package.
// The reconciler classifies the same state the same way; both read the record rather
// than each other.
func awaitingExternal(rec activity.Record) bool {
	return rec.Runtime.SessionID != "" && !rec.Runtime.Reconciled && !rec.ActualCost.Known()
}

// Tree is the persisted budget hierarchy, each node with its current position.
type Tree struct {
	// Roots are the budgets with no parent, in ID order.
	Roots []*Node

	// At is the instant every position was computed for.
	At time.Time

	// Errors names budgets whose position could not be read, with the reason. A
	// single unreadable budget must not blank the whole tree.
	Errors map[string]string
}

// Node is one budget in the tree.
type Node struct {
	Position Position
	Children []*Node
}

// Empty reports whether no budgets are defined at all -- the first-run state, which
// is a legitimate thing for a dashboard to show rather than an error.
func (t Tree) Empty() bool { return len(t.Roots) == 0 }

// Flatten returns every node depth-first, parents before children, with its depth.
// It is what a table of the hierarchy iterates.
func (t Tree) Flatten() []FlatNode {
	var out []FlatNode
	var walk func(n *Node, depth int)
	walk = func(n *Node, depth int) {
		out = append(out, FlatNode{Node: n, Depth: depth})
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	for _, root := range t.Roots {
		walk(root, 0)
	}
	return out
}

// FlatNode is a tree node with its indentation depth.
type FlatNode struct {
	*Node
	Depth int
}

// Tree returns every defined budget with its current position.
//
// One Totals read per budget, and one Periods read per budget: no per-child query
// inside a per-parent loop, so a hierarchy costs a number of queries proportional to
// its size rather than to its size squared.
func (r *Reporter) Tree(ctx context.Context) (Tree, error) {
	at := r.clock()
	defs, err := r.led.Definitions(ctx)
	if err != nil {
		return Tree{}, err
	}

	t := Tree{At: at, Errors: map[string]string{}}
	nodes := make(map[string]*Node, len(defs))
	order := make([]string, 0, len(defs))

	for _, def := range defs {
		pos, err := r.positionOf(ctx, def, at)
		if err != nil {
			// A budget with no materialized period yet is normal on a fresh ledger.
			// Record why and show the rest.
			t.Errors[def.ID] = err.Error()
			pos = Position{BudgetID: def.ID, Name: def.Name, ParentID: def.ParentID, At: at}
		}
		nodes[def.ID] = &Node{Position: pos}
		order = append(order, def.ID)
	}
	sort.Strings(order)

	for _, id := range order {
		n := nodes[id]
		parent, ok := nodes[n.Position.ParentID]
		if n.Position.ParentID == "" || !ok {
			// A child whose parent is missing from the definition set is attached at the
			// root rather than dropped: losing a budget from a spend display is worse
			// than showing it in the wrong place.
			t.Roots = append(t.Roots, n)
			continue
		}
		parent.Children = append(parent.Children, n)
	}
	return t, nil
}

// Periods lists a budget's materialized periods, most recent first, for the period
// selector.
//
// Monthly is not special here: these are whatever envelopes the definition actually
// generated, so a one-week demo budget, an academic grant, and a monthly recurrence
// all list the same way.
func (r *Reporter) Periods(ctx context.Context, budgetID string) ([]PeriodOption, error) {
	periods, err := r.led.Periods(ctx, budgetID)
	if err != nil {
		return nil, err
	}
	at := r.clock()
	out := make([]PeriodOption, 0, len(periods))
	for i := len(periods) - 1; i >= 0; i-- {
		p := periods[i]
		out = append(out, PeriodOption{
			PeriodID: p.ID,
			Seq:      p.Seq,
			Start:    p.Envelope.Start,
			End:      p.Envelope.End,
			State:    p.State,
			Current:  containsInstant(p, at),
		})
	}
	return out, nil
}

// PeriodOption is one selectable period.
type PeriodOption struct {
	PeriodID string
	Seq      int
	Start    time.Time
	End      time.Time
	State    ledger.PeriodState
	Current  bool
}

// PositionIn reports a budget's position in a specific materialized period, for
// looking at a prior period rather than the current one.
//
// The instant used is the reporter's clock clamped into the period, so a past period
// reads as fully elapsed and a future one as not started -- rather than reporting a
// closed period's pacing as though it were still running.
func (r *Reporter) PositionIn(ctx context.Context, budgetID, periodID string) (Position, error) {
	p, err := r.led.Period(ctx, periodID)
	if err != nil {
		return Position{}, err
	}
	if p.BudgetID != budgetID {
		return Position{}, fmt.Errorf("%w: period %q belongs to budget %q, not %q",
			ledger.ErrInvalidArgument, periodID, p.BudgetID, budgetID)
	}
	def, _, err := r.led.Definition(ctx, budgetID)
	if err != nil {
		return Position{}, err
	}

	at := r.clock()
	if !at.Before(p.Envelope.End) {
		at = p.Envelope.End
	} else if at.Before(p.Envelope.Start) {
		at = p.Envelope.Start
	}

	tot, err := r.led.Totals(ctx, ledger.Scope{BudgetID: budgetID, PeriodID: periodID}, at)
	if err != nil {
		return Position{}, err
	}
	return position(def, p, tot, at), nil
}

// errNotConfigured wraps ErrNoActivity with the query that needed it.
func errNotConfigured(what string) error {
	return fmt.Errorf("%w: %s is unavailable", ErrNoActivity, what)
}

// NotFound reports whether an error means a record or row is simply absent, which a
// dashboard renders as a 404 rather than a 500.
//
// It lives here rather than in the HTTP layer because which errors mean "absent" is a
// fact about the stores, not about the transport. A handler that maintained its own list
// would drift from this one and start reporting a mistyped budget id as a server fault.
func NotFound(err error) bool {
	return errors.Is(err, activity.ErrNotFound) ||
		errors.Is(err, ledger.ErrBudgetNotFound) ||
		errors.Is(err, ledger.ErrNoSuchPeriodRow) ||
		errors.Is(err, ledger.ErrReservationNotFound)
}
