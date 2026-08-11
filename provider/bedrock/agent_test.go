package bedrock_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	agenttypes "github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime/types"

	"throttle/activity"
	activitysqlite "throttle/activity/sqlite"
	"throttle/budget"
	"throttle/engine"
	"throttle/ledger/sqlite"
	"throttle/money"
	"throttle/pricing"
	"throttle/provider/bedrock"
	"throttle/usage"
)

// haikuID is a second priced model, for the turns that invoke more than one.
const haikuID = "anthropic.claude-haiku-4-5-20251001-v1:0"

// novaLiteID is priced low enough that one step's exact cost is a fraction of a
// microdollar, which is what makes the rounding boundary observable.
const novaLiteID = "amazon.nova-lite-v1:0"

// agentHarness is the Converse harness plus a fake agent runtime client.
type agentHarness struct {
	*harness
	agent  *fakeAgentAPI
	reader *fakeAgentReader
}

func newAgentHarness(t *testing.T, allocation string, opts ...func(*bedrock.Config)) *agentHarness {
	t.Helper()
	reader := newFakeAgentReader()
	api := &fakeAgentAPI{readers: []*fakeAgentReader{reader}}
	h := newHarness(t, allocation, append([]func(*bedrock.Config){withAgent(api)}, opts...)...)
	return &agentHarness{harness: h, agent: api, reader: reader}
}

// newAgentHarnessWithLease builds an agent harness on a real clock with a short
// lease, so a turn can outlive a lease quantum in milliseconds rather than minutes.
// It registers a child under a parent so ancestor encumbrance is observable; the
// child's own allocation is generous, so only the parent's ceiling can refuse.
func newAgentHarnessWithLease(t *testing.T, parent string, lease time.Duration, opts ...func(*bedrock.Config)) *agentHarness {
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
	// A borrow window spanning the period makes the paced allowance the whole
	// envelope from the first instant. These tests are about leases and contention,
	// and a period that has only just started paces to nearly nothing -- which would
	// refuse every invocation for a reason that has nothing to do with what is under
	// test. Pacing has its own tests.
	const wholePeriod = 62 * 24 * time.Hour
	for _, def := range []budget.Definition{
		{ID: "team", Allocation: dollars(t, parent), Recurrence: budget.RecurMonthly, AnchorAt: anchor, Borrow: wholePeriod},
		{ID: "child", ParentID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly, AnchorAt: anchor, Borrow: wholePeriod},
	} {
		if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", def.ID, err)
		}
	}

	reader := newFakeAgentReader()
	api := &fakeAgentAPI{readers: []*fakeAgentReader{reader}}
	h := buildHarness(t, eng, store, clock, append([]func(*bedrock.Config){withAgent(api)}, opts...)...)
	return &agentHarness{harness: h, agent: api, reader: reader}
}

// invoke is the ordinary governed invocation, against a declared ceiling.
func (h *agentHarness) invoke(t *testing.T, ctx context.Context, requestID string, maxCost money.Money) (*bedrock.AgentStream, error) {
	t.Helper()
	return h.client.InvokeAgent(ctx, bedrock.AgentRequest{
		BudgetID:  "team",
		RequestID: requestID,
		Input:     agentRequest(),
		MaxCost:   maxCost,
	})
}

// 1. The invocation is refused before a stream exists: nothing ran, so nothing was
// billed, and this is the one path that releases the hold.
func TestAgentCallFailureReleasesTheHold(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.agent.err = errors.New("ResourceNotFoundException: no such agent alias")

	_, err := h.invoke(t, context.Background(), "agent-refused", dollars(t, "1.00"))
	if !errors.Is(err, bedrock.ErrProvider) {
		t.Fatalf("error = %v, want ErrProvider", err)
	}

	if tot := h.totals(t); tot.Reserved != 0 || tot.Spent != 0 {
		t.Errorf("reserved %s, spent %s, want the hold returned: nothing ran", tot.Reserved, tot.Spent)
	}
	rec := getRecord(t, acts, "agent-refused")
	if rec.Status != activity.StatusReleased {
		t.Errorf("status = %q, want released", rec.Status)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want provider-error", rec.Outcome)
	}
	if !rec.ActualCost.Known() || rec.ActualCost.Amount != 0 {
		t.Errorf("ActualCost = %s, want a known zero: nothing was billed", rec.ActualCost)
	}
}

