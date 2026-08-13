package openai_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/usage"
)

// The happy path: a Chat Completions request is estimated, reserved, executed, and
// settled at the priced actual, using the same catalog and the same ledger a Responses
// request uses.
//
// The arithmetic is deliberately identical to TestRespondSettlesActualCost. The same
// model, the same token counts, and the same expected charge, because pricing is a
// property of the model and the tokens rather than of which endpoint delivered them --
// and OpenAI's own pricing page says the endpoints are not priced separately. A
// divergence here would mean the second API family had acquired a second accounting
// engine.
func TestCompleteSettlesActualCost(t *testing.T) {
	h := newChatHarness(t, "1000")

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID:  "team",
		RequestID: "chat-1",
		Params:    chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !res.Settled {
		t.Fatal("the request should have settled")
	}
	if res.Completion == nil {
		t.Fatal("the SDK completion must be passed through to the caller")
	}

	// 1000 prompt at $1.25/M plus 500 completion at $10.00/M = $0.00125 + $0.005.
	want := dollars(t, "0.00625")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Errorf("cost should be known for a fixture-priced model with no tools: %s", res.Cost.Reason)
	}
	if res.Estimate.Cost.Amount <= res.Charge.ActualCost {
		t.Errorf("estimate %s should exceed actual %s for a capped request",
			res.Estimate.Cost.Amount, res.Charge.ActualCost)
	}

	tot := h.totals(t)
	if tot.Spent != want {
		t.Errorf("ledger Spent = %s, want %s", tot.Spent, want)
	}
	if tot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0: the hold must be consumed by settlement", tot.Reserved)
	}
}

// A Chat Completions charge and a Responses charge for the same model and the same token
// counts are equal, and both land in the same ledger.
//
// This is the acceptance criterion about the catalog saying it once. The captured quote
// carries no operation field, so one catalog fact serves both families -- and the way to
// prove that is not to inspect the catalog but to charge through both paths and compare.
func TestBothAPIFamiliesPriceFromOneCatalogFact(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.api.out = completedResponse(t, gpt51, 1000, 500)
	h.chat.out = completion(t, gpt51, 1000, 500)

	viaResponses, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "both-resp", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	viaChat, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "both-chat", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if viaResponses.Charge.ActualCost != viaChat.Charge.ActualCost {
		t.Errorf("Responses charged %s and Chat Completions charged %s for identical usage on "+
			"identical model: OpenAI does not price its endpoints separately, so one catalog fact "+
			"must serve both", viaResponses.Charge.ActualCost, viaChat.Charge.ActualCost)
	}
	// The same frozen provenance, from the same catalog row.
	if viaResponses.Quote.Provenance != viaChat.Quote.Provenance {
		t.Errorf("the two families captured different provenance (%+v vs %+v): the rates are "+
			"one fact about the model", viaResponses.Quote.Provenance, viaChat.Quote.Provenance)
	}

	// And the budget saw both.
	if got, want := h.totals(t).Spent, viaResponses.Charge.ActualCost+viaChat.Charge.ActualCost; got != want {
		t.Errorf("Spent = %s, want %s: both families charge the same budget", got, want)
	}
}

// The operation distinguishes the three governed calls, and it is the only thing that
// does. Nothing was added to the neutral schema to carry an "API family".
func TestChatOperationIsDistinct(t *testing.T) {
	h := newChatHarness(t, "1000")

	if _, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-op", Params: chatRequest(gpt51, maxOut(2000)),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "resp-op", Params: request(gpt51, maxOut(2000)),
	}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	chat := h.record(t, "chat-op")
	resp := h.record(t, "resp-op")

	if chat.Identity.Operation != "chat-completions" {
		t.Errorf("chat operation = %q, want chat-completions", chat.Identity.Operation)
	}
	if resp.Identity.Operation != "responses" {
		t.Errorf("responses operation = %q, want responses", resp.Identity.Operation)
	}
	if chat.Identity.Operation == resp.Identity.Operation {
		t.Error("the two API families must be distinguishable in the durable record")
	}
	// Everything else about the identity is the same, because it is the same model
	// reached through the same access path.
	if chat.Identity.AccessProvider != resp.Identity.AccessProvider ||
		chat.Identity.Publisher != resp.Identity.Publisher ||
		chat.Identity.ProviderModelID != resp.Identity.ProviderModelID {
		t.Errorf("the two records disagree about a model they share: %+v vs %+v",
			chat.Identity, resp.Identity)
	}
}

