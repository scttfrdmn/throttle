package pricing_test

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

func sonnetIdentity() usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "anthropic.claude-sonnet-4-20250514-v1:0",
		Operation:       "converse",
		Region:          "us-east-1",
	}
}

func staticCatalog(t *testing.T, in, out string) *pricing.Static {
	t.Helper()
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetIdentity().ProviderModelID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, in)),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, out)),
		},
		Provenance: pricing.Provenance{Source: "test", Version: "v1"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	return cat
}

// The core guarantee of the captured quote: a price change landing between
// admission and settlement must not change what an in-flight request costs.
// Otherwise the amount reserved and the amount charged come from different price
// sheets, and no later reader can tell which.
func TestCapturedQuoteSurvivesCatalogChange(t *testing.T) {
	cat := staticCatalog(t, "3.00", "15.00")

	quote, err := cat.Capture(sonnetIdentity(), at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The catalog is refreshed mid-request. An override outranks the shipped entry,
	// so a live re-query would now price this request very differently.
	if err := cat.Override(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetIdentity().ProviderModelID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "99.00")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "99.00")),
		},
	}); err != nil {
		t.Fatalf("Override: %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  1_000_000,
		usage.OutputTokens: 1_000_000,
	})

	priced, err := quote.Price(u)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if want := dollars(t, "18.00"); priced.Cost.Amount != want {
		t.Errorf("settled cost = %s, want %s from the captured rates, not the refreshed catalog",
			priced.Cost.Amount, want)
	}

	// Confirm the catalog really did change, so the test above is not vacuous.
	live, err := cat.Quote(context.Background(), sonnetIdentity(), u, at)
	if err != nil {
		t.Fatalf("live Quote: %v", err)
	}
	if live.Cost.Amount == priced.Cost.Amount {
		t.Fatal("the catalog did not actually change; the test proves nothing")
	}
}

// A captured quote must survive a process restart, or a request in flight when the
// process died can never be reconciled on its original basis.
func TestCapturedQuoteRoundTripsThroughJSON(t *testing.T) {
	cat := staticCatalog(t, "3.00", "15.00")
	quote, err := cat.Capture(sonnetIdentity(), at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	s, err := pricing.MarshalQuote(quote)
	if err != nil {
		t.Fatalf("MarshalQuote: %v", err)
	}
	back, err := pricing.UnmarshalQuote(s)
	if err != nil {
		t.Fatalf("UnmarshalQuote: %v", err)
	}

	if back.ProviderModelID != quote.ProviderModelID {
		t.Errorf("ProviderModelID = %q, want %q", back.ProviderModelID, quote.ProviderModelID)
	}
	if back.Provenance.Source != "test" || back.Provenance.Version != "v1" {
		t.Errorf("Provenance = %+v, want the captured provenance preserved", back.Provenance)
	}
	if !back.CapturedAt.Equal(at) {
		t.Errorf("CapturedAt = %s, want %s", back.CapturedAt, at)
	}

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  1_000_000,
		usage.OutputTokens: 1_000_000,
	})
	before, _ := quote.Price(u)
	after, err := back.Price(u)
	if err != nil {
		t.Fatalf("Price after restart: %v", err)
	}
	if after.Cost.Amount != before.Cost.Amount {
		t.Errorf("cost after restart = %s, want %s: a persisted quote must price identically",
			after.Cost.Amount, before.Cost.Amount)
	}
	if after.Cost.Completeness != usage.CostKnown {
		t.Errorf("completeness = %q, want known", after.Cost.Completeness)
	}
}

// A local override is a negotiated rate, and its provenance must survive capture:
// months later, "why was this cheaper?" has to be answerable.
func TestCapturedQuotePreservesOverrideProvenance(t *testing.T) {
	cat := staticCatalog(t, "3.00", "15.00")
	if err := cat.Override(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetIdentity().ProviderModelID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "1.00")),
		},
	}); err != nil {
		t.Fatalf("Override: %v", err)
	}

	quote, err := cat.Capture(sonnetIdentity(), at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if quote.Provenance.Source != "local-override" {
		t.Errorf("Provenance.Source = %q, want local-override", quote.Provenance.Source)
	}

	back, err := pricing.UnmarshalQuote(mustMarshalQuote(t, quote))
	if err != nil {
		t.Fatalf("UnmarshalQuote: %v", err)
	}
	if back.Provenance.Source != "local-override" {
		t.Errorf("persisted Provenance.Source = %q, want local-override", back.Provenance.Source)
	}
}

func mustMarshalQuote(t *testing.T, q pricing.CapturedQuote) string {
	t.Helper()
	s, err := pricing.MarshalQuote(q)
	if err != nil {
		t.Fatalf("MarshalQuote: %v", err)
	}
	return s
}

