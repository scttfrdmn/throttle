// Package report is the read model behind throttle's dashboards and reports.
//
// It is a query layer, not a second accounting system. Every monetary figure it
// returns is either read directly from the ledger or computed by the budget
// package's own pacing math; nothing here re-derives money from telemetry because
// that would be convenient for a chart. The division of labour is fixed:
//
//	ledger   -> authoritative money (spent, reserved, charges, period envelopes)
//	activity -> attribution and history (who called what, with which usage, how complete)
//
// Where the two disagree, this package reports the disagreement rather than
// choosing a winner. A dashboard that silently resolved it would be presenting an
// opinion as a measurement.
//
// # No presentation
//
// Nothing here imports net/http or html/template, and no field holds markup, a
// colour, or a CSS class. The vocabulary is money, time, counts, and identity, so
// the same queries serve a web page, a CLI report, and a JSON endpoint without any
// of them teaching this package about the others.
//
// # No writes
//
// There is no method here that mutates anything. A reader cannot accidentally
// reconcile, settle, release, or advance a period by looking at a budget, because
// the consumer-defined store interfaces below do not include the verbs that would
// let it.
package report

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/money"
)

// Ledger is the read subset of the durable accounting store.
//
// A consumer-defined interface listing exactly the reads a report needs. The
// omissions are the point: Reserve, Settle, Release, and Advance are absent, so no
// amount of refactoring inside this package can turn a page view into a write.
type Ledger interface {
	Definitions(ctx context.Context) ([]budget.Definition, error)
	Definition(ctx context.Context, budgetID string) (budget.Definition, int, error)
	Chain(ctx context.Context, budgetID string) ([]budget.Definition, error)
	Period(ctx context.Context, periodID string) (ledger.Period, error)
	Periods(ctx context.Context, budgetID string) ([]ledger.Period, error)
	Totals(ctx context.Context, scope ledger.Scope, now time.Time) (ledger.Totals, error)
	Charges(ctx context.Context, scope ledger.Scope, from, to time.Time, limit int) ([]ledger.Charge, error)
	Reservations(ctx context.Context, states []ledger.ReservationState, limit int) ([]ledger.Reservation, error)
}

// Activity is the read subset of the telemetry store.
//
// It is optional. A reporter built without one answers every budget-position
// question and no request-history question, which is a materially less useful
// dashboard but not a broken one -- the same trade the provider adapters make when
// they treat a nil store as "do not record".
type Activity interface {
	List(ctx context.Context, f activity.Filter) ([]activity.Record, error)
	Get(ctx context.Context, requestID string) (activity.Record, error)
}

// Config builds a Reporter.
type Config struct {
	// Ledger is required: it is where money lives.
	Ledger Ledger

	// Activity is optional. Nil means request history is unavailable, which
	// HasActivity reports so a caller can say so rather than showing an empty table
	// that looks like "no requests".
	Activity Activity

	// Clock defaults to time.Now. Every figure this package produces is a function
	// of a single instant, so an injected clock makes pacing math testable at exact
	// boundaries.
	Clock func() time.Time
}

// Reporter answers read-model queries.
type Reporter struct {
	led   Ledger
	acts  Activity
	clock func() time.Time
}

// ErrNoLedger reports a Reporter built without a ledger.
var ErrNoLedger = errors.New("report: a ledger is required")

// ErrNoActivity reports a query that needs the activity store on a Reporter built
// without one. It is distinct from an empty result: "throttle is not recording
// request telemetry" and "throttle recorded no requests" are different facts, and a
// dashboard that conflated them would show an empty table as though it were news.
var ErrNoActivity = errors.New("report: no activity store is configured")

