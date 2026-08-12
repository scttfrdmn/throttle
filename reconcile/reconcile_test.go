package reconcile_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	activitysqlite "github.com/scttfrdmn/throttle/activity/sqlite"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/reconcile"
	"github.com/scttfrdmn/throttle/usage"
)

// These tests run against the real SQLite stores rather than fakes. Reconciliation
// exists because two durable stores can disagree, and a fake that cannot actually
// hold inconsistent state would let every one of these pass without proving
// anything.

var at = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func dollars(d int64) money.Money { return money.Money(d) * money.PerDollar }

// fixture is a pair of stores plus the helpers to put them into whichever
// half-finished state a test needs.
type fixture struct {
	t    *testing.T
	dir  string
	led  *sqlite.Store
	acts *activitysqlite.Store
	now  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), now: at}
	f.open()
	f.define(budget.Definition{
		ID:         "team",
		Allocation: dollars(100),
		Recurrence: budget.RecurMonthly,
		AnchorAt:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	})
	return f
}

// open opens (or reopens) both stores. Reopening is how the restart tests get a
// genuinely new set of database handles.
func (f *fixture) open() {
	f.t.Helper()
	led, err := sqlite.Open(context.Background(), filepath.Join(f.dir, "ledger.db"))
	if err != nil {
		f.t.Fatalf("open ledger: %v", err)
	}
	acts, err := activitysqlite.Open(context.Background(), filepath.Join(f.dir, "activity.db"))
	if err != nil {
		f.t.Fatalf("open activity: %v", err)
	}
	f.led, f.acts = led, acts
	f.t.Cleanup(func() {
		led.Close()
		acts.Close()
	})
}

// restart closes both stores and opens new handles onto the same files, which is
// the closest a test gets to the process having died.
func (f *fixture) restart() {
	f.t.Helper()
	f.led.Close()
	f.acts.Close()
	f.open()
}

func (f *fixture) define(defs ...budget.Definition) {
	f.t.Helper()
	for _, d := range defs {
		if err := f.led.PutDefinition(context.Background(), d); err != nil {
			f.t.Fatalf("PutDefinition(%q): %v", d.ID, err)
		}
	}
}

func (f *fixture) reconciler(cfg reconcile.Config) *reconcile.Reconciler {
	f.t.Helper()
	if cfg.Ledger == nil {
		cfg.Ledger = f.led
	}
	if cfg.Activity == nil {
		cfg.Activity = f.acts
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return f.now }
	}
	r, err := reconcile.New(cfg)
	if err != nil {
		f.t.Fatalf("reconcile.New: %v", err)
	}
	return r
}

func identity() usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		Publisher:       "anthropic",
		CanonicalModel:  "claude-sonnet-4",
		ProviderModelID: "anthropic.claude-sonnet-4-20250514-v1:0",
		Region:          "us-east-1",
	}
}

// quote is the immutable quote captured at admission: $3/M in, $15/M out.
func quote() pricing.CapturedQuote {
	return pricing.CapturedQuote{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "anthropic.claude-sonnet-4-20250514-v1:0",
		Region:          "us-east-1",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(3)),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(15)),
		},
		Provenance: pricing.Provenance{Source: "test-catalog", Version: "v1", Currency: "USD"},
		CapturedAt: at,
	}
}

// observedUsage is 100k in / 20k out, which the captured quote prices at $0.60.
func observedUsage() usage.Usage {
	return usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  100_000,
		usage.OutputTokens: 20_000,
	})
}

const observedCost = money.Money(600_000) // $0.60 in microdollars

