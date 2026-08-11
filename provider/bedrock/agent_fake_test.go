package bedrock_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime"
	agenttypes "github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime/types"

	"throttle/provider/bedrock"
)

// fakeAgentReader implements bedrockagentruntime.ResponseStreamReader, the mocking
// seam the SDK documents for the InvokeAgent event stream.
//
// As with fakeReader, the fake is driven through the SDK's own
// NewInvokeAgentEventStream rather than by implementing bedrock.AgentEventStream
// directly, so the governed path is exercised against the real
// *InvokeAgentEventStream -- its closeOnce, its Err() precedence, its Events()
// delegation.
type fakeAgentReader struct {
	// events is emitted in order and unbuffered, matching the SDK's own reader, so a
	// test can observe genuine backpressure.
	events chan agenttypes.ResponseStream

	// pace, if positive, is waited between events, for a turn slow enough to outlive
	// a lease quantum.
	pace time.Duration

	errMu sync.Mutex
	err   error

	closes atomic.Int32
	done   chan struct{}
	once   sync.Once
}

func newFakeAgentReader() *fakeAgentReader {
	return &fakeAgentReader{
		events: make(chan agenttypes.ResponseStream),
		done:   make(chan struct{}),
	}
}

func (f *fakeAgentReader) Events() <-chan agenttypes.ResponseStream { return f.events }

func (f *fakeAgentReader) Close() error {
	f.closes.Add(1)
	f.once.Do(func() { close(f.done) })
	return nil
}

func (f *fakeAgentReader) Err() error {
	f.errMu.Lock()
	defer f.errMu.Unlock()
	return f.err
}

func (f *fakeAgentReader) setErr(err error) {
	f.errMu.Lock()
	f.err = err
	f.errMu.Unlock()
}

func (f *fakeAgentReader) closeCount() int { return int(f.closes.Load()) }

// emit writes the given events then closes the channel, as the SDK's reader does at
// EOF. It stops early if the stream is closed underneath it, which keeps an
// abandoned-stream test from leaking this goroutine.
func (f *fakeAgentReader) emit(events ...agenttypes.ResponseStream) {
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

// hang produces nothing and never closes the channel: an agent that accepted the
// invocation and then went quiet.
func (f *fakeAgentReader) hang() {}

// emitThenFail writes events, sets a stream error, and closes the channel, which is
// how the SDK reports a mid-stream failure.
func (f *fakeAgentReader) emitThenFail(err error, events ...agenttypes.ResponseStream) {
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

// fakeAgentAPI stands in for the Bedrock Agent Runtime client.
type fakeAgentAPI struct {
	mu sync.Mutex

	// readers are handed out one per call, so a session test can give each turn its
	// own event sequence. The last one repeats once they run out.
	readers []*fakeAgentReader

	// stream overrides readers when a test needs to return something other than an
	// SDK-wrapped reader (a nil stream, say).
	stream bedrock.AgentEventStream
	useRaw bool

	err   error
	calls int

	inputs []*bedrockagentruntime.InvokeAgentInput
}

func (f *fakeAgentAPI) InvokeAgent(_ context.Context, in *bedrockagentruntime.InvokeAgentInput, _ ...func(*bedrockagentruntime.Options)) (bedrock.AgentEventStream, error) {
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
	reader := f.readers[min(f.calls-1, len(f.readers)-1)]
	// The SDK's own constructor, per its doc comment: "This function should only be
	// used for testing and mocking the InvokeAgentEventStream stream within your
	// application."
	return bedrockagentruntime.NewInvokeAgentEventStream(func(es *bedrockagentruntime.InvokeAgentEventStream) {
		es.Reader = reader
	}), nil
}

func (f *fakeAgentAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAgentAPI) input(i int) *bedrockagentruntime.InvokeAgentInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.inputs) {
		return nil
	}
	return f.inputs[i]
}

func (f *fakeAgentAPI) lastInput() *bedrockagentruntime.InvokeAgentInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil
	}
	return f.inputs[len(f.inputs)-1]
}

// withAgent wires a fake agent client into a harness.
func withAgent(api *fakeAgentAPI) func(*bedrock.Config) {
	return func(c *bedrock.Config) { c.AgentClient = api }
}

// agentRequest builds an InvokeAgent input. The prompt text is the same sentinel the
// content assertions look for, so a leak would be caught.
func agentRequest() *bedrockagentruntime.InvokeAgentInput {
	return &bedrockagentruntime.InvokeAgentInput{
		AgentId:      aws.String("AGENT123456"),
		AgentAliasId: aws.String("ALIAS7890"),
		SessionId:    aws.String("session-abc"),
		InputText:    aws.String("what is the airspeed velocity of an unladen swallow?"),
	}
}

