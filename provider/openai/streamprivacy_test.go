package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/activity"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/reconcile"
	"github.com/scttfrdmn/throttle/report"
	"github.com/scttfrdmn/throttle/usage"
)

// 26, 27, 28. A stream carries every kind of content throttle must not persist, and it
// carries it in the one form that is hardest to resist: already decoded, already in
// memory, arriving through throttle's own code on its way to the caller.
//
// Checked by serializing the whole durable record and searching it, rather than by
// asserting on the fields the adapter happens to set -- a leak would arrive in a field
// nobody thought to check, and an accumulator built "just for accounting" would be
// exactly that.
func TestStreamPersistsNoContent(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		// Output text, delta by delta and then whole.
		deltaEvent(t, 1, outputText),
		event(t, fmt.Sprintf(`{"type": "response.output_text.done", "sequence_number": 2,
			"item_id": "msg_1", "output_index": 0, "content_index": 0, "text": %q}`, outputText)),
		// Reasoning, as summary and as content.
		event(t, fmt.Sprintf(`{"type": "response.reasoning_summary_text.delta", "sequence_number": 3,
			"item_id": "rs_1", "output_index": 0, "summary_index": 0, "delta": %q}`, reasoningText)),
		event(t, fmt.Sprintf(`{"type": "response.reasoning_text.done", "sequence_number": 4,
			"item_id": "rs_1", "output_index": 0, "content_index": 0, "text": %q}`, reasoningText)),
		// A refusal.
		event(t, fmt.Sprintf(`{"type": "response.refusal.done", "sequence_number": 5,
			"item_id": "msg_1", "output_index": 0, "content_index": 0, "refusal": %q}`, outputText)),
		// Function call arguments.
		event(t, fmt.Sprintf(`{"type": "response.function_call_arguments.done", "sequence_number": 6,
			"item_id": "fc_1", "output_index": 1, "arguments": %q, "name": "lookup_balance"}`, toolArgsText)),
		// MCP call arguments.
		event(t, fmt.Sprintf(`{"type": "response.mcp_call_arguments.done", "sequence_number": 7,
			"item_id": "mcp_1", "output_index": 2, "arguments": %q}`, toolArgsText)),
		// A code interpreter's input.
		event(t, fmt.Sprintf(`{"type": "response.code_interpreter_call_code.done", "sequence_number": 8,
			"item_id": "ci_1", "output_index": 3, "code": %q}`, structuredTxt)),
		// A completed output item, carrying a tool result fed back in.
		event(t, fmt.Sprintf(`{"type": "response.output_item.done", "sequence_number": 9,
			"output_index": 1, "item": {"type": "function_call", "id": "fc_1", "call_id": "call_1",
			"name": "lookup_balance", "arguments": %q, "status": "completed"}}`, toolResultText)),
		// An annotation quoting a source document.
		event(t, fmt.Sprintf(`{"type": "response.output_text.annotation.added", "sequence_number": 10,
			"item_id": "msg_1", "output_index": 0, "content_index": 0, "annotation_index": 0,
			"annotation": {"type": "file_citation", "file_id": "file_1", "filename": %q, "index": 0}}`, fileText)),
		// And a terminal Response carrying the generated output in full, which is the
		// single richest source of content in the whole stream.
		event(t, fmt.Sprintf(`{"type": "response.completed", "sequence_number": 11,
			"response": {"id": "resp_privacy", "object": "response", "status": "completed", "model": %q,
			"instructions": %q,
			"output": [
				{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
				 "content": [{"type": "output_text", "text": %q, "annotations": []}]},
				{"type": "reasoning", "id": "rs_1",
				 "summary": [{"type": "summary_text", "text": %q}],
				 "encrypted_content": "gAAAAABm-encrypted-reasoning-payload"},
				{"type": "function_call", "id": "fc_1", "call_id": "call_1",
				 "name": "lookup_balance", "arguments": %q, "status": "completed"}
			],
			"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500,
				"output_tokens_details": {"reasoning_tokens": 200}}}}`,
			gpt51, instructionTxt, outputText, reasoningText, toolArgsText)),
	}

	h := newStreamHarness(t, "1000", events)

	// The request carries content too, and the caller's own metadata must survive while
	// the content does not.
	in := request(gpt51, maxOut(2000))
	in.Instructions = param.NewOpt(instructionTxt)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-privacy", Params: in,
		Metadata: map[string]string{"workload": "nightly-report"},
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	// The caller reads every event, which is the whole point: throttle forwarded all of
	// this content and persisted none of it.
	drain(t, s)
	s.Close()

	if !s.Result().Settled {
		t.Fatalf("the stream should have settled: Err = %v", s.Err())
	}

	rec := h.record(t, "stream-privacy")
	blob, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	for _, forbidden := range []string{
		promptText, instructionTxt, outputText, reasoningText,
		toolArgsText, toolResultText, structuredTxt, fileText,
		"encrypted-reasoning-payload", "lookup_balance", "summary_text",
	} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("streamed content reached durable storage: %q", forbidden)
		}
	}

	// What does survive is accounting: the counts, the identity, and the caller's own
	// attribution.
	if got := tokens(rec.ActualUsage, usage.ReasoningTokens); got != 200 {
		t.Errorf("recorded reasoning tokens = %d, want 200: a count is accounting, not content", got)
	}
	if rec.Metadata["workload"] != "nightly-report" {
		t.Errorf("caller metadata = %v, want the workload attribution preserved", rec.Metadata)
	}
	if rec.Metadata["openai.response_id"] != "resp_privacy" {
		t.Errorf("response id = %q, want the provider's own identifier recorded",
			rec.Metadata["openai.response_id"])
	}
}

