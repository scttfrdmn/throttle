// Package usage is the provider-neutral vocabulary for what a request consumed
// and who served it.
//
// The boundary this package exists to enforce:
//
//	provider response -> normalized Usage -> pricing -> money.Money
//
// The budget engine sees money and scopes. It never sees tokens, because a
// budget does not care whether a dollar was spent on tokens, images, or seconds
// of audio. Provider adapters own the left-hand side of that arrow and must not
// reach past pricing.
package usage

import (
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// Dimension names a billable unit.
//
// The common text-model dimensions are named constants so that adapters agree on
// spelling, but Dimension is a string rather than an enum: a provider that bills
// for something throttle has never heard of must still be representable, or the
// accounting silently drops a real charge. Prefer an existing constant; invent a
// name only when the provider genuinely bills a new unit.
type Dimension string

// Text-model dimensions, common enough to be shared vocabulary.
const (
	InputTokens     Dimension = "input_tokens"
	OutputTokens    Dimension = "output_tokens"
	ReasoningTokens Dimension = "reasoning_tokens"

	// CacheReadTokens is input served from a prompt cache, normally cheaper than
	// fresh input. CacheWriteTokens is input written into the cache, normally
	// dearer. They are separate dimensions because they carry separate prices.
	CacheReadTokens  Dimension = "cache_read_tokens"
	CacheWriteTokens Dimension = "cache_write_tokens"

	// CacheWrite5mTokens and CacheWrite1hTokens are cache writes whose lifetime the
	// provider prices differently: a five-minute entry and a one-hour entry are
	// written by the same request and billed at different multiples of the input
	// rate.
	//
	// They are separate dimensions from CacheWriteTokens for the reason every
	// dimension here is separate: the price differs. A provider that distinguishes
	// them authoritatively and an adapter that folded them together would charge one
	// lifetime's rate for the other's tokens, and a request mixing the two would be
	// wrong in a direction nobody could recover from the record.
	//
	// A provider that reports cache writes without distinguishing their lifetime
	// keeps using CacheWriteTokens. These are for the case where the response itself
	// decomposes the total, and an adapter must never guess which one applies from
	// the request's cache_control syntax: what was requested and what was written
	// are different facts.
	CacheWrite5mTokens Dimension = "cache_write_5m_tokens"
	CacheWrite1hTokens Dimension = "cache_write_1h_tokens"

	// InputAudioTokens and OutputAudioTokens are audio carried through a
	// token-billed multimodal model: the provider reports a token count, not a
	// duration, and prices it per token like text -- but at its own rate, which is
	// commonly several times the text rate in each direction.
	//
	// They are distinct from AudioSeconds, which belongs to models genuinely
	// documented as duration-billed. Substituting one for the other would either
	// invent a duration the provider never reported or charge audio at a text rate;
	// both are exactly the sort of quiet mispricing this package exists to prevent.
	//
	// They are distinct from InputTokens and OutputTokens for the same reason
	// CacheReadTokens is: a separate price means a separate dimension. An adapter
	// that folded audio into the text counts would charge 6-13x too little on the
	// providers observed so far, and would do so invisibly.
	InputAudioTokens  Dimension = "input_audio_tokens"
	OutputAudioTokens Dimension = "output_audio_tokens"
)

// Dimensions beyond text, named here so the first adapter to need one does not
// have to guess at spelling. Nothing in v0.1 emits these.
const (
	Requests     Dimension = "requests"
	Images       Dimension = "images"
	AudioSeconds Dimension = "audio_seconds"
	VideoSeconds Dimension = "video_seconds"
	Searches     Dimension = "searches"
	ToolCalls    Dimension = "tool_calls"
)

// Hosted-runtime resource dimensions: compute consumed by a platform that runs
// agent code, billed by resource-time rather than by tokens.
//
// # The canonical-unit invariant
//
// Every dimension has one canonical integer atomic unit, fixed where the dimension
// is defined. These two are the first dimensions whose provider reports a decimal
// rather than a count, so they are where that invariant gets tested — and it holds
// without introducing a float. The provider's decimal is converted exactly, at a
// declared scale, by Nano.
//
// The scale is one billionth of the unit the provider prices in, rather than a
// physically atomic unit such as a byte-second. That is deliberate: a byte-second
// would require throttle to decide whether a "GB-hour" means 10^9 or 2^30 bytes,
// which providers routinely leave implicit. Guessing wrong would scale every
// memory charge by several percent, permanently, inside the durable unit. Nano-units
// of the provider's own unit never need to know: they preserve the provider's
// meaning verbatim and leave the definition where it belongs, with whoever bills.
const (
	// RuntimeVCPUNanoHours is virtual-CPU time, in billionths of a vCPU-hour.
	//
	// Providers commonly bill this for active processing only, excluding time the
	// agent spends waiting on a model or a tool. It must therefore never be derived
	// from wall-clock invocation or session duration: an adapter that inferred CPU
	// from elapsed time would invent a charge, and inventing charges is the one thing
	// this package exists to prevent.
	RuntimeVCPUNanoHours Dimension = "runtime_vcpu_nano_hours"

	// RuntimeMemoryNanoGBHours is memory reservation over time, in billionths of a
	// GB-hour.
	//
	// Commonly billed for a whole session -- including start-up, idle periods, and
	// platform overhead -- rather than for active processing. A meaningful share of
	// it therefore belongs to no individual request, which is why it is normalized
	// as observed and never apportioned here.
	RuntimeMemoryNanoGBHours Dimension = "runtime_memory_nano_gb_hours"
)

// NanoScale is the denominator of a nano-unit dimension: the atomic unit is one
// NanoScale-th of the unit the provider quotes a price in.
const NanoScale = 1_000_000_000

// ErrNotDecimal reports a quantity that is not a decimal number at all.
var ErrNotDecimal = errors.New("usage: quantity is not a decimal number")

// Nano converts a provider-reported decimal quantity into nano-units exactly,
// truncating toward zero.
//
// The input is a string rather than a float64 because that is how a provider
// actually delivers it -- in JSON, as decimal text -- and parsing it into a float
// first would introduce a representation error before any accounting happened.
// math/big does the conversion, so the only loss is the declared truncation.
//
// # On truncating
//
// Digits below a nano-unit are dropped rather than rounded, so a normalized
// quantity never exceeds what the provider reported. Rounding up could make
// throttle's figure larger than the provider's own telemetry, which is the wrong
// direction for a number a provider may itself describe as approximate.
//
// The residual is bounded by one nano-unit per observation. At the rates hosted
// runtimes charge, that is on the order of a ten-billionth of a cent -- orders of
// magnitude below the precision providers claim for this telemetry, which is the
// honest way to pick a quantization: fine enough to disappear into the source's own
// stated error.
//
// Truncation is toward zero rather than downward, so the same "never exceed what was
// reported" property holds for a negative quantity: -0.0000000015 becomes one
// nano-unit, not two.
//
// # Limitation
//
// Exact parsing accepts everything big.Rat does, which includes hexadecimal and
// rational spellings ("0x10", "1/4") that no provider emits. A malformed field that
// happens to look like one converts rather than erroring. Tightening this to strict
// decimal would change a public semantic, so it is recorded here instead; see
// TestNanoAcceptsHexAsAKnownLimitation.
func Nano(decimal string) (int64, error) {
	s := strings.TrimSpace(decimal)
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrNotDecimal)
	}
	// SetString accepts decimal and rational forms exactly, and rejects the float
	// spellings -- Inf, NaN -- that have no place in an accounting quantity.
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrNotDecimal, decimal)
	}
	r.Mul(r, new(big.Rat).SetInt64(NanoScale))
	// Quo truncates toward zero for both signs, which is what "never exceed what
	// the provider reported" requires of a negative quantity too.
	n := new(big.Int).Quo(r.Num(), r.Denom())
	if !n.IsInt64() {
		return 0, fmt.Errorf("usage: %s nano-units overflows int64", decimal)
	}
	return n.Int64(), nil
}

