package report

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"throttle/activity"
	"throttle/budget"
	"throttle/ledger"
	"throttle/money"
	"throttle/usage"
)

// --- the empty dashboard ---------------------------------------------------

// No budgets at all is the first-run state. It is a legitimate thing to render, not
// an error and not a page of zeros presented as facts.
func TestEmptyDashboardHasNoBudgetsAndNoPanic(t *testing.T) {
	w := newWorld(t)

	tree, err := w.rep.Tree(w.ctx)
	if err != nil {
		t.Fatalf("Tree on an empty ledger: %v", err)
	}
	if !tree.Empty() {
		t.Errorf("Empty() = false with no budgets defined: %+v", tree.Roots)
	}
	if len(tree.Flatten()) != 0 {
		t.Error("Flatten() returned nodes for an empty tree")
	}
	if tree.At.IsZero() {
		t.Error("At is zero; even an empty tree was computed at an instant")
	}

	// Asking about a budget that does not exist is a not-found, which a handler turns
	// into a 404 rather than a 500.
	_, err = w.rep.Summary(w.ctx, "nope")
	if err == nil {
		t.Fatal("Summary of an unknown budget returned no error")
	}
	if !errors.Is(err, ledger.ErrBudgetNotFound) {
		t.Errorf("err = %v, want ledger.ErrBudgetNotFound", err)
	}
	if !NotFound(err) {
		t.Error("NotFound() = false for an unknown budget")
	}
}

// A budget that exists with no activity and no spend reads as zero spend against a
// real envelope -- not as an error, and not as a missing page.
func TestBudgetWithNoActivityRendersZeroSpendHonestly(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Position.Spent != 0 || sum.Position.Reserved != 0 {
		t.Errorf("Spent/Reserved = %s/%s, want zero", sum.Position.Spent, sum.Position.Reserved)
	}
	if sum.Position.Total != dollars(1000) {
		t.Errorf("Total = %s, want %s", sum.Position.Total, dollars(1000))
	}
	if !sum.ActivityAvailable {
		t.Error("ActivityAvailable = false with a working, empty activity store")
	}
	if sum.Activity.Requests != 0 {
		t.Errorf("Requests = %d, want 0", sum.Activity.Requests)
	}
	if !sum.Health.Clean() {
		t.Error("Clean() = false with nothing recorded")
	}
	if sum.Disagreement.Material() {
		t.Errorf("Material() = true on an untouched budget: %+v", sum.Disagreement)
	}
	// Zero spend is a measurement, so the average burn is a real zero rate rather than
	// unknown -- time has elapsed and nothing was spent in it.
	if !sum.Position.AverageBurn.Known || sum.Position.AverageBurn.PerHour != 0 {
		t.Errorf("AverageBurn = %+v, want a known zero", sum.Position.AverageBurn)
	}
	// And the whole allocation is still spendable.
	if sum.Position.SpendableNow <= 0 {
		t.Errorf("SpendableNow = %s on an untouched budget", sum.Position.SpendableNow)
	}
}

// A budget defined but with no materialized period is normal on a fresh ledger. The
// tree shows the budget and records why its position is missing.
func TestTreeShowsABudgetWithNoMaterializedPeriod(t *testing.T) {
	w := newWorld(t)
	if err := w.led.PutDefinition(w.ctx, monthly("research", "", dollars(1000))); err != nil {
		t.Fatalf("PutDefinition: %v", err)
	}

	tree, err := w.rep.Tree(w.ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree.Roots) != 1 {
		t.Fatalf("got %d roots, want the budget shown despite having no period", len(tree.Roots))
	}
	if tree.Roots[0].Position.BudgetID != "research" {
		t.Errorf("BudgetID = %q", tree.Roots[0].Position.BudgetID)
	}
	if tree.Errors["research"] == "" {
		t.Error("Errors has no entry explaining the missing period")
	}
}

