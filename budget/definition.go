package budget

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"throttle/money"
)

// Recurrence is how a budget definition generates successive periods.
type Recurrence string

const (
	// RecurNone is a single fixed period, such as a grant with a hard end date.
	// It never generates a successor.
	RecurNone Recurrence = "none"

	// RecurDaily, RecurWeekly, and RecurMonthly step the calendar in the
	// definition's timezone, so period bounds follow local calendar rules
	// including DST transitions and varying month lengths.
	RecurDaily   Recurrence = "daily"
	RecurWeekly  Recurrence = "weekly"
	RecurMonthly Recurrence = "monthly"

	// RecurDuration steps by a fixed wall-clock duration, for periods that are
	// not calendar-aligned (for example a 6-hour experiment window).
	RecurDuration Recurrence = "duration"
)

// Definition is the durable configuration of a budget: the recurring rule from
// which concrete periods are generated. It is the thing that is persisted and
// shared between processes; an Envelope is one materialized period of it.
//
// A definition never holds spend. Spend lives in the ledger against a period.
type Definition struct {
	ID string

	// ParentID links this budget into a hierarchy. Empty means a root budget.
	// A request against this budget also consumes headroom from every ancestor.
	ParentID string

	Name string

	// Allocation is the new money granted per period.
	Allocation money.Money

	// Borrow lets a period pull spend forward along its pacing curve.
	Borrow time.Duration

	Rollover RolloverPolicy

	Recurrence Recurrence

	// Every is the period length when Recurrence is RecurDuration.
	Every time.Duration

	// Location is the timezone in which calendar recurrence is evaluated. Pacing
	// math is always UTC; this only decides where period boundaries fall. Nil
	// means UTC.
	Location *time.Location

	// AnchorAt is the start of the first period. For calendar recurrences the
	// boundaries step from this instant, so an anchor of the 15th produces
	// periods running 15th-to-15th.
	AnchorAt time.Time

	// EndAt optionally bounds the whole definition, after which no further
	// periods are generated. Zero means open-ended.
	EndAt time.Time
}

// Errors returned when working with definitions and periods.
var (
	// ErrNoSuchPeriod means the requested period is outside the definition.
	ErrNoSuchPeriod = errors.New("budget: no such period")

	// ErrNotRecurring means a successor was requested from a definition that
	// generates exactly one period.
	ErrNotRecurring = errors.New("budget: definition does not recur")
)

func (d Definition) location() *time.Location {
	if d.Location == nil {
		return time.UTC
	}
	return d.Location
}

// Validate reports whether the definition is well formed.
func (d Definition) Validate() error {
	if d.ID == "" {
		return errors.New("budget: definition id is required")
	}
	if d.ID == d.ParentID {
		return fmt.Errorf("budget %q: cannot be its own parent", d.ID)
	}
	if d.Allocation < 0 {
		return fmt.Errorf("budget %q: allocation cannot be negative", d.ID)
	}
	if d.Borrow < 0 {
		return fmt.Errorf("budget %q: borrow window cannot be negative", d.ID)
	}
	if d.AnchorAt.IsZero() {
		return fmt.Errorf("budget %q: anchor time is required", d.ID)
	}
	if !d.EndAt.IsZero() && !d.EndAt.After(d.AnchorAt) {
		return fmt.Errorf("budget %q: end must be after the anchor", d.ID)
	}
	switch d.Recurrence {
	case RecurNone:
		// A non-recurring definition needs an explicit end; otherwise its single
		// period has no bound and pacing has no denominator.
		if d.EndAt.IsZero() {
			return fmt.Errorf("budget %q: a non-recurring definition requires an end", d.ID)
		}
	case RecurDaily, RecurWeekly, RecurMonthly:
	case RecurDuration:
		if d.Every <= 0 {
			return fmt.Errorf("budget %q: duration recurrence requires a positive period length", d.ID)
		}
	case "":
		return fmt.Errorf("budget %q: recurrence is required", d.ID)
	default:
		return fmt.Errorf("budget %q: unknown recurrence %q", d.ID, d.Recurrence)
	}
	if d.Rollover.Cap < 0 {
		return fmt.Errorf("budget %q: rollover cap cannot be negative", d.ID)
	}
	if d.Rollover.CapBasisPoints < 0 {
		return fmt.Errorf("budget %q: rollover cap basis points cannot be negative", d.ID)
	}
	if d.Rollover.Cap > 0 && d.Rollover.CapBasisPoints > 0 {
		return fmt.Errorf("budget %q: rollover cap may be an absolute amount or a percentage, not both", d.ID)
	}
	switch d.Rollover.Mode {
	case "", RolloverNone, RolloverCredit, RolloverBalance:
	default:
		return fmt.Errorf("budget %q: unknown rollover mode %q", d.ID, d.Rollover.Mode)
	}
	return nil
}

