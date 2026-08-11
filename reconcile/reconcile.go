// Package reconcile repairs bookkeeping that a process crash left half-finished.
//
// # What this is for
//
// A governed request touches two durable stores that cannot be committed in one
// transaction: the ledger, which holds the money, and the activity store, which
// holds the observability record. Between them sits a paid provider call. A process
// that dies in the wrong microsecond therefore leaves a ledger and an activity
// store telling different stories about the same request, and nothing running is
// left to finish the sentence: engine.Transaction lives entirely in memory, so its
// notion of "this request is mid-flight" dies with the process.
//
// This package reconstructs what happened from the two durable stores and finishes
// the bookkeeping — but only when the stores already contain enough authoritative
// information to finish it truthfully.
//
// # The three classes, and why only one is repairable
//
// A record whose bookkeeping is incomplete is in one of three genuinely different
// situations, and the entire design of this package is the distinction between
// them:
//
//   - Bookkeeping is incomplete, and enough authoritative information exists to
//     complete it. The ledger settled and the activity write never landed; the
//     provider's usage and the quote frozen at admission are both durably present
//     and settlement never ran. These are repaired.
//
//   - The provider's outcome is genuinely unknown. The call may have been served
//     and billed, and no durable evidence of usage exists. These stay unresolved.
//
//   - The accounting is intentionally unresolved, awaiting an observation that has
//     not happened yet — a hosted runtime's resource consumption arrives through
//     the platform's observability, long after the invocation. These are not crash
//     damage at all, and treating them as such would be a bug.
//
// Only the first becomes money. The other two are classified, reported, and left
// exactly as they are. A reconciler that turned an unknown cost into a tidy zero
// would be worse than one that did nothing, because the database would then look
// correct while being wrong.
//
// # What it may not do
//
//   - It never consults a pricing catalog. This package imports no catalog and holds
//     no reference to one: a settlement it replays is priced by the immutable quote
//     captured when the request was admitted, so a price change after the crash
//     cannot rewrite history. That guarantee is structural rather than a rule
//     somebody has to remember.
//   - It never invents a cost, an amount, or a provider identity that was not
//     durably recorded.
//   - It never treats an expired lease as evidence that spend was zero. A lapsed
//     hold has stopped blocking headroom; that says nothing about what the provider
//     billed.
//   - It never reads or writes request content. Every fact it uses is a status, an
//     amount, a usage count, or an identifier, which is exactly why a content-free
//     activity record is enough to recover from a crash.
//   - It contains no provider conditionals. Classification reads normalized fields
//     that adapters already write, so a new provider needs no change here.
//
// # Authority
//
// The ledger is authoritative for money. Activity is repaired from the ledger, never
// the reverse: an activity-store failure must never be able to fail a paid provider
// call, so activity is the store most likely to be the incomplete one. The two
// exceptions run the other way — a durably recorded provider usage or a durably
// recorded no-billable-usage failure lets a stalled ledger transition finish — and
// both are guarded on facts the adapter observed from the provider, not on the
// activity store's opinion of what should have happened.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"throttle/activity"
	"throttle/ledger"
	"throttle/money"
	"throttle/pricing"
	"throttle/usage"
)

// Class is the outcome of reconciling one record.
//
// The set exists so a summary cannot flatter itself. "Unresolved" and "awaiting
// external usage" are not failures and not repairs; counting them as either would
// misreport the health of the system in opposite directions.
type Class string

const (
	// ClassRepaired means durable state was incomplete and has been completed from
	// authoritative information.
	ClassRepaired Class = "repaired"

	// ClassConsistent means the two stores already agree. Nothing was written.
	ClassConsistent Class = "already_consistent"

	// ClassUnresolved means the record is legitimately unresolved and was left that
	// way: either the provider outcome is unknown, or the cost cannot be priced by
	// the quote this request was admitted under.
	ClassUnresolved Class = "unresolved"

	// ClassAwaiting means the record is waiting for an external observation that has
	// not arrived. Distinct from ClassUnresolved because this is the designed
	// terminal state of a hosted runtime invocation, not damage.
	ClassAwaiting Class = "awaiting_external_usage"

	// ClassOrphaned means one store holds a record the other does not, and there is
	// not enough durable information to repair it without inventing something.
	ClassOrphaned Class = "orphaned"

	// ClassFailed means reconciliation could not reach a consistent state, e.g. a
	// storage error. It is the only class that is an error.
	ClassFailed Class = "failed"
)

// Reason is the durable classification behind a Class: why this record is in the
// state it is in.
//
// It is the field that keeps a hosted runtime invocation awaiting delayed telemetry
// from being mistaken for a crashed transaction, and it is derived from normalized
// durable facts rather than from provider names.
type Reason string

