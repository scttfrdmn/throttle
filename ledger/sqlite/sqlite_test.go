package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/ledgertest"
	"github.com/scttfrdmn/throttle/money"
)

func dollars(d int64) money.Money { return money.Money(d) * money.PerDollar }

var base = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// monthly is the definition used by the store-specific tests. The conformance
// suite has its own copy; duplicating three lines is cheaper than exporting a
// test helper across packages.
func monthly(id, parent string, allocation money.Money) budget.Definition {
	return budget.Definition{
		ID:         id,
		ParentID:   parent,
		Allocation: allocation,
		Recurrence: budget.RecurMonthly,
		AnchorAt:   base,
	}
}

// newFileStore builds a store backed by a real file in the test's temp dir, so
// the tests exercise the same code path as production rather than a special
// in-memory mode.
func newFileStore(t *testing.T) *Store {
	t.Helper()
	return openAt(t, filepath.Join(t.TempDir(), "ledger.db"))
}

func openAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestConformance runs the ledger contract suite against both storage
// configurations. There is only one implementation of these semantics now, so the
// suite has to be the thing that pins them down; running it against the in-memory
// configuration as well catches anything that quietly depends on WAL or on a
// multi-connection pool.
func TestConformance(t *testing.T) {
	configs := map[string]func(*testing.T) ledger.Ledger{
		"file":   func(t *testing.T) ledger.Ledger { return newFileStore(t) },
		"memory": func(t *testing.T) ledger.Ledger { return openAt(t, ":memory:") },
	}
	for name, factory := range configs {
		t.Run(name, func(t *testing.T) {
			ledgertest.Run(t, factory)
		})
	}
}

// mustPut is the local equivalent of the conformance suite's helper.
func mustPut(t *testing.T, s *Store, defs ...budget.Definition) {
	t.Helper()
	for _, def := range defs {
		if err := s.PutDefinition(context.Background(), def); err != nil {
			t.Fatalf("PutDefinition(%q): %v", def.ID, err)
		}
	}
}

func reserve(t *testing.T, s *Store, id, budgetID string, amount money.Money,
	now time.Time, ceiling money.Money, lease time.Duration) error {
	t.Helper()
	chain, err := s.Chain(context.Background(), budgetID)
	if err != nil {
		return err
	}
	ceilings := map[string]money.Money{}
	for _, def := range chain {
		ceilings[def.ID] = ceiling
	}
	res := ledger.Reservation{
		ID: id, BudgetID: budgetID, RequestID: "req-" + id,
		Amount: amount, EstimatedCost: amount, CreatedAt: now,
	}
	if lease > 0 {
		res.ExpiresAt = now.Add(lease)
		res.Lease = lease
	}
	_, err = s.Reserve(context.Background(), ledger.ReserveRequest{
		Reservation: res, Ceilings: ceilings, Now: now,
	})
	return err
}

// scopeTotals reads a budget's totals for the period containing now.
func scopeTotals(t *testing.T, s *Store, budgetID string, now time.Time) ledger.Totals {
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

// TestDefinitionsSurviveReopen is a stated acceptance criterion: a budget
// definition must outlive the process that declared it, and a second process
// must not be able to install different rules for the same budget.
func TestDefinitionsSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	def := monthly("research", "", dollars(4000))
	def.Name = "Research"
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance, CapBasisPoints: 2500}
	child := monthly("agents", "research", dollars(1000))

	s := openAt(t, path)
	mustPut(t, s, def, child)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openAt(t, path)
	got, revision, err := reopened.Definition(ctx, "research")
	if err != nil {
		t.Fatalf("definition did not survive reopen: %v", err)
	}
	if revision != 1 {
		t.Errorf("revision = %d, want 1", revision)
	}
	if got.Fingerprint() != def.Fingerprint() {
		t.Errorf("definition changed across restart:\n got %+v\nwant %+v", got, def)
	}

	// The hierarchy survived too, so a request against the child still knows to
	// consume the parent.
	chain, err := reopened.Chain(ctx, "agents")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 || chain[0].ID != "agents" || chain[1].ID != "research" {
		t.Errorf("chain = %+v, want agents then research", chain)
	}

	// A second process starting up with different rules must be refused rather
	// than silently governing the same spend by its own numbers.
	conflicting := monthly("research", "", dollars(9000))
	if err := reopened.PutDefinition(ctx, conflicting); !errors.Is(err, ledger.ErrDefinitionConflict) {
		t.Errorf("error = %v, want ErrDefinitionConflict", err)
	}
	if again, _, _ := reopened.Definition(ctx, "research"); again.Allocation != dollars(4000) {
		t.Errorf("allocation = %s, want the original %s", again.Allocation, dollars(4000))
	}
}

