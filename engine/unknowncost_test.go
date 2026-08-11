package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"throttle/budget"
	"throttle/money"
	"throttle/usage"
)

// unpriced is an estimate for a model the catalog cannot price at all.
func unpriced(reason string) usage.Estimate {
	return usage.Estimate{Cost: usage.UnknownCost(reason)}
}

// Enforce mode cannot honestly govern a dollar budget it cannot measure against,
// so an unpriced request is refused -- and refused before the provider is called,
// which is the only point at which refusing costs nothing.
func TestBeginDeniesUnknownCostUnderEnforcement(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start)
	ctx := context.Background()

	tx, dec, err := eng.Begin(ctx, Request{
		BudgetID:  "research",
		RequestID: "req-1",
		Estimate:  unpriced("no price for anthropic.model-released-yesterday"),
	})
	if err == nil {
		t.Fatal("enforce mode must refuse a request whose cost is unknown")
	}
	if !errors.Is(err, ErrCostUnknown) {
		t.Errorf("error = %v, want ErrCostUnknown", err)
	}
	if tx != nil {
		t.Fatal("a denied request must not hold a reservation")
	}
	if dec.Admitted {
		t.Error("Decision.Admitted must be false")
	}
	if dec.Outcome != budget.OutcomeDeny {
		t.Errorf("Outcome = %q, want deny", dec.Outcome)
	}
	// The reason has to name the actual problem, not just "denied": an operator
	// looking at this needs to know it is a pricing gap, not a spent budget.
	if dec.Reason == "" || !strings.Contains(dec.Reason, "unknown") {
		t.Errorf("Reason = %q, want it to say the cost is unknown", dec.Reason)
	}
	if dec.BindingBudgetID == "" {
		t.Error("a denial must name a binding budget")
	}

	// Nothing was reserved, so the budget is untouched and the next priced request
	// still has its full headroom.
	st, err := eng.Status(ctx, "research")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Snapshot.Reserved != 0 || st.Snapshot.Spent != 0 {
		t.Errorf("Reserved = %s, Spent = %s, want an untouched budget", st.Snapshot.Reserved, st.Snapshot.Spent)
	}
}

// A denial for an unpriced model must not depend on there being no headroom: the
// budget here is enormous, and the request is still refused.
func TestBeginDeniesUnknownCostEvenWithAmpleHeadroom(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeEnforce, start, unpaced("rich", "", dollars(1_000_000)))
	_, _, err := eng.Begin(context.Background(), Request{
		BudgetID:  "rich",
		RequestID: "req-1",
		Estimate:  unpriced("no price"),
	})
	if !errors.Is(err, ErrCostUnknown) {
		t.Errorf("error = %v, want ErrCostUnknown regardless of headroom", err)
	}
}

// Monitor mode observes rather than governs, so it admits an unpriced request --
// but it must record that the cost is unknown rather than let it pass as free.
func TestBeginAdmitsUnknownCostUnderMonitoring(t *testing.T) {
	eng, _ := newEngine(t, ModeMonitor, start)
	ctx := context.Background()

	tx, dec, err := eng.Begin(ctx, Request{
		BudgetID:  "research",
		RequestID: "req-1",
		Estimate:  unpriced("no price for anthropic.model-released-yesterday"),
	})
	if err != nil {
		t.Fatalf("monitor mode must admit an unpriced request: %v", err)
	}
	if !dec.Admitted {
		t.Error("Decision.Admitted must be true in monitor mode")
	}
	if !dec.CostUnknown {
		t.Error("Decision.CostUnknown must be set: the request was admitted without a price")
	}
	if dec.Mode != ModeMonitor {
		t.Errorf("Mode = %q, want monitor", dec.Mode)
	}
	// The hold is zero because there was no amount to hold, not because the request
	// is free. The distinction lives in CostUnknown and in the activity record.
	if got := tx.Reservation().Amount; got != 0 {
		t.Errorf("reserved %s for an unpriced request, want 0: no amount was knowable", got)
	}
}

