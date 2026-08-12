package report

import (
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/money"
)

// day is the sampling granularity most of these tests reason in.
const day = 24 * time.Hour

// envelope is a plain $1000 envelope over the reference month.
func envelope(alloc money.Money) budget.Envelope {
	return budget.Envelope{
		ID:         "research",
		Allocation: alloc,
		Start:      base,
		End:        base.Add(monthDuration),
	}
}

// --- pacing semantics ------------------------------------------------------

// The pace balance must be signed and it must be measured against settled spend.
// An absolute value would turn "borrowed $40" into "banked $40".
func TestPaceBalanceIsSignedBothWays(t *testing.T) {
	env := envelope(dollars(1000))
	def := monthly("research", "", dollars(1000))
	half := base.Add(monthDuration / 2)

	cases := []struct {
		name  string
		spent money.Money
		want  money.Money
		sign  string
	}{
		// Half the month elapsed, so the target is $500.
		{"behind pace banks", dollars(200), dollars(300), "banked"},
		{"exactly on pace", dollars(500), 0, "level"},
		{"ahead of pace borrows", dollars(700), dollars(-200), "borrowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := position(def, period(env), totals(tc.spent, 0), half)
			if pos.TargetByNow != dollars(500) {
				t.Fatalf("TargetByNow = %s, want %s", pos.TargetByNow, dollars(500))
			}
			if pos.PaceBalance != tc.want {
				t.Errorf("PaceBalance = %s, want %s", pos.PaceBalance, tc.want)
			}
			switch tc.sign {
			case "banked":
				if !pos.Banked() || pos.Borrowed() {
					t.Errorf("Banked=%v Borrowed=%v, want banked", pos.Banked(), pos.Borrowed())
				}
			case "borrowed":
				if !pos.Borrowed() || pos.Banked() {
					t.Errorf("Banked=%v Borrowed=%v, want borrowed", pos.Banked(), pos.Borrowed())
				}
			default:
				if pos.Banked() || pos.Borrowed() {
					t.Errorf("Banked=%v Borrowed=%v, want neither", pos.Banked(), pos.Borrowed())
				}
			}
		})
	}
}

// The pace balance is target minus SETTLED spend, not target minus committed.
// budget.Snapshot.Bank is the second thing, and it answers the engine's admission
// question rather than the dashboard's "did money leave faster than planned?".
func TestPaceBalanceIgnoresReservationsAndSnapshotBankDoesNot(t *testing.T) {
	env := envelope(dollars(1000))
	def := monthly("research", "", dollars(1000))
	half := base.Add(monthDuration / 2)

	pos := position(def, period(env), totals(dollars(400), dollars(300)), half)

	if want := dollars(100); pos.PaceBalance != want {
		t.Errorf("PaceBalance = %s, want %s (target 500 - spent 400)", pos.PaceBalance, want)
	}
	snap := env.Snapshot(half, dollars(400), dollars(300))
	if want := dollars(-200); snap.Bank != want {
		t.Fatalf("budget bank = %s, want %s; this test's premise has changed", snap.Bank, want)
	}
	if pos.PaceBalance == snap.Bank {
		t.Error("PaceBalance and Snapshot.Bank agree; the read model has started reporting the admission figure as the pace balance")
	}
}

// Reserved is money that is promised, not money that is gone. It must never be
// added into Spent, and Committed must be available as its own field so no caller
// has to add the two and risk labelling the sum "spent".
func TestReservedIsNeverFoldedIntoSpent(t *testing.T) {
	def := monthly("research", "", dollars(1000))
	pos := position(def, period(envelope(dollars(1000))),
		totals(dollars(120), dollars(80)), base.Add(monthDuration/2))

	if pos.Spent != dollars(120) {
		t.Errorf("Spent = %s, want %s", pos.Spent, dollars(120))
	}
	if pos.Reserved != dollars(80) {
		t.Errorf("Reserved = %s, want %s", pos.Reserved, dollars(80))
	}
	if pos.Committed != dollars(200) {
		t.Errorf("Committed = %s, want %s", pos.Committed, dollars(200))
	}
	if pos.Spent == pos.Committed {
		t.Error("Spent equals Committed with a live reservation: reservations have been folded into spend")
	}
}

