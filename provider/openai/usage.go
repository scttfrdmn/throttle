package openai

import (
	"errors"
	"fmt"

	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/usage"
)

// ErrUsageInconsistent reports that a response's usage object contradicts itself.
//
// It exists because the decomposition below is arithmetic on numbers the provider
// supplied, and arithmetic can come out impossible: cached plus cache-write tokens
// exceeding the input total, or reasoning tokens exceeding output. Clamping to zero
// would silently discard real tokens; trusting the subtraction would record a
// negative count. Both are worse than saying the figures cannot be reconciled and
// letting the unknown-cost path handle it.
var ErrUsageInconsistent = errors.New("openai: reported usage is internally inconsistent")

// normalizeUsage converts an OpenAI Responses usage object into throttle's
// provider-neutral dimensions.
//
// # Why this is subtractive
//
// throttle's dimensions are disjoint by construction -- each carries its own price
// and they are summed -- while OpenAI's usage object is inclusive. OpenAI documents
// input_tokens as the total "including cached and cache-write tokens", and its own
// worked example confirms the arithmetic: 1000 input = 400 cached + 100 written +
// 500 uncached. Reasoning is inclusive the same way: max_output_tokens is defined as
// bounding "visible output tokens and reasoning tokens" together, and reasoning is
// billed at the output rate.
//
// So the mapping subtracts rather than copies. Copying input_tokens into InputTokens
// while also recording CachedTokens would charge the cached and written tokens twice
// -- once at the full input rate inside the total, once at their own rate -- and the
// cheaper cached rate makes the error an overcharge that looks plausible. The tests
// pin figures where the two readings differ visibly.
//
// # What each dimension means afterwards
//
//	InputTokens      fresh, uncached input, billed at the input rate
//	CacheReadTokens  input served from cache, billed at the cached rate
//	CacheWriteTokens input written to cache, dearer than fresh on models that charge it
//	OutputTokens     visible output only, reasoning excluded
//	ReasoningTokens  reasoning, priced at the output rate by the catalog
//
// TotalTokens is discarded. It sums dimensions carrying four different prices, so
// costing it would be wrong in the ordinary case, not just the edge case.
//
// # Absent is not zero
//
// The SDK reports usage as a value type, so a zero field is ambiguous on its face.
// Presence is read from the JSON metadata instead: a response that never mentioned
// cached tokens records no cache dimension at all, rather than a zero that would
// imply the provider priced one at nothing.
func normalizeUsage(u responses.ResponseUsage) (usage.Usage, error) {
	var out usage.Usage

	// A usage object that arrived with neither token total is not a usage object.
	// This is how an absent `usage` on an incomplete or failed response is detected:
	// the field is a struct, so it is never nil, and the zero value would otherwise
	// look like a free request.
	if !u.JSON.InputTokens.Valid() && !u.JSON.OutputTokens.Valid() {
		return out, nil
	}

	if u.JSON.InputTokens.Valid() {
		cached, hasCached := detail(u.JSON.InputTokensDetails, u.InputTokensDetails.JSON.CachedTokens, u.InputTokensDetails.CachedTokens)
		written, hasWritten := detail(u.JSON.InputTokensDetails, u.InputTokensDetails.JSON.CacheWriteTokens, u.InputTokensDetails.CacheWriteTokens)

		fresh := u.InputTokens - cached - written
		if fresh < 0 {
			return usage.Usage{}, fmt.Errorf("%w: input_tokens is %d but its breakdown reports %d cached and %d written, which cannot both be included in it",
				ErrUsageInconsistent, u.InputTokens, cached, written)
		}
		out.Set(usage.InputTokens, fresh)
		if hasCached {
			out.Set(usage.CacheReadTokens, cached)
		}
		if hasWritten {
			out.Set(usage.CacheWriteTokens, written)
		}
	}

	if u.JSON.OutputTokens.Valid() {
		reasoning, hasReasoning := detail(u.JSON.OutputTokensDetails, u.OutputTokensDetails.JSON.ReasoningTokens, u.OutputTokensDetails.ReasoningTokens)

		visible := u.OutputTokens - reasoning
		if visible < 0 {
			return usage.Usage{}, fmt.Errorf("%w: output_tokens is %d but its breakdown reports %d reasoning tokens, which cannot be included in it",
				ErrUsageInconsistent, u.OutputTokens, reasoning)
		}
		out.Set(usage.OutputTokens, visible)
		if hasReasoning {
			out.Set(usage.ReasoningTokens, reasoning)
		}
	}

	return out, nil
}

// detail reads one field out of a usage breakdown, reporting whether it was really
// there.
//
// Both the parent object and the field itself have to be present: a response that
// omitted input_tokens_details entirely mentioned no cached tokens, and neither did
// one that sent the object without the field. Either way the dimension is absent
// rather than zero, which is what keeps a model with no prompt caching from
// recording a cache dimension it was never billed for.
func detail(parent, field interface {
	Valid() bool
}, n int64) (int64, bool) {
	if !parent.Valid() || !field.Valid() {
		return 0, false
	}
	return n, true
}

// hasUsage reports whether a response carried usage figures at all.
//
// OpenAI does not list usage among a Response's required fields, and does not
// document whether a failed or incomplete response carries it. So its absence is a
// case the lifecycle has to handle rather than an anomaly, and this is the check
// that distinguishes "reported nothing" from "reported zero".
func hasUsage(r *responses.Response) bool {
	if r == nil {
		return false
	}
	return r.JSON.Usage.Valid() &&
		(r.Usage.JSON.InputTokens.Valid() || r.Usage.JSON.OutputTokens.Valid())
}
