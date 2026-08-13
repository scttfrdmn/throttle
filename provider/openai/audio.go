package openai

import (
	"sort"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"

	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// Audio in Chat Completions, and why it is treated as an unpriced exposure.
//
// # Audio is a distinct billing dimension, not a variety of text
//
// OpenAI's audio-capable Chat Completions models bill audio in *tokens*, reported in
// the usage object's own audio_tokens counters, at rates several times the text rates
// in each direction -- on gpt-audio, $32.00/M input and $64.00/M output against
// $2.50/M and $10.00/M for text, so roughly 6x and 13x. Folding those tokens into
// InputTokens and OutputTokens would therefore undercharge an audio request by an
// order of magnitude, silently, on a request that looks entirely ordinary.
//
// They are tokens and not seconds. usage.AudioSeconds exists for models genuinely
// documented as duration-billed, and using it here would mean converting a count the
// provider reported into a duration it did not -- inventing a measurement to fit a
// rate that does not apply. So Chat Completions audio normalizes into
// usage.InputAudioTokens and usage.OutputAudioTokens, which carry their own rates for
// exactly the reason CacheReadTokens does.
//
// # Why an audio request is currently an incomplete cost
//
// The price fixtures throttle ships carry no audio-token rates: the models they cover
// are text models, and adding an audio-model pricing sheet is its own piece of work
// with its own provenance. Until those rates exist, throttle cannot state the complete
// monetary exposure of a request that may consume or produce audio.
//
// The handling follows from that rather than from a policy choice. Under enforce, such
// a request is denied before OpenAI is called, because admitting it would authorize
// spend whose size throttle does not know -- and the existing admission gate already
// does this, since an estimate whose cost is not Known is what enforce refuses. Under
// monitor it is admitted with a zero hold under the same unknown-cost semantics every
// other unpriceable request uses. Neither path invents an exposure figure to
// manufacture a reservation.
//
// # Why this needs no new machinery
//
// Adding authoritative per-model input and output audio-token rates to a catalog is
// sufficient to make audio requests fully priced: hasAudioRates then finds them, the
// exposure is complete, the captured quote prices the audio dimensions like any other,
// and nothing in the lifecycle changes. That is the property this design exists to
// preserve.

// audioDimensions are the dimensions an audio-capable request can consume.
//
// Both directions, always, regardless of which one the request obviously involves.
// Audio input with a text-only response still bills input audio tokens, and a
// text-prompted audio response still bills output audio tokens -- and a request that
// asked for audio output may also be handed audio input on a later turn of the same
// conversation. Naming both is the conservative reading, and the reason string names
// only what is genuinely missing.
var audioDimensions = []usage.Dimension{usage.InputAudioTokens, usage.OutputAudioTokens}

// audioExposure reports the audio dimensions this request could consume that the
// captured quote holds no rate for.
//
// Returns nil in the two cases that need no flag: the request cannot involve audio at
// all, or the quote prices both audio dimensions. A quote that prices one direction
// but not the other names only the direction it cannot price, since that is the only
// part that is actually unknown.
func audioExposure(p chatParams, quote pricing.CapturedQuote) []usage.Dimension {
	if !p.carriesAudio {
		return nil
	}
	var out []usage.Dimension
	for _, d := range audioDimensions {
		if _, ok := quote.Rate(d); !ok {
			out = append(out, d)
		}
	}
	sortDimensions(out)
	return out
}

// carriesAudio reports whether a Chat Completions request can consume or produce audio
// tokens.
//
// Three independent signals, because a caller can reach audio three ways and any one
// of them is enough to change the bill:
//
//   - the audio param is set, which requests an audio response and is what OpenAI's
//     own guide tells a caller to send;
//   - modalities names audio, which is how output modality is selected;
//   - a message carries an input_audio content part, or an assistant turn references a
//     previous audio response, either of which bills input audio tokens.
//
// Checked from the request rather than the response, because this decides admission and
// admission happens before there is a response. A false negative here would admit a
// request under enforce whose exposure throttle cannot bound, so each signal is read
// independently rather than inferred from the model name -- a model ID this build has
// never seen must still be handled correctly, and audio capability is not derivable
// from a string.
func carriesAudio(in oai.ChatCompletionNewParams) bool {
	if !param.IsOmitted(in.Audio) {
		return true
	}
	for _, m := range in.Modalities {
		if m == "audio" {
			return true
		}
	}
	for _, msg := range in.Messages {
		if messageCarriesAudio(msg) {
			return true
		}
	}
	return false
}

// messageCarriesAudio reports whether one message carries audio.
//
// Only two of the six message variants can: a user message with an input_audio content
// part, and an assistant message referencing a previous audio response. The others --
// developer, system, tool, function -- take text content only, by the SDK's own types,
// so there is nothing to check rather than nothing checked.
func messageCarriesAudio(msg oai.ChatCompletionMessageParamUnion) bool {
	if u := msg.OfUser; u != nil {
		for _, part := range u.Content.OfArrayOfContentParts {
			if part.OfInputAudio != nil {
				return true
			}
		}
	}
	if a := msg.OfAssistant; a != nil {
		// A prior audio response fed back in. It is content the model re-reads, so it is
		// billed, and the reference carries no counters to say how much.
		if !param.IsOmitted(a.Audio) {
			return true
		}
	}
	return false
}

// sortDimensions orders dimensions by name, in place.
//
// Shared by every place this package builds a dimension list, because usage.Cost's
// constructors sort Unpriced and a caller comparing two records' unpriced sets depends
// on one ordering rather than two.
func sortDimensions(dims []usage.Dimension) {
	sort.Slice(dims, func(i, j int) bool { return dims[i] < dims[j] })
}
