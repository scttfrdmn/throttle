package report

import (
	"math/big"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/money"
)

// The gap between the actual line and the target line IS the pace balance. That is the
// whole point of the chart, so it is worth pinning arithmetically rather than trusting
// the two to be drawn from compatible baselines.
func TestTimelineGapEqualsThePaceBalance(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	// Half a month in, $300 spent against a $500 target: $200 banked.
	w.spend("s1", "research", dollars(300), base.Add(monthDuration/4))

	tl, err := w.rep.Timeline(w.ctx, "research", p.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	actual := tl.Actual[len(tl.Actual)-1].Amount
	target := interpolate(t, tl.Target, tl.Now)
	gap, ok := money.Sub(target, actual)
	if !ok {
		t.Fatal("subtracting the two lines overflowed")
	}
	if gap != sum.Position.PaceBalance {
		t.Errorf("the visible gap is %s but the reported pace balance is %s; a reader "+
			"would be shown one thing and told another", gap, sum.Position.PaceBalance)
	}
	if gap != dollars(200) {
		t.Errorf("gap = %s, want %s banked", gap, dollars(200))
	}
}

// interpolate reads a pacing curve at an instant, linearly between its samples.
//
// The product of a microdollar delta and a nanosecond offset does not fit in an int64,
// so it is computed in big.Int -- the same reason budget.prorate does.
func interpolate(t *testing.T, pts []Point, at time.Time) money.Money {
	t.Helper()
	if len(pts) == 0 {
		t.Fatal("empty curve")
	}
	if !at.After(pts[0].At) {
		return pts[0].Amount
	}
	for i := 1; i < len(pts); i++ {
		if at.After(pts[i].At) {
			continue
		}
		span := pts[i].At.Sub(pts[i-1].At)
		if span == 0 {
			return pts[i].Amount
		}
		delta := new(big.Int).SetInt64(int64(pts[i].Amount - pts[i-1].Amount))
		delta.Mul(delta, big.NewInt(int64(at.Sub(pts[i-1].At))))
		delta.Quo(delta, big.NewInt(int64(span)))
		return pts[i-1].Amount + money.Money(delta.Int64())
	}
	return pts[len(pts)-1].Amount
}

// The actual line steps at each charge's own persisted timestamp. Nothing is
// interpolated between charges and no request is synthesized.
func TestActualLineStepsAtPersistedChargeTimes(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	at1 := base.Add(2 * day)
	at2 := base.Add(9 * day)
	w.spend("s1", "research", dollars(30), at1)
	w.spend("s2", "research", dollars(70), at2)

	tl, err := w.rep.Timeline(w.ctx, "research", p.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.Charges != 2 {
		t.Fatalf("Charges = %d, want 2", tl.Charges)
	}
	// A baseline point at the period start, one per charge, and one at the now marker.
	if len(tl.Actual) != 4 {
		t.Fatalf("got %d points, want 4: %+v", len(tl.Actual), tl.Actual)
	}
	if !tl.Actual[0].At.Equal(p.Envelope.Start) || tl.Actual[0].Amount != 0 {
		t.Errorf("the line does not start at zero spend: %+v", tl.Actual[0])
	}
	if !tl.Actual[1].At.Equal(at1) || tl.Actual[1].Amount != dollars(30) {
		t.Errorf("first step = %+v, want %s at %s", tl.Actual[1], dollars(30), at1)
	}
	if !tl.Actual[2].At.Equal(at2) || tl.Actual[2].Amount != dollars(100) {
		t.Errorf("second step = %+v, want a cumulative %s at %s", tl.Actual[2], dollars(100), at2)
	}
	// The line reaches now rather than stopping at the last charge, which would read as
	// though spend had gone flat for a reason.
	if !tl.Actual[3].At.Equal(w.now) || tl.Actual[3].Amount != dollars(100) {
		t.Errorf("last point = %+v, want the running total carried to %s", tl.Actual[3], w.now)
	}
	for i := 1; i < len(tl.Actual); i++ {
		if tl.Actual[i].At.Before(tl.Actual[i-1].At) {
			t.Errorf("points are not in time order at %d", i)
		}
		if tl.Actual[i].Amount < tl.Actual[i-1].Amount {
			t.Errorf("cumulative spend decreased at %d", i)
		}
	}
}

// Reservations are a band from now rightward, not a stacked area over history. Drawing
// a hold as a past charge would imply money left the account when it did not.
func TestReservationsAreNotStackedOverHistory(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(100), base.Add(2*day))
	w.hold("h1", "research", dollars(40), w.now)

	tl, err := w.rep.Timeline(w.ctx, "research", p.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.Reserved != dollars(40) {
		t.Errorf("Reserved = %s, want %s", tl.Reserved, dollars(40))
	}
	if tl.Committed != dollars(140) {
		t.Errorf("Committed = %s, want spent plus reserved", tl.Committed)
	}
	// Every point on the actual line is settled spend only.
	for _, pt := range tl.Actual {
		if pt.Amount > dollars(100) {
			t.Errorf("the actual line reaches %s at %s; a reservation was drawn into history",
				pt.Amount, pt.At)
		}
	}
}

// The allowed envelope is only a separate line when borrowing is configured, so a
// chart does not draw two identical curves and imply a distinction that is not there.
func TestAllowedEnvelopeAppearsOnlyWithBorrowing(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("plain", "", dollars(1000)))

	tl, err := w.rep.Timeline(w.ctx, "plain", p.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.HasBorrow {
		t.Error("HasBorrow = true with no borrow window configured")
	}
	if len(tl.Allowed) != 0 {
		t.Errorf("Allowed has %d points with no borrowing configured", len(tl.Allowed))
	}

	borrowing := monthly("borrowing", "", dollars(1000))
	borrowing.Borrow = 48 * time.Hour
	bp := w.define(borrowing)

	tl2, err := w.rep.Timeline(w.ctx, "borrowing", bp.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if !tl2.HasBorrow {
		t.Fatal("HasBorrow = false with a 48h borrow window")
	}
	if len(tl2.Allowed) != len(tl2.Target) {
		t.Fatalf("Allowed has %d points and Target %d; they must be sampled together",
			len(tl2.Allowed), len(tl2.Target))
	}
	ahead := 0
	for i := range tl2.Allowed {
		if tl2.Allowed[i].Amount < tl2.Target[i].Amount {
			t.Errorf("the allowed envelope is below the target at %s", tl2.Allowed[i].At)
		}
		if tl2.Allowed[i].Amount > tl2.Target[i].Amount {
			ahead++
		}
	}
	if ahead == 0 {
		t.Error("the allowed envelope never exceeds the target; borrowing pulls spend forward")
	}
}

// The now marker is clamped into the period, so a closed period's marker sits at its
// end rather than off the right edge of the chart.
func TestNowMarkerIsClampedIntoThePeriod(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	w.now = base.Add(monthDuration + 10*day)
	late, err := w.rep.Timeline(w.ctx, "research", p.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if !late.Now.Equal(p.Envelope.End) {
		t.Errorf("Now = %s, want it clamped to the period end %s", late.Now, p.Envelope.End)
	}

	w.now = base.Add(-10 * day)
	early, err := w.rep.Timeline(w.ctx, "research", p.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if !early.Now.Equal(p.Envelope.Start) {
		t.Errorf("Now = %s, want it clamped to the period start %s", early.Now, p.Envelope.Start)
	}
	// And the chart still has axes: an unstarted period is drawable.
	if early.Total != dollars(1000) || len(early.Target) == 0 {
		t.Errorf("an unstarted period has no drawable envelope: %+v", early)
	}
}

// A timeline with no charges at all is a flat line at the baseline, not an error.
func TestTimelineWithNoChargesIsFlat(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	tl, err := w.rep.Timeline(w.ctx, "research", p.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.Charges != 0 {
		t.Errorf("Charges = %d, want 0", tl.Charges)
	}
	if len(tl.Actual) != 2 {
		t.Fatalf("got %d points, want a baseline and a now marker: %+v", len(tl.Actual), tl.Actual)
	}
	for _, pt := range tl.Actual {
		if pt.Amount != 0 {
			t.Errorf("point %+v is non-zero on a budget that spent nothing", pt)
		}
	}
	if tl.Truncated {
		t.Error("Truncated = true with no charges")
	}
	if len(tl.Target) != pacingSamples+1 {
		t.Errorf("Target has %d points, want %d samples", len(tl.Target), pacingSamples+1)
	}
	if tl.Target[len(tl.Target)-1].Amount != dollars(1000) {
		t.Errorf("the pacing curve ends at %s, want the whole allocation",
			tl.Target[len(tl.Target)-1].Amount)
	}
}

// Called with no period, the timeline uses the one containing the clock.
func TestTimelineWithoutAPeriodUsesTheCurrentOne(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	tl, err := w.rep.Timeline(w.ctx, "research", "")
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.PeriodID != p.ID {
		t.Errorf("PeriodID = %q, want the current period %q", tl.PeriodID, p.ID)
	}
	if !tl.Start.Equal(p.Envelope.Start) || !tl.End.Equal(p.Envelope.End) {
		t.Errorf("bounds = %s..%s, want the envelope's own %s..%s",
			tl.Start, tl.End, p.Envelope.Start, p.Envelope.End)
	}
}

// The line for a child budget is the child's own share of each charge, not the parent's
// figure repeated.
func TestTimelineUsesThisScopesLegAmount(t *testing.T) {
	w, periods := scopedWorld(t)
	w.spend("s1", "nlp", dollars(120), base.Add(2*day), "nlp", "research")

	child, err := w.rep.Timeline(w.ctx, "nlp", periods["nlp"].ID)
	if err != nil {
		t.Fatalf("Timeline(nlp): %v", err)
	}
	if got := child.Actual[len(child.Actual)-1].Amount; got != dollars(120) {
		t.Errorf("child line reaches %s, want %s", got, dollars(120))
	}
	if child.Total != dollars(400) {
		t.Errorf("child Total = %s, want its own allocation", child.Total)
	}

	parent, err := w.rep.Timeline(w.ctx, "research", periods["research"].ID)
	if err != nil {
		t.Fatalf("Timeline(research): %v", err)
	}
	if got := parent.Actual[len(parent.Actual)-1].Amount; got != dollars(120) {
		t.Errorf("parent line reaches %s, want the rolled-up %s", got, dollars(120))
	}
	if parent.Total != dollars(1000) {
		t.Errorf("parent Total = %s, want %s", parent.Total, dollars(1000))
	}
}

// The projection is the straight-line one the read model already computes, carried onto
// the chart rather than recomputed differently for it.
func TestTimelineProjectionMatchesThePosition(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(300), base.Add(2*day))

	tl, err := w.rep.Timeline(w.ctx, "research", p.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if tl.Projection != sum.Position.Projection {
		t.Errorf("the chart projects %+v and the panel %+v; one number, two claims",
			tl.Projection, sum.Position.Projection)
	}
}

// A rollover credit lifts the target curve, not the actual line, so the visible gap
// remains the pace balance and an inherited credit reads as banked from the outset.
func TestCarryLiftsTheTargetCurveNotTheActualLine(t *testing.T) {
	w := newWorld(t)

	def := monthly("research", "", dollars(1000))
	def.Rollover = budget.RolloverPolicy{Mode: budget.RolloverCredit, Cap: dollars(200)}
	if err := w.led.PutDefinition(w.ctx, def); err != nil {
		t.Fatalf("PutDefinition: %v", err)
	}
	// A period materialized with an inherited carry, which is what a rollover produces.
	p, err := w.led.EnsurePeriod(w.ctx, "research", w.now)
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	if _, err := w.led.DB().ExecContext(w.ctx,
		`UPDATE periods SET carry = ? WHERE id = ?`, int64(dollars(150)), p.ID); err != nil {
		t.Fatalf("setting the carry: %v", err)
	}

	tl, err := w.rep.Timeline(w.ctx, "research", p.ID)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if tl.Actual[0].Amount != 0 {
		t.Errorf("the actual line starts at %s, want zero spend", tl.Actual[0].Amount)
	}
	if tl.Target[0].Amount != dollars(150) {
		t.Errorf("the target curve starts at %s, want the inherited credit %s",
			tl.Target[0].Amount, dollars(150))
	}
	if tl.Total != dollars(1150) {
		t.Errorf("Total = %s, want carry plus allocation %s", tl.Total, dollars(1150))
	}

	// The gap between the curves is still the pace balance, with the credit counted
	// once -- on the target side, where it belongs.
	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	target := interpolate(t, tl.Target, tl.Now)
	gap, ok := money.Sub(target, tl.Actual[len(tl.Actual)-1].Amount)
	if !ok {
		t.Fatal("subtracting the two lines overflowed")
	}
	if gap != sum.Position.PaceBalance {
		t.Errorf("gap = %s but PaceBalance = %s with a carry in play", gap, sum.Position.PaceBalance)
	}
	if sum.Position.CarryIn != dollars(150) {
		t.Errorf("CarryIn = %s, want %s displayed to the reader", sum.Position.CarryIn, dollars(150))
	}
	if sum.Position.Rollover.Mode != budget.RolloverCredit {
		t.Errorf("Rollover.Mode = %q, want the configured policy visible", sum.Position.Rollover.Mode)
	}
	if sum.Position.Rollover.Cap != dollars(200) {
		t.Errorf("Rollover.Cap = %s, want %s", sum.Position.Rollover.Cap, dollars(200))
	}
}

// Rollover is not shown as a live figure when it is disabled, so the primary display
// stays uncluttered on the ordinary case.
func TestRolloverIsAbsentWhenDisabled(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Position.CarryIn != 0 {
		t.Errorf("CarryIn = %s with no rollover configured", sum.Position.CarryIn)
	}
	if sum.Position.Rollover.Mode != "" && sum.Position.Rollover.Mode != budget.RolloverNone {
		t.Errorf("Rollover.Mode = %q, want none", sum.Position.Rollover.Mode)
	}
}

// A reservation belonging to another budget is not listed under this one.
func TestReservationsFilterByBudget(t *testing.T) {
	w, _ := scopedWorld(t)
	w.hold("h1", "nlp", dollars(40), w.now, "nlp", "research")
	w.hold("h2", "vision", dollars(25), w.now, "vision", "research")

	nlp, err := w.rep.Reservations(w.ctx, "nlp", 0)
	if err != nil {
		t.Fatalf("Reservations(nlp): %v", err)
	}
	if len(nlp) != 1 || nlp[0].ReservationID != "h1" {
		t.Errorf("nlp holds = %+v, want only h1", nlp)
	}

	// The parent sees both, because both holds encumber it through their legs.
	parent, err := w.rep.Reservations(w.ctx, "research", 0)
	if err != nil {
		t.Fatalf("Reservations(research): %v", err)
	}
	if len(parent) != 2 {
		t.Errorf("parent holds = %d, want both children's", len(parent))
	}

	all, err := w.rep.Reservations(w.ctx, "", 0)
	if err != nil {
		t.Fatalf("Reservations(all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("unfiltered holds = %d, want 2", len(all))
	}
}

// A timeline for a period that does not exist is a not-found, not a blank chart.
func TestTimelineForAMissingPeriodIsNotFound(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	_, err := w.rep.Timeline(w.ctx, "research", "research@99")
	if err == nil {
		t.Fatal("Timeline of an unknown period returned no error")
	}
	if !NotFound(err) {
		t.Errorf("err = %v, want a not-found the handler can turn into a 404", err)
	}
}

// A hold's own amount and the adapter's estimate are separate figures, because they
// need not agree.
func TestHoldKeepsAmountAndEstimateSeparate(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	if _, err := w.led.Reserve(w.ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID: "h1", BudgetID: "research", RequestID: "req-h1",
			Amount: dollars(50), EstimatedCost: dollars(35),
			CreatedAt: w.now, ExpiresAt: w.now.Add(time.Hour), Lease: time.Hour,
			Identity: bedrockIdentity(),
		},
		Ceilings: unlimited("research"),
		Now:      w.now,
	}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	holds, err := w.rep.Reservations(w.ctx, "research", 0)
	if err != nil {
		t.Fatalf("Reservations: %v", err)
	}
	if len(holds) != 1 {
		t.Fatalf("got %d holds, want 1", len(holds))
	}
	if holds[0].Amount != dollars(50) {
		t.Errorf("Amount = %s, want the headroom held %s", holds[0].Amount, dollars(50))
	}
	if holds[0].EstimatedCost != dollars(35) {
		t.Errorf("EstimatedCost = %s, want the prediction %s", holds[0].EstimatedCost, dollars(35))
	}
}
