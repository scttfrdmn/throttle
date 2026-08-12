package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/usage"
)

func dollars(d int64) money.Money { return money.Money(d) * money.PerDollar }

// start is the anchor for every test budget: the beginning of a 31-day month.
var start = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// fakeClock makes pacing deterministic. Time only moves when a test moves it, so
// whether a request is ahead of pace is reproducible rather than timing-dependent.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(at time.Time) *fakeClock { return &fakeClock{now: at} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// monthly is a recurring definition anchored at start.
func monthly(id, parent string, allocation money.Money) budget.Definition {
	return budget.Definition{
		ID:         id,
		ParentID:   parent,
		Allocation: allocation,
		Recurrence: budget.RecurMonthly,
		AnchorAt:   start,
	}
}

// newEngine builds an engine over a file-backed ledger holding one $3000/month
// budget named "research".
func newEngine(t *testing.T, mode Mode, at time.Time) (*Engine, *fakeClock) {
	t.Helper()
	eng, clock, _ := newEngineWith(t, mode, at, monthly("research", "", dollars(3000)))
	return eng, clock
}

// newEngineWith registers the given definitions, in order, so a child can name a
// parent declared earlier in the list.
func newEngineWith(t *testing.T, mode Mode, at time.Time, defs ...budget.Definition) (*Engine, *fakeClock, *sqlite.Store) {
	t.Helper()
	ctx := context.Background()

	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := newClock(at)
	eng, err := New(Config{
		Ledger: store,
		Clock:  clock.Now,
		Lease:  time.Hour,
		// A short poll keeps the bounded-re-evaluation tests fast without changing
		// what they prove.
		WaitPoll: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, def := range defs {
		if err := eng.Register(ctx, def, mode); err != nil {
			t.Fatalf("Register(%q): %v", def.ID, err)
		}
	}
	return eng, clock, store
}

// unpaced is a definition whose borrow window spans the whole period, so the
// paced allowance is the full envelope from the first instant.
//
// Tests about ceilings and contention use it so that the number under test is a
// round allocation rather than a prorated fraction of one. Pacing has its own
// tests; mixing the two only makes a contention test's arithmetic fragile.
func unpaced(id, parent string, allocation money.Money) budget.Definition {
	def := monthly(id, parent, allocation)
	def.Borrow = 62 * 24 * time.Hour
	return def
}

func estimate(cost money.Money) usage.Estimate {
	return usage.Estimate{Cost: usage.KnownCost(cost)}
}

// spent is the observed cost of a completed request. Tests that care only about
// budget arithmetic report a known cost; the unknown-cost path is exercised
// explicitly where it is the subject.
func spent(cost money.Money) usage.Actual { return usage.Actual{Cost: usage.KnownCost(cost)} }

// racingLedger steals headroom between the engine's advisory read and its
// reservation, which is the race the ledger's authority exists to settle. Doing it
// with an interceptor rather than real concurrency makes the recomputation
// deterministic.
type racingLedger struct {
	ledger.Ledger
	once   sync.Once
	amount money.Money
	at     time.Time
}

func (l *racingLedger) Reserve(ctx context.Context, req ledger.ReserveRequest) (ledger.Reservation, error) {
	l.once.Do(func() {
		// An unlimited ceiling: this stands in for a caller whose own advisory read
		// happened earlier, not for a caller escaping enforcement.
		ceilings := make(map[string]money.Money, len(req.Ceilings))
		for id := range req.Ceilings {
			ceilings[id] = money.Max
		}
		_, err := l.Ledger.Reserve(ctx, ledger.ReserveRequest{
			Reservation: ledger.Reservation{
				ID: "winner", BudgetID: req.BudgetID, Amount: l.amount, CreatedAt: l.at,
			},
			Ceilings: ceilings,
			Now:      l.at,
		})
		if err != nil {
			panic("racingLedger: setting up the race: " + err.Error())
		}
	})
	return l.Ledger.Reserve(ctx, req)
}

// newRacingEngine builds an engine whose first reservation is guaranteed to lose a
// race for amount against the given budget.
func newRacingEngine(t *testing.T, at time.Time, amount money.Money, def budget.Definition) *Engine {
	t.Helper()
	ctx := context.Background()

	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	eng, err := New(Config{
		Ledger: &racingLedger{Ledger: store, amount: amount, at: at},
		Clock:  newClock(at).Now,
		Lease:  time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Register(ctx, def, ModeEnforce); err != nil {
		t.Fatal(err)
	}
	return eng
}

// spend runs a whole transaction that settles at its estimate.
func spend(t *testing.T, eng *Engine, id, budgetID string, cost money.Money) ledger.Charge {
	t.Helper()
	ctx := context.Background()
	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: budgetID, RequestID: id, Estimate: estimate(cost),
	})
	if err != nil {
		t.Fatalf("Begin(%q): %v", id, err)
	}
	c, err := tx.Settle(ctx, spent(cost))
	if err != nil {
		t.Fatalf("Settle(%q): %v", id, err)
	}
	return c
}

func TestNewRequiresLedger(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected an error when no ledger is configured")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	store, err := sqlite.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for name, cfg := range map[string]Config{
		"negative lease":    {Ledger: store, Lease: -time.Second},
		"negative max wait": {Ledger: store, MaxWait: -time.Second},
		"negative poll":     {Ledger: store, WaitPoll: -time.Second},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRegisterValidatesDefinition(t *testing.T) {
	eng, _, _ := newEngineWith(t, ModeEnforce, start)
	ctx := context.Background()

	bad := monthly("broken", "", dollars(100))
	bad.AnchorAt = time.Time{}
	if err := eng.Register(ctx, bad, ModeEnforce); err == nil {
		t.Error("expected an invalid definition to be refused")
	}
	if err := eng.Register(ctx, monthly("ok", "", dollars(100)), "nonsense"); err == nil {
		t.Error("expected an unknown enforcement mode to be refused")
	}
}

// TestDefinitionsAreDurableAndUnambiguous is a stated acceptance criterion: a
// budget definition outlives the process that declared it, and two processes
// cannot silently govern the same money by different rules.
func TestDefinitionsAreDurableAndUnambiguous(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	def := monthly("research", "", dollars(3000))
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance, CapBasisPoints: 5000}

	first, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	engA, err := New(Config{Ledger: first, Clock: newClock(start).Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := engA.Register(ctx, def, ModeEnforce); err != nil {
		t.Fatal(err)
	}
	first.Close()

	// A second process comes up. Declaring the same rules is idempotent.
	second, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	engB, err := New(Config{Ledger: second, Clock: newClock(start).Now})
	if err != nil {
		t.Fatal(err)
	}
	if err := engB.Register(ctx, def, ModeEnforce); err != nil {
		t.Errorf("re-registering identical rules: %v", err)
	}

	// Declaring different rules is not.
	if err := engB.Register(ctx, monthly("research", "", dollars(9000)), ModeEnforce); !errors.Is(err, ledger.ErrDefinitionConflict) {
		t.Errorf("error = %v, want ErrDefinitionConflict", err)
	}

	got, revision, err := engB.Definition(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if revision != 1 {
		t.Errorf("revision = %d, want 1", revision)
	}
	if got.Fingerprint() != def.Fingerprint() {
		t.Error("the stored definition changed across processes")
	}
}

func TestUpdateRequiresRevision(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start)
	ctx := context.Background()

	raised := monthly("research", "", dollars(5000))
	if err := eng.Update(ctx, raised, 7, ModeWait); !errors.Is(err, ledger.ErrRevisionMismatch) {
		t.Fatalf("error = %v, want ErrRevisionMismatch", err)
	}
	if err := eng.Update(ctx, raised, 1, ModeWait); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, revision, err := eng.Definition(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if got.Allocation != dollars(5000) || revision != 2 {
		t.Errorf("after update: allocation %s revision %d, want %s and 2",
			got.Allocation, revision, dollars(5000))
	}
	if st, _ := eng.Status(ctx, "research"); st.Mode != ModeWait {
		t.Errorf("mode = %s, want wait", st.Mode)
	}
}

func TestUnknownBudget(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start)
	ctx := context.Background()

	if _, err := eng.Status(ctx, "nope"); !errors.Is(err, ErrBudgetNotFound) {
		t.Errorf("Status: error = %v, want ErrBudgetNotFound", err)
	}
	if _, err := eng.Check(ctx, "nope", dollars(1)); !errors.Is(err, ErrBudgetNotFound) {
		t.Errorf("Check: error = %v, want ErrBudgetNotFound", err)
	}
	if _, _, err := eng.Begin(ctx, Request{
		BudgetID: "nope", RequestID: "r", Estimate: estimate(dollars(1)),
	}); !errors.Is(err, ErrBudgetNotFound) {
		t.Errorf("Begin: error = %v, want ErrBudgetNotFound", err)
	}
}

// TestStatusAnswersMilestoneQuestions checks the headline numbers at a known
// point on the pacing curve: ten days into a 31-day month with nothing spent.
func TestStatusAnswersMilestoneQuestions(t *testing.T) {
	eng, clock := newEngine(t, ModeEnforce, start)
	ctx := context.Background()

	clock.Set(start.AddDate(0, 0, 10))
	st, err := eng.Status(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	s := st.Snapshot

	want := dollars(3000) * 10 / 31
	if s.Target != want {
		t.Errorf("Target = %s, want %s", s.Target, want)
	}
	if s.Bank != want {
		t.Errorf("Bank = %s, want %s: nothing spent, so the whole target is banked", s.Bank, want)
	}
	if s.AvailableNow != want {
		t.Errorf("AvailableNow = %s, want %s", s.AvailableNow, want)
	}
	if s.PeriodRemaining != dollars(3000) {
		t.Errorf("PeriodRemaining = %s, want the full %s", s.PeriodRemaining, dollars(3000))
	}
	if st.Period.State != ledger.StateOpen {
		t.Errorf("period state = %s, want open", st.Period.State)
	}
	if st.Period.Provisional() {
		t.Error("the first period's carry is final: nothing precedes it")
	}
	if st.ProjectedSpend != 0 {
		t.Errorf("ProjectedSpend = %s, want 0 with nothing spent", st.ProjectedSpend)
	}
}

func TestBorrowedStatusIsNegativeBank(t *testing.T) {
	eng, clock := newEngine(t, ModeMonitor, start)
	ctx := context.Background()

	// Spend the whole month's money on day one. Monitor mode admits it.
	clock.Set(start.Add(time.Hour))
	spend(t, eng, "big", "research", dollars(3000))

	clock.Set(start.AddDate(0, 0, 10))
	st, err := eng.Status(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if st.Snapshot.Bank >= 0 {
		t.Errorf("Bank = %s, want negative after spending far ahead of pace", st.Snapshot.Bank)
	}
	if st.Snapshot.PeriodRemaining != 0 {
		t.Errorf("PeriodRemaining = %s, want 0", st.Snapshot.PeriodRemaining)
	}
	if st.ProjectedSpend <= dollars(3000) {
		t.Errorf("ProjectedSpend = %s, want more than the allocation", st.ProjectedSpend)
	}
}

func TestBeginReserveExecuteReconcile(t *testing.T) {
	eng, clock := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	tx, dec, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "req-1", Estimate: estimate(dollars(10)),
		Identity: usage.ModelIdentity{Publisher: "anthropic", Family: "claude"},
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if dec.Outcome != budget.OutcomeAllow || !dec.Admitted {
		t.Fatalf("decision = %+v, want an admitted allow", dec)
	}
	if dec.BindingBudgetID != "research" {
		t.Errorf("BindingBudgetID = %q, want research", dec.BindingBudgetID)
	}

	res := tx.Reservation()
	if res.Amount != dollars(10) {
		t.Errorf("reserved %s, want %s", res.Amount, dollars(10))
	}
	if len(res.Legs) != 1 {
		t.Errorf("legs = %d, want 1 for a root budget", len(res.Legs))
	}
	if !res.ExpiresAt.Equal(clock.Now().Add(time.Hour)) {
		t.Errorf("ExpiresAt = %s, want a one-hour lease from now", res.ExpiresAt)
	}
	if res.Identity.Publisher != "anthropic" {
		t.Errorf("Identity = %+v, want the model identity preserved", res.Identity)
	}

	// While the hold is live it consumes headroom.
	if st, _ := eng.Status(ctx, "research"); st.Snapshot.Reserved != dollars(10) {
		t.Errorf("Reserved = %s, want %s", st.Snapshot.Reserved, dollars(10))
	}

	clock.Advance(time.Minute)
	c, err := tx.Settle(ctx, spent(dollars(9)))
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if c.ActualCost != dollars(9) || c.Overrun() != 0 {
		t.Errorf("charge = %+v, want $9 with no overrun", c)
	}

	st, _ := eng.Status(ctx, "research")
	if st.Snapshot.Spent != dollars(9) {
		t.Errorf("Spent = %s, want %s", st.Snapshot.Spent, dollars(9))
	}
	if st.Snapshot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0 after settlement", st.Snapshot.Reserved)
	}
}

func TestSettleRecordsOverrun(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "r", Estimate: estimate(dollars(10)),
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := tx.Settle(ctx, spent(dollars(25)))
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if c.Overrun() != dollars(15) {
		t.Errorf("Overrun = %s, want %s", c.Overrun(), dollars(15))
	}
	// Reality wins: the full actual cost is charged, not the estimate.
	if st, _ := eng.Status(ctx, "research"); st.Snapshot.Spent != dollars(25) {
		t.Errorf("Spent = %s, want the actual %s", st.Snapshot.Spent, dollars(25))
	}
}

func TestReleaseFreesHeadroom(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "r", Estimate: estimate(dollars(10)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	st, _ := eng.Status(ctx, "research")
	if st.Snapshot.Reserved != 0 || st.Snapshot.Spent != 0 {
		t.Errorf("after release: spent %s reserved %s, want both zero",
			st.Snapshot.Spent, st.Snapshot.Reserved)
	}
}

func TestDoubleResolveIsRefused(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "r", Estimate: estimate(dollars(10)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Settle(ctx, spent(dollars(10))); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Settle(ctx, spent(dollars(10))); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("second Settle: error = %v, want ErrAlreadyResolved", err)
	}
	if err := tx.Release(ctx); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("Release after Settle: error = %v, want ErrAlreadyResolved", err)
	}
	if err := tx.Renew(ctx); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Errorf("Renew after Settle: error = %v, want ErrAlreadyResolved", err)
	}
}

func TestDuplicateRequestIDIsRefused(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	req := Request{BudgetID: "research", RequestID: "same", Estimate: estimate(dollars(10))}
	if _, _, err := eng.Begin(ctx, req); err != nil {
		t.Fatal(err)
	}
	// The derived reservation ID makes a repeat a duplicate rather than a second
	// hold for the same call.
	_, dec, err := eng.Begin(ctx, req)
	if !errors.Is(err, ledger.ErrDuplicateReservation) {
		t.Fatalf("error = %v, want ErrDuplicateReservation", err)
	}
	// A duplicate is not a headroom verdict, so the advisory decision is unchanged.
	if dec.Outcome != budget.OutcomeAllow {
		t.Errorf("outcome = %s, want the unchanged allow", dec.Outcome)
	}
}

func TestBeginRejectsBadRequests(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start)
	ctx := context.Background()

	for name, req := range map[string]Request{
		"no budget":         {RequestID: "r", Estimate: estimate(dollars(1))},
		"no ids":            {BudgetID: "research", Estimate: estimate(dollars(1))},
		"negative estimate": {BudgetID: "research", RequestID: "r", Estimate: estimate(-dollars(1))},
	} {
		if _, _, err := eng.Begin(ctx, req); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestEnforceDeniesOverEnvelope(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	// More than the whole period's money can never fit: a deny, not a wait.
	_, dec, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "huge", Estimate: estimate(dollars(4000)),
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
	if dec.Outcome != budget.OutcomeDeny {
		t.Errorf("outcome = %s, want deny", dec.Outcome)
	}
	if dec.Admitted {
		t.Error("a denied request must not be admitted")
	}
}

func TestEnforceWaitsWhenAheadOfPace(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	// $2000 fits the month but not the tenth-of-the-month paced allowance.
	_, dec, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "ahead", Estimate: estimate(dollars(2000)),
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
	if dec.Outcome != budget.OutcomeWait {
		t.Fatalf("outcome = %s, want wait", dec.Outcome)
	}
	if !dec.RetryAt.After(dec.Snapshot.At) {
		t.Errorf("RetryAt %s is not in the future", dec.RetryAt)
	}
	if dec.Shortfall <= 0 {
		t.Errorf("Shortfall = %s, want a positive amount", dec.Shortfall)
	}
}

func TestMonitorModeNeverBlocks(t *testing.T) {
	eng, _ := newEngine(t, ModeMonitor, start.AddDate(0, 0, 10))
	ctx := context.Background()

	tx, dec, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "over", Estimate: estimate(dollars(99000)),
	})
	if err != nil {
		t.Fatalf("monitor mode must admit: %v", err)
	}
	if !dec.Admitted {
		t.Error("Admitted = false in monitor mode")
	}
	if dec.Outcome != budget.OutcomeDeny {
		t.Errorf("outcome = %s, want the honest deny reported alongside admission", dec.Outcome)
	}
	if _, err := tx.Settle(ctx, spent(dollars(99000))); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if st, _ := eng.Status(ctx, "research"); !st.Snapshot.Overspent() {
		t.Error("monitor mode must record the overspend rather than prevent it")
	}
}

func TestSetModeChangesPosture(t *testing.T) {
	eng, _ := newEngine(t, ModeMonitor, start.AddDate(0, 0, 10))
	ctx := context.Background()

	if err := eng.SetMode("research", ModeEnforce); err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "r", Estimate: estimate(dollars(9000)),
	}); !errors.Is(err, ErrDenied) {
		t.Errorf("error = %v, want ErrDenied after switching to enforce", err)
	}
	if err := eng.SetMode("research", "nonsense"); err == nil {
		t.Error("expected an unknown mode to be refused")
	}
}

// --- Hierarchy ---------------------------------------------------------------

// hierarchy is research $1000 -> agents $600 -> literature-agent $300, monthly.
func hierarchy(t *testing.T, mode Mode, at time.Time) (*Engine, *fakeClock) {
	t.Helper()
	eng, clock, _ := newEngineWith(t, mode, at,
		monthly("research", "", dollars(1000)),
		monthly("agents", "research", dollars(600)),
		monthly("literature-agent", "agents", dollars(300)),
	)
	return eng, clock
}

// TestChildRequestConsumesWholeChain is the acceptance criterion that spend
// against a leaf is spend against every ancestor.
func TestChildRequestConsumesWholeChain(t *testing.T) {
	eng, _ := hierarchy(t, ModeEnforce, start.AddDate(0, 0, 20))
	ctx := context.Background()
	ids := []string{"literature-agent", "agents", "research"}

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "literature-agent", RequestID: "r", Estimate: estimate(dollars(50)),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if len(tx.Reservation().Legs) != len(ids) {
		t.Fatalf("legs = %d, want one per budget in the chain", len(tx.Reservation().Legs))
	}
	for _, id := range ids {
		st, err := eng.Status(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if st.Snapshot.Reserved != dollars(50) {
			t.Errorf("%s Reserved = %s, want %s", id, st.Snapshot.Reserved, dollars(50))
		}
	}

	if _, err := tx.Settle(ctx, spent(dollars(40))); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		st, _ := eng.Status(ctx, id)
		if st.Snapshot.Spent != dollars(40) {
			t.Errorf("%s Spent = %s, want %s", id, st.Snapshot.Spent, dollars(40))
		}
		if st.Snapshot.Reserved != 0 {
			t.Errorf("%s Reserved = %s, want 0 after settlement", id, st.Snapshot.Reserved)
		}
	}
}

// TestReleaseFreesWholeChain is the same guarantee on the abandonment path.
func TestReleaseFreesWholeChain(t *testing.T) {
	eng, _ := hierarchy(t, ModeEnforce, start.AddDate(0, 0, 20))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "literature-agent", RequestID: "r", Estimate: estimate(dollars(50)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Release(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"literature-agent", "agents", "research"} {
		st, _ := eng.Status(ctx, id)
		if st.Snapshot.Committed != 0 {
			t.Errorf("%s Committed = %s, want 0 after release", id, st.Snapshot.Committed)
		}
	}
}

// TestAncestorLimitBindsTheChild is the difference between "you are out of money"
// and "your parent is".
func TestAncestorLimitBindsTheChild(t *testing.T) {
	ctx := context.Background()
	// The child has more allocation than its parent, so the parent binds first.
	eng, _, _ := newEngineWith(t, ModeEnforce, start.AddDate(0, 0, 10),
		unpaced("research", "", dollars(100)),
		unpaced("agents", "research", dollars(1000)),
	)

	_, dec, err := eng.Begin(ctx, Request{
		BudgetID: "agents", RequestID: "r", Estimate: estimate(dollars(500)),
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
	if dec.BindingBudgetID != "research" {
		t.Errorf("BindingBudgetID = %q, want research", dec.BindingBudgetID)
	}
	if dec.Outcome != budget.OutcomeDeny {
		t.Errorf("outcome = %s, want deny: $500 exceeds the parent's whole period", dec.Outcome)
	}
	if !strings.Contains(dec.Reason, `parent budget "research"`) {
		t.Errorf("Reason = %q, want it to name the limiting parent", dec.Reason)
	}

	// Nothing was written: a chain reserves entirely or not at all.
	for _, id := range []string{"agents", "research"} {
		st, _ := eng.Status(ctx, id)
		if st.Snapshot.Reserved != 0 {
			t.Errorf("%s Reserved = %s, want 0 after a refused chain", id, st.Snapshot.Reserved)
		}
	}
}

// TestStrictestModeInChainWins: a monitored child inside an enforced parent is
// still spending the parent's real money.
func TestStrictestModeInChainWins(t *testing.T) {
	ctx := context.Background()
	eng, _, _ := newEngineWith(t, ModeEnforce, start.AddDate(0, 0, 10),
		unpaced("research", "", dollars(100)))
	if err := eng.Register(ctx, unpaced("agents", "research", dollars(1000)), ModeMonitor); err != nil {
		t.Fatal(err)
	}

	_, dec, err := eng.Begin(ctx, Request{
		BudgetID: "agents", RequestID: "r", Estimate: estimate(dollars(500)),
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want the enforced parent to deny a monitored child", err)
	}
	if dec.Mode != ModeEnforce {
		t.Errorf("Mode = %s, want enforce (the strictest in the chain)", dec.Mode)
	}
}

// TestMonitoredAncestorDoesNotCapTheChild is the same rule from the other side: a
// monitored budget must observe overspend, so its ceiling cannot bind.
func TestMonitoredAncestorDoesNotCapTheChild(t *testing.T) {
	ctx := context.Background()
	eng, _, _ := newEngineWith(t, ModeMonitor, start.AddDate(0, 0, 10),
		unpaced("research", "", dollars(100)))
	if err := eng.Register(ctx, unpaced("agents", "research", dollars(1000)), ModeEnforce); err != nil {
		t.Fatal(err)
	}

	tx, dec, err := eng.Begin(ctx, Request{
		BudgetID: "agents", RequestID: "r", Estimate: estimate(dollars(500)),
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if dec.Mode != ModeEnforce {
		t.Errorf("Mode = %s, want enforce", dec.Mode)
	}
	// The monitored ancestor still records the spend.
	if _, err := tx.Settle(ctx, spent(dollars(500))); err != nil {
		t.Fatal(err)
	}
	if st, _ := eng.Status(ctx, "research"); st.Snapshot.Spent != dollars(500) {
		t.Errorf("monitored ancestor Spent = %s, want %s", st.Snapshot.Spent, dollars(500))
	}
}

func TestStatusChainReportsEveryAncestor(t *testing.T) {
	eng, _ := hierarchy(t, ModeEnforce, start.AddDate(0, 0, 20))
	ctx := context.Background()

	chain, err := eng.StatusChain(ctx, "literature-agent")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"literature-agent", "agents", "research"}
	if len(chain) != len(want) {
		t.Fatalf("chain length = %d, want %d", len(chain), len(want))
	}
	for i, id := range want {
		if chain[i].BudgetID != id {
			t.Errorf("chain[%d] = %q, want %q (nearest first)", i, chain[i].BudgetID, id)
		}
	}
}

func TestCheckChainExplainsTheDecision(t *testing.T) {
	eng, _ := hierarchy(t, ModeEnforce, start.AddDate(0, 0, 20))
	ctx := context.Background()

	dec, links, err := eng.CheckChain(ctx, "literature-agent", dollars(400))
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 3 {
		t.Fatalf("links = %d, want 3", len(links))
	}
	// $400 exceeds the leaf's whole $300 allocation, so the leaf itself denies.
	if dec.BindingBudgetID != "literature-agent" || dec.Outcome != budget.OutcomeDeny {
		t.Errorf("decision = %+v, want the leaf to deny", dec)
	}
	if links[0].Decision.Outcome != budget.OutcomeDeny {
		t.Errorf("leaf link outcome = %s, want deny", links[0].Decision.Outcome)
	}
	if links[2].Decision.Outcome == budget.OutcomeDeny {
		t.Errorf("root link outcome = %s, want better than deny: the root has room",
			links[2].Decision.Outcome)
	}
}

// TestConcurrentChildrenCannotOversubscribeParent is the acceptance criterion that
// the hierarchy holds under contention.
func TestConcurrentChildrenCannotOversubscribeParent(t *testing.T) {
	ctx := context.Background()
	// The root allows $100 for the whole period; both children allow far more, so
	// only the root can bind.
	eng, _, _ := newEngineWith(t, ModeEnforce, start.AddDate(0, 0, 10),
		unpaced("research", "", dollars(100)),
		unpaced("agents", "research", dollars(10000)),
		unpaced("coding", "research", dollars(10000)),
	)

	const (
		workers = 24
		amount  = 10
	)
	children := []string{"agents", "coding"}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := eng.Begin(ctx, Request{
				BudgetID:  children[i%len(children)],
				RequestID: fmt.Sprintf("r%02d", i),
				Estimate:  estimate(dollars(amount)),
			})
			switch {
			case err == nil:
				mu.Lock()
				granted++
				mu.Unlock()
			case errors.Is(err, ErrDenied):
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if want := 100 / amount; granted != want {
		t.Errorf("granted %d, want exactly %d", granted, want)
	}
	root, _ := eng.Status(ctx, "research")
	if root.Snapshot.Reserved > dollars(100) {
		t.Errorf("root Reserved = %s, oversubscribing %s", root.Snapshot.Reserved, dollars(100))
	}
	var sum money.Money
	for _, child := range children {
		st, _ := eng.Status(ctx, child)
		sum += st.Snapshot.Reserved
	}
	if sum != root.Snapshot.Reserved {
		t.Errorf("children hold %s but the root holds %s; the rollup must be exact",
			sum, root.Snapshot.Reserved)
	}
}

func TestConcurrentRequestsCannotOversubscribe(t *testing.T) {
	ctx := context.Background()
	eng, _, _ := newEngineWith(t, ModeEnforce, start.AddDate(0, 0, 10),
		unpaced("research", "", dollars(3000)))

	const (
		workers = 40
		amount  = 100
	)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := eng.Begin(ctx, Request{
				BudgetID: "research", RequestID: fmt.Sprintf("r%02d", i),
				Estimate: estimate(dollars(amount)),
			})
			switch {
			case err == nil:
				mu.Lock()
				granted++
				mu.Unlock()
			case errors.Is(err, ErrDenied):
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if want := 3000 / amount; granted != want {
		t.Errorf("granted %d, want exactly %d", granted, want)
	}
}

// TestLostRaceIsNotAlwaysAWait covers the rule that the engine's admission
// calculation is advisory: when the ledger refuses a hold the engine expected to
// succeed, the outcome is recomputed from refreshed state rather than assumed.
//
// Here the refreshed state shows the request can never fit the period, so the
// honest answer is deny -- not a wait that would never end.
func TestLostRaceIsNotAlwaysAWait(t *testing.T) {
	ctx := context.Background()
	// The whole period's money is $100 and the race takes $60 of it.
	eng := newRacingEngine(t, start.AddDate(0, 0, 10), dollars(60),
		unpaced("research", "", dollars(100)))

	_, dec, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "loser", Estimate: estimate(dollars(60)),
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
	if dec.Outcome != budget.OutcomeDeny {
		t.Errorf("outcome = %s, want deny: only $40 of the period's $100 remains", dec.Outcome)
	}
	if dec.Admitted {
		t.Error("a refused reservation must not report itself admitted")
	}
}

// TestLostRaceThatCanStillFitReportsWait is the other half of the same rule: when
// the refreshed state shows the request could still be admitted later, the caller
// is told to retry rather than told no.
func TestLostRaceThatCanStillFitReportsWait(t *testing.T) {
	ctx := context.Background()
	// Mid-period with pacing in force, so consuming the current allowance leaves
	// room later in the period. The paced allowance at day 15 of 31 is $1500.
	at := start.AddDate(0, 0, 15)
	eng := newRacingEngine(t, at, dollars(3100)*15/31, monthly("research", "", dollars(3100)))

	_, dec, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "loser", Estimate: estimate(dollars(100)),
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
	if dec.Outcome != budget.OutcomeWait {
		t.Errorf("outcome = %s, want wait: $100 still fits the period", dec.Outcome)
	}
	if dec.Admitted {
		t.Error("a refused reservation must not report itself admitted")
	}
	if dec.RetryAt.IsZero() {
		t.Error("a wait outcome must say when to retry")
	}
}

// --- Leases ------------------------------------------------------------------

// TestRenewPreventsExpiry: a live request extends its lease rather than losing its
// headroom to recovery.
func TestRenewPreventsExpiry(t *testing.T) {
	eng, clock := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "long", Estimate: estimate(dollars(10)),
	})
	if err != nil {
		t.Fatal(err)
	}
	first := tx.Reservation().ExpiresAt

	// Half a lease in, the request is still running and renews.
	clock.Advance(30 * time.Minute)
	if err := tx.Renew(ctx); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	renewed := tx.Reservation()
	if !renewed.ExpiresAt.After(first) {
		t.Errorf("ExpiresAt = %s, want later than %s", renewed.ExpiresAt, first)
	}
	if renewed.RenewCount != 1 {
		t.Errorf("RenewCount = %d, want 1", renewed.RenewCount)
	}

	// Past the original deadline, recovery must not reclaim it.
	clock.Advance(45 * time.Minute)
	recovered, err := eng.Recover(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 0 {
		t.Errorf("recovered %d holds, want none: the lease was renewed", len(recovered))
	}
	if st, _ := eng.Status(ctx, "research"); st.Snapshot.Reserved != dollars(10) {
		t.Errorf("Reserved = %s, want the hold still live", st.Snapshot.Reserved)
	}

	// And it still settles normally.
	if _, err := tx.Settle(ctx, spent(dollars(10))); err != nil {
		t.Fatalf("Settle after renewal: %v", err)
	}
}

// TestAbandonedHoldStopsConsumingHeadroom is the counterpart: a process that dies
// without renewing must not strand money.
func TestAbandonedHoldStopsConsumingHeadroom(t *testing.T) {
	eng, clock, _ := newEngineWith(t, ModeEnforce, start.AddDate(0, 0, 10),
		unpaced("research", "", dollars(3000)))
	ctx := context.Background()

	if _, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "abandoned", Estimate: estimate(dollars(3000)),
	}); err != nil {
		t.Fatal(err)
	}
	// Full: nothing else fits while the hold is live.
	if _, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "blocked", Estimate: estimate(dollars(10)),
	}); !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied while the hold is live", err)
	}

	// Past the lease the hold stops counting even before recovery runs. That is
	// what makes recovery a cleanup task rather than a correctness dependency.
	clock.Advance(2 * time.Hour)
	st, _ := eng.Status(ctx, "research")
	if st.Snapshot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0 once the lease lapsed", st.Snapshot.Reserved)
	}
	if st.ExpiredCount != 1 || st.ReservedExpired != dollars(3000) {
		t.Errorf("expired holds = %d holding %s, want 1 holding %s",
			st.ExpiredCount, st.ReservedExpired, dollars(3000))
	}
	if _, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "after", Estimate: estimate(dollars(10)),
	}); err != nil {
		t.Errorf("after the lease lapsed: %v", err)
	}
}

