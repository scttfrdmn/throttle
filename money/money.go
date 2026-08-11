// Package money represents currency amounts as signed integer microdollars.
//
// Floating-point values never enter accounting or persistence. Every arithmetic
// helper here is overflow-checked: an int64 microdollar can hold roughly
// ±9.2 trillion dollars, so overflow is not reachable with realistic budgets,
// but silently wrapping a spend total is a corruption bug rather than a
// rounding inconvenience, so it is always detected instead.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Money is an amount in microdollars. One dollar is 1,000,000 units.
type Money int64

const (
	// PerDollar is the number of microdollars in one dollar.
	PerDollar Money = 1_000_000

	// Max and Min are the representable bounds.
	Max Money = math.MaxInt64
	Min Money = math.MinInt64
)

// ErrOverflow reports that an operation could not be represented in a Money.
var ErrOverflow = errors.New("money: arithmetic overflow")

// maxWholeDollars is the largest whole-dollar component Parse can represent.
const maxWholeDollars = uint64(math.MaxInt64) / uint64(PerDollar)

// Parse accepts ordinary dollar strings such as "4000", "4000.25", "$4,000.25",
// and "-12.500001", down to six decimal places.
//
// Parsing is deliberately strict. A malformed amount is rejected rather than
// coerced, because a silently misparsed budget is worse than a failed command.
func Parse(s string) (Money, error) {
	orig := s

	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")

	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg = true
		s = s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}

	// The currency symbol may appear before or after the sign ("-$5", "$-5").
	s = strings.TrimPrefix(s, "$")
	if !neg && strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}

	if s == "" {
		return 0, fmt.Errorf("money: empty value %q", orig)
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if strings.Contains(frac, ".") {
		return 0, fmt.Errorf("money: invalid value %q", orig)
	}
	if whole == "" && frac == "" {
		// Reject a bare "." with no digits on either side.
		return 0, fmt.Errorf("money: invalid value %q", orig)
	}
	if whole == "" {
		whole = "0"
	}
	if hasFrac && frac == "" {
		// Accept a trailing point ("5.") as zero fractional microdollars.
		frac = "0"
	}
	if len(frac) > 6 {
		return 0, fmt.Errorf("money: %q has more than six decimal places", orig)
	}

	// ParseUint with an explicit base rejects signs, underscores, spaces, and
	// any other non-digit, which is exactly the strictness wanted here.
	w, err := strconv.ParseUint(whole, 10, 64)
	if err != nil {
		// A well-formed but enormous digit string is an overflow, not a syntax error.
		if errors.Is(err, strconv.ErrRange) {
			return 0, fmt.Errorf("money: value %q overflows: %w", orig, ErrOverflow)
		}
		return 0, fmt.Errorf("money: invalid value %q", orig)
	}
	var f uint64
	if frac != "" {
		if f, err = strconv.ParseUint(frac, 10, 64); err != nil {
			return 0, fmt.Errorf("money: invalid value %q", orig)
		}
		// "5.5" means 500000 microdollars, not 5.
		for i := len(frac); i < 6; i++ {
			f *= 10
		}
	}

	if w > maxWholeDollars {
		return 0, fmt.Errorf("money: value %q overflows: %w", orig, ErrOverflow)
	}
	micro := w*uint64(PerDollar) + f
	if micro > uint64(math.MaxInt64) {
		return 0, fmt.Errorf("money: value %q overflows: %w", orig, ErrOverflow)
	}

	v := Money(micro)
	if neg {
		v = -v
	}
	return v, nil
}

// magnitude returns the absolute value of m as a uint64. Unlike -m this is
// correct for Min, whose absolute value does not fit in an int64.
func (m Money) magnitude() uint64 {
	if m < 0 {
		return -uint64(m)
	}
	return uint64(m)
}

func (m Money) sign() string {
	if m < 0 {
		return "-$"
	}
	return "$"
}

// String renders the exact amount with all six decimal places.
func (m Money) String() string {
	mag := m.magnitude()
	return fmt.Sprintf("%s%d.%06d", m.sign(), mag/uint64(PerDollar), mag%uint64(PerDollar))
}

// CentsString renders the amount rounded to cents, half away from zero.
//
// This is a lossy display helper. It must never be used to compute, compare, or
// persist an amount.
func (m Money) CentsString() string {
	mag := m.magnitude()
	const perCent = uint64(PerDollar) / 100
	cents := (mag + perCent/2) / perCent
	return fmt.Sprintf("%s%d.%02d", m.sign(), cents/100, cents%100)
}

// Add returns a+b, reporting false if the result overflows.
func Add(a, b Money) (Money, bool) {
	s := a + b
	// Overflow happened iff the operands share a sign that the result does not.
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s >= 0) {
		return 0, false
	}
	return s, true
}

// Sub returns a-b, reporting false if the result overflows.
func Sub(a, b Money) (Money, bool) {
	d := a - b
	if (b < 0 && d < a) || (b > 0 && d > a) {
		return 0, false
	}
	return d, true
}

// Sum adds every value, reporting false if any intermediate step overflows.
func Sum(vs ...Money) (Money, bool) {
	var total Money
	for _, v := range vs {
		var ok bool
		if total, ok = Add(total, v); !ok {
			return 0, false
		}
	}
	return total, true
}

// MulInt returns m*n, reporting false if the result overflows.
func MulInt(m Money, n int64) (Money, bool) {
	if m == 0 || n == 0 {
		return 0, true
	}
	p := m * Money(n)
	if p/Money(n) != m || (m == Min && n == -1) {
		return 0, false
	}
	return p, true
}
