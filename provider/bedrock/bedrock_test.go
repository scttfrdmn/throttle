package bedrock_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"throttle/budget"
	"throttle/engine"
	"throttle/ledger"
	"throttle/ledger/sqlite"
	"throttle/money"
	"throttle/pricing"
	"throttle/pricing/fixtures"
	"throttle/provider/bedrock"
	"throttle/usage"
)

const sonnetID = "anthropic.claude-sonnet-4-5-20250929-v1:0"

var now = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

func dollars(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return m
}

// fakeConverse stands in for the Bedrock Runtime client. The whole governed path
// runs against it, which is what lets `go test ./...` work with no AWS credentials,
// no network, and no mocking framework.
type fakeConverse struct {
	mu sync.Mutex

	// out and err are what the next call returns. Both may be set: Bedrock can
	// report usage alongside an error, and that usage is billable.
	out *bedrockruntime.ConverseOutput
	err error

	// block, if non-nil, is waited on before returning, to simulate a slow provider.
	block chan struct{}

	calls  int
	inputs []*bedrockruntime.ConverseInput
}

func (f *fakeConverse) Converse(ctx context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	f.mu.Lock()
	f.calls++
	f.inputs = append(f.inputs, in)
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

func (f *fakeConverse) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeConverse) lastInput() *bedrockruntime.ConverseInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil
	}
	return f.inputs[len(f.inputs)-1]
}

// fakeCounter stands in for the CountTokens API. Guarded, because admission runs
// on the caller's goroutine and the concurrency tests admit from many at once.
type fakeCounter struct {
	mu     sync.Mutex
	tokens int32
	err    error
	calls  int
	inputs []*bedrockruntime.CountTokensInput
}

func (f *fakeCounter) CountTokens(_ context.Context, in *bedrockruntime.CountTokensInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.CountTokensOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	return &bedrockruntime.CountTokensOutput{InputTokens: aws.Int32(f.tokens)}, nil
}

// response builds a Converse output with the given token counts.
func response(in, out int32) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		Usage: &brtypes.TokenUsage{
			InputTokens:  aws.Int32(in),
			OutputTokens: aws.Int32(out),
			TotalTokens:  aws.Int32(in + out),
		},
		Metrics:    &brtypes.ConverseMetrics{LatencyMs: aws.Int64(1234)},
		StopReason: brtypes.StopReasonEndTurn,
		Output: &brtypes.ConverseOutputMemberMessage{
			Value: brtypes.Message{
				Role:    brtypes.ConversationRoleAssistant,
				Content: []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: "hello"}},
			},
		},
	}
}