// A read must never materialize a period. Somebody opening a browser tab must not
// write to a ledger it is not spending from.
func TestReadingADashboardDoesNotMaterializePeriods(t *testing.T) {
	w := newWorld(t)
	if err := w.led.PutDefinition(w.ctx, monthly("research", "", dollars(1000))); err != nil {
		t.Fatalf("PutDefinition: %v", err)
	}

	before, err := w.led.Periods(w.ctx, "research")
	if err != nil {
		t.Fatalf("Periods: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("the fixture materialized %d periods already", len(before))
	}

	// Every read a dashboard page performs.
	_, _ = w.rep.Tree(w.ctx)
	_, _ = w.rep.Summary(w.ctx, "research")
	_, _ = w.rep.Periods(w.ctx, "research")
	_, _ = w.rep.Timeline(w.ctx, "research", "")
	_, _ = w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	_, _ = w.rep.Reservations(w.ctx, "research", 0)

	after, err := w.led.Periods(w.ctx, "research")
	if err != nil {
		t.Fatalf("Periods: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("a read materialized %d period rows", len(after))
	}
}

// --- degraded activity store ----------------------------------------------

// Money comes from the ledger, so a broken telemetry database degrades the request
// history without emptying the monetary figures.
func TestABrokenActivityStoreDegradesRatherThanEmpties(t *testing.T) {
	dir := t.TempDir()
	w := newWorldIn(t, dir, true)
	p := w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(300), w.now)
	w.record(settledRecord("r1", "research", p.ID, dollars(300), w.now))

	// Close the activity store underneath the reporter, which is what a locked,
	// deleted, or corrupt telemetry database looks like from here.
	if err := w.acts.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary must still succeed on ledger data: %v", err)
	}
	if sum.Position.Spent != dollars(300) {
		t.Errorf("Spent = %s, want the ledger's %s", sum.Position.Spent, dollars(300))
	}
	if sum.ActivityAvailable {
		t.Error("ActivityAvailable = true after the activity store failed")
	}
	if sum.ActivityError == "" {
		t.Error("ActivityError is empty; a read failure must be explained, not hidden")
	}
	// Reporting a delta equal to the whole ledger would be alarming nonsense.
	if sum.Disagreement.Material() {
		t.Errorf("Material() = true when there is nothing to disagree with: %+v", sum.Disagreement)
	}
	if sum.Disagreement.LedgerSpent != dollars(300) {
		t.Errorf("LedgerSpent = %s, want the authoritative figure", sum.Disagreement.LedgerSpent)
	}
}

// A ledger file that cannot be opened at all is a startup failure, not a blank page.
func TestOpeningAMissingLedgerDirectoryFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "no", "such", "dir")
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the fixture directory unexpectedly exists")
	}
	// Nothing to assert about the reporter here: New requires a ledger, and that is
	// the property worth pinning.
	if _, err := New(Config{}); err == nil {
		t.Error("New with no ledger returned no error")
	} else if !errors.Is(err, ErrNoLedger) {
		t.Errorf("err = %v, want ErrNoLedger", err)
	}
}

// --- ledger / activity disagreement ---------------------------------------

// The two stores are written by separate transactions with a paid provider call
// between them. The dashboard surfaces the difference; it does not resolve it.
func TestDisagreementIsSurfacedNotResolved(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	// The ledger settled $300. Activity recorded only $250 of it -- a crash between
	// the two writes.
	w.spend("s1", "research", dollars(300), w.now)
	w.record(settledRecord("r1", "research", p.ID, dollars(250), w.now))

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Position.Spent != dollars(300) {
		t.Errorf("Spent = %s; the ledger remains monetary truth", sum.Position.Spent)
	}
	d := sum.Disagreement
	if d.LedgerSpent != dollars(300) || d.ActivitySpent != dollars(250) {
		t.Errorf("Disagreement = %s vs %s, want $300 vs $250", d.LedgerSpent, d.ActivitySpent)
	}
	if d.Delta != dollars(50) {
		t.Errorf("Delta = %s, want %s", d.Delta, dollars(50))
	}
	if !d.ActivityComplete {
		t.Error("ActivityComplete = false; both records were fully priced")
	}
	if !d.Material() {
		t.Error("Material() = false for a $50 unexplained difference")
	}
}

