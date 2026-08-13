package openai_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/pricing/fixtures"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/usage"
)

// A finish reason is not a monetary outcome.
//
// Every one of these completions stopped for a reason other than "stop", and every one of
// them reported authoritative usage. All of them charge. There is no finish reason that
// releases a hold, deliberately: OpenAI billed the tokens it generated regardless of why
// it stopped generating them, and a release would assert it did not.
//
// The table covers the reasons OpenAI documents, so a later addition to the vocabulary
// has an obvious place to go and an obvious expectation to meet.
func TestEveryFinishReasonChargesWhenUsageExists(t *testing.T) {
	cases := []struct {
		reason string
		// stopped is whether the caller is told generation ended early. tool_calls is not
		// an early ending: the model finished its turn by asking for a tool, which is the
		// API working as designed and is what an agent loop sees on nearly every iteration.
		stopped bool
	}{
		{"length", true},
		{"content_filter", true},
		{"tool_calls", false},
		{"function_call", false},
		{"stop", false},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			h := newChatHarness(t, "1000")
			h.chat.out = complete(t, fmt.Sprintf(`{
				"id": "chatcmpl_%s", "object": "chat.completion", "created": 1786000000, "model": %q,
				"choices": [{"index": 0, "finish_reason": %q,
					"message": {"role": "assistant", "content": "partial"}}],
				"usage": {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500}
			}`, tc.reason, gpt51, tc.reason))

			id := "finish-" + tc.reason
			res, err := h.client.Complete(context.Background(), openai.ChatRequest{
				BudgetID: "team", RequestID: id, Params: chatRequest(gpt51, maxOut(2000)),
			})

			// The money, first and identically in every case.
			if !res.Settled {
				t.Fatalf("a completion reporting usage must settle whatever its finish reason: %v", err)
			}
			want := dollars(t, "0.00625")
			if res.Charge.ActualCost != want {
				t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
			}
			if got := h.totals(t).Spent; got != want {
				t.Errorf("Spent = %s, want %s: OpenAI billed these tokens", got, want)
			}
			if got := h.totals(t).Reserved; got != 0 {
				t.Errorf("Reserved = %s, want 0: the hold was consumed by the charge, not released", got)
			}

			// Then whether the caller is told, which is a separate question.
			if tc.stopped {
				if !errors.Is(err, openai.ErrCompletionStopped) {
					t.Errorf("error = %v, want ErrCompletionStopped so a caller cannot mistake a "+
						"truncated answer for a complete one", err)
				}
				if !strings.Contains(err.Error(), tc.reason) {
					t.Errorf("error %q should name the provider's own reason", err)
				}
			} else if err != nil {
				t.Errorf("unexpected error for a %s finish: %v", tc.reason, err)
			}

			rec := h.record(t, id)
			if rec.Status != activity.StatusSettled {
				t.Errorf("status = %q, want %q: the request is settled either way",
					rec.Status, activity.StatusSettled)
			}
			if tc.stopped {
				if rec.Outcome != activity.OutcomeProviderError {
					t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeProviderError)
				}
				if !strings.Contains(rec.Error, tc.reason) {
					t.Errorf("recorded error %q should keep the finish reason", rec.Error)
				}
			} else if rec.Outcome != activity.OutcomeSuccess {
				t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeSuccess)
			}
		})
	}
}

// A refusal charges. "No useful assistant text" and "no provider spend" are different
// things, and equating them would let a budget lose sight of a workload that gets
// refused a lot.
//
// The refusal text itself is not persisted; that is checked in the privacy tests. What is
// recorded is that generation was filtered, which is the fact an operator can act on.
func TestRefusalIsChargedAndItsTextIsNotTheReason(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_refusal", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "content_filter",
			"message": {"role": "assistant", "content": null,
				"refusal": "I can't help with that request."}}],
		"usage": {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-refusal", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrCompletionStopped) {
		t.Fatalf("Complete error = %v, want ErrCompletionStopped", err)
	}
	if !res.Settled {
		t.Fatal("a refusal that consumed tokens is real spend")
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	rec := h.record(t, "chat-refusal")
	if !strings.Contains(rec.Error, "content_filter") {
		t.Errorf("recorded error %q should name the provider's own classification", rec.Error)
	}
	if strings.Contains(rec.Error, "I can't help") {
		t.Error("the refusal text reached the durable record: it is model-generated prose about " +
			"the prompt and belongs nowhere in throttle")
	}
}

