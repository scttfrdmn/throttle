// Package bedrock adapts the AWS Bedrock Runtime Converse API to throttle's
// accounting model.
//
// It is a shim, not a framework. A caller builds a *bedrockruntime.ConverseInput
// exactly as they would for the AWS SDK, hands it to Client.Converse with a budget
// ID, and gets back the SDK's own *bedrockruntime.ConverseOutput. Nothing about
// the request or response is reinterpreted, and no throttle abstraction stands
// between the caller and the model.
//
// What the shim adds is the transaction around the call:
//
//	estimate -> reserve -> execute -> reconcile
//
// # The boundary
//
// AWS types stop here. This package converts a Bedrock response into
// usage.Usage and usage.ModelIdentity, prices it through a pricing.Catalog, and
// hands money.Money to the engine. Nothing in budget, ledger, money, or pricing
// imports an AWS type, which is what keeps the budget engine provider-neutral and
// lets a second provider reuse all of it.
//
// # Credentials
//
// This package's tests never require AWS credentials. Client depends on a narrow
// ConverseAPI interface rather than on *bedrockruntime.Client, so the whole
// transaction is exercised against a fake. The one test that talks to AWS is
// explicitly opt-in.
package bedrock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"throttle/activity"
	"throttle/engine"
	"throttle/ledger"
	"throttle/money"
	"throttle/pricing"
	"throttle/usage"
)

// ConverseAPI is the slice of the Bedrock Runtime client this adapter uses.
//
// A consumer-defined interface rather than *bedrockruntime.Client: it documents
// exactly which calls throttle makes, and it makes the entire governed path
// testable without credentials, a network, or a mocking framework.
// *bedrockruntime.Client satisfies it.
type ConverseAPI interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// TokenCounterAPI is the optional preflight token count. It is separate from
// ConverseAPI because it is a distinct, billable extra round trip that a caller
// may reasonably decline: a client without it still works, with a weaker estimate.
type TokenCounterAPI interface {
	CountTokens(ctx context.Context, in *bedrockruntime.CountTokensInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.CountTokensOutput, error)
}

// Errors returned by this package.
var (
	// ErrNoClient means the adapter was built without a Bedrock client.
	ErrNoClient = errors.New("bedrock: no Converse client configured")

	// ErrProvider wraps an error returned by Bedrock itself, so a caller can tell a
	// provider failure from a budget refusal or an accounting failure.
	ErrProvider = errors.New("bedrock: provider call failed")

	// ErrAccounting means the provider call succeeded but throttle could not record
	// it. The response is still returned: the caller got their answer, and hiding
	// the bookkeeping failure would be worse than reporting it.
	ErrAccounting = errors.New("bedrock: request succeeded but could not be recorded")

	// ErrOutcomeUnknown means the call was interrupted in a way that leaves it
	// genuinely unknown whether Bedrock served and billed the request. The
	// reservation is deliberately left outstanding; see Client.Converse.
	ErrOutcomeUnknown = errors.New("bedrock: request outcome is unknown")

	// ErrCostUnresolved means the request ran and its usage is known, but its cost
	// is not fully priceable. The usage is recorded and the hold stays encumbered
	// awaiting reconciliation; the response is still returned.
	ErrCostUnresolved = errors.New("bedrock: request cost is unresolved")
)

// DefaultMaxOutputTokens bounds the output half of an estimate when the caller
// sets no MaxTokens.
//
// Some ceiling is required: output tokens are unknowable before generation, so
// without a cap there is no upper bound to reserve against. This value is a
// throttle-side assumption, reported as such in Estimate.Note, and never presented
// as something the provider stated.
const DefaultMaxOutputTokens = 4096

