package openai_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/money"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/reconcile"
)

// Stress tests for the five interleavings that decide whether the streaming lifecycle
// is boring: Close against the terminal event, cancellation against the terminal
// event, lease renewal against the terminal event, the idle bound against an entry to
// Next, and reconciliation against a settlement that has just become durable.
//
// # How these are made deterministic
//
// Not by sleeping and hoping. Each test builds the collision it is about out of
// synchronization, then repeats it: the fake stream can park a reader on a chosen
// event, and releasing that event and firing the competing operation from two
// goroutines that start together puts the two paths in the same window on every
// iteration. Repetition explores the orderings the scheduler happens to pick, and
// -race watches for the memory races an unsynchronized version would have.
//
// What is asserted is never "this happened within N milliseconds". It is that whatever
// order the race resolved in, the invariant survives: exactly one terminal accounting
// action, exactly one charge, no released hold without authoritative usage, no
// surviving goroutine, no immortal lease. Both outcomes of each race are legitimate --
// what would be a bug is a third one.
//
// The repeat counts are small enough to keep the suite quick; the interesting knob is
// -count, which the brief asks these to be run under.
const stressRounds = 40

// raceStart returns a starter that releases n goroutines simultaneously.
//
// Two goroutines that merely start at the same time in program order do not collide
// often enough to be worth running: the first is typically finished before the second
// is scheduled. Parking both on one channel makes the collision the common case
// instead of the rare one.
func raceStart(n int) (start func(), wait func(), gate chan struct{}, wg *sync.WaitGroup) {
	gate = make(chan struct{})
	wg = &sync.WaitGroup{}
	wg.Add(n)
	return func() { close(gate) }, wg.Wait, gate, wg
}

// chargeCount reports how many charges a budget has recorded. One per settled request
// is the invariant every one of these tests is ultimately checking.
func chargeCount(t *testing.T, h *streamHarness, budgetID string) int {
	t.Helper()
	p, err := h.ledger.EnsurePeriod(context.Background(), budgetID, h.clock())
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	charges, err := h.ledger.Charges(context.Background(),
		ledger.Scope{BudgetID: budgetID, PeriodID: p.ID}, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("Charges: %v", err)
	}
	return len(charges)
}

// assertOneTerminalOutcome checks the invariants that hold however a race resolved.
//
// Deliberately indifferent to which side won. A stream that settled and a stream whose
// hold stayed outstanding are both correct outcomes of a Close racing a terminal event;
// what cannot happen is two charges, a released hold with no usage behind it, or a
// record that never reached a terminal status at all.
func assertOneTerminalOutcome(t *testing.T, h *streamHarness, requestID string, s *openai.Stream) {
	t.Helper()

	res := s.Result()
	if res == nil {
		t.Fatal("the stream never reached a terminal state")
	}

	rec := h.record(t, requestID)
	switch rec.Status {
	case activity.StatusSettled:
		// Settled means an authoritative terminal Response was observed, so there must be
		// exactly one charge for exactly the real usage.
		if got := chargeCount(t, h, "team"); got != 1 {
			t.Errorf("status settled with %d charges, want exactly 1", got)
		}
		if got := h.totals(t).Spent; got != dollars(t, "0.00625") {
			t.Errorf("Spent = %s, want $0.00625: a settled stream charged the wrong amount", got)
		}
		if !res.Settled {
			t.Error("the record says settled but the Result does not")
		}
	case activity.StatusOutstanding, activity.StatusUnresolved:
		// The race went the other way: no authoritative usage, so the hold stays and
		// nothing was charged. That is the conservative outcome, not a failure.
		if got := chargeCount(t, h, "team"); got != 0 {
			t.Errorf("status %q with %d charges: nothing may be charged without authoritative usage",
				rec.Status, got)
		}
		if h.totals(t).Reserved == 0 {
			t.Errorf("status %q but the hold was released: local events never prove zero spend",
				rec.Status)
		}
		if res.Cost.Known() {
			t.Errorf("cost %s is known on a %q record", res.Cost.Amount, rec.Status)
		}
	default:
		t.Errorf("status = %q, want settled, outstanding, or unresolved", rec.Status)
	}

	// Whatever happened, it happened once.
	if got := h.up.closeCount(); got > 1 {
		t.Errorf("the provider stream was closed %d times, want 1", got)
	}
}

