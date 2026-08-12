package fixtures

import (
	"time"

	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// OpenAI fixture provenance.
//
// Source names both what these figures are -- hand-entered fixtures -- and where
// they came from, because OpenAI publishes no machine-readable rate card. There is
// no price API, and the OpenAPI specification contains no pricing data at all:
// every mention of price in it is a prose link to the page below. So these numbers
// were read off that page by a person, and the provenance says so rather than
// implying a feed.
const (
	OpenAISource         = "throttle-fixture:developers.openai.com/api/docs/pricing"
	OpenAIFixtureVersion = "2026-08-12-fixture-1"
)

// openAIRetrievedAt is the day the figures below were read off the pricing page.
//
// EffectiveFrom is deliberately left zero. OpenAI's pricing page states no
// effective date for its token prices -- it dates individual policy changes, but
// not the rates themselves -- and a zero EffectiveFrom is how Provenance says "the
// catalog does not track it". Backdating an invented date would dress a retrieval
// date up as a publication date, which is the one thing provenance exists to
// prevent. RetrievedAt carries the fact that is actually known.
var openAIRetrievedAt = time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

// openAIPrice builds one price row from per-million-token dollar figures.
//
// Empty cachedIn or cacheWrite means the pricing page prints a dash for that cell,
// which is not the same as zero: it means the dimension is not separately priced
// for this model. Reasoning is priced at the output rate rather than given a rate
// of its own, because that is what OpenAI bills -- see the note on reasoningRate.
func openAIPrice(modelID, tier, in, cachedIn, cacheWrite, out string) pricing.Price {
	outRate := dollars(out)
	rates := map[usage.Dimension]pricing.Rate{
		usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(in)),
		usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, outRate),
		// Reasoning tokens are billed as output tokens, at the output rate. They get
		// their own entry at that same rate because the adapter reports them as a
		// separate dimension -- see normalizeUsage -- and a dimension the response
		// reports with no rate in the quote would make an ordinary reasoning request
		// settle as a partial cost. Equal rates are asserted by test, so a later edit
		// cannot quietly make reasoning cheaper or dearer than the output it is part of.
		usage.ReasoningTokens: pricing.PerMillion(usage.ReasoningTokens, outRate),
	}
	if cachedIn != "" {
		rates[usage.CacheReadTokens] = pricing.PerMillion(usage.CacheReadTokens, dollars(cachedIn))
	}
	if cacheWrite != "" {
		rates[usage.CacheWriteTokens] = pricing.PerMillion(usage.CacheWriteTokens, dollars(cacheWrite))
	}
	return pricing.Price{
		AccessProvider:  "openai",
		ProviderModelID: modelID,
		ServiceTier:     tier,
		Rates:           rates,
		Provenance: pricing.Provenance{
			Source:      OpenAISource,
			Version:     OpenAIFixtureVersion,
			RetrievedAt: openAIRetrievedAt,
			Currency:    "USD",
		},
	}
}

// OpenAI service tiers that carry their own published price table.
//
// These are the values that appear in a response's service_tier, which is what
// pricing keys on. Two of them need explaining:
//
//   - "priority" is what the Fast mode table prices. OpenAI renamed Priority
//     processing to Fast mode in July 2026 but kept the wire value: a request
//     asking for either fast or priority comes back reporting priority, so the
//     served tier this catalog must recognize is priority.
//   - The empty tier is the ordinary case. A request that sets no service_tier
//     defaults to auto, and auto resolves to whatever the project is configured
//     for -- so at admission there is no tier to name yet. The tier-less row
//     carries standard rates, and settlement re-prices against whichever tier the
//     response says actually served the call.
//
// Deliberately absent: "scale" and "batch". Batch is a different API this adapter
// does not call. Scale appears in the service_tier enum but on no pricing table,
// so there is no rate to record for it.
const (
	openAITierDefault  = "default"
	openAITierPriority = "priority"
	openAITierFlex     = "flex"
)