// Config configures a Client.
type Config struct {
	// Client is the Bedrock Runtime client. Required.
	Client ConverseAPI

	// StreamClient enables governed ConverseStream calls. Optional: a caller who
	// only makes non-streaming calls needs nothing here.
	//
	// Wrap a real client with Streaming(c) to satisfy it. It is a separate field
	// because *bedrockruntime.Client cannot satisfy ConverseStreamAPI directly --
	// the SDK's ConverseStreamOutput hides its event stream behind GetStream.
	StreamClient ConverseStreamAPI

	// AgentClient enables governed Agents Classic InvokeAgent calls. Optional: a
	// caller who only invokes models directly needs nothing here.
	//
	// Wrap a real *bedrockagentruntime.Client with Agent(c) to satisfy it. It is a
	// separate field for two reasons: it is a different AWS service client, and like
	// ConverseStreamOutput, InvokeAgentOutput hides its event stream behind GetStream.
	AgentClient InvokeAgentAPI

	// RuntimeClient enables governed AgentCore InvokeAgentRuntime calls. Optional,
	// and needed only for the outer edge of a hosted agent: code running inside an
	// AgentCore runtime governs its own model calls through Client and Engine like
	// any other caller, and needs nothing here.
	//
	// *bedrockagentcore.Client satisfies it directly -- no wrapper, because
	// InvokeAgentRuntimeOutput.Response is a plain io.ReadCloser rather than an event
	// stream behind an unexported field.
	RuntimeClient RuntimeAPI

	// Counter enables preflight input token counting via the Bedrock CountTokens
	// API. Optional: it is an extra billable round trip per request, so a caller
	// opts in. Without it, input is estimated heuristically.
	//
	// *bedrockruntime.Client satisfies this, so passing the same client for both
	// fields is the normal way to enable it.
	Counter TokenCounterAPI

	// Engine governs the spend. Required.
	Engine *engine.Engine

	// Catalog prices usage. Required: without it throttle cannot convert tokens to
	// money, and guessing is not an option.
	//
	// If it also implements pricing.RateSource -- *pricing.Static does -- the
	// adapter captures an immutable quote at admission and settles against that,
	// so a catalog update mid-request cannot change the request's accounting basis.
	Catalog pricing.Catalog

	// Activity records governed requests durably. Optional: without it the
	// transaction still works, but a request whose process dies mid-call leaves no
	// trace, and unresolved costs cannot be reconciled later.
	//
	// A recording failure never fails a provider call. The caller has already paid
	// for the response; withholding it because telemetry is unavailable would be
	// the wrong trade.
	Activity activity.Store

	// Region is recorded on the identity and used for pricing. It should match the
	// region the client is configured for; the SDK does not expose it.
	Region string

	// MaxOutputTokens overrides DefaultMaxOutputTokens for estimating requests that
	// set no cap of their own.
	MaxOutputTokens int32

	// StreamStallTimeout bounds how long a governed stream waits for the caller to
	// accept a single event before treating the stream as abandoned.
	//
	// It exists so a caller who neither reads, closes, nor cancels cannot pin a
	// goroutine and renew a hold indefinitely. Zero means the reservation lease,
	// which is generous by construction: a caller slower than the lease is about to
	// have its hold reclaimed regardless.
	StreamStallTimeout time.Duration
}

// Client is a governed Bedrock Runtime client.
type Client struct {
	api         ConverseAPI
	streamAPI   ConverseStreamAPI
	agentAPI    InvokeAgentAPI
	runtimeAPI  RuntimeAPI
	counter     TokenCounterAPI
	engine      *engine.Engine
	catalog     pricing.Catalog
	rates       pricing.RateSource
	activity    activity.Store
	region      string
	maxOut      int32
	streamStall time.Duration
}

// New builds a governed client.
func New(cfg Config) (*Client, error) {
	if cfg.Client == nil {
		return nil, ErrNoClient
	}
	if cfg.Engine == nil {
		return nil, errors.New("bedrock: an engine is required")
	}
	if cfg.Catalog == nil {
		return nil, errors.New("bedrock: a pricing catalog is required")
	}
	if cfg.MaxOutputTokens < 0 {
		return nil, errors.New("bedrock: max output tokens cannot be negative")
	}
	if cfg.StreamStallTimeout < 0 {
		return nil, errors.New("bedrock: stream stall timeout cannot be negative")
	}
	maxOut := cfg.MaxOutputTokens
	if maxOut == 0 {
		maxOut = DefaultMaxOutputTokens
	}
	c := &Client{
		api:         cfg.Client,
		streamAPI:   cfg.StreamClient,
		agentAPI:    cfg.AgentClient,
		runtimeAPI:  cfg.RuntimeClient,
		counter:     cfg.Counter,
		engine:      cfg.Engine,
		catalog:     cfg.Catalog,
		activity:    cfg.Activity,
		region:      cfg.Region,
		maxOut:      maxOut,
		streamStall: cfg.StreamStallTimeout,
	}
	// Quote capture is opportunistic: a catalog that cannot hand out its rates
	// still prices requests, it just cannot freeze them, so settlement falls back
	// to a live quote. Every catalog throttle ships supports capture.
	if rs, ok := cfg.Catalog.(pricing.RateSource); ok {
		c.rates = rs
	}
	return c, nil
}

