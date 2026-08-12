package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/scttfrdmn/throttle/activity"
	activitysqlite "github.com/scttfrdmn/throttle/activity/sqlite"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/pricing/fixtures"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/usage"
)

// Models from the pricing fixtures, so the arithmetic in these tests is checkable
// against published rates rather than against invented ones.
const (
	gpt51  = "gpt-5.1"     // $1.25/M input, $0.125/M cached, $10.00/M output
	mini   = "gpt-5-mini"  // $0.25/M input, $0.025/M cached, $2.00/M output
	fourOM = "gpt-4o-mini" // $0.15/M input, $0.075/M cached, $0.60/M output
)

var now = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

func dollars(t *testing.T, s string) money.Money {
	t.Helper()
	m, err := money.Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return m
}

// fakeResponses stands in for the OpenAI Responses service. The whole governed path
// runs against it, which is what lets `go test ./...` work with no OpenAI
// credentials, no network, and no mocking framework.
type fakeResponses struct {
	mu sync.Mutex

	// out and err are what the next call returns. Both may be set: a provider can
	// report usage alongside an error, and that usage is billable.
	out *responses.Response
	err error

	// block, if non-nil, is waited on before returning, to simulate a slow provider.
	block chan struct{}

	calls  int
	params []responses.ResponseNewParams
}

func (f *fakeResponses) New(ctx context.Context, body responses.ResponseNewParams, _ ...option.RequestOption) (*responses.Response, error) {
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

func (f *fakeResponses) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeResponses) lastParams() responses.ResponseNewParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.params) == 0 {
		return responses.ResponseNewParams{}
	}
	return f.params[len(f.params)-1]
}

// fakeCounter stands in for the input-token count endpoint. Guarded, because
// admission runs on the caller's goroutine and the concurrency test admits from many
// at once.
type fakeCounter struct {
	mu     sync.Mutex
	tokens int64
	err    error
	calls  int
	params []responses.InputTokenCountParams
}

func (f *fakeCounter) Count(_ context.Context, body responses.InputTokenCountParams, _ ...option.RequestOption) (*responses.InputTokenCountResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.params = append(f.params, body)
	if f.err != nil {
		return nil, f.err
	}
	return &responses.InputTokenCountResponse{InputTokens: f.tokens}, nil
}

// respond builds a Response from JSON rather than by assigning struct fields.
//
// This is not incidental. The SDK reports field presence through unexported metadata
// that only its unmarshaller populates, and presence is exactly what the adapter reads
// to distinguish "the provider reported zero cached tokens" from "the provider did not
// mention caching". A struct literal would set the values and leave every presence bit
// false, so the tests would exercise a state the wire can never produce.
func respond(t *testing.T, body string) *responses.Response {
	t.Helper()
	var r responses.Response
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshalling a response fixture: %v", err)
	}
	return &r
}

// completedResponse is the ordinary case: a finished response with plain input and
// output token counts and no breakdown.
func completedResponse(t *testing.T, model string, in, out int64) *responses.Response {
	t.Helper()
	return respond(t, fmt.Sprintf(`{
		"id": "resp_test", "object": "response", "status": "completed", "model": %q,
		"usage": {"input_tokens": %d, "output_tokens": %d, "total_tokens": %d}
	}`, model, in, out, in+out))
}

// request builds a Responses request the way a caller would, using the SDK's own
// types.
func request(model string, maxOut *int64) responses.ResponseNewParams {
	in := responses.ResponseNewParams{
		Model: shared.ResponsesModel(model),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: param.NewOpt("what is the airspeed velocity of an unladen swallow?"),
		},
	}
	if maxOut != nil {
		in.MaxOutputTokens = param.NewOpt(*maxOut)
	}
	return in
}

func maxOut(n int64) *int64 { return &n }

