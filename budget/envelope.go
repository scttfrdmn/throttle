// Package budget implements provider-neutral spending envelopes and linear
// pacing. It answers, for a given allocation over a time period: what should
// have been spent by now, how much is banked or borrowed, how much may be
// committed right now, and when an unaffordable request becomes affordable.
//
// This package must never import a provider SDK.
package budget

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/scttfrdmn/throttle/money"
)

// RolloverMode selects what happens to an envelope's unused or overspent
// allocation when the next envelope in a recurrence begins.
type RolloverMode string

const (
	// RolloverNone discards leftover allocation; each envelope starts fresh.
	RolloverNone RolloverMode = "none"

	// RolloverCredit carries unspent allocation forward but never carries debt.
	RolloverCredit RolloverMode = "credit"

	// RolloverBalance carries the true signed balance forward, so an overspent
	// envelope reduces the next envelope's available spend.
	RolloverBalance RolloverMode = "balance"
)

// RolloverPolicy describes how a closing envelope's balance enters the next one.
//
// A positive-carry cap may be configured as an absolute amount or as a
// percentage, never both. Both forms resolve to Money via ResolveCap before any
// carry is computed, so percentage arithmetic never reaches the ledger.
type RolloverPolicy struct {
	Mode RolloverMode

	// Cap limits the magnitude of positive carry into the next envelope as an
	// absolute amount. Zero means unset.
	Cap money.Money

	// CapBasisPoints limits positive carry as a fraction of the closing period's
	// allocation, in basis points (2500 = 25%). Zero means unset.
	CapBasisPoints int64
}

// Normalized is the mode's canonical spelling.
//
// The zero value means the same thing as RolloverNone: no carry. Both spellings
// are accepted everywhere a mode is read, so anything that compares modes rather
// than acting on them -- a fingerprint, a display string, a stored column --
// normalizes first, or two identical policies compare unequal.
func (m RolloverMode) Normalized() RolloverMode {
	if m == "" {
		return RolloverNone
	}
	return m
}

// Capped reports whether any positive-carry cap is configured.
func (p RolloverPolicy) Capped() bool { return p.Cap > 0 || p.CapBasisPoints > 0 }

// Envelope is a spending allocation over a half-open time period [Start, End).
//
// "Month" is deliberately absent: monthly budgets are a recurrence layer over
// sequential envelopes, not a concept inside pacing math.
type Envelope struct {
	ID       string
	ParentID string

	// Allocation is the new money granted for this period. It must not be negative.
	Allocation money.Money

	// Carry is the signed balance inherited from the previous envelope.
	// Negative carry represents inherited debt.
	Carry money.Money

	Start time.Time
	End   time.Time

	// Borrow lets the workload pull spend forward along the pacing curve by up
	// to this much time. It changes timing only, never the total allocation.
	Borrow time.Duration

	Rollover RolloverPolicy
}

// ErrOverflow reports that an envelope's arithmetic exceeded the money range.
var ErrOverflow = errors.New("budget: arithmetic overflow")