// Bounds returns the half-open [start, end) of period seq, counting from zero.
//
// Calendar recurrences step in the definition's timezone, so a monthly budget
// anchored on the 1st gets 28, 29, 30, or 31 days as the calendar dictates, and a
// DST transition makes one period an hour shorter or longer than its neighbours.
// That is the intent: a "month" of budget should match the user's month.
func (d Definition) Bounds(seq int) (start, end time.Time, err error) {
	if seq < 0 {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: negative sequence %d", ErrNoSuchPeriod, seq)
	}

	loc := d.location()
	anchor := d.AnchorAt.In(loc)

	switch d.Recurrence {
	case RecurNone:
		if seq != 0 {
			return time.Time{}, time.Time{}, fmt.Errorf("%w: %w", ErrNoSuchPeriod, ErrNotRecurring)
		}
		start, end = d.AnchorAt.UTC(), d.EndAt.UTC()
	case RecurDuration:
		start = d.AnchorAt.UTC().Add(time.Duration(seq) * d.Every)
		end = start.Add(d.Every)
	case RecurDaily:
		start = anchor.AddDate(0, 0, seq)
		end = anchor.AddDate(0, 0, seq+1)
	case RecurWeekly:
		start = anchor.AddDate(0, 0, 7*seq)
		end = anchor.AddDate(0, 0, 7*(seq+1))
	case RecurMonthly:
		start = addMonths(anchor, seq)
		end = addMonths(anchor, seq+1)
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("budget %q: unknown recurrence %q", d.ID, d.Recurrence)
	}

	start, end = start.UTC(), end.UTC()

	// A bounded definition's last period is truncated at the definition end
	// rather than extending past it.
	if !d.EndAt.IsZero() {
		endAt := d.EndAt.UTC()
		if !start.Before(endAt) {
			return time.Time{}, time.Time{}, fmt.Errorf("%w: period %d starts at or after the definition end", ErrNoSuchPeriod, seq)
		}
		if end.After(endAt) {
			end = endAt
		}
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("%w: period %d is empty", ErrNoSuchPeriod, seq)
	}
	return start, end, nil
}

// addMonths advances t by n calendar months, clamping the day to the last day of
// the target month.
//
// time.AddDate normalizes overflow, so January 31 plus one month becomes March 3
// rather than February 28. For a budget anchored at month end that would silently
// shift every subsequent boundary, so the day is clamped instead.
func addMonths(t time.Time, n int) time.Time {
	year, month, day := t.Date()
	hour, min, sec := t.Clock()

	target := time.Date(year, month, 1, hour, min, sec, t.Nanosecond(), t.Location()).AddDate(0, n, 0)
	ty, tm, _ := target.Date()

	if last := daysInMonth(ty, tm); day > last {
		day = last
	}
	return time.Date(ty, tm, day, hour, min, sec, t.Nanosecond(), t.Location())
}