// A batch where the choices disagree records every reason, because they are all true and
// an operator investigating a partly-truncated batch needs to see which.
func TestDivergentFinishReasonsAreAllRecorded(t *testing.T) {
	h := newChatHarness(t, "1000")
	in := chatRequest(gpt51, maxOut(2000))
	in.N = param.NewOpt(int64(3))
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_mixed", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [
			{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "a"}},
			{"index": 1, "finish_reason": "length", "message": {"role": "assistant", "content": "b"}},
			{"index": 2, "finish_reason": "content_filter", "message": {"role": "assistant", "content": null}}
		],
		"usage": {"prompt_tokens": 1000, "completion_tokens": 1500, "total_tokens": 2500}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-mixed", Params: in,
	})
	if !errors.Is(err, openai.ErrCompletionStopped) {
		t.Fatalf("Complete error = %v, want ErrCompletionStopped", err)
	}
	if !res.Settled {
		t.Fatal("the batch consumed tokens and must settle")
	}
	rec := h.record(t, "chat-mixed")
	for _, want := range []string{"content_filter", "length"} {
		if !strings.Contains(rec.Error, want) {
			t.Errorf("recorded error %q should include %q: the choices genuinely differed",
				rec.Error, want)
		}
	}
	if strings.Contains(rec.Error, "stop") && !strings.Contains(rec.Error, "stopped") {
		// "stopped because of" contains "stop"; only a bare listing would be wrong.
		t.Errorf("recorded error %q lists the natural stop as though it were a problem", rec.Error)
	}
}

// Caller-executed tools acquire no invented OpenAI charge.
//
// Both Chat Completions tool variants are the caller's own code: OpenAI bills the schema
// and the arguments as tokens, which the usage object reports in full, and the execution
// happens on the caller's machine. Assigning it an OpenAI dollar amount would invent a
// charge, and marking the cost incomplete would tell a caller throttle is missing a
// provider rate it is not missing.
func TestCallerExecutedToolsAreFullyPriced(t *testing.T) {
	h := newChatHarness(t, "1000")

	in := chatRequest(gpt51, maxOut(2000))
	in.Tools = []oai.ChatCompletionToolUnionParam{
		{OfFunction: &oai.ChatCompletionFunctionToolParam{
			Function: oai.FunctionDefinitionParam{
				Name:        "get_weather",
				Description: param.NewOpt("look up the weather"),
			},
		}},
		{OfCustom: &oai.ChatCompletionCustomToolParam{
			Custom: oai.ChatCompletionCustomToolCustomParam{Name: "run_query"},
		}},
	}
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_tools", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "tool_calls",
			"message": {"role": "assistant", "content": null,
				"tool_calls": [{"id": "call_1", "type": "function",
					"function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}]}}],
		"usage": {"prompt_tokens": 1200, "completion_tokens": 40, "total_tokens": 1240}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-tools", Params: in,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !res.Cost.Known() {
		t.Errorf("a request using only caller-executed tools is fully priceable, got %s: %s",
			res.Cost.State(), res.Cost.Reason)
	}
	if len(res.Cost.Unpriced) != 0 {
		t.Errorf("Unpriced = %v, want empty: nothing about these tools is billed outside the "+
			"usage object", res.Cost.Unpriced)
	}
	// 1200 at $1.25/M = $0.0015; 40 at $10/M = $0.0004. The tokens, and nothing added for
	// the caller's own execution.
	if want := dollars(t, "0.0019"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: the schema and arguments are billed as tokens and "+
			"the execution is not OpenAI's to charge for", res.Charge.ActualCost, want)
	}
	// The tool schema does raise the input estimate, which is the one legitimate effect a
	// tool has on exposure.
	bare, err := h.client.EstimateChat(context.Background(), chatRequest(gpt51, maxOut(2000)))
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	withTools, err := h.client.EstimateChat(context.Background(), in)
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	bareIn, _ := bare.Usage.Get(usage.InputTokens)
	toolIn, _ := withTools.Usage.Get(usage.InputTokens)
	if toolIn <= bareIn {
		t.Errorf("input estimate with tools = %d, without = %d: tool schemas are sent with the "+
			"request and billed as input", toolIn, bareIn)
	}
}

// web_search_options makes a request incompletely priceable, and it is not in the tool
// list.
//
// This is the Chat Completions-specific trap. The hosted tools that make a Responses
// request unpriceable are not available in this API family at all, so a classifier that
// only walked the tool list would report a web-searching request as fully priced -- while
// OpenAI bills web search per call on its pricing page and reports it nowhere in the
// response.
func TestWebSearchOptionsMakesTheCostIncomplete(t *testing.T) {
	h := newChatHarness(t, "1000")

	in := chatRequest(gpt51, maxOut(2000))
	in.WebSearchOptions = oai.ChatCompletionNewParamsWebSearchOptions{
		SearchContextSize: "medium",
	}

	est, err := h.client.EstimateChat(context.Background(), in)
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if est.Cost.Known() {
		t.Fatal("web search is billed per call and reported nowhere in the response, so the " +
			"token cost is a floor")
	}
	if !strings.Contains(est.Cost.Reason, "web_search_options") {
		t.Errorf("reason %q should name the request field, so an operator can find it in their "+
			"own code", est.Cost.Reason)
	}
	// It has no tools at all, which is the point: the tool list is empty and the request is
	// still not fully priceable.
	if len(in.Tools) != 0 {
		t.Fatal("this test is only meaningful with an empty tool list")
	}

	// Under monitor it runs and settles as a floor rather than a total.
	m := newMonitorChatHarness(t)
	m.chat.out = completion(t, gpt51, 1000, 500)
	res, err := m.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-websearch", Params: in,
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Complete error = %v, want ErrCostUnresolved", err)
	}
	if res.Cost.State() != usage.CostPartial {
		t.Errorf("cost state = %v, want CostPartial: the tokens priced, the per-call charge did not",
			res.Cost.State())
	}
	if res.Cost.Amount == 0 {
		t.Error("the token floor is a valid figure and should not be erased")
	}
	if got := m.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0: a floor must not settle as a total", got)
	}
}

