package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/money"
)

// (18) A dashboard reads while requests are being governed. Both stores are SQLite, both
// are written by another process in real use, and a page load must not corrupt a read,
// block a write, or trip the race detector.
//
// The write side here is the real ledger transaction -- reserve, then settle or release --
// because that is what a reader contends with: a reservation's leg set and a charge's leg
// set are each written inside one IMMEDIATE transaction, and a reader that saw half of one
// would report money that does not exist. The activity record is written after the money,
// which is the real ordering and the reason the two stores can legitimately disagree for a
// moment.
func TestConcurrentLedgerWritesWhileDashboardReads(t *testing.T) {
	w := newWorld(t)
	parent := w.define(monthly("research", "", dollars(10_000)))
	child := w.define(monthly("child", "research", dollars(4_000)))

	const (
		writers        = 4
		perWriter      = 12
		readers        = 6
		readsPerReader = 12
		charge         = 25 // cents
	)

	// Every surface a browser touches on a poll, so the contended reads are the real ones
	// rather than one cheap query.
	paths := []string{
		"/?budget=research",
		"/?budget=child",
		"/?budget=research&period=" + parent.ID,
		"/api/summary?budget=research",
		"/api/activity?budget=child",
		"/api/activity?budget=child&unresolved=1",
		"/api/breakdown?budget=child",
		"/api/timeline?budget=research",
	}

	var (
		wg      sync.WaitGroup
		settled atomic.Int64
		reads   atomic.Int64
		once    sync.Once
	)
	stop := make(chan struct{})
	halt := func() { once.Do(func() { close(stop) }) }

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			ctx := context.Background()
			for n := 0; n < perWriter; n++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("w%d-%d", worker, n)
				at := base.Add(time.Duration(worker*perWriter+n) * time.Minute)

				rv, err := w.led.Reserve(ctx, ledger.ReserveRequest{
					Reservation: ledger.Reservation{
						ID: id, BudgetID: "child", RequestID: id,
						Amount: cents(charge), EstimatedCost: cents(charge), CreatedAt: at,
						ExpiresAt: at.Add(time.Hour), Lease: time.Hour,
						Identity: bedrockIdentity(),
					},
					// The real chain: a leaf charge consumes its ancestors, so every write
					// touches two budgets' leg sets and both are readable mid-flight.
					Ceilings: unlimited("child", "research"),
					Now:      at,
				})
				if err != nil {
					t.Errorf("Reserve(%q): %v", id, err)
					halt()
					return
				}

				// Two in three settle and one in three releases, so a reader can encounter a
				// hold about to become a charge and one about to become nothing.
				if n%3 == 2 {
					if err := w.led.Release(ctx, rv.ID); err != nil {
						t.Errorf("Release(%q): %v", id, err)
						halt()
						return
					}
					continue
				}
				if _, err := w.led.Settle(ctx, ledger.Settlement{
					ReservationID: rv.ID, ActualCost: cents(charge),
					CompletedAt: at.Add(time.Second),
				}); err != nil {
					t.Errorf("Settle(%q): %v", id, err)
					halt()
					return
				}
				settled.Add(1)

				rec := settledRecord(id, "child", child.ID, cents(charge), at)
				rec.ReservationID = rv.ID
				rec.Scopes = []activity.Scope{
					{BudgetID: "child", PeriodID: child.ID},
					{BudgetID: "research", PeriodID: parent.ID},
				}
				if err := w.acts.Complete(ctx, rec); err != nil {
					t.Errorf("Complete(%q): %v", id, err)
					halt()
					return
				}
			}
		}(i)
	}

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; n < readsPerReader; n++ {
				path := paths[(worker*readsPerReader+n)%len(paths)]
				req := httptest.NewRequest(http.MethodGet, path, nil)
				rec := httptest.NewRecorder()
				w.srv.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK {
					t.Errorf("GET %s during concurrent writes = %d, want 200\n%s",
						path, rec.Code, rec.Body.String())
					halt()
					return
				}
				// A busy-timeout failure surfaces as a rendered error rather than a status,
				// because a telemetry read failure degrades a panel by design. That must not
				// be how this test passes.
				if body := rec.Body.String(); strings.Contains(body, "database is locked") ||
					strings.Contains(body, "database table is locked") {
					t.Errorf("GET %s reported a locked database:\n%s", path, body)
					halt()
					return
				}
				reads.Add(1)
			}
		}(i)
	}

	wg.Wait()

	if got := reads.Load(); got != readers*readsPerReader {
		t.Errorf("%d reads completed, want %d", got, readers*readsPerReader)
	}
	if settled.Load() == 0 {
		t.Fatal("no writes committed, so the reads were uncontended and prove nothing")
	}

	// The quiescent read agrees with the ledger exactly: nothing was lost to a partially
	// visible transaction, and nothing is left held.
	body := w.html("/?budget=child")
	want := usd(cents(charge) * money.Money(settled.Load()))
	if got := figure(t, body, "spent"); got != want {
		t.Errorf("Spent after %d settlements = %q, want %q", settled.Load(), got, want)
	}
	if got := figure(t, body, "reserved"); got != "$0.00" {
		t.Errorf("Reserved = %q with every hold resolved, want $0.00", got)
	}

	// The parent saw every leaf charge, because that is what the ancestor leg set is for.
	if got := figure(t, w.html("/?budget=research"), "spent"); got != want {
		t.Errorf("parent Spent = %q, want %q", got, want)
	}

	// And the two stores agree, so no disagreement notice is warranted.
	mustNotContain(t, body, "The ledger is authoritative.",
		"a quiesced ledger and activity store that agree must report no disagreement")
}

