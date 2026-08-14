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
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/pricing"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/usage"
)

// Audio, and why these tests are about incompleteness rather than about prices.
//
// OpenAI's audio-capable Chat Completions models bill audio in tokens at rates several
// times the text rates -- on gpt-audio, $32.00/M in and $64.00/M out against $2.50 and
// $10.00 for text. The fixtures throttle ships carry no audio rates, because they cover
// text models and an audio pricing sheet is its own piece of work with its own
// provenance. So an audio request is currently a request whose complete monetary exposure
// throttle cannot state, and these tests pin what happens then: denied under enforce
// before OpenAI is called, admitted under monitor, and settled as a floor with the audio
// dimensions named -- never as a completed price, and never with the audio component
// rendered as zero.
//
// The last test in the file is the one that makes the rest safe to ship: adding
// authoritative audio rates to a catalog is sufficient to make such a request fully
// priced, with nothing about the lifecycle changing.

// The four ways a caller reaches audio, each detected independently.
//
// Independently and not by model name, because a model ID this build has never seen must
// still be handled correctly and audio capability is not derivable from a string. A false
// negative here is the expensive one: it admits a request under enforce whose exposure
// throttle cannot bound.
func TestAudioRequestsAreDetectedFromTheRequest(t *testing.T) {
	audioParam := chatRequest(gpt51, maxOut(500))
	audioParam.Audio = oai.ChatCompletionAudioParam{
		Format: "mp3",
		Voice: oai.ChatCompletionAudioParamVoiceUnion{
			OfString: param.NewOpt("alloy"),
		},
	}

	modalities := chatRequest(gpt51, maxOut(500))
	modalities.Modalities = []string{"text", "audio"}

	inputAudio := oai.ChatCompletionNewParams{
		Model:               gpt51,
		MaxCompletionTokens: param.NewOpt(int64(500)),
		Messages: []oai.ChatCompletionMessageParamUnion{
			{OfUser: &oai.ChatCompletionUserMessageParam{
				Content: oai.ChatCompletionUserMessageParamContentUnion{
					OfArrayOfContentParts: []oai.ChatCompletionContentPartUnionParam{
						oai.TextContentPart("transcribe this"),
						oai.InputAudioContentPart(oai.ChatCompletionContentPartInputAudioInputAudioParam{
							Data: "AAAA", Format: "wav",
						}),
					},
				},
			}},
		},
	}

	priorAudio := chatRequest(gpt51, maxOut(500))
	priorAudio.Messages = append(priorAudio.Messages, oai.ChatCompletionMessageParamUnion{
		OfAssistant: &oai.ChatCompletionAssistantMessageParam{
			Audio: oai.ChatCompletionAssistantMessageParamAudio{ID: "audio_abc123"},
		},
	})

	cases := []struct {
		name string
		in   oai.ChatCompletionNewParams
	}{
		{"audio param", audioParam},
		{"modalities names audio", modalities},
		{"input_audio content part", inputAudio},
		{"assistant references a prior audio response", priorAudio},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newChatHarness(t, "1000")
			est, err := h.client.EstimateChat(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("EstimateChat: %v", err)
			}
			if est.Cost.Known() {
				t.Fatalf("cost is known for an audio-bearing request against a catalog with no "+
					"audio rates: %s", est.Cost)
			}
			if !strings.Contains(est.Cost.Reason, "audio") {
				t.Errorf("cost reason %q should say audio is why the figure is not a total",
					est.Cost.Reason)
			}
		})
	}

	// The control: the same request without any audio signal is fully priced. Without
	// this, a change that marked every request incomplete would pass the four cases above.
	h := newChatHarness(t, "1000")
	est, err := h.client.EstimateChat(context.Background(), chatRequest(gpt51, maxOut(500)))
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if !est.Cost.Known() {
		t.Errorf("a text-only request should be fully priced, got %s: %s", est.Cost.State(), est.Cost.Reason)
	}
}