// Settlement happens exactly once. Double settlement would double the recorded spend,
// and the ledger's charge rows are the only place that shows it.
func TestChatSettlesExactlyOnce(t *testing.T) {
	h := newChatHarness(t, "1000")

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-once", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	p, err := h.ledger.EnsurePeriod(context.Background(), "team", h.clock())
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	charges, err := h.ledger.Charges(context.Background(),
		ledger.Scope{BudgetID: "team", PeriodID: p.ID}, now.Add(-time.Hour), now.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("Charges: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("got %d charges for one request, want exactly 1", len(charges))
	}
	if got := h.totals(t).Spent; got != res.Charge.ActualCost {
		t.Errorf("Spent = %s, want %s: settlement must not be applied twice", got, res.Charge.ActualCost)
	}
}

// Cached prompt tokens are a subset of prompt_tokens, not an addition to them. This is
// the Chat Completions object's own inclusion relationship, verified here rather than
// inherited from the Responses test.
func TestChatCachedPromptIsNotDoubleCharged(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_cache", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {
			"prompt_tokens": 1000,
			"prompt_tokens_details": {"cached_tokens": 400},
			"completion_tokens": 500,
			"total_tokens": 1500
		}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-cache", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got, _ := res.Usage.Get(usage.InputTokens); got != 600 {
		t.Errorf("InputTokens = %d, want 600 (1000 prompt minus 400 cached)", got)
	}
	if got, _ := res.Usage.Get(usage.CacheReadTokens); got != 400 {
		t.Errorf("CacheReadTokens = %d, want 400", got)
	}

	// 600 fresh at $1.25/M = $0.00075; 400 cached at $0.125/M = $0.00005; 500 output at
	// $10/M = $0.005. The wrong reading -- pricing all 1000 reported prompt tokens *and*
	// the 400 cached ones -- comes to $0.0063.
	want := dollars(t, "0.0058")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (double-charging cached tokens gives $0.0063)",
			res.Charge.ActualCost, want)
	}
}

// Cache writes are also inclusive details of prompt_tokens, and dearer rather than
// cheaper. The model here is the only fixture family publishing a cache-write price.
func TestChatCacheWriteIsNotDoubleCharged(t *testing.T) {
	h := newChatHarness(t, "1000")
	const luna = "gpt-5.6-luna" // $0.20/M input, $0.02/M cached, $0.25/M cache write, $1.20/M output
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_cw", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {
			"prompt_tokens": 10000,
			"prompt_tokens_details": {"cached_tokens": 2000, "cache_write_tokens": 3000},
			"completion_tokens": 1000,
			"total_tokens": 11000
		}
	}`, luna))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-cw", Params: chatRequest(luna, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// The three prompt dimensions are disjoint and sum to the reported total.
	fresh, _ := res.Usage.Get(usage.InputTokens)
	cached, _ := res.Usage.Get(usage.CacheReadTokens)
	written, _ := res.Usage.Get(usage.CacheWriteTokens)
	if fresh != 5000 || cached != 2000 || written != 3000 {
		t.Errorf("prompt decomposition = %d fresh / %d cached / %d written, want 5000/2000/3000",
			fresh, cached, written)
	}
	if fresh+cached+written != 10000 {
		t.Errorf("the prompt dimensions sum to %d, want the reported 10000", fresh+cached+written)
	}

	// 5000 at $0.20/M = $0.001; 2000 at $0.02/M = $0.00004; 3000 at $0.25/M = $0.00075;
	// 1000 output at $1.20/M = $0.0012.
	if want := dollars(t, "0.00299"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
}

// Reasoning tokens are an inclusive detail of completion_tokens, kept as their own
// dimension and priced at the output rate. Subtracting them without pricing them would
// silently drop the charge for most of a reasoning model's output.
func TestChatReasoningTokensAreNotDoubleCharged(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_reason", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {
			"prompt_tokens": 1000,
			"completion_tokens": 500,
			"completion_tokens_details": {"reasoning_tokens": 300},
			"total_tokens": 1500
		}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-reason", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got, _ := res.Usage.Get(usage.OutputTokens); got != 200 {
		t.Errorf("OutputTokens = %d, want 200 (500 completion minus 300 reasoning)", got)
	}
	if got, _ := res.Usage.Get(usage.ReasoningTokens); got != 300 {
		t.Errorf("ReasoningTokens = %d, want 300: the count is visible without being charged twice", got)
	}
	// Identical to a request with no reasoning breakdown, because reasoning is billed at
	// the output rate: 1000 at $1.25/M + 500 at $10/M.
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: reasoning is part of the output it decomposes",
			res.Charge.ActualCost, want)
	}
}

// The load-bearing test for the difference between the two API families' usage objects.
//
// Rejected prediction tokens are an inclusive detail of completion_tokens, exactly like
// reasoning tokens -- and unlike reasoning tokens they carry no distinct rate. OpenAI
// bills them as ordinary completion tokens. So applying the Responses subtraction formula
// here would understate the charge by exactly the rejected count.
//
// The figures make the failure unmissable: 4000 of the 5000 completion tokens are
// rejected predictions, so a wrong reading charges a fifth of the truth.
func TestRejectedPredictionTokensDoNotLowerTheCharge(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_pred", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {
			"prompt_tokens": 1000,
			"completion_tokens": 5000,
			"completion_tokens_details": {"accepted_prediction_tokens": 200, "rejected_prediction_tokens": 4000},
			"total_tokens": 6000
		}
	}`, gpt51))

	withPrediction := chatRequest(gpt51, maxOut(8000))
	withPrediction.Prediction = oai.ChatCompletionPredictionContentParam{
		Content: oai.ChatCompletionPredictionContentContentUnionParam{
			OfString: param.NewOpt("a draft the model was asked to revise"),
		},
	}

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-pred", Params: withPrediction,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Every one of the 5000 completion tokens is charged at the output rate. None of them
	// is carved out for a rate that does not exist.
	if got, _ := res.Usage.Get(usage.OutputTokens); got != 5000 {
		t.Errorf("OutputTokens = %d, want 5000: prediction counters are a breakdown of the "+
			"completion tokens, and OpenAI bills rejected ones at the completion rate", got)
	}
	// 1000 at $1.25/M = $0.00125; 5000 at $10.00/M = $0.05.
	want := dollars(t, "0.05125")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s. Subtracting the 4000 rejected tokens the way the "+
			"Responses formula subtracts reasoning tokens would charge $0.01125 -- a fifth of "+
			"the real bill, and the error grows precisely as predicted outputs work less well",
			res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Errorf("a prediction request is fully priceable: %s", res.Cost.Reason)
	}

	// And no dimension was invented for a rate that does not exist. A dimension per
	// detail counter would force every catalog to carry two more rates and keep them
	// equal to the output rate forever.
	for _, d := range res.Usage.Dimensions() {
		if strings.Contains(string(d), "prediction") {
			t.Errorf("usage carries a %q dimension: prediction counters have no distinct rate, "+
				"so a dimension for them would assert a price distinction OpenAI does not make", d)
		}
	}
}

