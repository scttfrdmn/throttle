package openai_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/usage"
)

// A provider error with no usage means nothing was billed, so the headroom goes back.
func TestProviderErrorReleasesReservation(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = nil
	h.api.err = errors.New("the provider is having a bad day")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-fail", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrProvider) {
		t.Fatalf("Respond error = %v, want ErrProvider", err)
	}
	if res.Settled {
		t.Error("nothing should settle when the provider billed nothing")
	}

	tot := h.totals(t)
	if tot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0: an unbilled failure must release its hold", tot.Reserved)
	}
	if tot.Spent != 0 {
		t.Errorf("Spent = %s, want 0", tot.Spent)
	}
	if rec := h.record(t, "req-fail"); rec.Status != activity.StatusReleased {
		t.Errorf("activity status = %q, want %q", rec.Status, activity.StatusReleased)
	}
}

// A provider error that arrives *with* usage is real spend. A partially served
// request the provider billed for must be settled, not released.
func TestProviderErrorWithUsageSettles(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = completedResponse(t, gpt51, 1000, 500)
	h.api.err = errors.New("the connection dropped after generation")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-partial", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrProvider) {
		t.Fatalf("Respond error = %v, want ErrProvider", err)
	}
	if !res.Settled {
		t.Fatal("usage reported alongside an error is billable and must settle")
	}
	if want := dollars(t, "0.00625"); h.totals(t).Spent != want {
		t.Errorf("Spent = %s, want %s", h.totals(t).Spent, want)
	}
	if rec := h.record(t, "req-partial"); rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want %q: the provider error must stay visible even though "+
			"the usage settled", rec.Outcome, activity.OutcomeProviderError)
	}
}

// An OpenAI 429 is a provider failure, not throttle's budget denial. A caller that
// cannot tell them apart will go looking at the wrong system, and will retry the wrong
// thing.
func TestRateLimitIsNotABudgetDenial(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = nil
	h.api.err = apiError(t, http.StatusTooManyRequests, "rate_limit_exceeded", "rate_limit_error",
		"Rate limit reached for gpt-5.1 in organization org-x")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-429", Params: request(gpt51, maxOut(2000)),
	})

	if !errors.Is(err, openai.ErrProvider) {
		t.Fatalf("Respond error = %v, want ErrProvider", err)
	}
	if errors.Is(err, engine.ErrDenied) || errors.Is(err, engine.ErrCostUnknown) {
		t.Errorf("a provider rate limit was reported as a budget refusal: %v", err)
	}
	if !res.Decision.Admitted {
		t.Error("the budget admitted this request; the refusal came from OpenAI")
	}

	rec := h.record(t, "req-429")
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want %q, not a budget denial", rec.Outcome, activity.OutcomeProviderError)
	}
	if rec.Outcome == activity.OutcomeBudgetDenied {
		t.Error("an OpenAI 429 must never be recorded as a budget denial: they are different failure domains")
	}
	// The classification is kept, since that is what an operator acts on.
	if !strings.Contains(rec.Error, "429") || !strings.Contains(rec.Error, "rate_limit") {
		t.Errorf("recorded error %q should identify the HTTP status and the provider's classification", rec.Error)
	}
}

// A budget denial does not call the provider at all, and is recorded as throttle's own
// refusal.
func TestBudgetDenialDoesNotCallProvider(t *testing.T) {
	// An allocation far too small for the reservation this request needs.
	h := newHarness(t, "0.0001")

	_, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-denied", Params: request(gpt51, maxOut(100000)),
	})
	if !errors.Is(err, engine.ErrDenied) {
		t.Fatalf("Respond error = %v, want ErrBudgetExceeded", err)
	}
	if h.api.callCount() != 0 {
		t.Error("a denied request must not reach the provider")
	}
	rec := h.record(t, "req-denied")
	if rec.Outcome != activity.OutcomeBudgetDenied {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeBudgetDenied)
	}
	if rec.Status != activity.StatusDenied {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusDenied)
	}
}

// An incomplete response that carries authoritative usage is charged for that usage.
// Generation stopping earlier than the caller hoped is not a refund.
func TestIncompleteResponseWithUsageChargesRealUsage(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_inc", "object": "response", "status": "incomplete", "model": %q,
		"incomplete_details": {"reason": "max_output_tokens"},
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-inc", Params: request(gpt51, maxOut(500)),
	})
	// The caller is told generation was truncated, so they cannot mistake it for a
	// complete answer.
	if !errors.Is(err, openai.ErrResponseIncomplete) {
		t.Fatalf("Respond error = %v, want ErrResponseIncomplete", err)
	}
	if !res.Settled {
		t.Fatal("an incomplete response with authoritative usage must still settle: those tokens were billed")
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if got := h.totals(t).Reserved; got != 0 {
		t.Errorf("Reserved = %s, want 0: the hold must not be left behind for a truncated response", got)
	}
	rec := h.record(t, "req-inc")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusSettled)
	}
	// The reason is a closed enumeration, so it is safe to persist and useful to keep.
	if !strings.Contains(rec.Error, "max_output_tokens") {
		t.Errorf("recorded error %q should say why generation stopped", rec.Error)
	}
}