type harness struct {
	client   *openai.Client
	api      *fakeResponses
	counter  *fakeCounter
	engine   *engine.Engine
	ledger   ledger.Ledger
	activity activity.Store
	clock    func() time.Time
}

func newHarness(t *testing.T, allocation string, opts ...func(*openai.Config)) *harness {
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
		AnchorAt:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
		t.Fatalf("Register: %v", err)
	}

	return buildHarness(t, eng, store, clock, opts...)
}

// buildHarness wires an adapter around an already-configured engine, so a test that
// needs its own budget hierarchy, clock, or catalog can supply one without duplicating
// the adapter's configuration.
func buildHarness(t *testing.T, eng *engine.Engine, store ledger.Ledger, clock func() time.Time, opts ...func(*openai.Config)) *harness {
	t.Helper()

	cat, err := fixtures.Catalog()
	if err != nil {
		t.Fatalf("fixtures.Catalog: %v", err)
	}

	acts, err := activitysqlite.Open(context.Background(), t.TempDir()+"/activity.db")
	if err != nil {
		t.Fatalf("activity sqlite.Open: %v", err)
	}
	t.Cleanup(func() { acts.Close() })

	api := &fakeResponses{out: completedResponse(t, gpt51, 1000, 500)}
	counter := &fakeCounter{tokens: 1000}

	cfg := openai.Config{
		Client:   api,
		Counter:  counter,
		Engine:   eng,
		Catalog:  cat,
		Activity: acts,
	}
	for _, o := range opts {
		o(&cfg)
	}

	c, err := openai.New(cfg)
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	return &harness{client: c, api: api, counter: counter, engine: eng, ledger: store, activity: acts, clock: clock}
}

func (h *harness) totals(t *testing.T) ledger.Totals {
	t.Helper()
	return h.totalsFor(t, "team")
}

func (h *harness) totalsFor(t *testing.T, budgetID string) ledger.Totals {
	t.Helper()
	p, err := h.ledger.EnsurePeriod(context.Background(), budgetID, h.clock())
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	tot, err := h.ledger.Totals(context.Background(), ledger.Scope{BudgetID: budgetID, PeriodID: p.ID}, h.clock())
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	return tot
}

func (h *harness) record(t *testing.T, requestID string) activity.Record {
	t.Helper()
	rec, err := h.activity.Get(context.Background(), requestID)
	if err != nil {
		t.Fatalf("activity.Get(%q): %v", requestID, err)
	}
	return rec
}

// The happy path, end to end: a request is estimated, reserved, executed, and
// reconciled, and the recorded cost is the priced actual rather than the estimate.
func TestRespondSettlesActualCost(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID:  "team",
		RequestID: "req-1",
		Params:    request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if !res.Settled {
		t.Fatal("the request should have settled")
	}
	if res.Response == nil {
		t.Fatal("the SDK response must be passed through to the caller")
	}

	// 1000 input at $1.25/M plus 500 output at $10.00/M = $0.00125 + $0.005.
	want := dollars(t, "0.00625")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Error("cost should be known for a fixture-priced model with no tools")
	}

	// The reservation was for the output cap (2000 tokens), so the actual must be
	// lower -- and it is the actual that lands in the ledger.
	if res.Estimate.Cost.Amount <= res.Charge.ActualCost {
		t.Errorf("estimate %s should exceed actual %s for a capped request",
			res.Estimate.Cost.Amount, res.Charge.ActualCost)
	}

	tot := h.totals(t)
	if tot.Spent != want {
		t.Errorf("ledger Spent = %s, want %s", tot.Spent, want)
	}
	if tot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0: the hold must be consumed by settlement", tot.Reserved)
	}
}