// The #30 service-tier rule, on the Chat Completions path.
//
// The tier the request asked for is intent; the tier the response reports is what served
// the call and what OpenAI billed. When the served tier was frozen as an alternate at
// admission, settlement prices from that frozen rate.
func TestServedTierSettlesFromTheCapturedAlternate(t *testing.T) {
	h := newChatHarness(t, "1000")
	// Asks for nothing in particular; OpenAI serves it on priority, which is twice the
	// standard rate for this model.
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_tier", "object": "chat.completion", "created": 1786000000, "model": %q,
		"service_tier": "priority",
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-tier", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Identity.ServiceTier != "priority" {
		t.Errorf("recorded tier = %q, want priority: the response reports what actually served "+
			"the call", res.Identity.ServiceTier)
	}
	// Priority is $2.50/M input and $20.00/M output for this model: 1000 and 500 tokens
	// come to $0.0025 + $0.01. The standard-rate reading would be $0.00625.
	want := dollars(t, "0.0125")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: the served tier's frozen rates, not the standard ones",
			res.Charge.ActualCost, want)
	}
	if got := h.record(t, "chat-tier").Identity.ServiceTier; got != "priority" {
		t.Errorf("durable tier = %q, want priority", got)
	}
}

// A served tier nobody captured a rate for is unresolved. Not the requested tier's rate,
// not the standard rate, and not a fresh catalog lookup.
//
// All three fallbacks would produce a confident number computed from a price sheet this
// request did not run under. The honest answer leaves the hold encumbered and says which
// tiers were captured, so a human can add the row.
func TestUncapturedServedTierIsUnresolved(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_scale", "object": "chat.completion", "created": 1786000000, "model": %q,
		"service_tier": "scale",
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	in := chatRequest(gpt51, maxOut(2000))
	in.ServiceTier = oai.ChatCompletionNewParamsServiceTierPriority

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-scale", Params: in,
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Complete error = %v, want ErrCostUnresolved", err)
	}
	if res.Cost.Known() {
		t.Fatal("a tier no rate was frozen for must not produce a known cost")
	}
	if res.Cost.Amount != 0 {
		t.Errorf("Amount = %s: with no applicable rates there is no valid floor either, so the "+
			"cost is unknown rather than partial", res.Cost.Amount)
	}
	if !strings.Contains(res.Cost.Reason, "scale") {
		t.Errorf("reason %q should name the tier that served the call", res.Cost.Reason)
	}
	// No fallback happened: nothing settled, and the hold stays encumbered.
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0: neither the requested tier's rate nor the standard rate "+
			"may stand in for a tier that was never priced", got)
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: the request ran and was billed, so the headroom stays encumbered")
	}
	rec := h.record(t, "chat-scale")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusUnresolved)
	}
	if rec.ActualUsage.Empty() {
		t.Error("the usage must be persisted: it is what a later reconciliation prices")
	}
}