// A failed response that reports usage is still charged: the provider billed for the
// tokens it consumed before failing.
func TestFailedResponseWithUsageSettles(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_failed", "object": "response", "status": "failed", "model": %q,
		"error": {"code": "server_error", "message": "something went wrong internally"},
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-failed", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrResponseFailed) {
		t.Fatalf("Respond error = %v, want ErrResponseFailed", err)
	}
	if !res.Settled {
		t.Fatal("a failed response that reported usage must settle on that usage")
	}
	rec := h.record(t, "req-failed")
	if !strings.Contains(rec.Error, "server_error") {
		t.Errorf("recorded error %q should carry the provider's error code", rec.Error)
	}
	// The provider's free-text message can quote the prompt, so it is not persisted.
	if strings.Contains(rec.Error, "something went wrong internally") {
		t.Error("the provider's free-text message must not be persisted: it is unbounded content")
	}
}

// A failed response with no usage has nothing known to have been billed, so the hold
// is released.
func TestFailedResponseWithoutUsageReleases(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_nf", "object": "response", "status": "failed", "model": %q,
		"error": {"code": "invalid_prompt", "message": "your prompt was rejected"}
	}`, gpt51))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-nf", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrResponseFailed) {
		t.Fatalf("Respond error = %v, want ErrResponseFailed", err)
	}
	if res.Settled {
		t.Error("nothing should settle when no usage was reported")
	}
	if got := h.totals(t).Reserved; got != 0 {
		t.Errorf("Reserved = %s, want 0", got)
	}
	if rec := h.record(t, "req-nf"); rec.Status != activity.StatusReleased {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusReleased)
	}
}

// A cancelled call is genuinely ambiguous: OpenAI cannot cancel a synchronous response
// server-side, so the request may well have been served and billed. The hold stays
// outstanding rather than being released on a guess.
func TestCancellationLeavesReservationOutstanding(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.block = make(chan struct{})
	t.Cleanup(func() { close(h.api.block) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.client.Respond(ctx, openai.Request{
			BudgetID: "team", RequestID: "req-cancel", Params: request(gpt51, maxOut(2000)),
		})
		done <- err
	}()

	waitFor(t, func() bool { return h.api.callCount() == 1 })
	cancel()

	err := <-done
	if !errors.Is(err, openai.ErrOutcomeUnknown) {
		t.Fatalf("Respond error = %v, want ErrOutcomeUnknown", err)
	}
	// This is the load-bearing assertion: a cancelled call is not a free call.
	if errors.Is(err, openai.ErrProvider) {
		t.Error("a cancelled call must not be reported as a provider failure: the request may have been served")
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: an ambiguous outcome must leave the hold outstanding, since the request may have been billed")
	}
	rec := h.record(t, "req-cancel")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
	if rec.Outcome != activity.OutcomeCancelled {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeCancelled)
	}
	if rec.ActualCost.Known() {
		t.Error("the cost of an interrupted call is not known")
	}
}

// A deadline is recorded distinctly from a cancellation. Both are ambiguous, but they
// mean different things operationally: one is a client giving up, the other a limit
// being hit.
func TestDeadlineIsDistinguishedFromCancellation(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.block = make(chan struct{})
	t.Cleanup(func() { close(h.api.block) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := h.client.Respond(ctx, openai.Request{
		BudgetID: "team", RequestID: "req-timeout", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrOutcomeUnknown) {
		t.Fatalf("Respond error = %v, want ErrOutcomeUnknown", err)
	}
	if rec := h.record(t, "req-timeout"); rec.Outcome != activity.OutcomeTimeout {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeTimeout)
	}
}

// A completed response with no usage metadata at all is an unresolvable accounting
// state, not a free request.
func TestCompletedResponseWithoutUsageIsOutstanding(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_nousage", "object": "response", "status": "completed", "model": %q
	}`, gpt51))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-nousage", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrAccounting) {
		t.Fatalf("Respond error = %v, want ErrAccounting", err)
	}
	if res.Cost.Known() {
		t.Error("a response with no usage metadata cannot have a known cost")
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("the hold must stay outstanding: the request ran and was presumably billed")
	}
}

