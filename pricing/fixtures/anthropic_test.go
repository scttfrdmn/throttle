package fixtures

import (
	"context"
	"strings"
	"testing"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// anthropicRows indexes the Anthropic fixtures by (model, geography).
func anthropicRows(t *testing.T) map[string]map[string]pricing.Price {
	t.Helper()
	out := map[string]map[string]pricing.Price{}
	for _, p := range Anthropic() {
		if out[p.ProviderModelID] == nil {
			out[p.ProviderModelID] = map[string]pricing.Price{}
		}
		if _, dup := out[p.ProviderModelID][p.InferenceGeo]; dup {
			t.Fatalf("%s has two rows for geography %q", p.ProviderModelID, p.InferenceGeo)
		}
		out[p.ProviderModelID][p.InferenceGeo] = p
	}
	return out
}

// ratioIs reports whether rate a is exactly (num/den) times rate b, without a float.
func ratioIs(a, b pricing.Rate, num, den int64) bool {
	if a.Unit != b.Unit {
		return false
	}
	return int64(a.PerUnit)*den == int64(b.PerUnit)*num
}

// The cache dimensions sit at the published multiples of base input, exactly.
//
// The pricing page gives the cache columns twice over -- as printed dollar figures and
// as multipliers of base input (1.25x for a five-minute write, 2x for an hour, 0.1x for
// a read) -- so the two have to agree or a figure was transcribed wrong. The direction
// that matters most is the cache read: Anthropic's usage counters are additive, so
// cache-read tokens are never also counted as input, and a read priced at the input rate
// overcharges that portion of the request tenfold rather than by a rounding margin.
func TestAnthropicCacheRatesMatchThePublishedMultipliers(t *testing.T) {
	for model, byGeo := range anthropicRows(t) {
		for geo, p := range byGeo {
			what := model + "/" + quotedGeo(geo)
			in := p.Rates[usage.InputTokens]
			for _, c := range []struct {
				d        usage.Dimension
				num, den int64
				printed  string
			}{
				{usage.CacheWrite5mTokens, 5, 4, "1.25x base input"},
				{usage.CacheWrite1hTokens, 2, 1, "2x base input"},
				{usage.CacheReadTokens, 1, 10, "0.1x base input"},
			} {
				got, ok := p.Rates[c.d]
				if !ok {
					t.Errorf("%s has no %s rate, so a request using it would settle as partially "+
						"priced rather than known", what, c.d)
					continue
				}
				if !ratioIs(got, in, c.num, c.den) {
					t.Errorf("%s prices %s at %s per %d against base input %s per %d; the page "+
						"publishes %s", what, c.d, got.PerUnit, got.Unit, in.PerUnit, in.Unit, c.printed)
				}
			}
		}
	}
}

// No row prices the undifferentiated cache-write dimension.
//
// This is the fixture's half of the double-charge guard. Anthropic reports
// cache_creation_input_tokens as the *sum* of its per-TTL children rather than as a
// separate charge, so a catalog that also priced usage.CacheWriteTokens would bill the
// same tokens twice for any adapter that reported both -- and the mistake would look
// like a plausible number, not an error.
func TestAnthropicDoesNotPriceTheAggregateCacheWriteDimension(t *testing.T) {
	for _, p := range Anthropic() {
		if r, ok := p.Rates[usage.CacheWriteTokens]; ok {
			t.Errorf("%s/%q prices %s at %s: that counter is the sum of the per-TTL cache writes, "+
				"so pricing it alongside them charges the same tokens twice",
				p.ProviderModelID, p.InferenceGeo, usage.CacheWriteTokens, r.PerUnit)
		}
	}
}

// The US rows are the base rates times the published 1.1x, on every token dimension and
// on nothing else.
//
// Both halves are load-bearing. The multiplier's scope is enumerated on the page --
// input, output, cache writes, cache reads -- so applying it to the per-search charge
// would be inventing a modifier, and applying it to only some token categories would
// undercharge a cache-heavy US request. The arithmetic is exact integer arithmetic done
// once here, at authoring time, rather than a multiplier applied from memory at
// settlement.
func TestAnthropicUSRowsApplyThePublishedMultiplierToTokensOnly(t *testing.T) {
	tokenDims := []usage.Dimension{
		usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens,
		usage.CacheWrite5mTokens, usage.CacheWrite1hTokens,
	}
	for model, byGeo := range anthropicRows(t) {
		base, ok := byGeo[""]
		if !ok {
			t.Errorf("%s has no geography-less row, so a request that named no inference "+
				"geography could not be priced at admission", model)
			continue
		}
		us, ok := byGeo[anthropicGeoUS]
		if !ok {
			t.Errorf("%s has no %q row, so a request served under US-only inference would settle "+
				"unresolved rather than at the published premium", model, anthropicGeoUS)
			continue
		}
		for _, d := range tokenDims {
			if !ratioIs(us.Rates[d], base.Rates[d], usMultiplierNum, usMultiplierDen) {
				t.Errorf("%s prices %s at %s under %q and %s under global: the page publishes a "+
					"1.1x multiplier on every token category", model, d,
					us.Rates[d].PerUnit, anthropicGeoUS, base.Rates[d].PerUnit)
			}
		}
		if got, want := us.Rates[usage.Searches], base.Rates[usage.Searches]; got != want {
			t.Errorf("%s prices web search at %s per %d under %q and %s per %d under global: the "+
				"data-residency multiplier is published for token categories, and extending it to "+
				"the per-search charge would apply a modifier the page does not state",
				model, got.PerUnit, got.Unit, anthropicGeoUS, want.PerUnit, want.Unit)
		}
	}
}

// Anti-vacuous companion to the test above: the geographies really do differ, by an
// amount no rounding could produce.
//
// Written against a named figure rather than a ratio so that a change collapsing the two
// sheets into one fails here loudly. A million Opus 5 input tokens is $5.00 globally and
// $5.50 under US-only inference; a million output tokens is $25.00 and $27.50.
func TestAnthropicGeographyChangesTheBill(t *testing.T) {
	rows := anthropicRows(t)["claude-opus-5"]
	for _, c := range []struct {
		geo       string
		in, out   money.Money
		otherName string
	}{
		{"", 5_000_000, 25_000_000, "the global sheet"},
		{anthropicGeoGlobal, 5_000_000, 25_000_000, "the global sheet"},
		{anthropicGeoUS, 5_500_000, 27_500_000, "the US sheet"},
	} {
		p, ok := rows[c.geo]
		if !ok {
			t.Fatalf("claude-opus-5 has no row for geography %q", c.geo)
		}
		if got := p.Rates[usage.InputTokens].PerUnit; got != c.in {
			t.Errorf("claude-opus-5/%s input = %s, want %s", quotedGeo(c.geo), got, c.in)
		}
		if got := p.Rates[usage.OutputTokens].PerUnit; got != c.out {
			t.Errorf("claude-opus-5/%s output = %s, want %s", quotedGeo(c.geo), got, c.out)
		}
	}
}

// The geography-less row and the explicit global row price identically, and both exist.
//
// The same pairing the OpenAI fixture needs for its tier-less and default rows, for the
// same two reasons. A request that named no inference geography has nothing to key on at
// admission and resolves through the unqualified row; the named global row is what makes
// global reachable as a captured alternate, since only a qualified row becomes one and
// the response reports where inference actually ran. If they ever disagreed, the same
// request would cost different amounts depending on whether the caller spelled the
// default out.
func TestAnthropicGlobalRowMatchesTheGeographylessRow(t *testing.T) {
	for model, byGeo := range anthropicRows(t) {
		unqualified, ok := byGeo[""]
		if !ok {
			t.Errorf("%s has no geography-less row", model)
			continue
		}
		global, ok := byGeo[anthropicGeoGlobal]
		if !ok {
			t.Errorf("%s has no explicit %q row, so a request admitted without a geography that "+
				"came back reporting global would have no frozen alternate to re-price from",
				model, anthropicGeoGlobal)
			continue
		}
		for d, want := range unqualified.Rates {
			got, ok := global.Rates[d]
			if !ok {
				t.Errorf("%s: the global row has no %s rate but the geography-less row does", model, d)
				continue
			}
			if got != want {
				t.Errorf("%s: the global row prices %s at %s per %d and the geography-less row at "+
					"%s per %d; both describe standard pricing and must agree",
					model, d, got.PerUnit, got.Unit, want.PerUnit, want.Unit)
			}
		}
	}
}

// Web search is priced per search, at the published figure and on the published
// denominator.
//
// Pinned as a figure because the unit is the part that goes wrong: $10 per 1,000
// searches quoted per search would overcharge by a thousand, and the mistake is
// invisible in a row of numbers.
func TestAnthropicPricesWebSearchPerThousandSearches(t *testing.T) {
	for _, p := range Anthropic() {
		r, ok := p.Rates[usage.Searches]
		if !ok {
			t.Errorf("%s/%q has no %s rate, so a web-search request could not be fully priced",
				p.ProviderModelID, p.InferenceGeo, usage.Searches)
			continue
		}
		if r.Unit != 1_000 || r.PerUnit != 10*money.PerDollar {
			t.Errorf("%s/%q prices %s at %s per %d, want $10 per 1000",
				p.ProviderModelID, p.InferenceGeo, usage.Searches, r.PerUnit, r.Unit)
		}
	}
}

// The Anthropic fixtures are dated by retrieval, not by a backdated effective date.
//
// The same choice the OpenAI fixture makes and for the same reason, pinned separately
// because "backdate it like Bedrock does" is the obvious-looking edit in either file.
// The page's only stated date is the retracted Sonnet 5 increase, which is a date on
// which nothing happens.
func TestAnthropicFixturesAreDatedByRetrieval(t *testing.T) {
	for _, p := range Anthropic() {
		if !p.Provenance.EffectiveFrom.IsZero() {
			t.Errorf("%s/%q claims an effective date of %s: Anthropic publishes none for its token "+
				"rates, and inventing one misrepresents a retrieval as a publication",
				p.ProviderModelID, p.InferenceGeo, p.Provenance.EffectiveFrom)
		}
		if p.Provenance.RetrievedAt.IsZero() {
			t.Errorf("%s/%q records no retrieval date, so nothing dates the figure at all",
				p.ProviderModelID, p.InferenceGeo)
		}
		if !strings.Contains(p.Provenance.Source, "claude.com") {
			t.Errorf("%s/%q has source %q, which does not name where the figures were read from",
				p.ProviderModelID, p.InferenceGeo, p.Provenance.Source)
		}
	}
}

// Direct Anthropic and Bedrock are separate access providers, and their model IDs do not
// cross over.
//
// One of throttle's model-identity invariants: the same underlying Claude reached two
// ways is two access paths, sharing a publisher and a model family and nothing else.
// Pricing keys on (access provider, provider model ID), and this is the check that the
// key is doing its job -- a Bedrock inference-profile ID must not be priceable as a
// direct model, and a bare Anthropic model ID must not be priceable as a Bedrock one.
func TestAnthropicAndBedrockClaudeDoNotShareAPriceSheet(t *testing.T) {
	cat, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	// Priceable on the access provider that serves it.
	for _, ok := range []usage.ModelIdentity{
		{AccessProvider: "anthropic", ProviderModelID: "claude-haiku-4-5", Operation: "messages"},
		{AccessProvider: "aws-bedrock", ProviderModelID: "anthropic.claude-haiku-4-5-20251001-v1:0"},
	} {
		if _, err := cat.Capture(ok, at); err != nil {
			t.Errorf("Capture(%s/%s): %v", ok.AccessProvider, ok.ProviderModelID, err)
		}
	}
	// And not on the other one.
	for _, crossed := range []usage.ModelIdentity{
		{AccessProvider: "aws-bedrock", ProviderModelID: "claude-haiku-4-5"},
		{AccessProvider: "anthropic", ProviderModelID: "anthropic.claude-haiku-4-5-20251001-v1:0", Operation: "messages"},
	} {
		if _, err := cat.Capture(crossed, at); err == nil {
			t.Errorf("Capture(%s/%s) succeeded: direct Anthropic and Bedrock are different access "+
				"paths with different price sheets, and a model ID from one must not resolve "+
				"against the other", crossed.AccessProvider, crossed.ProviderModelID)
		}
	}
}

// There is no phantom long-context band: a very large request prices at the same
// per-token rate as a small one.
//
// Claude 4.6 and later include the full million-token context window at standard
// pricing, so the honest fixture states one input rate and the test pins that decision.
// A hundredfold difference in size produces exactly a hundredfold difference in cost,
// which is what "no band" means arithmetically. The values straddle the threshold a
// contributor might be tempted to add -- 200k tokens -- from either side.
func TestAnthropicHasNoLongContextBand(t *testing.T) {
	cat, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	id := usage.ModelIdentity{
		AccessProvider:  "anthropic",
		ProviderModelID: "claude-sonnet-5",
		Operation:       "messages",
	}
	cost := func(t *testing.T, tokens int64) money.Money {
		t.Helper()
		q, err := cat.Quote(context.Background(), id,
			usage.New(map[usage.Dimension]int64{usage.InputTokens: tokens}), at)
		if err != nil {
			t.Fatalf("Quote(%d input tokens): %v", tokens, err)
		}
		if !q.Cost.Known() {
			t.Fatalf("Quote(%d input tokens) = %s (%s): %s",
				tokens, q.Cost.Amount, q.Cost.State(), q.Cost.Reason)
		}
		return q.Cost.Amount
	}

	// Sonnet 5 input is $2/MTok: 9k tokens is $0.018, 900k tokens is $1.80.
	small, large := cost(t, 9_000), cost(t, 900_000)
	if want := money.Money(18_000); small != want {
		t.Errorf("9k input tokens = %s, want %s", small, want)
	}
	if want := money.Money(1_800_000); large != want {
		t.Errorf("900k input tokens = %s, want %s", large, want)
	}
	if int64(large) != int64(small)*100 {
		t.Errorf("900k tokens costs %s and 9k costs %s: a hundred times the tokens must cost a "+
			"hundred times as much, because the full context window is priced at the standard "+
			"rate and there is no premium band to select", large, small)
	}
}

// An unlisted Anthropic model is unpriced, not free.
//
// The fixture covers a handful of models out of a catalog that grows without notice, and
// the honest outcome for the rest is "throttle cannot price this" -- which the engine
// turns into a denial under enforcement. Catalog recognition is not a condition of
// identity: the model is perfectly representable, it just has no rate.
func TestUnknownAnthropicModelIsUnpricedNotFree(t *testing.T) {
	cat, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	id := usage.ModelIdentity{
		AccessProvider:  "anthropic",
		ProviderModelID: "claude-opus-6-not-yet-announced",
		Operation:       "messages",
	}
	if _, err := cat.Capture(id, at); err == nil {
		t.Fatal("Capture of an unlisted model succeeded; a small fixture set must not silently " +
			"price a model it has never heard of")
	}
	q, err := cat.Quote(context.Background(), id,
		usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000}), at)
	if err == nil {
		t.Error("Quote of an unlisted model returned no error")
	}
	if q.Cost.State() != usage.CostUnknown {
		t.Errorf("cost state = %q (%s), want %q: a million tokens on an unlisted model is not free",
			q.Cost.State(), q.Cost.Amount, usage.CostUnknown)
	}
}

// quotedGeo names a geography for a failure message, distinguishing the unqualified row
// from a row that names one.
func quotedGeo(geo string) string {
	if geo == "" {
		return "(no geography)"
	}
	return `"` + geo + `"`
}