// A text-only message shape carries no audio, which is what makes the detection above
// meaningful rather than a blanket flag.
//
// Checked across the variants a caller actually sends, because the detector walks the
// message array and a bug that returned true for any content-part list would look
// conservative while denying every multimodal text request under enforce.
func TestTextOnlyMessagesDoNotCarryAudio(t *testing.T) {
	h := newChatHarness(t, "1000")

	in := chatRequest(gpt51, maxOut(500))
	in.Messages = []oai.ChatCompletionMessageParamUnion{
		oai.SystemMessage("be terse"),
		{OfUser: &oai.ChatCompletionUserMessageParam{
			Content: oai.ChatCompletionUserMessageParamContentUnion{
				OfArrayOfContentParts: []oai.ChatCompletionContentPartUnionParam{
					oai.TextContentPart("a question"),
				},
			},
		}},
		oai.AssistantMessage("an earlier answer"),
		oai.ToolMessage("a tool result", "call_1"),
	}

	est, err := h.client.EstimateChat(context.Background(), in)
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if !est.Cost.Known() {
		t.Errorf("a text-only conversation must be fully priced, got %s: %s",
			est.Cost.State(), est.Cost.Reason)
	}
	// And modalities that name only text is not an audio signal.
	in.Modalities = []string{"text"}
	est, err = h.client.EstimateChat(context.Background(), in)
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if !est.Cost.Known() {
		t.Errorf(`modalities ["text"] is not an audio request: %s`, est.Cost.Reason)
	}
}

// Enforce denies an audio request with no captured audio rates, before OpenAI is called.
//
// Before, not after. The whole point of denying is that the spend has not happened yet:
// admitting it would authorize an amount throttle does not know, and there is no
// exposure figure to invent that would make the reservation honest.
func TestAudioWithoutRatesIsDeniedBeforeExecutionUnderEnforce(t *testing.T) {
	h := newChatHarness(t, "1000")

	_, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "audio-enforce", Params: audioRequest(gpt51, 500),
	})
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("Complete error = %v, want ErrCostUnknown", err)
	}
	if h.chat.callCount() != 0 {
		t.Fatalf("OpenAI was called %d times: a request whose exposure throttle cannot bound "+
			"must be refused before the money is spent", h.chat.callCount())
	}
	if got := h.totals(t).Reserved; got != 0 {
		t.Errorf("Reserved = %s, want 0: nothing was admitted, so nothing is held", got)
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}

	rec := h.record(t, "audio-enforce")
	if rec.Status != activity.StatusDenied {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusDenied)
	}
	// Recorded as unpriced rather than budget-denied. The budget had $1000 free; what
	// failed was throttle's ability to price the request, and the two call for entirely
	// different operator action.
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("outcome = %q, want %q: the budget was not full, the price was missing",
			rec.Outcome, activity.OutcomeUnpriced)
	}
	if !strings.Contains(rec.ActualCost.Reason, "audio") {
		t.Errorf("recorded reason %q should name audio", rec.ActualCost.Reason)
	}
	// The dimensions are named, not just the fact. A reconciler that later learns the
	// rates needs to know which ones were missing.
	if len(rec.ActualCost.Unpriced) == 0 {
		t.Error("the unpriced audio dimensions must be named on the record")
	}
	if got := dimensionSet(rec.ActualCost.Unpriced); !got[usage.InputAudioTokens] || !got[usage.OutputAudioTokens] {
		t.Errorf("unpriced = %v, want both audio directions", rec.ActualCost.Unpriced)
	}
}

// Monitor admits the same request, under the same unknown-cost semantics every other
// unpriceable request uses -- holding the part of it that is arithmetic.
//
// The hold is the text floor rather than zero, and that is the shared engine's rule
// rather than an audio one: an estimate downgraded to a floor still names an amount that
// will be spent, and offering it to the next caller would be releasing spend that has not
// happened yet. Zero is reserved for a request where nothing at all is knowable.
func TestAudioWithoutRatesIsAdmittedUnderMonitor(t *testing.T) {
	h := newMonitorChatHarness(t)
	h.chat.out = audioCompletion(t, gpt51)

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "audio-monitor", Params: audioRequest(gpt51, 500),
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Complete error = %v, want ErrCostUnresolved", err)
	}
	if h.chat.callCount() != 1 {
		t.Fatalf("OpenAI was called %d times, want 1: monitor mode observes rather than prevents",
			h.chat.callCount())
	}
	if res.Completion == nil {
		t.Error("the caller must still get their completion in monitor mode")
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0: an incompletely priced request must not settle", got)
	}
	// The hold is the knowable floor and it stays encumbered, because money was spent
	// that throttle cannot fully name.
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: the text portion of this request is priced arithmetic, and holding " +
			"nothing against it offers already-committed headroom to the next caller")
	}

	rec := h.record(t, "audio-monitor")
	if rec.EnforcementMode != engine.ModeMonitor {
		t.Errorf("mode = %q, want %q", rec.EnforcementMode, engine.ModeMonitor)
	}
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusUnresolved)
	}
	if rec.ActualUsage.Empty() {
		t.Error("the audio usage must be persisted: it is what makes the record reconcilable")
	}
}

