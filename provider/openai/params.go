package openai

import (
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// params is the subset of a Responses request that admission needs: the fields
// determining identity, the input token count, the output ceiling, and whether the
// request's tools put part of its charge out of reach.
//
// It exists for the same reason Bedrock's does, and streaming is what it was built
// for: a streaming Responses request is identified, estimated, and quoted by exactly
// this code, because the streaming and non-streaming forms of one request consume the
// same tokens. Two code paths that price the same request are the one inconsistency
// this package cannot tolerate. See streamParams, which differs from responseParams
// in the operation and in nothing else.
//
// It holds references to the caller's slices rather than copies. Nothing here
// mutates them, and copying prompt content to measure its length would be pointless
// work on the request path.
type params struct {
	operation string

	modelID     string
	serviceTier string

	// instructions and input are the two forms input arrives in: a plain string or a
	// list of items. Both are read, since a caller may use either.
	instructions string
	inputText    string
	inputItems   []responses.ResponseInputItemUnionParam

	tools []responses.ToolUnionParam

	// maxOutputTokens is the caller's output cap, or nil when they set none.
	//
	// A caller-set cap is a real bound on the request: OpenAI defines
	// max_output_tokens as covering visible output and reasoning together, so it
	// bounds the whole output side. throttle's default is an assumption, and the
	// estimate reports which one it used.
	//
	// It is read but never written. throttle does not modify the caller's requested
	// output cap: the request that reaches OpenAI is the request the caller built.
	maxOutputTokens *int64

	// previousResponseID and conversation matter to estimation rather than to
	// identity: either makes the real input larger than anything measurable in this
	// request, because OpenAI prepends history server-side. The heuristic cannot see
	// it, and saying so is more useful than a silently low number.
	carriesHistory bool
}

// responseParams reads the fields admission needs out of a Responses request.
func responseParams(in responses.ResponseNewParams) params {
	p := params{
		operation:      OperationResponses,
		modelID:        modelOf(in.Model),
		serviceTier:    requestTier(in.ServiceTier),
		instructions:   in.Instructions.Or(""),
		inputText:      in.Input.OfString.Or(""),
		inputItems:     in.Input.OfInputItemList,
		tools:          in.Tools,
		carriesHistory: in.PreviousResponseID.Valid() || !isZeroConversation(in.Conversation),
	}
	if n := in.MaxOutputTokens; n.Valid() && n.Value > 0 {
		limit := n.Value
		p.maxOutputTokens = &limit
	}
	return p
}

// streamParams reads the same fields out of the same request type for a streaming
// call.
//
// The SDK uses one params type for both forms -- NewStreaming takes the identical
// ResponseNewParams and sets `stream: true` itself -- so there is genuinely nothing
// to read differently. The operation is the whole difference, and it is set here
// rather than by the caller so that no path can create a streaming request that
// records itself as a non-streaming one.
func streamParams(in responses.ResponseNewParams) params {
	p := responseParams(in)
	p.operation = OperationResponsesStream
	return p
}

// isZeroConversation reports whether no conversation was attached.
//
// The union has no emptiness predicate of its own, so this checks its variants. A
// conversation means server-side history is prepended to the input, which the
// heuristic estimate cannot measure.
func isZeroConversation(c responses.ResponseNewParamsConversationUnion) bool {
	return !c.OfString.Valid() && c.OfConversationObject == nil
}

// countParams builds the preflight input-count request for these params.
//
// It mirrors the real request as closely as the count endpoint allows -- model,
// input, instructions, tools, reasoning, text config, tool choice -- because that is
// what makes the count worth an extra round trip: OpenAI counts the actual request
// shape, including tool schemas, which no local approximation can do.
//
// Built from the caller's own request object rather than from params, since the count
// endpoint takes nearly the same fields and reconstructing them from a reduced struct
// would be a second chance to get them wrong.
func countParams(in responses.ResponseNewParams) responses.InputTokenCountParams {
	out := responses.InputTokenCountParams{
		Model:             param.NewOpt(modelOf(in.Model)),
		Instructions:      in.Instructions,
		ParallelToolCalls: in.ParallelToolCalls,
		Tools:             in.Tools,
		Reasoning:         in.Reasoning,
	}
	if in.Input.OfString.Valid() {
		out.Input.OfString = param.NewOpt(in.Input.OfString.Value)
	} else if len(in.Input.OfInputItemList) > 0 {
		out.Input.OfResponseInputItemArray = in.Input.OfInputItemList
	}
	if in.PreviousResponseID.Valid() {
		out.PreviousResponseID = in.PreviousResponseID
	}
	return out
}