// reserve takes a real hold through the ledger, exactly as the engine would.
func (f *fixture) reserve(id string, amount money.Money, lease time.Duration) ledger.Reservation {
	f.t.Helper()
	ctx := context.Background()
	chain, err := f.led.Chain(ctx, "team")
	if err != nil {
		f.t.Fatalf("Chain: %v", err)
	}
	ceilings := map[string]money.Money{}
	for _, d := range chain {
		ceilings[d.ID] = dollars(100)
	}
	res := ledger.Reservation{
		ID:            "res-" + id,
		BudgetID:      "team",
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

// begin writes the pre-call activity record, the way an adapter does before it
// dials the provider. Everything after this point is what a crash interrupts.
func (f *fixture) begin(id string, res ledger.Reservation) activity.Record {
	f.t.Helper()
	rec := activity.Record{
		RequestID:     id,
		ReservationID: res.ID,
		BudgetID:      "team",
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

func (f *fixture) complete(rec activity.Record) {
	f.t.Helper()
	if err := f.acts.Complete(context.Background(), rec); err != nil {
		f.t.Fatalf("activity Complete(%q): %v", rec.RequestID, err)
	}
}

func (f *fixture) get(id string) activity.Record {
	f.t.Helper()
	rec, err := f.acts.Get(context.Background(), id)
	if err != nil {
		f.t.Fatalf("activity Get(%q): %v", id, err)
	}
	return rec
}

func (f *fixture) reservation(id string) ledger.Reservation {
	f.t.Helper()
	res, err := f.led.Get(context.Background(), id)
	if err != nil {
		f.t.Fatalf("ledger Get(%q): %v", id, err)
	}
	return res
}

func scopesOf(r ledger.Reservation) []activity.Scope {
	out := make([]activity.Scope, 0, len(r.Legs))
	for _, l := range r.Legs {
		out = append(out, activity.Scope{BudgetID: l.Scope.BudgetID, PeriodID: l.Scope.PeriodID, Depth: l.Depth})
	}
	return out
}

// settle drives a real settlement through the ledger.
func (f *fixture) settle(resID string, cost money.Money) ledger.Charge {
	f.t.Helper()
	c, err := f.led.Settle(context.Background(), ledger.Settlement{
		ReservationID: resID,
		ActualCost:    cost,
		Usage: usage.Actual{
			Identity: identity(),
			Usage:    observedUsage(),
			Cost:     usage.KnownCost(cost),
		},
		CompletedAt: f.now,
	})
	if err != nil {
		f.t.Fatalf("Settle(%q): %v", resID, err)
	}
	return c
}

func (f *fixture) totals(budgetID string) ledger.Totals {
	f.t.Helper()
	ctx := context.Background()
	p, err := f.led.EnsurePeriod(ctx, budgetID, f.now)
	if err != nil {
		f.t.Fatalf("EnsurePeriod: %v", err)
	}
	tot, err := f.led.Totals(ctx, ledger.Scope{BudgetID: budgetID, PeriodID: p.ID}, f.now)
	if err != nil {
		f.t.Fatalf("Totals: %v", err)
	}
	return tot
}

// --- 1. The ledger settled and the activity write never landed ---------------

// TestRepairsRecordFromSettledLedger is the canonical crash: money committed, then
// the process died before the telemetry write. The record has to catch up, and the
// money must not move a second time.
func TestRepairsRecordFromSettledLedger(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-1", dollars(1), time.Minute)
	f.begin("req-1", res)
	charge := f.settle(res.ID, observedCost)

	before := f.totals("team")

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-1")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassRepaired {
		t.Fatalf("class = %q, want %q (%s)", got.Class, reconcile.ClassRepaired, got.Detail)
	}
	if got.Reason != reconcile.ReasonCrashRepairable {
		t.Errorf("reason = %q, want %q", got.Reason, reconcile.ReasonCrashRepairable)
	}
	// No second charge: the money already moved before the crash, so a repair that
	// reported a monetary transition would double-count it in any summary.
	if got.Money != reconcile.MoneyNone {
		t.Errorf("money = %q, want no monetary transition", got.Money)
	}

	rec := f.get("req-1")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusSettled)
	}
	if !rec.ActualCost.Known() || rec.ActualCost.Amount != charge.ActualCost {
		t.Errorf("actual cost = %v, want a known %s", rec.ActualCost, charge.ActualCost.CentsString())
	}
	if rec.ActualUsage.Count(usage.InputTokens) != 100_000 {
		t.Errorf("usage was not recovered from the charge: %v", rec.ActualUsage)
	}

	after := f.totals("team")
	if after.Spent != before.Spent {
		t.Errorf("spend changed from %s to %s: the repair charged again",
			before.Spent.CentsString(), after.Spent.CentsString())
	}
	if len(rec.Repairs) != 1 {
		t.Fatalf("got %d repair entries, want 1", len(rec.Repairs))
	}
	if rec.Repairs[0].ObservedStatus != activity.StatusPending {
		t.Errorf("audit lost the observed status: %q", rec.Repairs[0].ObservedStatus)
	}
}

// --- 2. The ledger released and the activity write never landed -------------

func TestRepairsRecordFromReleasedLedger(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-2", dollars(1), time.Minute)
	f.begin("req-2", res)
	if err := f.led.Release(context.Background(), res.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-2")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassRepaired {
		t.Fatalf("class = %q, want %q (%s)", got.Class, reconcile.ClassRepaired, got.Detail)
	}
	rec := f.get("req-2")
	if rec.Status != activity.StatusReleased {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusReleased)
	}
	// Zero here is the ledger's own claim -- Release means no billable usage -- and
	// not an inference the reconciler made.
	if !rec.ActualCost.Known() || rec.ActualCost.Amount != 0 {
		t.Errorf("actual cost = %v, want a known zero", rec.ActualCost)
	}
}

// --- 3. Usage and quote durable, settlement never ran -----------------------

// TestReplaysSettlementFromDurableUsage is the inverse crash: the adapter wrote
// what the provider reported and died before the ledger transition. Enough
// authoritative information exists to finish it, so it is finished -- once.
func TestReplaysSettlementFromDurableUsage(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-3", dollars(1), time.Minute)
	rec := f.begin("req-3", res)
	rec.ActualUsage = observedUsage()
	rec.CompletedAt = f.now.Add(2 * time.Second)
	f.complete(rec)

	r := f.reconciler(reconcile.Config{})
	got, err := r.Reconcile(context.Background(), "req-3")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassRepaired || got.Money != reconcile.MoneySettled {
		t.Fatalf("class = %q money = %q, want repaired/settled (%s)", got.Class, got.Money, got.Detail)
	}
	if got.Amount != observedCost {
		t.Errorf("settled %s, want %s from the captured quote", got.Amount.CentsString(), observedCost.CentsString())
	}
	if f.reservation(res.ID).State != ledger.StateSettled {
		t.Errorf("reservation state = %q, want settled", f.reservation(res.ID).State)
	}
	if spent := f.totals("team").Spent; spent != observedCost {
		t.Errorf("spend = %s, want %s", spent.CentsString(), observedCost.CentsString())
	}

	// Exactly once: a second pass finds the ledger already settled and writes no
	// further money.
	again, err := r.Reconcile(context.Background(), "req-3")
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if again.Class != reconcile.ClassConsistent {
		t.Errorf("second pass class = %q, want %q (%s)", again.Class, reconcile.ClassConsistent, again.Detail)
	}
	if spent := f.totals("team").Spent; spent != observedCost {
		t.Errorf("spend after a second pass = %s, want %s", spent.CentsString(), observedCost.CentsString())
	}

	audit := f.get("req-3").Repairs
	if len(audit) == 0 {
		t.Fatal("the replayed settlement left no audit entry")
	}
	if audit[0].QuoteSource != "test-catalog" || audit[0].QuoteVersion != "v1" {
		t.Errorf("audit does not identify the quote used: %+v", audit[0])
	}
	if audit[0].Money != "settled" || audit[0].Amount != observedCost {
		t.Errorf("audit does not record the money moved: %+v", audit[0])
	}
}

// --- 4. Anti-vacuous: the live catalog moves before reconciliation ----------