// The partial-cost floor, which is the core of the audio decision.
//
// throttle knows authoritative text usage and authoritative text rates, and does not know
// authoritative audio rates. So the honest answer is a floor: the mathematically valid
// known text cost, plus the audio dimensions named as unpriced.
//
// The floor is valid only because the usage decomposition proves the two parts are
// disjoint. Both audio_tokens counters are inclusive details of their parent totals, so
// subtracting them leaves a text figure that contains no audio and the audio counters
// contain no text. There is no overlap to double-charge and no gap to miss.
func TestAudioActualSettlesAsAFloorWithNamedUnpricedDimensions(t *testing.T) {
	h := newMonitorChatHarness(t)
	h.chat.out = audioCompletion(t, gpt51)

	res, _ := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "audio-floor", Params: audioRequest(gpt51, 500),
	})

	// The four dimensions, disjoint, summing to the reported totals.
	inText, _ := res.Usage.Get(usage.InputTokens)
	inAudio, ok := res.Usage.Get(usage.InputAudioTokens)
	if !ok {
		t.Fatal("input audio tokens must be preserved as their own dimension")
	}
	outText, _ := res.Usage.Get(usage.OutputTokens)
	outAudio, ok := res.Usage.Get(usage.OutputAudioTokens)
	if !ok {
		t.Fatal("output audio tokens must be preserved as their own dimension")
	}
	if inText != 200 || inAudio != 800 {
		t.Errorf("prompt split = %d text / %d audio, want 200/800 from a reported 1000", inText, inAudio)
	}
	if outText != 100 || outAudio != 400 {
		t.Errorf("completion split = %d text / %d audio, want 100/400 from a reported 500", outText, outAudio)
	}

	// The cost is a floor, and the floor is the text arithmetic exactly: 200 at $1.25/M
	// plus 100 at $10.00/M.
	if res.Cost.State() != usage.CostPartial {
		t.Fatalf("cost state = %v, want CostPartial: text is known, audio is not, and both facts "+
			"are true at once", res.Cost.State())
	}
	if want := dollars(t, "0.00125"); res.Cost.Amount != want {
		t.Errorf("floor = %s, want %s (the priced text portion, and nothing standing in for audio)",
			res.Cost.Amount, want)
	}
	if res.Cost.Known() {
		t.Error("a floor is not a known cost, and a caller must not be able to treat it as one")
	}

	// Never zero. Not the amount, and not the audio component: the missing part is named,
	// which is the difference between "we could not price this" and "this was free".
	if res.Cost.Amount == 0 {
		t.Error("the text floor is a real, valid figure and should not be erased")
	}
	got := dimensionSet(res.Cost.Unpriced)
	if !got[usage.InputAudioTokens] || !got[usage.OutputAudioTokens] {
		t.Errorf("Unpriced = %v, want both audio dimensions named", res.Cost.Unpriced)
	}
	if res.Settled {
		t.Error("a floor must not settle as though it were a total")
	}

	// And the same is true of the durable record, which is what a reconciler reads.
	rec := h.record(t, "audio-floor")
	if rec.ActualCost.State() != usage.CostPartial {
		t.Errorf("recorded cost state = %v, want CostPartial", rec.ActualCost.State())
	}
	if n, ok := rec.ActualUsage.Get(usage.OutputAudioTokens); !ok || n != 400 {
		t.Errorf("recorded output audio tokens = %d (present %v), want 400", n, ok)
	}
	if !strings.Contains(rec.ActualCost.Reason, "input_audio_tokens") ||
		!strings.Contains(rec.ActualCost.Reason, "output_audio_tokens") {
		t.Errorf("recorded reason %q should name the dimensions by their neutral names, which is "+
			"what a later reconciliation matches on", rec.ActualCost.Reason)
	}
}

// Audio tokens are not folded into the text dimensions, which is the mistake that would
// undercharge an audio request by an order of magnitude while looking entirely ordinary.
//
// Anti-vacuous by comparison: the same totals reported without an audio breakdown price
// higher, because every token is then text. If audio were folded in, the two would be
// equal.
func TestAudioTokensAreNotPricedAsText(t *testing.T) {
	h := newMonitorChatHarness(t)

	h.chat.out = audioCompletion(t, gpt51)
	withAudio, _ := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "audio-split", Params: audioRequest(gpt51, 500),
	})

	// The same 1000 and 500, with no audio breakdown: all text.
	h.chat.out = completion(t, gpt51, 1000, 500)
	allText, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "audio-none", Params: chatRequest(gpt51, maxOut(500)),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if withAudio.Cost.Amount >= allText.Cost.Amount {
		t.Errorf("the audio request's text floor (%s) is not below the all-text charge (%s): the "+
			"audio tokens have been priced at text rates, which understates an audio bill by "+
			"roughly an order of magnitude", withAudio.Cost.Amount, allText.Cost.Amount)
	}
	if !allText.Cost.Known() {
		t.Errorf("the all-text control should be fully priced: %s", allText.Cost.Reason)
	}
}

