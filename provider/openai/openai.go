// Package openai adapts the OpenAI Responses API to throttle's accounting model.
//
// It is a shim, not a framework. A caller builds a responses.ResponseNewParams
// exactly as they would for the OpenAI Go SDK, hands it to Client.Respond with a
// budget ID, and gets back the SDK's own *responses.Response. Nothing about the
// request or response is reinterpreted, no field is rewritten, and no throttle
// abstraction stands between the caller and the model. There is deliberately no
// throttle.Generate or throttle.Chat here: the caller keeps thinking in OpenAI SDK
// concepts.
//
// What the shim adds is the transaction around the call:
//
//	estimate -> reserve -> execute -> reconcile
//
// # The boundary
//
// OpenAI SDK types stop here. This package converts a Responses reply into
// usage.Usage and usage.ModelIdentity, prices it through a pricing.Catalog, and
// hands money.Money to the engine. Nothing in budget, ledger, money, pricing,
// usage, engine, activity, reconcile, report, or dashboard imports an OpenAI type,
// which is what keeps the budget engine provider-neutral -- and this package is the
// evidence that it really is, since it reuses every one of those unchanged.
//
// # What this adapter cannot fully account for
//
// Two limits are structural rather than incidental, and the adapter reports both
// rather than papering over them:
//
// OpenAI charges for several hosted tools in units its API response cannot express
// -- per call for web search, per GB-day for file storage, per container-session for
// code interpreter. A request carrying such a tool is settled as an explicitly
// unresolved cost with a token floor, never as a completed price. See tools.go.
//
// A synchronous Responses call cannot be cancelled server-side; OpenAI permits
// cancellation only for background responses. So a caller who gives up mid-call
// leaves a genuinely ambiguous outcome, handled the same conservative way Bedrock
// handles it: the hold stays outstanding rather than being released on a guess.
//
// # Credentials
//
// This package's tests never require OpenAI credentials. Client depends on narrow
// ResponsesAPI and InputTokenCounter interfaces rather than on *openai.Client, so
// the whole transaction is exercised against a fake. The adapter never reads, holds,
// or persists an API key: credentials are the SDK's business, resolved from the
// environment by openai.NewClient in the usual way. The one test that talks to
// OpenAI is explicitly opt-in and skips cleanly without credentials.
package openai

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// ResponsesAPI is the slice of the OpenAI client this adapter uses.
//
// A consumer-defined interface rather than *openai.Client: it documents exactly
// which call throttle makes, and it makes the entire governed path testable without
// credentials, a network, or a mocking framework. An openai.Client's Responses
// service satisfies it -- see Responses.
type ResponsesAPI interface {
	New(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) (*responses.Response, error)
}

// InputTokenCounter is the optional preflight input token count.
//
// Separate from ResponsesAPI because it is a distinct extra round trip a caller may
// reasonably decline: a client without it still works, with a weaker and
// honestly-labelled estimate.
//
// It counts the real request shape server-side -- including tool schemas, which no
// local approximation can measure -- so it is much better than the heuristic. It is
// still not exact: OpenAI does not document this count as equal to what will be
// billed, and it covers only the input side. See Client.estimate.
type InputTokenCounter interface {
	Count(ctx context.Context, body responses.InputTokenCountParams, opts ...option.RequestOption) (*responses.InputTokenCountResponse, error)
}

// Errors returned by this package.
var (
	// ErrNoClient means the adapter was built without an OpenAI client.
	ErrNoClient = errors.New("openai: no Responses client configured")

	// ErrProvider wraps an error returned by OpenAI itself, so a caller can tell a
	// provider failure from a budget refusal or an accounting failure.
	//
	// This distinction is load-bearing for rate limits in particular: an OpenAI 429
	// and throttle's own budget denial are different failure domains, and a caller
	// that cannot tell them apart will retry the wrong one.
	ErrProvider = errors.New("openai: provider call failed")

	// ErrAccounting means the provider call succeeded but throttle could not record
	// it. The response is still returned: the caller got their answer, and hiding the
	// bookkeeping failure would be worse than reporting it.
	ErrAccounting = errors.New("openai: request succeeded but could not be recorded")

	// ErrOutcomeUnknown means the call was interrupted in a way that leaves it
	// genuinely unknown whether OpenAI served and billed the request. The reservation
	// is deliberately left outstanding; see Client.Respond.
	ErrOutcomeUnknown = errors.New("openai: request outcome is unknown")

	// ErrCostUnresolved means the request ran and its usage is known, but its cost is
	// not fully priceable. The usage is recorded and the hold stays encumbered
	// awaiting reconciliation; the response is still returned.
	ErrCostUnresolved = errors.New("openai: request cost is unresolved")
)

