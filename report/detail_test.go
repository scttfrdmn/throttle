package report

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"throttle/activity"
	"throttle/money"
	"throttle/usage"
)

// agentRecord is a managed agent turn: ONE governed transaction with internal model
// invocations beneath it.
func agentRecord(requestID, budgetID, periodID string, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, periodID, cents(90), at)
	rec.Identity.Operation = "invoke-agent"
	rec.Estimate.Quality = usage.QualityHeuristic
	rec.Agent = activity.Agent{
		AgentID:   "AGENT123",
		AliasID:   "TSTALIASID",
		Version:   "3",
		SessionID: "sess-abc",
		Events:    map[string]int{"tool-invocation": 2, "knowledge-base-lookup": 1},
		Note:      "non-model activity occurred that the provider does not price per call",
		Steps: []activity.AgentStep{
			{
				Seq: 1, Kind: "preprocessing", TraceID: "trace-1",
				Identity: bedrockIdentity(),
				Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 300, usage.OutputTokens: 20}),
				Cost:     usage.KnownCost(cents(10)),
				Latency:  200 * time.Millisecond,
				At:       at,
			},
			{
				Seq: 2, Kind: "orchestration", TraceID: "trace-2",
				Collaborator: "researcher",
				Identity:     bedrockIdentity(),
				Usage:        usage.New(map[usage.Dimension]int64{usage.InputTokens: 900, usage.OutputTokens: 250}),
				Cost:         usage.KnownCost(cents(55)),
				Latency:      1200 * time.Millisecond,
				At:           at.Add(300 * time.Millisecond),
			},
			{
				Seq: 3, Kind: "postprocessing",
				// A step whose model the provider never named: a real charge with no
				// identity, which must render as an absence rather than a guess.
				Identity: usage.ModelIdentity{AccessProvider: "aws-bedrock", Operation: "invoke-agent"},
				Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 400, usage.OutputTokens: 60}),
				Cost:     usage.KnownCost(cents(25)),
				Latency:  400 * time.Millisecond,
				At:       at.Add(1600 * time.Millisecond),
			},
		},
	}
	return rec
}

// runtimeRecord is a hosted-runtime invocation whose compute cost arrives out of band
// and has not arrived.
func runtimeRecord(requestID, budgetID, periodID string, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, periodID, cents(0), at)
	rec.Identity = usage.ModelIdentity{
		AccessProvider:  "aws-bedrock-agentcore",
		ProviderModelID: "",
		Operation:       "invoke-agent-runtime",
	}
	rec.ActualUsage = usage.Usage{}
	rec.ActualCost = usage.UnknownCost("hosted runtime resource usage is reported out of band")
	rec.Estimate.Cost = usage.UnknownCost("a hosted runtime's compute cost is not knowable at admission")
	rec.Estimate.Quality = usage.QualityHeuristic
	rec.Reserved = dollars(2)
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeUnpriced
	rec.Runtime = activity.HostedRuntime{
		RuntimeID:     "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/my-agent",
		Qualifier:     "DEFAULT",
		Account:       "123456789012",
		SessionID:     "runtime-session-77",
		RequestID:     "provider-req-9",
		TraceID:       "trace-xyz",
		StatusCode:    200,
		ContentType:   "application/json",
		PayloadBytes:  812,
		ResponseBytes: 2044,
		Note:          "session-level resource usage is not divisible across invocations",
	}
	return rec
}

// --- agent compound transactions -------------------------------------------