// AudioSeconds is not used. It belongs to models genuinely documented as duration-billed,
// and using it for a token count would convert a measurement the provider reported into
// one it did not, to fit a rate that does not apply.
func TestAudioIsNotMeasuredInSeconds(t *testing.T) {
	h := newMonitorChatHarness(t)
	h.chat.out = audioCompletion(t, gpt51)

	res, _ := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "audio-seconds", Params: audioRequest(gpt51, 500),
	})
	if _, ok := res.Usage.Get(usage.AudioSeconds); ok {
		t.Error("audio_seconds is present: Chat Completions reports audio tokens, and a duration " +
			"throttle derived from a token count would be a measurement nobody made")
	}
	for _, d := range res.Cost.Unpriced {
		if d == usage.AudioSeconds {
			t.Error("audio_seconds named as unpriced: it is not a dimension this API bills in")
		}
	}
}

// A response that reports audio tokens for a model whose quote has no audio rate is a
// partial cost even from pricing alone -- no request-side exposure needed.
//
// Worth pinning separately because the two mechanisms overlap by design. Pricing names
// unpriced dimensions it was actually asked to price; the request's exposure names ones it
// might be. A request that somehow reached settlement without its exposure flagged still
// must not settle an audio-bearing usage object as a total.
func TestReportedAudioTokensAlonePreventAFullPrice(t *testing.T) {
	h := newMonitorChatHarness(t)
	// No audio signal in the request at all -- and the response reports audio anyway,
	// which is the case throttle cannot rule out.
	h.chat.out = audioCompletion(t, gpt51)

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "audio-surprise", Params: chatRequest(gpt51, maxOut(500)),
	})
	if !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Complete error = %v, want ErrCostUnresolved", err)
	}
	if res.Cost.Known() {
		t.Error("a usage object reporting audio tokens the quote cannot price is not fully priced")
	}
	if got := dimensionSet(res.Cost.Unpriced); !got[usage.InputAudioTokens] || !got[usage.OutputAudioTokens] {
		t.Errorf("Unpriced = %v, want the audio dimensions pricing could not rate", res.Cost.Unpriced)
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}
}

// The test that makes the rest of this file a temporary state rather than a limitation.
//
// A catalog carrying authoritative per-model input and output audio-token rates prices an
// audio request completely: it is admitted under enforce, it settles, and the charge is
// the four dimensions at their own rates. Nothing about the Chat Completions lifecycle
// changes -- no new branch, no new field, no different code path. Adding the rates is the
// whole of the work.
func TestAudioRatesMakeAnAudioRequestFullyPriced(t *testing.T) {
	// gpt-audio's published rates: $2.50/M text in, $10.00/M text out, $32.00/M audio in,
	// $64.00/M audio out.
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "openai",
		ProviderModelID: "gpt-audio",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:       pricing.PerMillion(usage.InputTokens, dollars(t, "2.50")),
			usage.OutputTokens:      pricing.PerMillion(usage.OutputTokens, dollars(t, "10.00")),
			usage.ReasoningTokens:   pricing.PerMillion(usage.ReasoningTokens, dollars(t, "10.00")),
			usage.InputAudioTokens:  pricing.PerMillion(usage.InputAudioTokens, dollars(t, "32.00")),
			usage.OutputAudioTokens: pricing.PerMillion(usage.OutputAudioTokens, dollars(t, "64.00")),
		},
		Provenance: pricing.Provenance{Source: "test-audio", Version: "audio-1", Currency: "USD"},
	})
	if err != nil {
		t.Fatalf("pricing.NewStatic: %v", err)
	}

	h := newChatHarness(t, "1000", func(cfg *openai.Config) { cfg.Catalog = cat })
	h.chat.out = audioCompletion(t, "gpt-audio")

	est, err := h.client.EstimateChat(context.Background(), audioRequest("gpt-audio", 500))
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if !est.Cost.Known() {
		t.Fatalf("with audio rates captured, the estimate is complete; got %s: %s",
			est.Cost.State(), est.Cost.Reason)
	}

	res, err := h.client.Complete(context.Background(), openai.ChatRequest{
		BudgetID: "team", RequestID: "audio-priced", Params: audioRequest("gpt-audio", 500),
	})
	if err != nil {
		t.Fatalf("Complete under enforce with audio rates: %v", err)
	}
	if !res.Settled {
		t.Fatal("a fully priced audio request settles like any other")
	}
	if !res.Cost.Known() {
		t.Errorf("cost should be known: %s", res.Cost.Reason)
	}
	if len(res.Cost.Unpriced) != 0 {
		t.Errorf("Unpriced = %v, want empty", res.Cost.Unpriced)
	}

	// 200 text in at $2.50/M = $0.0005; 800 audio in at $32.00/M = $0.0256;
	// 100 text out at $10.00/M = $0.001; 400 audio out at $64.00/M = $0.0256.
	want := dollars(t, "0.0527")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	// And the audio half dominates, which is the whole reason it cannot be folded into
	// text: $0.0512 of a $0.0527 bill.
	if got := h.totals(t).Spent; got != want {
		t.Errorf("Spent = %s, want %s", got, want)
	}
}