// SpendableNow is AllowedByNow-Spent-Reserved. An encumbrance genuinely reduces
// what may be committed next, which is the one place Reserved belongs.
func TestSpendableNowSubtractsReservations(t *testing.T) {
	def := monthly("research", "", dollars(1000))
	env := envelope(dollars(1000))
	half := base.Add(monthDuration / 2)

	free := position(def, period(env), totals(dollars(100), 0), half)
	held := position(def, period(env), totals(dollars(100), dollars(150)), half)

	if want := dollars(400); free.SpendableNow != want {
		t.Errorf("SpendableNow with no holds = %s, want %s", free.SpendableNow, want)
	}
	if want := dollars(250); held.SpendableNow != want {
		t.Errorf("SpendableNow with a $150 hold = %s, want %s", held.SpendableNow, want)
	}
	if diff := free.SpendableNow - held.SpendableNow; diff != dollars(150) {
		t.Errorf("a $150 hold reduced spendable-now by %s, want $150.00", diff)
	}
}

// RemainingAllocation is signed: an overcommitted envelope reports the overrun
// rather than clamping to zero.
func TestRemainingAllocationIsSignedWhenOverspent(t *testing.T) {
	def := monthly("research", "", dollars(1000))
	pos := position(def, period(envelope(dollars(1000))),
		totals(dollars(1100), 0), base.Add(monthDuration/2))

	if !pos.Overspent() {
		t.Error("Overspent() = false with $1100 spent against a $1000 envelope")
	}
	if want := dollars(-100); pos.RemainingAllocation != want {
		t.Errorf("RemainingAllocation = %s, want %s", pos.RemainingAllocation, want)
	}
	if pos.SpendableNow != 0 {
		t.Errorf("SpendableNow = %s, want 0: a negative amount is not a spendable quantity", pos.SpendableNow)
	}
}

// --- burn rates ------------------------------------------------------------

// Average burn is exactly Spent/elapsed. Nothing is smoothed, weighted, or
// windowed: it is an average to date and it is labelled as one.
func TestAverageBurnIsSpentOverElapsedTime(t *testing.T) {
	// $240 over 10 days is $1/hour.
	r := averageBurn(dollars(240), 10*day, day, ConfidenceOK)
	if !r.Known {
		t.Fatal("Known = false with real elapsed time and real spend")
	}
	if want := dollars(1); r.PerHour != want {
		t.Errorf("PerHour = %s, want %s", r.PerHour, want)
	}
	if want := dollars(24); r.Suggested() != want {
		t.Errorf("Suggested (per day) = %s, want %s", r.Suggested(), want)
	}
}

// Sustainable burn is the remaining spendable allocation over the remaining time.
func TestSustainableBurnIsRemainingOverRemainingTime(t *testing.T) {
	// $480 left over 10 days is $2/hour.
	r := sustainableBurn(dollars(480), 10*day, day)
	if !r.Known {
		t.Fatal("Known = false with headroom and time left")
	}
	if want := dollars(2); r.PerHour != want {
		t.Errorf("PerHour = %s, want %s", r.PerHour, want)
	}
	if want := dollars(48); r.In(day) != want {
		t.Errorf("In(day) = %s, want %s", r.In(day), want)
	}
}

// The two rates are the ones the brief names, and the position wires them to those
// formulas and no others.
func TestPositionUsesTheTwoNamedBurnFormulas(t *testing.T) {
	def := monthly("research", "", dollars(1000))
	env := envelope(dollars(1000))
	at := base.Add(10 * day)

	pos := position(def, period(env), totals(dollars(240), 0), at)

	if want := perHour(dollars(240), 10*day); pos.AverageBurn.PerHour != want {
		t.Errorf("AverageBurn = %s, want spent/elapsed = %s", pos.AverageBurn.PerHour, want)
	}
	if want := perHour(dollars(760), 21*day); pos.SustainableBurn.PerHour != want {
		t.Errorf("SustainableBurn = %s, want remaining/timeLeft = %s", pos.SustainableBurn.PerHour, want)
	}
}