// The compound transaction is visible as detail beneath one governed request, and the
// steps carry measurements only.
func TestAgentDetailRendersTheCompoundTransaction(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.record(agentRecord("r1", "research", p.ID, w.now))

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if d.Agent == nil {
		t.Fatal("Agent detail is nil for a record with agent steps")
	}
	if !d.Event.Compound || d.Event.StepCount != 3 {
		t.Errorf("Compound=%v StepCount=%d, want true and 3", d.Event.Compound, d.Event.StepCount)
	}
	if len(d.Agent.Steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(d.Agent.Steps))
	}

	wantKinds := []string{"preprocessing", "orchestration", "postprocessing"}
	for i, k := range wantKinds {
		if d.Agent.Steps[i].Kind != k {
			t.Errorf("step %d kind = %q, want %q", i+1, d.Agent.Steps[i].Kind, k)
		}
		if d.Agent.Steps[i].Seq != i+1 {
			t.Errorf("step %d Seq = %d", i+1, d.Agent.Steps[i].Seq)
		}
		if len(d.Agent.Steps[i].Usage) == 0 {
			t.Errorf("step %d has no usage; the decomposition is the point", i+1)
		}
	}
	if d.Agent.Steps[1].Collaborator != "researcher" {
		t.Errorf("Collaborator = %q, want the delegated agent", d.Agent.Steps[1].Collaborator)
	}

	// The request-level charge is the authoritative figure, and the steps are detail
	// beneath it -- not three transactions.
	if d.Agent.Total.State != CostKnown || d.Agent.Total.Value != cents(90) {
		t.Errorf("Total = %s (%s), want the request-level charge %s",
			d.Agent.Total.Value, d.Agent.Total.State, cents(90))
	}
	if d.Agent.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q", d.Agent.SessionID)
	}
	if d.Agent.Events["tool-invocation"] != 2 {
		t.Errorf("Events = %v, want the non-model activity counts", d.Agent.Events)
	}
	if d.Agent.Note == "" {
		t.Error("Note is empty; the turn's accounting limitation should carry through")
	}
	// A heuristic estimate is a declaration, not a promise, and saying so is the
	// difference between the two.
	if d.Event.EstimateQuality != string(usage.QualityHeuristic) {
		t.Errorf("EstimateQuality = %q, want heuristic for a managed agent turn", d.Event.EstimateQuality)
	}
}

// A step whose model the provider never named still renders, with no identity
// invented for it.
func TestAgentStepWithNoModelIdentityRendersAsAbsent(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.record(agentRecord("r1", "research", p.ID, w.now))

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	step := d.Agent.Steps[2]
	if step.Model != "" || step.ModelKnown {
		t.Errorf("Model = %q (known=%v), want an absent identity rather than the turn's model",
			step.Model, step.ModelKnown)
	}
	if step.Cost.State != CostKnown || step.Cost.Value != cents(25) {
		t.Errorf("Cost = %s (%s); an unidentified model still has a real charge",
			step.Cost.Value, step.Cost.State)
	}
}

// Rounding happens once, at the request level. When the per-step column does not add
// up, the gap is explained rather than made to disappear by adjusting a step.
func TestAgentStepsMayNotSumToTheRequestCharge(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	rec := agentRecord("r1", "research", p.ID, w.now)
	// Steps total 90c. Make the request-level charge a cent higher, as single-rounding
	// of an exactly accumulated turn can.
	rec.ActualCost = usage.KnownCost(cents(91))
	w.record(rec)

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if d.Agent.StepSum != cents(90) {
		t.Errorf("StepSum = %s, want %s", d.Agent.StepSum, cents(90))
	}
	if d.Agent.Total.Value != cents(91) {
		t.Errorf("Total = %s, want the authoritative request charge %s", d.Agent.Total.Value, cents(91))
	}
	if want := cents(1); d.Agent.RoundingGap != want {
		t.Errorf("RoundingGap = %s, want %s", d.Agent.RoundingGap, want)
	}
	if !d.Agent.GapVisible() {
		t.Error("GapVisible() = false for a gap of a whole cent; it will be visible in the column")
	}

	// A sub-half-cent gap needs no explanation, because it will not show.
	rec2 := agentRecord("r2", "research", p.ID, w.now)
	rec2.ActualCost = usage.KnownCost(cents(90) + money.Money(2))
	w.record(rec2)
	d2, err := w.rep.Detail(w.ctx, "r2")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if d2.Agent.GapVisible() {
		t.Errorf("GapVisible() = true for a %s gap, which rounds away", d2.Agent.RoundingGap)
	}
}

