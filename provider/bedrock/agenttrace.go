package bedrock

import (
	"fmt"
	"strings"
	"time"

	agenttypes "github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime/types"

	"throttle/activity"
	"throttle/pricing"
	"throttle/usage"
)

// Step kinds recorded for an observed model invocation.
//
// They are the provider's own phase names, normalized. A step's kind comes from
// PromptType when the trace supplies it, since that distinguishes a knowledge-base
// response generation from an ordinary orchestration turn; otherwise it comes from
// which trace member the event arrived on, which is always known.
const (
	stepPreProcessing     = "pre-processing"
	stepOrchestration     = "orchestration"
	stepPostProcessing    = "post-processing"
	stepRoutingClassifier = "routing-classifier"

	// stepFailure is a model invocation the agent reported usage for while failing.
	// The tokens were still consumed, so the step exists.
	stepFailure = "failure"
)

// unpriceableActivity are the non-model activities Agents Classic reports without
// any billable quantity.
//
// Each of these really costs money -- a Lambda invocation, a vector-store query, a
// code-interpreter session, a guardrail evaluation -- and the trace carries timing
// only. throttle can therefore observe that they happened and can price none of
// them, and the activity record says so rather than implying they were free.
var unpriceableActivity = map[string]bool{
	"action-group":                  true,
	"action-group-code-interpreter": true,
	"knowledge-base":                true,
	"agent-collaborator":            true,
	"guardrail":                     true,
	"custom-orchestration":          true,
}

// agentAccount is the normalized accounting state of one InvokeAgent turn.
//
// It is the whole privacy boundary for the agent path. Trace events go in; only
// identifiers, usage, cost, and timing come out. No raw agenttypes value is
// retained, and there is no field here that could hold a prompt, a model response,
// reasoning content, an action payload, a retrieved passage, or a collaborator
// message -- so those cannot reach a durable table even by accident.
//
// Only the pump goroutine touches it.
type agentAccount struct {
	region string

	// steps are the observed model invocations in the order they first appeared.
	steps []activity.AgentStep

	// index maps a provider trace ID to its step, which is how the two halves of one
	// invocation are joined: the model ID arrives on the input event and the usage on
	// the output event, and TraceId is the only thing that links them.
	index map[string]int

	events map[string]int

	// version is the agent version the trace reported, which the request does not
	// carry -- the caller names an alias, and the alias resolves to a version.
	version string

	// returnedControl records that the agent handed control back to the caller. It is
	// a successful protocol outcome, not an error.
	returnedControl bool

	// failed and failureCode record that the agent reported a failure trace. The
	// failure *reason* is deliberately not kept: it is service-generated prose that
	// can quote the prompt or the model's output.
	failed      bool
	failureCode int32

	// operationTotal is the provider's own duration for the whole agent invocation,
	// reported on the final response only.
	operationTotal time.Duration
}

func newAgentAccount(region string) *agentAccount {
	return &agentAccount{region: region, index: make(map[string]int, 8)}
}

