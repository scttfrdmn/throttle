package anthropic_test

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

	anth "github.com/anthropics/anthropic-sdk-go"

	"github.com/scttfrdmn/throttle/activity"
	anthropic "github.com/scttfrdmn/throttle/provider/anthropic"
	"github.com/scttfrdmn/throttle/reconcile"
	"github.com/scttfrdmn/throttle/usage"
)

// Everything a Messages request can carry that must never reach durable storage.
//
// Distinct sentinels rather than one, because a leak arrives in the field nobody thought
// to check and the point of a whole-record search is to name which one. The list is
// Anthropic-shaped: a system block, a thinking block with its signature, tool input and
// tool result, a document's title and its plaintext body, a search result's source, an
// image payload, and a provider error message that quotes the prompt back.
const (
	userText       = "SENTINEL-USER-my full medical history and my mother's maiden name"
	systemText     = "SENTINEL-SYSTEM-you are a helpful assistant named Bruce"
	assistantText  = "SENTINEL-ASSISTANT-here is the summary you asked for"
	thinkingText   = "SENTINEL-THINKING-the user is probably trying to"
	signatureText  = "SENTINEL-SIGNATURE-EiAxYmM5"
	redactedText   = "SENTINEL-REDACTED-OPAQUE-BLOB"
	toolInputText  = "SENTINEL-TOOL-INPUT-078-05-1120"
	toolResultText = "SENTINEL-TOOL-RESULT-the balance is $4,200"
	docTitleText   = "SENTINEL-DOC-TITLE-Q3 acquisition memo"
	docContextText = "SENTINEL-DOC-CONTEXT-confidential draft"
	docBodyText    = "SENTINEL-DOC-BODY-the terms of the deal are"
	searchSrcText  = "SENTINEL-SEARCH-SOURCE-https://internal.invalid/deal"
	searchTitle    = "SENTINEL-SEARCH-TITLE-internal wiki page"
	imageDataText  = "SENTINEL-IMAGE-BASE64-PAYLOAD"
	userIDText     = "SENTINEL-USER-ID-a1b2c3d4"
	apiKeyText     = "sk-ant-SENTINEL-API-KEY-do-not-store"
)

// contentRequest builds a request carrying every kind of content this API accepts.
//
// One request rather than one per block type, because the assertion is about the record as
// a whole: a leak through any block reaches the same place, and a single settled record
// searched for every sentinel at once is a stronger claim than a dozen narrower ones.
func contentRequest() anth.MessageNewParams {
	toolResult := anth.ToolResultBlockParam{ToolUseID: "toolu_1"}
	toolResult.Content = []anth.ToolResultBlockParamContentUnion{
		{OfText: &anth.TextBlockParam{Text: toolResultText}},
	}

	doc := anth.DocumentBlockParam{
		Source: anth.DocumentBlockParamSourceUnion{
			OfText: &anth.PlainTextSourceParam{Data: docBodyText},
		},
	}
	doc.Title = anth.String(docTitleText)
	doc.Context = anth.String(docContextText)

	in := anth.MessageNewParams{
		Model:     anth.Model(opus5),
		MaxTokens: 2000,
		System:    []anth.TextBlockParam{{Text: systemText}},
		Messages: []anth.MessageParam{{
			Role: anth.MessageParamRoleUser,
			Content: []anth.ContentBlockParamUnion{
				{OfText: &anth.TextBlockParam{Text: userText}},
				{OfThinking: &anth.ThinkingBlockParam{
					Thinking: thinkingText, Signature: signatureText,
				}},
				{OfRedactedThinking: &anth.RedactedThinkingBlockParam{Data: redactedText}},
				{OfToolUse: &anth.ToolUseBlockParam{
					ID: "toolu_1", Name: "lookup_balance",
					Input: map[string]any{"ssn": toolInputText},
				}},
				{OfToolResult: &toolResult},
				{OfDocument: &doc},
				{OfSearchResult: &anth.SearchResultBlockParam{
					Source: searchSrcText, Title: searchTitle,
					Content: []anth.TextBlockParam{{Text: assistantText}},
				}},
				{OfImage: &anth.ImageBlockParam{
					Source: anth.ImageBlockParamSourceUnion{
						OfBase64: &anth.Base64ImageSourceParam{
							Data:      imageDataText,
							MediaType: anth.Base64ImageSourceMediaTypeImagePNG,
						},
					},
				}},
			},
		}},
	}
	// Anthropic's own metadata field, which travels with the request and is Anthropic's
	// to store. It must not enter throttle's record either.
	in.Metadata = anth.MetadataParam{UserID: anth.String(userIDText)}
	return in
}

