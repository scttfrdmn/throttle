package openai

import (
	"fmt"

	oai "github.com/openai/openai-go/v3"

	"github.com/scttfrdmn/throttle/usage"
)

// normalizeChatUsage converts an OpenAI Chat Completions usage object into throttle's
// provider-neutral dimensions.
//
// # Why this is a separate function from normalizeUsage
//
// The two usage objects look like renamings of each other: prompt_tokens for
// input_tokens, completion_tokens for output_tokens, the same cached and reasoning
// details underneath. Reusing the Responses normalizer's formula here would be the
// natural move and it would be wrong, because two of the four detailed counters in
// this object stand in a different relationship to the total than the Responses
// counters do. Field names that look alike are not evidence of billing equivalence,
// and this is the function where that stops being an abstract point.
//
// # What is subtracted, and why
//
// Subtraction is right for a detail that is included in its parent total *and* carries
// a different rate. Both conditions have to hold. If a detail is included and carries
// the same rate, subtracting it and pricing it separately gives the same answer but
// invents a dimension; if it is included and throttle subtracts it without pricing it,
// the charge silently drops. If it is not included at all, subtracting it undercharges
// outright.
//
//	cached_tokens        subtracted from prompt_tokens.     Included, own (cheaper) rate.
//	cache_write_tokens   subtracted from prompt_tokens.     Included, own (dearer) rate.
//	prompt audio_tokens  subtracted from prompt_tokens.     Included, own (much dearer) rate.
//	reasoning_tokens     subtracted from completion_tokens. Included, priced at output rate,
//	                     kept distinct because the catalog prices it as its own dimension
//	                     and a reader needs to see how much of the output was reasoning.
//	completion audio_tokens  subtracted from completion_tokens. Included, own rate.
//
// # What is deliberately not subtracted
//
// accepted_prediction_tokens and rejected_prediction_tokens are *not* subtracted, and
// get no dimension of their own.
//
// They are a breakdown of completion_tokens, like reasoning tokens -- but unlike
// reasoning tokens they carry no distinct rate. OpenAI is explicit that rejected
// prediction tokens "are still counted in the total completion tokens for purposes of
// billing", and its predicted-outputs guide states that tokens provided which are not
// part of the final completion "are still charged at completion token rates". So the
// billing primitive remains the authoritative completion-token amount.
//
// Subtracting them would understate the charge by exactly the rejected count, which on
// a request using predicted outputs badly can be most of the generation -- the failure
// mode is largest precisely when the feature is working least well. Giving them their
// own dimension instead would be harmless arithmetically but would assert a rate
// distinction that does not exist, and every catalog would then have to carry two rates
// that must be kept equal to the output rate forever. Neither is preferable to reading
// the counters and not acting on them.
//
// A test pins this directly: a large rejected-prediction count must not lower the
// charged completion amount.
//
// # What is discarded
//
// total_tokens. It sums dimensions carrying up to six different prices, so pricing from
// it would be wrong in the ordinary case rather than the edge case.
//
// # Absent is not zero
//
// Presence is read from the SDK's JSON metadata, exactly as in normalizeUsage. A
// response that never mentioned cached tokens records no cache dimension, rather than a
// zero that would imply the provider priced one at nothing. This matters more here than
// in Responses: this usage object carries five optional details, and a text model
// reports none of the audio ones.
func normalizeChatUsage(u oai.CompletionUsage) (usage.Usage, error) {
	var out usage.Usage

	// A usage object that arrived with neither token total is not a usage object. The
	// field is a struct, so it is never nil, and the zero value would otherwise look like
	// a free request.
	if !u.JSON.PromptTokens.Valid() && !u.JSON.CompletionTokens.Valid() {
		return out, nil
	}

	if u.JSON.PromptTokens.Valid() {
		d := u.PromptTokensDetails
		cached, hasCached := detail(u.JSON.PromptTokensDetails, d.JSON.CachedTokens, d.CachedTokens)
		written, hasWritten := detail(u.JSON.PromptTokensDetails, d.JSON.CacheWriteTokens, d.CacheWriteTokens)
		audio, hasAudio := detail(u.JSON.PromptTokensDetails, d.JSON.AudioTokens, d.AudioTokens)

		text := u.PromptTokens - cached - written - audio
		if text < 0 {
			return usage.Usage{}, fmt.Errorf("%w: prompt_tokens is %d but its breakdown reports %d cached, %d written, and %d audio, which cannot all be included in it",
				ErrUsageInconsistent, u.PromptTokens, cached, written, audio)
		}
		out.Set(usage.InputTokens, text)
		if hasCached {
			out.Set(usage.CacheReadTokens, cached)
		}
		if hasWritten {
			out.Set(usage.CacheWriteTokens, written)
		}
		if hasAudio {
			out.Set(usage.InputAudioTokens, audio)
		}
	}

	if u.JSON.CompletionTokens.Valid() {
		d := u.CompletionTokensDetails
		reasoning, hasReasoning := detail(u.JSON.CompletionTokensDetails, d.JSON.ReasoningTokens, d.ReasoningTokens)
		audio, hasAudio := detail(u.JSON.CompletionTokensDetails, d.JSON.AudioTokens, d.AudioTokens)

		// Prediction counters are read and not subtracted; see the comment above. They are
		// inside this total and billed at the completion rate, so they stay inside the text
		// figure rather than being carved out of it.
		text := u.CompletionTokens - reasoning - audio
		if text < 0 {
			return usage.Usage{}, fmt.Errorf("%w: completion_tokens is %d but its breakdown reports %d reasoning and %d audio, which cannot both be included in it",
				ErrUsageInconsistent, u.CompletionTokens, reasoning, audio)
		}
		out.Set(usage.OutputTokens, text)
		if hasReasoning {
			out.Set(usage.ReasoningTokens, reasoning)
		}
		if hasAudio {
			out.Set(usage.OutputAudioTokens, audio)
		}
	}

	return out, nil
}

// hasChatUsage reports whether a completion carried usage figures at all.
//
// OpenAI does not document usage as guaranteed on every completion, and a request that
// failed partway may omit it. So its absence is a case the lifecycle handles rather
// than an anomaly, and this is the check that distinguishes "reported nothing" from
// "reported zero".
func hasChatUsage(c *oai.ChatCompletion) bool {
	if c == nil {
		return false
	}
	return c.JSON.Usage.Valid() &&
		(c.Usage.JSON.PromptTokens.Valid() || c.Usage.JSON.CompletionTokens.Valid())
}
