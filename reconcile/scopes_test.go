package reconcile_test

import (
	"context"
	"testing"
	"time"

	"throttle/activity"
	"throttle/budget"
	"throttle/engine"
	"throttle/ledger"
	"throttle/money"
	"throttle/reconcile"
	"throttle/usage"
)

// Attribution has to survive a crash, because a repair that loses it silently moves spend
// out of a parent's reported figures.
//
// A child's spend is visible from its parent through the activity record's scopes. A
// reconciler that completed a record without them would leave the money correct in the
// ledger and the reporting wrong: the parent's authoritative total would include the spend
// while its request history would not, and nobody looking at either page would see a reason
// to doubt it.
//
// Two recoveries are covered, because the durable evidence differs:
//
//   - A record exists and its bookkeeping is incomplete. Its scopes were written before the
//     provider call and are carried through the repair unchanged.
//   - No record exists at all. Nothing is invented, but the attribution is still recoverable,
//     because the reservation's own legs name every scope the hold consumed.

// squad is a child of the fixture's team budget, so a request against it consumes two
// envelopes.
func (f *fixture) defineChild(t *testing.T) {
	t.Helper()
	f.define(budget.Definition{
		ID:         "squad",
		ParentID:   "team",
		Allocation: dollars(50),
		Recurrence: budget.RecurMonthly,
		AnchorAt:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	})
}

// reserveChild takes a real hold against the child, which the ledger charges to the whole
// chain. Written out rather than generalizing the fixture's own reserve, which is about the
// single-budget crashes the rest of the file exercises.
func (f *fixture) reserveChild(id string, amount money.Money, lease time.Duration) ledger.Reservation {
	f.t.Helper()
	ctx := context.Background()
	chain, err := f.led.Chain(ctx, "squad")
	if err != nil {
		f.t.Fatalf("Chain: %v", err)
	}
	if len(chain) != 2 {
		f.t.Fatalf("chain has %d budgets, want the child and its parent", len(chain))
	}
	ceilings := map[string]money.Money{}
	for _, d := range chain {
		ceilings[d.ID] = dollars(100)
	}
	res := ledger.Reservation{
		ID:            "res-" + id,
		BudgetID:      "squad",
		RequestID:     id,
		Amount:        amount,
		EstimatedCost: amount,
		CreatedAt:     f.now,
		Identity:      identity(),
	}
	if lease > 0 {
		res.ExpiresAt = f.now.Add(lease)
		res.Lease = lease
	}
	out, err := f.led.Reserve(ctx, ledger.ReserveRequest{Reservation: res, Ceilings: ceilings, Now: f.now})
	if err != nil {
		f.t.Fatalf("Reserve(%q): %v", id, err)
	}
	return out
}

// beginChild writes the pre-call record for a request against the child, with the scopes
// derived from the hold rather than named at the call site.
func (f *fixture) beginChild(id string, res ledger.Reservation) activity.Record {
	f.t.Helper()
	rec := activity.Record{
		RequestID:     id,
		ReservationID: res.ID,
		BudgetID:      "squad",
		Scopes:        scopesOf(res),
		Identity:      identity(),
		Estimate: usage.Estimate{
			Identity: identity(),
			Cost:     usage.KnownCost(res.Amount),
			Quality:  usage.QualityHeuristic,
		},
		Quote:           quote(),
		Reserved:        res.Amount,
		EnforcementMode: engine.ModeEnforce,
		Status:          activity.StatusPending,
		StartedAt:       f.now,
	}
	if err := f.acts.Begin(context.Background(), rec); err != nil {
		f.t.Fatalf("activity Begin(%q): %v", id, err)
	}
	return rec
}

func scopeSet(rec activity.Record) map[string]activity.Scope {
	out := map[string]activity.Scope{}
	for _, s := range rec.Scopes {
		out[s.BudgetID] = s
	}
	return out
}

