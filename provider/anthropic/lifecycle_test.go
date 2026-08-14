package anthropic_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	anthropic "github.com/scttfrdmn/throttle/provider/anthropic"
	"github.com/scttfrdmn/throttle/usage"
)

// The lifecycle half of #29: every state a governed Messages request can end in, and the
// accounting each one produces.
//
// The states themselves are not Anthropic's -- they are throttle's, and they are the same
// states the Bedrock and OpenAI adapters reach through the same shared code. What is
// Anthropic-specific is which real-world event lands in which state, and that is what
// these tests pin. Two of them matter more than the rest: an interrupted call cannot be
// released, because a synchronous Messages call cannot be cancelled server-side, and a
// stop reason never decides whether money moved.

// A provider error with no usage releases the hold in full. Nothing was billed, so
// holding the money would deny the next caller headroom that was never spent.
func TestProviderErrorReleasesReservation(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.setResponse(nil, apiError(t, http.StatusBadRequest, "invalid_request_error",
		"max_tokens: 9999999 > 64000, which is the maximum allowed"))

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-err", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrProvider) {
		t.Fatalf("NewMessage error = %v, want ErrProvider", err)
	}
	if got := h.totals(t).Reserved; got != 0 {
		t.Errorf("Reserved = %s, want 0: a request that was never billed must give its hold back", got)
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}
	if !res.Cost.Known() || res.Cost.Amount != 0 {
		t.Errorf("cost = %v: a call that provably consumed nothing is a known zero, which is the "+
			"one place zero is the right answer", res.Cost)
	}
	rec := h.record(t, "req-err")
	if rec.Status != activity.StatusReleased {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusReleased)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeProviderError)
	}
}

// A provider error arriving *with* usage is real spend. A partially served request
// Anthropic billed for is charged, and the caller is told both facts.
func TestProviderErrorWithUsageSettles(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.setResponse(message(t, opus5, 1000, 500),
		apiError(t, http.StatusInternalServerError, "api_error", "internal server error"))

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-err-usage", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrProvider) {
		t.Fatalf("NewMessage error = %v, want ErrProvider", err)
	}
	if !res.Settled {
		t.Fatal("usage reported alongside an error is still billable and must settle")
	}
	if want := dollars(t, "0.0175"); h.totals(t).Spent != want {
		t.Errorf("Spent = %s, want %s", h.totals(t).Spent, want)
	}
	rec := h.record(t, "req-err-usage")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusSettled)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want %q: the money settled and the request still failed",
			rec.Outcome, activity.OutcomeProviderError)
	}
}

// Anthropic's rate limiting is not throttle's denial. A caller that cannot tell them
// apart will retry the wrong one: throttle's refusal means spend more slowly, a 429
// means wait, and a 529 means Anthropic is overloaded.
func TestProviderRateLimitAndOverloadAreNotBudgetDenials(t *testing.T) {
	for name, e := range map[string]struct {
		status int
		kind   string
	}{
		"rate limit": {http.StatusTooManyRequests, "rate_limit_error"},
		"overloaded": {529, "overloaded_error"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "1000")
			h.api.setResponse(nil, apiError(t, e.status, e.kind, "please retry later"))

			_, err := h.client.NewMessage(context.Background(), anthropic.Request{
				BudgetID: "team", RequestID: "req-" + e.kind, Params: request(opus5, 2000),
			})
			if !errors.Is(err, anthropic.ErrProvider) {
				t.Fatalf("NewMessage error = %v, want ErrProvider", err)
			}
			if errors.Is(err, engine.ErrDenied) {
				t.Error("a provider rate limit must not be reported as a budget denial")
			}
			rec := h.record(t, "req-"+e.kind)
			if rec.Outcome != activity.OutcomeProviderError {
				t.Errorf("outcome = %q, want %q: throttle's own refusal is OutcomeBudgetDenied, "+
					"and conflating them sends an operator to the wrong system",
					rec.Outcome, activity.OutcomeProviderError)
			}
			// The classification survives to the record, since that is what an operator acts
			// on hours later.
			if !strings.Contains(rec.Error, fmt.Sprint(e.status)) {
				t.Errorf("recorded error %q should carry the HTTP status", rec.Error)
			}
			if !strings.Contains(rec.Error, e.kind) {
				t.Errorf("recorded error %q should carry Anthropic's own error type", rec.Error)
			}
		})
	}
}

// A budget refusal never reaches the provider. Governing spend that has already been
// incurred is not governing it.
func TestBudgetDenialDoesNotCallProvider(t *testing.T) {
	h := newHarness(t, "0.000001")

	_, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-denied", Params: request(opus5, 2000),
	})
	if err == nil {
		t.Fatal("a request far above its allocation should have been denied")
	}
	if h.api.callCount() != 0 {
		t.Error("Anthropic was called for a denied request")
	}
	if h.counter.callCount() > 1 {
		t.Errorf("the counter was called %d times for one denied request", h.counter.callCount())
	}
	rec := h.record(t, "req-denied")
	if rec.Status != activity.StatusDenied {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusDenied)
	}
	if rec.Outcome != activity.OutcomeBudgetDenied {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeBudgetDenied)
	}
}

// An interrupted call leaves its hold outstanding, because the outcome is genuinely
// unknown: Anthropic cannot cancel a synchronous message server-side, so the request may
// well have been served and billed after the caller gave up.
//
// This is the assertion that distinguishes the honest handling from the convenient one.
// Releasing would be the tidy answer and it would erase real spend.
func TestCancellationLeavesReservationOutstanding(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.block = make(chan struct{})
	t.Cleanup(func() { close(h.api.block) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.client.NewMessage(ctx, anthropic.Request{
			BudgetID: "team", RequestID: "req-cancel", Params: request(opus5, 2000),
		})
		done <- err
	}()

	waitFor(t, func() bool { return h.api.callCount() == 1 })
	cancel()

	err := <-done
	if !errors.Is(err, anthropic.ErrOutcomeUnknown) {
		t.Fatalf("NewMessage error = %v, want ErrOutcomeUnknown", err)
	}
	if errors.Is(err, anthropic.ErrProvider) {
		t.Error("a cancelled call is not a provider failure: the request may have been served in full")
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: an ambiguous outcome must leave the hold outstanding, since Anthropic " +
			"cannot cancel a synchronous message and may have billed it")
	}
	rec := h.record(t, "req-cancel")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
	if rec.Outcome != activity.OutcomeCancelled {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeCancelled)
	}
	if rec.ActualCost.Known() {
		t.Error("the cost of an interrupted call is not known -- and specifically not a known zero")
	}
}

