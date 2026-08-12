package report

import (
	"context"
	"sort"
	"time"

	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/money"
)

// Timeline is budget-over-time: what pacing said should have been spent, what was
// actually spent, and where the clock is now.
//
// The vertical distance between the actual line and the target line IS the pace
// balance. That is the point of the chart: a reader should be able to see banked or
// borrowed without reading a number.
type Timeline struct {
	BudgetID string
	PeriodID string

	Start time.Time
	End   time.Time

	// Now is the current-time marker. It is clamped into the period, so a closed
	// period's marker sits at its end rather than off the right edge.
	Now time.Time

	// Total is the envelope ceiling: carry plus allocation. It is the natural top of
	// the vertical axis.
	Total money.Money

	// Target is the pacing curve, and Allowed the same curve pulled forward by the
	// borrow window. Allowed equals Target when no borrowing is configured, and
	// HasBorrow says which, so a chart does not draw two identical lines.
	Target    []Point
	Allowed   []Point
	HasBorrow bool

	// Actual is cumulative settled spend from zero, stepping at each charge's own
	// timestamp.
	//
	// It is built from persisted charge times and amounts. Nothing is interpolated
	// between charges and no request is synthesized: a step function of what really
	// happened is more honest than a smooth line through it.
	//
	// It starts at zero while Target starts at Carry, which is exactly what makes the
	// vertical distance between the two lines the pace balance.
	Actual []Point

	// Committed is cumulative settled spend plus currently encumbered reservations,
	// drawn only from Now rightward as a single step.
	//
	// It is deliberately not a stacked area over the actual line across history: a
	// reservation is not a charge that occurred at a past instant, and drawing it as
	// one would imply money left the account when it did not.
	Committed money.Money

	// Reserved is the encumbered amount behind Committed, separately so a chart can
	// label the band for what it is.
	Reserved money.Money

	// Charges is the number of settled charges the actual line is built from, and
	// Truncated reports that older charges exist beyond the query limit -- in which
	// case the line starts partway up and says so rather than pretending the period
	// began at zero spend.
	Charges   int
	Truncated bool

	// Projection is the straight-line extrapolation to the period end, for an
	// optional dashed continuation of the actual line.
	Projection Projection
}

// Point is one (time, cumulative money) sample.
type Point struct {
	At     time.Time
	Amount money.Money
}

// pacingSamples is how many points the target and allowed curves are drawn with.
// Linear pacing needs two, but sampling more keeps the curve honest if pacing ever
// stops being linear, and it costs nothing.
const pacingSamples = 24

// DefaultTimelineCharges bounds how many charges a timeline reads.
const DefaultTimelineCharges = 2000

