package openai

import (
	oai "github.com/openai/openai-go/v3"
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

// The OpenAI API calls this adapter governs.
//
// Named operations rather than bare strings, and distinct from each other, because
// each is a separate billable shape: an operation is what tells a later reader which
// API a stranded record belongs to, and Chat Completions reports a
// differently-shaped usage object for what looks like the same work.
//
// Streaming is a separate operation for a narrower reason. The two forms consume
// identical tokens and are priced by identical code, so the distinction is not about
// money -- it is that a record whose process died mid-request is diagnosed
// differently depending on whether usage was supposed to arrive in a return value or
// in a terminal event. The hyphenated form follows the same convention as Bedrock's
// converse-stream.
//
// The operation is the *only* place the API family is recorded, which is why it is a
// closed set of named constants rather than something derived. A report or dashboard
// that needs to tell the three apart reads Identity.Operation, an opaque
// provider-call string that already exists on every record; adding a top-level
// "api family" field to the neutral schema would be inventing a cross-provider
// concept to describe one provider's product history.
const (
	OperationResponses       = "responses"
	OperationResponsesStream = "responses-stream"
	OperationChatCompletions = "chat-completions"
)

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

// tierOf reads a service tier from a Responses request or response.
//
// One function for both directions because the Responses API uses one schema for
// both, and because which of them is authoritative depends on the moment: the
// request's tier selects a price at admission, and the response's tier -- which may
// differ -- is what actually served the call. See normalizeTier for the rule.
func tierOf(t responses.ResponseServiceTier) string { return normalizeTier(string(t)) }

// requestTier reads the tier out of request params, which the SDK types
// differently from the response's despite the identical value set.
func requestTier(t responses.ResponseNewParamsServiceTier) string {
	return tierOf(responses.ResponseServiceTier(t))
}

// chatModelOf reads the model out of a Chat Completions request.
//
// shared.ChatModel is an alias for string, exactly like shared.ResponsesModel, so any
// model name is valid at the type level here too. A model this build has never heard
// of reaches OpenAI unmodified and identifies as itself, which is checked by test:
// nothing about identity requires the fixtures or a catalog to recognize the string.
func chatModelOf(m shared.ChatModel) string { return string(m) }

// chatTierOf reads a service tier from a Chat Completions response, and
// chatRequestTier from its request params.
//
// Deliberately not the same functions as tierOf and requestTier despite the value sets
// being identical -- auto, default, flex, scale, priority, fast in both APIs -- because
// the SDK declares four separate named string types and converting between two API
// families' enums would assert that OpenAI intends them to stay identical. It has
// already diverged the two APIs elsewhere. The normalization rule *is* shared, by
// routing through normalizeTier, since that rule is throttle's own.
func chatTierOf(t oai.ChatCompletionServiceTier) string { return normalizeTier(string(t)) }

func chatRequestTier(t oai.ChatCompletionNewParamsServiceTier) string {
	return normalizeTier(string(t))
}

// normalizeTier applies throttle's one rule about service tiers.
//
// "auto" is normalized away. It is not a tier, it is an instruction to resolve one
// server-side from project configuration, so recording it as an identity's tier would
// name a price sheet that does not exist. An empty tier is the honest admission-time
// state for an auto request, and the response reports what it resolved to.
func normalizeTier(t string) string {
	if t == "auto" {
		return ""
	}
	return t
}