// A successful request settles exactly once. Double settlement would double the
// recorded spend, and the ledger totals are the only place that shows it.
func TestRespondSettlesExactlyOnce(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-once", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	p, err := h.ledger.EnsurePeriod(context.Background(), "team", h.clock())
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	charges, err := h.ledger.Charges(context.Background(),
		ledger.Scope{BudgetID: "team", PeriodID: p.ID}, now.Add(-time.Hour), now.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("Charges: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("got %d charges for one request, want exactly 1", len(charges))
	}
	if charges[0].ActualCost != res.Charge.ActualCost {
		t.Errorf("the recorded charge %s differs from the reported one %s",
			charges[0].ActualCost, res.Charge.ActualCost)
	}
	if got := h.totals(t).Spent; got != res.Charge.ActualCost {
		t.Errorf("Spent = %s, want %s: settlement must not be applied twice", got, res.Charge.ActualCost)
	}
}

// Cached input tokens are a subset of input_tokens, not an addition to them. Pricing
// the reported total *and* the cached count would charge the cached tokens twice.
//
// The figures are chosen so the two readings differ by more than rounding: 400 of
// 1000 input tokens are cached, and the cached rate is a tenth of the input rate.
func TestCachedInputIsNotDoubleCharged(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_c", "object": "response", "status": "completed", "model": %q,
		"usage": {
			"input_tokens": 1000,
			"input_tokens_details": {"cached_tokens": 400},
			"output_tokens": 500,
			"total_tokens": 1500
		}
	}`, gpt51))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-cache", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	// The dimensions must be disjoint: 600 fresh input, 400 cached.
	if got, _ := res.Usage.Get(usage.InputTokens); got != 600 {
		t.Errorf("InputTokens = %d, want 600 (1000 reported minus 400 cached)", got)
	}
	if got, _ := res.Usage.Get(usage.CacheReadTokens); got != 400 {
		t.Errorf("CacheReadTokens = %d, want 400", got)
	}

	// 600 fresh at $1.25/M = $0.00075; 400 cached at $0.125/M = $0.00005; 500 output
	// at $10/M = $0.005.
	want := dollars(t, "0.0058")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}

	// The wrong reading -- pricing all 1000 reported input tokens *and* the 400 cached
	// ones -- would come to $0.0063. Naming it here means a regression fails with a
	// diagnosis rather than just a number.
	if doubled := dollars(t, "0.0063"); res.Charge.ActualCost == doubled {
		t.Errorf("ActualCost = %s: the cached tokens were charged twice, once inside "+
			"input_tokens and once at the cached rate", doubled)
	}
}

// Reasoning tokens are included in output_tokens and billed at the output rate, so
// the breakdown must be preserved without charging them twice.
func TestReasoningIsNotDoubleCharged(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_r", "object": "response", "status": "completed", "model": %q,
		"usage": {
			"input_tokens": 1000,
			"output_tokens": 500,
			"output_tokens_details": {"reasoning_tokens": 300},
			"total_tokens": 1500
		}
	}`, gpt51))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-reason", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	if got, _ := res.Usage.Get(usage.OutputTokens); got != 200 {
		t.Errorf("OutputTokens = %d, want 200 (500 reported minus 300 reasoning)", got)
	}
	if got, _ := res.Usage.Get(usage.ReasoningTokens); got != 300 {
		t.Errorf("ReasoningTokens = %d, want 300: the breakdown must be preserved", got)
	}

	// Reasoning is priced at the output rate, so the total is identical to a request
	// with 500 plain output tokens: $0.00125 input + $0.005 output.
	want := dollars(t, "0.00625")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: reasoning is billed at the output rate, "+
			"so splitting it out must not change the total", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Error("a reasoning request must price completely: the catalog carries a reasoning rate")
	}
}

