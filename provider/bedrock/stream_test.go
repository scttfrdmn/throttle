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
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"throttle/activity"
	activitysqlite "throttle/activity/sqlite"
	"throttle/budget"
	"throttle/engine"
	"throttle/ledger"
	"throttle/ledger/sqlite"
	"throttle/pricing"
	"throttle/provider/bedrock"
	"throttle/usage"
)

// streamHarness is the Converse harness plus a streaming client.
type streamHarness struct {
	*harness
	stream *fakeStreamAPI
	reader *fakeReader
}

func newStreamHarness(t *testing.T, allocation string, opts ...func(*bedrock.Config)) *streamHarness {
	t.Helper()
	reader := newFakeReader()
	api := &fakeStreamAPI{reader: reader}
	h := newHarness(t, allocation, append([]func(*bedrock.Config){withStream(api)}, opts...)...)
	return &streamHarness{harness: h, stream: api, reader: reader}
}

// 1. A normal complete stream: events arrive incrementally, the terminal metadata
// is observed, the captured quote prices it, it settles exactly once, and the
// activity record says settled.
func TestStreamSettlesFromTerminalMetadata(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs)
	h.reader.emit(normalStream(1000, 500)...)

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID:  "team",
		RequestID: "stream-1",
		Input:     streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}

	events := drain(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(events) != 7 {
		t.Fatalf("received %d events, want all 7 forwarded", len(events))
	}
	if got := textOf(events[2]); got != "the airspeed velocity" {
		t.Errorf("first delta = %q, want the provider's own text unmodified", got)
	}

	res := s.Result()
	if res == nil {
		t.Fatal("Result must be available once the stream is terminal")
	}
	if !res.Settled {
		t.Fatalf("the stream should have settled: %v", s.Err())
	}
	// 1000 input at $3/M plus 500 output at $15/M = $0.003 + $0.0075.
	want := dollars(t, "0.0105")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if res.Usage.Count(usage.OutputTokens) != 500 {
		t.Errorf("output tokens = %d, want 500 from the metadata event", res.Usage.Count(usage.OutputTokens))
	}
	if res.Identity.Operation != bedrock.OperationConverseStream {
		t.Errorf("Operation = %q, want %q", res.Identity.Operation, bedrock.OperationConverseStream)
	}

	// Settled exactly once: nothing pending, and the charge is the only spend.
	tot := h.totals(t)
	if tot.Reserved != 0 || tot.PendingCount != 0 {
		t.Errorf("reserved = %s in %d holds, want the hold fully resolved", tot.Reserved, tot.PendingCount)
	}
	if tot.Spent != want {
		t.Errorf("spent = %s, want %s charged exactly once", tot.Spent, want)
	}

	rec := getRecord(t, acts, "stream-1")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want settled", rec.Status)
	}
	if rec.ProviderLatency != 4321*time.Millisecond {
		t.Errorf("ProviderLatency = %s, want the streaming metrics latency", rec.ProviderLatency)
	}
	assertNoContent(t, rec)
}

// The events must reach the caller as they are produced, not in a batch after the
// stream ends. Proven by a stream that blocks after the first delta: the caller
// receives it while the provider is still generating.
func TestStreamDeliversEventsIncrementally(t *testing.T) {
	h := newStreamHarness(t, "1000")

	release := make(chan struct{})
	go func() {
		defer close(h.reader.events)
		h.reader.events <- msgStart()
		h.reader.events <- delta("first")
		<-release // The provider is still generating at this point.
		h.reader.events <- metadata(1000, 500)
	}()

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-inc", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	defer s.Close()

	// If throttle buffered the response, this receive would block until the stream
	// finished -- which cannot happen until we release it.
	<-s.Events()
	got := <-s.Events()
	if textOf(got) != "first" {
		t.Fatalf("second event = %v, want the first delta delivered before the stream ended", got)
	}
	close(release)
	drain(t, s)
}

