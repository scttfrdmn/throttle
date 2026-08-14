package anthropic

import (
	"crypto/rand"
	"encoding/hex"

	anth "github.com/anthropics/anthropic-sdk-go"
)

// Heuristic constants for estimating input tokens without a count from Anthropic.
//
// Rough by construction. They exist so a client that has not enabled the counter can
// still reserve something sensible, and every estimate built from them is labelled
// QualityHeuristic so a caller knows not to treat it as a bound.
const (
	// bytesPerToken is the low end of the common BPE range of roughly 3.5-4.5 bytes per
	// token. The low end deliberately: overestimating input reserves too much, which is
	// recoverable, while underestimating admits work the budget cannot afford.
	//
	// Anthropic's newer models tokenize more finely still -- the documentation notes that
	// Claude 4.7 and later produce roughly 30% more tokens for the same text than earlier
	// models did -- which is another reason this errs low and another reason the counter
	// is worth enabling.
	bytesPerToken = 3

	// perBlockOverhead accounts for role markers and block framing, which every provider
	// adds and none bills as zero.
	perBlockOverhead = 4

	// nonTextBlockTokens stands in for a block whose token cost is not a function of
	// anything measurable here -- an image, a PDF, a search result. It is intentionally
	// crude: a real count requires Anthropic's own counter, which is exactly what
	// Config.Counter is for.
	nonTextBlockTokens = 1000
)

// estimateInputTokens approximates a request's input tokens from the length of its
// content.
//
// A fallback, not an accounting path. It measures content without inspecting it, reads
// no block it does not have to, and retains nothing: the return value is a number.
func estimateInputTokens(p params) int64 {
	var n int64

	for _, s := range p.system {
		n += perBlockOverhead + textTokens(s.Text)
	}

	for _, m := range p.messages {
		n += perBlockOverhead
		for _, b := range m.Content {
			n += contentBlockTokens(b)
		}
	}

	if len(p.tools) > 0 {
		// Tool schemas are sent with the request and billed as input. Their JSON is not
		// reachable as text without serializing SDK param objects, so each tool takes a
		// flat approximation -- and this is a large part of why the counter is worth
		// enabling: it counts the real schemas.
		n += int64(len(p.tools)) * nonTextBlockTokens / 4
	}

	return n
}

// contentBlockTokens measures one content block.
//
// The union has seventeen variants and this reads the text-bearing ones. Everything else
// takes the non-text placeholder, which is the honest handling of an image or a PDF: its
// cost is not derivable from anything visible here.
//
// Thinking blocks a caller feeds back are measured by their length like any other input,
// because that is what they cost as input. Nothing about the reasoning inside them is
// read or kept.
func contentBlockTokens(b anth.ContentBlockParamUnion) int64 {
	switch {
	case b.OfText != nil:
		return textTokens(b.OfText.Text)
	case b.OfThinking != nil:
		return textTokens(b.OfThinking.Thinking)
	case b.OfRedactedThinking != nil:
		return textTokens(b.OfRedactedThinking.Data)
	case b.OfToolUse != nil:
		// The arguments are an arbitrary any, so their serialized length is not reachable
		// without marshalling the caller's data. A flat allowance instead: measuring it
		// would mean handling tool arguments, and throttle has no reason to touch those.
		return nonTextBlockTokens / 4
	case b.OfToolResult != nil:
		return toolResultTokens(b.OfToolResult)
	case b.OfSearchResult != nil:
		var n int64
		for _, t := range b.OfSearchResult.Content {
			n += textTokens(t.Text)
		}
		return n
	case b.OfMidConvSystem != nil:
		var n int64
		for _, t := range b.OfMidConvSystem.Content {
			n += textTokens(t.Text)
		}
		return n
	default:
		// An image, a document, a container upload, a server-tool result. Not measurable
		// from here.
		return nonTextBlockTokens
	}
}

// toolResultTokens measures a tool result the caller is feeding back in.
//
// Billed as ordinary input, like any other content. Its own content is a nested union;
// only the text arm is measurable, and the others take the placeholder.
func toolResultTokens(r *anth.ToolResultBlockParam) int64 {
	var n int64
	for _, c := range r.Content {
		switch {
		case c.OfText != nil:
			n += textTokens(c.OfText.Text)
		case c.OfSearchResult != nil:
			for _, t := range c.OfSearchResult.Content {
				n += textTokens(t.Text)
			}
		default:
			n += nonTextBlockTokens
		}
	}
	return n
}

func textTokens(s string) int64 {
	if s == "" {
		return 0
	}
	// Round up: a fragment shorter than one token's worth of bytes still costs a token.
	return (int64(len(s)) + bytesPerToken - 1) / bytesPerToken
}

// newRequestID generates an identifier for a call the caller did not name. Random rather
// than sequential so two processes sharing a ledger cannot collide, which would make one
// request's retry look like another's.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "anthropic-" + hex.EncodeToString(b[:]), nil
}
