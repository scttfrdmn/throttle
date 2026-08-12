package sqlite_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/activity/sqlite"
)

// (14) Several handles creating the same database at the same instant must all succeed.
//
// The ledger store has the same test for the same reason: switching a fresh file into WAL mode
// takes an exclusive lock that SQLite does not run the busy handler for, so busy_timeout does
// not cover it and one handle would win while the rest failed at ping.
func TestConcurrentFreshOpenAllSucceed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")

	const handles = 8
	var wg sync.WaitGroup
	for range handles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := sqlite.Open(context.Background(), path)
			if err != nil {
				t.Errorf("Open on a database being created: %v", err)
				return
			}
			t.Cleanup(func() { s.Close() })
		}()
	}
	wg.Wait()
}

// (15) Migrations must be idempotent, and this store's are the ones that could fail to be.
//
// The ledger is CREATE TABLE IF NOT EXISTS throughout, but the activity table gains columns by
// ALTER -- because IF NOT EXISTS does nothing to a table that already exists, so a column added
// to the schema would be present for a fresh database and missing for an existing one. ALTER
// TABLE ADD COLUMN has no IF NOT EXISTS, so the second run is an error that is deliberately
// swallowed. Opening repeatedly is therefore the test that matters: it exercises the ALTERs
// against a database that already has every one of those columns, and a write proves the row
// shape still matches the INSERT.
func TestRepeatedOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "activity.db")

	first := open(t, path)
	if err := first.Complete(ctx, settled(t)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for i := range 3 {
		s, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		// The existing record is still there and still readable.
		got, err := s.Get(ctx, "req-1")
		if err != nil {
			t.Fatalf("Get after open %d: %v", i, err)
		}
		if got.Status != activity.StatusSettled {
			t.Errorf("open %d: status = %q, want settled", i, got.Status)
		}
		// And the table still accepts a write: a column the INSERT names but the
		// table lacks fails here rather than at open.
		rec := settled(t)
		rec.RequestID = "req-after-open"
		if err := s.Complete(ctx, rec); err != nil {
			t.Fatalf("write after open %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}