// Validate reports whether the envelope is well formed. All other methods
// assume a validated envelope; they are written to be safe rather than correct
// on invalid input, but their answers are only meaningful after Validate.
func (e Envelope) Validate() error {
	if e.ID == "" {
		return errors.New("budget: id is required")
	}
	if e.Allocation < 0 {
		return fmt.Errorf("budget %q: allocation cannot be negative", e.ID)
	}
	if e.Start.IsZero() || e.End.IsZero() {
		return fmt.Errorf("budget %q: start and end are required", e.ID)
	}
	if !e.End.After(e.Start) {
		return fmt.Errorf("budget %q: end must be after start", e.ID)
	}
	// time.Time.Sub saturates at ±292 years. An envelope that long is a
	// configuration error, and letting it through would silently distort pacing.
	if e.End.Sub(e.Start) <= 0 {
		return fmt.Errorf("budget %q: period is too long to represent", e.ID)
	}
	if e.Borrow < 0 {
		return fmt.Errorf("budget %q: borrow window cannot be negative", e.ID)
	}
	if _, ok := money.Add(e.Carry, e.Allocation); !ok {
		return fmt.Errorf("budget %q: carry plus allocation overflows: %w", e.ID, ErrOverflow)
	}
	if e.Rollover.Cap < 0 {
		return fmt.Errorf("budget %q: rollover cap cannot be negative", e.ID)
	}
	if e.Rollover.CapBasisPoints < 0 {
		return fmt.Errorf("budget %q: rollover cap basis points cannot be negative", e.ID)
	}
	if e.Rollover.Cap > 0 && e.Rollover.CapBasisPoints > 0 {
		return fmt.Errorf("budget %q: rollover cap may be an absolute amount or a percentage, not both", e.ID)
	}
	switch e.Rollover.Mode {
	case "", RolloverNone, RolloverCredit, RolloverBalance:
	default:
		return fmt.Errorf("budget %q: unknown rollover mode %q", e.ID, e.Rollover.Mode)
	}
	return nil
}

// Duration is the length of the envelope period.
func (e Envelope) Duration() time.Duration { return e.End.Sub(e.Start) }

// Elapsed is the time from Start to at, clamped to [0, Duration]. Clamping at
// the boundaries is what makes Target and Allowed monotonic and total-bounded.
func (e Envelope) Elapsed(at time.Time) time.Duration {
	if !at.After(e.Start) {
		return 0
	}
	if !at.Before(e.End) {
		return e.Duration()
	}
	return at.Sub(e.Start)
}

// Total is the maximum cumulative spend this envelope can ever authorize.
func (e Envelope) Total() money.Money {
	t, ok := money.Add(e.Carry, e.Allocation)
	if !ok {
		// Validate rejects this; saturate rather than wrap if it slips through.
		if e.Allocation > 0 {
			return money.Max
		}
		return money.Min
	}
	return t
}

// prorate returns total*elapsed/duration using exact integer arithmetic.
//
// The intermediate product of two int64 values does not fit in an int64, so a
// big.Int is used. These are control-plane calculations performed per request,
// not per token, so the allocation is not worth trading correctness for.
func prorate(total money.Money, elapsed, duration time.Duration) money.Money {
	if total == 0 || elapsed <= 0 || duration <= 0 {
		return 0
	}
	if elapsed >= duration {
		return total
	}
	n := new(big.Int).Mul(big.NewInt(int64(total)), big.NewInt(int64(elapsed)))
	// Quo truncates toward zero, which for negative carry-driven totals errs
	// toward reporting less pacing credit rather than more.
	n.Quo(n, big.NewInt(int64(duration)))
	return money.Money(n.Int64()) // |result| <= |total|, so this cannot overflow.
}

// Target is the cumulative spend that linear pacing says should have been
// consumed by at, including inherited carry.
func (e Envelope) Target(at time.Time) money.Money {
	v, _ := money.Add(e.Carry, prorate(e.Allocation, e.Elapsed(at), e.Duration()))
	return v
}

// Allowed is the cumulative spend permitted at at, after the borrow window
// pulls the pacing curve forward. It never exceeds Total.
func (e Envelope) Allowed(at time.Time) money.Money {
	d := e.Duration()
	elapsed := e.Elapsed(at)

	// Advance by the borrow window without overflowing the duration.
	if e.Borrow > 0 {
		if remaining := d - elapsed; e.Borrow >= remaining {
			elapsed = d
		} else {
			elapsed += e.Borrow
		}
	}
	v, _ := money.Add(e.Carry, prorate(e.Allocation, elapsed, d))
	return v
}