// Event constructors, building the real SDK members so the adapter is exercised
// against the same types Bedrock produces.

// chunk is a piece of the agent's answer. It carries response content, which the
// adapter must forward and must never record.
func chunk(text string) agenttypes.ResponseStream {
	return &agenttypes.ResponseStreamMemberChunk{
		Value: agenttypes.PayloadPart{Bytes: []byte(text)},
	}
}

// tracePart wraps a Trace in the envelope the service sends, with the identifiers
// that travel alongside every part.
func tracePart(tr agenttypes.Trace) agenttypes.ResponseStream {
	return &agenttypes.ResponseStreamMemberTrace{
		Value: agenttypes.TracePart{
			AgentId:      aws.String("AGENT123456"),
			AgentAliasId: aws.String("ALIAS7890"),
			AgentVersion: aws.String("7"),
			SessionId:    aws.String("session-abc"),
			EventTime:    aws.Time(now),
			Trace:        tr,
		},
	}
}

// collaboratorTrace is a trace part attributed to a delegated agent.
func collaboratorTrace(name string, tr agenttypes.Trace) agenttypes.ResponseStream {
	ev := tracePart(tr).(*agenttypes.ResponseStreamMemberTrace)
	ev.Value.CollaboratorName = aws.String(name)
	return ev
}

// modelInput is the half of an internal model invocation that names the model. Its
// Text field carries the agent's constructed prompt, and its InferenceConfiguration
// its stop sequences: both are content the adapter must not persist.
func modelInput(traceID, modelID string, pt agenttypes.PromptType) agenttypes.ModelInvocationInput {
	return agenttypes.ModelInvocationInput{
		TraceId:         aws.String(traceID),
		FoundationModel: aws.String(modelID),
		Type:            pt,
		Text:            aws.String("You are a helpful agent. The user asked: what is the airspeed velocity of an unladen swallow?"),
		InferenceConfiguration: &agenttypes.InferenceConfiguration{
			StopSequences: []string{"airspeed velocity"},
		},
	}
}

// usageMeta is the half that reports what the invocation consumed.
func usageMeta(in, out int32) *agenttypes.Metadata {
	return &agenttypes.Metadata{
		Usage:       &agenttypes.Usage{InputTokens: aws.Int32(in), OutputTokens: aws.Int32(out)},
		TotalTimeMs: aws.Int64(1500),
	}
}

// finalMeta is the metadata the service attaches to the final response, carrying the
// whole-operation duration.
func finalMeta() *agenttypes.Metadata {
	return &agenttypes.Metadata{
		StartTime:            aws.Time(now),
		EndTime:              aws.Time(now.Add(9 * time.Second)),
		OperationTotalTimeMs: aws.Int64(9000),
	}
}

// preInput and preOutput are the preprocessing model invocation.
func preInput(traceID, modelID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberPreProcessingTrace{
		Value: &agenttypes.PreProcessingTraceMemberModelInvocationInput{
			Value: modelInput(traceID, modelID, agenttypes.PromptTypePreProcessing),
		},
	})
}

func preOutput(traceID string, in, out int32) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberPreProcessingTrace{
		Value: &agenttypes.PreProcessingTraceMemberModelInvocationOutput{
			Value: agenttypes.PreProcessingModelInvocationOutput{
				TraceId:  aws.String(traceID),
				Metadata: usageMeta(in, out),
				ParsedResponse: &agenttypes.PreProcessingParsedResponse{
					Rationale: aws.String("the user asked about airspeed velocity, which is in scope"),
				},
			},
		},
	})
}

// orchInput and orchOutput are one orchestration model invocation. The output
// carries reasoning content and a raw model response, both of which are content.
func orchInput(traceID, modelID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberOrchestrationTrace{
		Value: &agenttypes.OrchestrationTraceMemberModelInvocationInput{
			Value: modelInput(traceID, modelID, agenttypes.PromptTypeOrchestration),
		},
	})
}

func orchOutput(traceID string, in, out int32) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberOrchestrationTrace{
		Value: &agenttypes.OrchestrationTraceMemberModelInvocationOutput{
			Value: agenttypes.OrchestrationModelInvocationOutput{
				TraceId:     aws.String(traceID),
				Metadata:    usageMeta(in, out),
				RawResponse: &agenttypes.RawResponse{Content: aws.String("hello, the airspeed velocity is about 11 metres per second")},
				ReasoningContent: &agenttypes.ReasoningContentBlockMemberReasoningText{
					Value: agenttypes.ReasoningTextBlock{Text: aws.String("I should look up airspeed velocity")},
				},
			},
		},
	})
}

