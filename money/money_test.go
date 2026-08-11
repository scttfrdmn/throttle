package money

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	tests := map[string]Money{
		"4000":          4_000_000_000,
		"4000.25":       4_000_250_000,
		"$4,000.25":     4_000_250_000,
		"0.000001":      1,
		"-12.500001":    -12_500_001,
		"-$12.500001":   -12_500_001,
		"$-12.500001":   -12_500_001,
		"+5":            5_000_000,
		"5.":            5_000_000,
		".5":            500_000,
		"0":             0,
		"-0":            0,
		"  42.10  ":     42_100_000,
		"1.5":           1_500_000,
		"1.05":          1_050_000,
		"9223372036854": 9_223_372_036_854_000_000,
	}
	for in, want := range tests {
		got, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Parse(%q) = %d, want %d", in, int64(got), int64(want))
		}
	}
}

// TestParseRejectsMalformed covers the class of bug where a stray sign or
// non-digit inside the value silently produced a wrong amount. "1.-5" once
// parsed as $0.95.
func TestParseRejectsMalformed(t *testing.T) {
	bad := []string{
		"", "  ", "$", "-", "+", "-$", ".", "..",
		"1..2", "1.2.3", "abc", "1abc", "0x10", "1e6",
		"1.-5", "1.+5", "-1.-5", "1 000", "1_000", "$$5",
		"1.0000001", // sub-microdollar precision
		"--5", "5-", // misplaced signs
		"NaN", "Inf",
	}
	for _, in := range bad {
		if got, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %d, want error", in, int64(got))
		}
	}
}

func TestParseRejectsOverflow(t *testing.T) {
	// These once wrapped silently: "10000000000000" produced a negative amount.
	for _, in := range []string{
		"10000000000000",
		"9223372036855",
		"99999999999999999999999999",
		"-10000000000000",
		"9223372036854.775808", // one microdollar past MaxInt64
	} {
		got, err := Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) = %d, want overflow error", in, int64(got))
			continue
		}
		if !errors.Is(err, ErrOverflow) && !strings.Contains(err.Error(), "overflow") {
			t.Errorf("Parse(%q): error %v, want overflow", in, err)
		}
	}
}

func TestParseMaxRepresentable(t *testing.T) {
	got, err := Parse("9223372036854.775807")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != Max {
		t.Fatalf("got %d, want %d", int64(got), int64(Max))
	}
}

func TestParseRoundTrip(t *testing.T) {
	for _, v := range []Money{0, 1, -1, 999_999, 4_000_250_000, -12_500_001, Max, Min + 1} {
		got, err := Parse(v.String())
		if err != nil {
			t.Errorf("Parse(%q): %v", v.String(), err)
			continue
		}
		if got != v {
			t.Errorf("round trip of %d gave %d", int64(v), int64(got))
		}
	}
}

func TestString(t *testing.T) {
	tests := map[Money]string{
		0:             "$0.000000",
		1:             "$0.000001",
		-1:            "-$0.000001",
		999_999:       "$0.999999",
		1_000_000:     "$1.000000",
		4_000_250_000: "$4000.250000",
		-12_500_001:   "-$12.500001",
	}
	for in, want := range tests {
		if got := in.String(); got != want {
			t.Errorf("Money(%d).String() = %q, want %q", int64(in), got, want)
		}
	}
}

// TestStringExtremes covers the negation-overflow bug that rendered MinInt64 as
// the nonsense string "-$-9223372036854.-775808".
func TestStringExtremes(t *testing.T) {
	if got, want := Min.String(), "-$9223372036854.775808"; got != want {
		t.Errorf("Min.String() = %q, want %q", got, want)
	}
	if got, want := Max.String(), "$9223372036854.775807"; got != want {
		t.Errorf("Max.String() = %q, want %q", got, want)
	}
	if got, want := Min.CentsString(), "-$9223372036854.78"; got != want {
		t.Errorf("Min.CentsString() = %q, want %q", got, want)
	}
}

func TestCentsStringRoundsHalfAwayFromZero(t *testing.T) {
	tests := map[Money]string{
		0:         "$0.00",
		4_000:     "$0.00", // $0.004 rounds down
		5_000:     "$0.01", // $0.005 rounds away from zero
		-5_000:    "-$0.01",
		999_999:   "$1.00", // was "$0.99" under truncation
		-999_999:  "-$1.00",
		994_999:   "$0.99",
		1_005_000: "$1.01",
	}
	for in, want := range tests {
		if got := in.CentsString(); got != want {
			t.Errorf("Money(%d).CentsString() = %q, want %q", int64(in), got, want)
		}
	}
}

func TestAddSubOverflow(t *testing.T) {
	if _, ok := Add(Max, 1); ok {
		t.Error("Add(Max, 1) should overflow")
	}
	if _, ok := Add(Min, -1); ok {
		t.Error("Add(Min, -1) should overflow")
	}
	if v, ok := Add(Max, Min); !ok || v != -1 {
		t.Errorf("Add(Max, Min) = %d, %v; want -1, true", int64(v), ok)
	}
	if _, ok := Sub(Min, 1); ok {
		t.Error("Sub(Min, 1) should overflow")
	}
	if _, ok := Sub(Max, -1); ok {
		t.Error("Sub(Max, -1) should overflow")
	}
	if v, ok := Sub(100, 40); !ok || v != 60 {
		t.Errorf("Sub(100, 40) = %d, %v; want 60, true", int64(v), ok)
	}
}

func TestSum(t *testing.T) {
	if v, ok := Sum(1, 2, 3); !ok || v != 6 {
		t.Errorf("Sum(1,2,3) = %d, %v", int64(v), ok)
	}
	if _, ok := Sum(Max, 1, -1); ok {
		t.Error("Sum should report intermediate overflow")
	}
	if v, ok := Sum(); !ok || v != 0 {
		t.Errorf("Sum() = %d, %v; want 0, true", int64(v), ok)
	}
}

func TestMulInt(t *testing.T) {
	if v, ok := MulInt(5*PerDollar, 3); !ok || v != 15*PerDollar {
		t.Errorf("MulInt = %d, %v", int64(v), ok)
	}
	if v, ok := MulInt(Max, 0); !ok || v != 0 {
		t.Errorf("MulInt(Max, 0) = %d, %v; want 0, true", int64(v), ok)
	}
	if _, ok := MulInt(Max, 2); ok {
		t.Error("MulInt(Max, 2) should overflow")
	}
	if _, ok := MulInt(Min, -1); ok {
		t.Error("MulInt(Min, -1) should overflow")
	}
}

func TestNoFloatDrift(t *testing.T) {
	// A microdollar amount that float64 cannot represent exactly must survive.
	const in = "9007199.254741"
	v, err := Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := Money(9_007_199_254_741); v != want {
		t.Fatalf("got %d, want %d", int64(v), int64(want))
	}
	if got := float64(v); got == math.Trunc(got) && Money(got) != v {
		t.Fatal("value silently changed via float64")
	}
}
