package report

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	activitysqlite "github.com/scttfrdmn/throttle/activity/sqlite"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	ledgersqlite "github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// The read model is tested against the real stores rather than against fakes.
// Its whole job is to ask the durable stores the right questions, and a fake that
// answered them the way this package expects would test the expectation instead of
// the behaviour.

func dollars(d int64) money.Money { return money.Money(d) * money.PerDollar }

func cents(c int64) money.Money { return money.Money(c) * money.PerDollar / 100 }

// micros is a figure in the storage unit itself, for per-request token costs that are
// smaller than a cent. Written out rather than as a fraction of a dollar because the
// arithmetic a test is pinning is arithmetic in microdollars.
func micros(m int64) money.Money { return money.Money(m) }

// base is the reference instant: the start of a month, so period boundaries land
// on round numbers in assertions.
var base = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// monthDuration is the length of the August 2026 period the fixtures use.
const monthDuration = 31 * 24 * time.Hour

func monthly(id, parent string, allocation money.Money) budget.Definition {
	return budget.Definition{
		ID:         id,
		ParentID:   parent,
		Name:       id,
		Allocation: allocation,
		Recurrence: budget.RecurMonthly,
		AnchorAt:   base,
	}
}

// world is a ledger, an activity store, a reporter over both, and a movable clock.
type world struct {
	t    *testing.T
	ctx  context.Context
	led  *ledgersqlite.Store
	acts *activitysqlite.Store
	rep  *Reporter

	// now is what the reporter's clock returns. Tests move it rather than sleeping.
	now time.Time
}

// newWorld builds a reporter over both stores.
func newWorld(t *testing.T) *world {
	t.Helper()
	w := newWorldIn(t, t.TempDir(), true)
	return w
}

// newLedgerOnlyWorld builds a reporter with no activity store, which is the
// configuration a dashboard runs in when telemetry is not being recorded.
func newLedgerOnlyWorld(t *testing.T) *world {
	t.Helper()
	return newWorldIn(t, t.TempDir(), false)
}

func newWorldIn(t *testing.T, dir string, withActivity bool) *world {
	t.Helper()
	ctx := context.Background()

	led, err := ledgersqlite.Open(ctx, filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { led.Close() })

	w := &world{t: t, ctx: ctx, led: led, now: base.Add(monthDuration / 2)}
	cfg := Config{Ledger: led, Clock: func() time.Time { return w.now }}

	if withActivity {
		acts, err := activitysqlite.Open(ctx, filepath.Join(dir, "activity.db"))
		if err != nil {
			t.Fatalf("open activity store: %v", err)
		}
		t.Cleanup(func() { acts.Close() })
		w.acts = acts
		cfg.Activity = acts
	}

	rep, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.rep = rep
	return w
}

// define stores a definition and materializes the period containing the clock.
func (w *world) define(def budget.Definition) ledger.Period {
	w.t.Helper()
	if err := w.led.PutDefinition(w.ctx, def); err != nil {
		w.t.Fatalf("PutDefinition(%q): %v", def.ID, err)
	}
	p, err := w.led.EnsurePeriod(w.ctx, def.ID, w.now)
	if err != nil {
		w.t.Fatalf("EnsurePeriod(%q): %v", def.ID, err)
	}
	return p
}

// period wraps an envelope as a materialized period, for the arithmetic tests
// that need no store at all.
func period(env budget.Envelope) ledger.Period {
	return ledger.Period{
		ID:         env.ID + "@0",
		BudgetID:   env.ID,
		Envelope:   env,
		State:      ledger.StateOpen,
		CarryFinal: true,
	}
}

// ledgerScope is the scope a test reads totals for directly, to check that a read
// through the reporter did not move money.
func ledgerScope(budgetID, periodID string) ledger.Scope {
	return ledger.Scope{BudgetID: budgetID, PeriodID: periodID}
}

// totals is a ledger position with no expired holds.
func totals(spent, reserved money.Money) ledger.Totals {
	t := ledger.Totals{Spent: spent, Reserved: reserved}
	if reserved > 0 {
		t.PendingCount = 1
	}
	return t
}

// engineOver builds an engine over the same ledger, so a test can pin the read
// model's arithmetic to the engine's.
func engineOver(w *world) (*engine.Engine, error) {
	return engine.New(engine.Config{Ledger: w.led, Clock: func() time.Time { return w.now }})
}

