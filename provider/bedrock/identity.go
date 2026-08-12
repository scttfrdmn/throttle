package bedrock

import (
	"strings"

	"github.com/scttfrdmn/throttle/usage"
)

// AccessProvider is the value this adapter records for the path a request took.
// It is not the publisher: a Claude model reached through Bedrock has access
// provider "aws-bedrock" and publisher "anthropic", and conflating them would
// make it impossible to ask either question.
const AccessProvider = "aws-bedrock"

// Operations recorded by this adapter. Each is a distinct billable shape, so the
// operation is part of the identity rather than a log detail.
//
// Streaming is a separate operation even though it is priced identically today:
// the operation is what tells a later reader that a stranded pending record
// belongs to a long-lived stream rather than to a single round trip.
// InvokeAgent is a distinct shape again: one API call that internally invokes a
// foundation model an unknown number of times. The operation is what tells a
// reader that a request's cost is a compound of observed steps rather than one
// model's usage, and that its model identity was discovered rather than requested.
// InvokeAgentRuntime is a distinct shape for a different reason again: it is not a
// model call at all. It invokes arbitrary code the caller deployed, whose billable
// dimensions are CPU time and memory-time rather than tokens, and whose internal
// model calls throttle cannot see from the outside. The operation is what tells a
// reader that this record's cost is a resource bill that arrives later, not a token
// count that arrived with the response.
const (
	OperationConverse           = "converse"
	OperationConverseStream     = "converse-stream"
	OperationInvokeAgent        = "invoke-agent"
	OperationInvokeAgentRuntime = "invoke-agent-runtime"
)

// Cross-region inference profile prefixes. Bedrock prepends a geography to a
// model ID to route across regions, e.g.
// "us.anthropic.claude-sonnet-4-5-20250929-v1:0". The prefix is not part of the
// model's own identity, so it is peeled off for enrichment -- but never removed
// from ProviderModelID, which stays verbatim because it is what the bill says.
var geoPrefixes = []string{"us.", "eu.", "apac.", "us-gov.", "global."}

// knownFamilies are the model lines throttle is willing to claim it recognizes.
//
// This set is enrichment and nothing depends on it: a model missing from it is
// fully usable, chargeable, and priceable, because pricing keys on the provider
// model ID. It exists so that reports can group a line across versions and access
// paths, and so that Known() means something. It is deliberately small, and
// growing it is never a prerequisite for supporting a new model.
var knownFamilies = map[string]bool{
	"claude":   true,
	"nova":     true,
	"llama":    true,
	"mistral":  true,
	"mixtral":  true,
	"titan":    true,
	"command":  true,
	"jamba":    true,
	"deepseek": true,
}

// Identify derives an identity from the model ID actually sent to Bedrock.
//
// The provider model ID is copied verbatim and treated as authoritative. Publisher
// and family are parsed from Bedrock's documented ID convention
// (`[geo.]publisher.model-version`), which is structural parsing rather than a
// lookup: a model released after this code was written still yields a publisher.
// When the convention does not hold, the enrichment fields stay empty and the
// identity remains valid and usable.
func Identify(modelID, region string, tier string) usage.ModelIdentity {
	id := usage.ModelIdentity{
		AccessProvider:  AccessProvider,
		ProviderModelID: modelID,
		Operation:       OperationConverse,
		Region:          region,
		ServiceTier:     tier,
	}

	name := modelID

	// An ARN identifies a provisioned throughput, custom model, or inference
	// profile. The trailing segment carries the model reference when there is one.
	if strings.HasPrefix(name, "arn:") {
		id.InferenceProfile = modelID
		if i := strings.LastIndex(name, "/"); i >= 0 && i+1 < len(name) {
			name = name[i+1:]
		} else {
			// An ARN with no resource path tells us nothing more; raw identity is
			// still intact and that is enough to account for the request.
			return id
		}
	}

	for _, p := range geoPrefixes {
		if strings.HasPrefix(name, p) {
			if id.InferenceProfile == "" {
				// A geography prefix *is* a cross-region inference profile, so record
				// that this request was routed rather than pinned.
				id.InferenceProfile = modelID
			}
			name = strings.TrimPrefix(name, p)
			break
		}
	}

	publisher, rest, ok := strings.Cut(name, ".")
	if !ok || publisher == "" || rest == "" {
		return id
	}
	id.Publisher = publisher

	family, base := familyOf(rest)
	if family == "" {
		return id
	}
	id.Family = publisher + "." + family

	if knownFamilies[base] {
		// CanonicalModel keeps the version, since two versions of one family are
		// different models with different prices. Only the ":0"-style revision is
		// dropped, because it identifies a provider-internal packaging of the same
		// priced model.
		name := rest
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		id.CanonicalModel = publisher + "." + name
	}
	return id
}

// familyOf splits a model name into its family and the first segment of that
// family: "claude-sonnet-4-5-20250929-v1:0" yields ("claude-sonnet", "claude").
//
// The family is the leading run of purely alphabetic segments, which is a weaker
// rule than a table of model names and deliberately so -- it degrades to an empty
// family on an unrecognized shape rather than guessing. The base segment is what
// knownFamilies is keyed on, so a new Claude variant is recognized without
// editing the table.
func familyOf(name string) (family, base string) {
	// Drop the ":0"-style revision first; it is never part of a family.
	if i := strings.IndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	var keep []string
	for i, p := range strings.Split(name, "-") {
		if p == "" {
			break
		}
		if isAlpha(p) {
			keep = append(keep, p)
			continue
		}
		// Some publishers fold the version into the first segment ("llama3",
		// "jamba1"). Its alphabetic prefix is the family; the digits are version, so
		// the family ends here.
		if i == 0 {
			if prefix := alphaPrefix(p); prefix != "" {
				keep = append(keep, prefix)
			}
		}
		break
	}
	if len(keep) == 0 {
		return "", ""
	}
	return strings.Join(keep, "-"), keep[0]
}

// alphaPrefix returns the leading alphabetic run of s, which separates a family
// name from a version fused onto it.
func alphaPrefix(s string) string {
	for i, r := range s {
		if r < 'a' || r > 'z' {
			return s[:i]
		}
	}
	return s
}

func isAlpha(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}
