package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/scttfrdmn/throttle/money"
)

// Scalar parsing: money, durations, dates. Every one of these reports the field path it
// failed on, because a config error whose message does not say where it is sends the
// reader back to re-read the whole file.

// parseMoney reads a monetary amount through the hardened parser.
//
// There is no other path. money.Parse handles "$4,000", "4000", and "4000.00" in integer
// arithmetic and rejects everything else; a strconv.ParseFloat here would be a second
// implementation of money with rounding error built in, which is the one thing the
// microdollar rule exists to prevent.
func parseMoney(path, raw string) (money.Money, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, fieldErr(path, "an amount is required")
	}
	m, err := money.Parse(raw)
	if err != nil {
		return 0, fieldErr(path, fmt.Sprintf("invalid money %q", raw))
	}
	if m < 0 {
		return 0, fieldErr(path, fmt.Sprintf("%q is negative; an allocation is money granted, not owed", raw))
	}
	return m, nil
}

// parseDuration reads a fixed wall-clock duration.
//
// Go's syntax, extended with "d" and "w" as exact multiples of 24h and 168h, because a
// borrow window is naturally written in days and "72h" makes a reader do arithmetic.
//
// Calendar units are refused rather than approximated. A month is 28 to 31 days and a year
// is 365 or 366, so "1mo" has no fixed length: accepting it as 720h is how "one month of
// borrow" silently becomes thirty days in February and twenty-eight days' worth of pacing
// in a month that has thirty-one. Recurrence is where calendar semantics live, and it has
// a timezone to evaluate them in; a duration does not.
func parseDuration(path, raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fieldErr(path, "a duration is required")
	}

	for _, unit := range []struct {
		suffix string
		why    string
	}{
		{"mo", "a calendar month is 28 to 31 days, so it has no fixed length"},
		{"month", "a calendar month is 28 to 31 days, so it has no fixed length"},
		{"months", "a calendar month is 28 to 31 days, so it has no fixed length"},
		{"y", "a calendar year is 365 or 366 days, so it has no fixed length"},
		{"yr", "a calendar year is 365 or 366 days, so it has no fixed length"},
		{"year", "a calendar year is 365 or 366 days, so it has no fixed length"},
		{"years", "a calendar year is 365 or 366 days, so it has no fixed length"},
	} {
		if strings.HasSuffix(strings.ToLower(s), unit.suffix) && hasLeadingDigit(s) {
			return 0, fieldErr(path, fmt.Sprintf(
				"%q uses a calendar unit, which is not a fixed duration: %s. "+
					"Use hours, days, or weeks here; calendar periods are set with period.recur",
				raw, unit.why))
		}
	}

	// Days and weeks are exact multiples and are expanded before Go sees the string.
	// They are wall-clock spans, not calendar days: a "1d" borrow window is 24 hours
	// across a DST transition, which is the correct reading for a span that is not
	// anchored to a calendar.
	expanded, err := expandDayWeek(s)
	if err != nil {
		return 0, fieldErr(path, fmt.Sprintf("invalid duration %q", raw))
	}

	d, err := time.ParseDuration(expanded)
	if err != nil {
		return 0, fieldErr(path, fmt.Sprintf(
			"invalid duration %q: use a unit such as 30m, 12h, 3d, or 2w", raw))
	}
	if d < 0 {
		return 0, fieldErr(path, fmt.Sprintf("%q is negative", raw))
	}
	return d, nil
}

func hasLeadingDigit(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			return true
		}
		if r != '-' && r != '+' && r != '.' && r != ' ' {
			return false
		}
	}
	return false
}

// expandDayWeek rewrites "3d" and "2w" into hours, leaving the rest of the string for
// time.ParseDuration. Mixed forms such as "1w2d12h" work.
func expandDayWeek(s string) (string, error) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		start := i
		for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == '-' || s[i] == '+') {
			i++
		}
		digits := s[start:i]
		unitStart := i
		for i < len(s) && !(s[i] >= '0' && s[i] <= '9') && s[i] != '.' {
			i++
		}
		unit := s[unitStart:i]

		switch unit {
		case "d", "w":
			if digits == "" {
				return "", fmt.Errorf("no count before %q", unit)
			}
			n, err := strconv.ParseFloat(digits, 64)
			if err != nil {
				return "", err
			}
			hours := 24.0
			if unit == "w" {
				hours = 168.0
			}
			// Formatted as hours with enough precision for any fractional count a person
			// would write, then handed to time.ParseDuration -- the float never reaches a
			// stored value, and nothing monetary is on this path.
			fmt.Fprintf(&b, "%gh", n*hours)
		default:
			b.WriteString(digits)
			b.WriteString(unit)
		}
	}
	return b.String(), nil
}

// dateLayouts are the accepted forms for an anchor or an end.
//
// A bare date is what a grant is written as, and RFC3339 is what a machine writes.
var dateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseDate reads an instant in the budget's own timezone.
//
// The timezone matters: a monthly budget anchored to "2026-09-01" in America/New_York
// starts at midnight in New York, not at midnight UTC, which is 20:00 the previous day
// there. Parsing in UTC and hoping would shift every period boundary by the offset.
//
// endOfDay is for a bare date used as an end bound. A grant "through 2028-08-31" includes
// the 31st, and reading it as midnight at the start of that day would expire the budget a
// day early -- a quiet accounting error, since nothing would look wrong.
func parseDate(path, raw string, loc *time.Location, endOfDay bool) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, fieldErr(path, "a date is required")
	}
	for _, layout := range dateLayouts {
		t, err := time.ParseInLocation(layout, s, loc)
		if err != nil {
			continue
		}
		if layout == "2006-01-02" && endOfDay {
			// The instant at which that day ends, which is the start of the next.
			return t.AddDate(0, 0, 1), nil
		}
		return t, nil
	}
	return time.Time{}, fieldErr(path, fmt.Sprintf(
		"invalid date %q: use 2026-09-01 or a full RFC3339 timestamp", raw))
}

// parseLocation reads a timezone name.
func parseLocation(path, raw string) (*time.Location, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(s)
	if err != nil {
		return nil, fieldErr(path, fmt.Sprintf(
			"unknown timezone %q: use an IANA name such as America/New_York or UTC", raw))
	}
	return loc, nil
}

// parsePercent converts a user-facing percentage to integer basis points.
//
// Refused rather than rounded when it is finer than a basis point: a cap the user typed
// and a cap throttle stores differing in the fourth decimal place is a discrepancy nobody
// would think to look for.
func parsePercent(path string, raw string) (int64, error) {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	if s == "" {
		return 0, fieldErr(path, "a percentage is required")
	}

	// Parsed as a decimal string rather than through a float, so 12.34 is exactly 1234
	// basis points and nothing depends on binary representation.
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 2 {
		return 0, fieldErr(path, fmt.Sprintf(
			"%q is finer than one basis point (0.01%%)", raw))
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 {
		return 0, fieldErr(path, fmt.Sprintf("invalid percentage %q", raw))
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil || f < 0 {
		return 0, fieldErr(path, fmt.Sprintf("invalid percentage %q", raw))
	}
	bp := w*100 + f
	if bp > 1_000_000 {
		return 0, fieldErr(path, fmt.Sprintf("%q is more than 10,000%%", raw))
	}
	return bp, nil
}