// New builds a Reporter.
func New(cfg Config) (*Reporter, error) {
	if cfg.Ledger == nil {
		return nil, ErrNoLedger
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Reporter{led: cfg.Ledger, acts: cfg.Activity, clock: clock}, nil
}

// HasActivity reports whether request history is available at all.
func (r *Reporter) HasActivity() bool { return r.acts != nil }

// Now is the reporter's clock, so a caller renders the same instant the figures
// were computed at rather than a slightly later one.
func (r *Reporter) Now() time.Time { return r.clock() }

// Confidence qualifies a figure derived from very little elapsed time.
//
// A rate measured over the first thirty seconds of a month is arithmetically
// correct and practically meaningless. Suppressing it would hide a real
// measurement; presenting it unqualified would invite a decision it cannot
// support. So it is shown, and marked.
type Confidence string

const (
	// ConfidenceNone means the figure could not be computed at all.
	ConfidenceNone Confidence = "none"

	// ConfidenceLow means too little of the period has elapsed for a rate or a
	// projection to mean much.
	ConfidenceLow Confidence = "low"

	// ConfidenceOK means enough of the period has elapsed for the figure to be
	// worth acting on.
	ConfidenceOK Confidence = "ok"
)

// lowConfidenceFraction is the share of a period below which rates and
// projections are marked low-confidence, in basis points: 5%.
//
// The threshold is a judgement, not a statistic, and it is deliberately crude. A
// cleverer rule would imply a statistical claim this package does not make.
const lowConfidenceFraction = 500

// Position is a budget's monetary and pacing position at one instant.
//
// The field vocabulary is fixed and is not interchangeable:
//
//   - Spent is settled actual spend. Money that is gone.
//   - Reserved is currently encumbered headroom: live holds and unresolved
//     liabilities. Money that is promised. It is NEVER folded into Spent, and it is
//     never called spent.
//   - PaceBalance is TargetByNow-Spent. Positive is banked, negative is borrowed.
//   - SpendableNow is AllowedByNow-Spent-Reserved.
//
// Several of these are legitimately negative, and they are reported signed. An
// absolute value would turn "borrowed $40" into "banked $40", which is the opposite
// of the truth.
type Position struct {
	BudgetID string
	Name     string
	ParentID string

	// At is the instant every figure below was computed for.
	At time.Time

	// Period is the materialized period the position is measured in, including its
	// lifecycle state and whether its carry is still provisional.
	Period ledger.Period

	Allocation money.Money

	// CarryIn is the signed balance inherited from the previous period. Negative
	// carry is inherited debt.
	CarryIn money.Money

	// Total is CarryIn+Allocation: the most this period can ever authorize.
	Total money.Money

	Spent    money.Money
	Reserved money.Money

	// Committed is Spent+Reserved. Reported as its own field so that no caller has
	// to add the two and risk labelling the sum "spent".
	Committed money.Money

	// TargetByNow is the linear pacing target at At, including carry.
	TargetByNow money.Money

	// AllowedByNow is TargetByNow plus whatever the borrow window pulls forward.
	AllowedByNow money.Money

	// PaceBalance is TargetByNow-Spent: positive banked, negative borrowed.
	//
	// Note that this is against settled spend, not against committed. It answers
	// "has money actually left faster than the plan?", which is the question the
	// bank/borrow display asks. Reserved appears in SpendableNow instead, where an
	// encumbrance genuinely reduces what may be committed next.
	PaceBalance money.Money

	// SpendableNow is AllowedByNow-Spent-Reserved: what may be committed at At. It
	// is clamped at zero, because a negative amount is not a spendable quantity.
	SpendableNow money.Money

	// RemainingAllocation is Total-Spent-Reserved, signed. Negative means the
	// envelope is overcommitted, which is reported rather than hidden.
	RemainingAllocation money.Money

	PeriodStart time.Time
	PeriodEnd   time.Time

	// Elapsed and TimeRemaining are clamped to the period, so neither goes negative
	// outside it.
	Elapsed        time.Duration
	TimeRemaining  time.Duration
	PeriodDuration time.Duration

	// AverageBurn is Spent/Elapsed as microdollars per hour: an average over the
	// whole elapsed period, not a current or instantaneous rate.
	AverageBurn Rate

	// SustainableBurn is RemainingAllocation/TimeRemaining as microdollars per
	// hour: the rate that exactly consumes what is left over the time left.
	SustainableBurn Rate

	// Pressure is AverageBurn/SustainableBurn, the throttle gauge.
	Pressure Pressure

	// Projection is straight-line end-of-period spend. It is an extrapolation of
	// the average rate to date and nothing more.
	Projection Projection

	// Rollover is the period's carry configuration, so the display can explain a
	// carry figure or stay quiet when rollover is off.
	Rollover budget.RolloverPolicy

	// Mode is absent deliberately. Enforcement posture is a property of the process
	// doing the spending, not of the budget, so a dashboard reading a shared ledger
	// cannot know it. The per-request posture that actually governed each call is on
	// the activity record instead.

	// ExpiredHolds and ExpiredAmount are holds whose leases lapsed and which have
	// not been recovered. Their headroom is already excluded from Reserved, so
	// showing them is how an operator learns recovery has work to do.
	ExpiredHolds  int
	ExpiredAmount money.Money

	// LiveHolds is the number of unexpired reservations behind Reserved.
	LiveHolds int
}

// Overspent reports whether commitments have exceeded the envelope total.
func (p Position) Overspent() bool { return p.RemainingAllocation < 0 }

// Banked reports whether spend is behind the pacing target.
func (p Position) Banked() bool { return p.PaceBalance > 0 }

// Borrowed reports whether spend has run ahead of the pacing target.
func (p Position) Borrowed() bool { return p.PaceBalance < 0 }

// Rate is an amount of money per unit time, held as microdollars per hour.
//
// The stored value is duration-normalized and does not change with the unit a
// display chooses. Per is the unit suggested for this period's length, so a
// six-hour experiment reports $/hour and a monthly budget reports $/day, without
// the underlying figure being rescaled into something a later comparison would get
// wrong.
type Rate struct {
	// PerHour is the rate in microdollars per hour. This is the value; everything
	// else about a Rate is presentation.
	PerHour money.Money

	// Per is the unit a display should prefer for this period.
	Per time.Duration

	// Known reports whether a rate could be computed at all. False means there was
	// no elapsed time to measure over, or no remaining time to spread across --
	// which is not a rate of zero.
	Known bool

	// Confidence qualifies a rate measured over very little elapsed time.
	Confidence Confidence
}

// In converts the rate to an amount per d, exactly, rounding half away from zero.
//
// Rounding once at the point of display is the same rule pricing follows: a caller
// that wants dollars per day should ask for them rather than multiply a rounded
// hourly figure by 24.
func (r Rate) In(d time.Duration) money.Money {
	if !r.Known || d <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(int64(r.PerHour)), big.NewInt(int64(d)))
	return money.Money(divRound(n, big.NewInt(int64(time.Hour))))
}

