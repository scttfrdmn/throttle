package openai

import (
	"sort"

	"github.com/openai/openai-go/v3/responses"
)

// toolBilling classifies what a tool means for throttle's ability to account for a
// request's full provider charge.
type toolBilling int

const (
	// billedInTokens means the tool's entire OpenAI charge appears in the response's
	// usage object. A request carrying only these tools can be fully priced.
	billedInTokens toolBilling = iota

	// billedOutsideUsage means OpenAI charges for this tool in a unit the usage
	// object cannot express -- per call, per GB-day, per container-session -- so the
	// token cost throttle computes is a floor rather than a total.
	billedOutsideUsage

	// billedByCaller means the work happens outside OpenAI: the caller's own
	// function, or a third party's MCP server. OpenAI bills the tokens, which are in
	// the usage object; whatever else it costs is not an OpenAI charge and throttle
	// assigns it nothing.
	billedByCaller
)

// toolCharges maps each tool type to how its charge is billed.
//
// # Why several of these are pessimistic
//
// OpenAI documents hosted-tool surcharges on its pricing page and nowhere in the
// API: web search at $10.00 per 1k calls, file search at $2.50 per 1k calls plus
// $0.10 per GB-day of storage, code interpreter and hosted shell containers at
// $0.03 to $1.92 per 20-minute session. None of it appears in a response. So for
// these tools the token figures throttle can see are demonstrably not the whole
// bill, and the honest outcome is a floor.
//
// For the rest -- computer use, image generation, programmatic tool calling,
// namespace, tool search, apply patch -- no separate charge could be verified in
// either direction. They are classified as billed outside usage anyway. An
// unverified "free" is the expensive mistake: it makes a request look fully priced
// when it may not be, and a floor that turns out to have been the total is merely
// conservative. Verifying one of these later moves it, and the direction of the
// error is the point.
//
// The two entries that are genuinely token-only are the caller's own tools, where
// there is nothing for OpenAI to charge beyond the tokens, and local_shell, which
// runs on the caller's machine -- as distinct from shell, which is the hosted
// variant that carries the container charge.
var toolCharges = map[string]toolBilling{
	// The caller's own code. OpenAI bills the schema and arguments as tokens and
	// nothing else; the execution is the caller's to pay for, and assigning it an
	// OpenAI dollar charge would invent one.
	"function": billedByCaller,
	"custom":   billedByCaller,

	// A third party's MCP server. OpenAI bills the tool definitions and results as
	// tokens, which the usage object reports in full. The server's own charges, if it
	// has any, are between the caller and that server -- not part of throttle's
	// provider bill, and not throttle's to invent.
	"mcp": billedByCaller,

	// Runs on the caller's machine: OpenAI emits the call, the caller executes it.
	// Token-only, unlike the hosted "shell".
	"local_shell": billedInTokens,

	// Documented per-call or per-resource charges, invisible to the response.
	"web_search":                    billedOutsideUsage,
	"web_search_2025_08_26":         billedOutsideUsage,
	"web_search_preview":            billedOutsideUsage,
	"web_search_preview_2025_03_11": billedOutsideUsage,
	"file_search":                   billedOutsideUsage,
	"code_interpreter":              billedOutsideUsage,
	"shell":                         billedOutsideUsage,

	// Unverified either way, so classified conservatively.
	"computer":                  billedOutsideUsage,
	"computer_use_preview":      billedOutsideUsage,
	"image_generation":          billedOutsideUsage,
	"programmatic_tool_calling": billedOutsideUsage,
	"namespace":                 billedOutsideUsage,
	"tool_search":               billedOutsideUsage,
	"apply_patch":               billedOutsideUsage,
}

// classifyTools examines a Responses request's tools and reports whether the response's
// usage object can account for the whole OpenAI charge.
//
// An unrecognized tool type counts as unaccounted. A tool this build has never heard
// of is exactly the case where assuming token-only would be wrong: OpenAI adds hosted
// tools with their own pricing, and a throttle that silently called such a request
// fully priced would understate real spend with no indication anything was missed.
//
// It returns the same exposure type the Chat Completions classifier does. Shared because
// the *conclusion* is provider-neutral -- a list of things whose charge cannot be derived
// from the reply -- while the two functions that reach it are not, since the two API
// families offer different tools through different union types and one of them expresses
// web search as a request field rather than a tool at all. Only the audio half is left
// empty here: the Responses adapter has no audio-bearing path in this slice.
func classifyTools(tools []responses.ToolUnionParam) exposure {
	seen := map[string]bool{}
	for _, t := range tools {
		name := toolType(t)
		if name == "" {
			// A tool union with no variant set carries no type to classify. It is a
			// malformed request that OpenAI will reject, so there is no charge to
			// account for and nothing to flag.
			continue
		}
		// The lookup's second result carries the weight here. The zero value of
		// toolBilling is billedInTokens, so an unrecognized tool type would otherwise
		// read as token-only -- the one classification an unknown tool must not get.
		billing, known := toolCharges[name]
		if !known || billing == billedOutsideUsage {
			seen[name] = true
		}
	}
	if len(seen) == 0 {
		return exposure{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return exposure{tools: out}
}

// toolType reports the discriminator of a tool param union.
//
// The SDK models the union as a struct of pointers, one per variant, so the type is
// read by finding the populated one.
//
// # Why Default() and not the field
//
// Most variants declare Type as a single-valued constant type whose zero value is the
// empty string; the SDK fills it in while marshalling, from the struct tag. So a caller
// who writes the idiomatic `&responses.ToolImageGenerationParam{}` leaves Type empty,
// and reading the field directly would classify that tool as having no type at all --
// which is the one classification a hosted tool must never get, since it would make a
// request throttle cannot fully price look fully priced.
//
// Default() returns the constant's declared value regardless, so it reports the type
// the SDK will actually send. Two variants are genuinely multi-valued -- web_search
// and web_search_preview each have a dated alternative -- and their fields are read,
// falling back to the base name when the caller left the choice to OpenAI.
func toolType(t responses.ToolUnionParam) string {
	switch {
	case t.OfFunction != nil:
		return string(t.OfFunction.Type.Default())
	case t.OfFileSearch != nil:
		return string(t.OfFileSearch.Type.Default())
	case t.OfComputer != nil:
		return string(t.OfComputer.Type.Default())
	case t.OfComputerUsePreview != nil:
		return string(t.OfComputerUsePreview.Type.Default())
	case t.OfWebSearch != nil:
		return or(string(t.OfWebSearch.Type), "web_search")
	case t.OfMcp != nil:
		return string(t.OfMcp.Type.Default())
	case t.OfCodeInterpreter != nil:
		return string(t.OfCodeInterpreter.Type.Default())
	case t.OfProgrammaticToolCalling != nil:
		return string(t.OfProgrammaticToolCalling.Type.Default())
	case t.OfImageGeneration != nil:
		return string(t.OfImageGeneration.Type.Default())
	case t.OfLocalShell != nil:
		return string(t.OfLocalShell.Type.Default())
	case t.OfShell != nil:
		return string(t.OfShell.Type.Default())
	case t.OfCustom != nil:
		return string(t.OfCustom.Type.Default())
	case t.OfNamespace != nil:
		return string(t.OfNamespace.Type.Default())
	case t.OfToolSearch != nil:
		return string(t.OfToolSearch.Type.Default())
	case t.OfWebSearchPreview != nil:
		return or(string(t.OfWebSearchPreview.Type), "web_search_preview")
	case t.OfApplyPatch != nil:
		return string(t.OfApplyPatch.Type.Default())
	default:
		return ""
	}
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