// DefaultMaxOutputTokens bounds the output half of an estimate when the caller sets
// no MaxOutputTokens.
//
// Some ceiling is required: output tokens are unknowable before generation, so
// without a cap there is no upper bound to reserve against. This value is a
// throttle-side assumption, reported as such in Estimate.Note, and never presented
// as something OpenAI stated. It does not reach the request: throttle does not set
// an output cap the caller did not ask for.
const DefaultMaxOutputTokens = 4096

// Config configures a Client.
type Config struct {
	// Client is the OpenAI Responses service. Required.
	//
	// Wrap a real *openai.Client with Responses(c) to satisfy it.
	Client ResponsesAPI

	// StreamClient enables governed streaming Responses calls. Optional: a caller who
	// only makes non-streaming calls needs nothing here, and Client.RespondStreaming
	// reports ErrNoStreamClient without it.
	//
	// Separate from Client rather than folded into it because the SDK types the two
	// calls differently -- NewStreaming has no error return -- and because a governed
	// stream carries lifecycle obligations, lease renewal and abandonment detection,
	// that a single round trip does not.
	//
	// Wrap a real *openai.Client with Streaming(c) to satisfy it.
	StreamClient StreamAPI

	// Counter enables preflight input token counting via OpenAI's
	// POST /responses/input_tokens. Optional: it is an extra round trip per request,
	// so a caller opts in. Without it, input is estimated from content length.
	//
	// Wrap a real *openai.Client with Counter(c) to satisfy it.
	Counter InputTokenCounter

	// Engine governs the spend. Required.
	Engine *engine.Engine

	// Catalog prices usage. Required: without it throttle cannot convert tokens to
	// money, and guessing is not an option.
	//
	// If it also implements pricing.RateSource -- *pricing.Static does -- the adapter
	// captures an immutable quote at admission and settles against that, so a catalog
	// update mid-request cannot change the request's accounting basis.
	Catalog pricing.Catalog

	// Activity records governed requests durably. Optional: without it the
	// transaction still works, but a request whose process dies mid-call leaves no
	// trace, and unresolved costs cannot be reconciled later.
	Activity activity.Store

	// MaxOutputTokens overrides DefaultMaxOutputTokens for estimating requests that
	// set no cap of their own. It is used for estimation only and never sent.
	MaxOutputTokens int64

	// StreamStallTimeout bounds how long a governed stream tolerates a caller that
	// has stopped consuming it before treating the stream as abandoned.
	//
	// It exists so a caller who neither reads, closes, nor cancels cannot renew a hold
	// indefinitely. It is emphatically not a maximum generation time: the bound
	// measures the gap between a caller receiving one event and asking for the next,
	// and it does not run while the caller is blocked inside Stream.Next waiting on
	// OpenAI. A slow provider is not a stalled consumer.
	//
	// Zero means the reservation lease, which is generous by construction: a caller
	// idler than the lease is about to have its hold reclaimed regardless. The same
	// setting and the same meaning as the Bedrock adapter's, so there is one concept
	// rather than two.
	StreamStallTimeout time.Duration
}

// Client is a governed OpenAI Responses client.
type Client struct {
	api         ResponsesAPI
	streamAPI   StreamAPI
	counter     InputTokenCounter
	engine      *engine.Engine
	catalog     pricing.Catalog
	rates       pricing.RateSource
	activity    activity.Store
	maxOut      int64
	streamStall time.Duration
}