// An incomplete activity figure is expected to sit below the ledger. That is what
// "floor" means, and it is not a disagreement.
func TestAnIncompleteActivityFigureIsNotADisagreement(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(300), w.now)

	rec := settledRecord("r1", "research", p.ID, dollars(250), w.now)
	rec.ActualCost = usage.PartialCost(dollars(250),
		[]usage.Dimension{usage.CacheWriteTokens}, "no rate for cache writes")
	w.record(rec)

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Disagreement.ActivityComplete {
		t.Fatal("ActivityComplete = true with an unpriced dimension")
	}
	if sum.Disagreement.Delta != dollars(50) {
		t.Errorf("Delta = %s, want %s", sum.Disagreement.Delta, dollars(50))
	}
	if sum.Disagreement.Material() {
		t.Error("Material() = true for a floor sitting below the ledger, which is expected")
	}
}

// An activity figure ABOVE the ledger is material whatever its completeness: a floor
// cannot legitimately exceed monetary truth.
func TestActivityAboveTheLedgerIsAlwaysMaterial(t *testing.T) {
	d := Disagreement{
		LedgerSpent:      dollars(100),
		ActivitySpent:    dollars(150),
		ActivityComplete: false,
		Delta:            -dollars(50),
	}
	if !d.Material() {
		t.Error("Material() = false when the activity floor exceeds the ledger")
	}
}

// --- sub-budget hierarchy -------------------------------------------------

// scopedWorld builds research -> research/nlp and research/vision, all monthly.
func scopedWorld(t *testing.T) (*world, map[string]ledger.Period) {
	t.Helper()
	w := newWorld(t)
	periods := map[string]ledger.Period{}
	periods["research"] = w.define(monthly("research", "", dollars(1000)))
	periods["nlp"] = w.define(monthly("nlp", "research", dollars(400)))
	periods["vision"] = w.define(monthly("vision", "research", dollars(300)))
	return w, periods
}

// The persisted hierarchy renders as a hierarchy, and a child's spend rolls up to its
// parent because that is how the ledger recorded the legs.
func TestSubBudgetHierarchyScopesAndRollsUp(t *testing.T) {
	w, periods := scopedWorld(t)

	// $120 through the child, which the ledger charges to both scopes.
	w.spend("s1", "nlp", dollars(120), w.now, "nlp", "research")
	w.record(childRecord("r1", "nlp", periods["nlp"].ID, periods["research"].ID, dollars(120), w.now))

	child, err := w.rep.Summary(w.ctx, "nlp")
	if err != nil {
		t.Fatalf("Summary(nlp): %v", err)
	}
	if child.Position.Spent != dollars(120) {
		t.Errorf("child Spent = %s, want %s", child.Position.Spent, dollars(120))
	}
	if child.Position.Total != dollars(400) {
		t.Errorf("child Total = %s, want its own allocation %s", child.Position.Total, dollars(400))
	}

	parent, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary(research): %v", err)
	}
	if parent.Position.Spent != dollars(120) {
		t.Errorf("parent Spent = %s, want the child's spend rolled up", parent.Position.Spent)
	}
	if parent.Position.Total != dollars(1000) {
		t.Errorf("parent Total = %s, want %s", parent.Position.Total, dollars(1000))
	}

	// A sibling that spent nothing must not inherit the other child's spend.
	sib, err := w.rep.Summary(w.ctx, "vision")
	if err != nil {
		t.Fatalf("Summary(vision): %v", err)
	}
	if sib.Position.Spent != 0 {
		t.Errorf("sibling Spent = %s, want zero", sib.Position.Spent)
	}

	// The chain explains the child's constraint: itself, then its parent.
	if len(child.Chain) != 2 {
		t.Fatalf("chain has %d entries, want the child and its parent", len(child.Chain))
	}
	if child.Chain[0].BudgetID != "nlp" || child.Chain[1].BudgetID != "research" {
		t.Errorf("chain = %q -> %q, want nlp -> research",
			child.Chain[0].BudgetID, child.Chain[1].BudgetID)
	}
}

