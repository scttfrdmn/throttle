package activity

import (
	"context"
	"errors"
	"sort"
	"time"

	"throttle/money"
	"throttle/usage"
)

// ErrNotFound means no record has the given request ID.
var ErrNotFound = errors.New("activity: record not found")

// Store persists activity records.
//
// A consumer-defined interface: the SQLite implementation lives in
// activity/sqlite, and a caller who wants none of it passes nothing. Adapters
// treat a nil store as "do not record", because failing a provider call the
// caller has already paid for, merely because telemetry is unavailable, would be
// the wrong trade.
type Store interface {
	// Begin records a request at admission, before the provider is called. Writing
	// pre-call is what makes a request whose process dies mid-call visible
	// afterwards: without it, a crash leaves no evidence that money may have moved.
	//
	// It must be idempotent on RequestID, so a retry updates rather than duplicates.
	Begin(ctx context.Context, r Record) error

	// Complete records the resolution of a request: status, outcome, actual usage,
	// cost and completeness, timings.
	Complete(ctx context.Context, r Record) error

	// Get returns one record by request ID.
	Get(ctx context.Context, requestID string) (Record, error)

	// List returns records matching a filter, most recent first.
	List(ctx context.Context, f Filter) ([]Record, error)
}

// Filter selects records for listing.
type Filter struct {
	// BudgetID limits to a budget. Scoped matching includes records where the
	// budget appears as any scope, not just as the named budget, so a parent sees
	// its children's spend.
	BudgetID string

	// PeriodID limits to one period.
	PeriodID string

	// From and To bound StartedAt to the half-open window [From, To). Zero means
	// unbounded.
	From time.Time
	To   time.Time

	// Statuses limits to the given statuses. Empty means all.
	Statuses []Status

	// UnresolvedOnly limits to records whose cost is not fully known, which is the
	// query behind "3 unpriced requests".
	UnresolvedOnly bool

	// Limit caps the number returned. Zero means no cap.
	Limit int
}

// matches reports whether a record satisfies the filter. Shared by store
// implementations so filter semantics cannot drift between them.
func (f Filter) matches(r Record) bool {
	if f.BudgetID != "" && !r.touches(f.BudgetID) {
		return false
	}
	if f.PeriodID != "" && !r.inPeriod(f.PeriodID) {
		return false
	}
	if !f.From.IsZero() && r.StartedAt.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && !r.StartedAt.Before(f.To) {
		return false
	}
	if f.UnresolvedOnly && r.ActualCost.Known() && r.Status != StatusUnresolved {
		return false
	}
	if len(f.Statuses) > 0 {
		found := false
		for _, s := range f.Statuses {
			if r.Status == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (r Record) touches(budgetID string) bool {
	if r.BudgetID == budgetID {
		return true
	}
	for _, s := range r.Scopes {
		if s.BudgetID == budgetID {
			return true
		}
	}
	return false
}

func (r Record) inPeriod(periodID string) bool {
	for _, s := range r.Scopes {
		if s.PeriodID == periodID {
			return true
		}
	}
	return false
}

// Summarize reduces records to dashboard figures.
//
// It deliberately reports incompleteness rather than hiding it. A total that
// silently omits unpriceable spend is worse than one that admits it is a floor,
// because only the second lets a reader decide whether to trust it.
func Summarize(records []Record) Summary {
	s := Summary{Complete: true, Requests: len(records)}
	seen := map[usage.Dimension]bool{}

	for _, r := range records {
		amount, complete := r.Spent()
		if v, ok := money.Add(s.Spend, amount); ok {
			s.Spend = v
		} else {
			s.Spend = money.Max
		}
		if !complete {
			s.Complete = false
		}
		if r.Status == StatusUnresolved || r.Status == StatusOutstanding {
			s.Unresolved++
			if v, ok := money.Add(s.Encumbered, r.Reserved); ok {
				s.Encumbered = v
			}
		}
		for _, d := range r.ActualCost.Unpriced {
			if !seen[d] {
				seen[d] = true
				s.UnpricedDimensions = append(s.UnpricedDimensions, d)
			}
		}
	}

	sort.Slice(s.UnpricedDimensions, func(i, j int) bool {
		return s.UnpricedDimensions[i] < s.UnpricedDimensions[j]
	})
	return s
}
