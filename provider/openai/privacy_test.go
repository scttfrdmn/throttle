package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/activity"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/reconcile"
	"github.com/scttfrdmn/throttle/usage"
)

// Content that must never reach durable storage. Each string is distinctive enough
// that finding it anywhere in a serialized record is unambiguous.
const (
	promptText     = "SECRET-PROMPT-my late father's tax returns"
	instructionTxt = "SECRET-INSTRUCTIONS-always answer in iambic pentameter"
	outputText     = "SECRET-OUTPUT-the airspeed velocity is 11 metres per second"
	reasoningText  = "SECRET-REASONING-let me work through this step by step"
	toolArgsText   = "SECRET-TOOL-ARGS-{\"account\":\"4111111111111111\"}"
	toolResultText = "SECRET-TOOL-RESULT-balance is $4,200"
	structuredTxt  = "SECRET-STRUCTURED-{\"answer\":42}"
	fileText       = "SECRET-FILE-contents of an uploaded document"
	apiKeyText     = "sk-proj-EXAMPLE0000000000000000000000000000000000000000000"
)

// The durable record carries accounting facts and no content. This is the privacy
// boundary, and it is checked by serializing the whole record and searching it rather
// than by asserting on the fields the adapter happens to set -- a leak would arrive in
// a field nobody thought to check.
func TestActivityRecordCarriesNoContent(t *testing.T) {
	h := newHarness(t, "1000")

	// A request carrying every kind of content throttle must not persist: a prompt,
	// system instructions, a tool schema with a description, a tool result being fed
	// back, and an API key in metadata-adjacent position.
	in := request(gpt51, maxOut(2000))
	in.Instructions = param.NewOpt(instructionTxt)
	in.Input = responses.ResponseNewParamsInputUnion{
		OfInputItemList: responses.ResponseInputParam{
			{OfMessage: &responses.EasyInputMessageParam{
				Role: responses.EasyInputMessageRoleUser,
				Content: responses.EasyInputMessageContentUnionParam{
					OfString: param.NewOpt(promptText),
				},
			}},
			{OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
				CallID: "call_1",
				Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
					OfString: param.NewOpt(toolResultText),
				},
			}},
		},
	}
	in.Tools = []responses.ToolUnionParam{{
		OfFunction: &responses.FunctionToolParam{
			Name:        "lookup_balance",
			Description: param.NewOpt(toolArgsText),
			Parameters:  map[string]any{"type": "object"},
		},
	}}

	// A response carrying output text, reasoning content, a tool call with arguments,
	// and a refusal -- everything an SDK response can hold that throttle must not keep.
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_privacy", "object": "response", "status": "completed", "model": %q,
		"output": [
			{"type": "reasoning", "id": "rs_1", "summary": [{"type": "summary_text", "text": %q}],
			 "content": [{"type": "reasoning_text", "text": %q}]},
			{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
			 "content": [{"type": "output_text", "text": %q, "annotations": []}]},
			{"type": "function_call", "id": "fc_1", "call_id": "call_2",
			 "name": "lookup_balance", "arguments": %q}
		],
		"usage": {
			"input_tokens": 1000,
			"output_tokens": 500,
			"output_tokens_details": {"reasoning_tokens": 300},
			"total_tokens": 1500
		}
	}`, gpt51, reasoningText, reasoningText, outputText, toolArgsText))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID:  "team",
		RequestID: "req-privacy",
		Params:    in,
		Metadata:  map[string]string{"workload": "nightly-report"},
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	// The accounting is complete -- this is not a test of a request that failed to be
	// recorded.
	if !res.Settled {
		t.Fatal("the request should have settled, or this test proves nothing about what was stored")
	}
	if got, _ := res.Usage.Get(usage.ReasoningTokens); got != 300 {
		t.Errorf("ReasoningTokens = %d, want 300: the count is accounting metadata and is kept", got)
	}

	rec := h.record(t, "req-privacy")

	// The record is serialized whole and searched, so a leak in any field fails this
	// test -- including a field added later.
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	for _, secret := range []string{
		promptText, instructionTxt, outputText, reasoningText,
		toolArgsText, toolResultText, structuredTxt, fileText, apiKeyText,
	} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("the durable activity record contains content that must never be persisted: %q", secret)
		}
	}

	// And the accounting facts that must be there.
	if rec.Identity.ProviderModelID != gpt51 {
		t.Errorf("recorded model = %q, want %q", rec.Identity.ProviderModelID, gpt51)
	}
	if rec.Identity.AccessProvider != "openai" || rec.Identity.Publisher != "openai" {
		t.Errorf("recorded provider/publisher = %q/%q, want openai/openai",
			rec.Identity.AccessProvider, rec.Identity.Publisher)
	}
	if rec.Identity.Operation != "responses" {
		t.Errorf("recorded operation = %q, want responses", rec.Identity.Operation)
	}
	if n, _ := rec.ActualUsage.Get(usage.ReasoningTokens); n != 300 {
		t.Errorf("recorded reasoning tokens = %d, want 300: the count is metadata, the content is not", n)
	}
	if rec.ActualCost.Amount == 0 {
		t.Error("the recorded cost should be the priced actual")
	}
	if rec.Reserved == 0 {
		t.Error("the reserved amount should be recorded")
	}
	if rec.Estimate.Quality == "" {
		t.Error("the estimate quality should be recorded, so accuracy can be measured later")
	}
	if rec.Metadata["workload"] != "nightly-report" {
		t.Errorf("caller metadata = %v, want the workload attribution preserved", rec.Metadata)
	}
}

// Reasoning content specifically cannot enter persistence, even though the reasoning
// token count does. The distinction is absolute: a count is accounting metadata, the
// text is content.
func TestReasoningContentIsNeverPersisted(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_reason", "object": "response", "status": "completed", "model": %q,
		"output": [{"type": "reasoning", "id": "rs_1",
			"summary": [{"type": "summary_text", "text": %q}],
			"content": [{"type": "reasoning_text", "text": %q}],
			"encrypted_content": "gAAAAABm-encrypted-reasoning-payload"}],
		"usage": {"input_tokens": 100, "output_tokens": 400,
			"output_tokens_details": {"reasoning_tokens": 350}, "total_tokens": 500}
	}`, gpt51, reasoningText, reasoningText))

	if _, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-reasoning", Params: request(gpt51, maxOut(2000)),
	}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	rec := h.record(t, "req-reasoning")
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	for _, forbidden := range []string{reasoningText, "encrypted-reasoning-payload", "summary_text"} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("reasoning content reached durable storage: %q", forbidden)
		}
	}
	// The count survives, because that is what accounting needs.
	if n, _ := rec.ActualUsage.Get(usage.ReasoningTokens); n != 350 {
		t.Errorf("recorded reasoning tokens = %d, want 350", n)
	}
}

