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

// CapturedQuote is an immutable snapshot of the rates a request will be priced
// by, taken when the request is admitted and replayed when it settles.
//
// It exists because a catalog is mutable and a request is not instantaneous. If
// settlement re-queried the live catalog, a price refresh landing mid-request
// would silently change that request's accounting basis: the amount reserved and
// the amount charged would come from different price sheets, and no later reader
// could tell. Capturing the rates makes settlement a pure function of the
// captured quote and the observed usage.
//
// It holds rates rather than a computed amount because the amount is not yet
// knowable: the request has not run. It holds only what is needed to price
// whatever Usage comes back, plus the provenance to explain it. No provider SDK
// object is ever serialized here.
//
// A CapturedQuote is treated as immutable once taken. It round-trips through JSON
// so a historical request stays reproducibly priceable across catalog updates,
// price changes, and process restarts.
type CapturedQuote struct {
	// AccessProvider and ProviderModelID identify what was priced. The provider
	// model ID is the exact string sent to the provider.
	AccessProvider  string `json:"access_provider"`
	ProviderModelID string `json:"provider_model_id"`

	// Region and ServiceTier are the access dimensions that selected this price,
	// recorded so it is clear why these rates and not others.
	Region      string `json:"region,omitempty"`
	ServiceTier string `json:"service_tier,omitempty"`

	// Rates are the captured per-dimension prices. A dimension absent here cannot
	// be priced by this quote, which is the condition that produces an unresolved
	// charge at settlement.
	Rates map[usage.Dimension]Rate `json:"rates"`

	// Provenance is where the rates came from: source, version, effective date.
	// Preserved verbatim, including "local-override", so a negotiated rate stays
	// attributable long after the override itself is gone.
	Provenance Provenance `json:"provenance"`

	// CapturedAt is when the quote was taken, which is the time the rates were
	// selected against. Replaying uses the captured rates directly rather than
	// re-resolving effective dates.
	CapturedAt time.Time `json:"captured_at"`

	// Alternates are quotes for the same model under service tiers other than the
	// one requested, keyed by tier.
	//
	// They exist because a provider may serve a request on a tier the caller did not
	// ask for, at a different price. Which tier served the call is a fact about what
	// happened, not a change to the price sheet -- so the alternates are captured at
	// admission, from the same catalog at the same instant as the primary. Selecting
	// one at settlement is still a replay of frozen rates; re-querying the live
	// catalog to price a substituted tier would let a mid-request price refresh
	// change the accounting basis, which is exactly what capturing prevents.
	//
	// Alternates are one level deep: an alternate never carries its own.
	Alternates map[string]CapturedQuote `json:"alternates,omitempty"`
}

// Valid reports whether the quote can price anything at all.
func (q CapturedQuote) Valid() bool {
	return q.ProviderModelID != "" && len(q.Rates) > 0
}

// For selects the captured quote applicable to the identity the provider actually
// served, which may name a different service tier than the request asked for.
//
// It never consults a catalog: an unrecognized tier falls back to the primary
// quote, because pricing a request by the rates it was admitted under is closer to
// the truth than pricing it by nothing.
func (q CapturedQuote) For(id usage.ModelIdentity) CapturedQuote {
	if id.ServiceTier == "" || id.ServiceTier == q.ServiceTier {
		return q
	}
	if alt, ok := q.Alternates[id.ServiceTier]; ok && alt.Valid() {
		return alt
	}
	return q
}

// Rate returns the captured rate for a dimension.
func (q CapturedQuote) Rate(d usage.Dimension) (Rate, bool) {
	r, ok := q.Rates[d]
	return r, ok
}