// total_tokens is never the pricing input. It sums dimensions carrying up to six
// different prices, so pricing from it is wrong in the ordinary case rather than the
// edge case.
//
// Checked by lying: the fixture reports a total_tokens that does not match its own parts.
// The charge must follow the parts.
func TestChatTotalTokensIsNotPriced(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_total", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 999999}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-total", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: the charge must come from the priced dimensions, "+
			"not from a total that sums differently-priced things", res.Charge.ActualCost, want)
	}
	for _, d := range res.Usage.Dimensions() {
		if strings.Contains(string(d), "total") {
			t.Errorf("usage carries a %q dimension: total_tokens is discarded", d)
		}
	}
}

// A usage object whose breakdown cannot fit inside its own totals is not trusted. The
// hold stays outstanding: the request ran, so releasing would assert zero spend, and the
// arithmetic gives nothing honest to charge.
func TestChatInconsistentUsageLeavesTheHoldOutstanding(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_bad", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {
			"prompt_tokens": 100,
			"prompt_tokens_details": {"cached_tokens": 900},
			"completion_tokens": 50,
			"total_tokens": 150
		}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-bad-usage", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrAccounting) {
		t.Fatalf("Complete error = %v, want ErrAccounting", err)
	}
	if res.Cost.Known() {
		t.Error("a contradictory usage object must not produce a known cost")
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0: there was nothing trustworthy to charge", got)
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: the request ran, so the hold must not be released")
	}
	rec := h.record(t, "chat-bad-usage")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
	if rec.Outcome != activity.OutcomeAccountingError {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeAccountingError)
	}
}

// Absent is not zero. A text model reports none of the optional detail counters, and the
// record must not claim the provider priced a cache read at nothing.
func TestChatAbsentDetailsAreNotRecordedAsZero(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = completion(t, gpt51, 1000, 500)

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-absent", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	for _, d := range []usage.Dimension{
		usage.CacheReadTokens, usage.CacheWriteTokens, usage.ReasoningTokens,
		usage.InputAudioTokens, usage.OutputAudioTokens,
	} {
		if _, ok := res.Usage.Get(d); ok {
			t.Errorf("%s is present in usage, but the provider never mentioned it: absence is "+
				"not a zero the provider reported", d)
		}
	}
	// A detail the provider *does* report as zero is present, because that is a fact it
	// stated.
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_zero", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {"prompt_tokens": 1000, "prompt_tokens_details": {"cached_tokens": 0},
			"completion_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	res, err = h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-zero", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := res.Usage.Get(usage.CacheReadTokens); !ok {
		t.Error("a cached count the provider explicitly reported as zero should be recorded as such")
	}
}

