package anthropic

import (
	"strconv"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/scttfrdmn/throttle/usage"
)

// AccessProvider is the path a request took: Anthropic's own API, directly.
//
// This is emphatically not the same access provider as Claude on Bedrock, and keeping
// them apart is one of throttle's model-identity invariants rather than a bookkeeping
// nicety. The same underlying model reached the two ways has different rates,
// different model IDs, a different credential chain, a different failure surface, and
// a different party sending the bill. A report that merged them would answer "what did
// we spend on Claude" while making "which vendor do we owe" unanswerable.
//
// What the two paths do share is Publisher and, eventually, a canonical model family.
// That is exactly why those are separate fields.
const AccessProvider = "anthropic"

// Publisher is who made the models this adapter reaches.
//
// Identical in value to AccessProvider here, because Anthropic serves its own models,
// and that coincidence is the reason the fields must stay distinct: this same
// Publisher value appears on Bedrock records, where the access provider is
// "aws-bedrock". Collapsing the two would make the cross-access-path comparison
// impossible on the one provider where it matters most.
const Publisher = "anthropic"

// OperationMessages is the Anthropic API this adapter governs.
//
// Non-streaming only. Streaming Messages will be a separate operation for the same
// reason Bedrock's converse-stream and OpenAI's responses-stream are: the two forms
// consume identical tokens and are priced by identical code, so the distinction is not
// about money -- it is that a record whose process died mid-request is diagnosed
// differently depending on whether usage was supposed to arrive in a return value or
// in a terminal event.
const OperationMessages = "messages"

// Identify derives an identity from the model string the caller sent.
//
// The model ID is copied verbatim and is authoritative. Nothing is parsed out of it:
// unlike Bedrock's `publisher.model-version` convention, "claude-sonnet-5" delimits no
// version and names no publisher, and an alias like "claude-sonnet-latest" would parse
// to something actively misleading. So this enriches only what the access path itself
// establishes -- the publisher.
//
// CanonicalModel is deliberately left empty. It means "the catalog recognizes this
// model", and this adapter consults no catalog at identification time. Empty is a
// legitimate state: the identity is valid, chargeable, and priceable, because pricing
// keys on the provider model ID. A model Anthropic released this morning identifies
// exactly as well as one from last year -- and if the fixtures have no rate for it, the
// consequence is an explicitly unknown cost, never a free request.
//
// ServiceTier and InferenceGeo are left for the caller to fill in, because neither is
// knowable from the model string and the two are read from different places at
// different moments. See geoOf and tierOf.
func Identify(modelID string) usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  AccessProvider,
		ProviderModelID: modelID,
		Operation:       OperationMessages,
		Publisher:       Publisher,
	}
}

// modelOf reads the model out of a request or a response.
//
// anthropic.Model is an alias for string, so any model name is valid at the type
// level. That is the behaviour throttle wants and it is checked by test: a model this
// build has never heard of must reach Anthropic unmodified and identify as itself.
//
// One function for both directions because the SDK uses one type for both, and because
// the two answer different questions rather than competing: the request's model is
// what the caller asked for, and the response's is what ran. An alias resolving to a
// dated snapshot makes them differ, and both are kept. See Accounting.ServedModelID.
func modelOf(m anth.Model) string { return string(m) }

// requestGeo reads the inference geography the caller asked for, if any.
//
// An omitted parameter yields the empty string, and that is deliberately *not*
// normalized to Anthropic's documented "global" default. A workspace can carry a
// default inference geography, so an omitted parameter can still be served -- and
// priced -- in a geography the request never named. "Not stated" is therefore the only
// honest admission-time value, and the response is what says where inference actually
// happened.
//
// Nothing else is ever consulted. Not the machine's location, not an IP address, not
// an AWS region, not a timezone. A geography that changes the price has to come from
// the provider's own request or response field or not be claimed at all.
func requestGeo(g param.Opt[string]) string {
	if !g.Valid() {
		return ""
	}
	return g.Value
}