// Usage is a normalized count per billable dimension.
//
// A map rather than a struct of named integers: a provider that bills for a new
// unit needs no change here, and pricing can iterate exactly the dimensions
// actually present. An absent dimension means "not consumed", which is distinct
// from a dimension the provider reported as zero.
type Usage struct {
	dims map[Dimension]int64
}

// New builds usage from a set of dimension counts.
func New(pairs map[Dimension]int64) Usage {
	if len(pairs) == 0 {
		return Usage{}
	}
	u := Usage{dims: make(map[Dimension]int64, len(pairs))}
	for d, v := range pairs {
		u.dims[d] = v
	}
	return u
}

// Set records a count, replacing any previous value for the dimension.
func (u *Usage) Set(d Dimension, n int64) {
	if u.dims == nil {
		u.dims = make(map[Dimension]int64, 4)
	}
	u.dims[d] = n
}

// Add accumulates a count, which is how a multi-step operation — an agent turn
// invoking several models — aggregates into one record.
func (u *Usage) Add(d Dimension, n int64) {
	if u.dims == nil {
		u.dims = make(map[Dimension]int64, 4)
	}
	u.dims[d] += n
}

// Get returns the count and whether the dimension was reported at all. The
// second result is the difference between "the provider said zero" and "the
// provider never mentioned it", which matters when deciding whether a missing
// price is a problem.
func (u Usage) Get(d Dimension) (int64, bool) {
	n, ok := u.dims[d]
	return n, ok
}