// contentReply is a response carrying everything a Message can hold that throttle must
// not keep: assistant text, a thinking block and its signature, a tool-use block with
// arguments, and a server-tool result.
func contentReply(t *testing.T) *anth.Message {
	t.Helper()
	return reply(t, fmt.Sprintf(`{
		"id": "msg_privacy", "type": "message", "role": "assistant", "model": %q,
		"stop_reason": "end_turn", "stop_sequence": %q,
		"content": [
			{"type": "text", "text": %q},
			{"type": "thinking", "thinking": %q, "signature": %q},
			{"type": "redacted_thinking", "data": %q},
			{"type": "tool_use", "id": "toolu_2", "name": "lookup_balance",
				"input": {"ssn": %q}},
			{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1", "content": [
				{"type": "web_search_result", "url": %q, "title": %q,
					"encrypted_content": %q, "page_age": "2 days ago"}]}
		],
		"usage": {"input_tokens": 1000, "output_tokens": 500,
			"output_tokens_details": {"thinking_tokens": 300},
			"server_tool_use": {"web_search_requests": 1}}
	}`, opus5, assistantText, assistantText, thinkingText, signatureText, redactedText,
		toolInputText, searchSrcText, searchTitle, docBodyText))
}

// The durable record of a Messages request carries accounting facts and no content.
//
// Serialized whole and searched, rather than asserted field by field, for the reason the
// sentinels are distinct: the field a leak arrives in is by definition one nobody thought
// to assert on. Every sentinel above is injected into this one request and every one of
// them must be absent from the record it produces.
func TestActivityRecordCarriesNoContent(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = contentReply(t)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID:  "team",
		RequestID: "req-privacy",
		Params:    contentRequest(),
		Metadata:  map[string]string{"workload": "nightly-report"},
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if !res.Settled {
		t.Fatal("the request must settle, or this test proves nothing about a completed record")
	}

	rec := h.record(t, "req-privacy")
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	for _, secret := range []string{
		userText, systemText, assistantText, thinkingText, signatureText, redactedText,
		toolInputText, toolResultText, docTitleText, docContextText, docBodyText,
		searchSrcText, searchTitle, imageDataText, userIDText,
	} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("the durable record contains content that must never be persisted: %q", secret)
		}
	}
	// Nor the provider's handles on content it is holding. A tool-use ID or a container ID
	// is not an accounting fact; it is a pointer at data throttle has no business keeping.
	for _, forbidden := range []string{"toolu_1", "toolu_2", "srvtoolu_1", "internal.invalid"} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("the record contains %q, which is a handle on content rather than an "+
				"accounting fact", forbidden)
		}
	}

	// And the accounting facts that must be there, since a record that kept nothing would
	// pass the search above trivially.
	if rec.Identity.AccessProvider != "anthropic" || rec.Identity.Publisher != "anthropic" {
		t.Errorf("recorded provider/publisher = %q/%q, want anthropic/anthropic",
			rec.Identity.AccessProvider, rec.Identity.Publisher)
	}
	if rec.Identity.Operation != "messages" {
		t.Errorf("recorded operation = %q, want messages", rec.Identity.Operation)
	}
	if rec.Identity.ProviderModelID != opus5 {
		t.Errorf("recorded model = %q, want %q", rec.Identity.ProviderModelID, opus5)
	}
	for _, want := range []struct {
		d usage.Dimension
		n int64
	}{
		{usage.InputTokens, 1000},
		{usage.OutputTokens, 500},
		{usage.Searches, 1},
	} {
		if got, _ := rec.ActualUsage.Get(want.d); got != want.n {
			t.Errorf("recorded %s = %d, want %d: a count is accounting metadata even where the "+
				"content it counts is not", want.d, got, want.n)
		}
	}
	if rec.ActualCost.Amount == 0 {
		t.Error("the recorded cost should be the priced actual")
	}
	if rec.Metadata["workload"] != "nightly-report" {
		t.Errorf("caller metadata = %v, want the workload attribution preserved", rec.Metadata)
	}
	if rec.RequestID != "req-privacy" || rec.BudgetID != "team" {
		t.Error("the record must identify the request and the budget it was charged to")
	}
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusSettled)
	}
}

