package budget

import (
	"math/big"
	"time"

	"throttle/money"
)

// BasisPointsPerWhole is the denominator for percentage rollover caps. Caps are
// expressed in basis points (2500 = 25%) so that percentage configuration stays
// in integer arithmetic; a float percentage would reintroduce the rounding class
// of bug that microdollar accounting exists to avoid.
const BasisPointsPerWhole = 10_000

// ResolveCap returns the absolute positive-carry cap for a period whose new-money
// allocation is allocation. Zero means uncapped.
//
// The two configuration forms are mutually exclusive (Validate enforces it), and
// both resolve to Money here so that no percentage arithmetic reaches the ledger.
//
// A percentage resolves against the closing period's allocation rather than its
// total. Resolving against the total would let carry compound: a capped carry
// would enlarge the next total, which would enlarge the next cap, and a budget
// configured to keep "25% at most" would drift upward every period.
func (p RolloverPolicy) ResolveCap(allocation money.Money) money.Money {
	if p.CapBasisPoints <= 0 {
		return p.Cap
	}
	if allocation <= 0 {
		return 0
	}
	// allocation * bp / 10000, exactly. The product exceeds int64 for large
	// allocations, so it is computed in big.Int and truncated downward: a cap
	// that rounds up would carry money the policy did not authorize.
	n := new(big.Int).Mul(big.NewInt(int64(allocation)), big.NewInt(p.CapBasisPoints))
	n.Quo(n, big.NewInt(BasisPointsPerWhole))
	if !n.IsInt64() {
		return money.Max
	}
	return money.Money(n.Int64())
}

// Close computes the final balance of an envelope given total actual spend.
//
// The balance is signed: positive means the envelope finished under its total,
// negative means it overspent. Reservations are deliberately not an input --
// closing an envelope requires that outstanding reservations have already been
// settled or released, because an unsettled reservation is not yet a fact.
func (e Envelope) Close(spent money.Money) money.Money {
	balance, ok := money.Sub(e.Total(), spent)
	if !ok {
		return money.Min
	}
	return balance
}

// ProvisionalClose computes the balance an envelope would have if every hold
// still outstanding against it settled at its full reserved amount.
//
// This is the conservative reading used for the carry handed to a successor
// period while the closing period is still draining: it can only understate the
// carry, never overstate it, so a successor never spends money that the draining
// period turns out to have needed.
func (e Envelope) ProvisionalClose(spent, reserved money.Money) money.Money {
	committed, ok := money.Add(spent, reserved)
	if !ok {
		return money.Min
	}
	return e.Close(committed)
}

// CarryInto computes the carry that a closing balance contributes to the next
// envelope, applying the rollover policy. allocation is the closing period's
// new-money allocation, which a percentage cap resolves against.
//
// Semantics:
//
//   - RolloverNone ("" behaves the same): no carry in either direction. Unspent
//     allocation is forfeited and overspend is forgiven.
//   - RolloverCredit: positive balance carries forward, capped; debt does not
//     carry.
//   - RolloverBalance: the signed balance carries forward. Positive carry is
//     capped; debt is never capped or forgiven, because suppressing debt would
//     silently forgive money that was really spent.
//
// The cap applies to the carry entering the next envelope, not to the sum of
// carry and the next allocation.
func (p RolloverPolicy) CarryInto(balance, allocation money.Money) money.Money {
	switch p.Mode {
	case RolloverCredit:
		if balance <= 0 {
			return 0
		}
		return p.capPositive(balance, allocation)
	case RolloverBalance:
		if balance < 0 {
			// Debt passes through uncapped. This is the one asymmetry in the
			// policy and it is deliberate.
			return balance
		}
		return p.capPositive(balance, allocation)
	default: // RolloverNone and the zero value.
		return 0
	}
}

func (p RolloverPolicy) capPositive(balance, allocation money.Money) money.Money {
	if cap := p.ResolveCap(allocation); cap > 0 && balance > cap {
		return cap
	}
	return balance
}

// Next returns the successor envelope for a recurrence, carrying the closing
// balance forward under the rollover policy.
//
// The successor spans [e.End, end) so that consecutive envelopes tile the
// timeline without gaps or overlap. Callers own the recurrence calendar: this
// method takes an explicit end rather than assuming a month, so that calendar
// month lengths, DST, and grant periods stay outside the pacing math. Definition
// is the durable, recurrence-aware way to generate these; this method remains the
// primitive it is built from.
func (e Envelope) Next(id string, end time.Time, spent money.Money) Envelope {
	next := e
	next.ID = id
	next.Carry = e.Rollover.CarryInto(e.Close(spent), e.Allocation)
	next.Start = e.End
	next.End = end
	return next
}