// A hosted tool whose charge OpenAI bills outside the usage object means throttle
// cannot account for the whole bill. The request is supported, but its cost is
// explicitly a floor -- never reported as fully priced.
func TestHostedToolRequestIsNotFullyPriced(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(gpt51, maxOut(2000))
	in.Tools = []responses.ToolUnionParam{{
		OfWebSearch: &responses.WebSearchToolParam{Type: responses.WebSearchToolTypeWebSearch},
	}}

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-tool", Params: in,
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Respond error = %v, want ErrCostUnresolved", err)
	}

	// The tokens are known and recorded in full.
	if got, _ := res.Usage.Get(usage.InputTokens); got != 1000 {
		t.Errorf("InputTokens = %d, want 1000: token usage is still authoritative", got)
	}
	// But the cost is a floor, not a total.
	if res.Cost.Known() {
		t.Error("a request carrying a web search tool must not be reported as fully priced: " +
			"OpenAI bills web search per call, outside the usage object")
	}
	if res.Cost.State() != usage.CostPartial {
		t.Errorf("cost state = %v, want CostPartial: the token cost is a real floor", res.Cost.State())
	}
	if res.Cost.Amount <= 0 {
		t.Error("the floor should carry the token cost that is known, rather than zero")
	}
	if !strings.Contains(res.Cost.Reason, "web_search") {
		t.Errorf("cost reason %q should name the tool whose charge could not be accounted for", res.Cost.Reason)
	}
	if !res.Unresolved {
		t.Error("the result should be marked unresolved")
	}

	// The hold stays encumbered rather than being released, so the budget does not
	// offer already-spent headroom to the next caller.
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: an unresolved cost must keep its hold encumbered")
	}
	rec := h.record(t, "req-tool")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusUnresolved)
	}
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeUnpriced)
	}
	if rec.ActualCost.Known() {
		t.Error("the durable record must not claim a known cost either")
	}
}

// The caller's own function tool is charged as tokens and nothing else. Assigning an
// OpenAI dollar charge to code OpenAI never ran would invent a cost.
func TestCallerFunctionToolGetsNoInventedCost(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(gpt51, maxOut(2000))
	in.Tools = []responses.ToolUnionParam{{
		OfFunction: &responses.FunctionToolParam{
			Name:        "lookup_order",
			Description: param.NewOpt("look up an order in the caller's own database"),
			Parameters:  map[string]any{"type": "object"},
		},
	}}

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-fn", Params: in,
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if !res.Cost.Known() {
		t.Errorf("a caller's own function tool is billed as tokens only, so the cost is fully "+
			"known: %s", res.Cost.Reason)
	}
	// Identical to the same request with no tools: the caller's execution is theirs to
	// pay for and OpenAI charges nothing extra for it.
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: no cost may be invented for the caller's own function",
			res.Charge.ActualCost, want)
	}
	if !res.Settled {
		t.Error("a token-only request must settle normally")
	}
}

// A remote MCP server's own charges are between the caller and that server. OpenAI
// bills the tokens, which are reported in full, and throttle invents nothing for the
// third party.
func TestRemoteMCPToolGetsNoInventedCost(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(gpt51, maxOut(2000))
	in.Tools = []responses.ToolUnionParam{{
		OfMcp: &responses.ToolMcpParam{
			ServerLabel: "example-docs",
			ServerURL:   param.NewOpt("https://mcp.example.com/sse"),
		},
	}}

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-mcp", Params: in,
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if !res.Cost.Known() {
		t.Errorf("an MCP tool's OpenAI charge is its tokens, which are known: %s", res.Cost.Reason)
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: no cost may be invented for a third party's MCP server",
			res.Charge.ActualCost, want)
	}
}

// A tool whose separate charge could not be verified is treated conservatively rather
// than assumed free. OpenAI adds hosted tools with their own pricing, and the expensive
// mistake is calling such a request fully priced.
func TestUnverifiedToolChargeIsTreatedConservatively(t *testing.T) {
	// image_generation is in the pessimistic set: no separate charge could be verified
	// in either direction, so it is classified as billed outside usage.
	h := newHarness(t, "1000")

	in := request(gpt51, maxOut(2000))
	in.Tools = []responses.ToolUnionParam{{
		OfImageGeneration: &responses.ToolImageGenerationParam{},
	}}

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-unverified-tool", Params: in,
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Respond error = %v, want ErrCostUnresolved", err)
	}
	if res.Cost.Known() {
		t.Error("a tool whose charge could not be verified must not be treated as token-only")
	}
}

