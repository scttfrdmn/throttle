// Package pricing turns normalized usage into money.
//
// It is the only place that knows what things cost. Provider adapters report what
// was consumed and never what it was worth, which is what keeps prices out of
// invocation code and lets a user override a price without touching an adapter.
//
// Two rules hold throughout:
//
//   - No floating point. A rate is an integer numerator over an integer unit, and
//     a cost is computed in exact arithmetic. A float rate would round
//     unpredictably at scale, and the whole ledger exists to avoid that.
//   - No invented prices. An unpriceable request yields an explicit unknown cost,
//     never a zero and never the price of a "similar" model.
package pricing

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"throttle/money"
	"throttle/usage"
)

var (
	// ErrNoPrice reports that the catalog has no applicable price. It is the
	// honest answer for a model released after the catalog was written.
	ErrNoPrice = errors.New("pricing: no price for this model")

	// ErrNoRate reports that a model is priced but a dimension it reported is
	// not, e.g. a model that starts billing for a new unit. Charging only the
	// dimensions we happen to know would understate the bill, so this is an
	// error rather than a partial total.
	ErrNoRate = errors.New("pricing: no rate for billable dimension")
)

// Rate is the price of one billable dimension: PerUnit microdollars for every
// Unit units consumed.
//
// Unit exists because provider prices are quoted per thousand or per million
// tokens, and dividing at quote time would either lose precision or force a
// float. Storing the denominator keeps the arithmetic exact: a price of $3.00 per
// million input tokens is PerUnit=3_000_000, Unit=1_000_000.
type Rate struct {
	Dimension usage.Dimension
	PerUnit   money.Money
	Unit      int64
}

// Exact returns the cost of n units as an exact rational, without rounding.
//
// This is the form costs are accumulated in. Rounding is deferred to the charge
// boundary, because rounding each dimension separately and then summing biases
// the total: a request with many small dimensions rounds up many times, and the
// drift accumulates in the provider's favor across a period of billing.
func (r Rate) Exact(n int64) (*big.Rat, error) {
	if r.Unit <= 0 {
		return nil, fmt.Errorf("pricing: rate for %s has a non-positive unit", r.Dimension)
	}
	if n == 0 {
		return new(big.Rat), nil
	}
	// Exact intermediate: a million tokens at a per-million rate already
	// multiplies to 1e12, and a large batch of a dear model would overflow int64
	// before the division brought it back down.
	num := new(big.Int).Mul(big.NewInt(int64(r.PerUnit)), big.NewInt(n))
	return new(big.Rat).SetFrac(num, big.NewInt(r.Unit)), nil
}

// Cost returns the cost of n units rounded to the microdollar.
//
// It is the single-dimension convenience form. A multi-dimension charge must
// accumulate with Exact and round once via Round, or it rounds per line and
// drifts.
func (r Rate) Cost(n int64) (money.Money, error) {
	x, err := r.Exact(n)
	if err != nil {
		return 0, err
	}
	m, err := Round(x)
	if err != nil {
		return 0, fmt.Errorf("pricing: cost of %d %s: %w", n, r.Dimension, err)
	}
	return m, nil
}

// Round converts an exact cost to microdollars, rounding half away from zero.
//
// Rounding must be decided rather than inherited: a per-token price times a token
// count rarely lands on a whole microdollar. Half-up on the magnitude is
// symmetric, so a negative amount (a credit) rounds the same distance as the
// positive amount it reverses rather than drifting toward positive. The residual
// error is bounded by half a microdollar per charge — six orders of magnitude
// below a cent, and, crucially, per charge rather than per dimension.
func Round(x *big.Rat) (money.Money, error) {
	if x == nil {
		return 0, nil
	}
	num, den := x.Num(), x.Denom()

	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	rem.Abs(rem)
	rem.Mul(rem, big.NewInt(2))
	if rem.Cmp(den) >= 0 {
		if num.Sign() < 0 {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	if !quo.IsInt64() {
		return 0, errors.New("pricing: cost overflows int64 microdollars")
	}
	return money.Money(quo.Int64()), nil
}

// Provenance records where a price came from, so a surprising number can be
// traced rather than argued about. Prices are data with a source and a date, not
// business logic.
type Provenance struct {
	// Source identifies the origin, e.g. a URL, "aws-price-list", or
	// "local-override".
	Source string

	// Version is the catalog version or price list revision, when known.
	Version string

	// EffectiveFrom is when these prices took effect. A zero value means the
	// catalog does not track it.
	EffectiveFrom time.Time

	// RetrievedAt is when throttle obtained them.
	RetrievedAt time.Time

	// Currency is always "USD" in v0.1; money is microdollars. Recorded so a
	// future multi-currency catalog cannot silently mix units.
	Currency string
}

// Price is the full set of rates for one model on one access path.
type Price struct {
	// AccessProvider and ProviderModelID key the price. The provider model ID is
	// used rather than a canonical name because it is the identity the provider's
	// own bill refers to, and it exists even for models throttle cannot name.
	AccessProvider  string
	ProviderModelID string

	// Region and ServiceTier narrow the price when they affect it. Empty matches
	// any value, so a catalog need only spell out the dimensions that vary.
	Region      string
	ServiceTier string

	Rates      map[usage.Dimension]Rate
	Provenance Provenance
}

// Quote is a priced result: what it cost, and on what authority.
type Quote struct {
	// Cost may be unknown or partial. A caller must not treat either as zero, and
	// must not treat a partial amount as a total.
	Cost usage.Cost

	// PerDimension breaks the cost down, which is what makes a total auditable.
	// Entries are rounded individually and so may not sum exactly to Cost.Amount,
	// which is rounded once over the whole charge.
	PerDimension map[usage.Dimension]money.Money

	Provenance Provenance

	// Unpriced lists dimensions that were reported but had no rate. Non-empty
	// implies Cost is partial or unknown.
	Unpriced []usage.Dimension

	// Captured is the immutable rate set this quote was computed from, suitable
	// for persisting and replaying at settlement.
	Captured CapturedQuote
}

// Catalog prices normalized usage.
//
// Implementations must retain provenance, must not guess, and must report an
// unpriceable request as an explicit unknown rather than as zero. A consumer-side
// interface: pricing implementations are chosen by the caller, so a user-supplied
// override catalog is a first-class possibility rather than a special case.
type Catalog interface {
	// Quote prices usage for an identity at a point in time. It returns
	// ErrNoPrice when the model is unknown to the catalog and ErrNoRate when a
	// reported dimension has no rate.
	Quote(ctx context.Context, identity usage.ModelIdentity, u usage.Usage, at time.Time) (Quote, error)
}
