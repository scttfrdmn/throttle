package openai_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/usage"
)

// 1, 3. The ordinary complete stream: the hold is taken before the stream exists,
// events arrive as the SDK produced them, the terminal Response settles from the
// captured quote, and it settles exactly once.
func TestStreamSettlesFromTerminalResponse(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-1", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	defer s.Close()

	// Admission happened before the provider was called: the reservation exists and the
	// pre-call activity record is already durable.
	if s.ReservationID() == "" {
		t.Fatal("a governed stream must hold a reservation before the provider is called")
	}
	if got := h.record(t, "stream-1").Status; got != activity.StatusPending {
		t.Errorf("status before the stream is read = %q, want %q", got, activity.StatusPending)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("the estimate must be reserved before the stream is created")
	}

	if n := drain(t, s); n != 4 {
		t.Errorf("received %d events, want 4: throttle must forward every event the SDK produced", n)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err = %v, want nil for a completed stream", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}

	res := s.Result()
	if res == nil {
		t.Fatal("a terminal stream must report a Result")
	}
	if !res.Settled {
		t.Fatal("a completed stream with usage must settle")
	}

	// 1000 input at $1.25/M plus 500 output at $10.00/M.
	want := dollars(t, "0.00625")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}

	tot := h.totals(t)
	if tot.Spent != want {
		t.Errorf("ledger Spent = %s, want %s", tot.Spent, want)
	}
	if tot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0: settlement must consume the hold", tot.Reserved)
	}

	rec := h.record(t, "stream-1")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusSettled)
	}
	if rec.Outcome != activity.OutcomeSuccess {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeSuccess)
	}
	// The operation is what tells a later reader which API a record belongs to.
	if rec.Identity.Operation != "responses-stream" {
		t.Errorf("Operation = %q, want %q", rec.Identity.Operation, "responses-stream")
	}
}

// 3, 12, 18, 19. Settlement happens exactly once however many times the stream is
// closed or iterated afterwards. The ledger totals are the only place a double
// settlement shows, so they are what this checks.
func TestStreamSettlesExactlyOnce(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-once", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)

	// An EOF after the terminal event, then two closes, then more iteration. None of it
	// may account again.
	if s.Next() {
		t.Error("Next after the terminal state must report false")
	}
	for i := 0; i < 3; i++ {
		if err := s.Close(); err != nil {
			t.Errorf("Close #%d = %v, want nil: Close must be idempotent", i+1, err)
		}
	}

	p, err := h.ledger.EnsurePeriod(context.Background(), "team", h.clock())
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	charges, err := h.ledger.Charges(context.Background(),
		ledger.Scope{BudgetID: "team", PeriodID: p.ID}, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("Charges: %v", err)
	}
	if len(charges) != 1 {
		t.Fatalf("recorded %d charges, want exactly 1", len(charges))
	}
	if got := h.totals(t).Spent; got != dollars(t, "0.00625") {
		t.Errorf("Spent = %s: the stream settled more than once", got)
	}
	// The provider's stream is closed once, not once per Close call.
	if got := h.up.closeCount(); got != 1 {
		t.Errorf("the provider stream was closed %d times, want 1", got)
	}
}

// 3. Accounting is complete before the terminal event is handed to the caller, so a
// caller who sees completion and simply stops iterating cannot leave the request
// unaccounted.
//
// This is the anti-vacuous form: it never calls Close and never reads past the
// terminal event, which is exactly the shape that would strand a hold if accounting
// were deferred to Close or to a trailing EOF.
func TestTerminalAccountingCompletesBeforeTheEventIsDelivered(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-eager", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	for s.Next() {
		ev := s.Current()
		if ev.Type != "response.completed" {
			continue
		}
		// The caller is holding the terminal event and has done nothing else. The money
		// must already be recorded.
		if s.Result() == nil {
			t.Fatal("the terminal event was delivered before accounting completed")
		}
		if got := h.totals(t).Spent; got != dollars(t, "0.00625") {
			t.Errorf("Spent = %s at terminal-event delivery, want 0.00625: accounting must be "+
				"complete before the caller sees completion", got)
		}
		if got := h.totals(t).Reserved; got != 0 {
			t.Errorf("Reserved = %s at terminal-event delivery: the hold was not yet consumed", got)
		}
		return
	}
	t.Fatal("the stream never delivered a terminal event")
}

// 4. Usage from a terminal event is normalized subtractively, the same way a
// non-streaming response is: OpenAI's figures are inclusive and throttle's dimensions
// are disjoint.
//
// The figures are chosen so the two readings differ visibly, which is what makes this
// anti-vacuous: copying input_tokens instead of subtracting would charge the cached
// tokens twice and produce the larger figure named below.
func TestStreamUsageIsNormalizedSubtractively(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, mini),
		event(t, fmt.Sprintf(`{
			"type": "response.completed", "sequence_number": 9,
			"response": {
				"id": "resp_cached", "object": "response", "status": "completed", "model": %q,
				"usage": {
					"input_tokens": 10000, "output_tokens": 2000, "total_tokens": 12000,
					"input_tokens_details": {"cached_tokens": 8000},
					"output_tokens_details": {"reasoning_tokens": 1500}
				}
			}
		}`, mini)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-cached", Params: request(mini, maxOut(4000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if got := tokens(res.Usage, usage.InputTokens); got != 2000 {
		t.Errorf("InputTokens = %d, want 2000 (10000 total less 8000 cached)", got)
	}
	if got := tokens(res.Usage, usage.CacheReadTokens); got != 8000 {
		t.Errorf("CacheReadTokens = %d, want 8000", got)
	}
	if got := tokens(res.Usage, usage.OutputTokens); got != 500 {
		t.Errorf("OutputTokens = %d, want 500 (2000 total less 1500 reasoning)", got)
	}
	if got := tokens(res.Usage, usage.ReasoningTokens); got != 1500 {
		t.Errorf("ReasoningTokens = %d, want 1500", got)
	}

	// gpt-5-mini: 2000 fresh at $0.25/M = $0.0005, 8000 cached at $0.025/M = $0.0002,
	// 500 visible plus 1500 reasoning at $2.00/M = $0.004.
	want := dollars(t, "0.0047")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	// The double-charged reading, named so a regression is diagnosable rather than just
	// a wrong number.
	if double := dollars(t, "0.0067"); res.Charge.ActualCost == double {
		t.Errorf("ActualCost = %s: the cached tokens were charged twice, once inside the "+
			"input total and once at the cached rate", double)
	}
}

// 5. A stream served on a tier the request did not name settles from the rates frozen
// at admission for the tier that actually ran.
func TestStreamActualServiceTierUsesFrozenRates(t *testing.T) {
	in := request(mini, maxOut(2000))
	in.ServiceTier = responses.ResponseNewParamsServiceTierFast
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, mini),
		event(t, fmt.Sprintf(`{
			"type": "response.completed", "sequence_number": 9,
			"response": {
				"id": "resp_tier", "object": "response", "status": "completed", "model": %q,
				"service_tier": "priority",
				"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
			}
		}`, mini)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-tier", Params: in,
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if res.Identity.ServiceTier != "priority" {
		t.Errorf("ServiceTier = %q, want %q: the terminal response reports the tier that ran",
			res.Identity.ServiceTier, "priority")
	}
	// gpt-5-mini priority: $0.45/M input, $3.60/M output.
	want := dollars(t, "0.00225")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (priority rates)", res.Charge.ActualCost, want)
	}
	if std := dollars(t, "0.00125"); res.Charge.ActualCost == std {
		t.Errorf("ActualCost = %s: the stream ran on priority but was priced at the standard rate", std)
	}
}

