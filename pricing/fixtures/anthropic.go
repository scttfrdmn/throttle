package fixtures

import (
	"time"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// Anthropic fixture provenance.
//
// Source names both what these figures are -- hand-entered fixtures -- and the page
// they were read from. Anthropic publishes no machine-readable rate card: the
// pricing page is prose and tables, and the OpenAPI specification carries no rates.
// So a person read them, and the provenance says so rather than implying a feed.
const (
	AnthropicSource         = "throttle-fixture:platform.claude.com/docs/en/about-claude/pricing"
	AnthropicFixtureVersion = "2026-08-12-fixture-1"
)

// anthropicRetrievedAt is the day the figures below were read off the pricing page.
//
// EffectiveFrom is deliberately left zero, for the reason already written into
// openAIRetrievedAt: the page states no effective date for its token rates, and
// backdating the retrieval date would dress it up as a publication date. RetrievedAt
// carries the fact that is actually known.
//
// The one date the page does state is a negative: Claude Sonnet 5's launch price
// "is now the standard price" and the increase scheduled for 1 September 2026 "will
// not occur". So there is no promotional rate here whose end date needs encoding,
// and no "today's rate" is being flattened into timeless fixture truth -- the
// published standard rate happens to be the launch rate.
var anthropicRetrievedAt = time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)

// Inference geographies that carry their own price sheet.
//
// These are the values the API accepts for inference_geo and reports back in
// usage.inference_geo, which is what pricing keys on. Geography is a price selector
// here in the way service tier is on OpenAI: the response says where inference
// actually ran, a workspace default means an omitted request parameter can still
// resolve to us, and the two are priced differently. See pricing.selector.
const (
	anthropicGeoGlobal = "global"
	anthropicGeoUS     = "us"
)

// The published US data-residency multiplier, as an exact integer ratio.
//
// The pricing page states it once: US-only inference through inference_geo "incurs a
// 1.1x multiplier on all token pricing categories, including input tokens, output
// tokens, cache writes, and cache reads". Kept as 11/10 rather than 1.1 because a
// float multiplier has no place anywhere near an accounting figure, and applied here
// at fixture-authoring time rather than at settlement: the catalog states rates, and
// nothing in the request path multiplies anything.
//
// Both the base rate and the multiplier come from the same page at the same
// AnthropicFixtureVersion, so the product is as attributable as the operands.
const (
	usMultiplierNum = 11
	usMultiplierDen = 10
)

// times11over10 applies the published US multiplier exactly.
//
// A base rate that is not a whole number of microdollars after multiplication would
// mean the published figure has more precision than money can hold, which is a
// fixture-authoring error rather than a runtime condition -- so it panics at init,
// exactly as dollars does for a malformed literal, rather than rounding silently.
func times11over10(m money.Money) money.Money {
	scaled := int64(m) * usMultiplierNum
	if scaled%usMultiplierDen != 0 {
		panic("fixtures: the US multiplier does not apply exactly to " + m.String())
	}
	return money.Money(scaled / usMultiplierDen)
}

// anthropicRates is one model's token rates, per million tokens, as published.
//
// Five separate figures rather than a base rate and three multipliers, even though
// the page presents the cache columns as multipliers of base input (1.25x, 2x, 0.1x)
// and prints the products in the same table. Both are published; the products are
// what the fixture states, because a rate throttle can point at is a rate it can be
// argued with.
type anthropicRates struct {
	in, write5m, write1h, cacheRead, out string
}