// TestDurabilityAcrossReopen is the reason this store exists: spend must
// survive process restart.
func TestDurabilityAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	s := openAt(t, path)
	mustPut(t, s, monthly("research", "", dollars(1000)))
	if err := reserve(t, s, "a", "research", dollars(10), base, dollars(100), time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settle(ctx, ledger.Settlement{
		ReservationID: "a", ActualCost: dollars(7), CompletedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	// A second hold is left pending across the restart.
	if err := reserve(t, s, "b", "research", dollars(5), base, dollars(100), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openAt(t, path)
	tot := scopeTotals(t, reopened, "research", base)
	if tot.Spent != dollars(7) {
		t.Errorf("Spent after reopen = %s, want %s", tot.Spent, dollars(7))
	}
	if tot.Reserved != dollars(5) {
		t.Errorf("Reserved after reopen = %s, want %s", tot.Reserved, dollars(5))
	}

	// The settled reservation must still be resolved, so a replayed settle
	// cannot double-charge.
	if _, err := reopened.Settle(ctx, ledger.Settlement{
		ReservationID: "a", ActualCost: dollars(7), CompletedAt: base,
	}); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("re-settling after reopen: error = %v, want ErrAlreadyResolved", err)
	}
}

// TestPeriodStateSurvivesReopen checks that period transitions are durable
// rather than reconstructed in memory: a period closed before a restart must
// stay closed, and its carry must still be the one its successor is running on.
func TestPeriodStateSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	def := monthly("research", "", dollars(1000))
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance}

	s := openAt(t, path)
	mustPut(t, s, def)
	if err := reserve(t, s, "a", "research", dollars(400), base, money.Max, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Settle(ctx, ledger.Settlement{
		ReservationID: "a", ActualCost: dollars(400), CompletedAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	next := base.AddDate(0, 1, 0).Add(time.Hour)
	second, err := s.EnsurePeriod(ctx, "research", next)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openAt(t, path)
	periods, err := reopened.Periods(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 2 {
		t.Fatalf("periods = %d, want 2", len(periods))
	}
	if periods[0].State != ledger.StateClosed {
		t.Errorf("first period state = %s, want closed", periods[0].State)
	}
	if want := dollars(600); periods[0].ClosingBalance != want {
		t.Errorf("closing balance = %s, want %s", periods[0].ClosingBalance, want)
	}
	if periods[1].ID != second.ID {
		t.Errorf("second period = %q, want %q", periods[1].ID, second.ID)
	}
	if want := dollars(600); periods[1].Envelope.Carry != want {
		t.Errorf("carry after reopen = %s, want %s", periods[1].Envelope.Carry, want)
	}
	if !periods[1].CarryFinal {
		t.Error("carry must still be final after reopen")
	}
}

// TestCrashRecoveryReleasesStrandedHeadroom simulates a process dying with a
// hold outstanding: after expiry the budget must be usable again.
func TestCrashRecoveryReleasesStrandedHeadroom(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	s := openAt(t, path)
	mustPut(t, s, monthly("research", "", dollars(1000)))
	// Reserve nearly the whole ceiling, then "crash" without settling.
	if err := reserve(t, s, "stranded", "research", dollars(95), base, dollars(100), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	s.Close()

	reopened := openAt(t, path)

	// Immediately after restart the hold is still live and still blocks spend.
	if err := reserve(t, reopened, "next", "research", dollars(50), base.Add(time.Minute),
		dollars(100), 0); !errors.Is(err, ledger.ErrInsufficientHeadroom) {
		t.Errorf("before expiry: error = %v, want ErrInsufficientHeadroom", err)
	}

	// Once expired, recovery reclaims it and the budget is usable again.
	after := base.Add(10 * time.Minute)
	recovered, err := reopened.RecoverExpired(ctx, "research", after)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0].ID != "stranded" {
		t.Fatalf("recovered = %+v, want the stranded hold", recovered)
	}
	if err := reserve(t, reopened, "next", "research", dollars(50), after, dollars(100), 0); err != nil {
		t.Errorf("after recovery: %v", err)
	}
	// An expired hold is not spend: no usage was ever observed.
	if tot := scopeTotals(t, reopened, "research", after); tot.Spent != 0 {
		t.Errorf("Spent = %s, want 0 (an expired hold must not be charged)", tot.Spent)
	}
}

// TestCrossProcessReservationIsAtomic opens several independent Store handles to
// the same file, which is the closest in-process analogue of several throttle
// processes sharing a ledger. Their combined grants must not oversubscribe the
// ceiling.
func TestCrossProcessReservationIsAtomic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	const handles = 4
	stores := make([]*Store, handles)
	for i := range stores {
		stores[i] = openAt(t, path)
	}
	mustPut(t, stores[0], monthly("research", "", dollars(10000)))

	const (
		ceiling  = 100
		amount   = 10
		perStore = 15
	)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for i, s := range stores {
		for j := range perStore {
			wg.Add(1)
			go func(s *Store, id string) {
				defer wg.Done()
				_, err := s.Reserve(ctx, ledger.ReserveRequest{
					Reservation: ledger.Reservation{
						ID: id, BudgetID: "research", Amount: dollars(amount), CreatedAt: base,
					},
					Ceilings: map[string]money.Money{"research": dollars(ceiling)},
					Now:      base,
				})
				switch {
				case err == nil:
					mu.Lock()
					granted++
					mu.Unlock()
				case errors.Is(err, ledger.ErrInsufficientHeadroom):
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}(s, fmt.Sprintf("h%d-%02d", i, j))
		}
	}
	wg.Wait()

	if want := ceiling / amount; granted != want {
		t.Errorf("granted %d reservations across %d handles, want exactly %d", granted, handles, want)
	}
	if tot := scopeTotals(t, stores[0], "research", base); tot.Reserved > dollars(ceiling) {
		t.Errorf("Reserved = %s, oversubscribing ceiling %s", tot.Reserved, dollars(ceiling))
	}
}

// TestCrossProcessHierarchyIsAtomic is the same guarantee one level up: separate
// handles reserving against different children must not collectively oversubscribe
// the ancestor they share.
func TestCrossProcessHierarchyIsAtomic(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	writer := openAt(t, path)
	mustPut(t, writer,
		monthly("research", "", dollars(10000)),
		monthly("agents", "research", dollars(10000)),
		monthly("coding", "research", dollars(10000)),
	)

	const (
		handles = 4
		perPair = 12
		amount  = 10
		ceiling = 100
	)
	children := []string{"agents", "coding"}
	ceilings := map[string]money.Money{
		"agents":   dollars(10000),
		"coding":   dollars(10000),
		"research": dollars(ceiling),
	}

	stores := make([]*Store, handles)
	for i := range stores {
		stores[i] = openAt(t, path)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for i, s := range stores {
		for j := range perPair {
			child := children[j%len(children)]
			wg.Add(1)
			go func(s *Store, child, id string) {
				defer wg.Done()
				_, err := s.Reserve(ctx, ledger.ReserveRequest{
					Reservation: ledger.Reservation{
						ID: id, BudgetID: child, Amount: dollars(amount), CreatedAt: base,
					},
					Ceilings: ceilings,
					Now:      base,
				})
				switch {
				case err == nil:
					mu.Lock()
					granted++
					mu.Unlock()
				case errors.Is(err, ledger.ErrInsufficientHeadroom):
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}(s, child, fmt.Sprintf("h%d-%02d", i, j))
		}
	}
	wg.Wait()

	if want := ceiling / amount; granted != want {
		t.Errorf("granted %d reservations, want exactly %d", granted, want)
	}
	root := scopeTotals(t, stores[0], "research", base)
	if root.Reserved > dollars(ceiling) {
		t.Errorf("root Reserved = %s, oversubscribing %s", root.Reserved, dollars(ceiling))
	}
	var sum money.Money
	for _, child := range children {
		sum += scopeTotals(t, stores[0], child, base).Reserved
	}
	if sum != root.Reserved {
		t.Errorf("children hold %s but the root holds %s; rollup must be exact", sum, root.Reserved)
	}
}

// TestCrossProcessDefinitionRaceHasOneWinner checks that racing processes
// declaring the same budget converge on one definition rather than producing two
// rows or two sets of rules.
func TestCrossProcessDefinitionRaceHasOneWinner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	const handles = 6
	stores := make([]*Store, handles)
	for i := range stores {
		stores[i] = openAt(t, path)
	}

	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Add(1)
		go func(s *Store, i int) {
			defer wg.Done()
			def := monthly("research", "", dollars(1000))
			if i%2 == 1 {
				// Same semantics, different display name: must not be a conflict.
				def.Name = "Research"
			}
			if err := s.PutDefinition(ctx, def); err != nil && !errors.Is(err, ledger.ErrDefinitionConflict) {
				t.Errorf("PutDefinition: %v", err)
			}
		}(s, i)
	}
	wg.Wait()

	defs, err := stores[0].Definitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 {
		t.Fatalf("stored %d definitions, want 1: %+v", len(defs), defs)
	}
	if defs[0].Allocation != dollars(1000) {
		t.Errorf("allocation = %s, want %s", defs[0].Allocation, dollars(1000))
	}
}

// TestCrossProcessPeriodMaterializationIsUnique guards the UNIQUE (budget_id, seq)
// constraint: two handles crossing a boundary at the same moment must not split
// one period's spend across two rows.
func TestCrossProcessPeriodMaterializationIsUnique(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	writer := openAt(t, path)
	mustPut(t, writer, monthly("research", "", dollars(1000)))

	const handles = 6
	stores := make([]*Store, handles)
	for i := range stores {
		stores[i] = openAt(t, path)
	}

	at := base.AddDate(0, 3, 1) // three unmaterialized periods behind it
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]bool{}
	)
	for _, s := range stores {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			p, err := s.EnsurePeriod(ctx, "research", at)
			if err != nil {
				t.Errorf("EnsurePeriod: %v", err)
				return
			}
			mu.Lock()
			ids[p.ID] = true
			mu.Unlock()
		}(s)
	}
	wg.Wait()

	if len(ids) != 1 {
		t.Errorf("handles saw %d distinct periods, want 1: %v", len(ids), ids)
	}
	periods, err := stores[0].Periods(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 4 {
		t.Errorf("materialized %d periods, want 4", len(periods))
	}
	for i, p := range periods {
		if p.Seq != i {
			t.Errorf("periods[%d].Seq = %d, want %d", i, p.Seq, i)
		}
	}
}

// TestMicrodollarsSurviveRoundTrip guards against a REAL/float column type
// sneaking into the schema and rounding money.
func TestMicrodollarsSurviveRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)
	mustPut(t, s, monthly("research", "", money.Max/2))

	// A value that float64 cannot hold exactly.
	odd := money.Money(9_007_199_254_740_993)
	if err := reserve(t, s, "a", "research", odd, base, money.Max, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Amount != odd {
		t.Errorf("Amount round-tripped as %d, want %d", int64(got.Amount), int64(odd))
	}

	c, err := s.Settle(ctx, ledger.Settlement{ReservationID: "a", ActualCost: odd, CompletedAt: base})
	if err != nil {
		t.Fatal(err)
	}
	if c.ActualCost != odd {
		t.Errorf("ActualCost = %d, want %d", int64(c.ActualCost), int64(odd))
	}
	if tot := scopeTotals(t, s, "research", base); tot.Spent != odd {
		t.Errorf("Spent = %d, want %d", int64(tot.Spent), int64(odd))
	}
}

// TestSchemaRejectsBothRolloverCapForms checks that the mutually exclusive cap
// forms are enforced by the database, not only by validation in Go. The schema is
// the last line of defence if a future writer path forgets to validate.
func TestSchemaRejectsBothRolloverCapForms(t *testing.T) {
	s := newFileStore(t)
	_, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO budgets
		 (id, parent_id, name, allocation, borrow_ns, rollover_mode, rollover_cap,
		  rollover_cap_bp, recurrence, recurrence_ns, timezone, anchor_at, end_at,
		  fingerprint, revision, created_at, updated_at)
		 VALUES ('x', NULL, '', 1000, 0, 'credit', 100, 2500, 'monthly', 0, 'UTC', 0, 0, 'f', 1, 0, 0)`)
	if err == nil {
		t.Error("schema accepted both an absolute and a percentage rollover cap")
	}
}

func TestOpenRejectsBadPath(t *testing.T) {
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "no-such-dir", "x.db"))
	if err == nil {
		t.Fatal("expected an error opening a database in a missing directory")
	}
}