// A deadline is recorded distinctly from a cancellation. Both are ambiguous, and they
// mean different things operationally: a client gave up, or a limit was reached.
func TestDeadlineIsDistinguishedFromCancellation(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.block = make(chan struct{})
	t.Cleanup(func() { close(h.api.block) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := h.client.NewMessage(ctx, anthropic.Request{
		BudgetID: "team", RequestID: "req-timeout", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrOutcomeUnknown) {
		t.Fatalf("NewMessage error = %v, want ErrOutcomeUnknown", err)
	}
	if rec := h.record(t, "req-timeout"); rec.Outcome != activity.OutcomeTimeout {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeTimeout)
	}
}

// Every stop reason charges when usage exists. A stop reason describes the generation,
// not the bill, and there is deliberately no release path keyed on one.
//
// The reasons are the SDK's current set plus one this build has never seen, because
// Anthropic adds them -- pause_turn, refusal, and model_context_window_exceeded were all
// additions -- and a decoder handed a new one accepts it cleanly, so the value really
// does reach this code.
func TestEveryStopReasonChargesWhenUsageExists(t *testing.T) {
	for _, reason := range []string{
		"end_turn",
		"max_tokens",
		"stop_sequence",
		"tool_use",
		"pause_turn",
		"refusal",
		"model_context_window_exceeded",
		// Not in this build's SDK. The point of the case: a future reason must charge.
		"budget_exhausted_by_a_future_api",
	} {
		t.Run(reason, func(t *testing.T) {
			h := newHarness(t, "1000")
			h.api.out = reply(t, fmt.Sprintf(`{
				"id": "msg_stop", "type": "message", "role": "assistant", "model": %q,
				"content": [{"type": "text", "text": "a partial answer"}],
				"stop_reason": %q,
				"usage": {"input_tokens": 1000, "output_tokens": 500}
			}`, opus5, reason))

			res, err := h.client.NewMessage(context.Background(), anthropic.Request{
				BudgetID: "team", RequestID: "req-stop-" + reason, Params: request(opus5, 2000),
			})
			// A non-natural stop is reported to the caller, and reporting it is not a refund.
			natural := map[string]bool{
				"end_turn": true, "stop_sequence": true, "tool_use": true, "pause_turn": true,
			}[reason]
			switch {
			case natural && err != nil:
				t.Fatalf("NewMessage: %v", err)
			case !natural && !errors.Is(err, anthropic.ErrGenerationStopped):
				t.Fatalf("NewMessage error = %v, want ErrGenerationStopped", err)
			}

			if !res.Settled {
				t.Fatal("authoritative usage exists, so it is charged whatever the generation did")
			}
			if want := dollars(t, "0.0175"); h.totals(t).Spent != want {
				t.Errorf("Spent = %s, want %s: a %s stop consumed the same tokens as any other",
					h.totals(t).Spent, want, reason)
			}
			rec := h.record(t, "req-stop-"+reason)
			if rec.Status != activity.StatusSettled {
				t.Errorf("status = %q, want %q", rec.Status, activity.StatusSettled)
			}
			// The reason survives in Anthropic's own vocabulary, so "we charged for a
			// truncated answer" stays legible later.
			if !natural && !strings.Contains(rec.Error, reason) {
				t.Errorf("recorded error %q should name the stop reason", rec.Error)
			}
		})
	}
}

// A refusal records its category and not its explanation. The category is a value from a
// fixed set; the explanation is model-generated prose about the prompt, which makes it
// content.
func TestRefusalRecordsItsCategoryAndNotItsExplanation(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = reply(t, fmt.Sprintf(`{
		"id": "msg_refuse", "type": "message", "role": "assistant", "model": %q,
		"content": [], "stop_reason": "refusal",
		"stop_details": {
			"type": "refusal", "category": "cyber",
			"explanation": "the user asked how to SENTINEL-REFUSAL-EXPLANATION"
		},
		"usage": {"input_tokens": 1000, "output_tokens": 500}
	}`, opus5))

	_, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-refusal", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrGenerationStopped) {
		t.Fatalf("NewMessage error = %v, want ErrGenerationStopped", err)
	}
	rec := h.record(t, "req-refusal")
	if !strings.Contains(rec.Error, "cyber") {
		t.Errorf("recorded error %q should carry the refusal category, which is the actionable fact", rec.Error)
	}
	if strings.Contains(rec.Error, "SENTINEL-REFUSAL-EXPLANATION") {
		t.Error("the refusal explanation reached durable storage: it is model-generated prose about " +
			"the prompt, so it is content rather than metadata")
	}
	// And it settled, because a refused generation still consumed tokens Anthropic billed.
	if want := dollars(t, "0.0175"); h.totals(t).Spent != want {
		t.Errorf("Spent = %s, want %s: a refusal is not a refund", h.totals(t).Spent, want)
	}
}

// A stop_sequence is not recorded, because the caller wrote it. A caller who uses a
// fragment of their own prompt as a delimiter would otherwise have it persisted.
func TestStopSequenceValueIsNotRecorded(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = reply(t, fmt.Sprintf(`{
		"id": "msg_seq", "type": "message", "role": "assistant", "model": %q,
		"content": [{"type": "text", "text": "an answer"}],
		"stop_reason": "stop_sequence", "stop_sequence": "SENTINEL-STOP-SEQUENCE",
		"usage": {"input_tokens": 1000, "output_tokens": 500}
	}`, opus5))

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-seq", Params: request(opus5, 2000),
	}); err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	rec := h.record(t, "req-seq")
	if strings.Contains(fmt.Sprint(rec), "SENTINEL-STOP-SEQUENCE") {
		t.Error("the caller's own stop sequence reached the durable record: they wrote it, so it is " +
			"request content")
	}
}

// The SDK refuses an unstreamed request whose max_tokens implies more than ten minutes of
// generation, and it refuses locally -- before any HTTP request. So nothing was billed and
// the whole hold goes back.
func TestLocalPreflightRefusalReleasesTheWholeHold(t *testing.T) {
	h := newHarness(t, "1000")
	// The SDK's own error text for its local check. What matters is that it carries no
	// provider payload and no usage, which is the state the release path keys on.
	h.api.setResponse(nil, errors.New("anthropic: streaming is strongly recommended for operations "+
		"that may take longer than 10 minutes"))

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-preflight", Params: request(opus5, 200000),
	})
	if !errors.Is(err, anthropic.ErrProvider) {
		t.Fatalf("NewMessage error = %v, want ErrProvider", err)
	}
	if got := h.totals(t).Reserved; got != 0 {
		t.Errorf("Reserved = %s, want 0: a request refused before it was sent was never billed", got)
	}
	if !res.Cost.Known() || res.Cost.Amount != 0 {
		t.Errorf("cost = %v, want a known zero", res.Cost)
	}
	if rec := h.record(t, "req-preflight"); rec.Status != activity.StatusReleased {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusReleased)
	}
}