// Dimensions returns the priceable dimensions in a stable order.
func (q CapturedQuote) Dimensions() []usage.Dimension {
	out := make([]usage.Dimension, 0, len(q.Rates))
	for d := range q.Rates {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Accumulate adds the exact cost of usage under these rates into total, without
// rounding, and reports how many dimensions were priced and which were not.
//
// It exists because the rounding boundary is the charge and a charge is not always
// one model invocation. A managed agent turn is one charge covering several
// internal model calls, possibly on different models with different rates:
// accumulating each call into a shared rational and rounding the sum once is the
// only way that charge obeys the same rule a single Converse response does.
// Rounding each call and summing the results would reintroduce exactly the
// per-line drift Round's contract exists to prevent.
//
// total must be non-nil and is mutated in place. If perDimension is non-nil it is
// called with each priced dimension's exact contribution, for auditing.
//
// A dimension reported with a zero count needs no rate: nothing was consumed, so
// nothing is owed. Unpriced names only the nonzero dimensions with no rate.
func (q CapturedQuote) Accumulate(u usage.Usage, total *big.Rat, perDimension func(usage.Dimension, *big.Rat)) (priced int, unpriced []usage.Dimension, err error) {
	for _, d := range u.Dimensions() {
		n := u.Count(d)
		r, ok := q.Rates[d]
		if !ok {
			if n == 0 {
				continue
			}
			unpriced = append(unpriced, d)
			continue
		}
		x, err := r.Exact(n)
		if err != nil {
			return priced, unpriced, err
		}
		total.Add(total, x)
		priced++
		if perDimension != nil {
			perDimension(d, x)
		}
	}
	return priced, unpriced, nil
}

// Price computes the cost of usage under the captured rates.
//
// The whole charge is accumulated as one exact rational and rounded once, so a
// request with many small dimensions does not round up once per dimension. See
// Round for the rounding rule.
//
// Three outcomes, and the difference between the last two is the point of the
// method:
//
//   - every reported dimension has a rate: CostKnown.
//   - some dimensions priced, at least one nonzero dimension did not: CostPartial,
//     carrying the priced floor and naming what is missing. Not a total.
//   - the quote is empty or nothing priced: CostUnknown.
//
// A dimension reported with a zero count needs no rate: nothing was consumed, so
// nothing is owed and its absence from the quote is harmless.
func (q CapturedQuote) Price(u usage.Usage) (Priced, error) {
	if !q.Valid() {
		reason := fmt.Sprintf("no captured rates for %s", q.ProviderModelID)
		return Priced{Cost: usage.UnknownCost(reason)}, fmt.Errorf("%w: %s", ErrNoPrice, reason)
	}

	total := new(big.Rat)
	per := make(map[usage.Dimension]money.Money, len(q.Rates))

	priced, unpriced, err := q.Accumulate(u, total, func(d usage.Dimension, x *big.Rat) {
		// The per-dimension breakdown is what makes a total auditable, so it is
		// still reported. These are rounded individually and therefore may not sum
		// to Cost.Amount; that discrepancy is the rounding drift the charge-level
		// total deliberately avoids.
		if m, rErr := Round(x); rErr == nil {
			per[d] = m
		}
	})
	if err != nil {
		return Priced{Cost: usage.UnknownCost(err.Error())}, err
	}

	amount, err := Round(total)
	if err != nil {
		return Priced{Cost: usage.UnknownCost(err.Error())},
			fmt.Errorf("pricing: total for %s: %w", q.ProviderModelID, err)
	}

	out := Priced{PerDimension: per, Provenance: q.Provenance, Unpriced: unpriced}
	switch {
	case len(unpriced) == 0:
		out.Cost = usage.KnownCost(amount)
		return out, nil
	case priced == 0:
		reason := fmt.Sprintf("%s has no rate for %v", q.ProviderModelID, unpriced)
		out.Cost = usage.UnknownCost(reason)
		out.Cost.Unpriced = unpriced
		return out, fmt.Errorf("%w: %s", ErrNoRate, reason)
	default:
		reason := fmt.Sprintf("%s has no rate for %v", q.ProviderModelID, unpriced)
		out.Cost = usage.PartialCost(amount, unpriced, reason)
		return out, fmt.Errorf("%w: %s", ErrNoRate, reason)
	}
}

// Priced is the result of applying a quote to usage.
type Priced struct {
	// Cost carries both the amount and how complete it is. A partial cost's
	// Amount is a floor, not a total.
	Cost usage.Cost

	// PerDimension breaks the cost down for auditing. Each entry is rounded
	// independently, so the entries may not sum exactly to Cost.Amount.
	PerDimension map[usage.Dimension]money.Money

	Provenance Provenance

	// Unpriced lists the nonzero dimensions that had no captured rate.
	Unpriced []usage.Dimension
}

// Capture takes an immutable quote for an identity from a catalog.
//
// It is the reservation-time half of the pricing contract: Capture once at
// admission, then CapturedQuote.Price at settlement. A catalog that cannot price
// the model returns ErrNoPrice and an unusable quote, which is what enforce mode
// refuses on.
func Capture(cat RateSource, id usage.ModelIdentity, at time.Time) (CapturedQuote, error) {
	if cat == nil {
		return CapturedQuote{}, fmt.Errorf("pricing: no catalog supplied")
	}
	return cat.Capture(id, at)
}

// RateSource is a catalog that can hand out its rates for capture.
//
// Separate from Catalog because capturing and pricing are different operations
// with different lifetimes: a catalog is live and mutable, a captured quote is
// frozen. Kept small so a user-supplied catalog is cheap to implement.
type RateSource interface {
	// Capture returns the rates applicable to an identity at a point in time. It
	// returns ErrNoPrice when the model is unknown, with a zero CapturedQuote.
	Capture(id usage.ModelIdentity, at time.Time) (CapturedQuote, error)
}

// MarshalQuote serializes a captured quote for persistence.
func MarshalQuote(q CapturedQuote) (string, error) {
	b, err := json.Marshal(q)
	if err != nil {
		return "", fmt.Errorf("pricing: marshal quote: %w", err)
	}
	return string(b), nil
}

// UnmarshalQuote restores a captured quote from persistence.
//
// An empty or "{}" payload yields a zero quote and no error: a request admitted
// in monitor mode with no price never had a quote to store, and reading one back
// must not look like corruption.
func UnmarshalQuote(s string) (CapturedQuote, error) {
	if s == "" || s == "{}" || s == "null" {
		return CapturedQuote{}, nil
	}
	var q CapturedQuote
	if err := json.Unmarshal([]byte(s), &q); err != nil {
		return CapturedQuote{}, fmt.Errorf("pricing: unmarshal quote: %w", err)
	}
	return q, nil
}