// Rounding accounts for fractions of a cent. A gap larger than that is a disagreement
// between the provider's own figures, and calling it rounding would be a display
// explaining away an accounting fact.
func TestGapTooLargeForRoundingIsNotAttributedToRounding(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	// A cent, which is 100 times what rounding five steps and a total could produce.
	rec := agentRecord("r1", "research", p.ID, w.now)
	rec.ActualCost = usage.KnownCost(cents(91))
	w.record(rec)

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if !d.Agent.GapVisible() {
		t.Fatal("GapVisible() = false for a one-cent gap")
	}
	if d.Agent.GapExplainedByRounding() {
		t.Errorf("GapExplainedByRounding() = true for a %s gap across %d steps; display "+
			"rounding cannot produce a whole cent", d.Agent.RoundingGap, len(d.Agent.Steps))
	}

	// The gap rounding really can produce: under half a display unit per figure. It is
	// invisible in the column, and it is also genuinely rounding.
	rec2 := agentRecord("r2", "research", p.ID, w.now)
	rec2.ActualCost = usage.KnownCost(cents(90) + money.Money(120))
	w.record(rec2)
	d2, err := w.rep.Detail(w.ctx, "r2")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if !d2.Agent.GapExplainedByRounding() {
		t.Errorf("GapExplainedByRounding() = false for a %s gap across %d steps, which is "+
			"within the last displayed place", d2.Agent.RoundingGap, len(d2.Agent.Steps))
	}

	// A negative gap -- the steps summing above the charge -- is the same question with the
	// sign flipped, and the seeded fixtures showed it is the direction that actually occurs.
	rec3 := agentRecord("r3", "research", p.ID, w.now)
	rec3.ActualCost = usage.KnownCost(cents(70))
	w.record(rec3)
	d3, err := w.rep.Detail(w.ctx, "r3")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if d3.Agent.RoundingGap >= 0 {
		t.Fatalf("RoundingGap = %s, want a negative gap", d3.Agent.RoundingGap)
	}
	if d3.Agent.GapExplainedByRounding() {
		t.Errorf("GapExplainedByRounding() = true for a %s gap; magnitude is what matters, "+
			"not direction", d3.Agent.RoundingGap)
	}
}

// --- hosted runtime --------------------------------------------------------

// An unreconciled hosted-runtime invocation says its cost is not known. It does not
// say zero, and it does not receive a computed share of a session bill.
func TestHostedRuntimeCostShowsAsUnresolvedNotZero(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("agents", "", dollars(1000)))
	w.record(runtimeRecord("r1", "agents", p.ID, w.now))

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if d.Runtime == nil {
		t.Fatal("Runtime detail is nil for a hosted-runtime invocation")
	}
	if d.Runtime.Reconciled {
		t.Error("Reconciled = true before any out-of-band observation arrived")
	}
	if d.Runtime.ReconciledCost.State != CostUnresolved {
		t.Errorf("ReconciledCost state = %q, want unresolved", d.Runtime.ReconciledCost.State)
	}
	if s := d.Runtime.ReconciledCost.String(); s != "unresolved" {
		t.Errorf("ReconciledCost renders as %q, want %q", s, "unresolved")
	}
	if d.Runtime.ReconciledCost.Reason == "" {
		t.Error("Reason is empty; an unresolved runtime cost must explain why")
	}
	if !d.Runtime.SessionScoped {
		t.Error("SessionScoped = false; a session-level charge is not divisible across invocations")
	}
	if d.Runtime.SessionID != "runtime-session-77" {
		t.Errorf("SessionID = %q; it is the only identifier shared with the usage telemetry", d.Runtime.SessionID)
	}
	if d.Runtime.MaxExposure != dollars(2) {
		t.Errorf("MaxExposure = %s, want the headroom that was held %s", d.Runtime.MaxExposure, dollars(2))
	}
	if d.Runtime.StatusCode != 200 {
		t.Errorf("StatusCode = %d", d.Runtime.StatusCode)
	}
	if d.Runtime.RuntimeID == "" || d.Runtime.Qualifier == "" {
		t.Error("runtime identity is incomplete")
	}

	// The event itself must not read as a settled free request.
	if d.Event.Actual.State == CostKnown {
		t.Error("Actual cost reads as known for an invocation nothing has priced")
	}
	if strings.Contains(d.Event.Actual.String(), "0.00") {
		t.Errorf("Actual renders as %q", d.Event.Actual.String())
	}
	if !d.Event.HostedRuntime {
		t.Error("HostedRuntime = false")
	}
	if !d.Event.AwaitingExternal {
		t.Error("AwaitingExternal = false; this is a designed terminal state, not damage")
	}
}