// Snapshot is a point-in-time view of an envelope's position.
type Snapshot struct {
	At time.Time

	// Target is the paced cumulative spend for At.
	Target money.Money

	// Allowed is Target plus whatever the borrow window pulls forward.
	Allowed money.Money

	Spent    money.Money
	Reserved money.Money

	// Committed is Spent+Reserved: money that is gone or promised.
	Committed money.Money

	// Bank is Target-Committed. Positive means underspent relative to pace;
	// negative means future allocation has already been consumed.
	Bank money.Money

	// AvailableNow is how much may be committed at At. It is never negative,
	// because a negative value is not a spendable quantity.
	AvailableNow money.Money

	// PeriodRemaining is the signed whole-envelope headroom. It is negative
	// when actual spend has exceeded the envelope total; overrun is reported
	// rather than hidden.
	PeriodRemaining money.Money

	TimeRemaining time.Duration

	// SustainableRate is the spend-per-hour that exactly consumes
	// PeriodRemaining over TimeRemaining. Zero once the period is over or the
	// envelope is exhausted.
	SustainableRate money.Money
}

// Overspent reports whether committed spend has exceeded the envelope total.
func (s Snapshot) Overspent() bool { return s.PeriodRemaining < 0 }

// Snapshot computes the envelope position at at for the given spend and
// outstanding reservations.
func (e Envelope) Snapshot(at time.Time, spent, reserved money.Money) Snapshot {
	target := e.Target(at)
	allowed := e.Allowed(at)

	committed, ok := money.Add(spent, reserved)
	if !ok {
		committed = money.Max
	}

	available, ok := money.Sub(allowed, committed)
	if !ok || available < 0 {
		available = 0
	}

	remaining, ok := money.Sub(e.Total(), committed)
	if !ok {
		remaining = money.Min
	}

	bank, ok := money.Sub(target, committed)
	if !ok {
		bank = money.Min
	}

	timeRemaining := time.Duration(0)
	if at.Before(e.End) {
		timeRemaining = e.End.Sub(at)
		if timeRemaining < 0 { // Sub saturated on an extreme timestamp.
			timeRemaining = 0
		}
	}

	return Snapshot{
		At:              at,
		Target:          target,
		Allowed:         allowed,
		Spent:           spent,
		Reserved:        reserved,
		Committed:       committed,
		Bank:            bank,
		AvailableNow:    available,
		PeriodRemaining: remaining,
		TimeRemaining:   timeRemaining,
		SustainableRate: ratePerHour(remaining, timeRemaining),
	}
}

// ratePerHour converts an amount remaining over a duration into an hourly rate.
//
// Rounding down matters: a rate that rounds up is advice to overspend. The
// previous implementation floored the duration to whole hours and then clamped
// it to a minimum of one hour, which told a caller with 90 minutes left and
// $1000 unspent that $1000/hour was sustainable -- a 50% overrun.
func ratePerHour(remaining money.Money, d time.Duration) money.Money {
	if remaining <= 0 || d <= 0 {
		return 0
	}
	n := new(big.Int).Mul(big.NewInt(int64(remaining)), big.NewInt(int64(time.Hour)))
	n.Quo(n, big.NewInt(int64(d)))
	if !n.IsInt64() {
		return money.Max
	}
	return money.Money(n.Int64())
}

// Outcome is the enforcement decision for a request.
type Outcome string

const (
	// OutcomeAllow means the estimate fits the envelope right now.
	OutcomeAllow Outcome = "allow"

	// OutcomeWait means the estimate does not fit now but will fit later in
	// this envelope as the pacing curve advances.
	OutcomeWait Outcome = "wait"

	// OutcomeDeny means the estimate cannot fit this envelope at all.
	OutcomeDeny Outcome = "deny"
)

// Decision is the explicit result of an admission check.
//
// This is deliberately a struct rather than a bool: enforcement modes and
// future policy layers need the reason and the timing, not just a yes/no.
type Decision struct {
	Outcome  Outcome
	Estimate money.Money

	// RetryAt is the earliest time the estimate becomes affordable, set only
	// when Outcome is OutcomeWait.
	RetryAt time.Time

	// Shortfall is how much more headroom the estimate needs to be admitted
	// now. Zero when the outcome is OutcomeAllow.
	Shortfall money.Money

	Reason   string
	Snapshot Snapshot
}