// The tree is the persisted parent/child structure, with each node's own position.
func TestTreeRendersTheHierarchy(t *testing.T) {
	w, _ := scopedWorld(t)
	w.spend("s1", "nlp", dollars(120), w.now, "nlp", "research")

	tree, err := w.rep.Tree(w.ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree.Roots) != 1 {
		t.Fatalf("got %d roots, want 1: %+v", len(tree.Roots), tree.Roots)
	}
	root := tree.Roots[0]
	if root.Position.BudgetID != "research" {
		t.Fatalf("root = %q, want research", root.Position.BudgetID)
	}
	if len(root.Children) != 2 {
		t.Fatalf("root has %d children, want 2", len(root.Children))
	}
	// Children in ID order, so the table is stable between page loads.
	if root.Children[0].Position.BudgetID != "nlp" || root.Children[1].Position.BudgetID != "vision" {
		t.Errorf("children = %q, %q, want nlp then vision",
			root.Children[0].Position.BudgetID, root.Children[1].Position.BudgetID)
	}

	flat := tree.Flatten()
	if len(flat) != 3 {
		t.Fatalf("Flatten returned %d nodes, want 3", len(flat))
	}
	if flat[0].Depth != 0 || flat[1].Depth != 1 || flat[2].Depth != 1 {
		t.Errorf("depths = %d, %d, %d, want 0, 1, 1", flat[0].Depth, flat[1].Depth, flat[2].Depth)
	}
	for _, n := range flat {
		if n.Position.Total == 0 {
			t.Errorf("%q has no allocation; every node carries its own position", n.Position.BudgetID)
		}
	}
	if len(tree.Errors) != 0 {
		t.Errorf("Errors = %v, want none", tree.Errors)
	}
}

// hidingParent is the real ledger with one definition withheld from Definitions.
//
// The sqlite ledger refuses to store a child whose parent it does not know and its
// foreign keys refuse to let the parent be deleted afterwards, so an orphan is
// unreachable through the store's API -- which is the point. Tree's orphan branch is
// defensive, against a partially restored or hand-edited database, and this is the
// narrowest way to exercise it: every other read still goes to the real store.
type hidingParent struct {
	Ledger
	hide string
}

func (h hidingParent) Definitions(ctx context.Context) ([]budget.Definition, error) {
	defs, err := h.Ledger.Definitions(ctx)
	if err != nil {
		return nil, err
	}
	out := defs[:0]
	for _, d := range defs {
		if d.ID != h.hide {
			out = append(out, d)
		}
	}
	return out, nil
}