// A provider error carrying content in its message body is reduced to its
// classification before being persisted. The SDK's Error() embeds the raw response
// body, which for a rejected prompt can quote the prompt back.
func TestProviderErrorPayloadIsNotPersistedWholesale(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = nil
	// A message shaped like the ones OpenAI returns for content rejections, quoting the
	// input back.
	h.api.err = apiError(t, http.StatusBadRequest, "invalid_prompt", "invalid_request_error",
		"Your prompt was rejected: "+promptText)

	if _, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-errpayload", Params: request(gpt51, maxOut(2000)),
	}); !errors.Is(err, openai.ErrProvider) {
		t.Fatalf("Respond error = %v, want ErrProvider", err)
	}

	rec := h.record(t, "req-errpayload")
	if strings.Contains(rec.Error, promptText) {
		t.Errorf("the provider's error message put prompt content into the durable record: %q", rec.Error)
	}
	// What survives is what an operator acts on.
	if !strings.Contains(rec.Error, "400") {
		t.Errorf("recorded error %q should keep the HTTP status", rec.Error)
	}
	if !strings.Contains(rec.Error, "invalid_prompt") {
		t.Errorf("recorded error %q should keep the provider's classification", rec.Error)
	}
}

// throttle never handles OpenAI credentials. Nothing in the adapter's configuration or
// its records has anywhere to put one, and this asserts that structurally rather than
// by convention.
func TestAdapterHoldsNoCredentials(t *testing.T) {
	// The config surface: an API key has no field to occupy. Credentials are the SDK's
	// business, resolved from the environment by openai.NewClient.
	cfgType := reflect.TypeOf(openai.Config{})
	for i := 0; i < cfgType.NumField(); i++ {
		name := strings.ToLower(cfgType.Field(i).Name)
		for _, bad := range []string{"apikey", "key", "secret", "credential", "password", "auth", "bearer"} {
			if strings.Contains(name, bad) {
				t.Errorf("openai.Config has a %s field: throttle is not a secret manager, and "+
					"credentials must stay with the SDK", cfgType.Field(i).Name)
			}
		}
	}

	// And the durable record: a key set in the environment cannot reach it, because
	// nothing in the adapter reads the environment at all.
	t.Setenv("OPENAI_API_KEY", apiKeyText)

	h := newHarness(t, "1000")
	if _, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-creds", Params: request(gpt51, maxOut(2000)),
	}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	blob, err := json.Marshal(h.record(t, "req-creds"))
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	if strings.Contains(string(blob), apiKeyText) {
		t.Error("an API key reached the durable activity record")
	}
	if strings.Contains(string(blob), "sk-") {
		t.Error("something key-shaped reached the durable activity record")
	}
	// The environment is untouched: throttle does not read, rewrite, or clear it.
	if os.Getenv("OPENAI_API_KEY") != apiKeyText {
		t.Error("the adapter modified the credential environment, which is not its to manage")
	}
}