// 2. A complete invocation: every observed model invocation's usage is aggregated,
// the compound cost is exact, and the one outer reservation settles exactly once.
func TestAgentTurnSettlesOnceFromAggregatedTraceUsage(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.reader.emit(normalAgentTurn(sonnetID)...)

	s, err := h.invoke(t, context.Background(), "agent-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	events := drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Every event is forwarded, trace parts included: enabling the trace for
	// accounting gives the caller a superset of what they asked for, not a filtered
	// version of it.
	if len(events) != 12 {
		t.Fatalf("received %d events, want all 12 forwarded", len(events))
	}

	res := s.Result()
	if res == nil {
		t.Fatal("Result must be available once the stream is terminal")
	}
	if !res.Settled {
		t.Fatalf("the invocation should have settled: %v", s.Err())
	}

	// Four model invocations: pre 400/40, orch-1 1200/180, orch-2 1800/220, post
	// 600/90.
	if got := res.Usage.Count(usage.InputTokens); got != 4000 {
		t.Errorf("input tokens = %d, want 4000 aggregated across the turn", got)
	}
	if got := res.Usage.Count(usage.OutputTokens); got != 530 {
		t.Errorf("output tokens = %d, want 530 aggregated across the turn", got)
	}
	// 4000 at $3/M = $0.012, 530 at $15/M = $0.00795.
	want := dollars(t, "0.01995")
	if !res.Cost.Known() {
		t.Errorf("Cost = %s, want a known cost", res.Cost)
	}
	if res.Cost.Amount != want {
		t.Errorf("Cost = %s, want %s", res.Cost, want)
	}

	// One transaction, one charge. The internal model calls are accounting detail,
	// not transactions: four child charges here would be four fictions.
	tot := h.totals(t)
	if tot.Spent != want {
		t.Errorf("spent = %s, want %s charged exactly once", tot.Spent, want)
	}
	if tot.Reserved != 0 || tot.PendingCount != 0 {
		t.Errorf("reserved = %s in %d holds, want the one hold fully resolved", tot.Reserved, tot.PendingCount)
	}
	charges, err := h.charges(t)
	if err != nil {
		t.Fatalf("charges: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("recorded %d charges, want exactly 1 for the compound transaction", len(charges))
	}
	if charges[0].ActualCost != want {
		t.Errorf("charge = %s, want the aggregate %s", charges[0].ActualCost, want)
	}

	// The outer identity names the operation, not a model, because the outer call is
	// not a model call.
	if res.Identity.Operation != bedrock.OperationInvokeAgent {
		t.Errorf("Operation = %q, want %q", res.Identity.Operation, bedrock.OperationInvokeAgent)
	}
	if res.Identity.ProviderModelID != "" {
		t.Errorf("outer ProviderModelID = %q, want empty: the agent invocation is not a model invocation",
			res.Identity.ProviderModelID)
	}

	rec := getRecord(t, acts, "agent-1")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want settled", rec.Status)
	}
	if rec.Outcome != activity.OutcomeSuccess {
		t.Errorf("outcome = %q, want success", rec.Outcome)
	}
	if rec.ActualCost.Amount != want {
		t.Errorf("persisted cost = %s, want %s", rec.ActualCost, want)
	}
	if rec.Agent.AgentID != "AGENT123456" || rec.Agent.AliasID != "ALIAS7890" {
		t.Errorf("agent identifiers = %q/%q, want the invoked agent and alias",
			rec.Agent.AgentID, rec.Agent.AliasID)
	}
	if rec.Agent.SessionID != "session-abc" {
		t.Errorf("session = %q, want the provider's session identifier as a telemetry dimension",
			rec.Agent.SessionID)
	}
	// The request named an alias, not a version. The version is only knowable from
	// the trace, which is why it is read from there.
	if rec.Agent.Version != "7" {
		t.Errorf("version = %q, want the version the trace reported", rec.Agent.Version)
	}
	if rec.ProviderLatency != 9*time.Second {
		t.Errorf("ProviderLatency = %s, want the operation total the agent reported", rec.ProviderLatency)
	}
	assertNoContent(t, rec)
	assertNoAgentContent(t, rec)
}

// 3. Several orchestration model steps: each is preserved individually and the
// aggregate is exact. This is the compound-accounting claim -- an operator can see
// where the money went inside one transaction.
func TestAgentPreservesPerStepUsageBeneathOneTransaction(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.reader.emit(normalAgentTurn(sonnetID)...)

	s, err := h.invoke(t, context.Background(), "agent-steps", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := getRecord(t, acts, "agent-steps")
	if len(rec.Agent.Steps) != 4 {
		t.Fatalf("recorded %d steps, want the 4 observed model invocations: %+v",
			len(rec.Agent.Steps), rec.Agent.Steps)
	}

	wantKinds := []string{"pre-processing", "orchestration", "orchestration", "post-processing"}
	wantIn := []int64{400, 1200, 1800, 600}
	wantOut := []int64{40, 180, 220, 90}
	for i, step := range rec.Agent.Steps {
		if step.Seq != i+1 {
			t.Errorf("step %d Seq = %d, want %d", i, step.Seq, i+1)
		}
		if step.Kind != wantKinds[i] {
			t.Errorf("step %d Kind = %q, want %q", i, step.Kind, wantKinds[i])
		}
		if got := step.Usage.Count(usage.InputTokens); got != wantIn[i] {
			t.Errorf("step %d input = %d, want %d", i, got, wantIn[i])
		}
		if got := step.Usage.Count(usage.OutputTokens); got != wantOut[i] {
			t.Errorf("step %d output = %d, want %d", i, got, wantOut[i])
		}
		// The model identity is on the input event and the usage on the output event.
		// They arrive separately and are joined by trace ID; nothing else links them.
		if step.Identity.ProviderModelID != sonnetID {
			t.Errorf("step %d model = %q, want %q joined from the input event",
				i, step.Identity.ProviderModelID, sonnetID)
		}
		if step.Identity.Operation != bedrock.OperationInvokeAgent {
			t.Errorf("step %d operation = %q, want %q",
				i, step.Identity.Operation, bedrock.OperationInvokeAgent)
		}
		if step.TraceID == "" {
			t.Errorf("step %d has no trace id, so it cannot be tied to a provider-side trace", i)
		}
		if !step.Cost.Known() {
			t.Errorf("step %d cost = %s, want a known per-step contribution", i, step.Cost)
		}
		if step.Latency != 1500*time.Millisecond {
			t.Errorf("step %d latency = %s, want the per-invocation duration", i, step.Latency)
		}
	}

	// The record's own aggregate is the authoritative figure and is what settled.
	if got := rec.ActualUsage.Count(usage.InputTokens); got != 4000 {
		t.Errorf("aggregate input = %d, want the sum of the steps", got)
	}
	if rec.ActualCost.Amount != dollars(t, "0.01995") {
		t.Errorf("aggregate cost = %s, want the exactly-accumulated $0.01995", rec.ActualCost)
	}
	if got, complete := rec.Spent(); !complete || got != rec.ActualCost.Amount {
		t.Errorf("Spent() = %s (complete %v), want the authoritative aggregate %s",
			got, complete, rec.ActualCost)
	}
	// These particular figures are exact per step, so they happen to sum. The turn
	// where they do not is TestAgentRoundsTheCompoundChargeOnce.
	var stepSum money.Money
	for _, step := range rec.Agent.Steps {
		stepSum += step.Cost.Amount
	}
	if stepSum != rec.ActualCost.Amount {
		t.Errorf("steps summed to %s against an aggregate of %s", stepSum, rec.ActualCost)
	}
}

// The single-rounding rule, made visible. Twenty tiny model invocations are charged
// as one accumulated amount rounded once, not as twenty separately-rounded amounts
// -- which drift upward by up to a rounding unit per step.
func TestAgentRoundsTheCompoundChargeOnce(t *testing.T) {
	h := newAgentHarness(t, "1000")

	// Nine input tokens on nova-lite at $0.06/M is 0.54 microdollars: over half a
	// unit, so a step rounded on its own becomes 1. Twenty of them is 10.8 exactly,
	// which rounds to 11 -- against the 20 that per-step rounding would produce.
	const steps = 20
	events := make([]agenttypes.ResponseStream, 0, steps*2)
	for i := range steps {
		id := fmt.Sprintf("t-%d", i)
		events = append(events, orchInput(id, novaLiteID), orchOutput(id, 9, 0))
	}
	h.reader.emit(events...)

	s, err := h.invoke(t, context.Background(), "agent-rounding", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	res := s.Result()
	if !res.Settled {
		t.Fatalf("the turn should have settled: %v", s.Err())
	}
	if len(res.Steps) != steps {
		t.Fatalf("priced %d components, want %d", len(res.Steps), steps)
	}
	if got := res.Usage.Count(usage.InputTokens); got != steps*9 {
		t.Errorf("input tokens = %d, want %d across the steps", got, steps*9)
	}

	if res.Cost.Amount != 11 {
		t.Errorf("Cost = %d microdollars, want 11: the turn must be accumulated exactly and "+
			"rounded once at the charge boundary", res.Cost.Amount)
	}
	// The per-step figures are presentation and each rounds up on its own. Their sum
	// is the wrong answer, and proving the charge is not that sum is the point.
	var stepSum money.Money
	for _, c := range res.Steps {
		stepSum += c.Amount
	}
	if stepSum != 20 {
		t.Errorf("per-step amounts summed to %d, want 20; the fixture no longer exercises drift",
			stepSum)
	}
	if tot := h.totals(t); tot.Spent != 11 {
		t.Errorf("spent = %d microdollars, want 11 rather than the %d per-step rounding would charge",
			tot.Spent, stepSum)
	}
}

// 4. The trace names a model the captured set cannot price. The whole outer
// transaction is unresolved, the hold stays encumbered, and the priced subset is
// never reported as the total.
func TestAgentUnpricedModelLeavesTheWholeTurnUnresolved(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)

	// The agent invoked one model the catalog prices and one it does not.
	h.reader.emit(
		preInput("t-pre", sonnetID), preOutput("t-pre", 400, 40),
		orchInput("t-orch", unpricedModelID), orchOutput("t-orch", 1200, 180),
	)

	s, err := h.invoke(t, context.Background(), "agent-unpriced", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); !errors.Is(err, bedrock.ErrCostUnresolved) {
		t.Fatalf("Close error = %v, want ErrCostUnresolved", err)
	}

	res := s.Result()
	if res.Settled {
		t.Error("a turn containing an unpriceable model must not settle")
	}
	if !res.Unresolved {
		t.Error("Result.Unresolved must be set")
	}
	if res.Cost.Known() {
		t.Errorf("Cost = %s, want an incomplete cost", res.Cost)
	}
	if res.Cost.Completeness != usage.CostPartial {
		t.Errorf("completeness = %q, want partial: the preprocessing step did price",
			res.Cost.Completeness)
	}

	// The hold stays encumbered. Releasing it would offer already-spent headroom to
	// the next caller; settling the floor would report it as the total.
	tot := h.totals(t)
	if tot.Reserved != dollars(t, "1.00") {
		t.Errorf("reserved = %s, want the $1.00 hold still encumbered", tot.Reserved)
	}
	if tot.Spent != 0 {
		t.Errorf("spent = %s, want 0: a floor must not be settled as a total", tot.Spent)
	}

	rec := getRecord(t, acts, "agent-unpriced")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want unresolved", rec.Status)
	}
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("outcome = %q, want unpriced", rec.Outcome)
	}
	// Usage is fully preserved, the unpriceable step included: it is the only
	// evidence of what is owing.
	if got := rec.ActualUsage.Count(usage.InputTokens); got != 1600 {
		t.Errorf("preserved input = %d, want all 1600 including the unpriced step", got)
	}
	if _, complete := rec.Spent(); complete {
		t.Error("Spent() reported complete; an unresolved turn makes a total incomplete")
	}
	// The priced subset is still a floor: 400 at $3/M plus 40 at $15/M.
	if rec.ActualCost.AtLeast() != dollars(t, "0.0018") {
		t.Errorf("floor = %s, want the $0.0018 that did price", rec.ActualCost)
	}
}

