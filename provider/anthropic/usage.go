package anthropic

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/respjson"

	"github.com/scttfrdmn/throttle/usage"
)

// ErrUsageInconsistent reports that a response's usage object contradicts itself.
//
// Two shapes reach it: a negative counter, and a per-TTL cache breakdown that exceeds
// the total it is supposed to break down. Neither is something Anthropic should
// produce, and neither can be reconciled by arithmetic -- so the honest answer is that
// the figures cannot be trusted, which sends the request down the unknown-cost path
// with its hold left standing.
//
// A breakdown that falls *short* of its total is deliberately not this error. That is
// incomplete rather than impossible, and it has a correct answer: price what is
// decomposed and carry the remainder as cache writes of undetermined lifetime. See
// normalizeUsage.
var ErrUsageInconsistent = errors.New("anthropic: reported usage is internally inconsistent")

// unknownDimensionPrefix namespaces a usage counter this build has never heard of.
//
// Prefixed, unlike every dimension in the usage package, and for the reason the
// package's own naming rule gives: a generic identifier is for a unit that is genuinely
// provider-neutral, and the unit here is by definition unknown. Calling a new counter
// "requests" or "searches" because the name looked familiar would be asserting a
// meaning nobody has established. What matters is that the number survives with an
// identifier a later catalog can price, not that it looks tidy.
const unknownDimensionPrefix = "anthropic."

