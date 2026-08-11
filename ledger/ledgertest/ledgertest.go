// Package ledgertest is a conformance suite for ledger.Ledger implementations.
//
// The suite is the executable specification of the reservation contract. It
// exists so that a store cannot quietly diverge from the semantics that matter
// for real money: atomicity across a budget hierarchy, exactly-once settlement,
// lease expiry, and period transitions.
package ledgertest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"throttle/budget"
	"throttle/ledger"
	"throttle/money"
)

// Factory builds a fresh, empty store for one test.
type Factory func(t *testing.T) ledger.Ledger

func dollars(d int64) money.Money { return money.Money(d) * money.PerDollar }

// base is the reference instant: the start of a month, so monthly period
// boundaries land on round numbers in assertions.
var base = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// monthly builds a recurring monthly definition anchored at base.
func monthly(id, parent string, allocation money.Money) budget.Definition {
	return budget.Definition{
		ID:         id,
		ParentID:   parent,
		Allocation: allocation,
		Recurrence: budget.RecurMonthly,
		AnchorAt:   base,
	}
}

// Run executes the full conformance suite against a store implementation.
func Run(t *testing.T, newStore Factory) {
	t.Helper()

	tests := map[string]func(*testing.T, ledger.Ledger){
		// Definitions.
		"DefinitionRoundTrip":          testDefinitionRoundTrip,
		"DefinitionIsIdempotent":       testDefinitionIsIdempotent,
		"ConflictingDefinitionRefused": testConflictingDefinitionRefused,
		"RenameIsNotAConflict":         testRenameIsNotAConflict,
		"InvalidDefinitionRefused":     testInvalidDefinitionRefused,
		"UpdateRequiresRevision":       testUpdateRequiresRevision,
		"MissingParentRefused":         testMissingParentRefused,
		"CycleRefused":                 testCycleRefused,
		"ChainIsNearestFirst":          testChainIsNearestFirst,
		"UnknownBudgetIsNotFound":      testUnknownBudgetIsNotFound,

		// Periods.
		"EnsurePeriodIsIdempotent":       testEnsurePeriodIsIdempotent,
		"PeriodsMaterializeGaps":         testPeriodsMaterializeGaps,
		"PeriodSnapshotsDefinition":      testPeriodSnapshotsDefinition,
		"PeriodAdvancesToDraining":       testPeriodAdvancesToDraining,
		"DrainedPeriodCloses":            testDrainedPeriodCloses,
		"CarryFlowsToNextPeriod":         testCarryFlowsToNextPeriod,
		"ProvisionalCarryIsConservative": testProvisionalCarryIsConservative,
		"MoneyIsConservedAcrossPeriods":  testMoneyIsConservedAcrossPeriods,
		"DebtCarriesUncapped":            testDebtCarriesUncapped,
		"ElapsedPeriodRefusesNewWork":    testElapsedPeriodRefusesNewWork,

		// Reservation basics.
		"ReserveAndSettle":             testReserveAndSettle,
		"ReserveRespectsCeiling":       testReserveRespectsCeiling,
		"CeilingIsExact":               testCeilingIsExact,
		"DeniedReserveWritesNothing":   testDeniedReserveWritesNothing,
		"DuplicateReservationRefused":  testDuplicateReservationRefused,
		"MissingCeilingRefused":        testMissingCeilingRefused,
		"InvalidReserveRefused":        testInvalidReserveRefused,
		"ZeroAmountAllowed":            testZeroAmountAllowed,
		"DoubleSettleRefused":          testDoubleSettleRefused,
		"ReleaseAfterSettleRefused":    testReleaseAfterSettleRefused,
		"SettleAfterReleaseRefused":    testSettleAfterReleaseRefused,
		"ReleaseFreesHeadroom":         testReleaseFreesHeadroom,
		"OverrunIsRecorded":            testOverrunIsRecorded,
		"UnknownReservationIsNotFound": testUnknownReservationIsNotFound,

		// Leases.
		"ExpiredHoldFreesHeadroom":      testExpiredHoldFreesHeadroom,
		"RenewPreventsExpiry":           testRenewPreventsExpiry,
		"RenewUsesLeaseQuantum":         testRenewUsesLeaseQuantum,
		"RenewAfterExpiryRefused":       testRenewAfterExpiryRefused,
		"RenewAfterResolveRefused":      testRenewAfterResolveRefused,
		"RenewNeverMovesDeadlineBack":   testRenewNeverMovesDeadlineBack,
		"SettleAfterExpiryStillCharges": testSettleAfterExpiryStillCharges,
		"ZeroExpiryNeverExpires":        testZeroExpiryNeverExpires,
		"RecoverReclaimsAbandoned":      testRecoverReclaimsAbandoned,
		"RecoverIsIdempotent":           testRecoverIsIdempotent,

		// Hierarchy.
		"ChildReservesWholeChain":       testChildReservesWholeChain,
		"ChildFailsWhenAncestorFull":    testChildFailsWhenAncestorFull,
		"AncestorFailureWritesNothing":  testAncestorFailureWritesNothing,
		"SettleUpdatesWholeChainOnce":   testSettleUpdatesWholeChainOnce,
		"ReleaseFreesWholeChainOnce":    testReleaseFreesWholeChainOnce,
		"SiblingsShareParentHeadroom":   testSiblingsShareParentHeadroom,
		"ParentSpendExcludesSiblings":   testParentSpendExcludesSiblings,
		"RecoverReclaimsChildViaParent": testRecoverReclaimsChildViaParent,

		// Concurrency.
		"ConcurrentReserveCeiling":        testConcurrentReserveCeiling,
		"ConcurrentChildrenRespectParent": testConcurrentChildrenRespectParent,
		"ConcurrentSettleAndRelease":      testConcurrentSettleAndRelease,
		"ConcurrentRenewIsSafe":           testConcurrentRenewIsSafe,
		"ConcurrentEnsurePeriod":          testConcurrentEnsurePeriod,
		"ConcurrentMixedLoad":             testConcurrentMixedLoad,

		// Arithmetic.
		"NoOverflowOnHugeAmounts": testNoOverflowOnHugeAmounts,
		"MicrodollarPrecision":    testMicrodollarPrecision,

		// Reporting.
		"ChargesAreScopedAndOrdered": testChargesAreScopedAndOrdered,
		"ChargesWindowIsHalfOpen":    testChargesWindowIsHalfOpen,
	}

	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			fn(t, newStore(t))
		})
	}
}

// --- helpers ---------------------------------------------------------------

// mustPut stores definitions in order, so a parent exists before its child.
func mustPut(t *testing.T, s ledger.Ledger, defs ...budget.Definition) {
	t.Helper()
	for _, def := range defs {
		if err := s.PutDefinition(context.Background(), def); err != nil {
			t.Fatalf("PutDefinition(%q): %v", def.ID, err)
		}
	}
}

// chainIDs is the reference hierarchy from the milestone brief, nearest first.
var chainIDs = []string{"literature-agent", "agents", "research"}

// hierarchy builds research ($1000) -> agents ($600) -> literature-agent ($300).
func hierarchy(t *testing.T, s ledger.Ledger) {
	t.Helper()
	mustPut(t, s,
		monthly("research", "", dollars(1000)),
		monthly("agents", "research", dollars(600)),
		monthly("literature-agent", "agents", dollars(300)),
	)
}

// ceil builds a ceiling map from alternating id/amount pairs, so a test can name
// exactly the constraint it means to exercise.
func ceil(pairs ...any) map[string]money.Money {
	m := map[string]money.Money{}
	for i := 0; i+1 < len(pairs); i += 2 {
		switch v := pairs[i+1].(type) {
		case money.Money:
			m[pairs[i].(string)] = v
		case int:
			m[pairs[i].(string)] = money.Money(v)
		default:
			panic(fmt.Sprintf("ceil: %T is not an amount", v))
		}
	}
	return m
}

// unlimited gives every named budget an effectively infinite ceiling, for tests
// about something other than headroom.
func unlimited(ids ...string) map[string]money.Money {
	m := map[string]money.Money{}
	for _, id := range ids {
		m[id] = money.Max
	}
	return m
}

// reserve is the common case: a hold against one budget with explicit ceilings.
func reserve(t *testing.T, s ledger.Ledger, id, budgetID string, amount money.Money,
	now time.Time, ceilings map[string]money.Money) (ledger.Reservation, error) {
	t.Helper()
	return s.Reserve(context.Background(), ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID: id, BudgetID: budgetID, RequestID: "req-" + id,
			Amount: amount, EstimatedCost: amount, CreatedAt: now,
		},
		Ceilings: ceilings,
		Now:      now,
	})
}

func mustReserve(t *testing.T, s ledger.Ledger, id, budgetID string, amount money.Money,
	now time.Time, ceilings map[string]money.Money) ledger.Reservation {
	t.Helper()
	r, err := reserve(t, s, id, budgetID, amount, now, ceilings)
	if err != nil {
		t.Fatalf("Reserve(%q): %v", id, err)
	}
	return r
}