// 2. A stream slower than its lease quantum: the hold is renewed, the headroom
// stays encumbered on the ancestor throughout, settlement still succeeds, and the
// keepalive goroutine exits.
func TestSlowStreamRenewsItsLease(t *testing.T) {
	// A 300ms lease with a 100ms renewal interval against a stream that takes ~500ms.
	h := newStreamHarnessWithLease(t, "1000", 300*time.Millisecond)
	h.reader.pace = 60 * time.Millisecond
	h.reader.emit(normalStream(1000, 500)...)

	before := runtime.NumGoroutine()

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "child", RequestID: "stream-slow", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}

	// While the stream is alive the ancestor's headroom stays encumbered: a long
	// stream must not become invisible to a parent budget partway through.
	var sawEncumbered bool
	for ev := range s.Events() {
		_ = ev
		if h.scopeTotals(t, "team").Reserved > 0 {
			sawEncumbered = true
		}
	}
	if !sawEncumbered {
		t.Error("the parent budget's headroom must stay encumbered while a child's stream is alive")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res := s.Result()
	if !res.Settled {
		t.Fatalf("a renewed stream must still settle: %v", s.Err())
	}

	// The renewal really happened, rather than the stream merely finishing inside
	// one lease quantum.
	r, err := h.ledger.Get(context.Background(), res.ReservationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.RenewCount == 0 {
		t.Error("a stream outliving its lease quantum must have renewed the hold")
	}

	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
}

// 3. The most important semantic: a caller who closes before the metadata event
// does not get their money back. The underlying stream closes, the reservation
// stays, and the outcome is recorded as unknown.
func TestCloseBeforeMetadataRetainsTheHold(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs)
	h.reader.emit(normalStream(1000, 500)...)

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-early-close", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}

	// Read two events, then walk away: metadata has definitely not arrived.
	<-s.Events()
	<-s.Events()

	closeErr := s.Close()
	if !errors.Is(closeErr, bedrock.ErrOutcomeUnknown) {
		t.Fatalf("Close err = %v, want ErrOutcomeUnknown", closeErr)
	}

	// The AWS stream is closed: throttle owns it and must not leak the connection.
	if h.reader.closeCount() == 0 {
		t.Error("Close must close the underlying AWS event stream")
	}

	// The hold stays. A caller that stopped reading is not proof the model stopped
	// generating, so releasing here would hand already-spent headroom back.
	tot := h.totals(t)
	if tot.Reserved == 0 {
		t.Fatal("the reservation must NOT be released merely because the caller stopped reading")
	}
	if tot.Spent != 0 {
		t.Errorf("spent = %s, want nothing charged: no usage was ever reported", tot.Spent)
	}

	res := s.Result()
	if res.Settled {
		t.Error("a stream closed before metadata cannot have settled")
	}
	if res.Cost.Known() {
		t.Errorf("Cost = %s, want explicitly unknown", res.Cost)
	}

	rec := getRecord(t, acts, "stream-early-close")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}
	if rec.Outcome != activity.OutcomeCancelled {
		t.Errorf("outcome = %q, want cancelled", rec.Outcome)
	}
	if rec.ActualCost.State() != usage.CostUnknown {
		t.Errorf("cost completeness = %q, want unknown -- never a zero", rec.ActualCost.State())
	}
	assertNoContent(t, rec)
}

// 4. Context cancellation mid-stream: exactly one terminal action, no accounting
// loss, and no goroutine left behind.
func TestStreamContextCancellationTakesOneTerminalAction(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs)
	h.reader.emit(normalStream(1000, 500)...)

	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())

	s, err := h.client.ConverseStream(ctx, bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-cancel", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		cancel()
		t.Fatalf("ConverseStream: %v", err)
	}

	<-s.Events()
	cancel()

	// The caller's read loop still terminates rather than hanging.
	for range s.Events() {
	}
	if err := s.Close(); !errors.Is(err, bedrock.ErrOutcomeUnknown) {
		t.Fatalf("Close err = %v, want ErrOutcomeUnknown", err)
	}

	tot := h.totals(t)
	if tot.Reserved == 0 {
		t.Error("a cancelled stream must leave its hold outstanding: the model may have kept generating")
	}

	rec := getRecord(t, acts, "stream-cancel")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}
	if rec.Outcome != activity.OutcomeCancelled {
		t.Errorf("outcome = %q, want cancelled", rec.Outcome)
	}
	// The record survived cancellation: bookkeeping runs on a detached context.
	if rec.CompletedAt.IsZero() {
		t.Error("the activity record must be completed even though the caller's context was cancelled")
	}

	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
}

// A deadline rather than a cancel is recorded as a timeout, because "the caller
// gave up" and "the caller ran out of time" are different operational stories.
func TestStreamTimeoutRecordsTimeout(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs)
	// A stream that never sends anything and never ends: only the deadline can end it.
	h.reader.hang()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	s, err := h.client.ConverseStream(ctx, bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-timeout", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)
	_ = s.Close()

	rec := getRecord(t, acts, "stream-timeout")
	if rec.Outcome != activity.OutcomeTimeout {
		t.Errorf("outcome = %q, want timeout", rec.Outcome)
	}
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}
}