// Suggested is the rate in the unit Per, which is the figure a display shows.
func (r Rate) Suggested() money.Money { return r.In(r.Per) }

// Pressure is the throttle gauge: average burn to date divided by the burn rate
// that would exactly consume the remaining allocation over the remaining time.
//
// # What 100% means
//
// Exactly this: the average rate to date is precisely the rate that consumes
// RemainingAllocation over TimeRemaining. Continue at it and the period ends at the
// envelope total, to the microdollar. Above 100% the current rate overruns before
// the period ends; below it, the period finishes under.
//
// # What it does not mean
//
// It is not percent of budget spent. Those two numbers move differently and answer
// different questions: a budget can be 90% spent and under pressure 50% (nearly
// done, and slowing down is not required), or 10% spent and over 300% (barely
// started, and burning far too fast for the remaining month). Every display of this
// value must carry that distinction, which is why the type has no method that
// returns a bare percentage without a state alongside it.
//
// # Why it is not the quotient of the two displayed rates
//
// The ratio is computed in one exact step from the underlying integers, so it does
// not accumulate the rounding of the two rates a reader sees. A reader dividing the
// two displayed dollar figures by hand may land a hair off; the gauge is right.
type Pressure struct {
	// BasisPoints is the ratio in basis points: 10000 is exactly 100%. Valid only
	// when State is PressureMeasured.
	BasisPoints int64

	// State says whether there is a reading, and if not, why not.
	State PressureState

	// Confidence qualifies a reading taken over very little elapsed time.
	Confidence Confidence
}

// PressureState is why the gauge reads what it reads.
//
// Four of the five values are explicitly not a number. A gauge that rendered every
// unmeasurable condition as 0% would report an idle workload, an over-budget one,
// and a period that has not started as though they were the same healthy state.
type PressureState string

