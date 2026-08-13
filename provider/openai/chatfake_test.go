package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	openai "github.com/scttfrdmn/throttle/provider/openai"
)

// fakeChat stands in for the OpenAI Chat Completions service.
//
// A separate fake from fakeResponses, because it satisfies a separate interface over a
// separate request and response type. That is the point rather than an inconvenience:
// if one fake could serve both, the adapter would have collapsed the two API families
// into one internal request model, which is the thing #28 exists to avoid.
type fakeChat struct {
	mu sync.Mutex

	// out and err are what the next call returns. Both may be set: a provider can
	// report usage alongside an error, and that usage is billable.
	out *oai.ChatCompletion
	err error

	// block, if non-nil, is waited on before returning, to simulate a slow provider.
	block chan struct{}

	calls  int
	params []oai.ChatCompletionNewParams
}

func (f *fakeChat) New(ctx context.Context, body oai.ChatCompletionNewParams, _ ...option.RequestOption) (*oai.ChatCompletion, error) {
	f.mu.Lock()
	f.calls++
	f.params = append(f.params, body)
	block, out, err := f.block, f.out, f.err
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, err
}

func (f *fakeChat) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeChat) lastParams() oai.ChatCompletionNewParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.params) == 0 {
		return oai.ChatCompletionNewParams{}
	}
	return f.params[len(f.params)-1]
}

// complete builds a ChatCompletion from JSON rather than by assigning struct fields.
//
// Same reason respond does it for a Response, and it matters more here. This usage
// object carries five optional detail counters, and the adapter distinguishes "the
// provider reported zero audio tokens" from "the provider did not mention audio" by
// reading presence metadata that only the SDK's unmarshaller populates. A struct literal
// would leave every presence bit false while setting the values, so the tests would
// exercise a state the wire cannot produce -- and the absent-is-not-zero rule would go
// unchecked.
func complete(t *testing.T, body string) *oai.ChatCompletion {
	t.Helper()
	var c oai.ChatCompletion
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("unmarshalling a completion fixture: %v", err)
	}
	return &c
}

// completion is the ordinary case: one choice that stopped naturally, with plain prompt
// and completion counts and no breakdown.
func completion(t *testing.T, model string, in, out int64) *oai.ChatCompletion {
	t.Helper()
	return complete(t, fmt.Sprintf(`{
		"id": "chatcmpl_test", "object": "chat.completion", "created": 1786000000, "model": %q,
		"choices": [{"index": 0, "finish_reason": "stop",
			"message": {"role": "assistant", "content": "an answer"}}],
		"usage": {"prompt_tokens": %d, "completion_tokens": %d, "total_tokens": %d}
	}`, model, in, out, in+out))
}

// chatRequest builds a Chat Completions request the way a caller would, with the SDK's
// own types and its own helper constructors.
func chatRequest(model string, cap *int64) oai.ChatCompletionNewParams {
	in := oai.ChatCompletionNewParams{
		Model: shared.ChatModel(model),
		Messages: []oai.ChatCompletionMessageParamUnion{
			oai.UserMessage("what is the airspeed velocity of an unladen swallow?"),
		},
	}
	if cap != nil {
		in.MaxCompletionTokens = param.NewOpt(*cap)
	}
	return in
}

// withChat attaches a Chat Completions fake to a harness.
func withChat(api *fakeChat) func(*openai.Config) {
	return func(c *openai.Config) { c.ChatClient = api }
}

// chatHarness is the standard harness plus the Chat Completions fake, so a test can set
// what the provider returns and read the ledger and activity store back.
type chatHarness struct {
	*harness
	chat *fakeChat
}

// newChatHarness builds an enforce-mode budget with both API families configured
// against it.
//
// Both, deliberately. A Chat Completions request and a Responses request against one
// budget have to share one ledger, one engine, and one activity store, and several tests
// below depend on that being literally true rather than merely arranged alike.
func newChatHarness(t *testing.T, allocation string, opts ...func(*openai.Config)) *chatHarness {
	t.Helper()
	api := &fakeChat{}
	h := newHarness(t, allocation, append([]func(*openai.Config){withChat(api)}, opts...)...)
	api.mu.Lock()
	api.out = completion(t, gpt51, 1000, 500)
	api.mu.Unlock()
	return &chatHarness{harness: h, chat: api}
}
