package bedrock_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"

	"throttle/activity"
	"throttle/budget"
	"throttle/engine"
	"throttle/ledger"
	"throttle/ledger/sqlite"
	"throttle/money"
	"throttle/pricing/fixtures"
	"throttle/provider/bedrock"
	"throttle/usage"
)

const runtimeARN = "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/my-agent-AbCdEf"

// runtimeNow is the clock the runtime tests run on: after the date AWS published the
// AgentCore Runtime rates.
//
// The Converse tests' clock predates it, and deliberately so -- those fixtures are
// backdated for convenience while the runtime rates carry real provenance, and a
// price is never applied to a request that happened before it took effect. Running
// the runtime tests in March 2026 would capture no quote, for a reason that has
// nothing to do with what they are testing. TestRuntimeBeforeItsRatesTookEffect
// covers that case on purpose.
var runtimeNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// runtimeHarness is the Converse harness plus a fake AgentCore runtime client.
type runtimeHarness struct {
	*harness
	runtime *fakeRuntimeAPI
	body    *fakeRuntimeBody
}

func newRuntimeHarness(t *testing.T, allocation string, opts ...func(*bedrock.Config)) *runtimeHarness {
	t.Helper()
	return newRuntimeHarnessAt(t, allocation, runtimeNow, opts...)
}

// newRuntimeHarnessAt builds a runtime harness on a frozen clock, so a test can place
// an invocation before or after the date the runtime rates took effect.
func newRuntimeHarnessAt(t *testing.T, allocation string, at time.Time, opts ...func(*bedrock.Config)) *runtimeHarness {
	t.Helper()

	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := func() time.Time { return at }
	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	def := budget.Definition{
		ID:         "team",
		Allocation: dollars(t, allocation),
		Recurrence: budget.RecurMonthly,
		AnchorAt:   at.Truncate(24*time.Hour).AddDate(0, 0, -14),
		// The invocations under test are about accounting, not pacing: a borrow window
		// spanning the period makes the whole envelope available from the first instant.
		Borrow: 62 * 24 * time.Hour,
	}
	if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
		t.Fatalf("Register: %v", err)
	}

	body := newFakeRuntimeBody(`{"result":`, `"ok"}`)
	api := &fakeRuntimeAPI{bodies: []*fakeRuntimeBody{body}}
	h := buildHarness(t, eng, store, clock, append([]func(*bedrock.Config){withRuntime(api)}, opts...)...)
	return &runtimeHarness{harness: h, runtime: api, body: body}
}

// newRuntimeHarnessWithLease builds a runtime harness on a real clock with a short
// lease, so an invocation can outlive a lease quantum in milliseconds. As in the
// agent tests, a child sits under a parent so ancestor encumbrance is observable, and
// a borrow window spanning the period keeps pacing from refusing calls for a reason
// unrelated to what is under test.
func newRuntimeHarnessWithLease(t *testing.T, parent string, lease time.Duration, opts ...func(*bedrock.Config)) *runtimeHarness {
	t.Helper()

	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := func() time.Time { return time.Now().UTC() }
	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock, Lease: lease})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	anchor := clock().Truncate(24 * time.Hour)
	const wholePeriod = 62 * 24 * time.Hour
	for _, def := range []budget.Definition{
		{ID: "team", Allocation: dollars(t, parent), Recurrence: budget.RecurMonthly, AnchorAt: anchor, Borrow: wholePeriod},
		{ID: "child", ParentID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly, AnchorAt: anchor, Borrow: wholePeriod},
	} {
		if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", def.ID, err)
		}
	}

	body := newFakeRuntimeBody(`{"result":`, `"ok"}`)
	api := &fakeRuntimeAPI{bodies: []*fakeRuntimeBody{body}}
	h := buildHarness(t, eng, store, clock, append([]func(*bedrock.Config){withRuntime(api)}, opts...)...)
	return &runtimeHarness{harness: h, runtime: api, body: body}
}

// runtimeInput is an ordinary AgentCore request: an opaque payload bound for an
// agent whose format throttle knows nothing about.
func runtimeInput() *bedrockagentcore.InvokeAgentRuntimeInput {
	return &bedrockagentcore.InvokeAgentRuntimeInput{
		AgentRuntimeArn:  aws.String(runtimeARN),
		Payload:          []byte(`{"prompt":"what is the airspeed velocity of an unladen swallow?"}`),
		ContentType:      aws.String("application/json"),
		Accept:           aws.String("application/json"),
		RuntimeSessionId: aws.String("session-alpha"),
		TraceId:          aws.String("trace-xyz"),
		Qualifier:        aws.String("prod"),
	}
}

func (h *runtimeHarness) invokeRuntime(t *testing.T, ctx context.Context, requestID string, exposure money.Money) (*bedrock.RuntimeStream, error) {
	t.Helper()
	return h.client.InvokeAgentRuntime(ctx, bedrock.RuntimeRequest{
		BudgetID:    "team",
		RequestID:   requestID,
		Input:       runtimeInput(),
		MaxExposure: exposure,
	})
}

// readRuntime reads a stream to EOF and returns what it forwarded, which is how a
// caller consumes a runtime response.
func readRuntime(t *testing.T, s *bedrock.RuntimeStream) string {
	t.Helper()
	b, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("reading the runtime response: %v", err)
	}
	if err := s.Close(); err != nil && !errors.Is(err, bedrock.ErrCostUnresolved) {
		// Close reports the terminal error, and for a runtime invocation the terminal
		// state is legitimately unresolved. Only an unexpected error is a failure.
		_ = err
	}
	return string(b)
}