// A child whose parent is missing from the definition set is shown at the root rather
// than dropped. Losing a budget from a spend display is worse than misplacing it.
func TestAnOrphanedChildIsStillShown(t *testing.T) {
	w, _ := scopedWorld(t)

	rep, err := New(Config{
		Ledger: hidingParent{Ledger: w.led, hide: "research"},
		Clock:  func() time.Time { return w.now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tree, err := rep.Tree(w.ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(tree.Roots) != 2 {
		t.Fatalf("got %d roots, want both orphans shown: %+v", len(tree.Roots), tree.Roots)
	}
	for _, root := range tree.Roots {
		if root.Position.ParentID != "research" {
			t.Errorf("%q ParentID = %q; the claimed parent is retained even though it is missing",
				root.Position.BudgetID, root.Position.ParentID)
		}
		if root.Position.Total == 0 {
			t.Errorf("%q lost its allocation", root.Position.BudgetID)
		}
	}
}

// A leaf with headroom can still be governed by an ancestor that has none, and the
// binding budget is what explains a refusal.
func TestBindingBudgetNamesTheTightestAncestor(t *testing.T) {
	w, _ := scopedWorld(t)

	// The parent is nearly exhausted; the child has almost all of its own allocation.
	w.spend("s1", "vision", dollars(295), w.now, "vision", "research")
	w.spend("s2", "research", dollars(700), w.now, "research")

	child, err := w.rep.Summary(w.ctx, "nlp")
	if err != nil {
		t.Fatalf("Summary(nlp): %v", err)
	}
	if child.Position.SpendableNow <= 0 {
		t.Fatalf("the child has no headroom of its own: %s", child.Position.SpendableNow)
	}
	if child.Binding != "research" {
		t.Errorf("Binding = %q, want research: the parent is what would refuse the next request",
			child.Binding)
	}

	// With no ancestor tighter than the leaf, the leaf is its own constraint.
	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary(research): %v", err)
	}
	if sum.Binding != "research" {
		t.Errorf("Binding = %q, want research", sum.Binding)
	}
}

// Ties go to the budget the caller named, which is the more useful answer.
func TestBindingTiesGoToTheNamedBudget(t *testing.T) {
	chain := []Position{
		{BudgetID: "nlp", SpendableNow: dollars(100)},
		{BudgetID: "research", SpendableNow: dollars(100)},
	}
	if got := binding(chain); got != "nlp" {
		t.Errorf("binding = %q, want the leaf on a tie", got)
	}
	if got := binding(nil); got != "" {
		t.Errorf("binding of an empty chain = %q, want empty", got)
	}
}

// Activity scoped to a child returns the child's requests; scoped to the parent it
// returns them too, because the hold consumed both scopes.
func TestActivityScopesThroughTheHierarchy(t *testing.T) {
	w, periods := scopedWorld(t)
	w.record(childRecord("r1", "nlp", periods["nlp"].ID, periods["research"].ID, dollars(120), w.now))

	child, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "nlp"})
	if err != nil {
		t.Fatalf("Activity(nlp): %v", err)
	}
	if len(child.Events) != 1 {
		t.Fatalf("child sees %d events, want 1", len(child.Events))
	}

	parent, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity(research): %v", err)
	}
	if len(parent.Events) != 1 {
		t.Fatalf("parent sees %d events, want its child's request", len(parent.Events))
	}
	if parent.Events[0].BudgetID != "nlp" {
		t.Errorf("BudgetID = %q, want the budget the caller actually named", parent.Events[0].BudgetID)
	}

	sib, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "vision"})
	if err != nil {
		t.Fatalf("Activity(vision): %v", err)
	}
	if len(sib.Events) != 0 {
		t.Errorf("the sibling sees %d events, want none", len(sib.Events))
	}
}

// childRecord is a request against a child budget, charged to both scopes.
func childRecord(requestID, budgetID, childPeriod, parentPeriod string, cost money.Money, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, childPeriod, cost, at)
	rec.Scopes = []activity.Scope{
		{BudgetID: budgetID, PeriodID: childPeriod, Depth: 0},
		{BudgetID: "research", PeriodID: parentPeriod, Depth: 1},
	}
	return rec
}

// --- periods --------------------------------------------------------------