// The activity store never sees a stream event, however many go past. A durable record
// per request, not a log of the stream.
//
// Checked by counting writes rather than by inspecting them: an implementation that
// wrote one row per event would still pass a content search if the events happened to
// be content-free, and the invariant is about the shape of durable state.
func TestStreamDoesNotBecomeAnEventLog(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{createdEvent(t, gpt51)}
	for i := 1; i <= 50; i++ {
		events = append(events, deltaEvent(t, i, fmt.Sprintf("word-%d ", i)))
	}
	events = append(events, completedEvent(t, gpt51, 1000, 500))

	counting := &countingActivityStore{}
	h := newStreamHarness(t, "1000", events,
		func(c *openai.Config) { c.Activity = counting })

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-eventlog", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	if n := drain(t, s); n != len(events) {
		t.Fatalf("received %d events, want %d", n, len(events))
	}
	s.Close()

	// Two writes: the pre-call record that makes the reservation recoverable, and the
	// terminal one that settles it. Not fifty-two.
	if got := counting.writes(); got != 2 {
		t.Errorf("the activity store was written %d times for a %d-event stream, want 2: "+
			"throttle records requests, not stream events", got, len(events))
	}
	// And the record carries no per-event structure at all.
	blob, err := json.Marshal(counting.last())
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	for _, forbidden := range []string{"word-1", "word-50", "sequence_number"} {
		if strings.Contains(string(blob), forbidden) {
			t.Errorf("the durable record carries stream-event detail: %q", forbidden)
		}
	}
}

// countingActivityStore records how many times a record was written, so a test can
// assert on the shape of durable state rather than only its content.
type countingActivityStore struct {
	mu      sync.Mutex
	n       int
	records map[string]activity.Record
	recent  activity.Record
}

func (c *countingActivityStore) Begin(ctx context.Context, rec activity.Record) error {
	return c.write(rec)
}

func (c *countingActivityStore) Complete(ctx context.Context, rec activity.Record) error {
	return c.write(rec)
}

func (c *countingActivityStore) write(rec activity.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	if c.records == nil {
		c.records = map[string]activity.Record{}
	}
	c.records[rec.RequestID] = rec
	c.recent = rec
	return nil
}

func (c *countingActivityStore) Get(ctx context.Context, requestID string) (activity.Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rec, ok := c.records[requestID]
	if !ok {
		return activity.Record{}, activity.ErrNotFound
	}
	return rec, nil
}

func (c *countingActivityStore) List(ctx context.Context, f activity.Filter) ([]activity.Record, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]activity.Record, 0, len(c.records))
	for _, rec := range c.records {
		out = append(out, rec)
	}
	return out, nil
}

func (c *countingActivityStore) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *countingActivityStore) last() activity.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recent
}

