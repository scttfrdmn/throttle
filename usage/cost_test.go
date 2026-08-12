package usage_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/usage"
)

// The zero value must not read as a free request. A Cost nobody filled in is the
// most likely accounting bug in the whole system, so it defaults to unknown.
func TestZeroCostIsUnknownNotFree(t *testing.T) {
	var c usage.Cost
	if c.Known() {
		t.Error("a zero Cost must not report itself as known")
	}
	// The zero value is the empty string, which State resolves so that an unset
	// Cost and an explicitly unknown one compare the same.
	if c.State() != usage.CostUnknown {
		t.Errorf("State = %q, want unknown", c.State())
	}
	if c.AtLeast() != 0 {
		t.Errorf("AtLeast = %s, want 0", c.AtLeast())
	}
	if strings.Contains(c.String(), "$") {
		t.Errorf("String() = %q, an unknown cost must not render as a dollar amount", c.String())
	}
}

// A genuinely free request is a different fact from an unpriceable one, and the
// type must be able to say so.
func TestKnownZeroIsDistinctFromUnknown(t *testing.T) {
	free := usage.KnownCost(0)
	unknown := usage.UnknownCost("no price for this model")

	if !free.Known() {
		t.Error("a known zero cost must report itself as known")
	}
	if unknown.Known() {
		t.Error("an unknown cost must not report itself as known")
	}
	if free.String() == unknown.String() {
		t.Errorf("a known zero and an unknown cost both render as %q", free.String())
	}
}

// The distinction a single boolean cannot carry: "priced everything" versus
// "priced most of it". The second is a floor, and it must name what is missing so a
// later reconciliation knows which prices it needs.
func TestPartialCostIsAFloorThatNamesWhatIsMissing(t *testing.T) {
	c := usage.PartialCost(money.Money(812_410_000),
		[]usage.Dimension{usage.Dimension("video-sec"), usage.Dimension("audio-sec")},
		"no rate for video-sec, audio-sec")

	if c.Known() {
		t.Error("a partial cost must not report itself as known")
	}
	if !c.Partial() {
		t.Error("a partial cost must report itself as partial")
	}
	if c.Resolved() {
		t.Error("a partial cost still has dimensions owing, so it is not resolved")
	}
	if c.AtLeast() != money.Money(812_410_000) {
		t.Errorf("AtLeast = %s, want the priced floor", c.AtLeast())
	}
	// Sorted, so a record's unpriced list is stable regardless of map iteration.
	if len(c.Unpriced) != 2 || c.Unpriced[0] != usage.Dimension("audio-sec") {
		t.Errorf("Unpriced = %v, want a sorted list", c.Unpriced)
	}
	// The rendering is the dashboard's "$812.41+".
	s := c.String()
	if !strings.HasPrefix(s, "$812.41+") {
		t.Errorf("String() = %q, want a $812.41+ floor", s)
	}
	if !strings.Contains(s, "video-sec") {
		t.Errorf("String() = %q, want the unpriced dimensions named", s)
	}
}

// PartialCost copies its input, so a caller reusing its slice cannot retroactively
// change what a record says it failed to price.
func TestPartialCostCopiesUnpriced(t *testing.T) {
	dims := []usage.Dimension{usage.Dimension("b"), usage.Dimension("a")}
	c := usage.PartialCost(money.Money(1), dims, "reason")
	dims[0] = usage.Dimension("mutated")
	for _, d := range c.Unpriced {
		if d == usage.Dimension("mutated") {
			t.Fatal("PartialCost must copy its unpriced dimensions")
		}
	}
}

// Or forces a caller that needs a number to be explicit about what it substitutes
// for an unknown, rather than silently getting a zero.
func TestCostOr(t *testing.T) {
	if got := usage.KnownCost(money.Money(500)).Or(999); got != 500 {
		t.Errorf("Or on a known cost = %s, want 500", got)
	}
	if got := usage.UnknownCost("no price").Or(999); got != 999 {
		t.Errorf("Or on an unknown cost = %s, want the fallback 999", got)
	}
	if got := usage.PartialCost(money.Money(500), nil, "partial").Or(999); got != 999 {
		t.Errorf("Or on a partial cost = %s, want the fallback: a floor is not a total", got)
	}
}

// All three states must survive persistence intact. A completeness lost in
// serialization turns a floor back into a total.
func TestCostJSONRoundTripsAllStates(t *testing.T) {
	cases := []struct {
		name string
		cost usage.Cost
	}{
		{"known", usage.KnownCost(money.Money(4_500_000))},
		{"known zero", usage.KnownCost(0)},
		{"unknown", usage.UnknownCost("no price for anthropic.new-model")},
		{"partial", usage.PartialCost(money.Money(18_000_000),
			[]usage.Dimension{usage.Dimension("video-sec")}, "no rate for video-sec")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.cost)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var back usage.Cost
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if back.Completeness != c.cost.Completeness {
				t.Errorf("Completeness = %q, want %q", back.Completeness, c.cost.Completeness)
			}
			if back.Known() != c.cost.Known() {
				t.Errorf("Known() = %v, want %v", back.Known(), c.cost.Known())
			}
			if back.AtLeast() != c.cost.AtLeast() {
				t.Errorf("AtLeast = %s, want %s", back.AtLeast(), c.cost.AtLeast())
			}
			if len(back.Unpriced) != len(c.cost.Unpriced) {
				t.Errorf("Unpriced = %v, want %v", back.Unpriced, c.cost.Unpriced)
			}
			if back.Reason != c.cost.Reason {
				t.Errorf("Reason = %q, want %q", back.Reason, c.cost.Reason)
			}
		})
	}
}

// An unknown cost must not persist an amount at all: a stored zero would read back
// as a free request to anything that ignored completeness.
func TestUnknownCostPersistsNoAmount(t *testing.T) {
	b, err := json.Marshal(usage.UnknownCost("no price"))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), `"amount"`) {
		t.Errorf("marshaled unknown cost = %s, want no amount field", b)
	}
}

// Records written before completeness existed carried only an amount and a
// boolean. Reading one back must not downgrade a known cost to unpriced.
func TestCostJSONBackCompat(t *testing.T) {
	t.Run("amount present means known", func(t *testing.T) {
		var c usage.Cost
		if err := json.Unmarshal([]byte(`{"amount":4500000}`), &c); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if !c.Known() || c.Amount != money.Money(4_500_000) {
			t.Errorf("got %+v, want a known 4500000", c)
		}
	})

	t.Run("no amount means unknown", func(t *testing.T) {
		var c usage.Cost
		if err := json.Unmarshal([]byte(`{"reason":"no price"}`), &c); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if c.Known() {
			t.Error("a record with no amount must not read back as known")
		}
	})

	t.Run("an unrecognized completeness is an error", func(t *testing.T) {
		var c usage.Cost
		if err := json.Unmarshal([]byte(`{"amount":1,"completeness":"probably"}`), &c); err == nil {
			t.Error("an unrecognized completeness must fail rather than be guessed at")
		}
	})
}
