package pricing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

func dollars(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return m
}

var at = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func sonnet(t *testing.T) pricing.Price {
	t.Helper()
	return pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "anthropic.claude-sonnet-4-5-20250929-v1:0",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "15.00")),
		},
		Provenance: pricing.Provenance{Source: "test", Currency: "USD"},
	}
}

func identity(modelID string) usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: modelID,
		Operation:       "converse",
	}
}

// Rate.Cost is the arithmetic every dollar figure in throttle passes through, so
// it is checked against hand-computed values rather than against itself.
func TestRateCost(t *testing.T) {
	perMillion3 := pricing.PerMillion(usage.InputTokens, dollars(t, "3.00"))

	cases := []struct {
		name string
		rate pricing.Rate
		n    int64
		want money.Money
	}{
		{"a million tokens costs the per-million rate", perMillion3, 1_000_000, dollars(t, "3.00")},
		{"zero tokens are free", perMillion3, 0, 0},
		{"a thousand tokens is a thousandth", perMillion3, 1_000, dollars(t, "0.003")},
		{"one token is three microdollars", perMillion3, 1, 3},
		{"per-thousand rates scale too",
			pricing.PerThousand(usage.OutputTokens, dollars(t, "0.015")), 2_000, dollars(t, "0.03")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.rate.Cost(c.n)
			if err != nil {
				t.Fatalf("Cost(%d): %v", c.n, err)
			}
			if got != c.want {
				t.Errorf("Cost(%d) = %s, want %s", c.n, got, c.want)
			}
		})
	}
}

// A fractional microdollar has to round somewhere, and half-up is the documented
// choice. The bound that matters is that it never drifts: the error stays below
// one microdollar rather than accumulating.
func TestRateCostRoundsHalfUp(t *testing.T) {
	// 1 microdollar per 3 units: 1/3 rounds down, 2/3 rounds up.
	r := pricing.Rate{Dimension: usage.InputTokens, PerUnit: 1, Unit: 3}

	cases := []struct {
		n    int64
		want money.Money
	}{
		{1, 0}, // 0.333 -> 0
		{2, 1}, // 0.667 -> 1
		{3, 1}, // exactly 1
		{4, 1}, // 1.333 -> 1
		{5, 2}, // 1.667 -> 2
	}
	for _, c := range cases {
		got, err := r.Cost(c.n)
		if err != nil {
			t.Fatalf("Cost(%d): %v", c.n, err)
		}
		if got != c.want {
			t.Errorf("Cost(%d) = %d, want %d", c.n, int64(got), int64(c.want))
		}
	}

	// Exactly one half rounds up, away from zero, in both directions.
	half := pricing.Rate{Dimension: usage.InputTokens, PerUnit: 1, Unit: 2}
	if got, _ := half.Cost(1); got != 1 {
		t.Errorf("0.5 rounded to %d, want 1", int64(got))
	}
	credit := pricing.Rate{Dimension: usage.InputTokens, PerUnit: -1, Unit: 2}
	if got, _ := credit.Cost(1); got != -1 {
		t.Errorf("-0.5 rounded to %d, want -1 (symmetric)", int64(got))
	}
}

// The intermediate product of a per-million rate and a large token count exceeds
// int64 long before the division brings it back down. That is precisely why the
// arithmetic uses big.Int, so a plausible large batch must not overflow.
func TestRateCostLargeCountsDoNotOverflow(t *testing.T) {
	r := pricing.PerMillion(usage.InputTokens, dollars(t, "15.00"))

	// A billion tokens: 15e6 microdollars * 1e9 = 1.5e16 before division, which
	// would overflow a naive int64 multiply at higher rates and counts.
	got, err := r.Cost(1_000_000_000)
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if want := dollars(t, "15000.00"); got != want {
		t.Errorf("Cost = %s, want %s", got, want)
	}

	// Far enough past int64 microdollars that the result genuinely cannot be
	// represented; that must be an error, not a wrapped number.
	if _, err := r.Cost(int64(1) << 62); err == nil {
		t.Error("an unrepresentable cost should error rather than wrap")
	}
}

func TestRateCostRejectsBadUnit(t *testing.T) {
	for _, unit := range []int64{0, -1000} {
		r := pricing.Rate{Dimension: usage.InputTokens, PerUnit: 1000, Unit: unit}
		if _, err := r.Cost(10); err == nil {
			t.Errorf("Unit=%d should be rejected", unit)
		}
	}
}