// reserveWithLease creates a hold with an explicit lease deadline.
func reserveWithLease(t *testing.T, s ledger.Ledger, id, budgetID string, amount money.Money,
	now time.Time, lease time.Duration, ceilings map[string]money.Money) ledger.Reservation {
	t.Helper()
	r, err := s.Reserve(context.Background(), ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID: id, BudgetID: budgetID, RequestID: "req-" + id,
			Amount: amount, EstimatedCost: amount, CreatedAt: now,
			ExpiresAt: now.Add(lease), Lease: lease,
		},
		Ceilings: ceilings,
		Now:      now,
	})
	if err != nil {
		t.Fatalf("Reserve(%q): %v", id, err)
	}
	return r
}

func mustSettle(t *testing.T, s ledger.Ledger, id string, actual money.Money, at time.Time) ledger.Charge {
	t.Helper()
	c, err := s.Settle(context.Background(), ledger.Settlement{
		ReservationID: id, ActualCost: actual, CompletedAt: at,
	})
	if err != nil {
		t.Fatalf("Settle(%q): %v", id, err)
	}
	return c
}

// scopeOf finds the scope for a budget in a reservation's leg set.
func scopeOf(t *testing.T, r ledger.Reservation, budgetID string) ledger.Scope {
	t.Helper()
	for _, l := range r.Legs {
		if l.Scope.BudgetID == budgetID {
			return l.Scope
		}
	}
	t.Fatalf("reservation %q has no leg for budget %q (legs %+v)", r.ID, budgetID, r.Legs)
	return ledger.Scope{}
}

// totalsFor reads the totals for a budget's period containing now.
func totalsFor(t *testing.T, s ledger.Ledger, budgetID string, now time.Time) ledger.Totals {
	t.Helper()
	ctx := context.Background()
	p, err := s.EnsurePeriod(ctx, budgetID, now)
	if err != nil {
		t.Fatalf("EnsurePeriod(%q): %v", budgetID, err)
	}
	tot, err := s.Totals(ctx, ledger.Scope{BudgetID: budgetID, PeriodID: p.ID}, now)
	if err != nil {
		t.Fatalf("Totals(%q): %v", budgetID, err)
	}
	return tot
}

func assertTotals(t *testing.T, s ledger.Ledger, budgetID string, now time.Time, spent, reserved money.Money) {
	t.Helper()
	tot := totalsFor(t, s, budgetID, now)
	if tot.Spent != spent {
		t.Errorf("%s: Spent = %s, want %s", budgetID, tot.Spent, spent)
	}
	if tot.Reserved != reserved {
		t.Errorf("%s: Reserved = %s, want %s", budgetID, tot.Reserved, reserved)
	}
}

// assertScopeError checks that err refuses on behalf of a specific budget.
// Callers need to know which constraint said no, not merely that one did.
func assertScopeError(t *testing.T, err error, budgetID string, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
	var scopeErr *ledger.ScopeError
	if !errors.As(err, &scopeErr) {
		t.Fatalf("error %v is not a *ledger.ScopeError", err)
	}
	if scopeErr.BudgetID != budgetID {
		t.Errorf("refused by budget %q, want %q", scopeErr.BudgetID, budgetID)
	}
}

// --- definitions -----------------------------------------------------------

func testDefinitionRoundTrip(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	def := monthly("research", "", dollars(4000))
	def.Name = "Research"
	def.Borrow = 72 * time.Hour
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance, CapBasisPoints: 2500}
	mustPut(t, s, def)

	got, revision, err := s.Definition(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Errorf("revision = %d, want 1", revision)
	}
	// The fingerprint covers every semantic field, so comparing it catches a
	// column that failed to round-trip without enumerating them here.
	if got.Fingerprint() != def.Fingerprint() {
		t.Errorf("definition changed across storage:\n got %+v\nwant %+v", got, def)
	}
	if got.Name != "Research" {
		t.Errorf("Name = %q, want %q", got.Name, "Research")
	}
	if got.Borrow != 72*time.Hour {
		t.Errorf("Borrow = %v, want 72h", got.Borrow)
	}
	if got.Rollover.CapBasisPoints != 2500 {
		t.Errorf("CapBasisPoints = %d, want 2500", got.Rollover.CapBasisPoints)
	}

	all, err := s.Definitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("Definitions returned %d, want 1", len(all))
	}
}

func testDefinitionIsIdempotent(t *testing.T, s ledger.Ledger) {
	def := monthly("research", "", dollars(4000))
	mustPut(t, s, def)
	// Every process re-registers its definitions at startup, so storing an
	// identical definition must succeed rather than conflict.
	mustPut(t, s, def)
	mustPut(t, s, def)
}

// testConflictingDefinitionRefused is the acceptance criterion that two
// processes cannot silently govern the same ledger under different rules.
func testConflictingDefinitionRefused(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(4000)))

	mutate := map[string]func(*budget.Definition){
		"allocation": func(d *budget.Definition) { d.Allocation = dollars(9000) },
		"recurrence": func(d *budget.Definition) { d.Recurrence = budget.RecurWeekly },
		"anchor":     func(d *budget.Definition) { d.AnchorAt = base.AddDate(0, 0, 1) },
		"rollover":   func(d *budget.Definition) { d.Rollover.Mode = budget.RolloverBalance },
		"cap":        func(d *budget.Definition) { d.Rollover.Cap = dollars(100) },
		"borrow":     func(d *budget.Definition) { d.Borrow = time.Hour },
		"end":        func(d *budget.Definition) { d.EndAt = base.AddDate(1, 0, 0) },
	}
	for name, mutation := range mutate {
		def := monthly("research", "", dollars(4000))
		mutation(&def)
		if err := s.PutDefinition(ctx, def); !errors.Is(err, ledger.ErrDefinitionConflict) {
			t.Errorf("PutDefinition with differing %s: error = %v, want ErrDefinitionConflict", name, err)
		}
	}

	// The stored definition must be untouched by the rejected writes.
	got, revision, err := s.Definition(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if got.Allocation != dollars(4000) {
		t.Errorf("allocation = %s, want the original %s", got.Allocation, dollars(4000))
	}
	if revision != 1 {
		t.Errorf("revision = %d, want 1: a rejected write must not bump it", revision)
	}
}

func testRenameIsNotAConflict(t *testing.T, s ledger.Ledger) {
	def := monthly("research", "", dollars(4000))
	def.Name = "Research"
	mustPut(t, s, def)

	// A display name is not semantics, so renaming must not read as two processes
	// disagreeing about the rules.
	def.Name = "Research Group"
	mustPut(t, s, def)
}

func testInvalidDefinitionRefused(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()

	bad := map[string]budget.Definition{
		"no id":         {Recurrence: budget.RecurMonthly, AnchorAt: base},
		"no recurrence": {ID: "x", AnchorAt: base},
		"no anchor":     {ID: "x", Recurrence: budget.RecurMonthly},
		"negative allocation": {
			ID: "x", Recurrence: budget.RecurMonthly, AnchorAt: base, Allocation: -1,
		},
		"both cap forms": {
			ID: "x", Recurrence: budget.RecurMonthly, AnchorAt: base,
			Rollover: budget.RolloverPolicy{Mode: budget.RolloverCredit, Cap: dollars(1), CapBasisPoints: 100},
		},
		"self parent": {
			ID: "x", ParentID: "x", Recurrence: budget.RecurMonthly, AnchorAt: base,
		},
	}
	for name, def := range bad {
		if err := s.PutDefinition(ctx, def); err == nil {
			t.Errorf("PutDefinition(%s) succeeded, want an error", name)
		}
	}
}

func testUpdateRequiresRevision(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(4000)))

	next := monthly("research", "", dollars(5000))
	if err := s.UpdateDefinition(ctx, next, 7); !errors.Is(err, ledger.ErrRevisionMismatch) {
		t.Errorf("stale revision: error = %v, want ErrRevisionMismatch", err)
	}
	if err := s.UpdateDefinition(ctx, next, 1); err != nil {
		t.Fatalf("UpdateDefinition: %v", err)
	}

	got, revision, err := s.Definition(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if revision != 2 {
		t.Errorf("revision = %d, want 2", revision)
	}
	if got.Allocation != dollars(5000) {
		t.Errorf("allocation = %s, want %s", got.Allocation, dollars(5000))
	}
	// Replaying the same update must not apply twice: that is the whole point of
	// carrying an expected revision.
	if err := s.UpdateDefinition(ctx, next, 1); !errors.Is(err, ledger.ErrRevisionMismatch) {
		t.Errorf("replayed update: error = %v, want ErrRevisionMismatch", err)
	}
	if err := s.UpdateDefinition(ctx, monthly("ghost", "", dollars(1)), 1); !errors.Is(err, ledger.ErrBudgetNotFound) {
		t.Errorf("update of unknown budget: error = %v, want ErrBudgetNotFound", err)
	}
}

func testMissingParentRefused(t *testing.T, s ledger.Ledger) {
	err := s.PutDefinition(context.Background(), monthly("child", "nonexistent", dollars(100)))
	if !errors.Is(err, ledger.ErrBudgetNotFound) {
		t.Errorf("error = %v, want ErrBudgetNotFound", err)
	}
}