// 5. A provider stream error before any usage was reported. This is not evidence
// that nothing was generated, so the conservative outcome is a retained liability
// rather than a release.
func TestStreamErrorBeforeMetadataRetainsTheHold(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs)
	h.reader.emitThenFail(errStream, msgStart(), delta("partial"))

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-broke", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)

	closeErr := s.Close()
	if !errors.Is(closeErr, bedrock.ErrOutcomeUnknown) {
		t.Fatalf("Close err = %v, want ErrOutcomeUnknown", closeErr)
	}
	if !strings.Contains(closeErr.Error(), "model stream failed") {
		t.Errorf("the terminal error must carry the provider's own error: %v", closeErr)
	}

	tot := h.totals(t)
	if tot.Reserved == 0 {
		t.Fatal("a broken stream must not release the hold: a stream error is not proof of zero spend")
	}

	rec := getRecord(t, acts, "stream-broke")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want provider-error", rec.Outcome)
	}
	if rec.ActualCost.State() != usage.CostUnknown {
		t.Errorf("cost = %q, want unknown", rec.ActualCost.State())
	}
}

// The other stream-error state, and a genuinely different one: the API call itself
// failed, so no stream ever existed. Nothing was generated, so the hold goes back.
func TestStreamCallFailureReleasesTheHold(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs)
	h.stream.err = errors.New("ValidationException: bad request")

	_, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-refused", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, bedrock.ErrProvider) {
		t.Fatalf("err = %v, want ErrProvider", err)
	}

	tot := h.totals(t)
	if tot.Reserved != 0 {
		t.Errorf("reserved = %s, want the hold released: no stream was ever established", tot.Reserved)
	}

	rec := getRecord(t, acts, "stream-refused")
	if rec.Status != activity.StatusReleased {
		t.Errorf("status = %q, want released", rec.Status)
	}
	if !rec.ActualCost.Known() || rec.ActualCost.Amount != 0 {
		t.Errorf("cost = %s, want a KNOWN zero: nothing was generated", rec.ActualCost)
	}
}

// 6. A stream error after the authoritative usage arrived. The usage is real spend
// and is charged, and the caller hears about both.
func TestStreamErrorAfterMetadataStillSettles(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs)
	h.reader.emitThenFail(errStream, msgStart(), delta("hello"), metadata(1000, 500))

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-late-error", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)

	closeErr := s.Close()
	if !errors.Is(closeErr, bedrock.ErrProvider) {
		t.Fatalf("Close err = %v, want ErrProvider", closeErr)
	}

	res := s.Result()
	if !res.Settled {
		t.Fatal("usage reported before a stream error is authoritative and must be charged")
	}
	want := dollars(t, "0.0105")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if h.totals(t).Spent != want {
		t.Errorf("spent = %s, want %s", h.totals(t).Spent, want)
	}

	rec := getRecord(t, acts, "stream-late-error")
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want settled", rec.Status)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("outcome = %q, want provider-error alongside the settled usage", rec.Outcome)
	}
}

// 7. The provider bills a dimension the captured quote has no rate for. Streaming
// reuses the unresolved-liability semantics rather than inventing its own.
func TestStreamUnpriceableDimensionLeavesAnUnresolvedLiability(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	cat := noCacheCatalog(t)
	h := newStreamHarness(t, "1000", withActs, func(c *bedrock.Config) { c.Catalog = cat })

	h.reader.emit(msgStart(), delta("hello"), metadataUsage(&brtypes.TokenUsage{
		InputTokens:           aws.Int32(1000),
		OutputTokens:          aws.Int32(500),
		CacheReadInputTokens:  aws.Int32(200),
		CacheWriteInputTokens: aws.Int32(100),
	}))

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-unpriced", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)

	closeErr := s.Close()
	if !errors.Is(closeErr, bedrock.ErrCostUnresolved) {
		t.Fatalf("Close err = %v, want ErrCostUnresolved", closeErr)
	}

	res := s.Result()
	if !res.Unresolved {
		t.Fatal("a stream billed for an unpriceable dimension must be unresolved")
	}
	if res.Cost.State() != usage.CostPartial {
		t.Errorf("cost = %q, want partial: input and output were priced, the cache dimensions were not", res.Cost.State())
	}

	// The hold stays encumbered: the money was spent, so it must not be offered to
	// the next caller.
	tot := h.totals(t)
	if tot.Reserved == 0 {
		t.Error("an unresolved liability must keep consuming reserved headroom")
	}

	rec := getRecord(t, acts, "stream-unpriced")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want unresolved", rec.Status)
	}
	if len(rec.ActualCost.Unpriced) != 2 {
		t.Errorf("unpriced = %v, want both cache dimensions named", rec.ActualCost.Unpriced)
	}
	// The usage itself is preserved in full, which is what makes reconciliation
	// possible later.
	if got, ok := rec.ActualUsage.Get(usage.CacheReadTokens); !ok || got != 200 {
		t.Errorf("cache reads = %d (present %v), want 200 preserved verbatim", got, ok)
	}
}