// Thinking content is never persisted, and the count of thinking tokens is.
//
// Its own test because the temptation is specific: the SDK exposes thinking_tokens beside
// the thinking blocks, so an adapter reaching for one is a line away from keeping the
// other. The count is accounting -- it proves the output figure is inclusive -- and the
// text is not.
func TestThinkingContentIsNeverPersisted(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = contentReply(t)

	in := request(opus5, 2000)
	in.Thinking = anth.ThinkingConfigParamOfEnabled(1024)

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-thinking", Params: in,
	}); err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	rec := h.record(t, "req-thinking")
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	for _, secret := range []string{thinkingText, signatureText, redactedText} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("reasoning content reached durable storage: %q", secret)
		}
	}
	// Output is the inclusive total and there is no separate thinking dimension, so the
	// tokens are accounted exactly once at the output rate.
	if got, _ := rec.ActualUsage.Get(usage.OutputTokens); got != 500 {
		t.Errorf("OutputTokens = %d, want the inclusive 500", got)
	}
	if _, ok := rec.ActualUsage.Get(usage.ReasoningTokens); ok {
		t.Error("a thinking dimension was recorded: thinking tokens are already inside " +
			"output_tokens, and a second dimension would either double-charge them or " +
			"invent a rate for them")
	}
}

// Tool arguments and tool results are content, whatever their size. Their size reaches the
// estimate; nothing else about them reaches anything.
func TestToolArgumentsAndResultsAreMeasuredNotStored(t *testing.T) {
	h := newHarness(t, "1000", withoutCounter())

	small := request(opus5, 2000)
	large := request(opus5, 2000)
	result := anth.ToolResultBlockParam{ToolUseID: "toolu_1"}
	result.Content = []anth.ToolResultBlockParamContentUnion{
		{OfText: &anth.TextBlockParam{Text: toolResultText + strings.Repeat(" padding", 500)}},
	}
	large.Messages = append(large.Messages, anth.MessageParam{
		Role:    anth.MessageParamRoleUser,
		Content: []anth.ContentBlockParamUnion{{OfToolResult: &result}},
	})

	estSmall, err := h.client.Estimate(context.Background(), small)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	estLarge, err := h.client.Estimate(context.Background(), large)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	inSmall, _ := estSmall.Usage.Get(usage.InputTokens)
	inLarge, _ := estLarge.Usage.Get(usage.InputTokens)
	if inLarge <= inSmall {
		t.Errorf("input estimate with a large tool result = %d, without = %d: a tool result is "+
			"billed as input and its size must count", inLarge, inSmall)
	}

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-toolresult", Params: large,
	}); err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	blob, err := json.Marshal(h.record(t, "req-toolresult"))
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	if strings.Contains(string(blob), toolResultText) {
		t.Error("a tool result's content reached durable storage; only its size should have")
	}
}