// Every hosted tool the SDK offers is classified, and classified from a param built the
// way a caller actually builds one.
//
// This is a trap worth a dedicated test. Most tool variants declare their type as a
// single-valued constant whose Go zero value is the empty string -- the SDK fills it in
// while marshalling. So `&responses.ToolCodeInterpreterParam{}` is both the idiomatic
// way to ask for a tool and a struct whose Type field reads as empty. Classifying by
// that field would silently skip the tool and report a request throttle cannot fully
// price as fully priced, which is the exact failure this whole path exists to prevent.
func TestEveryHostedToolIsClassifiedFromADefaultedParam(t *testing.T) {
	// Tools OpenAI hosts and bills separately, in units no response reports. Each is
	// built with no Type set, as a caller would.
	hosted := map[string]responses.ToolUnionParam{
		"web_search":                {OfWebSearch: &responses.WebSearchToolParam{}},
		"web_search_preview":        {OfWebSearchPreview: &responses.WebSearchPreviewToolParam{}},
		"file_search":               {OfFileSearch: &responses.FileSearchToolParam{}},
		"code_interpreter":          {OfCodeInterpreter: &responses.ToolCodeInterpreterParam{}},
		"shell":                     {OfShell: &responses.FunctionShellToolParam{}},
		"computer":                  {OfComputer: &responses.ComputerToolParam{}},
		"computer_use_preview":      {OfComputerUsePreview: &responses.ComputerUsePreviewToolParam{}},
		"image_generation":          {OfImageGeneration: &responses.ToolImageGenerationParam{}},
		"programmatic_tool_calling": {OfProgrammaticToolCalling: &responses.ToolProgrammaticToolCallingParam{}},
		"namespace":                 {OfNamespace: &responses.NamespaceToolParam{}},
		"tool_search":               {OfToolSearch: &responses.ToolSearchToolParam{}},
		"apply_patch":               {OfApplyPatch: &responses.ApplyPatchToolParam{}},
	}

	for name, tool := range hosted {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "1000")
			in := request(gpt51, maxOut(2000))
			in.Tools = []responses.ToolUnionParam{tool}

			res, err := h.client.Respond(context.Background(), openai.Request{
				BudgetID: "team", RequestID: "req-hosted-" + name, Params: in,
			})
			if !errors.Is(err, openai.ErrCostUnresolved) {
				t.Fatalf("Respond error = %v, want ErrCostUnresolved", err)
			}
			if res.Cost.Known() {
				t.Errorf("a request carrying %s was reported as fully priced", name)
			}
			if !strings.Contains(res.Cost.Reason, name) {
				t.Errorf("cost reason %q should name %s", res.Cost.Reason, name)
			}
		})
	}

	// The other side of the same trap: tools OpenAI bills only as tokens must still be
	// recognized from a defaulted param, or every request using one would be needlessly
	// marked unresolved.
	tokenOnly := map[string]responses.ToolUnionParam{
		"function":    {OfFunction: &responses.FunctionToolParam{Name: "f", Parameters: map[string]any{}}},
		"custom":      {OfCustom: &responses.CustomToolParam{Name: "c"}},
		"mcp":         {OfMcp: &responses.ToolMcpParam{ServerLabel: "s"}},
		"local_shell": {OfLocalShell: &responses.ToolLocalShellParam{}},
	}

	for name, tool := range tokenOnly {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "1000")
			in := request(gpt51, maxOut(2000))
			in.Tools = []responses.ToolUnionParam{tool}

			res, err := h.client.Respond(context.Background(), openai.Request{
				BudgetID: "team", RequestID: "req-tokenonly-" + name, Params: in,
			})
			if err != nil {
				t.Fatalf("Respond: %v", err)
			}
			if !res.Cost.Known() {
				t.Errorf("%s is billed as tokens only, so the cost is fully known: %s", name, res.Cost.Reason)
			}
		})
	}
}

// In enforce mode, a model throttle cannot price does not execute. An unknown price is
// not a zero price.
func TestUnknownPriceBlocksExecutionInEnforceMode(t *testing.T) {
	h := newHarness(t, "1000")

	// A plausible future model, absent from the fixture catalog.
	const unknown = "gpt-6-turbo-2027-01-01"
	_, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-unpriced", Params: request(unknown, maxOut(2000)),
	})
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("Respond error = %v, want ErrCostUnknown", err)
	}
	if h.api.callCount() != 0 {
		t.Error("enforce mode must not execute a request whose cost it cannot determine")
	}
	rec := h.record(t, "req-unpriced")
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeUnpriced)
	}
	// The identity is still recorded with the exact model ID, so an operator can see
	// what was attempted and add a price for it.
	if rec.Identity.ProviderModelID != unknown {
		t.Errorf("recorded model = %q, want %q", rec.Identity.ProviderModelID, unknown)
	}
}