// Usage containing a dimension the quote has no rate for is a partial cost: the
// priced amount is a floor, and the record must name what it could not price.
// Reporting the floor as a total would understate the bill.
func TestQuotePartialCostNamesUnpricedDimensions(t *testing.T) {
	cat := staticCatalog(t, "3.00", "15.00")
	quote, err := cat.Capture(sonnetIdentity(), at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The model starts billing for a dimension the price sheet predates.
	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:            1_000_000,
		usage.OutputTokens:           1_000_000,
		usage.Dimension("video-sec"): 30,
	})

	priced, err := quote.Price(u)
	if err == nil {
		t.Fatal("Price must report that a billable dimension had no rate")
	}
	if priced.Cost.Completeness != usage.CostPartial {
		t.Fatalf("completeness = %q, want partial", priced.Cost.Completeness)
	}
	if want := dollars(t, "18.00"); priced.Cost.Amount != want {
		t.Errorf("floor = %s, want %s", priced.Cost.Amount, want)
	}
	if len(priced.Cost.Unpriced) != 1 || priced.Cost.Unpriced[0] != usage.Dimension("video-sec") {
		t.Errorf("Unpriced = %v, want [video-sec]", priced.Cost.Unpriced)
	}
	if priced.Cost.Known() {
		t.Error("a partial cost must not report itself as known")
	}
	// The rendering has to advertise incompleteness, or a dashboard shows $18.00
	// flat for a request that cost more.
	if !strings.Contains(priced.Cost.String(), "+") {
		t.Errorf("String() = %q, want a floor marker", priced.Cost.String())
	}
}

// A dimension reported as zero needs no rate: nothing was consumed, so nothing is
// owed, and treating its absence as unpriceable would make ordinary requests
// unresolved.
func TestQuoteZeroCountDimensionNeedsNoRate(t *testing.T) {
	cat := staticCatalog(t, "3.00", "15.00")
	quote, err := cat.Capture(sonnetIdentity(), at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:      1_000,
		usage.OutputTokens:     1_000,
		usage.CacheWriteTokens: 0,
	})
	priced, err := quote.Price(u)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if priced.Cost.Completeness != usage.CostKnown {
		t.Errorf("completeness = %q, want known: a zero-count dimension owes nothing", priced.Cost.Completeness)
	}
}

// An unpriced model yields an explicit unknown, never a zero. A zero would be
// indistinguishable from a free request and would silently understate spend.
func TestQuoteUnknownModelIsNotZero(t *testing.T) {
	cat := staticCatalog(t, "3.00", "15.00")
	id := sonnetIdentity()
	id.ProviderModelID = "some.model-released-yesterday"

	if _, err := cat.Capture(id, at); err == nil {
		t.Fatal("Capture must refuse an unknown model")
	}

	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000})
	q, err := cat.Quote(context.Background(), id, u, at)
	if err == nil {
		t.Fatal("Quote must report an unknown model")
	}
	if q.Cost.Completeness != usage.CostUnknown {
		t.Errorf("completeness = %q, want unknown", q.Cost.Completeness)
	}
	if q.Cost.Known() {
		t.Error("an unpriced model must not produce a known cost")
	}
	if q.Cost.AtLeast() != 0 {
		t.Errorf("AtLeast = %s, want 0", q.Cost.AtLeast())
	}
	// The distinction that matters: this is not "$0.00", it is "we do not know".
	if q.Cost.String() == money.Money(0).String() {
		t.Errorf("String() = %q, an unknown cost must not render as a zero amount", q.Cost.String())
	}
	if q.Cost.Reason == "" {
		t.Error("an unknown cost must say why")
	}
}