// A usage object with neither token total is treated as no usage at all, rather than as
// a free request.
func TestChatUsageWithNoTotalsIsNotFree(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_nousage", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}]
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-nousage", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrAccounting) {
		t.Fatalf("Complete error = %v, want ErrAccounting", err)
	}
	if res.Cost.Known() {
		t.Error("a completion that reported no usage must not produce a known cost")
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: the request ran, so the hold stays outstanding rather than being released")
	}
	if got := h.record(t, "chat-nousage").Outcome; got != activity.OutcomeAccountingError {
		t.Errorf("outcome = %q, want %q", got, activity.OutcomeAccountingError)
	}
}

// n multiplies the pre-admission output ceiling, because OpenAI charges for the tokens
// generated across all of the choices.
//
// Anti-vacuous by construction: the two estimates are compared to each other and to the
// exact multiple, so a change that dropped the multiplication fails rather than passing
// on a loose inequality.
func TestMultipleChoicesMultiplyExposure(t *testing.T) {
	h := newChatHarness(t, "1000")

	one := chatRequest(gpt51, maxOut(1000))
	five := chatRequest(gpt51, maxOut(1000))
	five.N = param.NewOpt(int64(5))

	estOne, err := h.client.EstimateChat(context.Background(), one)
	if err != nil {
		t.Fatalf("EstimateChat(n=1): %v", err)
	}
	estFive, err := h.client.EstimateChat(context.Background(), five)
	if err != nil {
		t.Fatalf("EstimateChat(n=5): %v", err)
	}

	outOne, _ := estOne.Usage.Get(usage.OutputTokens)
	outFive, _ := estFive.Usage.Get(usage.OutputTokens)
	if outOne != 1000 {
		t.Errorf("n=1 output ceiling = %d, want the caller's cap of 1000", outOne)
	}
	if outFive != 5000 {
		t.Errorf("n=5 output ceiling = %d, want 5000: OpenAI charges for the tokens generated "+
			"across all of the choices", outFive)
	}

	// The input half is identical, so the whole difference is the output multiplication.
	inOne, _ := estOne.Usage.Get(usage.InputTokens)
	inFive, _ := estFive.Usage.Get(usage.InputTokens)
	if inOne != inFive {
		t.Errorf("input estimate differs between n=1 (%d) and n=5 (%d): n multiplies output only",
			inOne, inFive)
	}
	// And the reserved money follows, which is the part that actually governs.
	if estFive.Cost.Amount <= estOne.Cost.Amount {
		t.Errorf("n=5 estimate %s is not above n=1 estimate %s: a reservation that ignored n "+
			"would authorize a fifth of what the request can spend",
			estFive.Cost.Amount, estOne.Cost.Amount)
	}
	if !strings.Contains(estFive.Note, "n is 5") {
		t.Errorf("estimate note %q should disclose the multiplier", estFive.Note)
	}
	if strings.Contains(estOne.Note, "n is") {
		t.Errorf("estimate note %q should not mention a multiplier for n=1", estOne.Note)
	}
}

// n does not multiply settlement. The provider's reported usage already reflects what was
// generated across every choice, so multiplying it again would charge five times for work
// billed once.
func TestMultipleChoicesDoNotMultiplyTheCharge(t *testing.T) {
	h := newChatHarness(t, "1000")

	five := chatRequest(gpt51, maxOut(1000))
	five.N = param.NewOpt(int64(5))
	// The provider reports the total across all five choices, which is what it bills.
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_n", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [
			{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "a"}},
			{"index": 1, "finish_reason": "stop", "message": {"role": "assistant", "content": "b"}},
			{"index": 2, "finish_reason": "stop", "message": {"role": "assistant", "content": "c"}},
			{"index": 3, "finish_reason": "stop", "message": {"role": "assistant", "content": "d"}},
			{"index": 4, "finish_reason": "stop", "message": {"role": "assistant", "content": "e"}}
		],
		"usage": {"prompt_tokens": 1000, "completion_tokens": 2500, "total_tokens": 3500}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-n5", Params: five,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got, _ := res.Usage.Get(usage.OutputTokens); got != 2500 {
		t.Errorf("OutputTokens = %d, want the reported 2500: settlement is driven by what the "+
			"provider says it generated", got)
	}
	// 1000 at $1.25/M = $0.00125; 2500 at $10/M = $0.025. Multiplying by n again would
	// charge $0.125 of output.
	if want := dollars(t, "0.02625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: the reported usage already covers every choice",
			res.Charge.ActualCost, want)
	}
	// The estimate was above the actual, which is what a conservative ceiling is for.
	if res.Estimate.Cost.Amount <= res.Charge.ActualCost {
		t.Errorf("estimate %s should exceed actual %s", res.Estimate.Cost.Amount, res.Charge.ActualCost)
	}
}