// A dimension the captured quote has no rate for behaves the same way as an
// unpriceable model: the turn is incomplete rather than understated.
func TestAgentUnpricedDimensionLeavesTheTurnIncomplete(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	// A catalog that prices output but not input, so a perfectly ordinary model
	// invocation reports a dimension with no rate.
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "15.00")),
		},
		Provenance: pricing.Provenance{Source: "test", Version: "output-only"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	h := newAgentHarness(t, "1000", withActs, func(c *bedrock.Config) { c.Catalog = cat })
	h.reader.emit(orchInput("t-orch", sonnetID), orchOutput("t-orch", 1200, 180))

	s, err := h.invoke(t, context.Background(), "agent-dim", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); !errors.Is(err, bedrock.ErrCostUnresolved) {
		t.Fatalf("Close error = %v, want ErrCostUnresolved", err)
	}

	rec := getRecord(t, acts, "agent-dim")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want unresolved", rec.Status)
	}
	var found bool
	for _, d := range rec.ActualCost.Unpriced {
		if d == usage.InputTokens {
			found = true
		}
	}
	if !found {
		t.Errorf("Unpriced = %v, want the input dimension named so reconciliation knows what it needs",
			rec.ActualCost.Unpriced)
	}
	if tot := h.totals(t); tot.Reserved == 0 {
		t.Error("the hold must stay encumbered while a dimension is unpriced")
	}
}

// 5. The caller cancels before the trace reported any usage. The agent may have
// invoked several models already, so the outcome is unknown and the hold is not
// optimistically released.
func TestAgentCancellationBeforeUsageRetainsTheHold(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	// A model invocation has started -- so a model really is running -- and the
	// stream then stays open, which is what makes cancellation the only way out.
	go func() {
		select {
		case h.reader.events <- orchInput("t-orch", sonnetID):
		case <-h.reader.done:
			return
		}
		<-h.reader.done
	}()

	ctx, cancel := context.WithCancel(context.Background())
	s, err := h.invoke(t, ctx, "agent-cancel", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	<-s.Events() // The invocation is genuinely under way.
	cancel()

	drainAgent(t, s)
	if err := s.Close(); !errors.Is(err, bedrock.ErrOutcomeUnknown) {
		t.Fatalf("Close error = %v, want ErrOutcomeUnknown", err)
	}

	if tot := h.totals(t); tot.Reserved == 0 {
		t.Error("a cancelled agent turn must not release its hold: the agent may have spent money")
	}
	rec := getRecord(t, acts, "agent-cancel")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}
	if rec.Outcome != activity.OutcomeCancelled {
		t.Errorf("outcome = %q, want cancelled", rec.Outcome)
	}
	if rec.ActualCost.Known() {
		t.Errorf("ActualCost = %s, want an explicit unknown, never a free turn", rec.ActualCost)
	}
	// The step is recorded even with no usage. It is evidence a model was invoked,
	// which is what makes the outstanding hold justifiable rather than arbitrary.
	if len(rec.Agent.Steps) != 1 {
		t.Errorf("recorded %d steps, want the one invocation that had started", len(rec.Agent.Steps))
	}
}

// A deadline is recorded as a timeout rather than a cancellation: the operational
// story differs even though the accounting is identical.
func TestAgentTimeoutRecordsTimeout(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.reader.hang()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	s, err := h.invoke(t, ctx, "agent-timeout", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); !errors.Is(err, bedrock.ErrOutcomeUnknown) {
		t.Fatalf("Close error = %v, want ErrOutcomeUnknown", err)
	}
	rec := getRecord(t, acts, "agent-timeout")
	if rec.Outcome != activity.OutcomeTimeout {
		t.Errorf("outcome = %q, want timeout", rec.Outcome)
	}
	if tot := h.totals(t); tot.Reserved == 0 {
		t.Error("a timed-out agent turn must keep its hold")
	}
}

// 6. The stream fails after usage was observed. The tokens the agent reported are
// real spend regardless of what happened next, so the turn still settles -- and the
// caller is told both facts.
func TestAgentStreamErrorAfterUsageStillSettles(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.reader.emitThenFail(errAgentStream,
		preInput("t-pre", sonnetID), preOutput("t-pre", 400, 40),
		orchInput("t-orch", sonnetID), orchOutput("t-orch", 1200, 180),
	)

	s, err := h.invoke(t, context.Background(), "agent-err-after", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); !errors.Is(err, bedrock.ErrProvider) {
		t.Fatalf("Close error = %v, want ErrProvider", err)
	}

	res := s.Result()
	if !res.Settled {
		t.Fatal("usage the agent already reported is real spend and must settle")
	}
	// 1600 input at $3/M = $0.0048, 220 output at $15/M = $0.0033.
	want := dollars(t, "0.0081")
	if res.Cost.Amount != want {
		t.Errorf("Cost = %s, want the %s actually observed", res.Cost, want)
	}
	if tot := h.totals(t); tot.Spent != want {
		t.Errorf("spent = %s, want %s", tot.Spent, want)
	}
	rec := getRecord(t, acts, "agent-err-after")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want settled", rec.Status)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want provider-error alongside the settlement", rec.Outcome)
	}
	if rec.Error == "" {
		t.Error("the stream failure must be recorded even though the turn settled")
	}
}

// A stream error before any usage leaves the outcome unknown, not free.
func TestAgentStreamErrorBeforeUsageRetainsTheHold(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.reader.emitThenFail(errAgentStream, orchInput("t-orch", sonnetID))

	s, err := h.invoke(t, context.Background(), "agent-err-before", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); !errors.Is(err, bedrock.ErrOutcomeUnknown) {
		t.Fatalf("Close error = %v, want ErrOutcomeUnknown", err)
	}
	if tot := h.totals(t); tot.Reserved == 0 {
		t.Error("a failed agent turn must keep its hold: models may have run before the failure")
	}
	rec := getRecord(t, acts, "agent-err-before")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want provider-error", rec.Outcome)
	}
}

// 7. Return control is a successful protocol outcome, not an error. Whatever the
// agent consumed getting there settles, and a follow-up InvokeAgent is a separate
// transaction rather than a continuation of this one's hold.
func TestAgentReturnControlSettlesAsASuccessfulOutcome(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	second := newFakeAgentReader()
	h.agent.readers = append(h.agent.readers, second)

	h.reader.emit(
		preInput("t-pre", sonnetID), preOutput("t-pre", 400, 40),
		orchInput("t-orch", sonnetID), orchOutput("t-orch", 1200, 180),
		returnControl(),
	)

	s, err := h.invoke(t, context.Background(), "agent-return", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	events := drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The return-control event reaches the caller: acting on it is their job.
	if _, ok := events[len(events)-1].(*agenttypes.ResponseStreamMemberReturnControl); !ok {
		t.Fatalf("last event = %T, want the return-control event forwarded", events[len(events)-1])
	}

	res := s.Result()
	if !res.ReturnedControl {
		t.Error("Result.ReturnedControl must report the protocol outcome")
	}
	if !res.Settled {
		t.Fatalf("returning control is a success, so the observed spend must settle: %v", s.Err())
	}
	if res.Unresolved {
		t.Error("returning control is not an unresolved cost")
	}
	want := dollars(t, "0.0081")
	if res.Cost.Amount != want {
		t.Errorf("Cost = %s, want the %s consumed before control was returned", res.Cost, want)
	}

	rec := getRecord(t, acts, "agent-return")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want settled", rec.Status)
	}
	if rec.Outcome != activity.OutcomeSuccess {
		t.Errorf("outcome = %q, want success", rec.Outcome)
	}
	if rec.Agent.Events["return-control"] != 1 {
		t.Errorf("events = %v, want the return-control outcome counted", rec.Agent.Events)
	}
	if !strings.Contains(rec.Agent.Note, "separate transaction") {
		t.Errorf("note = %q, want it to state that a follow-up InvokeAgent is a new transaction",
			rec.Agent.Note)
	}

	// And the follow-up really is a separate transaction, against its own hold.
	second.emit(orchInput("t-orch-2", sonnetID), orchOutput("t-orch-2", 500, 60))
	s2, err := h.invoke(t, context.Background(), "agent-return-2", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("second InvokeAgent: %v", err)
	}
	drainAgent(t, s2)
	if err := s2.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if s2.ReservationID() == s.ReservationID() {
		t.Error("the follow-up invocation must take its own reservation")
	}
	charges, err := h.charges(t)
	if err != nil {
		t.Fatalf("charges: %v", err)
	}
	if len(charges) != 2 {
		t.Errorf("recorded %d charges, want 2: each InvokeAgent is its own transaction", len(charges))
	}
}