// tinyCatalog prices n dimensions at PerUnit microdollars per unit units, which is
// how the rounding-drift tests manufacture many sub-microdollar lines.
func tinyCatalog(t *testing.T, n int, perUnit money.Money, unit int64) (*pricing.Static, usage.Usage) {
	t.Helper()
	rates := make(map[usage.Dimension]pricing.Rate, n)
	counts := make(map[usage.Dimension]int64, n)
	for i := 0; i < n; i++ {
		d := usage.Dimension(dimName(i))
		rates[d] = pricing.Rate{Dimension: d, PerUnit: perUnit, Unit: unit}
		counts[d] = 1
	}
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "tiny",
		Rates:           rates,
		Provenance:      pricing.Provenance{Source: "test"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	return cat, usage.New(counts)
}

func tinyIdentity() usage.ModelIdentity {
	return usage.ModelIdentity{AccessProvider: "aws-bedrock", ProviderModelID: "tiny", Operation: "converse"}
}

func dimName(i int) string {
	return "d" + string(rune('a'+i/26)) + string(rune('a'+i%26))
}

// Rounding happens once per charge, not once per dimension. With enough small
// dimensions, per-line rounding drifts by a predictable amount in one direction;
// this pins the boundary so a refactor cannot quietly move it back.
func TestRoundingHappensOncePerCharge(t *testing.T) {
	// 1 microdollar per 2 units at 1 unit each: exactly half a microdollar per
	// line, which rounds up to a whole microdollar on its own.
	const dims = 100
	cat, u := tinyCatalog(t, dims, 1, 2)

	quote, err := cat.Capture(tinyIdentity(), at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	priced, err := quote.Price(u)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}

	// 100 dimensions x 1/2 microdollar = exactly 50 microdollars.
	if priced.Cost.Amount != money.Money(50) {
		t.Errorf("cost = %d microdollars, want 50: the charge must round once, not per dimension",
			priced.Cost.Amount)
	}
	// Per-dimension rounding would have produced 100 -- double. The breakdown still
	// reports the individually rounded lines, which is why it is documented as not
	// summing to the total.
	var lines money.Money
	for _, m := range priced.PerDimension {
		lines += m
	}
	if lines != money.Money(dims) {
		t.Errorf("per-dimension total = %d, want %d: the drift this test guards must be real", lines, dims)
	}
}

// The same property from the other direction: many dimensions that each round
// down must not vanish. Rounding per line would price this whole request at zero.
func TestManyTinyDimensionsDoNotRoundAwayToNothing(t *testing.T) {
	const dims = 40
	// A tenth of a microdollar per line.
	cat, u := tinyCatalog(t, dims, 1, 10)

	quote, err := cat.Capture(tinyIdentity(), at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	priced, err := quote.Price(u)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	// 40 x 1/10 = exactly 4 microdollars. Per-line rounding gives 0.
	if priced.Cost.Amount != money.Money(4) {
		t.Errorf("cost = %d microdollars, want 4: tiny per-line costs must not round away", priced.Cost.Amount)
	}
}

// Round is the documented boundary, so its rule is pinned directly: half away
// from zero, symmetric across the sign so a credit reverses a charge exactly.
func TestRoundHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		num, den int64
		want     money.Money
	}{
		{1, 2, 1},   // exactly half rounds up
		{-1, 2, -1}, // and its negation rounds down by the same distance
		{1, 3, 0},
		{2, 3, 1},
		{-2, 3, -1},
		{3, 2, 2},
		{-3, 2, -2},
		{0, 1, 0},
		{7, 1, 7},
	}
	for _, c := range cases {
		got, err := pricing.Round(new(big.Rat).SetFrac64(c.num, c.den))
		if err != nil {
			t.Fatalf("Round(%d/%d): %v", c.num, c.den, err)
		}
		if got != c.want {
			t.Errorf("Round(%d/%d) = %d, want %d", c.num, c.den, got, c.want)
		}
	}
}

// A tier the provider substituted must be priced from rates captured at
// admission, not from a live re-query: which tier served the call is a fact about
// what happened, but the price sheet is still frozen.
func TestCapturedQuoteCarriesTierAlternates(t *testing.T) {
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetIdentity().ProviderModelID,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			},
			Provenance: pricing.Provenance{Source: "test"},
		},
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetIdentity().ProviderModelID,
			ServiceTier:     "flex",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "1.50")),
			},
			Provenance: pricing.Provenance{Source: "test-flex"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	quote, err := cat.Capture(sonnetIdentity(), at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	served := sonnetIdentity()
	served.ServiceTier = "flex"

	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})
	priced, err := quote.For(served).Price(u)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if want := dollars(t, "1.50"); priced.Cost.Amount != want {
		t.Errorf("flex cost = %s, want %s", priced.Cost.Amount, want)
	}

	// Alternates must survive persistence too, or a restart loses the ability to
	// price a substituted tier on its original basis.
	back, err := pricing.UnmarshalQuote(mustMarshalQuote(t, quote))
	if err != nil {
		t.Fatalf("UnmarshalQuote: %v", err)
	}
	after, err := back.For(served).Price(u)
	if err != nil {
		t.Fatalf("Price after restart: %v", err)
	}
	if after.Cost.Amount != priced.Cost.Amount {
		t.Errorf("flex cost after restart = %s, want %s", after.Cost.Amount, priced.Cost.Amount)
	}

	// An unknown tier falls back to the admitted rates rather than to nothing.
	odd := sonnetIdentity()
	odd.ServiceTier = "tier-invented-later"
	fallback, err := quote.For(odd).Price(u)
	if err != nil {
		t.Fatalf("Price with unknown tier: %v", err)
	}
	if want := dollars(t, "3.00"); fallback.Cost.Amount != want {
		t.Errorf("unknown-tier cost = %s, want the admitted %s", fallback.Cost.Amount, want)
	}
}

// An empty stored quote reads back as a zero quote, not as corruption: a request
// admitted in monitor mode with no price never had one to store.
func TestUnmarshalQuoteToleratesEmptyPayloads(t *testing.T) {
	for _, s := range []string{"", "{}", "null"} {
		q, err := pricing.UnmarshalQuote(s)
		if err != nil {
			t.Errorf("UnmarshalQuote(%q): %v", s, err)
		}
		if q.Valid() {
			t.Errorf("UnmarshalQuote(%q) reported a usable quote", s)
		}
	}
}

// An invalid quote must refuse to price rather than return a zero cost.
func TestInvalidQuoteRefusesToPrice(t *testing.T) {
	var q pricing.CapturedQuote
	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000})
	priced, err := q.Price(u)
	if err == nil {
		t.Fatal("an empty quote must not price anything")
	}
	if priced.Cost.Completeness != usage.CostUnknown {
		t.Errorf("completeness = %q, want unknown", priced.Cost.Completeness)
	}
	if priced.Cost.Amount != 0 || priced.Cost.Known() {
		t.Error("an empty quote must not produce a known amount")
	}
}
