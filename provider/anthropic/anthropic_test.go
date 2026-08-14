package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/scttfrdmn/throttle/activity"
	activitysqlite "github.com/scttfrdmn/throttle/activity/sqlite"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing/fixtures"
	anthropic "github.com/scttfrdmn/throttle/provider/anthropic"
)

// Models from the pricing fixtures, so the arithmetic in these tests is checkable
// against published rates rather than against invented ones. Per million tokens:
//
//	                  input   5m write   1h write   cache read   output
//	claude-opus-5     $5.00    $6.25      $10.00     $0.50       $25.00
//	claude-sonnet-5   $2.00    $2.50       $4.00     $0.20       $10.00
//	claude-haiku-4-5  $1.00    $1.25       $2.00     $0.10        $5.00
const (
	opus5   = "claude-opus-5"
	sonnet5 = "claude-sonnet-5"
	haiku45 = "claude-haiku-4-5"
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

// fakeMessages stands in for the Anthropic Messages service. The whole governed path
// runs against it, which is what lets `go test ./...` work with no Anthropic
// credentials, no network, and no mocking framework.
type fakeMessages struct {
	mu sync.Mutex

	// out and err are what the next call returns. Both may be set: a provider can
	// report usage alongside an error, and that usage is billable.
	out *anth.Message
	err error

	// block, if non-nil, is waited on before returning, to simulate a slow provider.
	block chan struct{}

	calls  int
	params []anth.MessageNewParams
}

func (f *fakeMessages) New(ctx context.Context, body anth.MessageNewParams, _ ...option.RequestOption) (*anth.Message, error) {
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

func (f *fakeMessages) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeMessages) lastParams() anth.MessageNewParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.params) == 0 {
		return anth.MessageNewParams{}
	}
	return f.params[len(f.params)-1]
}

func (f *fakeMessages) setResponse(out *anth.Message, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out, f.err = out, err
}

// fakeCounter stands in for POST /v1/messages/count_tokens. Guarded, because admission
// runs on the caller's goroutine and the concurrency test admits from many at once.
type fakeCounter struct {
	mu     sync.Mutex
	tokens int64
	err    error
	calls  int
	params []anth.MessageCountTokensParams
}

func (f *fakeCounter) CountTokens(_ context.Context, body anth.MessageCountTokensParams, _ ...option.RequestOption) (*anth.MessageTokensCount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.params = append(f.params, body)
	if f.err != nil {
		return nil, f.err
	}
	return &anth.MessageTokensCount{InputTokens: f.tokens}, nil
}

func (f *fakeCounter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCounter) lastParams() anth.MessageCountTokensParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.params) == 0 {
		return anth.MessageCountTokensParams{}
	}
	return f.params[len(f.params)-1]
}

// reply builds a *anth.Message from JSON rather than by assigning struct fields.
//
// This is not incidental. The SDK reports field presence through unexported metadata
// that only its unmarshaller populates, and presence is exactly what the adapter reads
// to distinguish "the provider reported zero cache reads" from "the provider did not
// mention caching". A struct literal would set the values and leave every presence bit
// false, so the tests would exercise a state the wire can never produce.
//
// It is also the only way to test the cases that matter most here: an unknown usage
// counter and an unfamiliar stop reason cannot be expressed as struct fields at all,
// because this build has no fields for them.
func reply(t *testing.T, body string) *anth.Message {
	t.Helper()
	var m anth.Message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("unmarshalling a message fixture: %v", err)
	}
	return &m
}

// message is the ordinary case: a finished message with plain input and output token
// counts and no cache activity.
func message(t *testing.T, model string, in, out int64) *anth.Message {
	t.Helper()
	return reply(t, fmt.Sprintf(`{
		"id": "msg_test", "type": "message", "role": "assistant", "model": %q,
		"content": [{"type": "text", "text": "an answer"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": %d, "output_tokens": %d}
	}`, model, in, out))
}