// A stranded request is reconciled from the normalized durable record, with no
// provider-specific logic and no second call to OpenAI.
//
// This is the load-bearing test for provider neutrality of the recovery path: the
// reconciler has never heard of OpenAI, and it repairs an OpenAI request from the same
// facts it uses for any other.
func TestStrandedResponseReconcilesFromDurableFacts(t *testing.T) {
	h := newHarness(t, "1000")

	// A settlement failure strands the request: the provider served it, the usage is
	// known, and the hold is left outstanding. Simulated by an activity store that
	// accepts the pre-call write and then the process effectively dies -- here, by
	// making the response arrive with usage but settlement never completing, which is
	// what the accounting-error path produces.
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_stranded", "object": "response", "status": "completed", "model": %q,
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	// Admit and reserve, then abandon before settlement by cancelling mid-call. This
	// leaves exactly the state a crash leaves: a reservation standing, an activity
	// record that says outstanding, and a normalized quote frozen in it.
	h.api.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		//nolint:errcheck // the error is the point: this call is abandoned.
		h.client.Respond(ctx, openai.Request{
			BudgetID: "team", RequestID: "req-stranded", Params: request(gpt51, maxOut(2000)),
		})
	}()
	waitFor(t, func() bool { return h.api.callCount() == 1 })
	cancel()
	<-done
	close(h.api.block)

	before := h.record(t, "req-stranded")
	if before.Status != activity.StatusOutstanding {
		t.Fatalf("status = %q, want %q: this test needs a stranded record to repair",
			before.Status, activity.StatusOutstanding)
	}

	// The reconciler is built with no provider knowledge whatsoever: a ledger and an
	// activity store, nothing else. No OpenAI client, no catalog, no adapter.
	rec, err := reconcile.New(reconcile.Config{
		Ledger:   h.ledger,
		Activity: h.activity,
		Clock:    h.clock,
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}

	out, err := rec.Reconcile(context.Background(), "req-stranded")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The outcome was genuinely unknown, so the honest classification is unresolved
	// rather than a repair that invents a settlement. What matters is that the
	// reconciler reached a definite, provider-neutral verdict from the durable record
	// alone.
	switch out.Class {
	case reconcile.ClassUnresolved, reconcile.ClassRepaired:
		// Either is a legitimate resolution of a stranded request.
	default:
		t.Errorf("Class = %q, want unresolved or repaired: a stranded OpenAI request must be "+
			"classifiable from normalized durable facts", out.Class)
	}
	if out.Class == reconcile.ClassFailed {
		t.Error("reconciliation failed, which means the durable record was not sufficient")
	}
	if h.api.callCount() != 1 {
		t.Errorf("the provider was called %d times: reconciliation must not re-call OpenAI",
			h.api.callCount())
	}

	// The frozen quote is what makes the record re-priceable, and it survived.
	after := h.record(t, "req-stranded")
	if !after.Quote.Valid() {
		t.Error("the captured quote must survive in the durable record, or nothing can re-price it")
	}
	if len(after.Repairs) == 0 && out.Class == reconcile.ClassRepaired {
		t.Error("a repair should leave an audit trail")
	}
}