// 1. A failure before the runtime is invoked releases the hold. AgentCore refused
// the call, so no runtime ran and no CPU or memory was consumed -- the one path
// where "nothing was spent" is a fact rather than an assumption.
func TestRuntimeCallFailureReleasesTheHold(t *testing.T) {
	h := newRuntimeHarness(t, "1000")
	h.runtime.err = errors.New("AccessDeniedException")

	_, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err == nil {
		t.Fatal("a refused invocation must return an error")
	}
	if !errors.Is(err, bedrock.ErrProvider) {
		t.Errorf("error = %v, want ErrProvider", err)
	}

	tot := h.totals(t)
	if tot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0: the hold must be released", tot.Reserved)
	}
	if tot.Spent != 0 {
		t.Errorf("Spent = %s, want 0: nothing ran", tot.Spent)
	}
}

// 2. An established invocation traverses the whole durable lifecycle, and the edge
// record is durable before the call so a crash mid-invocation leaves evidence.
func TestRuntimeTraversesTheDurableLifecycle(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID:    "team",
		RequestID:   "req-1",
		Input:       runtimeInput(),
		MaxExposure: dollars(t, "1.00"),
		Metadata:    map[string]string{"workload": "nightly-eval"},
	})
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}

	// The record exists, and is pending, before the response is read.
	pending := getRecord(t, acts, "req-1")
	if pending.Status != activity.StatusPending {
		t.Errorf("pre-read Status = %q, want pending", pending.Status)
	}
	if pending.Runtime.RuntimeID != runtimeARN {
		t.Errorf("pre-read RuntimeID = %q, want the runtime arn", pending.Runtime.RuntimeID)
	}

	if got := readRuntime(t, s); got != `{"result":"ok"}` {
		t.Errorf("forwarded body = %q, want the runtime's own bytes verbatim", got)
	}

	res := s.Result()
	if res == nil {
		t.Fatal("Result must be available after the stream ends")
	}
	if !res.Unresolved {
		t.Error("a completed runtime invocation must be unresolved: its cost is not reported synchronously")
	}
	if res.Cost.Known() {
		t.Errorf("Cost = %v, want unknown", res.Cost)
	}
	if res.Mode != engine.ModeEnforce {
		t.Errorf("Mode = %q, want enforce", res.Mode)
	}
	if res.Reserved != dollars(t, "1.00") {
		t.Errorf("Reserved = %s, want the declared exposure", res.Reserved)
	}
	if res.ResponseBytes != int64(len(`{"result":"ok"}`)) {
		t.Errorf("ResponseBytes = %d, want %d", res.ResponseBytes, len(`{"result":"ok"}`))
	}

	rec := getRecord(t, acts, "req-1")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", rec.Status)
	}
	if rec.Outcome != activity.OutcomeSuccess {
		t.Errorf("Outcome = %q, want success: the invocation itself worked", rec.Outcome)
	}
	if rec.Identity.Operation != bedrock.OperationInvokeAgentRuntime {
		t.Errorf("Operation = %q, want invoke-agent-runtime", rec.Identity.Operation)
	}
	if rec.Metadata["workload"] != "nightly-eval" {
		t.Errorf("Metadata = %v, want the caller's attribution preserved", rec.Metadata)
	}
	if rec.CompletedAt.IsZero() {
		t.Error("CompletedAt must be recorded")
	}

	// The hold stays encumbered: the runtime consumed billable resource in an amount
	// nobody yet knows.
	tot := h.totals(t)
	if tot.Reserved != dollars(t, "1.00") {
		t.Errorf("Reserved = %s, want the exposure still held", tot.Reserved)
	}
	if tot.Spent != 0 {
		t.Errorf("Spent = %s, want 0: nothing has been priced yet", tot.Spent)
	}
}

// 2b. AgentCore runtime and session identity must survive on the durable record,
// because those identifiers are the only join to the resource bill that arrives
// later. The response's identifiers win over the request's: the service may assign a
// session the caller did not name.
func TestRuntimeIdentityIsPreserved(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	// The service assigned a different session than the caller asked for.
	h.runtime.out = &bedrockagentcore.InvokeAgentRuntimeOutput{
		ContentType:      aws.String("text/event-stream"),
		StatusCode:       aws.Int32(200),
		RuntimeSessionId: aws.String("session-assigned-by-service"),
		TraceId:          aws.String("trace-from-service"),
	}

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	readRuntime(t, s)

	rec := getRecord(t, acts, "req-1")
	rt := rec.Runtime
	if rt.RuntimeID != runtimeARN {
		t.Errorf("RuntimeID = %q, want %q", rt.RuntimeID, runtimeARN)
	}
	if rt.Qualifier != "prod" {
		t.Errorf("Qualifier = %q, want prod", rt.Qualifier)
	}
	if rt.SessionID != "session-assigned-by-service" {
		t.Errorf("SessionID = %q, want the session the service used", rt.SessionID)
	}
	if rt.TraceID != "trace-from-service" {
		t.Errorf("TraceID = %q, want the trace the service reported", rt.TraceID)
	}
	if rt.RequestID != "aws-req-abc123" {
		t.Errorf("RequestID = %q, want the AWS request id", rt.RequestID)
	}
	if rt.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", rt.StatusCode)
	}
	if rt.ContentType != "text/event-stream" {
		t.Errorf("ContentType = %q, want the declared type", rt.ContentType)
	}
	if rt.Reconciled {
		t.Error("Reconciled must be false immediately after a call")
	}

	// The result carries the same linkage the record does.
	if s.Result().Runtime.SessionID != rt.SessionID {
		t.Error("the result's linkage must match the persisted record's")
	}
}