// Both breakdowns at once, with every dimension populated. This is the case where a
// naive additive mapping is most wrong.
func TestFullBreakdownPricesDisjointDimensions(t *testing.T) {
	h := newHarness(t, "1000")
	// gpt-5.6-luna is the fixture model that publishes a cache-write rate.
	const luna = "gpt-5.6-luna"
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_f", "object": "response", "status": "completed", "model": %q,
		"usage": {
			"input_tokens": 1000,
			"input_tokens_details": {"cached_tokens": 400, "cache_write_tokens": 100},
			"output_tokens": 500,
			"output_tokens_details": {"reasoning_tokens": 300},
			"total_tokens": 1500
		}
	}`, luna))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-full", Params: request(luna, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	// This is OpenAI's own worked example: 1000 = 400 cached + 100 written + 500 fresh.
	for _, c := range []struct {
		dim  usage.Dimension
		want int64
	}{
		{usage.InputTokens, 500},
		{usage.CacheReadTokens, 400},
		{usage.CacheWriteTokens, 100},
		{usage.OutputTokens, 200},
		{usage.ReasoningTokens, 300},
	} {
		if got, ok := res.Usage.Get(c.dim); !ok || got != c.want {
			t.Errorf("%s = %d (present %v), want %d", c.dim, got, ok, c.want)
		}
	}

	// The five dimensions must sum back to what the provider reported. If they do not,
	// tokens were either invented or lost.
	var in, out int64
	for _, d := range []usage.Dimension{usage.InputTokens, usage.CacheReadTokens, usage.CacheWriteTokens} {
		n, _ := res.Usage.Get(d)
		in += n
	}
	for _, d := range []usage.Dimension{usage.OutputTokens, usage.ReasoningTokens} {
		n, _ := res.Usage.Get(d)
		out += n
	}
	if in != 1000 || out != 500 {
		t.Errorf("the decomposed dimensions sum to %d input and %d output, want 1000 and 500", in, out)
	}

	// 500 fresh at $0.20/M = $0.0001; 400 cached at $0.02/M = $0.000008; 100 written
	// at $0.25/M = $0.000025; 500 output-and-reasoning at $1.20/M = $0.0006.
	want := dollars(t, "0.000733")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
}

// A dimension the provider did not mention is absent, not zero. Recording a zero
// cache read would assert the provider priced one at nothing.
func TestAbsentBreakdownIsNotZero(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-absent", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	for _, d := range []usage.Dimension{usage.CacheReadTokens, usage.CacheWriteTokens, usage.ReasoningTokens} {
		if _, ok := res.Usage.Get(d); ok {
			t.Errorf("%s is present, but the response never mentioned it", d)
		}
	}

	// A response that sends the details object but omits the field is the same case:
	// nothing was said about caching.
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_e", "object": "response", "status": "completed", "model": %q,
		"usage": {"input_tokens": 1000, "input_tokens_details": {}, "output_tokens": 500, "total_tokens": 1500}
	}`, gpt51))
	res, err = h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-absent-2", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if _, ok := res.Usage.Get(usage.CacheReadTokens); ok {
		t.Error("an empty input_tokens_details object mentions no cached tokens, so the dimension must be absent")
	}
	if got, _ := res.Usage.Get(usage.InputTokens); got != 1000 {
		t.Errorf("InputTokens = %d, want 1000: with no cached tokens reported, nothing is subtracted", got)
	}
}