// Once the observation arrives, it is reported as the later, separately measured
// figure it is.
func TestHostedRuntimeReconciledCostIsKeptSeparate(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("agents", "", dollars(1000)))

	rec := runtimeRecord("r1", "agents", p.ID, w.now)
	rec.Runtime.Reconciled = true
	rec.Runtime.ReconciledUsage = usage.New(map[usage.Dimension]int64{
		usage.RuntimeVCPUNanoHours:     250_000_000,
		usage.RuntimeMemoryNanoGBHours: 900_000_000,
	})
	rec.Runtime.ReconciledCost = usage.KnownCost(cents(37))
	rec.Runtime.ReconciledAt = w.now.Add(20 * time.Minute)
	rec.Runtime.ReconciledFrom = "session-resource-usage"
	w.record(rec)

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if !d.Runtime.Reconciled {
		t.Fatal("Reconciled = false after an observation was attributed")
	}
	if d.Runtime.ReconciledCost.State != CostKnown || d.Runtime.ReconciledCost.Value != cents(37) {
		t.Errorf("ReconciledCost = %s (%s), want a known %s",
			d.Runtime.ReconciledCost.Value, d.Runtime.ReconciledCost.State, cents(37))
	}
	if len(d.Runtime.ReconciledUsage) != 2 {
		t.Errorf("ReconciledUsage has %d dimensions, want 2 runtime dimensions", len(d.Runtime.ReconciledUsage))
	}
	if d.Runtime.ReconciledFrom == "" || d.Runtime.ReconciledAt.IsZero() {
		t.Error("the provenance of a later observation must travel with it")
	}
	// The record's own usage is still empty: a later figure must not be mistaken for
	// something measured at the time of the call.
	if len(d.Event.Usage) != 0 {
		t.Errorf("Event.Usage = %+v, want empty; the call itself reported no usage", d.Event.Usage)
	}
}

// --- reconciliation visibility ---------------------------------------------

// The reconciler's typed reasons carry through verbatim, so an operator sees why a
// record is in the state it is in.
func TestReconciliationTrailIsVisibleWithItsTypedReasons(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	rec := settledRecord("r1", "research", p.ID, cents(412), w.now)
	rec.Repairs = []activity.Reconciliation{
		{
			At:                  w.now.Add(time.Minute),
			Class:               "repaired",
			Reason:              "crash-repairable",
			ObservedStatus:      activity.StatusOutstanding,
			ObservedReservation: "active",
			ProducedStatus:      activity.StatusSettled,
			Money:               "settled",
			Amount:              cents(412),
			QuoteSource:         "aws-price-list",
			QuoteVersion:        "2026-08-01",
			Detail:              "replayed the captured quote",
		},
		{
			At:     w.now.Add(2 * time.Minute),
			Class:  "already_consistent",
			Reason: "",
		},
	}
	w.record(rec)

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if len(d.Repairs) != 2 {
		t.Fatalf("got %d repairs, want 2", len(d.Repairs))
	}
	first := d.Repairs[0]
	if first.Class != "repaired" || first.Reason != "crash-repairable" {
		t.Errorf("Class/Reason = %q/%q, want the reconciler's own vocabulary", first.Class, first.Reason)
	}
	if first.ObservedStatus != string(activity.StatusOutstanding) ||
		first.ProducedStatus != string(activity.StatusSettled) {
		t.Errorf("the before/after statuses did not carry through: %q -> %q",
			first.ObservedStatus, first.ProducedStatus)
	}
	if first.Money != "settled" || first.Amount != cents(412) {
		t.Errorf("money movement = %q %s, want settled %s", first.Money, first.Amount, cents(412))
	}
	// A replayed settlement must prove which immutable quote priced it, rather than
	// whatever the catalog says now.
	if first.QuoteSource != "aws-price-list" || first.QuoteVersion != "2026-08-01" {
		t.Errorf("quote provenance = %q/%q", first.QuoteSource, first.QuoteVersion)
	}
	if !d.Event.Repaired {
		t.Error("Repaired = false on a record carrying a reconciliation trail")
	}

	// And the summary counts it, so the reconciliation panel has something to show.
	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Health.Repaired != 1 {
		t.Errorf("Health.Repaired = %d, want 1", sum.Health.Repaired)
	}
}