// A code-execution tool is refused under enforce, before Anthropic is called. Its charge
// is container time -- a five-minute minimum against a monthly per-organization
// allowance -- and no field of any response reports a duration, so the maximum exposure
// cannot be bounded.
func TestCodeExecutionIsRefusedBeforeExecutionUnderEnforce(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	in.Tools = []anth.ToolUnionParam{{
		OfCodeExecutionTool20250522: &anth.CodeExecutionTool20250522Param{},
	}}

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-code-exec", Params: in,
	})
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("NewMessage error = %v, want engine.ErrCostUnknown", err)
	}
	if h.api.callCount() != 0 {
		t.Error("Anthropic was called for a request whose exposure could not be bounded: enforce " +
			"must refuse before the money is spent, not after")
	}
	if res.Estimate.Cost.Known() {
		t.Error("the estimate's cost must not be known when part of the charge is invisible")
	}
	rec := h.record(t, "req-code-exec")
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("outcome = %q, want %q: 'the budget was full' and 'throttle could not price this' "+
			"call for entirely different operator action", rec.Outcome, activity.OutcomeUnpriced)
	}
	if !strings.Contains(rec.Error, "code_execution") {
		t.Errorf("recorded error %q should name the tool that could not be priced", rec.Error)
	}
}

// Under monitor the same request executes and settles as a floor: the tokens are real and
// priced, and what cannot be seen is named rather than assumed to be zero.
func TestCodeExecutionSettlesAsAFloorUnderMonitor(t *testing.T) {
	h := monitorHarness(t, "1000")

	in := request(opus5, 2000)
	in.Tools = []anth.ToolUnionParam{{
		OfCodeExecutionTool20250825: &anth.CodeExecutionTool20250825Param{},
	}}

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-code-monitor", Params: in,
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved", err)
	}
	if h.api.callCount() != 1 {
		t.Error("monitor mode should have executed the request")
	}
	if res.Message == nil {
		t.Error("the caller should still get their message in monitor mode")
	}
	if res.Cost.State() != usage.CostPartial {
		t.Fatalf("cost state = %v, want CostPartial: the tokens are a valid floor", res.Cost.State())
	}
	// The floor is the real token cost, neither discarded nor inflated by a guess at
	// container time.
	if want := dollars(t, "0.0175"); res.Cost.Amount != want {
		t.Errorf("floor = %s, want %s", res.Cost.Amount, want)
	}
	if got := res.Cost.AtLeast(); got != dollars(t, "0.0175") {
		t.Errorf("AtLeast() = %s, want %s: a partial cost must report the floor it is at least",
			got, dollars(t, "0.0175"))
	}
	// Nothing settled, because a floor is not a total.
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s: a floor must not be recorded as though it were the whole bill", got)
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: money was spent that throttle cannot fully name, so the hold stays")
	}
	if !strings.Contains(res.Cost.Reason, "container time") {
		t.Errorf("cost reason %q should say why the figure is a floor", res.Cost.Reason)
	}
}

// A container in the response establishes a time-billed charge whatever the request's
// tool list said -- a tool inherited from a prior turn, a beta surface, a server-side
// default. The container's presence is what matters, not the tool block's.
func TestContainerInTheResponseMakesTheCostAFloor(t *testing.T) {
	h := monitorHarness(t, "1000")
	h.api.out = reply(t, fmt.Sprintf(`{
		"id": "msg_container", "type": "message", "role": "assistant", "model": %q,
		"content": [{"type": "text", "text": "an answer"}], "stop_reason": "end_turn",
		"container": {"id": "container_abc123", "expires_at": "2026-08-12T13:00:00Z"},
		"usage": {"input_tokens": 1000, "output_tokens": 500}
	}`, opus5))

	// No tools at all in the request: the exposure is discovered from the response.
	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-container", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved", err)
	}
	if res.Cost.State() != usage.CostPartial {
		t.Errorf("cost state = %v, want CostPartial", res.Cost.State())
	}
	if !strings.Contains(res.Cost.Reason, "container") {
		t.Errorf("cost reason %q should name the container", res.Cost.Reason)
	}
}

// The caller's own function costs Anthropic nothing beyond tokens, so throttle assigns it
// nothing. Inventing an execution fee for code that ran on the caller's machine would be
// inventing a charge.
func TestCallerToolsGetNoInventedCost(t *testing.T) {
	for name, tool := range map[string]anth.ToolUnionParam{
		"custom function": {OfTool: &anth.ToolParam{
			Name:        "lookup_order",
			InputSchema: anth.ToolInputSchemaParam{},
		}},
		"bash":        {OfBashTool20250124: &anth.ToolBash20250124Param{}},
		"text editor": {OfTextEditor20250728: &anth.ToolTextEditor20250728Param{}},
		"memory":      {OfMemoryTool20250818: &anth.MemoryTool20250818Param{}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "1000")
			in := request(opus5, 2000)
			in.Tools = []anth.ToolUnionParam{tool}

			res, err := h.client.NewMessage(context.Background(), anthropic.Request{
				BudgetID: "team", RequestID: "req-caller-tool", Params: in,
			})
			if err != nil {
				t.Fatalf("NewMessage: %v", err)
			}
			if !res.Cost.Known() {
				t.Errorf("a tool the caller executes is fully accountable in tokens: %s", res.Cost.Reason)
			}
			// Exactly the token cost, with no execution surcharge added anywhere.
			if want := dollars(t, "0.0175"); res.Charge.ActualCost != want {
				t.Errorf("ActualCost = %s, want %s: the caller's own execution is not an Anthropic charge",
					res.Charge.ActualCost, want)
			}
		})
	}
}

// A tool variant this build cannot classify makes the cost incomplete. The distinction is
// who executes and bills the tool, and an unclassified tool answers neither question --
// so the conservative reading is the only honest one.
//
// The zero value of the billing classification is "unverified" precisely so that a
// variant added to a future SDK, or a switch arm somebody forgot, lands here rather than
// on the optimistic answer.
func TestUnverifiedToolBillingIsTreatedConservatively(t *testing.T) {
	h := monitorHarness(t, "1000")

	in := request(opus5, 2000)
	// Server-side, and no charge could be confirmed in either direction from official
	// documentation. Classified conservatively rather than assumed free.
	in.Tools = []anth.ToolUnionParam{{
		OfToolSearchToolBm25_20251119: &anth.ToolSearchToolBm25_20251119Param{},
	}}

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-unverified", Params: in,
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved: a tool whose billing throttle "+
			"cannot state must not settle as fully priced", err)
	}
	if res.Cost.Known() {
		t.Error("an unverified tool charge must not produce a known cost")
	}
	if !strings.Contains(res.Cost.Reason, "tool_search") {
		t.Errorf("cost reason %q should name the tool", res.Cost.Reason)
	}
}

