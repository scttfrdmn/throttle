package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"

	"github.com/scttfrdmn/throttle/activity"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/reconcile"
	"github.com/scttfrdmn/throttle/usage"
)

// Chat Completions content that must never reach durable storage.
//
// A separate set of sentinels from the Responses ones, so a failure names which API family
// leaked. The shapes are Chat Completions-specific: a refusal string, a function-call
// argument blob, an audio transcript, and a logprob token -- none of which the Responses
// object has, and each of which is a place a leak could arrive that the existing privacy
// test would never look.
const (
	chatPromptText     = "SECRET-CHAT-PROMPT-my medical history in full"
	chatSystemText     = "SECRET-CHAT-SYSTEM-you are a helpful assistant named Bruce"
	chatOutputText     = "SECRET-CHAT-OUTPUT-here is the summary you asked for"
	chatRefusalText    = "SECRET-CHAT-REFUSAL-I cannot help with that"
	chatToolArgsText   = `SECRET-CHAT-TOOL-ARGS-{"ssn":"078-05-1120"}`
	chatToolResultText = "SECRET-CHAT-TOOL-RESULT-the balance is $4,200"
	chatTranscriptText = "SECRET-CHAT-TRANSCRIPT-spoken words of the answer"
	chatLogprobText    = "SECRET-CHAT-LOGPROB-token"
	chatPredictionText = "SECRET-CHAT-PREDICTION-the draft document being revised"
	chatAudioData      = "SECRET-CHAT-AUDIO-BASE64-PAYLOAD"
)