// A provider error quoting the prompt back is reduced to its classification before being
// persisted.
//
// Anthropic's Error.Error() embeds the entire raw response body, so persisting it verbatim
// would persist whatever the provider chose to quote -- and a 400 for an oversized or
// malformed request routinely quotes the request. The record keeps the status, the
// provider's own error type, and the request ID, which is everything an operator acts on.
func TestProviderErrorPayloadIsNotPersistedWholesale(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.setResponse(nil, apiError(t, http.StatusBadRequest, "invalid_request_error",
		"Your prompt was rejected: "+userText))

	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-errpayload", Params: contentRequest(),
	}); !errors.Is(err, anthropic.ErrProvider) {
		t.Fatalf("NewMessage error = %v, want ErrProvider", err)
	}

	rec := h.record(t, "req-errpayload")
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	if strings.Contains(string(blob), userText) {
		t.Errorf("the provider's error message put prompt content into the record: %q", rec.Error)
	}
	if !strings.Contains(rec.Error, "400") {
		t.Errorf("recorded error %q should keep the HTTP status", rec.Error)
	}
	if !strings.Contains(rec.Error, "invalid_request_error") {
		t.Errorf("recorded error %q should keep Anthropic's own error type", rec.Error)
	}
}

// throttle holds no Anthropic credentials, in its config surface or in its records.
//
// The SDK owns its credential chain -- API key, auth token, profile, identity token file,
// federation -- and throttle deliberately has no field for any of it and reads none of it.
// A key present in the environment is invisible to this adapter, and the environment is
// left exactly as the caller set it.
func TestAdapterHoldsNoCredentials(t *testing.T) {
	cfgType := reflect.TypeOf(anthropic.Config{})
	for i := 0; i < cfgType.NumField(); i++ {
		name := strings.ToLower(cfgType.Field(i).Name)
		for _, bad := range []string{
			"apikey", "key", "secret", "credential", "password", "auth", "bearer",
			"token", "profile", "federation", "identity",
		} {
			if strings.Contains(name, bad) {
				t.Errorf("anthropic.Config has a %s field: throttle is not a secret manager, and "+
					"credentials must stay with the SDK", cfgType.Field(i).Name)
			}
		}
	}

	// Every secret the current SDK resolves from the environment, set at once. None can
	// reach the record, because nothing in the adapter reads the environment at all.
	env := map[string]string{
		"ANTHROPIC_API_KEY":             apiKeyText,
		"ANTHROPIC_AUTH_TOKEN":          "SENTINEL-AUTH-TOKEN",
		"ANTHROPIC_PROFILE":             "SENTINEL-PROFILE",
		"ANTHROPIC_FEDERATION_RULE_ID":  "SENTINEL-FEDERATION-RULE",
		"ANTHROPIC_ORGANIZATION_ID":     "SENTINEL-ORG-ID",
		"ANTHROPIC_IDENTITY_TOKEN":      "SENTINEL-IDENTITY-TOKEN",
		"ANTHROPIC_IDENTITY_TOKEN_FILE": "SENTINEL-IDENTITY-TOKEN-FILE",
		"ANTHROPIC_WEBHOOK_SIGNING_KEY": "SENTINEL-WEBHOOK-KEY",
		"ANTHROPIC_CUSTOM_HEADERS":      "SENTINEL-CUSTOM-HEADERS",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	h := newHarness(t, "1000")
	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-creds", Params: request(opus5, 2000),
	}); err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	blob, err := json.Marshal(h.record(t, "req-creds"))
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	for k, v := range env {
		if strings.Contains(string(blob), v) {
			t.Errorf("the value of %s reached the durable activity record", k)
		}
	}
	if strings.Contains(string(blob), "sk-ant-") {
		t.Error("something key-shaped reached the durable activity record")
	}
	// The environment is untouched: throttle does not read, rewrite, or clear it, because
	// clearing a caller's credentials in production would break every other client in the
	// process.
	for k, v := range env {
		if os.Getenv(k) != v {
			t.Errorf("the adapter modified %s, which is not its to manage", k)
		}
	}
}

