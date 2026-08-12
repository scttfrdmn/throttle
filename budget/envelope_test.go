package budget

import (
	"math/big"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/money"
)

func dollars(d int64) money.Money { return money.Money(d) * money.PerDollar }

func testEnvelope() Envelope {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return Envelope{
		ID:         "research",
		Allocation: dollars(4_000),
		Start:      start,
		End:        start.Add(30 * 24 * time.Hour),
	}
}

func TestLinearBankAndBorrow(t *testing.T) {
	e := testEnvelope()
	e.Borrow = 3 * 24 * time.Hour
	at := e.Start.Add(15 * 24 * time.Hour)
	s := e.Snapshot(at, dollars(1_500), 0)

	if want := dollars(2_000); s.Target != want {
		t.Errorf("Target = %s, want %s", s.Target, want)
	}
	if want := dollars(500); s.Bank != want {
		t.Errorf("Bank = %s, want %s", s.Bank, want)
	}
	if want := dollars(2_400); s.Allowed != want {
		t.Errorf("Allowed = %s, want %s", s.Allowed, want)
	}
	if want := dollars(900); s.AvailableNow != want {
		t.Errorf("AvailableNow = %s, want %s", s.AvailableNow, want)
	}
	if want := dollars(2_500); s.PeriodRemaining != want {
		t.Errorf("PeriodRemaining = %s, want %s", s.PeriodRemaining, want)
	}
}

func TestPeriodBoundaries(t *testing.T) {
	e := testEnvelope()

	// Before and at Start nothing is paced yet.
	if got := e.Target(e.Start.Add(-time.Hour)); got != 0 {
		t.Errorf("Target before start = %s, want 0", got)
	}
	if got := e.Target(e.Start); got != 0 {
		t.Errorf("Target at start = %s, want 0", got)
	}

	// At and after End the full allocation is paced, never more.
	if got := e.Target(e.End); got != e.Allocation {
		t.Errorf("Target at end = %s, want %s", got, e.Allocation)
	}
	if got := e.Target(e.End.Add(365 * 24 * time.Hour)); got != e.Allocation {
		t.Errorf("Target long after end = %s, want %s", got, e.Allocation)
	}

	// One nanosecond before the end must not yet be the full allocation.
	if got := e.Target(e.End.Add(-time.Nanosecond)); got >= e.Allocation {
		t.Errorf("Target just before end = %s, want < %s", got, e.Allocation)
	}
}

func TestTargetIsMonotonic(t *testing.T) {
	e := testEnvelope()
	prev := money.Money(money.Min)
	for h := -2; h <= 24*30+2; h++ {
		got := e.Target(e.Start.Add(time.Duration(h) * time.Hour))
		if got < prev {
			t.Fatalf("Target went backwards at hour %d: %s after %s", h, got, prev)
		}
		if got > e.Total() {
			t.Fatalf("Target at hour %d = %s exceeds total %s", h, got, e.Total())
		}
		prev = got
	}
}

func TestBorrowNeverExceedsTotal(t *testing.T) {
	e := testEnvelope()
	e.Borrow = 100 * 24 * time.Hour // Borrow window longer than the period.

	if got := e.Allowed(e.Start); got != e.Total() {
		t.Errorf("Allowed at start with huge borrow = %s, want %s", got, e.Total())
	}
	for h := 0; h <= 24*30; h++ {
		if got := e.Allowed(e.Start.Add(time.Duration(h) * time.Hour)); got > e.Total() {
			t.Fatalf("Allowed at hour %d = %s exceeds total %s", h, got, e.Total())
		}
	}
}

func TestBorrowPullsForwardExactly(t *testing.T) {
	e := testEnvelope()
	e.Borrow = 72 * time.Hour

	at := e.Start.Add(10 * 24 * time.Hour)
	// 13 days of a 30-day $4000 envelope.
	if want, got := dollars(4_000)*13/30, e.Allowed(at); got != want {
		t.Errorf("Allowed = %s, want %s", got, want)
	}
	// Borrowing must not change the target curve.
	if want, got := dollars(4_000)*10/30, e.Target(at); got != want {
		t.Errorf("Target = %s, want %s", got, want)
	}
}

