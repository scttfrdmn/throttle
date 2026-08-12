package fixtures

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// These tests are about the fixtures as data, not about the pricing engine. The
// engine's own suite proves that rates multiply correctly; what is checked here is
// that the numbers being fed to it say what the published price tables say, and that
// they keep saying so after somebody edits this package.

// at is an instant after every fixture's effective date, so a price is never excluded
// merely for predating the query.
var at = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

// Reasoning tokens are billed as output tokens, so the two rates must be equal on
// every OpenAI row.
//
// This is the invariant the OpenAI fixture's own comment promises, and it is asserted
// rather than merely written down because the failure is silent in both directions.
// Making reasoning cheaper understates the bill for exactly the models most likely to
// produce a lot of it; making it dearer overstates it. Neither shows up as an error --
// only as a number that is quietly wrong.
func TestOpenAIReasoningIsPricedAsOutput(t *testing.T) {
	for _, p := range OpenAI() {
		out, ok := p.Rates[usage.OutputTokens]
		if !ok {
			t.Errorf("%s/%q has no output rate", p.ProviderModelID, p.ServiceTier)
			continue
		}
		reasoning, ok := p.Rates[usage.ReasoningTokens]
		if !ok {
			// A missing reasoning rate is not a cheaper reasoning rate: it makes an
			// ordinary reasoning request settle as a partial cost, because the response
			// reports a dimension the quote cannot price.
			t.Errorf("%s/%q has no reasoning rate, so a reasoning response would settle "+
				"as partially priced", p.ProviderModelID, p.ServiceTier)
			continue
		}
		if reasoning.PerUnit != out.PerUnit || reasoning.Unit != out.Unit {
			t.Errorf("%s/%q prices reasoning at %s per %d and output at %s per %d: "+
				"OpenAI bills reasoning tokens as output tokens, at the output rate",
				p.ProviderModelID, p.ServiceTier,
				reasoning.PerUnit, reasoning.Unit, out.PerUnit, out.Unit)
		}
	}
}

// Cached input is cheaper than fresh input, everywhere it is priced at all.
//
// The discount is the whole reason the cached dimension exists, so a row where the two
// are equal -- or where cached is dearer -- is a transcription error rather than a
// pricing decision.
func TestOpenAICachedInputIsDiscounted(t *testing.T) {
	for _, p := range OpenAI() {
		cached, ok := p.Rates[usage.CacheReadTokens]
		if !ok {
			continue
		}
		in := p.Rates[usage.InputTokens]
		if cached.Unit != in.Unit {
			t.Fatalf("%s/%q quotes input and cached input on different denominators (%d vs %d), "+
				"so this comparison is meaningless",
				p.ProviderModelID, p.ServiceTier, in.Unit, cached.Unit)
		}
		if cached.PerUnit >= in.PerUnit {
			t.Errorf("%s/%q prices cached input at %s and fresh input at %s: a cache read is "+
				"cheaper than a fresh read, so this is a transcription error",
				p.ProviderModelID, p.ServiceTier, cached.PerUnit, in.PerUnit)
		}
	}
}

// everyFixture is every price this package produces, across both access providers.
func everyFixture() []pricing.Price {
	all := append([]pricing.Price(nil), Bedrock()...)
	all = append(all, agentCoreRuntime()...)
	return append(all, OpenAI()...)
}

// Every fixture carries provenance identifying it as a fixture.
//
// A price with no source is a number a reader cannot argue with. The Source strings
// both name themselves as fixtures, which is what keeps a surprising cost traceable to
// this file rather than mistaken for a live rate.
func TestEveryFixtureCarriesFixtureProvenance(t *testing.T) {
	for _, p := range everyFixture() {
		what := p.AccessProvider + "/" + p.ProviderModelID
		prov := p.Provenance
		if !strings.Contains(prov.Source, "fixture") {
			t.Errorf("%s has source %q, which does not say it is a fixture: a stale "+
				"hand-entered rate must not be able to pass for live pricing", what, prov.Source)
		}
		if prov.Version == "" {
			t.Errorf("%s has no version, so a stale fixture cannot be told from a fresh one", what)
		}
		if prov.Currency != "USD" {
			t.Errorf("%s has currency %q, want USD: money is microdollars and a "+
				"mislabelled currency would mix units", what, prov.Currency)
		}
		// One of the two dates must be real. OpenAI publishes no effective date for its
		// token rates and AWS does, so requiring a specific one would force whichever
		// catalog lacks it to invent a date -- but a row with neither is a price from
		// nowhere.
		if prov.EffectiveFrom.IsZero() && prov.RetrievedAt.IsZero() {
			t.Errorf("%s carries neither an effective date nor a retrieval date", what)
		}
	}
}