// normalizeUsage converts an Anthropic Messages usage object into throttle's
// provider-neutral dimensions.
//
// # Why this is additive, and why that matters
//
// This is the function issue #29 exists to get right. throttle's dimensions are
// disjoint by construction -- each carries its own price and they are summed -- and
// Anthropic's counters are *already* disjoint. So the mapping copies. It does not
// subtract, and subtracting would be the single most expensive mistake in this package.
//
// Anthropic says so twice, in its own words. The SDK's Usage doc: "Total input tokens
// in a request is the summation of `input_tokens`, `cache_creation_input_tokens`, and
// `cache_read_input_tokens`." And the API docs, explaining why: "The `input_tokens`
// field represents only the tokens that come after the last cache breakpoint in your
// request -- not all the input tokens you sent."
//
// That is the opposite of OpenAI's Responses API, where input_tokens is documented as
// the total "including cached and cache-write tokens" and the adapter therefore has to
// subtract. Reaching for that model here fails silently and expensively. A request
// serving a 100,000-token cached prefix with 50 fresh tokens reports
//
//	input_tokens: 50, cache_read_input_tokens: 100000
//
// for a true total of 100,050 tokens. OpenAI-style subtraction yields 50 minus 100,000,
// which clamped to zero prices the request at fifty tokens' worth of input -- a bill
// two thousand times too small, arrived at by code that looks reasonable. The tests pin
// exactly that shape.
//
// # What each dimension means afterwards
//
//	InputTokens         input after the last cache breakpoint, at the input rate
//	CacheReadTokens     input served from cache, at a tenth of the input rate
//	CacheWrite5mTokens  input written to a five-minute cache, at 1.25x input
//	CacheWrite1hTokens  input written to a one-hour cache, at 2x input
//	CacheWriteTokens    cache writes whose lifetime the response did not state
//	OutputTokens        all generated tokens, thinking included, at the output rate
//	Searches            web searches Anthropic performed, at $10 per thousand
//
// # Cache writes are priced by lifetime
//
// A five-minute write and a one-hour write are different money for identical tokens, so
// the lifetime is a financial fact and the two get their own dimensions. The response
// decomposes cache_creation_input_tokens into ephemeral_5m and ephemeral_1h children,
// and this reads the children -- summing them only to check them against the parent.
// The aggregate is never itself recorded when it is decomposed, because it is the sum
// rather than an additional charge, and recording both would bill the same tokens twice
// at a plausible-looking number.
//
// The lifetime is never inferred from the request's cache_control markers, and
// Anthropic documents why that would be wrong rather than merely unwise: the system
// inserts its own breakpoint, and "This automatic breakpoint always uses the default
// 5-minute TTL, independent of any TTL you set on your own `cache_control` markers ...
// so you may see 5-minute cache writes even when every `cache_control` you set uses a
// 1-hour TTL." What was asked for and what was written are different facts.
//
// When the children do not add up to the parent, the shortfall is real cache-write
// tokens of undetermined lifetime, and it is recorded as the undifferentiated
// CacheWriteTokens. No fixture prices that dimension, so the request settles as a
// partial cost -- the decomposed portion is a mathematically valid floor, and the
// remainder is named rather than guessed at. Assigning it to either lifetime would be a
// coin flip between rates that differ by 60%.
//
// # Thinking is output, priced once
//
// output_tokens_details.thinking_tokens is deliberately not a dimension. Anthropic
// documents thinking tokens as already inside output_tokens -- "`output_tokens` remains
// the inclusive, authoritative total used for billing. This object provides a read-only
// decomposition for observability" -- and bills them at the ordinary output rate. So
// output is copied whole and priced once. Splitting it out and pricing both halves at
// the output rate would double the cost of every extended-thinking request; recording
// the split as a separate dimension with no rate would make every such request settle
// partially priced for no reason. A field existing in the SDK is not a reason to invent
// a billing dimension.
//
// # Absent is not zero
//
// The SDK reports usage as a value type, so a zero field is ambiguous on its face.
// Presence is read from the JSON metadata instead: a response that never mentioned
// cache reads records no cache-read dimension, rather than a zero that would imply
// throttle knows the provider charged nothing for one.
func normalizeUsage(u anth.Usage) (usage.Usage, error) {
	var out usage.Usage

	// A usage object with neither token total is not a usage object. Both fields are
	// required by the Messages schema, so this is defensive rather than expected -- but
	// the alternative reading of an all-zero struct is a free request.
	if !u.JSON.InputTokens.Valid() && !u.JSON.OutputTokens.Valid() {
		return out, nil
	}

	for _, c := range []struct {
		name string
		n    int64
		ok   bool
	}{
		{"input_tokens", u.InputTokens, u.JSON.InputTokens.Valid()},
		{"output_tokens", u.OutputTokens, u.JSON.OutputTokens.Valid()},
		{"cache_read_input_tokens", u.CacheReadInputTokens, u.JSON.CacheReadInputTokens.Valid()},
		{"cache_creation_input_tokens", u.CacheCreationInputTokens, u.JSON.CacheCreationInputTokens.Valid()},
	} {
		if c.ok && c.n < 0 {
			return usage.Usage{}, fmt.Errorf("%w: %s is %d, and a negative token count cannot be charged or refunded",
				ErrUsageInconsistent, c.name, c.n)
		}
	}

	if u.JSON.InputTokens.Valid() {
		// Copied, not adjusted. See the additive discussion above.
		out.Set(usage.InputTokens, u.InputTokens)
	}
	if u.JSON.OutputTokens.Valid() {
		// Copied whole, thinking included, priced once.
		out.Set(usage.OutputTokens, u.OutputTokens)
	}
	if u.JSON.CacheReadInputTokens.Valid() {
		// Its own dimension at its own rate. A cache read is a tenth of the input price,
		// so folding it into InputTokens would overcharge that portion tenfold -- and
		// because the counters are disjoint, folding is not even arithmetically tempting:
		// it is a straight relabelling of cheap tokens as dear ones.
		out.Set(usage.CacheReadTokens, u.CacheReadInputTokens)
	}

	if err := normalizeCacheCreation(u, &out); err != nil {
		return usage.Usage{}, err
	}

	// Web search is a separate billable unit, taken from the authoritative counter.
	//
	// Never from the response's content blocks. A failed search still produces a result
	// block -- Anthropic returns web_search_tool_result_error inside an HTTP 200 -- and
	// documents that "If an error occurs during web search, the web search will not be
	// billed." Counting blocks would invent charges out of failures. The counter reports
	// what was billed, which is the only figure worth pricing.
	if u.ServerToolUse.JSON.WebSearchRequests.Valid() {
		if n := u.ServerToolUse.WebSearchRequests; n < 0 {
			return usage.Usage{}, fmt.Errorf("%w: server_tool_use.web_search_requests is %d, and a negative search count cannot be charged",
				ErrUsageInconsistent, n)
		}
		out.Set(usage.Searches, u.ServerToolUse.WebSearchRequests)
	}

	// web_fetch_requests is deliberately absent. It is a real counter, and it carries no
	// surcharge: a fetch's cost is the tokens the fetched content becomes, which
	// input_tokens already reports. Minting a $0 rate to make the counter appear
	// "supported" would assert a price Anthropic has not published, and recording the
	// count as an unpriced dimension would make every web-fetch request settle partially
	// priced for no monetary reason at all. The count is kept as metadata instead; see
	// requestMetadata.

	// A counter this build has never seen is carried through rather than dropped. See
	// unknownUsageDimensions for why that is not the same as pricing it.
	for d, n := range unknownUsageDimensions(u) {
		out.Set(d, n)
	}

	return out, nil
}