// A rate's stored value is per hour regardless of the unit a display prefers, so a
// later comparison of two rates cannot be wrong because one was rescaled.
func TestRateStaysDurationNormalized(t *testing.T) {
	short := averageBurn(dollars(24), 24*time.Hour, displayUnit(6*time.Hour), ConfidenceOK)
	long := averageBurn(dollars(24), 24*time.Hour, displayUnit(31*day), ConfidenceOK)

	if short.PerHour != long.PerHour {
		t.Errorf("PerHour differs by display unit: %s vs %s", short.PerHour, long.PerHour)
	}
	if short.Per != time.Hour {
		t.Errorf("a six-hour period suggests %s, want hourly", short.Per)
	}
	if long.Per != day {
		t.Errorf("a monthly period suggests %s, want daily", long.Per)
	}
	// Rounding happens once, at the point of display.
	if want := dollars(24); long.In(day) != want {
		t.Errorf("In(day) = %s, want %s", long.In(day), want)
	}
}

// No elapsed time is not a rate of zero, and neither is an exhausted budget.
func TestRatesAreUnknownRatherThanZeroAtTheEdges(t *testing.T) {
	if r := averageBurn(dollars(100), 0, day, ConfidenceNone); r.Known {
		t.Error("average burn is Known with zero elapsed time")
	}
	if r := sustainableBurn(dollars(100), 0, day); r.Known {
		t.Error("sustainable burn is Known with no time remaining")
	}
	if r := sustainableBurn(dollars(-5), 10*day, day); r.Known {
		t.Error("sustainable burn is Known with a negative remaining allocation")
	}
	if r := sustainableBurn(dollars(-5), 10*day, day); r.PerHour < 0 {
		t.Errorf("sustainable burn is negative (%s): an overspent budget has no sustainable rate", r.PerHour)
	}
}

// perHour must not divide by zero on any input.
func TestPerHourIsSafeAtZero(t *testing.T) {
	if got := perHour(dollars(10), 0); got != 0 {
		t.Errorf("perHour(_, 0) = %s, want 0", got)
	}
	if got := perHour(0, day); got != 0 {
		t.Errorf("perHour(0, _) = %s, want 0", got)
	}
}

// --- the throttle gauge ----------------------------------------------------

// 100% means precisely one thing: the average rate to date is the rate that
// consumes the remaining allocation over the remaining time.
func TestPressureAtOneHundredPercentHasExactMeaning(t *testing.T) {
	// $100 over 10 days against $100 left over 10 days.
	p := pressure(dollars(100), 10*day, dollars(100), 10*day, ConfidenceOK)
	if !p.Measured() {
		t.Fatalf("State = %q, want measured", p.State)
	}
	if p.BasisPoints != 10_000 {
		t.Errorf("BasisPoints = %d, want 10000 (exactly 100%%)", p.BasisPoints)
	}
	if p.OverRedline() {
		t.Error("OverRedline() = true at exactly 100%")
	}
}

// The gauge is not percent of budget spent. This is the case that proves it: 90% of
// the budget is gone and the gauge reads exactly 100%, because the remaining tenth
// exactly matches the remaining tenth of the month.
func TestPressureIsNotPercentOfBudgetSpent(t *testing.T) {
	env := envelope(dollars(1000))
	def := monthly("research", "", dollars(1000))
	at := base.Add(time.Duration(float64(monthDuration) * 0.9))

	pos := position(def, period(env), totals(dollars(900), 0), at)

	if !pos.Pressure.Measured() {
		t.Fatalf("State = %q, want measured", pos.Pressure.State)
	}
	// Allow a basis point of slack for the truncation of 90% of a month.
	if bp := pos.Pressure.BasisPoints; bp < 9_990 || bp > 10_010 {
		t.Errorf("BasisPoints = %d, want ~10000 while 90%% of the budget is spent", bp)
	}
	spentShare := int64(pos.Spent) * 10_000 / int64(pos.Total)
	if spentShare == pos.Pressure.BasisPoints {
		t.Error("the gauge equals percent-of-budget-spent; they must be different measurements")
	}
}

// The other direction: barely started, burning far too fast.
func TestPressureReportsOverRedline(t *testing.T) {
	env := envelope(dollars(1000))
	def := monthly("research", "", dollars(1000))
	at := base.Add(time.Duration(float64(monthDuration) * 0.02))

	pos := position(def, period(env), totals(dollars(100), 0), at)

	if !pos.Pressure.OverRedline() {
		t.Errorf("OverRedline() = false at %d bp with 10%% spent in 2%% of the month",
			pos.Pressure.BasisPoints)
	}
	if pos.Pressure.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %q after 2%% of the period, want low", pos.Pressure.Confidence)
	}
}