// orchOutputCollab is an orchestration output attributed to a collaborator agent.
func orchOutputCollab(name, traceID string, in, out int32) agenttypes.ResponseStream {
	ev := orchOutput(traceID, in, out).(*agenttypes.ResponseStreamMemberTrace)
	ev.Value.CollaboratorName = aws.String(name)
	return ev
}

func orchInputCollab(name, traceID, modelID string) agenttypes.ResponseStream {
	ev := orchInput(traceID, modelID).(*agenttypes.ResponseStreamMemberTrace)
	ev.Value.CollaboratorName = aws.String(name)
	return ev
}

// postInput and postOutput are the postprocessing model invocation, and postOutput
// carries the final-response metadata.
func postInput(traceID, modelID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberPostProcessingTrace{
		Value: &agenttypes.PostProcessingTraceMemberModelInvocationInput{
			Value: modelInput(traceID, modelID, agenttypes.PromptTypePostProcessing),
		},
	})
}

func postOutput(traceID string, in, out int32) agenttypes.ResponseStream {
	md := usageMeta(in, out)
	md.OperationTotalTimeMs = aws.Int64(9000)
	return tracePart(&agenttypes.TraceMemberPostProcessingTrace{
		Value: &agenttypes.PostProcessingTraceMemberModelInvocationOutput{
			Value: agenttypes.PostProcessingModelInvocationOutput{
				TraceId:  aws.String(traceID),
				Metadata: md,
				ParsedResponse: &agenttypes.PostProcessingParsedResponse{
					Text: aws.String("hello, the airspeed velocity is about 11 metres per second"),
				},
			},
		},
	})
}

// routingInput and routingOutput are a multi-agent routing classifier invocation.
func routingInput(traceID, modelID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberRoutingClassifierTrace{
		Value: &agenttypes.RoutingClassifierTraceMemberModelInvocationInput{
			Value: modelInput(traceID, modelID, agenttypes.PromptTypeRoutingClassifier),
		},
	})
}

func routingOutput(traceID string, in, out int32) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberRoutingClassifierTrace{
		Value: &agenttypes.RoutingClassifierTraceMemberModelInvocationOutput{
			Value: agenttypes.RoutingClassifierModelInvocationOutput{
				TraceId:  aws.String(traceID),
				Metadata: usageMeta(in, out),
				RawResponse: &agenttypes.RawResponse{
					Content: aws.String("route to the swallow specialist"),
				},
			},
		},
	})
}

// actionInvocation is an action-group call: real provider cost, no billable
// quantity anywhere in the trace.
func actionInvocation(traceID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberOrchestrationTrace{
		Value: &agenttypes.OrchestrationTraceMemberInvocationInput{
			Value: agenttypes.InvocationInput{
				TraceId:        aws.String(traceID),
				InvocationType: agenttypes.InvocationTypeActionGroup,
				ActionGroupInvocationInput: &agenttypes.ActionGroupInvocationInput{
					ActionGroupName: aws.String("swallow-facts"),
					Function:        aws.String("lookupVelocity"),
					Parameters: []agenttypes.Parameter{{
						Name:  aws.String("species"),
						Value: aws.String("unladen swallow"),
					}},
				},
			},
		},
	})
}

// actionObservation is what the action returned. Its metadata reports timing only.
func actionObservation(traceID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberOrchestrationTrace{
		Value: &agenttypes.OrchestrationTraceMemberObservation{
			Value: agenttypes.Observation{
				TraceId: aws.String(traceID),
				Type:    agenttypes.TypeActionGroup,
				ActionGroupInvocationOutput: &agenttypes.ActionGroupInvocationOutput{
					Text:     aws.String("11 metres per second"),
					Metadata: &agenttypes.Metadata{TotalTimeMs: aws.Int64(120)},
				},
			},
		},
	})
}

// kbInvocation and kbObservation are a knowledge-base lookup: the query and the
// retrieved passages are both content.
func kbInvocation(traceID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberOrchestrationTrace{
		Value: &agenttypes.OrchestrationTraceMemberInvocationInput{
			Value: agenttypes.InvocationInput{
				TraceId:        aws.String(traceID),
				InvocationType: agenttypes.InvocationTypeKnowledgeBase,
				KnowledgeBaseLookupInput: &agenttypes.KnowledgeBaseLookupInput{
					KnowledgeBaseId: aws.String("KB123"),
					Text:            aws.String("airspeed velocity of an unladen swallow"),
				},
			},
		},
	})
}