// The durable record of a Chat Completions request carries accounting facts and no
// content.
//
// Serialized whole and searched, rather than asserted field by field, because a leak
// arrives in the field nobody thought to check. The completion here carries everything a
// ChatCompletion can hold that throttle must not keep: assistant text, a refusal, tool
// call arguments, an audio payload and its transcript, logprobs, annotations, and a
// moderation verdict.
func TestChatRecordCarriesNoContent(t *testing.T) {
	h := newChatHarness(t, "1000")

	// A request carrying every kind of content: a system instruction, a prompt, a tool
	// schema with a description, a tool result fed back in, and a predicted output.
	in := oai.ChatCompletionNewParams{
		Model:               gpt51,
		MaxCompletionTokens: param.NewOpt(int64(2000)),
		Messages: []oai.ChatCompletionMessageParamUnion{
			oai.SystemMessage(chatSystemText),
			oai.UserMessage(chatPromptText),
			oai.ToolMessage(chatToolResultText, "call_1"),
		},
		Tools: []oai.ChatCompletionToolUnionParam{
			{OfFunction: &oai.ChatCompletionFunctionToolParam{
				Function: oai.FunctionDefinitionParam{
					Name:        "lookup_balance",
					Description: param.NewOpt(chatToolArgsText),
					Parameters:  map[string]any{"type": "object"},
				},
			}},
		},
		Prediction: oai.ChatCompletionPredictionContentParam{
			Content: oai.ChatCompletionPredictionContentContentUnionParam{
				OfString: param.NewOpt(chatPredictionText),
			},
		},
		Metadata: map[string]string{"caller_note": chatPromptText},
	}

	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_privacy", "object": "chat.completion", "created": 1786000000, "model": %q,
		"system_fingerprint": "fp_abc123",
		"moderation": {"flagged": false, "categories": {"violence": false}},
		"choices": [{
			"index": 0, "finish_reason": "stop",
			"message": {
				"role": "assistant",
				"content": %q,
				"refusal": %q,
				"annotations": [{"type": "url_citation", "url_citation": {
					"url": "https://example.invalid/secret-page", "title": %q,
					"start_index": 0, "end_index": 10}}],
				"audio": {"id": "audio_1", "data": %q, "expires_at": 1786003600, "transcript": %q},
				"tool_calls": [{"id": "call_2", "type": "function",
					"function": {"name": "lookup_balance", "arguments": %q}}]
			},
			"logprobs": {"content": [{"token": %q, "logprob": -0.5, "bytes": [83]}]}
		}],
		"usage": {
			"prompt_tokens": 1000,
			"prompt_tokens_details": {"cached_tokens": 200},
			"completion_tokens": 500,
			"completion_tokens_details": {"reasoning_tokens": 100,
				"accepted_prediction_tokens": 50, "rejected_prediction_tokens": 120},
			"total_tokens": 1500
		}
	}`, gpt51, chatOutputText, chatRefusalText, chatOutputText, chatAudioData,
		chatTranscriptText, chatToolArgsText, chatLogprobText))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID:  "team",
		RequestID: "chat-privacy",
		Params:    in,
		Metadata:  map[string]string{"workload": "nightly-report"},
	})
	// Audio tokens were not reported, so this settles normally. If it did not, the test
	// would prove nothing about a completed record.
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !res.Settled {
		t.Fatal("the request should have settled, or this test proves nothing about what was stored")
	}

	rec := h.record(t, "chat-privacy")
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	for _, secret := range []string{
		chatPromptText, chatSystemText, chatOutputText, chatRefusalText,
		chatToolArgsText, chatToolResultText, chatTranscriptText, chatLogprobText,
		chatPredictionText, chatAudioData,
		// The Responses sentinels too, so a shared helper cannot leak one family's
		// content through the other's path.
		promptText, outputText, reasoningText, apiKeyText,
	} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("the durable record of a Chat Completions request contains content that must "+
				"never be persisted: %q", secret)
		}
	}
	// Nor the provider's own opaque identifiers for content it is holding.
	for _, forbidden := range []string{"audio_1", "url_citation", "example.invalid", "fp_abc123"} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("the record contains %q, which is a handle on content rather than an "+
				"accounting fact", forbidden)
		}
	}

	// And the accounting facts that must be there.
	if rec.Identity.Operation != "chat-completions" {
		t.Errorf("recorded operation = %q, want chat-completions", rec.Identity.Operation)
	}
	if rec.Identity.ProviderModelID != gpt51 {
		t.Errorf("recorded model = %q, want %q", rec.Identity.ProviderModelID, gpt51)
	}
	if rec.Identity.AccessProvider != "openai" || rec.Identity.Publisher != "openai" {
		t.Errorf("recorded provider/publisher = %q/%q, want openai/openai",
			rec.Identity.AccessProvider, rec.Identity.Publisher)
	}
	// The counts survive, because a count is accounting metadata and the text is not.
	for _, want := range []struct {
		d usage.Dimension
		n int64
	}{
		{usage.InputTokens, 800},
		{usage.CacheReadTokens, 200},
		{usage.OutputTokens, 400},
		{usage.ReasoningTokens, 100},
	} {
		if got, _ := rec.ActualUsage.Get(want.d); got != want.n {
			t.Errorf("recorded %s = %d, want %d", want.d, got, want.n)
		}
	}
	if rec.ActualCost.Amount == 0 {
		t.Error("the recorded cost should be the priced actual")
	}
	if rec.Metadata["workload"] != "nightly-report" {
		t.Errorf("caller metadata = %v, want the workload attribution preserved", rec.Metadata)
	}
	// The caller's own SDK-level Metadata field is OpenAI's to store, not throttle's: it
	// travels with the request and does not enter throttle's record.
	if strings.Contains(string(blob), "caller_note") {
		t.Error("the request's OpenAI-side metadata reached throttle's record; only throttle's " +
			"own Metadata argument belongs there")
	}
}

// A tool result being fed back is frequently the largest message in an agent loop, and it
// is content. Its size reaches the estimate; nothing else about it reaches anything.
func TestChatToolResultSizeIsMeasuredAndItsContentIsNot(t *testing.T) {
	h := newChatHarness(t, "1000")

	small := chatRequest(gpt51, maxOut(500))
	large := chatRequest(gpt51, maxOut(500))
	large.Messages = append(large.Messages,
		oai.ToolMessage(chatToolResultText+strings.Repeat(" padding", 500), "call_1"))

	estSmall, err := h.client.EstimateChat(context.Background(), small)
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	estLarge, err := h.client.EstimateChat(context.Background(), large)
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	inSmall, _ := estSmall.Usage.Get(usage.InputTokens)
	inLarge, _ := estLarge.Usage.Get(usage.InputTokens)
	if inLarge <= inSmall {
		t.Errorf("input estimate with a large tool result = %d, without = %d: a tool result is "+
			"billed as input and its size must count", inLarge, inSmall)
	}

	h.chat.out = completion(t, gpt51, 5000, 100)
	if _, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-toolresult", Params: large,
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	blob, err := json.Marshal(h.record(t, "chat-toolresult"))
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	if strings.Contains(string(blob), chatToolResultText) {
		t.Error("the tool result's content reached durable storage; only its size should have")
	}
}

// A provider error quoting the prompt back is reduced to its classification before being
// persisted, on this path as on the Responses one.
func TestChatProviderErrorPayloadIsNotPersistedWholesale(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = nil
	h.chat.err = apiError(t, http.StatusBadRequest, "invalid_prompt", "invalid_request_error",
		"Your prompt was rejected: "+chatPromptText)

	if _, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-errpayload", Params: chatRequest(gpt51, maxOut(2000)),
	}); !errors.Is(err, openai.ErrProvider) {
		t.Fatalf("Complete error = %v, want ErrProvider", err)
	}

	rec := h.record(t, "chat-errpayload")
	if strings.Contains(rec.Error, chatPromptText) {
		t.Errorf("the provider's error message put prompt content into the record: %q", rec.Error)
	}
	if !strings.Contains(rec.Error, "400") || !strings.Contains(rec.Error, "invalid_prompt") {
		t.Errorf("recorded error %q should keep the status and classification an operator acts on",
			rec.Error)
	}
}

// The served model is recorded as metadata when it differs from the one requested,
// because an alias resolving to a dated snapshot is a fact about what was billed.
//
// As metadata rather than as a new top-level field: the neutral schema already has a place
// for provider-specific facts, and "which snapshot served this" is one.
func TestChatServedModelIsRecordedSeparately(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = completion(t, "gpt-5.1-2026-04-14", 1000, 500)

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-served", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.ServedModelID != "gpt-5.1-2026-04-14" {
		t.Errorf("ServedModelID = %q, want the dated snapshot the provider reported", res.ServedModelID)
	}
	// The requested ID stays the identity, because that is what pricing keys on and what
	// the caller asked for.
	if res.Identity.ProviderModelID != gpt51 {
		t.Errorf("ProviderModelID = %q, want the requested %q: the two are independent facts",
			res.Identity.ProviderModelID, gpt51)
	}
	rec := h.record(t, "chat-served")
	if got := rec.Metadata["openai.served_model"]; got != "gpt-5.1-2026-04-14" {
		t.Errorf("metadata[openai.served_model] = %q, want the snapshot", got)
	}
	if rec.Identity.ProviderModelID != gpt51 {
		t.Errorf("recorded model = %q, want the requested one", rec.Identity.ProviderModelID)
	}

	// A completion echoing back the same model records nothing, because there is no second
	// fact to record.
	h.chat.out = completion(t, gpt51, 100, 50)
	res, err = h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-served-same", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.ServedModelID != "" {
		t.Errorf("ServedModelID = %q, want empty when the provider served what was asked for",
			res.ServedModelID)
	}
	if _, ok := h.record(t, "chat-served-same").Metadata["openai.served_model"]; ok {
		t.Error("served_model metadata was written for a request served on the requested model")
	}
}

// A stranded Chat Completions request is reconciled from the normalized durable record,
// with no provider-specific logic, no SDK types in the reconciler, and no second call to
// OpenAI.
//
// This is the crash-repair criterion. The reconciler has never heard of OpenAI and cannot
// tell which API family stranded the record -- it repairs it from the same neutral facts it
// uses for any other provider, which is what makes "no second accounting engine" true of
// the recovery path too rather than only of the happy path.
func TestStrandedChatCompletionReconcilesFromDurableFacts(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = completion(t, gpt51, 1000, 500)
	h.chat.block = make(chan struct{})

	// Abandon the call mid-flight, which leaves exactly the state a crash leaves: a
	// reservation standing, an activity record saying outstanding, and a frozen quote in it.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		//nolint:errcheck // the error is the point: this call is abandoned.
		h.client.Complete(ctx, openai.ChatRequest{
			BudgetID: "team", RequestID: "chat-stranded", Params: chatRequest(gpt51, maxOut(2000)),
		})
	}()
	waitFor(t, func() bool { return h.chat.callCount() == 1 })
	cancel()
	<-done
	close(h.chat.block)

	before := h.record(t, "chat-stranded")
	if before.Status != activity.StatusOutstanding {
		t.Fatalf("status = %q, want %q: this test needs a stranded record to repair",
			before.Status, activity.StatusOutstanding)
	}
	if before.Identity.Operation != "chat-completions" {
		t.Errorf("operation = %q, want chat-completions: the record must say which API stranded",
			before.Identity.Operation)
	}

	// Built with a ledger and an activity store and nothing else. No OpenAI client, no
	// catalog, no adapter.
	rec, err := reconcile.New(reconcile.Config{
		Ledger:   h.ledger,
		Activity: h.activity,
		Clock:    h.clock,
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}

	out, err := rec.Reconcile(context.Background(), "chat-stranded")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	switch out.Class {
	case reconcile.ClassUnresolved, reconcile.ClassRepaired:
		// Either is a legitimate resolution of a genuinely ambiguous outcome.
	default:
		t.Errorf("Class = %q, want unresolved or repaired: a stranded Chat Completions request "+
			"must be classifiable from normalized durable facts alone", out.Class)
	}
	if h.chat.callCount() != 1 {
		t.Errorf("the provider was called %d times: reconciliation must not re-call OpenAI",
			h.chat.callCount())
	}
	after := h.record(t, "chat-stranded")
	if !after.Quote.Valid() {
		t.Error("the captured quote must survive in the durable record, or nothing can re-price it")
	}
}

// ObserveChat normalizes a completion obtained outside the governed path and declines to
// invent a price, for a caller reconciling by hand.
func TestObserveChatNormalizesWithoutPricing(t *testing.T) {
	h := newChatHarness(t, "1000")

	// Counts chosen so every dimension lands on a whole microdollar, keeping this a test of
	// the decomposition rather than of rounding.
	out := complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_obs", "object": "chat.completion", "created": 1786000000, "model": %q,
		"service_tier": "flex",
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {"prompt_tokens": 10000, "prompt_tokens_details": {"cached_tokens": 2000},
			"completion_tokens": 5000, "total_tokens": 15000}
	}`, mini))

	actual, err := h.client.ObserveChat(context.Background(), out)
	if err != nil {
		t.Fatalf("ObserveChat: %v", err)
	}
	if got, _ := actual.Usage.Get(usage.InputTokens); got != 8000 {
		t.Errorf("InputTokens = %d, want 8000", got)
	}
	if got, _ := actual.Usage.Get(usage.CacheReadTokens); got != 2000 {
		t.Errorf("CacheReadTokens = %d, want 2000", got)
	}
	if actual.Identity.ServiceTier != "flex" {
		t.Errorf("ServiceTier = %q, want flex", actual.Identity.ServiceTier)
	}
	if actual.Identity.Operation != "chat-completions" {
		t.Errorf("Operation = %q, want chat-completions", actual.Identity.Operation)
	}
	if actual.Cost.Known() {
		t.Error("ObserveChat reports usage only, so the cost must be explicitly unknown")
	}

	// Pricing is the separate step and reaches the same numbers the governed path does --
	// and reaches them through the shared Price method, not a Chat-specific one.
	m, err := h.client.Price(context.Background(), actual, now)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	// gpt-5-mini flex: 8000 fresh at $0.125/M, 2000 cached at $0.0125/M, 5000 output at
	// $1.00/M.
	if want := dollars(t, "0.006025"); m != want {
		t.Errorf("Price = %s, want %s", m, want)
	}
}

// A Chat Completions request runs with no OpenAI credentials present, and the adapter
// still has nowhere to put one.
//
// The structural half of this is already asserted over openai.Config; what is new here is
// that adding a second API family did not add a credential-shaped seam, and that the chat
// path does not read the environment either.
func TestChatSuiteRequiresNoCredentials(t *testing.T) {
	for _, key := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "OPENAI_ORG_ID"} {
		t.Setenv(key, "")
	}
	h := newChatHarness(t, "1000")
	if _, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-nocreds", Params: chatRequest(gpt51, maxOut(2000)),
	}); err != nil {
		t.Fatalf("Complete with no credentials: %v", err)
	}

	t.Setenv("OPENAI_API_KEY", apiKeyText)
	if _, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-creds", Params: chatRequest(gpt51, maxOut(2000)),
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	blob, err := json.Marshal(h.record(t, "chat-creds"))
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	if strings.Contains(string(blob), apiKeyText) || strings.Contains(string(blob), "sk-") {
		t.Error("something key-shaped reached the durable record of a Chat Completions request")
	}
}