// The low-confidence threshold has to hold at the scale budgets are actually written at.
//
// This is a regression test for an arithmetic overflow rather than for a judgement: the
// threshold compared elapsed*10,000 against duration*500 in nanoseconds, which wraps
// int64 at about ten and a half days. A month-long budget at its halfway mark therefore
// reported "very little of the period has elapsed" over fifteen days of real data, and
// every figure the caveat attaches to -- both rates, the gauge, the projection -- carried
// it. The bug lived in the ordinary case, which is why no edge-case test found it.
func TestConfidenceHoldsAtPeriodsLongerThanTenDays(t *testing.T) {
	for _, p := range []struct {
		name     string
		duration time.Duration
	}{
		{"an hour", time.Hour},
		{"a day", day},
		{"a week", 7 * day},
		{"a month", 31 * day},
		{"a quarter", 92 * day},
		{"an academic year", 273 * day},
		{"a decade", 3653 * day},
	} {
		// Below the 5% threshold is low; above it is not. The point of the sweep is that
		// the answer must not depend on the period's length.
		low := time.Duration(float64(p.duration) * 0.01)
		if got := confidence(low, p.duration); got != ConfidenceLow {
			t.Errorf("confidence(1%% of %s) = %q, want low", p.name, got)
		}
		for _, frac := range []float64{0.10, 0.25, 0.50, 0.75, 0.99} {
			el := time.Duration(float64(p.duration) * frac)
			if got := confidence(el, p.duration); got != ConfidenceOK {
				t.Errorf("confidence(%.0f%% of %s) = %q, want ok", frac*100, p.name, got)
			}
		}
		if got := confidence(p.duration, p.duration); got != ConfidenceOK {
			t.Errorf("confidence(all of %s) = %q, want ok", p.name, got)
		}
	}

	// The threshold itself: 5% of a period is the boundary, in both directions, and a
	// month is long enough that the old expression got this backwards.
	month := 31 * day
	if got := confidence(time.Duration(float64(month)*0.049), month); got != ConfidenceLow {
		t.Errorf("confidence(4.9%% of a month) = %q, want low", got)
	}
	if got := confidence(time.Duration(float64(month)*0.051), month); got != ConfidenceOK {
		t.Errorf("confidence(5.1%% of a month) = %q, want ok", got)
	}

	// Sub-second periods cannot reach the threshold by rounding to zero on both sides.
	if got := confidence(time.Millisecond, 100*time.Millisecond); got != ConfidenceOK {
		t.Errorf("confidence over a sub-second period = %q, want ok rather than a divide", got)
	}
}

// Four states, only one of which is a number. A gauge that rendered every
// unmeasurable condition as 0% would report an idle workload, an over-budget one,
// and a period that has not started as the same healthy state.
func TestPressureEdgeCasesAreStatesNotZero(t *testing.T) {
	cases := []struct {
		name      string
		spent     money.Money
		elapsed   time.Duration
		remaining money.Money
		timeLeft  time.Duration
		want      PressureState
	}{
		{"period has not started", 0, 0, dollars(100), 10 * day, PressureNotStarted},
		{"period has ended", dollars(50), 10 * day, dollars(50), 0, PressureEnded},
		{"allocation exhausted", dollars(100), 10 * day, 0, 10 * day, PressureNoHeadroom},
		{"allocation overspent", dollars(120), 10 * day, dollars(-20), 10 * day, PressureNoHeadroom},
		{"nothing burned yet", 0, 10 * day, dollars(100), 10 * day, PressureMeasured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := pressure(tc.spent, tc.elapsed, tc.remaining, tc.timeLeft, ConfidenceOK)
			if p.State != tc.want {
				t.Fatalf("State = %q, want %q", p.State, tc.want)
			}
			if tc.want != PressureMeasured && p.Measured() {
				t.Error("Measured() = true for a state that has no reading")
			}
			if tc.want != PressureMeasured && p.OverRedline() {
				t.Error("OverRedline() = true for a state that has no reading")
			}
		})
	}

	// An idle workload over real elapsed time is the only zero the gauge produces.
	idle := pressure(0, 10*day, dollars(100), 10*day, ConfidenceOK)
	if !idle.Measured() || idle.BasisPoints != 0 {
		t.Errorf("idle gauge = %d bp in state %q, want a measured 0", idle.BasisPoints, idle.State)
	}
}