// OpenAI returns fixture prices for a handful of current OpenAI models.
//
// # What this is not
//
// It is not the OpenAI pricing page. It is a deliberately small set chosen to
// exercise every priced dimension the Responses adapter can produce -- fresh
// input, cached input, cache writes, output, and reasoning -- across the three
// service tiers that price differently. A model absent from here produces an
// explicit unknown cost, which is the correct outcome, not a zero.
//
// # Why every figure is written out
//
// The tiers are not multiples of each other, so none of them can be derived. Fast
// mode is 2x standard input for gpt-5.1 and gpt-5.6-luna but 1.8x for gpt-5-mini;
// cached input is a tenth of input for the gpt-5 family and a half for gpt-4o-mini;
// gpt-4o-mini is on no Flex table at all. Every cell below is transcribed from the
// published table for that tier, because computing one would have been wrong.
//
// # Known limitation: context-length bands
//
// OpenAI prices long context (over 272K tokens) at roughly double the input rate,
// as separate columns on the same row. These fixtures encode the short-context
// rates only. throttle's dimension model prices a dimension at one rate per
// identity and has no notion of a request falling into a length band, so a
// very large request is under-priced rather than unpriced. That is a real gap and
// it is recorded as one.
func OpenAI() []pricing.Price {
	type entry struct {
		id string
		// Each tier is transcribed independently: in, cached input, cache write,
		// output. An empty cell means the published table prints a dash there; an
		// entirely absent tier means the model is on no table for it.
		standard [4]string
		priority [4]string
		flex     [4]string
	}
	entries := []entry{
		// The gpt-5.6 family is the only one publishing a cache-write price. Cache
		// writes are billed at 1.25x the uncached input rate on these models and carry
		// no additional fee on earlier ones, which is why every other row's cache-write
		// cell is a dash rather than a zero.
		{
			id:       "gpt-5.6-luna",
			standard: [4]string{"0.20", "0.02", "0.25", "1.20"},
			priority: [4]string{"0.40", "0.04", "0.50", "2.40"},
			flex:     [4]string{"0.10", "0.01", "0.125", "0.60"},
		},
		{
			id:       "gpt-5.1",
			standard: [4]string{"1.25", "0.125", "", "10.00"},
			priority: [4]string{"2.50", "0.25", "", "20.00"},
			flex:     [4]string{"0.625", "0.0625", "", "5.00"},
		},
		{
			id:       "gpt-5-mini",
			standard: [4]string{"0.25", "0.025", "", "2.00"},
			priority: [4]string{"0.45", "0.045", "", "3.60"},
			flex:     [4]string{"0.125", "0.0125", "", "1.00"},
		},
		{
			// On no Flex table, so it has no flex row here. A request this model serves
			// on flex therefore has no frozen rate for the tier that ran, and settles as
			// unresolved rather than at the standard rate -- see CapturedQuote.For. That
			// is the honest outcome: this model is priced by tier, and the tiers that
			// were priced bound a flex request's cost in neither direction.
			id:       "gpt-4o-mini",
			standard: [4]string{"0.15", "0.075", "", "0.60"},
			priority: [4]string{"0.25", "0.125", "", "1.00"},
		},
	}

	out := make([]pricing.Price, 0, len(entries)*4)
	for _, e := range entries {
		// Two rows carry the standard rates, at different specificities, and both are
		// needed. The tier-less row prices a request that named no tier, since auto
		// resolves server-side and there is nothing to key on at admission. The
		// explicit "default" row is what makes default reachable as a captured
		// alternate: a request admitted as flex that OpenAI serves on default must
		// re-price from frozen default rates, and only a named tier becomes an
		// alternate.
		out = append(out,
			openAIPrice(e.id, "", e.standard[0], e.standard[1], e.standard[2], e.standard[3]),
			openAIPrice(e.id, openAITierDefault, e.standard[0], e.standard[1], e.standard[2], e.standard[3]),
		)
		// A slice rather than a map, so the catalog is seeded in a fixed order. Prices
		// for different tiers never shadow each other, but an unstable seed order is
		// the kind of thing that makes one test in a hundred runs fail differently.
		for _, t := range []struct {
			tier  string
			rates [4]string
		}{
			{openAITierPriority, e.priority},
			{openAITierFlex, e.flex},
		} {
			tier, rates := t.tier, t.rates
			if rates[0] == "" {
				// The model is on no published table for this tier. Recording nothing is
				// the honest outcome; inventing a multiple of the standard rate would not
				// be, and these tiers are demonstrably not uniform multiples.
				continue
			}
			out = append(out, openAIPrice(e.id, tier, rates[0], rates[1], rates[2], rates[3]))
		}
	}
	return out
}
