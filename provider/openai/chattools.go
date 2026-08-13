package openai

import (
	"sort"

	oai "github.com/openai/openai-go/v3"

	"github.com/scttfrdmn/throttle/pricing"
)

// classifyChatRequest reports what a Chat Completions request will not be able to
// account for.
//
// # Why the tool list is almost never the answer here
//
// Chat Completions has only two tool variants, function and custom, and both are the
// caller's own code. OpenAI bills the schema and the arguments as tokens and nothing
// else; the execution happens on the caller's machine and is theirs to pay for.
// Assigning it an OpenAI dollar charge would invent one -- the same conclusion tools.go
// reaches for the Responses equivalents, reached through the same toolBilling
// vocabulary rather than a second one.
//
// The hosted tools that make a Responses request unpriceable are simply not available
// in this API family. Web search is the exception and it is not a tool here at all: it
// is the web_search_options request field, billed per call on OpenAI's pricing page and
// invisible in the response. A classifier that only walked the tool list would report
// such a request as fully priced, which is the specific mistake this function exists to
// avoid.
//
// # Audio
//
// Audio exposure is folded in here because it belongs to the same question -- what part
// of this request's bill can throttle name -- and because it depends on the captured
// quote, which admission has by this point. See audio.go.
func classifyChatRequest(p chatParams, quote pricing.CapturedQuote) exposure {
	var e exposure

	if p.webSearch {
		// Named as the request field rather than as "web_search", so an operator reading
		// the reason string can find it in their own code.
		e.tools = append(e.tools, "web_search_options")
	}

	// Any tool type this build does not recognize is treated as unaccountable, for the
	// same reason as in Responses: OpenAI adds billable capabilities, and a throttle that
	// silently called such a request fully priced would understate real spend with no
	// indication anything was missed. The zero value of toolBilling is billedInTokens, so
	// the lookup's second result is what carries the weight.
	seen := map[string]bool{}
	for _, t := range p.tools {
		name := chatToolType(t)
		if name == "" {
			// A tool union with no variant set carries no type to classify. OpenAI will
			// reject the request, so there is no charge to account for and nothing to flag.
			continue
		}
		billing, known := toolCharges[name]
		if !known || billing == billedOutsideUsage {
			seen[name] = true
		}
	}
	for n := range seen {
		e.tools = append(e.tools, n)
	}
	sort.Strings(e.tools)

	e.audio = audioExposure(p, quote)
	return e
}

// chatToolType reports the discriminator of a Chat Completions tool param union.
//
// Read through Default() for the same reason toolType does: the SDK declares Type as a
// single-valued constant whose zero value is the empty string and fills it in while
// marshalling, so a caller who writes the idiomatic struct literal leaves it empty.
// Reading the field directly would classify such a tool as having no type -- which for
// an unrecognized tool would be the one classification that makes a request throttle
// cannot fully price look fully priced.
func chatToolType(t oai.ChatCompletionToolUnionParam) string {
	switch {
	case t.OfFunction != nil:
		return string(t.OfFunction.Type.Default())
	case t.OfCustom != nil:
		return string(t.OfCustom.Type.Default())
	default:
		return ""
	}
}