// New builds a governed client.
func New(cfg Config) (*Client, error) {
	if cfg.Client == nil {
		return nil, ErrNoClient
	}
	if cfg.Engine == nil {
		return nil, errors.New("openai: an engine is required")
	}
	if cfg.Catalog == nil {
		return nil, errors.New("openai: a pricing catalog is required")
	}
	if cfg.MaxOutputTokens < 0 {
		return nil, errors.New("openai: max output tokens cannot be negative")
	}
	if cfg.StreamStallTimeout < 0 {
		return nil, errors.New("openai: stream stall timeout cannot be negative")
	}
	maxOut := cfg.MaxOutputTokens
	if maxOut == 0 {
		maxOut = DefaultMaxOutputTokens
	}
	c := &Client{
		api:         cfg.Client,
		streamAPI:   cfg.StreamClient,
		counter:     cfg.Counter,
		engine:      cfg.Engine,
		catalog:     cfg.Catalog,
		activity:    cfg.Activity,
		maxOut:      maxOut,
		streamStall: cfg.StreamStallTimeout,
	}
	// Quote capture is opportunistic: a catalog that cannot hand out its rates still
	// prices requests, it just cannot freeze them, so settlement falls back to a live
	// quote. Every catalog throttle ships supports capture.
	if rs, ok := cfg.Catalog.(pricing.RateSource); ok {
		c.rates = rs
	}
	return c, nil
}

// Name identifies the access provider this adapter governs.
func (c *Client) Name() string { return AccessProvider }

// Request is a governed Responses call.
type Request struct {
	// BudgetID is the budget to charge. Required.
	BudgetID string

	// Params is the SDK request, passed to OpenAI unchanged. Required.
	//
	// Unchanged is meant literally. throttle does not adjust MaxOutputTokens, does not
	// set or rewrite Store, does not touch PromptCacheOptions or ServiceTier, and adds
	// no field of its own. Governing a request is not the same as editing it, and a
	// caller who cannot predict what reaches OpenAI cannot reason about their own
	// bill.
	Params responses.ResponseNewParams

	// RequestID identifies this call for reconciliation. It is also the basis of the
	// reservation ID, so retrying an ambiguous failure with the same RequestID is
	// idempotent rather than double-reserving. Generated if empty.
	RequestID string

	// Metadata is recorded on the reservation and charge, for attributing spend to a
	// workload. Prompt and response content is never recorded.
	Metadata map[string]string

	// Options are passed through to the SDK call.
	Options []option.RequestOption
}

// Result is the outcome of a governed call: the provider's own response plus what
// throttle recorded about it.
type Result struct {
	// Response is the SDK response, unmodified. It may be non-nil even when Respond
	// returns an error, if the provider answered but accounting failed.
	Response *responses.Response

	// Identity is what was actually called. CanonicalModel is empty for OpenAI, which
	// means throttle does not claim a normalized name for the model, not that anything
	// failed. ProviderModelID is the caller's exact string; ServedModelID reports
	// what OpenAI said it used, when that differs.
	Identity usage.ModelIdentity

	// ServedModelID is the model OpenAI reported serving the request, when it differs
	// from the one requested -- typically an alias resolving to a dated snapshot, e.g.
	// a request for "gpt-4o" served by "gpt-4o-2024-08-06".
	//
	// Kept separate from Identity.ProviderModelID rather than overwriting it. Both are
	// facts and they answer different questions: what the caller asked for, and what
	// ran. OpenAI's own schema does not state that this field is the resolved model --
	// request and response share one definition -- so throttle records the difference
	// without asserting what caused it.
	ServedModelID string

	// Estimate is what was predicted, including its quality. Comparing it with Usage
	// is how estimate accuracy gets measured later.
	Estimate usage.Estimate

	// Usage is what the provider reported consuming, decomposed into throttle's
	// disjoint dimensions. See normalizeUsage: OpenAI's figures are inclusive and
	// throttle's are not.
	Usage usage.Usage

	// Cost is the priced actual. It may be legitimately unknown or partial while Usage
	// is fully known -- an unpriced model, or a hosted tool whose charge OpenAI does
	// not report.
	Cost usage.Cost

	// Quote is the rate set captured at admission and used to price Cost. It is
	// returned so a caller can see, and later replay, the basis on which this request
	// was accounted -- not whatever the catalog says now.
	Quote pricing.CapturedQuote

	// Unresolved reports that the request ran and spent money throttle could not
	// name. Its reservation stays encumbered rather than being released, so the budget
	// does not offer already-spent headroom to the next caller.
	Unresolved bool

	// Mode is the enforcement posture that actually governed this request -- the
	// strictest in the budget chain, which is not necessarily the leaf's own.
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

	// Latency is the wall clock the caller experienced, which includes transport and
	// SDK retries.
	//
	// There is no provider-reported counterpart. A Response carries no latency field,
	// and its completed_at is present only on a completed response -- so unlike
	// Bedrock, which reports its own latency, nothing here can be attributed to
	// OpenAI. Recording zero for provider latency is the honest outcome; deriving it
	// from wall clock would invent a measurement.
	Latency time.Duration
}