func request(modelID string, maxTokens *int32) *bedrockruntime.ConverseInput {
	in := &bedrockruntime.ConverseInput{
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

type harness struct {
	client  *bedrock.Client
	api     *fakeConverse
	counter *fakeCounter
	engine  *engine.Engine
	ledger  ledger.Ledger
	clock   func() time.Time
}

func newHarness(t *testing.T, allocation string, opts ...func(*bedrock.Config)) *harness {
	t.Helper()

	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	current := now
	clock := func() time.Time { return current }

	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	def := budget.Definition{
		ID:         "team",
		Allocation: dollars(t, allocation),
		Recurrence: budget.RecurMonthly,
		AnchorAt:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
		t.Fatalf("Register: %v", err)
	}

	return buildHarness(t, eng, store, clock, opts...)
}

// buildHarness wires an adapter around an already-configured engine, so a test
// that needs its own budget hierarchy, clock, or lease can supply one without
// duplicating the adapter's configuration.
func buildHarness(t *testing.T, eng *engine.Engine, store ledger.Ledger, clock func() time.Time, opts ...func(*bedrock.Config)) *harness {
	t.Helper()

	cat, err := fixtures.Catalog()
	if err != nil {
		t.Fatalf("fixtures.Catalog: %v", err)
	}

	api := &fakeConverse{out: response(1000, 500)}
	counter := &fakeCounter{tokens: 1000}

	cfg := bedrock.Config{
		Client:  api,
		Counter: counter,
		Engine:  eng,
		Catalog: cat,
		Region:  "us-east-1",
	}
	for _, o := range opts {
		o(&cfg)
	}

	c, err := bedrock.New(cfg)
	if err != nil {
		t.Fatalf("bedrock.New: %v", err)
	}
	return &harness{client: c, api: api, counter: counter, engine: eng, ledger: store, clock: clock}
}

func (h *harness) totals(t *testing.T) ledger.Totals {
	t.Helper()
	p, err := h.ledger.EnsurePeriod(context.Background(), "team", h.clock())
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	tot, err := h.ledger.Totals(context.Background(), ledger.Scope{BudgetID: "team", PeriodID: p.ID}, h.clock())
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	return tot
}

// The happy path, end to end: a request is estimated, reserved, executed, and
// reconciled, and the recorded cost is the priced actual rather than the estimate.
func TestConverseSettlesActualCost(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID:  "team",
		RequestID: "req-1",
		Input:     request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if !res.Settled {
		t.Fatal("the request should have settled")
	}
	if res.Output == nil {
		t.Fatal("the SDK response must be passed through to the caller")
	}

	// 1000 input at $3/M plus 500 output at $15/M = $0.003 + $0.0075.
	want := dollars(t, "0.0105")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Error("cost should be known for a fixture-priced model")
	}

	// The reservation was for the output *cap* (2000 tokens), so the actual must be
	// lower -- and it is the actual that lands in the ledger.
	if res.Estimate.Cost.Amount <= res.Charge.ActualCost {
		t.Errorf("estimate %s should exceed actual %s for a capped request",
			res.Estimate.Cost.Amount, res.Charge.ActualCost)
	}

	tot := h.totals(t)
	if tot.Spent != want {
		t.Errorf("Spent = %s, want %s", tot.Spent, want)
	}
	if tot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0: the hold should be gone", tot.Reserved)
	}

	// Usage must be normalized into throttle's dimensions.
	if got := res.Usage.Count(usage.InputTokens); got != 1000 {
		t.Errorf("input tokens = %d, want 1000", got)
	}
	if got := res.Usage.Count(usage.OutputTokens); got != 500 {
		t.Errorf("output tokens = %d, want 500", got)
	}
	// Provider-reported latency, not the caller's wall clock.
	if res.Charge.Usage.ProviderLatency != 1234*time.Millisecond {
		t.Errorf("ProviderLatency = %v, want 1.234s", res.Charge.Usage.ProviderLatency)
	}
	// The enforcement mode that actually governed the request must be recorded.
	if res.Mode != engine.ModeEnforce {
		t.Errorf("Mode = %q, want enforce", res.Mode)
	}
}

// The request must reach Bedrock unchanged. throttle is a shim, not a rewriter:
// no prompt economy, no model substitution, no injected parameters.
func TestConversePassesRequestThroughUnchanged(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(sonnetID, aws.Int32(2000))
	if _, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: in,
	}); err != nil {
		t.Fatalf("Converse: %v", err)
	}

	got := h.api.lastInput()
	if got != in {
		t.Fatal("the adapter must pass the caller's own input to the SDK, not a copy or a rewrite")
	}
	if *got.ModelId != sonnetID {
		t.Errorf("ModelId = %q, want the caller's %q", *got.ModelId, sonnetID)
	}
	if *got.InferenceConfig.MaxTokens != 2000 {
		t.Error("the caller's MaxTokens was modified")
	}
}

// A request the budget cannot afford must not reach the provider at all.
func TestConverseDeniedDoesNotCallProvider(t *testing.T) {
	// A budget so small that even the estimate cannot fit.
	h := newHarness(t, "0.000001")

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, engine.ErrDenied) {
		t.Fatalf("err = %v, want ErrDenied", err)
	}
	if h.api.callCount() != 0 {
		t.Error("a denied request must not reach the provider")
	}
	if res.Settled {
		t.Error("nothing should have settled")
	}
	if h.totals(t).Committed() != 0 {
		t.Error("a denied request must not commit money")
	}
}