// daysInMonth returns the number of days in a month, correct for leap years.
func daysInMonth(year int, month time.Month) int {
	// Day 0 of the following month is the last day of this one.
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// PeriodFor returns the sequence number of the period containing at.
func (d Definition) PeriodFor(at time.Time) (int, error) {
	if at.Before(d.AnchorAt) {
		return 0, fmt.Errorf("%w: %s precedes the anchor %s",
			ErrNoSuchPeriod, at.UTC().Format(time.RFC3339), d.AnchorAt.UTC().Format(time.RFC3339))
	}
	if !d.EndAt.IsZero() && !at.Before(d.EndAt) {
		return 0, fmt.Errorf("%w: %s is at or after the definition end", ErrNoSuchPeriod, at.UTC().Format(time.RFC3339))
	}

	// An estimate followed by a short correction walk. Calendar periods vary in
	// length, so the estimate can be off by one; it is never off by many.
	seq := d.estimateSeq(at)
	for range 8 {
		start, end, err := d.Bounds(seq)
		if err != nil {
			if seq == 0 {
				return 0, err
			}
			seq--
			continue
		}
		switch {
		case at.Before(start):
			seq--
		case !at.Before(end):
			seq++
		default:
			return seq, nil
		}
		if seq < 0 {
			return 0, fmt.Errorf("%w: %s precedes the first period", ErrNoSuchPeriod, at.UTC().Format(time.RFC3339))
		}
	}
	return 0, fmt.Errorf("%w: could not locate a period containing %s", ErrNoSuchPeriod, at.UTC().Format(time.RFC3339))
}

func (d Definition) estimateSeq(at time.Time) int {
	loc := d.location()
	anchor := d.AnchorAt.In(loc)
	local := at.In(loc)

	switch d.Recurrence {
	case RecurNone:
		return 0
	case RecurDuration:
		return int(at.Sub(d.AnchorAt) / d.Every)
	case RecurDaily:
		return int(local.Sub(anchor) / (24 * time.Hour))
	case RecurWeekly:
		return int(local.Sub(anchor) / (7 * 24 * time.Hour))
	case RecurMonthly:
		ay, am, _ := anchor.Date()
		ly, lm, _ := local.Date()
		return (ly-ay)*12 + int(lm) - int(am)
	}
	return 0
}

// Envelope materializes period seq as an envelope with the given inherited carry.
//
// The allocation, borrow window, and rollover policy are copied from the
// definition at materialization time. Callers that persist periods should store
// that snapshot, so that later editing a definition cannot retroactively rewrite
// what a closed period was allowed to spend.
func (d Definition) Envelope(seq int, carry money.Money) (Envelope, error) {
	start, end, err := d.Bounds(seq)
	if err != nil {
		return Envelope{}, err
	}
	env := Envelope{
		ID:         d.PeriodID(seq),
		ParentID:   d.ParentID,
		Allocation: d.Allocation,
		Carry:      carry,
		Start:      start,
		End:        end,
		Borrow:     d.Borrow,
		Rollover:   d.Rollover,
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// PeriodID is the stable identifier for a materialized period.
//
// The sequence number is part of the ID rather than only the date, so the
// identifier stays unique and ordered even if a definition's anchor is later
// moved.
func (d Definition) PeriodID(seq int) string {
	return d.ID + "@" + strconv.Itoa(seq)
}

// Fingerprint is a stable hash of the semantic fields of a definition.
//
// Two processes that disagree about a budget's definition must not silently
// share its ledger, and comparing fingerprints is how that disagreement is
// detected. Name is excluded: it is a display label, so renaming a budget is not
// a semantic conflict.
//
// Fields that have more than one spelling are normalized first. A rollover mode
// of "" and one of "none" describe the same policy -- Validate accepts both and
// CarryInto returns zero for both -- so hashing them differently would make two
// identical budgets look like a conflict. That is not hypothetical: a config
// file that omits a rollover block produces "" and the ledger stores "none",
// which reported drift between a definition and its own stored copy.
func (d Definition) Fingerprint() string {
	loc := "UTC"
	if d.Location != nil {
		loc = d.Location.String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "id=%s\x00parent=%s\x00alloc=%d\x00borrow=%d\x00",
		d.ID, d.ParentID, int64(d.Allocation), int64(d.Borrow))
	fmt.Fprintf(&b, "mode=%s\x00cap=%d\x00capbp=%d\x00",
		d.Rollover.Mode.Normalized(), int64(d.Rollover.Cap), d.Rollover.CapBasisPoints)
	fmt.Fprintf(&b, "recur=%s\x00every=%d\x00tz=%s\x00anchor=%d\x00end=%d",
		d.Recurrence, int64(d.Every), loc, d.AnchorAt.UTC().UnixNano(), endNanos(d.EndAt))

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// endNanos maps a zero end time to 0 rather than to a huge negative nanosecond
// count, so an open-ended definition fingerprints stably.
func endNanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}