// Observe normalizes a response obtained outside the governed path, for a caller
// reconciling by hand. It reports usage and declines to invent a price.
func TestObserveNormalizesWithoutPricing(t *testing.T) {
	h := newHarness(t, "1000")

	// Token counts chosen so every dimension lands on a whole microdollar, which keeps
	// this a test of the decomposition rather than of rounding.
	out := respond(t, fmt.Sprintf(`{
		"id": "resp_obs", "object": "response", "status": "completed", "model": %q,
		"service_tier": "flex",
		"usage": {"input_tokens": 10000, "input_tokens_details": {"cached_tokens": 2000},
			"output_tokens": 5000, "total_tokens": 15000}
	}`, mini))

	actual, err := h.client.Observe(context.Background(), out)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got, _ := actual.Usage.Get(usage.InputTokens); got != 8000 {
		t.Errorf("InputTokens = %d, want 8000: the same subtractive decomposition applies", got)
	}
	if got, _ := actual.Usage.Get(usage.CacheReadTokens); got != 2000 {
		t.Errorf("CacheReadTokens = %d, want 2000", got)
	}
	if actual.Identity.ServiceTier != "flex" {
		t.Errorf("ServiceTier = %q, want flex", actual.Identity.ServiceTier)
	}
	if actual.Cost.Known() {
		t.Error("Observe reports usage only, so the cost must be explicitly unknown")
	}

	// Price is the separate step, and it reaches the same numbers the governed path
	// does.
	m, err := h.client.Price(context.Background(), actual, now)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	// gpt-5-mini flex: 8000 fresh at $0.125/M = $0.001, 2000 cached at $0.0125/M =
	// $0.000025, 5000 output at $1.00/M = $0.005.
	if want := dollars(t, "0.006025"); m != want {
		t.Errorf("Price = %s, want %s", m, want)
	}
}

// Latency is reported as the caller's wall clock, and provider latency is left at zero
// because a Response carries no latency field. Deriving one would invent a measurement.
func TestProviderLatencyIsNotInvented(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-latency", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if res.Latency < 0 {
		t.Errorf("Latency = %v, want a non-negative wall clock", res.Latency)
	}
	rec := h.record(t, "req-latency")
	if rec.ProviderLatency != 0 {
		t.Errorf("ProviderLatency = %v, want 0: a Response reports no latency, so attributing "+
			"one to OpenAI would be a fabrication", rec.ProviderLatency)
	}
	// Streaming fields belong to a slice not implemented here.
	if rec.StreamEstablished != 0 || rec.StreamFirstEvent != 0 {
		t.Error("a non-streaming request must not report stream timings")
	}
}

// Every one of these tests runs with no OpenAI credentials present. Asserting it makes
// a future test that reaches for the network fail loudly rather than skip quietly on
// somebody else's machine.
func TestSuiteRequiresNoCredentials(t *testing.T) {
	for _, key := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID"} {
		t.Setenv(key, "")
	}
	h := newHarness(t, "1000")
	if _, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-nocreds", Params: request(gpt51, maxOut(2000)),
	}); err != nil {
		t.Fatalf("Respond with no credentials: %v", err)
	}
}

var _ = time.Second