func TestStaticQuote(t *testing.T) {
	cat, err := pricing.NewStatic(sonnet(t))
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  1_000_000,
		usage.OutputTokens: 100_000,
	})
	q, err := cat.Quote(context.Background(), identity("anthropic.claude-sonnet-4-5-20250929-v1:0"), u, at)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if !q.Cost.Known() {
		t.Fatalf("cost should be known, got %v", q.Cost)
	}
	// $3.00 of input plus $1.50 of output.
	if want := dollars(t, "4.50"); q.Cost.Amount != want {
		t.Errorf("cost = %s, want %s", q.Cost.Amount, want)
	}
	if q.PerDimension[usage.InputTokens] != dollars(t, "3.00") {
		t.Errorf("input breakdown = %s, want $3.00", q.PerDimension[usage.InputTokens])
	}
	if q.Provenance.Source != "test" {
		t.Errorf("provenance was not carried through: %+v", q.Provenance)
	}
}

// The central rule: an unknown model must not be priced at zero, and must not be
// mapped to something that looks similar.
func TestStaticQuoteUnknownModelIsUnknownNotZero(t *testing.T) {
	cat, err := pricing.NewStatic(sonnet(t))
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})
	q, err := cat.Quote(context.Background(), identity("anthropic.claude-something-new-v9:0"), u, at)
	if !errors.Is(err, pricing.ErrNoPrice) {
		t.Fatalf("err = %v, want ErrNoPrice", err)
	}
	if q.Cost.Known() {
		t.Errorf("an unknown model must not yield a known cost, got %s", q.Cost.Amount)
	}
	if q.Cost.Reason == "" {
		t.Error("an unknown cost must explain itself")
	}
	// The distinction that matters: unknown is not zero.
	if q.Cost.Or(-1) != -1 {
		t.Error("Or() must return the fallback for an unknown cost")
	}
}

// A model that starts billing a dimension the catalog has no rate for must not
// produce a partial total, which would understate the bill while looking precise.
func TestStaticQuoteUnpricedDimensionIsUnknown(t *testing.T) {
	cat, err := pricing.NewStatic(sonnet(t))
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:      1_000_000,
		usage.CacheWriteTokens: 500_000, // sonnet fixture has no cache rate
	})
	q, err := cat.Quote(context.Background(), identity("anthropic.claude-sonnet-4-5-20250929-v1:0"), u, at)
	if !errors.Is(err, pricing.ErrNoRate) {
		t.Fatalf("err = %v, want ErrNoRate", err)
	}
	if q.Cost.Known() {
		t.Errorf("a partially priceable usage must not report a known cost, got %s", q.Cost.Amount)
	}
	if len(q.Unpriced) != 1 || q.Unpriced[0] != usage.CacheWriteTokens {
		t.Errorf("Unpriced = %v, want [cache_write_tokens]", q.Unpriced)
	}
	// The breakdown of what *could* be priced is still useful for diagnosis.
	if q.PerDimension[usage.InputTokens] != dollars(t, "3.00") {
		t.Errorf("the priceable part should still be broken down, got %v", q.PerDimension)
	}
}

// A dimension reported as zero with no rate is not a pricing gap: nothing was
// consumed, so nothing is owed.
func TestStaticQuoteZeroCountUnpricedDimensionIsFine(t *testing.T) {
	cat, err := pricing.NewStatic(sonnet(t))
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:      1_000_000,
		usage.CacheReadTokens:  0,
		usage.CacheWriteTokens: 0,
	})
	q, err := cat.Quote(context.Background(), identity("anthropic.claude-sonnet-4-5-20250929-v1:0"), u, at)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if !q.Cost.Known() || q.Cost.Amount != dollars(t, "3.00") {
		t.Errorf("cost = %v, want a known $3.00", q.Cost)
	}
}

// Region and tier narrow a price. A specific entry must win over a general one,
// or a priority-tier request would be billed at the standard rate.
func TestStaticQuotePrefersMoreSpecificPrice(t *testing.T) {
	general := sonnet(t)
	general.Provenance.Source = "general"

	priority := sonnet(t)
	priority.ServiceTier = "priority"
	priority.Provenance.Source = "priority"
	priority.Rates = map[usage.Dimension]pricing.Rate{
		usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "6.00")),
		usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "30.00")),
	}

	// Registered general-first, so ordering cannot be an accident of insertion.
	cat, err := pricing.NewStatic(general, priority)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})

	id := identity("anthropic.claude-sonnet-4-5-20250929-v1:0")
	id.ServiceTier = "priority"
	q, err := cat.Quote(context.Background(), id, u, at)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Cost.Amount != dollars(t, "6.00") || q.Provenance.Source != "priority" {
		t.Errorf("priority request priced at %s from %q, want $6.00 from priority",
			q.Cost.Amount, q.Provenance.Source)
	}

	// A request with no tier still gets the general price.
	q, err = cat.Quote(context.Background(), identity("anthropic.claude-sonnet-4-5-20250929-v1:0"), u, at)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Cost.Amount != dollars(t, "3.00") || q.Provenance.Source != "general" {
		t.Errorf("untiered request priced at %s from %q, want $3.00 from general",
			q.Cost.Amount, q.Provenance.Source)
	}
}