// normalizeCacheCreation maps the per-TTL cache-write breakdown, checking it against
// the aggregate it decomposes.
//
// Three outcomes, and each is a different fact about what is known:
//
//   - the children account for the aggregate exactly: both lifetimes are recorded and
//     the aggregate is not, since it is their sum rather than a further charge.
//   - the children fall short: what they cover is recorded, and the shortfall becomes
//     undifferentiated CacheWriteTokens -- real tokens whose price depends on a
//     lifetime the response did not state. No fixture prices that dimension, so the
//     result is a floor plus a named gap.
//   - the children exceed the aggregate: impossible, and reported as such.
func normalizeCacheCreation(u anth.Usage, out *usage.Usage) error {
	fivem, hasFivem := cacheChild(u, u.CacheCreation.JSON.Ephemeral5mInputTokens, u.CacheCreation.Ephemeral5mInputTokens)
	onehr, hasOnehr := cacheChild(u, u.CacheCreation.JSON.Ephemeral1hInputTokens, u.CacheCreation.Ephemeral1hInputTokens)

	if hasFivem {
		if fivem < 0 {
			return fmt.Errorf("%w: cache_creation.ephemeral_5m_input_tokens is %d", ErrUsageInconsistent, fivem)
		}
		out.Set(usage.CacheWrite5mTokens, fivem)
	}
	if hasOnehr {
		if onehr < 0 {
			return fmt.Errorf("%w: cache_creation.ephemeral_1h_input_tokens is %d", ErrUsageInconsistent, onehr)
		}
		out.Set(usage.CacheWrite1hTokens, onehr)
	}

	if !u.JSON.CacheCreationInputTokens.Valid() {
		// No aggregate to reconcile against. Whatever the children reported stands on its
		// own; there is nothing to be short of.
		return nil
	}

	remainder := u.CacheCreationInputTokens - fivem - onehr
	switch {
	case remainder < 0:
		return fmt.Errorf("%w: cache_creation_input_tokens is %d but its breakdown reports %d written for five minutes and %d for an hour, and a total cannot be smaller than the sum it is the sum of",
			ErrUsageInconsistent, u.CacheCreationInputTokens, fivem, onehr)
	case remainder > 0:
		// Real cache-write tokens of undetermined lifetime. Recorded under the
		// undifferentiated dimension, which no Anthropic fixture prices, so the request
		// settles as a floor with the gap named rather than at a guessed lifetime. Both
		// shapes land here: children that partly account for the aggregate, and an
		// aggregate that arrived with no breakdown at all.
		out.Set(usage.CacheWriteTokens, remainder)
	}
	return nil
}

// cacheChild reads one per-TTL cache-write counter, reporting whether it was really
// there.
//
// Both the cache_creation object and the field itself have to be present. A response
// that omitted the object mentioned no lifetime breakdown, and neither did one that
// sent the object without the field -- and in either case the tokens still exist in the
// aggregate, so the difference between "absent" and "zero" decides whether they are
// priced at a known lifetime or carried as a remainder.
func cacheChild(u anth.Usage, field interface{ Valid() bool }, n int64) (int64, bool) {
	if !u.JSON.CacheCreation.Valid() || !field.Valid() {
		return 0, false
	}
	return n, true
}

