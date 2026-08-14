// Package anthropic adapts Anthropic's own Messages API to throttle's accounting
// model.
//
// It is a shim, not a framework. A caller builds the SDK's own MessageNewParams
// exactly as they would for the Anthropic Go SDK, hands them to throttle with a
// budget ID, and gets back the SDK's own *anthropic.Message. Nothing about the
// request or response is reinterpreted, no field is rewritten, and no throttle
// abstraction stands between the caller and the model. Written out, a governed call
// reads as
//
//	native Anthropic client -> throttle Anthropic wrapper -> Messages.New
//
// There is deliberately no throttle.Generate, throttle.Chat, or throttle.Message
// here: the caller keeps thinking in Anthropic Messages concepts.
//
// What the shim adds is the transaction around the call:
//
//	estimate -> reserve -> execute -> reconcile
//
// # Why this provider is the interesting one
//
// throttle's engine is provider-neutral, and Bedrock and OpenAI are not a hard test
// of that claim: both bill in tokens, both report an inclusive input total, and both
// decompose it the same way. Anthropic's direct API does none of those things the same
// way, and every difference is a place a neutral engine could have needed a
// provider-shaped exception:
//
//   - Usage counters are additive and disjoint rather than inclusive. input_tokens
//     excludes what was read from cache and what was written to it; the SDK's own
//     Usage doc states the total is the sum of the three. Applying OpenAI's subtraction
//     here would price a 100,000-token cache read as 50 tokens. See normalizeUsage.
//   - Cache writes are priced by lifetime, so a five-minute write and a one-hour write
//     are different money for identical tokens. The response decomposes them, and the
//     request cannot be used to guess which happened. See normalizeUsage again.
//   - Inference geography re-rates every token category and is reported on the
//     response, so a request that named no geography can still be served in a priced
//     one. That is issue #30's rule on a second axis, and it is handled by the shared
//     pricing selector rather than here.
//   - Some server-side tools are billed in units the response cannot express.
//
// None of that reached the engine. The only shared-layer change this adapter needed
// was the geography axis on the existing price selector, which is a generalization of
// a rule that already existed rather than an Anthropic special case.
//
// # The boundary
//
// Anthropic SDK types stop here. This package converts an Anthropic reply into
// usage.Usage and usage.ModelIdentity, prices it through a pricing.Catalog, and hands
// money.Money to the engine. Nothing in budget, ledger, money, pricing, usage, engine,
// activity, reconcile, report, or dashboard imports an Anthropic type -- see
// provider/boundary_test.go, which asserts both that the neutral packages do not
// depend on the SDK and that this package does, so the absence cannot pass for the
// wrong reason.
//
// Direct Anthropic and Claude on Bedrock are deliberately *not* the same access
// provider. They share a publisher and a model family; they differ in rates, in model
// IDs, and in who sends the bill. Collapsing them would be the model-identity mistake
// throttle exists to avoid.
//
// # What this adapter cannot fully account for
//
// Two limits are structural rather than incidental, and the adapter reports both
// rather than papering over them:
//
// Code execution is billed by container time, with a monthly free allowance per
// organization. A response reports a container ID and an expiry, and no duration at
// all. So a request carrying that tool can never settle as a fully known cost: it is
// refused under enforce and settled as a partial cost with a token floor under
// monitor. See tools.go.
//
// A synchronous Messages call cannot be cancelled server-side. So a caller who gives
// up mid-call leaves a genuinely ambiguous outcome, handled the same conservative way
// the other adapters handle it: the hold stays outstanding rather than being released
// on a guess.
//
// # Credentials
//
// This package's tests never require Anthropic credentials. Client depends on narrow
// MessagesAPI and TokenCounter interfaces rather than on *anthropic.Client, so the
// whole transaction is exercised against a fake. The adapter never reads, holds, or
// persists a credential of any kind, and throttle's own configuration has no field for
// one: credentials are the SDK's business, resolved by anthropic.NewClient from its own
// chain -- an API key, an auth token, a named profile, or environment-var federation.
// throttle leaves all of it intact and does not clear or override any of it. The one
// test that talks to Anthropic is double-gated and skips cleanly without permission.
package anthropic