// A stranded request is reconciled from the normalized durable record, with no
// Anthropic-specific logic and no second call to Anthropic.
//
// The load-bearing test for provider neutrality of the recovery path: the reconciler has
// never heard of Anthropic, it imports no Anthropic SDK, and it repairs a Messages request
// from the same frozen facts it uses for any other provider. Content is not among those
// facts, which is why a record that keeps none is still reconcilable.
func TestStrandedMessageReconcilesFromDurableFacts(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000000, "output_tokens": 200000, "inference_geo": "us"
	}`)
	h.api.block = make(chan struct{})

	// Cancelling mid-call strands the request in exactly the state a crash leaves: the
	// hold standing, the record outstanding, and the captured quote frozen inside it.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.client.NewMessage(ctx, anthropic.Request{
			BudgetID: "team", RequestID: "req-stranded", Params: contentRequest(),
		})
	}()
	waitFor(t, func() bool { return h.api.callCount() == 1 })
	cancel()
	<-done
	close(h.api.block)

	rec := h.record(t, "req-stranded")
	if rec.Status == activity.StatusSettled {
		t.Fatal("the request settled, so there is nothing stranded to reconcile")
	}
	// The record carries no content, which is the precondition for the rest of this test
	// meaning anything.
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	for _, secret := range []string{userText, systemText, toolInputText, docBodyText} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("a stranded record kept content: %q", secret)
		}
	}

	// And the reconciler resolves it from those facts alone. It is built with a ledger and
	// an activity store and nothing else: no Anthropic client, no catalog, no adapter.
	rc, err := reconcile.New(reconcile.Config{
		Ledger:   h.ledger,
		Activity: h.activity,
		Clock:    h.clock,
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}
	out, err := rc.Reconcile(context.Background(), "req-stranded")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	switch out.Class {
	case reconcile.ClassUnresolved, reconcile.ClassRepaired:
		// Either is a definite verdict reached from the durable record alone.
	default:
		t.Errorf("Class = %q, want unresolved or repaired: a stranded Messages request must be "+
			"classifiable from normalized durable facts", out.Class)
	}
	if h.api.callCount() != 1 {
		t.Errorf("Anthropic was called %d times, want 1: reconciliation must replay the frozen "+
			"quote rather than re-ask the provider what happened", h.api.callCount())
	}
	// The frozen quote is what makes the record re-priceable, and it survived. A US-served
	// request needs the captured alternate specifically, which is the geography axis's stake
	// in reconciliation.
	after := h.record(t, "req-stranded")
	if !after.Quote.Valid() {
		t.Error("the captured quote must survive in the durable record, or nothing can re-price it")
	}
}

// Observe reports normalized usage without pricing it or storing content, which is the
// path a caller uses to account for a request throttle did not govern.
func TestObserveReportsUsageWithoutContent(t *testing.T) {
	h := newHarness(t, "1000")

	u, err := h.client.Observe(context.Background(), contentReply(t))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	blob, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshalling the usage: %v", err)
	}
	for _, secret := range []string{assistantText, thinkingText, signatureText, toolInputText} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("Observe returned content: %q", secret)
		}
	}
	if got, _ := u.Usage.Get(usage.InputTokens); got != 1000 {
		t.Errorf("InputTokens = %d, want 1000", got)
	}
}

// Every test in this package runs with no Anthropic credentials present. Asserting it
// makes a future test that reaches for the network fail loudly here rather than pass
// quietly on a machine that happens to have a key.
func TestSuiteRequiresNoCredentials(t *testing.T) {
	for _, key := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
		"ANTHROPIC_PROFILE", "ANTHROPIC_IDENTITY_TOKEN", "ANTHROPIC_IDENTITY_TOKEN_FILE",
		"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID",
	} {
		t.Setenv(key, "")
	}
	h := newHarness(t, "1000")
	if _, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-nocreds", Params: request(opus5, 2000),
	}); err != nil {
		t.Fatalf("NewMessage with no credentials: %v", err)
	}
}
