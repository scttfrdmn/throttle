package bedrock_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"throttle/provider/bedrock"
)

// fakeReader implements bedrockruntime.ConverseStreamOutputReader, which is the
// mocking seam the SDK itself documents for event streams.
//
// Driving the fake through the SDK's own NewConverseStreamEventStream, rather than
// implementing bedrock.EventStream directly, is deliberate: it proves the governed
// path works against the real *ConverseStreamEventStream -- its closeOnce, its
// Err() precedence, its Events() delegation -- rather than against throttle's idea
// of what that type does.
type fakeReader struct {
	// events is emitted in order. Unbuffered on purpose, matching the SDK's own
	// reader, so a test can observe genuine backpressure.
	events chan brtypes.ConverseStreamOutput

	// pace, if positive, is waited between events, for tests that need a stream
	// slow enough to outlive a lease quantum.
	pace time.Duration

	// err is what Err() reports after the stream ends: a mid-stream failure.
	errMu sync.Mutex
	err   error

	closes atomic.Int32
	done   chan struct{}
	once   sync.Once
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		events: make(chan brtypes.ConverseStreamOutput),
		done:   make(chan struct{}),
	}
}

func (f *fakeReader) Events() <-chan brtypes.ConverseStreamOutput { return f.events }

// Close must tolerate multiple concurrent calls, per the SDK's interface contract.
func (f *fakeReader) Close() error {
	f.closes.Add(1)
	f.once.Do(func() { close(f.done) })
	return nil
}

func (f *fakeReader) Err() error {
	f.errMu.Lock()
	defer f.errMu.Unlock()
	return f.err
}

func (f *fakeReader) setErr(err error) {
	f.errMu.Lock()
	f.err = err
	f.errMu.Unlock()
}

func (f *fakeReader) closeCount() int { return int(f.closes.Load()) }

// emit writes the given events, then closes the channel, exactly as the SDK's
// reader does at EOF. It stops early if the stream is closed underneath it, which
// is what keeps an abandoned-stream test from leaking this goroutine.
func (f *fakeReader) emit(events ...brtypes.ConverseStreamOutput) {
	go func() {
		defer close(f.events)
		for _, ev := range events {
			if f.pace > 0 {
				select {
				case <-time.After(f.pace):
				case <-f.done:
					return
				}
			}
			select {
			case f.events <- ev:
			case <-f.done:
				return
			}
		}
	}()
}

// hang produces nothing and never closes the channel, standing in for a provider
// that accepted the request and then went quiet. Only something on throttle's side
// -- a deadline, a cancel, a Close, the stall bound -- can end such a stream.
func (f *fakeReader) hang() {}

// emitThenFail writes events, sets a stream error, and closes the channel. This is
// how the SDK reports a mid-stream failure: the channel closes and Err() becomes
// non-nil.
func (f *fakeReader) emitThenFail(err error, events ...brtypes.ConverseStreamOutput) {
	go func() {
		defer close(f.events)
		for _, ev := range events {
			select {
			case f.events <- ev:
			case <-f.done:
				return
			}
		}
		f.setErr(err)
	}()
}

// fakeStreamAPI stands in for the streaming half of the Bedrock Runtime client.
type fakeStreamAPI struct {
	mu sync.Mutex

	// reader is handed to the next call, wrapped in a real SDK event stream.
	reader *fakeReader

	// stream overrides reader when a test needs to return something other than an
	// SDK-wrapped reader (a nil stream, say).
	stream bedrock.EventStream
	useRaw bool

	err   error
	calls int

	inputs []*bedrockruntime.ConverseStreamInput
}

func (f *fakeStreamAPI) ConverseStream(_ context.Context, in *bedrockruntime.ConverseStreamInput, _ ...func(*bedrockruntime.Options)) (bedrock.EventStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	if f.useRaw {
		return f.stream, nil
	}
	// The SDK's own constructor, per its doc comment: "This function should only be
	// used for testing and mocking the ConverseStreamEventStream stream within your
	// application."
	reader := f.reader
	return bedrockruntime.NewConverseStreamEventStream(func(es *bedrockruntime.ConverseStreamEventStream) {
		es.Reader = reader
	}), nil
}

func (f *fakeStreamAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeStreamAPI) lastInput() *bedrockruntime.ConverseStreamInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil
	}
	return f.inputs[len(f.inputs)-1]
}

// Event constructors. These build the real SDK event members, so the adapter is
// exercised against the same types Bedrock produces.

func msgStart() brtypes.ConverseStreamOutput {
	return &brtypes.ConverseStreamOutputMemberMessageStart{
		Value: brtypes.MessageStartEvent{Role: brtypes.ConversationRoleAssistant},
	}
}