// testCycleRefused matters because a cycle would make "the set of scopes this
// request consumes" undefined and the ancestor walk nonterminating.
func testCycleRefused(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s,
		monthly("a", "", dollars(100)),
		monthly("b", "a", dollars(100)),
		monthly("c", "b", dollars(100)),
	)

	// Repointing a's parent at its own descendant closes a loop.
	if err := s.UpdateDefinition(ctx, monthly("a", "c", dollars(100)), 1); !errors.Is(err, ledger.ErrCycle) {
		t.Errorf("error = %v, want ErrCycle", err)
	}

	// The hierarchy must be undamaged by the rejected write.
	chain, err := s.Chain(ctx, "c")
	if err != nil {
		t.Fatalf("Chain after rejected cycle: %v", err)
	}
	if len(chain) != 3 {
		t.Errorf("chain length = %d, want 3", len(chain))
	}
}

func testChainIsNearestFirst(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	hierarchy(t, s)

	chain, err := s.Chain(ctx, "literature-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != len(chainIDs) {
		t.Fatalf("chain length = %d, want %d (%+v)", len(chain), len(chainIDs), chain)
	}
	for i, id := range chainIDs {
		if chain[i].ID != id {
			t.Errorf("chain[%d] = %q, want %q", i, chain[i].ID, id)
		}
	}

	// A root's chain is just itself.
	root, err := s.Chain(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(root) != 1 || root[0].ID != "research" {
		t.Errorf("root chain = %+v, want just research", root)
	}
}

func testUnknownBudgetIsNotFound(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	if _, _, err := s.Definition(ctx, "ghost"); !errors.Is(err, ledger.ErrBudgetNotFound) {
		t.Errorf("Definition: error = %v, want ErrBudgetNotFound", err)
	}
	if _, err := s.Chain(ctx, "ghost"); !errors.Is(err, ledger.ErrBudgetNotFound) {
		t.Errorf("Chain: error = %v, want ErrBudgetNotFound", err)
	}
	if _, err := s.EnsurePeriod(ctx, "ghost", base); !errors.Is(err, ledger.ErrBudgetNotFound) {
		t.Errorf("EnsurePeriod: error = %v, want ErrBudgetNotFound", err)
	}
	if _, err := reserve(t, s, "r1", "ghost", dollars(1), base, unlimited("ghost")); !errors.Is(err, ledger.ErrBudgetNotFound) {
		t.Errorf("Reserve: error = %v, want ErrBudgetNotFound", err)
	}
}

// --- periods ---------------------------------------------------------------

func testEnsurePeriodIsIdempotent(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))

	first, err := s.EnsurePeriod(ctx, "research", base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnsurePeriod(ctx, "research", base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Errorf("two instants in the same period produced %q and %q", first.ID, second.ID)
	}

	periods, err := s.Periods(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 1 {
		t.Errorf("materialized %d period rows, want 1", len(periods))
	}
	if !first.Envelope.Start.Equal(base) {
		t.Errorf("period starts %s, want %s", first.Envelope.Start, base)
	}
	if want := base.AddDate(0, 1, 0); !first.Envelope.End.Equal(want) {
		t.Errorf("period ends %s, want %s", first.Envelope.End, want)
	}
	if !first.CarryFinal {
		t.Error("the first period's carry must be final: nothing precedes it")
	}
	if first.State != ledger.StateOpen {
		t.Errorf("state = %s, want open", first.State)
	}
}

// testPeriodsMaterializeGaps covers a budget that goes unused for months: the
// intermediate periods must still exist, or carry would skip them.
func testPeriodsMaterializeGaps(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	def := monthly("research", "", dollars(1000))
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverCredit}
	mustPut(t, s, def)

	// Jump straight to the fourth month without ever touching the first three.
	p, err := s.EnsurePeriod(ctx, "research", base.AddDate(0, 3, 1))
	if err != nil {
		t.Fatal(err)
	}
	if p.Seq != 3 {
		t.Fatalf("period seq = %d, want 3", p.Seq)
	}

	periods, err := s.Periods(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 4 {
		t.Fatalf("materialized %d periods, want 4 (gaps must be filled)", len(periods))
	}
	for i, got := range periods {
		if got.Seq != i {
			t.Errorf("periods[%d].Seq = %d, want %d", i, got.Seq, i)
		}
	}
	// Three unused periods each carried their whole allocation forward.
	if want := dollars(3000); p.Envelope.Carry != want {
		t.Errorf("carry into period 3 = %s, want %s", p.Envelope.Carry, want)
	}
	if !p.CarryFinal {
		t.Error("carry must be final: every predecessor closed with nothing outstanding")
	}
}

// testPeriodSnapshotsDefinition is why periods store their own allocation: an
// edit must not retroactively change what a past period was allowed to spend.
func testPeriodSnapshotsDefinition(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))

	first, err := s.EnsurePeriod(ctx, "research", base)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDefinition(ctx, monthly("research", "", dollars(9000)), 1); err != nil {
		t.Fatal(err)
	}

	reloaded, err := s.Period(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Envelope.Allocation != dollars(1000) {
		t.Errorf("existing period allocation = %s, want the snapshot %s",
			reloaded.Envelope.Allocation, dollars(1000))
	}

	// Future periods pick up the new allocation.
	next, err := s.EnsurePeriod(ctx, "research", base.AddDate(0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if next.Envelope.Allocation != dollars(9000) {
		t.Errorf("new period allocation = %s, want the updated %s",
			next.Envelope.Allocation, dollars(9000))
	}
}

func testPeriodAdvancesToDraining(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))

	p, err := s.EnsurePeriod(ctx, "research", base)
	if err != nil {
		t.Fatal(err)
	}

	// A hold outstanding at the boundary keeps the period draining rather than
	// closing, so its charge can still land in the period that authorized it.
	mustReserve(t, s, "r1", "research", dollars(10), base.Add(time.Hour), unlimited("research"))

	after := p.Envelope.End.Add(time.Minute)
	if _, err := s.Advance(ctx, "research", after); err != nil {
		t.Fatal(err)
	}
	reloaded, err := s.Period(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.State != ledger.StateDraining {
		t.Errorf("state past the boundary with a hold outstanding = %s, want draining", reloaded.State)
	}
	// Draining says the closing balance is not yet known. CarryFinal is a
	// statement about carry-in, not about the balance, so it is not the flag to
	// check here.
	if reloaded.ClosingBalance != 0 || !reloaded.ClosedAt.IsZero() {
		t.Errorf("a draining period recorded a closing balance %s at %s; it is not closed yet",
			reloaded.ClosingBalance, reloaded.ClosedAt)
	}

	// It must still accept the settlement of the hold it authorized.
	mustSettle(t, s, "r1", dollars(10), after)
}

func testDrainedPeriodCloses(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))

	p, err := s.EnsurePeriod(ctx, "research", base)
	if err != nil {
		t.Fatal(err)
	}
	mustReserve(t, s, "r1", "research", dollars(100), base.Add(time.Hour), unlimited("research"))

	after := p.Envelope.End.Add(time.Minute)
	if _, err := s.Advance(ctx, "research", after); err != nil {
		t.Fatal(err)
	}

	// Settling the last outstanding hold drains the period, which then closes.
	mustSettle(t, s, "r1", dollars(80), after)
	if _, err := s.Advance(ctx, "research", after); err != nil {
		t.Fatal(err)
	}

	closed, err := s.Period(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != ledger.StateClosed {
		t.Fatalf("state = %s, want closed", closed.State)
	}
	// The charge landed in the authorizing period, so its balance reflects $80.
	if want := dollars(920); closed.ClosingBalance != want {
		t.Errorf("closing balance = %s, want %s", closed.ClosingBalance, want)
	}
	if !closed.CarryFinal {
		t.Error("a closed period's carry must be final")
	}
	if closed.ClosedAt.IsZero() {
		t.Error("ClosedAt must be recorded")
	}
}

func testCarryFlowsToNextPeriod(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	def := monthly("research", "", dollars(1000))
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance}
	mustPut(t, s, def)

	first, err := s.EnsurePeriod(ctx, "research", base)
	if err != nil {
		t.Fatal(err)
	}
	mustReserve(t, s, "r1", "research", dollars(400), base.Add(time.Hour), unlimited("research"))
	mustSettle(t, s, "r1", dollars(400), base.Add(2*time.Hour))

	// Cross the boundary. The first period has nothing outstanding, so it closes
	// and its $600 balance carries forward as a final figure.
	second, err := s.EnsurePeriod(ctx, "research", first.Envelope.End.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if want := dollars(600); second.Envelope.Carry != want {
		t.Errorf("carry = %s, want %s", second.Envelope.Carry, want)
	}
	if !second.CarryFinal {
		t.Error("carry should be final: the predecessor had nothing outstanding")
	}
	if want := dollars(1600); second.Envelope.Total() != want {
		t.Errorf("total = %s, want %s", second.Envelope.Total(), want)
	}
}