// TestReplayIgnoresCurrentCatalogPrices is the test that makes the "never consult
// live pricing" rule mean something. The catalog is mutated far beyond any rounding
// difference before the repair runs; if the reconciler consulted it, the settled
// amount could not possibly come out at the original figure.
func TestReplayIgnoresCurrentCatalogPrices(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-4", dollars(1), time.Minute)
	rec := f.begin("req-4", res)
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	// A hundredfold price rise, live, after the request was admitted.
	live, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "anthropic.claude-sonnet-4-20250514-v1:0",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(300)),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(1500)),
		},
		Provenance: pricing.Provenance{
			Source: "test-catalog", Version: "v2",
			EffectiveFrom: at.Add(-time.Hour), Currency: "USD",
		},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	// Proof the mutation is real and would be visible if it were consulted.
	q, err := live.Quote(context.Background(), identity(), observedUsage(), f.now)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Cost.Amount == observedCost {
		t.Fatal("the mutated catalog prices the same as the captured quote; the test would prove nothing")
	}

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-4")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Amount != observedCost {
		t.Fatalf("settled %s, want %s: the repair used current prices instead of the quote captured at admission",
			got.Amount.CentsString(), observedCost.CentsString())
	}
	if v := f.get("req-4").Repairs[0].QuoteVersion; v != "v1" {
		t.Errorf("audit names quote version %q, want the captured v1", v)
	}
}

// --- 5. Two concurrent reconcilers -----------------------------------------

// TestConcurrentReconcilersProduceOneSettlement pins the concurrency invariant:
// exactly one monetary transition wins and the loser converges on it rather than
// erroring, which is what makes it safe to run this from several processes.
func TestConcurrentReconcilersProduceOneSettlement(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-5", dollars(1), time.Minute)
	rec := f.begin("req-5", res)
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	const n = 8
	results := make([]reconcile.Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := f.reconciler(reconcile.Config{})
			<-start
			results[i], errs[i] = r.Reconcile(context.Background(), "req-5")
		}(i)
	}
	close(start)
	wg.Wait()

	settlements := 0
	for i, res := range results {
		if errs[i] != nil {
			t.Fatalf("reconciler %d: %v", i, errs[i])
		}
		if res.Class == reconcile.ClassFailed {
			t.Fatalf("reconciler %d failed instead of converging: %v (%s)", i, res.Err, res.Detail)
		}
		if res.Money == reconcile.MoneySettled {
			settlements++
			if res.Amount != observedCost {
				t.Errorf("reconciler %d settled %s, want %s", i, res.Amount.CentsString(), observedCost.CentsString())
			}
		}
	}
	if settlements != 1 {
		t.Errorf("%d reconcilers reported settling; exactly one monetary transition may win", settlements)
	}
	if spent := f.totals("team").Spent; spent != observedCost {
		t.Errorf("spend = %s, want exactly one charge of %s", spent.CentsString(), observedCost.CentsString())
	}
	if got := f.get("req-5").Status; got != activity.StatusSettled {
		t.Errorf("status = %q, want settled", got)
	}
}

// --- 6. Idempotence -------------------------------------------------------

// TestRepeatedReconcileIsIdempotent runs the whole sweep repeatedly over a mixed
// population. Reconciliation is meant to be safe to run at every process start, so
// running it five times must not differ from running it once.
func TestRepeatedReconcileIsIdempotent(t *testing.T) {
	f := newFixture(t)

	// Repairable: usage and quote durable, settlement never ran.
	resA := f.reserve("req-a", dollars(1), time.Minute)
	recA := f.begin("req-a", resA)
	recA.ActualUsage = observedUsage()
	f.complete(recA)

	// Repairable the other way: ledger settled, record stale.
	resB := f.reserve("req-b", dollars(1), time.Minute)
	f.begin("req-b", resB)
	f.settle(resB.ID, dollars(2))

	// Must stay unresolved: no usage anywhere.
	resC := f.reserve("req-c", dollars(1), time.Minute)
	f.begin("req-c", resC)

	r := f.reconciler(reconcile.Config{})
	var first reconcile.Summary
	for i := 0; i < 5; i++ {
		sum, err := r.ReconcilePending(context.Background())
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if i == 0 {
			first = sum
			continue
		}
		if sum.Repaired != 0 {
			t.Errorf("pass %d repaired %d records; the first pass should have finished the work", i, sum.Repaired)
		}
		if sum.Settled != 0 {
			t.Errorf("pass %d moved %s; money must move exactly once", i, sum.Settled.CentsString())
		}
	}
	if first.Repaired != 2 {
		t.Errorf("first pass repaired %d, want 2", first.Repaired)
	}
	if first.Unresolved != 1 {
		t.Errorf("first pass left %d unresolved, want 1", first.Unresolved)
	}
	want, _ := money.Add(observedCost, 0)
	if first.Settled != want {
		t.Errorf("first pass settled %s, want %s", first.Settled.CentsString(), want.CentsString())
	}
	if spent := f.totals("team").Spent; spent != observedCost+dollars(2) {
		t.Errorf("total spend = %s, want %s", spent.CentsString(), (observedCost + dollars(2)).CentsString())
	}
}

// --- 7. Process restart ---------------------------------------------------

// TestRepairSurvivesProcessRestart closes both databases and reopens them, which is
// as close as a test gets to the process having died: the in-memory Transaction that
// knew about this request is gone, and the repair has to work from durable state
// alone.
func TestRepairSurvivesProcessRestart(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-7", dollars(1), time.Minute)
	rec := f.begin("req-7", res)
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	f.restart()

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-7")
	if err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	if got.Class != reconcile.ClassRepaired || got.Amount != observedCost {
		t.Fatalf("class = %q amount = %s, want repaired at %s (%s)",
			got.Class, got.Amount.CentsString(), observedCost.CentsString(), got.Detail)
	}
	if f.reservation(res.ID).State != ledger.StateSettled {
		t.Error("the hold was not settled after a restart")
	}
}

// --- 8. An expired lease is not proof of zero spend -----------------------