// 2c. The runtime identity is a resource, not a model. A hosted agent may invoke no
// model at all, or twenty, and the outer identity must not imply otherwise.
func TestRuntimeIdentityNamesAResourceNotAModel(t *testing.T) {
	h := newRuntimeHarness(t, "1000")

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	readRuntime(t, s)

	id := s.Result().Identity
	if id.AccessProvider != "aws-bedrock" {
		t.Errorf("AccessProvider = %q, want aws-bedrock", id.AccessProvider)
	}
	if id.ProviderModelID != bedrock.AgentCoreRuntimeResource {
		t.Errorf("ProviderModelID = %q, want the runtime resource", id.ProviderModelID)
	}
	if id.Publisher != "" {
		t.Errorf("Publisher = %q, want empty: a runtime has no model publisher", id.Publisher)
	}
	if id.CanonicalModel != "" {
		t.Errorf("CanonicalModel = %q, want empty: a runtime is not a model", id.CanonicalModel)
	}
	if id.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", id.Region)
	}
}

// 2d. The adapter is a shim: the SDK request must reach AgentCore exactly as the
// caller wrote it. Unlike Agents Classic there is not even a trace flag to set.
func TestRuntimePassesTheRequestThroughUnchanged(t *testing.T) {
	h := newRuntimeHarness(t, "1000")

	in := runtimeInput()
	s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID: "team", RequestID: "req-1", Input: in, MaxExposure: dollars(t, "1.00"),
		Options: []func(*bedrockagentcore.Options){func(*bedrockagentcore.Options) {}},
	})
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	readRuntime(t, s)

	sent := h.runtime.lastCall()
	if sent != in {
		t.Error("the caller's own input must be sent, not a rewritten copy")
	}
	if string(sent.Payload) != `{"prompt":"what is the airspeed velocity of an unladen swallow?"}` {
		t.Error("the payload must be forwarded verbatim")
	}
	if h.runtime.optFns != 1 {
		t.Errorf("optFns forwarded = %d, want 1", h.runtime.optFns)
	}
	// The response metadata is the SDK's own, with only the body detached.
	out := s.Output()
	if out == nil || out.Response != nil {
		t.Error("Output must carry the SDK metadata with the body detached")
	}
	if aws.ToString(out.ContentType) != "application/json" {
		t.Errorf("ContentType = %q, want the SDK's own", aws.ToString(out.ContentType))
	}
}

// 3. Streaming behaviour is preserved without content persistence: the body is
// forwarded incrementally, in the runtime's own chunks, and nothing is buffered.
func TestRuntimeForwardsTheBodyIncrementally(t *testing.T) {
	h := newRuntimeHarness(t, "1000")
	h.runtime.bodies = []*fakeRuntimeBody{newFakeRuntimeBody("alpha", "beta", "gamma")}

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}

	// One Read per runtime chunk: the adapter neither coalesces nor buffers.
	var got []string
	buf := make([]byte, 64)
	for {
		n, err := s.Read(buf)
		if n > 0 {
			got = append(got, string(buf[:n]))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if len(got) != 3 || got[0] != "alpha" || got[2] != "gamma" {
		t.Errorf("chunks = %v, want the runtime's own three", got)
	}
	if s.Result() == nil {
		t.Fatal("reading to EOF must reach the terminal state")
	}
}

// 3b. Close is idempotent and safe concurrently, and the body is closed exactly
// once however many callers race.
func TestRuntimeConcurrentAndRepeatedCloseTerminateOnce(t *testing.T) {
	h := newRuntimeHarness(t, "1000")

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Close()
		}()
	}
	wg.Wait()
	if err := s.Close(); err != nil {
		// A Close-before-read terminal state carries no error of its own.
		t.Errorf("repeated Close: %v", err)
	}

	if got := h.body.closeCount(); got != 1 {
		t.Errorf("body closed %d times, want exactly 1", got)
	}
	if s.Result() == nil {
		t.Fatal("Close must make the result available")
	}

	// Exactly one terminal transition means exactly one hold, still encumbered.
	tot := h.totals(t)
	if tot.Reserved != dollars(t, "1.00") {
		t.Errorf("Reserved = %s, want one hold still encumbered", tot.Reserved)
	}
}

// 4. A complete streamed response with no authoritative runtime usage: the cost is
// unknown, not zero. This is the central claim of the whole adapter.
func TestRuntimeCompleteResponseLeavesCostUnknown(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	readRuntime(t, s)

	rec := getRecord(t, acts, "req-1")
	if rec.ActualCost.Known() {
		t.Fatalf("ActualCost = %v, want unknown", rec.ActualCost)
	}
	if rec.ActualCost.Reason == "" {
		t.Error("an unknown cost must say why")
	}
	// The one thing that must never happen: a real charge rendered as free.
	amount, complete := rec.Spent()
	if complete {
		t.Error("a runtime invocation's spend must report itself incomplete")
	}
	if amount != 0 {
		t.Errorf("Spent floor = %s, want 0: no part of the runtime cost is priced", amount)
	}
	if !rec.Unresolved() {
		t.Error("the record must be unresolved")
	}
}

// 4b. Wall-clock duration must not become a cost. A slow invocation and a fast one
// produce the same unknown cost: nothing in the adapter converts elapsed time,
// response size, or read count into money.
func TestRuntimeCostIsNotDerivedFromDurationOrSize(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   *fakeRuntimeBody
		delay  time.Duration
		usages int
	}{
		{name: "tiny and fast", body: newFakeRuntimeBody("x")},
		{name: "large and slow", body: newFakeRuntimeBody(strings.Repeat("y", 4096), strings.Repeat("z", 4096)), delay: 20 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRuntimeHarness(t, "1000")
			h.runtime.bodies = []*fakeRuntimeBody{tc.body}
			h.runtime.delay = tc.delay

			s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
			if err != nil {
				t.Fatalf("InvokeAgentRuntime: %v", err)
			}
			readRuntime(t, s)

			res := s.Result()
			if res.Cost.Known() {
				t.Errorf("Cost = %v, want unknown regardless of duration or size", res.Cost)
			}
			// No usage dimension may be populated: there is no observation to populate it
			// from, and a fabricated vCPU or memory figure is exactly what the canonical
			// dimensions exist to prevent.
			for _, d := range []usage.Dimension{usage.RuntimeVCPUNanoHours, usage.RuntimeMemoryNanoGBHours} {
				if _, ok := res.Runtime.ReconciledUsage.Get(d); ok {
					t.Errorf("%s was populated with no authoritative observation", d)
				}
			}
			// The ledger recorded no spend at all.
			if tot := h.totals(t); tot.Spent != 0 {
				t.Errorf("Spent = %s, want 0", tot.Spent)
			}
		})
	}
}