func blockStart(i int32) brtypes.ConverseStreamOutput {
	return &brtypes.ConverseStreamOutputMemberContentBlockStart{
		Value: brtypes.ContentBlockStartEvent{ContentBlockIndex: aws.Int32(i)},
	}
}

func delta(text string) brtypes.ConverseStreamOutput {
	return &brtypes.ConverseStreamOutputMemberContentBlockDelta{
		Value: brtypes.ContentBlockDeltaEvent{
			ContentBlockIndex: aws.Int32(0),
			Delta:             &brtypes.ContentBlockDeltaMemberText{Value: text},
		},
	}
}

func blockStop(i int32) brtypes.ConverseStreamOutput {
	return &brtypes.ConverseStreamOutputMemberContentBlockStop{
		Value: brtypes.ContentBlockStopEvent{ContentBlockIndex: aws.Int32(i)},
	}
}

func msgStop() brtypes.ConverseStreamOutput {
	return &brtypes.ConverseStreamOutputMemberMessageStop{
		Value: brtypes.MessageStopEvent{StopReason: brtypes.StopReasonEndTurn},
	}
}

// metadata is the terminal event that carries the only authoritative usage a
// stream ever reports.
func metadata(in, out int32) brtypes.ConverseStreamOutput {
	return &brtypes.ConverseStreamOutputMemberMetadata{
		Value: brtypes.ConverseStreamMetadataEvent{
			Usage: &brtypes.TokenUsage{
				InputTokens:  aws.Int32(in),
				OutputTokens: aws.Int32(out),
				TotalTokens:  aws.Int32(in + out),
			},
			Metrics: &brtypes.ConverseStreamMetrics{LatencyMs: aws.Int64(4321)},
		},
	}
}

// metadataTier is metadata reporting the service tier that actually served the
// call, which may differ from the one requested and may price differently.
func metadataTier(in, out int32, tier brtypes.ServiceTierType) brtypes.ConverseStreamOutput {
	ev := metadata(in, out).(*brtypes.ConverseStreamOutputMemberMetadata)
	ev.Value.ServiceTier = &brtypes.ServiceTier{Type: tier}
	return ev
}

// metadataUsage is metadata reporting an arbitrary usage map, for exercising a
// dimension the captured quote has no rate for.
func metadataUsage(tu *brtypes.TokenUsage) brtypes.ConverseStreamOutput {
	return &brtypes.ConverseStreamOutputMemberMetadata{
		Value: brtypes.ConverseStreamMetadataEvent{
			Usage:   tu,
			Metrics: &brtypes.ConverseStreamMetrics{LatencyMs: aws.Int64(4321)},
		},
	}
}

// normalStream is the ordinary event sequence Bedrock emits.
func normalStream(in, out int32) []brtypes.ConverseStreamOutput {
	return []brtypes.ConverseStreamOutput{
		msgStart(), blockStart(0), delta("the airspeed velocity"), delta(" of an unladen swallow"),
		blockStop(0), msgStop(), metadata(in, out),
	}
}

func streamRequest(modelID string, maxTokens *int32) *bedrockruntime.ConverseStreamInput {
	in := &bedrockruntime.ConverseStreamInput{
		ModelId: aws.String(modelID),
		Messages: []brtypes.Message{{
			Role:    brtypes.ConversationRoleUser,
			Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: "what is the airspeed velocity of an unladen swallow?"}},
		}},
	}
	if maxTokens != nil {
		in.InferenceConfig = &brtypes.InferenceConfiguration{MaxTokens: maxTokens}
	}
	return in
}

// withStream wires a fake streaming client into a harness, returning both.
func withStream(api *fakeStreamAPI) func(*bedrock.Config) {
	return func(c *bedrock.Config) { c.StreamClient = api }
}

// drain reads a stream to completion, returning the events in order.
func drain(t *testing.T, s *bedrock.Stream) []brtypes.ConverseStreamOutput {
	t.Helper()
	var got []brtypes.ConverseStreamOutput
	for ev := range s.Events() {
		got = append(got, ev)
	}
	return got
}

// textOf extracts a delta's text, used only to assert incremental delivery.
func textOf(ev brtypes.ConverseStreamOutput) string {
	d, ok := ev.(*brtypes.ConverseStreamOutputMemberContentBlockDelta)
	if !ok {
		return ""
	}
	t, ok := d.Value.Delta.(*brtypes.ContentBlockDeltaMemberText)
	if !ok {
		return ""
	}
	return t.Value
}

// errStream is a stream error indistinguishable from a real one for accounting
// purposes: something failed mid-response.
var errStream = errors.New("ModelStreamErrorException: the model stream failed")