// TestExpiredHoldStillSettles is the guarantee that lease expiry is a headroom
// mechanism and nothing more. Recovery has already marked the hold expired to stop
// it blocking the budget; the money it owes is still owed, and late-arriving usage
// must still be able to settle it.
func TestExpiredHoldStillSettles(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-8", dollars(1), time.Minute)
	rec := f.begin("req-8", res)
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	// The lease lapses and recovery reclaims the headroom.
	f.now = at.Add(time.Hour)
	expired, err := f.led.RecoverExpired(context.Background(), "team", f.now)
	if err != nil {
		t.Fatalf("RecoverExpired: %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("recovered %d holds, want 1", len(expired))
	}
	if got := f.reservation(res.ID).State; got != ledger.StateExpired {
		t.Fatalf("state = %q, want expired", got)
	}

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-8")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassRepaired || got.Money != reconcile.MoneySettled {
		t.Fatalf("class = %q money = %q, want a late settlement (%s)", got.Class, got.Money, got.Detail)
	}
	if got.Amount != observedCost {
		t.Errorf("settled %s, want %s", got.Amount.CentsString(), observedCost.CentsString())
	}
	if spent := f.totals("team").Spent; spent != observedCost {
		t.Errorf("spend = %s, want %s: an expired lease must not become a zero charge",
			spent.CentsString(), observedCost.CentsString())
	}
}

// --- 9. No authoritative usage stays outcome-unknown ----------------------

// TestNoUsageStaysOutcomeUnknown is the single most important negative test. The
// provider may have served and billed this request; nothing durable says either
// way. Every available way of resolving it is a guess, so it must stay unresolved --
// and above all must never become a zero-cost success.
func TestNoUsageStaysOutcomeUnknown(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-9", dollars(1), time.Minute)
	f.begin("req-9", res)

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-9")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassUnresolved {
		t.Fatalf("class = %q, want %q (%s)", got.Class, reconcile.ClassUnresolved, got.Detail)
	}
	if got.Reason != reconcile.ReasonProviderOutcomeUnknown {
		t.Errorf("reason = %q, want %q", got.Reason, reconcile.ReasonProviderOutcomeUnknown)
	}
	if got.Money != reconcile.MoneyNone {
		t.Fatalf("money = %q: an unknown outcome may not move money", got.Money)
	}

	if state := f.reservation(res.ID).State; state != ledger.StatePending {
		t.Errorf("reservation state = %q, want the hold left standing", state)
	}
	rec := f.get("req-9")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
	if rec.ActualCost.Known() {
		t.Fatalf("actual cost = %v, want an explicitly unknown cost, never a zero", rec.ActualCost)
	}
	if spent := f.totals("team").Spent; spent != 0 {
		t.Errorf("spend = %s, want nothing charged", spent.CentsString())
	}
	if enc := f.totals("team").Reserved; enc != dollars(1) {
		t.Errorf("reserved = %s, want the estimate still encumbered", enc.CentsString())
	}
}

// --- 10. Usage known, quote cannot price it -------------------------------

// TestUnpricedDimensionStaysUnresolved covers the third class: the facts are all
// here and the money still cannot be named, because the quote frozen at admission
// has no rate for a dimension the provider billed. Pricing data resolves this, not
// repair.
func TestUnpricedDimensionStaysUnresolved(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-10", dollars(1), time.Minute)
	rec := f.begin("req-10", res)
	rec.ActualUsage = usage.New(map[usage.Dimension]int64{
		usage.InputTokens: 100_000,
		// The captured quote has no rate for this.
		usage.CacheWriteTokens: 50_000,
	})
	f.complete(rec)

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-10")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassUnresolved {
		t.Fatalf("class = %q, want %q (%s)", got.Class, reconcile.ClassUnresolved, got.Detail)
	}
	if got.Reason != reconcile.ReasonPricingUnresolved {
		t.Errorf("reason = %q, want %q", got.Reason, reconcile.ReasonPricingUnresolved)
	}
	if got.Money != reconcile.MoneyNone {
		t.Fatal("an unpriceable request may not settle")
	}
	if state := f.reservation(res.ID).State; state != ledger.StatePending {
		t.Errorf("state = %q, want the hold left encumbered", state)
	}
	out := f.get("req-10")
	if out.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want %q", out.Status, activity.StatusUnresolved)
	}
	// Partial is not known: the floor is recorded, and it is not presented as a total.
	if out.ActualCost.Known() {
		t.Errorf("cost = %v, want an unresolved cost", out.ActualCost)
	}
	if out.ActualCost.AtLeast() == 0 {
		t.Error("the priced floor was lost; a partial cost should keep what it could price")
	}
}

// --- 11. AgentCore awaiting delayed session usage ------------------------

// TestAwaitingExternalUsageIsNotCrashDamage is the distinction the classifier exists
// for. A hosted runtime invocation whose resource consumption is reported out of
// band, later, is in its designed terminal state. Calling it stranded would put a
// permanent healthy state into a damage report, and repairing it would mean
// inventing the number that has not arrived.
func TestAwaitingExternalUsageIsNotCrashDamage(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-11", dollars(1), time.Minute)
	rec := f.begin("req-11", res)
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeSuccess
	rec.ActualCost = usage.UnknownCost("AgentCore reports runtime resource consumption per session, out of band")
	rec.Runtime = activity.HostedRuntime{
		RuntimeID: "arn:aws:bedrock-agentcore:us-east-1:111122223333:runtime/r-1",
		SessionID: "session-abc",
		Note:      "runtime resource usage is reported per session",
	}
	rec.CompletedAt = f.now.Add(3 * time.Second)
	f.complete(rec)

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-11")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassAwaiting {
		t.Fatalf("class = %q, want %q (%s)", got.Class, reconcile.ClassAwaiting, got.Detail)
	}
	if got.Reason != reconcile.ReasonAwaitingExternalUsage {
		t.Errorf("reason = %q, want %q", got.Reason, reconcile.ReasonAwaitingExternalUsage)
	}
	if got.Money != reconcile.MoneyNone {
		t.Fatal("a record awaiting external usage may not move money")
	}
	if state := f.reservation(res.ID).State; state != ledger.StatePending {
		t.Errorf("state = %q, want the hold left standing", state)
	}
	// Untouched: not even the status is rewritten, because the adapter's conclusion is
	// an observation and the reconciler has nothing to add to it.
	after := f.get("req-11")
	if after.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want it left %q", after.Status, activity.StatusUnresolved)
	}
	if len(after.Repairs) != 0 {
		t.Errorf("a record awaiting data was rewritten: %+v", after.Repairs)
	}
	if after.ActualCost.Known() {
		t.Errorf("cost = %v, want it left unknown", after.ActualCost)
	}

	sum, err := f.reconciler(reconcile.Config{}).ReconcilePending(context.Background())
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if sum.Awaiting != 1 || sum.Unresolved != 0 || sum.Repaired != 0 {
		t.Errorf("summary = awaiting %d / unresolved %d / repaired %d, want it counted only as awaiting",
			sum.Awaiting, sum.Unresolved, sum.Repaired)
	}
}