// The reconciliation panel's counts distinguish the three reasons a total can be
// incomplete, because they call for different responses.
func TestHealthSeparatesUnresolvedUnknownAndAwaiting(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	unpriced := settledRecord("r1", "research", p.ID, cents(0), w.now)
	unpriced.Status = activity.StatusUnresolved
	unpriced.Outcome = activity.OutcomeUnpriced
	unpriced.ActualCost = usage.PartialCost(cents(10),
		[]usage.Dimension{usage.CacheWriteTokens}, "no rate for cache writes")
	w.record(unpriced)

	crashed := settledRecord("r2", "research", p.ID, cents(0), w.now)
	crashed.Status = activity.StatusOutstanding
	crashed.Outcome = activity.OutcomeAccountingError
	crashed.ActualCost = usage.UnknownCost("the process died mid-call")
	w.record(crashed)

	w.record(runtimeRecord("r3", "research", p.ID, w.now))

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	h := sum.Health
	if h.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1 (the unpriced request)", h.Unresolved)
	}
	if h.OutcomeUnknown != 1 {
		t.Errorf("OutcomeUnknown = %d, want 1 (the crashed request)", h.OutcomeUnknown)
	}
	if h.AwaitingExternal != 1 {
		t.Errorf("AwaitingExternal = %d, want 1 (the hosted runtime invocation)", h.AwaitingExternal)
	}
	if h.Clean() {
		t.Error("Clean() = true with three incomplete records")
	}
	if !h.Needs() {
		t.Error("Needs() = false with an unpriced request and a crashed one")
	}
	if !reflect.DeepEqual(h.UnpricedDimensions, []string{string(usage.CacheWriteTokens)}) {
		t.Errorf("UnpricedDimensions = %v, want the dimension the catalog is missing", h.UnpricedDimensions)
	}
	if sum.Activity.Complete {
		t.Error("Activity.Complete = true; the period total is a floor")
	}
}

// A period whose records all settled needs no reconciliation attention.
func TestHealthIsCleanWhenEverythingSettled(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.record(settledRecord("r1", "research", p.ID, cents(412), w.now))

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if !sum.Health.Clean() {
		t.Errorf("Clean() = false with one settled request: %+v", sum.Health)
	}
	if sum.Health.Needs() {
		t.Error("Needs() = true with nothing outstanding")
	}
	if !sum.Activity.Complete {
		t.Error("Activity.Complete = false with one fully priced request")
	}
}

// Unresolved is a read. Listing what needs repair must never repair it.
func TestUnresolvedListsWithoutMovingMoney(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", cents(412), w.now)
	w.record(settledRecord("r1", "research", p.ID, cents(412), w.now))

	stuck := settledRecord("r2", "research", p.ID, cents(0), w.now)
	stuck.Status = activity.StatusUnresolved
	stuck.ActualCost = usage.UnknownCost("no rate")
	w.record(stuck)

	before, err := w.led.Totals(w.ctx, ledgerScope("research", p.ID), w.now)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}

	page, err := w.rep.Unresolved(w.ctx, ActivityQuery{BudgetID: "research", PeriodID: p.ID})
	if err != nil {
		t.Fatalf("Unresolved: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].RequestID != "r2" {
		t.Fatalf("Unresolved returned %d events, want just the stuck one: %+v", len(page.Events), page.Events)
	}

	after, err := w.led.Totals(w.ctx, ledgerScope("research", p.ID), w.now)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if after != before {
		t.Errorf("the ledger moved during a read: %+v -> %+v", before, after)
	}
}

// --- privacy ---------------------------------------------------------------

// Nothing reachable through the read model can carry a prompt, a response, a trace
// rationale, or a runtime payload. The check is structural, over the display types'
// own field sets, so a field added later that could hold content fails here rather
// than shipping.
func TestNoDisplayTypeCanHoldContent(t *testing.T) {
	banned := []string{
		"prompt", "response", "completion", "message", "messages", "text",
		"body", "payload", "content", "input", "output", "rationale",
		"trace", "chunk", "delta", "tool", "arguments", "result", "system",
	}
	// Fields whose names contain a banned word but are demonstrably not content: a
	// media type and an opaque provider identifier. Sizes and counts are excluded by
	// the type test below rather than by name.
	allowed := map[string]bool{
		"ContentType": true,
		"TraceID":     true,
	}

	// Only text can hold content. A byte count named PayloadBytes is a measurement; a
	// string named Payload would be the leak. Restricting the name test to textual
	// fields is what keeps this check honest rather than merely strict.
	textual := func(rt reflect.Type) bool {
		for rt.Kind() == reflect.Ptr || rt.Kind() == reflect.Slice ||
			rt.Kind() == reflect.Array || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		return rt.Kind() == reflect.String || rt.Kind() == reflect.Uint8
	}

	var check func(t reflect.Type, path string, seen map[reflect.Type]bool)
	check = func(rt reflect.Type, path string, seen map[reflect.Type]bool) {
		for rt.Kind() == reflect.Ptr || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if allowed[f.Name] || !textual(f.Type) {
				check(f.Type, path+"."+f.Name, seen)
				continue
			}
			lower := strings.ToLower(f.Name)
			for _, bad := range banned {
				if lower == bad || strings.HasSuffix(lower, bad) {
					t.Errorf("%s.%s (%s) is a text field named like request or response "+
						"content; the dashboard must expose no payload", path, f.Name, f.Type)
				}
			}
			check(f.Type, path+"."+f.Name, seen)
		}
	}

	for _, rt := range []reflect.Type{
		reflect.TypeOf(Event{}),
		reflect.TypeOf(Detail{}),
		reflect.TypeOf(AgentDetail{}),
		reflect.TypeOf(AgentStep{}),
		reflect.TypeOf(RuntimeDetail{}),
		reflect.TypeOf(Repair{}),
		reflect.TypeOf(Summary{}),
		reflect.TypeOf(Position{}),
		reflect.TypeOf(Breakdown{}),
		reflect.TypeOf(Timeline{}),
		reflect.TypeOf(Hold{}),
	} {
		check(rt, rt.Name(), map[reflect.Type]bool{})
	}
}

