package anthropic

import (
	"context"
	"errors"
	"fmt"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/scttfrdmn/throttle/activity"
)

// Messages adapts a real Anthropic client to the MessagesAPI interface.
//
// A thin wrapper because MessageService is a struct value inside a struct-value Client,
// so it cannot satisfy an interface with a pointer-receiver method directly. Nothing is
// added: the call, its parameters, and its options pass straight through, so the request
// Anthropic receives is the one the caller built.
func Messages(c *anth.Client) MessagesAPI { return messagesClient{c: c} }

type messagesClient struct{ c *anth.Client }

func (m messagesClient) New(ctx context.Context, body anth.MessageNewParams, opts ...option.RequestOption) (*anth.Message, error) {
	return m.c.Messages.New(ctx, body, opts...)
}

// Counter adapts a real Anthropic client to the TokenCounter interface, enabling
// preflight input token counts. It is a separate wrapper because counting is an extra
// round trip a caller opts into; see Config.Counter.
func Counter(c *anth.Client) TokenCounter { return counterClient{c: c} }

type counterClient struct{ c *anth.Client }

func (r counterClient) CountTokens(ctx context.Context, body anth.MessageCountTokensParams, opts ...option.RequestOption) (*anth.MessageTokensCount, error) {
	return r.c.Messages.CountTokens(ctx, body, opts...)
}

// ErrGenerationStopped means generation ended for a reason other than reaching a natural
// stopping point.
//
// Reported so a caller cannot mistake a truncated, refused, or paused answer for a
// complete one. It is emphatically not a refund, and that distinction is the whole
// content of this file: a stop reason describes what happened to the *generation*, and
// says nothing about the monetary outcome. Whenever the message carries authoritative
// usage, that usage is charged -- max_tokens, refusal, and a context window overrun
// included.
var ErrGenerationStopped = errors.New("anthropic: generation stopped before a natural stop")

// naturalStops are the stop reasons that mean the model finished its turn as designed.
//
// A set rather than a switch, and an open one rather than exhaustive, because the two
// directions of unknown matter differently.
//
// end_turn is the plain case. tool_use is equally natural in the sense that matters
// here: the model finished its turn by asking for a tool, which is the API working
// exactly as designed and is what an agent loop sees on nearly every iteration.
// stop_sequence means the caller's own stop sequence fired, which is the caller getting
// what they asked for. pause_turn is Anthropic pausing a long-running server-tool turn
// for the caller to continue -- also by design, and specifically not an error.
//
// Everything else is reported: max_tokens, refusal, model_context_window_exceeded, and
// any value a future model introduces. Reporting an unfamiliar reason is the safe
// direction, since the alternative is silently calling something normal that nobody has
// classified.
var naturalStops = map[string]bool{
	"":              true, // said nothing, which is not the same as said something bad
	"end_turn":      true,
	"tool_use":      true,
	"stop_sequence": true,
	"pause_turn":    true,
}

// stoppedNaturally reports whether generation ended as designed.
//
// The stop reason is compared as an open string, never against an exhaustive switch over
// the SDK's current constants. Anthropic adds stop reasons -- pause_turn, refusal, and
// model_context_window_exceeded were all additions -- and a decoder handed an unfamiliar
// one accepts it cleanly rather than failing, so the value really does reach this code.
// An exhaustive switch with a default of "normal" would eventually classify a new
// abnormal ending as fine.
func stoppedNaturally(m *anth.Message) bool {
	if m == nil {
		return true
	}
	return naturalStops[string(m.StopReason)]
}

// stopOutcome maps a message's stop reason to a durable activity outcome, or empty when
// generation ended naturally.
//
// Everything that is not a natural stop lands on OutcomeProviderError, matching the other
// adapters. The vocabulary is deliberately provider-neutral and deliberately not
// extended: a truncated or refused message is a provider-side reason the request did not
// deliver what was asked, which is what that outcome means. The specific reason goes in
// Record.Error, where a provider's own classification belongs.
//
// Note what this does *not* do: it does not influence whether the request settles. Status
// and money are decided separately, and by the time this is consulted the charge has
// already been recorded from authoritative usage. A refused generation still consumed
// tokens, and Anthropic still billed them.
func stopOutcome(m *anth.Message) activity.Outcome {
	if stoppedNaturally(m) {
		return ""
	}
	return activity.OutcomeProviderError
}

// stopReason renders why generation stopped, using Anthropic's own vocabulary and
// nothing else.
//
// A refusal is the case worth being explicit about. Anthropic populates stop_details
// only on a refusal, and it carries two fields: a category from a fixed set -- cyber,
// bio, frontier_llm, reasoning_extraction, general_harms -- and a free-text explanation.
// Only the category is recorded. The explanation is model-generated prose about the
// prompt, which makes it content rather than metadata, and Anthropic documents that it
// "is not guaranteed to be stable" in any case. The category is the actionable fact and
// the safe one.
func stopReason(m *anth.Message) string {
	if m == nil {
		return ""
	}
	reason := string(m.StopReason)
	if reason == "" {
		return "the provider reported no stop reason"
	}
	out := "generation stopped because of " + reason
	if cat := string(m.StopDetails.Category); cat != "" {
		out += " (category " + cat + ")"
	}
	// stop_sequence names which of the caller's own sequences fired. Deliberately not
	// recorded: the caller wrote it, so it is request content, and a caller who uses a
	// fragment of the prompt as a delimiter would have it persisted.
	return out
}

// stopError converts a non-natural stop into an error for the caller.
func stopError(m *anth.Message) error {
	if stoppedNaturally(m) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrGenerationStopped, stopReason(m))
}

// redactProviderError renders an Anthropic error for durable storage, keeping its
// classification and dropping its body.
//
// The SDK's Error.Error() is `fmt.Sprintf("%s %s", statusInfo, r.JSON.raw)` -- it embeds
// the raw JSON response body verbatim, and the request URL with it. For a request
// rejected on its content, that body can quote the prompt back. Persisting it would put
// caller content into throttle's activity store through the error field, which is the one
// thing the privacy boundary exists to prevent. RawJSON, DumpRequest, and DumpResponse
// are never touched for the same reason.
//
// So an API error is reduced to what an operator actually needs in order to act:
//
//	HTTP status  429 means retry, 401 means fix credentials, 529 means back off
//	error type   Anthropic's own classification, a fixed vocabulary
//	request ID   what Anthropic support asks for, and it identifies nothing else
//
// Any other error -- a transport failure, a DNS error, the SDK's own local refusal of an
// unstreamed long-generation request -- carries no provider payload and is recorded as-is.
func redactProviderError(err error) string {
	var apiErr *anth.Error
	if !errors.As(err, &apiErr) {
		return err.Error()
	}
	out := fmt.Sprintf("the provider returned HTTP %d", apiErr.StatusCode)
	if t := apiErr.Type(); t != "" {
		out += " (" + string(t) + ")"
	}
	if apiErr.RequestID != "" {
		out += " request " + apiErr.RequestID
	}
	return out
}
