package usage

import (
	"errors"
	"sort"
	"strings"

	"github.com/scttfrdmn/throttle/money"
)

// ErrCostUnknown reports that usage is known but cannot be fully priced.
//
// This is an explicit condition rather than a zero cost: pricing a request at
// nothing because the catalog lacked an entry would understate spend and quietly
// corrupt every aggregate built on it.
var ErrCostUnknown = errors.New("usage: cost cannot be determined")

// Completeness states how much of a usage record was successfully priced.
//
// A single boolean cannot carry this. "Every dimension priced" and "three of four
// dimensions priced" are both non-zero totals, but only the first is a cost; the
// second is a floor. Conflating them lets a report state a total that is
// definitely too low as though it were definitely right.
type Completeness string

const (
	// CostUnknown means nothing could be priced — typically no price for the
	// model at all. Amount is meaningless.
	CostUnknown Completeness = "unknown"

	// CostPartial means some dimensions priced and at least one nonzero dimension
	// did not. Amount is a lower bound, never a total, and Unpriced names what is
	// missing.
	CostPartial Completeness = "partial"

	// CostKnown means every reported dimension was priced. Amount is the cost.
	CostKnown Completeness = "known"
)

// Cost is an amount together with how complete it is.
//
// Known usage with unpriceable cost is a real state — a model released before the
// pricing catalog caught up with it — and the data model represents it rather
// than forcing a guess.
type Cost struct {
	// Amount is the total when Completeness is CostKnown, a lower bound when
	// CostPartial, and meaningless when CostUnknown.
	Amount money.Money

	// Completeness is CostKnown, CostPartial, or CostUnknown.
	//
	// The empty zero value means CostUnknown: a Cost nobody filled in must not read
	// as a known zero. Compare through State rather than against this field
	// directly, so an unset value is not mistaken for an unrecognized one.
	Completeness Completeness

	// Unpriced names the nonzero dimensions that had no rate. Non-empty implies
	// Completeness is CostPartial or CostUnknown, and it is what tells a later
	// reconciliation which prices it needs.
	Unpriced []Dimension

	// Reason explains an incomplete cost in words, for operators.
	Reason string
}

// KnownCost is a fully determined amount.
func KnownCost(m money.Money) Cost {
	return Cost{Amount: m, Completeness: CostKnown}
}

// UnknownCost is a cost that could not be determined at all.
func UnknownCost(reason string) Cost {
	return Cost{Completeness: CostUnknown, Reason: reason}
}

// PartialCost is a floor: the priced dimensions summed to amount, while unpriced
// dimensions were consumed and remain unaccounted for.
//
// It is deliberately awkward to read as a total. A caller must consult Known
// before treating Amount as the cost of anything.
func PartialCost(amount money.Money, unpriced []Dimension, reason string) Cost {
	d := make([]Dimension, len(unpriced))
	copy(d, unpriced)
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return Cost{
		Amount:       amount,
		Completeness: CostPartial,
		Unpriced:     d,
		Reason:       reason,
	}
}

// State is Completeness with the zero value resolved to CostUnknown.
//
// Persistence and comparison should go through this: an unfilled Cost and one
// explicitly marked unknown describe the same accounting fact, and only one of
// them matches the constant.
func (c Cost) State() Completeness {
	if c.Completeness == "" {
		return CostUnknown
	}
	return c.Completeness
}

// Known reports whether Amount is the complete cost. Callers that need a number
// they can add to a ledger must check this, not merely whether Amount is
// non-zero.
func (c Cost) Known() bool { return c.Completeness == CostKnown }

// Resolved reports whether the cost needs no further pricing work. Only a fully
// known cost is resolved: a partial total still has dimensions owing.
func (c Cost) Resolved() bool { return c.Completeness == CostKnown }

// Partial reports whether some but not all of the usage was priced.
func (c Cost) Partial() bool { return c.Completeness == CostPartial }

// AtLeast is the amount that is definitely owed: the full cost when known, the
// priced floor when partial, and zero when nothing could be priced.
//
// This is the number a dashboard renders as "$812.41+". It is never the amount to
// charge a budget — a floor charged as a total would understate spend — but it is
// the honest lower bound on what has already been incurred.
func (c Cost) AtLeast() money.Money {
	switch c.Completeness {
	case CostKnown, CostPartial:
		return c.Amount
	default:
		return 0
	}
}

// Or returns the amount when fully known and the fallback otherwise, so a caller
// that must produce a number is explicit about what it substitutes.
func (c Cost) Or(fallback money.Money) money.Money {
	if c.Known() {
		return c.Amount
	}
	return fallback
}

func (c Cost) String() string {
	switch c.Completeness {
	case CostKnown:
		return c.Amount.CentsString()
	case CostPartial:
		s := c.Amount.CentsString() + "+ (unpriced: " + joinDimensions(c.Unpriced) + ")"
		return s
	default:
		if c.Reason != "" {
			return "unknown (" + c.Reason + ")"
		}
		return "unknown"
	}
}

func joinDimensions(ds []Dimension) string {
	if len(ds) == 0 {
		return "none"
	}
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = string(d)
	}
	return strings.Join(parts, ", ")
}
