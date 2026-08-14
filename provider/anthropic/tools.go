package anthropic

import (
	"fmt"
	"sort"

	anth "github.com/anthropics/anthropic-sdk-go"
)

// toolBilling classifies what a tool means for throttle's ability to account for a
// request's whole Anthropic charge.
//
// The question it answers is deliberately not "is a tool block present" but "who
// executes this, and in what unit does Anthropic bill it". Those come apart in both
// directions: the caller's own function produces tool-use blocks and costs Anthropic
// nothing beyond tokens, while a server-side container can be billed for time even
// when it is never invoked.
type toolBilling int

const (
	// billingUnverified means throttle cannot state how this tool is billed.
	//
	// Deliberately the zero value, so that every path which fails to classify a tool --
	// a variant added to a future SDK, a switch arm somebody forgot -- lands here rather
	// than on the optimistic answer. An unverified "token-only" is the expensive
	// mistake: it makes a request look fully priced when part of the bill was never
	// visible, and nothing in the record indicates anything was missed. A floor that
	// turns out to have been the total is merely conservative.
	billingUnverified toolBilling = iota

	// billedByCaller means the work happens on the caller's machine. Anthropic bills
	// the schema, the arguments, and the results as tokens, which the usage object
	// reports in full; whatever the execution itself costs is not an Anthropic charge
	// and throttle assigns it none. Inventing an execution fee for the caller's own
	// function would be inventing a charge.
	billedByCaller

	// billedInTokens means Anthropic executes the tool and the entire charge still
	// arrives as tokens in the usage object. A request carrying only these can be fully
	// priced.
	billedInTokens

	// billedByCountedDimension means Anthropic executes the tool and charges for it
	// separately, but reports an authoritative count of the billable unit in the usage
	// object -- so throttle prices it from a captured rate and the total stays complete.
	billedByCountedDimension

	// billedOutsideUsage means Anthropic executes the tool and charges in a unit no
	// field of the response reports. The token cost throttle computes is a floor rather
	// than a total, and no arithmetic available to this adapter can close the gap.
	billedOutsideUsage
)

// toolKind is what a request's tool was, in Anthropic's own naming.
//
// The kind is taken from the SDK's declared name constant rather than from the dated
// type discriminator, because billing follows the kind: every web_search variant
// carries the same per-search charge, and a new dated revision of one should inherit
// its predecessor's classification rather than arrive unclassified. The dated types are
// how Anthropic versions a tool's *schema*; the name is what says what it does.
type toolKind struct {
	name    string
	billing toolBilling
}

// classifyTools examines a Messages request's tools and reports whether the response
// can account for the whole Anthropic charge.
//
// # Why this is a switch and not a map
//
// The SDK models the tool list as a struct of nineteen pointers, one per variant, and
// only the populated one carries a name. Reading the name field directly does not work:
// eighteen variants declare it as a single-valued constant type whose zero value is the
// empty string, filled in from a struct tag at marshal time -- so the idiomatic
// `&anth.WebSearchTool20250305Param{}` has an empty Name and would classify as no tool
// at all. Default() returns what the SDK will actually send.
//
// # Why an unrecognized variant is detected rather than ignored
//
// The switch is exhaustive against v1.63.0's nineteen variants, and Anthropic adds
// tools. A build compiled against a newer SDK will one day be handed a variant this
// switch does not name, and the failure mode worth engineering against is not a crash:
// it is a server-side tool with its own charge falling through to "nothing to see here"
// and settling as fully priced.
//
// So the fallthrough is not silent. The SDK's own generated accessors -- GetName and
// GetType, which it regenerates whenever it adds a variant -- report whether *some*
// variant is populated. A tool that is populated but unnamed by this switch is an
// unknown tool of unknown billing, and it makes the cost incomplete. A union with no
// variant set at all is a malformed request Anthropic will reject, so there is no charge
// to account for and nothing to flag.
func classifyTools(tools []anth.ToolUnionParam) exposure {
	unaccounted := map[string]bool{}
	for _, t := range tools {
		k := toolKindOf(t)
		if k.name == "" {
			// No variant populated: a malformed request, rejected before it is billed.
			continue
		}
		switch k.billing {
		case billedByCaller, billedInTokens, billedByCountedDimension:
			// Fully accountable, for three different reasons. See toolBilling.
		default:
			// billedOutsideUsage and billingUnverified alike. They differ in what is known
			// about them and not in what throttle can do about them.
			unaccounted[k.name] = true
		}
	}
	if len(unaccounted) == 0 {
		return exposure{}
	}
	out := make([]string, 0, len(unaccounted))
	for n := range unaccounted {
		out = append(out, n)
	}
	sort.Strings(out)
	return exposure{tools: out}
}