// unlimited builds ceilings that refuse nothing, for tests that are not about
// admission.
func unlimited(ids ...string) map[string]money.Money {
	out := make(map[string]money.Money, len(ids))
	for _, id := range ids {
		out[id] = money.Max
	}
	return out
}

// spend reserves and settles in one step, which is what puts money in the ledger.
func (w *world) spend(id, budgetID string, amount money.Money, at time.Time, chain ...string) ledger.Charge {
	w.t.Helper()
	if len(chain) == 0 {
		chain = []string{budgetID}
	}
	rv, err := w.led.Reserve(w.ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID: id, BudgetID: budgetID, RequestID: "req-" + id,
			Amount: amount, EstimatedCost: amount, CreatedAt: at,
			ExpiresAt: at.Add(time.Hour), Lease: time.Hour,
		},
		Ceilings: unlimited(chain...),
		Now:      at,
	})
	if err != nil {
		w.t.Fatalf("Reserve(%q): %v", id, err)
	}
	c, err := w.led.Settle(w.ctx, ledger.Settlement{
		ReservationID: rv.ID, ActualCost: amount, CompletedAt: at,
	})
	if err != nil {
		w.t.Fatalf("Settle(%q): %v", id, err)
	}
	return c
}

// hold reserves without settling, leaving headroom encumbered.
func (w *world) hold(id, budgetID string, amount money.Money, at time.Time, chain ...string) ledger.Reservation {
	w.t.Helper()
	if len(chain) == 0 {
		chain = []string{budgetID}
	}
	rv, err := w.led.Reserve(w.ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID: id, BudgetID: budgetID, RequestID: "req-" + id,
			Amount: amount, EstimatedCost: amount, CreatedAt: at,
			ExpiresAt: at.Add(time.Hour), Lease: time.Hour,
			Identity: bedrockIdentity(),
		},
		Ceilings: unlimited(chain...),
		Now:      at,
	})
	if err != nil {
		w.t.Fatalf("Reserve(%q): %v", id, err)
	}
	return rv
}

// record writes an activity record.
func (w *world) record(rec activity.Record) activity.Record {
	w.t.Helper()
	if err := w.acts.Complete(w.ctx, rec); err != nil {
		w.t.Fatalf("Complete(%q): %v", rec.RequestID, err)
	}
	return rec
}

// bedrockIdentity is the canonical three-facet identity: the access path is AWS
// Bedrock, the publisher is Anthropic, and the model is a third thing again.
func bedrockIdentity() usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		Publisher:       "anthropic",
		Family:          "claude-sonnet",
		CanonicalModel:  "claude-sonnet-4",
		ProviderModelID: "anthropic.claude-sonnet-4-20250514-v1:0",
		Operation:       "converse",
		Region:          "us-east-1",
	}
}

// settledRecord is an ordinary priced request.
func settledRecord(requestID, budgetID, periodID string, cost money.Money, at time.Time) activity.Record {
	return activity.Record{
		RequestID:     requestID,
		ReservationID: "res-" + requestID,
		BudgetID:      budgetID,
		Scopes:        []activity.Scope{{BudgetID: budgetID, PeriodID: periodID}},
		Identity:      bedrockIdentity(),
		Estimate: usage.Estimate{
			Identity: bedrockIdentity(),
			Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 1000}),
			Cost:     usage.KnownCost(cost),
			Quality:  usage.QualityConservative,
		},
		Quote: pricing.CapturedQuote{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: bedrockIdentity().ProviderModelID,
			Provenance: pricing.Provenance{
				Source: "aws-price-list", Version: "2026-08-01", RetrievedAt: at, Currency: "USD",
			},
		},
		Reserved: cost,
		ActualUsage: usage.New(map[usage.Dimension]int64{
			usage.InputTokens:     1200,
			usage.OutputTokens:    400,
			usage.CacheReadTokens: 90,
		}),
		ActualCost:      usage.KnownCost(cost),
		EnforcementMode: engine.ModeEnforce,
		Status:          activity.StatusSettled,
		Outcome:         activity.OutcomeSuccess,
		StartedAt:       at,
		CompletedAt:     at.Add(2 * time.Second),
		Latency:         2 * time.Second,
		ProviderLatency: 1900 * time.Millisecond,
	}
}
