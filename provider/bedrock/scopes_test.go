package bedrock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"throttle/activity"
	activitysqlite "throttle/activity/sqlite"
	"throttle/budget"
	"throttle/engine"
	"throttle/ledger"
	"throttle/ledger/sqlite"
	"throttle/money"
	"throttle/provider/bedrock"
)

// wholePeriod is a borrow window covering the whole month, so these tests exercise
// attribution rather than pacing: a request refused for being early in the month would be
// testing the pacing curve, not the scope chain.
const wholePeriod = 62 * 24 * time.Hour

// The activity-scope invariant.
//
// Parent-level reporting has no other mechanism. A child's spend appears in its parent's
// figures because the child's activity record carries a scope for every budget the
// reservation consumed, and a report for the parent matches on those scopes. So an activity
// record that named only the leaf would make a parent's reported spend understate reality --
// silently, and by exactly the amount its children spent.
//
// The contract has two halves, and both are tested here:
//
//   - The ledger derives a reservation's legs from stored parent links, not from anything the
//     caller says. A caller cannot omit an ancestor and thereby escape its ceiling.
//   - The provider path derives activity scopes from those legs (scopesOf), not from a list
//     assembled at the call site. So the recorded attribution is the same chain the money
//     actually moved through.
//
// Together those mean the attribution cannot drift from the accounting: there is one source
// for both, and it is the stored hierarchy.

// newDeepHarness builds team → division → child, each with room to spend, so a request
// against the leaf consumes three envelopes.
//
// Three levels rather than two on purpose: a two-level test passes for an implementation that
// records "the budget and its parent", which is not the invariant. The invariant is the whole
// consumed chain, however deep.
func newDeepHarness(t *testing.T, opts ...func(*bedrock.Config)) (*harness, *activitysqlite.Store) {
	t.Helper()

	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	current := now
	clock := func() time.Time { return current }

	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	anchor := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, def := range []budget.Definition{
		{ID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly, AnchorAt: anchor,
			Borrow: wholePeriod},
		{ID: "division", ParentID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly,
			AnchorAt: anchor, Borrow: wholePeriod},
		{ID: "child", ParentID: "division", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly,
			AnchorAt: anchor, Borrow: wholePeriod},
	} {
		if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", def.ID, err)
		}
	}

	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := buildHarness(t, eng, store, clock, append([]func(*bedrock.Config){opt}, opts...)...)
	return h, acts
}

// scopeIDs lists the budgets a record is attributed to.
func scopeIDs(rec activity.Record) []string {
	out := make([]string, 0, len(rec.Scopes))
	for _, s := range rec.Scopes {
		out = append(out, s.BudgetID)
	}
	return out
}

func hasScope(rec activity.Record, budgetID string) bool {
	for _, s := range rec.Scopes {
		if s.BudgetID == budgetID {
			return true
		}
	}
	return false
}

// (21) A request against a leaf records the leaf and every consumed ancestor.
func TestActivityScopesCarryTheWholeAncestorChain(t *testing.T) {
	h, acts := newDeepHarness(t)

	if _, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID:  "child",
		RequestID: "req-chain",
		Input:     request(sonnetID, aws.Int32(2000)),
	}); err != nil {
		t.Fatalf("Converse: %v", err)
	}

	rec := getRecord(t, acts, "req-chain")

	for _, id := range []string{"child", "division", "team"} {
		if !hasScope(rec, id) {
			t.Errorf("Scopes = %v, want %q present: an omitted ancestor understates its spend",
				scopeIDs(rec), id)
		}
	}
	if len(rec.Scopes) != 3 {
		t.Errorf("Scopes = %v, want exactly the three consumed budgets", scopeIDs(rec))
	}

	// Every scope names a period, or the spend belongs to no envelope and cannot be
	// attributed to one.
	for _, s := range rec.Scopes {
		if s.PeriodID == "" {
			t.Errorf("scope %q has no period", s.BudgetID)
		}
	}

	// And each scope's period is the one that budget's own money moved through. A parent
	// whose calendar differs from its child's would otherwise have spend filed against the
	// wrong envelope.
	for _, s := range rec.Scopes {
		p, err := h.ledger.EnsurePeriod(context.Background(), s.BudgetID, h.clock())
		if err != nil {
			t.Fatalf("EnsurePeriod(%s): %v", s.BudgetID, err)
		}
		if s.PeriodID != p.ID {
			t.Errorf("scope %q names period %q, want %q", s.BudgetID, s.PeriodID, p.ID)
		}
	}
}

