package openai

import (
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/scttfrdmn/throttle/usage"
)

// AccessProvider is the path a request took: OpenAI's own API, directly.
//
// Publisher is recorded separately and identically here, because OpenAI publishes
// the models it serves. That the two coincide is a fact about this provider, not a
// reason to collapse the fields: the same Publisher value has to be comparable
// across access paths, and a GPT model reached through some other gateway would
// share the publisher while differing in access provider.
const AccessProvider = "openai"

// Publisher is who made the models this adapter reaches.
const Publisher = "openai"

// OperationResponses is the Responses API call this adapter governs.
//
// A named operation rather than a bare string, and distinct from any future
// streaming or Chat Completions operation, because each is a separate billable
// shape: an operation is what tells a later reader which API a stranded record
// belongs to, and Chat Completions reports a differently-shaped usage object for
// what looks like the same work.
const OperationResponses = "responses"

// Identify derives an identity from the model string the caller sent.
//
// The model ID is copied verbatim and is authoritative. Unlike Bedrock's
// `publisher.model-version` convention there is nothing structural to parse out of
// an OpenAI model ID -- "gpt-5.1" names no publisher and delimits no version -- so
// this function enriches only what it can state without guessing: the publisher,
// which is known from the access path itself.
//
// CanonicalModel is deliberately left empty. It means "the catalog recognizes this
// model", and this adapter has no catalog to consult at identification time. An
// empty CanonicalModel is a legitimate state: the identity is valid, chargeable,
// and priceable, because pricing keys on the provider model ID. A model OpenAI
// released this morning identifies exactly as well as one from last year.
func Identify(modelID string, tier string) usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  AccessProvider,
		ProviderModelID: modelID,
		Operation:       OperationResponses,
		Publisher:       Publisher,
		ServiceTier:     tier,
	}
}

// modelOf reads the model out of a request.
//
// shared.ResponsesModel is an alias for string, so any model name is valid at the
// type level. That is the behaviour throttle wants and it is checked by test: a
// model this build has never heard of must reach the provider unmodified.
func modelOf(m shared.ResponsesModel) string { return string(m) }

// tierOf reads a service tier from a request or a response.
//
// One function for both directions because OpenAI uses one schema for both, and
// because which of them is authoritative depends on the moment: the request's tier
// selects a price at admission, and the response's tier -- which may differ -- is
// what actually served the call.
//
// "auto" is normalized away. It is not a tier, it is an instruction to resolve one
// server-side from project configuration, so recording it as an identity's tier
// would name a price sheet that does not exist. An empty tier is the honest
// admission-time state for an auto request, and the response reports what it
// resolved to.
func tierOf(t responses.ResponseServiceTier) string {
	if t == responses.ResponseServiceTierAuto {
		return ""
	}
	return string(t)
}

// requestTier reads the tier out of request params, which the SDK types
// differently from the response's despite the identical value set.
func requestTier(t responses.ResponseNewParamsServiceTier) string {
	return tierOf(responses.ResponseServiceTier(t))
}