// The OpenAI fixtures are dated by retrieval, not by a backdated effective date.
//
// OpenAI's pricing page states no effective date for its token rates. Recording an
// invented one would dress a retrieval date up as a publication date, so RetrievedAt
// carries the fact that is actually known and EffectiveFrom stays zero. This pins that
// choice, because "just backdate it like Bedrock does" is the obvious-looking edit.
func TestOpenAIFixturesAreDatedByRetrieval(t *testing.T) {
	for _, p := range OpenAI() {
		if !p.Provenance.EffectiveFrom.IsZero() {
			t.Errorf("%s/%q claims an effective date of %s: OpenAI publishes none for its "+
				"token rates, and inventing one misrepresents a retrieval as a publication",
				p.ProviderModelID, p.ServiceTier, p.Provenance.EffectiveFrom)
		}
		if p.Provenance.RetrievedAt.IsZero() {
			t.Errorf("%s/%q records no retrieval date, so nothing dates the figure at all",
				p.ProviderModelID, p.ServiceTier)
		}
		if !strings.Contains(p.Provenance.Source, "openai.com") {
			t.Errorf("%s/%q has source %q, which does not name where the figures were read from",
				p.ProviderModelID, p.ServiceTier, p.Provenance.Source)
		}
	}
}

// The premium tiers are not multiples of the standard rates, so none of them could have
// been computed.
//
// This is the fixture file's central claim about itself, and it is worth an assertion:
// if every tier were a uniform multiple, a future contributor would be right to replace
// the transcribed table with arithmetic. They are not, and the counterexample is that
// gpt-5.1 doubles at priority while gpt-5-mini goes to 1.8x.
func TestOpenAITiersAreNotDerivable(t *testing.T) {
	ratio := func(t *testing.T, model, tier string) *big64 {
		t.Helper()
		var std, alt money.Money
		for _, p := range OpenAI() {
			if p.ProviderModelID != model {
				continue
			}
			switch p.ServiceTier {
			case openAITierDefault:
				std = p.Rates[usage.InputTokens].PerUnit
			case tier:
				alt = p.Rates[usage.InputTokens].PerUnit
			}
		}
		if std == 0 || alt == 0 {
			t.Fatalf("%s has no %s or default input rate to compare", model, tier)
		}
		return &big64{num: int64(alt), den: int64(std)}
	}

	luna := ratio(t, "gpt-5.6-luna", openAITierPriority)
	mini := ratio(t, "gpt-5-mini", openAITierPriority)
	if luna.equal(mini) {
		t.Errorf("gpt-5.6-luna and gpt-5-mini share a priority multiplier (%s): if every tier "+
			"were a uniform multiple of standard, these tables could be computed rather than "+
			"transcribed -- check whether a rate was derived instead of read", luna)
	}
}

// big64 is an exact ratio of two rates, for comparing multipliers without a float.
type big64 struct{ num, den int64 }

func (r *big64) equal(o *big64) bool { return r.num*o.den == o.num*r.den }
func (r *big64) String() string {
	return money.Money(r.num).String() + "/" + money.Money(r.den).String()
}

// A model absent from the fixtures produces an explicit unknown cost, not a zero.
//
// This is what makes a deliberately small catalog safe. The fixtures cover four OpenAI
// models out of dozens, and the honest outcome for the rest is "throttle cannot price
// this", which the engine turns into a denial under enforcement.
func TestUnknownOpenAIModelIsUnpricedNotFree(t *testing.T) {
	cat, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	id := usage.ModelIdentity{
		AccessProvider:  "openai",
		ProviderModelID: "gpt-6-not-yet-announced",
		Operation:       "responses",
	}
	if _, err := cat.Capture(id, at); err == nil {
		t.Fatal("Capture of an unlisted model succeeded; a small fixture set must not " +
			"silently price a model it has never heard of")
	}

	q, err := cat.Quote(context.Background(), id,
		usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000}), at)
	if err == nil {
		t.Error("Quote of an unlisted model returned no error")
	}
	if q.Cost.Known() {
		t.Errorf("Quote of an unlisted model reports a known cost of %s", q.Cost.Amount)
	}
	if q.Cost.State() != usage.CostUnknown {
		t.Errorf("cost state = %q, want %q: a million tokens on an unlisted model is not free",
			q.Cost.State(), usage.CostUnknown)
	}
}