// A quote that prices one audio direction but not the other names only the direction it
// cannot price. Half-known is more useful than "audio", and it is what a reconciler needs
// to know which rate to go and find.
func TestPartialAudioRatesNameOnlyTheMissingDirection(t *testing.T) {
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "openai",
		ProviderModelID: "gpt-audio-in-only",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:      pricing.PerMillion(usage.InputTokens, dollars(t, "2.50")),
			usage.OutputTokens:     pricing.PerMillion(usage.OutputTokens, dollars(t, "10.00")),
			usage.ReasoningTokens:  pricing.PerMillion(usage.ReasoningTokens, dollars(t, "10.00")),
			usage.InputAudioTokens: pricing.PerMillion(usage.InputAudioTokens, dollars(t, "32.00")),
		},
		Provenance: pricing.Provenance{Source: "test-audio", Version: "audio-1", Currency: "USD"},
	})
	if err != nil {
		t.Fatalf("pricing.NewStatic: %v", err)
	}

	h := newChatHarness(t, "1000", func(cfg *openai.Config) { cfg.Catalog = cat })

	est, err := h.client.EstimateChat(context.Background(), audioRequest("gpt-audio-in-only", 500))
	if err != nil {
		t.Fatalf("EstimateChat: %v", err)
	}
	if est.Cost.Known() {
		t.Fatal("output audio has no rate, so the exposure is still incomplete")
	}
	got := dimensionSet(est.Cost.Unpriced)
	if got[usage.InputAudioTokens] {
		t.Errorf("Unpriced = %v: input audio has a captured rate and must not be named",
			est.Cost.Unpriced)
	}
	if !got[usage.OutputAudioTokens] {
		t.Errorf("Unpriced = %v, want output_audio_tokens", est.Cost.Unpriced)
	}
	if strings.Contains(est.Cost.Reason, "input_audio_tokens") {
		t.Errorf("reason %q should not name a dimension throttle can price", est.Cost.Reason)
	}
}

// audioRequest builds a request that asks for an audio response, which is what OpenAI's
// own audio guide tells a caller to send.
func audioRequest(model string, cap int64) oai.ChatCompletionNewParams {
	in := chatRequest(model, &cap)
	in.Modalities = []string{"text", "audio"}
	in.Audio = oai.ChatCompletionAudioParam{
		Format: "mp3",
		Voice:  oai.ChatCompletionAudioParamVoiceUnion{OfString: param.NewOpt("alloy")},
	}
	return in
}

// audioCompletion reports usage decomposed into text and audio in both directions.
//
// 1000 prompt tokens of which 800 are audio, and 500 completion tokens of which 400 are
// audio. The audio share is large deliberately: it makes the difference between pricing
// audio at its own rate, pricing it as text, and dropping it entirely visible in the
// resulting figure rather than lost in rounding.
func audioCompletion(t *testing.T, model string) *oai.ChatCompletion {
	t.Helper()
	return complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_audio", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop",
			"message": {"role": "assistant", "content": null,
				"audio": {"id": "audio_abc", "data": "AAAA", "expires_at": 1786003600,
					"transcript": "a spoken answer"}}}],
		"usage": {
			"prompt_tokens": 1000,
			"prompt_tokens_details": {"audio_tokens": 800, "cached_tokens": 0},
			"completion_tokens": 500,
			"completion_tokens_details": {"audio_tokens": 400},
			"total_tokens": 1500
		}
	}`, model))
}

// dimensionSet indexes a dimension list for membership assertions.
func dimensionSet(dims []usage.Dimension) map[usage.Dimension]bool {
	out := make(map[usage.Dimension]bool, len(dims))
	for _, d := range dims {
		out[d] = true
	}
	return out
}
