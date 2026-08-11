package bedrock

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// Heuristic constants for estimating input tokens without a tokenizer.
//
// These are rough by construction. They exist so that a client without
// Config.Counter can still reserve something sensible, and every estimate built
// from them is labelled QualityHeuristic so a caller knows not to trust it as a
// bound.
const (
	// bytesPerToken is a conservative average for English prose across the common
	// BPE tokenizers, which sit around 3.5-4.5 bytes per token. The low end is used
	// deliberately: overestimating input reserves too much, which is recoverable,
	// while underestimating admits work the budget cannot afford.
	bytesPerToken = 3

	// perMessageOverhead accounts for role markers and message framing, which every
	// provider adds and none of them bills as zero.
	perMessageOverhead = 4

	// nonTextBlockTokens is a placeholder for a block whose token cost cannot be
	// derived from its byte length -- an image, a document, an audio clip. It is
	// intentionally crude: a real count requires the provider's tokenizer, which is
	// exactly what Config.Counter is for.
	nonTextBlockTokens = 1000
)

// estimateInputTokens approximates the input tokens of a request from the length
// of its content.
//
// This is a fallback, not an accounting path. It never sees prompt content beyond
// measuring it, and it does not attempt to be clever: a caller who needs an
// accurate count enables CountTokens.
//
// It takes params rather than an SDK request so that a streaming and a
// non-streaming request with the same messages are estimated identically.
func estimateInputTokens(p params) int64 {
	var n int64
	for _, m := range p.messages {
		n += perMessageOverhead
		for _, b := range m.Content {
			n += contentBlockTokens(b)
		}
	}
	for _, s := range p.system {
		switch v := s.(type) {
		case *types.SystemContentBlockMemberText:
			n += textTokens(v.Value)
		default:
			// A guardrail or cache-point system block carries little or no billable
			// text of its own.
			n += perMessageOverhead
		}
	}
	if p.toolConfig != nil {
		// Tool schemas are sent with the request and billed as input. Their JSON is
		// not reachable as text here without serializing a document.Interface, so
		// each tool is charged a flat approximation.
		n += int64(len(p.toolConfig.Tools)) * nonTextBlockTokens / 4
	}
	return n
}

func contentBlockTokens(b types.ContentBlock) int64 {
	switch v := b.(type) {
	case *types.ContentBlockMemberText:
		return textTokens(v.Value)
	case *types.ContentBlockMemberToolResult:
		var n int64
		for _, rc := range v.Value.Content {
			if t, ok := rc.(*types.ToolResultContentBlockMemberText); ok {
				n += textTokens(t.Value)
				continue
			}
			n += nonTextBlockTokens
		}
		return n
	case *types.ContentBlockMemberToolUse:
		return perMessageOverhead
	case *types.ContentBlockMemberReasoningContent:
		return perMessageOverhead
	case *types.ContentBlockMemberCachePoint:
		return 0
	case nil:
		return 0
	default:
		// An image, document, video, audio, or search-result block. Its token cost is
		// not a function of anything measurable here, so it gets the placeholder.
		return nonTextBlockTokens
	}
}

func textTokens(s string) int64 {
	if s == "" {
		return 0
	}
	// Round up: a fragment shorter than one token's worth of bytes still costs a
	// token.
	return (int64(len(s)) + bytesPerToken - 1) / bytesPerToken
}

// newRequestID generates an identifier for a call the caller did not name. It is
// random rather than sequential so that two processes sharing a ledger cannot
// collide, which would make one request's retry look like another's.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "bedrock-" + hex.EncodeToString(b[:]), nil
}