// 8. The provider serves the request on a service tier the caller did not ask for.
// The alternate captured at admission prices it, and the live catalog is never
// consulted -- proven by making the catalog fail loudly if it is.
func TestStreamAlternateTierUsesTheCapturedQuote(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")

	prov := pricing.Provenance{Source: "test", Version: "1"}
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider: "aws-bedrock", ProviderModelID: sonnetID, Provenance: prov,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "15.00")),
			},
		},
		// A flex tier priced at a tenth of standard, captured as an alternate.
		pricing.Price{
			AccessProvider: "aws-bedrock", ProviderModelID: sonnetID, ServiceTier: "flex", Provenance: prov,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "0.30")),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "1.50")),
			},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	h := newStreamHarness(t, "1000", withActs, func(c *bedrock.Config) { c.Catalog = cat })
	h.reader.emit(msgStart(), delta("hello"), metadataTier(1000, 500, "flex"))

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-tier", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)

	// While the stream is in flight, an override makes flex outrageously expensive.
	// Settlement replays the alternate captured at admission, so this must not reach
	// the charge; if it does, the catalog was consulted at settlement.
	if err := cat.Override(pricing.Price{
		AccessProvider: "aws-bedrock", ProviderModelID: sonnetID, ServiceTier: "flex",
		Provenance: pricing.Provenance{Source: "local-override", Version: "2"},
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "900.00")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "900.00")),
		},
	}); err != nil {
		t.Fatalf("Override: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res := s.Result()
	if !res.Settled {
		t.Fatalf("the stream should have settled: %v", s.Err())
	}
	if res.Identity.ServiceTier != "flex" {
		t.Errorf("ServiceTier = %q, want flex: the tier that served the call is authoritative", res.Identity.ServiceTier)
	}
	// 1000 at $0.30/M plus 500 at $1.50/M = $0.0003 + $0.00075.
	want := dollars(t, "0.00105")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s from the captured flex alternate", res.Charge.ActualCost, want)
	}

	rec := getRecord(t, acts, "stream-tier")
	if rec.Quote.Provenance.Version != "1" {
		t.Errorf("quote version = %q, want the version captured at admission", rec.Quote.Provenance.Version)
	}
}

// 9. Concurrent Close from many goroutines: no double-settle, no double-release,
// no panic, no corruption. Every caller sees the same outcome.
func TestConcurrentCloseIsSafe(t *testing.T) {
	h := newStreamHarness(t, "1000")
	h.reader.emit(normalStream(1000, 500)...)

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-concurrent-close", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}

	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			errs[i] = s.Close()
		}()
	}
	// A concurrent reader too, so Close races an in-flight forward.
	go func() {
		for range s.Events() {
		}
	}()
	wg.Wait()

	// Every caller sees the same outcome, not a different story depending on who won
	// the race. There is one terminal error value, so this is identity, not equality.
	for i := 1; i < n; i++ {
		if errs[i] != errs[0] {
			t.Fatalf("Close returned inconsistent results: %v vs %v", errs[0], errs[i])
		}
	}
	// The underlying stream is closed exactly once, whatever the caller does.
	if got := h.reader.closeCount(); got != 1 {
		t.Errorf("the underlying stream was closed %d times, want exactly 1", got)
	}
	// Exactly one terminal action against the ledger.
	tot := h.totals(t)
	if tot.PendingCount+tot.ExpiredCount > 1 {
		t.Errorf("%d holds outstanding, want at most the one this stream made", tot.PendingCount+tot.ExpiredCount)
	}
}

