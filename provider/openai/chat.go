package openai

import (
	"context"
	"errors"
	"fmt"
	"time"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// ChatRequest is a governed Chat Completions call.
type ChatRequest struct {
	// BudgetID is the budget to charge. Required.
	BudgetID string

	// Params is the SDK request, passed to OpenAI unchanged. Required.
	//
	// Unchanged is meant literally, and the list of things throttle does not do to a
	// Chat Completions request is longer than for Responses because the API offers more
	// to meddle with. throttle does not adjust MaxCompletionTokens or MaxTokens, does
	// not migrate the deprecated one to the current one, does not add a cap to a request
	// that declared none, does not touch N, ServiceTier, Store, Prediction,
	// PromptCacheKey, Modalities, Audio, Tools, or ToolChoice, and adds no field of its
	// own. Governing a request is not the same as editing it, and a caller who cannot
	// predict what reaches OpenAI cannot reason about their own bill.
	Params oai.ChatCompletionNewParams

	// RequestID identifies this call for reconciliation. It is also the basis of the
	// reservation ID, so retrying an ambiguous failure with the same RequestID is
	// idempotent rather than double-reserving. Generated if empty.
	RequestID string

	// Metadata is recorded on the reservation and charge, for attributing spend to a
	// workload. Message and response content is never recorded.
	Metadata map[string]string

	// Options are passed through to the SDK call.
	Options []option.RequestOption
}

// Complete makes a governed, non-streaming Chat Completions call.
//
// The request is the SDK's own ChatCompletionNewParams and the reply is the SDK's own
// *ChatCompletion. Written out, a call reads as
//
//	native OpenAI client -> throttle OpenAI wrapper -> Chat.Completions.New
//
// which is the point: the code still looks like OpenAI Chat Completions, because it is.
//
// Responses is the newer and preferred surface -- OpenAI's own SDK recommends it for new
// projects, and so does throttle. This exists because real applications use Chat
// Completions and a budget that cannot see their spend is not governing anything.
//
// # Failure handling
//
// The rules are the Responses adapter's rules, reached through the same code, because
// they are properties of throttle's accounting model rather than of an API family. What
// is specific to Chat Completions is how the provider expresses each condition:
//
//   - Refused by budget: nothing is reserved and nothing is called, but the refusal is
//     recorded. A request whose complete exposure is unknown -- an unpriced model, or
//     audio with no captured audio rates -- is refused here under enforce, before
//     OpenAI is called.
//   - Provider returns an error with no usage: the hold is released, because nothing was
//     billed. An OpenAI 429 lands here, recorded as a provider error rather than a
//     budget denial.
//   - Provider returns an error *and* usage: the usage is settled. A partially served
//     request OpenAI billed for is real spend.
//   - Generation stopped early -- the cap was reached, a content filter intervened, the
//     model refused -- and usage was reported: the usage is charged. A finish reason
//     describes the generation, not the bill. There is no release path keyed on a
//     finish reason, deliberately: whenever authoritative usage exists it is accounted.
//   - Tools or request options whose charges OpenAI does not report, or audio tokens
//     with no captured rate: settled as a partial cost with a text floor and the
//     unpriceable dimensions named. Never as a completed price.
//   - Provider succeeds but settlement fails: the completion is returned alongside
//     ErrAccounting. The hold stays outstanding rather than being released, since
//     releasing would erase spend that happened.
//   - Context cancelled or deadline exceeded mid-call: the outcome is genuinely
//     ambiguous -- OpenAI cannot cancel a synchronous completion server-side, so the
//     request may well have been served and billed -- and the hold is left outstanding
//     with ErrOutcomeUnknown.
//
// Releasing and settling use a context detached from the caller's, so a cancelled or
// timed-out request can still finish its bookkeeping.
func (c *Client) Complete(ctx context.Context, req ChatRequest) (*ChatResult, error) {
	if c.chatAPI == nil {
		return nil, ErrNoChatClient
	}
	if req.BudgetID == "" {
		return nil, errors.New("openai: budget id is required")
	}
	if chatModelOf(req.Params.Model) == "" {
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

	p := chatCompletionParams(req.Params)
	est, quote, exposure := c.estimateChat(ctx, p)
	res := &ChatResult{Accounting: Accounting{Identity: est.Identity, Estimate: est, Quote: quote}}

	start := c.engine.Now()
	adm, err := c.admit(ctx, req.BudgetID, requestID, est, &res.Accounting, req.Metadata)
	if err != nil {
		return res, err
	}
	rec := adm.rec

	out, callErr := c.chatAPI.New(ctx, req.Params, req.Options...)
	res.Completion = out
	res.Latency = c.engine.Now().Sub(start)
	rec.Latency = res.Latency
	rec.CompletedAt = c.engine.Now()

	// A completion with usage is billable even if the call also errored, so usage is
	// checked before the error is.
	if !hasChatUsage(out) {
		return res, c.completeWithoutUsage(ctx, adm, res, out, callErr)
	}

	// The completion's own identity is authoritative for what actually ran: OpenAI
	// reports the tier that served the call, which can differ from the one asked for and
	// can change the price, and it reports the model it used.
	if tier := chatTierOf(out.ServiceTier); tier != "" {
		res.Identity.ServiceTier = tier
	}
	if served := chatModelOf(out.Model); served != "" && served != res.Identity.ProviderModelID {
		res.ServedModelID = served
	}

	u, normErr := normalizeChatUsage(out.Usage)
	if normErr != nil {
		return res, c.inconsistentUsage(&res.Accounting, rec, adm.settleCtx, u, normErr)
	}

	if err := c.settleUsage(&res.Accounting, &rec, adm.settleCtx, adm.tx, u, exposure, callErr); err != nil {
		return res, err
	}

	// Settled. A completion that stopped early yet reported usage is settled on that
	// usage and the caller is told why it stopped -- which keeps "we charged for a
	// truncated answer" legible later, without ever having been a reason not to charge.
	if outcome := chatStatusOutcome(out); outcome != "" {
		rec.Outcome = outcome
		rec.Error = chatStopReason(out)
	}
	c.record(adm.settleCtx, rec, false)
	return res, chatStatusError(out)
}

// completeWithoutUsage handles every path where the provider reported no usage.
//
// Three endings, and they differ in what is known rather than in how the code is
// arranged: an interrupted call, a provider error, and a completion that came back
// looking fine but reported nothing. Each delegates to the shared handler, so a Chat
// Completions request in one of these states is recorded identically to a Responses
// request in the same state.
//
// There is deliberately no fourth case for "the completion came back with a non-stop
// finish reason and no usage". Unlike a Responses response, which carries an explicit
// status, a completion's finish reason describes generation and cannot distinguish a
// request that was never served from one that was served and truncated. Releasing on it
// would guess, and guessing in the direction of "free" is the expensive guess.
func (c *Client) completeWithoutUsage(ctx context.Context, adm *admission, res *ChatResult, out *oai.ChatCompletion, callErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return c.interrupted(&res.Accounting, adm.rec, adm.settleCtx, ctxErr, callErr)
	}
	if callErr != nil {
		return c.providerErrorWithoutUsage(&res.Accounting, adm.rec, adm.settleCtx, adm.tx, callErr)
	}
	// A completion with no usage metadata is not something OpenAI should produce. If it
	// also reports a non-natural finish, that is recorded as the reason, but the
	// accounting outcome is the same: unresolvable, hold left standing.
	rec := adm.rec
	if outcome := chatStatusOutcome(out); outcome != "" {
		rec.Outcome = outcome
	}
	return c.noUsageMetadata(&res.Accounting, rec, adm.settleCtx)
}

// EstimateChat predicts what a Chat Completions call will consume and cost.
//
// No Chat Completions estimate is ever QualityExact, and it is weaker than the
// Responses equivalent on the input side: OpenAI publishes no input-token counting
// endpoint for this API family -- the counter is defined on the Responses resource and
// takes Responses-shaped input -- so the input half is always a heuristic measured from
// content length. Feeding these messages to the Responses counter would count a
// different request.
func (c *Client) EstimateChat(ctx context.Context, in oai.ChatCompletionNewParams) (usage.Estimate, error) {
	if chatModelOf(in.Model) == "" {
		return usage.Estimate{}, errors.New("openai: a request with a model is required")
	}
	est, _, _ := c.estimateChat(ctx, chatCompletionParams(in))
	return est, nil
}

// estimateChat builds the estimate, the captured quote, and the request's exposure.
//
// The three are produced together because the exposure depends on the quote -- whether
// audio has a captured rate is a fact about the frozen rates, not about the request
// alone -- and the estimate's cost depends on the exposure. Returning them separately
// computed would let the reservation and the settlement disagree about what this request
// could be priced for.
//
// # The output ceiling
//
// Output tokens are unknowable before generation, so some ceiling is required or there
// is nothing to reserve against. The caller's own cap is used when they set one, from
// either max_completion_tokens or the deprecated max_tokens; otherwise throttle's
// assumption, reported as such. Neither is ever sent.
//
// The ceiling is multiplied by n. OpenAI charges for the tokens generated across all of
// the choices, so a request for five completions has five times the output exposure, and
// a reservation that ignored it would authorize a fifth of what the request can spend.
// The multiplication happens here and only here: settlement uses the provider's reported
// usage, which already reflects what was actually generated across every choice, and
// multiplying that again would charge five times for work billed once.
func (c *Client) estimateChat(ctx context.Context, p chatParams) (usage.Estimate, pricing.CapturedQuote, exposure) {
	id := Identify(p.modelID, p.serviceTier)
	id.Operation = p.operation

	perChoice, callerSet := c.chatOutputCeiling(p)
	// n multiplies potential output exposure. int64 throughout, and the product is not
	// bounded here: a caller who asks for a great many choices of a large cap has a
	// genuinely large exposure, and quietly capping it would understate what they
	// authorized.
	maxOut := perChoice * p.choices
	inTokens := estimateChatInputTokens(p)

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  inTokens,
		usage.OutputTokens: maxOut,
	})

	est := usage.Estimate{Identity: id, Usage: u}

	// Heuristic on the input side always, since there is no counter for this API. The
	// output side being a caller-declared bound does not rescue that: actual input may
	// exceed the guess, so the estimate as a whole is not a bound.
	est.Quality = usage.QualityHeuristic
	note := "input estimated from message content length; OpenAI publishes no input-token count endpoint for Chat Completions"
	if callerSet {
		note = join(note, fmt.Sprintf("output bounded by the request's %s", p.capField))
	} else {
		note = join(note, fmt.Sprintf("the request set no output cap, so output was bounded by throttle's assumption of %d", perChoice))
	}
	if p.choices > 1 {
		note = join(note, fmt.Sprintf("n is %d, and OpenAI charges for the tokens generated across all choices, so the output ceiling is multiplied accordingly", p.choices))
	}
	est.Note = note

	at := c.engine.Now()
	quote := c.captureChat(ctx, id, u, &est, at)

	// Exposure is classified once the quote exists, and it is what turns a request
	// throttle cannot fully price into one the engine will refuse under enforce. Applied
	// to the estimate's cost rather than to a flag of its own: the admission gate already
	// refuses an estimate whose cost is not Known, and routing this through the same
	// mechanism is why audio needs no new lifecycle.
	exp := classifyChatRequest(p, quote)
	if !exp.complete() {
		est.Cost = exp.downgrade(est.Cost)
	}
	return est, quote, exp
}