// A budget with no room left must still admit an unpriced request under
// monitoring: the whole point of monitor mode is that it never blocks.
func TestMonitorAdmitsUnknownCostOnAnExhaustedBudget(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeMonitor, start, unpaced("tiny", "", money.Money(1)))
	ctx := context.Background()

	// Spend the budget out first.
	tx, _, err := eng.Begin(ctx, Request{BudgetID: "tiny", RequestID: "req-1", Estimate: estimate(dollars(10))})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Settle(ctx, spent(dollars(10))); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if _, dec, err := eng.Begin(ctx, Request{
		BudgetID: "tiny", RequestID: "req-2", Estimate: unpriced("no price"),
	}); err != nil {
		t.Fatalf("monitor mode must not block: %v", err)
	} else if !dec.CostUnknown {
		t.Error("Decision.CostUnknown must be set")
	}
}

// The strictest mode in the chain governs, so a monitored child under an enforcing
// parent is still refused: the parent's dollars are the ones at risk.
func TestUnknownCostDenialFollowsTheStrictestModeInTheChain(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeEnforce, start,
		unpaced("parent", "", dollars(1000)),
		unpaced("child", "parent", dollars(100)),
	)
	if err := eng.SetMode("child", ModeMonitor); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	_, dec, err := eng.Begin(context.Background(), Request{
		BudgetID:  "child",
		RequestID: "req-1",
		Estimate:  unpriced("no price"),
	})
	if !errors.Is(err, ErrCostUnknown) {
		t.Fatalf("error = %v, want ErrCostUnknown: the enforcing parent governs", err)
	}
	if dec.Admitted {
		t.Error("a monitored child under an enforcing parent must not be admitted unpriced")
	}
}

// A known price at admission must remain the basis at settlement. This is the
// engine's half of that contract: a priced estimate settles normally, and the
// amount charged is the actual, not the estimate.
func TestKnownCostAtBeginSettlesNormally(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeEnforce, start, unpaced("research", "", dollars(3000)))
	ctx := context.Background()

	tx, dec, err := eng.Begin(ctx, Request{
		BudgetID:  "research",
		RequestID: "req-1",
		Estimate:  estimate(dollars(10)),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if dec.CostUnknown {
		t.Error("a priced request must not be marked cost-unknown")
	}
	if got := tx.Reservation().Amount; got != dollars(10) {
		t.Errorf("reserved %s, want %s", got, dollars(10))
	}

	charge, err := tx.Settle(ctx, spent(dollars(12)))
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	// The actual is authoritative even over the reservation: an overrun is recorded,
	// not hidden.
	if charge.ActualCost != dollars(12) {
		t.Errorf("ActualCost = %s, want %s", charge.ActualCost, dollars(12))
	}
	if tx.Unresolved() {
		t.Error("a settled request is not unresolved")
	}
}

// A cost throttle cannot name must not settle. Settling the priced floor as though
// it were the total would understate spend in a way no later reader could detect.
func TestSettleRefusesAnUnknownCost(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeEnforce, start, unpaced("research", "", dollars(3000)))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "req-1", Estimate: estimate(dollars(10)),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	partial := usage.Actual{
		Usage: usage.New(map[usage.Dimension]int64{
			usage.InputTokens:            1000,
			usage.Dimension("video-sec"): 30,
		}),
		Cost: usage.PartialCost(dollars(3), []usage.Dimension{usage.Dimension("video-sec")},
			"no rate for video-sec"),
	}
	if _, err := tx.Settle(ctx, partial); !errors.Is(err, ErrCostUnresolved) {
		t.Fatalf("Settle error = %v, want ErrCostUnresolved", err)
	}

	// Refusing to settle must not resolve the transaction either way: the caller
	// still has to decide what to do, and the hold is still live.
	st, err := eng.Status(ctx, "research")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Snapshot.Reserved != dollars(10) {
		t.Errorf("Reserved = %s, want the hold still standing at %s", st.Snapshot.Reserved, dollars(10))
	}
}