// 6. A terminal response reporting a tier nobody captured a rate for is unresolved,
// not priced at the requested or standard rate. This is #30's invariant, exercised
// through the streaming path.
func TestStreamUnknownActualTierIsUnresolved(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		event(t, fmt.Sprintf(`{
			"type": "response.completed", "sequence_number": 9,
			"response": {
				"id": "resp_turbo", "object": "response", "status": "completed", "model": %q,
				"service_tier": "turbo-2027",
				"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
			}
		}`, gpt51)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-turbo", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	if err := s.Err(); !errors.Is(err, openai.ErrCostUnresolved) {
		t.Fatalf("Err = %v, want ErrCostUnresolved", err)
	}
	res := s.Result()
	if res.Cost.Known() {
		t.Errorf("cost = %s: an uncaptured tier must not be priced from any fallback", res.Cost.Amount)
	}
	if !res.Unresolved {
		t.Error("the request must be marked unresolved")
	}
	// The standard-rate figure, named so a fallback regression is unmistakable.
	if res.Cost.State() == usage.CostKnown && res.Cost.Amount == dollars(t, "0.00625") {
		t.Error("the request fell back to the standard tier's rates")
	}
	// The usage is still recorded: the tokens are known even though the price is not.
	if got := tokens(res.Usage, usage.InputTokens); got != 1000 {
		t.Errorf("InputTokens = %d, want 1000: usage is known even when pricing is not", got)
	}
	// The hold stays encumbered rather than being released or settled.
	if h.totals(t).Reserved == 0 {
		t.Error("an unresolved stream must leave its hold encumbered")
	}
	if h.totals(t).Spent != 0 {
		t.Error("an unpriceable stream must not record spend")
	}

	rec := h.record(t, "stream-turbo")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusUnresolved)
	}
	if rec.Identity.ServiceTier != "turbo-2027" {
		t.Errorf("recorded ServiceTier = %q, want the tier the provider served", rec.Identity.ServiceTier)
	}
}

// 7. A catalog change between admission and the terminal event cannot change what the
// stream costs. The quote captured at admission is the accounting basis.
func TestStreamSettlesFromTheQuoteCapturedAtAdmission(t *testing.T) {
	cat := &mutableCatalog{}
	cat.set(t, "1.25", "10.00")

	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500),
		func(cfg *openai.Config) { cfg.Catalog = cat })

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-frozen", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	// The stream is established and admission is done. Raising every rate a
	// hundredfold now must not reach this request.
	cat.set(t, "125.00", "1000.00")

	drain(t, s)
	s.Close()

	if want := dollars(t, "0.00625"); s.Result().Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: settlement must replay the rates captured at "+
			"admission", s.Result().Charge.ActualCost, want)
	}
}

// 8. An incomplete Response is not free. Usage the provider reported is usage the
// provider billed, even though generation stopped early.
func TestIncompleteStreamWithUsageChargesRealUsage(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		deltaEvent(t, 1, "the airspeed velo"),
		event(t, fmt.Sprintf(`{
			"type": "response.incomplete", "sequence_number": 9,
			"response": {
				"id": "resp_inc", "object": "response", "status": "incomplete", "model": %q,
				"incomplete_details": {"reason": "max_output_tokens"},
				"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
			}
		}`, gpt51)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-inc", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if !res.Settled {
		t.Fatal("an incomplete stream that reported usage must be charged: truncation is not a refund")
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !errors.Is(s.Err(), openai.ErrResponseIncomplete) {
		t.Errorf("Err = %v, want ErrResponseIncomplete: the caller must not mistake a "+
			"truncated answer for a complete one", s.Err())
	}

	rec := h.record(t, "stream-inc")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusSettled)
	}
	// The provider's own closed-vocabulary reason survives, so "we charged for a
	// truncated answer" stays legible.
	if !strings.Contains(rec.Error, "max_output_tokens") {
		t.Errorf("Error = %q, want the provider's incomplete reason", rec.Error)
	}
}

// 9. An incomplete Response with no usage is not reconstructed from the deltas that
// went past, and is not released either.
func TestIncompleteStreamWithoutUsageIsNotReconstructed(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		deltaEvent(t, 1, "a long stretch of generated text that would tokenize to something"),
		deltaEvent(t, 2, " and something more, all of which throttle must refuse to count"),
		event(t, fmt.Sprintf(`{
			"type": "response.incomplete", "sequence_number": 9,
			"response": {
				"id": "resp_nousage", "object": "response", "status": "incomplete", "model": %q,
				"incomplete_details": {"reason": "content_filter"}
			}
		}`, gpt51)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-inc-nousage", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if res.Settled {
		t.Fatal("nothing authoritative was reported: there is nothing to settle")
	}
	if res.Cost.Known() {
		t.Errorf("cost = %s: usage was reconstructed from streamed content", res.Cost.Amount)
	}
	if !res.Usage.Empty() {
		t.Errorf("usage = %v: the deltas were counted", res.Usage)
	}
	// Not released: generation demonstrably ran, since its events came through this code.
	if h.totals(t).Reserved == 0 {
		t.Error("an incomplete stream with no usage must not release its hold")
	}
	if got := h.record(t, "stream-inc-nousage").Status; got != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", got, activity.StatusOutstanding)
	}
	if !strings.Contains(res.Cost.Reason, "content_filter") {
		t.Errorf("cost reason = %q, want the provider's incomplete reason preserved", res.Cost.Reason)
	}
}

