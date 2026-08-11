package pricing

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"time"

	"throttle/money"
	"throttle/usage"
)

// QuoteSet is an immutable set of captured quotes taken in one catalog read, for a
// request whose model identities are not knowable before it runs.
//
// # Why a set is needed at all
//
// CapturedQuote assumes the model is known at admission, which holds for a direct
// model invocation: the caller names the model, so one quote is frozen and replayed
// at settlement. A managed agent invocation breaks that assumption. The runtime API
// takes an agent identifier, not a model identifier, and the agent may invoke
// several foundation models internally — for preprocessing, orchestration, routing,
// postprocessing, and any collaborator it delegates to. Which models those are is
// discovered from the response, after the money has been spent.
//
// The tempting fix — look up each model's price as its usage arrives — is exactly
// what CapturedQuote exists to forbid. A price refresh landing mid-invocation would
// let one internal call be costed on one price sheet and the next on another, with
// nothing in the record to show it happened.
//
// So the whole candidate rate set is frozen instead, at admission, in one read at
// one instant. Settlement is then a lookup in a frozen set rather than a query
// against a live catalog: the same guarantee CapturedQuote gives, widened from one
// model to a bounded set of them. A model the set does not contain is unpriceable,
// which flows into the ordinary partial/unresolved semantics rather than into a
// re-query.
//
// A QuoteSet is treated as immutable once taken, and round-trips through JSON so a
// historical request stays reproducibly priceable.
type QuoteSet struct {
	// AccessProvider is the path the quotes were captured for.
	AccessProvider string `json:"access_provider"`

	// Quotes are the captured quotes, keyed by exact provider model ID. Each member
	// carries its own rates, provenance, and tier alternates, all from the same read.
	Quotes map[string]CapturedQuote `json:"quotes,omitempty"`

	// CapturedAt is the single instant every member was captured at. It is the
	// evidence that the set is one read and not an accumulation of several.
	CapturedAt time.Time `json:"captured_at"`

	// Note explains a set that is empty or narrower than the caller might expect,
	// e.g. a catalog that cannot enumerate its models.
	Note string `json:"note,omitempty"`
}

// Valid reports whether the set can price anything at all.
func (s QuoteSet) Valid() bool { return len(s.Quotes) > 0 }