// The strings a fully populated record can reach through the read model are
// identifiers, states, and reasons. None of them is text a caller sent or received --
// there is nowhere for such text to have come from, and this asserts that end to end.
func TestNoContentReachesTheReadModelFromARecord(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("agents", "", dollars(1000)))

	// A record with every content-carrying opportunity filled with a marker. If any
	// of it were storable, it would appear downstream.
	rec := agentRecord("r1", "agents", p.ID, w.now)
	rec.Error = "provider returned ThrottlingException"
	rec.Metadata = map[string]string{"team": "physics"}
	w.record(rec)

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}

	// The activity record itself has no field that could have carried content, which
	// is why the read model cannot leak any: assert that upstream property directly.
	recType := reflect.TypeOf(activity.Record{})
	for _, name := range []string{"Prompt", "Response", "Messages", "Body", "Content", "Completion"} {
		if _, ok := recType.FieldByName(name); ok {
			t.Errorf("activity.Record has a %q field; the dashboard's privacy property "+
				"rests on it having none", name)
		}
	}
	stepType := reflect.TypeOf(activity.AgentStep{})
	for _, name := range []string{"Prompt", "Response", "Rationale", "Trace", "Output"} {
		if _, ok := stepType.FieldByName(name); ok {
			t.Errorf("activity.AgentStep has a %q field; step detail must be measurements only", name)
		}
	}

	// An error message is diagnostics, not content, and is the one free-text field.
	if d.Event.Error == "" {
		t.Error("the provider error was dropped; it is diagnostics the operator needs")
	}
	if d.Event.Metadata["team"] != "physics" {
		t.Error("caller attribution metadata was dropped")
	}
}

// A request that does not exist is absent, not a server error.
func TestDetailOfAMissingRequestIsNotFound(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	_, err := w.rep.Detail(w.ctx, "nope")
	if err == nil {
		t.Fatal("Detail of an unknown request returned no error")
	}
	if !errors.Is(err, activity.ErrNotFound) {
		t.Errorf("err = %v, want activity.ErrNotFound so a handler can answer 404", err)
	}
	if !NotFound(err) {
		t.Error("NotFound() = false for a missing record")
	}
}

// A plain settled request has no agent or runtime detail, and the detail view must not
// invent an empty section for it.
func TestDetailOfAPlainRequestHasNoCompoundSections(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.record(settledRecord("r1", "research", p.ID, cents(412), w.now))

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if d.Agent != nil {
		t.Error("Agent detail is non-nil for a plain model call")
	}
	if d.Runtime != nil {
		t.Error("Runtime detail is non-nil for a plain model call")
	}
	if len(d.Repairs) != 0 {
		t.Error("Repairs is non-empty for a record nothing repaired")
	}
	if d.QuoteSource != "aws-price-list" || d.QuoteVersion != "2026-08-01" {
		t.Errorf("quote provenance = %q/%q, want the captured catalog version",
			d.QuoteSource, d.QuoteVersion)
	}
}

func TestUnpricedNamesRendersDimensions(t *testing.T) {
	c := usage.PartialCost(cents(1),
		[]usage.Dimension{usage.CacheWriteTokens, usage.ReasoningTokens}, "partial")
	got := unpricedNames(c)
	want := []string{string(usage.CacheWriteTokens), string(usage.ReasoningTokens)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unpricedNames = %v, want %v", got, want)
	}
	if unpricedNames(usage.KnownCost(cents(1))) != nil {
		t.Error("unpricedNames of a complete cost is not nil")
	}
}