// 17, 18, 19. Close racing the terminal event.
//
// The caller is parked on the read of the terminal event when Close arrives. The two
// paths meet inside the same sync.Once from opposite directions: one about to settle
// from authoritative usage, one about to record an abandoned stream. Either may win.
// Neither may double-account, and a Close that loses must not undo the settlement.
func TestStressCloseAgainstTerminalEvent(t *testing.T) {
	for round := 0; round < stressRounds; round++ {
		h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))
		requestID := "stress-close"

		// The read of the terminal event (index 3) parks until released.
		h.up.blockFrom(3)

		s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
			BudgetID: "team", RequestID: requestID, Params: request(gpt51, maxOut(2000)),
		})
		if err != nil {
			t.Fatalf("RespondStreaming: %v", err)
		}

		reader := make(chan struct{})
		go func() {
			defer close(reader)
			for s.Next() {
				_ = s.Current()
			}
		}()

		// Wait until the caller is demonstrably parked on the terminal read, so the race
		// is with that event and not with an earlier one.
		waitFor(t, func() bool { return h.up.readCount() >= 4 })

		start, wait, gate, wg := raceStart(2)
		go func() { defer wg.Done(); <-gate; h.up.release() }()
		go func() { defer wg.Done(); <-gate; _ = s.Close() }()
		start()
		wait()
		<-reader

		// A second Close from the losing side's perspective, which must still be free.
		if err := s.Close(); err != nil && !errors.Is(err, openai.ErrOutcomeUnknown) &&
			!errors.Is(err, openai.ErrCostUnresolved) {
			t.Errorf("Close after the race = %v", err)
		}

		assertOneTerminalOutcome(t, h, requestID, s)
	}
}

// 16. Cancellation racing the terminal event.
//
// Harder than the Close race, because the supervisor is a third party: cancelling the
// context wakes it, and it terminalizes from its own goroutine while the reader is
// terminalizing from the caller's. Whichever arrives first, the hold must survive a
// cancellation that wins -- local cancellation is not evidence of zero provider spend
// -- and a settlement that wins must not be undone.
func TestStressCancellationAgainstTerminalEvent(t *testing.T) {
	for round := 0; round < stressRounds; round++ {
		h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))
		requestID := "stress-cancel"

		h.up.blockFrom(3)

		ctx, cancel := context.WithCancel(context.Background())
		s, err := h.client.RespondStreaming(ctx, openai.StreamRequest{
			BudgetID: "team", RequestID: requestID, Params: request(gpt51, maxOut(2000)),
		})
		if err != nil {
			cancel()
			t.Fatalf("RespondStreaming: %v", err)
		}

		reader := make(chan struct{})
		go func() {
			defer close(reader)
			for s.Next() {
				_ = s.Current()
			}
		}()

		waitFor(t, func() bool { return h.up.readCount() >= 4 })

		start, wait, gate, wg := raceStart(2)
		go func() { defer wg.Done(); <-gate; h.up.release() }()
		go func() { defer wg.Done(); <-gate; cancel() }()
		start()
		wait()
		<-reader

		// The terminal state is reached by whoever got there, including the supervisor, so
		// this waits for it rather than assuming the reader produced it.
		waitFor(t, func() bool { return s.Result() != nil })
		_ = s.Close()
		cancel()

		assertOneTerminalOutcome(t, h, requestID, s)

		// The supervisor is gone either way.
		waitFor(t, func() bool { return h.reservationRenewalsStable(t, s.ReservationID()) })
	}
}

// 23, 24. Lease renewal racing the terminal event.
//
// The supervisor renews on a ticker while the reader settles. A renewal that lands
// after the settlement finds the transaction already resolved, which must be handled
// as "nothing left to do" rather than as an error that corrupts the record -- and a
// renewal in flight must not be able to resurrect a hold that settlement consumed.
//
// The lease is short enough that renewals tick throughout, and the terminal read is
// released only once a renewal has demonstrably happened, so the collision is real
// rather than assumed.
func TestStressRenewalAgainstTerminalEvent(t *testing.T) {
	for round := 0; round < stressRounds/4; round++ {
		h := newStreamHarnessWithLease(t, "1000", 30*time.Millisecond,
			normalEventStream(t, gpt51, 1000, 500), generousStall)
		requestID := "stress-renew"

		h.up.blockFrom(3)

		s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
			BudgetID: "team", RequestID: requestID, Params: request(gpt51, maxOut(2000)),
		})
		if err != nil {
			t.Fatalf("RespondStreaming: %v", err)
		}

		reader := make(chan struct{})
		go func() {
			defer close(reader)
			for s.Next() {
				_ = s.Current()
			}
		}()

		// The keepalive is demonstrably running before the terminal event is released, so
		// the settlement lands among renewals rather than after they finished.
		waitFor(t, func() bool { return h.reservation(t, s.ReservationID()).RenewCount > 0 })
		h.up.release()
		<-reader
		waitFor(t, func() bool { return s.Result() != nil })
		_ = s.Close()

		rec := h.record(t, requestID)
		if rec.Status != activity.StatusSettled {
			t.Fatalf("status = %q, want settled: the terminal event was delivered", rec.Status)
		}
		if got := chargeCount(t, h, "team"); got != 1 {
			t.Errorf("recorded %d charges, want exactly 1", got)
		}
		// A renewal racing the settlement is expected and is not an error worth recording:
		// the ledger reports the transaction already resolved, and that is a no-op.
		if rec.Error != "" {
			t.Errorf("record error = %q: a renewal that lost to settlement is not a failure", rec.Error)
		}
		// And no renewal outlived the settlement.
		waitFor(t, func() bool { return h.reservationRenewalsStable(t, s.ReservationID()) })
	}
}