// Wait is how long the caller must wait before retrying, or zero if the
// decision was not a wait.
func (d Decision) Wait(now time.Time) time.Duration {
	if d.Outcome != OutcomeWait || !d.RetryAt.After(now) {
		return 0
	}
	return d.RetryAt.Sub(now)
}

// Admit decides whether an estimated cost can be committed against the
// envelope at now, and if not, whether waiting would help.
func (e Envelope) Admit(now time.Time, spent, reserved, estimate money.Money) Decision {
	s := e.Snapshot(now, spent, reserved)
	d := Decision{Estimate: estimate, Snapshot: s}

	if estimate < 0 {
		d.Outcome = OutcomeDeny
		d.Reason = "estimate cannot be negative"
		return d
	}

	// A zero-cost request commits nothing, so it cannot breach the envelope even
	// when the envelope is already overspent. Making it wait would stall callers
	// for no accounting benefit.
	if estimate == 0 {
		d.Outcome = OutcomeAllow
		return d
	}

	committedAfter, ok := money.Add(s.Committed, estimate)
	if !ok {
		d.Outcome = OutcomeDeny
		d.Reason = "estimate overflows the ledger"
		return d
	}

	if committedAfter <= s.Allowed {
		d.Outcome = OutcomeAllow
		return d
	}

	if shortfall, ok := money.Sub(committedAfter, s.Allowed); ok {
		d.Shortfall = shortfall
	}

	// Past the envelope total, no amount of waiting helps.
	if committedAfter > e.Total() {
		d.Outcome = OutcomeDeny
		d.Reason = "estimate exceeds the remaining envelope allocation"
		return d
	}
	if !now.Before(e.End) {
		d.Outcome = OutcomeDeny
		d.Reason = "envelope period has ended"
		return d
	}

	at, affordable := e.affordableAt(now, committedAfter)
	if !affordable {
		d.Outcome = OutcomeDeny
		d.Reason = "estimate does not become affordable before the period ends"
		return d
	}
	d.Outcome = OutcomeWait
	d.RetryAt = at
	d.Reason = "estimate exceeds the currently paced allowance"
	return d
}

// affordableAt returns the earliest time at which cumulative committed spend of
// committedAfter falls inside the allowed pacing curve, assuming no further
// spend or releases occur.
func (e Envelope) affordableAt(now time.Time, committedAfter money.Money) (time.Time, bool) {
	if e.Allocation <= 0 {
		// With no new money the curve never rises, so waiting cannot help.
		return time.Time{}, false
	}

	// Solve Allocation*elapsed/Duration >= committedAfter-Carry for elapsed.
	need, ok := money.Sub(committedAfter, e.Carry)
	if !ok {
		return time.Time{}, false
	}
	if need < 0 {
		need = 0
	}

	elapsed, ok := mulDivCeil(int64(need), e.Duration(), e.Allocation)
	if !ok {
		return time.Time{}, false
	}

	// Allowed() looks ahead by Borrow, so the wall-clock time that produces
	// this effective elapsed is earlier by the borrow window.
	at := e.Start.Add(elapsed).Add(-e.Borrow)
	if at.Before(now) {
		at = now
	}
	if at.After(e.End) {
		return time.Time{}, false
	}
	return at, true
}

// mulDivCeil returns ceil(a*b/divisor) as a duration, reporting false if the
// result cannot be represented. Rounding up is required: rounding down would
// return a retry time at which the request is still a microdollar short.
func mulDivCeil(a int64, b time.Duration, divisor money.Money) (time.Duration, bool) {
	if divisor == 0 {
		return 0, false
	}
	n := new(big.Int).Mul(big.NewInt(a), big.NewInt(int64(b)))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, big.NewInt(int64(divisor)), r)
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, false
	}
	return time.Duration(q.Int64()), true
}