// In monitor mode the same request executes, with its cost explicitly unknown rather
// than silently zero.
func TestMonitorModeExecutesWithExplicitlyUnknownCost(t *testing.T) {
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
	h := buildHarness(t, eng, store, clock)

	const unknown = "gpt-6-turbo-2027-01-01"
	h.api.out = completedResponse(t, unknown, 1000, 500)

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-monitor", Params: request(unknown, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Respond error = %v, want ErrCostUnresolved", err)
	}
	if h.api.callCount() != 1 {
		t.Error("monitor mode should have executed the request")
	}
	if res.Response == nil {
		t.Error("the caller should still get their response in monitor mode")
	}

	// Usage is fully known; only the money is not.
	if got, _ := res.Usage.Get(usage.InputTokens); got != 1000 {
		t.Errorf("InputTokens = %d, want 1000: usage is knowable without a price", got)
	}
	if res.Cost.Known() {
		t.Error("cost must remain explicitly unknown for an unpriced model")
	}
	if res.Cost.State() != usage.CostUnknown {
		t.Errorf("cost state = %v, want CostUnknown", res.Cost.State())
	}
	// The one thing that must never happen: an unpriced request reported as free.
	if res.Cost.Amount != 0 && res.Cost.Known() {
		t.Error("an unknown cost must not be rendered as a known amount")
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s: an unpriceable request must not be settled at a made-up amount", got)
	}
	rec := h.record(t, "req-monitor")
	if rec.EnforcementMode != engine.ModeMonitor {
		t.Errorf("recorded mode = %q, want %q: posture cannot be reconstructed later",
			rec.EnforcementMode, engine.ModeMonitor)
	}
	if rec.ActualUsage.Empty() {
		t.Error("the usage must be persisted even though the cost is unknown")
	}
}

// An unknown model is a valid identity. throttle does not require a model to exist in
// its catalog before it can be represented, and the exact provider ID is preserved.
func TestUnknownModelRemainsAValidIdentity(t *testing.T) {
	h := newHarness(t, "1000")
	const unknown = "gpt-6-turbo-2027-01-01"

	est, err := h.client.Estimate(context.Background(), request(unknown, maxOut(2000)))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if !est.Identity.Valid() {
		t.Error("an unknown model must still produce a valid identity")
	}
	if est.Identity.ProviderModelID != unknown {
		t.Errorf("ProviderModelID = %q, want the caller's exact string %q",
			est.Identity.ProviderModelID, unknown)
	}
	if est.Identity.Known() {
		t.Error("Known() reports whether throttle recognizes the model, which it does not here")
	}
	// The distinction that matters: unrecognized is not free.
	if est.Cost.Known() {
		t.Error("an unknown model's cost must be unknown, not zero")
	}
	if est.Usage.Empty() {
		t.Error("usage is still estimable for a model throttle does not recognize")
	}
}

// The model OpenAI reports serving is kept separately from the one the caller asked
// for. Both are facts, and they answer different questions.
func TestServedModelIsRecordedSeparately(t *testing.T) {
	h := newHarness(t, "1000")
	// A request for an alias, served by a dated snapshot.
	h.api.out = completedResponse(t, "gpt-4o-mini-2024-07-18", 1000, 500)

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-served", Params: request(fourOM, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if res.Identity.ProviderModelID != fourOM {
		t.Errorf("ProviderModelID = %q, want the requested %q: the caller's string is not overwritten",
			res.Identity.ProviderModelID, fourOM)
	}
	if res.ServedModelID != "gpt-4o-mini-2024-07-18" {
		t.Errorf("ServedModelID = %q, want the served snapshot", res.ServedModelID)
	}
	// Pricing follows the requested ID, which is what the catalog and the pricing page
	// are keyed on.
	if want := dollars(t, "0.00045"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if got := h.record(t, "req-served").Metadata["openai.served_model"]; got != "gpt-4o-mini-2024-07-18" {
		t.Errorf("recorded served model = %q, want the served snapshot", got)
	}
}

// A response reporting the same model it was asked for records no difference. The
// served-model field means "this differed", so populating it identically would make it
// meaningless.
func TestIdenticalServedModelIsNotRecorded(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-same", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if res.ServedModelID != "" {
		t.Errorf("ServedModelID = %q, want empty when the served model matches the request", res.ServedModelID)
	}
	if _, ok := h.record(t, "req-same").Metadata["openai.served_model"]; ok {
		t.Error("no served-model metadata should be written when nothing differed")
	}
}

// throttle does not modify the caller's request. This is checked against what the fake
// actually received, since the promise is about what reaches OpenAI.
func TestRequestReachesProviderUnmodified(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(gpt51, maxOut(77))
	in.Store = param.NewOpt(true)
	in.Temperature = param.NewOpt(0.5)
	in.ServiceTier = responses.ResponseNewParamsServiceTierFlex
	in.Instructions = param.NewOpt("be terse")

	if _, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-verbatim", Params: in,
	}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	got := h.api.lastParams()
	// The output cap in particular: throttle observes it and uses it for estimation,
	// and must not adjust it. No adaptive output reduction.
	if !got.MaxOutputTokens.Valid() || got.MaxOutputTokens.Value != 77 {
		t.Errorf("MaxOutputTokens = %v, want the caller's 77 unmodified", got.MaxOutputTokens)
	}
	// Retention is the caller's decision, and throttle's own non-persistence is a
	// separate matter from the provider's.
	if !got.Store.Valid() || got.Store.Value != true {
		t.Errorf("Store = %v, want the caller's setting untouched", got.Store)
	}
	if got.ServiceTier != responses.ResponseNewParamsServiceTierFlex {
		t.Errorf("ServiceTier = %q, want the caller's %q", got.ServiceTier, responses.ResponseNewParamsServiceTierFlex)
	}
	if got.Instructions.Value != "be terse" {
		t.Errorf("Instructions = %q, want the caller's text", got.Instructions.Value)
	}
	if got.Temperature.Value != 0.5 {
		t.Errorf("Temperature = %v, want 0.5", got.Temperature.Value)
	}
}

