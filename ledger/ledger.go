// Package ledger defines the durable accounting contract for throttle.
//
// The operational transaction is estimate -> reserve -> execute -> reconcile.
// A store implementation must make the headroom check and the reservation
// insert a single atomic step: a check-then-spend design races under
// concurrency, letting several callers observe the same headroom and
// collectively oversubscribe it.
//
// # Scopes and legs
//
// A reservation does not hold headroom against one budget. It holds a set of
// legs, one per constraint it consumes, and every leg is checked and written in
// the same transaction: either the whole set reserves or none of it does.
//
// Today the leg set is exactly the budget's ancestor chain, so a request against
// a leaf budget also consumes its parents' headroom. The representation is
// deliberately a set rather than a chain, so a future overlapping constraint
// (a per-model or per-workload cap, for example) is an additional leg rather
// than a schema change. Those constraints are not implemented.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"throttle/budget"
	"throttle/money"
	"throttle/usage"
)

// Errors returned by Ledger implementations.
var (
	// ErrInsufficientHeadroom means the reservation would exceed a ceiling. It
	// wraps into ScopeError to say which scope refused.
	ErrInsufficientHeadroom = errors.New("ledger: insufficient budget headroom")

	// ErrReservationNotFound means no reservation has the given ID.
	ErrReservationNotFound = errors.New("ledger: reservation not found")

	// ErrChargeNotFound means the reservation exists but has not settled into a
	// charge. It is distinct from ErrReservationNotFound: "this hold never became
	// money" and "this hold never existed" lead a reconciler to opposite
	// conclusions.
	ErrChargeNotFound = errors.New("ledger: no charge for this reservation")

	// ErrDuplicateReservation means the reservation ID is already in use. IDs are
	// caller-supplied so that a retry after an ambiguous failure is idempotent
	// rather than double-reserving.
	ErrDuplicateReservation = errors.New("ledger: duplicate reservation")

	// ErrAlreadyResolved means the reservation was already settled or released.
	// Settling twice would double-count real money.
	ErrAlreadyResolved = errors.New("ledger: reservation already resolved")

	// ErrLeaseExpired means a renewal arrived after the lease had already lapsed.
	// The headroom was freed and may have been taken, so it cannot be silently
	// reclaimed; the request may still settle, and will be recorded as an overrun.
	ErrLeaseExpired = errors.New("ledger: reservation lease has expired")

	// ErrInvalidArgument means the request was malformed.
	ErrInvalidArgument = errors.New("ledger: invalid argument")

	// ErrBudgetNotFound means no definition is stored under the given ID.
	ErrBudgetNotFound = errors.New("ledger: budget not found")

	// ErrDefinitionConflict means a definition with the same ID but different
	// semantics is already stored. Two processes must not silently govern the
	// same ledger under different rules.
	ErrDefinitionConflict = errors.New("ledger: conflicting budget definition")

	// ErrRevisionMismatch means an update was attempted against a stale revision.
	ErrRevisionMismatch = errors.New("ledger: budget definition revision mismatch")

	// ErrCycle means the parent links would form a cycle.
	ErrCycle = errors.New("ledger: budget hierarchy contains a cycle")

	// ErrPeriodClosed means the target period no longer accepts new reservations.
	ErrPeriodClosed = errors.New("ledger: budget period is closed to new reservations")

	// ErrNoSuchPeriodRow means no period row has the given ID. It is distinct
	// from budget.ErrNoSuchPeriod, which means the period is outside a
	// definition's schedule rather than merely not yet materialized.
	ErrNoSuchPeriodRow = errors.New("ledger: period not materialized")

	// ErrMissingCeiling means the caller did not supply a ceiling for a scope the
	// reservation must consume. Reserving without a ceiling would silently skip a
	// constraint, so it is an error rather than an unlimited default.
	ErrMissingCeiling = errors.New("ledger: no ceiling supplied for scope")
)

// ScopeError identifies which budget scope caused a failure.
type ScopeError struct {
	BudgetID string
	PeriodID string
	Err      error
}

func (e *ScopeError) Error() string {
	return fmt.Sprintf("budget %q period %q: %v", e.BudgetID, e.PeriodID, e.Err)
}