// Respond makes a governed, non-streaming Responses call.
//
// The sequence is: estimate the cost, reserve it atomically across the budget chain,
// call OpenAI, then reconcile against what actually happened. The request and
// response are the SDK's own.
//
// # Failure handling
//
// Each failure mode gets the treatment that records reality rather than the one that
// simplifies the code. These are the same rules the Bedrock adapter follows, because
// they are properties of the accounting model rather than of a provider:
//
//   - Refused by budget: nothing is reserved and nothing is called, but the refusal
//     is recorded.
//   - Provider returns an error with no usage: the hold is released, because nothing
//     was billed.
//   - Provider returns an error *and* usage: the usage is settled. A partially served
//     request OpenAI billed for is real spend.
//   - Response is incomplete -- the output cap was reached, or a content filter
//     stopped it -- and carries usage: the usage is charged. Generation stopping
//     earlier than the caller hoped is not a refund.
//   - Response is failed or incomplete with no usage: nothing is known to have been
//     billed, so the hold is released, with the provider's reported error recorded.
//   - Tools whose charges OpenAI does not report: settled as unresolved with a token
//     floor. Never priced as complete.
//   - Provider succeeds but settlement fails: the response is returned alongside
//     ErrAccounting. The hold stays outstanding rather than being released, since
//     releasing would erase spend that happened.
//   - Context cancelled or deadline exceeded mid-call: the outcome is genuinely
//     ambiguous -- OpenAI cannot cancel a synchronous response server-side, so the
//     request may well have been served and billed -- and the hold is left
//     outstanding with ErrOutcomeUnknown.
//
// Releasing and settling use a context detached from the caller's, so a cancelled or
// timed-out request can still finish its bookkeeping.
func (c *Client) Respond(ctx context.Context, req Request) (*Result, error) {
	if req.BudgetID == "" {
		return nil, errors.New("openai: budget id is required")
	}
	if modelOf(req.Params.Model) == "" {
		return nil, errors.New("openai: a request with a model is required")
	}

	requestID := req.RequestID
	if requestID == "" {
		id, err := newRequestID()
		if err != nil {
			return nil, fmt.Errorf("openai: generating a request id: %w", err)
		}
		requestID = id
	}

	p := responseParams(req.Params)
	est, quote, err := c.estimate(ctx, req.Params, p)
	if err != nil {
		return nil, err
	}
	res := &Result{Identity: est.Identity, Estimate: est, Quote: quote}

	// Classified before the call, from the request rather than the response. A hosted
	// tool's surcharge is incurred by asking for the tool, and the response does not
	// report it either way -- so the request is the only place this is knowable.
	exposure := classifyTools(req.Params.Tools)

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
		// A denied request is recorded too. "The budget stopped this" is exactly what an
		// operator needs to see, and it is invisible if only successful calls are
		// written.
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

	// Written before the call, so a process that dies mid-request leaves evidence that
	// money may have moved. Without this, a crash is indistinguishable from a request
	// that never happened.
	c.record(settleCtx, rec, true)

	out, callErr := c.api.New(ctx, req.Params, req.Options...)
	res.Response = out
	res.Latency = c.engine.Now().Sub(start)
	rec.Latency = res.Latency
	rec.CompletedAt = c.engine.Now()

	// A response with usage is billable even if the call also errored, so usage is
	// checked before the error is.
	if !hasUsage(out) {
		return res, c.finishWithoutUsage(ctx, settleCtx, tx, res, rec, out, callErr)
	}

	// The response's own identity is authoritative for what actually ran: OpenAI
	// reports the tier that served the call, which can differ from the one asked for
	// and can change the price, and it reports the model it used.
	if tier := tierOf(out.ServiceTier); tier != "" {
		res.Identity.ServiceTier = tier
	}
	if served := modelOf(out.Model); served != "" && served != res.Identity.ProviderModelID {
		res.ServedModelID = served
	}

	u, normErr := normalizeUsage(out.Usage)
	if normErr != nil {
		// The provider's own figures contradict each other, so there is nothing
		// trustworthy to charge. The hold stays outstanding rather than being released:
		// the request ran, and the usage object proves tokens were consumed even though
		// its arithmetic does not add up.
		res.Usage = u
		rec.ActualUsage = u
		rec.Status = activity.StatusOutstanding
		rec.Outcome = activity.OutcomeAccountingError
		rec.ActualCost = usage.UnknownCost(normErr.Error())
		res.Cost = rec.ActualCost
		err := fmt.Errorf("%w: %w, so reservation %s is left outstanding", ErrAccounting, normErr, res.ReservationID)
		rec.Error = err.Error()
		c.record(settleCtx, rec, false)
		return res, err
	}

	res.Usage = u
	rec.Identity = res.Identity
	rec.ActualUsage = u
	if res.ServedModelID != "" {
		rec.Metadata = withServedModel(rec.Metadata, res.ServedModelID)
	}

	actual := usage.Actual{Identity: res.Identity, Usage: u}

	cost, quoteErr := c.priceActual(settleCtx, quote, res.Identity, u)

	// A hosted tool whose charge OpenAI bills outside the usage object means the token
	// cost is a floor, however completely the tokens themselves priced. Downgrading a
	// known cost here is the point: enforce mode must never report such a request as
	// fully priced.
	if !exposure.complete() {
		cost = incompleteForTools(cost, exposure)
	}
	actual.Cost = cost
	res.Cost = cost
	rec.ActualCost = cost

	if !cost.Known() {
		// The request ran and cost money throttle cannot name. The hold stays
		// encumbered: releasing it would report spent money as available, and settling
		// the partial floor as a total would understate real spend.
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

	// Settled, but the provider still reported a failure alongside the usage it billed
	// for. The caller needs to know both.
	if callErr != nil {
		rec.Outcome = activity.OutcomeProviderError
		rec.Error = redactProviderError(callErr)
		c.record(settleCtx, rec, false)
		return res, fmt.Errorf("%w: %w (usage was recorded)", ErrProvider, callErr)
	}

	// A response that stopped early or failed, yet reported usage, is settled on that
	// usage and the caller is told why it stopped. Recording the reason as an outcome
	// keeps "we charged for a truncated answer" legible later.
	if status := statusOutcome(out); status != "" {
		rec.Outcome = status
		rec.Error = responseReason(out)
	}
	c.record(settleCtx, rec, false)
	if err := statusError(out); err != nil {
		return res, err
	}
	return res, nil
}

// finishWithoutUsage handles every path where the provider reported no usage.
//
// Split out because there are four genuinely different endings here and inlining
// them made the shape of Respond hard to see: an interrupted call, a provider error,
// a provider-reported failure, and a success that reported nothing.
func (c *Client) finishWithoutUsage(ctx, settleCtx context.Context, tx *engine.Transaction, res *Result, rec activity.Record, out *responses.Response, callErr error) error {
	// Cancellation is not the same as a clean provider refusal: OpenAI cannot cancel a
	// synchronous response server-side, so the request may have been served and billed
	// before the caller gave up, and no response came back to prove it either way.
	if ctxErr := ctx.Err(); ctxErr != nil {
		rec.Status = activity.StatusOutstanding
		rec.Outcome = activity.OutcomeCancelled
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			rec.Outcome = activity.OutcomeTimeout
		}
		rec.ActualCost = usage.UnknownCost("the call was interrupted before any usage was reported")
		res.Cost = rec.ActualCost
		err := fmt.Errorf("%w: the call was interrupted (%v), so reservation %s is left outstanding (the provider returned: %v)",
			ErrOutcomeUnknown, ctxErr, res.ReservationID, callErr)
		rec.Error = err.Error()
		c.record(settleCtx, rec, false)
		return err
	}

	if callErr != nil {
		// A provider-side error with no usage means nothing was billed, so the headroom
		// goes back. This is where an OpenAI 429 lands, and it is recorded as a provider
		// error: it is not a budget denial, and conflating the two would send a caller
		// looking at the wrong system.
		rec.Status = activity.StatusReleased
		rec.Outcome = activity.OutcomeProviderError
		rec.ActualCost = usage.KnownCost(0)
		res.Cost = rec.ActualCost
		if relErr := tx.Release(settleCtx); relErr != nil {
			err := fmt.Errorf("%w: %w (releasing reservation %s also failed: %v)",
				ErrProvider, callErr, res.ReservationID, relErr)
			rec.Error = err.Error()
			c.record(settleCtx, rec, false)
			return err
		}
		rec.Error = redactProviderError(callErr)
		c.record(settleCtx, rec, false)
		return fmt.Errorf("%w: %w", ErrProvider, callErr)
	}

	// No transport error, but the response itself says it failed or stopped early and
	// reported no usage. OpenAI does not document whether such a response carries
	// usage; when it does not, there is nothing known to have been billed, so the hold
	// is released and the provider's stated reason is recorded.
	if out != nil && !completed(out) {
		rec.Status = activity.StatusReleased
		rec.Outcome = statusOutcome(out)
		rec.ActualCost = usage.KnownCost(0)
		res.Cost = rec.ActualCost
		if relErr := tx.Release(settleCtx); relErr != nil {
			err := fmt.Errorf("%w: the response was %s and reported no usage, and releasing reservation %s failed: %w",
				ErrAccounting, out.Status, res.ReservationID, relErr)
			rec.Error = err.Error()
			c.record(settleCtx, rec, false)
			return err
		}
		rec.Error = responseReason(out)
		c.record(settleCtx, rec, false)
		return statusError(out)
	}

	// A completed response with no usage metadata is not something OpenAI should
	// produce, and the honest answer is to admit the accounting is unresolvable rather
	// than record a zero-cost request. The hold stays outstanding.
	rec.Status = activity.StatusOutstanding
	rec.Outcome = activity.OutcomeAccountingError
	rec.ActualCost = usage.UnknownCost("the provider reported no usage metadata")
	res.Cost = rec.ActualCost
	err := fmt.Errorf("%w: the provider returned no usage metadata, so reservation %s is left outstanding",
		ErrAccounting, res.ReservationID)
	rec.Error = err.Error()
	c.record(settleCtx, rec, false)
	return err
}