// A provider error with no usage means nothing was billed, so the hold must go
// back. Leaving it would starve the budget for the rest of the lease.
func TestConverseProviderErrorReleasesReservation(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = nil
	h.api.err = errors.New("ValidationException: bad request")

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, bedrock.ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	if res.Settled {
		t.Error("a failed call must not settle")
	}

	tot := h.totals(t)
	if tot.Committed() != 0 {
		t.Errorf("Committed = %s, want 0: the hold should have been released", tot.Committed())
	}

	r, err := h.ledger.Get(context.Background(), res.ReservationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.State != ledger.StateReleased {
		t.Errorf("reservation state = %q, want released", r.State)
	}
}

// The case that must not be simplified away: Bedrock can return usage *and* an
// error. That usage was billed, so it is settled rather than released.
func TestConverseErrorWithUsageIsSettled(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = response(1000, 200)
	h.api.err = errors.New("ModelStreamErrorException: interrupted after partial generation")

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	// The caller must still hear about the provider failure.
	if !errors.Is(err, bedrock.ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	if !res.Settled {
		t.Fatal("usage the provider billed for must be recorded even though the call failed")
	}

	// 1000 input + 200 output = $0.003 + $0.003.
	want := dollars(t, "0.006")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if h.totals(t).Spent != want {
		t.Errorf("Spent = %s, want %s", h.totals(t).Spent, want)
	}
}

// A success with no usage metadata leaves the accounting unresolvable. Recording
// zero would be a lie, so the hold stays outstanding and the caller is told.
func TestConverseSuccessWithoutUsageLeavesHoldOutstanding(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = &bedrockruntime.ConverseOutput{StopReason: brtypes.StopReasonEndTurn}
	h.api.err = nil

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, bedrock.ErrAccounting) {
		t.Fatalf("err = %v, want ErrAccounting", err)
	}
	if res.Settled {
		t.Error("nothing could be settled")
	}
	// Outstanding, not released: the request may well have been billed.
	r, err := h.ledger.Get(context.Background(), res.ReservationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.State != ledger.StatePending {
		t.Errorf("reservation state = %q, want pending", r.State)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("the hold should still be consuming headroom")
	}
}

// Cancellation mid-call is genuinely ambiguous: the provider may have served and
// billed the request. Releasing would erase real spend, so the hold stays.
func TestConverseCancellationLeavesOutcomeUnknown(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.block = make(chan struct{}) // never closed; the call hangs

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		res *bedrock.Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := h.client.Converse(ctx, bedrock.Request{
			BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
		})
		done <- result{r, err}
	}()

	// Cancel only once the provider call is genuinely in flight. Cancelling on a
	// timer instead would sometimes land during admission, where the correct answer
	// is a plain context error and nothing is outstanding.
	waitFor(t, func() bool { return h.api.callCount() > 0 })
	cancel()

	got := <-done
	res, err := got.res, got.err
	if !errors.Is(err, bedrock.ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrOutcomeUnknown", err)
	}
	if res.Settled {
		t.Error("an ambiguous outcome must not settle")
	}

	// The error must name the reservation, or an operator cannot reconcile it.
	if !strings.Contains(err.Error(), res.ReservationID) {
		t.Errorf("the error should name the outstanding reservation: %v", err)
	}

	r, getErr := h.ledger.Get(context.Background(), res.ReservationID)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if r.State != ledger.StatePending {
		t.Errorf("reservation state = %q, want pending: the outcome is unknown", r.State)
	}
}

// A caller deadline that expires mid-call is the same ambiguity as cancellation.
func TestConverseTimeoutLeavesOutcomeUnknown(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.block = make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := h.client.Converse(ctx, bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, bedrock.ErrOutcomeUnknown) {
		t.Fatalf("err = %v, want ErrOutcomeUnknown", err)
	}
}

// A provider-side timeout is a plain provider error, not an ambiguity: the SDK
// returned, and it reported no usage.
func TestConverseProviderTimeoutReleases(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = nil
	h.api.err = fmt.Errorf("ModelTimeoutException: the model took too long")

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, bedrock.ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}
	if h.totals(t).Committed() != 0 {
		t.Error("a provider timeout with no usage should release the hold")
	}
	if errors.Is(err, bedrock.ErrOutcomeUnknown) {
		t.Error("a returned provider error is not an unknown outcome")
	}
	_ = res
}