// Models returns the provider model IDs the set can price, in a stable order.
func (s QuoteSet) Models() []string {
	out := make([]string, 0, len(s.Quotes))
	for id := range s.Quotes {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// For returns the captured quote for an identity, and whether the set has one.
//
// The lookup is on the exact provider model ID, and the service tier selects among
// that quote's alternates — the same rules a single captured quote follows, so a
// compound charge and a simple one price identically.
//
// It never consults a catalog. A model absent from the set has no price as far as
// this request is concerned, whatever the catalog may say now.
func (s QuoteSet) For(id usage.ModelIdentity) (CapturedQuote, bool) {
	q, ok := s.Quotes[id.ProviderModelID]
	if !ok || !q.Valid() {
		return CapturedQuote{}, false
	}
	return q.For(id), true
}

// Component is one priced step of a compound charge: the usage of a single
// observed model invocation, and what it contributed.
//
// Amount is rounded for reporting and the components therefore may not sum to the
// charge's total, exactly as the per-dimension breakdown of a single charge may
// not. The total is the authoritative figure; a component is auditing detail.
type Component struct {
	// Identity is the model this step ran on. ProviderModelID may be empty when the
	// provider reported usage without naming the model, which is a representable
	// state and not an error.
	Identity usage.ModelIdentity

	// Usage is what this step consumed.
	Usage usage.Usage

	// Amount is this step's rounded contribution, valid only when Priced is true.
	Amount money.Money

	// Priced reports whether this step had a quote at all. False makes the whole
	// compound cost incomplete.
	Priced bool

	// Unpriced names this step's nonzero dimensions that had no rate.
	Unpriced []usage.Dimension

	// Reason explains an unpriced or partially priced step.
	Reason string
}

// PriceComponents prices a compound charge: several observed model invocations
// settling as one transaction.
//
// The rounding rule is the point of this function. Every step's exact cost
// accumulates into one rational and the sum is rounded exactly once, so a turn made
// of twenty small model calls is charged like one charge rather than rounded twenty
// times. This is the same boundary CapturedQuote.Price applies across dimensions,
// applied across invocations as well.
//
// Completeness is conservative in the way the rest of the system is:
//
//   - every step priced in full: CostKnown, and the amount is the total.
//   - some priced, at least one step or dimension not: CostPartial. The amount is a
//     floor, never a total, and the unpriced dimensions of every incomplete step are
//     named so a later reconciliation knows what it needs.
//   - nothing priceable: CostUnknown.
//
// A step whose model the set cannot price makes the whole charge incomplete even
// though the steps around it priced cleanly. Reporting the priced subset as the
// total would understate real spend, and the aggregate must equal the basis
// settlement uses.
func (s QuoteSet) PriceComponents(steps []Component) (usage.Cost, []Component, error) {
	out := make([]Component, len(steps))
	copy(out, steps)

	total := new(big.Rat)
	pricedSteps, unpricedSteps := 0, 0
	seen := make(map[usage.Dimension]bool)
	var missing []usage.Dimension
	var firstErr error
	reasons := make([]string, 0, 2)

	addMissing := func(ds []usage.Dimension) {
		for _, d := range ds {
			if !seen[d] {
				seen[d] = true
				missing = append(missing, d)
			}
		}
	}

	for i := range out {
		step := &out[i]
		quote, ok := s.For(step.Identity)
		if !ok {
			unpricedSteps++
			step.Priced = false
			step.Reason = unpricedStepReason(step.Identity)
			addMissing(step.Usage.Dimensions())
			reasons = append(reasons, step.Reason)
			continue
		}

		// Accumulated into the shared rational: this step's contribution is exact
		// until the whole charge is rounded, once, below.
		exact := new(big.Rat)
		priced, unpriced, err := quote.Accumulate(step.Usage, exact, nil)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			unpricedSteps++
			step.Priced = false
			step.Reason = err.Error()
			addMissing(step.Usage.Dimensions())
			reasons = append(reasons, step.Reason)
			continue
		}
		total.Add(total, exact)

		// The step amount is rounded for reporting only. Components may therefore not
		// sum to the total, which is the drift the single rounding avoids.
		if amount, rErr := Round(exact); rErr == nil {
			step.Amount = amount
		}
		step.Unpriced = unpriced

		switch {
		case len(unpriced) == 0:
			step.Priced = true
			pricedSteps++
		case priced == 0:
			unpricedSteps++
			step.Priced = false
			step.Reason = fmt.Sprintf("%s has no rate for %v", quote.ProviderModelID, unpriced)
			addMissing(unpriced)
			reasons = append(reasons, step.Reason)
		default:
			// Partially priced: it contributed a floor, so it counts as priced for the
			// purpose of "was anything priceable" while still spoiling completeness.
			pricedSteps++
			step.Priced = false
			step.Reason = fmt.Sprintf("%s has no rate for %v", quote.ProviderModelID, unpriced)
			addMissing(unpriced)
			reasons = append(reasons, step.Reason)
		}
	}

	if len(steps) == 0 {
		return usage.UnknownCost("no model invocations were observed"), out, nil
	}

	amount, err := Round(total)
	if err != nil {
		return usage.UnknownCost(err.Error()), out, err
	}

	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	reason := joinReasons(reasons)

	switch {
	case unpricedSteps == 0 && len(missing) == 0:
		return usage.KnownCost(amount), out, firstErr
	case pricedSteps == 0:
		cost := usage.UnknownCost(reason)
		cost.Unpriced = missing
		if firstErr == nil {
			firstErr = fmt.Errorf("%w: %s", ErrNoPrice, reason)
		}
		return cost, out, firstErr
	default:
		if firstErr == nil {
			firstErr = fmt.Errorf("%w: %s", ErrNoRate, reason)
		}
		return usage.PartialCost(amount, missing, reason), out, firstErr
	}
}

// unpricedStepReason explains a step the set has no quote for, distinguishing a
// model that was named and not priced from one the provider never named. The
// second is not a catalog gap and must not be reported as one.
func unpricedStepReason(id usage.ModelIdentity) string {
	if id.ProviderModelID == "" {
		return "a model invocation reported usage without naming the model, so it cannot be priced"
	}
	return fmt.Sprintf("no captured price for %s", id.ProviderModelID)
}

// joinReasons collapses repeated step reasons, since one missing price across ten
// orchestration steps is one problem and reads better as one.
func joinReasons(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	seen := make(map[string]bool, len(reasons))
	uniq := make([]string, 0, len(reasons))
	for _, r := range reasons {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		uniq = append(uniq, r)
	}
	sort.Strings(uniq)
	out := ""
	for i, r := range uniq {
		if i > 0 {
			out += "; "
		}
		out += r
	}
	return out
}

// ModelLister is a catalog that can enumerate what it prices.
//
// Separate from RateSource and deliberately optional: it is only needed by a
// request whose models are unknown until it runs, and a catalog that cannot
// enumerate still prices every direct model invocation perfectly. Asserted for
// rather than required, so a user-supplied catalog stays cheap to implement.
// *Static satisfies it.
type ModelLister interface {
	// Models returns the identities the catalog has prices for.
	Models() []usage.ModelIdentity
}

// CaptureSet freezes every rate the catalog holds for an access provider, in one
// read, for a request whose models are not knowable in advance.
//
// The extra parameters narrow the capture to the access dimensions the request will
// actually run under: prices vary by region and tier, so capturing a rate selected
// for the wrong region would be worse than capturing nothing.
//
// A catalog that cannot enumerate its models yields an empty set with a Note
// explaining why, not an error. The caller's request then has no priceable models,
// which is the existing unknown-cost condition and is handled by posture rather
// than by a failure here.
func CaptureSet(cat RateSource, accessProvider, region string, at time.Time) (QuoteSet, error) {
	set := QuoteSet{AccessProvider: accessProvider, CapturedAt: at}
	if cat == nil {
		set.Note = "no catalog was supplied, so no rates could be captured"
		return set, fmt.Errorf("pricing: no catalog supplied")
	}
	lister, ok := cat.(ModelLister)
	if !ok {
		set.Note = "the catalog cannot enumerate its models, so no rates could be captured in advance"
		return set, nil
	}

	quotes := make(map[string]CapturedQuote)
	for _, id := range lister.Models() {
		if id.AccessProvider != accessProvider {
			continue
		}
		id.Region = region
		q, err := cat.Capture(id, at)
		if err != nil {
			// A model the catalog lists but cannot price under these access dimensions
			// is simply absent from the set. It is not an error: the set is a snapshot
			// of what is priceable, and settlement reports anything missing.
			continue
		}
		quotes[id.ProviderModelID] = q
	}
	if len(quotes) == 0 {
		set.Note = fmt.Sprintf("the catalog prices no %s models for region %q", accessProvider, region)
		return set, nil
	}
	set.Quotes = quotes
	return set, nil
}

// Retain returns the subset of the set covering the given components, for
// persistence.
//
// The whole captured set is what settlement priced against, but the models a
// request never touched are not part of its accounting story, and a catalog of a
// few hundred models would otherwise be written to every agent request's record.
// The retained quotes are the same frozen ones, with the same CapturedAt, so the
// record stays reproducibly priceable for the invocations that actually happened —
// which is the only claim the record makes.
func (s QuoteSet) Retain(steps []Component) QuoteSet {
	out := QuoteSet{AccessProvider: s.AccessProvider, CapturedAt: s.CapturedAt, Note: s.Note}
	for _, step := range steps {
		q, ok := s.Quotes[step.Identity.ProviderModelID]
		if !ok {
			continue
		}
		if out.Quotes == nil {
			out.Quotes = make(map[string]CapturedQuote, len(steps))
		}
		out.Quotes[step.Identity.ProviderModelID] = q
	}
	return out
}

// MarshalQuoteSet serializes a quote set for persistence.
func MarshalQuoteSet(s QuoteSet) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("pricing: marshal quote set: %w", err)
	}
	return string(b), nil
}

// UnmarshalQuoteSet restores a quote set from persistence. An empty or "{}"
// payload yields a zero set and no error, since a request that captured no rates
// must not read back as corruption.
func UnmarshalQuoteSet(s string) (QuoteSet, error) {
	if s == "" || s == "{}" || s == "null" {
		return QuoteSet{}, nil
	}
	var out QuoteSet
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return QuoteSet{}, fmt.Errorf("pricing: unmarshal quote set: %w", err)
	}
	return out, nil
}