// total_tokens is never the billing primitive. It sums dimensions carrying four
// different prices, so costing it would be wrong in the ordinary case.
func TestTotalTokensIsNotUsedForBilling(t *testing.T) {
	h := newHarness(t, "1000")
	// A deliberately wrong total. If it reached the accounting path, the cost would
	// change; the dimensions are what matter.
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_t", "object": "response", "status": "completed", "model": %q,
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 999999}
	}`, gpt51))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-total", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: a nonsensical total_tokens must not affect the price",
			res.Charge.ActualCost, want)
	}
	// The dimension does not exist in throttle's vocabulary, so it cannot be priced
	// even by accident.
	for _, d := range res.Usage.Dimensions() {
		if strings.Contains(string(d), "total") {
			t.Errorf("usage carries a %q dimension: totals are display-only, never billable", d)
		}
	}
}

// Usage whose own arithmetic is impossible is not silently clamped. Clamping would
// discard real tokens; trusting the subtraction would record a negative count.
func TestInconsistentUsageIsReportedNotClamped(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_i", "object": "response", "status": "completed", "model": %q,
		"usage": {
			"input_tokens": 100,
			"input_tokens_details": {"cached_tokens": 400},
			"output_tokens": 500,
			"total_tokens": 600
		}
	}`, gpt51))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-bad", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrAccounting) {
		t.Fatalf("Respond error = %v, want ErrAccounting", err)
	}
	if res.Settled {
		t.Error("nothing should settle when the provider's own figures contradict each other")
	}
	if res.Cost.Known() {
		t.Error("cost must not be known when usage could not be normalized")
	}

	// The request ran, so the hold stays outstanding rather than being released.
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("the reservation should be left outstanding: the request ran and consumed tokens")
	}
	if rec := h.record(t, "req-bad"); rec.Status != activity.StatusOutstanding {
		t.Errorf("activity status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
}

// The captured quote is the accounting basis. A catalog change between admission and
// settlement must not change what the request costs.
func TestCapturedQuoteSurvivesCatalogChange(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := func() time.Time { return now }
	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := eng.Register(context.Background(), budget.Definition{
		ID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly,
		AnchorAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}, engine.ModeEnforce); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A mutable catalog standing in for a price refresh landing mid-request.
	cat := &mutableCatalog{}
	cat.set(t, "1.25", "10.00")

	h := buildHarness(t, eng, store, clock, func(cfg *openai.Config) { cfg.Catalog = cat })

	// The quote is captured during admission. Raising every rate a hundredfold after
	// that must not reach this request.
	h.api.block = make(chan struct{})
	done := make(chan *openai.Result, 1)
	errs := make(chan error, 1)
	go func() {
		res, err := h.client.Respond(context.Background(), openai.Request{
			BudgetID: "team", RequestID: "req-frozen", Params: request(gpt51, maxOut(2000)),
		})
		done <- res
		errs <- err
	}()

	// Wait for admission to have happened, which is what the call count proves.
	waitFor(t, func() bool { return h.api.callCount() == 1 })
	cat.set(t, "125.00", "1000.00")
	close(h.api.block)

	res, err := <-done, <-errs
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: settlement must replay the rates captured at "+
			"admission, not whatever the catalog says now", res.Charge.ActualCost, want)
	}
}