// testProvisionalCarryIsConservative exercises the drain-before-close rule: a
// successor starting while its predecessor drains must assume every outstanding
// hold spends in full, and may only be revised upward when the truth arrives.
func testProvisionalCarryIsConservative(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	def := monthly("research", "", dollars(1000))
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance}
	mustPut(t, s, def)

	first, err := s.EnsurePeriod(ctx, "research", base)
	if err != nil {
		t.Fatal(err)
	}
	// $200 settled, $300 still in flight when the boundary arrives.
	mustReserve(t, s, "settled", "research", dollars(200), base.Add(time.Hour), unlimited("research"))
	mustSettle(t, s, "settled", dollars(200), base.Add(2*time.Hour))
	mustReserve(t, s, "inflight", "research", dollars(300),
		first.Envelope.End.Add(-time.Minute), unlimited("research"))

	after := first.Envelope.End.Add(time.Minute)
	second, err := s.EnsurePeriod(ctx, "research", after)
	if err != nil {
		t.Fatal(err)
	}
	// Provisional carry assumes the in-flight hold spends in full: 1000-200-300.
	if want := dollars(500); second.Envelope.Carry != want {
		t.Errorf("provisional carry = %s, want %s", second.Envelope.Carry, want)
	}
	if second.CarryFinal {
		t.Error("carry must be marked provisional while the predecessor drains")
	}

	// The request comes in under its estimate, so the true balance is higher.
	mustSettle(t, s, "inflight", dollars(100), after)
	if _, err := s.Advance(ctx, "research", after); err != nil {
		t.Fatal(err)
	}

	final, err := s.Period(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := dollars(700); final.Envelope.Carry != want {
		t.Errorf("final carry = %s, want %s", final.Envelope.Carry, want)
	}
	if !final.CarryFinal {
		t.Error("carry must be final once the predecessor closed")
	}
	if final.Envelope.Carry < second.Envelope.Carry {
		t.Errorf("carry was revised downward (%s -> %s); a provisional carry must never overstate",
			second.Envelope.Carry, final.Envelope.Carry)
	}

	// The charge landed in the authorizing period, not in the new one.
	firstClosed, err := s.Period(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstClosed.State != ledger.StateClosed {
		t.Errorf("predecessor state = %s, want closed", firstClosed.State)
	}
	if want := dollars(700); firstClosed.ClosingBalance != want {
		t.Errorf("predecessor closing balance = %s, want %s", firstClosed.ClosingBalance, want)
	}
	assertTotals(t, s, "research", after, 0, 0) // the successor is untouched
}

// testMoneyIsConservedAcrossPeriods is the stated acceptance criterion for
// period transition: over several recurring periods, nothing is created or lost.
//
// The invariant checked is the one that matters for accounting: for every period,
// allocation + carry-in = spend + carry-out. Under balance rollover that chains,
// so total allocation granted equals total spend plus the final carry.
func testMoneyIsConservedAcrossPeriods(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	def := monthly("research", "", dollars(1000))
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance}
	mustPut(t, s, def)

	// Spend a different amount in each of six months, including one month that
	// overspends so debt has to flow too.
	spends := []int64{300, 1000, 1400, 0, 250, 900}
	for seq, amount := range spends {
		at := base.AddDate(0, seq, 0).Add(time.Hour)
		id := fmt.Sprintf("r%d", seq)
		mustReserve(t, s, id, "research", dollars(amount), at, unlimited("research"))
		mustSettle(t, s, id, dollars(amount), at.Add(time.Minute))
	}

	// Advance past the last period so every period but the current one closes.
	last := base.AddDate(0, len(spends)-1, 0).Add(time.Hour)
	if _, err := s.Advance(ctx, "research", last); err != nil {
		t.Fatal(err)
	}

	periods, err := s.Periods(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != len(spends) {
		t.Fatalf("materialized %d periods, want %d", len(periods), len(spends))
	}

	// Per-period conservation: what came in must equal what went out.
	var totalSpent money.Money
	for i, p := range periods {
		spent := totalsFor(t, s, "research", p.Envelope.Start.Add(time.Minute)).Spent
		if want := dollars(spends[i]); spent != want {
			t.Errorf("period %d spend = %s, want %s", i, spent, want)
		}
		totalSpent += spent

		if want := dollars(spends[i]); p.Envelope.Total()-want != p.Envelope.Close(want) {
			t.Errorf("period %d: Close disagrees with Total-spend", i)
		}
		// Carry-in of the successor must equal this period's closing balance,
		// because balance rollover is uncapped here.
		if i+1 < len(periods) {
			if p.State != ledger.StateClosed {
				t.Errorf("period %d state = %s, want closed", i, p.State)
			}
			if got, want := periods[i+1].Envelope.Carry, p.ClosingBalance; got != want {
				t.Errorf("carry into period %d = %s, want the predecessor's closing balance %s",
					i+1, got, want)
			}
		}
	}

	// Whole-run conservation: every dollar granted is either spent or still in
	// the current period's carry.
	granted := dollars(1000 * int64(len(spends)))
	current := periods[len(periods)-1]
	accountedFor := totalSpent + current.Envelope.Carry + current.Envelope.Allocation
	// carry-in of the last period + its allocation covers what remains; adding
	// spend from all previous periods must reconstruct the total grant.
	priorSpend := totalSpent - dollars(spends[len(spends)-1])
	if got := priorSpend + current.Envelope.Carry + current.Envelope.Allocation; got != granted {
		t.Errorf("prior spend %s + current carry %s + current allocation %s = %s, want the total grant %s",
			priorSpend, current.Envelope.Carry, current.Envelope.Allocation, got, granted)
	}
	_ = accountedFor
}

// testDebtCarriesUncapped is the product rule that debt must never be silently
// capped or forgiven while balance rollover is in use.
func testDebtCarriesUncapped(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	def := monthly("research", "", dollars(100))
	// A tight positive cap that must not apply to debt.
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance, Cap: dollars(10)}
	mustPut(t, s, def)

	// Overspend badly: a $500 actual against a $1 estimate.
	at := base.Add(time.Hour)
	mustReserve(t, s, "r0", "research", dollars(1), at, unlimited("research"))
	mustSettle(t, s, "r0", dollars(500), at.Add(time.Minute))

	next := base.AddDate(0, 1, 0).Add(time.Hour)
	second, err := s.EnsurePeriod(ctx, "research", next)
	if err != nil {
		t.Fatal(err)
	}
	if want := -dollars(400); second.Envelope.Carry != want {
		t.Errorf("carry = %s, want the full debt %s (a positive cap must not clamp debt)",
			second.Envelope.Carry, want)
	}
	if want := -dollars(300); second.Envelope.Total() != want {
		t.Errorf("total = %s, want %s: inherited debt exceeds the new allocation", second.Envelope.Total(), want)
	}
}

// testElapsedPeriodRefusesNewWork checks that a period whose window has passed
// never admits new spend, however the store chooses to signal it.
func testElapsedPeriodRefusesNewWork(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))

	first, err := s.EnsurePeriod(ctx, "research", base)
	if err != nil {
		t.Fatal(err)
	}
	after := first.Envelope.End.Add(time.Hour)
	if _, err := s.Advance(ctx, "research", after); err != nil {
		t.Fatal(err)
	}

	// A reservation timestamped inside the elapsed period must not be admitted
	// against it: that period was already closed out.
	inside := first.Envelope.Start.Add(time.Hour)
	r, err := s.Reserve(ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID: "late", BudgetID: "research", Amount: dollars(10), CreatedAt: inside,
		},
		Ceilings: unlimited("research"),
		Now:      inside,
	})
	if err != nil {
		// Refusing outright is a valid answer.
		return
	}
	for _, leg := range r.Legs {
		if leg.Scope.PeriodID == first.ID {
			t.Errorf("reservation was admitted against the elapsed period %q", first.ID)
		}
	}
}

// --- reservation basics ----------------------------------------------------