// The recorded scopes are exactly the reservation's legs, which is what makes them accounting
// rather than annotation.
//
// Asserted against the ledger's own reservation rather than against a hand-written list: if
// the derivation were ever replaced by a caller-side list, this comparison is what fails.
func TestActivityScopesMatchTheReservationLegs(t *testing.T) {
	h, acts := newDeepHarness(t)

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID:  "child",
		RequestID: "req-legs",
		Input:     request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}

	rec := getRecord(t, acts, "req-legs")
	if rec.ReservationID != res.ReservationID {
		t.Fatalf("ReservationID = %q, want %q", rec.ReservationID, res.ReservationID)
	}

	// The legs the ledger actually held, taken from the charge trail for the leaf's period.
	chain, err := h.ledger.Chain(context.Background(), "child")
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("Chain has %d budgets, want 3", len(chain))
	}

	want := map[string]bool{}
	for _, def := range chain {
		want[def.ID] = true
	}
	for _, s := range rec.Scopes {
		if !want[s.BudgetID] {
			t.Errorf("record is attributed to %q, which is not in the budget's chain", s.BudgetID)
		}
		delete(want, s.BudgetID)
	}
	if len(want) > 0 {
		t.Errorf("chain members missing from the record's scopes: %v", want)
	}
}

// (22) A parent's reporting sees its child's request and its child's spend.
//
// This is the property the whole invariant exists to support, tested from a real provider
// call rather than from a hand-written record: the query a parent-level report runs is a
// scope match, and it finds the child's request because the adapter recorded the chain.
//
// Asserted on both stores, because they answer different halves and a dashboard shows both.
// The ledger is authoritative for the money; the activity store supplies the attribution. A
// parent whose total moved but whose history was empty would look like spend from nowhere.
func TestParentReportingSeesChildActivity(t *testing.T) {
	h, acts := newDeepHarness(t)
	ctx := context.Background()

	res, err := h.client.Converse(ctx, bedrock.Request{
		BudgetID:  "child",
		RequestID: "req-rollup",
		Input:     request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if !res.Settled {
		t.Fatal("the request should have settled; there is no spend to attribute otherwise")
	}
	spent := res.Cost.Amount
	if spent <= 0 {
		t.Fatalf("the call cost %s, so this test proves nothing about attribution", spent)
	}

	// Every budget in the chain, including the grandparent, answers the query -- the
	// intermediate one is not special.
	for _, id := range []string{"child", "division", "team"} {
		found, err := acts.List(ctx, activity.Filter{BudgetID: id})
		if err != nil {
			t.Fatalf("List(%s): %v", id, err)
		}
		if len(found) != 1 {
			t.Errorf("%s sees %d requests, want the child's", id, len(found))
			continue
		}
		// The named budget stays the child's. An ancestor sees the request without the
		// request being reattributed to it.
		if found[0].BudgetID != "child" {
			t.Errorf("%s sees the request attributed to %q, want the budget the caller named",
				id, found[0].BudgetID)
		}
		if got := activity.Summarize(found).Spend; got != spent {
			t.Errorf("%s sees spend %s, want the child's %s", id, got.CentsString(), spent.CentsString())
		}
	}

	// And the money agrees at every level, which is what makes the attribution a report of
	// reality rather than a label.
	for _, id := range []string{"child", "division", "team"} {
		p, err := h.ledger.EnsurePeriod(ctx, id, h.clock())
		if err != nil {
			t.Fatalf("EnsurePeriod(%s): %v", id, err)
		}
		tot, err := h.ledger.Totals(ctx, ledger.Scope{BudgetID: id, PeriodID: p.ID}, h.clock())
		if err != nil {
			t.Fatalf("Totals(%s): %v", id, err)
		}
		if tot.Spent != spent {
			t.Errorf("%s ledger spend = %s, want %s", id, tot.Spent.CentsString(), spent.CentsString())
		}
	}

	// A sibling of the child inherits nothing. Without this, a scope list containing every
	// budget in the store would pass everything above.
	if err := h.engine.Register(ctx, budget.Definition{
		ID: "sibling", ParentID: "division", Allocation: dollars(t, "1000"),
		Recurrence: budget.RecurMonthly, AnchorAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Borrow: wholePeriod,
	}, engine.ModeEnforce); err != nil {
		t.Fatalf("Register sibling: %v", err)
	}
	found, err := acts.List(ctx, activity.Filter{BudgetID: "sibling"})
	if err != nil {
		t.Fatalf("List(sibling): %v", err)
	}
	if len(found) != 0 {
		t.Errorf("the sibling sees %d requests, want none", len(found))
	}
}

// (23) A caller cannot construct a request that escapes an ancestor's ceiling.
//
// There is no field on a request for "which budgets this consumes", and this asserts why that
// matters rather than merely that the field is absent: the ledger derives the chain from
// stored parent links, so naming the leaf is naming the whole chain whether the caller
// intended it or not. A parent with no headroom refuses a child request even though the
// child's own allocation would afford it.
func TestNamingALeafCannotBypassAnAncestorCeiling(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	current := now
	clock := func() time.Time { return current }
	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	// The parent is the binding constraint: a fraction of a cent, against a child that
	// could afford the request many times over.
	anchor := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, def := range []budget.Definition{
		{ID: "team", Allocation: dollars(t, "0.0001"), Recurrence: budget.RecurMonthly, AnchorAt: anchor,
			Borrow: wholePeriod},
		{ID: "child", ParentID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly,
			AnchorAt: anchor, Borrow: wholePeriod},
	} {
		if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", def.ID, err)
		}
	}

	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := buildHarness(t, eng, store, clock, opt)

	_, err = h.client.Converse(context.Background(), bedrock.Request{
		BudgetID:  "child",
		RequestID: "req-bypass",
		Input:     request(sonnetID, aws.Int32(2000)),
	})
	if err == nil {
		t.Fatal("a request the parent cannot afford was admitted by naming only the child")
	}
	if h.api.callCount() != 0 {
		t.Errorf("the provider was called %d times for a denied request", h.api.callCount())
	}

	// The denial is recorded and attributed to the child, so a reader can see the refusal
	// rather than inferring it from an absence.
	rec := getRecord(t, acts, "req-bypass")
	if rec.Status != activity.StatusDenied {
		t.Errorf("Status = %q, want denied", rec.Status)
	}
	if rec.BudgetID != "child" {
		t.Errorf("BudgetID = %q, want child", rec.BudgetID)
	}
}

// Reserve itself refuses a leg with no ceiling, which is the ledger-level half of the same
// invariant: an ancestor cannot be reserved against without a limit to check.
//
// Tested directly against the ledger rather than through the adapter, because this is the
// property the adapter relies on and it should fail here if it ever changes.
func TestReserveRequiresACeilingForEveryLeg(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	anchor := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, def := range []budget.Definition{
		{ID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly, AnchorAt: anchor},
		{ID: "child", ParentID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly,
			AnchorAt: anchor},
	} {
		if err := store.PutDefinition(ctx, def); err != nil {
			t.Fatalf("PutDefinition %s: %v", def.ID, err)
		}
	}

	// A ceiling for the leaf and none for its parent: the caller has, in effect, tried to
	// spend against the child without acknowledging the parent.
	if _, err := store.EnsurePeriod(ctx, "child", now); err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	_, err = store.Reserve(ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID:       "res-1",
			BudgetID: "child",
			Amount:   dollars(t, "1"),
		},
		Ceilings: map[string]money.Money{"child": dollars(t, "1000")},
		Now:      now,
	})
	if !errors.Is(err, ledger.ErrMissingCeiling) {
		t.Errorf("err = %v, want ErrMissingCeiling: an ancestor with no ceiling is unchecked", err)
	}

	// With every leg's ceiling present it succeeds, and the hold names both budgets. Without
	// this half, the test above would pass for a Reserve that rejected everything.
	res, err := store.Reserve(ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID:       "res-2",
			BudgetID: "child",
			Amount:   dollars(t, "1"),
		},
		Ceilings: map[string]money.Money{
			"child": dollars(t, "1000"),
			"team":  dollars(t, "1000"),
		},
		Now: now,
	})
	if err != nil {
		t.Fatalf("Reserve with every ceiling: %v", err)
	}
	if len(res.Legs) != 2 {
		t.Fatalf("the hold has %d legs, want one per budget in the chain", len(res.Legs))
	}
	// Nearest first, which is the order scopesOf preserves into the activity record.
	if res.Legs[0].Scope.BudgetID != "child" || res.Legs[1].Scope.BudgetID != "team" {
		t.Errorf("legs = %q then %q, want child then team",
			res.Legs[0].Scope.BudgetID, res.Legs[1].Scope.BudgetID)
	}
}