// Name identifies the access provider this adapter governs.
func (c *Client) Name() string { return AccessProvider }

// Request is a governed Converse call.
type Request struct {
	// BudgetID is the budget to charge. Required.
	BudgetID string

	// Input is the SDK request, passed to Bedrock unchanged. Required.
	Input *bedrockruntime.ConverseInput

	// RequestID identifies this call for reconciliation. It is also the basis of
	// the reservation ID, so retrying an ambiguous failure with the same RequestID
	// is idempotent rather than double-reserving. Generated if empty.
	RequestID string

	// Metadata is recorded on the reservation and charge, for attributing spend to
	// a workload. Prompt and response content is never recorded.
	Metadata map[string]string

	// Options are passed through to the SDK call.
	Options []func(*bedrockruntime.Options)
}

// Result is the outcome of a governed call: the provider's own response plus what
// throttle recorded about it.
type Result struct {
	// Output is the SDK response, unmodified. It may be non-nil even when Converse
	// returns an error, if the provider answered but accounting failed.
	Output *bedrockruntime.ConverseOutput

	// Identity is what was actually called. CanonicalModel may be empty; that means
	// throttle does not recognize the model, not that anything failed.
	Identity usage.ModelIdentity

	// Estimate is what was predicted, including its quality. Comparing it with
	// Usage is how estimate accuracy gets measured later.
	Estimate usage.Estimate

	// Usage is what the provider reported consuming.
	Usage usage.Usage

	// Cost is the priced actual. It may be legitimately unknown while Usage is
	// fully known, when the catalog does not price this model.
	Cost usage.Cost

	// Quote is the rate set captured at admission and used to price Cost. It is
	// returned so a caller can see, and later replay, the basis on which this
	// request was accounted -- not whatever the catalog says now.
	Quote pricing.CapturedQuote

	// Unresolved reports that the request ran and spent money throttle could not
	// name. Its reservation stays encumbered rather than being released, so the
	// budget does not offer already-spent headroom to the next caller.
	Unresolved bool

	// Mode is the enforcement posture that actually governed this request -- the
	// strictest in the budget chain, which is not necessarily the leaf's own. It is
	// reported so that a recorded request can later be read correctly: spend
	// admitted under monitor mode was never subject to the ceiling.
	Mode engine.Mode

	// Decision is the admission decision that authorized the call.
	Decision engine.Decision

	// ReservationID is the hold this call was made under.
	ReservationID string

	// Charge is the durable record, valid when Settled is true.
	Charge ledger.Charge

	// Settled reports whether the spend was recorded. False means the hold is still
	// outstanding, and the accompanying error says why.
	Settled bool

	// Latency is the wall clock the caller experienced, which includes transport
	// and SDK retries. ProviderLatency, in Charge.Usage, is what Bedrock reported.
	Latency time.Duration
}