// incompleteForTools downgrades a cost to reflect a charge throttle cannot see.
//
// The token figures may have priced perfectly; the point is that they are not the
// whole bill. A known cost becomes a floor, and a cost that was already partial or
// unknown keeps its own reason alongside this one -- both facts are true and a
// reconciliation needs both.
func incompleteForTools(cost usage.Cost, e toolExposure) usage.Cost {
	reason := fmt.Sprintf("OpenAI bills %s outside the response's usage object (per call, per stored GB, or per container session), so the token cost is a floor rather than a total",
		joinAnd(e.unaccounted))
	switch cost.State() {
	case usage.CostKnown:
		return usage.PartialCost(cost.Amount, nil, reason)
	case usage.CostPartial:
		return usage.PartialCost(cost.Amount, cost.Unpriced, cost.Reason+"; "+reason)
	default:
		return usage.UnknownCost(cost.Reason + "; " + reason)
	}
}

// priceActual prices observed usage, preferring the quote captured at admission.
//
// Replaying the captured rates is what makes settlement reproducible: a catalog
// update between admission and settlement must not change what this request costs.
// Only a request that was never priceable falls back to the live catalog, and it has
// nothing to be inconsistent with.
func (c *Client) priceActual(ctx context.Context, quote pricing.CapturedQuote, id usage.ModelIdentity, u usage.Usage) (usage.Cost, error) {
	if quote.Valid() {
		// For picks the captured tier the provider actually served on, which can differ
		// from the one requested and can price differently.
		//
		// A tier no rate was frozen for is refused rather than priced at the admitted
		// rates. The call happened and cost money, so the answer is an unknown cost that
		// leaves the reservation encumbered -- not a confident figure computed from a
		// price sheet this request did not run under.
		applicable, tierErr := quote.For(id)
		if tierErr != nil {
			return usage.UnknownCost(tierErr.Error()), tierErr
		}
		priced, err := applicable.Price(u)
		return priced.Cost, err
	}
	q, err := c.catalog.Quote(ctx, id, u, c.engine.Now())
	return q.Cost, err
}