func (e *ScopeError) Unwrap() error { return e.Err }

// Scope is one accounting dimension: a budget within one of its periods.
//
// Spend is always attributed to a (budget, period) pair rather than to a budget
// alone, because a budget's headroom is a property of the period it is in.
type Scope struct {
	BudgetID string
	PeriodID string
}

func (s Scope) String() string { return s.BudgetID + "/" + s.PeriodID }

// PeriodState is the lifecycle position of a materialized period.
type PeriodState string

const (
	// StateOpen means the period accepts new reservations.
	StateOpen PeriodState = "open"

	// StateDraining means the period's time has passed but reservations it
	// authorized are still in flight. It accepts no new reservations; existing
	// holds may still settle against it, so attribution stays correct.
	StateDraining PeriodState = "draining"

	// StateClosed means every hold has resolved and the closing balance is final.
	// A closed period never changes again.
	StateClosed PeriodState = "closed"
)

// Period is a materialized budget period.
//
// The envelope is a snapshot taken when the period was created, so editing a
// definition later cannot retroactively change what a past period was allowed to
// spend.
type Period struct {
	ID       string
	BudgetID string
	Seq      int
	Envelope budget.Envelope
	State    PeriodState

	// CarryFinal reports whether Envelope.Carry can still change. Carry is
	// provisional while the preceding period is draining: it is computed as if
	// every outstanding hold there settles at its full reserved amount, which can
	// only understate the carry, never overstate it.
	CarryFinal bool

	// ClosingBalance is the signed final balance, valid once State is
	// StateClosed.
	ClosingBalance money.Money

	ClosedAt time.Time
}

// Provisional reports whether this period is still running on a carry that may
// be revised upward once its predecessor finishes draining.
func (p Period) Provisional() bool { return !p.CarryFinal }

// ReservationState is the lifecycle position of a reservation.
type ReservationState string

const (
	// StatePending means the reservation holds headroom and is not yet resolved.
	StatePending ReservationState = "pending"

	// StateSettled means actual cost was recorded and the hold was replaced by
	// a charge.
	StateSettled ReservationState = "settled"

	// StateReleased means the hold was returned without a charge, because the
	// request failed or was cancelled with no billable usage.
	StateReleased ReservationState = "released"

	// StateExpired means the lease lapsed and the hold was reclaimed by recovery
	// rather than by its owner. An expired hold may still settle: the request
	// really happened, so its cost is real.
	StateExpired ReservationState = "expired"
)

// Totals is the committed position of one scope.
//
// Spent counts settled charges. Reserved counts live, unexpired holds. The
// budget engine treats Spent+Reserved as committed money.
type Totals struct {
	Spent    money.Money
	Reserved money.Money

	// ReservedExpired is headroom held by reservations whose leases have lapsed
	// and which have not yet been reclaimed. It is excluded from Reserved so that
	// a crashed process cannot deadlock a budget forever, but it is reported so
	// operators can see that recovery has work to do.
	ReservedExpired money.Money

	// PendingCount and ExpiredCount are the number of live and lapsed holds.
	PendingCount int
	ExpiredCount int
}

// Committed is the money that is either gone or promised.
func (t Totals) Committed() money.Money {
	v, ok := money.Add(t.Spent, t.Reserved)
	if !ok {
		return money.Max
	}
	return v
}

// Reservation is a hold on budget headroom for one in-flight request.
//
// The hold is a lease, not an assumption about how long a request takes. A
// long-running request renews its lease; an abandoned one lapses and stops
// consuming headroom.
type Reservation struct {
	// ID is caller-supplied and unique per reservation attempt.
	ID string

	// BudgetID is the budget the request named. Legs record every scope the hold
	// actually consumes, including ancestors.
	BudgetID string

	// RequestID ties the reservation to the caller's request for reconciliation.
	RequestID string

	// Amount is the estimated cost being held. It must not be negative.
	Amount money.Money

	// EstimatedCost is what the adapter predicted, retained for later analysis
	// of estimate quality. It is usually equal to Amount.
	EstimatedCost money.Money

	CreatedAt time.Time

	// ExpiresAt is the lease deadline. A zero value means the hold never expires,
	// which risks stranding headroom if the owning process dies; stores should
	// encourage a finite lease.
	ExpiresAt time.Time

	// Lease is the renewal quantum: how far past "now" a Renew call extends the
	// deadline when it does not specify its own.
	Lease time.Duration

	RenewedAt  time.Time
	RenewCount int

	State ReservationState

	// Legs are the scopes this hold consumes, nearest first (depth 0 is the named
	// budget, depth 1 its parent, and so on).
	Legs []Leg

	Identity usage.ModelIdentity
	Metadata map[string]string
}