// 10. A failed Response that reported usage is accounted on that usage.
func TestFailedStreamWithUsageIsAccounted(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		event(t, fmt.Sprintf(`{
			"type": "response.failed", "sequence_number": 9,
			"response": {
				"id": "resp_failed", "object": "response", "status": "failed", "model": %q,
				"error": {"code": "server_error", "message": "the model stopped unexpectedly"},
				"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}
			}
		}`, gpt51)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-failed", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if !res.Settled {
		t.Fatal("a failed response that reported usage must be charged for it")
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !errors.Is(s.Err(), openai.ErrResponseFailed) {
		t.Errorf("Err = %v, want ErrResponseFailed", s.Err())
	}
}

// 11, 36. A failed Response with no usage stays conservative, and a provider failure
// is never reported as a budget denial. A caller that cannot tell the two apart will
// retry the wrong system.
func TestFailedStreamWithoutUsageIsConservativeAndNotADenial(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		deltaEvent(t, 1, "partial output"),
		event(t, fmt.Sprintf(`{
			"type": "response.failed", "sequence_number": 9,
			"response": {
				"id": "resp_failed2", "object": "response", "status": "failed", "model": %q,
				"error": {"code": "rate_limit_exceeded", "message": "slow down"}
			}
		}`, gpt51)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-failed-nousage", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	if errors.Is(s.Err(), engine.ErrDenied) {
		t.Fatal("an OpenAI failure must never be reported as a budget denial")
	}
	res := s.Result()
	if res.Cost.Known() {
		t.Errorf("cost = %s: the outcome was not knowable", res.Cost.Amount)
	}
	// The provider executed -- its events came through -- so the hold stays.
	if h.totals(t).Reserved == 0 {
		t.Error("a provider-executed failure with no usage must keep its hold encumbered")
	}

	rec := h.record(t, "stream-failed-nousage")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
	if rec.Outcome == activity.OutcomeBudgetDenied {
		t.Error("outcome = budget-denied for a provider failure")
	}
	if !strings.Contains(rec.Error, "rate_limit_exceeded") {
		t.Errorf("Error = %q, want the provider's error code", rec.Error)
	}
}

// 2. A stream that never reached the model releases its hold. Nothing was generated,
// and the pinned SDK cannot retry once a stream exists -- so a refused establishment
// cannot be a generation that already happened.
func TestStreamEstablishmentFailureReleasesTheHold(t *testing.T) {
	h := newStreamHarness(t, "1000", nil)
	h.up.establishErr = apiError(t, 429, "rate_limit_exceeded", "requests", "too many requests")

	_, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-refused", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrProvider) {
		t.Fatalf("RespondStreaming error = %v, want ErrProvider", err)
	}
	if errors.Is(err, engine.ErrDenied) {
		t.Fatal("a provider refusal is not a budget denial")
	}

	if got := h.totals(t).Reserved; got != 0 {
		t.Errorf("Reserved = %s, want 0: nothing was generated, so the headroom goes back", got)
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}

	rec := h.record(t, "stream-refused")
	if rec.Status != activity.StatusReleased {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusReleased)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeProviderError)
	}
	// The stream is closed: throttle owns it and must not leak the connection.
	if h.up.closeCount() == 0 {
		t.Error("a refused stream must still be closed")
	}
}

// 13. A stream that ends cleanly without a terminal Response is not a success. The
// SDK reports a clean end of stream and a finished generation identically, so only a
// terminal Response distinguishes them.
func TestEOFWithoutTerminalResponseIsNotSuccess(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		deltaEvent(t, 1, "the airspeed "),
		deltaEvent(t, 2, "velocity is 11 metres per second"),
		// And then the transport simply stops. No response.completed.
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-eof", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if res.Settled {
		t.Fatal("an EOF without a terminal response must not settle: nothing authoritative arrived")
	}
	if res.Cost.Known() {
		t.Errorf("cost = %s, want unknown: an EOF is not a completion", res.Cost.Amount)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("an EOF without a terminal response must keep its hold encumbered")
	}
	if h.totals(t).Spent != 0 {
		t.Error("an EOF without a terminal response must not record spend")
	}
	if got := h.record(t, "stream-eof").Status; got != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", got, activity.StatusOutstanding)
	}
	if !strings.Contains(res.Cost.Reason, "without a terminal response") {
		t.Errorf("cost reason = %q, want it to say the stream ended without a terminal response",
			res.Cost.Reason)
	}
}

// 14. A trailing transport error after an authoritative terminal Response does not
// undo the settlement that Response authorized.
func TestStreamErrorAfterTerminalResponseDoesNotUndoAccounting(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))
	h.up.err = &ssestream.StreamError{
		Message: "received error while streaming: " + promptText,
		Event:   ssestream.Event{Type: "error", Data: []byte(`{"error":{"message":"` + promptText + `"}}`)},
	}

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-trailing-err", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if !res.Settled {
		t.Fatal("a terminal response already authorized this settlement: a trailing transport " +
			"error must not undo it")
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if got := h.totals(t).Spent; got != dollars(t, "0.00625") {
		t.Errorf("Spent = %s: the settlement was undone by a trailing error", got)
	}
}

// 15. A stream error before any terminal Response preserves the hold: the request may
// well have been served and billed, and the transport failing says nothing either way.
func TestStreamErrorBeforeTerminalResponsePreservesTheHold(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		deltaEvent(t, 1, "partial"),
	}
	h := newStreamHarness(t, "1000", events)
	h.up.err = &ssestream.StreamError{
		Message: "received error while streaming: " + outputText,
		Event:   ssestream.Event{Type: "error", Data: []byte(outputText)},
	}

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-err", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	if !errors.Is(s.Err(), openai.ErrOutcomeUnknown) {
		t.Errorf("Err = %v, want ErrOutcomeUnknown", s.Err())
	}
	if h.totals(t).Reserved == 0 {
		t.Error("a stream error before a terminal response must keep its hold encumbered")
	}
	rec := h.record(t, "stream-err")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
	// 26, 27. The SDK's StreamError carries the raw SSE frame in both of its fields, so
	// neither may reach durable state.
	if strings.Contains(rec.Error, outputText) {
		t.Errorf("Error = %q: the raw stream payload reached durable storage", rec.Error)
	}
}

// 16. A cancelled context after the stream is established preserves the conservative
// liability: local cancellation does not prove zero provider spend.
func TestStreamCancellationPreservesConservativeLiability(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	ctx, cancel := context.WithCancel(context.Background())
	s, err := h.client.RespondStreaming(ctx, openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-cancel", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	// One event read, then the caller gives up before the terminal response.
	if !s.Next() {
		t.Fatal("the first event should have arrived")
	}
	cancel()

	// The stream must reach a terminal state whether or not the caller reads again.
	waitFor(t, func() bool { return s.Result() != nil })
	s.Close()

	res := s.Result()
	if res.Settled {
		t.Fatal("a cancelled stream reported no authoritative usage: there is nothing to settle")
	}
	if res.Cost.Known() {
		t.Errorf("cost = %s: cancellation does not establish what was spent", res.Cost.Amount)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("cancellation must not release the hold: OpenAI cannot cancel a response server-side")
	}
	rec := h.record(t, "stream-cancel")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
	if rec.Outcome != activity.OutcomeCancelled {
		t.Errorf("outcome = %q, want %q", rec.Outcome, activity.OutcomeCancelled)
	}
	// 26. Usage is not inferred from the deltas delivered before cancellation.
	if !res.Usage.Empty() {
		t.Errorf("usage = %v: the deltas delivered before cancellation were counted", res.Usage)
	}
}

// A deadline is recorded distinctly from a cancellation. Both leave the same
// liability, and an operator reading the record needs to know which happened.
func TestStreamDeadlineIsDistinguishedFromCancellation(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	s, err := h.client.RespondStreaming(ctx, openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-deadline", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	s.Next()
	waitFor(t, func() bool { return s.Result() != nil })
	s.Close()

	if got := h.record(t, "stream-deadline").Outcome; got != activity.OutcomeTimeout {
		t.Errorf("outcome = %q, want %q", got, activity.OutcomeTimeout)
	}
}

// 17. Closing before a terminal Response does not release the reservation. A caller
// that stopped reading is not evidence that the provider stopped generating.
func TestCloseBeforeTerminalResponseRetainsTheHold(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-close", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	if !s.Next() {
		t.Fatal("the first event should have arrived")
	}
	if err := s.Close(); !errors.Is(err, openai.ErrOutcomeUnknown) {
		t.Errorf("Close = %v, want ErrOutcomeUnknown: the outcome is genuinely unknown", err)
	}

	// Close blocks until accounting is durable, so the Result is available immediately.
	res := s.Result()
	if res == nil {
		t.Fatal("Close must not return before accounting is complete")
	}
	if res.Settled {
		t.Error("an early Close settles nothing: no authoritative usage arrived")
	}
	if h.totals(t).Reserved == 0 {
		t.Error("an early Close must leave the hold outstanding")
	}
	if got := h.record(t, "stream-close").Status; got != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", got, activity.StatusOutstanding)
	}
	// The provider's stream is closed: throttle owns it.
	if h.up.closeCount() == 0 {
		t.Error("Close must close the provider's stream")
	}
}