// An unresolved liability keeps consuming its reserved headroom. This is the whole
// mechanism: the money was spent, so the budget must not offer it to the next
// caller merely because throttle cannot name the amount.
func TestUnresolvedLiabilityKeepsConsumingHeadroom(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeEnforce, start, unpaced("team", "", dollars(100)))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "team", RequestID: "req-1", Estimate: estimate(dollars(60)),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	actual := usage.Actual{
		Usage: usage.New(map[usage.Dimension]int64{
			usage.InputTokens:            1000,
			usage.Dimension("video-sec"): 30,
		}),
		Cost: usage.PartialCost(dollars(3), []usage.Dimension{usage.Dimension("video-sec")},
			"no rate for video-sec"),
	}
	if err := tx.MarkUnresolved(ctx, actual); err != nil {
		t.Fatalf("MarkUnresolved: %v", err)
	}
	if !tx.Unresolved() {
		t.Error("the transaction must report itself unresolved")
	}

	// The reservation still stands, so the $60 is not available again.
	st, err := eng.Status(ctx, "team")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Snapshot.Reserved != dollars(60) {
		t.Errorf("Reserved = %s, want %s still encumbered", st.Snapshot.Reserved, dollars(60))
	}

	// And the next request sees only the remaining headroom. A released hold would
	// have let this $50 request through.
	if _, _, err := eng.Begin(ctx, Request{
		BudgetID: "team", RequestID: "req-2", Estimate: estimate(dollars(50)),
	}); !errors.Is(err, ErrDenied) {
		t.Errorf("error = %v, want ErrDenied: the unresolved hold must still block", err)
	}

	// $40 fits alongside the encumbrance, which confirms the hold is a normal
	// reservation rather than a total freeze.
	if _, _, err := eng.Begin(ctx, Request{
		BudgetID: "team", RequestID: "req-3", Estimate: estimate(dollars(40)),
	}); err != nil {
		t.Errorf("a request within the remaining headroom must still be admitted: %v", err)
	}
}

// Releasing an unresolved liability would claim already-spent money back as
// available, so it is refused outright.
func TestReleaseRefusesAnUnresolvedLiability(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeEnforce, start, unpaced("team", "", dollars(100)))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "team", RequestID: "req-1", Estimate: estimate(dollars(60)),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.MarkUnresolved(ctx, usage.Actual{
		Cost: usage.UnknownCost("no price for this model"),
	}); err != nil {
		t.Fatalf("MarkUnresolved: %v", err)
	}

	if err := tx.Release(ctx); !errors.Is(err, ErrCostUnresolved) {
		t.Fatalf("Release error = %v, want ErrCostUnresolved", err)
	}

	// The hold survived the refused release.
	st, err := eng.Status(ctx, "team")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Snapshot.Reserved != dollars(60) {
		t.Errorf("Reserved = %s, want the encumbrance intact at %s", st.Snapshot.Reserved, dollars(60))
	}
}

// A known cost must go through Settle. Allowing MarkUnresolved to swallow it would
// let real, nameable spend sit unrecorded in the ledger.
func TestMarkUnresolvedRefusesAKnownCost(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeEnforce, start, unpaced("research", "", dollars(3000)))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "req-1", Estimate: estimate(dollars(10)),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.MarkUnresolved(ctx, spent(dollars(9))); err == nil {
		t.Error("MarkUnresolved must refuse a cost that is fully known")
	}
	if tx.Unresolved() {
		t.Error("a refused MarkUnresolved must not mark the transaction")
	}
}

// Once pricing arrives, an unresolved liability is still settleable: the hold was
// never resolved, so the ordinary settlement path closes it.
func TestUnresolvedLiabilityCanSettleOnceItIsPriced(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeEnforce, start, unpaced("team", "", dollars(100)))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "team", RequestID: "req-1", Estimate: estimate(dollars(60)),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := tx.MarkUnresolved(ctx, usage.Actual{
		Cost: usage.UnknownCost("no rate for video-sec"),
	}); err != nil {
		t.Fatalf("MarkUnresolved: %v", err)
	}

	// The missing rate is added and the real cost becomes nameable.
	charge, err := tx.Settle(ctx, spent(dollars(72)))
	if err != nil {
		t.Fatalf("Settle after reconciliation: %v", err)
	}
	if charge.ActualCost != dollars(72) {
		t.Errorf("ActualCost = %s, want %s", charge.ActualCost, dollars(72))
	}

	st, err := eng.Status(ctx, "team")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Snapshot.Spent != dollars(72) {
		t.Errorf("Spent = %s, want %s", st.Snapshot.Spent, dollars(72))
	}
	if st.Snapshot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0 once the liability settled", st.Snapshot.Reserved)
	}
}
