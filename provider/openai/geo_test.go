package openai_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/pricing"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/usage"
)

// Inference geography is a second axis of the access-dimension rule service tier
// established, and adding it must leave all three OpenAI families settling exactly as
// they did.
//
// OpenAI has no inference-geography parameter and reports none, so the adapter never
// populates the field. That is the claim worth testing: the shared narrowing code now
// consults the axis on every lookup, and the OpenAI fixtures are the ones that price
// several service tiers per model. Wholesale rather than per-axis narrowing would break
// tier selection here the moment any provider's rows named a geography, and the symptom
// would be an ordinary request going unresolved.
func TestGeographyAxisLeavesTierSelectionAloneOnResponses(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(mini, maxOut(2000))
	in.ServiceTier = responses.ResponseNewParamsServiceTierAuto
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_geo", "object": "response", "status": "completed", "model": %q,
		"service_tier": "flex",
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
	}`, mini))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-geo-responses", Params: in,
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if res.Identity.InferenceGeo != "" {
		t.Errorf("InferenceGeo = %q, want empty: OpenAI reports no inference geography",
			res.Identity.InferenceGeo)
	}
	if !res.Cost.Known() {
		t.Fatalf("cost = %s (%s), want known: %s", res.Cost.Amount, res.Cost.State(), res.Cost.Reason)
	}
	// gpt-5-mini flex: $0.125/M input, $1.00/M output. The tier still selects.
	if want := dollars(t, "0.000625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (flex rates)", res.Charge.ActualCost, want)
	}
	if std := dollars(t, "0.00125"); res.Charge.ActualCost == std {
		t.Errorf("ActualCost = %s: the geography axis broke tier selection and the standard rate "+
			"was used for a request served on flex", std)
	}
	if res.Quote.InferenceGeo != "" {
		t.Errorf("captured InferenceGeo = %q, want empty: no OpenAI fixture row prices a geography",
			res.Quote.InferenceGeo)
	}
}

// The same, on Chat Completions.
func TestGeographyAxisLeavesTierSelectionAloneOnChatCompletions(t *testing.T) {
	h := newChatHarness(t, "1000")
	h.chat.out = complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_geo", "object": "chat.completion", "created": 1786000000, "model": %q,
		"service_tier": "priority",
		"choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "hi"}}],
		"usage": {"prompt_tokens": 1000, "completion_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "chat-geo", Params: chatRequest(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Identity.InferenceGeo != "" {
		t.Errorf("InferenceGeo = %q, want empty", res.Identity.InferenceGeo)
	}
	// gpt-5.1 priority: $2.50/M input, $20.00/M output.
	if want := dollars(t, "0.0125"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (priority rates)", res.Charge.ActualCost, want)
	}
	if std := dollars(t, "0.00625"); res.Charge.ActualCost == std {
		t.Errorf("ActualCost = %s: the standard rate was used for a request served on priority", std)
	}
	if got := h.record(t, "chat-geo").Identity.InferenceGeo; got != "" {
		t.Errorf("durable InferenceGeo = %q, want empty", got)
	}
}

// And on Responses streaming, where the terminal event is the accounting boundary.
func TestGeographyAxisLeavesTierSelectionAloneOnStreaming(t *testing.T) {
	in := request(mini, maxOut(2000))
	in.ServiceTier = responses.ResponseNewParamsServiceTierAuto
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, mini),
		event(t, fmt.Sprintf(`{
			"type": "response.completed", "sequence_number": 9,
			"response": {
				"id": "resp_geo_stream", "object": "response", "status": "completed", "model": %q,
				"service_tier": "priority",
				"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
			}
		}`, mini)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-geo", Params: in,
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if res.Identity.InferenceGeo != "" {
		t.Errorf("InferenceGeo = %q, want empty", res.Identity.InferenceGeo)
	}
	if !res.Cost.Known() {
		t.Fatalf("cost = %s (%s), want known: %s", res.Cost.Amount, res.Cost.State(), res.Cost.Reason)
	}
	// gpt-5-mini priority: $0.45/M input, $3.60/M output.
	if want := dollars(t, "0.00225"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (priority rates)", res.Charge.ActualCost, want)
	}
	if std := dollars(t, "0.00125"); res.Charge.ActualCost == std {
		t.Errorf("ActualCost = %s: the standard rate was used for a stream served on priority", std)
	}
}

// A tier that was never priced still reports the tier axis alone on OpenAI, where no
// geography exists.
//
// The generalized error carries both axes, and the wrong version of this change would
// fill the geography in from somewhere -- sending an operator to add a row for a
// dimension this provider does not have.
func TestUncapturedTierReasonNamesNoGeographyOnOpenAI(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_unpriced", "object": "response", "status": "completed", "model": %q,
		"service_tier": "turbo-2027",
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-geo-unpriced-tier", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Respond error = %v, want ErrCostUnresolved", err)
	}
	if res.Cost.Known() {
		t.Fatal("a tier no rate was frozen for must not settle as known")
	}
	if !strings.Contains(res.Cost.Reason, "turbo-2027") {
		t.Errorf("reason %q must name the tier that served the call", res.Cost.Reason)
	}
	if strings.Contains(res.Cost.Reason, "inference geography") {
		t.Errorf("reason %q names a geography for a provider that reports none", res.Cost.Reason)
	}
}

// A geography-qualified row does not price an OpenAI request, whose geography is
// unknown because the provider does not report one.
//
// The direction the per-axis rule makes possible: once a catalog row for a model names
// a geography, the axis selects between price sheets for that model, so a request
// carrying no geography must not fall through onto the geography-qualified sheet. Under
// enforce the call does not happen at all, because nothing bounds what it would cost.
func TestGeoQualifiedRowDoesNotPriceAGeographylessOpenAIRequest(t *testing.T) {
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "openai",
		ProviderModelID: gpt51,
		InferenceGeo:    "us",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "9.99")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "99.90")),
		},
		Provenance: pricing.Provenance{Source: "test-us-only"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	h := newHarness(t, "1000", func(c *openai.Config) { c.Catalog = cat })
	h.api.out = completedResponse(t, gpt51, 1000, 500)

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-geo-mismatch", Params: request(gpt51, maxOut(2000)),
	})
	if err == nil {
		t.Fatalf("Respond succeeded at %s: a row qualified for geography %q must not price a "+
			"request whose geography is unknown", res.Charge.ActualCost, "us")
	}
	if res.Cost.Known() {
		t.Errorf("cost = %s, known: nothing here bounds what the request cost", res.Cost.Amount)
	}
	if got := h.api.callCount(); got != 0 {
		t.Errorf("provider calls = %d, want 0: enforce must refuse before spending money it "+
			"cannot bound", got)
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}
}