// TestOverrunIsVisible pins the fix for PeriodRemaining being clamped at zero,
// which reported a $500 overrun as "nothing left" instead of "$500 over".
func TestOverrunIsVisible(t *testing.T) {
	e := testEnvelope()
	s := e.Snapshot(e.Start.Add(15*24*time.Hour), dollars(4_500), 0)

	if want := dollars(-500); s.PeriodRemaining != want {
		t.Errorf("PeriodRemaining = %s, want %s", s.PeriodRemaining, want)
	}
	if !s.Overspent() {
		t.Error("Overspent() = false, want true")
	}
	if s.AvailableNow != 0 {
		t.Errorf("AvailableNow = %s, want 0", s.AvailableNow)
	}
	if want := dollars(-2_500); s.Bank != want {
		t.Errorf("Bank = %s, want %s", s.Bank, want)
	}
	if s.SustainableRate != 0 {
		t.Errorf("SustainableRate = %s, want 0 when overspent", s.SustainableRate)
	}
}

func TestNegativeBankFromDebtCarry(t *testing.T) {
	e := testEnvelope()
	e.Carry = dollars(-1_000) // Inherited debt.

	if want, got := dollars(-1_000), e.Target(e.Start); got != want {
		t.Errorf("Target at start = %s, want %s", got, want)
	}
	if want, got := dollars(3_000), e.Total(); got != want {
		t.Errorf("Total = %s, want %s", got, want)
	}
	// Halfway through, pacing has earned $2000 against $1000 of inherited debt.
	s := e.Snapshot(e.Start.Add(15*24*time.Hour), 0, 0)
	if want := dollars(1_000); s.Target != want {
		t.Errorf("Target = %s, want %s", s.Target, want)
	}
	if want := dollars(1_000); s.AvailableNow != want {
		t.Errorf("AvailableNow = %s, want %s", s.AvailableNow, want)
	}
}

// TestSustainableRateNeverAdvisesOverspend pins the fix for the rate helper
// flooring the remaining time to whole hours and clamping to a one-hour
// minimum, which advised $1000/hour with 90 minutes and $1000 left.
func TestSustainableRateNeverAdvisesOverspend(t *testing.T) {
	e := testEnvelope()

	cases := []struct {
		name      string
		remaining time.Duration
		spent     money.Money
	}{
		{"90 minutes left", 90 * time.Minute, 0},
		{"30 minutes left", 30 * time.Minute, 0},
		{"1 minute left", time.Minute, 0},
		{"1 second left", time.Second, 0},
		{"half spent, 2h left", 2 * time.Hour, dollars(2_000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := e.End.Add(-tc.remaining)
			s := e.Snapshot(at, tc.spent, 0)

			// Burning at the sustainable rate for the remaining time must not
			// exceed the remaining budget. The intermediate product exceeds
			// int64, so this projection is done in big.Int.
			n := new(big.Int).Mul(big.NewInt(int64(s.SustainableRate)), big.NewInt(int64(tc.remaining)))
			n.Quo(n, big.NewInt(int64(time.Hour)))
			if !n.IsInt64() {
				t.Fatalf("projection did not fit in int64: %s", n)
			}
			projected := money.Money(n.Int64())
			if projected > s.PeriodRemaining {
				t.Errorf("burning %s/hour for %v spends %s but only %s remains",
					s.SustainableRate, tc.remaining, projected, s.PeriodRemaining)
			}
		})
	}
}

func TestSustainableRateAfterPeriodEnd(t *testing.T) {
	e := testEnvelope()
	s := e.Snapshot(e.End, 0, 0)
	if s.TimeRemaining != 0 {
		t.Errorf("TimeRemaining = %v, want 0", s.TimeRemaining)
	}
	if s.SustainableRate != 0 {
		t.Errorf("SustainableRate = %s, want 0", s.SustainableRate)
	}
}