// --- 12. No per-invocation allocation of a session charge ----------------

// TestSessionUsageIsNotApportionedAcrossInvocations pins the resolution of #20.
// Several invocations share one runtime session, and the session's resource cost is
// not divisible among them by any honest rule -- not evenly, not by bytes, not by
// latency. So reconciliation attributes nothing, and each invocation's cost stays
// unknown.
func TestSessionUsageIsNotApportionedAcrossInvocations(t *testing.T) {
	f := newFixture(t)
	ids := []string{"req-12a", "req-12b", "req-12c"}
	// Deliberately unequal in every dimension a heuristic might reach for.
	bytes := []int64{10, 1_000, 100_000}
	latency := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second}

	for i, id := range ids {
		res := f.reserve(id, dollars(1), time.Minute)
		rec := f.begin(id, res)
		rec.Status = activity.StatusUnresolved
		rec.Outcome = activity.OutcomeSuccess
		rec.ActualCost = usage.UnknownCost("runtime resource consumption is reported per session")
		rec.Latency = latency[i]
		rec.Runtime = activity.HostedRuntime{
			RuntimeID:     "arn:aws:bedrock-agentcore:us-east-1:111122223333:runtime/r-1",
			SessionID:     "session-shared",
			PayloadBytes:  bytes[i],
			ResponseBytes: bytes[i] * 2,
		}
		f.complete(rec)
	}

	sum, err := f.reconciler(reconcile.Config{}).ReconcilePending(context.Background())
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if sum.Awaiting != len(ids) {
		t.Errorf("awaiting = %d, want %d", sum.Awaiting, len(ids))
	}
	if sum.Settled != 0 || sum.Repaired != 0 {
		t.Errorf("settled %s across %d repairs: session runtime cost must not be apportioned to invocations",
			sum.Settled.CentsString(), sum.Repaired)
	}
	for _, id := range ids {
		rec := f.get(id)
		if rec.ActualCost.Known() {
			t.Errorf("%s was given a cost of %s; no per-invocation runtime amount may be derived",
				id, rec.ActualCost.Amount.CentsString())
		}
		if rec.Runtime.ReconciledCost.Known() || !rec.Runtime.ReconciledUsage.Empty() {
			t.Errorf("%s was given apportioned session usage: %+v", id, rec.Runtime)
		}
	}
	if spent := f.totals("team").Spent; spent != 0 {
		t.Errorf("spend = %s, want nothing charged from a session-level amount", spent.CentsString())
	}
}

// --- 13. The authorizing period owns the charge --------------------------

// TestRepairChargesTheAuthorizingPeriod is the period-attribution invariant. A
// request admitted in August is August's spend even when its repair happens in
// September; moving it would make both months wrong and would let a budget be
// overspent twice.
func TestRepairChargesTheAuthorizingPeriod(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-13", dollars(1), time.Minute)
	rec := f.begin("req-13", res)
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	authorizing := res.Legs[0].Scope
	ctx := context.Background()

	// Time moves into the next month and the ledger advances. The authorizing period
	// cannot close while a settleable hold belongs to it, which is the structural
	// guarantee this test also documents.
	f.now = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	if _, err := f.led.Advance(ctx, "team", f.now); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	authState, err := f.led.Period(ctx, authorizing.PeriodID)
	if err != nil {
		t.Fatalf("Period: %v", err)
	}
	if authState.State == ledger.StateClosed {
		t.Fatalf("the authorizing period closed with a settleable hold outstanding, "+
			"which would make late settlement impossible: %+v", authState)
	}

	got, err := f.reconciler(reconcile.Config{}).Reconcile(ctx, "req-13")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassRepaired {
		t.Fatalf("class = %q, want repaired (%s)", got.Class, got.Detail)
	}

	charge, err := f.led.ChargeFor(ctx, res.ID)
	if err != nil {
		t.Fatalf("ChargeFor: %v", err)
	}
	for _, leg := range charge.Legs {
		if leg.Scope.PeriodID != legScope(res, leg.Depth).PeriodID {
			t.Errorf("charge leg at depth %d landed in %q, want the authorizing period %q",
				leg.Depth, leg.Scope.PeriodID, legScope(res, leg.Depth).PeriodID)
		}
	}

	// The August envelope carries the spend; September's is untouched.
	augTotals, err := f.led.Totals(ctx, authorizing, f.now)
	if err != nil {
		t.Fatalf("Totals(august): %v", err)
	}
	if augTotals.Spent != observedCost {
		t.Errorf("august spend = %s, want %s", augTotals.Spent.CentsString(), observedCost.CentsString())
	}
	sept, err := f.led.EnsurePeriod(ctx, "team", f.now)
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	if sept.ID == authorizing.PeriodID {
		t.Fatal("the period did not roll over; the test is not exercising a cross-period repair")
	}
	septTotals, err := f.led.Totals(ctx, ledger.Scope{BudgetID: "team", PeriodID: sept.ID}, f.now)
	if err != nil {
		t.Fatalf("Totals(september): %v", err)
	}
	if septTotals.Spent != 0 {
		t.Errorf("september spend = %s, want nothing: a recovered charge stays with the period that authorized it",
			septTotals.Spent.CentsString())
	}
}