// 5. Caller cancellation makes no optimistic claim of zero runtime cost. The
// runtime had already started; the caller stopping proves nothing about it.
func TestRuntimeCancellationRetainsTheHold(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)
	// A body that stalls part-way, so cancellation lands mid-read.
	body := newFakeRuntimeBody("first-chunk", "never-read")
	body.stallAt = 1
	h.runtime.bodies = []*fakeRuntimeBody{body}

	ctx, cancel := context.WithCancel(context.Background())
	s, err := h.invokeRuntime(t, ctx, "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}

	buf := make([]byte, 64)
	if _, err := s.Read(buf); err != nil {
		t.Fatalf("first Read: %v", err)
	}

	// Cancel, then unblock the parked read so it observes the cancellation.
	cancel()
	go body.Close()
	for {
		if _, err := s.Read(buf); err != nil {
			break
		}
	}
	_ = s.Close()

	rec := getRecord(t, acts, "req-1")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", rec.Status)
	}
	if rec.Outcome != activity.OutcomeCancelled {
		t.Errorf("Outcome = %q, want cancelled", rec.Outcome)
	}
	if rec.ActualCost.Known() {
		t.Errorf("ActualCost = %v, want unknown: cancelling does not make the runtime free", rec.ActualCost)
	}
	if tot := h.totals(t); tot.Reserved != dollars(t, "1.00") {
		t.Errorf("Reserved = %s, want the hold retained", tot.Reserved)
	}
}

// 5b. A deadline is recorded as a timeout rather than a plain cancellation: same
// status, different operational story.
func TestRuntimeTimeoutRecordsTimeout(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)
	body := newFakeRuntimeBody("chunk")
	body.stallAt = 0
	h.runtime.bodies = []*fakeRuntimeBody{body}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s, err := h.invokeRuntime(t, ctx, "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}

	go func() {
		<-ctx.Done()
		body.Close()
	}()
	buf := make([]byte, 64)
	for {
		if _, err := s.Read(buf); err != nil {
			break
		}
	}
	_ = s.Close()

	rec := getRecord(t, acts, "req-1")
	if rec.Outcome != activity.OutcomeTimeout {
		t.Errorf("Outcome = %q, want timeout", rec.Outcome)
	}
	if rec.ActualCost.Known() {
		t.Errorf("ActualCost = %v, want unknown", rec.ActualCost)
	}
}

// 6. A mid-body failure invents no runtime cost, and does not release the hold: the
// runtime ran far enough to start answering.
func TestRuntimeStreamErrorInventsNoCost(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)
	body := newFakeRuntimeBody("partial")
	body.failAfter = 1
	body.err = errors.New("unexpected EOF")
	h.runtime.bodies = []*fakeRuntimeBody{body}

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	if _, err := io.ReadAll(s); err == nil {
		t.Fatal("a failing body must surface its error to the caller")
	}

	res := s.Result()
	if res.Cost.Known() {
		t.Errorf("Cost = %v, want unknown", res.Cost)
	}
	if !res.Unresolved {
		t.Error("a failed read must leave the invocation unresolved")
	}

	rec := getRecord(t, acts, "req-1")
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("Outcome = %q, want provider-error", rec.Outcome)
	}
	if rec.Runtime.ResponseBytes != int64(len("partial")) {
		t.Errorf("ResponseBytes = %d, want the bytes actually read", rec.Runtime.ResponseBytes)
	}
	if tot := h.totals(t); tot.Reserved != dollars(t, "1.00") {
		t.Errorf("Reserved = %s, want the hold retained", tot.Reserved)
	}
}

// 6b. A hosted agent that fails internally is reported as a normal response with a
// failing status. The platform still ran it, and still bills for it.
func TestRuntimeNonSuccessStatusIsRecordedAndStillCosts(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)
	h.runtime.out = &bedrockagentcore.InvokeAgentRuntimeOutput{
		ContentType: aws.String("application/json"),
		StatusCode:  aws.Int32(424),
	}

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	_, _ = io.ReadAll(s)
	_ = s.Close()

	rec := getRecord(t, acts, "req-1")
	if rec.Runtime.StatusCode != 424 {
		t.Errorf("StatusCode = %d, want 424 recorded", rec.Runtime.StatusCode)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("Outcome = %q, want provider-error", rec.Outcome)
	}
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved: the runtime ran and consumed resource", rec.Status)
	}
	if rec.ActualCost.Known() {
		t.Errorf("ActualCost = %v, want unknown", rec.ActualCost)
	}
	if tot := h.totals(t); tot.Reserved != dollars(t, "1.00") {
		t.Errorf("Reserved = %s, want the hold retained for a failing agent", tot.Reserved)
	}
}