// The deprecated max_tokens is honoured as an output ceiling when it is the only cap the
// caller set. Treating it as absent would substitute throttle's assumption for a bound
// the caller actually declared.
func TestDeprecatedMaxTokensIsHonouredAsACap(t *testing.T) {
	h := newChatHarness(t, "1000")

	in := chatRequest(gpt51, nil)
	in.MaxTokens = param.NewOpt(int64(777))

	est, err := h.client.EstimateChat(context.Background(), in)
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if got, _ := est.Usage.Get(usage.OutputTokens); got != 777 {
		t.Errorf("output ceiling = %d, want the caller's max_tokens of 777", got)
	}
	if !strings.Contains(est.Note, "max_tokens") {
		t.Errorf("note %q should name which cap was honoured", est.Note)
	}

	// The current field wins when both are set, because that is the one OpenAI applies.
	in.MaxCompletionTokens = param.NewOpt(int64(1234))
	est, err = h.client.EstimateChat(context.Background(), in)
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if got, _ := est.Usage.Get(usage.OutputTokens); got != 1234 {
		t.Errorf("output ceiling = %d, want max_completion_tokens (1234) to win over max_tokens", got)
	}
	if !strings.Contains(est.Note, "max_completion_tokens") {
		t.Errorf("note %q should name max_completion_tokens", est.Note)
	}
}

// throttle does not rewrite the caller's caps, does not migrate the deprecated one, and
// does not add a cap to a request that declared none.
func TestChatRequestReachesProviderUnmodified(t *testing.T) {
	h := newChatHarness(t, "1000")

	in := chatRequest(gpt51, nil)
	in.MaxTokens = param.NewOpt(int64(777))
	in.N = param.NewOpt(int64(3))
	in.Store = param.NewOpt(true)
	in.PromptCacheKey = param.NewOpt("caller-chosen-key")
	h.chat.out = completion(t, gpt51, 100, 50)

	if _, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-unmodified", Params: in,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	sent := h.chat.lastParams()
	if sent.MaxCompletionTokens.Valid() {
		t.Errorf("throttle set max_completion_tokens to %d: the caller left it unset and a "+
			"migrated cap is still a cap they did not write", sent.MaxCompletionTokens.Value)
	}
	if !sent.MaxTokens.Valid() || sent.MaxTokens.Value != 777 {
		t.Errorf("max_tokens = %v, want the caller's 777 untouched", sent.MaxTokens)
	}
	if !sent.N.Valid() || sent.N.Value != 3 {
		t.Errorf("n = %v, want the caller's 3 untouched", sent.N)
	}
	if !sent.Store.Valid() || !sent.Store.Value {
		t.Errorf("store = %v, want the caller's value untouched", sent.Store)
	}
	if sent.PromptCacheKey.Value != "caller-chosen-key" {
		t.Errorf("prompt_cache_key = %q, want the caller's own", sent.PromptCacheKey.Value)
	}
	if got := chatModelString(sent.Model); got != gpt51 {
		t.Errorf("model = %q, want %q", got, gpt51)
	}
	if len(sent.Messages) != len(in.Messages) {
		t.Errorf("throttle changed the message count from %d to %d", len(in.Messages), len(sent.Messages))
	}
}

// An uncapped request is estimated against throttle's own assumption, disclosed as such,
// and the assumption never reaches OpenAI.
func TestUncappedChatRequestIsNotGivenACap(t *testing.T) {
	h := newChatHarness(t, "1000")

	est, err := h.client.EstimateChat(context.Background(), chatRequest(gpt51, nil))
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if got, _ := est.Usage.Get(usage.OutputTokens); got != openai.DefaultMaxOutputTokens {
		t.Errorf("output ceiling = %d, want the default assumption %d", got, openai.DefaultMaxOutputTokens)
	}
	if !strings.Contains(est.Note, "throttle's assumption") {
		t.Errorf("note %q must disclose that the ceiling is throttle's own assumption", est.Note)
	}
}

// Every Chat Completions estimate is heuristic. OpenAI publishes no input-token counting
// endpoint for this API family, so there is no better number to prefer, and calling the
// estimate exact would assert a guarantee nobody made.
func TestChatEstimateIsAlwaysHeuristic(t *testing.T) {
	h := newChatHarness(t, "1000")

	est, err := h.client.EstimateChat(context.Background(), chatRequest(gpt51, maxOut(2000)))
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if est.Quality != usage.QualityHeuristic {
		t.Errorf("Quality = %q, want %q", est.Quality, usage.QualityHeuristic)
	}
	if !strings.Contains(est.Note, "no input-token count endpoint") {
		t.Errorf("note %q should say why the input half cannot be counted", est.Note)
	}
	// The Responses counter is configured on this client and must not have been consulted:
	// it takes Responses-shaped input, so feeding it these messages would count a
	// different request.
	if h.counter.calls != 0 {
		t.Errorf("the Responses input-token counter was called %d times for a Chat Completions "+
			"request: it would be counting a different request", h.counter.calls)
	}
}