// An unknown model must remain usable: the call goes through, usage is recorded
// normally, and only the cost is unknown.
func TestConverseUnknownModelUsageIsKnownCostIsNot(t *testing.T) {
	h := newHarness(t, "1000")
	const unknownID = "newvendor.brand-new-model-v1:0"
	h.api.out = response(1000, 500)

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(unknownID, aws.Int32(2000)),
	})

	// Provisional behavior: the engine currently refuses to reserve an unknown
	// cost. What matters for this slice is that it is refused explicitly rather
	// than admitted at zero -- the recorded policy is a separate decision.
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("err = %v, want ErrCostUnknown", err)
	}

	// Usage estimation still worked; only pricing did not.
	if res.Estimate.Usage.Count(usage.InputTokens) == 0 {
		t.Error("usage must be estimable for an unpriced model")
	}
	if res.Estimate.Cost.Known() {
		t.Error("the cost of an unpriced model must not be known")
	}
	if res.Estimate.Cost.Reason == "" {
		t.Error("an unknown cost must explain itself")
	}
	// Identity must still be usable and must retain the raw ID verbatim.
	if res.Identity.ProviderModelID != unknownID {
		t.Errorf("ProviderModelID = %q, want %q", res.Identity.ProviderModelID, unknownID)
	}
	if !res.Identity.Valid() {
		t.Error("an unpriced model's identity must still be valid")
	}
	if res.Identity.Known() {
		t.Error("an unrecognized model should not claim canonical identity")
	}
	// Crucially: not admitted at zero cost.
	if h.totals(t).Committed() != 0 {
		t.Error("an unpriced request must not commit a zero-cost charge")
	}
}

// Retrying an ambiguous failure with the same RequestID must not double-reserve.
func TestConverseRetryWithSameRequestIDIsIdempotent(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = &bedrockruntime.ConverseOutput{StopReason: brtypes.StopReasonEndTurn} // no usage: hold stays

	first, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, bedrock.ErrAccounting) {
		t.Fatalf("first call err = %v, want ErrAccounting", err)
	}
	before := h.totals(t).Reserved

	// The retry must be refused as a duplicate rather than holding a second time.
	_, err = h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, ledger.ErrDuplicateReservation) {
		t.Fatalf("retry err = %v, want ErrDuplicateReservation", err)
	}
	if after := h.totals(t).Reserved; after != before {
		t.Errorf("Reserved went from %s to %s: the retry double-reserved", before, after)
	}
	_ = first
}

// Settling twice must not double-count real money.
func TestConverseDoesNotDoubleSettle(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	spent := h.totals(t).Spent

	// Settling the same reservation directly must be refused.
	_, err = h.ledger.Settle(context.Background(), ledger.Settlement{
		ReservationID: res.ReservationID,
		ActualCost:    dollars(t, "5.00"),
		CompletedAt:   now,
	})
	if !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Fatalf("err = %v, want ErrAlreadyResolved", err)
	}
	if got := h.totals(t).Spent; got != spent {
		t.Errorf("Spent changed from %s to %s on a duplicate settlement", spent, got)
	}
}

