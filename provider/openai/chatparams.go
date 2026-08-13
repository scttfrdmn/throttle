package openai

import (
	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

// chatParams is the subset of a Chat Completions request that admission needs.
//
// A separate type from params rather than a shared one, and that is the deliberate
// choice this slice turns on. The two structs overlap in what they hold -- a model, a
// tier, an output ceiling -- and merging them would look like removing duplication. It
// would in fact assert that the two APIs' admission inputs mean the same thing, and
// several of them do not:
//
//   - Responses has one output cap, max_output_tokens, defined as covering visible and
//     reasoning tokens together. Chat Completions has two, max_completion_tokens and a
//     deprecated max_tokens, with different applicability across model families.
//   - Chat Completions has n, which multiplies billable output and has no Responses
//     counterpart at all.
//   - Chat Completions reaches web search through a request field rather than a tool,
//     so the tool list is not where all of its unaccountable charges live.
//   - Chat Completions can bill audio tokens; the Responses path this adapter governs
//     does not surface them.
//   - Responses input arrives as a string or an item list; Chat Completions input is a
//     message array whose six variants nest content differently.
//
// A single struct spanning both would need a field for each of those anyway, and every
// reader would then have to know which fields apply to which family. Two small structs
// that each mean one thing are cheaper to be correct about. What the two genuinely
// share -- identity, quote capture, reservation, settlement -- is shared as code, in
// estimate and in lifecycle.go, not as a merged data model.
//
// Like params, it holds references to the caller's slices rather than copies, and
// nothing here mutates them.
type chatParams struct {
	operation string

	modelID     string
	serviceTier string

	// messages is the caller's message array, read for length and for audio content.
	messages []oai.ChatCompletionMessageParamUnion

	tools []oai.ChatCompletionToolUnionParam

	// webSearch reports that the request enables web search through
	// web_search_options, which is a request field here rather than a tool.
	//
	// It matters because OpenAI bills web search per call, on its pricing page and
	// nowhere in the response -- so a request with it set cannot be fully priced, and a
	// classifier that only looked at the tool list would miss that entirely.
	webSearch bool

	// maxOutputTokens is the caller's output cap, or nil when they set none.
	//
	// Read but never written, exactly as in params: throttle does not modify the
	// caller's requested output cap, and a request that reaches OpenAI with a cap
	// throttle chose is a request the caller cannot reason about.
	maxOutputTokens *int64

	// capField names which cap the ceiling came from, for the estimate's note. A
	// caller who set the deprecated field should be told that is what was honoured.
	capField string

	// choices is n: how many completions the request asks for, defaulting to 1.
	//
	// It multiplies potential output exposure, because OpenAI charges for the tokens
	// generated across all of the choices -- its own documentation on the field says
	// so. It therefore belongs in the conservative ceiling and nowhere else: actual
	// settlement is driven by the provider's reported usage, which already reflects
	// what was really generated, and multiplying that again would double-charge.
	choices int64

	// carriesAudio reports that this request can consume or produce audio tokens,
	// which are billed at their own rates. See audio.go.
	carriesAudio bool
}

// chatCompletionParams reads the fields admission needs out of a Chat Completions
// request.
func chatCompletionParams(in oai.ChatCompletionNewParams) chatParams {
	p := chatParams{
		operation:    OperationChatCompletions,
		modelID:      chatModelOf(in.Model),
		serviceTier:  chatRequestTier(in.ServiceTier),
		messages:     in.Messages,
		tools:        in.Tools,
		webSearch:    !param.IsOmitted(in.WebSearchOptions),
		choices:      chatChoices(in.N),
		carriesAudio: carriesAudio(in),
	}
	p.maxOutputTokens, p.capField = chatOutputCap(in)
	return p
}

// chatOutputCap reads the caller's output ceiling, preferring the current field.
//
// OpenAI has two: max_completion_tokens, documented as an upper bound on generated
// tokens "including visible output tokens and reasoning tokens", and max_tokens, which
// it deprecates and states is not compatible with o-series models. The current field
// wins when both are set, because that is the one OpenAI will honour.
//
// The deprecated field is still read rather than ignored. A real application that
// predates the rename sets it, OpenAI still applies it on the models that accept it,
// and treating it as absent would substitute throttle's assumed default for a bound the
// caller actually declared -- reserving against a ceiling the request does not have.
//
// Neither field is ever written. throttle does not rewrite the caller's cap, does not
// migrate max_tokens to max_completion_tokens, and does not add a cap to a request that
// declared none: the request that reaches OpenAI is the request the caller built.
func chatOutputCap(in oai.ChatCompletionNewParams) (*int64, string) {
	if n := in.MaxCompletionTokens; n.Valid() && n.Value > 0 {
		limit := n.Value
		return &limit, "max_completion_tokens"
	}
	if n := in.MaxTokens; n.Valid() && n.Value > 0 {
		limit := n.Value
		return &limit, "max_tokens"
	}
	return nil, ""
}

// chatChoices reads n, the number of completions requested.
//
// Absent means one. A zero or negative value is also treated as one rather than as a
// reason to reserve nothing: OpenAI will reject such a request, and an exposure of zero
// is the one answer that could admit unbounded spend if it did not.
func chatChoices(n param.Opt[int64]) int64 {
	if !n.Valid() || n.Value < 1 {
		return 1
	}
	return n.Value
}