// Every tool variant in this SDK classifies from a param built the idiomatic way.
//
// This is the anti-vacuous guard on the classifier. Eighteen of the nineteen variants
// declare Name as a single-valued constant type whose zero value is the empty string,
// filled in from a struct tag at marshal time -- so reading the field directly off
// `&anth.WebSearchTool20250305Param{}` yields "" and would classify as no tool at all.
// The classifier calls Default() for that reason, and this test is what proves it still
// does: every one of these is the shape a caller actually writes.
func TestEveryToolVariantIsClassifiedFromADefaultedParam(t *testing.T) {
	// Every variant of ToolUnionParam in anthropic-sdk-go v1.63.0, and what each one
	// means for throttle's ability to account for the whole charge.
	cases := []struct {
		name        string
		tool        anth.ToolUnionParam
		accountable bool
	}{
		{"custom", anth.ToolUnionParam{OfTool: &anth.ToolParam{Name: "f"}}, true},
		{"bash", anth.ToolUnionParam{OfBashTool20250124: &anth.ToolBash20250124Param{}}, true},
		{"text_editor_20250124", anth.ToolUnionParam{OfTextEditor20250124: &anth.ToolTextEditor20250124Param{}}, true},
		{"text_editor_20250429", anth.ToolUnionParam{OfTextEditor20250429: &anth.ToolTextEditor20250429Param{}}, true},
		{"text_editor_20250728", anth.ToolUnionParam{OfTextEditor20250728: &anth.ToolTextEditor20250728Param{}}, true},
		{"memory_20250818", anth.ToolUnionParam{OfMemoryTool20250818: &anth.MemoryTool20250818Param{}}, true},
		{"web_search_20250305", anth.ToolUnionParam{OfWebSearchTool20250305: &anth.WebSearchTool20250305Param{}}, true},
		{"web_search_20260209", anth.ToolUnionParam{OfWebSearchTool20260209: &anth.WebSearchTool20260209Param{}}, true},
		{"web_search_20260318", anth.ToolUnionParam{OfWebSearchTool20260318: &anth.WebSearchTool20260318Param{}}, true},
		{"web_fetch_20250910", anth.ToolUnionParam{OfWebFetchTool20250910: &anth.WebFetchTool20250910Param{}}, true},
		{"web_fetch_20260209", anth.ToolUnionParam{OfWebFetchTool20260209: &anth.WebFetchTool20260209Param{}}, true},
		{"web_fetch_20260309", anth.ToolUnionParam{OfWebFetchTool20260309: &anth.WebFetchTool20260309Param{}}, true},
		{"web_fetch_20260318", anth.ToolUnionParam{OfWebFetchTool20260318: &anth.WebFetchTool20260318Param{}}, true},
		{"code_execution_20250522", anth.ToolUnionParam{OfCodeExecutionTool20250522: &anth.CodeExecutionTool20250522Param{}}, false},
		{"code_execution_20250825", anth.ToolUnionParam{OfCodeExecutionTool20250825: &anth.CodeExecutionTool20250825Param{}}, false},
		{"code_execution_20260120", anth.ToolUnionParam{OfCodeExecutionTool20260120: &anth.CodeExecutionTool20260120Param{}}, false},
		{"code_execution_20260521", anth.ToolUnionParam{OfCodeExecutionTool20260521: &anth.CodeExecutionTool20260521Param{}}, false},
		{"tool_search_bm25", anth.ToolUnionParam{OfToolSearchToolBm25_20251119: &anth.ToolSearchToolBm25_20251119Param{}}, false},
		{"tool_search_regex", anth.ToolUnionParam{OfToolSearchToolRegex20251119: &anth.ToolSearchToolRegex20251119Param{}}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, "1000")
			in := request(opus5, 2000)
			in.Tools = []anth.ToolUnionParam{c.tool}

			est, err := h.client.Estimate(context.Background(), in)
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			if got := est.Cost.Known(); got != c.accountable {
				t.Errorf("estimate cost Known = %v, want %v (reason %q). A variant that classifies as "+
					"nothing at all reads as fully accountable, which is how a server-side charge "+
					"would pass through unnoticed", got, c.accountable, est.Cost.Reason)
			}
		})
	}

	// The list above must stay exhaustive, so the count is asserted against the union's
	// own field count. A variant added to a newer SDK fails here, in a test whose message
	// says what to do, rather than silently going unclassified in production.
	if got, want := len(cases), unionVariants(anth.ToolUnionParam{}); got != want {
		t.Errorf("this test covers %d tool variants but ToolUnionParam has %d: classify the new "+
			"variant in toolKindOf and add it here", got, want)
	}
}

// A request whose model has no captured rate is refused under enforce, before Anthropic
// is called. An unknown model is never a free one.
func TestUnknownModelIsRefusedUnderEnforce(t *testing.T) {
	h := newHarness(t, "1000")
	const unknown = "claude-opus-7-20270301"

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-unknown-model", Params: request(unknown, 2000),
	})
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("NewMessage error = %v, want engine.ErrCostUnknown", err)
	}
	if h.api.callCount() != 0 {
		t.Error("Anthropic was called for a model throttle could not price")
	}
	// The identity is still recorded with the caller's exact string, so an operator can
	// see what was attempted and add a rate for it.
	rec := h.record(t, "req-unknown-model")
	if rec.Identity.ProviderModelID != unknown {
		t.Errorf("recorded model = %q, want %q: catalog recognition is not a condition of identity",
			rec.Identity.ProviderModelID, unknown)
	}
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeUnpriced)
	}
	if res.Estimate.Cost.Known() {
		t.Error("an unpriceable model must not produce a known estimate cost")
	}
}

// In monitor mode the same request executes, with its cost explicitly unknown rather than
// silently zero. Usage is fully known; only the money is not.
func TestUnknownModelExecutesUnderMonitorWithUnknownCost(t *testing.T) {
	h := monitorHarness(t, "1000")
	const unknown = "claude-opus-7-20270301"
	h.api.out = message(t, unknown, 1000, 500)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-monitor-model", Params: request(unknown, 2000),
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved", err)
	}
	if h.api.callCount() != 1 {
		t.Error("monitor mode should have executed the request")
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
	rec := h.record(t, "req-monitor-model")
	if rec.EnforcementMode != engine.ModeMonitor {
		t.Errorf("recorded mode = %q, want %q: posture cannot be reconstructed later",
			rec.EnforcementMode, engine.ModeMonitor)
	}
	if rec.ActualUsage.Empty() {
		t.Error("usage must be persisted even when the cost is unknown, since that is what lets a " +
			"later catalog update resolve the request without calling Anthropic again")
	}
}