// 19. Concurrent Close from many goroutines: no double settle, no double release, no
// panic, and exactly one accounting action.
func TestConcurrentCloseIsSafe(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-concurrent-close", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.Close()
			// Result and Err must be safe to read from any goroutine.
			_ = s.Result()
			_ = s.Err()
		}()
	}
	close(start)
	wg.Wait()

	if got := h.up.closeCount(); got != 1 {
		t.Errorf("the provider stream was closed %d times, want 1", got)
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s: nothing authoritative arrived, so nothing may be charged", got)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("the hold must still be encumbered")
	}
}

// 20. A caller who takes a stream and never calls Next at all cannot renew a hold
// forever. The bound is armed at construction, not at the first read.
func TestStreamNeverReadCannotRenewForever(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500),
		func(c *openai.Config) { c.StreamStallTimeout = 40 * time.Millisecond })

	before := runtime.NumGoroutine()

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-never-read", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	// No Next, no Close, no cancel. The stream must terminalize on its own.
	waitFor(t, func() bool { return s.Result() != nil })

	res := s.Result()
	if res.Settled {
		t.Error("an unread stream reported no usage: there is nothing to settle")
	}
	// Abandonment says nothing about provider spend, so the hold stays.
	if h.totals(t).Reserved == 0 {
		t.Error("an abandoned stream must leave its hold outstanding")
	}
	if got := h.record(t, "stream-never-read").Status; got != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", got, activity.StatusOutstanding)
	}

	// 24, 25. No immortal renewal and no pinned goroutine.
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
	assertRenewalStopped(t, h, res.ReservationID)
}

// 21. A caller who stops between two Next calls hits the consumer-idle bound.
func TestIdleConsumerBetweenReadsIsAbandonment(t *testing.T) {
	// A stream with plenty left to send, so the caller's silence is what ends it.
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500),
		func(c *openai.Config) { c.StreamStallTimeout = 40 * time.Millisecond })

	before := runtime.NumGoroutine()

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-idle", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	// One read, then abandon: no Close, no cancel, no further reads.
	if !s.Next() {
		t.Fatal("the first event should have arrived")
	}

	waitFor(t, func() bool { return s.Result() != nil })

	res := s.Result()
	if res.Settled {
		t.Error("an abandoned stream cannot have settled: usage was never reported")
	}
	if h.totals(t).Reserved == 0 {
		t.Error("an abandoned stream must leave its hold outstanding")
	}
	if got := h.record(t, "stream-idle").Status; got != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", got, activity.StatusOutstanding)
	}
	if !strings.Contains(res.Cost.Reason, "stopped consuming") {
		t.Errorf("cost reason = %q, want it to name consumer abandonment", res.Cost.Reason)
	}

	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
	assertRenewalStopped(t, h, res.ReservationID)
}

// 22. A caller blocked inside Next on a slow provider is NOT abandonment, however
// long the provider takes. The idle bound must not become a maximum generation time.
//
// The provider is held for many multiples of the bound while the caller waits inside
// Next, and the stream still completes and settles normally.
func TestSlowProviderInsideNextIsNotAbandonment(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500),
		func(c *openai.Config) { c.StreamStallTimeout = 20 * time.Millisecond })

	// The read at index 1 -- the first delta -- blocks until released.
	release := h.up.blockFrom(1)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-slow", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	// The caller enters Next and stays there. Nothing else touches the stream.
	done := make(chan int, 1)
	go func() {
		n := 0
		for s.Next() {
			_ = s.Current()
			n++
		}
		done <- n
	}()

	// Wait until the caller is demonstrably parked in the provider's read, then hold it
	// there for many times the idle bound. If the watchdog counted this as abandonment,
	// the stream would terminalize here.
	<-h.up.entered
	waitFor(t, func() bool { return h.up.readCount() >= 2 })
	time.Sleep(200 * time.Millisecond)

	if s.Result() != nil {
		t.Fatal("a caller waiting inside Next on a slow provider was treated as abandonment: " +
			"the idle bound has become a maximum generation time")
	}

	close(release)
	<-done
	s.Close()

	res := s.Result()
	if !res.Settled {
		t.Fatalf("a slow but complete stream must settle: Err = %v", s.Err())
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
}

// 21, 22. A caller who pauses between events, over and over, is consuming the stream
// rather than abandoning it. This is the watchdog's other branch: the caller is outside
// Next when the timer fires, but has entered and left it since the last sample, so the
// bound is re-armed instead of enforced.
//
// The pauses are each shorter than the bound while the run as a whole is several times
// longer than it, which is the case that distinguishes the two readings. The watchdog
// fires on its own schedule rather than the caller's, so without the progress check the
// first firing that landed in a pause would end a perfectly healthy stream -- and a
// bound of one interval total would make the whole mechanism a stopwatch on the caller.
func TestCallerPausingBetweenEventsIsNotAbandonment(t *testing.T) {
	const idle = 40 * time.Millisecond

	// Enough events that the watchdog is guaranteed to fire during a pause rather than
	// only after the stream has finished.
	events := []responses.ResponseStreamEventUnion{createdEvent(t, gpt51)}
	for i := 1; i <= 8; i++ {
		events = append(events, deltaEvent(t, i, "word "))
	}
	events = append(events, completedEvent(t, gpt51, 1000, 500))

	h := newStreamHarness(t, "1000", events,
		func(c *openai.Config) { c.StreamStallTimeout = idle })

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-paused-caller", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	n := 0
	for s.Next() {
		_ = s.Current()
		n++
		// Well inside the bound, but repeated until the total far exceeds it.
		time.Sleep(idle / 2)
	}
	s.Close()

	if n != len(events) {
		t.Errorf("received %d events, want %d: a caller pausing between events was cut off",
			n, len(events))
	}
	res := s.Result()
	if !res.Settled {
		t.Fatalf("a caller who keeps reading must settle normally however long the whole "+
			"stream takes: Err = %v", s.Err())
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
}

// 23. A genuinely active long stream renews its hold before the lease expires.
//
// Real clock and a short lease, because lease expiry is compared against wall clock
// inside the ledger; the determinism comes from waiting for the renewal count to move,
// not from sleeping a guessed interval.
func TestActiveStreamRenewsItsLease(t *testing.T) {
	h := newStreamHarnessWithLease(t, "1000", leaseQuantum,
		normalEventStream(t, gpt51, 1000, 500), generousStall)

	before := runtime.NumGoroutine()

	// The read of the terminal event blocks, so the stream stays legitimately alive.
	release := h.up.blockFrom(3)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-renew", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for s.Next() {
			_ = s.Current()
		}
	}()

	// The renewal is what proves the keepalive is running: the hold's renew count moves
	// while the caller is still waiting on the provider.
	waitFor(t, func() bool { return h.reservation(t, s.ReservationID()).RenewCount > 0 })

	// And the hold has not expired underneath the live stream.
	if r := h.reservation(t, s.ReservationID()); !r.ExpiresAt.After(time.Now().UTC()) {
		t.Errorf("the hold expired at %s while the stream was still running", r.ExpiresAt)
	}

	close(release)
	<-done
	s.Close()

	if !s.Result().Settled {
		t.Fatalf("a renewed stream must still settle: Err = %v", s.Err())
	}

	// 24. The renewal stops at the terminal state.
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
	assertRenewalStopped(t, h, s.ReservationID())
}