// 8. The caller did not enable the trace. throttle enables it on a copy of their
// input, because the trace is the only place per-invocation usage is reported --
// and the caller's own struct is not mutated.
func TestAgentEnablesTraceOnACopyOfTheCallersInput(t *testing.T) {
	h := newAgentHarness(t, "1000")
	h.reader.emit(normalAgentTurn(sonnetID)...)

	in := agentRequest() // EnableTrace unset, as for a caller who never asked for it.
	s, err := h.client.InvokeAgent(context.Background(), bedrock.AgentRequest{
		BudgetID: "team", RequestID: "agent-trace", Input: in, MaxCost: dollars(t, "1.00"),
	})
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sent := h.agent.lastInput()
	if sent.EnableTrace == nil || !*sent.EnableTrace {
		t.Error("throttle must enable the trace, or it cannot obtain per-invocation usage at all")
	}
	// The caller's struct is theirs. Mutating one they may reuse or share would be a
	// surprising side effect of asking throttle to govern one call.
	if in.EnableTrace != nil {
		t.Errorf("the caller's input was mutated: EnableTrace = %v", *in.EnableTrace)
	}
	if sent == in {
		t.Error("the input sent to AWS must be a copy, not the caller's own struct")
	}
	// Everything else passes through unchanged.
	if aws.ToString(sent.AgentId) != aws.ToString(in.AgentId) ||
		aws.ToString(sent.AgentAliasId) != aws.ToString(in.AgentAliasId) ||
		aws.ToString(sent.SessionId) != aws.ToString(in.SessionId) ||
		aws.ToString(sent.InputText) != aws.ToString(in.InputText) {
		t.Error("the request must otherwise reach AWS exactly as the caller wrote it")
	}
	if !s.Result().Settled {
		t.Errorf("the turn should have settled: %v", s.Err())
	}
}

// A turn that reports no model usage at all is not a free turn. Either the trace
// was suppressed or the agent answered without invoking a model, and from here
// those are indistinguishable -- so throttle refuses to claim exact accounting.
func TestAgentTurnWithoutUsageIsNotFree(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	// Chunks only: an answer with no trace, as if the trace had been stripped.
	h.reader.emit(chunk("the airspeed velocity"), chunk(" is about 11 metres per second"))

	s, err := h.invoke(t, context.Background(), "agent-no-usage", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); !errors.Is(err, bedrock.ErrAccounting) {
		t.Fatalf("Close error = %v, want ErrAccounting", err)
	}

	rec := getRecord(t, acts, "agent-no-usage")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}
	if rec.ActualCost.Known() {
		t.Errorf("ActualCost = %s, want an explicit unknown", rec.ActualCost)
	}
	if rec.ActualCost.String() == usage.KnownCost(0).String() {
		t.Errorf("an unaccounted turn rendered as %q, indistinguishable from a free one",
			rec.ActualCost.String())
	}
	if tot := h.totals(t); tot.Reserved == 0 {
		t.Error("an unaccountable turn must keep its hold")
	}
	assertNoContent(t, rec)
	assertNoContentInRow(t, acts, "agent-no-usage")
}

