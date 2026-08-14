package reconcile_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/reconcile"
	"github.com/scttfrdmn/throttle/usage"
)

// A request served in an inference geography the captured quote never priced stays
// unresolved through reconciliation, on the same terms as an unpriced service tier.
//
// The point of proving it here rather than only in pricing: reconciliation is where the
// guarantee has to be structural. This package holds no catalog at all, so a repair can
// only replay the frozen quote against the persisted normalized usage — there is no
// second provider request to make, no SDK to import, and nothing for a catalogue update
// to reach. A geography re-rates every token dimension, so the bands that were captured
// bound this one's cost in neither direction and the honest answer is to leave the hold
// encumbered and name what would resolve it.
func TestUncapturedInferenceGeoStaysUnresolvedThroughReconciliation(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-geo", dollars(1), time.Minute)
	rec := f.begin("req-geo", res)

	// Captured for a model priced by geography, and served in one that was not.
	q := quote()
	q.InferenceGeo = "global"
	alt := quote()
	alt.InferenceGeo = "us"
	q.Alternates = map[string]pricing.CapturedQuote{"|geo=us": alt}
	rec.Quote = q
	rec.Identity = identity()
	rec.Identity.InferenceGeo = "eu-invented-later"
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	// Anti-vacuous: the frozen rates do price this usage, to the figure every other
	// replay test settles at. That figure is what the repair must refuse to claim.
	if priced, err := q.Price(observedUsage()); err != nil || priced.Cost.Amount != observedCost {
		t.Fatalf("the captured rates priced %s (err %v); this test needs a known %s",
			priced.Cost.Amount.CentsString(), err, observedCost.CentsString())
	}

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-geo")
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
		t.Fatalf("money = %q: a request whose geography was never priced may not settle", got.Money)
	}
	if state := f.reservation(res.ID).State; state != ledger.StatePending {
		t.Errorf("state = %q, want the hold left encumbered", state)
	}
	if spent := f.totals("team").Spent; spent != 0 {
		t.Errorf("spend = %s, want nothing charged", spent.CentsString())
	}

	out := f.get("req-geo")
	if out.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want %q", out.Status, activity.StatusUnresolved)
	}
	if out.ActualCost.Known() {
		t.Errorf("cost = %v, want an unresolved cost", out.ActualCost)
	}
	if out.ActualCost.Amount == observedCost {
		t.Errorf("cost = %s: the repair priced one geography by rates captured for another",
			observedCost.CentsString())
	}
	if !strings.Contains(out.ActualCost.Reason, "eu-invented-later") {
		t.Errorf("reason %q must name the geography that served the call, so an operator knows "+
			"what pricing would resolve it", out.ActualCost.Reason)
	}
	// The observed geography survives the repair: overwriting it with the admitted one
	// would erase the only record of what actually needs pricing.
	if out.Identity.InferenceGeo != "eu-invented-later" {
		t.Errorf("recorded geography = %q, want the observed one preserved", out.Identity.InferenceGeo)
	}

	// Repeated passes converge rather than eventually settling.
	again, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-geo")
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if again.Class != reconcile.ClassUnresolved || again.Money != reconcile.MoneyNone {
		t.Errorf("second pass = %q/%q, want it still unresolved with no money moved",
			again.Class, again.Money)
	}
}