// The gauge is one exact expression rather than the quotient of two rounded rates,
// so it does not accumulate their rounding. This pins the arithmetic on a case
// where dividing the rounded figures visibly disagrees.
func TestPressureIsNotTheQuotientOfTwoRoundedRates(t *testing.T) {
	spent, elapsed := money.Money(7), 3*time.Hour
	remaining, left := money.Money(11), 7*time.Hour

	p := pressure(spent, elapsed, remaining, left, ConfidenceOK)

	// The honest value: 10000 * 7 * 7h / (3h * 11) = 14848.48... -> 14848.
	if p.BasisPoints != 14_848 {
		t.Errorf("BasisPoints = %d, want 14848", p.BasisPoints)
	}
	// What dividing the two displayed per-hour rates would have produced. Both
	// truncate to microdollars per hour, and at these magnitudes that is severe.
	avg, sus := perHour(spent, elapsed), perHour(remaining, left)
	if sus != 0 {
		naive := int64(avg) * 10_000 / int64(sus)
		if naive == p.BasisPoints {
			t.Skip("the two methods agree on this input; the test needs a sharper one")
		}
	}
}

// --- projection ------------------------------------------------------------

// The projection is straight-line: spent scaled by the whole period over the
// elapsed part, and nothing more sophisticated.
func TestProjectionIsStraightLine(t *testing.T) {
	env := envelope(dollars(1000))
	at := base.Add(10 * day)
	snap := env.Snapshot(at, dollars(300), 0)

	proj := projection(env, snap, env.Elapsed(at), ConfidenceOK)

	if !proj.Known {
		t.Fatal("Known = false with real elapsed spend")
	}
	// $300 in 10 of 31 days extrapolates to $930.
	if want := dollars(930); proj.Amount != want {
		t.Errorf("Amount = %s, want %s", proj.Amount, want)
	}
	if proj.OverBy != 0 {
		t.Errorf("OverBy = %s, want 0 for a projection under the total", proj.OverBy)
	}
	if want := dollars(70); proj.UnderBy != want {
		t.Errorf("UnderBy = %s, want %s", proj.UnderBy, want)
	}
}

// The read model's projection must agree with the engine's, or two parts of
// throttle would answer the same question differently.
func TestProjectionAgreesWithTheEngine(t *testing.T) {
	w := newLedgerOnlyWorld(t)
	def := monthly("research", "", dollars(1000))
	w.define(def)
	w.now = base.Add(10 * day)
	w.spend("r1", "research", dollars(300), base.Add(day))

	pos, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	eng, err := engineOver(w)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	st, err := eng.Status(w.ctx, "research")
	if err != nil {
		t.Fatalf("engine Status: %v", err)
	}
	if pos.Position.Projection.Amount != st.ProjectedSpend {
		t.Errorf("report projects %s, engine projects %s",
			pos.Position.Projection.Amount, st.ProjectedSpend)
	}
}

// Before any time has elapsed there is no rate to extrapolate, so there is no
// projection -- rather than a "projection" that is really just the current figure.
func TestProjectionIsUnknownBeforeAnyElapsedTime(t *testing.T) {
	env := envelope(dollars(1000))
	snap := env.Snapshot(base, dollars(50), dollars(10))

	proj := projection(env, snap, 0, ConfidenceNone)

	if proj.Known {
		t.Error("Known = true at the instant the period began")
	}
	if proj.Amount != 0 {
		t.Errorf("Amount = %s, want 0 when there is nothing to project from", proj.Amount)
	}
	if proj.Confidence != ConfidenceNone {
		t.Errorf("Confidence = %q, want none", proj.Confidence)
	}
}

// A projection over an elapsed period is just what was spent, and one that has
// overrun says by how much.
func TestProjectionOverTheTotalReportsTheOverrun(t *testing.T) {
	env := envelope(dollars(1000))
	at := base.Add(10 * day)
	snap := env.Snapshot(at, dollars(500), 0)

	proj := projection(env, snap, env.Elapsed(at), ConfidenceOK)
	if want := dollars(1550); proj.Amount != want {
		t.Errorf("Amount = %s, want %s", proj.Amount, want)
	}
	if want := dollars(550); proj.OverBy != want {
		t.Errorf("OverBy = %s, want %s", proj.OverBy, want)
	}
	if proj.UnderBy != 0 {
		t.Errorf("UnderBy = %s, want 0 for an overrunning projection", proj.UnderBy)
	}

	end := base.Add(monthDuration)
	done := projection(env, env.Snapshot(end, dollars(700), 0), env.Elapsed(end), ConfidenceOK)
	if want := dollars(700); done.Amount != want {
		t.Errorf("Amount at period end = %s, want the settled figure %s", done.Amount, want)
	}
}