func legScope(res ledger.Reservation, depth int) ledger.Scope {
	for _, l := range res.Legs {
		if l.Depth == depth {
			return l.Scope
		}
	}
	return ledger.Scope{}
}

// --- 14. No invented identity -------------------------------------------

// TestOrphanedReservationInventsNothing covers a hold whose activity record never
// landed at all. The usage, the quote, and the outcome were never durably recorded,
// so there is nothing to repair from -- and fabricating a plausible record would put
// invented provider metadata into the durable observability store.
func TestOrphanedReservationInventsNothing(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-14", dollars(1), time.Minute)
	// No activity record at all: the crash landed between Reserve and Begin.

	sum, err := f.reconciler(reconcile.Config{}).ReconcilePending(context.Background())
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if sum.Orphaned != 1 {
		t.Fatalf("orphaned = %d, want 1 (%+v)", sum.Orphaned, sum.Results)
	}
	got := sum.Results[0]
	if got.Class != reconcile.ClassOrphaned || got.Reason != reconcile.ReasonIncompleteRecord {
		t.Errorf("class = %q reason = %q, want orphaned/incomplete-record", got.Class, got.Reason)
	}
	if got.Money != reconcile.MoneyNone {
		t.Error("an orphaned hold may not move money")
	}
	if _, err := f.acts.Get(context.Background(), "req-14"); !errors.Is(err, activity.ErrNotFound) {
		t.Errorf("an activity record was fabricated for an orphaned hold: %v", err)
	}
	if state := f.reservation(res.ID).State; state != ledger.StatePending {
		t.Errorf("state = %q, want the hold left standing for a human to judge", state)
	}

	// The mirror case: a record naming a reservation the ledger has never heard of.
	stray := activity.Record{
		RequestID:     "req-14b",
		ReservationID: "res-does-not-exist",
		BudgetID:      "team",
		Status:        activity.StatusPending,
		StartedAt:     f.now,
	}
	if err := f.acts.Begin(context.Background(), stray); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	res2, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-14b")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res2.Class != reconcile.ClassOrphaned {
		t.Errorf("class = %q, want orphaned (%s)", res2.Class, res2.Detail)
	}
	if got := f.get("req-14b"); got.Identity != (usage.ModelIdentity{}) {
		t.Errorf("identity was invented for a record that never recorded one: %+v", got.Identity)
	}
}

// --- 15. Content privacy ------------------------------------------------

// TestReconciliationNeedsNoContent is a structural privacy check. Reconciliation
// must work from statuses, counts, amounts, and identifiers alone -- if it ever
// needed a prompt or a response to decide anything, throttle would have to persist
// request content to be crash-safe, which it must never do.
func TestReconciliationNeedsNoContent(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-15", dollars(1), time.Minute)
	rec := f.begin("req-15", res)
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	// The record as stored holds no content, and reconciliation still resolves it.
	stored := f.get("req-15")
	if got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-15"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	} else if got.Class != reconcile.ClassRepaired {
		t.Fatalf("class = %q, want repaired from content-free state (%s)", got.Class, got.Detail)
	}
	_ = stored

	// Nothing the repair wrote may contain content either. The secret is a stand-in
	// for a prompt: if it appears anywhere in the repaired row, something copied a
	// payload into the audit trail.
	const secret = "the-user-prompt-text"
	after := f.get("req-15")
	for _, r := range after.Repairs {
		blob := strings.Join([]string{r.Class, r.Reason, r.Detail, string(r.ObservedStatus), string(r.ProducedStatus)}, " ")
		if strings.Contains(blob, secret) {
			t.Errorf("the audit entry contains request content: %q", blob)
		}
	}
	if after.Error != "" && strings.Contains(after.Error, secret) {
		t.Error("the repaired record's error carries content")
	}

	// And the mechanism must not require it: a record with no agent trace, no runtime
	// payload, and no metadata reconciles exactly the same way.
	if !after.Agent.Empty() {
		t.Error("the repair populated an agent trace it was never given")
	}
	if !after.Runtime.Empty() {
		t.Error("the repair populated runtime detail it was never given")
	}
}

// --- 17. Crash at each lifecycle boundary ------------------------------