// Monthly is not special. A one-week demo budget, an academic grant with fixed dates,
// and a monthly recurrence all report through the same envelope fields.
func TestPeriodsAreWhateverTheDefinitionGenerated(t *testing.T) {
	w := newWorld(t)

	weekly := budget.Definition{
		ID: "demo", Name: "demo", Allocation: dollars(50),
		Recurrence: budget.RecurNone,
		AnchorAt:   base,
		EndAt:      base.Add(7 * 24 * time.Hour),
	}
	if err := w.led.PutDefinition(w.ctx, weekly); err != nil {
		t.Fatalf("PutDefinition: %v", err)
	}
	w.now = base.Add(3 * 24 * time.Hour)
	if _, err := w.led.EnsurePeriod(w.ctx, "demo", w.now); err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}

	sum, err := w.rep.Summary(w.ctx, "demo")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if !sum.Position.PeriodStart.Equal(base) {
		t.Errorf("PeriodStart = %s, want %s", sum.Position.PeriodStart, base)
	}
	if want := base.Add(7 * 24 * time.Hour); !sum.Position.PeriodEnd.Equal(want) {
		t.Errorf("PeriodEnd = %s, want the definition's own end %s", sum.Position.PeriodEnd, want)
	}
	if want := 7 * 24 * time.Hour; sum.Position.PeriodDuration != want {
		t.Errorf("PeriodDuration = %s, want %s", sum.Position.PeriodDuration, want)
	}
	// Three of seven days in: the target is 3/7 of $50.
	if want := dollars(50) * 3 / 7; sum.Position.TargetByNow != want {
		t.Errorf("TargetByNow = %s, want %s", sum.Position.TargetByNow, want)
	}
	// A week-long period reads better per day than per hour, and the underlying rate
	// stays duration-normalized either way.
	if sum.Position.AverageBurn.Per != 24*time.Hour {
		t.Errorf("display unit = %s, want daily for a one-week period", sum.Position.AverageBurn.Per)
	}
	if sum.Position.SustainableBurn.Per != 24*time.Hour {
		t.Errorf("sustainable display unit = %s, want daily", sum.Position.SustainableBurn.Per)
	}
}

// The period selector lists the materialized periods, most recent first, marking the
// one containing the clock.
func TestPeriodsListsMostRecentFirstAndMarksTheCurrent(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	// Materialize the next two months by advancing the clock deliberately.
	for _, at := range []time.Time{
		time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
	} {
		if _, err := w.led.EnsurePeriod(w.ctx, "research", at); err != nil {
			t.Fatalf("EnsurePeriod(%s): %v", at, err)
		}
	}
	w.now = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	opts, err := w.rep.Periods(w.ctx, "research")
	if err != nil {
		t.Fatalf("Periods: %v", err)
	}
	if len(opts) != 3 {
		t.Fatalf("got %d periods, want 3", len(opts))
	}
	if !opts[0].Start.After(opts[1].Start) {
		t.Error("periods are not most-recent-first")
	}
	current := 0
	for _, o := range opts {
		if o.Current {
			current++
			if o.Start.Month() != time.September {
				t.Errorf("the current period starts in %s, want September", o.Start.Month())
			}
		}
		if o.PeriodID == "" || o.End.IsZero() {
			t.Errorf("period option is incomplete: %+v", o)
		}
	}
	if current != 1 {
		t.Errorf("%d periods marked current, want exactly 1", current)
	}
}

// A prior period reads as fully elapsed rather than as though it were still running.
func TestPositionInAPriorPeriodIsFullyElapsed(t *testing.T) {
	w := newWorld(t)
	first := w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(400), w.now)

	// Move to the next month.
	w.now = time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	if _, err := w.led.EnsurePeriod(w.ctx, "research", w.now); err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}

	pos, err := w.rep.PositionIn(w.ctx, "research", first.ID)
	if err != nil {
		t.Fatalf("PositionIn: %v", err)
	}
	if pos.TimeRemaining != 0 {
		t.Errorf("TimeRemaining = %s in a closed period, want zero", pos.TimeRemaining)
	}
	if pos.Elapsed != pos.PeriodDuration {
		t.Errorf("Elapsed = %s, want the whole period %s", pos.Elapsed, pos.PeriodDuration)
	}
	if pos.Spent != dollars(400) {
		t.Errorf("Spent = %s, want %s", pos.Spent, dollars(400))
	}
	// At the end of a period the pacing target is the whole allocation, so $400 spent
	// of $1000 is $600 banked.
	if want := dollars(600); pos.PaceBalance != want {
		t.Errorf("PaceBalance = %s, want %s", pos.PaceBalance, want)
	}
	if pos.Pressure.State != PressureEnded {
		t.Errorf("Pressure state = %q, want ended", pos.Pressure.State)
	}
	// The straight-line projection of a finished period is what it actually spent.
	if pos.Projection.Known && pos.Projection.Amount != dollars(400) {
		t.Errorf("Projection = %s, want the settled figure %s", pos.Projection.Amount, dollars(400))
	}
}