// Converse makes a governed, non-streaming Converse call.
//
// The sequence is: estimate the cost, reserve it atomically across the budget
// chain, call Bedrock, then reconcile against what actually happened. The request
// and response are the SDK's own.
//
// # Failure handling
//
// Each failure mode gets the treatment that records reality rather than the one
// that simplifies the code:
//
//   - Refused by budget: nothing is reserved and nothing is called.
//   - Provider returns an error with no usage: the hold is released, because
//     nothing was billed.
//   - Provider returns an error *and* usage: the usage is settled. A partially
//     served request that Bedrock billed for is real spend.
//   - Provider succeeds but settlement fails: the response is returned alongside
//     ErrAccounting. The hold stays outstanding rather than being released, since
//     releasing would erase spend that happened.
//   - Context cancelled or deadline exceeded mid-call: the outcome is genuinely
//     ambiguous, so the hold is left outstanding and ErrOutcomeUnknown is
//     returned. See the note on ErrOutcomeUnknown.
//   - Process dies mid-call: the lease lapses and recovery reclaims the hold. The
//     spend is lost to the ledger until durable pre-call activity records exist.
//
// Releasing and settling use a context detached from the caller's, so a cancelled
// or timed-out request can still finish its bookkeeping.
func (c *Client) Converse(ctx context.Context, req Request) (*Result, error) {
	if req.BudgetID == "" {
		return nil, errors.New("bedrock: budget id is required")
	}
	if req.Input == nil || req.Input.ModelId == nil || *req.Input.ModelId == "" {
		return nil, errors.New("bedrock: a Converse input with a model id is required")
	}

	requestID := req.RequestID
	if requestID == "" {
		id, err := newRequestID()
		if err != nil {
			return nil, fmt.Errorf("bedrock: generating a request id: %w", err)
		}
		requestID = id
	}

	est, quote, err := c.estimate(ctx, converseParams(req.Input))
	if err != nil {
		return nil, err
	}
	res := &Result{Identity: est.Identity, Estimate: est, Quote: quote}

	start := c.engine.Now()
	rec := activity.Record{
		RequestID: requestID,
		BudgetID:  req.BudgetID,
		Identity:  est.Identity,
		Estimate:  est,
		Quote:     quote,
		Status:    activity.StatusPending,
		StartedAt: start,
		Metadata:  req.Metadata,
	}

	tx, dec, err := c.engine.Begin(ctx, engine.Request{
		BudgetID:  req.BudgetID,
		RequestID: requestID,
		Estimate:  est,
		Identity:  est.Identity,
		Metadata:  req.Metadata,
	})
	res.Decision = dec
	res.Mode = dec.Mode
	rec.EnforcementMode = dec.Mode
	if err != nil {
		// A denied request is recorded too. "The budget stopped this" is exactly the
		// kind of thing an operator needs to see, and it is invisible if only
		// successful calls are written.
		rec.Status = activity.StatusDenied
		rec.Outcome = activity.OutcomeBudgetDenied
		if errors.Is(err, engine.ErrCostUnknown) {
			rec.Outcome = activity.OutcomeUnpriced
			rec.ActualCost = est.Cost
		}
		rec.Error = err.Error()
		rec.CompletedAt = c.engine.Now()
		c.record(context.WithoutCancel(ctx), rec, false)
		return res, err
	}
	res.ReservationID = tx.Reservation().ID
	rec.ReservationID = res.ReservationID
	rec.Reserved = tx.Reservation().Amount
	rec.Scopes = scopesOf(tx.Reservation())

	// Bookkeeping must survive the caller's context ending, or a timed-out request
	// would strand its own hold.
	settleCtx := context.WithoutCancel(ctx)

	// Written before the call, so a process that dies mid-request leaves evidence
	// that money may have moved. Without this, a crash is indistinguishable from a
	// request that never happened.
	c.record(settleCtx, rec, true)

	out, callErr := c.api.Converse(ctx, req.Input, req.Options...)
	res.Output = out
	res.Latency = c.engine.Now().Sub(start)
	rec.Latency = res.Latency
	rec.CompletedAt = c.engine.Now()

	// A response with usage is billable even if the call also errored, so usage is
	// checked before the error is.
	if out == nil || out.Usage == nil {
		// Cancellation is not the same as a clean provider refusal: the request may
		// have been served and billed before the caller gave up, and no response came
		// back to prove it either way.
		if ctxErr := ctx.Err(); ctxErr != nil {
			rec.Status = activity.StatusOutstanding
			rec.Outcome = activity.OutcomeCancelled
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				rec.Outcome = activity.OutcomeTimeout
			}
			rec.ActualCost = usage.UnknownCost("the call was interrupted before any usage was reported")
			err := fmt.Errorf("%w: the call was interrupted (%v), so reservation %s is left outstanding (the provider returned: %v)",
				ErrOutcomeUnknown, ctxErr, res.ReservationID, callErr)
			rec.Error = err.Error()
			c.record(settleCtx, rec, false)
			return res, err
		}
		if callErr == nil {
			// A success with no usage metadata is not something Bedrock should do, and
			// the honest response is to admit the accounting is unresolvable rather
			// than to record a zero-cost request. The hold stays outstanding.
			rec.Status = activity.StatusOutstanding
			rec.Outcome = activity.OutcomeAccountingError
			rec.ActualCost = usage.UnknownCost("the provider reported no usage metadata")
			err := fmt.Errorf("%w: the provider returned no usage metadata, so reservation %s is left outstanding",
				ErrAccounting, res.ReservationID)
			rec.Error = err.Error()
			c.record(settleCtx, rec, false)
			return res, err
		}
		// A provider-side error with no usage means nothing was billed, so the
		// headroom goes back.
		rec.Status = activity.StatusReleased
		rec.Outcome = activity.OutcomeProviderError
		rec.ActualCost = usage.KnownCost(0)
		if relErr := tx.Release(settleCtx); relErr != nil {
			err := fmt.Errorf("%w: %w (releasing reservation %s also failed: %v)",
				ErrProvider, callErr, res.ReservationID, relErr)
			rec.Error = err.Error()
			c.record(settleCtx, rec, false)
			return res, err
		}
		rec.Error = callErr.Error()
		c.record(settleCtx, rec, false)
		return res, fmt.Errorf("%w: %w", ErrProvider, callErr)
	}

	// The response's own identity is authoritative over the request's: Bedrock
	// reports the tier that actually served the call, which can differ from the one
	// asked for and can change the price.
	if tier := tierOf(out.ServiceTier); tier != "" {
		res.Identity.ServiceTier = tier
	}

	res.Usage = normalizeTokens(out.Usage)
	rec.Identity = res.Identity
	rec.ActualUsage = res.Usage
	rec.ProviderLatency = providerLatency(out.Metrics)

	actual := usage.Actual{
		Identity:        res.Identity,
		Usage:           res.Usage,
		ProviderLatency: rec.ProviderLatency,
	}

	cost, quoteErr := c.priceActual(settleCtx, quote, res.Identity, res.Usage)
	actual.Cost = cost
	res.Cost = cost
	rec.ActualCost = cost

	if !cost.Known() {
		// The request ran and cost money throttle cannot name. The hold stays
		// encumbered: releasing it would report spent money as available, and
		// settling the partial floor as a total would understate real spend.
		if markErr := tx.MarkUnresolved(settleCtx, actual); markErr != nil {
			rec.Status = activity.StatusOutstanding
			rec.Error = markErr.Error()
			c.record(settleCtx, rec, false)
			return res, fmt.Errorf("%w: %w", ErrAccounting, markErr)
		}
		res.Unresolved = true
		rec.Status = activity.StatusUnresolved
		rec.Outcome = activity.OutcomeUnpriced
		err := fmt.Errorf("%w: %s: reservation %s stays encumbered pending reconciliation",
			ErrCostUnresolved, cost.Reason, res.ReservationID)
		rec.Error = err.Error()
		c.record(settleCtx, rec, false)
		if callErr != nil {
			return res, fmt.Errorf("%w (the provider also returned: %v)", err, callErr)
		}
		return res, err
	}

	charge, setErr := tx.Settle(settleCtx, actual)
	if setErr != nil {
		rec.Status = activity.StatusOutstanding
		rec.Outcome = activity.OutcomeAccountingError
		err := fmt.Errorf("%w: reservation %s is left outstanding: %w", ErrAccounting, res.ReservationID, setErr)
		if quoteErr != nil {
			err = fmt.Errorf("%w (pricing: %v)", err, quoteErr)
		}
		rec.Error = err.Error()
		c.record(settleCtx, rec, false)
		if callErr != nil {
			return res, fmt.Errorf("%w (the provider also returned: %v)", err, callErr)
		}
		return res, err
	}
	res.Charge = charge
	res.Settled = true
	rec.Status = activity.StatusSettled
	rec.Outcome = activity.OutcomeSuccess

	// Settled, but the provider still reported a failure alongside the usage it
	// billed for. The caller needs to know both.
	if callErr != nil {
		rec.Outcome = activity.OutcomeProviderError
		rec.Error = callErr.Error()
		c.record(settleCtx, rec, false)
		return res, fmt.Errorf("%w: %w (usage was recorded)", ErrProvider, callErr)
	}
	c.record(settleCtx, rec, false)
	return res, nil
}