// Leg is one scope's share of a reservation or charge.
//
// Amount is per-leg so that a future constraint could consume a different amount
// than the named budget does. Every leg of a hierarchical reservation currently
// carries the same amount: spending a dollar in a child spends a dollar of the
// parent.
type Leg struct {
	Scope  Scope
	Depth  int
	Amount money.Money
}

// Expired reports whether the lease has lapsed at at.
func (r Reservation) Expired(at time.Time) bool {
	return !r.ExpiresAt.IsZero() && at.After(r.ExpiresAt)
}

// Scopes returns the scopes this reservation consumes.
func (r Reservation) Scopes() []Scope {
	out := make([]Scope, len(r.Legs))
	for i, l := range r.Legs {
		out[i] = l.Scope
	}
	return out
}

// ReserveRequest asks a store to atomically verify headroom in every scope and
// record a hold.
type ReserveRequest struct {
	Reservation

	// Ceilings is the cumulative Spent+Reserved ceiling permitted per budget,
	// computed by the engine from each scope's pacing curve. The store derives
	// the scope set from stored parent links and fails with ErrMissingCeiling if
	// any required budget is absent, so a forgotten ancestor cannot silently go
	// unchecked.
	Ceilings map[string]money.Money

	// Now is the evaluation time, so that expiry and period selection are
	// deterministic and testable rather than depending on the store's clock.
	Now time.Time
}

// RenewRequest extends a live reservation's lease.
type RenewRequest struct {
	ReservationID string

	// Now is the evaluation time. A renewal that arrives after the lease has
	// already lapsed fails with ErrLeaseExpired.
	Now time.Time

	// Extend is how far past Now the new deadline should be. Zero uses the
	// reservation's own lease quantum.
	Extend time.Duration
}

// Settlement records the actual observed cost of a completed request.
type Settlement struct {
	ReservationID string

	// ActualCost is authoritative even when it exceeds the reservation. Reality
	// is recorded rather than clamped to the estimate.
	ActualCost money.Money

	Usage       usage.Actual
	CompletedAt time.Time
	Metadata    map[string]string
}

// Charge is the durable record of money actually spent.
type Charge struct {
	ID            string
	ReservationID string
	BudgetID      string
	RequestID     string

	EstimatedCost money.Money
	ReservedCost  money.Money
	ActualCost    money.Money

	// Legs are the scopes the charge was applied to. They mirror the
	// reservation's legs, so the actual cost lands in the period that authorized
	// the request even when settlement arrives after that period has ended.
	Legs []Leg

	Usage      usage.Actual
	Identity   usage.ModelIdentity
	OccurredAt time.Time
	Latency    time.Duration

	// PolicyActions is reserved for future policy visibility and is empty in
	// v0.1. It exists so that adding policy later does not change this schema.
	PolicyActions []string

	Metadata map[string]string
}

// Overrun is how much the actual cost exceeded the amount reserved. It is zero
// when the request came in at or under its estimate.
func (c Charge) Overrun() money.Money {
	if c.ActualCost <= c.ReservedCost {
		return 0
	}
	d, ok := money.Sub(c.ActualCost, c.ReservedCost)
	if !ok {
		return 0
	}
	return d
}