// A replayed settlement keeps the ancestor chain on the record, so the parent still sees
// its child's request afterwards.
func TestRepairPreservesAncestorAttribution(t *testing.T) {
	f := newFixture(t)
	f.defineChild(t)
	ctx := context.Background()

	res := f.reserveChild("req-scope-1", dollars(1), time.Minute)
	rec := f.beginChild("req-scope-1", res)
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	// The scopes as they stood before the repair, which is what the repair must not lose.
	before := scopeSet(f.get("req-scope-1"))
	if len(before) != 2 {
		t.Fatalf("the pre-crash record names %d budgets, want the child and its parent", len(before))
	}

	got, err := f.reconciler(reconcile.Config{}).Reconcile(ctx, "req-scope-1")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassRepaired {
		t.Fatalf("class = %q, want repaired (%s)", got.Class, got.Detail)
	}

	after := scopeSet(f.get("req-scope-1"))
	for id, want := range before {
		have, ok := after[id]
		if !ok {
			t.Errorf("the repaired record no longer names %q: its spend has left that budget's history", id)
			continue
		}
		if have.PeriodID != want.PeriodID {
			t.Errorf("scope %q names period %q after the repair, want %q", id, have.PeriodID, want.PeriodID)
		}
		if have.Depth != want.Depth {
			t.Errorf("scope %q is at depth %d after the repair, want %d", id, have.Depth, want.Depth)
		}
	}
	if len(after) != len(before) {
		t.Errorf("the repaired record names %d budgets, want %d", len(after), len(before))
	}

	// And the attribution is not merely present on the record: querying by the parent finds
	// the child's request, which is the property a parent-level report depends on.
	found, err := f.acts.List(ctx, activity.Filter{BudgetID: "team"})
	if err != nil {
		t.Fatalf("List(team): %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("the parent sees %d requests after the repair, want its child's", len(found))
	}
	if found[0].RequestID != "req-scope-1" || found[0].BudgetID != "squad" {
		t.Errorf("parent sees %+v, want the child's request attributed to the child", found[0])
	}

	// The money moved through both envelopes, which is why both must appear.
	if spent := f.totals("squad").Spent; spent != observedCost {
		t.Errorf("child spend = %s, want %s", spent.CentsString(), observedCost.CentsString())
	}
	if spent := f.totals("team").Spent; spent != observedCost {
		t.Errorf("parent spend = %s, want the child's spend rolled up (%s)",
			spent.CentsString(), observedCost.CentsString())
	}
}

// A crash before the record was written leaves nothing to repair, but the attribution is
// still recoverable: the hold itself names every scope it consumed.
//
// This is what makes the "orphan invents nothing" rule affordable. Refusing to fabricate a
// record does not mean the chain is lost, because the ledger holds it independently of the
// telemetry store.
func TestAncestorAttributionIsRecoverableFromTheLedger(t *testing.T) {
	f := newFixture(t)
	f.defineChild(t)
	ctx := context.Background()

	res := f.reserveChild("req-scope-2", dollars(1), time.Minute)
	// No activity record: the crash landed between Reserve and Begin. Found through a pass
	// over the ledger rather than by request ID, because by ID there is nothing to look up.
	sum, err := f.reconciler(reconcile.Config{}).ReconcilePending(ctx)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if sum.Orphaned != 1 {
		t.Fatalf("orphaned = %d, want 1 (%+v)", sum.Orphaned, sum.Results)
	}
	if got := sum.Results[0]; got.Class != reconcile.ClassOrphaned {
		t.Fatalf("class = %q, want orphaned (%s)", got.Class, got.Detail)
	}

	// Nothing was invented, and nothing was lost either. The hold read back from the ledger
	// still names the whole chain, at the right periods, so the attribution can be
	// reconstructed by anything that needs it.
	held, err := f.led.Get(ctx, res.ID)
	if err != nil {
		t.Fatalf("ledger Get: %v", err)
	}
	if len(held.Legs) != 2 {
		t.Fatalf("the hold has %d legs, want one per budget in the chain", len(held.Legs))
	}
	recovered := scopesOf(held)
	seen := map[string]bool{}
	for _, s := range recovered {
		seen[s.BudgetID] = true
		if s.PeriodID == "" {
			t.Errorf("recovered scope %q names no period", s.BudgetID)
		}
	}
	for _, id := range []string{"squad", "team"} {
		if !seen[id] {
			t.Errorf("recovered scopes = %v, want %q present", recovered, id)
		}
	}
	// Nearest first, which is the order a record would have stored.
	if recovered[0].BudgetID != "squad" || recovered[1].BudgetID != "team" {
		t.Errorf("recovered scopes = %v, want the child then its parent", recovered)
	}
}