// toolKindOf reports what a tool param union holds and how it is billed.
//
// The classifications, and the evidence for each:
//
//	custom function     billedByCaller. The caller's own code, executed by the caller.
//	                    Anthropic bills the schema and the arguments as tokens; the
//	                    execution is the caller's to pay for.
//	bash                billedByCaller. Anthropic defines the schema, the caller runs
//	                    the command. Distinct from the bash *inside* code execution,
//	                    which is server-side and arrives as a content block, not a tool.
//	text editor         billedByCaller. Same shape: Anthropic's schema, the caller's
//	                    filesystem.
//	memory              billedByCaller. Anthropic's schema over storage the caller
//	                    provides and manages.
//	web_search          billedByCountedDimension. Anthropic executes it and charges
//	                    $10 per thousand searches, and reports the billable count in
//	                    usage.server_tool_use.web_search_requests. See normalizeUsage
//	                    for why that counter is used and the content blocks are not.
//	web_fetch           billedInTokens. Anthropic executes it and publishes no
//	                    surcharge: the cost of a fetch is the tokens the fetched
//	                    content becomes, which input_tokens already reports.
//	code_execution      billedOutsideUsage. Billed by container time, with a
//	                    per-organization monthly free allowance. See below.
//	tool_search         billingUnverified. Server-side, and no charge could be
//	                    confirmed in either direction. Classified conservatively.
//
// # Why code execution can never settle as a known cost
//
// The charge is container time -- a five-minute minimum, a free allowance per
// organization per month, then an hourly rate -- and a response reports a container ID
// and an expiry timestamp and no duration whatsoever. Every route to a number is a
// fabrication: dividing the monthly allowance across requests, assuming the allowance
// is or is not exhausted, deriving runtime from API latency, or turning a request count
// into a duration. Anthropic also documents that "If files are included in the request,
// execution time is billed even if the tool is not called", so even a request whose
// response shows no execution at all may have been billed for some.
//
// So a request carrying it is refused under enforce, before Anthropic is called, and
// settles under monitor as a partial cost carrying the token floor. Establishing the
// real figure needs organization-level billing data, which is separate work and is not
// this adapter's to guess at.
func toolKindOf(t anth.ToolUnionParam) toolKind {
	switch {
	case t.OfTool != nil:
		// The one variant whose Name is the caller's own string rather than a constant.
		// The name is not used for classification -- every custom tool is the caller's
		// code by definition -- and it is not recorded either, since a function name is
		// closer to request content than to accounting metadata.
		return toolKind{name: "custom", billing: billedByCaller}

	case t.OfBashTool20250124 != nil:
		return toolKind{name: string(t.OfBashTool20250124.Name.Default()), billing: billedByCaller}

	case t.OfTextEditor20250124 != nil:
		return toolKind{name: string(t.OfTextEditor20250124.Name.Default()), billing: billedByCaller}
	case t.OfTextEditor20250429 != nil:
		return toolKind{name: string(t.OfTextEditor20250429.Name.Default()), billing: billedByCaller}
	case t.OfTextEditor20250728 != nil:
		return toolKind{name: string(t.OfTextEditor20250728.Name.Default()), billing: billedByCaller}

	case t.OfMemoryTool20250818 != nil:
		return toolKind{name: string(t.OfMemoryTool20250818.Name.Default()), billing: billedByCaller}

	case t.OfWebSearchTool20250305 != nil:
		return toolKind{name: string(t.OfWebSearchTool20250305.Name.Default()), billing: billedByCountedDimension}
	case t.OfWebSearchTool20260209 != nil:
		return toolKind{name: string(t.OfWebSearchTool20260209.Name.Default()), billing: billedByCountedDimension}
	case t.OfWebSearchTool20260318 != nil:
		return toolKind{name: string(t.OfWebSearchTool20260318.Name.Default()), billing: billedByCountedDimension}

	case t.OfWebFetchTool20250910 != nil:
		return toolKind{name: string(t.OfWebFetchTool20250910.Name.Default()), billing: billedInTokens}
	case t.OfWebFetchTool20260209 != nil:
		return toolKind{name: string(t.OfWebFetchTool20260209.Name.Default()), billing: billedInTokens}
	case t.OfWebFetchTool20260309 != nil:
		return toolKind{name: string(t.OfWebFetchTool20260309.Name.Default()), billing: billedInTokens}
	case t.OfWebFetchTool20260318 != nil:
		return toolKind{name: string(t.OfWebFetchTool20260318.Name.Default()), billing: billedInTokens}

	case t.OfCodeExecutionTool20250522 != nil:
		return toolKind{name: string(t.OfCodeExecutionTool20250522.Name.Default()), billing: billedOutsideUsage}
	case t.OfCodeExecutionTool20250825 != nil:
		return toolKind{name: string(t.OfCodeExecutionTool20250825.Name.Default()), billing: billedOutsideUsage}
	case t.OfCodeExecutionTool20260120 != nil:
		return toolKind{name: string(t.OfCodeExecutionTool20260120.Name.Default()), billing: billedOutsideUsage}
	case t.OfCodeExecutionTool20260521 != nil:
		return toolKind{name: string(t.OfCodeExecutionTool20260521.Name.Default()), billing: billedOutsideUsage}

	case t.OfToolSearchToolBm25_20251119 != nil:
		return toolKind{name: string(t.OfToolSearchToolBm25_20251119.Name.Default()), billing: billingUnverified}
	case t.OfToolSearchToolRegex20251119 != nil:
		return toolKind{name: string(t.OfToolSearchToolRegex20251119.Name.Default()), billing: billingUnverified}

	default:
		return unrecognizedTool(t)
	}
}

