package report

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"throttle/ledger"
)

// A dashboard reads a live ledger while requests are being admitted and settled
// against it. Under -race this pins two things at once: that the read model holds no
// shared mutable state, and that concurrent readers do not deadlock or trip SQLite's
// locking against a writer in another goroutine.
func TestDashboardReadsWhileTheLedgerIsBeingWritten(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(10_000)))

	const (
		writes  = 60
		readers = 6
	)

	ctx, cancel := context.WithCancel(w.ctx)
	var wg sync.WaitGroup

	// One writer, spending real money through the ordinary reserve/settle path.
	writeErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		for i := 0; i < writes; i++ {
			// Within the last hour, so the holds that are left in flight are still
			// inside their lease when a reader looks at them.
			at := w.now.Add(-time.Duration(writes-i) * time.Minute)
			rv, err := w.led.Reserve(ctx, ledger.ReserveRequest{
				Reservation: ledger.Reservation{
					ID: fmt.Sprintf("res-%d", i), BudgetID: "research",
					RequestID: fmt.Sprintf("req-%d", i),
					Amount:    cents(50), EstimatedCost: cents(50),
					CreatedAt: at, ExpiresAt: at.Add(time.Hour), Lease: time.Hour,
					Identity: bedrockIdentity(),
				},
				Ceilings: unlimited("research"),
				Now:      at,
			})
			if err != nil {
				writeErr <- fmt.Errorf("reserve %d: %w", i, err)
				return
			}
			// Every third request stays in flight, so a reader always has some
			// encumbrance to observe alongside settled spend.
			if i%3 == 0 {
				continue
			}
			if _, err := w.led.Settle(ctx, ledger.Settlement{
				ReservationID: rv.ID, ActualCost: cents(50), CompletedAt: at.Add(time.Second),
			}); err != nil {
				writeErr <- fmt.Errorf("settle %d: %w", i, err)
				return
			}
			if err := w.acts.Complete(ctx, settledRecord(
				fmt.Sprintf("req-%d", i), "research", p.ID, cents(50), at)); err != nil {
				writeErr <- fmt.Errorf("record %d: %w", i, err)
				return
			}
		}
	}()

	// Several concurrent readers, each doing what one page load does.
	readErr := make(chan error, readers)
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				if err := onePageLoad(ctx, w.rep, "research", p.ID); err != nil {
					// Cancelling the context mid-query surfaces as whatever the driver
					// calls an interrupted statement, which is orderly shutdown rather
					// than a concurrency failure.
					if ctx.Err() != nil {
						return
					}
					readErr <- err
					return
				}
			}
		}()
	}

	wg.Wait()
	close(writeErr)
	close(readErr)

	for err := range writeErr {
		t.Errorf("writer: %v", err)
	}
	for err := range readErr {
		t.Errorf("reader: %v", err)
	}

	// And the figures still add up afterwards: 60 reservations, 40 settled at 50c.
	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if want := cents(50) * 40; sum.Position.Spent != want {
		t.Errorf("Spent = %s, want %s", sum.Position.Spent, want)
	}
	if want := cents(50) * 20; sum.Position.Reserved != want {
		t.Errorf("Reserved = %s, want %s still encumbered", sum.Position.Reserved, want)
	}
	if sum.Position.Spent+sum.Position.Reserved != sum.Position.Committed {
		t.Errorf("Committed = %s, want spent plus reserved", sum.Position.Committed)
	}
}

// onePageLoad performs every read a dashboard page performs, so a reader goroutine
// exercises the same query set the HTTP layer will.
func onePageLoad(ctx context.Context, rep *Reporter, budgetID, periodID string) error {
	if _, err := rep.Summary(ctx, budgetID); err != nil {
		return ignoreCancelled(fmt.Errorf("summary: %w", err))
	}
	if _, err := rep.Tree(ctx); err != nil {
		return ignoreCancelled(fmt.Errorf("tree: %w", err))
	}
	if _, err := rep.Timeline(ctx, budgetID, periodID); err != nil {
		return ignoreCancelled(fmt.Errorf("timeline: %w", err))
	}
	if _, err := rep.Activity(ctx, ActivityQuery{BudgetID: budgetID, Limit: 25}); err != nil {
		return ignoreCancelled(fmt.Errorf("activity: %w", err))
	}
	if _, err := rep.Breakdowns(ctx, Facets, ActivityQuery{BudgetID: budgetID}); err != nil {
		return ignoreCancelled(fmt.Errorf("breakdowns: %w", err))
	}
	if _, err := rep.Reservations(ctx, budgetID, 0); err != nil {
		return ignoreCancelled(fmt.Errorf("reservations: %w", err))
	}
	if _, err := rep.Unresolved(ctx, ActivityQuery{BudgetID: budgetID}); err != nil {
		return ignoreCancelled(fmt.Errorf("unresolved: %w", err))
	}
	if _, err := rep.Periods(ctx, budgetID); err != nil {
		return ignoreCancelled(fmt.Errorf("periods: %w", err))
	}
	return nil
}

// ignoreCancelled drops the error a reader gets from the writer finishing and
// cancelling the context, which is orderly shutdown rather than a failure.
func ignoreCancelled(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}

// Two reporters over the same ledger file must agree, because neither caches monetary
// truth. This is the multi-process property: a dashboard in one process must not show a
// figure a governor in another has moved past.
func TestSeparateReportersOverOneLedgerAgree(t *testing.T) {
	dir := t.TempDir()
	w := newWorldIn(t, dir, true)
	w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(100), w.now)

	// A second reporter opening the same files, as a second process would.
	other := newWorldIn(t, dir, true)
	other.now = w.now

	first, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	second, err := other.rep.Summary(other.ctx, "research")
	if err != nil {
		t.Fatalf("Summary from the second reporter: %v", err)
	}
	if first.Position.Spent != second.Position.Spent {
		t.Fatalf("two reporters disagree: %s vs %s", first.Position.Spent, second.Position.Spent)
	}

	// Money moves through the first. The second must see it on its next read, because
	// it holds no cached copy.
	w.spend("s2", "research", dollars(50), w.now)

	after, err := other.rep.Summary(other.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if after.Position.Spent != dollars(150) {
		t.Errorf("the second reporter still reads %s after the first spent to %s; "+
			"monetary truth was cached in a reader", after.Position.Spent, dollars(150))
	}
}
