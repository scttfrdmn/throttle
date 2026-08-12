// Package provider holds what is common to provider adapters.
//
// An adapter's job is narrow and entirely one-directional: translate a provider's
// own request and response into throttle's normalized vocabulary, and never the
// other way around. Adapters import provider SDKs; nothing that imports an
// adapter's SDK types belongs in budget, ledger, money, or pricing.
//
// Deliberately absent: a generic LLM client interface. Adapters expose their
// provider's own types, because a caller who wants Bedrock's Converse should get
// Bedrock's Converse, with governance wrapped around it rather than a
// lowest-common-denominator API in front of it. That is why there is no
// Client interface here for adapters to implement.
package provider

import (
	"context"
	"time"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/usage"
)

// Estimator predicts what a request will consume, before it runs.
//
// The request type is the provider's own, so this is a per-adapter interface
// parameterized by the caller rather than a shared one. It is stated here to fix
// the contract every adapter honors: an estimate reports its own quality, and an
// estimate that cannot be priced returns a known usage with an explicitly unknown
// cost rather than a guess.
type Estimator[Request any] interface {
	Estimate(ctx context.Context, request Request) (usage.Estimate, error)
}

// Observer normalizes a provider response into observed usage.
//
// It must report what the provider actually said, including dimensions throttle
// does not recognize, and must not price anything: pricing is a separate step with
// its own provenance.
type Observer[Response any] interface {
	Observe(ctx context.Context, response Response) (usage.Actual, error)
}

// Activity is the durable record of one governed request.
//
// It is defined here, rather than in an adapter, because every provider produces
// the same shape and the dashboard should not care which one served a request.
// Unknown canonical identity and unknown cost are legitimate states, not errors.
//
// Prompt and response content is deliberately absent. Recording what a request
// cost does not require recording what it said, and defaulting to storing content
// would make throttle a transcript store, which it is not.
type Activity struct {
	RequestID     string
	ReservationID string
	BudgetID      string

	// Identity is what was called. CanonicalModel may be empty.
	Identity usage.ModelIdentity

	// Estimate is what was predicted, including its quality, retained so estimate
	// accuracy can be measured against Usage rather than assumed.
	Estimate usage.Estimate

	// Reserved is the amount actually held, which can differ from the estimate.
	Reserved money.Money

	// Usage is what the provider reported, and Cost is that usage priced. Cost may
	// be unknown while Usage is fully known.
	Usage usage.Usage
	Cost  usage.Cost

	// EnforcementMode is the posture that actually governed the request -- the
	// strictest in the budget chain. Recorded because spend admitted under monitor
	// mode was never subject to a ceiling, and a reader cannot infer that later.
	EnforcementMode string

	// Outcome states how the request ended: admitted and settled, denied, released,
	// or left outstanding.
	Outcome string

	// Error is the provider or accounting error, when there was one.
	Error string

	StartedAt       time.Time
	CompletedAt     time.Time
	Latency         time.Duration
	ProviderLatency time.Duration

	Metadata map[string]string
}