// 21, 22. The idle bound racing an entry to Next.
//
// This is the interleaving the consumer-idle design is most likely to get wrong: the
// watchdog fires exactly as the caller returns to Next. If it samples the caller's
// progress carelessly it will kill a healthy stream, and if it never fires it will let
// an abandoned one renew forever. Both would be silent.
//
// The collision is manufactured by pausing for very close to the whole bound between
// reads, repeatedly, so firings land on both sides of an entry to Next. The assertion
// is not "the stream survived" -- with an idle bound this tight, being judged abandoned
// is a legitimate verdict -- but that whichever verdict was reached is internally
// consistent, and that the stream always reaches exactly one of them.
func TestStressIdleBoundAgainstNextEntry(t *testing.T) {
	const idle = 15 * time.Millisecond

	for round := 0; round < stressRounds/4; round++ {
		events := []responses.ResponseStreamEventUnion{createdEvent(t, gpt51)}
		for i := 1; i <= 6; i++ {
			events = append(events, deltaEvent(t, i, "word "))
		}
		events = append(events, completedEvent(t, gpt51, 1000, 500))

		h := newStreamHarness(t, "1000", events, func(c *openai.Config) { c.StreamStallTimeout = idle })
		requestID := "stress-idle"

		before := runtime.NumGoroutine()

		s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
			BudgetID: "team", RequestID: requestID, Params: request(gpt51, maxOut(2000)),
		})
		if err != nil {
			t.Fatalf("RespondStreaming: %v", err)
		}

		// Read with pauses just under the bound, so a watchdog firing repeatedly lands
		// near an entry to Next rather than in the middle of a long sleep.
		for s.Next() {
			_ = s.Current()
			time.Sleep(idle - idle/8)
		}
		_ = s.Close()

		assertOneTerminalOutcome(t, h, requestID, s)

		// Whichever way it went, the watchdog goroutine is gone and the lease is not
		// being renewed behind it.
		waitFor(t, func() bool { return runtime.NumGoroutine() <= before+1 })
		waitFor(t, func() bool { return h.reservationRenewalsStable(t, s.ReservationID()) })
	}
}