// anthropicPrice builds one price row.
//
// geoRate is what turns a base row into a US row: identity for global and standard
// pricing, the published multiplier for us. It is applied to every token dimension
// and to none of the others, which is the literal scope the page gives it.
func anthropicPrice(modelID, geo string, r anthropicRates, geoRate func(money.Money) money.Money) pricing.Price {
	perMillion := func(d usage.Dimension, figure string) pricing.Rate {
		return pricing.PerMillion(d, geoRate(dollars(figure)))
	}
	return pricing.Price{
		AccessProvider:  "anthropic",
		ProviderModelID: modelID,
		InferenceGeo:    geo,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  perMillion(usage.InputTokens, r.in),
			usage.OutputTokens: perMillion(usage.OutputTokens, r.out),

			// Cache reads are their own dimension at their own rate -- a tenth of base
			// input -- because they are not ordinary input. Anthropic's usage counters are
			// additive rather than inclusive: input_tokens excludes what was read from
			// cache, so a cache read priced at the input rate is not a rounding
			// difference, it is a tenfold error on that portion of the request.
			usage.CacheReadTokens: perMillion(usage.CacheReadTokens, r.cacheRead),

			// Cache writes are split by lifetime because the lifetimes are priced
			// differently: 1.25x base input for five minutes, 2x for an hour. TTL is
			// therefore financially meaningful, and the response reports the split
			// authoritatively -- see the note on the aggregate below.
			usage.CacheWrite5mTokens: perMillion(usage.CacheWrite5mTokens, r.write5m),
			usage.CacheWrite1hTokens: perMillion(usage.CacheWrite1hTokens, r.write1h),

			// Deliberately absent: a rate for usage.CacheWriteTokens. Anthropic's
			// cache_creation_input_tokens is the *sum* of the per-TTL figures rather
			// than a separate charge, so pricing an undifferentiated cache-write
			// dimension alongside its two children would bill the same tokens twice.
			// An adapter that cannot account for all of the aggregate reports the
			// remainder as unresolved instead of folding it into a bucket.

			// Web search is charged per search on top of tokens: $10 per 1,000. Priced
			// from the authoritative server_tool_use counter, never from counting tool
			// blocks in the response -- a failed search returns a content block and is
			// not billed.
			//
			// Not multiplied by geography. The data-residency paragraph enumerates what
			// the 1.1x applies to -- input, output, cache writes, cache reads -- and the
			// web search price is published without geographic qualification. Extending
			// the multiplier to it would be applying a modifier the page does not state.
			usage.Searches: pricing.PerThousand(usage.Searches, dollars("10")),
		},
		Provenance: pricing.Provenance{
			Source:      AnthropicSource,
			Version:     AnthropicFixtureVersion,
			RetrievedAt: anthropicRetrievedAt,
			Currency:    "USD",
		},
	}
}

// Anthropic returns fixture prices for a handful of current direct-API Claude models.
//
// # What this is not
//
// It is not the Anthropic pricing page. It is a deliberately small set chosen to
// exercise every priced dimension the Messages adapter can produce -- fresh input,
// output, cache reads, five-minute cache writes, one-hour cache writes, web searches
// -- under both priced inference geographies. A model absent from here produces an
// explicit unknown cost, which is the correct outcome and never a zero. Recognition
// here is not required for identity: an unknown or future model ID is representable,
// it just is not priceable.
//
// Direct Anthropic access is a different access provider from Bedrock, deliberately,
// even for the same underlying model: the rates differ, the model IDs differ, and the
// bill comes from a different vendor. They share a publisher and a model family and
// nothing else.
//
// # Deliberately absent
//
// No batch rates: the Batch API is a different API this adapter does not call. No
// fast-mode rates: the request field is beta-only on the SDK's stable Messages
// params, so a request this adapter can make cannot select it. No retired models. No
// context-length bands -- and unlike the OpenAI fixture, that is not a limitation
// here: Claude 4.6 and later "include the full 1M token context window at standard
// pricing", so a 900k-token request is priced at the same per-token rate as a 9k one
// and inventing a threshold would be inventing a charge.
func Anthropic() []pricing.Price {
	type entry struct {
		id    string
		rates anthropicRates
	}
	entries := []entry{
		{"claude-opus-5", anthropicRates{"5", "6.25", "10", "0.50", "25"}},
		{"claude-sonnet-5", anthropicRates{"2", "2.50", "4", "0.20", "10"}},
		{"claude-sonnet-4-6", anthropicRates{"3", "3.75", "6", "0.30", "15"}},
		{"claude-haiku-4-5", anthropicRates{"1", "1.25", "2", "0.10", "5"}},
		// One dated ID alongside its alias, because a caller may pin either and pricing
		// keys on the exact string sent. The alias and the dated release it currently
		// points at are priced identically today, and are separate rows rather than one
		// shared row precisely because that is not guaranteed to stay true.
		{"claude-haiku-4-5-20251001", anthropicRates{"1", "1.25", "2", "0.10", "5"}},
	}

	identity := func(m money.Money) money.Money { return m }

	out := make([]pricing.Price, 0, len(entries)*3)
	for _, e := range entries {
		// Three rows per model, and each is needed for a different reason.
		//
		// The geography-less row prices a request at admission. inference_geo is
		// optional, defaults to global, and may be defaulted to us by workspace
		// configuration -- so at admission there is frequently no geography to key on,
		// and a catalog that only stated qualified rows would refuse an ordinary
		// request.
		//
		// The explicit global row is what makes global reachable as a captured
		// alternate: only a qualified row becomes one, and a request admitted with no
		// geography that comes back reporting global must re-price from rates frozen
		// for global rather than from the unqualified sheet.
		//
		// The us row is the priced modifier. A request served in us prices from it, and
		// a request served in a geography no row states settles unresolved rather than
		// at base rates -- see pricing.CapturedQuote.For.
		out = append(out,
			anthropicPrice(e.id, "", e.rates, identity),
			anthropicPrice(e.id, anthropicGeoGlobal, e.rates, identity),
			anthropicPrice(e.id, anthropicGeoUS, e.rates, times11over10),
		)
	}
	return out
}