// An unknown model identifies completely without the catalog recognizing it, and its
// unknown price blocks execution under enforce rather than being treated as zero.
func TestChatUnknownModelIdentifiesAndBlocksUnderEnforce(t *testing.T) {
	h := newChatHarness(t, "1000")
	const unknown = "gpt-6-audio-preview-2027-01-01"

	est, err := h.client.EstimateChat(context.Background(), chatRequest(unknown, maxOut(2000)))
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if !est.Identity.Valid() {
		t.Error("an unknown model must still produce a valid identity")
	}
	if est.Identity.ProviderModelID != unknown {
		t.Errorf("ProviderModelID = %q, want the caller's exact string %q",
			est.Identity.ProviderModelID, unknown)
	}
	if est.Identity.Operation != "chat-completions" {
		t.Errorf("Operation = %q, want chat-completions", est.Identity.Operation)
	}
	if est.Identity.Known() {
		t.Error("Known() reports catalog recognition, which is absent here")
	}
	if est.Cost.Known() {
		t.Error("an unknown model's cost must be unknown, not zero")
	}
	if est.Usage.Empty() {
		t.Error("usage is still estimable for a model throttle does not recognize")
	}

	_, err = h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-unknown", Params: chatRequest(unknown, maxOut(2000)),
	})
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("Complete error = %v, want ErrCostUnknown", err)
	}
	if h.chat.callCount() != 0 {
		t.Error("enforce mode must not execute a request whose cost it cannot determine")
	}
	rec := h.record(t, "chat-unknown")
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeUnpriced)
	}
	if rec.Identity.ProviderModelID != unknown {
		t.Errorf("recorded model = %q, want %q: an operator has to see what was attempted",
			rec.Identity.ProviderModelID, unknown)
	}
}

// Under monitor the same unpriceable request executes, with its cost explicitly unknown.
func TestChatUnknownModelExecutesUnderMonitor(t *testing.T) {
	h := newMonitorChatHarness(t)
	const unknown = "gpt-6-audio-preview-2027-01-01"
	h.chat.out = completion(t, unknown, 1000, 500)

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-monitor", Params: chatRequest(unknown, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Complete error = %v, want ErrCostUnresolved", err)
	}
	if h.chat.callCount() != 1 {
		t.Error("monitor mode should have executed the request")
	}
	if res.Completion == nil {
		t.Error("the caller should still get their completion in monitor mode")
	}
	if got, _ := res.Usage.Get(usage.InputTokens); got != 1000 {
		t.Errorf("InputTokens = %d, want 1000: usage is knowable without a price", got)
	}
	if res.Cost.State() != usage.CostUnknown {
		t.Errorf("cost state = %v, want CostUnknown", res.Cost.State())
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s: an unpriceable request must not settle at a made-up amount", got)
	}
	rec := h.record(t, "chat-monitor")
	if rec.EnforcementMode != engine.ModeMonitor {
		t.Errorf("recorded mode = %q, want %q", rec.EnforcementMode, engine.ModeMonitor)
	}
	if rec.ActualUsage.Empty() {
		t.Error("usage must be persisted even though the cost is unknown")
	}
}

// A budget with no headroom refuses the request before OpenAI is called, and the refusal
// is a budget denial rather than a pricing failure.
func TestChatBudgetDenialDoesNotCallProvider(t *testing.T) {
	h := newChatHarness(t, "0.000001")

	_, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-denied", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err == nil {
		t.Fatal("Complete should have been refused by the budget")
	}
	if errors.Is(err, engine.ErrCostUnknown) {
		t.Errorf("error = %v, want a budget denial: this model is priced", err)
	}
	if h.chat.callCount() != 0 {
		t.Error("a denied request must not reach OpenAI")
	}
	rec := h.record(t, "chat-denied")
	if rec.Status != activity.StatusDenied {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusDenied)
	}
	if rec.Outcome != activity.OutcomeBudgetDenied {
		t.Errorf("outcome = %q, want %q: a full budget is not an unpriced model",
			rec.Outcome, activity.OutcomeBudgetDenied)
	}
}