// A request that names one service tier and is served on another settles on the
// frozen rates for the tier that actually ran -- not on a live catalog lookup.
//
// OpenAI documents this as normal: "the response will show service_tier=priority
// regardless of if you specify service_tier=fast or priority in your request".
func TestActualServiceTierUsesFrozenRates(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(mini, maxOut(2000))
	in.ServiceTier = responses.ResponseNewParamsServiceTierFast
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_tier", "object": "response", "status": "completed", "model": %q,
		"service_tier": "priority",
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
	}`, mini))

	res, err := h.client.Respond(context.Background(), openai.Request{
		BudgetID: "team", RequestID: "req-tier", Params: in,
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	// The identity records the tier that served the request, since that is what was
	// billed.
	if res.Identity.ServiceTier != "priority" {
		t.Errorf("ServiceTier = %q, want %q: the response reports the tier actually used",
			res.Identity.ServiceTier, "priority")
	}

	// gpt-5-mini priority: $0.45/M input, $3.60/M output. Transcribed from the pricing
	// page, and deliberately not a round multiple of the standard rate -- 1.8x, where
	// gpt-5.1's priority input is 2x. Rates cannot be derived, only read.
	want := dollars(t, "0.00225")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (priority rates)", res.Charge.ActualCost, want)
	}
	// The standard-rate reading, named so a regression is diagnosable.
	if std := dollars(t, "0.00125"); res.Charge.ActualCost == std {
		t.Errorf("ActualCost = %s: the request was served on priority but priced at the standard rate", std)
	}
	if !res.Quote.Valid() {
		t.Error("the captured quote should be returned so the caller can see the basis used")
	}
}

// The quote captured at admission carries the alternate tiers, so a response that
// reports a tier the request did not name still prices from frozen knowledge.
func TestAlternateTierIsFrozenAtAdmission(t *testing.T) {
	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := func() time.Time { return now }
	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := eng.Register(context.Background(), budget.Definition{
		ID: "team", Allocation: dollars(t, "1000"), Recurrence: budget.RecurMonthly,
		AnchorAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}, engine.ModeEnforce); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cat := &mutableCatalog{}
	cat.set(t, "1.25", "10.00")
	h := buildHarness(t, eng, store, clock, func(cfg *openai.Config) { cfg.Catalog = cat })

	// The request names no tier; the response comes back on the premium one.
	h.api.block = make(chan struct{})
	h.api.out = respond(t, fmt.Sprintf(`{
		"id": "resp_alt", "object": "response", "status": "completed", "model": %q,
		"service_tier": "priority",
		"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
	}`, gpt51))

	done := make(chan *openai.Result, 1)
	errs := make(chan error, 1)
	go func() {
		res, err := h.client.Respond(context.Background(), openai.Request{
			BudgetID: "team", RequestID: "req-alt", Params: request(gpt51, maxOut(2000)),
		})
		done <- res
		errs <- err
	}()

	waitFor(t, func() bool { return h.api.callCount() == 1 })
	// The catalog is mutated dramatically while the call is in flight, including the
	// priority rows the response is about to select.
	cat.set(t, "125.00", "1000.00")
	close(h.api.block)

	res, err := <-done, <-errs
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	// The frozen priority rates: $2.50/M input, $20.00/M output.
	want := dollars(t, "0.0125")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: the alternate tier must price from the quote "+
			"captured at admission, not from the mutated catalog", res.Charge.ActualCost, want)
	}
}

// mutableCatalog is a pricing catalog whose rates can be changed mid-test, standing
// in for a price refresh landing between admission and settlement.
//
// It delegates to a real *pricing.Static rather than faking the pricing logic, so the
// quote capture under test is the production one.
type mutableCatalog struct {
	mu     sync.RWMutex
	static *pricing.Static
}

func (m *mutableCatalog) set(t *testing.T, input, output string) {
	t.Helper()
	rate := func(d usage.Dimension, s string) pricing.Rate {
		return pricing.PerMillion(d, dollars(t, s))
	}
	price := func(tier, in, out string) pricing.Price {
		return pricing.Price{
			AccessProvider:  "openai",
			ProviderModelID: gpt51,
			ServiceTier:     tier,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:     rate(usage.InputTokens, in),
				usage.OutputTokens:    rate(usage.OutputTokens, out),
				usage.ReasoningTokens: rate(usage.ReasoningTokens, out),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "test-1", Currency: "USD"},
		}
	}
	// Priority is twice standard for this model, per the pricing page.
	double := func(s string) string {
		d := dollars(t, s)
		return (d * 2).String()
	}
	s, err := pricing.NewStatic(
		price("", input, output),
		price("default", input, output),
		price("priority", double(input), double(output)),
	)
	if err != nil {
		t.Fatalf("pricing.NewStatic: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.static = s
}

func (m *mutableCatalog) current() *pricing.Static {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.static
}

func (m *mutableCatalog) Quote(ctx context.Context, id usage.ModelIdentity, u usage.Usage, at time.Time) (pricing.Quote, error) {
	return m.current().Quote(ctx, id, u, at)
}

func (m *mutableCatalog) Capture(id usage.ModelIdentity, at time.Time) (pricing.CapturedQuote, error) {
	return m.current().Capture(id, at)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a condition")
		}
		time.Sleep(time.Millisecond)
	}
}