// An unknown model is a valid identity. Catalog recognition is not a precondition of
// representing a request.
func TestUnknownModelRemainsAValidIdentity(t *testing.T) {
	h := newHarness(t, "1000")
	const unknown = "claude-something-nobody-has-shipped-yet"

	est, err := h.client.Estimate(context.Background(), request(unknown, 2000))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Identity.ProviderModelID != unknown {
		t.Errorf("ProviderModelID = %q, want %q verbatim", est.Identity.ProviderModelID, unknown)
	}
	if est.Identity.AccessProvider != "anthropic" || est.Identity.Publisher != "anthropic" {
		t.Errorf("identity = %+v: the access path establishes both fields regardless of the model", est.Identity)
	}
	if est.Usage.Empty() {
		t.Error("the usage estimate is knowable for any model: it is a function of the request")
	}
	if est.Cost.Known() {
		t.Error("an unpriced model yields an explicitly unknown cost, never a zero one")
	}
}

// Direct Anthropic and Claude on Bedrock are different access providers sharing a
// publisher. Collapsing them would answer "what did we spend on Claude" while making
// "which vendor do we owe" unanswerable.
func TestIdentityFields(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-identity", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	id := res.Identity
	if id.AccessProvider != "anthropic" {
		t.Errorf("AccessProvider = %q, want %q", id.AccessProvider, "anthropic")
	}
	if id.AccessProvider == "aws-bedrock" {
		t.Error("direct Anthropic must never identify as Bedrock")
	}
	if id.Publisher != "anthropic" {
		t.Errorf("Publisher = %q, want \"anthropic\" -- the same value a Bedrock Claude record "+
			"carries, which is exactly why the fields are separate", id.Publisher)
	}
	if id.Operation != "messages" {
		t.Errorf("Operation = %q, want %q", id.Operation, "messages")
	}
	if id.ProviderModelID != opus5 {
		t.Errorf("ProviderModelID = %q, want %q", id.ProviderModelID, opus5)
	}
	// Empty means "throttle claims no normalized name", not "something failed".
	if id.CanonicalModel != "" {
		t.Errorf("CanonicalModel = %q, want empty: this adapter consults no catalog at "+
			"identification time", id.CanonicalModel)
	}
	if got := h.client.Name(); got != "anthropic" {
		t.Errorf("Name() = %q, want %q", got, "anthropic")
	}
}

// An alias is preserved as the caller wrote it, and what Anthropic served is recorded
// separately. Both are facts and they answer different questions.
func TestServedModelIsRecordedSeparatelyFromTheAlias(t *testing.T) {
	h := newHarness(t, "1000")
	const alias = "claude-opus-5"
	const served = "claude-opus-5-20260115"
	h.api.out = message(t, served, 1000, 500)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-alias", Params: request(alias, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if res.Identity.ProviderModelID != alias {
		t.Errorf("ProviderModelID = %q, want the caller's own %q: rewriting the request into the "+
			"resolved ID would destroy the fact that an alias was used",
			res.Identity.ProviderModelID, alias)
	}
	if res.ServedModelID != served {
		t.Errorf("ServedModelID = %q, want %q: keeping only the alias would lose which model the "+
			"money was actually spent on", res.ServedModelID, served)
	}
	rec := h.record(t, "req-alias")
	if got := rec.Metadata["anthropic.served_model"]; got != served {
		t.Errorf("recorded served model = %q, want %q", got, served)
	}
	if rec.Identity.ProviderModelID != alias {
		t.Errorf("recorded model = %q, want %q", rec.Identity.ProviderModelID, alias)
	}
}

// A served model identical to the requested one is not recorded, because there is no
// second fact to keep.
func TestIdenticalServedModelIsNotRecorded(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-same-model", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if res.ServedModelID != "" {
		t.Errorf("ServedModelID = %q, want empty when the served model is the requested one", res.ServedModelID)
	}
	if _, ok := h.record(t, "req-same-model").Metadata["anthropic.served_model"]; ok {
		t.Error("a served model equal to the requested one adds nothing and should not be recorded")
	}
}

// An alias whose captured rate is missing settles unresolved rather than being priced as
// though the alias were immutable. Anthropic may re-aim an alias at a differently-priced
// model without notice, so an alias with no rate of its own is exactly the mutable-pricing
// ambiguity that has to fail safely.
func TestUnpricedAliasFailsSafelyRatherThanBeingResolved(t *testing.T) {
	h := monitorHarness(t, "1000")
	const alias = "claude-opus-latest"
	// The alias has no fixture rate; the model it resolved to does.
	h.api.out = message(t, opus5, 1000, 500)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-alias-unpriced", Params: request(alias, 2000),
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved", err)
	}
	if res.Cost.Known() {
		t.Error("an alias must not be priced through the model it happened to resolve to: the next " +
			"call may resolve to a different one")
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}
	// Both identities survive, so a later catalog carrying the alias can resolve this.
	if res.ServedModelID != opus5 {
		t.Errorf("ServedModelID = %q, want %q", res.ServedModelID, opus5)
	}
}

// The request reaches Anthropic exactly as the caller built it. A caller who cannot
// predict what is sent cannot reason about their own bill.
func TestRequestReachesProviderUnmodified(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	in.Temperature = anth.Float(0.3)
	in.TopK = anth.Int(40)
	in.StopSequences = []string{"###"}
	in.ServiceTier = anth.MessageNewParamsServiceTierAuto
	in.InferenceGeo = anth.String("us")
	in.Tools = []anth.ToolUnionParam{{
		OfWebSearchTool20250305: &anth.WebSearchTool20250305Param{MaxUses: anth.Int(3)},
	}}
	in.Metadata = anth.MetadataParam{UserID: anth.String("user-42")}

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-unmodified", Params: in,
	}); err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	got := h.api.lastParams()
	if got.MaxTokens != 2000 {
		t.Errorf("max_tokens reached Anthropic as %d, want 2000: throttle never adjusts the caller's "+
			"output cap, not even downward and not even when the budget is nearly exhausted", got.MaxTokens)
	}
	if got.Model != anth.Model(opus5) {
		t.Errorf("model = %q, want %q", got.Model, opus5)
	}
	if got.Temperature != in.Temperature || got.TopK != in.TopK {
		t.Error("sampling parameters were altered")
	}
	if len(got.StopSequences) != 1 || got.StopSequences[0] != "###" {
		t.Errorf("stop sequences = %v, want the caller's own", got.StopSequences)
	}
	if got.ServiceTier != in.ServiceTier {
		t.Errorf("service tier = %q, want %q", got.ServiceTier, in.ServiceTier)
	}
	if got.InferenceGeo != in.InferenceGeo {
		t.Errorf("inference geo = %v, want %v", got.InferenceGeo, in.InferenceGeo)
	}
	if len(got.Tools) != 1 || got.Tools[0].OfWebSearchTool20250305 == nil {
		t.Errorf("tools = %v, want the caller's own", got.Tools)
	}
	if got.Metadata.UserID != in.Metadata.UserID {
		t.Errorf("metadata = %v, want the caller's own", got.Metadata)
	}
	if len(got.Messages) != len(in.Messages) {
		t.Errorf("messages = %d, want %d", len(got.Messages), len(in.Messages))
	}
}

