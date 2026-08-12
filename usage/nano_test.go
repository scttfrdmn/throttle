package usage_test

import (
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// Nano's quantization is a public semantic, not an implementation detail: it decides
// what a provider's decimal telemetry becomes in the canonical integer unit every
// later stage prices from. These tests pin the behaviour so that changing it has to
// be a deliberate decision rather than a refactor, and record why truncation is the
// direction chosen.

// TestNanoConvertsExactly covers the quantities that land on a nano-unit boundary,
// where there is nothing to round and the conversion should be exact.
func TestNanoConvertsExactly(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1", usage.NanoScale},
		{"2.5", 2_500_000_000},
		{"0.000000001", 1},             // exactly one nano-unit
		{"0.5", 500_000_000},           // a half unit
		{"1.234567891", 1_234_567_891}, // nine decimal places, the full resolution
		{"-1.5", -1_500_000_000},       // a negative quantity is representable
		{"9223372036.854775807", 9_223_372_036_854_775_807}, // the largest that fits
	}
	for _, tc := range cases {
		got, err := usage.Nano(tc.in)
		if err != nil {
			t.Errorf("Nano(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Nano(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestNanoTruncatesTowardZero is the documented rounding behaviour: digits finer
// than a nano-unit are dropped, not rounded.
//
// The direction is deliberate. Truncating means a normalized quantity never exceeds
// what the provider reported, so throttle's figure cannot come out larger than the
// provider's own telemetry — the wrong direction for a number the provider itself
// describes as approximate. The cost is a systematic understatement bounded by one
// nano-unit per observation.
//
// Note the negative cases: truncation toward zero, rather than floor, keeps that
// "never exceed what was reported" property for a negative quantity too. A floor
// would push -0.0000000015 to -2 nano-units, which is further from zero than the
// provider said.
func TestNanoTruncatesTowardZero(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		why  string
	}{
		{"0.0000000015", 1, "a half nano-unit is dropped, not rounded up"},
		{"0.0000000019", 1, "nor is nine tenths of one"},
		{"0.0000000001", 0, "a tenth of a nano-unit becomes nothing"},
		{"0.00000000099999", 0, "and so does very nearly a whole one"},
		{"1.9999999999", 1_999_999_999, "the residual is under one nano-unit even at scale"},
		{"-0.0000000015", -1, "toward zero, not floor"},
		{"-0.0000000019", -1, "still toward zero"},
		{"-1.9999999999", -1_999_999_999, "and at scale"},
	}
	for _, tc := range cases {
		got, err := usage.Nano(tc.in)
		if err != nil {
			t.Errorf("Nano(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Nano(%q) = %d, want %d (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}

// TestNanoResidualIsBoundedByOneUnit states the limitation as an invariant rather
// than as prose: whatever is dropped, the result is never above the reported
// quantity and never more than one nano-unit below it.
func TestNanoResidualIsBoundedByOneUnit(t *testing.T) {
	inputs := []string{
		"0", "0.1", "0.0000000001", "0.0000000019", "1", "1.5",
		"3.141592653589793", "1000000.000000000999", "-2.7182818284",
	}
	one := big.NewRat(1, 1)
	for _, in := range inputs {
		got, err := usage.Nano(in)
		if err != nil {
			t.Errorf("Nano(%q): %v", in, err)
			continue
		}
		exact, ok := new(big.Rat).SetString(in)
		if !ok {
			t.Fatalf("test input %q is not a decimal", in)
		}
		exact.Mul(exact, new(big.Rat).SetInt64(usage.NanoScale))
		diff := new(big.Rat).Sub(exact, new(big.Rat).SetInt64(got))

		// Positive quantities lose a little; negative ones gain a little, because
		// truncation moves both toward zero. Either way the magnitude is under one.
		if new(big.Rat).Abs(diff).Cmp(one) >= 0 {
			t.Errorf("Nano(%q) = %d, which is off by %s nano-units: more than the declared one-unit residual",
				in, got, diff.FloatString(12))
		}
		if exact.Sign() >= 0 && diff.Sign() < 0 {
			t.Errorf("Nano(%q) = %d, which exceeds the reported quantity; truncation must never round up", in, got)
		}
	}
}

// TestNanoRejectsNonDecimals covers the inputs that are not accounting quantities at
// all. Float spellings are rejected rather than coerced: a usage figure of NaN is a
// bug in the caller or a surprise in the provider's telemetry, and either way
// silently turning it into a number would put a fiction into the ledger.
func TestNanoRejectsNonDecimals(t *testing.T) {
	for _, in := range []string{"", "   ", "abc", "NaN", "Inf", "+Inf", "-Inf", "1.2.3", "1e", "1,5"} {
		if got, err := usage.Nano(in); !errors.Is(err, usage.ErrNotDecimal) {
			t.Errorf("Nano(%q) = %d, %v; want ErrNotDecimal", in, got, err)
		}
	}
}

// TestNanoAcceptsHexAsAKnownLimitation records a behaviour rather than endorsing it.
//
// The underlying exact parser accepts big.Rat's full input vocabulary, which includes
// hexadecimal and rational spellings no provider emits. Tightening it to strict
// decimal would be a change to a public semantic, so it is documented and left: the
// consequence is that a malformed field which happens to look like hex converts
// instead of erroring. Real telemetry does not produce these, and a caller passing
// one has a bug that this function is the wrong place to catch.
func TestNanoAcceptsHexAsAKnownLimitation(t *testing.T) {
	got, err := usage.Nano("0x10")
	if err != nil {
		// If this ever starts erroring, that is a deliberate tightening: update the
		// doc comment on Nano to match.
		t.Skipf("Nano now rejects hex, which is a tightening rather than a regression: %v", err)
	}
	if got != 16*usage.NanoScale {
		t.Errorf("Nano(%q) = %d, want %d as the documented consequence of exact parsing",
			"0x10", got, 16*usage.NanoScale)
	}
	// The rational spelling is accepted for the same reason.
	if got, err := usage.Nano("1/4"); err != nil || got != 250_000_000 {
		t.Errorf("Nano(%q) = %d, %v; want 250000000", "1/4", got, err)
	}
}

// TestNanoAcceptsProviderSpellings covers the forms telemetry actually arrives in.
// Exponent notation is the one that matters: a serializer emitting 1.5e-3 for a
// small quantity is common, and rejecting it would lose real usage.
func TestNanoAcceptsProviderSpellings(t *testing.T) {
	cases := map[string]int64{
		"1.5e-3":  1_500_000,
		"1.5E-3":  1_500_000,
		"2e0":     2 * usage.NanoScale,
		"1e-9":    1,
		"1e-10":   0, // below the quantization, and truncated to nothing
		" 0.25 ":  250_000_000,
		"+0.25":   250_000_000,
		"0.250":   250_000_000,
		"00.2500": 250_000_000,
	}
	for in, want := range cases {
		got, err := usage.Nano(in)
		if err != nil {
			t.Errorf("Nano(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Nano(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestNanoOverflowIsAnError checks the upper bound. A quantity too large for the
// canonical unit is refused rather than wrapped: a silently negative usage count
// would price as a credit.
func TestNanoOverflowIsAnError(t *testing.T) {
	for _, in := range []string{"9223372036.854775808", "10000000000", "-10000000000", "1e30"} {
		got, err := usage.Nano(in)
		if err == nil {
			t.Errorf("Nano(%q) = %d, want an overflow error", in, got)
			continue
		}
		if errors.Is(err, usage.ErrNotDecimal) {
			t.Errorf("Nano(%q) reported a parse error for an overflow: %v", in, err)
		}
		if !strings.Contains(err.Error(), "overflow") {
			t.Errorf("Nano(%q) error does not name the overflow: %v", in, err)
		}
	}
	// One nano-unit further down, the boundary value itself converts. Covered in
	// TestNanoConvertsExactly; named here so the two halves of the boundary are
	// findable together.
}

// TestNanoUsesNoFloat64 is the property that motivates the string input. A decimal
// that no float64 can represent must survive exactly, which it cannot do if the
// value passes through a float on the way in.
func TestNanoUsesNoFloat64(t *testing.T) {
	// 0.1 is not representable in binary floating point; the nearest float64 is
	// slightly above it. Nine of these places must still come out exact.
	got, err := usage.Nano("0.100000000")
	if err != nil {
		t.Fatalf("Nano: %v", err)
	}
	if got != 100_000_000 {
		t.Fatalf("Nano(\"0.100000000\") = %d, want exactly 100000000", got)
	}

	// A quantity whose ninth decimal place is beyond float64's ~15-17 significant
	// digits when combined with a large integer part. Through a float64 this would
	// lose the tail; exactly, it does not.
	got, err = usage.Nano("123456789.123456789")
	if err != nil {
		t.Fatalf("Nano: %v", err)
	}
	if got != 123_456_789_123_456_789 {
		t.Errorf("Nano lost precision: got %d, want 123456789123456789", got)
	}
}

// TestNanoRoundTripsThroughPricing is the end-to-end reason the quantization is set
// where it is: a nano-unit quantity priced by a per-nano-unit rate has to come out
// as an exact integer amount of money, with rounding happening once, in pricing.
func TestNanoRoundTripsThroughPricing(t *testing.T) {
	// 0.5 vCPU-hours, reported as decimal text the way telemetry delivers it.
	n, err := usage.Nano("0.5")
	if err != nil {
		t.Fatalf("Nano: %v", err)
	}
	u := usage.New(map[usage.Dimension]int64{usage.RuntimeVCPUNanoHours: n})

	// $0.08 per vCPU-hour, expressed as a rate over the canonical nano-unit.
	q := pricing.CapturedQuote{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "runtime",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.RuntimeVCPUNanoHours: pricing.PerNanoUnit(usage.RuntimeVCPUNanoHours, money.Money(80_000)),
		},
		Provenance: pricing.Provenance{Source: "test", Version: "v1"},
	}
	priced, err := q.Price(u)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if !priced.Cost.Known() {
		t.Fatalf("cost = %v, want a known amount", priced.Cost)
	}
	// Half of $0.08, exactly, with no float64 anywhere on the path.
	if priced.Cost.Amount != money.Money(40_000) {
		t.Errorf("cost = %s (%d microdollars), want $0.04",
			priced.Cost.Amount.CentsString(), int64(priced.Cost.Amount))
	}
}

// TestNanoDimensionsAreProviderNeutral pins the naming. The canonical runtime
// dimensions describe a resource, not a vendor: a second platform billing CPU-time
// prices against the same dimension, and putting a vendor name in a generic unit
// would fork the pricing catalog by provider for no reason.
func TestNanoDimensionsAreProviderNeutral(t *testing.T) {
	for _, d := range []usage.Dimension{usage.RuntimeVCPUNanoHours, usage.RuntimeMemoryNanoGBHours} {
		name := strings.ToLower(string(d))
		for _, vendor := range []string{"aws", "amazon", "bedrock", "agentcore", "azure", "gcp", "openai", "anthropic"} {
			if strings.Contains(name, vendor) {
				t.Errorf("dimension %q names the vendor %q; generic usage dimensions must stay provider-neutral", d, vendor)
			}
		}
		// The unit is in the name, so a reader of a stored integer can tell what it
		// counts without consulting the code that wrote it.
		if !strings.Contains(name, "nano") || !strings.Contains(name, "hours") {
			t.Errorf("dimension %q does not name its canonical unit, so a reader cannot tell what an integer count means", d)
		}
	}
}