// unknownUsageDimensions collects billable-looking counters this build does not know
// about.
//
// Anthropic adds usage counters -- cache_creation, output_tokens_details,
// inference_geo, and server_tool_use were all additions -- and a build compiled against
// today's SDK will one day receive tomorrow's. The failure mode worth engineering
// against is not a crash: it is a new charge arriving in a field nothing reads, so the
// request settles as fully priced while part of the bill is silently discarded. A
// number nobody can price must make the cost partial, not disappear.
//
// So an unrecognized numeric field becomes a namespaced dimension. No fixture prices
// it, which is the point: pricing reports it as unpriced, the cost becomes a floor, and
// the record carries both the count and its identifier -- enough for a later catalog
// update plus reconciliation to resolve the request without ever calling Anthropic
// again.
//
// # Why presence is counted rather than validated
//
// Detection uses the length of the extra-fields map, never the fields' Valid method.
// That is empirical: an unknown JSON field lands in ExtraFields with its raw text
// intact but reports Valid() == false in every case tested -- for a number, for a zero,
// for a string, and for null. A guard written the obvious way, skipping fields that do
// not validate, would skip all of them and quietly restore exactly the silent-discard
// behaviour this function exists to prevent.
//
// Non-numeric extra fields are not dimensions -- a string cannot be multiplied by a
// rate -- but they are still evidence the response outgrew this build, and
// observedExposure reports them so the cost cannot come out fully known on the strength
// of counters throttle could not read.
func unknownUsageDimensions(u anth.Usage) map[usage.Dimension]int64 {
	var out map[usage.Dimension]int64
	for prefix, fields := range unknownUsageFields(u) {
		for name, raw := range fields {
			n, numeric := parseCount(raw)
			if !numeric || n == 0 {
				continue
			}
			if out == nil {
				out = map[usage.Dimension]int64{}
			}
			out[usage.Dimension(unknownDimensionPrefix+prefix+name)] = n
		}
	}
	return out
}

// unknownUsageFields returns every extra field on the usage object and its nested
// breakdowns, keyed by a path prefix so two objects' unknown fields cannot collide.
//
// All three nested objects are walked, not just the top level, because a new counter is
// at least as likely to appear inside an existing breakdown as beside it: a third cache
// lifetime would land in cache_creation, and a new server-side tool's request count in
// server_tool_use.
func unknownUsageFields(u anth.Usage) map[string]map[string]string {
	out := map[string]map[string]string{}
	for prefix, extra := range map[string]map[string]respjson.Field{
		"":                 u.JSON.ExtraFields,
		"cache_creation.":  u.CacheCreation.JSON.ExtraFields,
		"server_tool_use.": u.ServerToolUse.JSON.ExtraFields,
	} {
		if len(extra) == 0 {
			continue
		}
		fields := map[string]string{}
		for name, f := range extra {
			fields[name] = f.Raw()
		}
		out[prefix] = fields
	}
	return out
}

// parseCount reads a raw JSON value as a token or request count.
//
// A count is what can be multiplied by a rate, so anything that is not an integer is
// not a dimension -- a string, an object, a null, a fractional number. Those are still
// reported, by observedExposure, since a field this build cannot read is evidence the
// response outgrew it. Splitting the parse out is what lets both callers agree on which
// fields those are.
func parseCount(raw string) (int64, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// sortDimensions orders dimensions for a stable reason string and a comparable Unpriced
// list.
//
// Sorting is not cosmetic: usage.Cost's own constructors sort Unpriced so that two
// records describing the same gap compare equal, and a reconciler grouping records by
// what they are missing depends on that.
func sortDimensions(dims []usage.Dimension) {
	sort.Slice(dims, func(i, j int) bool { return dims[i] < dims[j] })
}

// hasUsage reports whether a message carried usage figures at all.
//
// Usage is a required field of the Messages response schema and output_tokens is
// documented as non-zero even for an empty completion, so its absence should not
// happen. It is still checked, because the lifecycle has to distinguish "reported
// nothing" -- which is unresolvable -- from "reported zero", which would be a free
// request.
func hasUsage(m *anth.Message) bool {
	if m == nil {
		return false
	}
	return m.JSON.Usage.Valid() &&
		(m.Usage.JSON.InputTokens.Valid() || m.Usage.JSON.OutputTokens.Valid())
}