// A period belonging to a different budget is a bad request, not a silently wrong page.
func TestPositionInRejectsAPeriodFromAnotherBudget(t *testing.T) {
	w, periods := scopedWorld(t)

	_, err := w.rep.PositionIn(w.ctx, "research", periods["nlp"].ID)
	if err == nil {
		t.Fatal("PositionIn accepted a period belonging to another budget")
	}
	if !errors.Is(err, ledger.ErrInvalidArgument) {
		t.Errorf("err = %v, want ledger.ErrInvalidArgument", err)
	}
}

// A future period is not started, and an expired one has ended. Both render honestly
// rather than dividing by zero.
func TestFutureAndExpiredPeriodsRenderHonestly(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	w.now = base.Add(-24 * time.Hour)
	future, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary before the period starts: %v", err)
	}
	if future.Position.Elapsed != 0 {
		t.Errorf("Elapsed = %s before the start, want zero", future.Position.Elapsed)
	}
	if future.Position.Pressure.State != PressureNotStarted {
		t.Errorf("Pressure = %q, want not-started", future.Position.Pressure.State)
	}
	if future.Position.AverageBurn.Known {
		t.Error("AverageBurn is known before any time elapsed")
	}
	if future.Position.Projection.Known {
		t.Error("a projection was offered before any time elapsed")
	}

	w.now = base.Add(monthDuration + 24*time.Hour)
	expired, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary after the period ends: %v", err)
	}
	if expired.Position.Period.ID != p.ID {
		t.Errorf("period = %q, want the nearest materialized one %q", expired.Position.Period.ID, p.ID)
	}
	if expired.Position.TimeRemaining != 0 {
		t.Errorf("TimeRemaining = %s after the end, want zero", expired.Position.TimeRemaining)
	}
	if expired.Position.Pressure.State != PressureEnded {
		t.Errorf("Pressure = %q, want ended", expired.Position.Pressure.State)
	}
	if expired.Position.SustainableBurn.Known {
		t.Error("a sustainable rate was offered with no time left to sustain it")
	}
}

// --- fully spent and overspent --------------------------------------------

// A fully spent budget has no headroom, and an overspent one is reported as negative
// rather than clamped into looking merely exhausted.
func TestFullySpentAndOverspentStates(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("full", "", dollars(100)))
	w.spend("s1", "full", dollars(100), w.now)

	sum, err := w.rep.Summary(w.ctx, "full")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Position.RemainingAllocation != 0 {
		t.Errorf("RemainingAllocation = %s, want zero", sum.Position.RemainingAllocation)
	}
	if sum.Position.SpendableNow != 0 {
		t.Errorf("SpendableNow = %s, want zero", sum.Position.SpendableNow)
	}
	if sum.Position.Overspent() {
		t.Error("Overspent() = true for a budget spent exactly to its allocation")
	}
	if sum.Position.Pressure.State != PressureNoHeadroom {
		t.Errorf("Pressure = %q, want no-headroom", sum.Position.Pressure.State)
	}
	if sum.Position.SustainableBurn.Known {
		t.Error("a sustainable rate was offered with no allocation left")
	}

	// Actual cost may exceed a reservation. The overrun is recorded, not hidden.
	w.define(monthly("over", "", dollars(100)))
	over := w.hold("h1", "over", dollars(90), w.now)
	if _, err := w.led.Settle(w.ctx, ledger.Settlement{
		ReservationID: over.ID, ActualCost: dollars(130), CompletedAt: w.now,
	}); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	sum2, err := w.rep.Summary(w.ctx, "over")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum2.Position.Spent != dollars(130) {
		t.Errorf("Spent = %s, want the real %s", sum2.Position.Spent, dollars(130))
	}
	if want := -dollars(30); sum2.Position.RemainingAllocation != want {
		t.Errorf("RemainingAllocation = %s, want %s rendered honestly negative",
			sum2.Position.RemainingAllocation, want)
	}
	if !sum2.Position.Overspent() {
		t.Error("Overspent() = false for a budget $30 past its allocation")
	}
	if sum2.Position.SpendableNow != 0 {
		t.Errorf("SpendableNow = %s, want zero rather than negative headroom",
			sum2.Position.SpendableNow)
	}
	if !sum2.Position.Borrowed() {
		t.Error("Borrowed() = false for spend well ahead of pace")
	}
}