// TestConvergesFromEveryLifecycleBoundary walks the points at which a process can
// die during one governed request and asserts that a repair pass reaches a defensible
// terminal state from each. This is the matrix that makes the classifier's coverage
// explicit rather than incidental.
func TestConvergesFromEveryLifecycleBoundary(t *testing.T) {
	cases := []struct {
		name string
		// crash puts the two stores into the state that boundary leaves behind.
		crash      func(f *fixture, id string) ledger.Reservation
		wantClass  reconcile.Class
		wantReason reconcile.Reason
		wantStatus activity.Status
		wantSpend  money.Money
	}{
		{
			name: "after reserve, before the activity record",
			crash: func(f *fixture, id string) ledger.Reservation {
				return f.reserve(id, dollars(1), time.Minute)
			},
			wantClass:  reconcile.ClassOrphaned,
			wantReason: reconcile.ReasonIncompleteRecord,
		},
		{
			name: "after the activity record, before the provider call",
			crash: func(f *fixture, id string) ledger.Reservation {
				res := f.reserve(id, dollars(1), time.Minute)
				f.begin(id, res)
				return res
			},
			wantClass:  reconcile.ClassUnresolved,
			wantReason: reconcile.ReasonProviderOutcomeUnknown,
			wantStatus: activity.StatusOutstanding,
		},
		{
			name: "after usage was recorded, before settlement",
			crash: func(f *fixture, id string) ledger.Reservation {
				res := f.reserve(id, dollars(1), time.Minute)
				rec := f.begin(id, res)
				rec.ActualUsage = observedUsage()
				f.complete(rec)
				return res
			},
			wantClass:  reconcile.ClassRepaired,
			wantReason: reconcile.ReasonCrashRepairable,
			wantStatus: activity.StatusSettled,
			wantSpend:  observedCost,
		},
		{
			name: "after settlement, before the final activity write",
			crash: func(f *fixture, id string) ledger.Reservation {
				res := f.reserve(id, dollars(1), time.Minute)
				f.begin(id, res)
				f.settle(res.ID, observedCost)
				return res
			},
			wantClass:  reconcile.ClassRepaired,
			wantReason: reconcile.ReasonCrashRepairable,
			wantStatus: activity.StatusSettled,
			wantSpend:  observedCost,
		},
		{
			name: "after a provider refusal was recorded, before the release",
			crash: func(f *fixture, id string) ledger.Reservation {
				res := f.reserve(id, dollars(1), time.Minute)
				rec := f.begin(id, res)
				rec.Status = activity.StatusReleased
				rec.Outcome = activity.OutcomeProviderError
				rec.ActualCost = usage.KnownCost(0)
				f.complete(rec)
				return res
			},
			wantClass:  reconcile.ClassRepaired,
			wantReason: reconcile.ReasonCrashRepairable,
			wantStatus: activity.StatusReleased,
		},
		{
			name: "after the release, before the final activity write",
			crash: func(f *fixture, id string) ledger.Reservation {
				res := f.reserve(id, dollars(1), time.Minute)
				f.begin(id, res)
				if err := f.led.Release(context.Background(), res.ID); err != nil {
					f.t.Fatalf("Release: %v", err)
				}
				return res
			},
			wantClass:  reconcile.ClassRepaired,
			wantReason: reconcile.ReasonCrashRepairable,
			wantStatus: activity.StatusReleased,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			const id = "req-boundary"
			res := tc.crash(f, id)

			// A restart, because that is what actually happened.
			f.restart()
			r := f.reconciler(reconcile.Config{})

			sum, err := r.ReconcilePending(context.Background())
			if err != nil {
				t.Fatalf("ReconcilePending: %v", err)
			}
			if len(sum.Results) != 1 {
				t.Fatalf("examined %d records, want 1: %+v", len(sum.Results), sum.Results)
			}
			got := sum.Results[0]
			if got.Class != tc.wantClass {
				t.Errorf("class = %q, want %q (%s)", got.Class, tc.wantClass, got.Detail)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if tc.wantStatus != "" {
				if st := f.get(id).Status; st != tc.wantStatus {
					t.Errorf("status = %q, want %q", st, tc.wantStatus)
				}
			}
			if spent := f.totals("team").Spent; spent != tc.wantSpend {
				t.Errorf("spend = %s, want %s", spent.CentsString(), tc.wantSpend.CentsString())
			}
			_ = res

			// Rerunning converges: no further repair, no further money.
			second, err := r.ReconcilePending(context.Background())
			if err != nil {
				t.Fatalf("second ReconcilePending: %v", err)
			}
			if second.Repaired != 0 || second.Settled != 0 || second.Released != 0 {
				t.Errorf("a second pass repaired %d and moved %s/%s; the first should have converged",
					second.Repaired, second.Settled.CentsString(), second.Released.CentsString())
			}
			if spent := f.totals("team").Spent; spent != tc.wantSpend {
				t.Errorf("spend after a second pass = %s, want %s", spent.CentsString(), tc.wantSpend.CentsString())
			}
		})
	}
}

// --- 18. The summary does not flatter itself ---------------------------

// TestSummaryCountsEachClassHonestly builds one of everything and checks the
// arithmetic. The failure this guards against is a summary that reads clean: an
// unresolved record counted as repaired claims money is accounted for when it is
// not, and counted as failed makes a healthy system look broken.
func TestSummaryCountsEachClassHonestly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// repaired
	resR := f.reserve("s-repaired", dollars(1), time.Minute)
	recR := f.begin("s-repaired", resR)
	recR.ActualUsage = observedUsage()
	f.complete(recR)

	// A finished request, for contrast. It is deliberately not expected in the counts
	// below: its activity row is terminal and its hold is resolved, so it is in
	// neither candidate set. A sweep that examined every settled request would scan
	// the whole history of the ledger to learn nothing.
	resC := f.reserve("s-consistent", dollars(1), time.Minute)
	recC := f.begin("s-consistent", resC)
	charge := f.settle(resC.ID, dollars(2))
	recC.Status = activity.StatusSettled
	recC.ActualUsage = observedUsage()
	recC.ActualCost = usage.KnownCost(charge.ActualCost)
	recC.CompletedAt = f.now
	f.complete(recC)

	// unresolved: outcome unknown
	resU := f.reserve("s-unresolved", dollars(1), time.Minute)
	f.begin("s-unresolved", resU)

	// awaiting external usage
	resA := f.reserve("s-awaiting", dollars(1), time.Minute)
	recA := f.begin("s-awaiting", resA)
	recA.Status = activity.StatusUnresolved
	recA.ActualCost = usage.UnknownCost("runtime resource consumption is reported per session")
	recA.Runtime = activity.HostedRuntime{RuntimeID: "rt", SessionID: "sess-1"}
	f.complete(recA)

	// orphaned: a hold with no record
	f.reserve("s-orphan", dollars(1), time.Minute)

	// released, needing repair on the activity side
	resL := f.reserve("s-released", dollars(1), time.Minute)
	f.begin("s-released", resL)
	if err := f.led.Release(ctx, resL.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}

	sum, err := f.reconciler(reconcile.Config{}).ReconcilePending(ctx)
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}

	if sum.Scanned != 5 {
		t.Errorf("scanned = %d, want 5 (the finished request is in neither candidate set)", sum.Scanned)
	}
	if sum.Repaired != 2 {
		t.Errorf("repaired = %d, want 2 (the replayed settlement and the stale release)", sum.Repaired)
	}
	if sum.Consistent != 0 {
		t.Errorf("consistent = %d, want 0 from a sweep: a finished request is not a candidate", sum.Consistent)
	}
	// Asked about directly, though, it reports consistent rather than being mistaken
	// for damage.
	if got, err := f.reconciler(reconcile.Config{}).Reconcile(ctx, "s-consistent"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	} else if got.Class != reconcile.ClassConsistent {
		t.Errorf("the finished request classifies as %q, want %q (%s)", got.Class, reconcile.ClassConsistent, got.Detail)
	}
	if sum.Unresolved != 1 {
		t.Errorf("unresolved = %d, want 1", sum.Unresolved)
	}
	if sum.Awaiting != 1 {
		t.Errorf("awaiting = %d, want 1", sum.Awaiting)
	}
	if sum.Orphaned != 1 {
		t.Errorf("orphaned = %d, want 1", sum.Orphaned)
	}
	if sum.Failed != 0 {
		t.Errorf("failed = %d, want 0: unresolved and awaiting records are not errors", sum.Failed)
	}
	if total := sum.Repaired + sum.Consistent + sum.Unresolved + sum.Awaiting + sum.Orphaned + sum.Failed; total != sum.Scanned {
		t.Errorf("the classes sum to %d but %d were scanned; something is counted twice or not at all", total, sum.Scanned)
	}
	// Only the replayed settlement moved money, and only for its own amount.
	if sum.Settled != observedCost {
		t.Errorf("settled = %s, want %s", sum.Settled.CentsString(), observedCost.CentsString())
	}
	if sum.Released != 0 {
		t.Errorf("released = %s, want nothing: the stale release repaired a record, not a hold",
			sum.Released.CentsString())
	}
}

