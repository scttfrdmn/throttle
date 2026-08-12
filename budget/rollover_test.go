package budget

import (
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/money"
)

// alloc is the closing period's allocation, which a percentage cap resolves
// against. These tests use absolute caps, where it does not affect the result.
var alloc = dollars(4_000)

func TestClose(t *testing.T) {
	e := testEnvelope() // $4000

	if got, want := e.Close(dollars(3_000)), dollars(1_000); got != want {
		t.Errorf("underspent balance = %s, want %s", got, want)
	}
	if got := e.Close(dollars(4_000)); got != 0 {
		t.Errorf("exact balance = %s, want 0", got)
	}
	if got, want := e.Close(dollars(4_500)), dollars(-500); got != want {
		t.Errorf("overspent balance = %s, want %s", got, want)
	}

	// Carry participates in the closing balance.
	e.Carry = dollars(500)
	if got, want := e.Close(dollars(4_000)), dollars(500); got != want {
		t.Errorf("balance with carry = %s, want %s", got, want)
	}
}

func TestCarryIntoRolloverNone(t *testing.T) {
	p := RolloverPolicy{Mode: RolloverNone}
	if got := p.CarryInto(dollars(1_000), alloc); got != 0 {
		t.Errorf("credit with rollover none = %s, want 0", got)
	}
	if got := p.CarryInto(dollars(-1_000), alloc); got != 0 {
		t.Errorf("debt with rollover none = %s, want 0", got)
	}

	// The zero value must behave identically to an explicit "none".
	var zero RolloverPolicy
	if got := zero.CarryInto(dollars(1_000), alloc); got != 0 {
		t.Errorf("zero-value policy carried %s, want 0", got)
	}
}

func TestCarryIntoRolloverCredit(t *testing.T) {
	p := RolloverPolicy{Mode: RolloverCredit}
	if got, want := p.CarryInto(dollars(1_000), alloc), dollars(1_000); got != want {
		t.Errorf("credit = %s, want %s", got, want)
	}
	// Credit mode never carries debt forward.
	if got := p.CarryInto(dollars(-1_000), alloc); got != 0 {
		t.Errorf("debt under credit mode = %s, want 0", got)
	}
}

func TestCarryIntoRolloverBalance(t *testing.T) {
	p := RolloverPolicy{Mode: RolloverBalance}
	if got, want := p.CarryInto(dollars(1_000), alloc), dollars(1_000); got != want {
		t.Errorf("credit = %s, want %s", got, want)
	}
	if got, want := p.CarryInto(dollars(-1_000), alloc), dollars(-1_000); got != want {
		t.Errorf("debt = %s, want %s", got, want)
	}
}

func TestRolloverCapLimitsCreditOnly(t *testing.T) {
	for _, mode := range []RolloverMode{RolloverCredit, RolloverBalance} {
		p := RolloverPolicy{Mode: mode, Cap: dollars(250)}

		if got, want := p.CarryInto(dollars(1_000), alloc), dollars(250); got != want {
			t.Errorf("%s: capped credit = %s, want %s", mode, got, want)
		}
		if got, want := p.CarryInto(dollars(100), alloc), dollars(100); got != want {
			t.Errorf("%s: credit under cap = %s, want %s", mode, got, want)
		}
		if got, want := p.CarryInto(dollars(250), alloc), dollars(250); got != want {
			t.Errorf("%s: credit exactly at cap = %s, want %s", mode, got, want)
		}
	}

	// A cap must never shrink carried debt: that would forgive real spend.
	p := RolloverPolicy{Mode: RolloverBalance, Cap: dollars(250)}
	if got, want := p.CarryInto(dollars(-1_000), alloc), dollars(-1_000); got != want {
		t.Errorf("capped debt = %s, want %s (debt must not be capped)", got, want)
	}
}

func TestNextEnvelopeTilesTheTimeline(t *testing.T) {
	e := testEnvelope()
	e.Rollover = RolloverPolicy{Mode: RolloverCredit}

	nextEnd := e.End.Add(30 * 24 * time.Hour)
	n := e.Next("research-2", nextEnd, dollars(3_000))

	if err := n.Validate(); err != nil {
		t.Fatalf("successor envelope invalid: %v", err)
	}
	if n.ID != "research-2" {
		t.Errorf("ID = %q", n.ID)
	}
	if !n.Start.Equal(e.End) {
		t.Errorf("successor starts at %v, want %v (periods must not gap or overlap)", n.Start, e.End)
	}
	if !n.End.Equal(nextEnd) {
		t.Errorf("successor ends at %v, want %v", n.End, nextEnd)
	}
	if want := dollars(1_000); n.Carry != want {
		t.Errorf("Carry = %s, want %s", n.Carry, want)
	}
	if n.Allocation != e.Allocation {
		t.Errorf("Allocation = %s, want %s (a new period grants new money)", n.Allocation, e.Allocation)
	}
	// The successor's spendable total includes the banked carry.
	if want := dollars(5_000); n.Total() != want {
		t.Errorf("Total = %s, want %s", n.Total(), want)
	}
}