// priceActual prices observed usage, preferring the quote captured at admission.
//
// Replaying the captured rates is what makes settlement reproducible: a catalog
// update between admission and settlement must not change what this request costs.
// Only a request that was never priceable falls back to the live catalog, and it
// has nothing to be inconsistent with.
func (c *Client) priceActual(ctx context.Context, quote pricing.CapturedQuote, id usage.ModelIdentity, u usage.Usage) (usage.Cost, error) {
	if quote.Valid() {
		// For picks the captured tier the provider actually served on, which can
		// differ from the one requested and can price differently.
		priced, err := quote.For(id).Price(u)
		return priced.Cost, err
	}
	q, err := c.catalog.Quote(ctx, id, u, c.engine.Now())
	return q.Cost, err
}

// record persists an activity record, never failing the request over it.
//
// Telemetry is not worth a provider response the caller has already paid for, so a
// store error is dropped. The alternative -- surfacing it -- would mean a
// misconfigured activity database could break governed calls that are otherwise
// working perfectly.
func (c *Client) record(ctx context.Context, rec activity.Record, begin bool) {
	if c.activity == nil {
		return
	}
	if begin {
		_ = c.activity.Begin(ctx, rec)
		return
	}
	_ = c.activity.Complete(ctx, rec)
}

// scopesOf converts a reservation's legs into activity scopes, so a record keeps
// its attribution after the reservation itself is gone.
func scopesOf(r ledger.Reservation) []activity.Scope {
	out := make([]activity.Scope, 0, len(r.Legs))
	for _, l := range r.Legs {
		out = append(out, activity.Scope{
			BudgetID: l.Scope.BudgetID,
			PeriodID: l.Scope.PeriodID,
			Depth:    l.Depth,
		})
	}
	return out
}