// observe folds one trace part into the accounting state.
func (a *agentAccount) observe(tp agenttypes.TracePart) {
	if v := deref(tp.AgentVersion); v != "" {
		a.version = v
	}
	collab := deref(tp.CollaboratorName)
	at := derefTime(tp.EventTime)

	switch t := tp.Trace.(type) {
	case *agenttypes.TraceMemberPreProcessingTrace:
		switch p := t.Value.(type) {
		case *agenttypes.PreProcessingTraceMemberModelInvocationInput:
			a.modelInput(collab, stepPreProcessing, p.Value)
		case *agenttypes.PreProcessingTraceMemberModelInvocationOutput:
			a.modelOutput(collab, stepPreProcessing, deref(p.Value.TraceId), p.Value.Metadata, at)
		}

	case *agenttypes.TraceMemberOrchestrationTrace:
		switch p := t.Value.(type) {
		case *agenttypes.OrchestrationTraceMemberModelInvocationInput:
			a.modelInput(collab, stepOrchestration, p.Value)
		case *agenttypes.OrchestrationTraceMemberModelInvocationOutput:
			a.modelOutput(collab, stepOrchestration, deref(p.Value.TraceId), p.Value.Metadata, at)
		case *agenttypes.OrchestrationTraceMemberInvocationInput:
			a.countInvocation(p.Value.InvocationType)
		case *agenttypes.OrchestrationTraceMemberObservation:
			a.observation(p.Value)
			// Rationale is deliberately unhandled: it is the model's reasoning text.
		}

	case *agenttypes.TraceMemberPostProcessingTrace:
		switch p := t.Value.(type) {
		case *agenttypes.PostProcessingTraceMemberModelInvocationInput:
			a.modelInput(collab, stepPostProcessing, p.Value)
		case *agenttypes.PostProcessingTraceMemberModelInvocationOutput:
			a.modelOutput(collab, stepPostProcessing, deref(p.Value.TraceId), p.Value.Metadata, at)
		}

	case *agenttypes.TraceMemberRoutingClassifierTrace:
		switch p := t.Value.(type) {
		case *agenttypes.RoutingClassifierTraceMemberModelInvocationInput:
			a.modelInput(collab, stepRoutingClassifier, p.Value)
		case *agenttypes.RoutingClassifierTraceMemberModelInvocationOutput:
			a.modelOutput(collab, stepRoutingClassifier, deref(p.Value.TraceId), p.Value.Metadata, at)
		case *agenttypes.RoutingClassifierTraceMemberInvocationInput:
			a.countInvocation(p.Value.InvocationType)
		case *agenttypes.RoutingClassifierTraceMemberObservation:
			a.observation(p.Value)
		}

	case *agenttypes.TraceMemberGuardrailTrace:
		// The assessments name the words, topics, and PII a guardrail matched, which
		// is content. Only the fact of an evaluation is counted.
		a.count("guardrail")

	case *agenttypes.TraceMemberCustomOrchestrationTrace:
		// The event carries the orchestration Lambda's prompt text. Counted only.
		a.count("custom-orchestration")

	case *agenttypes.TraceMemberFailureTrace:
		a.failed = true
		if c := t.Value.FailureCode; c != nil {
			a.failureCode = *c
		}
		// A failing step can still have burned tokens, and those are real spend.
		if md := t.Value.Metadata; md != nil && md.Usage != nil {
			a.modelOutput(collab, stepFailure, deref(t.Value.TraceId), md, at)
		}
	}
}

// modelInput records the half of an invocation that names the model.
func (a *agentAccount) modelInput(collab, kind string, in agenttypes.ModelInvocationInput) {
	if t := normalizeEnum(string(in.Type)); t != "" {
		// PromptType is more specific than the trace member it arrived on: a
		// knowledge-base response generation is reported inside an orchestration trace.
		kind = t
	}
	step := a.step(deref(in.TraceId), collab, kind)
	if m := deref(in.FoundationModel); m != "" {
		id := Identify(m, a.region, "")
		id.Operation = OperationInvokeAgent
		step.Identity = id
	}
	// InferenceConfiguration is not recorded: it carries stop sequences, which are
	// caller-authored text.
}

// modelOutput records the half of an invocation that reports what it consumed.
//
// A step reaching here without a matching input event has real usage and no model
// identity. That is left as it is: an unnamed model is unpriceable, which flows into
// the ordinary partial-cost semantics, whereas substituting the agent's configured
// model would be a guess presented as a measurement.
func (a *agentAccount) modelOutput(collab, kind, traceID string, md *agenttypes.Metadata, at time.Time) {
	step := a.step(traceID, collab, kind)
	if md == nil {
		return
	}
	if u := md.Usage; u != nil {
		// Absent is distinct from zero, so a dimension the provider did not mention is
		// not recorded as consumed-nothing.
		if u.InputTokens != nil {
			step.Usage.Add(usage.InputTokens, int64(*u.InputTokens))
		}
		if u.OutputTokens != nil {
			step.Usage.Add(usage.OutputTokens, int64(*u.OutputTokens))
		}
	}
	if md.TotalTimeMs != nil {
		step.Latency = time.Duration(*md.TotalTimeMs) * time.Millisecond
	}
	switch {
	case md.StartTime != nil:
		step.At = md.StartTime.UTC()
	case md.EndTime != nil:
		step.At = md.EndTime.UTC()
	case !at.IsZero() && step.At.IsZero():
		step.At = at
	}
	a.operationTime(md)
}

// observation records what an action, lookup, or collaborator returned -- which is
// to say, records that it happened.
//
// No usage is taken from here even though the payloads carry a Metadata. Those
// metadata objects report timing only, and a collaborator's model spend arrives
// separately as its own trace parts with CollaboratorName set, so reading usage here
// would either invent a number or count the same tokens twice.
func (a *agentAccount) observation(obs agenttypes.Observation) {
	if fr := obs.FinalResponse; fr != nil {
		a.operationTime(fr.Metadata)
	}
}