func TestRenewAfterExpiryIsRefusedButSettlementIsNot(t *testing.T) {
	eng, clock := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "gone", Estimate: estimate(dollars(10)),
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	if err := tx.Renew(ctx); !errors.Is(err, ledger.ErrLeaseExpired) {
		t.Errorf("Renew: error = %v, want ErrLeaseExpired", err)
	}
	// An expired hold whose request finished anyway must still charge reality.
	c, err := tx.Settle(ctx, spent(dollars(12)))
	if err != nil {
		t.Fatalf("Settle after expiry: %v", err)
	}
	if c.ActualCost != dollars(12) {
		t.Errorf("ActualCost = %s, want %s", c.ActualCost, dollars(12))
	}
	if st, _ := eng.Status(ctx, "research"); st.Snapshot.Spent != dollars(12) {
		t.Errorf("Spent = %s, want %s", st.Snapshot.Spent, dollars(12))
	}
}

func TestKeepAliveRenewsUntilResolved(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// A real clock here: KeepAlive is about wall-clock tickers. The lease is long
	// enough that nothing depends on how fast the test runs.
	eng, err := New(Config{Ledger: store, Lease: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	def := monthly("research", "", dollars(3000))
	def.AnchorAt = eng.Now().Add(-time.Hour)
	if err := eng.Register(ctx, def, ModeEnforce); err != nil {
		t.Fatal(err)
	}

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "streaming", Estimate: estimate(dollars(1)),
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- tx.KeepAlive(ctx, time.Millisecond) }()

	// Let it renew a few times, then finish the request.
	time.Sleep(50 * time.Millisecond)
	if _, err := tx.Settle(ctx, spent(dollars(1))); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("KeepAlive returned %v, want nil once the request resolved", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("KeepAlive did not stop after the transaction resolved")
	}
	if got, err := store.Get(ctx, tx.Reservation().ID); err != nil {
		t.Fatal(err)
	} else if got.RenewCount == 0 {
		t.Error("KeepAlive never renewed the lease")
	}
}

func TestKeepAliveRejectsBadInterval(t *testing.T) {
	eng, _ := newEngine(t, ModeEnforce, start.AddDate(0, 0, 10))
	ctx := context.Background()

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "r", Estimate: estimate(dollars(1)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.KeepAlive(ctx, 0); err == nil {
		t.Error("expected a non-positive interval to be refused")
	}
}

func TestRecoverAllCoversTheWholeForestOnce(t *testing.T) {
	eng, clock := hierarchy(t, ModeEnforce, start.AddDate(0, 0, 20))
	ctx := context.Background()

	for i, id := range []string{"research", "agents", "literature-agent"} {
		if _, _, err := eng.Begin(ctx, Request{
			BudgetID: id, RequestID: fmt.Sprintf("r%d", i), Estimate: estimate(dollars(5)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	clock.Advance(2 * time.Hour)

	recovered, err := eng.RecoverAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Three holds, each reported once even though the leaf's hold is reachable
	// through two ancestors.
	if len(recovered) != 3 {
		t.Errorf("recovered %d holds, want 3: %+v", len(recovered), recovered)
	}
	seen := map[string]bool{}
	for _, r := range recovered {
		if seen[r.ID] {
			t.Errorf("%s reported twice", r.ID)
		}
		seen[r.ID] = true
	}
}

// --- Waiting -----------------------------------------------------------------

func TestWaitReturnsImmediatelyWhenAffordable(t *testing.T) {
	eng, _ := newEngine(t, ModeWait, start.AddDate(0, 0, 10))
	ctx := context.Background()

	began := time.Now()
	if err := eng.Wait(ctx, "research", dollars(10)); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Errorf("Wait blocked for %s on an affordable request", elapsed)
	}
}

func TestWaitRefusesImpossibleRequest(t *testing.T) {
	eng, _ := newEngine(t, ModeWait, start.AddDate(0, 0, 10))
	ctx := context.Background()

	// More than the period's whole envelope: waiting cannot help.
	if err := eng.Wait(ctx, "research", dollars(9000)); !errors.Is(err, ErrDenied) {
		t.Errorf("error = %v, want ErrDenied", err)
	}
}

// TestWaitProceedsWhenPacingCatchesUp is the baseline: the waiter is released by
// the passage of time.
func TestWaitProceedsWhenPacingCatchesUp(t *testing.T) {
	eng, clock := newEngine(t, ModeWait, start.Add(time.Hour))
	ctx := context.Background()

	// $500 does not fit an hour into the month but fits the period.
	dec, err := eng.Check(ctx, "research", dollars(500))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Outcome != budget.OutcomeWait {
		t.Fatalf("outcome = %s, want wait", dec.Outcome)
	}

	done := make(chan error, 1)
	go func() { done <- eng.Wait(ctx, "research", dollars(500)) }()

	// Advance the fake clock past the point where pacing affords it.
	time.Sleep(10 * time.Millisecond)
	clock.Set(dec.RetryAt.Add(time.Minute))

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Wait returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the clock passed RetryAt")
	}
}

// TestWaitObservesHeadroomFreedBeforeRetryAt is the acceptance criterion for
// bounded re-evaluation: a waiter must be able to proceed because someone
// released, not only because time passed.
func TestWaitObservesHeadroomFreedBeforeRetryAt(t *testing.T) {
	ctx := context.Background()
	// Deep into the period so the paced allowance is nearly the whole allocation
	// and the only thing blocking the waiter is the outstanding hold.
	eng, clock, _ := newEngineWith(t, ModeWait, start.AddDate(0, 0, 30),
		monthly("research", "", dollars(3000)))

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "hog", Estimate: estimate(dollars(2800)),
	})
	if err != nil {
		t.Fatal(err)
	}

	dec, err := eng.Check(ctx, "research", dollars(200))
	if err != nil {
		t.Fatal(err)
	}
	if dec.Outcome != budget.OutcomeWait {
		t.Fatalf("outcome = %s, want wait while the hold is live", dec.Outcome)
	}
	retryAt := dec.RetryAt

	done := make(chan error, 1)
	go func() { done <- eng.Wait(ctx, "research", dollars(200)) }()

	// Free the headroom without moving the clock at all. A waiter that slept until
	// RetryAt would still be blocked.
	time.Sleep(10 * time.Millisecond)
	if err := tx.Release(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Wait returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not notice the released headroom")
	}
	if now := clock.Now(); !now.Before(retryAt) {
		t.Errorf("the clock reached %s, at or past RetryAt %s: this did not prove early wakeup",
			now, retryAt)
	}
}

func TestWaitRespectsMaxWait(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	clock := newClock(start.Add(time.Hour))
	eng, err := New(Config{
		Ledger: store, Clock: clock.Now, MaxWait: time.Minute, WaitPoll: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Register(ctx, monthly("research", "", dollars(3000)), ModeWait); err != nil {
		t.Fatal(err)
	}

	// $500 becomes affordable days from now, far beyond the one-minute limit. The
	// caller learns that at once rather than one poll interval at a time.
	if err := eng.Wait(ctx, "research", dollars(500)); !errors.Is(err, ErrWaitTooLong) {
		t.Errorf("error = %v, want ErrWaitTooLong", err)
	}
}

func TestWaitRespectsContextDeadline(t *testing.T) {
	eng, _ := newEngine(t, ModeWait, start.Add(time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := eng.Wait(ctx, "research", dollars(500)); !errors.Is(err, ErrWaitTooLong) {
		t.Errorf("error = %v, want ErrWaitTooLong", err)
	}
}

func TestWaitRespectsContextCancellation(t *testing.T) {
	eng, _ := newEngine(t, ModeWait, start.Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- eng.Wait(ctx, "research", dollars(500)) }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait ignored cancellation")
	}
}

// --- Period transitions ------------------------------------------------------

// TestPeriodTransitionConservesMoney is the acceptance criterion that recurring
// budgets stay correct across several periods.
func TestPeriodTransitionConservesMoney(t *testing.T) {
	ctx := context.Background()
	def := monthly("research", "", dollars(1000))
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance}
	eng, clock, store := newEngineWith(t, ModeMonitor, start, def)

	// Spend a different amount in each of three periods, at the end of each so
	// pacing is not the constraint under test.
	spends := []money.Money{dollars(400), dollars(1200), dollars(600)}
	for i, amount := range spends {
		clock.Set(start.AddDate(0, i+1, 0).Add(-time.Hour))
		spend(t, eng, fmt.Sprintf("r%d", i), "research", amount)
	}

	// Move into a fourth period and close everything out.
	clock.Set(start.AddDate(0, 3, 0).Add(time.Hour))
	if _, err := eng.Status(ctx, "research"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Advance(ctx, "research"); err != nil {
		t.Fatal(err)
	}

	periods, err := store.Periods(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if len(periods) != 4 {
		t.Fatalf("periods = %d, want 4", len(periods))
	}

	// With balance rollover and no cap, carry accumulates as allocation minus
	// spend. Nothing is created or destroyed at a boundary.
	var running money.Money
	for i, p := range periods {
		if p.Envelope.Carry != running {
			t.Errorf("period %d carry = %s, want %s", i, p.Envelope.Carry, running)
		}
		var spent money.Money
		if i < len(spends) {
			spent = spends[i]
		}
		running += p.Envelope.Allocation - spent
	}

	// Total granted minus total spent equals the money still available, which is
	// the fourth period's allocation plus its carry.
	var totalSpent money.Money
	for _, s := range spends {
		totalSpent += s
	}
	granted := dollars(1000) * 4
	if got := periods[3].Envelope.Total(); got != granted-totalSpent {
		t.Errorf("money available in period 3 = %s, want granted %s minus spent %s = %s",
			got, granted, totalSpent, granted-totalSpent)
	}
}

// TestHoldAcrossBoundarySettlesInTheAuthorizingPeriod pins down the boundary rule:
// a charge lands in the period that admitted it, and the successor starts on a
// conservative provisional carry until the predecessor drains.
func TestHoldAcrossBoundarySettlesInTheAuthorizingPeriod(t *testing.T) {
	ctx := context.Background()
	def := monthly("research", "", dollars(1000))
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverBalance}
	// Just before the boundary, so period 0 authorizes the hold.
	eng, clock, store := newEngineWith(t, ModeMonitor, start.AddDate(0, 1, 0).Add(-time.Minute), def)

	tx, _, err := eng.Begin(ctx, Request{
		BudgetID: "research", RequestID: "crossing", Estimate: estimate(dollars(300)),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cross the boundary. Period 0 drains; period 1 opens on a provisional carry
	// computed as if the outstanding hold settles in full.
	clock.Set(start.AddDate(0, 1, 0).Add(time.Hour))
	second, err := eng.Status(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if second.Period.Seq != 1 {
		t.Fatalf("period seq = %d, want 1", second.Period.Seq)
	}
	if !second.Period.Provisional() {
		t.Error("the successor's carry must be provisional while the predecessor drains")
	}
	if want := dollars(700); second.Period.Envelope.Carry != want {
		t.Errorf("provisional carry = %s, want %s (the hold assumed to settle in full)",
			second.Period.Envelope.Carry, want)
	}

	// It settles for less than it held, and the charge belongs to period 0.
	if _, err := tx.Settle(ctx, spent(dollars(100))); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Advance(ctx, "research"); err != nil {
		t.Fatal(err)
	}

	periods, err := store.Periods(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if periods[0].State != ledger.StateClosed {
		t.Errorf("period 0 state = %s, want closed once drained", periods[0].State)
	}
	if want := dollars(900); periods[0].ClosingBalance != want {
		t.Errorf("period 0 closing balance = %s, want %s", periods[0].ClosingBalance, want)
	}
	// The carry is revised upward only, and is now final.
	if want := dollars(900); periods[1].Envelope.Carry != want {
		t.Errorf("final carry = %s, want %s", periods[1].Envelope.Carry, want)
	}
	if periods[1].Provisional() {
		t.Error("the carry must be final once the predecessor closed")
	}
	// The charge is attributed to the authorizing period, not to the successor.
	tot, err := store.Totals(ctx,
		ledger.Scope{BudgetID: "research", PeriodID: periods[1].ID}, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tot.Spent != 0 {
		t.Errorf("period 1 Spent = %s, want 0: the charge belongs to period 0", tot.Spent)
	}
}

// TestRolloverCapFormsAreEquivalent is a stated acceptance criterion: an absolute
// cap and a percentage cap configured to equivalent values behave identically.
func TestRolloverCapFormsAreEquivalent(t *testing.T) {
	ctx := context.Background()

	absolute := monthly("absolute", "", dollars(1000))
	absolute.Rollover = budget.RolloverPolicy{Mode: budget.RolloverCredit, Cap: dollars(250)}

	// 25% of a $1000 allocation is $250.
	percentage := monthly("percentage", "", dollars(1000))
	percentage.Rollover = budget.RolloverPolicy{Mode: budget.RolloverCredit, CapBasisPoints: 2500}

	eng, clock, store := newEngineWith(t, ModeMonitor, start, absolute, percentage)

	// Spend $100 in each, leaving a $900 balance that both caps must clamp to $250.
	clock.Set(start.AddDate(0, 1, 0).Add(-time.Hour))
	spend(t, eng, "a", "absolute", dollars(100))
	spend(t, eng, "p", "percentage", dollars(100))

	clock.Set(start.AddDate(0, 1, 0).Add(time.Hour))
	for _, id := range []string{"absolute", "percentage"} {
		if _, err := eng.Status(ctx, id); err != nil {
			t.Fatal(err)
		}
		periods, err := store.Periods(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(periods) != 2 {
			t.Fatalf("%s: periods = %d, want 2", id, len(periods))
		}
		if want := dollars(900); periods[0].ClosingBalance != want {
			t.Errorf("%s closing balance = %s, want %s", id, periods[0].ClosingBalance, want)
		}
		if want := dollars(250); periods[1].Envelope.Carry != want {
			t.Errorf("%s carry = %s, want the capped %s", id, periods[1].Envelope.Carry, want)
		}
	}
}

func TestAdvanceAllCoversEveryBudget(t *testing.T) {
	eng, clock := hierarchy(t, ModeEnforce, start)
	ctx := context.Background()

	clock.Set(start.AddDate(0, 1, 0).Add(time.Hour))
	changed, err := eng.AdvanceAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for _, p := range changed {
		if p.State == ledger.StateClosed {
			closed++
		}
	}
	if closed != 3 {
		t.Errorf("closed %d periods, want one per budget: %+v", closed, changed)
	}
	// Idempotent.
	again, err := eng.AdvanceAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a second AdvanceAll changed %d periods, want none", len(again))
	}
}

// TestRequestAfterDefinitionEndIsRefused: a fixed-term budget stops accepting work
// when its term ends rather than silently extending itself.
func TestRequestAfterDefinitionEndIsRefused(t *testing.T) {
	ctx := context.Background()
	def := budget.Definition{
		ID:         "grant",
		Allocation: dollars(1000),
		Recurrence: budget.RecurNone,
		AnchorAt:   start,
		EndAt:      start.AddDate(0, 1, 0),
	}
	eng, clock, _ := newEngineWith(t, ModeEnforce, start.Add(time.Hour), def)

	clock.Set(start.AddDate(0, 2, 0))
	_, _, err := eng.Begin(ctx, Request{
		BudgetID: "grant", RequestID: "late", Estimate: estimate(dollars(1)),
	})
	if !errors.Is(err, budget.ErrNoSuchPeriod) {
		t.Errorf("error = %v, want ErrNoSuchPeriod", err)
	}
}

func TestProjectedSpendExtrapolatesTheBurnRate(t *testing.T) {
	eng, clock := newEngine(t, ModeMonitor, start)
	ctx := context.Background()

	clock.Set(start.AddDate(0, 0, 15))
	spend(t, eng, "r", "research", dollars(1000))

	st, err := eng.Status(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	// $1000 over 15 of 31 days projects to about $2066.
	if want := dollars(1000) * 31 / 15; st.ProjectedSpend != want {
		t.Errorf("ProjectedSpend = %s, want %s", st.ProjectedSpend, want)
	}

	// Once the period has fully elapsed there is nothing left to extrapolate.
	clock.Set(start.AddDate(0, 1, 0).Add(-time.Nanosecond))
	st, err = eng.Status(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	if st.ProjectedSpend != dollars(1000) {
		t.Errorf("ProjectedSpend at period end = %s, want the actual %s",
			st.ProjectedSpend, dollars(1000))
	}
}