// record persists an activity record, never failing the request over it.
//
// Telemetry is not worth a provider response the caller has already paid for, so a
// store error is dropped.
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

// scopesOf converts a reservation's legs into activity scopes, so a record keeps its
// attribution after the reservation itself is gone.
//
// The legs are the whole input, deliberately: the recorded attribution has to be the
// chain the money actually moved through, which is the one the ledger derived from
// stored parent links, not one this adapter reconstructs from the budget id it was
// handed.
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

// withServedModel records the model OpenAI reported using, when it differs from the
// one asked for.
//
// Metadata rather than a column: the identity's ProviderModelID is the caller's
// string and stays that way, and a served-model field on the neutral Record would be
// a schema change made for one provider's alias resolution. The caller's map is
// copied rather than written to, since it belongs to them.
func withServedModel(m map[string]string, served string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out["openai.served_model"] = served
	return out
}

// Estimate predicts what a Responses call will consume and cost.
//
// No Responses estimate is ever QualityExact, for two independent reasons. Output
// tokens cannot be known before generation. And even the input half, counted by
// OpenAI's own endpoint, is not documented as equal to what will be billed -- so
// calling it exact would assert a guarantee OpenAI has not made.
func (c *Client) Estimate(ctx context.Context, in responses.ResponseNewParams) (usage.Estimate, error) {
	if modelOf(in.Model) == "" {
		return usage.Estimate{}, errors.New("openai: a request with a model is required")
	}
	est, _, err := c.estimate(ctx, in, responseParams(in))
	return est, err
}