// max_tokens: 0 is a real value, not an omission -- Anthropic documents it as populating
// the prompt cache without generating a response. So it is passed through and reserved
// against as stated, with no output exposure invented for it.
func TestZeroMaxTokensIsRespectedRatherThanDefaulted(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 100,
		"cache_creation_input_tokens": 500000,
		"cache_creation": {"ephemeral_5m_input_tokens": 500000, "ephemeral_1h_input_tokens": 0},
		"output_tokens": 1
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-zero-max", Params: request(opus5, 0),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if got := h.api.lastParams().MaxTokens; got != 0 {
		t.Errorf("max_tokens reached Anthropic as %d, want 0: a documented value must not be "+
			"replaced with a throttle default", got)
	}
	// The estimate reserved nothing for output, because the caller asked for none.
	if got, _ := res.Estimate.Usage.Get(usage.OutputTokens); got != 0 {
		t.Errorf("estimated output = %d, want 0: reserving for generation the caller explicitly "+
			"asked not to happen would substitute an assumption for a fact", got)
	}
	if !res.Settled {
		t.Error("a cache-population request settles like any other")
	}
}

// A negative output cap is refused before anything is reserved. Anthropic would reject it
// too; refusing first is the same answer without the round trip.
func TestNegativeMaxTokensIsRefusedBeforeReserving(t *testing.T) {
	h := newHarness(t, "1000")

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-negative-max", Params: request(opus5, -1),
	}); err == nil {
		t.Fatal("a negative max_tokens should have been refused")
	}
	if h.api.callCount() != 0 {
		t.Error("Anthropic was called with a negative output cap")
	}
	if got := h.totals(t).Reserved; got != 0 {
		t.Errorf("Reserved = %s, want 0: nothing should have been reserved", got)
	}
}

// No Messages estimate is ever exact. Output cannot be known before generation, and the
// input half is not exact either: Anthropic documents count_tokens as an estimate that
// may include system-added tokens the caller is not billed for.
func TestEstimateIsNeverExact(t *testing.T) {
	h := newHarness(t, "1000")

	est, err := h.client.Estimate(context.Background(), request(opus5, 2000))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Quality == usage.QualityExact {
		t.Error("no Messages estimate may be QualityExact: Anthropic explicitly declines to " +
			"guarantee that count_tokens equals billable input")
	}
	if est.Quality != usage.QualityConservative {
		t.Errorf("Quality = %q, want %q with a counter configured", est.Quality, usage.QualityConservative)
	}
	if h.counter.callCount() != 1 {
		t.Errorf("the counter was called %d times, want 1", h.counter.callCount())
	}
	// The note says which method was used, because an estimate whose provenance is
	// invisible cannot be judged.
	if !strings.Contains(est.Note, "count_tokens") {
		t.Errorf("note = %q, should name the method", est.Note)
	}
	if !strings.Contains(est.Note, "max_tokens") {
		t.Errorf("note = %q, should say the output half is bounded by the caller's own cap", est.Note)
	}
}

// Without a counter the estimate is the heuristic, and it says so. A caller who declined
// the extra round trip gets a weaker bound, not a fabricated one.
func TestEstimateWithoutCounterIsHeuristic(t *testing.T) {
	h := newHarness(t, "1000", withoutCounter())

	est, err := h.client.Estimate(context.Background(), request(opus5, 2000))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Quality != usage.QualityHeuristic {
		t.Errorf("Quality = %q, want %q", est.Quality, usage.QualityHeuristic)
	}
	if h.counter.callCount() != 0 {
		t.Error("no counter was configured, so no counting call may be made")
	}
	if got, _ := est.Usage.Get(usage.InputTokens); got <= 0 {
		t.Errorf("estimated input = %d, want a positive figure from the content length", got)
	}
}

// A counting failure degrades the estimate rather than failing the request or inventing a
// number.
func TestCounterFailureDegradesTheEstimate(t *testing.T) {
	h := newHarness(t, "1000")
	h.counter.err = errors.New("count_tokens: 503 service unavailable")

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-count-fail", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if res.Estimate.Quality != usage.QualityHeuristic {
		t.Errorf("Quality = %q, want %q: a failed count must not be reported as a count",
			res.Estimate.Quality, usage.QualityHeuristic)
	}
	if !strings.Contains(res.Estimate.Note, "failed") {
		t.Errorf("note = %q, should say the count failed", res.Estimate.Note)
	}
	// The request still ran and still settled on real usage.
	if !res.Settled {
		t.Error("a counting failure must not fail the request")
	}
}

// Counting a request never mutates it, and no content it sees is persisted. The count is
// a number, and the number is all that comes back.
func TestCountingDoesNotMutateTheRequest(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	in.System = []anth.TextBlockParam{{Text: "a system instruction"}}
	in.StopSequences = []string{"###"}
	in.InferenceGeo = anth.String("us")
	before := fmt.Sprintf("%+v", in)

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-count-mutate", Params: in,
	}); err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if after := fmt.Sprintf("%+v", in); after != before {
		t.Errorf("the caller's request was modified by being counted:\nbefore %s\nafter  %s", before, after)
	}

	// The count endpoint is not a subset of the message endpoint, and the fields it does
	// not accept must not be sent to it.
	counted := h.counter.lastParams()
	if counted.Model != anth.Model(opus5) {
		t.Errorf("counted model = %q, want %q", counted.Model, opus5)
	}
	if len(counted.System.OfTextBlockArray) != 1 {
		t.Errorf("counted system = %v, want the caller's own block array", counted.System)
	}
	if len(counted.Messages) != len(in.Messages) {
		t.Errorf("counted messages = %d, want %d", len(counted.Messages), len(in.Messages))
	}
}

// No network call happens merely because a credential exists. Counting is opt-in per
// client, at construction.
func TestNoCountingCallWithoutAConfiguredCounter(t *testing.T) {
	h := newHarness(t, "1000", withoutCounter())

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-no-count", Params: request(opus5, 2000),
	}); err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if h.counter.callCount() != 0 {
		t.Error("a request made a counting call with no counter configured")
	}
}

// Spend attributes to every ancestor, and the whole chain survives on the record.
func TestChildSpendAttributesToAncestors(t *testing.T) {
	h := hierarchyHarness(t, map[string]string{"org": "1000", "team": "100"})

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-child", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	want := res.Charge.ActualCost
	if got := h.totalsFor(t, "team").Spent; got != want {
		t.Errorf("team Spent = %s, want %s", got, want)
	}
	if got := h.totalsFor(t, "org").Spent; got != want {
		t.Errorf("org Spent = %s, want %s: a parent's spend must include its children's", got, want)
	}

	rec := h.record(t, "req-child")
	seen := map[string]bool{}
	for _, s := range rec.Scopes {
		seen[s.BudgetID] = true
	}
	if !seen["team"] || !seen["org"] {
		t.Errorf("recorded scopes = %v, want both team and org: attribution has to survive the "+
			"reservation it was derived from", rec.Scopes)
	}
}