func testReserveAndSettle(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	r := mustReserve(t, s, "r1", "research", dollars(10), now, unlimited("research"))
	if r.State != ledger.StatePending {
		t.Errorf("state = %s, want pending", r.State)
	}
	if len(r.Legs) != 1 {
		t.Fatalf("legs = %+v, want exactly one", r.Legs)
	}
	assertTotals(t, s, "research", now, 0, dollars(10))

	c := mustSettle(t, s, "r1", dollars(7), now.Add(time.Second))
	if c.ActualCost != dollars(7) {
		t.Errorf("ActualCost = %s, want %s", c.ActualCost, dollars(7))
	}
	if c.ReservedCost != dollars(10) {
		t.Errorf("ReservedCost = %s, want %s", c.ReservedCost, dollars(10))
	}
	if c.Latency != time.Second {
		t.Errorf("Latency = %v, want 1s", c.Latency)
	}
	if c.Overrun() != 0 {
		t.Errorf("Overrun = %s, want 0: the request came in under its estimate", c.Overrun())
	}
	// The hold is gone and the actual cost is now spend.
	assertTotals(t, s, "research", now, dollars(7), 0)

	got, err := s.Get(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ledger.StateSettled {
		t.Errorf("state = %s, want settled", got.State)
	}
}

func testReserveRespectsCeiling(t *testing.T, s ledger.Ledger) {
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	mustReserve(t, s, "r1", "research", dollars(60), now, ceil("research", dollars(100)))
	_, err := reserve(t, s, "r2", "research", dollars(50), now, ceil("research", dollars(100)))
	assertScopeError(t, err, "research", ledger.ErrInsufficientHeadroom)
}

func testCeilingIsExact(t *testing.T, s ledger.Ledger) {
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	// Exactly at the ceiling is allowed.
	mustReserve(t, s, "exact", "research", dollars(100), now, ceil("research", dollars(100)))
	assertTotals(t, s, "research", now, 0, dollars(100))

	// One microdollar more is not.
	_, err := reserve(t, s, "over", "research", money.Money(1), now, ceil("research", dollars(100)))
	if !errors.Is(err, ledger.ErrInsufficientHeadroom) {
		t.Errorf("one microdollar over the ceiling: error = %v, want ErrInsufficientHeadroom", err)
	}
}

// testDeniedReserveWritesNothing matters because a refused hold that left a row
// behind would permanently consume headroom it was never granted.
func testDeniedReserveWritesNothing(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	if _, err := reserve(t, s, "denied", "research", dollars(500), now,
		ceil("research", dollars(100))); !errors.Is(err, ledger.ErrInsufficientHeadroom) {
		t.Fatalf("error = %v, want ErrInsufficientHeadroom", err)
	}
	assertTotals(t, s, "research", now, 0, 0)
	if _, err := s.Get(ctx, "denied"); !errors.Is(err, ledger.ErrReservationNotFound) {
		t.Errorf("Get after denial: error = %v, want ErrReservationNotFound", err)
	}
	// The ID must be reusable, since nothing was recorded under it.
	mustReserve(t, s, "denied", "research", dollars(10), now, ceil("research", dollars(100)))
}

func testDuplicateReservationRefused(t *testing.T, s ledger.Ledger) {
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	mustReserve(t, s, "r1", "research", dollars(10), now, unlimited("research"))
	if _, err := reserve(t, s, "r1", "research", dollars(10), now,
		unlimited("research")); !errors.Is(err, ledger.ErrDuplicateReservation) {
		t.Errorf("error = %v, want ErrDuplicateReservation", err)
	}
	// Caller-supplied IDs exist so a retry after an ambiguous failure does not
	// double-reserve.
	assertTotals(t, s, "research", now, 0, dollars(10))
}

// testMissingCeilingRefused stops a caller from escaping a constraint simply by
// forgetting to mention it.
func testMissingCeilingRefused(t *testing.T, s ledger.Ledger) {
	hierarchy(t, s)
	now := base.Add(time.Hour)

	// Ceilings for the leaf and its parent, but not for the root.
	_, err := reserve(t, s, "r1", "literature-agent", dollars(10), now,
		ceil("literature-agent", dollars(100), "agents", dollars(100)))
	assertScopeError(t, err, "research", ledger.ErrMissingCeiling)
	for _, id := range chainIDs {
		assertTotals(t, s, id, now, 0, 0)
	}
}

func testInvalidReserveRefused(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	bad := map[string]ledger.Reservation{
		"negative amount": {ID: "neg", BudgetID: "research", Amount: -dollars(1), CreatedAt: now},
		"no id":           {BudgetID: "research", Amount: dollars(1), CreatedAt: now},
		"no budget":       {ID: "nob", Amount: dollars(1), CreatedAt: now},
	}
	for name, res := range bad {
		_, err := s.Reserve(ctx, ledger.ReserveRequest{
			Reservation: res, Ceilings: unlimited("research"), Now: now,
		})
		if !errors.Is(err, ledger.ErrInvalidArgument) {
			t.Errorf("Reserve(%s): error = %v, want ErrInvalidArgument", name, err)
		}
	}
	assertTotals(t, s, "research", now, 0, 0)
}

func testZeroAmountAllowed(t *testing.T, s ledger.Ledger) {
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	// A zero-cost hold commits nothing but must still be recordable, so a caller
	// with no usable estimate can still settle a real cost later.
	mustReserve(t, s, "zero", "research", 0, now, ceil("research", 0))
	assertTotals(t, s, "research", now, 0, 0)

	mustSettle(t, s, "zero", dollars(5), now)
	assertTotals(t, s, "research", now, dollars(5), 0)
}

func testDoubleSettleRefused(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	mustReserve(t, s, "r1", "research", dollars(10), now, unlimited("research"))
	mustSettle(t, s, "r1", dollars(7), now)

	if _, err := s.Settle(ctx, ledger.Settlement{
		ReservationID: "r1", ActualCost: dollars(7), CompletedAt: now,
	}); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("error = %v, want ErrAlreadyResolved", err)
	}
	// The assertion that matters: the money was counted once.
	assertTotals(t, s, "research", now, dollars(7), 0)
}

func testReleaseAfterSettleRefused(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	mustReserve(t, s, "r1", "research", dollars(10), now, unlimited("research"))
	mustSettle(t, s, "r1", dollars(7), now)

	if err := s.Release(ctx, "r1"); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("error = %v, want ErrAlreadyResolved", err)
	}
	// A release must never erase a real charge.
	assertTotals(t, s, "research", now, dollars(7), 0)
}

func testSettleAfterReleaseRefused(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	mustReserve(t, s, "r1", "research", dollars(10), now, unlimited("research"))
	if err := s.Release(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settle(ctx, ledger.Settlement{
		ReservationID: "r1", ActualCost: dollars(7), CompletedAt: now,
	}); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("error = %v, want ErrAlreadyResolved", err)
	}
	assertTotals(t, s, "research", now, 0, 0)
}

func testReleaseFreesHeadroom(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	mustReserve(t, s, "r1", "research", dollars(90), now, ceil("research", dollars(100)))
	if _, err := reserve(t, s, "r2", "research", dollars(90), now,
		ceil("research", dollars(100))); !errors.Is(err, ledger.ErrInsufficientHeadroom) {
		t.Fatalf("error = %v, want ErrInsufficientHeadroom", err)
	}
	if err := s.Release(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	mustReserve(t, s, "r2", "research", dollars(90), now, ceil("research", dollars(100)))
	assertTotals(t, s, "research", now, 0, dollars(90))
}

func testOverrunIsRecorded(t *testing.T, s ledger.Ledger) {
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	mustReserve(t, s, "r1", "research", dollars(10), now, ceil("research", dollars(20)))
	// The real cost exceeds both the hold and the ceiling. Reality wins: hiding
	// the overrun would make the ledger disagree with the provider's bill.
	c := mustSettle(t, s, "r1", dollars(50), now)
	if want := dollars(40); c.Overrun() != want {
		t.Errorf("Overrun = %s, want %s", c.Overrun(), want)
	}
	assertTotals(t, s, "research", now, dollars(50), 0)
}

func testUnknownReservationIsNotFound(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	if _, err := s.Get(ctx, "ghost"); !errors.Is(err, ledger.ErrReservationNotFound) {
		t.Errorf("Get: error = %v, want ErrReservationNotFound", err)
	}
	if err := s.Release(ctx, "ghost"); !errors.Is(err, ledger.ErrReservationNotFound) {
		t.Errorf("Release: error = %v, want ErrReservationNotFound", err)
	}
	if _, err := s.Settle(ctx, ledger.Settlement{
		ReservationID: "ghost", ActualCost: dollars(1),
	}); !errors.Is(err, ledger.ErrReservationNotFound) {
		t.Errorf("Settle: error = %v, want ErrReservationNotFound", err)
	}
	if _, err := s.Renew(ctx, ledger.RenewRequest{
		ReservationID: "ghost", Now: base, Extend: time.Minute,
	}); !errors.Is(err, ledger.ErrReservationNotFound) {
		t.Errorf("Renew: error = %v, want ErrReservationNotFound", err)
	}
}

// --- leases ----------------------------------------------------------------

// testExpiredHoldFreesHeadroom is the abandoned-request criterion: headroom must
// stop being consumed even if recovery never runs.
func testExpiredHoldFreesHeadroom(t *testing.T, s ledger.Ledger) {
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "abandoned", "research", dollars(95), now, 5*time.Minute,
		ceil("research", dollars(100)))

	// Inside the lease the hold blocks new spend.
	if _, err := reserve(t, s, "blocked", "research", dollars(50), now.Add(time.Minute),
		ceil("research", dollars(100))); !errors.Is(err, ledger.ErrInsufficientHeadroom) {
		t.Errorf("inside the lease: error = %v, want ErrInsufficientHeadroom", err)
	}

	// Past the lease the headroom is free again, with no recovery call: a crashed
	// process must not be able to deadlock a budget.
	after := now.Add(10 * time.Minute)
	tot := totalsFor(t, s, "research", after)
	if tot.Reserved != 0 {
		t.Errorf("Reserved after expiry = %s, want 0", tot.Reserved)
	}
	if tot.ReservedExpired != dollars(95) {
		t.Errorf("ReservedExpired = %s, want %s", tot.ReservedExpired, dollars(95))
	}
	if tot.ExpiredCount != 1 {
		t.Errorf("ExpiredCount = %d, want 1", tot.ExpiredCount)
	}
	mustReserve(t, s, "after", "research", dollars(50), after, ceil("research", dollars(100)))
}

