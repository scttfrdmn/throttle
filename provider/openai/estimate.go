package openai

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/openai/openai-go/v3/responses"
)

// Heuristic constants for estimating input tokens without a count from OpenAI.
//
// Rough by construction. They exist so a client that has not enabled the counter
// can still reserve something sensible, and every estimate built from them is
// labelled QualityHeuristic so a caller knows not to treat it as a bound.
const (
	// bytesPerToken is the low end of the common BPE range of roughly 3.5-4.5 bytes
	// per token. The low end deliberately: overestimating input reserves too much,
	// which is recoverable, while underestimating admits work the budget cannot
	// afford.
	bytesPerToken = 3

	// perItemOverhead accounts for role markers and item framing, which every
	// provider adds and none bills as zero.
	perItemOverhead = 4

	// nonTextItemTokens stands in for an item whose token cost is not a function of
	// anything measurable here -- an image, a file, an audio clip. It is
	// intentionally crude: a real count requires OpenAI's own counter, which is
	// exactly what Config.Counter is for.
	nonTextItemTokens = 1000
)

// estimateInputTokens approximates a request's input tokens from the length of its
// content.
//
// A fallback, not an accounting path. It measures content without inspecting it and
// does not try to be clever: a caller who needs an accurate count enables the
// counter.
func estimateInputTokens(p params) int64 {
	var n int64

	if p.instructions != "" {
		n += perItemOverhead + textTokens(p.instructions)
	}

	// The plain-string form of input: one message, measured directly.
	if p.inputText != "" {
		n += perItemOverhead + textTokens(p.inputText)
	}

	for _, item := range p.inputItems {
		n += perItemOverhead + inputItemTokens(item)
	}

	if len(p.tools) > 0 {
		// Tool schemas are sent with the request and billed as input. Their JSON is not
		// reachable as text without serializing SDK param objects, so each tool takes a
		// flat approximation -- and this is a large part of why the counter is worth
		// enabling: it counts the real schemas.
		n += int64(len(p.tools)) * nonTextItemTokens / 4
	}

	return n
}

// inputItemTokens measures one input item.
//
// The SDK's input item union has many variants and this reads only the text-bearing
// ones. Everything else takes the non-text placeholder, which is the honest handling
// of an image or a file: its cost is not derivable from anything visible here.
func inputItemTokens(item responses.ResponseInputItemUnionParam) int64 {
	switch {
	case item.OfMessage != nil:
		// The easy form's content is itself a union: a bare string or a content list.
		c := item.OfMessage.Content
		if c.OfString.Valid() {
			return textTokens(c.OfString.Value)
		}
		return contentListTokens(c.OfInputItemContentList)
	case item.OfInputMessage != nil:
		return contentListTokens(item.OfInputMessage.Content)
	case item.OfFunctionCallOutput != nil:
		// A tool result the caller is feeding back in. Billed as input like any other
		// content.
		return textTokens(item.OfFunctionCallOutput.Output.OfString.Or(""))
	case item.OfCustomToolCallOutput != nil:
		return textTokens(item.OfCustomToolCallOutput.Output.OfString.Or(""))
	case item.OfReasoning != nil:
		// Reasoning items carry an encrypted or summarized payload rather than
		// measurable text, and throttle does not read reasoning content in any case.
		return perItemOverhead
	default:
		return nonTextItemTokens
	}
}

// contentListTokens measures a message's content list.
func contentListTokens(content responses.ResponseInputMessageContentListParam) int64 {
	var n int64
	for _, c := range content {
		switch {
		case c.OfInputText != nil:
			n += textTokens(c.OfInputText.Text)
		default:
			// An image, file, or audio part.
			n += nonTextItemTokens
		}
	}
	return n
}

func textTokens(s string) int64 {
	if s == "" {
		return 0
	}
	// Round up: a fragment shorter than one token's worth of bytes still costs a
	// token.
	return (int64(len(s)) + bytesPerToken - 1) / bytesPerToken
}

// newRequestID generates an identifier for a call the caller did not name. Random
// rather than sequential so two processes sharing a ledger cannot collide, which
// would make one request's retry look like another's.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "openai-" + hex.EncodeToString(b[:]), nil
}