// 24, 25. Every terminal path stops the supervisor: no leaked goroutine and no
// immortal renewal, whichever way the stream ended.
//
// One table over every ending, because the guarantee is about the set of paths rather
// than about any one of them -- a new terminal path that forgot to stop the supervisor
// is exactly the regression this catches.
func TestEveryTerminalPathStopsTheSupervisor(t *testing.T) {
	cases := []struct {
		name string
		// events the provider will produce.
		events func(*testing.T) []responses.ResponseStreamEventUnion
		// streamErr is what the stream reports when it stops producing.
		streamErr error
		// run drives the stream to its terminal state.
		run func(*testing.T, *openai.Stream, context.CancelFunc)
	}{
		{
			name:   "completed",
			events: func(t *testing.T) []responses.ResponseStreamEventUnion { return normalEventStream(t, gpt51, 1000, 500) },
			run:    func(t *testing.T, s *openai.Stream, _ context.CancelFunc) { drain(t, s) },
		},
		{
			name: "incomplete",
			events: func(t *testing.T) []responses.ResponseStreamEventUnion {
				return []responses.ResponseStreamEventUnion{
					createdEvent(t, gpt51),
					event(t, fmt.Sprintf(`{"type": "response.incomplete", "sequence_number": 9,
						"response": {"id": "r", "object": "response", "status": "incomplete", "model": %q,
						"incomplete_details": {"reason": "max_output_tokens"},
						"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}}`, gpt51)),
				}
			},
			run: func(t *testing.T, s *openai.Stream, _ context.CancelFunc) { drain(t, s) },
		},
		{
			name: "failed",
			events: func(t *testing.T) []responses.ResponseStreamEventUnion {
				return []responses.ResponseStreamEventUnion{
					createdEvent(t, gpt51),
					event(t, fmt.Sprintf(`{"type": "response.failed", "sequence_number": 9,
						"response": {"id": "r", "object": "response", "status": "failed", "model": %q,
						"error": {"code": "server_error", "message": "x"}}}`, gpt51)),
				}
			},
			run: func(t *testing.T, s *openai.Stream, _ context.CancelFunc) { drain(t, s) },
		},
		{
			name: "cancelled terminal status",
			events: func(t *testing.T) []responses.ResponseStreamEventUnion {
				// The pinned SDK enumerates no response.cancelled event, so cancellation can
				// only arrive as a terminal Response status. A rule keyed on event type names
				// would miss this; one keyed on the Response's own status does not.
				return []responses.ResponseStreamEventUnion{
					createdEvent(t, gpt51),
					event(t, fmt.Sprintf(`{"type": "response.completed", "sequence_number": 9,
						"response": {"id": "r", "object": "response", "status": "cancelled", "model": %q,
						"usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}}`, gpt51)),
				}
			},
			run: func(t *testing.T, s *openai.Stream, _ context.CancelFunc) { drain(t, s) },
		},
		{
			name: "stream error",
			events: func(t *testing.T) []responses.ResponseStreamEventUnion {
				return []responses.ResponseStreamEventUnion{createdEvent(t, gpt51)}
			},
			streamErr: &ssestream.StreamError{Message: "boom", Event: ssestream.Event{Type: "error"}},
			run:       func(t *testing.T, s *openai.Stream, _ context.CancelFunc) { drain(t, s) },
		},
		{
			name: "eof without terminal",
			events: func(t *testing.T) []responses.ResponseStreamEventUnion {
				return []responses.ResponseStreamEventUnion{createdEvent(t, gpt51), deltaEvent(t, 1, "x")}
			},
			run: func(t *testing.T, s *openai.Stream, _ context.CancelFunc) { drain(t, s) },
		},
		{
			name:   "close",
			events: func(t *testing.T) []responses.ResponseStreamEventUnion { return normalEventStream(t, gpt51, 1000, 500) },
			run: func(t *testing.T, s *openai.Stream, _ context.CancelFunc) {
				s.Next()
				s.Close()
			},
		},
		{
			name:   "cancellation",
			events: func(t *testing.T) []responses.ResponseStreamEventUnion { return normalEventStream(t, gpt51, 1000, 500) },
			run: func(t *testing.T, s *openai.Stream, cancel context.CancelFunc) {
				s.Next()
				cancel()
				waitFor(t, func() bool { return s.Result() != nil })
			},
		},
		{
			name:   "idle abandonment",
			events: func(t *testing.T) []responses.ResponseStreamEventUnion { return normalEventStream(t, gpt51, 1000, 500) },
			run: func(t *testing.T, s *openai.Stream, _ context.CancelFunc) {
				s.Next()
				waitFor(t, func() bool { return s.Result() != nil })
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A short lease so the renewal is genuinely running, and a short idle bound so
			// the abandonment case terminalizes within the test. The other cases reach their
			// terminal state before the bound matters.
			h := newStreamHarnessWithLease(t, "1000", leaseQuantum, tc.events(t),
				func(c *openai.Config) { c.StreamStallTimeout = 60 * time.Millisecond })
			h.up.err = tc.streamErr

			before := runtime.NumGoroutine()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, err := h.client.RespondStreaming(ctx, openai.StreamRequest{
				BudgetID: "team", RequestID: "stream-" + tc.name, Params: request(gpt51, maxOut(2000)),
			})
			if err != nil {
				t.Fatalf("RespondStreaming: %v", err)
			}

			tc.run(t, s, cancel)
			s.Close()

			if s.Result() == nil {
				t.Fatal("the stream must reach a terminal state")
			}

			// No goroutine left behind.
			waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
			// And no immortal lease renewal.
			assertRenewalStopped(t, h, s.Result().ReservationID)
		})
	}
}

