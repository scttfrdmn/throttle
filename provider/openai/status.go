package openai

import (
	"context"
	"errors"
	"fmt"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/activity"
)

// Responses adapts a real OpenAI client to the ResponsesAPI interface.
//
// A thin wrapper because ResponseService is a struct value, not a pointer, so it
// cannot satisfy an interface with a pointer-receiver method directly. Nothing is
// added: the call, its parameters, and its options pass straight through.
func Responses(c *oai.Client) ResponsesAPI { return responsesClient{c: c} }

type responsesClient struct{ c *oai.Client }

func (r responsesClient) New(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) (*responses.Response, error) {
	return r.c.Responses.New(ctx, body, opts...)
}

// Counter adapts a real OpenAI client to the InputTokenCounter interface, enabling
// preflight input token counts. It is a separate wrapper because counting is an extra
// round trip a caller opts into; see Config.Counter.
func Counter(c *oai.Client) InputTokenCounter { return counterClient{c: c} }

type counterClient struct{ c *oai.Client }

func (r counterClient) Count(ctx context.Context, body responses.InputTokenCountParams, opts ...option.RequestOption) (*responses.InputTokenCountResponse, error) {
	return r.c.Responses.InputTokens.Count(ctx, body, opts...)
}

// Errors describing a response OpenAI returned but did not complete.
//
// These are distinct from ErrProvider, which wraps a transport or API-level failure.
// A response that arrives and says it failed is a different event from a call that
// never produced one: the first has an ID, may carry usage, and may have been
// billed.
var (
	// ErrResponseFailed means OpenAI returned a response whose own status is failed.
	ErrResponseFailed = errors.New("openai: the provider marked the response failed")

	// ErrResponseIncomplete means generation stopped before finishing -- the output cap
	// was reached, or a content filter intervened.
	//
	// Reported as an error so a caller cannot mistake a truncated answer for a complete
	// one, but it is not a refund: if the response carried usage, that usage is charged.
	ErrResponseIncomplete = errors.New("openai: the provider returned an incomplete response")

	// ErrResponseCancelled means the response was cancelled server-side. Only a
	// background response can be cancelled, so this cannot arise from a caller
	// abandoning a synchronous call.
	ErrResponseCancelled = errors.New("openai: the provider cancelled the response")

	// ErrResponseUnfinished means the response came back still in progress or queued.
	// A non-streaming create should not return one, and treating it as finished would
	// settle a request that is still running.
	ErrResponseUnfinished = errors.New("openai: the provider returned an unfinished response")
)

// completed reports whether a response finished normally.
//
// An empty status counts as completed: status is not a required field on a Response,
// so its absence means the provider said nothing about it, not that something went
// wrong.
func completed(r *responses.Response) bool {
	switch r.Status {
	case "", responses.ResponseStatusCompleted:
		return true
	default:
		return false
	}
}

// statusOutcome maps a response's status to a durable activity outcome, or empty for
// a response that completed normally.
//
// Everything that is not a clean completion lands on OutcomeProviderError. The
// vocabulary is deliberately provider-neutral and deliberately not extended here: a
// truncated response is a provider-side reason the request did not deliver what was
// asked, which is what that outcome means. The specific reason goes in Record.Error,
// where a provider's own words belong.
func statusOutcome(r *responses.Response) activity.Outcome {
	if r == nil || completed(r) {
		return ""
	}
	return activity.OutcomeProviderError
}

// statusError converts a non-completed response status into an error for the caller.
func statusError(r *responses.Response) error {
	if r == nil || completed(r) {
		return nil
	}
	reason := responseReason(r)
	switch r.Status {
	case responses.ResponseStatusFailed:
		return fmt.Errorf("%w: %s", ErrResponseFailed, reason)
	case responses.ResponseStatusIncomplete:
		return fmt.Errorf("%w: %s", ErrResponseIncomplete, reason)
	case responses.ResponseStatusCancelled:
		return ErrResponseCancelled
	default:
		// in_progress or queued.
		return fmt.Errorf("%w: %s", ErrResponseUnfinished, r.Status)
	}
}

// responseReason renders why a response did not complete, using the provider's own
// classification rather than its prose.
//
// The error code and the incomplete reason are both closed enumerations -- fixed
// vocabularies like server_error, rate_limit_exceeded, max_output_tokens,
// content_filter -- which makes them safe to persist. The provider's free-text
// message is deliberately not included: it can quote the prompt back, and this string
// is written to durable activity. See redactProviderError for the same reasoning
// applied to API errors.
func responseReason(r *responses.Response) string {
	var out string
	if code := string(r.Error.Code); code != "" {
		out = "the provider reported " + code
	}
	if reason := string(r.IncompleteDetails.Reason); reason != "" {
		out = join(out, "generation stopped because of "+reason)
	}
	if out == "" {
		// Neither field was populated, so the status is all there is to report.
		return "the provider reported status " + string(r.Status)
	}
	return out
}

// redactProviderError renders an OpenAI error for durable storage, keeping its
// classification and dropping its body.
//
// The SDK's Error.Error() embeds the raw JSON response body, and for a request
// rejected on its content -- an invalid prompt, a content policy refusal -- that body
// can quote the prompt. Persisting it verbatim would put caller content into
// throttle's activity store through the error field, which is the one thing the
// privacy boundary exists to prevent.
//
// So an API error is reduced to what an operator actually needs to act: the HTTP
// status, and the type, code, and parameter OpenAI classified it as. All three are
// fixed vocabularies. Any other error -- a transport failure, a DNS error -- is
// carrying no provider payload and is recorded as-is.
//
// A streaming error gets the same treatment for a stronger reason: the SDK's
// StreamError carries the raw SSE frame in both of its fields -- its Message
// interpolates the raw error JSON, and its Event.Data is the frame verbatim -- so
// neither can be persisted. Nothing structured survives that reduction, because the
// SDK does not parse the frame into fields; saying so plainly is better than
// recording a payload that may quote the prompt.
func redactProviderError(err error) string {
	var streamErr *ssestream.StreamError
	if errors.As(err, &streamErr) {
		return "the provider's event stream reported an error (the provider's message is not recorded: it carries the raw stream payload)"
	}

	var apiErr *oai.Error
	if !errors.As(err, &apiErr) {
		return err.Error()
	}
	out := fmt.Sprintf("the provider returned HTTP %d", apiErr.StatusCode)
	if apiErr.Type != "" {
		out += " (" + apiErr.Type
		if apiErr.Code != "" {
			out += "/" + apiErr.Code
		}
		if apiErr.Param != "" {
			out += " on " + apiErr.Param
		}
		out += ")"
	}
	return out
}