// A geography the quote did capture replays from the frozen alternate, deterministically
// and from durable facts alone.
//
// The complement of the test above, and the reason the alternate is captured at
// admission rather than looked up: a crash after the usage was persisted must settle at
// the geography's own rate without a second provider call. The alternate here is priced
// at 1.1x the primary, which is small enough that pricing it at the primary's rate would
// be a plausible-looking figure rather than an obvious one -- so the assertion names both
// numbers.
func TestCapturedInferenceGeoReplaysFromTheFrozenAlternate(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-geo-alt", dollars(1), time.Minute)
	rec := f.begin("req-geo-alt", res)

	q := quote()
	q.InferenceGeo = "global"
	// 1.1x on both dimensions: $3.30/M in, $16.50/M out.
	alt := quote()
	alt.InferenceGeo = "us"
	alt.Rates = map[usage.Dimension]pricing.Rate{
		usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(3)+300_000),
		usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(16)+500_000),
	}
	alt.Provenance.Source = "test-catalog-us"
	q.Alternates = map[string]pricing.CapturedQuote{"|geo=us": alt}

	rec.Quote = q
	rec.Identity = identity()
	rec.Identity.InferenceGeo = "us"
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-geo-alt")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Money != reconcile.MoneySettled {
		t.Fatalf("money = %q, want the request settled from the frozen alternate (%s)",
			got.Money, got.Detail)
	}
	// 100k at $3.30/M = $0.33; 20k at $16.50/M = $0.33. Total $0.66.
	want := observedCost + 60_000
	if got.Amount != want {
		t.Errorf("settled %s, want %s from the geography's own frozen rates",
			got.Amount.CentsString(), want.CentsString())
	}
	if got.Amount == observedCost {
		t.Errorf("settled %s: the primary geography's rates were replayed for a request served "+
			"in another", observedCost.CentsString())
	}
	if spent := f.totals("team").Spent; spent != want {
		t.Errorf("spend = %s, want %s", spent.CentsString(), want.CentsString())
	}

	out := f.get("req-geo-alt")
	if !out.ActualCost.Known() {
		t.Errorf("cost = %v, want known: the geography that served the call was priced", out.ActualCost)
	}
	if out.Identity.InferenceGeo != "us" {
		t.Errorf("recorded geography = %q, want it preserved", out.Identity.InferenceGeo)
	}

	// A second pass moves no more money: settlement is still exactly once.
	again, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-geo-alt")
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if again.Money == reconcile.MoneySettled {
		t.Error("the second pass settled again")
	}
	if spent := f.totals("team").Spent; spent != want {
		t.Errorf("spend after a second pass = %s, want %s", spent.CentsString(), want.CentsString())
	}
}

// A quote persisted before the geography axis existed still reconciles.
//
// Written against a stored record rather than a freshly built quote on purpose: the
// alternates map is keyed by a selector encoding whose no-geography form is the bare
// service tier it was keyed by before, and the compatibility claim is about the bytes in
// the ledger. A repair of a record written by an earlier build is exactly the case that
// cannot be re-captured.
func TestTierOnlyQuotesFromBeforeTheGeoAxisStillReconcile(t *testing.T) {
	f := newFixture(t)
	res := f.reserve("req-legacy", dollars(1), time.Minute)
	rec := f.begin("req-legacy", res)

	// The shape an older build wrote: a tier-qualified primary, alternates keyed by a
	// bare tier string, and no geography anywhere.
	q := quote()
	q.ServiceTier = "standard"
	alt := quote()
	alt.ServiceTier = "flex"
	alt.Rates = map[usage.Dimension]pricing.Rate{
		usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(1)+500_000),
		usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(7)+500_000),
	}
	q.Alternates = map[string]pricing.CapturedQuote{"flex": alt}

	rec.Quote = q
	rec.Identity = identity()
	rec.Identity.ServiceTier = "flex"
	rec.ActualUsage = observedUsage()
	f.complete(rec)

	got, err := f.reconciler(reconcile.Config{}).Reconcile(context.Background(), "req-legacy")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.Money != reconcile.MoneySettled {
		t.Fatalf("money = %q, want the stored tier alternate to still resolve after the geography "+
			"axis exists (%s)", got.Money, got.Detail)
	}
	// Half the primary's rates on both dimensions, so half the figure.
	want := observedCost / 2
	if got.Amount != want {
		t.Errorf("settled %s, want %s from the stored flex alternate",
			got.Amount.CentsString(), want.CentsString())
	}
	if got.Amount == observedCost {
		t.Errorf("settled %s: the stored alternate was not found and the primary's rates were "+
			"used instead", observedCost.CentsString())
	}
}