// A terminal stream whose hold is still outstanding stops renewing it. This is the one
// shape where nothing else can stop the keepalive, so it is the shape that proves the
// terminal path stops it deliberately.
//
// Every other ending has an accidental backstop: a settled transaction makes the next
// Renew report ErrAlreadyResolved, and a short idle bound makes the watchdog fire and
// return. Here the transaction stays unresolved -- an early Close leaves the hold
// encumbered on purpose -- and the idle bound is far away, so an unstopped supervisor
// would renew this hold until the process died. That is precisely the immortal
// reservation the design exists to prevent.
func TestTerminalStreamStopsRenewingAnOutstandingHold(t *testing.T) {
	h := newStreamHarnessWithLease(t, "1000", leaseQuantum,
		normalEventStream(t, gpt51, 1000, 500), generousStall)

	before := runtime.NumGoroutine()

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-outstanding-renew", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	// Read one event, then close early. The hold stays outstanding, so the transaction
	// will never refuse a renewal.
	if !s.Next() {
		t.Fatal("the first event should have arrived")
	}
	s.Close()

	if h.totals(t).Reserved == 0 {
		t.Fatal("this test needs an outstanding hold: an early Close must not release it")
	}

	// Several renewal intervals go by. A running keepalive would have ticked repeatedly.
	renewals := h.reservation(t, s.ReservationID()).RenewCount
	time.Sleep(3 * leaseQuantum / 2)
	if got := h.reservation(t, s.ReservationID()).RenewCount; got != renewals {
		t.Errorf("the outstanding hold renewed %d more times after the stream ended: the "+
			"reservation is immortal", got-renewals)
	}
	// And the hold is allowed to expire, which is what lets reconciliation find it.
	if r := h.reservation(t, s.ReservationID()); r.ExpiresAt.After(time.Now().UTC()) {
		t.Errorf("the hold still expires at %s, in the future: it is being kept alive", r.ExpiresAt)
	}
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
}

// assertRenewalStopped proves the lease renewal really stopped, by observing that the
// hold's renew count does not move after the stream is terminal.
//
// The renew count rather than a goroutine count, because a goroutine that has returned
// but not yet been scheduled out is indistinguishable from one that is still renewing,
// and the renewal is the thing that actually keeps headroom encumbered.
func assertRenewalStopped(t *testing.T, h *streamHarness, reservationID string) {
	t.Helper()
	renewals := h.reservation(t, reservationID).RenewCount
	// Two renewal intervals of the short lease used by these tests, so a still-running
	// keepalive would have ticked.
	time.Sleep(2 * leaseQuantum / 3)
	if got := h.reservation(t, reservationID).RenewCount; got != renewals {
		t.Errorf("the hold renewed %d more times after the stream ended: the keepalive is immortal",
			got-renewals)
	}
}

// 29. An event kind this build has never heard of passes through untouched. Forwarding
// must not require understanding: throttle inspects events for accounting metadata, it
// does not gatekeep them.
func TestUnknownEventKindsPassThroughUntouched(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		// A future non-terminal event, with a type no version of the SDK enumerates and
		// fields throttle knows nothing about.
		event(t, `{"type": "response.hologram.delta", "sequence_number": 1,
			"delta": "some future modality", "item_id": "holo_1", "output_index": 3}`),
		// A future event that even carries something Response-shaped, but whose status is
		// not terminal. Terminality is a status, and an unrecognized status is not one.
		event(t, fmt.Sprintf(`{"type": "response.rethinking", "sequence_number": 2,
			"response": {"id": "resp_stream", "object": "response", "status": "deliberating", "model": %q}}`, gpt51)),
		completedEvent(t, gpt51, 1000, 500),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-unknown", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	var seen []string
	for s.Next() {
		ev := s.Current()
		seen = append(seen, ev.Type)
		if ev.Type == "response.hologram.delta" {
			// The event reaches the caller with its fields intact, not reduced to something
			// throttle understands.
			if ev.Delta != "some future modality" {
				t.Errorf("Delta = %q, want the SDK's own value: throttle rewrote an event", ev.Delta)
			}
			if ev.SequenceNumber != 1 {
				t.Errorf("SequenceNumber = %d, want 1", ev.SequenceNumber)
			}
		}
	}
	s.Close()

	want := []string{"response.created", "response.hologram.delta", "response.rethinking", "response.completed"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v: throttle must forward every event untouched", seen, want)
	}

	// The unrecognized Response-carrying event did not terminalize the stream early: the
	// real completed event is what settled it.
	if !s.Result().Settled {
		t.Fatal("the stream should have settled from response.completed")
	}
	if want := dollars(t, "0.00625"); s.Result().Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", s.Result().Charge.ActualCost, want)
	}
}

// Terminality is the Response's own status, not the event's type name. A terminal
// Response arriving under an event name this build has never seen still settles.
//
// This is the anti-vacuous half of the terminality rule, and it is not hypothetical:
// the pinned SDK enumerates no response.cancelled event at all, so a cancelled
// Response can only ever arrive under some other event's name. A rule keyed on type
// names would leave this stream unaccounted and its hold standing, which is why the
// pair below is asserted in both directions.
func TestTerminalityIsTheResponseStatusNotTheEventName(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		// A completed Response under an event name that does not exist in this SDK.
		event(t, fmt.Sprintf(`{"type": "response.finished", "sequence_number": 9,
			"response": {"id": "r", "object": "response", "status": "completed", "model": %q,
			"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}}}`, gpt51)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-unnamed-terminal", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if !res.Settled {
		t.Fatalf("a completed Response must settle whatever the event carrying it is called: "+
			"Err = %v", s.Err())
	}
	if want := dollars(t, "0.00625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
}

// And the other direction: an event named like a terminal one whose Response says it
// is still running is not an accounting boundary. Only the status decides.
func TestTerminalEventNameWithRunningResponseIsNotTerminal(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		// The name says completed; the Response says otherwise, and carries usage figures
		// that a name-keyed rule would settle on.
		event(t, fmt.Sprintf(`{"type": "response.completed", "sequence_number": 5,
			"response": {"id": "r", "object": "response", "status": "in_progress", "model": %q,
			"usage": {"input_tokens": 999999, "output_tokens": 999999, "total_tokens": 1999998}}}`, gpt51)),
		// The real ending, with the real figures.
		completedEvent(t, gpt51, 1000, 500),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-named-terminal", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	if n := drain(t, s); n != 3 {
		t.Errorf("received %d events, want 3: a still-running Response must not end the stream", n)
	}
	s.Close()

	if want := dollars(t, "0.00625"); s.Result().Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: an in-progress Response under a terminal event "+
			"name was settled", s.Result().Charge.ActualCost, want)
	}
}

