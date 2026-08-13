package openai

import (
	"context"
	"errors"
	"fmt"
	"sort"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/scttfrdmn/throttle/activity"
)

// ChatCompletions adapts a real OpenAI client to the ChatCompletionsAPI interface.
//
// A thin wrapper because ChatCompletionService is a struct value, not a pointer, so it
// cannot satisfy an interface with a pointer-receiver method directly. Nothing is
// added: the call, its parameters, and its options pass straight through.
func ChatCompletions(c *oai.Client) ChatCompletionsAPI { return chatClient{c: c} }

type chatClient struct{ c *oai.Client }

func (r chatClient) New(ctx context.Context, body oai.ChatCompletionNewParams, opts ...option.RequestOption) (*oai.ChatCompletion, error) {
	return r.c.Chat.Completions.New(ctx, body, opts...)
}

// ErrCompletionStopped means generation ended for a reason other than reaching a
// natural stopping point.
//
// Reported so a caller cannot mistake a truncated or filtered answer for a complete
// one. It is emphatically not a refund, and that distinction is the whole content of
// this file: a finish reason describes what happened to the *generation*, and says
// nothing about the monetary outcome. Whenever the completion carries authoritative
// usage, that usage is charged -- length, content_filter, and tool_calls included.
var ErrCompletionStopped = errors.New("openai: generation stopped before a natural stop")

// finishReasons collects the distinct finish reasons across a completion's choices.
//
// Across choices, plural, because n > 1 produces several and they can differ: one
// choice may stop naturally while another hits the cap. Recording only the first would
// lose that, and it is exactly the sort of thing an operator investigating a truncated
// batch needs to see.
//
// The reasons are a closed vocabulary -- stop, length, tool_calls, content_filter,
// function_call -- which is what makes them safe to persist. Nothing else from a choice
// is read here: not the message content, not a refusal string, not a tool call's name or
// arguments, not the logprobs.
func finishReasons(c *oai.ChatCompletion) []string {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, ch := range c.Choices {
		if ch.FinishReason != "" {
			seen[ch.FinishReason] = true
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// completionFinishedNaturally reports whether every choice reached a natural stop.
//
// An empty finish reason counts as natural: it is not a required field, so its absence
// means the provider said nothing about it, not that something went wrong. A
// tool_calls finish is also natural in the sense that matters here -- the model
// finished its turn by asking for a tool, which is the API working as designed and is
// what an agent loop expects on nearly every iteration.
//
// length and content_filter are the two that are reported to the caller. Both are
// charged when usage exists.
func completionFinishedNaturally(c *oai.ChatCompletion) bool {
	for _, r := range finishReasons(c) {
		switch r {
		case "stop", "tool_calls", "function_call", "":
		default:
			return false
		}
	}
	return true
}

// chatStatusOutcome maps a completion's finish reasons to a durable activity outcome,
// or empty when generation ended naturally.
//
// Everything that is not a natural stop lands on OutcomeProviderError, matching the
// Responses adapter. The vocabulary is deliberately provider-neutral and deliberately
// not extended: a truncated or filtered completion is a provider-side reason the request
// did not deliver what was asked, which is what that outcome means. The specific reason
// goes in Record.Error, where a provider's own classification belongs.
//
// Note what this does *not* do: it does not influence whether the request settles.
// Status and money are decided separately, and by the time this is consulted the charge
// has already been recorded from authoritative usage.
func chatStatusOutcome(c *oai.ChatCompletion) activity.Outcome {
	if c == nil || completionFinishedNaturally(c) {
		return ""
	}
	return activity.OutcomeProviderError
}

// chatStopReason renders why generation stopped, using OpenAI's own closed vocabulary
// and nothing else.
//
// A refusal is the case worth being explicit about. When a model refuses, the finish
// reason is content_filter or the choice carries a refusal string -- and the refusal
// text is model-generated prose about the prompt, so it is never persisted and never
// read here. What is recorded is that generation was filtered, which is the actionable
// fact.
func chatStopReason(c *oai.ChatCompletion) string {
	reasons := finishReasons(c)
	if len(reasons) == 0 {
		return "the provider reported no finish reason"
	}
	return "generation stopped because of " + joinAnd(reasons)
}

// chatStatusError converts a non-natural finish into an error for the caller.
func chatStatusError(c *oai.ChatCompletion) error {
	if c == nil || completionFinishedNaturally(c) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrCompletionStopped, chatStopReason(c))
}