// A request that sets no output cap is not given one. throttle's default bounds the
// estimate only.
func TestUncappedRequestIsNotGivenACap(t *testing.T) {
	h := newHarness(t, "1000")

	if _, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-uncapped", Params: request(gpt51, nil),
	}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got := h.api.lastParams(); got.MaxOutputTokens.Valid() {
		t.Errorf("MaxOutputTokens = %v: throttle must not add an output cap the caller did not ask for",
			got.MaxOutputTokens.Value)
	}

	// The estimate does use a ceiling, and says which one.
	est, err := h.client.Estimate(context.Background(), request(gpt51, nil))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got, _ := est.Usage.Get(usage.OutputTokens); got != openai.DefaultMaxOutputTokens {
		t.Errorf("estimated output = %d, want the default ceiling %d", got, openai.DefaultMaxOutputTokens)
	}
	if !strings.Contains(est.Note, "throttle's assumption") {
		t.Errorf("estimate note %q should disclose that the ceiling is throttle's assumption", est.Note)
	}
}

// A counted estimate is conservative, never exact: OpenAI does not document the count
// as equal to the billed count.
func TestEstimateIsNeverExact(t *testing.T) {
	h := newHarness(t, "1000")

	est, err := h.client.Estimate(context.Background(), request(gpt51, maxOut(2000)))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Quality == usage.QualityExact {
		t.Error("no Responses estimate can be exact: output is unknowable before generation, " +
			"and the input count is not documented as matching the bill")
	}
	if est.Quality != usage.QualityConservative {
		t.Errorf("Quality = %q, want %q with a counter and a caller-set cap",
			est.Quality, usage.QualityConservative)
	}
	if h.counter.calls != 1 {
		t.Errorf("counter calls = %d, want 1", h.counter.calls)
	}
	if got, _ := est.Usage.Get(usage.InputTokens); got != 1000 {
		t.Errorf("estimated input = %d, want the counted 1000", got)
	}
}

// Without a counter the estimate degrades honestly rather than failing.
func TestEstimateWithoutCounterIsHeuristic(t *testing.T) {
	h := newHarness(t, "1000", func(cfg *openai.Config) { cfg.Counter = nil })

	est, err := h.client.Estimate(context.Background(), request(gpt51, maxOut(2000)))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Quality != usage.QualityHeuristic {
		t.Errorf("Quality = %q, want %q", est.Quality, usage.QualityHeuristic)
	}
	if got, _ := est.Usage.Get(usage.InputTokens); got <= 0 {
		t.Errorf("estimated input = %d, want a positive approximation", got)
	}
	if !strings.Contains(est.Note, "content length") {
		t.Errorf("note %q should explain the method used", est.Note)
	}
}

// A failing counter degrades the estimate; it does not fail the request.
func TestCounterFailureDegradesTheEstimate(t *testing.T) {
	h := newHarness(t, "1000")
	h.counter.err = errors.New("the counter is unavailable")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-nocount", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if !res.Settled {
		t.Error("a counting failure must not prevent the request")
	}
	if res.Estimate.Quality != usage.QualityHeuristic {
		t.Errorf("Quality = %q, want %q after a counting failure", res.Estimate.Quality, usage.QualityHeuristic)
	}
	if !strings.Contains(res.Estimate.Note, "failed") {
		t.Errorf("note %q should say the count failed", res.Estimate.Note)
	}
}

// A request continuing a previous response has server-side history the heuristic
// cannot see, and says so instead of reporting a quietly low number.
func TestConversationHistoryIsDisclosed(t *testing.T) {
	h := newHarness(t, "1000", func(cfg *openai.Config) { cfg.Counter = nil })

	in := request(gpt51, maxOut(2000))
	in.PreviousResponseID = param.NewOpt("resp_earlier")

	est, err := h.client.Estimate(context.Background(), in)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Quality != usage.QualityHeuristic {
		t.Errorf("Quality = %q, want %q", est.Quality, usage.QualityHeuristic)
	}
	if !strings.Contains(est.Note, "prepends") {
		t.Errorf("note %q should disclose the unmeasurable server-side history", est.Note)
	}
}