func TestReservationsConsumeHeadroom(t *testing.T) {
	e := testEnvelope()
	at := e.Start.Add(15 * 24 * time.Hour)
	s := e.Snapshot(at, dollars(1_000), dollars(600))

	if want := dollars(1_600); s.Committed != want {
		t.Errorf("Committed = %s, want %s", s.Committed, want)
	}
	if want := dollars(400); s.AvailableNow != want {
		t.Errorf("AvailableNow = %s, want %s", s.AvailableNow, want)
	}
	if want := dollars(400); s.Bank != want {
		t.Errorf("Bank = %s, want %s", s.Bank, want)
	}
}

func TestAdmitAllow(t *testing.T) {
	e := testEnvelope()
	at := e.Start.Add(15 * 24 * time.Hour)
	d := e.Admit(at, dollars(1_000), 0, dollars(500))

	if d.Outcome != OutcomeAllow {
		t.Errorf("Outcome = %q, want %q (%s)", d.Outcome, OutcomeAllow, d.Reason)
	}
	if d.Shortfall != 0 {
		t.Errorf("Shortfall = %s, want 0", d.Shortfall)
	}
	if d.Wait(at) != 0 {
		t.Errorf("Wait = %v, want 0", d.Wait(at))
	}
}

func TestAdmitExactlyAtAllowedBoundary(t *testing.T) {
	e := testEnvelope()
	at := e.Start.Add(15 * 24 * time.Hour) // Allowed = $2000.

	if d := e.Admit(at, dollars(1_500), 0, dollars(500)); d.Outcome != OutcomeAllow {
		t.Errorf("spending exactly to the allowance: %q (%s)", d.Outcome, d.Reason)
	}
	// One microdollar more must not be allowed now.
	if d := e.Admit(at, dollars(1_500), 0, dollars(500)+1); d.Outcome != OutcomeWait {
		t.Errorf("one microdollar over: %q, want wait (%s)", d.Outcome, d.Reason)
	}
}

func TestAdmitWaitBecomesAffordable(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	e := Envelope{ID: "x", Allocation: dollars(1_000), Start: start, End: start.Add(10 * 24 * time.Hour)}

	now := start.Add(2 * 24 * time.Hour) // $200 paced.
	d := e.Admit(now, dollars(200), 0, dollars(100))
	if d.Outcome != OutcomeWait {
		t.Fatalf("Outcome = %q, want wait (%s)", d.Outcome, d.Reason)
	}
	want := start.Add(3 * 24 * time.Hour) // $300 paced.
	if !d.RetryAt.Equal(want) {
		t.Errorf("RetryAt = %v, want %v", d.RetryAt, want)
	}
	if got, want := d.Wait(now), 24*time.Hour; got != want {
		t.Errorf("Wait = %v, want %v", got, want)
	}
	if want := dollars(100); d.Shortfall != want {
		t.Errorf("Shortfall = %s, want %s", d.Shortfall, want)
	}
}

// TestAdmitRetryAtIsActuallyAffordable is the important property: the promised
// retry time must really admit the request, with no off-by-one-microdollar
// rounding. It would be a bad experience to wait and be denied again.
func TestAdmitRetryAtIsActuallyAffordable(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, borrow := range []time.Duration{0, time.Hour, 72 * time.Hour} {
		for _, est := range []money.Money{1, 7_777_777, dollars(1), dollars(137)} {
			for _, spent := range []money.Money{0, dollars(200), dollars(413) + 999_999} {
				e := Envelope{
					ID: "x", Allocation: dollars(1_000),
					Start: start, End: start.Add(10 * 24 * time.Hour),
					Borrow: borrow,
				}
				now := start.Add(2 * 24 * time.Hour)
				d := e.Admit(now, spent, 0, est)
				if d.Outcome != OutcomeWait {
					continue
				}
				again := e.Admit(d.RetryAt, spent, 0, est)
				if again.Outcome != OutcomeAllow {
					t.Errorf("borrow=%v est=%s spent=%s: at RetryAt %v outcome was %q (%s), want allow",
						borrow, est, spent, d.RetryAt, again.Outcome, again.Reason)
				}
			}
		}
	}
}