// Settlement replays the quote captured at admission and never queries the live catalog.
//
// This is what makes a Chat Completions charge reproducible: a price refresh landing
// between admission and settlement must not change what a request cost. Checked by
// actually landing one -- the rates are changed while the provider call is in flight,
// which is the window a real refresh would arrive in.
func TestChatSettlementReplaysTheAdmittedRates(t *testing.T) {
	cat := &mutableCatalog{}
	cat.set(t, "1.25", "10.00")

	h := newChatHarness(t, "1000", func(cfg *openai.Config) { cfg.Catalog = cat })
	h.chat.out = completion(t, gpt51, 1000, 500)
	h.chat.block = make(chan struct{})

	type outcome struct {
		res *openai.ChatResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := h.client.Complete(context.Background(), openai.ChatRequest{
			BudgetID: "team", RequestID: "chat-replay", Params: chatRequest(gpt51, maxOut(2000)),
		})
		done <- outcome{res, err}
	}()

	// The request is admitted and in flight. A tenfold price rise lands now.
	waitFor(t, func() bool { return h.chat.callCount() == 1 })
	cat.set(t, "12.50", "100.00")
	close(h.chat.block)
	got := <-done

	if got.err != nil {
		t.Fatalf("Complete: %v", got.err)
	}
	// The admitted rates: 1000 at $1.25/M plus 500 at $10.00/M. The refreshed sheet would
	// give $0.0625.
	want := dollars(t, "0.00625")
	if got.res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: settlement must price from the quote frozen at "+
			"admission, not from a catalog that moved while the call was in flight",
			got.res.Charge.ActualCost, want)
	}
	if s := h.totals(t).Spent; s != want {
		t.Errorf("Spent = %s, want %s", s, want)
	}
}

// A catalog that learns a tier after a request stranded on it cannot retroactively price
// that request.
//
// The repair belongs to reconciliation, deliberately, where the new rate is an explicit
// input that a human chose to apply -- rather than an invisible one that silently
// resolves history the next time somebody happens to look.
func TestCatalogLearningATierCannotPriceAStrandedChatRequest(t *testing.T) {
	cat := &mutableCatalog{}
	cat.set(t, "1.25", "10.00")

	h := newChatHarness(t, "1000", func(cfg *openai.Config) { cfg.Catalog = cat })
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_learn", "object": "chat.completion", "created": 1786000000, "model": %q,
		"service_tier": "flex",
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-learn", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Complete error = %v, want ErrCostUnresolved: no flex rate was frozen", err)
	}
	if res.Cost.Known() {
		t.Fatal("a tier that was never captured must not produce a known cost")
	}

	// Somebody adds the row afterwards. The stranded record does not move on its own.
	cat.addTier(t, "flex", money.Money(1_000_000))

	rec := h.record(t, "chat-learn")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want %q: the record is repaired by reconciliation, not by the "+
			"catalog changing underneath it", rec.Status, activity.StatusUnresolved)
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: the request was billed by OpenAI, so the headroom stays encumbered")
	}
}

// The captured quote carries no operation, which is what lets one catalog fact serve both
// API families -- and is checked here rather than asserted, by capturing against each
// operation and comparing the frozen rates.
func TestCapturedQuoteIsIndifferentToOperation(t *testing.T) {
	cat, err := fixtures.Catalog()
	if err != nil {
		t.Fatalf("fixtures.Catalog: %v", err)
	}
	// Read through the interface the adapter itself uses, so this exercises the same
	// capture path production does rather than a concrete type's method.
	var rs pricing.RateSource = cat

	chatID := openai.Identify(gpt51, "")
	chatID.Operation = openai.OperationChatCompletions
	respID := openai.Identify(gpt51, "")
	respID.Operation = openai.OperationResponses

	chatQ, err := rs.Capture(chatID, now)
	if err != nil {
		t.Fatalf("Capture(chat): %v", err)
	}
	respQ, err := rs.Capture(respID, now)
	if err != nil {
		t.Fatalf("Capture(responses): %v", err)
	}

	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1000, usage.OutputTokens: 500})
	chatPriced, _ := chatQ.Price(u)
	respPriced, _ := respQ.Price(u)
	if chatPriced.Cost.Amount != respPriced.Cost.Amount {
		t.Errorf("the same usage priced %s under chat-completions and %s under responses: OpenAI "+
			"does not price its endpoints separately, so the catalog must not either",
			chatPriced.Cost.Amount, respPriced.Cost.Amount)
	}
	if len(chatQ.Rates) != len(respQ.Rates) {
		t.Errorf("the two captures froze different rate sets (%d vs %d dimensions)",
			len(chatQ.Rates), len(respQ.Rates))
	}
}