// A parent cannot be oversubscribed by concurrent children. Reservations are atomic
// across the chain, and this is where a race would surface as spend above the ceiling.
func TestParentIsNotOversubscribedUnderConcurrency(t *testing.T) {
	// The parent affords a handful of these requests; the children together would afford
	// far more if their holds did not both consume the parent.
	h := hierarchyHarness(t, map[string]string{"org": "0.25", "team-a": "100", "team-b": "100"})

	const attempts = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allowed int

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			child := "team-a"
			if i%2 == 1 {
				child = "team-b"
			}
			_, err := h.client.NewMessage(context.Background(), anthropic.Request{
				BudgetID:  child,
				RequestID: fmt.Sprintf("req-conc-%d", i),
				Params:    request(opus5, 2000),
			})
			if err != nil {
				return
			}
			mu.Lock()
			allowed++
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if allowed == 0 {
		t.Fatal("no request was admitted, so this test proves nothing about oversubscription")
	}
	if allowed == attempts {
		t.Fatal("every request was admitted, so the parent's ceiling was never the binding constraint")
	}

	ceiling := dollars(t, "0.25")
	if got := h.totalsFor(t, "org").Spent; got > ceiling {
		t.Errorf("org Spent = %s, above its %s allocation: concurrent children oversubscribed the parent",
			got, ceiling)
	}
	sum := h.totalsFor(t, "team-a").Spent + h.totalsFor(t, "team-b").Spent
	if sum != h.totalsFor(t, "org").Spent {
		t.Errorf("children spent %s but the parent recorded %s", sum, h.totalsFor(t, "org").Spent)
	}
}

// Concurrent requests that all settle unresolved keep the ancestor chain safe: nothing is
// charged, every hold stays encumbered, and the parent's encumbrance is the sum of its
// children's.
//
// The unresolved path is the one that ties money up rather than moving it, so a race here
// would hand out headroom twice for spend that already happened.
func TestConcurrentUnresolvedRequestsKeepTheChainSafe(t *testing.T) {
	h := hierarchyHarnessMode(t, map[string]string{"org": "1000", "team-a": "500", "team-b": "500"},
		engine.ModeMonitor)
	// A cache write whose lifetime the response never states: real tokens, no priceable
	// lifetime, so every one of these settles unresolved.
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 100,
		"cache_creation_input_tokens": 400000,
		"output_tokens": 100
	}`)

	const attempts = 20
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			child := "team-a"
			if i%2 == 1 {
				child = "team-b"
			}
			_, err := h.client.NewMessage(context.Background(), anthropic.Request{
				BudgetID:  child,
				RequestID: fmt.Sprintf("req-unres-%d", i),
				Params:    request(opus5, 2000),
			})
			if !errors.Is(err, anthropic.ErrCostUnresolved) {
				t.Errorf("NewMessage error = %v, want ErrCostUnresolved", err)
			}
		}(i)
	}
	wg.Wait()

	if got := h.totalsFor(t, "org").Spent; got != 0 {
		t.Errorf("org Spent = %s, want 0: an unresolved cost moves no money", got)
	}
	parent := h.totalsFor(t, "org").Reserved
	if parent == 0 {
		t.Fatal("the parent holds nothing, so the encumbrance was lost across the chain")
	}
	if sum := h.totalsFor(t, "team-a").Reserved + h.totalsFor(t, "team-b").Reserved; sum != parent {
		t.Errorf("children hold %s but the parent holds %s: an encumbrance was lost or double-counted "+
			"across the chain", sum, parent)
	}
}

// The captured quote is the accounting basis, so a catalog change between admission and
// settlement cannot alter what a request cost.
func TestSettlementRepricesFromTheCapturedQuote(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-quote", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if !res.Quote.Valid() {
		t.Fatal("a fixture catalog is a RateSource, so the quote must have been captured")
	}
	// The record carries it, which is what lets reconciliation reprice a crashed request
	// from frozen rates rather than by calling Anthropic again.
	rec := h.record(t, "req-quote")
	if !rec.Quote.Valid() {
		t.Error("the durable record must carry the captured quote")
	}
	if rec.Quote.Provenance.Source == "" {
		t.Error("the captured quote must name its provenance")
	}
	// Priced from the quote, not from a fresh lookup.
	if want := dollars(t, "0.0175"); rec.ActualCost.Amount != want {
		t.Errorf("recorded cost = %s, want %s", rec.ActualCost.Amount, want)
	}
}

// The service tier is preserved and prices nothing. Anthropic's published token rates do
// not vary by tier -- priority tier is documented as capacity commitments with burndown
// ratios rather than a price sheet -- so inferring a contract price from public rates
// would be inventing one.
func TestServiceTierIsPreservedAndPricesNothing(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	in.ServiceTier = anth.MessageNewParamsServiceTierStandardOnly
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000, "output_tokens": 500, "service_tier": "priority"
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-tier", Params: in,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if res.Identity.ServiceTier != "priority" {
		t.Errorf("ServiceTier = %q, want %q from the response", res.Identity.ServiceTier, "priority")
	}
	// Identical to the same request on any tier, because the tier selects no rate.
	if want := dollars(t, "0.0175"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: the tier must not act as a monetary selector",
			res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Errorf("a tier that prices nothing must not make the cost incomplete: %s", res.Cost.Reason)
	}
	// Both facts survive: what was asked for and what served it. An organization holding a
	// priority contract needs both to reconcile throttle's figures against its invoice.
	rec := h.record(t, "req-tier")
	if got := rec.Metadata["anthropic.requested_service_tier"]; got != "standard_only" {
		t.Errorf("requested tier = %q, want %q", got, "standard_only")
	}
	if rec.Identity.ServiceTier != "priority" {
		t.Errorf("recorded served tier = %q, want %q", rec.Identity.ServiceTier, "priority")
	}
}

// "auto" is not a tier, it is an instruction to resolve one, so it is not recorded as one.
func TestAutoTierIsNotRecordedAsATier(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	in.ServiceTier = anth.MessageNewParamsServiceTierAuto
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000, "output_tokens": 500, "service_tier": "standard"
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-auto-tier", Params: in,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if got := h.record(t, "req-auto-tier").Metadata["anthropic.requested_service_tier"]; got != "" {
		t.Errorf("requested tier = %q, want empty: 'auto' names no price sheet", got)
	}
	if res.Identity.ServiceTier != "standard" {
		t.Errorf("served tier = %q, want %q: the response says what auto resolved to",
			res.Identity.ServiceTier, "standard")
	}
	// And the request still reached Anthropic with auto, since throttle rewrites nothing.
	if got := h.api.lastParams().ServiceTier; got != anth.MessageNewParamsServiceTierAuto {
		t.Errorf("service tier reached Anthropic as %q, want %q", got, anth.MessageNewParamsServiceTierAuto)
	}
}

// Web fetch is recorded as metadata, where a statistic with no price belongs.
func TestWebFetchCountIsRecordedAsMetadata(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	in.Tools = []anth.ToolUnionParam{{
		OfWebFetchTool20260318: &anth.WebFetchTool20260318Param{},
	}}
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000, "output_tokens": 500,
		"server_tool_use": {"web_fetch_requests": 6}
	}`)

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-fetch-meta", Params: in,
	}); err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if got := h.record(t, "req-fetch-meta").Metadata["anthropic.web_fetch_requests"]; got != "6" {
		t.Errorf("recorded web fetch count = %q, want %q", got, "6")
	}
}