// 9. The privacy claim, against a trace deliberately stuffed with every kind of
// sensitive content the service can send: prompts, model responses, reasoning,
// rationale, action parameters, retrieved passages, and guardrail matches. None of
// it may reach the durable record.
func TestAgentTraceContentNeverReachesDurableActivity(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.reader.emit(
		preInput("t-pre", sonnetID), preOutput("t-pre", 400, 40),
		orchInput("t-orch", sonnetID), rationale("t-orch"), orchOutput("t-orch", 1200, 180),
		kbInvocation("t-orch"), kbObservation("t-orch"),
		actionInvocation("t-orch"), actionObservation("t-orch"),
		guardrailTrace("t-orch"),
		chunk("the airspeed velocity is about 11 metres per second"),
		postInput("t-post", sonnetID), postOutput("t-post", 600, 90),
	)

	s, err := h.invoke(t, context.Background(), "agent-privacy", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	// The caller does receive all of it: forwarding is not persisting.
	events := drainAgent(t, s)
	if len(events) != 13 {
		t.Fatalf("received %d events, want all 13 forwarded to the caller", len(events))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := getRecord(t, acts, "agent-privacy")
	assertNoContent(t, rec)
	assertNoAgentContent(t, rec)
	assertSchemaHoldsNoContent(t, acts)
	// The whole persisted row, column by column: a leak into any field, including
	// any nested JSON blob, shows up here even if the typed assertions miss it.
	assertNoContentInRow(t, acts, "agent-privacy")

	// The non-model activity is still visible as counts, which is the honest
	// position: it happened, it cost money, and throttle cannot price it.
	for _, kind := range []string{"knowledge-base", "action-group", "guardrail"} {
		if rec.Agent.Events[kind] == 0 {
			t.Errorf("events = %v, want %q observed", rec.Agent.Events, kind)
		}
	}
	if !strings.Contains(rec.Agent.Note, "without any billable quantity") {
		t.Errorf("note = %q, want it to state that unpriceable activity occurred rather than "+
			"implying it was free", rec.Agent.Note)
	}
	// The observable model spend still settles. Unpriceable non-model activity does
	// not make the foundation-model accounting incomplete; it makes the turn's true
	// total broader than throttle can see, which is what the note records.
	if !s.Result().Settled {
		t.Errorf("the observable model spend should still settle: %v", s.Err())
	}
}

// Non-model activity is never assigned an invented dollar cost. Its real cost lands
// on the AWS bill outside throttle's view, and a fabricated number would be worse
// than an acknowledged gap.
func TestAgentNonModelActivityIsCountedNotPriced(t *testing.T) {
	h := newAgentHarness(t, "1000")
	h.reader.emit(
		orchInput("t-orch", sonnetID), orchOutput("t-orch", 1000, 100),
		kbInvocation("t-orch"), kbObservation("t-orch"),
		actionInvocation("t-orch"), actionObservation("t-orch"),
	)

	s, err := h.invoke(t, context.Background(), "agent-unpriceable", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res := s.Result()
	// Exactly one priced component: the model invocation. The action and the lookup
	// are not components, because throttle has no rate for either.
	if len(res.Steps) != 1 {
		t.Fatalf("priced %d components, want only the model invocation: %+v", len(res.Steps), res.Steps)
	}
	// 1000 at $3/M plus 100 at $15/M.
	want := dollars(t, "0.0045")
	if res.Cost.Amount != want {
		t.Errorf("Cost = %s, want %s: model spend only, with no invented figure for the "+
			"action or the lookup", res.Cost, want)
	}
	if res.Agent.Events["action-group"] != 1 || res.Agent.Events["knowledge-base"] != 1 {
		t.Errorf("events = %v, want the action and the lookup observed", res.Agent.Events)
	}
}

// 10. Several turns in one agent session remain distinct request and activity
// records. The session groups records; it does not scope money.
func TestAgentSessionTurnsRemainDistinctTransactions(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	readers := []*fakeAgentReader{newFakeAgentReader(), newFakeAgentReader(), newFakeAgentReader()}
	api := &fakeAgentAPI{readers: readers}
	h := newHarness(t, "1000", withActs, withAgent(api))

	for i, r := range readers {
		id := fmt.Sprintf("t-%d", i)
		r.emit(orchInput(id, sonnetID), orchOutput(id, 1000, 100))
	}

	ids := make([]string, 0, len(readers))
	for i := range readers {
		requestID := fmt.Sprintf("agent-session-%d", i)
		ids = append(ids, requestID)
		s, err := h.client.InvokeAgent(context.Background(), bedrock.AgentRequest{
			BudgetID: "team", RequestID: requestID, Input: agentRequest(), MaxCost: dollars(t, "1.00"),
		})
		if err != nil {
			t.Fatalf("InvokeAgent %d: %v", i, err)
		}
		drainAgent(t, s)
		if err := s.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}

	seen := make(map[string]bool)
	for _, id := range ids {
		rec := getRecord(t, acts, id)
		if rec.Agent.SessionID != "session-abc" {
			t.Errorf("%s session = %q, want the shared session id", id, rec.Agent.SessionID)
		}
		if seen[rec.ReservationID] {
			t.Errorf("%s reused reservation %q: each API invocation is its own transaction",
				id, rec.ReservationID)
		}
		seen[rec.ReservationID] = true
		if rec.Status != activity.StatusSettled {
			t.Errorf("%s status = %q, want settled", id, rec.Status)
		}
	}

	charges, err := h.charges(t)
	if err != nil {
		t.Fatalf("charges: %v", err)
	}
	if len(charges) != 3 {
		t.Errorf("recorded %d charges, want one per InvokeAgent call", len(charges))
	}
	// Each turn was 1000 at $3/M plus 100 at $15/M.
	if tot := h.totals(t); tot.Spent != dollars(t, "0.0135") {
		t.Errorf("spent = %s, want three turns' worth", tot.Spent)
	}
}

// 11. A long orchestration outlives its lease quantum. The hold is renewed while
// the turn is alive, the ancestor's headroom stays encumbered throughout, and
// settlement still succeeds.
func TestLongAgentTurnRenewsItsLease(t *testing.T) {
	h := newAgentHarnessWithLease(t, "1000", 300*time.Millisecond)
	h.reader.pace = 60 * time.Millisecond
	h.reader.emit(normalAgentTurn(sonnetID)...)

	before := runtime.NumGoroutine()

	s, err := h.client.InvokeAgent(context.Background(), bedrock.AgentRequest{
		BudgetID: "child", RequestID: "agent-slow", Input: agentRequest(), MaxCost: dollars(t, "1.00"),
	})
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}

	var sawEncumbered bool
	for range s.Events() {
		if h.scopeTotals(t, "team").Reserved > 0 {
			sawEncumbered = true
		}
	}
	if !sawEncumbered {
		t.Error("the parent budget's headroom must stay encumbered while a child's agent turn is alive")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res := s.Result()
	if !res.Settled {
		t.Fatalf("a renewed agent turn must still settle: %v", s.Err())
	}
	// The renewal really happened, rather than the turn merely finishing inside one
	// lease quantum.
	r, err := h.ledger.Get(context.Background(), res.ReservationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.RenewCount == 0 {
		t.Error("an agent turn outliving its lease quantum must have renewed the hold")
	}

	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
}

// 12. A caller who neither reads, closes, nor cancels must not pin a goroutine and
// renew a hold forever. The stall bound ends the turn, the reservation is retained
// because the agent may have spent money, and nothing immortal is left behind.
func TestAbandonedAgentStreamStopsRenewingAndExits(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarnessWithLease(t, "1000", 300*time.Millisecond, withActs,
		func(c *bedrock.Config) { c.StreamStallTimeout = 100 * time.Millisecond })
	// A model invocation starts and the events that follow carry no usage, so
	// abandonment happens with the outcome genuinely unknown.
	h.reader.emit(
		orchInput("t-orch", sonnetID), rationale("t-orch"),
		chunk("the airspeed velocity"), orchOutput("t-orch", 1200, 180),
	)

	before := runtime.NumGoroutine()

	s, err := h.client.InvokeAgent(context.Background(), bedrock.AgentRequest{
		BudgetID: "child", RequestID: "agent-abandoned", Input: agentRequest(), MaxCost: dollars(t, "1.00"),
	})
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	// One event, then walk away entirely: no further read, no Close, no cancel.
	<-s.Events()

	// The pump must reach a terminal state on its own.
	waitFor(t, func() bool { return s.Result() != nil })

	res := s.Result()
	if res.Settled {
		t.Error("an abandoned turn whose models reported no usage cannot have settled")
	}
	rec := getRecord(t, acts, "agent-abandoned")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding: abandoning a stream is not evidence the agent stopped",
			rec.Status)
	}
	if rec.ActualCost.Known() {
		t.Errorf("ActualCost = %s, want an unknown", rec.ActualCost)
	}
	if tot := h.scopeTotals(t, "team"); tot.Reserved == 0 {
		t.Error("the hold must be retained: the agent may have invoked models already")
	}
	// The provider's stream is closed exactly once, by the owner.
	if got := h.reader.closeCount(); got != 1 {
		t.Errorf("the provider stream was closed %d times, want exactly 1", got)
	}
	// No immortal pump and no immortal keepalive.
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })

	r, err := h.ledger.Get(context.Background(), res.ReservationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	renewals := r.RenewCount
	time.Sleep(250 * time.Millisecond)
	r, err = h.ledger.Get(context.Background(), res.ReservationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.RenewCount != renewals {
		t.Errorf("the hold renewed %d more times after the turn ended: the keepalive is immortal",
			r.RenewCount-renewals)
	}
}

// Usage the provider already reported cannot be lost by abandonment. The pump
// observes each event before forwarding it, so a caller who walks away at exactly
// the wrong instant cannot make throttle forget spend it had already seen.
func TestAbandonedAgentStreamStillChargesObservedUsage(t *testing.T) {
	h := newAgentHarness(t, "1000",
		func(c *bedrock.Config) { c.StreamStallTimeout = 100 * time.Millisecond })
	h.reader.emit(
		orchInput("t-orch", sonnetID), orchOutput("t-orch", 1000, 100),
		chunk("the airspeed velocity"), chunk(" is about 11 metres per second"),
	)

	s, err := h.invoke(t, context.Background(), "agent-abandoned-paid", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	// Read up to and including the usage event, then abandon the stream.
	<-s.Events()
	<-s.Events()

	waitFor(t, func() bool { return s.Result() != nil })

	res := s.Result()
	if !res.Settled {
		t.Fatalf("usage the provider reported before abandonment is real spend: %v", s.Err())
	}
	want := dollars(t, "0.0045")
	if res.Cost.Amount != want {
		t.Errorf("Cost = %s, want the observed %s", res.Cost, want)
	}
	if tot := h.totals(t); tot.Spent != want || tot.Reserved != 0 {
		t.Errorf("spent %s reserved %s, want %s charged and the hold resolved",
			tot.Spent, tot.Reserved, want)
	}
}

// 13. Concurrent agent invocations against a child cannot oversubscribe the
// parent's ceiling. Every admitted turn's hold is real, so the parent's arithmetic
// holds however many callers race.
func TestConcurrentAgentTurnsCannotOversubscribeAnAncestor(t *testing.T) {
	// The parent allows $0.50 and each turn declares a $0.10 ceiling, so at most
	// five can be admitted at once however many are attempted.
	h := newAgentHarnessWithLease(t, "0.50", time.Minute)

	const attempts = 12
	readers := make([]*fakeAgentReader, attempts)
	for i := range readers {
		readers[i] = newFakeAgentReader()
	}
	h.agent.readers = readers
	// Readers that were never handed out still have a producer goroutine parked on
	// their channel; closing them releases it.
	t.Cleanup(func() {
		for _, r := range readers {
			r.Close()
		}
	})

	// Each turn holds its usage back until released, so every admitted hold is live
	// at the same time.
	release := make(chan struct{})
	for i, r := range readers {
		id := fmt.Sprintf("t-%d", i)
		go func(r *fakeAgentReader, id string) {
			defer close(r.events)
			select {
			case r.events <- orchInput(id, sonnetID):
			case <-r.done:
				return
			}
			select {
			case <-release:
			case <-r.done:
				return
			}
			select {
			case r.events <- orchOutput(id, 1000, 100):
			case <-r.done:
				return
			}
		}(r, id)
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		streams  []*bedrock.AgentStream
		admitted int
		denied   int
	)
	for i := range attempts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := h.client.InvokeAgent(context.Background(), bedrock.AgentRequest{
				BudgetID:  "child",
				RequestID: fmt.Sprintf("agent-conc-%d", i),
				Input:     agentRequest(),
				MaxCost:   dollars(t, "0.10"),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				denied++
				return
			}
			admitted++
			streams = append(streams, s)
		}(i)
	}
	wg.Wait()

	if admitted == 0 {
		t.Fatal("no invocation was admitted, so this test proves nothing")
	}
	if admitted > 5 {
		t.Errorf("admitted %d turns at $0.10 against a $0.50 parent: the ancestor was oversubscribed",
			admitted)
	}
	if denied == 0 {
		t.Error("the parent's ceiling must have refused something")
	}
	// The parent's reserved position matches exactly what was admitted: nothing
	// double-counted, nothing lost.
	if got := h.scopeTotals(t, "team").Reserved; got != money.Money(admitted)*dollars(t, "0.10") {
		t.Errorf("parent reserved = %s, want %d admitted holds at $0.10", got, admitted)
	}

	close(release)
	for _, s := range streams {
		drainAgent(t, s)
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}

	tot := h.scopeTotals(t, "team")
	if tot.Reserved != 0 || tot.PendingCount != 0 {
		t.Errorf("reserved = %s in %d holds, want all resolved", tot.Reserved, tot.PendingCount)
	}
	if want := money.Money(admitted) * dollars(t, "0.0045"); tot.Spent != want {
		t.Errorf("spent = %s, want %s: one charge per admitted turn", tot.Spent, want)
	}
}

// 14. Concurrent Closes, and a Close long after the turn completed, must reach
// exactly one terminal accounting action. The provider's stream closes once and the
// budget is charged once.
func TestAgentConcurrentAndRepeatedCloseSettleOnce(t *testing.T) {
	h := newAgentHarness(t, "1000")
	h.reader.emit(normalAgentTurn(sonnetID)...)

	s, err := h.invoke(t, context.Background(), "agent-close-race", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)

	const closers = 8
	var wg sync.WaitGroup
	errs := make([]error, closers)
	for i := range closers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.Close()
		}(i)
	}
	wg.Wait()

	// Every Close reports the same outcome, because there is only one.
	for i, err := range errs {
		if err != errs[0] {
			t.Errorf("Close %d returned %v, want the same terminal outcome as %v", i, err, errs[0])
		}
	}
	if err := s.Close(); err != errs[0] {
		t.Errorf("late Close returned %v, want %v", err, errs[0])
	}

	if got := h.reader.closeCount(); got != 1 {
		t.Errorf("the provider stream was closed %d times, want exactly 1", got)
	}
	want := dollars(t, "0.01995")
	if tot := h.totals(t); tot.Spent != want {
		t.Errorf("spent = %s, want %s charged exactly once", tot.Spent, want)
	}
	charges, err := h.charges(t)
	if err != nil {
		t.Fatalf("charges: %v", err)
	}
	if len(charges) != 1 {
		t.Errorf("recorded %d charges, want exactly 1", len(charges))
	}
}