// geoOf reads the inference geography Anthropic reported serving the request in.
//
// This is the authoritative pricing selector, and it is authoritative precisely
// because it can differ from the request: a request that named nothing resolves
// server-side through workspace configuration, and US-only inference carries a
// published multiplier across every token category. Reading it from the response is
// what makes the difference visible instead of assumed.
//
// Absent is not "global". A response whose usage object omits the field leaves this
// empty, so the request prices through the unqualified rate row rather than through a
// geography throttle invented for it.
func geoOf(u anth.Usage) string {
	if !u.JSON.InferenceGeo.Valid() {
		return ""
	}
	return u.InferenceGeo
}

// tierOf reads the service tier Anthropic reported serving the request on.
//
// Preserved as identity metadata and priced on nothing. Anthropic's published token
// rates do not vary by tier: standard and priority bill at the same per-token price,
// and priority tier's own documentation describes capacity commitments with burndown
// ratios rather than a price sheet -- capacity accounting, not a rate. Batch is a
// distinct API this adapter does not govern.
//
// So this is recorded rather than selected on, and that is the finding, not an
// omission. An organization holding a priority capacity contract has terms throttle
// cannot see, and the honest way to account for them is the pricing override
// capability that already exists -- not a multiplier inferred from public rates, which
// would be inventing a contract price.
//
// If Anthropic ever does publish tier-varying token rates, the shared price selector
// already carries the axis and this becomes a one-line change: return the tier and add
// the rows. That seam is why the field is captured at all.
func tierOf(u anth.Usage) string {
	if !u.JSON.ServiceTier.Valid() {
		return ""
	}
	return normalizeTier(string(u.ServiceTier))
}

// requestTier reads the tier the caller asked for.
//
// The request and response types are separate SDK enums with different value sets --
// a request says "auto" or "standard_only", a response says "standard", "priority", or
// "batch" -- so they are read by separate functions rather than converted. They share
// throttle's normalization rule, because that rule is throttle's own.
func requestTier(t anth.MessageNewParamsServiceTier) string {
	return normalizeTier(string(t))
}

// normalizeTier applies throttle's one rule about service tiers.
//
// "auto" is normalized away. It is not a tier, it is an instruction to resolve one
// server-side, so recording it as an identity's tier would name a price sheet that
// does not exist. An empty tier is the honest admission-time state for an auto
// request, and the response reports what it resolved to.
//
// "standard_only" is left alone rather than mapped to "standard". It is a request-side
// constraint, not a served tier, and the response says which tier actually served the
// call; rewriting one into the other would report a request field as though it were an
// observation.
func normalizeTier(t string) string {
	if t == "auto" {
		return ""
	}
	return t
}

// withServedModel records the model Anthropic reported using, when it differs from the
// one asked for.
//
// Metadata rather than a column: the identity's ProviderModelID is the caller's exact
// string and stays that way, and a served-model field on the neutral Record would be a
// schema change made for one provider's alias resolution. The caller's map is copied
// rather than written to, since it belongs to them.
//
// Recording both is what makes an alias safe to use. "claude-sonnet-latest" is not a
// model, it is a pointer that Anthropic may re-aim at a differently-priced model
// without notice, so rewriting the caller's request into the resolved ID would destroy
// the fact that an alias was used -- and keeping only the alias would lose which model
// the money was actually spent on. The pricing consequence is handled where it belongs:
// rates key on the string the caller sent, so an alias with no captured rate settles
// unresolved rather than being priced as though the alias were immutable.
func withServedModel(m map[string]string, served string) map[string]string {
	return withMetadata(m, "anthropic.served_model", served)
}

// withWebFetchCount records how many web fetches Anthropic performed.
//
// Metadata rather than a usage dimension, and that is the whole point of it being here:
// the counter is authoritative and carries no surcharge, so pricing it would require
// inventing a rate and recording it as unpriced would make every web-fetch request
// settle partially priced for no monetary reason. A statistic with no price is
// metadata. See webFetchCount.
func withWebFetchCount(m map[string]string, n int64) map[string]string {
	return withMetadata(m, "anthropic.web_fetch_requests", strconv.FormatInt(n, 10))
}

// withMetadata adds one safe fact to a metadata map without writing to the caller's.
//
// The copy is not defensive tidiness: the map came from the caller's Request, and a
// governed call that mutated it would change the caller's own state as a side effect of
// being accounted.
func withMetadata(m map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}
