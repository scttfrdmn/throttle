package openai

import (
	oai "github.com/openai/openai-go/v3"
)

// estimateChatInputTokens approximates a Chat Completions request's input tokens from
// the length of its content.
//
// A fallback in the Responses adapter; the only option here. OpenAI publishes no
// input-token counting endpoint for this API family, so there is no better number to
// prefer -- which is worth knowing when choosing between the two APIs, and is why every
// Chat Completions estimate is labelled QualityHeuristic.
//
// It reuses the same constants as the Responses heuristic. Not for tidiness: they encode
// the same two facts, that a token is roughly three or more bytes and that message
// framing is never free, and both facts are about the tokenizer rather than about an
// endpoint. Diverging them would imply throttle knows something about one API's
// tokenization that it does not know about the other's.
func estimateChatInputTokens(p chatParams) int64 {
	var n int64
	for _, m := range p.messages {
		n += perItemOverhead + chatMessageTokens(m)
	}
	if len(p.tools) > 0 {
		// Tool schemas are sent with the request and billed as input. Their JSON is not
		// reachable as text without serializing SDK param objects, so each tool takes a flat
		// approximation. There is no counter to fall back on here, so this is as good as the
		// input estimate for a tool-using request gets.
		n += int64(len(p.tools)) * nonTextItemTokens / 4
	}
	return n
}

// chatMessageTokens measures one message across the six role variants.
//
// Every variant is handled rather than defaulted, because each nests its content
// differently and a missed variant would silently contribute nothing -- understating the
// input of a request that uses it. The two deprecated variants are measured too: a real
// application that still sends them still pays for them.
func chatMessageTokens(m oai.ChatCompletionMessageParamUnion) int64 {
	switch {
	case m.OfUser != nil:
		c := m.OfUser.Content
		if c.OfString.Valid() {
			return textTokens(c.OfString.Value)
		}
		return chatContentPartTokens(c.OfArrayOfContentParts)
	case m.OfSystem != nil:
		c := m.OfSystem.Content
		if c.OfString.Valid() {
			return textTokens(c.OfString.Value)
		}
		return chatTextPartTokens(c.OfArrayOfContentParts)
	case m.OfDeveloper != nil:
		c := m.OfDeveloper.Content
		if c.OfString.Valid() {
			return textTokens(c.OfString.Value)
		}
		return chatTextPartTokens(c.OfArrayOfContentParts)
	case m.OfAssistant != nil:
		return chatAssistantTokens(m.OfAssistant)
	case m.OfTool != nil:
		// A tool result the caller is feeding back in. Billed as input like any other
		// content, and frequently the largest message in an agent loop.
		c := m.OfTool.Content
		if c.OfString.Valid() {
			return textTokens(c.OfString.Value)
		}
		return chatTextPartTokens(c.OfArrayOfContentParts)
	case m.OfFunction != nil:
		return textTokens(m.OfFunction.Content.Or(""))
	default:
		// A union with no variant set. OpenAI will reject the request, so there is nothing
		// to estimate.
		return 0
	}
}

// chatAssistantTokens measures a prior assistant turn.
//
// Its tool calls are measured by the length of their arguments because that is what was
// sent and therefore what is billed. Measured, not read: the arguments are model-
// generated text that may quote the prompt, so their length reaches the estimate and
// their content reaches nothing.
func chatAssistantTokens(a *oai.ChatCompletionAssistantMessageParam) int64 {
	var n int64
	if c := a.Content; c.OfString.Valid() {
		n += textTokens(c.OfString.Value)
	} else {
		for _, part := range c.OfArrayOfContentParts {
			switch {
			case part.OfText != nil:
				n += textTokens(part.OfText.Text)
			case part.OfRefusal != nil:
				n += textTokens(part.OfRefusal.Refusal)
			}
		}
	}
	for _, tc := range a.ToolCalls {
		switch {
		case tc.OfFunction != nil:
			n += perItemOverhead + textTokens(tc.OfFunction.Function.Name) + textTokens(tc.OfFunction.Function.Arguments)
		case tc.OfCustom != nil:
			n += perItemOverhead + textTokens(tc.OfCustom.Custom.Name) + textTokens(tc.OfCustom.Custom.Input)
		}
	}
	if fc := a.FunctionCall; fc.Name != "" || fc.Arguments != "" {
		n += perItemOverhead + textTokens(fc.Name) + textTokens(fc.Arguments)
	}
	if !isZeroAudioRef(a.Audio) {
		// A referenced prior audio response. Its token count is not derivable from an ID,
		// and it is audio rather than text -- so the placeholder stands in for its size
		// while the audio exposure handles the fact that its rate is unknown.
		n += nonTextItemTokens
	}
	return n
}

// chatContentPartTokens measures a user message's content parts.
func chatContentPartTokens(parts []oai.ChatCompletionContentPartUnionParam) int64 {
	var n int64
	for _, part := range parts {
		switch {
		case part.OfText != nil:
			n += textTokens(part.OfText.Text)
		default:
			// An image, an audio clip, or a file. None of their token cost is a function of
			// anything measurable here, so each takes the crude placeholder.
			n += nonTextItemTokens
		}
	}
	return n
}

// chatTextPartTokens measures the text-only content-part lists that four of the six
// message variants use.
func chatTextPartTokens(parts []oai.ChatCompletionContentPartTextParam) int64 {
	var n int64
	for _, part := range parts {
		n += textTokens(part.Text)
	}
	return n
}

// isZeroAudioRef reports whether an assistant message carries no prior-audio reference.
//
// The struct has no emptiness predicate of its own and its only field is the ID, so this
// checks that. Kept as a named function because the same question is asked in two places
// -- here for sizing and in messageCarriesAudio for exposure -- and the two must agree.
func isZeroAudioRef(a oai.ChatCompletionAssistantMessageParamAudio) bool { return a.ID == "" }