// A figure derived from a sliver of elapsed time is shown and marked, not hidden
// and not dressed up.
func TestConfidenceMarksVeryEarlyFigures(t *testing.T) {
	cases := []struct {
		elapsed time.Duration
		want    Confidence
	}{
		{0, ConfidenceNone},
		{time.Minute, ConfidenceLow},
		{monthDuration / 100, ConfidenceLow},
		{monthDuration / 10, ConfidenceOK},
		{monthDuration, ConfidenceOK},
	}
	for _, tc := range cases {
		if got := confidence(tc.elapsed, monthDuration); got != tc.want {
			t.Errorf("confidence(%s of a month) = %q, want %q", tc.elapsed, got, tc.want)
		}
	}
	if got := confidence(time.Hour, 0); got != ConfidenceNone {
		t.Errorf("confidence over a zero-length period = %q, want none", got)
	}
}

// --- zero-duration and boundary safety ------------------------------------

// A period with no length, a clock before the start, and a clock after the end
// must all produce a Position rather than a panic or a divide-by-zero.
func TestPositionIsSafeAtDegenerateBoundaries(t *testing.T) {
	def := monthly("research", "", dollars(1000))
	env := envelope(dollars(1000))

	cases := map[string]time.Time{
		"before the period starts": base.Add(-day),
		"exactly at the start":     base,
		"exactly at the end":       base.Add(monthDuration),
		"after the period ends":    base.Add(monthDuration + day),
	}
	for name, at := range cases {
		t.Run(name, func(t *testing.T) {
			pos := position(def, period(env), totals(dollars(100), dollars(10)), at)
			if pos.Elapsed < 0 {
				t.Errorf("Elapsed = %s, want a clamped non-negative duration", pos.Elapsed)
			}
			if pos.TimeRemaining < 0 {
				t.Errorf("TimeRemaining = %s, want a clamped non-negative duration", pos.TimeRemaining)
			}
			if pos.Pressure.Measured() && pos.Pressure.BasisPoints < 0 {
				t.Errorf("BasisPoints = %d, want a non-negative reading", pos.Pressure.BasisPoints)
			}
		})
	}
}

// The gauge's own denominators, exercised directly at zero. There is no reachable
// division by zero in the read model's arithmetic.
func TestGaugeAndRatesSurviveAZeroLengthPeriod(t *testing.T) {
	def := monthly("instant", "", dollars(10))
	env := budget.Envelope{
		ID: "instant", Allocation: dollars(10),
		Start: base, End: base.Add(time.Nanosecond),
	}
	pos := position(def, period(env), totals(dollars(5), 0), base.Add(time.Hour))

	if pos.Pressure.State != PressureEnded {
		t.Errorf("State = %q, want ended", pos.Pressure.State)
	}
	if pos.SustainableBurn.Known {
		t.Error("SustainableBurn is Known with no time remaining")
	}
	if pos.PeriodDuration <= 0 {
		t.Errorf("PeriodDuration = %s, want a positive duration", pos.PeriodDuration)
	}
}

// divRound rounds half away from zero in both directions, which is the rule
// pricing follows.
func TestRateInRoundsHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		perHour money.Money
		in      time.Duration
		want    money.Money
	}{
		{3, 30 * time.Minute, 2},   // 1.5 -> 2
		{-3, 30 * time.Minute, -2}, // -1.5 -> -2
		{1, 30 * time.Minute, 1},   // 0.5 -> 1
		{2, 90 * time.Minute, 3},   // exact
	}
	for _, tc := range cases {
		r := Rate{PerHour: tc.perHour, Known: true}
		if got := r.In(tc.in); got != tc.want {
			t.Errorf("Rate{%d}.In(%s) = %d, want %d", tc.perHour, tc.in, got, tc.want)
		}
	}
	if got := (Rate{PerHour: 100}).In(time.Hour); got != 0 {
		t.Errorf("an unknown rate converted to %d, want 0", got)
	}
}