// 15. A step whose usage arrives without a matching input event has real spend and
// no model identity. The invocation stays representable: an unnamed model is
// unpriceable, and substituting the agent's configured model would be a guess
// presented as a measurement.
func TestAgentStepWithUnknownModelIdentityRemainsRepresentable(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.reader.emit(
		preInput("t-pre", sonnetID), preOutput("t-pre", 400, 40),
		// Usage with no preceding input event: the model is never named.
		orchOutput("t-orphan", 1200, 180),
	)

	s, err := h.invoke(t, context.Background(), "agent-unknown-model", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); !errors.Is(err, bedrock.ErrCostUnresolved) {
		t.Fatalf("Close error = %v, want ErrCostUnresolved", err)
	}

	rec := getRecord(t, acts, "agent-unknown-model")
	if len(rec.Agent.Steps) != 2 {
		t.Fatalf("recorded %d steps, want both including the unnamed one", len(rec.Agent.Steps))
	}
	orphan := rec.Agent.Steps[1]
	if orphan.Identity.ProviderModelID != "" {
		t.Errorf("unnamed step model = %q, want empty rather than a guessed identity",
			orphan.Identity.ProviderModelID)
	}
	// Its usage is preserved: it is the only evidence of the spend.
	if got := orphan.Usage.Count(usage.InputTokens); got != 1200 {
		t.Errorf("unnamed step input = %d, want 1200 preserved", got)
	}
	if orphan.Cost.Known() {
		t.Errorf("unnamed step cost = %s, want unknown", orphan.Cost)
	}
	// The reason distinguishes "the provider never named it" from "the catalog has a
	// gap": different problems with different fixes.
	if !strings.Contains(orphan.Cost.Reason, "without naming the model") {
		t.Errorf("reason = %q, want it to say the provider never named the model", orphan.Cost.Reason)
	}
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want unresolved", rec.Status)
	}
	if tot := h.totals(t); tot.Reserved == 0 {
		t.Error("the hold must stay encumbered while part of the turn is unpriceable")
	}
}

// A failing model invocation still burned tokens, and those are real spend.
func TestAgentFailureTraceWithUsageIsStillCharged(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	// The failure reports usage against the trace ID of the step that named its
	// model, so the tokens it burned are priceable.
	h.reader.emit(
		orchInput("t-orch", sonnetID), orchOutput("t-orch", 1000, 100),
		failureTrace("t-orch", usageMeta(300, 20)),
	)

	s, err := h.invoke(t, context.Background(), "agent-failed", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res := s.Result()
	if !res.Settled {
		t.Fatalf("tokens burned by a failing step are real spend: %v", s.Err())
	}
	if got := res.Usage.Count(usage.InputTokens); got != 1300 {
		t.Errorf("input tokens = %d, want the failing step's 300 included", got)
	}
	// 1300 at $3/M plus 120 at $15/M.
	if want := dollars(t, "0.0057"); res.Cost.Amount != want {
		t.Errorf("Cost = %s, want %s", res.Cost, want)
	}

	rec := getRecord(t, acts, "agent-failed")
	// The failure code is kept; the failure reason is not, because it is
	// service-generated prose that can quote the prompt or the model's output.
	if !strings.Contains(rec.Agent.Note, "failure") || !strings.Contains(rec.Agent.Note, "424") {
		t.Errorf("note = %q, want the reported failure and its code", rec.Agent.Note)
	}
	assertNoAgentContent(t, rec)
	assertNoContentInRow(t, acts, "agent-failed")
}

// A routing classifier and a collaborator's model invocations are visible as
// accounting detail beneath the one transaction. Visibility, not sub-budget
// enforcement: the spend still lands on the budget that admitted the turn.
func TestAgentCollaboratorUsageIsDetailNotASubBudget(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.reader.emit(
		routingInput("t-route", haikuID), routingOutput("t-route", 200, 20),
		orchInputCollab("swallow-specialist", "t-collab", sonnetID),
		orchOutputCollab("swallow-specialist", "t-collab", 1000, 100),
		postInput("t-post", sonnetID), postOutput("t-post", 300, 30),
	)

	s, err := h.invoke(t, context.Background(), "agent-collab", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec := getRecord(t, acts, "agent-collab")
	if len(rec.Agent.Steps) != 3 {
		t.Fatalf("recorded %d steps, want the routing, collaborator, and postprocessing invocations",
			len(rec.Agent.Steps))
	}
	if rec.Agent.Steps[0].Kind != "routing-classifier" {
		t.Errorf("step 0 kind = %q, want routing-classifier", rec.Agent.Steps[0].Kind)
	}
	// Two different models in one turn, each priced from its own captured quote.
	if rec.Agent.Steps[0].Identity.ProviderModelID != haikuID {
		t.Errorf("routing model = %q, want %q", rec.Agent.Steps[0].Identity.ProviderModelID, haikuID)
	}
	if rec.Agent.Steps[1].Collaborator != "swallow-specialist" {
		t.Errorf("collaborator = %q, want the delegated agent named as a dimension",
			rec.Agent.Steps[1].Collaborator)
	}
	if rec.Agent.Steps[1].Identity.ProviderModelID != sonnetID {
		t.Errorf("collaborator model = %q, want %q",
			rec.Agent.Steps[1].Identity.ProviderModelID, sonnetID)
	}

	// Haiku 200 in at $1/M and 20 out at $5/M; Sonnet 1300 in at $3/M and 130 out at
	// $15/M.
	want := dollars(t, "0.00615")
	if rec.ActualCost.Amount != want {
		t.Errorf("cost = %s, want %s: two models, one accumulated charge", rec.ActualCost, want)
	}
	// One charge against one budget. A collaborator is not a sub-budget.
	charges, err := h.charges(t)
	if err != nil {
		t.Fatalf("charges: %v", err)
	}
	if len(charges) != 1 {
		t.Errorf("recorded %d charges, want 1: collaborator spend is detail, not a separate transaction",
			len(charges))
	}
}

// A caller who declares no ceiling has no knowable pre-cost. Enforce mode refuses,
// because it cannot honestly govern spend it cannot measure -- and it refuses before
// the provider is called, which is the only refusal worth anything.
func TestEnforceRefusesAnAgentTurnWithNoDeclaredCeiling(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)

	_, err := h.invoke(t, context.Background(), "agent-no-ceiling", 0)
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("error = %v, want engine.ErrCostUnknown", err)
	}
	if h.agent.callCount() != 0 {
		t.Errorf("the agent was invoked %d times, want 0", h.agent.callCount())
	}
	if tot := h.totals(t); tot.Reserved != 0 || tot.Spent != 0 {
		t.Errorf("reserved %s, spent %s, want an untouched budget", tot.Reserved, tot.Spent)
	}

	rec := getRecord(t, acts, "agent-no-ceiling")
	if rec.Status != activity.StatusDenied {
		t.Errorf("status = %q, want denied", rec.Status)
	}
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("outcome = %q, want unpriced: the reason was an unknowable cost, not a spent budget",
			rec.Outcome)
	}
	if !strings.Contains(rec.Estimate.Cost.Reason, "MaxCost") {
		t.Errorf("reason = %q, want it to name the knob that fixes this", rec.Estimate.Cost.Reason)
	}
}