const (
	// ReasonCrashRepairable means bookkeeping was interrupted and authoritative
	// information exists to finish it.
	ReasonCrashRepairable Reason = "crash-repairable"

	// ReasonProviderOutcomeUnknown means the provider may have served and billed the
	// request and no durable evidence of usage exists. Never resolvable by inference.
	ReasonProviderOutcomeUnknown Reason = "provider-outcome-unknown"

	// ReasonPricingUnresolved means usage is durably known and the quote captured at
	// admission cannot price it. Resolving it needs pricing data, not repair.
	ReasonPricingUnresolved Reason = "pricing-unresolved"

	// ReasonAwaitingExternalUsage means the billable quantity is reported out of band
	// and has not arrived yet.
	ReasonAwaitingExternalUsage Reason = "awaiting-external-usage"

	// ReasonIncompleteRecord means the durable record itself is missing the
	// information repair would need. It is the orphan reason.
	ReasonIncompleteRecord Reason = "incomplete-record"
)

// Money names the monetary transition a repair performed, if any.
type Money string

const (
	// MoneyNone means no money moved.
	MoneyNone Money = ""

	// MoneySettled means a hold became a charge.
	MoneySettled Money = "settled"

	// MoneyReleased means a hold was returned without a charge.
	MoneyReleased Money = "released"
)

// Result is the structured outcome for one record.
//
// Structured rather than a log line because the caller has to be able to tell
// "three requests were repaired" from "three requests still owe a price", and a
// string cannot be counted.
type Result struct {
	RequestID     string
	ReservationID string
	BudgetID      string

	Class  Class
	Reason Reason

	// Observed is the activity status found, and Produced the status written. They
	// are equal when nothing changed, and Produced is empty for an orphaned ledger
	// reservation with no activity record at all.
	Observed activity.Status
	Produced activity.Status

	// Money and Amount report the monetary transition. Amount is meaningful only
	// when Money is MoneySettled.
	Money  Money
	Amount money.Money

	// DryRun reports that this result describes what would have happened. Nothing
	// was written.
	DryRun bool

	// Detail explains the outcome in one sentence, for operators.
	Detail string

	// Err is set when Class is ClassFailed.
	Err error
}

// Repaired reports whether durable state changed (or would have, under dry run).
func (r Result) Repaired() bool { return r.Class == ClassRepaired }

// Summary is the outcome of a reconciliation pass.
type Summary struct {
	Scanned    int
	Repaired   int
	Consistent int
	Unresolved int
	Awaiting   int
	Orphaned   int
	Failed     int

	// Settled and Released are the money the pass moved.
	Settled  money.Money
	Released money.Money

	// Truncated reports that the pass hit its limit and did not see everything.
	// Reported rather than silent: a bounded pass that looked complete would let an
	// operator believe a clean summary covered the whole ledger.
	Truncated bool

	DryRun bool

	Results []Result
}

func (s *Summary) add(r Result) {
	s.Scanned++
	s.Results = append(s.Results, r)
	switch r.Class {
	case ClassRepaired:
		s.Repaired++
	case ClassConsistent:
		s.Consistent++
	case ClassUnresolved:
		s.Unresolved++
	case ClassAwaiting:
		s.Awaiting++
	case ClassOrphaned:
		s.Orphaned++
	case ClassFailed:
		s.Failed++
	}
	if r.DryRun {
		return
	}
	switch r.Money {
	case MoneySettled:
		if v, ok := money.Add(s.Settled, r.Amount); ok {
			s.Settled = v
		}
	case MoneyReleased:
		if v, ok := money.Add(s.Released, r.Amount); ok {
			s.Released = v
		}
	}
}

// Ledger is the accounting store, as reconciliation needs it.
//
// Deliberately narrower than ledger.Ledger: this package must be able to read a
// hold, read what it settled into, enumerate candidates, and complete a stalled
// transition. It has no business creating reservations or editing budget
// definitions, and an interface that allowed it to would invite exactly that.
type Ledger interface {
	Get(ctx context.Context, reservationID string) (ledger.Reservation, error)
	ChargeFor(ctx context.Context, reservationID string) (ledger.Charge, error)
	Reservations(ctx context.Context, states []ledger.ReservationState, limit int) ([]ledger.Reservation, error)
	Settle(ctx context.Context, settlement ledger.Settlement) (ledger.Charge, error)
	Release(ctx context.Context, reservationID string) error
}

// Activity is the observability store, as reconciliation needs it. Complete is the
// repair write: it is the same idempotent upsert the request path uses.
type Activity interface {
	Get(ctx context.Context, requestID string) (activity.Record, error)
	List(ctx context.Context, f activity.Filter) ([]activity.Record, error)
	Complete(ctx context.Context, r activity.Record) error
}