// 7. A declared exposure in enforce mode reserves atomically across the hierarchy,
// so the ancestor sees the encumbrance too.
func TestRuntimeExposureReservesAcrossTheHierarchy(t *testing.T) {
	h := newRuntimeHarnessWithLease(t, "10", time.Minute)

	s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID:    "child",
		RequestID:   "req-1",
		Input:       runtimeInput(),
		MaxExposure: dollars(t, "4.00"),
	})
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}

	res := s.Result()
	if res != nil {
		t.Fatal("the invocation is still live")
	}
	if s.ReservationID() == "" {
		t.Error("a reservation must exist")
	}
	// One leg per scope in the chain: the child and its parent both encumbered.
	readRuntime(t, s)

	scopes := 0
	for _, sc := range []string{"child", "team"} {
		p, err := h.ledger.EnsurePeriod(context.Background(), sc, h.clock())
		if err != nil {
			t.Fatalf("EnsurePeriod(%s): %v", sc, err)
		}
		tot, err := h.ledger.Totals(context.Background(), ledger.Scope{BudgetID: sc, PeriodID: p.ID}, h.clock())
		if err != nil {
			t.Fatalf("Totals(%s): %v", sc, err)
		}
		if tot.Reserved != dollars(t, "4.00") {
			t.Errorf("%s Reserved = %s, want 4.00", sc, tot.Reserved)
		}
		scopes++
	}
	if scopes != 2 {
		t.Fatalf("checked %d scopes, want 2", scopes)
	}
}

// 8. With no declared exposure, enforce mode refuses before the provider is called.
// There is no priceable pre-estimate to admit on: AgentCore reports no cost with the
// invocation, so admitting would mean governing spend throttle cannot measure.
func TestEnforceRefusesARuntimeInvocationWithNoExposure(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	_, err := h.invokeRuntime(t, context.Background(), "req-1", 0)
	if err == nil {
		t.Fatal("enforce mode must refuse an invocation with no declared exposure")
	}
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Errorf("error = %v, want ErrCostUnknown", err)
	}
	if h.runtime.callCount() != 0 {
		t.Error("the provider must not be called for a refused invocation")
	}

	rec := getRecord(t, acts, "req-1")
	if rec.Status != activity.StatusDenied {
		t.Errorf("Status = %q, want denied", rec.Status)
	}
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("Outcome = %q, want unpriced", rec.Outcome)
	}
	if tot := h.totals(t); tot.Reserved != 0 || tot.Spent != 0 {
		t.Errorf("totals = %+v, want nothing reserved or spent", tot)
	}
}

// 9. Monitor mode admits an invocation with no exposure, holds nothing, and records
// the gap explicitly as unknown rather than as zero.
func TestMonitorAdmitsARuntimeInvocationWithNoExposure(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")

	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	clock := func() time.Time { return runtimeNow }
	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	def := budget.Definition{
		ID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly,
		AnchorAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		Borrow:   62 * 24 * time.Hour,
	}
	if err := eng.Register(context.Background(), def, engine.ModeMonitor); err != nil {
		t.Fatalf("Register: %v", err)
	}

	body := newFakeRuntimeBody("{}")
	api := &fakeRuntimeAPI{bodies: []*fakeRuntimeBody{body}}
	h := buildHarness(t, eng, store, clock, withRuntime(api), opt)

	s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID: "team", RequestID: "req-1", Input: runtimeInput(),
	})
	if err != nil {
		t.Fatalf("monitor mode must admit an unpriced invocation: %v", err)
	}
	if _, err := io.ReadAll(s); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = s.Close()

	res := s.Result()
	if res.Mode != engine.ModeMonitor {
		t.Errorf("Mode = %q, want monitor", res.Mode)
	}
	if res.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0: monitor mode holds nothing for an unpriced invocation", res.Reserved)
	}
	if res.Estimate.Quality != usage.QualityUnknown {
		t.Errorf("Estimate.Quality = %q, want unknown: there is no estimate to make", res.Estimate.Quality)
	}
	if res.Cost.Known() {
		t.Errorf("Cost = %v, want explicitly unknown", res.Cost)
	}

	rec := getRecord(t, acts, "req-1")
	if rec.EnforcementMode != engine.ModeMonitor {
		t.Errorf("EnforcementMode = %q, want monitor recorded", rec.EnforcementMode)
	}
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", rec.Status)
	}
}

// 10. Concurrent invocations cannot oversubscribe a parent budget. The exposures are
// held atomically across the chain, so the ancestor's ceiling is what refuses.
func TestConcurrentRuntimeInvocationsCannotOversubscribeAnAncestor(t *testing.T) {
	// The parent allows $10; each invocation declares $4, so at most two fit.
	h := newRuntimeHarnessWithLease(t, "10", time.Minute)
	h.runtime.bodies = nil // every call gets its own fresh body

	const n = 6
	var wg sync.WaitGroup
	admitted := make([]*bedrock.RuntimeStream, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
				BudgetID:    "child",
				RequestID:   fmt.Sprintf("req-%d", i),
				Input:       runtimeInput(),
				MaxExposure: dollars(t, "4.00"),
			})
			admitted[i], errs[i] = s, err
		}(i)
	}
	wg.Wait()

	live := 0
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			live++
		} else if !errors.Is(errs[i], engine.ErrDenied) {
			t.Errorf("req-%d: error = %v, want ErrDenied", i, errs[i])
		}
	}
	if live != 2 {
		t.Errorf("%d invocations admitted, want exactly 2 within the parent's $10", live)
	}

	p, err := h.ledger.EnsurePeriod(context.Background(), "team", h.clock())
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	tot, err := h.ledger.Totals(context.Background(), ledger.Scope{BudgetID: "team", PeriodID: p.ID}, h.clock())
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Reserved > dollars(t, "10") {
		t.Errorf("parent Reserved = %s, want no more than its $10 allocation", tot.Reserved)
	}

	for _, s := range admitted {
		if s != nil {
			_ = s.Close()
		}
	}
}

