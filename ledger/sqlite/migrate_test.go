package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/ledger"
)

// (14) Several handles creating the same database at the same instant must all succeed.
//
// This is the regression test for the SQLITE_BUSY at open: switching a fresh file into WAL
// mode takes an exclusive lock, and SQLite does not run the busy handler for a journal-mode
// change, so busy_timeout does not cover it. Without openRetry this fails reliably -- one
// handle gets the lock and the rest die at ping, which is what a few machines running
// "throttle config apply" against a new ledger at the same moment looks like.
//
// Goroutines rather than processes: the lock is held on the file by whichever connection got
// there first, so the contention is the same and the test stays fast. What is asserted is
// only that opening succeeded; the transactional guarantees have their own tests above.
func TestConcurrentFreshOpenAllSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")

	const handles = 8
	var wg sync.WaitGroup
	for range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(context.Background(), path)
			if err != nil {
				t.Errorf("Open on a database being created: %v", err)
				return
			}
			t.Cleanup(func() { s.Close() })
		}()
	}
	wg.Wait()
}

// (15) Opening the same database repeatedly must change nothing.
//
// The schema is CREATE TABLE IF NOT EXISTS throughout, which is what makes the retry above
// safe -- so a statement that is not idempotent would break restart and concurrent open
// together. Asserted through the data rather than through the DDL: a re-created table would
// be an empty one.
func TestRepeatedOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	first := openAt(t, path)
	mustPut(t, first, monthly("research", "", dollars(1000)))
	if _, err := first.EnsurePeriod(ctx, "research", base); err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for i := range 3 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		defs, err := s.Definitions(ctx)
		if err != nil {
			t.Fatalf("Definitions after open %d: %v", i, err)
		}
		if len(defs) != 1 || defs[0].ID != "research" {
			t.Fatalf("open %d sees %+v, want the one stored definition", i, defs)
		}
		periods, err := s.Periods(ctx, "research")
		if err != nil {
			t.Fatalf("Periods after open %d: %v", i, err)
		}
		if len(periods) != 1 {
			t.Fatalf("open %d sees %d periods, want 1", i, len(periods))
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

// A store opened on an existing database must be able to write to it, not merely read it.
//
// Separate from the reopen tests, which restore and read: this catches a migration that
// leaves an existing database structurally behind a fresh one, where the failure appears at
// the first INSERT rather than at open.
func TestExistingDatabaseStaysWritable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger.db")

	first := openAt(t, path)
	mustPut(t, first, monthly("research", "", dollars(1000)))
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := openAt(t, path)
	mustPut(t, second, monthly("chat", "research", dollars(100)))
	if err := reserve(t, second, "r1", "chat", dollars(5), base, dollars(100), time.Hour); err != nil {
		t.Fatalf("Reserve against an existing database: %v", err)
	}
	if _, err := second.Settle(ctx, ledger.Settlement{
		ReservationID: "r1", ActualCost: dollars(4), CompletedAt: base,
	}); err != nil {
		t.Fatalf("Settle against an existing database: %v", err)
	}

	// Both legs, because the hierarchy is what a half-created database would drop.
	for _, id := range []string{"chat", "research"} {
		if tot := scopeTotals(t, second, id, base); tot.Spent != dollars(4) {
			t.Errorf("%s spent = %s, want %s", id, tot.Spent, dollars(4))
		}
	}
}