// One catalog holds both access providers, and their entries do not collide.
//
// Prices key on (access provider, provider model ID), so this is really a check that
// the key is doing its job -- and that a mixed catalog prices an OpenAI request from
// OpenAI rates rather than from whatever happened to be added first.
func TestOneCatalogServesBothProviders(t *testing.T) {
	cat, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	seen := map[string]int{}
	for _, id := range cat.Models() {
		seen[id.AccessProvider]++
	}
	for _, provider := range []string{"aws-bedrock", "openai"} {
		if seen[provider] == 0 {
			t.Errorf("the catalog has no %s models", provider)
		}
	}

	// A million fresh input tokens on gpt-5.1 at the standard rate is $1.25 exactly --
	// the published figure, which is also proof this row was not resolved against a
	// Bedrock price.
	got, err := cat.Quote(context.Background(),
		usage.ModelIdentity{
			AccessProvider:  "openai",
			ProviderModelID: "gpt-5.1",
			Operation:       "responses",
		},
		usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000}), at)
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if want := money.Money(1_250_000); got.Cost.Amount != want {
		t.Errorf("a million gpt-5.1 input tokens = %s, want %s", got.Cost.Amount, want)
	}
}

// The tier-less row and the explicit default row price identically, and both exist.
//
// Both are needed and for different reasons: a request that named no tier has nothing
// to key on at admission and resolves through the tier-less row, while the named
// "default" row is what makes default reachable as a captured alternate when OpenAI
// serves a flex request on standard. If they ever disagreed, the same request would
// cost different amounts depending on whether the tier was named -- which is the kind
// of discrepancy nobody would think to look for.
func TestOpenAIDefaultTierMatchesTheTierlessRow(t *testing.T) {
	byTier := map[string]map[usage.Dimension]pricing.Rate{}
	for _, p := range OpenAI() {
		if p.ProviderModelID != "gpt-5-mini" {
			continue
		}
		byTier[p.ServiceTier] = p.Rates
	}

	tierless, ok := byTier[""]
	if !ok {
		t.Fatal("gpt-5-mini has no tier-less row, so a request naming no service tier " +
			"could not be priced at admission")
	}
	named, ok := byTier[openAITierDefault]
	if !ok {
		t.Fatal("gpt-5-mini has no explicit default row, so a request served on default " +
			"after being admitted as flex would have no frozen alternate to re-price from")
	}
	for d, want := range tierless {
		got, ok := named[d]
		if !ok {
			t.Errorf("the default row has no %s rate but the tier-less row does", d)
			continue
		}
		if got != want {
			t.Errorf("%s: default row prices %s per %d, tier-less row %s per %d; the two "+
				"describe the same standard rate and must agree",
				d, got.PerUnit, got.Unit, want.PerUnit, want.Unit)
		}
	}
}

// Every fixture rate is exact: a whole number of microdollars over a stated
// denominator.
//
// The figures are written as strings and parsed rather than declared as float
// constants, and this is what proves the parse landed where it was meant to. A rate
// that needed a seventh decimal place would have failed to parse, and a rate quoted on
// a zero denominator would divide by nothing at pricing time.
func TestEveryFixtureRateIsExact(t *testing.T) {
	for _, p := range everyFixture() {
		for d, r := range p.Rates {
			if r.Unit <= 0 {
				t.Errorf("%s/%s/%q has a non-positive denominator %d",
					p.AccessProvider, p.ProviderModelID, d, r.Unit)
			}
			if r.PerUnit <= 0 {
				t.Errorf("%s/%s/%q is priced at %s: a zero rate says the dimension is free, "+
					"which is different from having no rate for it",
					p.AccessProvider, p.ProviderModelID, d, r.PerUnit)
			}
			if r.Dimension != "" && r.Dimension != d {
				t.Errorf("%s/%s: rate keyed as %q carries dimension %q",
					p.AccessProvider, p.ProviderModelID, d, r.Dimension)
			}
		}
	}
}