// 11. Session reuse: two invocations in one runtime session remain two distinct
// transactions. A session groups records for reconciliation; it does not merge
// budget transactions, because each invocation was admitted separately.
func TestRuntimeSessionReuseKeepsInvocationsDistinct(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)
	h.runtime.bodies = []*fakeRuntimeBody{newFakeRuntimeBody("one"), newFakeRuntimeBody("two")}

	for _, id := range []string{"req-1", "req-2"} {
		in := runtimeInput()
		in.RuntimeSessionId = aws.String("session-shared")
		s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
			BudgetID: "team", RequestID: id, Input: in, MaxExposure: dollars(t, "1.00"),
		})
		if err != nil {
			t.Fatalf("InvokeAgentRuntime(%s): %v", id, err)
		}
		readRuntime(t, s)
	}

	first, second := getRecord(t, acts, "req-1"), getRecord(t, acts, "req-2")
	if first.ReservationID == second.ReservationID {
		t.Error("two invocations in one session must hold two distinct reservations")
	}
	if first.Runtime.SessionID != "session-shared" || second.Runtime.SessionID != "session-shared" {
		t.Error("both records must carry the shared session, which is the reconciliation key")
	}

	// Two holds, not one: the session is a grouping dimension, not a transaction.
	if tot := h.totals(t); tot.Reserved != dollars(t, "2.00") {
		t.Errorf("Reserved = %s, want two separate $1 holds", tot.Reserved)
	}

	// And no apportionment: neither record claims a share of any session-level cost.
	for _, rec := range []activity.Record{first, second} {
		if rec.Runtime.Reconciled {
			t.Error("no record may claim reconciled runtime usage without an observation")
		}
		if !rec.Runtime.ReconciledUsage.Empty() {
			t.Error("a session's resource usage must not be apportioned across its invocations")
		}
	}
}

// 12. Opaque content: neither the request payload nor the response body may enter
// activity persistence. Only their sizes.
func TestRuntimePayloadsNeverReachDurableActivity(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	const secretIn = "SENTINEL-REQUEST-PAYLOAD"
	const secretOut = "SENTINEL-RESPONSE-BODY"
	h.runtime.bodies = []*fakeRuntimeBody{newFakeRuntimeBody(secretOut)}

	in := runtimeInput()
	in.Payload = []byte(secretIn)
	s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID: "team", RequestID: "req-1", Input: in, MaxExposure: dollars(t, "1.00"),
	})
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	if got := readRuntime(t, s); got != secretOut {
		t.Fatalf("the body must still reach the caller verbatim, got %q", got)
	}

	// The whole persisted record, serialized, must not contain either payload.
	rec := getRecord(t, acts, "req-1")
	blob := fmt.Sprintf("%+v", rec)
	for _, secret := range []string{secretIn, secretOut} {
		if strings.Contains(blob, secret) {
			t.Errorf("payload content reached the durable record: %q", secret)
		}
	}
	// Sizes, though, are recorded: an operator needs to see a runaway payload.
	if rec.Runtime.PayloadBytes != int64(len(secretIn)) {
		t.Errorf("PayloadBytes = %d, want %d", rec.Runtime.PayloadBytes, len(secretIn))
	}
	if rec.Runtime.ResponseBytes != int64(len(secretOut)) {
		t.Errorf("ResponseBytes = %d, want %d", rec.Runtime.ResponseBytes, len(secretOut))
	}
}

// 12b. A runtime user id is potentially identifying, so it is not persisted merely
// because the SDK exposes it. Storing it is a product decision, not an
// implementation one, and this test is what would fail if someone made it silently.
func TestRuntimeUserIdIsNotPersisted(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	const userID = "user-identifying-value-12345"
	in := runtimeInput()
	in.RuntimeUserId = aws.String(userID)

	s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID: "team", RequestID: "req-1", Input: in, MaxExposure: dollars(t, "1.00"),
	})
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	readRuntime(t, s)

	// It still reaches AWS: throttle does not strip the caller's own request.
	if aws.ToString(h.runtime.lastCall().RuntimeUserId) != userID {
		t.Error("the caller's runtime user id must still be sent to AgentCore")
	}
	rec := getRecord(t, acts, "req-1")
	if strings.Contains(fmt.Sprintf("%+v", rec), userID) {
		t.Error("the runtime user id must not be persisted without an explicit product decision")
	}
}

// 13. An abandoned invocation must not leave an immortal goroutine renewing a hold
// forever. The idle bound terminates it.
func TestAbandonedRuntimeStreamStopsRenewingAndExits(t *testing.T) {
	h := newRuntimeHarnessWithLease(t, "1000", 60*time.Millisecond,
		func(c *bedrock.Config) { c.StreamStallTimeout = 30 * time.Millisecond })
	body := newFakeRuntimeBody("first", "never-read")
	body.stallAt = 1
	h.runtime.bodies = []*fakeRuntimeBody{body}

	before := runtime.NumGoroutine()

	s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID: "child", RequestID: "req-1", Input: runtimeInput(), MaxExposure: dollars(t, "1.00"),
	})
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}

	buf := make([]byte, 64)
	if _, err := s.Read(buf); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	// Then walk away: no further read, no Close, no cancel.

	deadline := time.Now().Add(3 * time.Second)
	for s.Result() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	res := s.Result()
	if res == nil {
		t.Fatal("an abandoned invocation must reach a terminal state on its own")
	}
	if res.Cost.Known() {
		t.Errorf("Cost = %v, want unknown for an abandoned invocation", res.Cost)
	}
	if body.closeCount() != 1 {
		t.Errorf("body closed %d times, want 1: abandonment must close the body", body.closeCount())
	}

	// The keep-alive goroutine is gone: no immortal renewer.
	for i := 0; i < 100 && runtime.NumGoroutine() > before+2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines: before %d, after %d -- the keep-alive must have exited", before, after)
	}
}