// Latency is the caller's wall clock, and there is no provider-reported counterpart --
// a Message carries no timing field of any kind. Recording zero is the honest outcome;
// deriving one from wall clock would invent a measurement.
func TestProviderLatencyIsNotInvented(t *testing.T) {
	h := newHarness(t, "1000")

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-latency", Params: request(opus5, 2000),
	}); err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if got := h.record(t, "req-latency").ProviderLatency; got != 0 {
		t.Errorf("ProviderLatency = %v, want 0: a Message reports no timing, so anything here was "+
			"derived rather than measured", got)
	}
}

// Observe normalizes a message obtained outside the governed path, and reports it as
// unpriced rather than free.
func TestObserveReportsUsageWithoutPricingIt(t *testing.T) {
	h := newHarness(t, "1000")

	actual, err := h.client.Observe(context.Background(), usageReply(t, opus5, `{
		"input_tokens": 1000, "output_tokens": 500, "cache_read_input_tokens": 100000
	}`))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got, _ := actual.Usage.Get(usage.CacheReadTokens); got != 100000 {
		t.Errorf("CacheReadTokens = %d, want 100000", got)
	}
	if actual.Cost.Known() {
		t.Error("Observe reports usage only, so its cost must be explicitly unknown rather than zero")
	}
	if actual.Identity.AccessProvider != "anthropic" {
		t.Errorf("AccessProvider = %q, want %q", actual.Identity.AccessProvider, "anthropic")
	}
}

// Construction refuses a configuration that cannot account for anything, rather than
// failing later on the request path.
func TestNewRequiresItsDependencies(t *testing.T) {
	if _, err := anthropic.New(anthropic.Config{}); !errors.Is(err, anthropic.ErrNoClient) {
		t.Errorf("New with no client = %v, want ErrNoClient", err)
	}
	if _, err := anthropic.New(anthropic.Config{Client: &fakeMessages{}}); err == nil {
		t.Error("New with no engine should have failed: a client that cannot govern spend is not a " +
			"governed client")
	}
}

// monitorHarness builds a monitor-mode budget, for the tests about what happens when a
// request throttle cannot fully price is allowed to run anyway.
func monitorHarness(t *testing.T, allocation string) *harness {
	t.Helper()
	return hierarchyHarnessMode(t, map[string]string{"team": allocation}, engine.ModeMonitor)
}

// hierarchyHarness builds a budget chain under enforce. The map's "org" entry is the
// root and every other entry is its child, which is the only shape these tests need.
func hierarchyHarness(t *testing.T, allocations map[string]string) *harness {
	t.Helper()
	return hierarchyHarnessMode(t, allocations, engine.ModeEnforce)
}

func hierarchyHarnessMode(t *testing.T, allocations map[string]string, mode engine.Mode) *harness {
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

	anchor := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	register := func(id, parent, allocation string) {
		if err := eng.Register(context.Background(), budget.Definition{
			ID: id, ParentID: parent, Allocation: dollars(t, allocation),
			Recurrence: budget.RecurMonthly, AnchorAt: anchor,
		}, mode); err != nil {
			t.Fatalf("Register %s: %v", id, err)
		}
	}
	// The root first, so a child's parent always exists by the time it is registered.
	root := ""
	if a, ok := allocations["org"]; ok {
		register("org", "", a)
		root = "org"
	}
	for id, a := range allocations {
		if id == "org" {
			continue
		}
		register(id, root, a)
	}

	return buildHarness(t, eng, store, clock)
}

// unionVariants counts the variant fields of an SDK param union, so a test asserting it
// covers all of them cannot silently fall behind a newer SDK.
//
// The union's variants are its exported pointer fields named Of*; the rest is the SDK's
// own bookkeeping. Reflection is used here and nowhere in production code: this is a
// question about the shape of a generated type, which is exactly what a test should be
// allowed to ask and production code should not need to.
func unionVariants(u anth.ToolUnionParam) int {
	tv := reflect.TypeOf(u)
	n := 0
	for i := 0; i < tv.NumField(); i++ {
		if strings.HasPrefix(tv.Field(i).Name, "Of") {
			n++
		}
	}
	return n
}

// apiError builds a real SDK API error, so the classification path under test is the
// production one rather than a stand-in.
//
// The construction is roundabout for a reason. The SDK's Error carries its raw body and
// its parsed error type in unexported state that only UnmarshalJSON populates -- and
// UnmarshalJSON is also what makes Error() embed the body verbatim, which is the exact
// behaviour redaction exists to contain. Assigning the struct fields directly would
// produce an error whose Error() leaks nothing, so the privacy tests would pass against
// a fixture that could not have failed them.
//
// The message deliberately looks like leaked content for the same reason.
func apiError(t *testing.T, status int, kind, message string) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	body := fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, kind, message)

	apiErr := &anth.Error{}
	if err := apiErr.UnmarshalJSON([]byte(body)); err != nil {
		t.Fatalf("unmarshalling an api error: %v", err)
	}
	apiErr.StatusCode = status
	apiErr.RequestID = "req_011CQ" + kind
	apiErr.Request = req
	apiErr.Response = &http.Response{StatusCode: status}

	// The fixture is worthless unless it really does behave like the production error, so
	// both halves of that are checked here rather than assumed by every caller.
	if got := apiErr.Type(); got != shared.ErrorType(kind) {
		t.Fatalf("the fixture's error type is %q, want %q: unmarshalling did not populate it, so "+
			"the classification path is not being exercised", got, kind)
	}
	if !strings.Contains(apiErr.Error(), message) {
		t.Fatalf("the fixture's Error() does not embed the response body, so a privacy test using " +
			"it could not fail")
	}
	return apiErr
}