func TestAdmitDenyExceedsEnvelope(t *testing.T) {
	e := testEnvelope()
	at := e.Start.Add(15 * 24 * time.Hour)
	d := e.Admit(at, dollars(3_900), 0, dollars(200))

	if d.Outcome != OutcomeDeny {
		t.Errorf("Outcome = %q, want deny", d.Outcome)
	}
	if d.Reason == "" {
		t.Error("a denial must carry a reason")
	}
	if !d.RetryAt.IsZero() {
		t.Errorf("RetryAt = %v, want zero on deny", d.RetryAt)
	}
}

func TestAdmitDenyAfterPeriodEnd(t *testing.T) {
	e := testEnvelope()
	d := e.Admit(e.End, dollars(4_000), 0, dollars(1))
	if d.Outcome != OutcomeDeny {
		t.Errorf("Outcome = %q, want deny", d.Outcome)
	}
}

// TestAdmitDenyWhenTooLateInPeriod covers a request that fits the total
// allocation but cannot be reached by the pacing curve before End.
func TestAdmitDenyWhenTooLateInPeriod(t *testing.T) {
	e := testEnvelope()
	// One minute left, nothing spent: the full $4000 fits the envelope total and
	// the curve reaches it exactly at End, so this is affordable at End.
	at := e.End.Add(-time.Minute)
	d := e.Admit(at, 0, 0, dollars(4_000))
	if d.Outcome != OutcomeWait {
		t.Fatalf("Outcome = %q, want wait (%s)", d.Outcome, d.Reason)
	}
	if !d.RetryAt.Equal(e.End) {
		t.Errorf("RetryAt = %v, want End %v", d.RetryAt, e.End)
	}
}

func TestAdmitZeroEstimateAlwaysAllowed(t *testing.T) {
	e := testEnvelope()
	// Even when fully committed, a zero-cost request has nothing to reserve.
	if d := e.Admit(e.Start, dollars(4_000), 0, 0); d.Outcome != OutcomeAllow {
		t.Errorf("Outcome = %q, want allow (%s)", d.Outcome, d.Reason)
	}
}

func TestAdmitRejectsNegativeEstimate(t *testing.T) {
	e := testEnvelope()
	if d := e.Admit(e.Start, 0, 0, -1); d.Outcome != OutcomeDeny {
		t.Errorf("Outcome = %q, want deny for a negative estimate", d.Outcome)
	}
}

func TestAdmitZeroAllocationDeniesRatherThanWaits(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	e := Envelope{ID: "x", Allocation: 0, Carry: dollars(10), Start: start, End: start.Add(time.Hour)}

	// Fits the carry, so it is allowed immediately.
	if d := e.Admit(start, 0, 0, dollars(10)); d.Outcome != OutcomeAllow {
		t.Errorf("Outcome = %q, want allow (%s)", d.Outcome, d.Reason)
	}
	// Beyond the carry, the curve never rises, so waiting cannot help.
	if d := e.Admit(start, 0, 0, dollars(11)); d.Outcome != OutcomeDeny {
		t.Errorf("Outcome = %q, want deny (%s)", d.Outcome, d.Reason)
	}
}

func TestAdmitDoesNotOverflowOnHugeEstimate(t *testing.T) {
	e := testEnvelope()
	d := e.Admit(e.Start, money.Max-10, 0, money.Max-10)
	if d.Outcome != OutcomeDeny {
		t.Errorf("Outcome = %q, want deny", d.Outcome)
	}
}