const (
	// PressureMeasured means BasisPoints is a real ratio.
	PressureMeasured PressureState = "measured"

	// PressureNotStarted means no time has elapsed, so there is no rate to measure.
	// This is distinct from a rate of zero: the workload has not had the chance to
	// spend anything.
	PressureNotStarted PressureState = "not-started"

	// PressureEnded means the period is over. There is no remaining time to sustain
	// a rate across, so the question the gauge asks has stopped being a question.
	PressureEnded PressureState = "ended"

	// PressureNoHeadroom means the remaining allocation is zero or negative, so no
	// sustainable rate exists to compare against. The gauge is pegged and says so
	// rather than printing an enormous ratio that would look measured.
	PressureNoHeadroom PressureState = "no-headroom"
)

// Measured reports whether there is a numeric reading.
func (p Pressure) Measured() bool { return p.State == PressureMeasured }

// Percent is the reading as a percentage with two decimal places of basis-point
// resolution, for display. It is meaningless unless Measured.
func (p Pressure) Percent() float64 { return float64(p.BasisPoints) / 100 }

// OverRedline reports whether the average rate to date exceeds the sustainable
// rate. It is false whenever there is no reading, including when the budget is
// exhausted -- callers must check State rather than treat false as "healthy".
func (p Pressure) OverRedline() bool { return p.Measured() && p.BasisPoints > 10_000 }

// Projection is straight-line end-of-period spend.
//
// It extrapolates the average rate to date across the whole period and is nothing
// more sophisticated than that. It is labelled as straight-line wherever it appears
// because a projection presented without its method invites the reader to assume a
// forecast that does not exist.
type Projection struct {
	// Amount is projected total settled spend at the end of the period.
	Amount money.Money

	// Known reports whether a projection could be made. False before any time has
	// elapsed, when there is no rate to extrapolate.
	Known bool

	// Confidence is low when very little of the period has elapsed.
	Confidence Confidence

	// OverBy is how much the projection exceeds the envelope total, zero if it does
	// not. UnderBy is the reverse. Both are reported only when Known.
	OverBy  money.Money
	UnderBy money.Money
}

// position computes a Position from a period, its totals, and an instant.
//
// All pacing arithmetic is delegated to budget.Envelope.Snapshot, which is the
// canonical implementation: recomputing target, allowed, or bank here would create
// a second definition of the same words that could drift from the one enforcement
// uses.
func position(def budget.Definition, p ledger.Period, tot ledger.Totals, at time.Time) Position {
	env := p.Envelope
	snap := env.Snapshot(at, tot.Spent, tot.Reserved)

	elapsed := env.Elapsed(at)
	remaining := snap.TimeRemaining

	// PaceBalance is against settled spend, not against committed. snap.Bank is
	// target-committed, which is the engine's admission question; the dashboard's
	// bank/borrow display asks whether money has actually left faster than planned.
	paceBalance, ok := money.Sub(snap.Target, tot.Spent)
	if !ok {
		paceBalance = money.Min
	}

	pos := Position{
		BudgetID:            def.ID,
		Name:                def.Name,
		ParentID:            def.ParentID,
		At:                  at,
		Period:              p,
		Allocation:          env.Allocation,
		CarryIn:             env.Carry,
		Total:               env.Total(),
		Spent:               snap.Spent,
		Reserved:            snap.Reserved,
		Committed:           snap.Committed,
		TargetByNow:         snap.Target,
		AllowedByNow:        snap.Allowed,
		PaceBalance:         paceBalance,
		SpendableNow:        snap.AvailableNow,
		RemainingAllocation: snap.PeriodRemaining,
		PeriodStart:         env.Start,
		PeriodEnd:           env.End,
		Elapsed:             elapsed,
		TimeRemaining:       remaining,
		PeriodDuration:      env.Duration(),
		Rollover:            env.Rollover,
		ExpiredHolds:        tot.ExpiredCount,
		ExpiredAmount:       tot.ReservedExpired,
		LiveHolds:           tot.PendingCount,
	}

	conf := confidence(elapsed, env.Duration())
	unit := displayUnit(env.Duration())

	pos.AverageBurn = averageBurn(snap.Spent, elapsed, unit, conf)
	pos.SustainableBurn = sustainableBurn(snap.PeriodRemaining, remaining, unit)
	pos.Pressure = pressure(snap.Spent, elapsed, snap.PeriodRemaining, remaining, conf)
	pos.Projection = projection(env, snap, elapsed, conf)
	return pos
}