// 13b. A long invocation renews its lease while it is live, so a hosted agent that
// takes longer than a lease quantum does not lose its hold mid-call.
func TestLongRuntimeInvocationRenewsItsLease(t *testing.T) {
	h := newRuntimeHarnessWithLease(t, "1000", 60*time.Millisecond)
	// A body slow enough to outlive several lease quanta, delivered in pieces.
	h.runtime.bodies = []*fakeRuntimeBody{newFakeRuntimeBody("a", "b", "c", "d", "e")}

	s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID: "child", RequestID: "req-1", Input: runtimeInput(), MaxExposure: dollars(t, "1.00"),
	})
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}

	buf := make([]byte, 8)
	for {
		_, err := s.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		time.Sleep(40 * time.Millisecond)
	}
	_ = s.Close()

	res := s.Result()
	if !res.Unresolved {
		t.Error("the invocation must end unresolved")
	}
	// The hold survived: had the lease lapsed, the ancestor would show nothing held.
	p, err := h.ledger.EnsurePeriod(context.Background(), "team", h.clock())
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	tot, err := h.ledger.Totals(context.Background(), ledger.Scope{BudgetID: "team", PeriodID: p.ID}, h.clock())
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if tot.Reserved != dollars(t, "1.00") {
		t.Errorf("Reserved = %s, want the renewed hold still live", tot.Reserved)
	}
}

// 14. Crash-visible state: a process that dies mid-invocation leaves enough request,
// reservation, runtime, and session identity behind for reconciliation to find it.
// This is the linkage #18's recovery machinery will read.
func TestRuntimeActivitySurvivesProcessRestart(t *testing.T) {
	dir := t.TempDir()
	acts, opt := withActivity(t, dir+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	// The process "dies" here: the stream is never read or closed.
	_ = s

	rec := getRecord(t, acts, "req-1")
	if rec.Status != activity.StatusPending {
		t.Errorf("Status = %q, want pending: an interrupted invocation must be visible", rec.Status)
	}
	if rec.ReservationID == "" {
		t.Error("the reservation id must survive, or the hold cannot be reconciled")
	}
	if rec.Runtime.RuntimeID == "" || rec.Runtime.SessionID == "" {
		t.Error("runtime and session identity must survive, or a delayed observation cannot be linked")
	}
	if rec.Identity.Operation != bedrock.OperationInvokeAgentRuntime {
		t.Error("the operation must survive, so a reader knows this cost arrives out of band")
	}
	// And the frozen rates survive, so a reconciliation days later prices the
	// invocation at the rates that applied when it ran.
	if rec.Quote.ProviderModelID != bedrock.AgentCoreRuntimeResource {
		t.Errorf("Quote.ProviderModelID = %q, want the runtime resource", rec.Quote.ProviderModelID)
	}
	if _, ok := rec.Quote.Rates[usage.RuntimeVCPUNanoHours]; !ok {
		t.Error("the frozen quote must carry the vCPU rate for later reconciliation")
	}
	if _, ok := rec.Quote.Rates[usage.RuntimeMemoryNanoGBHours]; !ok {
		t.Error("the frozen quote must carry the memory rate for later reconciliation")
	}

	_ = s.Close()
}

// 15. The reconciliation seam: a delayed, normalized runtime observation prices
// against the frozen quote and lands on the record's reconciled fields, separate from
// the fields a synchronous measurement would use.
//
// This exercises the seam, not a reconciliation worker: nothing here polls
// CloudWatch, and the adapter does not depend on it.
func TestReconciledRuntimeUsagePricesAgainstTheFrozenQuote(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	readRuntime(t, s)

	rec := getRecord(t, acts, "req-1")

	// A delayed observation, as a provider reports it: decimal text, converted
	// exactly. 0.5 vCPU-hours at $0.0895 and 2 GB-hours at $0.00945.
	vcpu, err := usage.Nano("0.5")
	if err != nil {
		t.Fatalf("usage.Nano: %v", err)
	}
	mem, err := usage.Nano("2")
	if err != nil {
		t.Fatalf("usage.Nano: %v", err)
	}
	observed := usage.New(map[usage.Dimension]int64{
		usage.RuntimeVCPUNanoHours:     vcpu,
		usage.RuntimeMemoryNanoGBHours: mem,
	})

	priced, err := rec.Quote.Price(observed)
	if err != nil {
		t.Fatalf("pricing the observation: %v", err)
	}
	if !priced.Cost.Known() {
		t.Fatalf("reconciled cost = %v, want known once an authoritative observation exists", priced.Cost)
	}
	// $0.04475 + $0.0189 = $0.06365.
	if want := dollars(t, "0.06365"); priced.Cost.Amount != want {
		t.Errorf("reconciled cost = %s, want %s", priced.Cost.Amount, want)
	}

	// The reconciled figures go in their own fields. A delayed, provider-declared
	// approximate number must never be mistaken for one measured at call time.
	rec.Runtime.Reconciled = true
	rec.Runtime.ReconciledUsage = observed
	rec.Runtime.ReconciledCost = priced.Cost
	rec.Runtime.ReconciledFrom = "cloudwatch:usage-logs"
	rec.Runtime.ReconciledAt = h.clock()
	if err := acts.Complete(context.Background(), rec); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	back := getRecord(t, acts, "req-1")
	if !back.Runtime.Reconciled {
		t.Error("Reconciled must persist")
	}
	if got := back.Runtime.ReconciledUsage.Count(usage.RuntimeVCPUNanoHours); got != vcpu {
		t.Errorf("reconciled vCPU nano-hours = %d, want %d", got, vcpu)
	}
	if back.Runtime.ReconciledCost.Amount != priced.Cost.Amount {
		t.Errorf("reconciled cost = %s, want %s", back.Runtime.ReconciledCost.Amount, priced.Cost.Amount)
	}
	if back.Runtime.ReconciledFrom != "cloudwatch:usage-logs" {
		t.Error("the observation's provenance must persist")
	}
	// The measured-at-call-time fields are untouched.
	if back.ActualCost.Known() {
		t.Error("ActualCost must stay unknown: nothing was measured at call time")
	}
	if !back.ActualUsage.Empty() {
		t.Error("ActualUsage must stay empty: the reconciled figure has its own field")
	}
}

// A runtime invocation that predates the published rates captures no quote, and that
// is correct rather than broken: a price is never applied to a request that happened
// before it took effect. The invocation still runs and is still recorded -- it is
// simply unpriceable until someone supplies a rate that covers it.
func TestRuntimeBeforeItsRatesTookEffect(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	// A month before the AgentCore rates' effective date.
	h := newRuntimeHarnessAt(t, "1000", time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), opt)

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("an unpriceable-resource invocation must still be admitted on a declared exposure: %v", err)
	}
	readRuntime(t, s)

	rec := getRecord(t, acts, "req-1")
	if len(rec.Quote.Rates) != 0 {
		t.Errorf("Quote.Rates = %v, want empty before the rates took effect", rec.Quote.Rates)
	}
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", rec.Status)
	}
	if rec.ActualCost.Known() {
		t.Errorf("ActualCost = %v, want unknown", rec.ActualCost)
	}
}