// Monitor mode observes rather than governs, so it admits a turn whose pre-cost is
// unknown -- against a zero hold, with the gap recorded.
func TestMonitorAdmitsAnAgentTurnWithUnknownPreCost(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	if err := h.engine.SetMode("team", engine.ModeMonitor); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	h.reader.emit(normalAgentTurn(sonnetID)...)

	s, err := h.invoke(t, context.Background(), "agent-monitor", 0)
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	if !s.Decision().CostUnknown {
		t.Error("the decision must record that the cost was unknown at admission")
	}
	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res := s.Result()
	if !res.Settled {
		t.Fatalf("the observed spend must still settle: %v", s.Err())
	}
	if res.Estimate.Cost.Known() {
		t.Errorf("estimate = %s, want an explicit unknown", res.Estimate.Cost)
	}
	// Nothing was reserved, but the actual spend is measured exactly.
	want := dollars(t, "0.01995")
	if res.Cost.Amount != want {
		t.Errorf("Cost = %s, want the measured %s", res.Cost, want)
	}

	rec := getRecord(t, acts, "agent-monitor")
	if rec.EnforcementMode != engine.ModeMonitor {
		t.Errorf("mode = %q, want monitor: a reader must be able to tell the ceiling did not apply",
			rec.EnforcementMode)
	}
	if rec.Reserved != 0 {
		t.Errorf("Reserved = %s, want a zero hold for an unpriceable admission", rec.Reserved)
	}
}

// The declared ceiling is what gets reserved, and it is labelled a heuristic rather
// than a conservative bound: AWS will not stop the agent at throttle's number.
func TestAgentEstimateIsTheDeclaredCeilingAsAHeuristic(t *testing.T) {
	h := newAgentHarness(t, "1000")
	h.reader.hang()

	s, err := h.invoke(t, context.Background(), "agent-ceiling", dollars(t, "0.25"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	// The hold is live and equals the declared ceiling.
	if got := h.totals(t).Reserved; got != dollars(t, "0.25") {
		t.Errorf("reserved = %s, want the declared $0.25 ceiling", got)
	}

	h.reader.emit(normalAgentTurn(sonnetID)...)
	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res := s.Result()
	if res.Estimate.Quality != usage.QualityHeuristic {
		t.Errorf("quality = %q, want heuristic: a declared ceiling is not a bound AWS will honour",
			res.Estimate.Quality)
	}
	if res.Estimate.Cost.Amount != dollars(t, "0.25") {
		t.Errorf("estimated cost = %s, want the declared ceiling", res.Estimate.Cost)
	}
	if !res.Estimate.Usage.Empty() {
		t.Error("estimated usage must be empty: no token count is knowable before the agent runs")
	}
	if !strings.Contains(res.Estimate.Note, "does not stop the agent") {
		t.Errorf("note = %q, want it to state that AWS will not stop the agent at this figure",
			res.Estimate.Note)
	}
	// Actual spend is far below the ceiling, and the settled figure is the actual.
	if res.Cost.Amount >= dollars(t, "0.25") {
		t.Errorf("Cost = %s, want the measured actual rather than the reserved ceiling", res.Cost)
	}
	if tot := h.totals(t); tot.Spent != res.Cost.Amount || tot.Reserved != 0 {
		t.Errorf("spent %s reserved %s, want the actual charged and the hold released",
			tot.Spent, tot.Reserved)
	}
}

// A price refresh landing mid-turn must not change what the turn costs. The models
// are unknowable at admission, so the whole candidate rate set is frozen there and
// settlement is a lookup in the frozen set.
func TestAgentSettlementUsesTheQuoteSetFrozenAtAdmission(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "15.00")),
		},
		Provenance: pricing.Provenance{Source: "test", Version: "pre-refresh"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	h := newAgentHarness(t, "1000", withActs, func(c *bedrock.Config) { c.Catalog = cat })

	// The turn reports its first invocation, then waits while the catalog is
	// repriced underneath it, then reports the second.
	release := make(chan struct{})
	go func() {
		defer close(h.reader.events)
		h.reader.events <- orchInput("t-orch", sonnetID)
		h.reader.events <- orchOutput("t-orch", 1_000_000, 0)
		<-release
		h.reader.events <- postInput("t-post", sonnetID)
		h.reader.events <- postOutput("t-post", 1_000_000, 0)
	}()

	s, err := h.invoke(t, context.Background(), "agent-frozen", dollars(t, "100.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	<-s.Events()
	<-s.Events()

	if err := cat.Override(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "300.00")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "1500.00")),
		},
	}); err != nil {
		t.Fatalf("Override: %v", err)
	}
	close(release)

	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 2M input tokens at the frozen $3/M is $6.00. A per-step lookup against the
	// live catalog would have priced the first step at $3 and the second at $300,
	// which is the failure a frozen set exists to prevent.
	want := dollars(t, "6.00")
	res := s.Result()
	if res.Cost.Amount != want {
		t.Errorf("Cost = %s, want %s from the rates frozen at admission", res.Cost, want)
	}

	rec := getRecord(t, acts, "agent-frozen")
	if rec.ActualCost.Amount != want {
		t.Errorf("persisted cost = %s, want the frozen basis", rec.ActualCost)
	}
	// The persisted quote set replays to the same figure, so the record stays
	// reproducibly priceable after the catalog has moved on.
	if !rec.Quotes.Valid() {
		t.Fatal("the captured quote set must be persisted, or the turn cannot be re-priced")
	}
	if len(rec.Agent.Steps) != 2 {
		t.Fatalf("recorded %d steps, want 2", len(rec.Agent.Steps))
	}
	replayed, _, err := rec.Quotes.PriceComponents([]pricing.Component{
		{Identity: rec.Agent.Steps[0].Identity, Usage: rec.Agent.Steps[0].Usage},
		{Identity: rec.Agent.Steps[1].Identity, Usage: rec.Agent.Steps[1].Usage},
	})
	if err != nil {
		t.Fatalf("replaying from the persisted quote set: %v", err)
	}
	if replayed.Amount != want {
		t.Errorf("replayed cost = %s, want the recorded %s", replayed, want)
	}
	// Only the quotes actually used are retained, not a whole catalog snapshot on
	// every record.
	if got := rec.Quotes.Models(); len(got) != 1 || got[0] != sonnetID {
		t.Errorf("retained quotes = %v, want only the model the turn invoked", got)
	}
}

// A slow consumer gets backpressure rather than an unbounded buffer, so a governed
// turn cannot be made to accumulate the whole response in memory.
func TestAgentSlowConsumerGetsBackpressure(t *testing.T) {
	h := newAgentHarness(t, "1000")

	sent := make(chan int, 32)
	go func() {
		defer close(h.reader.events)
		for i := range 8 {
			select {
			case h.reader.events <- chunk(fmt.Sprintf("part %d", i)):
				sent <- i
			case <-h.reader.done:
				return
			}
		}
		for _, ev := range []agenttypes.ResponseStream{
			orchInput("t-orch", sonnetID), orchOutput("t-orch", 1000, 100),
		} {
			select {
			case h.reader.events <- ev:
			case <-h.reader.done:
				return
			}
		}
	}()

	s, err := h.invoke(t, context.Background(), "agent-backpressure", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}

	// Read one event, then stop for a moment. With unbuffered forwarding the
	// producer can be at most one event ahead of the consumer.
	<-s.Events()
	time.Sleep(50 * time.Millisecond)
	if got := len(sent); got > 3 {
		t.Errorf("the provider produced %d events while the caller had read 1: the stream is buffering",
			got)
	}

	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.Result().Settled {
		t.Errorf("the turn should have settled: %v", s.Err())
	}
}

// A client built without an agent client refuses clearly rather than panicking.
func TestInvokeAgentRequiresAnAgentClient(t *testing.T) {
	h := newHarness(t, "1000")
	_, err := h.client.InvokeAgent(context.Background(), bedrock.AgentRequest{
		BudgetID: "team", RequestID: "agent-none", Input: agentRequest(), MaxCost: dollars(t, "1.00"),
	})
	if !errors.Is(err, bedrock.ErrNoAgentClient) {
		t.Fatalf("error = %v, want ErrNoAgentClient", err)
	}
}

// A success with no event stream is not something the API should do, and throttle
// cannot tell whether the agent ran. The hold stays: guessing "free" would be a
// silent understatement.
func TestAgentWithNoEventStreamLeavesTheHold(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	api := &fakeAgentAPI{useRaw: true, stream: nil}
	h := newHarness(t, "1000", withActs, withAgent(api))

	_, err := h.client.InvokeAgent(context.Background(), bedrock.AgentRequest{
		BudgetID: "team", RequestID: "agent-nostream", Input: agentRequest(), MaxCost: dollars(t, "1.00"),
	})
	if !errors.Is(err, bedrock.ErrAccounting) {
		t.Fatalf("error = %v, want ErrAccounting", err)
	}
	if tot := h.totals(t); tot.Reserved == 0 {
		t.Error("the hold must stay when throttle cannot tell whether the agent ran")
	}
	rec := getRecord(t, acts, "agent-nostream")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}
	if rec.ActualCost.Known() {
		t.Errorf("ActualCost = %s, want an unknown", rec.ActualCost)
	}
}