// confidence qualifies figures derived from elapsed time.
//
// The comparison is taken in seconds rather than nanoseconds. Multiplying a nanosecond
// count by 10,000 overflows int64 at about ten and a half days, and a month-long budget
// is the ordinary case rather than the edge one: in nanoseconds, a month at its halfway
// mark wrapped to a negative product and reported low confidence, which put a "very
// little of the period has elapsed" caveat on a figure derived from fifteen days.
//
// Seconds keep the product inside int64 for any period shorter than about 29,000 years,
// and sub-second precision cannot move a 5% threshold on any period worth budgeting.
func confidence(elapsed, duration time.Duration) Confidence {
	switch {
	case elapsed <= 0 || duration <= 0:
		return ConfidenceNone
	case seconds(elapsed)*10_000 < seconds(duration)*lowConfidenceFraction:
		return ConfidenceLow
	default:
		return ConfidenceOK
	}
}

// seconds is a duration in whole seconds, at least one, so that a sub-second period
// cannot make the comparison above divide the difference between two zeroes.
func seconds(d time.Duration) int64 {
	s := int64(d / time.Second)
	if s < 1 {
		return 1
	}
	return s
}

// displayUnit picks the time unit a rate reads most naturally in for a period of
// this length. It changes presentation only: Rate.PerHour is unaffected.
func displayUnit(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return time.Hour
	case d < 48*time.Hour:
		// A six-hour experiment window in dollars per day would quote a figure the
		// period cannot reach.
		return time.Hour
	default:
		return 24 * time.Hour
	}
}

// averageBurn is Spent/elapsed, expressed per hour.
//
// It is an average across the whole elapsed period. Nothing here is smoothed,
// weighted, or windowed: an exponentially weighted average would be a different
// number wearing the same label, and a reader cannot compensate for a rate whose
// definition is hidden in a decay constant.
func averageBurn(spent money.Money, elapsed time.Duration, unit time.Duration, conf Confidence) Rate {
	if elapsed <= 0 {
		// No elapsed time is not a rate of zero. There has been no opportunity to
		// spend, so there is nothing to average.
		return Rate{Per: unit, Confidence: ConfidenceNone}
	}
	return Rate{
		PerHour:    perHour(spent, elapsed),
		Per:        unit,
		Known:      true,
		Confidence: conf,
	}
}

// sustainableBurn is the remaining allocation spread evenly over the remaining
// time, expressed per hour.
//
// A negative remaining allocation yields no rate rather than a negative one: an
// overspent budget has no sustainable rate, and rendering a negative dollars-per-day
// figure would read as income.
func sustainableBurn(remainingAllocation money.Money, remaining time.Duration, unit time.Duration) Rate {
	if remaining <= 0 || remainingAllocation <= 0 {
		return Rate{Per: unit, Confidence: ConfidenceNone}
	}
	return Rate{
		PerHour:    perHour(remainingAllocation, remaining),
		Per:        unit,
		Known:      true,
		Confidence: ConfidenceOK,
	}
}

// perHour converts an amount over a duration into microdollars per hour, exactly.
//
// The intermediate product of a microdollar amount and an hour in nanoseconds
// exceeds int64 for any realistic budget, so it is computed in big.Int. Truncation
// is toward zero, which for a sustainable rate means erring toward advising less
// spend rather than more.
func perHour(amount money.Money, d time.Duration) money.Money {
	if d <= 0 || amount == 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(int64(amount)), big.NewInt(int64(time.Hour)))
	n.Quo(n, big.NewInt(int64(d)))
	if !n.IsInt64() {
		if amount > 0 {
			return money.Max
		}
		return money.Min
	}
	return money.Money(n.Int64())
}