// --- Dry run ------------------------------------------------------------

// TestDryRunWritesNothing is why dry-run exists: an operator can see what a repair
// pass would do to real money before authorizing it.
func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-dry", dollars(1), time.Minute)
	rec := f.begin("req-dry", res)
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	sum, err := f.reconciler(reconcile.Config{DryRun: true}).ReconcilePending(context.Background())
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if sum.Repaired != 1 {
		t.Fatalf("repaired = %d, want the repair to be reported", sum.Repaired)
	}
	if !sum.DryRun || !sum.Results[0].DryRun {
		t.Error("the summary does not report that it was a dry run")
	}
	if sum.Settled != 0 {
		t.Errorf("a dry run totalled %s of settlement; it must not claim money moved",
			sum.Settled.CentsString())
	}
	if got := sum.Results[0].Amount; got != observedCost {
		t.Errorf("dry run reported %s, want the %s it would settle", got.CentsString(), observedCost.CentsString())
	}

	// Nothing changed anywhere.
	if state := f.reservation(res.ID).State; state != ledger.StatePending {
		t.Errorf("reservation state = %q, want it untouched", state)
	}
	if spent := f.totals("team").Spent; spent != 0 {
		t.Errorf("spend = %s, want nothing charged", spent.CentsString())
	}
	after := f.get("req-dry")
	if after.Status != activity.StatusPending {
		t.Errorf("status = %q, want it untouched", after.Status)
	}
	if len(after.Repairs) != 0 {
		t.Errorf("a dry run wrote an audit entry: %+v", after.Repairs)
	}

	// And the real pass then does the work.
	real, err := f.reconciler(reconcile.Config{}).ReconcilePending(context.Background())
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if real.Settled != observedCost {
		t.Errorf("the real pass settled %s, want %s", real.Settled.CentsString(), observedCost.CentsString())
	}
}

// --- Bounds -------------------------------------------------------------

// TestBoundedPassReportsTruncation checks the honesty of a bounded sweep. A pass
// that stopped early and said nothing would let a clean-looking summary imply the
// whole ledger had been examined.
func TestBoundedPassReportsTruncation(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		id := "req-bound-" + string(rune('a'+i))
		res := f.reserve(id, dollars(1), time.Minute)
		f.begin(id, res)
	}

	sum, err := f.reconciler(reconcile.Config{Limit: 2}).ReconcilePending(context.Background())
	if err != nil {
		t.Fatalf("ReconcilePending: %v", err)
	}
	if !sum.Truncated {
		t.Error("a pass that hit its limit did not report truncation")
	}
	if sum.Scanned > 4 {
		t.Errorf("scanned %d, want at most the limit per store", sum.Scanned)
	}
	if sum.Scanned == 0 {
		t.Error("a bounded pass examined nothing")
	}
}

// TestUnknownRequestIsAnError distinguishes "reconcile this specific request" from
// a sweep: a caller naming an id that does not exist has made a mistake, and
// reporting it as a clean result would hide the typo.
func TestUnknownRequestIsAnError(t *testing.T) {
	f := newFixture(t)
	_, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "nope")
	if !errors.Is(err, activity.ErrNotFound) {
		t.Errorf("err = %v, want activity.ErrNotFound", err)
	}
}

// TestDeniedRequestNeedsNoReconciliation covers the record that never took a hold.
// It is complete by construction, and a reconciler that treated it as damage would
// report every denial as an incident.
func TestDeniedRequestNeedsNoReconciliation(t *testing.T) {
	f := newFixture(t)
	rec := activity.Record{
		RequestID:       "req-denied",
		BudgetID:        "team",
		Identity:        identity(),
		Status:          activity.StatusDenied,
		Outcome:         activity.OutcomeBudgetDenied,
		EnforcementMode: engine.ModeEnforce,
		StartedAt:       f.now,
		CompletedAt:     f.now,
	}
	if err := f.acts.Complete(context.Background(), rec); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-denied")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Class != reconcile.ClassConsistent {
		t.Errorf("class = %q, want %q (%s)", got.Class, reconcile.ClassConsistent, got.Detail)
	}
}