// A user with negotiated pricing must be able to override the shipped numbers
// without editing an adapter or the catalog.
func TestStaticOverrideWins(t *testing.T) {
	cat, err := pricing.NewStatic(sonnet(t))
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	discounted := sonnet(t)
	discounted.Rates = map[usage.Dimension]pricing.Rate{
		usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "1.50")),
		usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "7.50")),
	}
	discounted.Provenance = pricing.Provenance{Currency: "USD"}
	if err := cat.Override(discounted); err != nil {
		t.Fatalf("Override: %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})
	q, err := cat.Quote(context.Background(), identity("anthropic.claude-sonnet-4-5-20250929-v1:0"), u, at)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if q.Cost.Amount != dollars(t, "1.50") {
		t.Errorf("cost = %s, want the overridden $1.50", q.Cost.Amount)
	}
	// Provenance must say the number came from a local override, or a surprising
	// figure is untraceable.
	if q.Provenance.Source != "local-override" {
		t.Errorf("provenance source = %q, want local-override", q.Provenance.Source)
	}
}

// A price that takes effect next month must not be applied to today's request.
func TestStaticQuoteRespectsEffectiveDate(t *testing.T) {
	future := sonnet(t)
	future.Provenance.Source = "future"
	future.Provenance.EffectiveFrom = at.Add(24 * time.Hour)
	future.Rates = map[usage.Dimension]pricing.Rate{
		usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "9.00")),
	}

	cat, err := pricing.NewStatic(future)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})
	if _, err := cat.Quote(context.Background(), identity("anthropic.claude-sonnet-4-5-20250929-v1:0"), u, at); !errors.Is(err, pricing.ErrNoPrice) {
		t.Fatalf("a not-yet-effective price should not apply: err = %v", err)
	}

	// It applies once it is in effect.
	q, err := cat.Quote(context.Background(), identity("anthropic.claude-sonnet-4-5-20250929-v1:0"), u, at.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("Quote after the effective date: %v", err)
	}
	if q.Cost.Amount != dollars(t, "9.00") {
		t.Errorf("cost = %s, want $9.00", q.Cost.Amount)
	}
}

func TestStaticAddRejectsInvalidPrices(t *testing.T) {
	base := sonnet(t)

	cases := []struct {
		name   string
		mutate func(p *pricing.Price)
	}{
		{"no access provider", func(p *pricing.Price) { p.AccessProvider = "" }},
		{"no model id", func(p *pricing.Price) { p.ProviderModelID = "" }},
		{"no rates", func(p *pricing.Price) { p.Rates = nil }},
		{"zero unit", func(p *pricing.Price) {
			p.Rates = map[usage.Dimension]pricing.Rate{
				usage.InputTokens: {Dimension: usage.InputTokens, PerUnit: 3, Unit: 0},
			}
		}},
		{"negative rate", func(p *pricing.Price) {
			p.Rates = map[usage.Dimension]pricing.Rate{
				usage.InputTokens: {Dimension: usage.InputTokens, PerUnit: -3, Unit: 1_000_000},
			}
		}},
		{"key disagrees with the declared dimension", func(p *pricing.Price) {
			p.Rates = map[usage.Dimension]pricing.Rate{
				usage.InputTokens: {Dimension: usage.OutputTokens, PerUnit: 3, Unit: 1_000_000},
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mutate(&p)
			if _, err := pricing.NewStatic(p); err == nil {
				t.Error("an invalid price should be refused at registration")
			}
		})
	}
}

// The catalog is read concurrently by every governed request, so a data race here
// would be a production race.
func TestStaticQuoteConcurrent(t *testing.T) {
	cat, err := pricing.NewStatic(sonnet(t))
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000})

	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 200; j++ {
				if _, err := cat.Quote(context.Background(), identity("anthropic.claude-sonnet-4-5-20250929-v1:0"), u, at); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatalf("concurrent Quote: %v", err)
		}
	}
}