// Child spend is attributed to every ancestor, so a parent's reported spend includes
// its children's. The scopes come from the reservation's legs, which is what makes this
// survive the reservation itself.
func TestChildSpendAttributesToAncestors(t *testing.T) {
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
		ID: "team", ParentID: "org", Allocation: dollars(t, "100"),
		Recurrence: budget.RecurMonthly, AnchorAt: anchor,
	}, engine.ModeEnforce); err != nil {
		t.Fatalf("Register team: %v", err)
	}
	h := buildHarness(t, eng, store, clock)

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-child", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	want := res.Charge.ActualCost
	if got := h.totalsFor(t, "team").Spent; got != want {
		t.Errorf("team Spent = %s, want %s", got, want)
	}
	if got := h.totalsFor(t, "org").Spent; got != want {
		t.Errorf("org Spent = %s, want %s: a parent's spend must include its children's", got, want)
	}

	// The durable record carries the whole chain, so attribution survives the
	// reservation.
	rec := h.record(t, "req-child")
	seen := map[string]bool{}
	for _, s := range rec.Scopes {
		seen[s.BudgetID] = true
	}
	if !seen["team"] || !seen["org"] {
		t.Errorf("recorded scopes = %v, want both team and org", rec.Scopes)
	}
}

// A parent cannot be oversubscribed by concurrent children. Reservations are atomic
// across the chain, and this is where a race would show up as spend above the ceiling.
func TestParentIsNotOversubscribedUnderConcurrency(t *testing.T) {
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

	// The parent affords roughly ten of these requests; the two children together
	// would afford far more if their holds did not both consume the parent.
	if err := eng.Register(context.Background(), budget.Definition{
		ID: "org", Allocation: dollars(t, "0.25"), Recurrence: budget.RecurMonthly, AnchorAt: anchor,
	}, engine.ModeEnforce); err != nil {
		t.Fatalf("Register org: %v", err)
	}
	for _, child := range []string{"team-a", "team-b"} {
		if err := eng.Register(context.Background(), budget.Definition{
			ID: child, ParentID: "org", Allocation: dollars(t, "100"),
			Recurrence: budget.RecurMonthly, AnchorAt: anchor,
		}, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", child, err)
		}
	}
	h := buildHarness(t, eng, store, clock)

	const attempts = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	var allowed int
	var spent int64

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			child := "team-a"
			if i%2 == 1 {
				child = "team-b"
			}
			res, err := h.client.Respond(context.Background(), openai.Request{
				BudgetID:  child,
				RequestID: fmt.Sprintf("req-conc-%d", i),
				Params:    request(gpt51, maxOut(2000)),
			})
			if err != nil {
				return
			}
			mu.Lock()
			allowed++
			spent += int64(res.Charge.ActualCost)
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
		t.Errorf("org Spent = %s, which exceeds its %s allocation: concurrent children oversubscribed the parent",
			got, ceiling)
	}
	// The children's own recorded spend must add up to the parent's, since every child
	// charge consumes the parent too.
	sum := h.totalsFor(t, "team-a").Spent + h.totalsFor(t, "team-b").Spent
	if sum != h.totalsFor(t, "org").Spent {
		t.Errorf("children spent %s but the parent recorded %s", sum, h.totalsFor(t, "org").Spent)
	}
}

// Concurrent requests that all come back on an unpriced tier keep the ancestor chain
// safe: nothing is charged, every hold stays encumbered, and the parent's encumbrance
// is the sum of its children's.
//
// The concurrency case for #30. An unresolved settlement is the path that keeps money
// tied up rather than moving it, so a race here would show as headroom being handed out
// twice for spend that has already happened -- the same failure the atomic-reservation
// invariant exists to prevent, reached through the pricing refusal instead of a charge.
func TestConcurrentUnpricedTierRequestsKeepTheChainSafe(t *testing.T) {
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
	for _, child := range []string{"team-a", "team-b"} {
		if err := eng.Register(context.Background(), budget.Definition{
			ID: child, ParentID: "org", Allocation: dollars(t, "500"),
			Recurrence: budget.RecurMonthly, AnchorAt: anchor,
		}, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", child, err)
		}
	}
	h := buildHarness(t, eng, store, clock)
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_conc_tier", "object": "response", "status": "completed", "model": %q,
		"service_tier": "turbo-2027",
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	const attempts = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	var unresolved, other int

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			child := "team-a"
			if i%2 == 1 {
				child = "team-b"
			}
			res, err := h.client.Respond(context.Background(), openai.Request{
				BudgetID:  child,
				RequestID: fmt.Sprintf("req-conc-tier-%d", i),
				Params:    request(gpt51, maxOut(2000)),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, openai.ErrCostUnresolved) && res != nil && !res.Cost.Known():
				unresolved++
			default:
				other++
			}
		}(i)
	}
	wg.Wait()

	if unresolved != attempts {
		t.Fatalf("%d of %d requests settled as unresolved (%d otherwise), want all of them: "+
			"an unpriced tier is not a race-dependent outcome", unresolved, attempts, other)
	}

	// Nothing charged anywhere, at any depth.
	for _, id := range []string{"org", "team-a", "team-b"} {
		if got := h.totalsFor(t, id).Spent; got != 0 {
			t.Errorf("%s Spent = %s, want 0: no cost was ever known", id, got)
		}
	}
	// Every hold stays encumbered, and the parent carries every child's.
	parent := h.totalsFor(t, "org").Reserved
	if parent == 0 {
		t.Fatal("org Reserved = 0: unresolved costs must keep their holds encumbered at every depth")
	}
	if sum := h.totalsFor(t, "team-a").Reserved + h.totalsFor(t, "team-b").Reserved; sum != parent {
		t.Errorf("children hold %s but the parent holds %s: an encumbrance was lost or double-counted "+
			"across the chain", sum, parent)
	}
	if parent > dollars(t, "1000") {
		t.Errorf("org Reserved = %s, above its allocation: concurrent unresolved requests "+
			"oversubscribed the parent", parent)
	}
}