// 33. Reconciliation racing a settlement that has just become durable.
//
// A recovery sweep does not know a request is mid-terminalization; it sees a pending or
// outstanding record and starts repairing it. Running the sweep concurrently with the
// terminal event checks that the two cannot combine into a double charge -- one from
// the adapter settling, one from the reconciler replaying -- and that whichever wrote
// last left a coherent record rather than a torn one.
func TestStressReconciliationAgainstTerminalDurability(t *testing.T) {
	for round := 0; round < stressRounds/4; round++ {
		h := newStreamHarness(t, "1000", normalEventStream(t, gpt51, 1000, 500))
		requestID := "stress-reconcile"

		rec, err := reconcile.New(reconcile.Config{
			Ledger: h.ledger, Activity: h.activity, Clock: h.clock,
		})
		if err != nil {
			t.Fatalf("reconcile.New: %v", err)
		}

		h.up.blockFrom(3)

		s, err := h.client.RespondStreaming(context.Background(), openai.StreamRequest{
			BudgetID: "team", RequestID: requestID, Params: request(gpt51, maxOut(2000)),
		})
		if err != nil {
			t.Fatalf("RespondStreaming: %v", err)
		}

		reader := make(chan struct{})
		go func() {
			defer close(reader)
			for s.Next() {
				_ = s.Current()
			}
		}()

		waitFor(t, func() bool { return h.up.readCount() >= 4 })

		// The sweep and the terminal event start together: the reconciler is reading the
		// durable record in the window where the adapter is writing it.
		start, wait, gate, wg := raceStart(2)
		go func() { defer wg.Done(); <-gate; h.up.release() }()
		go func() {
			defer wg.Done()
			<-gate
			// Errors are not asserted on: a sweep that collides with a live settlement may
			// legitimately fail to reach a verdict this instant. What must hold is that it
			// cannot charge a second time, which is checked below.
			_, _ = rec.ReconcilePending(context.Background())
		}()
		start()
		wait()
		<-reader
		waitFor(t, func() bool { return s.Result() != nil })
		_ = s.Close()

		// A second sweep, now that the stream is definitely terminal, is the shape a real
		// recovery loop takes: it runs again a minute later.
		if _, err := rec.ReconcilePending(context.Background()); err != nil {
			t.Fatalf("second ReconcilePending: %v", err)
		}

		if got := chargeCount(t, h, "team"); got > 1 {
			t.Errorf("recorded %d charges: a reconciliation sweep charged a settled stream again", got)
		}
		after := h.record(t, requestID)
		if after.Status == activity.StatusPending {
			t.Error("the record is still pending after the stream terminalized and two sweeps ran")
		}
		if after.Status == activity.StatusSettled && h.totals(t).Spent != dollars(t, "0.00625") {
			t.Errorf("Spent = %s, want $0.00625 for a settled stream", h.totals(t).Spent)
		}
		if after.Status == activity.StatusReleased {
			t.Error("reconciliation released a stream that reported authoritative usage")
		}
	}
}

// Many streams terminalizing every way at once, to check that the invariants hold
// under contention rather than only in isolation.
//
// The endings are mixed deliberately: settle, close early, cancel, and abandon, all
// against the same budget hierarchy at the same time. What is asserted is the only
// thing that must be true of the aggregate -- the parent's committed plus held money
// never exceeds its allocation, and every stream reached exactly one terminal state.
func TestStressMixedTerminalPathsUnderContention(t *testing.T) {
	const streams = 24

	// A real idle bound, because a quarter of these streams end by abandonment and the
	// bound is the only thing that terminalizes them. Long enough that the streams which
	// read straight through are not racing it.
	h := newHierarchicalStreamHarness(t, "1000", "1000",
		func(c *openai.Config) { c.StreamStallTimeout = 100 * time.Millisecond })
	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	terminal := make([]bool, streams)

	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			s, err := h.client.RespondStreaming(ctx, openai.StreamRequest{
				BudgetID:  "child",
				RequestID: fmt.Sprintf("stress-mixed-%d", i),
				Params:    request(gpt51, maxOut(2000)),
			})
			if err != nil {
				// A denial is a legitimate outcome under contention and needs no stream.
				if errors.Is(err, engine.ErrDenied) {
					terminal[i] = true
				}
				return
			}

			switch i % 4 {
			case 0: // Read to the end and settle.
				drain(t, s)
				_ = s.Close()
			case 1: // Close after one event.
				s.Next()
				_ = s.Close()
			case 2: // Cancel mid-stream.
				s.Next()
				cancel()
			case 3: // Abandon: no more reads, no close.
				s.Next()
			}

			waitFor(t, func() bool { return s.Result() != nil })
			terminal[i] = true
		}(i)
	}
	wg.Wait()

	for i, ok := range terminal {
		if !ok {
			t.Errorf("stream %d never reached a terminal state", i)
		}
	}

	// The parent's money is still coherent: nothing overcommitted, whichever order the
	// settlements, releases, and retentions landed in.
	tot := h.totalsFor(t, "team")
	committed, ok := money.Add(tot.Spent, tot.Reserved)
	if !ok {
		t.Fatal("spent plus reserved overflowed")
	}
	if committed > dollars(t, "1000") {
		t.Errorf("spent plus reserved = %s, which exceeds the parent's allocation", committed)
	}

	// And no stream left a goroutine behind, however it ended.
	waitFor(t, func() bool { return runtime.NumGoroutine() <= before+2 })
}

// reservationRenewalsStable reports whether a hold's renew count has stopped moving.
//
// Sampled twice rather than compared against a deadline, because what is under test is
// that the keepalive stopped, not how promptly. A supervisor still running would move
// the count between the two reads at some point, and waitFor keeps looking until it
// does not.
func (h *harness) reservationRenewalsStable(t *testing.T, id string) bool {
	t.Helper()
	first := h.reservation(t, id).RenewCount
	time.Sleep(5 * time.Millisecond)
	return h.reservation(t, id).RenewCount == first
}