func kbObservation(traceID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberOrchestrationTrace{
		Value: &agenttypes.OrchestrationTraceMemberObservation{
			Value: agenttypes.Observation{
				TraceId: aws.String(traceID),
				Type:    agenttypes.TypeKnowledgeBase,
				KnowledgeBaseLookupOutput: &agenttypes.KnowledgeBaseLookupOutput{
					Metadata: &agenttypes.Metadata{TotalTimeMs: aws.Int64(80)},
					RetrievedReferences: []agenttypes.RetrievedReference{{
						Content: &agenttypes.RetrievalResultContent{
							Text: aws.String("a European swallow cruises at about 11 metres per second"),
						},
					}},
				},
			},
		},
	})
}

// rationale is the model's reasoning text, forwarded to the caller and never
// recorded.
func rationale(traceID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberOrchestrationTrace{
		Value: &agenttypes.OrchestrationTraceMemberRationale{
			Value: agenttypes.Rationale{
				TraceId: aws.String(traceID),
				Text:    aws.String("the user wants the airspeed velocity, so I will look it up"),
			},
		},
	})
}

// guardrailTrace is a guardrail evaluation. Its assessments name the words and
// topics that matched, which is content.
func guardrailTrace(traceID string) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberGuardrailTrace{
		Value: agenttypes.GuardrailTrace{
			TraceId: aws.String(traceID),
			Action:  agenttypes.GuardrailActionIntervened,
			InputAssessments: []agenttypes.GuardrailAssessment{{
				WordPolicy: &agenttypes.GuardrailWordPolicyAssessment{
					CustomWords: []agenttypes.GuardrailCustomWord{{
						Match:  aws.String("airspeed velocity"),
						Action: agenttypes.GuardrailWordPolicyActionBlocked,
					}},
				},
			}},
		},
	})
}

// failureTrace reports a failed turn. Its FailureReason is service prose that can
// quote the prompt or the model's output.
func failureTrace(traceID string, md *agenttypes.Metadata) agenttypes.ResponseStream {
	return tracePart(&agenttypes.TraceMemberFailureTrace{
		Value: agenttypes.FailureTrace{
			TraceId:       aws.String(traceID),
			FailureCode:   aws.Int32(424),
			FailureReason: aws.String("the model could not answer: what is the airspeed velocity of an unladen swallow?"),
			Metadata:      md,
		},
	})
}

// returnControl is the agent handing control back to the caller. Its invocation
// inputs carry the action parameters, which are content.
func returnControl() agenttypes.ResponseStream {
	return &agenttypes.ResponseStreamMemberReturnControl{
		Value: agenttypes.ReturnControlPayload{
			InvocationId: aws.String("invoke-xyz"),
			InvocationInputs: []agenttypes.InvocationInputMember{
				&agenttypes.InvocationInputMemberMemberFunctionInvocationInput{
					Value: agenttypes.FunctionInvocationInput{
						ActionGroup: aws.String("swallow-facts"),
						Function:    aws.String("lookupVelocity"),
						Parameters: []agenttypes.FunctionParameter{{
							Name:  aws.String("species"),
							Value: aws.String("unladen swallow"),
						}},
					},
				},
			},
		},
	}
}

// normalAgentTurn is the ordinary sequence: preprocessing, one orchestration step,
// an action, a second orchestration step, and postprocessing. Four model
// invocations across three phases, all beneath one transaction.
func normalAgentTurn(modelID string) []agenttypes.ResponseStream {
	return []agenttypes.ResponseStream{
		preInput("t-pre", modelID), preOutput("t-pre", 400, 40),
		orchInput("t-orch-1", modelID), rationale("t-orch-1"), orchOutput("t-orch-1", 1200, 180),
		actionInvocation("t-orch-1"), actionObservation("t-orch-1"),
		orchInput("t-orch-2", modelID), orchOutput("t-orch-2", 1800, 220),
		chunk("the airspeed velocity is about 11 metres per second"),
		postInput("t-post", modelID), postOutput("t-post", 600, 90),
	}
}

// drainAgent reads a stream to completion, returning the events in order.
func drainAgent(t *testing.T, s *bedrock.AgentStream) []agenttypes.ResponseStream {
	t.Helper()
	var got []agenttypes.ResponseStream
	for ev := range s.Events() {
		got = append(got, ev)
	}
	return got
}

// errAgentStream is an agent stream error: something failed mid-turn.
var errAgentStream = errors.New("InternalServerException: the agent invocation failed")