// Ledger is the durable accounting store.
//
// Implementations must be safe for concurrent use and atomic across processes
// sharing the same storage. Every method takes a context so that callers can
// bound or cancel storage work.
type Ledger interface {
	// PutDefinition stores a budget definition. Storing a definition whose
	// semantics differ from the stored one returns ErrDefinitionConflict rather
	// than overwriting it, so two processes cannot silently disagree about the
	// rules governing shared spend. Re-storing an identical definition succeeds.
	PutDefinition(ctx context.Context, def budget.Definition) error

	// UpdateDefinition replaces a stored definition, requiring the caller to
	// state the revision it expects to replace. It affects future periods only;
	// periods already materialized keep their snapshot.
	UpdateDefinition(ctx context.Context, def budget.Definition, expectRevision int) error

	// Definition returns a stored definition and its revision.
	Definition(ctx context.Context, budgetID string) (budget.Definition, int, error)

	// Definitions returns every stored definition.
	Definitions(ctx context.Context) ([]budget.Definition, error)

	// Chain returns the budget and its ancestors, nearest first, which is the set
	// of scopes a request against budgetID must reserve against.
	Chain(ctx context.Context, budgetID string) ([]budget.Definition, error)

	// EnsurePeriod returns the period of budgetID containing at, materializing it
	// and any skipped predecessors if needed. It is idempotent and safe to call
	// concurrently from several processes.
	EnsurePeriod(ctx context.Context, budgetID string, at time.Time) (Period, error)

	// Period returns a materialized period by ID.
	Period(ctx context.Context, periodID string) (Period, error)

	// Periods returns every materialized period of a budget, in sequence order.
	Periods(ctx context.Context, budgetID string) ([]Period, error)

	// Advance performs due period transitions for a budget: it moves elapsed
	// periods to draining, closes drained ones, and finalizes carry. It is
	// idempotent, and stores may also run it implicitly.
	Advance(ctx context.Context, budgetID string, now time.Time) ([]Period, error)

	// Totals returns the committed position of one scope as of now.
	Totals(ctx context.Context, scope Scope, now time.Time) (Totals, error)

	// Reserve atomically checks every required scope against its ceiling and
	// records the hold with one leg per scope. It returns a ScopeError wrapping
	// ErrInsufficientHeadroom if any scope refuses, and records nothing.
	Reserve(ctx context.Context, req ReserveRequest) (Reservation, error)

	// Renew extends a pending reservation's lease so a long-running request does
	// not lose its headroom. It does not re-check headroom: the amount held is
	// unchanged, only the deadline moves.
	Renew(ctx context.Context, req RenewRequest) (Reservation, error)

	// Settle replaces a pending hold with a charge for the actual cost, applying
	// it to exactly the scopes the hold reserved, exactly once.
	Settle(ctx context.Context, settlement Settlement) (Charge, error)

	// Release returns a pending hold, across its whole leg set, without recording
	// a charge. It must only be used when no billable usage occurred.
	Release(ctx context.Context, reservationID string) error

	// Get returns a reservation by ID regardless of state, for reconciliation.
	Get(ctx context.Context, reservationID string) (Reservation, error)

	// ChargeFor returns the charge a reservation settled into, or
	// ErrChargeNotFound if it never settled.
	//
	// This is the question a reconciler must be able to ask: after a crash between
	// the ledger write and the telemetry write, the charge is the authoritative
	// record of what happened, and the reservation ID is the only handle on it.
	// Charges is scope-and-time-windowed and cannot answer it.
	ChargeFor(ctx context.Context, reservationID string) (Charge, error)

	// Reservations returns reservations in the given states, oldest first, limited
	// to at most limit records when limit > 0.
	//
	// It is the ledger-side enumeration reconciliation needs, and it is read-only:
	// RecoverExpired also finds lapsed holds but mutates them, which is the wrong
	// tool for a survey. Empty states means every state.
	//
	// The limit is what keeps a reconciliation pass bounded on a large ledger.
	Reservations(ctx context.Context, states []ReservationState, limit int) ([]Reservation, error)

	// RecoverExpired marks holds whose leases have lapsed as expired and returns
	// them, so a crashed process cannot strand budget headroom permanently.
	RecoverExpired(ctx context.Context, budgetID string, now time.Time) ([]Reservation, error)

	// Charges returns charges attributed to a scope in the half-open window
	// [from, to), most recent first, limited to at most limit records when
	// limit > 0.
	Charges(ctx context.Context, scope Scope, from, to time.Time, limit int) ([]Charge, error)
}