// elapsedPct is arithmetic on durations, and durations in nanoseconds overflow int64 when
// multiplied by 10,000 at about ten and a half days. A month-long budget is the ordinary
// case rather than the edge one, so the ordinary case is what this sweeps.
func TestElapsedPctHoldsAtEveryPeriodLength(t *testing.T) {
	lengths := []struct {
		name string
		d    time.Duration
	}{
		{"an hour", time.Hour},
		{"a day", 24 * time.Hour},
		{"a week", 7 * 24 * time.Hour},
		{"a month", monthDuration},
		{"a quarter", 92 * 24 * time.Hour},
		{"an academic year", 280 * 24 * time.Hour},
		{"a decade", 3652 * 24 * time.Hour},
	}
	fractions := []struct {
		num, den int64
		want     string
	}{
		{0, 1, "0.00"},
		{1, 100, "1.00"},
		{1, 4, "25.00"},
		{1, 2, "50.00"},
		{3, 4, "75.00"},
		{99, 100, "99.00"},
		{1, 1, "100.00"},
	}

	for _, l := range lengths {
		for _, f := range fractions {
			elapsed := time.Duration(int64(l.d) / f.den * f.num)
			if got := elapsedPct(elapsed, l.d); got != f.want {
				t.Errorf("elapsedPct(%d/%d of %s) = %q, want %q",
					f.num, f.den, l.name, got, f.want)
			}
		}
	}

	// The degenerate and out-of-range inputs, which must clamp rather than divide.
	for _, c := range []struct {
		name           string
		elapsed, total time.Duration
		want           string
	}{
		{"a zero-length period", 0, 0, "0.00"},
		{"before the period", -time.Hour, monthDuration, "0.00"},
		{"past the end", 2 * monthDuration, monthDuration, "100.00"},
		{"a nanosecond period", time.Nanosecond, time.Nanosecond, "100.00"},
		{"a negative period", time.Hour, -monthDuration, "0.00"},
	} {
		if got := elapsedPct(c.elapsed, c.total); got != c.want {
			t.Errorf("elapsedPct at %s = %q, want %q", c.name, got, c.want)
		}
	}
}