// Cache dimensions are priced separately from fresh input, so they must be
// normalized as their own dimensions rather than folded into input.
func TestConverseNormalizesCacheDimensions(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = &bedrockruntime.ConverseOutput{
		Usage: &brtypes.TokenUsage{
			InputTokens:           aws.Int32(1_000_000),
			OutputTokens:          aws.Int32(0),
			TotalTokens:           aws.Int32(1_000_000),
			CacheReadInputTokens:  aws.Int32(1_000_000),
			CacheWriteInputTokens: aws.Int32(1_000_000),
		},
		Metrics:    &brtypes.ConverseMetrics{LatencyMs: aws.Int64(10)},
		StopReason: brtypes.StopReasonEndTurn,
	}

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(10)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}

	if got := res.Usage.Count(usage.CacheReadTokens); got != 1_000_000 {
		t.Errorf("cache read = %d, want 1000000", got)
	}
	if got := res.Usage.Count(usage.CacheWriteTokens); got != 1_000_000 {
		t.Errorf("cache write = %d, want 1000000", got)
	}

	// $3.00 input + $0.30 cache read + $3.75 cache write, each at its own rate.
	want := dollars(t, "7.05")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (each dimension at its own rate)", res.Charge.ActualCost, want)
	}
}

// TotalTokens is a provider convenience that sums differently-priced dimensions.
// Costing it would be wrong, so it must be ignored.
func TestConverseIgnoresProviderTotalTokens(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = &bedrockruntime.ConverseOutput{
		Usage: &brtypes.TokenUsage{
			InputTokens:  aws.Int32(1_000_000),
			OutputTokens: aws.Int32(1_000_000),
			// Deliberately nonsense: if anything costed this, the total would be off.
			TotalTokens: aws.Int32(999_999_999),
		},
		Metrics:    &brtypes.ConverseMetrics{LatencyMs: aws.Int64(10)},
		StopReason: brtypes.StopReasonEndTurn,
	}

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(1_000_000)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if want := dollars(t, "18.00"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s ($3 input + $15 output)", res.Charge.ActualCost, want)
	}
}

// The response's tier is authoritative over the request's: Bedrock may serve a
// request on a different tier than the one asked for, at a different price.
func TestConverseUsesResponseTierForPricing(t *testing.T) {
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "15.00")),
			},
			Provenance: pricing.Provenance{Source: "test"},
		},
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			ServiceTier:     "flex",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "1.50")),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "7.50")),
			},
			Provenance: pricing.Provenance{Source: "test-flex"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	h := newHarness(t, "1000", func(c *bedrock.Config) { c.Catalog = cat })
	out := response(1_000_000, 0)
	// The caller asked for nothing in particular; Bedrock says it served flex.
	out.ServiceTier = &brtypes.ServiceTier{Type: brtypes.ServiceTierTypeFlex}
	h.api.out = out

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(10)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Identity.ServiceTier != "flex" {
		t.Errorf("ServiceTier = %q, want flex from the response", res.Identity.ServiceTier)
	}
	if want := dollars(t, "1.50"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want the flex price %s", res.Charge.ActualCost, want)
	}
}

// A monitored budget must record spend without ever blocking, and the recorded
// mode must say so -- otherwise a reader cannot tell that the ceiling did not apply.
func TestConverseMonitorModeRecordsWithoutBlocking(t *testing.T) {
	h := newHarness(t, "0.000001") // far too small to afford anything
	if err := h.engine.SetMode("team", engine.ModeMonitor); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("monitor mode must not block: %v", err)
	}
	if !res.Settled {
		t.Error("monitor mode must still record spend")
	}
	if res.Mode != engine.ModeMonitor {
		t.Errorf("Mode = %q, want monitor: the recorded request must say the ceiling did not apply", res.Mode)
	}
	if h.totals(t).Spent == 0 {
		t.Error("monitored spend must still be recorded")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer store.Close()

	eng, err := engine.New(engine.Config{Ledger: store})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	cat, err := fixtures.Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}

	if _, err := bedrock.New(bedrock.Config{Engine: eng, Catalog: cat}); !errors.Is(err, bedrock.ErrNoClient) {
		t.Errorf("err = %v, want ErrNoClient", err)
	}
	if _, err := bedrock.New(bedrock.Config{Client: &fakeConverse{}, Catalog: cat}); err == nil {
		t.Error("an engine should be required")
	}
	// A catalog is required: without one there is no way to convert usage to money,
	// and guessing is not permitted.
	if _, err := bedrock.New(bedrock.Config{Client: &fakeConverse{}, Engine: eng}); err == nil {
		t.Error("a pricing catalog should be required")
	}
}