// The invocation is validated before any budget is touched and before the provider
// is called. A missing agent, alias, or session identifier is refused too: without
// them the record could not attribute the spend to anything.
func TestInvokeAgentValidatesItsRequest(t *testing.T) {
	strip := func(f func(*bedrock.AgentRequest)) bedrock.AgentRequest {
		req := bedrock.AgentRequest{BudgetID: "team", Input: agentRequest(), MaxCost: dollars(t, "1.00")}
		f(&req)
		return req
	}
	cases := []struct {
		name string
		req  bedrock.AgentRequest
	}{
		{"no budget", strip(func(r *bedrock.AgentRequest) { r.BudgetID = "" })},
		{"no input", strip(func(r *bedrock.AgentRequest) { r.Input = nil })},
		{"no agent id", strip(func(r *bedrock.AgentRequest) { r.Input.AgentId = nil })},
		{"no alias id", strip(func(r *bedrock.AgentRequest) { r.Input.AgentAliasId = nil })},
		{"no session id", strip(func(r *bedrock.AgentRequest) { r.Input.SessionId = nil })},
		{"negative ceiling", strip(func(r *bedrock.AgentRequest) { r.MaxCost = -1 })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgentHarness(t, "1000")
			if _, err := h.client.InvokeAgent(context.Background(), tc.req); err == nil {
				t.Fatal("want an error")
			}
			if h.agent.callCount() != 0 {
				t.Error("validation must happen before the provider is called")
			}
			if tot := h.totals(t); tot.Reserved != 0 || tot.Spent != 0 {
				t.Errorf("reserved %s, spent %s, want an untouched budget", tot.Reserved, tot.Spent)
			}
		})
	}
}

// The pre-call activity write is what makes a crashed agent turn visible. Without
// it, a process that dies mid-turn is indistinguishable from one that never ran --
// and an agent turn is long enough for that to matter.
func TestAgentActivityIsWrittenBeforeTheProviderIsCalled(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newAgentHarness(t, "1000", withActs)
	h.reader.hang()

	s, err := h.invoke(t, context.Background(), "agent-pending", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}

	rec := getRecord(t, acts, "agent-pending")
	if rec.Status != activity.StatusPending {
		t.Errorf("in-flight status = %q, want pending", rec.Status)
	}
	if rec.ReservationID == "" {
		t.Error("the pre-call record must name the hold it is consuming")
	}
	if rec.Agent.AgentID != "AGENT123456" {
		t.Errorf("in-flight agent id = %q, want it recorded before the call", rec.Agent.AgentID)
	}
	if rec.ActualCost.Known() {
		t.Error("an in-flight turn has no known cost yet")
	}
	if got := s.Result(); got != nil {
		t.Error("Result must not be available before the stream is terminal")
	}
	_ = s.Close()
}

// An agent record must survive the process that wrote it, including its per-step
// detail and its quote set. This is what a later reconciliation reads.
func TestAgentActivitySurvivesProcessRestart(t *testing.T) {
	path := t.TempDir() + "/activity.db"
	store, withActs := withActivity(t, path)
	h := newAgentHarness(t, "1000", withActs)
	h.reader.emit(normalAgentTurn(sonnetID)...)

	s, err := h.invoke(t, context.Background(), "agent-restart", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgent: %v", err)
	}
	drainAgent(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	before := getRecord(t, store, "agent-restart")
	if err := store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}

	reopened := reopenActivity(t, path)
	after, err := reopened.Get(context.Background(), "agent-restart")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if !after.ActualCost.Known() || after.ActualCost.Amount != before.ActualCost.Amount {
		t.Errorf("cost after restart = %s, want %s", after.ActualCost, before.ActualCost)
	}
	if len(after.Agent.Steps) != len(before.Agent.Steps) {
		t.Fatalf("steps after restart = %d, want %d", len(after.Agent.Steps), len(before.Agent.Steps))
	}
	for i := range after.Agent.Steps {
		if after.Agent.Steps[i].Identity != before.Agent.Steps[i].Identity {
			t.Errorf("step %d identity after restart = %+v, want %+v",
				i, after.Agent.Steps[i].Identity, before.Agent.Steps[i].Identity)
		}
		if after.Agent.Steps[i].Usage.Count(usage.InputTokens) !=
			before.Agent.Steps[i].Usage.Count(usage.InputTokens) {
			t.Errorf("step %d usage did not survive the restart", i)
		}
	}
	if !after.Quotes.Valid() {
		t.Error("the quote set must survive the restart, or the turn cannot be re-priced")
	}
	if after.Agent.SessionID != before.Agent.SessionID || after.Agent.Version != before.Agent.Version {
		t.Error("the agent identifiers must survive the restart")
	}
	assertNoAgentContent(t, after)
}

// An agent turn works without an activity store, and a store failure never fails
// the invocation: the caller has already paid for the answer.
func TestAgentWorksWithoutActivityAndSurvivesStoreFailure(t *testing.T) {
	t.Run("no store", func(t *testing.T) {
		h := newAgentHarness(t, "1000")
		h.reader.emit(normalAgentTurn(sonnetID)...)
		s, err := h.invoke(t, context.Background(), "agent-nostore", dollars(t, "1.00"))
		if err != nil {
			t.Fatalf("InvokeAgent: %v", err)
		}
		drainAgent(t, s)
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if !s.Result().Settled {
			t.Error("the turn must settle without an activity store")
		}
	})

	t.Run("broken store", func(t *testing.T) {
		h := newAgentHarness(t, "1000", func(c *bedrock.Config) { c.Activity = brokenStore{} })
		h.reader.emit(normalAgentTurn(sonnetID)...)
		s, err := h.invoke(t, context.Background(), "agent-brokenstore", dollars(t, "1.00"))
		if err != nil {
			t.Fatalf("a telemetry failure must not fail the invocation: %v", err)
		}
		drainAgent(t, s)
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if !s.Result().Settled {
			t.Error("the turn must still settle")
		}
	})
}

// bannedTraceContent is every piece of content the test traces carry: prompts,
// model responses, reasoning, rationale, action parameters, retrieved passages, a
// routing decision, a failure reason, and the names of things whose payloads are
// content. None of it may appear in a durable record.
var bannedTraceContent = []string{
	"airspeed velocity",
	"11 metres per second",
	"I should look up",
	"the user wants",
	"unladen swallow",
	"European swallow",
	"route to the swallow",
	"could not answer",
	"You are a helpful agent",
	"the user asked",
	"lookupVelocity",
	"swallow-facts",
}

// assertNoAgentContent proves the compound-invocation detail holds no trace
// content. Every field of Agent and AgentStep is checked, because the guarantee is
// a property of the type and not of one writer's care.
func assertNoAgentContent(t *testing.T, rec activity.Record) {
	t.Helper()
	fields := []string{
		rec.Agent.Note, rec.Agent.AgentID, rec.Agent.AliasID,
		rec.Agent.Version, rec.Agent.SessionID,
	}
	for _, st := range rec.Agent.Steps {
		fields = append(fields, st.Kind, st.TraceID, st.Collaborator,
			st.Identity.ProviderModelID, st.Identity.CanonicalModel, st.Identity.Publisher,
			st.Identity.Family, st.Cost.Reason)
	}
	for kind := range rec.Agent.Events {
		fields = append(fields, kind)
	}
	for _, f := range fields {
		for _, b := range bannedTraceContent {
			if strings.Contains(f, b) {
				t.Errorf("agent detail field %q contains trace content %q", f, b)
			}
		}
	}
}

// assertNoContentInRow reads the whole persisted row as text and checks every
// column. It is the backstop for the typed assertions: a leak into any column, or
// into any nested JSON blob, shows up here.
func assertNoContentInRow(t *testing.T, s *activitysqlite.Store, requestID string) {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT * FROM activity WHERE request_id = ?`, requestID)
	if err != nil {
		t.Fatalf("select row: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("no row for %q", requestID)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for i, v := range vals {
		text := fmt.Sprint(v)
		for _, b := range bannedTraceContent {
			if strings.Contains(text, b) {
				t.Errorf("column %q holds trace content %q", cols[i], b)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// reopenActivity opens an existing activity database, standing in for the next
// process to start.
func reopenActivity(t *testing.T, path string) *activitysqlite.Store {
	t.Helper()
	store, err := activitysqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopening the activity store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