// A content lifecycle event is not an accounting boundary, however final its name
// sounds. output_text.done, function_call_arguments.done, and mcp_call.completed are
// all about content, not about the bill.
//
// Anti-vacuous: the stream ends after those events without any terminal Response, so a
// rule that mistook one for terminality would settle here -- and settle on no usage at
// all.
func TestContentDoneEventsAreNotTerminal(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		event(t, `{"type": "response.output_text.done", "sequence_number": 1,
			"item_id": "msg_1", "output_index": 0, "content_index": 0, "text": "done text"}`),
		event(t, `{"type": "response.function_call_arguments.done", "sequence_number": 2,
			"item_id": "fc_1", "output_index": 1, "arguments": "{}", "name": "lookup"}`),
		event(t, `{"type": "response.mcp_call.completed", "sequence_number": 3,
			"item_id": "mcp_1", "output_index": 2}`),
		event(t, `{"type": "response.output_item.done", "sequence_number": 4,
			"output_index": 0, "item": {"type": "message", "id": "msg_1", "role": "assistant",
			"status": "completed", "content": []}}`),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-content-done", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if res.Settled {
		t.Fatal("a content lifecycle event was mistaken for an accounting boundary")
	}
	if res.Cost.Known() {
		t.Errorf("cost = %s: no authoritative response arrived", res.Cost.Amount)
	}
	if !strings.Contains(res.Cost.Reason, "without a terminal response") {
		t.Errorf("cost reason = %q, want it to say no terminal response arrived", res.Cost.Reason)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("the hold must stay encumbered")
	}
}

// A non-terminal Response-carrying event -- created, queued, in_progress -- is not an
// accounting boundary either, even though it carries a full Response.
func TestInProgressResponseEventsAreNotTerminal(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		event(t, fmt.Sprintf(`{"type": "response.created", "sequence_number": 0,
			"response": {"id": "r", "object": "response", "status": "in_progress", "model": %q,
			"usage": {"input_tokens": 999999, "output_tokens": 999999, "total_tokens": 1999998}}}`, gpt51)),
		event(t, fmt.Sprintf(`{"type": "response.queued", "sequence_number": 1,
			"response": {"id": "r", "object": "response", "status": "queued", "model": %q}}`, gpt51)),
		event(t, fmt.Sprintf(`{"type": "response.in_progress", "sequence_number": 2,
			"response": {"id": "r", "object": "response", "status": "in_progress", "model": %q}}`, gpt51)),
		completedEvent(t, gpt51, 1000, 500),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-inprogress", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	// The absurd usage figures on the in-progress event were not settled: only the
	// terminal Response's own figures were.
	if want := dollars(t, "0.00625"); s.Result().Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: an in-progress event's usage was settled",
			s.Result().Charge.ActualCost, want)
	}
}

// An "error" event carries no Response, so it cannot be a terminal accounting
// boundary. It is a provider-reported failure whose outcome remains unknown.
func TestErrorEventIsNotATerminalResponse(t *testing.T) {
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		event(t, `{"type": "error", "sequence_number": 1, "code": "server_error",
			"message": "something went wrong", "param": null}`),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-error-event", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}

	// The event still reaches the caller: throttle does not swallow it.
	var sawError bool
	for s.Next() {
		if s.Current().Type == "error" {
			sawError = true
		}
	}
	s.Close()

	if !sawError {
		t.Error("the error event must be forwarded to the caller")
	}
	if s.Result().Settled {
		t.Error("an error event carries no authoritative usage and cannot settle a request")
	}
	if h.totals(t).Reserved == 0 {
		t.Error("the hold must stay encumbered: the outcome is unknown")
	}
}

// 30. A hosted-tool stream keeps #26's cost-completeness semantics: the token cost is
// a floor, never a total, because OpenAI bills the tool outside the usage object.
//
// Seeing a streamed tool event does not make the surcharge priceable, so the
// classification comes from the request, exactly as it does for a single round trip.
func TestHostedToolStreamRetainsCostIncompleteness(t *testing.T) {
	in := request(gpt51, maxOut(2000))
	in.Tools = []responses.ToolUnionParam{
		{OfWebSearch: &responses.WebSearchToolParam{Type: responses.WebSearchToolTypeWebSearch}},
	}
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		event(t, `{"type": "response.web_search_call.in_progress", "sequence_number": 1,
			"item_id": "ws_1", "output_index": 0}`),
		event(t, `{"type": "response.web_search_call.completed", "sequence_number": 2,
			"item_id": "ws_1", "output_index": 0}`),
		completedEvent(t, gpt51, 1000, 500),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-tool", Params: in,
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if res.Cost.Known() {
		t.Fatalf("cost = %s: a hosted tool's charge is not in the usage object, so the "+
			"token cost cannot be the total", res.Cost.Amount)
	}
	if res.Cost.State() != usage.CostPartial {
		t.Errorf("cost state = %q, want partial: the token half priced fine", res.Cost.State())
	}
	// The floor is the token cost, which is real information rather than nothing.
	if want := dollars(t, "0.00625"); res.Cost.AtLeast() != want {
		t.Errorf("AtLeast = %s, want %s", res.Cost.AtLeast(), want)
	}
	if !strings.Contains(res.Cost.Reason, "web_search") {
		t.Errorf("cost reason = %q, want it to name the tool", res.Cost.Reason)
	}
	if got := h.record(t, "stream-tool").Status; got != activity.StatusUnresolved {
		t.Errorf("status = %q, want %q", got, activity.StatusUnresolved)
	}
	// The hold stays encumbered rather than being settled at the floor.
	if h.totals(t).Reserved == 0 {
		t.Error("a partially-priced stream must keep its hold encumbered")
	}
}

// A hosted tool AND an uncaptured service tier: compound incompleteness. The final
// cost must represent the strongest missing knowledge without erasing either
// diagnostic.
func TestHostedToolWithUnknownTierKeepsBothDiagnostics(t *testing.T) {
	in := request(gpt51, maxOut(2000))
	in.Tools = []responses.ToolUnionParam{
		{OfWebSearch: &responses.WebSearchToolParam{Type: responses.WebSearchToolTypeWebSearch}},
	}
	events := []responses.ResponseStreamEventUnion{
		createdEvent(t, gpt51),
		event(t, fmt.Sprintf(`{"type": "response.completed", "sequence_number": 9,
			"response": {"id": "r", "object": "response", "status": "completed", "model": %q,
			"service_tier": "turbo-2027",
			"usage": {"input_tokens": 1000, "output_tokens": 500, "total_tokens": 1500}}}`, gpt51)),
	}
	h := newStreamHarness(t, "1000", events)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-compound", Params: in,
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	// The strongest missing knowledge wins: with no priceable tier there is not even a
	// floor, so the cost is unknown rather than partial.
	if res.Cost.State() != usage.CostUnknown {
		t.Errorf("cost state = %q, want unknown: an unpriceable tier leaves no floor to report",
			res.Cost.State())
	}
	// Both diagnostics survive. One incompleteness reason must not erase the other.
	if !strings.Contains(res.Cost.Reason, "turbo-2027") {
		t.Errorf("cost reason = %q, want the uncaptured tier named", res.Cost.Reason)
	}
	if !strings.Contains(res.Cost.Reason, "web_search") {
		t.Errorf("cost reason = %q, want the unpriceable tool named", res.Cost.Reason)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("the hold must stay encumbered")
	}
}