// 33. A crash after the terminal usage is durable, but before settlement finishes, is
// recoverable from the durable record alone: no stream to replay, no OpenAI client, no
// catalog.
//
// The reconciler is built with a ledger and an activity store and nothing else, which
// is the structural half of the guarantee -- it could not consult a provider if it
// wanted to.
func TestStrandedStreamReconcilesWithoutReplayingTheStream(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	// A stream established and then abandoned mid-flight: the hold stands, the pre-call
	// record is durable, and the frozen quote is in it. Exactly the state a crash leaves.
	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-stranded", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	if !s.Next() {
		t.Fatal("the first event should have arrived")
	}
	s.Close()

	before := h.record(t, "stream-stranded")
	if before.Status != activity.StatusOutstanding {
		t.Fatalf("status = %q, want %q: this test needs a stranded record to repair",
			before.Status, activity.StatusOutstanding)
	}
	if !before.Quote.Valid() {
		t.Fatal("the frozen quote must be durable, or nothing can re-price this request")
	}

	rec, err := reconcile.New(reconcile.Config{
		Ledger:   h.ledger,
		Activity: h.activity,
		Clock:    h.clock,
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}

	out, err := rec.Reconcile(context.Background(), "stream-stranded")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	switch out.Class {
	case reconcile.ClassUnresolved, reconcile.ClassRepaired:
		// Either is a legitimate verdict on a stranded stream. What matters is that a
		// definite, provider-neutral one was reached from durable facts.
	default:
		t.Errorf("Class = %q, want unresolved or repaired", out.Class)
	}
	if out.Class == reconcile.ClassFailed {
		t.Error("reconciliation failed, which means the durable record was not sufficient")
	}

	// Nothing re-read the stream, and nothing called OpenAI.
	if got := h.stream.callCount(); got != 1 {
		t.Errorf("the provider was called %d times: reconciliation must not re-call OpenAI", got)
	}
	if got := h.up.readCount(); got > 2 {
		t.Errorf("the stream was read %d times: reconciliation must not replay stream events", got)
	}
}

// 34. A stream that disappeared with no terminal usage stays provider-outcome-unknown
// through reconciliation. The reconciler must not turn "we do not know" into "it was
// free" merely because a hold expired.
func TestReconciliationKeepsAnUnknownStreamOutcomeUnknown(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		deltaEvent(t, 1, outputText),
		// And then nothing: no terminal Response.
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-vanished", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	rec, err := reconcile.New(reconcile.Config{
		Ledger: h.ledger, Activity: h.activity, Clock: h.clock,
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}
	if _, err := rec.Reconcile(context.Background(), "stream-vanished"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after := h.record(t, "stream-vanished")
	if after.ActualCost.Known() {
		t.Errorf("cost = %s after reconciliation: an unknown outcome must not become a "+
			"known one, and least of all a zero", after.ActualCost.Amount)
	}
	if after.Status == activity.StatusSettled {
		t.Error("a stream that reported no usage must not reconcile to settled")
	}
	// And no content survived the round trip through reconciliation either.
	blob, err := json.Marshal(after)
	if err != nil {
		t.Fatalf("marshalling the record: %v", err)
	}
	if strings.Contains(string(blob), outputText) {
		t.Error("streamed content reached the durable record via reconciliation")
	}
}

// 35. A crash on a stream whose served tier was never captured stays pricing-unresolved
// through reconciliation. The reconciler has no catalog, so it cannot invent the rate
// the adapter already declined to invent.
func TestReconciliationKeepsAnUncapturedTierUnresolved(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		event(t, fmt.Sprintf(`{"type": "response.completed", "sequence_number": 9,
			"response": {"id": "r", "object": "response", "status": "completed", "model": %q,
			"service_tier": "turbo-2027",
			"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}}}`, gpt51)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-tier-crash", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	if got := h.record(t, "stream-tier-crash").Status; got != activity.StatusUnresolved {
		t.Fatalf("status = %q, want %q before reconciliation", got, activity.StatusUnresolved)
	}

	rec, err := reconcile.New(reconcile.Config{
		Ledger: h.ledger, Activity: h.activity, Clock: h.clock,
	})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}
	if _, err := rec.Reconcile(context.Background(), "stream-tier-crash"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	after := h.record(t, "stream-tier-crash")
	if after.ActualCost.Known() {
		t.Errorf("cost = %s: the rate for the tier that served this request was never "+
			"captured, and reconciliation has no catalog to look it up in", after.ActualCost.Amount)
	}
	// The usage survives, so a human or a later true-up has everything but the rate.
	if got := tokens(after.ActualUsage, usage.InputTokens); got != 1000 {
		t.Errorf("recorded input tokens = %d, want 1000: the tokens are known even though "+
			"the price is not", got)
	}
	if after.Identity.ServiceTier != "turbo-2027" {
		t.Errorf("recorded tier = %q, want the tier that served the request", after.Identity.ServiceTier)
	}
}

// The two streaming timings the schema already carried are actually populated, on a
// real clock where they can be nonzero.
//
// Separate from the reporting test above because that one runs on the frozen harness
// clock, where every duration is legitimately zero and an assertion on one would pass
// for the wrong reason. Neither figure is compared against a wall-clock bound: what is
// under test is that the fields are written, not how fast the fake provider is.
func TestStreamRecordsItsTimings(t *testing.T) {
	h := newStreamHarnessWithLease(t, "1000", time.Minute,
		normalEventStream(t, gpt51, 1000, 500), generousStall)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-timings", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	rec := h.record(t, "stream-timings")
	if rec.StreamEstablished <= 0 {
		t.Error("StreamEstablished should record how long the provider took to hand back a stream")
	}
	if rec.StreamFirstEvent <= 0 {
		t.Error("StreamFirstEvent should record how long until the first event arrived")
	}
	// First event cannot precede establishment, and neither can outlast the request.
	if rec.StreamFirstEvent < rec.StreamEstablished {
		t.Errorf("first event at %s precedes establishment at %s", rec.StreamFirstEvent,
			rec.StreamEstablished)
	}
	if rec.Latency < rec.StreamFirstEvent {
		t.Errorf("total latency %s is less than time to first event %s", rec.Latency,
			rec.StreamFirstEvent)
	}
}