// TestPeriodBoundaryWithRolloverOnAndOff is the question "what happens at the
// period boundary with rollover enabled or disabled?", asserted end to end.
func TestPeriodBoundaryWithRolloverOnAndOff(t *testing.T) {
	const spent = 3_000 // $1000 underspent on a $4000 envelope.

	cases := []struct {
		name      string
		policy    RolloverPolicy
		wantCarry money.Money
		wantTotal money.Money
	}{
		{"disabled", RolloverPolicy{Mode: RolloverNone}, 0, dollars(4_000)},
		{"credit", RolloverPolicy{Mode: RolloverCredit}, dollars(1_000), dollars(5_000)},
		{"credit capped", RolloverPolicy{Mode: RolloverCredit, Cap: dollars(250)}, dollars(250), dollars(4_250)},
		{"balance", RolloverPolicy{Mode: RolloverBalance}, dollars(1_000), dollars(5_000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := testEnvelope()
			e.Rollover = tc.policy
			n := e.Next("next", e.End.Add(30*24*time.Hour), dollars(spent))

			if n.Carry != tc.wantCarry {
				t.Errorf("Carry = %s, want %s", n.Carry, tc.wantCarry)
			}
			if n.Total() != tc.wantTotal {
				t.Errorf("Total = %s, want %s", n.Total(), tc.wantTotal)
			}
			// The new period starts unspent, so nothing is paced at its start.
			if got := n.Target(n.Start); got != tc.wantCarry {
				t.Errorf("Target at new start = %s, want %s", got, tc.wantCarry)
			}
		})
	}
}

// TestPeriodBoundaryAfterOverspend covers debt at the boundary, where the
// modes differ most: only balance mode makes the next period pay it back.
func TestPeriodBoundaryAfterOverspend(t *testing.T) {
	const spent = 4_500 // $500 over a $4000 envelope.

	cases := []struct {
		name      string
		mode      RolloverMode
		wantCarry money.Money
		wantTotal money.Money
	}{
		{"disabled forgives debt", RolloverNone, 0, dollars(4_000)},
		{"credit forgives debt", RolloverCredit, 0, dollars(4_000)},
		{"balance carries debt", RolloverBalance, dollars(-500), dollars(3_500)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := testEnvelope()
			e.Rollover = RolloverPolicy{Mode: tc.mode}
			n := e.Next("next", e.End.Add(30*24*time.Hour), dollars(spent))

			if n.Carry != tc.wantCarry {
				t.Errorf("Carry = %s, want %s", n.Carry, tc.wantCarry)
			}
			if n.Total() != tc.wantTotal {
				t.Errorf("Total = %s, want %s", n.Total(), tc.wantTotal)
			}
			if err := n.Validate(); err != nil {
				t.Errorf("successor invalid: %v", err)
			}
		})
	}
}

// TestDebtCarryCannotBeSpentUntilEarned checks that an envelope inheriting debt
// starts underwater and must pace its way out.
func TestDebtCarryCannotBeSpentUntilEarned(t *testing.T) {
	e := testEnvelope()
	e.Rollover = RolloverPolicy{Mode: RolloverBalance}
	n := e.Next("next", e.End.Add(30*24*time.Hour), dollars(4_500))

	// At the very start, the inherited debt means nothing may be spent.
	if d := n.Admit(n.Start, 0, 0, dollars(1)); d.Outcome != OutcomeWait {
		t.Errorf("Outcome = %q, want wait (%s)", d.Outcome, d.Reason)
	}
	s := n.Snapshot(n.Start, 0, 0)
	if want := dollars(-500); s.Target != want {
		t.Errorf("Target at start = %s, want %s", s.Target, want)
	}
	if s.AvailableNow != 0 {
		t.Errorf("AvailableNow = %s, want 0 while underwater", s.AvailableNow)
	}

	// After pacing earns past the debt, spending is possible again.
	// $500 of debt against $4000/30d is repaid in 3.75 days.
	afterRepay := n.Start.Add(4 * 24 * time.Hour)
	if d := n.Admit(afterRepay, 0, 0, dollars(1)); d.Outcome != OutcomeAllow {
		t.Errorf("Outcome after repayment = %q, want allow (%s)", d.Outcome, d.Reason)
	}
}

func TestMultiPeriodChainConservesMoney(t *testing.T) {
	// Six sequential periods under balance rollover: total authorized spend must
	// equal total allocation plus or minus the final balance, with no leakage.
	e := testEnvelope()
	e.Rollover = RolloverPolicy{Mode: RolloverBalance}

	spends := []money.Money{
		dollars(3_000), dollars(5_000), dollars(4_000),
		dollars(2_000), dollars(4_500), dollars(3_500),
	}
	var totalAllocated, totalSpent money.Money
	cur := e
	for i, spent := range spends {
		if err := cur.Validate(); err != nil {
			t.Fatalf("period %d invalid: %v", i, err)
		}
		totalAllocated += cur.Allocation
		totalSpent += spent
		cur = cur.Next("p", cur.End.Add(30*24*time.Hour), spent)
	}

	// The final carry is exactly what was allocated but not spent.
	wantCarry := totalAllocated - totalSpent
	if cur.Carry != wantCarry {
		t.Errorf("final carry = %s, want %s (allocated %s, spent %s)",
			cur.Carry, wantCarry, totalAllocated, totalSpent)
	}
}