// pressure computes the throttle gauge.
//
// The ratio is (spent/elapsed) / (remainingAllocation/remaining), evaluated by
// cross-multiplication as
//
//	10000 * spent * remaining / (elapsed * remainingAllocation)
//
// in one exact big.Int step. Computing the two rates first and dividing them would
// square the truncation error and, worse, make the gauge disagree with the two
// figures displayed beside it in a way that depended on the period length.
//
// Every denominator is checked before it is used, so there is no reachable division
// by zero. The three not-a-number states are returned rather than collapsed to a
// zero reading, because "not started", "over budget", and "idle" are three
// different situations and only one of them is 0%.
func pressure(spent money.Money, elapsed time.Duration, remainingAllocation money.Money, remaining time.Duration, conf Confidence) Pressure {
	switch {
	case elapsed <= 0:
		return Pressure{State: PressureNotStarted, Confidence: ConfidenceNone}
	case remaining <= 0:
		return Pressure{State: PressureEnded, Confidence: ConfidenceNone}
	case remainingAllocation <= 0:
		// No sustainable rate exists to divide by. The denominator is genuinely zero
		// or negative rather than merely small, so there is no ratio to report -- and
		// a very large number here would look like a measurement.
		return Pressure{State: PressureNoHeadroom, Confidence: ConfidenceNone}
	case spent <= 0:
		// Real and meaningful: nothing has burned over real elapsed time. This is the
		// only zero reading the gauge produces.
		return Pressure{BasisPoints: 0, State: PressureMeasured, Confidence: conf}
	}

	num := new(big.Int).Mul(big.NewInt(int64(spent)), big.NewInt(int64(remaining)))
	num.Mul(num, big.NewInt(10_000))
	den := new(big.Int).Mul(big.NewInt(int64(elapsed)), big.NewInt(int64(remainingAllocation)))

	bp := new(big.Int).Quo(num, den)
	if !bp.IsInt64() {
		// A ratio this large is not distinguishable from "pegged" to any reader, but
		// it is still a measured ratio, so it is reported as one at the cap rather
		// than reclassified.
		return Pressure{BasisPoints: int64(^uint64(0) >> 1), State: PressureMeasured, Confidence: conf}
	}
	return Pressure{BasisPoints: bp.Int64(), State: PressureMeasured, Confidence: conf}
}

// projection extrapolates end-of-period spend from the average rate to date.
//
// It is straight-line and nothing more: spent * duration / elapsed, which is the
// same arithmetic engine.project already performs. Recomputing it here rather than
// calling the engine keeps this package off the engine's dependency, and the test
// suite pins the two to agree.
func projection(env budget.Envelope, snap budget.Snapshot, elapsed time.Duration, conf Confidence) Projection {
	duration := env.Duration()
	if elapsed <= 0 || duration <= 0 {
		// Before any time has elapsed there is no rate to extrapolate. Reporting the
		// committed amount as a "projection" would dress a current figure up as a
		// prediction.
		return Projection{Confidence: ConfidenceNone}
	}

	var amount money.Money
	if elapsed >= duration {
		// The period is over; the projection is simply what was spent.
		amount = snap.Spent
	} else {
		n := new(big.Int).Mul(big.NewInt(int64(snap.Spent)), big.NewInt(int64(duration)))
		n.Quo(n, big.NewInt(int64(elapsed)))
		if n.IsInt64() {
			amount = money.Money(n.Int64())
		} else {
			amount = money.Max
		}
	}

	proj := Projection{Amount: amount, Known: true, Confidence: conf}
	total := env.Total()
	if amount > total {
		if v, ok := money.Sub(amount, total); ok {
			proj.OverBy = v
		}
	} else if v, ok := money.Sub(total, amount); ok {
		proj.UnderBy = v
	}
	return proj
}

// divRound divides two big.Ints, rounding half away from zero, and returns an
// int64, saturating rather than wrapping.
func divRound(num, den *big.Int) int64 {
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	// Compare 2*|r| with |den| to decide the half.
	twice := new(big.Int).Abs(r)
	twice.Lsh(twice, 1)
	if twice.Cmp(new(big.Int).Abs(den)) >= 0 {
		if (num.Sign() < 0) != (den.Sign() < 0) {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		if q.Sign() < 0 {
			return int64(money.Min)
		}
		return int64(money.Max)
	}
	return q.Int64()
}