// apiError builds a real SDK API error, so the error-classification path under test is
// the production one rather than a stand-in.
//
// The body is deliberately given a message that looks like leaked content, since what
// this fixture is for is proving the message does not reach durable storage.
func apiError(t *testing.T, status int, code, kind, message string) error {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("building a request: %v", err)
	}
	body := fmt.Sprintf(`{"error":{"code":%q,"message":%q,"param":null,"type":%q}}`, code, message, kind)
	apiErr := &oai.Error{
		Code:       code,
		Message:    message,
		Type:       kind,
		StatusCode: status,
		Request:    req,
		Response:   &http.Response{StatusCode: status},
	}
	// The SDK populates its raw JSON through unmarshalling, which is also what makes
	// Error() embed the body -- the behaviour the redaction exists to contain.
	if err := apiErr.UnmarshalJSON([]byte(body)); err != nil {
		t.Fatalf("unmarshalling an api error: %v", err)
	}
	apiErr.StatusCode = status
	apiErr.Request = req
	apiErr.Response = &http.Response{StatusCode: status}
	return apiErr
}

// A request naming a model but no identity fields still identifies itself completely
// enough to account for, and names throttle's own provider vocabulary rather than the
// SDK's.
func TestIdentityFields(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-identity", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	id := res.Identity
	if id.AccessProvider != "openai" {
		t.Errorf("AccessProvider = %q, want %q", id.AccessProvider, "openai")
	}
	if id.Publisher != "openai" {
		t.Errorf("Publisher = %q, want %q: OpenAI publishes the models it serves", id.Publisher, "openai")
	}
	if id.Operation != "responses" {
		t.Errorf("Operation = %q, want %q", id.Operation, "responses")
	}
	if id.ProviderModelID != gpt51 {
		t.Errorf("ProviderModelID = %q, want %q", id.ProviderModelID, gpt51)
	}
	if h.client.Name() != "openai" {
		t.Errorf("Name() = %q, want %q", h.client.Name(), "openai")
	}
}

// A request naming a tier the response does not report keeps the requested tier, since
// that is the only tier information available.
func TestRequestedTierIsUsedWhenTheResponseIsSilent(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(mini, maxOut(2000))
	in.ServiceTier = responses.ResponseNewParamsServiceTierFlex
	// A response that reports no service tier at all.
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_silent", "object": "response", "status": "completed", "model": %q,
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
	}`, mini))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-flex", Params: in,
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if res.Identity.ServiceTier != "flex" {
		t.Errorf("ServiceTier = %q, want the requested %q", res.Identity.ServiceTier, "flex")
	}
	// gpt-5-mini flex: $0.125/M input, $1.00/M output.
	if want := dollars(t, "0.000625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (flex rates)", res.Charge.ActualCost, want)
	}
}

// The auto tier is not a priceable tier: it means "let OpenAI decide". Recording it as
// an identity dimension would key pricing on a request preference rather than on what
// ran.
func TestAutoTierIsNotRecordedAsATier(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(gpt51, maxOut(2000))
	in.ServiceTier = responses.ResponseNewParamsServiceTierAuto

	est, err := h.client.Estimate(context.Background(), in)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Identity.ServiceTier != "" {
		t.Errorf("ServiceTier = %q, want empty: auto names no tier", est.Identity.ServiceTier)
	}
	// It still prices, at the tier-less rate.
	if !est.Cost.Known() {
		t.Errorf("an auto-tier request must still price: %s", est.Cost.Reason)
	}
}

var _ = ledger.Scope{}
var _ = shared.ResponsesModel("")