// Timeline builds the budget-over-time view for a budget's period.
//
// The pacing curves come from the envelope, which is the same code enforcement uses.
// The actual line comes from ledger charges, because the ledger is monetary truth --
// building it from activity records would be recomputing money from telemetry for the
// convenience of a chart, which is precisely the thing this package does not do.
func (r *Reporter) Timeline(ctx context.Context, budgetID, periodID string) (Timeline, error) {
	var (
		p   ledger.Period
		err error
	)
	if periodID != "" {
		if p, err = r.led.Period(ctx, periodID); err != nil {
			return Timeline{}, err
		}
	} else {
		def, _, derr := r.led.Definition(ctx, budgetID)
		if derr != nil {
			return Timeline{}, derr
		}
		if p, err = r.periodContaining(ctx, def, r.clock()); err != nil {
			return Timeline{}, err
		}
	}

	env := p.Envelope
	at := r.clock()
	marker := at
	if marker.Before(env.Start) {
		marker = env.Start
	} else if marker.After(env.End) {
		marker = env.End
	}

	scope := ledger.Scope{BudgetID: budgetID, PeriodID: p.ID}
	tot, err := r.led.Totals(ctx, scope, at)
	if err != nil {
		return Timeline{}, err
	}

	t := Timeline{
		BudgetID:  budgetID,
		PeriodID:  p.ID,
		Start:     env.Start,
		End:       env.End,
		Now:       marker,
		Total:     env.Total(),
		HasBorrow: env.Borrow > 0,
		Reserved:  tot.Reserved,
	}
	if v, ok := money.Add(tot.Spent, tot.Reserved); ok {
		t.Committed = v
	}

	// The pacing curves, from the envelope's own math.
	for i := 0; i <= pacingSamples; i++ {
		at := env.Start.Add(time.Duration(int64(env.Duration()) * int64(i) / pacingSamples))
		t.Target = append(t.Target, Point{At: at, Amount: env.Target(at)})
		if t.HasBorrow {
			t.Allowed = append(t.Allowed, Point{At: at, Amount: env.Allowed(at)})
		}
	}

	// The actual line, from persisted charges.
	charges, err := r.led.Charges(ctx, scope, env.Start, env.End, DefaultTimelineCharges+1)
	if err != nil {
		return Timeline{}, err
	}
	if len(charges) > DefaultTimelineCharges {
		t.Truncated = true
		charges = charges[:DefaultTimelineCharges]
	}
	t.Charges = len(charges)

	// Charges arrive most recent first; the cumulative line needs them oldest first.
	sort.Slice(charges, func(i, j int) bool {
		if !charges[i].OccurredAt.Equal(charges[j].OccurredAt) {
			return charges[i].OccurredAt.Before(charges[j].OccurredAt)
		}
		return charges[i].ID < charges[j].ID
	})

	// The line starts at zero, because it is this period's settled spend and nothing
	// was spent before the period began.
	//
	// The target curve, by contrast, starts at the carry: an inherited credit is spend
	// the budget was already permitted. That is what makes the vertical distance
	// between the two lines equal Target-Spent -- the pace balance -- at every instant,
	// including at the period start, where a $150 credit reads as $150 banked. Starting
	// the actual line at the carry instead would cancel it out of the gap and the chart
	// would disagree with the figure printed beside it.
	var running money.Money
	t.Actual = append(t.Actual, Point{At: env.Start, Amount: running})
	for _, c := range charges {
		amount := c.ActualCost
		// Charges are read per leg, so the leg amount is this scope's share.
		for _, leg := range c.Legs {
			if leg.Scope == scope {
				amount = leg.Amount
				break
			}
		}
		if v, ok := money.Add(running, amount); ok {
			running = v
		}
		t.Actual = append(t.Actual, Point{At: c.OccurredAt, Amount: running})
	}
	// Extend to the marker so the line reaches "now" rather than stopping at the last
	// charge, which would read as though spend had gone flat for a reason.
	t.Actual = append(t.Actual, Point{At: marker, Amount: running})

	snap := env.Snapshot(at, tot.Spent, tot.Reserved)
	t.Projection = projection(env, snap, env.Elapsed(at), confidence(env.Elapsed(at), env.Duration()))
	return t, nil
}

// Reservations lists the live holds behind a Reserved figure.
//
// The point of showing these is that Reserved is not spend: a reader who sees $12
// encumbered should be able to find out that it is three in-flight agent turns rather
// than an accounting mystery.
func (r *Reporter) Reservations(ctx context.Context, budgetID string, limit int) ([]Hold, error) {
	if limit <= 0 {
		limit = DefaultActivityLimit
	}
	res, err := r.led.Reservations(ctx,
		[]ledger.ReservationState{ledger.StatePending, ledger.StateExpired}, limit)
	if err != nil {
		return nil, err
	}
	at := r.clock()

	var out []Hold
	for _, rv := range res {
		if budgetID != "" && !holdTouches(rv, budgetID) {
			continue
		}
		h := Hold{
			ReservationID: rv.ID,
			RequestID:     rv.RequestID,
			BudgetID:      rv.BudgetID,
			Amount:        rv.Amount,
			EstimatedCost: rv.EstimatedCost,
			CreatedAt:     rv.CreatedAt,
			ExpiresAt:     rv.ExpiresAt,
			State:         rv.State,
			Expired:       rv.State == ledger.StateExpired || rv.Expired(at),
			Age:           at.Sub(rv.CreatedAt),
			RenewCount:    rv.RenewCount,
		}
		h.Model, h.ModelKnown = displayModel(rv.Identity)
		h.Operation = rv.Identity.Operation
		h.AccessProvider = rv.Identity.AccessProvider
		out = append(out, h)
	}
	return out, nil
}

// Hold is one live or expired reservation.
type Hold struct {
	ReservationID string
	RequestID     string
	BudgetID      string

	// Amount is the headroom held. EstimatedCost is what the adapter predicted, which
	// is usually the same and is retained separately because it need not be.
	Amount        money.Money
	EstimatedCost money.Money

	CreatedAt time.Time
	ExpiresAt time.Time

	State ledger.ReservationState

	// Expired reports a lapsed lease. An expired hold is not evidence of zero spend:
	// the request may have been served and billed, and it remains settleable.
	Expired bool

	// Age is how long the hold has been standing, which is what makes a stranded hold
	// obvious.
	Age        time.Duration
	RenewCount int

	Operation      string
	AccessProvider string
	Model          string
	ModelKnown     bool
}

func holdTouches(rv ledger.Reservation, budgetID string) bool {
	if rv.BudgetID == budgetID {
		return true
	}
	for _, leg := range rv.Legs {
		if leg.Scope.BudgetID == budgetID {
			return true
		}
	}
	return false
}
