package usage

import "strings"

// ModelIdentity is what was called, expressed as independent dimensions rather
// than a Provider -> Model tree.
//
// The distinction that matters most: AccessProvider is the path the request took
// (AWS Bedrock), Publisher is who made the model (Anthropic). Collapsing them
// makes it impossible to ask either "how much did I spend through Bedrock?" or
// "how much did I spend on Claude across every access path?".
//
// Only AccessProvider, ProviderModelID, and Operation are required. Everything
// else is enrichment, because a Bedrock model released this morning must remain
// usable by a throttle build from last month: the exact provider model ID is the
// authoritative raw identity, and canonical naming is a convenience layered on
// top. A catalog that has never heard of a model must not be able to stop a
// request.
type ModelIdentity struct {
	// AccessProvider is the path the request took, e.g. "aws-bedrock".
	AccessProvider string

	// ProviderModelID is the exact identifier sent to the provider, verbatim.
	// This is authoritative identity and is never normalized, rewritten, or
	// inferred: it is what the bill will refer to.
	ProviderModelID string

	// Operation is the provider call made, e.g. "converse".
	Operation string

	// Publisher is who published the model, e.g. "anthropic", when known.
	Publisher string

	// Family groups model versions, e.g. "claude-sonnet", when known.
	Family string

	// CanonicalModel is throttle's normalized name for the model, when the
	// catalog recognizes it. Empty means unrecognized, which is a legitimate
	// state and not an error.
	CanonicalModel string

	// Region, ServiceTier, InferenceProfile, and Endpoint are access dimensions
	// that may affect price or availability. Each is optional because not every
	// provider has the concept.
	Region           string
	ServiceTier      string
	InferenceProfile string
	Endpoint         string

	// InferenceGeo is the geography a provider performed inference in, when the
	// provider treats that as a priced choice rather than as an endpoint.
	//
	// Distinct from Region, and the distinction is the reason this field exists.
	// Region is the endpoint the request was sent to -- an address. InferenceGeo is
	// a constraint on where the work may happen, requested as a parameter and
	// reported back in the response, and it re-rates every token dimension of the
	// request. A provider can have one, both, or neither.
	//
	// It is only ever the provider's own value, taken from the request parameter or
	// the response. It is never inferred from a machine's location, an IP address,
	// a cloud region, or a timezone: guessing it would produce a confident price
	// computed from a rate sheet the request did not run under.
	InferenceGeo string
}

// Known reports whether canonical enrichment was resolved. An identity can be
// perfectly usable — chargeable, priceable if the catalog keys on the provider
// ID — while unknown.
func (m ModelIdentity) Known() bool { return m.CanonicalModel != "" }

// Valid reports whether the identity carries the minimum needed to account for a
// request honestly: where it went, what was called, and which operation.
func (m ModelIdentity) Valid() bool {
	return m.AccessProvider != "" && m.ProviderModelID != "" && m.Operation != ""
}

// Describe renders the identity for logs and CLI output, preferring the
// canonical name but never hiding the provider ID, since that is the identity
// the provider's own bill will use.
func (m ModelIdentity) Describe() string {
	var b strings.Builder
	b.WriteString(m.AccessProvider)
	b.WriteString("/")
	switch {
	case m.Publisher != "" && m.CanonicalModel != "":
		b.WriteString(m.Publisher)
		b.WriteString("/")
		b.WriteString(m.CanonicalModel)
		b.WriteString(" (")
		b.WriteString(m.ProviderModelID)
		b.WriteString(")")
	default:
		b.WriteString(m.ProviderModelID)
	}
	if m.Region != "" {
		b.WriteString(" @")
		b.WriteString(m.Region)
	}
	if m.InferenceGeo != "" {
		b.WriteString(" geo:")
		b.WriteString(m.InferenceGeo)
	}
	if m.ServiceTier != "" {
		b.WriteString(" [")
		b.WriteString(m.ServiceTier)
		b.WriteString("]")
	}
	return b.String()
}