// An OpenAI 429 is a provider error, not a budget denial, and releases the hold because
// nothing was billed. A caller that cannot tell the two apart retries the wrong one.
func TestChatRateLimitIsNotABudgetDenial(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = nil
	h.chat.err = apiError(t, http.StatusTooManyRequests, "rate_limit_exceeded", "requests",
		"Rate limit reached for gpt-5.1")

	_, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-429", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrProvider) {
		t.Fatalf("Complete error = %v, want ErrProvider", err)
	}
	if errors.Is(err, engine.ErrDenied) {
		t.Error("an OpenAI rate limit must not be reported as a budget denial")
	}
	if got := h.totals(t).Reserved; got != 0 {
		t.Errorf("Reserved = %s, want 0: nothing was billed, so the headroom goes back", got)
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}
	rec := h.record(t, "chat-429")
	if rec.Status != activity.StatusReleased {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusReleased)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeProviderError)
	}
	if !strings.Contains(rec.Error, "429") {
		t.Errorf("recorded error %q should keep the status an operator acts on", rec.Error)
	}
}

// The other provider failures that are not budget denials, each releasing the hold
// because none of them billed anything.
func TestChatProviderFailuresAreDistinguishedFromBudgetDenial(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		kind   string
	}{
		{"auth", http.StatusUnauthorized, "invalid_api_key", "invalid_request_error"},
		{"permission", http.StatusForbidden, "insufficient_permissions", "invalid_request_error"},
		{"invalid_request", http.StatusBadRequest, "invalid_value", "invalid_request_error"},
		{"model_unavailable", http.StatusNotFound, "model_not_found", "invalid_request_error"},
		{"server_failure", http.StatusInternalServerError, "server_error", "server_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t, "1000")
			h.chat.out = nil
			h.chat.err = apiError(t, tc.status, tc.code, tc.kind, "the provider refused")

			_, err := h.client.Complete(context.Background(), openai.ChatRequest{
				BudgetID: "team", RequestID: "chat-" + tc.name, Params: chatRequest(gpt51, maxOut(2000)),
			})
			if !errors.Is(err, openai.ErrProvider) {
				t.Fatalf("Complete error = %v, want ErrProvider", err)
			}
			if errors.Is(err, engine.ErrDenied) || errors.Is(err, engine.ErrCostUnknown) {
				t.Errorf("error = %v: a provider failure is neither a budget denial nor an "+
					"unpriced model", err)
			}
			if got := h.totals(t).Reserved; got != 0 {
				t.Errorf("Reserved = %s, want 0: nothing was billed", got)
			}
			rec := h.record(t, "chat-"+tc.name)
			if rec.Outcome != activity.OutcomeProviderError {
				t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeProviderError)
			}
			if !strings.Contains(rec.Error, tc.code) {
				t.Errorf("recorded error %q should keep the provider's own classification %q",
					rec.Error, tc.code)
			}
		})
	}
}

// A provider error that arrives *with* usage settles that usage. A partially served
// request OpenAI billed for is real spend, and the caller is told both facts.
func TestChatProviderErrorWithUsageSettles(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = completion(t, gpt51, 1000, 500)
	h.chat.err = apiError(t, http.StatusInternalServerError, "server_error", "server_error",
		"the model failed after generating a partial answer")

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-err-usage", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrProvider) {
		t.Fatalf("Complete error = %v, want ErrProvider", err)
	}
	if !res.Settled {
		t.Error("usage the provider reported is usage the provider billed, so it settles")
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if got := h.totals(t).Spent; got != dollars(t, "0.00625") {
		t.Errorf("Spent = %s, want the billed usage: an error does not make spend disappear", got)
	}
	if got := h.record(t, "chat-err-usage").Status; got != activity.StatusSettled {
		t.Errorf("status = %q, want %q", got, activity.StatusSettled)
	}
}

// Cancellation mid-call leaves the outcome genuinely unknown, so the hold stays
// outstanding. OpenAI cannot cancel a synchronous completion server-side, so the request
// may well have been served and billed.
func TestChatCancellationLeavesReservationOutstanding(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.block = make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		res *openai.ChatResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := h.client.Complete(ctx, openai.ChatRequest{
			BudgetID: "team", RequestID: "chat-cancel", Params: chatRequest(gpt51, maxOut(2000)),
		})
		done <- outcome{res, err}
	}()
	waitFor(t, func() bool { return h.chat.callCount() == 1 })
	cancel()
	got := <-done
	close(h.chat.block)

	if !errors.Is(got.err, openai.ErrOutcomeUnknown) {
		t.Fatalf("Complete error = %v, want ErrOutcomeUnknown", got.err)
	}
	if got.res.Cost.Known() {
		t.Error("an interrupted call has no known cost")
	}
	if r := h.totals(t).Reserved; r == 0 {
		t.Error("Reserved = 0: the call may have been served and billed, so the hold must stay")
	}
	if s := h.totals(t).Spent; s != 0 {
		t.Errorf("Spent = %s, want 0: no usage was ever reported", s)
	}
	rec := h.record(t, "chat-cancel")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
	if rec.Outcome != activity.OutcomeCancelled {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeCancelled)
	}
}