import (
	"context"
	"errors"
	"fmt"
	"time"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// MessagesAPI is the slice of the Anthropic client this adapter uses.
//
// A consumer-defined interface rather than *anthropic.Client: it documents exactly
// which call throttle makes, and it makes the entire governed path testable without
// credentials, a network, or a mocking framework. An anthropic.Client's Messages
// service satisfies it -- see Messages.
//
// Only the non-streaming create is here. Streaming Messages is deliberately not
// governed yet; a governed stream carries lifecycle obligations a single round trip
// does not, and it is separate work.
type MessagesAPI interface {
	New(ctx context.Context, body anth.MessageNewParams, opts ...option.RequestOption) (*anth.Message, error)
}

// TokenCounter is the optional preflight input token count.
//
// Separate from MessagesAPI because it is a distinct extra round trip a caller may
// reasonably decline: a client without it still works, with a weaker and
// honestly-labelled estimate. It is also why no network call happens merely because a
// credential exists -- counting is opt-in, per client, at construction.
//
// It counts the real request shape server-side -- including tool schemas, which no
// local approximation can measure -- so it is much better than the heuristic. It is
// emphatically not exact, and Anthropic says so in two directions: the count is
// documented as an estimate, and it "may include tokens added automatically by
// Anthropic for system optimizations" that are not billed. It also does not apply
// caching logic, so it counts cached prefixes at full price. Every one of those errors
// points the same way, which is what makes it usable as a conservative bound. See
// Client.countInput.
type TokenCounter interface {
	CountTokens(ctx context.Context, body anth.MessageCountTokensParams, opts ...option.RequestOption) (*anth.MessageTokensCount, error)
}

// Errors returned by this package.
var (
	// ErrNoClient means the adapter was built without a Messages client.
	ErrNoClient = errors.New("anthropic: no Messages client configured")

	// ErrProvider wraps an error returned by Anthropic itself, so a caller can tell a
	// provider failure from a budget refusal or an accounting failure.
	//
	// This distinction is load-bearing for rate limits and overload in particular: an
	// Anthropic 429 or 529 and throttle's own budget denial are different failure
	// domains, and a caller that cannot tell them apart will retry the wrong one.
	ErrProvider = errors.New("anthropic: provider call failed")

	// ErrAccounting means the provider call succeeded but throttle could not record
	// it. The message is still returned: the caller got their answer, and hiding the
	// bookkeeping failure would be worse than reporting it.
	ErrAccounting = errors.New("anthropic: request succeeded but could not be recorded")

	// ErrOutcomeUnknown means the call was interrupted in a way that leaves it
	// genuinely unknown whether Anthropic served and billed the request. The
	// reservation is deliberately left outstanding; see Client.NewMessage.
	ErrOutcomeUnknown = errors.New("anthropic: request outcome is unknown")

	// ErrCostUnresolved means the request ran and its usage is known, but its cost is
	// not fully priceable. The usage is recorded and the hold stays encumbered
	// awaiting reconciliation; the message is still returned.
	ErrCostUnresolved = errors.New("anthropic: request cost is unresolved")
)

// Config configures a Client.
//
// Deliberately absent: an output-token default. Every other adapter needs one,
// because every other API lets a caller omit the output cap. The Messages API makes
// max_tokens required, and documents 0 as a meaningful value -- it populates the
// prompt cache without generating a response -- so there is no "the caller set
// nothing" state to substitute an assumption for. Inventing one would replace a fact
// with a guess. See Client.estimate.
//
// Also deliberately absent: any credential field. See the package doc.
type Config struct {
	// Client is the Anthropic Messages service. Required.
	//
	// Wrap a real *anthropic.Client with Messages(c) to satisfy it.
	Client MessagesAPI

	// Counter enables preflight input token counting via Anthropic's
	// POST /v1/messages/count_tokens. Optional: it is an extra round trip per request,
	// so a caller opts in. Without it, input is estimated from content length.
	//
	// Wrap a real *anthropic.Client with Counter(c) to satisfy it.
	Counter TokenCounter

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
}

// Client is a governed Anthropic Messages client.
type Client struct {
	api      MessagesAPI
	counter  TokenCounter
	engine   *engine.Engine
	catalog  pricing.Catalog
	rates    pricing.RateSource
	activity activity.Store
}

// New builds a governed client.
func New(cfg Config) (*Client, error) {
	if cfg.Client == nil {
		return nil, ErrNoClient
	}
	if cfg.Engine == nil {
		return nil, errors.New("anthropic: an engine is required")
	}
	if cfg.Catalog == nil {
		return nil, errors.New("anthropic: a pricing catalog is required")
	}
	c := &Client{
		api:      cfg.Client,
		counter:  cfg.Counter,
		engine:   cfg.Engine,
		catalog:  cfg.Catalog,
		activity: cfg.Activity,
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

// Request is a governed Messages call.
type Request struct {
	// BudgetID is the budget to charge. Required.
	BudgetID string

	// Params is the SDK request, passed to Anthropic unchanged. Required.
	//
	// Unchanged is meant literally. throttle does not adjust MaxTokens -- not even to
	// lower it, and not even when the budget is nearly exhausted -- does not touch
	// Thinking, OutputConfig, ServiceTier, InferenceGeo, CacheControl, Container,
	// Tools, ToolChoice, StopSequences, or Metadata, and adds no field of its own.
	// Governing a request is not the same as editing it, and a caller who cannot
	// predict what reaches Anthropic cannot reason about their own bill.
	Params anth.MessageNewParams

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

// Result is the outcome of a governed Messages call: the provider's own message plus
// what throttle recorded about it.
type Result struct {
	// Message is the SDK response, unmodified. It may be non-nil even when NewMessage
	// returns an error, if the provider answered but accounting failed.
	Message *anth.Message

	// Accounting is what throttle recorded, in provider-neutral terms.
	Accounting
}

// Accounting is what throttle recorded about a governed call, in terms that carry no
// Anthropic type at all.
type Accounting struct {
	// Identity is what was actually called. ProviderModelID is the caller's exact
	// string; ServedModelID reports what Anthropic said it used, when that differs.
	// CanonicalModel is empty, which means throttle does not claim a normalized name
	// for the model, not that anything failed.
	//
	// ServiceTier and InferenceGeo are filled in from the response, because the
	// response is what says where the request actually ran and on what tier. Neither
	// is ever inferred from anything else. See identity.go.
	Identity usage.ModelIdentity

	// ServedModelID is the model Anthropic reported serving the request, when it
	// differs from the one requested -- typically an alias resolving to a dated
	// snapshot.
	//
	// Kept separate from Identity.ProviderModelID rather than overwriting it. Both are
	// facts and they answer different questions: what the caller asked for, and what
	// ran.
	ServedModelID string

	// Estimate is what was predicted, including its quality. Comparing it with Usage
	// is how estimate accuracy gets measured later.
	Estimate usage.Estimate

	// Usage is what the provider reported consuming, decomposed into throttle's
	// disjoint dimensions. See normalizeUsage: Anthropic's counters are already
	// disjoint, which is the opposite of OpenAI's.
	Usage usage.Usage

	// Cost is the priced actual. It may be legitimately unknown or partial while Usage
	// is fully known -- an unpriced model, a geography no rate was frozen for, or a
	// server-side tool whose charge the response does not report.
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
	// the SDK's own retries.
	//
	// There is no provider-reported counterpart: a Message carries no latency or
	// timing field of any kind, so unlike Bedrock nothing here can be attributed to
	// the provider. Recording zero for provider latency is the honest outcome;
	// deriving it from wall clock would invent a measurement.
	Latency time.Duration
}

// NewMessage makes a governed, non-streaming Messages call.
//
// The sequence is: estimate the cost, reserve it atomically across the budget chain,
// call Anthropic, then reconcile against what actually happened. The request and
// response are the SDK's own, and the name echoes the SDK call it wraps.
//
// # Failure handling
//
// Each failure mode gets the treatment that records reality rather than the one that
// simplifies the code. These are the rules the Bedrock and OpenAI adapters follow,
// reached through the same lifecycle code, because they are properties of throttle's
// accounting model rather than of a provider:
//
//   - Refused by budget: nothing is reserved and nothing is called, but the refusal
//     is recorded. A request whose complete exposure is unknown -- an unpriced model,
//     or a code-execution tool billed by container time -- is refused here under
//     enforce, before Anthropic is called.
//   - Provider returns an error with no usage: the hold is released, because nothing
//     was billed. An Anthropic 429 or 529 lands here, recorded as a provider error
//     rather than a budget denial. So does the SDK's own local pre-flight refusal of
//     an unstreamed request whose max_tokens implies more than ten minutes of
//     generation: that check runs before any HTTP request, so there is genuinely
//     nothing to charge and the hold is released in full.
//   - Provider returns an error *and* usage: the usage is settled. A partially served
//     request Anthropic billed for is real spend.
//   - Generation stopped early -- max_tokens reached, the context window exceeded, a
//     refusal, a paused turn -- and usage was reported: the usage is charged. A stop
//     reason describes the generation, not the bill, and there is deliberately no
//     release path keyed on one. Whenever authoritative usage exists it is accounted.
//   - A server-side tool whose charge the response cannot express: settled as a
//     partial cost with a token floor, naming what is missing. Never as a completed
//     price, and never as zero.
//   - Provider succeeds but settlement fails: the message is returned alongside
//     ErrAccounting. The hold stays outstanding rather than being released, since
//     releasing would erase spend that happened.
//   - Context cancelled or deadline exceeded mid-call: the outcome is genuinely
//     ambiguous -- Anthropic cannot cancel a synchronous message server-side, so the
//     request may well have been served and billed -- and the hold is left
//     outstanding with ErrOutcomeUnknown.
//
// Releasing and settling use a context detached from the caller's, so a cancelled or
// timed-out request can still finish its bookkeeping.
func (c *Client) NewMessage(ctx context.Context, req Request) (*Result, error) {
	if c.api == nil {
		return nil, ErrNoClient
	}
	if req.BudgetID == "" {
		return nil, errors.New("anthropic: budget id is required")
	}
	if modelOf(req.Params.Model) == "" {
		return nil, errors.New("anthropic: a request with a model is required")
	}
	// max_tokens is required by the API and 0 is a documented value, so there is no
	// unset state to default -- but a negative cap would make the output half of the
	// estimate negative, and reserving a negative amount is not something to paper
	// over. Anthropic would reject it; refusing before reserving is the same answer
	// without the round trip.
	if req.Params.MaxTokens < 0 {
		return nil, fmt.Errorf("anthropic: max_tokens is %d: a negative output cap cannot be reserved against", req.Params.MaxTokens)
	}

	requestID := req.RequestID
	if requestID == "" {
		id, err := newRequestID()
		if err != nil {
			return nil, fmt.Errorf("anthropic: generating a request id: %w", err)
		}
		requestID = id
	}

	p := messageParams(req.Params)
	est, quote, exposure := c.estimate(ctx, req.Params, p)
	res := &Result{Accounting: Accounting{Identity: est.Identity, Estimate: est, Quote: quote}}

	metadata := requestMetadata(req.Metadata, p)

	start := c.engine.Now()
	adm, err := c.admit(ctx, req.BudgetID, requestID, est, &res.Accounting, metadata)
	if err != nil {
		return res, err
	}
	rec := adm.rec

	out, callErr := c.api.New(ctx, req.Params, req.Options...)
	res.Message = out
	res.Latency = c.engine.Now().Sub(start)
	rec.Latency = res.Latency
	rec.CompletedAt = c.engine.Now()

	// A message with usage is billable even if the call also errored, so usage is
	// checked before the error is.
	if !hasUsage(out) {
		return res, c.finishWithoutUsage(ctx, adm, res, out, callErr)
	}

	// The response's own identity is authoritative for what actually ran. Both of
	// these can select a different price sheet than admission captured, and both are
	// read from the provider's own fields rather than derived from anything else.
	if geo := geoOf(out.Usage); geo != "" {
		res.Identity.InferenceGeo = geo
	}
	if tier := tierOf(out.Usage); tier != "" {
		res.Identity.ServiceTier = tier
	}
	if served := modelOf(out.Model); served != "" && served != res.Identity.ProviderModelID {
		res.ServedModelID = served
	}

	// A statistic with no price, recorded where a statistic belongs. See webFetchCount
	// for why this is not a usage dimension.
	if n, ok := webFetchCount(out.Usage); ok {
		rec.Metadata = withWebFetchCount(rec.Metadata, n)
	}

	u, normErr := normalizeUsage(out.Usage)
	if normErr != nil {
		return res, c.inconsistentUsage(&res.Accounting, rec, adm.settleCtx, u, normErr)
	}

	// The response can widen what throttle could not account for: an unknown usage
	// counter, a container the request did not obviously ask for, a server-side tool
	// invoked without appearing in Tools. Merged rather than replacing the
	// request-side exposure, since both are true.
	exposure = exposure.merge(observedExposure(out))

	if err := c.settleUsage(&res.Accounting, &rec, adm.settleCtx, adm.tx, u, exposure, callErr); err != nil {
		return res, err
	}

	// Settled. A message that stopped early yet reported usage is settled on that
	// usage and the caller is told why it stopped -- which keeps "we charged for a
	// truncated answer" legible later, without ever having been a reason not to
	// charge.
	if outcome := stopOutcome(out); outcome != "" {
		rec.Outcome = outcome
		rec.Error = stopReason(out)
	}
	c.record(adm.settleCtx, rec, false)
	return res, stopError(out)
}

// finishWithoutUsage handles every path where the provider reported no usage.
//
// Three endings, and they differ in what is known rather than in how the code is
// arranged: an interrupted call, a provider error, and a message that came back
// looking fine but reported nothing. Each delegates to the shared handler, so a
// Messages request in one of these states is recorded identically to a Responses or
// Chat Completions request in the same state.
//
// There is deliberately no fourth case releasing the hold on a stop reason. A
// Responses response carries an explicit status, so one that says it failed and
// reported no usage is evidence nothing was billed; a Message carries only a stop
// reason, which describes generation and cannot distinguish never-served from
// served-and-truncated. Releasing on it would guess, and guessing in the direction of
// "free" is the expensive guess.
func (c *Client) finishWithoutUsage(ctx context.Context, adm *admission, res *Result, out *anth.Message, callErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return c.interrupted(&res.Accounting, adm.rec, adm.settleCtx, ctxErr, callErr)
	}
	if callErr != nil {
		return c.providerErrorWithoutUsage(&res.Accounting, adm.rec, adm.settleCtx, adm.tx, callErr)
	}
	// A message with no usage metadata is not something Anthropic should produce: usage
	// is a required field of the response schema, and output_tokens is documented as
	// non-zero even for an empty completion. If it also reports an unnatural stop, that
	// is recorded as the reason, but the accounting outcome is the same: unresolvable,
	// hold left standing.
	rec := adm.rec
	if outcome := stopOutcome(out); outcome != "" {
		rec.Outcome = outcome
	}
	return c.noUsageMetadata(&res.Accounting, rec, adm.settleCtx)
}

// priceActual prices observed usage, preferring the quote captured at admission.
//
// Replaying the captured rates is what makes settlement reproducible: a catalog
// update between admission and settlement must not change what this request costs.
// Only a request that was never priceable falls back to the live catalog, and it has
// nothing to be inconsistent with.
//
// For is what selects the geography the provider actually served in. A geography no
// rate was frozen for is refused rather than priced at the admitted rates: the call
// happened and cost money, so the answer is an unknown cost that leaves the
// reservation encumbered, not a confident figure computed from a price sheet this
// request did not run under. That is #30's rule, and it is the shared implementation
// rather than a copy of it.
func (c *Client) priceActual(ctx context.Context, quote pricing.CapturedQuote, id usage.ModelIdentity, u usage.Usage) (usage.Cost, error) {
	if quote.Valid() {
		applicable, selErr := quote.For(id)
		if selErr != nil {
			return usage.UnknownCost(selErr.Error()), selErr
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

// Estimate predicts what a Messages call will consume and cost.
//
// No Messages estimate is ever QualityExact. Output tokens cannot be known before
// generation, and the input half is not exact either even when Anthropic counts it:
// count_tokens is documented as an estimate that may include system-added tokens the
// caller is not billed for. Calling it exact would assert a guarantee Anthropic has
// explicitly declined to make.
func (c *Client) Estimate(ctx context.Context, in anth.MessageNewParams) (usage.Estimate, error) {
	if modelOf(in.Model) == "" {
		return usage.Estimate{}, errors.New("anthropic: a request with a model is required")
	}
	if in.MaxTokens < 0 {
		return usage.Estimate{}, fmt.Errorf("anthropic: max_tokens is %d: a negative output cap cannot be estimated against", in.MaxTokens)
	}
	est, _, _ := c.estimate(ctx, in, messageParams(in))
	return est, nil
}

// estimate builds the estimate, the captured quote, and the request's exposure.
//
// The three are produced together because the estimate's cost depends on the
// exposure: a request throttle cannot fully price must not be admitted under enforce
// as though it could be, and the mechanism for that is the estimate's cost not being
// Known. Computing them separately would let the reservation and the settlement
// disagree about what this request could be priced for.
//
// # The output ceiling
//
// max_tokens is the ceiling, verbatim, always. This is the one adapter that needs no
// throttle-side assumption: the field is required, so a caller always states a bound,
// and Anthropic documents that thinking tokens count against the same cap -- so one
// figure covers visible output and thinking together, which is also how it bills.
//
// Zero is a real value, not an omission: Anthropic documents max_tokens: 0 as
// populating the prompt cache without generating a response. A request like that has
// no output exposure, and reserving throttle's default instead would reserve for
// generation the caller has explicitly asked not to happen.
func (c *Client) estimate(ctx context.Context, in anth.MessageNewParams, p params) (usage.Estimate, pricing.CapturedQuote, exposure) {
	id := Identify(p.modelID)
	// The geography the caller asked for, if any, and never anything else. An omitted
	// parameter leaves this empty rather than assuming the documented "global" default,
	// because a workspace default can resolve it server-side to a priced geography --
	// so the honest admission-time state is "not stated", and the response reports what
	// actually happened.
	id.InferenceGeo = p.inferenceGeo

	inTokens, counted, note := c.countInput(ctx, in, p)

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  inTokens,
		usage.OutputTokens: p.maxTokens,
	})

	est := usage.Estimate{Identity: id, Usage: u}
	if counted {
		// Bounded above on both halves: Anthropic counted the real request shape, and
		// its documented error directions all overcount, while output is bounded by the
		// caller's own required cap. Conservative rather than exact, since the count is
		// documented as an estimate.
		est.Quality = usage.QualityConservative
	} else {
		// The input count is a guess, so the whole estimate is a guess: actual input may
		// exceed it even though output cannot.
		est.Quality = usage.QualityHeuristic
	}
	note = join(note, fmt.Sprintf("output bounded by the request's own max_tokens of %d, which Anthropic documents as covering thinking tokens as well as visible output", p.maxTokens))
	if p.cachedBlocks > 0 && counted {
		// Worth saying, because it is the one direction the count is wrong by a large
		// factor rather than a small one: a cached prefix is counted at full price here
		// and billed at a tenth of it.
		note = join(note, "the count does not apply prompt caching, so a request with cache breakpoints is estimated as though nothing were cached")
	}
	est.Note = note

	at := c.engine.Now()
	quote := c.capture(ctx, id, u, &est, at)

	// Exposure is classified from the request, before the call, because that is when
	// it is knowable and when it has to be acted on: a container-time charge is
	// incurred by asking for the tool, and no field of the reply reports it. Applied to
	// the estimate's cost rather than to a flag of its own, so the existing admission
	// gate refuses it under enforce with no new lifecycle.
	exp := classifyTools(p.tools)
	if !exp.complete() {
		est.Cost = exp.downgrade(est.Cost)
	}
	return est, quote, exp
}

// capture freezes the rates this request will be priced by, setting the estimate's
// cost as a side effect.
//
// The rates are captured at admission and settlement replays them. If the catalog
// were re-queried after the call, a price refresh landing mid-request would reserve
// against one price sheet and charge against another, with nothing in the record to
// show it happened. It is also what makes the geography selection at settlement a
// replay rather than a fresh lookup: the alternates for every priced geography are
// frozen here, in the same read.
func (c *Client) capture(ctx context.Context, id usage.ModelIdentity, u usage.Usage, est *usage.Estimate, at time.Time) pricing.CapturedQuote {
	if c.rates != nil {
		captured, err := c.rates.Capture(id, at)
		if err != nil {
			// An unpriceable model yields a known usage estimate with an explicitly
			// unknown cost. A legitimate state, not a failure: the engine decides what to
			// do with it according to enforcement posture. An unrecognized model is never
			// a free one.
			est.Cost = usage.UnknownCost(err.Error())
			return pricing.CapturedQuote{}
		}
		priced, _ := captured.Price(u)
		est.Cost = priced.Cost
		return captured
	}
	quote, _ := c.catalog.Quote(ctx, id, u, at)
	est.Cost = quote.Cost
	return quote.Captured
}

// countInput returns the input token count, whether Anthropic counted it, and a note
// explaining the method.
//
// Counting is a network call, so it happens only when the caller configured a counter.
// It never mutates the request: countParams builds a separate params object field by
// field, and nothing here writes to the caller's.
//
// A counting failure must not fail the request and must never be papered over with an
// invented number. It degrades the estimate to the heuristic, and the estimate says
// so -- which under enforce means the request is admitted against a weaker bound, not
// against a fabricated one.
func (c *Client) countInput(ctx context.Context, in anth.MessageNewParams, p params) (n int64, counted bool, note string) {
	if c.counter == nil {
		return estimateInputTokens(p), false,
			"input estimated from content length; enable Config.Counter for a count from Anthropic"
	}
	out, err := c.counter.CountTokens(ctx, countParams(in))
	switch {
	case err == nil && out != nil:
		return out.InputTokens, true,
			"input counted by Anthropic via messages/count_tokens, which counts the request including tool schemas; Anthropic documents the result as an estimate that may include system-added tokens you are not billed for"
	case ctx.Err() != nil:
		// The caller is going away; do not spend more of their deadline, and do not
		// pretend the fallback is as good.
		return estimateInputTokens(p), false,
			fmt.Sprintf("the input count was interrupted (%v), so input was estimated from content length", ctx.Err())
	default:
		return estimateInputTokens(p), false,
			fmt.Sprintf("the input count failed (%v), so input was estimated from content length", err)
	}
}

// Observe normalizes a Messages reply into observed usage, without pricing it. It
// exists for callers reconciling a message they obtained outside the governed path.
func (c *Client) Observe(_ context.Context, out *anth.Message) (usage.Actual, error) {
	if out == nil {
		return usage.Actual{}, errors.New("anthropic: a message is required")
	}
	u, err := normalizeUsage(out.Usage)
	if err != nil {
		return usage.Actual{}, err
	}
	id := Identify(modelOf(out.Model))
	id.InferenceGeo = geoOf(out.Usage)
	id.ServiceTier = tierOf(out.Usage)
	return usage.Actual{
		Identity: id,
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
