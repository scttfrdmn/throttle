package anthropic

import (
	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// params is the subset of a Messages request that admission needs: the fields
// determining identity, the input token count, the output ceiling, and whether the
// request's tools put part of its charge out of reach.
//
// It holds references to the caller's slices rather than copies. Nothing here mutates
// them -- reading a request to measure it must not change what gets sent -- and copying
// prompt content to measure its length would be pointless work on the request path.
type params struct {
	modelID     string
	serviceTier string

	// inferenceGeo is the geography the caller named, or empty when they named none.
	// Never a default filled in by throttle; see requestGeo.
	inferenceGeo string

	// system and messages are where input lives. Both are read: a request may carry
	// either, and usually carries both.
	system   []anth.TextBlockParam
	messages []anth.MessageParam

	tools []anth.ToolUnionParam

	// maxTokens is the caller's output cap, verbatim.
	//
	// Not a pointer, unlike the other adapters', because there is no absent state to
	// represent: max_tokens is required by the Messages API, so a caller always states a
	// bound and throttle never needs an assumption. Zero is a real value -- Anthropic
	// documents max_tokens: 0 as populating the prompt cache without generating a
	// response -- and it means this request has no output exposure, which is different
	// from meaning nothing was said.
	//
	// It is read and never written. throttle does not modify the caller's requested
	// output cap, not even downward, and not even when the budget is nearly exhausted:
	// the request that reaches Anthropic is the request the caller built.
	maxTokens int64

	// cachedBlocks counts the cache_control markers in the request.
	//
	// Not used for accounting -- what was written and at which lifetime is a fact only
	// the response establishes, and inferring it from these markers is specifically
	// wrong, since Anthropic inserts its own five-minute breakpoint regardless of the
	// TTLs a caller sets. It is used only to note that the input count overstates a
	// cache-heavy request, since count_tokens does not apply caching logic.
	cachedBlocks int
}

// messageParams reads the fields admission needs out of a Messages request.
func messageParams(in anth.MessageNewParams) params {
	p := params{
		modelID:      modelOf(in.Model),
		serviceTier:  requestTier(in.ServiceTier),
		inferenceGeo: requestGeo(in.InferenceGeo),
		system:       in.System,
		messages:     in.Messages,
		tools:        in.Tools,
		maxTokens:    in.MaxTokens,
	}
	p.cachedBlocks = countCacheBreakpoints(in)
	return p
}

// countCacheBreakpoints counts the cache_control markers the caller placed.
//
// A count, not a lifetime tally, and deliberately so. The request says what was asked
// for; only the response says what was written, and Anthropic documents the gap: its
// automatic breakpoint "always uses the default 5-minute TTL, independent of any TTL you
// set on your own `cache_control` markers", so a request whose every marker is one-hour
// can still produce five-minute writes. Reading TTLs here would produce a number that
// looks authoritative and is not.
func countCacheBreakpoints(in anth.MessageNewParams) int {
	n := 0
	if !param.IsOmitted(in.CacheControl) {
		n++
	}
	for _, s := range in.System {
		if !param.IsOmitted(s.CacheControl) {
			n++
		}
	}
	for _, m := range in.Messages {
		for _, b := range m.Content {
			// The SDK's own accessor, which finds the populated variant. Reading a field
			// off a specific variant would miss markers on the other sixteen.
			if cc := b.GetCacheControl(); cc != nil && !param.IsOmitted(*cc) {
				n++
			}
		}
	}
	return n
}

// countParams builds the preflight input-count request for a Messages request.
//
// # Why this is built field by field
//
// MessageCountTokensParams is not a subset of MessageNewParams, and treating it as one
// is the mistake this function exists to avoid. It has no MaxTokens, no InferenceGeo, no
// ServiceTier, no StopSequences, no Container, no Metadata, and no sampling parameters;
// its System is a union where the message request's is a plain slice; and its tool union
// is a separate type with the same nineteen variants. There is no conversion, no
// embedding, and no cast between the two -- so every field is assigned explicitly, and a
// field the count endpoint does not accept is simply not sent.
//
// It mirrors what the endpoint does accept as closely as possible, because that is what
// makes the count worth an extra round trip: Anthropic counts the actual request shape,
// including tool schemas and thinking configuration, which no local approximation can do.
//
// It never mutates the caller's request. The output is a fresh value, the slices it
// shares are only read, and nothing about counting a request changes what will be sent.
// Nor is any of the content it sees persisted: the count is a number, and the number is
// all that comes back.
func countParams(in anth.MessageNewParams) anth.MessageCountTokensParams {
	out := anth.MessageCountTokensParams{
		Model:         in.Model,
		Messages:      in.Messages,
		CacheControl:  in.CacheControl,
		OutputConfig:  in.OutputConfig,
		Thinking:      in.Thinking,
		ToolChoice:    in.ToolChoice,
		UserProfileID: in.UserProfileID,
	}
	if len(in.System) > 0 {
		// The count endpoint's System is a union of a bare string and a block array; the
		// message request always has the array form, so it assigns straight across.
		out.System.OfTextBlockArray = in.System
	}
	out.Tools = countTools(in.Tools)
	return out
}

// countTools converts the message request's tool union into the count endpoint's.
//
// A mechanical nineteen-arm switch, because that is what the SDK's types require. The
// two unions have byte-identical variant lists pointing at byte-identical param types,
// but they are distinct Go types with no conversion between them, and Go will not let one
// stand in for the other -- which is the type system doing its job, since Anthropic is
// free to diverge them and has diverged the count endpoint's other fields already.
//
// A variant this build does not name is dropped from the count rather than silently
// mistranslated. The consequence is a count that omits one tool's schema, which
// understates the input -- so the estimate becomes less conservative in exactly the case
// where a new tool appeared, and classifyTools independently flags that request's
// exposure. Guessing a variant to send instead could send the wrong schema to a live
// endpoint, which is worse.
func countTools(tools []anth.ToolUnionParam) []anth.MessageCountTokensToolUnionParam {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anth.MessageCountTokensToolUnionParam, 0, len(tools))
	for _, t := range tools {
		var c anth.MessageCountTokensToolUnionParam
		switch {
		case t.OfTool != nil:
			c.OfTool = t.OfTool
		case t.OfBashTool20250124 != nil:
			c.OfBashTool20250124 = t.OfBashTool20250124
		case t.OfCodeExecutionTool20250522 != nil:
			c.OfCodeExecutionTool20250522 = t.OfCodeExecutionTool20250522
		case t.OfCodeExecutionTool20250825 != nil:
			c.OfCodeExecutionTool20250825 = t.OfCodeExecutionTool20250825
		case t.OfCodeExecutionTool20260120 != nil:
			c.OfCodeExecutionTool20260120 = t.OfCodeExecutionTool20260120
		case t.OfCodeExecutionTool20260521 != nil:
			c.OfCodeExecutionTool20260521 = t.OfCodeExecutionTool20260521
		case t.OfMemoryTool20250818 != nil:
			c.OfMemoryTool20250818 = t.OfMemoryTool20250818
		case t.OfTextEditor20250124 != nil:
			c.OfTextEditor20250124 = t.OfTextEditor20250124
		case t.OfTextEditor20250429 != nil:
			c.OfTextEditor20250429 = t.OfTextEditor20250429
		case t.OfTextEditor20250728 != nil:
			c.OfTextEditor20250728 = t.OfTextEditor20250728
		case t.OfWebSearchTool20250305 != nil:
			c.OfWebSearchTool20250305 = t.OfWebSearchTool20250305
		case t.OfWebFetchTool20250910 != nil:
			c.OfWebFetchTool20250910 = t.OfWebFetchTool20250910
		case t.OfWebSearchTool20260209 != nil:
			c.OfWebSearchTool20260209 = t.OfWebSearchTool20260209
		case t.OfWebFetchTool20260209 != nil:
			c.OfWebFetchTool20260209 = t.OfWebFetchTool20260209
		case t.OfWebFetchTool20260309 != nil:
			c.OfWebFetchTool20260309 = t.OfWebFetchTool20260309
		case t.OfWebSearchTool20260318 != nil:
			c.OfWebSearchTool20260318 = t.OfWebSearchTool20260318
		case t.OfWebFetchTool20260318 != nil:
			c.OfWebFetchTool20260318 = t.OfWebFetchTool20260318
		case t.OfToolSearchToolBm25_20251119 != nil:
			c.OfToolSearchToolBm25_20251119 = t.OfToolSearchToolBm25_20251119
		case t.OfToolSearchToolRegex20251119 != nil:
			c.OfToolSearchToolRegex20251119 = t.OfToolSearchToolRegex20251119
		default:
			// Nothing populated, or a variant newer than this build. Dropped rather than
			// guessed at; see the doc above.
			continue
		}
		out = append(out, c)
	}
	return out
}

// requestMetadata records the safe, non-content facts about a request alongside the
// caller's own metadata.
//
// Every value here is either a number, an enum, or an identifier Anthropic defined.
// Nothing derived from prompt or response text appears, and the caller's map is copied
// rather than written to, since it belongs to them.
//
// The service tier is recorded because it is a real fact about how the request was
// served even though it selects no price -- see tierOf -- and because an organization
// with a priority capacity contract needs it to reconcile throttle's figures against
// their own invoice.
func requestMetadata(caller map[string]string, p params) map[string]string {
	if len(caller) == 0 && p.serviceTier == "" && p.inferenceGeo == "" {
		return nil
	}
	out := make(map[string]string, len(caller)+2)
	for k, v := range caller {
		out[k] = v
	}
	if p.serviceTier != "" {
		out["anthropic.requested_service_tier"] = p.serviceTier
	}
	if p.inferenceGeo != "" {
		out["anthropic.requested_inference_geo"] = p.inferenceGeo
	}
	return out
}
