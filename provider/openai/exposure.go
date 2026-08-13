package openai

import (
	"fmt"

	"github.com/scttfrdmn/throttle/usage"
)

// exposure is what a request tells throttle it will not be able to fully account for,
// known before the call from the request alone.
//
// It exists because "we priced every token the provider reported" and "we know what
// this request cost" are different claims, and the gap between them is knowable in
// advance. Two things open that gap:
//
//   - a hosted tool OpenAI bills in a unit its usage object cannot express: per call,
//     per stored GB-day, per container session.
//   - a request that can carry or produce audio tokens for which throttle holds no
//     captured rate.
//
// Both are properties of the request, not of the response, which is why this is built
// at admission. A hosted tool's surcharge is incurred by asking for the tool, and no
// field of the reply reports it either way; audio rates are missing or present before
// a single token is generated.
//
// The consequence is deliberately symmetric at both ends of the lifecycle. At
// admission an incomplete exposure makes the estimate's cost not-Known, which enforce
// mode refuses before the provider is called and monitor mode admits under the
// existing unknown-cost semantics. At settlement it downgrades the priced actual to a
// floor. Neither end invents a figure to stand in for the part throttle cannot price.
type exposure struct {
	// tools names the tool types whose charges cannot be derived from the response,
	// sorted, for the reason string on an unresolved cost.
	tools []string

	// audio names the audio dimensions this request could consume that throttle holds
	// no rate for, sorted. Empty when the request cannot involve audio, or when the
	// captured quote prices it.
	//
	// These are dimensions rather than a boolean because they are what a later
	// reconciliation needs: adding authoritative per-model audio-token rates to the
	// catalog must be enough to resolve such a record, with nothing about this
	// lifecycle changing.
	audio []usage.Dimension
}

// complete reports whether every billable dimension of this request can be accounted
// for.
func (e exposure) complete() bool { return len(e.tools) == 0 && len(e.audio) == 0 }

// downgrade turns a priced cost into an honest one given what this request could not
// account for.
//
// The token figures may have priced perfectly; the point is that they are not the
// whole bill. A known cost becomes a floor, and a cost that was already partial or
// unknown keeps its own reason alongside this one -- both facts are true and a
// reconciliation needs both.
//
// The unpriced dimensions are carried forward and unioned rather than replaced. When a
// response actually reports audio tokens, pricing has already named those dimensions
// unpriced on its own -- the quote has no rate for them -- and this adds nothing. When
// a response reports none, the request's audio exposure is still the reason its cost is
// a floor rather than a total, and dropping it here would leave a record no operator
// could act on.
func (e exposure) downgrade(cost usage.Cost) usage.Cost {
	if e.complete() {
		return cost
	}
	reason := e.reason()
	unpriced := unionDimensions(cost.Unpriced, e.audio)

	switch cost.State() {
	case usage.CostKnown:
		return usage.PartialCost(cost.Amount, unpriced, reason)
	case usage.CostPartial:
		return usage.PartialCost(cost.Amount, unpriced, cost.Reason+"; "+reason)
	default:
		out := usage.UnknownCost(joinReasons(cost.Reason, reason))
		out.Unpriced = unpriced
		return out
	}
}

// reason renders why this request's cost is not a total, in the provider's own terms.
func (e exposure) reason() string {
	var out string
	if len(e.tools) > 0 {
		out = fmt.Sprintf("OpenAI bills %s outside the response's usage object (per call, per stored GB, or per container session), so the token cost is a floor rather than a total",
			joinAnd(e.tools))
	}
	if len(e.audio) > 0 {
		out = join(out, fmt.Sprintf("this request can consume audio tokens, which OpenAI bills at their own rates, and throttle has no captured rate for %s, so the text cost is a floor rather than a total",
			joinAnd(dimensionNames(e.audio))))
	}
	return out
}

// joinReasons is join, skipping the empty string an unknown cost may carry.
func joinReasons(a, b string) string { return join(a, b) }

// unionDimensions merges two dimension lists, deduplicated and sorted.
//
// Sorting is not cosmetic: usage.Cost's own constructors sort Unpriced so that two
// records describing the same gap compare equal, and a reconciler grouping records by
// what they are missing depends on that.
func unionDimensions(a, b []usage.Dimension) []usage.Dimension {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[usage.Dimension]bool, len(a)+len(b))
	out := make([]usage.Dimension, 0, len(a)+len(b))
	for _, d := range append(append([]usage.Dimension{}, a...), b...) {
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sortDimensions(out)
	return out
}

func dimensionNames(dims []usage.Dimension) []string {
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		out = append(out, string(d))
	}
	return out
}