// testRenewPreventsExpiry is the acceptance criterion that a live long-running
// request keeps its headroom.
func testRenewPreventsExpiry(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "streaming", "research", dollars(95), now, 5*time.Minute,
		ceil("research", dollars(100)))

	// Renew repeatedly, as a live streaming request would, well past the original
	// deadline.
	at := now
	for i := range 10 {
		at = at.Add(4 * time.Minute)
		if _, err := s.Renew(ctx, ledger.RenewRequest{
			ReservationID: "streaming", Now: at, Extend: 5 * time.Minute,
		}); err != nil {
			t.Fatalf("renewal %d at +%v: %v", i, at.Sub(now), err)
		}
		if tot := totalsFor(t, s, "research", at); tot.Reserved != dollars(95) {
			t.Fatalf("after renewal %d: Reserved = %s, want the hold still counted at %s",
				i, tot.Reserved, dollars(95))
		}
	}

	// Forty minutes past a five-minute original lease, the hold still blocks spend.
	if _, err := reserve(t, s, "blocked", "research", dollars(50), at,
		ceil("research", dollars(100))); !errors.Is(err, ledger.ErrInsufficientHeadroom) {
		t.Errorf("a renewed hold stopped blocking spend: error = %v", err)
	}

	got, err := s.Get(ctx, "streaming")
	if err != nil {
		t.Fatal(err)
	}
	if got.RenewCount != 10 {
		t.Errorf("RenewCount = %d, want 10", got.RenewCount)
	}
	if !got.ExpiresAt.After(at) {
		t.Errorf("ExpiresAt = %s, want a deadline after %s", got.ExpiresAt, at)
	}
}

func testRenewUsesLeaseQuantum(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "r1", "research", dollars(10), now, 5*time.Minute, unlimited("research"))

	// Extend omitted: the reservation's own lease quantum applies.
	at := now.Add(time.Minute)
	got, err := s.Renew(ctx, ledger.RenewRequest{ReservationID: "r1", Now: at})
	if err != nil {
		t.Fatal(err)
	}
	if want := at.Add(5 * time.Minute); !got.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s (the lease quantum measured from now)", got.ExpiresAt, want)
	}
}

// testRenewAfterExpiryRefused: the headroom was already freed and may have been
// granted to another caller, so it cannot be silently reclaimed.
func testRenewAfterExpiryRefused(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "lapsed", "research", dollars(10), now, 5*time.Minute, unlimited("research"))

	late := now.Add(10 * time.Minute)
	if _, err := s.Renew(ctx, ledger.RenewRequest{
		ReservationID: "lapsed", Now: late, Extend: 5 * time.Minute,
	}); !errors.Is(err, ledger.ErrLeaseExpired) {
		t.Errorf("error = %v, want ErrLeaseExpired", err)
	}

	// Renewing an already-reclaimed hold fails the same way.
	if _, err := s.RecoverExpired(ctx, "research", late); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Renew(ctx, ledger.RenewRequest{
		ReservationID: "lapsed", Now: late, Extend: 5 * time.Minute,
	}); !errors.Is(err, ledger.ErrLeaseExpired) {
		t.Errorf("after recovery: error = %v, want ErrLeaseExpired", err)
	}
}

func testRenewAfterResolveRefused(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "settled", "research", dollars(10), now, time.Hour, unlimited("research"))
	mustSettle(t, s, "settled", dollars(10), now)
	if _, err := s.Renew(ctx, ledger.RenewRequest{
		ReservationID: "settled", Now: now, Extend: time.Hour,
	}); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("settled: error = %v, want ErrAlreadyResolved", err)
	}

	reserveWithLease(t, s, "released", "research", dollars(10), now, time.Hour, unlimited("research"))
	if err := s.Release(ctx, "released"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Renew(ctx, ledger.RenewRequest{
		ReservationID: "released", Now: now, Extend: time.Hour,
	}); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("released: error = %v, want ErrAlreadyResolved", err)
	}
}

// testRenewNeverMovesDeadlineBack guards against a short renewal shortening a
// lease, which would expose a live request to premature reclamation.
func testRenewNeverMovesDeadlineBack(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	r := reserveWithLease(t, s, "r1", "research", dollars(10), now, time.Hour, unlimited("research"))
	got, err := s.Renew(ctx, ledger.RenewRequest{
		ReservationID: "r1", Now: now.Add(time.Minute), Extend: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt.Before(r.ExpiresAt) {
		t.Errorf("ExpiresAt moved backward: %s -> %s", r.ExpiresAt, got.ExpiresAt)
	}
}

// testSettleAfterExpiryStillCharges: a slow-but-alive request really did cost
// money, so reality is recorded even though bookkeeping had given up on it.
func testSettleAfterExpiryStillCharges(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "slow", "research", dollars(10), now, 5*time.Minute, unlimited("research"))

	late := now.Add(time.Hour)
	if _, err := s.RecoverExpired(ctx, "research", late); err != nil {
		t.Fatal(err)
	}
	c, err := s.Settle(ctx, ledger.Settlement{
		ReservationID: "slow", ActualCost: dollars(12), CompletedAt: late,
	})
	if err != nil {
		t.Fatalf("settling an expired hold must still record the cost: %v", err)
	}
	if c.ActualCost != dollars(12) {
		t.Errorf("ActualCost = %s, want %s", c.ActualCost, dollars(12))
	}
	assertTotals(t, s, "research", late, dollars(12), 0)
}

func testZeroExpiryNeverExpires(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	// No ExpiresAt: the hold has no lease and never lapses.
	mustReserve(t, s, "eternal", "research", dollars(10), now, unlimited("research"))

	far := now.AddDate(0, 0, 5)
	tot := totalsFor(t, s, "research", far)
	if tot.Reserved != dollars(10) {
		t.Errorf("Reserved = %s, want the hold to persist at %s", tot.Reserved, dollars(10))
	}
	if tot.ExpiredCount != 0 {
		t.Errorf("ExpiredCount = %d, want 0", tot.ExpiredCount)
	}
	recovered, err := s.RecoverExpired(ctx, "research", far)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 0 {
		t.Errorf("recovered %d holds, want 0: a hold with no lease cannot expire", len(recovered))
	}
}

func testRecoverReclaimsAbandoned(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "gone", "research", dollars(50), now, 5*time.Minute, unlimited("research"))

	late := now.Add(time.Hour)
	recovered, err := s.RecoverExpired(ctx, "research", late)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != "gone" {
		t.Fatalf("recovered = %+v, want the abandoned hold", recovered)
	}
	if recovered[0].State != ledger.StateExpired {
		t.Errorf("state = %s, want expired", recovered[0].State)
	}
	// An expired hold is not spend: no usage was ever observed.
	assertTotals(t, s, "research", late, 0, 0)
}

func testRecoverIsIdempotent(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "gone", "research", dollars(50), now, time.Minute, unlimited("research"))
	late := now.Add(time.Hour)

	first, err := s.RecoverExpired(ctx, "research", late)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RecoverExpired(ctx, "research", late)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Errorf("first recovery reclaimed %d holds, want 1", len(first))
	}
	if len(second) != 0 {
		t.Errorf("second recovery reclaimed %d holds, want 0: recovery must be idempotent", len(second))
	}
}

// --- hierarchy -------------------------------------------------------------

// testChildReservesWholeChain is the core sub-budget behavior: a leaf request
// consumes its ancestors too.
func testChildReservesWholeChain(t *testing.T, s ledger.Ledger) {
	hierarchy(t, s)
	now := base.Add(time.Hour)

	r := mustReserve(t, s, "r1", "literature-agent", dollars(10), now,
		unlimited(chainIDs...))

	if len(r.Legs) != len(chainIDs) {
		t.Fatalf("legs = %+v, want one per budget in the chain", r.Legs)
	}
	for i, id := range chainIDs {
		leg := r.Legs[i]
		if leg.Scope.BudgetID != id {
			t.Errorf("legs[%d] = %q, want %q (nearest first)", i, leg.Scope.BudgetID, id)
		}
		if leg.Depth != i {
			t.Errorf("legs[%d].Depth = %d, want %d", i, leg.Depth, i)
		}
		if leg.Amount != dollars(10) {
			t.Errorf("legs[%d].Amount = %s, want %s", i, leg.Amount, dollars(10))
		}
		if leg.Scope.PeriodID == "" {
			t.Errorf("legs[%d] has no period: headroom is a property of a budget within a period", i)
		}
	}
	// Every ancestor sees the hold.
	for _, id := range chainIDs {
		assertTotals(t, s, id, now, 0, dollars(10))
	}
}

// testChildFailsWhenAncestorFull is a stated acceptance criterion.
func testChildFailsWhenAncestorFull(t *testing.T, s ledger.Ledger) {
	hierarchy(t, s)
	now := base.Add(time.Hour)

	// The leaf and its parent have room; the root does not.
	_, err := reserve(t, s, "r1", "literature-agent", dollars(50), now,
		ceil("literature-agent", dollars(100), "agents", dollars(100), "research", dollars(10)))
	assertScopeError(t, err, "research", ledger.ErrInsufficientHeadroom)

	// The middle budget can be the blocker too.
	_, err = reserve(t, s, "r2", "literature-agent", dollars(50), now,
		ceil("literature-agent", dollars(100), "agents", dollars(10), "research", dollars(100)))
	assertScopeError(t, err, "agents", ledger.ErrInsufficientHeadroom)

	// And with room everywhere, the same request succeeds.
	mustReserve(t, s, "r3", "literature-agent", dollars(50), now,
		ceil("literature-agent", dollars(100), "agents", dollars(100), "research", dollars(100)))
}