// 10. A deliberately slow consumer: throttle applies backpressure rather than
// buffering, and the accounting metadata still arrives intact.
func TestSlowConsumerGetsBackpressureNotBuffering(t *testing.T) {
	h := newStreamHarness(t, "1000")

	// Far more events than any reasonable buffer would hold.
	const deltas = 500
	events := []brtypes.ConverseStreamOutput{msgStart()}
	for i := range deltas {
		events = append(events, delta(fmt.Sprintf("chunk-%d", i)))
	}
	events = append(events, metadata(1000, 500))

	// sent counts what the provider has handed over; a buffering implementation
	// would run far ahead of the consumer.
	sent := make(chan struct{}, len(events)+1)
	go func() {
		defer close(h.reader.events)
		for _, ev := range events {
			select {
			case h.reader.events <- ev:
				sent <- struct{}{}
			case <-h.reader.done:
				return
			}
		}
	}()

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-slow-consumer", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}

	received := 0
	for range s.Events() {
		received++
		// The provider cannot be more than a couple of events ahead of the consumer:
		// one in throttle's hand, one blocked on the unbuffered send.
		if ahead := len(sent) - received; ahead > 2 {
			t.Fatalf("the provider ran %d events ahead of the consumer: throttle is buffering", ahead)
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if received != len(events) {
		t.Errorf("received %d events, want all %d", received, len(events))
	}

	// The whole point: a slow consumer must not cost throttle the usage.
	res := s.Result()
	if !res.Settled {
		t.Fatalf("a slow consumer must not lose the accounting metadata: %v", s.Err())
	}
	if res.Usage.Count(usage.OutputTokens) != 500 {
		t.Errorf("output tokens = %d, want 500", res.Usage.Count(usage.OutputTokens))
	}
}

// 11. Complete consumption then Close: the settlement already happened when the
// stream drained, and Close must not settle a second time.
func TestCloseAfterCompletionDoesNotSettleTwice(t *testing.T) {
	h := newStreamHarness(t, "1000")
	h.reader.emit(normalStream(1000, 500)...)

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-close-after", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)

	want := dollars(t, "0.0105")
	for i := range 3 {
		if err := s.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
		if got := h.totals(t).Spent; got != want {
			t.Fatalf("after %d closes spent = %s, want %s charged exactly once", i+1, got, want)
		}
	}

	charges, err := h.charges(t)
	if err != nil {
		t.Fatalf("Charges: %v", err)
	}
	if len(charges) != 1 {
		t.Errorf("%d charges recorded, want exactly 1", len(charges))
	}
}

// 12. A caller that abandons the stream without closing it and without cancelling
// its context. throttle must not renew a hold forever or pin a goroutine.
func TestAbandonedStreamStopsRenewingAndExits(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs, func(c *bedrock.Config) {
		c.StreamStallTimeout = 50 * time.Millisecond
	})
	// A stream with plenty left to send, so the caller's silence is what ends it.
	h.reader.emit(normalStream(1000, 500)...)

	before := runtime.NumGoroutine()

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-abandoned", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}

	// One read, then abandon: no Close, no cancel, no further reads.
	<-s.Events()

	// The stream must reach a terminal state on its own.
	waitFor(t, func() bool { return s.Result() != nil })

	res := s.Result()
	if res.Settled {
		t.Error("an abandoned stream cannot have settled: usage was never reported")
	}
	// Still not released: abandonment says nothing about provider spend.
	if h.totals(t).Reserved == 0 {
		t.Error("an abandoned stream must leave its hold outstanding")
	}

	rec := getRecord(t, acts, "stream-abandoned")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}

	// No immortal keepalive, and no pinned pump.
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })

	r, err := h.ledger.Get(context.Background(), res.ReservationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	renewals := r.RenewCount
	time.Sleep(150 * time.Millisecond)
	r, err = h.ledger.Get(context.Background(), res.ReservationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if r.RenewCount != renewals {
		t.Errorf("the hold renewed %d more times after the stream ended: the keepalive is immortal",
			r.RenewCount-renewals)
	}
}

// 13. Activity persistence: streaming telemetry survives a restart, and streaming
// did not create a content-retention path.
func TestStreamActivitySurvivesRestartAndHoldsNoContent(t *testing.T) {
	path := t.TempDir() + "/activity.db"
	acts, withActs := withActivity(t, path)
	// A real clock, because the durations are the point of this test: a frozen one
	// would record zeros and prove nothing about what survives the round trip.
	h := newStreamHarnessWithLease(t, "1000", time.Minute, withActs)
	h.reader.pace = time.Millisecond
	h.reader.emit(normalStream(1000, 500)...)

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-durable", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := acts.Close(); err != nil {
		t.Fatalf("Close store: %v", err)
	}

	// A different process reading the same file.
	reopened, err := activitysqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	rec, err := reopened.Get(context.Background(), "stream-durable")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Status != activity.StatusSettled {
		t.Errorf("status = %q, want settled after a restart", rec.Status)
	}
	if rec.Identity.Operation != bedrock.OperationConverseStream {
		t.Errorf("operation = %q, want converse-stream so a stranded record is identifiable", rec.Identity.Operation)
	}
	if rec.StreamFirstEvent <= 0 {
		t.Error("StreamFirstEvent must survive the round trip, so time-to-first-token is derivable")
	}
	if rec.StreamEstablished < 0 {
		t.Errorf("StreamEstablished = %s, want a non-negative duration", rec.StreamEstablished)
	}
	if rec.Latency <= 0 {
		t.Error("total stream duration must be recorded")
	}
	assertNoContent(t, rec)

	// The guarantee is about what the file CAN hold, not what this writer happened
	// to write. A streaming path that added a text column would fail here even if it
	// left the column empty today.
	assertSchemaHoldsNoContent(t, reopened)
}