// A deadline is recorded distinctly from a cancellation. Both leave the hold outstanding,
// and they call for different operator action.
func TestChatDeadlineIsDistinguishedFromCancellation(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.block = make(chan struct{})
	t.Cleanup(func() { close(h.chat.block) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := h.client.Complete(ctx, openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-deadline", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrOutcomeUnknown) {
		t.Fatalf("Complete error = %v, want ErrOutcomeUnknown", err)
	}
	if got := h.record(t, "chat-deadline").Outcome; got != activity.OutcomeTimeout {
		t.Errorf("outcome = %q, want %q", got, activity.OutcomeTimeout)
	}
}

// Complete on a client with no Chat Completions configured is a configuration error, and
// it is distinct from the Responses one so a caller knows which family to supply.
func TestCompleteWithoutAChatClient(t *testing.T) {
	h := newHarness(t, "1000") // Responses only.

	_, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-noclient", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrNoChatClient) {
		t.Fatalf("Complete error = %v, want ErrNoChatClient", err)
	}
	if errors.Is(err, openai.ErrNoClient) {
		t.Error("the two families are configured independently, so the errors must differ")
	}
}

// A client configured for Chat Completions only is valid, and its Responses method
// reports the corresponding error rather than panicking.
func TestChatOnlyClientIsValid(t *testing.T) {
	h := newChatOnlyHarness(t)

	if _, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chatonly-1", Params: chatRequest(gpt51, maxOut(2000)),
	}); err != nil {
		t.Fatalf("Complete on a Chat-only client: %v", err)
	}
	_, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "chatonly-2", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrNoClient) {
		t.Fatalf("Respond error = %v, want ErrNoClient", err)
	}
}

// A budget's spend is the sum across both API families, at every depth of the hierarchy.
// One engine, one ledger, one set of totals.
func TestChatSpendAttributesToAncestors(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := func() time.Time { return now }
	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	anchor := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if err := eng.Register(context.Background(), budget.Definition{
		ID: "org", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly, AnchorAt: anchor,
	}, engine.ModeEnforce); err != nil {
		t.Fatalf("Register org: %v", err)
	}
	if err := eng.Register(context.Background(), budget.Definition{
		ID: "team", ParentID: "org", Allocation: dollars(t, "500"),
		Recurrence: budget.RecurMonthly, AnchorAt: anchor,
	}, engine.ModeEnforce); err != nil {
		t.Fatalf("Register team: %v", err)
	}

	chat := &fakeChat{}
	h := buildHarness(t, eng, store, clock, withChat(chat))
	chat.out = completion(t, gpt51, 1000, 500)
	h.api.out = completedResponse(t, gpt51, 1000, 500)

	viaChat, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "hier-chat", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	viaResp, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "hier-resp", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	want := viaChat.Charge.ActualCost + viaResp.Charge.ActualCost
	for _, id := range []string{"team", "org"} {
		if got := h.totalsFor(t, id).Spent; got != want {
			t.Errorf("%s Spent = %s, want %s: both families spend one budget's money", id, got, want)
		}
	}
	// The chat record names the whole chain the money moved through.
	rec := h.record(t, "hier-chat")
	if len(rec.Scopes) < 2 {
		t.Errorf("recorded scopes = %v, want the child and its ancestor", rec.Scopes)
	}
}

// newMonitorChatHarness builds a monitor-mode budget with a Chat Completions fake.
func newMonitorChatHarness(t *testing.T) *chatHarness {
	t.Helper()
	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := func() time.Time { return now }
	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := eng.Register(context.Background(), budget.Definition{
		ID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly,
		AnchorAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}, engine.ModeMonitor); err != nil {
		t.Fatalf("Register: %v", err)
	}

	chat := &fakeChat{}
	h := buildHarness(t, eng, store, clock, withChat(chat))
	chat.out = completion(t, gpt51, 1000, 500)
	return &chatHarness{harness: h, chat: chat}
}

// newChatOnlyHarness builds a client configured for Chat Completions and nothing else,
// which is the configuration an application that never touches Responses would use.
func newChatOnlyHarness(t *testing.T) *chatHarness {
	t.Helper()
	chat := &fakeChat{}
	h := newHarness(t, "1000", withChat(chat), func(c *openai.Config) {
		c.Client = nil
		c.Counter = nil
	})
	chat.out = completion(t, gpt51, 1000, 500)
	return &chatHarness{harness: h, chat: chat}
}

// chatModelString reads a model back out of sent params, for assertions about what
// reached the provider.
func chatModelString(m shared.ChatModel) string { return string(m) }