// operationTime picks up the whole-invocation duration, which the provider reports
// on the final response only.
func (a *agentAccount) operationTime(md *agenttypes.Metadata) {
	if md != nil && md.OperationTotalTimeMs != nil {
		a.operationTotal = time.Duration(*md.OperationTotalTimeMs) * time.Millisecond
	}
}

// step returns the step for a trace ID, creating it on first sight.
//
// A step with no trace ID cannot be joined to anything, so it gets its own entry
// rather than being merged into an unrelated one.
func (a *agentAccount) step(traceID, collab, kind string) *activity.AgentStep {
	if traceID != "" {
		if i, ok := a.index[traceID]; ok {
			s := &a.steps[i]
			if s.Kind == "" || (kind != "" && kind != stepOrchestration && s.Kind == stepOrchestration) {
				s.Kind = kind
			}
			if s.Collaborator == "" {
				s.Collaborator = collab
			}
			return s
		}
	}
	a.steps = append(a.steps, activity.AgentStep{
		Seq:          len(a.steps) + 1,
		Kind:         kind,
		TraceID:      traceID,
		Collaborator: collab,
	})
	if traceID != "" {
		a.index[traceID] = len(a.steps) - 1
	}
	return &a.steps[len(a.steps)-1]
}

func (a *agentAccount) countInvocation(t agenttypes.InvocationType) {
	if k := normalizeEnum(string(t)); k != "" && k != "finish" {
		a.count(k)
	}
}

func (a *agentAccount) count(kind string) {
	if a.events == nil {
		a.events = make(map[string]int, 4)
	}
	a.events[kind]++
}

// observedUsage reports whether any model invocation reported usage at all. It is
// the difference between "the turn spent something measurable" and "throttle knows
// nothing about what this turn cost".
func (a *agentAccount) observedUsage() bool {
	for i := range a.steps {
		if !a.steps[i].Usage.Empty() {
			return true
		}
	}
	return false
}

// aggregate sums every observed step's usage, which is the turn's usage.
func (a *agentAccount) aggregate() usage.Usage {
	var out usage.Usage
	for i := range a.steps {
		for _, d := range a.steps[i].Usage.Dimensions() {
			out.Add(d, a.steps[i].Usage.Count(d))
		}
	}
	return out
}

// components converts the observed steps into the pricing input for one compound
// charge.
func (a *agentAccount) components() []pricing.Component {
	out := make([]pricing.Component, 0, len(a.steps))
	for i := range a.steps {
		out = append(out, pricing.Component{Identity: a.steps[i].Identity, Usage: a.steps[i].Usage})
	}
	return out
}

// detail assembles the durable agent record: identifiers, per-step accounting, event
// counts, and a note naming what could not be priced.
func (a *agentAccount) detail(agentID, aliasID, sessionID string) activity.Agent {
	out := activity.Agent{
		AgentID:   agentID,
		AliasID:   aliasID,
		Version:   a.version,
		SessionID: sessionID,
		Steps:     a.steps,
		Events:    a.events,
	}
	if a.returnedControl {
		out.Events = withEvent(out.Events, "return-control")
	}

	var notes []string
	for kind := range a.events {
		if unpriceableActivity[kind] {
			notes = append(notes,
				"this turn invoked actions, lookups, or guardrails whose provider costs Agents Classic reports without any billable quantity; only foundation-model spend is accounted here")
			break
		}
	}
	if a.returnedControl {
		notes = append(notes,
			"the agent returned control to the caller; a follow-up InvokeAgent is a separate transaction")
	}
	if a.failed {
		notes = append(notes, fmt.Sprintf("the agent reported a failure (code %d)", a.failureCode))
	}
	out.Note = strings.Join(notes, "; ")
	return out
}

func withEvent(m map[string]int, kind string) map[string]int {
	if m == nil {
		m = make(map[string]int, 1)
	}
	m[kind]++
	return m
}

// normalizeEnum turns a provider enum into throttle's lower-hyphen vocabulary:
// "ACTION_GROUP" becomes "action-group". Structural, so an enum value added after
// this code was written still normalizes.
func normalizeEnum(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToLower(strings.ReplaceAll(s, "_", "-"))
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