// providerLatency converts Bedrock's reported latency. It is what the provider
// says it spent, not the caller's wall clock.
func providerLatency(m *types.ConverseMetrics) time.Duration {
	if m == nil || m.LatencyMs == nil {
		return 0
	}
	return time.Duration(*m.LatencyMs) * time.Millisecond
}

// stallBound returns how long a governed stream waits for the caller to accept one
// event before treating the stream as abandoned.
//
// Zero configuration defaults to the reservation lease: a caller slower than that
// has already stopped being distinguishable from one that walked away, and the hold
// is about to be reclaimed anyway. A ledger with leases disabled gets no bound,
// which is the same trade that configuration already made.
func (c *Client) stallBound(lease time.Duration) time.Duration {
	if c.streamStall != 0 {
		return c.streamStall
	}
	return lease
}

// maxOutputTokens returns the output ceiling to estimate against, and whether the
// caller set it. A caller-set cap is a real bound on the request; throttle's
// default is an assumption, and the difference is reported in the estimate.
func (c *Client) maxOutputTokens(p params) (int32, bool) {
	if p.maxTokens != nil && *p.maxTokens > 0 {
		return *p.maxTokens, true
	}
	return c.maxOut, false
}

// Estimate predicts what a Converse call will consume and cost.
//
// Output tokens cannot be known before generation, so no Converse estimate is ever
// QualityExact even when the input count is exact: the estimate is bounded above by
// the output cap, which makes it conservative. Labelling it exact would be a lie
// the caller could not compensate for.
func (c *Client) Estimate(ctx context.Context, in *bedrockruntime.ConverseInput) (usage.Estimate, error) {
	if in == nil {
		return usage.Estimate{}, errors.New("bedrock: a Converse input with a model id is required")
	}
	est, _, err := c.estimate(ctx, converseParams(in))
	return est, err
}

// EstimateStream predicts what a ConverseStream call will consume and cost.
//
// It is the same estimate as Estimate: the streaming and non-streaming forms of a
// request consume the same tokens, and both run through one code path so they
// cannot disagree.
func (c *Client) EstimateStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput) (usage.Estimate, error) {
	if in == nil {
		return usage.Estimate{}, errors.New("bedrock: a ConverseStream input with a model id is required")
	}
	est, _, err := c.estimate(ctx, streamParams(in))
	return est, err
}