// 39. A streaming record renders through the same read model as any other, with no
// provider-specific case anywhere. A streaming request is an ordinary request that
// happened to arrive in pieces.
func TestStreamingRecordsNeedNoStreamingSpecificReporting(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	// One streaming request and one non-streaming one, on the same budget, priced from
	// the same rates and the same token counts.
	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-report", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	if _, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "plain-report", Params: request(gpt51, maxOut(2000)),
	}); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	rep, err := report.New(report.Config{
		Ledger: h.ledger, Activity: h.activity, Clock: h.clock,
	})
	if err != nil {
		t.Fatalf("report.New: %v", err)
	}

	page, err := rep.Activity(context.Background(), report.ActivityQuery{BudgetID: "team"})
	if err != nil {
		t.Fatalf("report.Activity: %v", err)
	}

	byID := map[string]report.Event{}
	for _, ev := range page.Events {
		byID[ev.RequestID] = ev
	}
	streamed, ok := byID["stream-report"]
	if !ok {
		t.Fatal("the streaming request does not appear in the activity report at all")
	}
	plain, ok := byID["plain-report"]
	if !ok {
		t.Fatal("the non-streaming request is missing from the report")
	}

	// The two events are identical in every accounting field, which is what "no
	// provider-specific streaming case" means concretely: the read model has nothing to
	// branch on, because there is no difference to branch on.
	if streamed.Actual.Value != plain.Actual.Value {
		t.Errorf("streamed cost = %s, non-streamed = %s: the same request in two forms must "+
			"report the same money", streamed.Actual.Value, plain.Actual.Value)
	}
	if streamed.Status != plain.Status {
		t.Errorf("streamed status = %q, non-streamed = %q", streamed.Status, plain.Status)
	}
	if streamed.Outcome != plain.Outcome {
		t.Errorf("streamed outcome = %q, non-streamed = %q", streamed.Outcome, plain.Outcome)
	}
	if streamed.Model != plain.Model || streamed.ModelKnown != plain.ModelKnown {
		t.Errorf("streamed model = %q (known %v), non-streamed = %q (known %v)",
			streamed.Model, streamed.ModelKnown, plain.Model, plain.ModelKnown)
	}
	if streamed.ProviderModelID != plain.ProviderModelID {
		t.Errorf("streamed provider model id = %q, non-streamed = %q",
			streamed.ProviderModelID, plain.ProviderModelID)
	}
	if streamed.AccessProvider != plain.AccessProvider {
		t.Errorf("streamed access provider = %q, non-streamed = %q",
			streamed.AccessProvider, plain.AccessProvider)
	}
	if len(streamed.Usage) != len(plain.Usage) {
		t.Errorf("streamed usage has %d dimensions, non-streamed %d: both are the same "+
			"request", len(streamed.Usage), len(plain.Usage))
	}

	// The operation is the one field that differs, and it is a label rather than a
	// branch: it tells a reader which API served the request without the read model
	// having to know what streaming is.
	if streamed.Operation != "responses-stream" {
		t.Errorf("streamed operation = %q, want responses-stream", streamed.Operation)
	}
	if plain.Operation != "responses" {
		t.Errorf("non-streamed operation = %q, want responses", plain.Operation)
	}

	// A streaming record's timings reach the read model through the fields the schema
	// already had, so the streaming path needed no new column. This asserts the plumbing
	// rather than a figure: the harness clock is frozen, so every duration here is
	// legitimately zero, and a non-streaming request has no first event to report at all.
	if plain.StreamFirstEvent != 0 {
		t.Error("a non-streaming request has no first event, and must report none")
	}

	// And the budget position sees one figure, arrived at by one code path.
	sum, err := rep.Summary(context.Background(), "team")
	if err != nil {
		t.Fatalf("report.Summary: %v", err)
	}
	if want := dollars(t, "0.0125"); sum.Position.Spent != want {
		t.Errorf("Spent = %s, want %s: both requests must land in the same position",
			sum.Position.Spent, want)
	}
	if sum.Health.Unresolved != 0 {
		t.Errorf("Health.Unresolved = %d, want 0: both requests settled cleanly",
			sum.Health.Unresolved)
	}
}