// The crash window: a stream interrupted before it resolves must leave enough
// durable state for #18's reconciliation to classify it. Not solved here -- only
// not made worse.
func TestInterruptedStreamLeavesReconcilableState(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs)
	// A stream that never completes, standing in for a process that dies mid-stream.
	h.reader.emit(msgStart(), delta("partial"))

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-crashy", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}

	// Read the pre-call record while the stream is still live: this is exactly what a
	// crash would leave behind.
	<-s.Events()
	rec := getRecord(t, acts, "stream-crashy")

	if rec.Status != activity.StatusPending {
		t.Errorf("status = %q, want pending: a live stream's record is what a crash strands", rec.Status)
	}
	if rec.Identity.Operation != bedrock.OperationConverseStream {
		t.Error("a stranded record must identify itself as a stream")
	}
	if rec.ReservationID == "" {
		t.Error("a stranded record must name its reservation, or the hold cannot be matched to it")
	}
	if len(rec.Scopes) == 0 {
		t.Error("a stranded record must name the scopes it encumbered")
	}
	if rec.ActualUsage.Count(usage.OutputTokens) != 0 {
		t.Error("a pending record must not claim usage: authoritative usage is observable by its absence")
	}
	if rec.ActualCost.Known() {
		t.Error("a pending record's cost must not read as known")
	}

	drain(t, s)
	_ = s.Close()
}

// 14. Hierarchical concurrency: many streams against a child cannot oversubscribe
// the parent's envelope, even while all of them are mid-stream.
func TestConcurrentStreamsCannotOversubscribeAnAncestor(t *testing.T) {
	// The parent allows $1.00; the child claims $10.00 it cannot actually have. Each
	// stream reserves 1000 in + 2000 out = $0.033, so the parent admits 30.
	h := newHierarchicalStreamHarness(t, "1.00", "10.00")

	const attempts = 60
	var (
		mu       sync.Mutex
		admitted int
		denied   int
		streams  []*bedrock.Stream
	)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
				BudgetID:  "child",
				RequestID: fmt.Sprintf("stream-conc-%d", i),
				Input:     streamRequest(sonnetID, aws.Int32(2000)),
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				denied++
				return
			}
			admitted++
			streams = append(streams, s)
		}()
	}
	wg.Wait()

	if denied == 0 {
		t.Fatal("the parent's ceiling must refuse some streams, or this test proves nothing")
	}

	// While every admitted stream is alive, the parent's encumbrance must not exceed
	// its allocation.
	tot := h.scopeTotals(t, "team")
	if tot.Reserved > dollars(t, "1.00") {
		t.Errorf("the parent holds %s against a $1.00 allocation: concurrent streams oversubscribed it", tot.Reserved)
	}
	perStream := dollars(t, "0.033")
	if max := int(dollars(t, "1.00") / perStream); admitted > max {
		t.Errorf("%d streams admitted, want at most %d within the parent's envelope", admitted, max)
	}

	for _, s := range streams {
		drain(t, s)
		_ = s.Close()
	}
}