// usageReply is a finished message whose usage object is supplied verbatim, for the
// tests that are about usage decomposition rather than about the lifecycle.
func usageReply(t *testing.T, model, usageJSON string) *anth.Message {
	t.Helper()
	return reply(t, fmt.Sprintf(`{
		"id": "msg_test", "type": "message", "role": "assistant", "model": %q,
		"content": [{"type": "text", "text": "an answer"}],
		"stop_reason": "end_turn",
		"usage": %s
	}`, model, usageJSON))
}

// request builds a Messages request the way a caller would, using the SDK's own types.
//
// max_tokens is required by the API, so unlike the other adapters' helpers there is no
// "unset" variant to offer: every request states its own ceiling.
func request(model string, maxTokens int64) anth.MessageNewParams {
	return anth.MessageNewParams{
		Model:     anth.Model(model),
		MaxTokens: maxTokens,
		Messages: []anth.MessageParam{{
			Role: anth.MessageParamRoleUser,
			Content: []anth.ContentBlockParamUnion{{
				OfText: &anth.TextBlockParam{Text: "what is the airspeed velocity of an unladen swallow?"},
			}},
		}},
	}
}

type harness struct {
	client   *anthropic.Client
	api      *fakeMessages
	counter  *fakeCounter
	engine   *engine.Engine
	ledger   ledger.Ledger
	activity activity.Store
	clock    func() time.Time
}

func newHarness(t *testing.T, allocation string, opts ...func(*anthropic.Config)) *harness {
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
func buildHarness(t *testing.T, eng *engine.Engine, store ledger.Ledger, clock func() time.Time, opts ...func(*anthropic.Config)) *harness {
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

	api := &fakeMessages{out: message(t, opus5, 1000, 500)}
	counter := &fakeCounter{tokens: 1000}

	cfg := anthropic.Config{
		Client:   api,
		Counter:  counter,
		Engine:   eng,
		Catalog:  cat,
		Activity: acts,
	}
	for _, o := range opts {
		o(&cfg)
	}

	c, err := anthropic.New(cfg)
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}
	return &harness{client: c, api: api, counter: counter, engine: eng, ledger: store, activity: acts, clock: clock}
}

// withMessages replaces the fake Messages service, for a test that needs its own.
func withMessages(api anthropic.MessagesAPI) func(*anthropic.Config) {
	return func(c *anthropic.Config) { c.Client = api }
}

// withoutCounter disables preflight counting, so the estimate falls back to the
// heuristic. Also the shape a caller who declined the extra round trip gets.
func withoutCounter() func(*anthropic.Config) {
	return func(c *anthropic.Config) { c.Counter = nil }
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

// record returns the durable activity record for a request, which is where the tests
// that care about what throttle *remembers* -- as opposed to what it returned -- look.
func (h *harness) record(t *testing.T, requestID string) activity.Record {
	t.Helper()
	rec, err := h.activity.Get(context.Background(), requestID)
	if err != nil {
		t.Fatalf("activity.Get(%q): %v", requestID, err)
	}
	return rec
}

// waitFor blocks until cond holds, so a test that has to observe a concurrent call in
// flight does not race on a sleep.
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

// The happy path, end to end: a request is estimated, reserved, executed, and
// reconciled, and the recorded cost is the priced actual rather than the estimate.
func TestNewMessageSettlesActualCost(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID:  "team",
		RequestID: "req-1",
		Params:    request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if !res.Settled {
		t.Fatal("the request should have settled")
	}
	if res.Message == nil {
		t.Fatal("the SDK message must be passed through to the caller")
	}

	// 1000 input at $5.00/M plus 500 output at $25.00/M = $0.005 + $0.0125.
	want := dollars(t, "0.0175")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Errorf("cost should be known for a fixture-priced model with no tools: %s", res.Cost.Reason)
	}

	// The reservation was for the caller's own max_tokens (2000), so the actual must be
	// lower -- and it is the actual that lands in the ledger.
	if res.Estimate.Cost.Amount <= res.Charge.ActualCost {
		t.Errorf("estimate %s should exceed actual %s for a request that generated less than its cap",
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
func TestNewMessageSettlesExactlyOnce(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-once", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
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
	if got := h.totals(t).Spent; got != res.Charge.ActualCost {
		t.Errorf("Spent = %s, want %s: settlement must not be applied twice", got, res.Charge.ActualCost)
	}
}