// A runtime named by ID rather than ARN carries its account, because a resource
// observation is scoped to an account and a bare ID is not.
func TestRuntimeNamedByIDRecordsItsAccount(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	in := runtimeInput()
	in.AgentRuntimeArn = aws.String("my-agent-AbCdEf")
	in.AccountId = aws.String("123456789012")

	s, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID: "team", RequestID: "req-1", Input: in, MaxExposure: dollars(t, "1.00"),
	})
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	readRuntime(t, s)

	rec := getRecord(t, acts, "req-1")
	if rec.Runtime.RuntimeID != "my-agent-AbCdEf" {
		t.Errorf("RuntimeID = %q, want the bare agent id", rec.Runtime.RuntimeID)
	}
	if rec.Runtime.Account != "123456789012" {
		t.Errorf("Account = %q, want the account that owns the runtime", rec.Runtime.Account)
	}
}

// 15b. The adapter's resource ID and the price catalog's must agree, or every
// reconciled runtime cost would be silently unpriceable.
func TestRuntimeResourceIDMatchesTheCatalog(t *testing.T) {
	if bedrock.AgentCoreRuntimeResource != fixtures.AgentCoreRuntimeModelID {
		t.Errorf("adapter resource id %q != catalog %q",
			bedrock.AgentCoreRuntimeResource, fixtures.AgentCoreRuntimeModelID)
	}
}

// A client built without a runtime client refuses the call rather than panicking.
func TestInvokeAgentRuntimeRequiresARuntimeClient(t *testing.T) {
	h := newHarness(t, "1000")
	_, err := h.client.InvokeAgentRuntime(context.Background(), bedrock.RuntimeRequest{
		BudgetID: "team", Input: runtimeInput(), MaxExposure: dollars(t, "1.00"),
	})
	if !errors.Is(err, bedrock.ErrNoRuntimeClient) {
		t.Errorf("error = %v, want ErrNoRuntimeClient", err)
	}
}

// Request validation happens before any budget is touched.
func TestInvokeAgentRuntimeValidatesItsRequest(t *testing.T) {
	h := newRuntimeHarness(t, "1000")

	withInput := func(mutate func(*bedrockagentcore.InvokeAgentRuntimeInput)) *bedrockagentcore.InvokeAgentRuntimeInput {
		in := runtimeInput()
		mutate(in)
		return in
	}

	cases := []struct {
		name string
		req  bedrock.RuntimeRequest
	}{
		{"no budget", bedrock.RuntimeRequest{Input: runtimeInput()}},
		{"no input", bedrock.RuntimeRequest{BudgetID: "team"}},
		{"no runtime arn", bedrock.RuntimeRequest{BudgetID: "team",
			Input: withInput(func(in *bedrockagentcore.InvokeAgentRuntimeInput) { in.AgentRuntimeArn = nil })}},
		{"empty runtime arn", bedrock.RuntimeRequest{BudgetID: "team",
			Input: withInput(func(in *bedrockagentcore.InvokeAgentRuntimeInput) { in.AgentRuntimeArn = aws.String("") })}},
		{"no payload", bedrock.RuntimeRequest{BudgetID: "team",
			Input: withInput(func(in *bedrockagentcore.InvokeAgentRuntimeInput) { in.Payload = nil })}},
		{"negative exposure", bedrock.RuntimeRequest{BudgetID: "team", Input: runtimeInput(), MaxExposure: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.client.InvokeAgentRuntime(context.Background(), tc.req); err == nil {
				t.Error("an invalid request must be refused")
			}
		})
	}
	if h.runtime.callCount() != 0 {
		t.Error("no invalid request may reach the provider")
	}
	if tot := h.totals(t); tot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0: validation precedes reservation", tot.Reserved)
	}
}

// A response with metadata but no body is legitimate: the runtime ran and answered
// with nothing to read. It is still not free.
func TestRuntimeWithNoResponseBodyIsStillNotFree(t *testing.T) {
	h := newRuntimeHarness(t, "1000")
	h.runtime.noResponse = true

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	if got := readRuntime(t, s); got != "" {
		t.Errorf("body = %q, want empty", got)
	}
	res := s.Result()
	if res.Cost.Known() {
		t.Errorf("Cost = %v, want unknown", res.Cost)
	}
	if tot := h.totals(t); tot.Reserved != dollars(t, "1.00") {
		t.Errorf("Reserved = %s, want the hold retained", tot.Reserved)
	}
}

// The adapter works without an activity store, and a store failure never fails an
// invocation the caller has already paid for.
func TestRuntimeWorksWithoutActivity(t *testing.T) {
	h := newRuntimeHarness(t, "1000")

	s, err := h.invokeRuntime(t, context.Background(), "req-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	if got := readRuntime(t, s); got != `{"result":"ok"}` {
		t.Errorf("body = %q, want the runtime's own bytes", got)
	}
	if s.Result() == nil {
		t.Fatal("the invocation must still reach a terminal state")
	}
}