// 31. A child budget's stream consumes the whole ancestor chain, so an ancestor cannot
// be overspent by a descendant's streaming request.
func TestChildStreamConsumesTheAncestorChain(t *testing.T) {
	h := newStreamHarnessWithLease(t, "1000", time.Minute,
		normalEventStream(t, gpt51, 1000, 500), generousStall)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "child", RequestID: "stream-child", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	want := dollars(t, "0.00625")
	for _, id := range []string{"child", "team"} {
		if got := h.totalsFor(t, id).Spent; got != want {
			t.Errorf("%s Spent = %s, want %s: a stream must consume the whole chain", id, got, want)
		}
	}
	// The recorded attribution is the chain the money actually moved through.
	rec := h.record(t, "stream-child")
	if len(rec.Scopes) != 2 {
		t.Errorf("recorded %d scopes, want 2 (child and its parent)", len(rec.Scopes))
	}
}

// 32. Concurrent streams cannot oversubscribe a shared ancestor. The parent's ceiling
// is the only thing that can refuse, and it must hold under a race.
func TestConcurrentStreamsCannotOversubscribeAnAncestor(t *testing.T) {
	// A parent that can afford about three of these requests, and a child whose own
	// allocation is far larger -- so only the parent's ceiling can refuse.
	h := newHierarchicalStreamHarness(t, "0.20", "1000")

	const attempts = 12
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
		denied   int
	)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
				BudgetID:  "child",
				RequestID: fmt.Sprintf("stream-race-%d", i),
				Params:    request(gpt51, maxOut(2000)),
			})
			mu.Lock()
			if err != nil {
				denied++
			} else {
				admitted++
			}
			mu.Unlock()
			if err != nil {
				if !errors.Is(err, engine.ErrDenied) {
					t.Errorf("error = %v, want ErrDenied", err)
				}
				return
			}
			drain(t, s)
			s.Close()
		}(i)
	}
	close(start)
	wg.Wait()

	if admitted == 0 {
		t.Fatal("no stream was admitted: the test proves nothing about a contended ancestor")
	}
	if denied == 0 {
		t.Fatal("every stream was admitted: the parent's ceiling was not contended")
	}

	// The invariant: the parent's committed position never exceeded its allocation.
	tot := h.totalsFor(t, "team")
	if committed := tot.Spent + tot.Reserved; committed > dollars(t, "0.20") {
		t.Errorf("the parent committed %s against an allocation of $0.20: concurrent streams "+
			"oversubscribed an ancestor", committed)
	}
}

// 37, 38 are the existing suites, which run unchanged. This is the streaming half of
// the same guarantee: the streaming and non-streaming estimates agree, because both
// forms of one request consume the same tokens and run through one code path.
func TestStreamEstimateMatchesRespondEstimate(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	in := request(gpt51, maxOut(2000))
	est, err := h.client.Estimate(context.Background(), in)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-est", Params: in,
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	// The admission decision is available before a single event is read, which is what
	// makes a stream governed rather than merely observed.
	if !s.Decision().Admitted {
		t.Error("an admitted stream must report an admitting decision")
	}
	drain(t, s)
	s.Close()

	res := s.Result()
	if res.Estimate.Cost.Amount != est.Cost.Amount {
		t.Errorf("streaming estimate = %s, non-streaming = %s: the two forms of one request "+
			"consume the same tokens", res.Estimate.Cost.Amount, est.Cost.Amount)
	}
	if tokens(res.Estimate.Usage, usage.InputTokens) != tokens(est.Usage, usage.InputTokens) {
		t.Error("the streaming and non-streaming estimates disagree on input tokens")
	}
	// The operation differs, which is the only thing that should.
	if res.Identity.Operation != "responses-stream" {
		t.Errorf("Operation = %q, want responses-stream", res.Identity.Operation)
	}
	if est.Identity.Operation != "responses" {
		t.Errorf("non-streaming Operation = %q, want responses", est.Identity.Operation)
	}
}

// The request reaches OpenAI unmodified, including the absence of a `stream` field:
// the SDK's NewStreaming sets that itself, and throttle must not pre-empt it.
func TestStreamRequestReachesProviderUnmodified(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))

	in := request(gpt51, maxOut(777))
	in.ServiceTier = responses.ResponseNewParamsServiceTierFlex
	in.Store = param.NewOpt(false)

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-passthrough", Params: in,
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	got := h.stream.lastParams()
	if got.MaxOutputTokens.Value != 777 {
		t.Errorf("MaxOutputTokens = %d, want the caller's 777", got.MaxOutputTokens.Value)
	}
	if got.ServiceTier != responses.ResponseNewParamsServiceTierFlex {
		t.Errorf("ServiceTier = %q, want the caller's flex", got.ServiceTier)
	}
	if got.Store.Value {
		t.Error("Store was rewritten: governing a request is not editing it")
	}
}

// A client built without a stream client reports so plainly rather than panicking, and
// reserves nothing.
func TestRespondStreamingRequiresAStreamClient(t *testing.T) {
	h := newHarness(t, "1000")

	_, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-noclient", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrNoStreamClient) {
		t.Fatalf("error = %v, want ErrNoStreamClient", err)
	}
	if h.totals(t).Reserved != 0 {
		t.Error("nothing may be reserved when no stream can be created")
	}
}

// A streaming call that produces no stream at all leaves the outcome unknown rather
// than free: throttle cannot tell whether the model ran.
func TestStreamWithNoEventStreamLeavesTheHold(t *testing.T) {
	h := newStreamHarness(t, "1000", nil)
	h.stream.nilStream = true

	_, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-nostream", Params: request(gpt51, maxOut(2000)),
	})
	if !errors.Is(err, openai.ErrAccounting) {
		t.Fatalf("error = %v, want ErrAccounting", err)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("the hold must stay outstanding: whether the model ran is unknowable")
	}
	if got := h.record(t, "stream-nostream").Status; got != activity.StatusOutstanding {
		t.Errorf("status = %q, want %q", got, activity.StatusOutstanding)
	}
}

// Enforce mode refuses an unpriceable streaming request before the provider is called,
// exactly as it does a non-streaming one.
func TestEnforceRejectsUnpriceableStreamBeforeCalling(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, "gpt-6-unreleased", 1000, 500))

	_, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-unpriced", Params: request("gpt-6-unreleased", maxOut(2000)),
	})
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("error = %v, want ErrCostUnknown", err)
	}
	if h.stream.callCount() != 0 {
		t.Error("a refused request must not reach the provider")
	}
	if h.totals(t).Reserved != 0 {
		t.Error("a refused request reserves nothing")
	}
}

// A stream works without an activity store: the transaction still governs, only the
// durable trail is missing.
func TestStreamWorksWithoutAnActivityStore(t *testing.T) {
	h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500),
		func(c *openai.Config) { c.Activity = nil })

	s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
		BudgetID: "team", RequestID: "stream-noacts", Params: request(gpt51, maxOut(2000)),
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	drain(t, s)
	s.Close()

	if !s.Result().Settled {
		t.Fatalf("the stream must still settle: Err = %v", s.Err())
	}
}