// unrecognizedTool names a tool variant this build's switch does not cover.
//
// The two accessors are the SDK's own generated ones, regenerated whenever a variant is
// added, so they see variants this file does not. If either reports a populated field,
// something real is in the union and its billing is by definition unverified.
//
// The name is whatever the SDK will send, so a record carries enough to identify the
// tool later even though this build could not classify it -- which is what makes such a
// request resolvable by a catalog update plus reconciliation rather than a rebuild.
func unrecognizedTool(t anth.ToolUnionParam) toolKind {
	if n := t.GetName(); n != nil && *n != "" {
		return toolKind{name: *n, billing: billingUnverified}
	}
	if ty := t.GetType(); ty != nil && *ty != "" {
		return toolKind{name: *ty, billing: billingUnverified}
	}
	if t.GetName() != nil || t.GetType() != nil {
		// Populated but reporting nothing readable: still a tool, still unclassifiable.
		return toolKind{name: "an unrecognized tool", billing: billingUnverified}
	}
	return toolKind{}
}

// observedExposure reports what the *response* revealed that throttle cannot account
// for.
//
// The request is where exposure is usually established, because a container charge is
// incurred by asking for the tool and no field of the reply reports it. But the request
// is not the only source, and two response-side surprises matter:
//
//   - a usage counter this build cannot read. A new numeric field becomes a namespaced
//     dimension nothing prices, which pricing already reports as unpriced -- but a new
//     *non*-numeric field cannot become a dimension at all, and it is still evidence
//     the response outgrew this build. Reporting it here is what stops such a request
//     settling as fully known on the strength of counters throttle could not parse.
//   - a container the request did not obviously ask for. A message that came back with
//     container metadata was served by something billed for time, whatever the tool
//     list said -- a tool inherited from a prior turn, a beta surface, a server-side
//     default. The charge is as invisible as the request-side case, and it is the
//     container's presence rather than the tool block's that establishes it.
//
// Merged with the request's exposure rather than replacing it: both are true, and a
// request can be unaccountable for one reason before the call and a different one after.
func observedExposure(m *anth.Message) exposure {
	if m == nil {
		return exposure{}
	}
	var out exposure

	if m.JSON.Container.Valid() && m.Container.ID != "" {
		out.tools = append(out.tools, "a code execution container")
	}

	var unknown []string
	for prefix, fields := range unknownUsageFields(m.Usage) {
		for name, raw := range fields {
			if _, numeric := parseCount(raw); numeric {
				// Already a dimension, already unpriced, already making the cost a floor.
				// Naming it twice would say nothing more.
				continue
			}
			unknown = append(unknown, prefix+name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		out.tools = append(out.tools, fmt.Sprintf("usage fields this build cannot read (%s)", joinAnd(unknown)))
	}
	sort.Strings(out.tools)
	return out
}

// webFetchCount reports the authoritative web-fetch count, if the response gave one.
//
// Not a usage dimension, because it carries no charge: a fetch costs the tokens its
// content becomes, and input_tokens already reports those. Minting a $0 rate to make
// the counter look supported would assert a price Anthropic has not published, and
// recording it as an unpriced dimension would make every web-fetch request settle
// partially priced for no monetary reason. It is kept as metadata, where a statistic
// belongs.
func webFetchCount(u anth.Usage) (int64, bool) {
	if !u.ServerToolUse.JSON.WebFetchRequests.Valid() {
		return 0, false
	}
	return u.ServerToolUse.WebFetchRequests, true
}