// A client without a streaming client refuses streaming rather than pretending.
func TestConverseStreamRequiresAStreamClient(t *testing.T) {
	h := newHarness(t, "1000")
	if _, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", Input: streamRequest(sonnetID, nil),
	}); !errors.Is(err, bedrock.ErrNoStreamClient) {
		t.Errorf("err = %v, want ErrNoStreamClient", err)
	}
}

// A stream is estimated exactly as the equivalent non-streaming request is, so the
// two halves of the adapter cannot reserve different amounts for the same work.
func TestStreamEstimateMatchesConverseEstimate(t *testing.T) {
	h := newStreamHarness(t, "1000")

	converse, err := h.client.Estimate(context.Background(), request(sonnetID, aws.Int32(2000)))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	stream, err := h.client.EstimateStream(context.Background(), streamRequest(sonnetID, aws.Int32(2000)))
	if err != nil {
		t.Fatalf("EstimateStream: %v", err)
	}

	if converse.Cost.Amount != stream.Cost.Amount {
		t.Errorf("stream estimate %s != converse estimate %s for the same request",
			stream.Cost, converse.Cost)
	}
	if converse.Quality != stream.Quality {
		t.Errorf("quality %q != %q", stream.Quality, converse.Quality)
	}
	// Identity differs in exactly one field, and deliberately.
	if stream.Identity.Operation != bedrock.OperationConverseStream {
		t.Errorf("Operation = %q, want converse-stream", stream.Identity.Operation)
	}
	converse.Identity.Operation = stream.Identity.Operation
	if converse.Identity != stream.Identity {
		t.Errorf("identity differs beyond the operation:\n converse %+v\n stream   %+v",
			converse.Identity, stream.Identity)
	}
}

// A malformed sequence -- no metadata event at all, despite Bedrock marking usage
// required on it -- must not read as a free request.
func TestStreamWithoutMetadataIsNotFree(t *testing.T) {
	acts, withActs := withActivity(t, t.TempDir()+"/activity.db")
	h := newStreamHarness(t, "1000", withActs)
	// Ends cleanly, but says nothing about usage.
	h.reader.emit(msgStart(), blockStart(0), delta("hello"), blockStop(0), msgStop())

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-no-meta", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)

	if err := s.Close(); !errors.Is(err, bedrock.ErrAccounting) {
		t.Fatalf("Close err = %v, want ErrAccounting", err)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("a stream that reported no usage must leave its hold outstanding, not release it")
	}
	rec := getRecord(t, acts, "stream-no-meta")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("status = %q, want outstanding", rec.Status)
	}
	if rec.ActualCost.State() != usage.CostUnknown {
		t.Errorf("cost = %q, want unknown rather than a zero", rec.ActualCost.State())
	}
}

// A success that hands back no stream at all is the same accounting state as a
// response with no usage: unresolvable, so the hold stays.
func TestStreamWithNoEventStreamLeavesTheHold(t *testing.T) {
	h := newStreamHarness(t, "1000")
	h.stream.useRaw = true
	h.stream.stream = nil

	_, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-nil", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, bedrock.ErrAccounting) {
		t.Fatalf("err = %v, want ErrAccounting", err)
	}
	if h.totals(t).Reserved == 0 {
		t.Error("the hold must stay: throttle cannot tell whether the model ran")
	}
}

// Under enforcement, a stream throttle cannot price is refused before the provider
// is called -- the same rule Converse follows, for the same reason.
func TestEnforceRejectsUnpriceableStreamBeforeCalling(t *testing.T) {
	h := newStreamHarness(t, "1000")

	_, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID:  "team",
		RequestID: "stream-unknown-model",
		Input:     streamRequest("acme.brand-new-model-v9", aws.Int32(2000)),
	})
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("err = %v, want ErrCostUnknown", err)
	}
	if h.stream.callCount() != 0 {
		t.Error("the provider must not be called for a request enforcement will refuse")
	}
}

// The input reaches Bedrock unchanged: throttle is a shim, not a rewriter.
func TestStreamPassesTheInputThrough(t *testing.T) {
	h := newStreamHarness(t, "1000")
	h.reader.emit(normalStream(1000, 500)...)

	in := streamRequest(sonnetID, aws.Int32(2000))
	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-passthrough", Input: in,
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)
	_ = s.Close()

	if got := h.stream.lastInput(); got != in {
		t.Error("the SDK request must be passed to Bedrock verbatim")
	}
}