func TestValidate(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	base := Envelope{ID: "x", Allocation: dollars(100), Start: start, End: start.Add(time.Hour)}

	if err := base.Validate(); err != nil {
		t.Fatalf("valid envelope rejected: %v", err)
	}

	bad := map[string]func(*Envelope){
		"missing id":           func(e *Envelope) { e.ID = "" },
		"negative alloc":       func(e *Envelope) { e.Allocation = -1 },
		"end before start":     func(e *Envelope) { e.End = e.Start.Add(-time.Hour) },
		"end equals start":     func(e *Envelope) { e.End = e.Start },
		"zero start":           func(e *Envelope) { e.Start = time.Time{} },
		"negative borrow":      func(e *Envelope) { e.Borrow = -time.Hour },
		"negative cap":         func(e *Envelope) { e.Rollover.Cap = -1 },
		"unknown rollover":     func(e *Envelope) { e.Rollover.Mode = "sideways" },
		"carry+alloc overflow": func(e *Envelope) { e.Carry = money.Max; e.Allocation = money.Max },
	}
	for name, mutate := range bad {
		e := base
		mutate(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}
}

func TestZeroDurationEnvelopeDoesNotDivideByZero(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// Invalid per Validate, but the math must not panic if it is reached.
	e := Envelope{ID: "x", Allocation: dollars(100), Start: start, End: start}
	if e.Validate() == nil {
		t.Fatal("a zero-duration envelope should fail validation")
	}
	_ = e.Target(start)
	_ = e.Allowed(start)
	_ = e.Snapshot(start, 0, 0)
	if d := e.Admit(start, 0, 0, dollars(50)); d.Outcome == OutcomeWait {
		t.Error("a zero-duration envelope must never promise a retry time")
	}
}

// TestCalendarMonthLengths checks that pacing is correct across real month
// lengths and a leap day, since a month is just an envelope here.
func TestCalendarMonthLengths(t *testing.T) {
	cases := []struct {
		name  string
		start time.Time
		days  int
	}{
		{"january 31 days", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 31},
		{"february 28 days", time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), 28},
		{"february leap 29 days", time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), 29},
		{"april 30 days", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), 30},
		{"december 31 days", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), 31},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			end := tc.start.AddDate(0, 1, 0)
			e := Envelope{ID: "m", Allocation: dollars(3_100), Start: tc.start, End: end}

			if err := e.Validate(); err != nil {
				t.Fatal(err)
			}
			if got, want := e.Duration(), time.Duration(tc.days)*24*time.Hour; got != want {
				t.Fatalf("Duration = %v, want %v", got, want)
			}
			// Full allocation is paced exactly at the month end, no more, no less.
			if got := e.Target(end); got != e.Allocation {
				t.Errorf("Target at end = %s, want %s", got, e.Allocation)
			}
			// Midpoint of the month is half the allocation.
			mid := tc.start.Add(e.Duration() / 2)
			if got, want := e.Target(mid), e.Allocation/2; got != want {
				t.Errorf("Target at midpoint = %s, want %s", got, want)
			}
		})
	}
}

// TestDSTSpanningEnvelope checks a period across a US DST transition. Pacing is
// in absolute elapsed time, so a "day" during the transition is 23 or 25 hours
// and the envelope end is what anchors the curve.
func TestDSTSpanningEnvelope(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("timezone data unavailable: %v", err)
	}
	// 2026-03-08 is the US spring-forward date.
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, loc)
	end := time.Date(2026, 4, 1, 0, 0, 0, 0, loc)
	e := Envelope{ID: "dst", Allocation: dollars(3_100), Start: start, End: end}

	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	// March in New York is 31 calendar days but one hour short in real time.
	if got, want := e.Duration(), 31*24*time.Hour-time.Hour; got != want {
		t.Errorf("Duration = %v, want %v", got, want)
	}
	if got := e.Target(end); got != e.Allocation {
		t.Errorf("Target at end = %s, want %s", got, e.Allocation)
	}
	if got := e.Target(start); got != 0 {
		t.Errorf("Target at start = %s, want 0", got)
	}
}

func TestSubMicrodollarPacingDoesNotRoundUp(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// $1 over 30 days: most instants pace to a fraction of a microdollar.
	e := Envelope{ID: "tiny", Allocation: money.PerDollar, Start: start, End: start.Add(30 * 24 * time.Hour)}

	// One second in, the paced amount is well under a microdollar and truncates
	// to zero rather than granting money that has not been earned.
	if got := e.Target(start.Add(time.Second)); got != 0 {
		t.Errorf("Target after 1s = %d microdollars, want 0", int64(got))
	}
	if got := e.Target(e.End); got != money.PerDollar {
		t.Errorf("Target at end = %s, want $1", got)
	}
}