// A budget with holds and no settled spend has real encumbrance and no spend. The two
// must not be conflated in either direction.
func TestActiveReservationsOnly(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))
	w.hold("h1", "research", dollars(40), w.now)
	w.hold("h2", "research", dollars(60), w.now)

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Position.Spent != 0 {
		t.Errorf("Spent = %s; holds are not spend", sum.Position.Spent)
	}
	if sum.Position.Reserved != dollars(100) {
		t.Errorf("Reserved = %s, want %s", sum.Position.Reserved, dollars(100))
	}
	if sum.Position.Committed != dollars(100) {
		t.Errorf("Committed = %s, want spent plus reserved", sum.Position.Committed)
	}
	if sum.Position.LiveHolds != 2 {
		t.Errorf("LiveHolds = %d, want 2", sum.Position.LiveHolds)
	}

	holds, err := w.rep.Reservations(w.ctx, "research", 0)
	if err != nil {
		t.Fatalf("Reservations: %v", err)
	}
	if len(holds) != 2 {
		t.Fatalf("got %d holds, want 2", len(holds))
	}
	for _, h := range holds {
		if h.Amount == 0 || h.ExpiresAt.IsZero() {
			t.Errorf("hold is incomplete: %+v", h)
		}
		if h.Expired {
			t.Errorf("hold %q reads as expired inside its lease", h.ReservationID)
		}
		if h.Model == "" {
			t.Errorf("hold %q has no model; a reader should see what is holding the money",
				h.ReservationID)
		}
	}
}

// An expired hold is not evidence of zero spend: the request may have been served and
// billed, and it remains settleable.
func TestExpiredHoldsAreVisibleAndNotTreatedAsZeroSpend(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))
	w.hold("h1", "research", dollars(40), w.now)

	// Past the lease.
	w.now = w.now.Add(2 * time.Hour)

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Position.ExpiredHolds != 1 {
		t.Errorf("ExpiredHolds = %d, want 1", sum.Position.ExpiredHolds)
	}
	if sum.Position.ExpiredAmount != dollars(40) {
		t.Errorf("ExpiredAmount = %s, want %s", sum.Position.ExpiredAmount, dollars(40))
	}
	if sum.Health.ExpiredHolds != 1 || sum.Health.ExpiredAmount != dollars(40) {
		t.Errorf("Health does not carry the expired holds: %+v", sum.Health)
	}
	if !sum.Health.Needs() {
		t.Error("Needs() = false with a lapsed lease outstanding")
	}

	holds, err := w.rep.Reservations(w.ctx, "research", 0)
	if err != nil {
		t.Fatalf("Reservations: %v", err)
	}
	if len(holds) != 1 {
		t.Fatalf("got %d holds, want the expired one still listed", len(holds))
	}
	if !holds[0].Expired {
		t.Error("Expired = false past the lease")
	}
	if holds[0].Age < time.Hour {
		t.Errorf("Age = %s, want the standing time that makes a stranded hold obvious", holds[0].Age)
	}
}