// estimate is Estimate plus the captured quote, which the governed path needs and the
// public signature has no room for.
func (c *Client) estimate(ctx context.Context, in responses.ResponseNewParams, p params) (usage.Estimate, pricing.CapturedQuote, error) {
	if p.modelID == "" {
		return usage.Estimate{}, pricing.CapturedQuote{}, errors.New("openai: a request with a model is required")
	}
	id := Identify(p.modelID, p.serviceTier)
	// The operation comes from the params rather than from Identify's default, so a
	// streaming request is estimated and quoted by this function while still recording
	// which API it was. Pricing keys on provider and model ID, so the two forms of one
	// request necessarily quote identically -- which is the point of routing both
	// through here.
	id.Operation = p.operation

	maxOut, callerSet := c.maxOutputTokens(p)
	inTokens, counted, note := c.countInput(ctx, in)

	// Output is estimated as one figure covering visible and reasoning tokens
	// together, which is how OpenAI's own cap is defined and how it bills: reasoning is
	// charged at the output rate. Splitting a prediction across the two would imply
	// knowledge of a ratio nobody has before generation.
	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  inTokens,
		usage.OutputTokens: maxOut,
	})

	est := usage.Estimate{Identity: id, Usage: u}
	switch {
	case counted && callerSet:
		// Input counted by OpenAI against the real request, output bounded by the
		// caller's own cap. Bounded above in both dimensions -- but conservative rather
		// than exact, since OpenAI does not promise the count matches the bill.
		est.Quality = usage.QualityConservative
	case counted:
		est.Quality = usage.QualityConservative
		note = join(note, fmt.Sprintf("the request set no max_output_tokens, so output was bounded by throttle's assumption of %d", maxOut))
	default:
		// The input count is a guess, so the whole estimate is a guess: actual usage may
		// exceed it in either dimension.
		est.Quality = usage.QualityHeuristic
	}
	if p.carriesHistory {
		// OpenAI prepends the prior turns server-side, so the real input is larger than
		// anything measurable in this request. The counter accounts for it; the heuristic
		// cannot, and saying so is more useful than a quietly low number.
		if counted {
			note = join(note, "the count includes conversation history prepended by OpenAI")
		} else {
			est.Quality = usage.QualityHeuristic
			note = join(note, "this request continues a previous response or conversation, whose history OpenAI prepends server-side and this estimate cannot see")
		}
	}
	est.Note = note

	// The rates are captured here, at admission, and settlement replays them. If the
	// catalog were re-queried after the call, a price refresh landing mid-request would
	// reserve against one price sheet and charge against another, with nothing in the
	// record to show it happened.
	at := c.engine.Now()
	if c.rates != nil {
		captured, err := c.rates.Capture(id, at)
		if err != nil {
			// An unpriceable model yields a known usage estimate with an explicitly
			// unknown cost. That is a legitimate state, not a failure: the engine decides
			// what to do with it according to enforcement posture.
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

// maxOutputTokens returns the output ceiling to estimate against, and whether the
// caller set it.
//
// A caller-set cap is a real bound on the request; throttle's default is an
// assumption, and the difference is reported in the estimate. Neither is ever sent:
// this figure informs the reservation, not the request.
func (c *Client) maxOutputTokens(p params) (int64, bool) {
	if p.maxOutputTokens != nil {
		return *p.maxOutputTokens, true
	}
	return c.maxOut, false
}

// countInput returns the input token count, whether OpenAI counted it, and a note
// explaining the method.
func (c *Client) countInput(ctx context.Context, in responses.ResponseNewParams) (n int64, counted bool, note string) {
	p := responseParams(in)
	if c.counter == nil {
		return estimateInputTokens(p), false,
			"input estimated from content length; enable Config.Counter for a count from OpenAI"
	}
	out, err := c.counter.Count(ctx, countParams(in))
	switch {
	case err == nil && out != nil:
		return out.InputTokens, true,
			"input counted by OpenAI via responses/input_tokens, which counts the request including tool schemas; OpenAI does not document this count as equal to the billed count"
	case ctx.Err() != nil:
		// The caller is going away; do not spend more of their deadline, and do not
		// pretend the fallback is as good.
		return estimateInputTokens(p), false,
			fmt.Sprintf("the input count was interrupted (%v), so input was estimated from content length", ctx.Err())
	default:
		// A counting failure must not fail the request. It degrades the estimate, and
		// the estimate says so.
		return estimateInputTokens(p), false,
			fmt.Sprintf("the input count failed (%v), so input was estimated from content length", err)
	}
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

// joinAnd renders a list for a human-readable reason string.
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		out := ""
		for i, s := range items[:len(items)-1] {
			if i > 0 {
				out += ", "
			}
			out += s
		}
		return out + ", and " + items[len(items)-1]
	}
}

// Observe normalizes a Responses reply into observed usage, without pricing it. It
// exists for callers reconciling a response they obtained outside the governed path.
func (c *Client) Observe(_ context.Context, out *responses.Response) (usage.Actual, error) {
	if out == nil {
		return usage.Actual{}, errors.New("openai: a response is required")
	}
	u, err := normalizeUsage(out.Usage)
	if err != nil {
		return usage.Actual{}, err
	}
	return usage.Actual{
		Identity: Identify(modelOf(out.Model), tierOf(out.ServiceTier)),
		Usage:    u,
		Cost:     usage.UnknownCost("not priced: Observe reports usage only"),
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