// Config configures a Reconciler.
type Config struct {
	// Ledger is authoritative for money. Required.
	Ledger Ledger

	// Activity is the durable request record. Required: without it there is nothing
	// to repair against, and a reconciler that ran on the ledger alone could only
	// report orphans.
	Activity Activity

	// Clock defaults to time.Now in UTC.
	Clock func() time.Time

	// DryRun classifies without writing anything.
	DryRun bool

	// Limit bounds how many candidates one pass examines, per side. Zero uses
	// DefaultLimit. A pass that hits the limit reports Truncated.
	Limit int
}

// DefaultLimit keeps a pass over a large ledger bounded and quick enough to run at
// process start. It is a bound on work, not a statement about how much damage is
// plausible: a truncated pass says so, and running again continues.
const DefaultLimit = 500

// Reconciler repairs stranded bookkeeping.
//
// Safe for concurrent use, including from several processes over the same stores:
// exactly one monetary transition can win, and the loser observes the resulting
// authoritative state and converges rather than failing. That guarantee comes from
// the ledger's own uniqueness constraints, not from a lock here.
type Reconciler struct {
	ledger   Ledger
	activity Activity
	clock    func() time.Time
	dryRun   bool
	limit    int
}

// New builds a Reconciler.
func New(cfg Config) (*Reconciler, error) {
	if cfg.Ledger == nil {
		return nil, errors.New("reconcile: a ledger is required")
	}
	if cfg.Activity == nil {
		return nil, errors.New("reconcile: an activity store is required")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	limit := cfg.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 0 {
		return nil, errors.New("reconcile: limit cannot be negative")
	}
	return &Reconciler{
		ledger:   cfg.Ledger,
		activity: cfg.Activity,
		clock:    clock,
		dryRun:   cfg.DryRun,
		limit:    limit,
	}, nil
}

// candidate is one unit of reconciliation work. Either side may be the one that
// knows about the request, so a candidate carries both handles and neither is
// required to resolve.
type candidate struct {
	requestID     string
	reservationID string
}

// Reconcile reconciles one request by ID.
//
// It is idempotent: running it on a record that is already consistent writes
// nothing and reports ClassConsistent.
func (r *Reconciler) Reconcile(ctx context.Context, requestID string) (Result, error) {
	if requestID == "" {
		return Result{}, errors.New("reconcile: request id is required")
	}
	rec, err := r.activity.Get(ctx, requestID)
	if errors.Is(err, activity.ErrNotFound) {
		return Result{}, fmt.Errorf("%w: %q", activity.ErrNotFound, requestID)
	}
	if err != nil {
		return Result{}, err
	}
	return r.reconcile(ctx, candidate{requestID: requestID, reservationID: rec.ReservationID}, &rec), nil
}

// ReconcilePending sweeps for stranded bookkeeping and reconciles what it finds.
//
// It surveys both stores, because either can be the one holding the evidence. The
// activity side finds requests that never reached a terminal state; the ledger side
// finds holds that are still standing, which is the only way to notice a
// reservation whose activity record never landed at all — and the only way to find
// the inverse crash, where the record resolved and the hold did not.
//
// It is bounded, restart-safe, and safe to run concurrently with request traffic
// and with another reconciler.
func (r *Reconciler) ReconcilePending(ctx context.Context) (Summary, error) {
	sum := Summary{DryRun: r.dryRun}

	// Requests that never reached a terminal state. Unresolved and outstanding are
	// included deliberately: most will be left exactly as they are, and confirming
	// that they are legitimately unresolved rather than crash damage is half of what
	// this pass is for.
	records, err := r.activity.List(ctx, activity.Filter{
		Statuses: []activity.Status{
			activity.StatusPending,
			activity.StatusOutstanding,
			activity.StatusUnresolved,
		},
		Limit: r.limit,
	})
	if err != nil {
		return sum, fmt.Errorf("reconcile: listing activity: %w", err)
	}
	if len(records) == r.limit {
		sum.Truncated = true
	}

	seen := make(map[string]bool, len(records))
	byID := make(map[string]*activity.Record, len(records))
	order := make([]candidate, 0, len(records))
	for i := range records {
		rec := &records[i]
		if seen[rec.RequestID] {
			continue
		}
		seen[rec.RequestID] = true
		byID[rec.RequestID] = rec
		order = append(order, candidate{requestID: rec.RequestID, reservationID: rec.ReservationID})
	}

	// Holds that are still standing. A pending or expired reservation is either
	// legitimately in flight, legitimately unresolved, or the money half of a
	// transition that never finished.
	holds, err := r.ledger.Reservations(ctx,
		[]ledger.ReservationState{ledger.StatePending, ledger.StateExpired}, r.limit)
	if err != nil {
		return sum, fmt.Errorf("reconcile: listing reservations: %w", err)
	}
	if len(holds) == r.limit {
		sum.Truncated = true
	}
	for _, h := range holds {
		if h.RequestID != "" && seen[h.RequestID] {
			continue
		}
		if h.RequestID != "" {
			seen[h.RequestID] = true
		}
		order = append(order, candidate{requestID: h.RequestID, reservationID: h.ID})
	}

	for _, c := range order {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		sum.add(r.reconcile(ctx, c, byID[c.requestID]))
	}
	return sum, nil
}

// reconcile classifies one candidate and applies whatever repair is safe.
//
// rec may be nil, meaning the caller has not already read the activity record.
func (r *Reconciler) reconcile(ctx context.Context, c candidate, rec *activity.Record) Result {
	out := Result{RequestID: c.requestID, ReservationID: c.reservationID, DryRun: r.dryRun}

	if rec == nil && c.requestID != "" {
		got, err := r.activity.Get(ctx, c.requestID)
		switch {
		case errors.Is(err, activity.ErrNotFound):
			// The activity store has no record of a request the ledger holds money
			// for. Recoverable? Only partly: the reservation carries the identity and
			// metadata the engine was given, but not the usage, the quote, or the
			// outcome. Reporting the orphan is honest; synthesizing a record from what
			// happens to be lying around would put invented provider metadata into the
			// durable observability store.
			return r.orphanFromLedger(ctx, out)
		case err != nil:
			return fail(out, err)
		default:
			rec = &got
		}
	}
	if rec == nil {
		return r.orphanFromLedger(ctx, out)
	}

	out.RequestID = rec.RequestID
	out.BudgetID = rec.BudgetID
	out.Observed = rec.Status
	out.Produced = rec.Status
	if out.ReservationID == "" {
		out.ReservationID = rec.ReservationID
	}

	// A record that never got a hold has no ledger side to reconcile against.
	if rec.ReservationID == "" {
		return r.withoutReservation(out, *rec)
	}

	res, err := r.ledger.Get(ctx, rec.ReservationID)
	switch {
	case errors.Is(err, ledger.ErrReservationNotFound):
		// The record names a hold the ledger has never heard of. The two stores are
		// not describing the same world; nothing here can be repaired by inference.
		out.Class = ClassOrphaned
		out.Reason = ReasonIncompleteRecord
		out.Detail = fmt.Sprintf("the record names reservation %q, which the ledger does not have", rec.ReservationID)
		return out
	case err != nil:
		return fail(out, err)
	}

	switch res.State {
	case ledger.StateSettled:
		return r.fromCharge(ctx, out, *rec)
	case ledger.StateReleased:
		return r.fromRelease(ctx, out, *rec)
	}
	// Pending or expired: the hold is still standing. Expiry is a headroom fact and
	// never evidence about spend, so both are treated identically here.
	return r.standing(ctx, out, *rec, res)
}

// orphanFromLedger reports a hold whose activity record is absent.
func (r *Reconciler) orphanFromLedger(ctx context.Context, out Result) Result {
	out.Class = ClassOrphaned
	out.Reason = ReasonIncompleteRecord
	if out.ReservationID == "" {
		out.Detail = "neither store has a record of this request"
		return out
	}
	res, err := r.ledger.Get(ctx, out.ReservationID)
	if err != nil {
		out.Detail = fmt.Sprintf("reservation %q has no activity record", out.ReservationID)
		return out
	}
	out.BudgetID = res.BudgetID
	out.Amount = res.Amount
	out.Detail = fmt.Sprintf("reservation %q holds %s on %s in state %s with no activity record; "+
		"its usage, quote, and outcome were never durably recorded",
		res.ID, res.Amount.CentsString(), res.BudgetID, res.State)
	return out
}

// withoutReservation classifies a record that holds no reservation.
func (r *Reconciler) withoutReservation(out Result, rec activity.Record) Result {
	switch rec.Status {
	case activity.StatusDenied:
		// A denied request never got a hold and never called a provider. Complete by
		// construction.
		out.Class = ClassConsistent
		out.Detail = "denied before a hold was taken"
		return out
	default:
		out.Class = ClassOrphaned
		out.Reason = ReasonIncompleteRecord
		out.Detail = fmt.Sprintf("the record is %s but names no reservation, so there is no accounting to reconcile against", rec.Status)
		return out
	}
}

// fromCharge repairs an activity record from the charge its hold settled into.
//
// This is the crash the whole package exists for: the ledger committed the money
// and the process died before the telemetry write. The charge is authoritative, so
// the repair is a transcription and not a judgement, and no second monetary
// transition happens.
func (r *Reconciler) fromCharge(ctx context.Context, out Result, rec activity.Record) Result {
	charge, err := r.ledger.ChargeFor(ctx, rec.ReservationID)
	if errors.Is(err, ledger.ErrChargeNotFound) {
		// Settled with no charge should be impossible: the state change and the charge
		// insert are one transaction. Reporting it beats guessing an amount.
		out.Class = ClassFailed
		out.Reason = ReasonIncompleteRecord
		out.Err = err
		out.Detail = fmt.Sprintf("reservation %q is settled but has no charge", rec.ReservationID)
		return out
	}
	if err != nil {
		return fail(out, err)
	}

	if rec.Status == activity.StatusSettled && rec.ActualCost.Known() && rec.ActualCost.Amount == charge.ActualCost {
		out.Class = ClassConsistent
		out.Detail = "the record already matches its charge"
		return out
	}

	repaired := rec
	repaired.Status = activity.StatusSettled
	repaired.ActualCost = usage.KnownCost(charge.ActualCost)
	if repaired.ActualUsage.Empty() && !charge.Usage.Usage.Empty() {
		repaired.ActualUsage = charge.Usage.Usage
	}
	if repaired.CompletedAt.IsZero() {
		repaired.CompletedAt = charge.OccurredAt
	}
	if repaired.Outcome == "" {
		// The charge proves the provider billed for the request, which is what
		// "success" means to this record. Any richer outcome the crash lost is not
		// recoverable and is not invented.
		repaired.Outcome = activity.OutcomeSuccess
	}

	out.Class = ClassRepaired
	out.Reason = ReasonCrashRepairable
	out.Produced = activity.StatusSettled
	out.Amount = charge.ActualCost
	out.Detail = fmt.Sprintf("the ledger had already charged %s; the record was %s and now matches",
		charge.ActualCost.CentsString(), rec.Status)

	// No money moves here: the charge already exists. Money stays MoneyNone so a
	// summary cannot double-count a settlement that happened before the crash.
	return r.write(ctx, out, repaired, rec, MoneyNone, 0, quoteRef{})
}

// fromRelease repairs an activity record whose hold the ledger already returned.
func (r *Reconciler) fromRelease(ctx context.Context, out Result, rec activity.Record) Result {
	switch rec.Status {
	case activity.StatusReleased, activity.StatusDenied:
		out.Class = ClassConsistent
		out.Detail = "the record already matches the released hold"
		return out
	}

	repaired := rec
	repaired.Status = activity.StatusReleased
	// Zero is the ledger's own claim, not an inference: Release may only be used when
	// no billable usage occurred, so a released hold is a statement that nothing was
	// spent. That is the one circumstance in which a zero cost is a measurement.
	repaired.ActualCost = usage.KnownCost(0)
	if repaired.CompletedAt.IsZero() {
		repaired.CompletedAt = r.clock()
	}
	// Outcome is deliberately left alone. The ledger records that the hold was
	// returned, not why, and the reason a request failed is not recoverable from it.

	out.Class = ClassRepaired
	out.Reason = ReasonCrashRepairable
	out.Produced = activity.StatusReleased
	out.Detail = fmt.Sprintf("the ledger had already released the hold; the record was %s and now matches", rec.Status)
	return r.write(ctx, out, repaired, rec, MoneyNone, 0, quoteRef{})
}

// standing classifies a record whose hold is still pending or expired, and finishes
// the transition when the durable facts already determine it.
func (r *Reconciler) standing(ctx context.Context, out Result, rec activity.Record, res ledger.Reservation) Result {
	// Awaiting an out-of-band observation is checked first, because it is the one
	// state that looks like damage and is not. The test is on normalized linkage
	// fields the adapter wrote — a session whose resource usage has not been
	// attributed — and not on which provider or operation produced them.
	if awaitingExternal(rec) {
		out.Class = ClassAwaiting
		out.Reason = ReasonAwaitingExternalUsage
		out.Amount = rec.Reserved
		out.Detail = fmt.Sprintf("runtime session %q has not reported resource usage yet; "+
			"%s stays encumbered and the cost is unknown, not zero",
			rec.Runtime.SessionID, rec.Reserved.CentsString())
		return out
	}

	// A durably recorded failure with no billable usage: the adapter observed the
	// provider refuse the call and recorded a known zero cost, and the release never
	// landed. Finishing it frees headroom the request never consumed.
	if releasable(rec) {
		return r.replayRelease(ctx, out, rec)
	}

	// No durable evidence of usage. The provider may have served and billed the
	// request; nothing here can tell. This is the state that must survive
	// reconciliation intact, because every way of resolving it is a guess.
	if rec.ActualUsage.Empty() {
		return r.markOutcomeUnknown(ctx, out, rec, res)
	}

	// Usage is durably present. Price it with the quote frozen at admission —
	// never with a catalog, which this package does not have.
	cost, ref := replay(rec)
	if !cost.Known() {
		return r.markPricingUnresolved(ctx, out, rec, cost)
	}

	if rec.Status == activity.StatusSettled {
		// The record says settled and the ledger never charged. Whichever way that
		// happened, the frozen quote and the observed usage are both here, so the
		// stalled monetary transition can finish.
		out.Detail = "the record was settled but its hold was never charged; replaying the captured quote"
	} else {
		out.Detail = fmt.Sprintf("usage and the quote captured at admission were durably recorded while the record was %s; replaying the captured quote", rec.Status)
	}
	return r.replaySettle(ctx, out, rec, cost, ref)
}

// replaySettle completes a settlement from durable facts.
//
// The amount comes from re-pricing the recorded usage under the recorded quote
// rather than from the record's stored cost, so a repair reproduces the arithmetic
// settlement would have done instead of trusting a number written by the process
// that then crashed.
func (r *Reconciler) replaySettle(ctx context.Context, out Result, rec activity.Record, cost usage.Cost, ref quoteRef) Result {
	out.Class = ClassRepaired
	out.Reason = ReasonCrashRepairable
	out.Produced = activity.StatusSettled
	out.Money = MoneySettled
	out.Amount = cost.Amount

	if r.dryRun {
		return out
	}

	when := rec.CompletedAt
	if when.IsZero() {
		when = r.clock()
	}
	charge, err := r.ledger.Settle(ctx, ledger.Settlement{
		ReservationID: rec.ReservationID,
		ActualCost:    cost.Amount,
		Usage: usage.Actual{
			Identity:        rec.Identity,
			Usage:           rec.ActualUsage,
			Cost:            cost,
			ProviderLatency: rec.ProviderLatency,
		},
		CompletedAt: when,
		Metadata:    rec.Metadata,
	})
	if errors.Is(err, ledger.ErrAlreadyResolved) {
		// Another reconciler won. Exactly one monetary transition happened, which is
		// the invariant; this one converges on the authoritative result rather than
		// reporting a conflict for an operator to investigate.
		converged := Result{
			RequestID: out.RequestID, ReservationID: out.ReservationID, BudgetID: out.BudgetID,
			Observed: out.Observed, Produced: out.Observed, DryRun: out.DryRun,
		}
		return r.reconcileAfterRace(ctx, converged, rec)
	}
	if err != nil {
		return fail(out, err)
	}

	repaired := rec
	repaired.Status = activity.StatusSettled
	repaired.ActualCost = cost
	if repaired.Outcome == "" {
		repaired.Outcome = activity.OutcomeSuccess
	}
	if repaired.CompletedAt.IsZero() {
		repaired.CompletedAt = charge.OccurredAt
	}
	out.Amount = charge.ActualCost
	return r.write(ctx, out, repaired, rec, MoneySettled, charge.ActualCost, ref)
}

// replayRelease completes a release from durable facts.
func (r *Reconciler) replayRelease(ctx context.Context, out Result, rec activity.Record) Result {
	out.Class = ClassRepaired
	out.Reason = ReasonCrashRepairable
	out.Produced = activity.StatusReleased
	out.Money = MoneyReleased
	out.Amount = rec.Reserved
	out.Detail = fmt.Sprintf("the record durably reports a failure with no billable usage, and %s was still held",
		rec.Reserved.CentsString())

	if r.dryRun {
		return out
	}
	err := r.ledger.Release(ctx, rec.ReservationID)
	if errors.Is(err, ledger.ErrAlreadyResolved) {
		converged := Result{
			RequestID: out.RequestID, ReservationID: out.ReservationID, BudgetID: out.BudgetID,
			Observed: out.Observed, Produced: out.Observed, DryRun: out.DryRun,
		}
		return r.reconcileAfterRace(ctx, converged, rec)
	}
	if err != nil {
		return fail(out, err)
	}
	return r.write(ctx, out, rec, rec, MoneyReleased, rec.Reserved, quoteRef{})
}

// reconcileAfterRace re-reads the hold after losing a race and reports the state
// that actually won.
func (r *Reconciler) reconcileAfterRace(ctx context.Context, out Result, rec activity.Record) Result {
	res, err := r.ledger.Get(ctx, rec.ReservationID)
	if err != nil {
		return fail(out, err)
	}
	switch res.State {
	case ledger.StateSettled:
		return r.fromCharge(ctx, out, rec)
	case ledger.StateReleased:
		return r.fromRelease(ctx, out, rec)
	default:
		out.Class = ClassFailed
		out.Reason = ReasonIncompleteRecord
		out.Err = ledger.ErrAlreadyResolved
		out.Detail = fmt.Sprintf("reservation %q reported itself resolved but reads back as %s", rec.ReservationID, res.State)
		return out
	}
}

// markOutcomeUnknown records that the provider's outcome cannot be determined.
//
// It never resolves anything. The hold stays where it is, and the cost stays
// unknown: a request that may have been served and billed is not free because
// nobody was watching when it ended.
func (r *Reconciler) markOutcomeUnknown(ctx context.Context, out Result, rec activity.Record, res ledger.Reservation) Result {
	out.Class = ClassUnresolved
	out.Reason = ReasonProviderOutcomeUnknown
	out.Amount = rec.Reserved

	stranded := fmt.Sprintf("no usage was durably recorded, so whether the provider served and billed this request is unknown; "+
		"%s stays encumbered under a %s hold", rec.Reserved.CentsString(), res.State)

	// Both of these are an adapter's own terminal conclusion that the accounting is
	// unresolved. Rewriting one would replace an observation with an inference and
	// destroy the outcome the adapter recorded, so the record is reported as it
	// stands and nothing is written.
	if rec.Status == activity.StatusOutstanding || rec.Status == activity.StatusUnresolved {
		out.Detail = stranded
		return out
	}

	// A record still marked pending is a request whose process died mid-call. Moving
	// it to outstanding is a statement about knowledge, not about money: nothing is
	// charged, nothing is released, and the cost stays explicitly unknown.
	repaired := rec
	repaired.Status = activity.StatusOutstanding
	if repaired.Outcome == "" {
		repaired.Outcome = activity.OutcomeAccountingError
	}
	if !repaired.ActualCost.Known() || repaired.ActualCost.Amount == 0 {
		repaired.ActualCost = usage.UnknownCost(
			"the process handling this request did not survive to record its outcome")
	}
	if repaired.CompletedAt.IsZero() {
		repaired.CompletedAt = r.clock()
	}

	out.Class = ClassUnresolved
	out.Produced = activity.StatusOutstanding
	out.Detail = "the process did not survive the call; " + stranded
	res2 := r.write(ctx, out, repaired, rec, MoneyNone, 0, quoteRef{})
	// The write repaired the record's state, but the accounting is still unresolved
	// and must be counted as such. Calling this "repaired" would let a summary imply
	// the money was accounted for.
	res2.Class = ClassUnresolved
	res2.Reason = ReasonProviderOutcomeUnknown
	return res2
}

// markPricingUnresolved records that usage is known and cannot be priced by the
// quote this request was admitted under.
func (r *Reconciler) markPricingUnresolved(ctx context.Context, out Result, rec activity.Record, cost usage.Cost) Result {
	out.Class = ClassUnresolved
	out.Reason = ReasonPricingUnresolved
	out.Amount = rec.Reserved
	detail := fmt.Sprintf("usage is known and the quote captured at admission cannot price it (%s); "+
		"%s stays encumbered", cost.Reason, rec.Reserved.CentsString())

	if rec.Status == activity.StatusUnresolved {
		out.Detail = detail
		return out
	}

	repaired := rec
	repaired.Status = activity.StatusUnresolved
	repaired.ActualCost = cost
	if repaired.Outcome == "" {
		repaired.Outcome = activity.OutcomeUnpriced
	}
	if repaired.CompletedAt.IsZero() {
		repaired.CompletedAt = r.clock()
	}
	out.Produced = activity.StatusUnresolved
	out.Detail = detail
	res := r.write(ctx, out, repaired, rec, MoneyNone, 0, quoteRef{})
	res.Class = ClassUnresolved
	res.Reason = ReasonPricingUnresolved
	return res
}

// write persists a repaired record with its audit entry.
//
// The entry is appended, never overwritten, and the observed state is recorded
// before the repair replaces it: the state a reconciler found is not recoverable
// from the row afterwards, and it is the first thing anybody investigating a crash
// asks for.
func (r *Reconciler) write(ctx context.Context, out Result, repaired, observed activity.Record, moved Money, amount money.Money, ref quoteRef) Result {
	if r.dryRun {
		return out
	}
	repaired.Repairs = append(append([]activity.Reconciliation(nil), observed.Repairs...), activity.Reconciliation{
		At:                  r.clock(),
		Class:               string(out.Class),
		Reason:              string(out.Reason),
		ObservedStatus:      observed.Status,
		ObservedReservation: observed.ReservationID,
		ProducedStatus:      repaired.Status,
		Money:               string(moved),
		Amount:              amount,
		QuoteSource:         ref.source,
		QuoteVersion:        ref.version,
		QuoteCapturedAt:     ref.capturedAt,
		Detail:              out.Detail,
	})
	if err := r.activity.Complete(ctx, repaired); err != nil {
		// The money is right and the record is not. Reporting this as repaired would
		// claim a consistency that does not exist; the next pass sees the ledger's
		// authoritative state and repairs the record from it.
		out.Class = ClassFailed
		out.Err = err
		out.Detail = fmt.Sprintf("%s, but the activity record could not be updated: %v", out.Detail, err)
		return out
	}
	return out
}

func fail(out Result, err error) Result {
	out.Class = ClassFailed
	out.Err = err
	out.Detail = err.Error()
	return out
}

// awaitingExternal reports whether this record's cost is waiting on an observation
// that arrives out of band.
//
// The test is deliberately structural: the adapter reached a terminal unresolved
// conclusion, a runtime session was recorded, its resource usage has not been
// attributed, and the cost is not known. Nothing about which provider or which
// operation appears here — an adapter that records the same linkage gets the same
// treatment, and a hosted runtime invocation is not mistaken for a crashed
// transaction.
//
// The status clause is what separates the two. An unresolved status means the
// adapter observed the invocation end and concluded the cost is reported elsewhere.
// A pending one means the process died before it could conclude anything, and
// whether the runtime ran at all is then unknown — crash damage, not a wait.
func awaitingExternal(rec activity.Record) bool {
	return rec.Status == activity.StatusUnresolved &&
		rec.Runtime.SessionID != "" && !rec.Runtime.Reconciled && !rec.ActualCost.Known()
}

// releasable reports whether the record durably proves no billable usage occurred.
//
// Every clause matters. A known zero cost is the adapter's own measurement that the
// provider refused the call before it ran; empty usage confirms nothing was
// reported; and the status is the adapter's conclusion, written durably before the
// ledger release it did not survive to complete.
func releasable(rec activity.Record) bool {
	return rec.Status == activity.StatusReleased &&
		rec.ActualCost.Known() && rec.ActualCost.Amount == 0 &&
		rec.ActualUsage.Empty()
}

// quoteRef identifies the immutable quote a replayed settlement priced with, for
// the audit trail. It is provenance, not rates: the rates live in the record.
type quoteRef struct {
	source     string
	version    string
	capturedAt time.Time
}

// replay prices durably recorded usage under the quote frozen at admission.
//
// This function is the reason a catalog is absent from this package. A historical
// request is priced by the rates it was admitted under, so a price change between
// the crash and the repair cannot alter what the request cost — and because there
// is no catalog to consult, that is not a rule this code follows but a fact about
// what it is able to do.
//
// A compound request is priced from its recorded steps under the frozen quote set,
// with the same single-rounding boundary settlement uses, so a repaired agent turn
// is charged the same amount an uninterrupted one would have been.
func replay(rec activity.Record) (usage.Cost, quoteRef) {
	if rec.ActualUsage.Empty() && len(rec.Agent.Steps) == 0 {
		// Nothing observed. Pricing an empty usage map under a valid quote would total
		// zero and call it known, which is precisely the manufactured cost this package
		// exists to refuse.
		return usage.UnknownCost("no usage was durably recorded"), quoteRef{}
	}

	if rec.Quotes.Valid() {
		components := make([]pricing.Component, 0, len(rec.Agent.Steps))
		for _, step := range rec.Agent.Steps {
			components = append(components, pricing.Component{
				Identity: step.Identity,
				Usage:    step.Usage,
			})
		}
		if len(components) == 0 {
			return usage.UnknownCost("a compound request recorded no model invocations to price"), quoteRef{}
		}
		cost, _, _ := rec.Quotes.PriceComponents(components)
		return cost, setRef(rec.Quotes)
	}

	if !rec.Quote.Valid() {
		return usage.UnknownCost("no quote was captured for this request, so its usage cannot be priced"), quoteRef{}
	}
	q := rec.Quote.For(rec.Identity)
	priced, _ := q.Price(rec.ActualUsage)
	return priced.Cost, quoteRef{
		source:     q.Provenance.Source,
		version:    q.Provenance.Version,
		capturedAt: q.CapturedAt,
	}
}

// setRef takes the provenance of a quote set from its first member in sorted order,
// so the audit entry names a source deterministically. Members of one set come from
// one read of one catalog, so any of them describes the set's provenance.
func setRef(s pricing.QuoteSet) quoteRef {
	ref := quoteRef{capturedAt: s.CapturedAt}
	models := s.Models()
	sort.Strings(models)
	for _, m := range models {
		q := s.Quotes[m]
		ref.source = q.Provenance.Source
		ref.version = q.Provenance.Version
		break
	}
	return ref
}