// Count returns the count, or zero if the dimension was not reported.
func (u Usage) Count(d Dimension) int64 { return u.dims[d] }

// Empty reports whether nothing at all was recorded.
func (u Usage) Empty() bool { return len(u.dims) == 0 }

// Dimensions returns the reported dimensions in a stable order, so output and
// persistence do not shuffle between runs.
func (u Usage) Dimensions() []Dimension {
	out := make([]Dimension, 0, len(u.dims))
	for d := range u.dims {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// All returns a copy of every reported dimension.
func (u Usage) All() map[Dimension]int64 {
	out := make(map[Dimension]int64, len(u.dims))
	for d, n := range u.dims {
		out[d] = n
	}
	return out
}

// Merge returns the sum of two usages, dimension by dimension.
func Merge(a, b Usage) Usage {
	out := Usage{dims: make(map[Dimension]int64, len(a.dims)+len(b.dims))}
	for d, n := range a.dims {
		out.dims[d] = n
	}
	for d, n := range b.dims {
		out.dims[d] += n
	}
	return out
}

// TotalTokens sums the token-denominated dimensions, for display only. Pricing
// must never use it: the dimensions carry different prices, so a single total
// cannot be costed.
func (u Usage) TotalTokens() int64 {
	var n int64
	for _, d := range []Dimension{
		InputTokens, OutputTokens, ReasoningTokens,
		CacheReadTokens, CacheWriteTokens, CacheWrite5mTokens, CacheWrite1hTokens,
	} {
		n += u.dims[d]
	}
	return n
}

func (u Usage) String() string {
	if u.Empty() {
		return "no usage recorded"
	}
	s := ""
	for i, d := range u.Dimensions() {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%s=%d", d, u.dims[d])
	}
	return s
}

// Quality states how much an estimate can be trusted. An estimate that overstates
// its own accuracy is worse than an obviously rough one, because a caller cannot
// compensate for a lie.
type Quality string

const (
	// QualityUnknown means no estimate could be formed at all.
	QualityUnknown Quality = "unknown"

	// QualityExact means the provider stated the billable quantity before the
	// call. Reserved for genuinely exact counts: for a text generation this
	// cannot apply to output, since nobody knows how much a model will emit
	// before it emits it.
	QualityExact Quality = "exact"

	// QualityConservative means the estimate is bounded above — actual usage
	// should not exceed it, e.g. exact input tokens plus the output cap the
	// caller set. Safe to reserve against.
	QualityConservative Quality = "conservative"

	// QualityHeuristic means the estimate is an informed guess, e.g. characters
	// divided by an average token length. Actual usage may exceed it.
	QualityHeuristic Quality = "heuristic"

	// QualityHistorical means the estimate came from observed past cost for
	// similar work. Not produced in v0.1.
	QualityHistorical Quality = "historical"
)

// Estimate is what a request is predicted to consume, before it runs.
type Estimate struct {
	Identity ModelIdentity
	Usage    Usage

	// Cost may be unknown even when Usage is fully known.
	Cost Cost

	// Quality states how far the estimate can be trusted.
	Quality Quality

	// Note explains anything the caller should know, e.g. that an output cap was
	// assumed because the request set none.
	Note string
}

// Actual is what a request really consumed, observed after it ran.
type Actual struct {
	Identity ModelIdentity
	Usage    Usage

	// Cost may be unknown even after a successful call: the provider reports
	// usage, throttle prices it, and the catalog may not know this model.
	Cost Cost

	// ProviderLatency is the latency the provider itself reported, when
	// available. It is not the caller's observed wall clock, which also includes
	// transport and retries.
	ProviderLatency time.Duration
}