// estimate is Estimate plus the captured quote, which the governed path needs and
// the public signature has no room for.
func (c *Client) estimate(ctx context.Context, p params) (usage.Estimate, pricing.CapturedQuote, error) {
	if p.modelID == "" {
		return usage.Estimate{}, pricing.CapturedQuote{}, errors.New("bedrock: a request with a model id is required")
	}
	id := Identify(p.modelID, c.region, tierOf(p.serviceTier))
	id.Operation = p.operation

	maxOut, callerSet := c.maxOutputTokens(p)

	inTokens, exactInput, note := c.countInput(ctx, p)

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  inTokens,
		usage.OutputTokens: int64(maxOut),
	})

	est := usage.Estimate{Identity: id, Usage: u}
	switch {
	case exactInput && callerSet:
		// Input is counted by the model's own tokenizer and output is capped by the
		// caller: actual usage should not exceed this.
		est.Quality = usage.QualityConservative
	case exactInput:
		est.Quality = usage.QualityConservative
		note = join(note, fmt.Sprintf("the request set no MaxTokens, so output was bounded by throttle's default of %d", maxOut))
	default:
		// The input count is a guess, so the whole estimate is a guess: actual usage
		// may exceed it in either dimension.
		est.Quality = usage.QualityHeuristic
	}
	est.Note = note

	// The rates are captured here, at admission, and settlement replays them. If
	// the catalog were re-queried after the call, a price refresh landing mid-request
	// would reserve against one price sheet and charge against another, with nothing
	// in the record to show it happened.
	at := c.engine.Now()
	if c.rates != nil {
		captured, err := c.rates.Capture(id, at)
		if err != nil {
			// An unpriceable model yields a known usage estimate with an explicitly
			// unknown cost. That is a legitimate state, not a failure: the engine
			// decides what to do with it according to enforcement posture.
			est.Cost = usage.UnknownCost(err.Error())
			return est, pricing.CapturedQuote{}, nil
		}
		priced, _ := captured.Price(u)
		est.Cost = priced.Cost
		return est, captured, nil
	}

	quote, err := c.catalog.Quote(ctx, id, u, at)
	est.Cost = quote.Cost
	if err != nil {
		return est, quote.Captured, nil
	}
	return est, quote.Captured, nil
}

// countInput returns the input token count, whether it came from the model's own
// tokenizer, and a note explaining the method.
func (c *Client) countInput(ctx context.Context, p params) (n int64, exact bool, note string) {
	if c.counter != nil {
		out, err := c.counter.CountTokens(ctx, p.countTokensInput())
		switch {
		case err == nil && out != nil && out.InputTokens != nil:
			return int64(*out.InputTokens), true, "input counted by the model's tokenizer via CountTokens"
		case ctx.Err() != nil:
			// The caller is going away; do not spend more of their deadline, and do
			// not pretend the fallback is as good.
			return estimateInputTokens(p), false,
				fmt.Sprintf("CountTokens was interrupted (%v), so input was estimated from content length", ctx.Err())
		default:
			// A counting failure must not fail the request. It degrades the estimate,
			// and the estimate says so.
			return estimateInputTokens(p), false,
				fmt.Sprintf("CountTokens failed (%v), so input was estimated from content length", err)
		}
	}
	return estimateInputTokens(p), false, "input estimated from content length; enable Config.Counter for a tokenizer count"
}

func join(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// Observe normalizes a Converse response into observed usage, without pricing it.
// It exists for callers reconciling a response they obtained outside the governed
// path.
func (c *Client) Observe(_ context.Context, out *bedrockruntime.ConverseOutput, modelID string) (usage.Actual, error) {
	if out == nil {
		return usage.Actual{}, errors.New("bedrock: a Converse output is required")
	}
	id := Identify(modelID, c.region, tierOf(out.ServiceTier))
	return usage.Actual{
		Identity:        id,
		Usage:           normalizeTokens(out.Usage),
		Cost:            usage.UnknownCost("not priced: Observe reports usage only"),
		ProviderLatency: providerLatency(out.Metrics),
	}, nil
}

// Price quotes observed usage, so a caller reconciling out-of-band usage can reach
// the same numbers the governed path does.
func (c *Client) Price(ctx context.Context, actual usage.Actual, at time.Time) (money.Money, error) {
	q, err := c.catalog.Quote(ctx, actual.Identity, actual.Usage, at)
	if err != nil {
		return 0, err
	}
	if !q.Cost.Known() {
		return 0, fmt.Errorf("%w: %s", usage.ErrCostUnknown, q.Cost.Reason)
	}
	return q.Cost.Amount, nil
}