// A stream works without an activity store: telemetry is optional, governance is
// not.
func TestStreamWorksWithoutAnActivityStore(t *testing.T) {
	h := newStreamHarness(t, "1000")
	h.reader.emit(normalStream(1000, 500)...)

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-no-store", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.Result().Settled {
		t.Error("the stream must settle with no activity store configured")
	}
}

// An activity store that fails every write must not fail a stream. The caller has
// already paid for the response.
func TestStreamActivityFailureDoesNotFailTheStream(t *testing.T) {
	h := newStreamHarness(t, "1000", func(c *bedrock.Config) { c.Activity = brokenStore{} })
	h.reader.emit(normalStream(1000, 500)...)

	s, err := h.client.ConverseStream(context.Background(), bedrock.StreamRequest{
		BudgetID: "team", RequestID: "stream-broken-store", Input: streamRequest(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	drain(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !s.Result().Settled {
		t.Error("a telemetry failure must not prevent settlement")
	}
}

// --- harness helpers specific to streaming ---

// newStreamHarnessWithLease builds a child-of-parent hierarchy on a real clock with
// the given lease, so a test can outlive a lease quantum in milliseconds rather than
// minutes, and so recorded durations are real rather than frozen.
func newStreamHarnessWithLease(t *testing.T, allocation string, lease time.Duration, opts ...func(*bedrock.Config)) *streamHarness {
	t.Helper()

	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	// A real clock: the lease is being exercised against wall-clock expiry.
	clock := func() time.Time { return time.Now().UTC() }

	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock, Lease: lease})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	anchor := clock().Truncate(24 * time.Hour)
	for _, def := range []budget.Definition{
		{ID: "team", Allocation: dollars(t, allocation), Recurrence: budget.RecurMonthly, AnchorAt: anchor},
		{ID: "child", ParentID: "team", Allocation: dollars(t, allocation), Recurrence: budget.RecurMonthly, AnchorAt: anchor},
	} {
		if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", def.ID, err)
		}
	}

	reader := newFakeReader()
	api := &fakeStreamAPI{reader: reader}
	h := buildHarness(t, eng, store, clock, append([]func(*bedrock.Config){withStream(api)}, opts...)...)
	return &streamHarness{harness: h, stream: api, reader: reader}
}

// newHierarchicalStreamHarness builds a child whose own allocation exceeds its
// parent's, so the parent's ceiling is the only thing that can refuse a request.
func newHierarchicalStreamHarness(t *testing.T, parent, child string) *streamHarness {
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
	anchor := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, def := range []budget.Definition{
		{ID: "team", Allocation: dollars(t, parent), Recurrence: budget.RecurMonthly, AnchorAt: anchor},
		{ID: "child", ParentID: "team", Allocation: dollars(t, child), Recurrence: budget.RecurMonthly, AnchorAt: anchor},
	} {
		if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", def.ID, err)
		}
	}

	reader := newFakeReader()
	api := &fakeStreamAPI{reader: reader}
	// Every stream shares one reader here; the tests using this harness only care
	// about admission, and drain whatever they get.
	reader.emit(normalStream(1000, 500)...)
	h := buildHarness(t, eng, store, clock, withStream(api))
	return &streamHarness{harness: h, stream: api, reader: reader}
}

// scopeTotals reports one budget's committed position.
func (h *harness) scopeTotals(t *testing.T, budgetID string) ledger.Totals {
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

// charges lists the charges recorded against the default budget.
func (h *harness) charges(t *testing.T) ([]ledger.Charge, error) {
	t.Helper()
	p, err := h.ledger.EnsurePeriod(context.Background(), "team", h.clock())
	if err != nil {
		return nil, err
	}
	return h.ledger.Charges(context.Background(),
		ledger.Scope{BudgetID: "team", PeriodID: p.ID},
		time.Time{}, time.Time{}, 0)
}

// assertSchemaHoldsNoContent proves the activity schema has no column that could
// hold prompt or generated content. It checks the schema rather than the writer,
// because the guarantee is about what the file can hold at all.
func assertSchemaHoldsNoContent(t *testing.T, s *activitysqlite.Store) {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT name FROM pragma_table_info('activity')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()

	banned := []string{"prompt", "message", "content", "text", "response", "completion", "output", "delta", "transcript"}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		for _, b := range banned {
			if strings.Contains(name, b) {
				t.Errorf("column %q could hold request or response content; activity is content-free by construction", name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}
