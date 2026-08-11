package bedrock_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/smithy-go/middleware"

	"throttle/provider/bedrock"
)

// The AgentCore fake is a reader rather than an event source, and that is the SDK's
// shape rather than a simplification: InvokeAgentRuntimeOutput.Response is the raw
// HTTP body, assigned straight from the transport. There is no typed event stream to
// stand in for, no metadata event, and nowhere in the response that a usage figure
// could appear.

// fakeRuntimeBody is a runtime response body under a test's control.
//
// It exists to make the pull-based lifecycle observable: a body can be delivered in
// pieces, stall indefinitely mid-read, or fail part-way through, and the test can
// count how many times it was closed.
type fakeRuntimeBody struct {
	// chunks are returned one per Read, so a test sees several reads rather than one.
	chunks [][]byte

	// failAfter, when >= 0, returns err instead of the chunk at that index.
	failAfter int
	err       error

	// stallAt, when >= 0, blocks at that chunk until the body is closed: a runtime that
	// accepted the invocation and then went quiet.
	stallAt int

	mu     sync.Mutex
	i      int
	closes atomic.Int32
	done   chan struct{}
	once   sync.Once
}

func newFakeRuntimeBody(chunks ...string) *fakeRuntimeBody {
	b := &fakeRuntimeBody{failAfter: -1, stallAt: -1, done: make(chan struct{})}
	for _, c := range chunks {
		b.chunks = append(b.chunks, []byte(c))
	}
	return b
}

func (b *fakeRuntimeBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	i := b.i
	b.mu.Unlock()

	if b.stallAt >= 0 && i == b.stallAt {
		<-b.done
		return 0, errors.New("body closed")
	}
	if b.failAfter >= 0 && i == b.failAfter {
		return 0, b.err
	}
	if i >= len(b.chunks) {
		return 0, io.EOF
	}

	b.mu.Lock()
	b.i++
	b.mu.Unlock()
	n := copy(p, b.chunks[i])
	return n, nil
}

func (b *fakeRuntimeBody) Close() error {
	b.closes.Add(1)
	b.once.Do(func() { close(b.done) })
	return nil
}

func (b *fakeRuntimeBody) closeCount() int { return int(b.closes.Load()) }

// fakeRuntimeAPI stands in for the AgentCore data-plane client.
type fakeRuntimeAPI struct {
	mu sync.Mutex

	// bodies are handed out one per call, so a session test can give each invocation
	// its own response. The last one repeats once they run out.
	bodies []*fakeRuntimeBody

	// out overrides the response metadata. Nil means a 200 echoing the request's
	// session and trace identifiers, which is what the service does.
	out *bedrockagentcore.InvokeAgentRuntimeOutput

	// noResponse omits the body entirely: metadata and nothing to read.
	noResponse bool

	// err refuses the invocation before any runtime runs.
	err error

	// delay is waited before answering, for a test that needs the call in flight.
	delay time.Duration

	calls  []*bedrockagentcore.InvokeAgentRuntimeInput
	optFns int
}

func (f *fakeRuntimeAPI) InvokeAgentRuntime(ctx context.Context, in *bedrockagentcore.InvokeAgentRuntimeInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in)
	f.optFns += len(optFns)
	err := f.err
	delay := f.delay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}

	out := f.response(in)
	if out == nil {
		return nil, nil
	}
	if !f.noResponse {
		out.Response = f.nextBody()
	}
	return out, nil
}

// response builds the metadata half of the answer, echoing the identifiers the
// service echoes and stamping an AWS request ID the way the SDK's middleware does.
func (f *fakeRuntimeAPI) response(in *bedrockagentcore.InvokeAgentRuntimeInput) *bedrockagentcore.InvokeAgentRuntimeOutput {
	f.mu.Lock()
	override := f.out
	f.mu.Unlock()

	var out bedrockagentcore.InvokeAgentRuntimeOutput
	if override != nil {
		out = *override
	} else {
		out = bedrockagentcore.InvokeAgentRuntimeOutput{
			ContentType:      aws.String("application/json"),
			StatusCode:       aws.Int32(200),
			RuntimeSessionId: in.RuntimeSessionId,
			TraceId:          in.TraceId,
		}
	}
	var md middleware.Metadata
	awsmiddleware.SetRequestIDMetadata(&md, "aws-req-abc123")
	out.ResultMetadata = md
	return &out
}

func (f *fakeRuntimeAPI) nextBody() *fakeRuntimeBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return newFakeRuntimeBody("{}")
	}
	if len(f.bodies) == 1 {
		return f.bodies[0]
	}
	b := f.bodies[0]
	f.bodies = f.bodies[1:]
	return b
}

func (f *fakeRuntimeAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRuntimeAPI) lastCall() *bedrockagentcore.InvokeAgentRuntimeInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func withRuntime(api *fakeRuntimeAPI) func(*bedrock.Config) {
	return func(c *bedrock.Config) { c.RuntimeClient = api }
}

// The adapter must satisfy its own declared seam with the real SDK client, which is
// the whole claim RuntimeAPI makes. A compile-time assertion is enough: constructing
// a *bedrockagentcore.Client would need credentials.
var _ bedrock.RuntimeAPI = (*bedrockagentcore.Client)(nil)
