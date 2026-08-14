package anthropic

import (
	"fmt"
	"sort"

	"github.com/scttfrdmn/throttle/usage"
)

// exposure is what a request or response tells throttle it will not be able to fully
// account for.
//
// It exists because "we priced every token Anthropic reported" and "we know what this
// request cost" are different claims, and the gap between them is nearly always
// knowable -- usually before the call. Three things open it:
//
//   - a server-side tool Anthropic bills in a unit its usage object cannot express.
//     Container time is the concrete case: a five-minute minimum, a monthly free
//     allowance per organization, an hourly rate, and a response that reports a
//     container ID and an expiry and no duration.
//   - a usage counter this build cannot read as a number, which cannot become a
//     dimension and so cannot be reported as unpriced by pricing.
//   - a container in the response, which establishes that something billed by time
//     served the request whatever the tool list said.
//
// The consequence is deliberately symmetric at both ends of the lifecycle. At admission
// an incomplete exposure makes the estimate's cost not-Known, which enforce mode refuses
// before Anthropic is called and monitor mode admits under the existing unknown-cost
// semantics. At settlement it downgrades the priced actual to a floor. Neither end
// invents a figure to stand in for the part throttle cannot price, and neither converts
// unknown provider spend into zero.
type exposure struct {
	// tools names what cannot be derived from the response, sorted, for the reason
	// string on a partial or unknown cost. The entries are Anthropic's own tool names
	// where a tool is what opened the gap, and a short phrase where something else did.
	tools []string

	// unpriced names dimensions this request could consume that throttle holds no
	// captured rate for, sorted.
	//
	// Dimensions rather than a boolean because they are what a later reconciliation
	// needs: adding the missing rate to the catalog must be enough to resolve such a
	// record, with nothing about this lifecycle changing.
	unpriced []usage.Dimension
}

// complete reports whether every billable dimension of this request can be accounted
// for.
func (e exposure) complete() bool { return len(e.tools) == 0 && len(e.unpriced) == 0 }

// merge combines what the request revealed with what the response did.
//
// Both are true and neither supersedes the other. A request can be unaccountable for
// one reason before the call -- a code-execution tool -- and for a different one after
// it, such as a usage counter that arrived in a field this build cannot read.
func (e exposure) merge(o exposure) exposure {
	if o.complete() {
		return e
	}
	if e.complete() {
		return o
	}
	return exposure{
		tools:    unionStrings(e.tools, o.tools),
		unpriced: unionDimensions(e.unpriced, o.unpriced),
	}
}

// downgrade turns a priced cost into an honest one given what could not be accounted
// for.
//
// The token figures may have priced perfectly; the point is that they are not the whole
// bill. A known cost becomes a floor, and a cost that was already partial or unknown
// keeps its own reason alongside this one -- both facts are true and a reconciliation
// needs both.
//
// The unpriced dimensions are unioned rather than replaced. When a response actually
// reports a dimension throttle has no rate for, pricing has already named it on its own
// and this adds nothing; when it reports none, the request's own exposure is still the
// reason the cost is a floor rather than a total, and dropping it here would leave a
// record no operator could act on.
func (e exposure) downgrade(cost usage.Cost) usage.Cost {
	if e.complete() {
		return cost
	}
	reason := e.reason()
	unpriced := unionDimensions(cost.Unpriced, e.unpriced)

	switch cost.State() {
	case usage.CostKnown:
		return usage.PartialCost(cost.Amount, unpriced, reason)
	case usage.CostPartial:
		return usage.PartialCost(cost.Amount, unpriced, cost.Reason+"; "+reason)
	default:
		out := usage.UnknownCost(join(cost.Reason, reason))
		out.Unpriced = unpriced
		return out
	}
}

// reason renders why this request's cost is not a total, in the provider's own terms.
func (e exposure) reason() string {
	var out string
	if len(e.tools) > 0 {
		out = fmt.Sprintf("Anthropic bills %s in a unit the response does not report (container time, charged from a five-minute minimum against a monthly per-organization allowance), so the token cost is a floor rather than a total",
			joinAnd(e.tools))
	}
	if len(e.unpriced) > 0 {
		out = join(out, fmt.Sprintf("throttle has no captured rate for %s, so the priced dimensions are a floor rather than a total",
			joinAnd(dimensionNames(e.unpriced))))
	}
	return out
}

// unionStrings merges two lists, deduplicated and sorted.
func unionStrings(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

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