// captureChat freezes the rates this request will be priced by, setting the estimate's
// cost as a side effect.
//
// The rates are captured at admission and settlement replays them. If the catalog were
// re-queried after the call, a price refresh landing mid-request would reserve against
// one price sheet and charge against another, with nothing in the record to show it
// happened.
func (c *Client) captureChat(ctx context.Context, id usage.ModelIdentity, u usage.Usage, est *usage.Estimate, at time.Time) pricing.CapturedQuote {
	if c.rates != nil {
		captured, err := c.rates.Capture(id, at)
		if err != nil {
			// An unpriceable model yields a known usage estimate with an explicitly unknown
			// cost. A legitimate state, not a failure: the engine decides what to do with it
			// according to enforcement posture.
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

// chatOutputCeiling returns the per-choice output ceiling and whether the caller set it.
//
// Per choice, not per request: n is applied by the caller of this function, so that the
// note can report the cap the caller actually wrote alongside the multiplier separately.
func (c *Client) chatOutputCeiling(p chatParams) (int64, bool) {
	if p.maxOutputTokens != nil {
		return *p.maxOutputTokens, true
	}
	return c.maxOut, false
}

// ObserveChat normalizes a Chat Completions reply into observed usage, without pricing
// it. It exists for callers reconciling a completion they obtained outside the governed
// path.
func (c *Client) ObserveChat(_ context.Context, out *oai.ChatCompletion) (usage.Actual, error) {
	if out == nil {
		return usage.Actual{}, errors.New("openai: a completion is required")
	}
	u, err := normalizeChatUsage(out.Usage)
	if err != nil {
		return usage.Actual{}, err
	}
	id := Identify(chatModelOf(out.Model), chatTierOf(out.ServiceTier))
	id.Operation = OperationChatCompletions
	return usage.Actual{
		Identity: id,
		Usage:    u,
		Cost:     usage.UnknownCost("not priced: ObserveChat reports usage only"),
	}, nil
}