// testAncestorFailureWritesNothing is the atomicity requirement: either the
// whole chain reserves or none of it does.
func testAncestorFailureWritesNothing(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	hierarchy(t, s)
	now := base.Add(time.Hour)

	if _, err := reserve(t, s, "partial", "literature-agent", dollars(50), now,
		ceil("literature-agent", dollars(100), "agents", dollars(100), "research", dollars(10))); err == nil {
		t.Fatal("expected the reservation to be refused")
	}

	// No scope may hold anything, including the ones that had room.
	for _, id := range chainIDs {
		assertTotals(t, s, id, now, 0, 0)
	}
	if _, err := s.Get(ctx, "partial"); !errors.Is(err, ledger.ErrReservationNotFound) {
		t.Errorf("Get: error = %v, want ErrReservationNotFound", err)
	}
}

// testSettleUpdatesWholeChainOnce is a stated acceptance criterion.
func testSettleUpdatesWholeChainOnce(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	hierarchy(t, s)
	now := base.Add(time.Hour)

	mustReserve(t, s, "r1", "literature-agent", dollars(10), now, unlimited(chainIDs...))

	c := mustSettle(t, s, "r1", dollars(7), now.Add(time.Second))
	if len(c.Legs) != len(chainIDs) {
		t.Fatalf("charge legs = %+v, want one per budget in the chain", c.Legs)
	}
	for _, leg := range c.Legs {
		if leg.Amount != dollars(7) {
			t.Errorf("charge leg %s = %s, want the actual cost %s",
				leg.Scope.BudgetID, leg.Amount, dollars(7))
		}
	}
	// Exactly once in every scope: $7 spent, nothing held.
	for _, id := range chainIDs {
		assertTotals(t, s, id, now, dollars(7), 0)
	}

	// A replayed settle must not double-charge any scope.
	if _, err := s.Settle(ctx, ledger.Settlement{
		ReservationID: "r1", ActualCost: dollars(7), CompletedAt: now,
	}); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("replay: error = %v, want ErrAlreadyResolved", err)
	}
	for _, id := range chainIDs {
		assertTotals(t, s, id, now, dollars(7), 0)
	}
}

// testReleaseFreesWholeChainOnce is a stated acceptance criterion.
func testReleaseFreesWholeChainOnce(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	hierarchy(t, s)
	now := base.Add(time.Hour)

	tight := ceil("literature-agent", dollars(100), "agents", dollars(100), "research", dollars(100))
	mustReserve(t, s, "r1", "literature-agent", dollars(90), now, tight)

	if err := s.Release(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range chainIDs {
		assertTotals(t, s, id, now, 0, 0)
	}

	// Releasing twice must not "free" anything a second time.
	if err := s.Release(ctx, "r1"); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("second release: error = %v, want ErrAlreadyResolved", err)
	}

	// The freed headroom is genuinely reusable across the whole chain: if release
	// had double-freed, this would still pass, so also assert the totals after.
	mustReserve(t, s, "r2", "literature-agent", dollars(90), now, tight)
	for _, id := range chainIDs {
		assertTotals(t, s, id, now, 0, dollars(90))
	}
}

func testSiblingsShareParentHeadroom(t *testing.T, s ledger.Ledger) {
	mustPut(t, s,
		monthly("research", "", dollars(1000)),
		monthly("agents", "research", dollars(600)),
		monthly("coding", "research", dollars(600)),
	)
	now := base.Add(time.Hour)

	// Each child has its own room, but they share the parent's.
	ceilings := ceil("agents", dollars(100), "coding", dollars(100), "research", dollars(100))
	mustReserve(t, s, "a1", "agents", dollars(60), now, ceilings)

	// The sibling's own budget has room; the shared parent does not.
	_, err := reserve(t, s, "c1", "coding", dollars(60), now, ceilings)
	assertScopeError(t, err, "research", ledger.ErrInsufficientHeadroom)

	// Within the parent's remaining room, the sibling proceeds.
	mustReserve(t, s, "c2", "coding", dollars(40), now, ceilings)
	assertTotals(t, s, "research", now, 0, dollars(100))
	assertTotals(t, s, "agents", now, 0, dollars(60))
	assertTotals(t, s, "coding", now, 0, dollars(40))
}

// testParentSpendExcludesSiblings checks that rollup goes upward only: a child
// must not see its sibling's spend.
func testParentSpendExcludesSiblings(t *testing.T, s ledger.Ledger) {
	mustPut(t, s,
		monthly("research", "", dollars(1000)),
		monthly("agents", "research", dollars(600)),
		monthly("coding", "research", dollars(600)),
	)
	now := base.Add(time.Hour)

	mustReserve(t, s, "a1", "agents", dollars(60), now, unlimited("agents", "research"))
	mustSettle(t, s, "a1", dollars(60), now.Add(time.Minute))

	assertTotals(t, s, "agents", now, dollars(60), 0)
	assertTotals(t, s, "research", now, dollars(60), 0)
	assertTotals(t, s, "coding", now, 0, 0)
}

func testRecoverReclaimsChildViaParent(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	hierarchy(t, s)
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "child-hold", "literature-agent", dollars(50), now, time.Minute,
		unlimited(chainIDs...))

	// Recovering the root must reclaim holds created against its descendants;
	// otherwise an operator would have to know every leaf to clean up after a
	// crash.
	late := now.Add(time.Hour)
	recovered, err := s.RecoverExpired(ctx, "research", late)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != "child-hold" {
		t.Fatalf("recovered = %+v, want the child's hold", recovered)
	}
	for _, id := range chainIDs {
		assertTotals(t, s, id, late, 0, 0)
	}
}

// --- concurrency -----------------------------------------------------------

func testConcurrentReserveCeiling(t *testing.T, s ledger.Ledger) {
	mustPut(t, s, monthly("research", "", dollars(10000)))
	now := base.Add(time.Hour)

	const (
		workers = 32
		amount  = 10
		ceiling = 100
	)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := reserve(t, s, fmt.Sprintf("r%02d", i), "research", dollars(amount), now,
				ceil("research", dollars(ceiling)))
			switch {
			case err == nil:
				mu.Lock()
				granted++
				mu.Unlock()
			case errors.Is(err, ledger.ErrInsufficientHeadroom):
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if want := ceiling / amount; granted != want {
		t.Errorf("granted %d of %d requests, want exactly %d", granted, workers, want)
	}
	if tot := totalsFor(t, s, "research", now); tot.Reserved > dollars(ceiling) {
		t.Errorf("Reserved = %s, oversubscribing the ceiling %s", tot.Reserved, dollars(ceiling))
	}
}

// testConcurrentChildrenRespectParent is a stated acceptance criterion:
// concurrent child reservations must not oversubscribe a shared ancestor.
func testConcurrentChildrenRespectParent(t *testing.T, s ledger.Ledger) {
	mustPut(t, s,
		monthly("research", "", dollars(10000)),
		monthly("agents", "research", dollars(10000)),
		monthly("literature-agent", "agents", dollars(10000)),
		monthly("coding", "research", dollars(10000)),
	)
	now := base.Add(time.Hour)

	// Every child could individually afford far more than the root allows, so the
	// root is the only real constraint and the race is entirely on it.
	const (
		perChild = 20
		amount   = 10
		ceiling  = 100
	)
	children := []string{"literature-agent", "coding"}
	ceilings := ceil(
		"literature-agent", dollars(10000),
		"agents", dollars(10000),
		"coding", dollars(10000),
		"research", dollars(ceiling),
	)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for _, child := range children {
		for i := range perChild {
			wg.Add(1)
			go func(child string, i int) {
				defer wg.Done()
				_, err := reserve(t, s, fmt.Sprintf("%s-%02d", child, i), child, dollars(amount), now, ceilings)
				switch {
				case err == nil:
					mu.Lock()
					granted++
					mu.Unlock()
				case errors.Is(err, ledger.ErrInsufficientHeadroom):
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}(child, i)
		}
	}
	wg.Wait()

	if want := ceiling / amount; granted != want {
		t.Errorf("granted %d reservations across two subtrees, want exactly %d", granted, want)
	}
	root := totalsFor(t, s, "research", now)
	if root.Reserved > dollars(ceiling) {
		t.Errorf("root Reserved = %s, oversubscribing %s", root.Reserved, dollars(ceiling))
	}
	// Rollup must be exact: the children's holds sum to the root's.
	var sum money.Money
	for _, child := range children {
		sum += totalsFor(t, s, child, now).Reserved
	}
	if sum != root.Reserved {
		t.Errorf("children hold %s but the root holds %s; rollup must be exact", sum, root.Reserved)
	}
}

func testConcurrentSettleAndRelease(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	hierarchy(t, s)
	now := base.Add(time.Hour)

	const holds = 20
	for i := range holds {
		mustReserve(t, s, fmt.Sprintf("r%02d", i), "literature-agent", dollars(1), now,
			unlimited(chainIDs...))
	}

	// Race a settle against a release on each hold. Exactly one must win: two
	// winners would either double-count money or erase a real charge.
	var (
		wg                sync.WaitGroup
		mu                sync.Mutex
		settled, released int
	)
	for i := range holds {
		id := fmt.Sprintf("r%02d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			switch _, err := s.Settle(ctx, ledger.Settlement{
				ReservationID: id, ActualCost: dollars(1), CompletedAt: now,
			}); {
			case err == nil:
				mu.Lock()
				settled++
				mu.Unlock()
			case errors.Is(err, ledger.ErrAlreadyResolved):
			default:
				t.Errorf("settle %s: %v", id, err)
			}
		}()
		go func() {
			defer wg.Done()
			switch err := s.Release(ctx, id); {
			case err == nil:
				mu.Lock()
				released++
				mu.Unlock()
			case errors.Is(err, ledger.ErrAlreadyResolved):
			default:
				t.Errorf("release %s: %v", id, err)
			}
		}()
	}
	wg.Wait()

	if settled+released != holds {
		t.Errorf("%d settles + %d releases = %d, want exactly %d (one winner per hold)",
			settled, released, settled+released, holds)
	}
	// Spend must equal the number of settle winners, in every scope.
	for _, id := range chainIDs {
		tot := totalsFor(t, s, id, now)
		if want := dollars(int64(settled)); tot.Spent != want {
			t.Errorf("%s: Spent = %s, want %s", id, tot.Spent, want)
		}
		if tot.Reserved != 0 {
			t.Errorf("%s: Reserved = %s, want 0", id, tot.Reserved)
		}
	}
}

func testConcurrentRenewIsSafe(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	reserveWithLease(t, s, "r1", "research", dollars(10), now, 10*time.Minute, unlimited("research"))

	const renewals = 16
	var wg sync.WaitGroup
	for range renewals {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Renew(ctx, ledger.RenewRequest{
				ReservationID: "r1", Now: now.Add(time.Minute), Extend: 10 * time.Minute,
			}); err != nil {
				t.Errorf("Renew: %v", err)
			}
		}()
	}
	wg.Wait()

	got, err := s.Get(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RenewCount != renewals {
		t.Errorf("RenewCount = %d, want %d (no lost updates)", got.RenewCount, renewals)
	}
	// The hold still holds exactly its amount: renewal must not multiply money.
	assertTotals(t, s, "research", now.Add(time.Minute), 0, dollars(10))
}

// testConcurrentEnsurePeriod checks that racing processes cannot create
// duplicate periods, which would split one budget's spend across two rows.
func testConcurrentEnsurePeriod(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]bool{}
	)
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.EnsurePeriod(ctx, "research", base.Add(time.Duration(i)*time.Minute))
			if err != nil {
				t.Errorf("EnsurePeriod: %v", err)
				return
			}
			mu.Lock()
			ids[p.ID] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(ids) != 1 {
		t.Errorf("concurrent callers saw %d distinct periods, want 1: %v", len(ids), ids)
	}
	periods, err := s.Periods(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 1 {
		t.Errorf("materialized %d period rows, want 1", len(periods))
	}
}

// testConcurrentMixedLoad drives every operation at once against a hierarchy,
// which is where a store's locking either holds together or does not.
func testConcurrentMixedLoad(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	hierarchy(t, s)
	now := base.Add(time.Hour)
	ceilings := unlimited(chainIDs...)

	const workers = 24
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("w%02d", i)
			r, err := s.Reserve(ctx, ledger.ReserveRequest{
				Reservation: ledger.Reservation{
					ID: id, BudgetID: "literature-agent", RequestID: id,
					Amount: dollars(1), EstimatedCost: dollars(1),
					CreatedAt: now, ExpiresAt: now.Add(time.Hour), Lease: time.Hour,
				},
				Ceilings: ceilings,
				Now:      now,
			})
			if err != nil {
				t.Errorf("Reserve %s: %v", id, err)
				return
			}
			if _, err := s.Renew(ctx, ledger.RenewRequest{
				ReservationID: id, Now: now.Add(time.Minute), Extend: time.Hour,
			}); err != nil {
				t.Errorf("Renew %s: %v", id, err)
			}
			switch i % 3 {
			case 0:
				if _, err := s.Settle(ctx, ledger.Settlement{
					ReservationID: id, ActualCost: dollars(1), CompletedAt: now.Add(2 * time.Minute),
				}); err != nil {
					t.Errorf("Settle %s: %v", id, err)
				}
			case 1:
				if err := s.Release(ctx, id); err != nil {
					t.Errorf("Release %s: %v", id, err)
				}
			default:
				// Left outstanding on purpose, so live holds coexist with the rest.
			}
			if _, err := s.Totals(ctx, scopeOf(t, r, "research"), now); err != nil {
				t.Errorf("Totals %s: %v", id, err)
			}
		}()
	}
	wg.Wait()

	var wantSpent, wantHeld money.Money
	for i := range workers {
		switch i % 3 {
		case 0:
			wantSpent += dollars(1)
		case 1: // released: neither spent nor held
		default:
			wantHeld += dollars(1)
		}
	}
	for _, id := range chainIDs {
		assertTotals(t, s, id, now.Add(2*time.Minute), wantSpent, wantHeld)
	}
}

// --- arithmetic ------------------------------------------------------------

// testNoOverflowOnHugeAmounts checks that a store sums money without wrapping,
// which would turn an enormous spend into a negative balance.
func testNoOverflowOnHugeAmounts(t *testing.T, s ledger.Ledger) {
	mustPut(t, s, monthly("research", "", money.Max/4))
	now := base.Add(time.Hour)

	huge := money.Max / 4
	mustReserve(t, s, "h1", "research", huge, now, ceil("research", money.Max))
	mustSettle(t, s, "h1", huge, now)
	mustReserve(t, s, "h2", "research", huge, now, ceil("research", money.Max))

	tot := totalsFor(t, s, "research", now)
	if tot.Spent != huge || tot.Reserved != huge {
		t.Errorf("Spent/Reserved = %s/%s, want %s/%s", tot.Spent, tot.Reserved, huge, huge)
	}
	if tot.Committed() < 0 {
		t.Errorf("Committed = %s: money wrapped around", tot.Committed())
	}

	// A third hold of the maximum size must be refused rather than wrap.
	if _, err := reserve(t, s, "h3", "research", money.Max, now,
		ceil("research", money.Max)); !errors.Is(err, ledger.ErrInsufficientHeadroom) {
		t.Errorf("error = %v, want ErrInsufficientHeadroom rather than an overflow", err)
	}
}

// testMicrodollarPrecision guards against a float column silently rounding money.
func testMicrodollarPrecision(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", money.Max/2))
	now := base.Add(time.Hour)

	// A value float64 cannot represent exactly.
	odd := money.Money(9_007_199_254_740_993)
	mustReserve(t, s, "odd", "research", odd, now, ceil("research", money.Max))

	got, err := s.Get(ctx, "odd")
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != odd {
		t.Errorf("Amount round-tripped as %d, want %d", int64(got.Amount), int64(odd))
	}
	if c := mustSettle(t, s, "odd", odd, now); c.ActualCost != odd {
		t.Errorf("ActualCost = %d, want %d", int64(c.ActualCost), int64(odd))
	}
	if tot := totalsFor(t, s, "research", now); tot.Spent != odd {
		t.Errorf("Spent = %d, want %d", int64(tot.Spent), int64(odd))
	}
	_ = ctx
}

// --- reporting -------------------------------------------------------------

func testChargesAreScopedAndOrdered(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	hierarchy(t, s)
	now := base.Add(time.Hour)

	const n = 3
	for i := range n {
		id := fmt.Sprintf("r%d", i)
		mustReserve(t, s, id, "literature-agent", dollars(10), now, unlimited(chainIDs...))
		mustSettle(t, s, id, dollars(int64(i+1)), now.Add(time.Duration(i)*time.Minute))
	}

	leaf, err := s.EnsurePeriod(ctx, "literature-agent", now)
	if err != nil {
		t.Fatal(err)
	}
	leafScope := ledger.Scope{BudgetID: "literature-agent", PeriodID: leaf.ID}

	charges, err := s.Charges(ctx, leafScope, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(charges) != n {
		t.Fatalf("charges = %d, want %d", len(charges), n)
	}
	for i := 1; i < len(charges); i++ {
		if charges[i-1].OccurredAt.Before(charges[i].OccurredAt) {
			t.Errorf("charges are not newest-first at index %d", i)
		}
	}

	// A child's spend is the parent's spend, attributed to the parent's period.
	root, err := s.EnsurePeriod(ctx, "research", now)
	if err != nil {
		t.Fatal(err)
	}
	rootCharges, err := s.Charges(ctx, ledger.Scope{BudgetID: "research", PeriodID: root.ID},
		time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootCharges) != n {
		t.Errorf("root charges = %d, want %d", len(rootCharges), n)
	}

	limited, err := s.Charges(ctx, leafScope, time.Time{}, time.Time{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Errorf("limited charges = %d, want 2", len(limited))
	}
}

func testChargesWindowIsHalfOpen(t *testing.T, s ledger.Ledger) {
	ctx := context.Background()
	mustPut(t, s, monthly("research", "", dollars(1000)))
	now := base.Add(time.Hour)

	for i := range 3 {
		id := fmt.Sprintf("r%d", i)
		mustReserve(t, s, id, "research", dollars(1), now, unlimited("research"))
		mustSettle(t, s, id, dollars(1), now.Add(time.Duration(i)*time.Hour))
	}

	p, err := s.EnsurePeriod(ctx, "research", now)
	if err != nil {
		t.Fatal(err)
	}
	scope := ledger.Scope{BudgetID: "research", PeriodID: p.ID}

	// [now, now+2h) includes the charges at now and now+1h, but not now+2h.
	got, err := s.Charges(ctx, scope, now, now.Add(2*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("half-open window returned %d charges, want 2", len(got))
	}
}
