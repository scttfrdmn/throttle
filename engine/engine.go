// Package engine composes budget definitions with a durable ledger into the
// operational transaction: estimate -> reserve -> execute -> reconcile.
//
// The engine is provider-neutral. It never imports a provider SDK; adapters
// hand it normalized estimates and observed usage.
//
// # Authority
//
// The engine's admission calculation is advisory. It reads each scope's totals,
// applies the pacing curve, and forms an opinion. The ledger is authoritative:
// it re-checks every ceiling inside the transaction that writes the hold. When
// the two disagree -- because a concurrent request took the headroom in between
// -- the engine refreshes and recomputes rather than assuming the reason.
//
// # Hierarchy
//
// A request against a budget consumes its ancestors too. The engine computes one
// ceiling per budget in the chain from that budget's own pacing curve, and the
// ledger reserves against all of them atomically. The strictest link governs.
package engine

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"throttle/budget"
	"throttle/ledger"
	"throttle/money"
	"throttle/usage"
)

// Mode is the enforcement posture for a budget.
type Mode string

const (
	// ModeMonitor records and reports but never blocks. Useful for observing a
	// workload before governing it.
	ModeMonitor Mode = "monitor"

	// ModeEnforce denies requests that do not fit the envelope.
	ModeEnforce Mode = "enforce"

	// ModeWait behaves like ModeEnforce but lets callers block until a request
	// becomes affordable, via Engine.Wait.
	ModeWait Mode = "wait"
)

// strictness orders modes so that a hierarchy with mixed postures resolves to
// the strictest one. A monitored child inside an enforced parent is still
// spending the parent's real money, so the parent's posture must win.
func strictness(m Mode) int {
	switch m {
	case ModeMonitor:
		return 0
	case ModeWait:
		return 1
	default:
		return 2
	}
}

// Errors returned by the engine.
var (
	// ErrDenied means the request cannot be admitted.
	ErrDenied = errors.New("engine: request denied by budget policy")

	// ErrBudgetNotFound means no definition is stored under the given ID.
	ErrBudgetNotFound = errors.New("engine: budget not found")

	// ErrWaitTooLong means the request would become affordable, but not within
	// the caller's deadline or the configured maximum wait.
	ErrWaitTooLong = errors.New("engine: wait exceeds the allowed limit")

	// ErrCostUnknown means the request could not be priced, so an enforced budget
	// has no monetary exposure to check it against.
	//
	// Enforcing a dollar budget requires knowing how many dollars are at stake.
	// When the price is unknown, an enforced budget denies: the alternative is to
	// admit unbounded spend while every report shows the budget untouched. A
	// monitored budget admits the same request and records the cost as explicitly
	// unpriced, because monitor mode's job is to observe rather than to prevent.
	ErrCostUnknown = errors.New("engine: cost is unknown")

	// ErrCostUnresolved means a completed request's actual cost could not be fully
	// determined, so its reservation stays encumbered pending reconciliation.
	ErrCostUnresolved = errors.New("engine: actual cost is unresolved")
)

// Clock supplies the current time. Tests inject a deterministic clock; the
// engine never calls time.Now directly, so pacing behavior is reproducible.
type Clock func() time.Time

// Config configures an engine.
type Config struct {
	Ledger ledger.Ledger

	// Clock defaults to time.Now in UTC.
	Clock Clock

	// Lease is how long a hold blocks headroom before recovery can reclaim it.
	// A request that outlives its lease should renew rather than assume a longer
	// one, so this is a renewal quantum and not a prediction of request duration.
	Lease time.Duration

	// MaxWait caps how long Wait will block. Zero means no cap.
	MaxWait time.Duration

	// WaitPoll bounds how long Wait sleeps between re-evaluations, so a release
	// elsewhere can let a waiter proceed before the pacing curve alone would.
	WaitPoll time.Duration
}

const (
	// DefaultLease is generous enough for long streaming or agent calls while
	// still guaranteeing that a crashed process frees headroom eventually.
	DefaultLease = 15 * time.Minute

	// DefaultWaitPoll is the re-evaluation interval for Wait. Short enough that a
	// freed hold is noticed promptly, long enough not to hammer the ledger.
	DefaultWaitPoll = 500 * time.Millisecond
)

// Engine governs spending against budget definitions stored in the ledger.
type Engine struct {
	ledger   ledger.Ledger
	clock    Clock
	lease    time.Duration
	maxWait  time.Duration
	waitPoll time.Duration

	// mu guards modes. Enforcement posture is deliberately not part of the stored
	// definition: it is how this process chooses to react, not an accounting fact
	// about the budget. Whether that should be durable is an open question.
	mu    sync.RWMutex
	modes map[string]Mode
}

// New builds an engine. It returns an error if the configuration is unusable.
func New(cfg Config) (*Engine, error) {
	if cfg.Ledger == nil {
		return nil, errors.New("engine: a ledger is required")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	lease := cfg.Lease
	if lease == 0 {
		lease = DefaultLease
	}
	if lease < 0 {
		return nil, errors.New("engine: lease cannot be negative")
	}
	if cfg.MaxWait < 0 {
		return nil, errors.New("engine: max wait cannot be negative")
	}
	if cfg.WaitPoll < 0 {
		return nil, errors.New("engine: wait poll interval cannot be negative")
	}
	poll := cfg.WaitPoll
	if poll == 0 {
		poll = DefaultWaitPoll
	}
	return &Engine{
		ledger:   cfg.Ledger,
		clock:    clock,
		lease:    lease,
		maxWait:  cfg.MaxWait,
		waitPoll: poll,
		modes:    make(map[string]Mode),
	}, nil
}

// Now exposes the engine's clock, so callers and tests share one notion of time.
func (e *Engine) Now() time.Time { return e.clock() }

// Register stores a budget definition and records this process's enforcement
// posture for it.
//
// The definition is durable and shared: registering one whose semantics differ
// from the stored definition fails with ledger.ErrDefinitionConflict rather than
// overwriting it, so two processes cannot govern the same spend by different
// rules. Changing the rules is Update.
func (e *Engine) Register(ctx context.Context, def budget.Definition, mode Mode) error {
	switch mode {
	case ModeMonitor, ModeEnforce, ModeWait:
	case "":
		mode = ModeEnforce
	default:
		return fmt.Errorf("engine: unknown enforcement mode %q", mode)
	}
	if err := e.ledger.PutDefinition(ctx, def); err != nil {
		return err
	}
	e.mu.Lock()
	e.modes[def.ID] = mode
	e.mu.Unlock()
	return nil
}

// Update replaces a stored definition, stating the revision it expects to
// replace. Periods already materialized keep their snapshot, so an update
// affects future periods only.
func (e *Engine) Update(ctx context.Context, def budget.Definition, expectRevision int, mode Mode) error {
	if err := e.ledger.UpdateDefinition(ctx, def, expectRevision); err != nil {
		return err
	}
	if mode != "" {
		e.mu.Lock()
		e.modes[def.ID] = mode
		e.mu.Unlock()
	}
	return nil
}

// SetMode changes the enforcement posture for a budget without touching its
// definition.
func (e *Engine) SetMode(budgetID string, mode Mode) error {
	switch mode {
	case ModeMonitor, ModeEnforce, ModeWait:
	default:
		return fmt.Errorf("engine: unknown enforcement mode %q", mode)
	}
	e.mu.Lock()
	e.modes[budgetID] = mode
	e.mu.Unlock()
	return nil
}

// mode returns the posture for a budget, defaulting to enforcement. Defaulting
// to enforce means a budget this process has not explicitly registered is
// governed rather than silently unmetered.
func (e *Engine) mode(budgetID string) Mode {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if m, ok := e.modes[budgetID]; ok {
		return m
	}
	return ModeEnforce
}

// Definition returns a stored definition and its revision.
func (e *Engine) Definition(ctx context.Context, budgetID string) (budget.Definition, int, error) {
	def, revision, err := e.ledger.Definition(ctx, budgetID)
	if errors.Is(err, ledger.ErrBudgetNotFound) {
		return budget.Definition{}, 0, fmt.Errorf("%w: %q", ErrBudgetNotFound, budgetID)
	}
	return def, revision, err
}

// Link is one budget's position within a request's constraint chain.
type Link struct {
	Definition budget.Definition
	Period     ledger.Period
	Totals     ledger.Totals
	Mode       Mode
	Snapshot   budget.Snapshot

	// Decision is this link's isolated verdict on the estimate. It is zero when
	// the chain was evaluated without an estimate.
	Decision budget.Decision
}

// evaluate resolves the constraint chain for a budget: every ancestor, its
// current period, and its committed totals.
//
// The chain comes from the ledger rather than from the caller, so a request
// cannot escape an ancestor's cap by failing to mention it.
func (e *Engine) evaluate(ctx context.Context, budgetID string, now time.Time, estimate money.Money) ([]Link, error) {
	chain, err := e.ledger.Chain(ctx, budgetID)
	if errors.Is(err, ledger.ErrBudgetNotFound) {
		return nil, fmt.Errorf("%w: %q", ErrBudgetNotFound, budgetID)
	}
	if err != nil {
		return nil, err
	}

	links := make([]Link, 0, len(chain))
	for _, def := range chain {
		p, err := e.ledger.EnsurePeriod(ctx, def.ID, now)
		if err != nil {
			return nil, fmt.Errorf("engine: period for %q: %w", def.ID, err)
		}
		tot, err := e.ledger.Totals(ctx, ledger.Scope{BudgetID: def.ID, PeriodID: p.ID}, now)
		if err != nil {
			return nil, fmt.Errorf("engine: totals for %q: %w", def.ID, err)
		}
		links = append(links, Link{
			Definition: def,
			Period:     p,
			Totals:     tot,
			Mode:       e.mode(def.ID),
			Snapshot:   p.Envelope.Snapshot(now, tot.Spent, tot.Reserved),
			Decision:   p.Envelope.Admit(now, tot.Spent, tot.Reserved, estimate),
		})
	}
	return links, nil
}

// Status is the answer to "where does this budget stand right now?"
type Status struct {
	BudgetID string
	Mode     Mode

	// Period is the materialized period the position is measured in. Its
	// Provisional flag reports whether the carry may still be revised upward
	// because the preceding period is still draining.
	Period ledger.Period

	Snapshot budget.Snapshot

	// ReservedExpired and ExpiredCount surface holds awaiting recovery, so
	// stranded headroom is visible rather than silent.
	ReservedExpired money.Money
	ExpiredCount    int
	PendingCount    int

	// ProjectedSpend extrapolates end-of-period spend from the current burn rate.
	ProjectedSpend money.Money
}

// Status reports the current position of a budget. It answers the milestone
// questions: what should have been spent, how much is banked or borrowed, and
// how much can be committed right now.
func (e *Engine) Status(ctx context.Context, budgetID string) (Status, error) {
	now := e.clock()
	p, err := e.ledger.EnsurePeriod(ctx, budgetID, now)
	if errors.Is(err, ledger.ErrBudgetNotFound) {
		return Status{}, fmt.Errorf("%w: %q", ErrBudgetNotFound, budgetID)
	}
	if err != nil {
		return Status{}, err
	}
	tot, err := e.ledger.Totals(ctx, ledger.Scope{BudgetID: budgetID, PeriodID: p.ID}, now)
	if err != nil {
		return Status{}, fmt.Errorf("engine: totals for %q: %w", budgetID, err)
	}
	return e.status(budgetID, p, tot, now), nil
}

func (e *Engine) status(budgetID string, p ledger.Period, tot ledger.Totals, now time.Time) Status {
	snap := p.Envelope.Snapshot(now, tot.Spent, tot.Reserved)
	return Status{
		BudgetID:        budgetID,
		Mode:            e.mode(budgetID),
		Period:          p,
		Snapshot:        snap,
		ReservedExpired: tot.ReservedExpired,
		ExpiredCount:    tot.ExpiredCount,
		PendingCount:    tot.PendingCount,
		ProjectedSpend:  project(p.Envelope, snap),
	}
}

// StatusChain reports the position of a budget and every ancestor, nearest
// first. A leaf with room can still be blocked by an ancestor, so the whole
// chain is what explains a decision.
func (e *Engine) StatusChain(ctx context.Context, budgetID string) ([]Status, error) {
	now := e.clock()
	links, err := e.evaluate(ctx, budgetID, now, 0)
	if err != nil {
		return nil, err
	}
	out := make([]Status, len(links))
	for i, l := range links {
		out[i] = e.status(l.Definition.ID, l.Period, l.Totals, now)
	}
	return out, nil
}

// project extrapolates end-of-period spend by scaling observed spend over
// elapsed time across the full period. Before any time has elapsed there is no
// rate to extrapolate, so the projection is just what has been committed.
func project(env budget.Envelope, s budget.Snapshot) money.Money {
	elapsed := env.Elapsed(s.At)
	if elapsed <= 0 {
		return s.Committed
	}
	total := env.Duration()
	if elapsed >= total {
		return s.Spent
	}
	// spent * total / elapsed. The intermediate product of a money amount and a
	// nanosecond duration far exceeds int64 (a $600 spend over 30 days is ~1e24),
	// so this is computed exactly in big.Int rather than saturating.
	n := new(big.Int).Mul(big.NewInt(int64(s.Spent)), big.NewInt(int64(total)))
	n.Quo(n, big.NewInt(int64(elapsed)))
	if !n.IsInt64() {
		return money.Max
	}
	return money.Money(n.Int64())
}

// Decision extends the budget decision with ledger-level context.
type Decision struct {
	budget.Decision

	// BudgetID is the budget the request named.
	BudgetID string

	// Mode is the effective posture: the strictest in the chain.
	Mode Mode

	// BindingBudgetID names the budget whose constraint produced this outcome. It
	// is the leaf's own ID on an allow, and may be an ancestor otherwise, which is
	// the difference between "you are out of money" and "your parent is".
	BindingBudgetID string

	// Admitted reports whether the caller may proceed. In monitor mode this is
	// true even when the envelope is exceeded, because monitor mode never blocks.
	Admitted bool

	// CostUnknown reports that the request was admitted without a price. Only
	// possible under monitor mode, and it means the hold is zero because no amount
	// could be determined -- not because the request costs nothing.
	CostUnknown bool
}

// combine reduces per-scope verdicts to the one the caller experiences.
//
// Only budgets that are actually being enforced can bind the outcome: a monitored
// budget records spend and reports its position, but it must never be the reason a
// request is refused, at any depth. When every budget in the chain is monitored
// there is nothing to enforce, so the strictest verdict is still reported -- an
// honest statement of the position -- alongside admission.
//
// Among the binding candidates the strictest verdict wins, and among waiting ones
// the latest retry time, because a request is affordable only once every
// constraint permits it.
func combine(links []Link, budgetID string, estimate money.Money) Decision {
	rank := map[budget.Outcome]int{
		budget.OutcomeAllow: 0,
		budget.OutcomeWait:  1,
		budget.OutcomeDeny:  2,
	}

	dec := Decision{BudgetID: budgetID, Mode: ModeMonitor}
	enforced := false
	for _, l := range links {
		if strictness(l.Mode) > strictness(dec.Mode) {
			dec.Mode = l.Mode
		}
		if l.Mode != ModeMonitor {
			enforced = true
		}
	}

	binding := -1
	for i, l := range links {
		if enforced && l.Mode == ModeMonitor {
			continue
		}
		switch {
		case binding < 0,
			rank[l.Decision.Outcome] > rank[links[binding].Decision.Outcome]:
			binding = i
		case l.Decision.Outcome == links[binding].Decision.Outcome &&
			l.Decision.Outcome == budget.OutcomeWait &&
			l.Decision.RetryAt.After(links[binding].Decision.RetryAt):
			binding = i
		}
	}
	if binding < 0 {
		// No links means no such budget; callers reach this only through a bug.
		dec.Outcome = budget.OutcomeDeny
		dec.Reason = "no budget constraints resolved"
		dec.Estimate = estimate
		return dec
	}

	l := links[binding]
	dec.Decision = l.Decision
	dec.BindingBudgetID = l.Definition.ID
	dec.Admitted = !enforced || dec.Outcome == budget.OutcomeAllow
	if dec.Outcome != budget.OutcomeAllow && l.Definition.ID != budgetID {
		dec.Reason = fmt.Sprintf("%s (limited by parent budget %q)", dec.Reason, l.Definition.ID)
	}
	return dec
}

// Check evaluates a request without reserving anything. It is the read-only
// preflight: use Begin to actually hold headroom. Because it reserves nothing,
// its answer can be stale by the time the caller acts on it.
func (e *Engine) Check(ctx context.Context, budgetID string, estimate money.Money) (Decision, error) {
	now := e.clock()
	links, err := e.evaluate(ctx, budgetID, now, estimate)
	if err != nil {
		return Decision{}, err
	}
	return combine(links, budgetID, estimate), nil
}

// CheckChain is Check with the per-scope detail retained, for explaining a
// decision rather than merely making it.
func (e *Engine) CheckChain(ctx context.Context, budgetID string, estimate money.Money) (Decision, []Link, error) {
	now := e.clock()
	links, err := e.evaluate(ctx, budgetID, now, estimate)
	if err != nil {
		return Decision{}, nil, err
	}
	return combine(links, budgetID, estimate), links, nil
}

// Request describes a governed call the caller is about to make.
type Request struct {
	BudgetID string

	// RequestID is the caller's identifier for the underlying call.
	RequestID string

	// ReservationID must be unique per attempt. When empty the engine derives one
	// from RequestID, which makes a retry idempotent rather than double-holding.
	ReservationID string

	// Estimate is the predicted cost, normally from a provider adapter.
	Estimate usage.Estimate

	Identity usage.ModelIdentity
	Metadata map[string]string
}

func (r Request) reservationID() string {
	if r.ReservationID != "" {
		return r.ReservationID
	}
	return "res-" + r.RequestID
}

// Transaction is an admitted request holding budget headroom across its whole
// scope chain. Exactly one of Settle or Release must be called, or the hold
// lingers until its lease lapses and recovery reclaims it.
type Transaction struct {
	engine   *Engine
	decision Decision

	mu          sync.Mutex
	reservation ledger.Reservation
	resolved    bool

	// unresolved marks a completed request whose cost could not be determined. Its
	// hold stays pending on purpose: see MarkUnresolved.
	unresolved bool
	actual     usage.Actual
}

// Decision returns the admission decision that authorized this transaction.
func (t *Transaction) Decision() Decision { return t.decision }

// Reservation returns the underlying hold, including one leg per scope it
// consumes.
func (t *Transaction) Reservation() ledger.Reservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reservation
}

// Begin admits a request and atomically reserves its estimated cost against
// every budget in its chain.
//
// In ModeMonitor a hold is still recorded so that concurrent requests see each
// other, but a request over the envelope is admitted rather than denied.
func (e *Engine) Begin(ctx context.Context, req Request) (*Transaction, Decision, error) {
	if req.BudgetID == "" {
		return nil, Decision{}, errors.New("engine: budget id is required")
	}
	if req.RequestID == "" && req.ReservationID == "" {
		return nil, Decision{}, errors.New("engine: request id or reservation id is required")
	}

	// An unpriced estimate has nothing to reserve, and what to do about that
	// depends on posture -- so the chain is evaluated first, with a zero estimate,
	// purely to learn the effective mode. Enforce cannot honestly govern spend it
	// cannot measure and denies; monitor admits and records the cost as unpriced.
	// Neither ever substitutes a guess or a zero amount.
	priced := req.Estimate.Cost.Known()
	estimate := money.Money(0)
	if priced {
		estimate = req.Estimate.Cost.Amount
		if estimate < 0 {
			return nil, Decision{}, errors.New("engine: estimated cost cannot be negative")
		}
	}

	now := e.clock()
	links, err := e.evaluate(ctx, req.BudgetID, now, estimate)
	if err != nil {
		return nil, Decision{}, err
	}
	dec := combine(links, req.BudgetID, estimate)

	if !priced {
		reason := req.Estimate.Cost.Reason
		if reason == "" {
			reason = "no cost estimate was supplied"
		}
		if dec.Mode != ModeMonitor {
			dec.Admitted = false
			dec.Outcome = budget.OutcomeDeny
			dec.Reason = "cost is unknown: " + reason
			if dec.BindingBudgetID == "" {
				dec.BindingBudgetID = req.BudgetID
			}
			return nil, dec, fmt.Errorf("%w: %s", ErrCostUnknown, reason)
		}
		// Monitor mode admits it. The hold is zero because there is no amount to
		// hold, not because the request is free -- the activity record carries the
		// unpriced cost so the gap is visible rather than implied.
		dec.CostUnknown = true
	}

	if !dec.Admitted {
		return nil, dec, fmt.Errorf("%w: %s", ErrDenied, dec.Reason)
	}

	identity := req.Identity
	if identity == (usage.ModelIdentity{}) {
		identity = req.Estimate.Identity
	}
	var expiresAt time.Time
	if e.lease > 0 {
		expiresAt = now.Add(e.lease)
	}

	res, err := e.ledger.Reserve(ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID:            req.reservationID(),
			BudgetID:      req.BudgetID,
			RequestID:     req.RequestID,
			Amount:        estimate,
			EstimatedCost: estimate,
			CreatedAt:     now,
			ExpiresAt:     expiresAt,
			Lease:         e.lease,
			Identity:      identity,
			Metadata:      req.Metadata,
		},
		Ceilings: e.ceilings(links, now),
		Now:      now,
	})
	if err != nil {
		out, refusal := e.refuse(ctx, req.BudgetID, estimate, dec, err)
		return nil, out, refusal
	}
	return &Transaction{engine: e, reservation: res, decision: dec}, dec, nil
}

// ceilings is the cumulative Spent+Reserved each budget may reach, one entry per
// scope the ledger will check. A monitored budget gets an unlimited ceiling,
// because monitor mode must observe overspend rather than prevent it.
func (e *Engine) ceilings(links []Link, now time.Time) map[string]money.Money {
	out := make(map[string]money.Money, len(links))
	for _, l := range links {
		if l.Mode == ModeMonitor {
			out[l.Definition.ID] = money.Max
			continue
		}
		out[l.Definition.ID] = l.Period.Envelope.Allowed(now)
	}
	return out
}

// refuse turns a ledger refusal into the decision and error the caller should see.
//
// The engine's own calculation was advisory: if the ledger rejected a hold the
// engine expected to succeed, the state changed underneath it. Re-evaluating
// produces the real outcome -- which may be deny -- rather than assuming a wait
// would help. Either way the caller gets ErrDenied, so a lost race is handled the
// same way as an up-front refusal instead of leaking as a storage-layer error.
func (e *Engine) refuse(ctx context.Context, budgetID string, estimate money.Money, dec Decision, cause error) (Decision, error) {
	switch {
	case errors.Is(cause, ledger.ErrInsufficientHeadroom):
	case errors.Is(cause, ledger.ErrPeriodClosed):
	default:
		// Duplicate IDs, invalid arguments and storage failures are not headroom
		// verdicts, so the advisory decision and the original error both stand.
		return dec, cause
	}

	fresh, err := e.evaluate(ctx, budgetID, e.clock(), estimate)
	if err != nil {
		// The refreshed read failed, so the only honest report is that the ledger
		// refused without a recomputed reason.
		dec.Admitted = false
		if dec.Outcome == budget.OutcomeAllow {
			dec.Outcome = budget.OutcomeDeny
			dec.Reason = "the ledger refused the reservation"
		}
		return dec, fmt.Errorf("%w: %s: %w", ErrDenied, dec.Reason, cause)
	}

	out := combine(fresh, budgetID, estimate)
	out.Admitted = false
	if out.Outcome == budget.OutcomeAllow {
		// Headroom exists again already: the loser of a race should retry rather
		// than treat a transient refusal as a verdict.
		out.Outcome = budget.OutcomeWait
		out.RetryAt = e.clock()
		out.Reason = "headroom was taken by a concurrent request"
	}
	return out, fmt.Errorf("%w: %s: %w", ErrDenied, out.Reason, cause)
}

// Settle reconciles the transaction with the actual observed cost, applying it
// to exactly the scopes the hold reserved.
//
// The actual cost is authoritative even when it exceeds the reservation: the
// overrun is recorded rather than hidden.
//
// An actual cost that is not fully known does not settle. It is marked unresolved
// instead: see MarkUnresolved. Settling a partial amount as though it were the
// total would understate real spend in a way no later reader could detect.
func (t *Transaction) Settle(ctx context.Context, actual usage.Actual) (ledger.Charge, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resolved {
		return ledger.Charge{}, ledger.ErrAlreadyResolved
	}
	if !actual.Cost.Known() {
		reason := actual.Cost.Reason
		if reason == "" {
			reason = "the provider reported usage that could not be priced"
		}
		return ledger.Charge{}, fmt.Errorf("%w: %s", ErrCostUnresolved, reason)
	}

	c, err := t.engine.ledger.Settle(ctx, ledger.Settlement{
		ReservationID: t.reservation.ID,
		ActualCost:    actual.Cost.Amount,
		Usage:         actual,
		CompletedAt:   t.engine.clock(),
	})
	if err != nil {
		return ledger.Charge{}, err
	}
	t.resolved = true
	return c, nil
}

// MarkUnresolved records that a completed request's cost could not be determined,
// leaving its reservation encumbered.
//
// This is the settlement path for a request that really ran and really cost money
// throttle cannot yet name -- because the provider billed a dimension the captured
// quote has no rate for, or because the model was never priced at all.
//
// Every other outcome would be a lie. Releasing the hold would claim the money
// back as available when it has already been spent. Settling at zero, or at the
// partial total of only the dimensions that happened to be priced, would report a
// number that is definitely too low as though it were right. So the hold stays:
// the reserved amount remains encumbered headroom, the usage is preserved, and the
// request is visible as owing a price.
//
// The reservation keeps its lease. An unresolved liability that outlives its lease
// stops blocking headroom the same way any other expired hold does -- a crashed
// process must not freeze a budget forever -- and remains settleable once pricing
// arrives.
func (t *Transaction) MarkUnresolved(ctx context.Context, actual usage.Actual) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resolved {
		return ledger.ErrAlreadyResolved
	}
	if actual.Cost.Known() {
		return errors.New("engine: cost is known; settle it rather than marking it unresolved")
	}
	// Deliberately no ledger write. The pending reservation IS the record of
	// encumbrance, and the activity record carries the usage and the unpriced
	// dimensions. Inventing an "unresolved" ledger state would mean a second way
	// for money to be outstanding, and the lease mechanism already covers it.
	t.unresolved = true
	t.actual = actual
	return nil
}

// Unresolved reports whether this transaction ended with a cost it could not
// determine, leaving its reservation encumbered.
func (t *Transaction) Unresolved() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unresolved
}

// Release returns the hold, across its whole scope chain, without recording a
// charge. It must only be used when no billable usage occurred; if the provider
// billed for a failed call, settle the real amount instead.
func (t *Transaction) Release(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resolved {
		return ledger.ErrAlreadyResolved
	}
	if t.unresolved {
		// The request ran and incurred cost. Freeing the hold would report the
		// money as available again when it has already been spent.
		return fmt.Errorf("%w: cannot release an unresolved liability", ErrCostUnresolved)
	}
	if err := t.engine.ledger.Release(ctx, t.reservation.ID); err != nil {
		return err
	}
	t.resolved = true
	return nil
}

// Renew extends the hold's lease so a long-running request does not lose its
// headroom.
//
// Callers that stream, poll, or invoke agents should renew periodically: the
// lease is a crash-recovery mechanism, not a prediction of how long the request
// takes. It re-checks nothing, because the amount held has not changed.
func (t *Transaction) Renew(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resolved {
		return ledger.ErrAlreadyResolved
	}
	res, err := t.engine.ledger.Renew(ctx, ledger.RenewRequest{
		ReservationID: t.reservation.ID,
		Now:           t.engine.clock(),
		Extend:        t.engine.lease,
	})
	if err != nil {
		return err
	}
	t.reservation = res
	return nil
}

// KeepAlive renews the hold every interval until ctx is cancelled or the
// transaction resolves. It is the convenience form of Renew for a caller that
// has nowhere natural to renew from, such as a blocking streaming read.
//
// It returns the error that stopped it, or nil if ctx ended or the transaction
// resolved normally.
func (t *Transaction) KeepAlive(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("engine: keep-alive interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			err := t.Renew(ctx)
			switch {
			case err == nil:
			case errors.Is(err, ledger.ErrAlreadyResolved):
				return nil // The request finished; nothing left to keep alive.
			default:
				return err
			}
		}
	}
}

// Wait blocks until the estimate becomes affordable, then returns.
//
// It answers "if it cannot be spent now but fits the period, when does it become
// affordable?" operationally. Waiting is bounded re-evaluation rather than a
// sleep until RetryAt: headroom can also appear because another request released
// or settled under its estimate, and a waiter must be able to notice that.
//
// There is no fairness queue. Several waiters may wake together and race, and
// the caller still races non-waiting callers, so Begin can fail after Wait
// returns.
func (e *Engine) Wait(ctx context.Context, budgetID string, estimate money.Money) error {
	start := e.clock()
	for {
		dec, err := e.Check(ctx, budgetID, estimate)
		if err != nil {
			return err
		}
		switch dec.Outcome {
		case budget.OutcomeAllow:
			return nil
		case budget.OutcomeDeny:
			return fmt.Errorf("%w: %s", ErrDenied, dec.Reason)
		}

		now := e.clock()
		projected := dec.Wait(now)

		// The limits are checked against the projected wait, not the poll interval,
		// so a caller learns immediately that a request is days away instead of
		// discovering it one poll at a time.
		if e.maxWait > 0 && projected > e.maxWait-now.Sub(start) {
			return fmt.Errorf("%w: %v exceeds the %v limit", ErrWaitTooLong, projected, e.maxWait)
		}
		// A context deadline is wall-clock, so it is compared as a remaining
		// duration rather than against an instant on the engine's clock. The two
		// agree in production and this keeps the comparison meaningful when a test
		// or simulation supplies its own clock.
		if deadline, ok := ctx.Deadline(); ok && projected > time.Until(deadline) {
			return fmt.Errorf("%w: %v exceeds the context deadline", ErrWaitTooLong, projected)
		}

		delay := projected
		if delay > e.waitPoll || delay <= 0 {
			delay = e.waitPoll
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Advance brings a budget's periods up to date: elapsed periods stop accepting
// work, drained ones close, carry is finalized, and the period containing now is
// materialized so a recurring budget nobody has touched since a boundary becomes
// current. It is idempotent.
//
// The returned periods are those whose state this call changed, in sequence order.
// It is reported by comparing before and after rather than by trusting one code
// path, because materializing a period also transitions it: a budget untouched for
// two months fills the gap and closes those periods in a single step, and a caller
// asking what happened should hear about all of it.
func (e *Engine) Advance(ctx context.Context, budgetID string) ([]ledger.Period, error) {
	now := e.clock()

	before, err := e.ledger.Periods(ctx, budgetID)
	if err != nil {
		return nil, err
	}
	was := make(map[string]ledger.PeriodState, len(before))
	for _, p := range before {
		was[p.ID] = p.State
	}

	if _, err := e.ledger.Advance(ctx, budgetID, now); err != nil {
		return nil, err
	}
	// ErrNoSuchPeriod means the definition's term has ended, so there is no current
	// period to materialize. The Advance above still closed out its last one.
	if _, err := e.ledger.EnsurePeriod(ctx, budgetID, now); err != nil && !errors.Is(err, budget.ErrNoSuchPeriod) {
		return nil, err
	}

	after, err := e.ledger.Periods(ctx, budgetID)
	if err != nil {
		return nil, err
	}
	var changed []ledger.Period
	for _, p := range after {
		prior, existed := was[p.ID]
		// A newly materialized period that is already open is just the current
		// period coming into existence, not a transition.
		if (!existed && p.State != ledger.StateOpen) || (existed && prior != p.State) {
			changed = append(changed, p)
		}
	}
	return changed, nil
}

// AdvanceAll performs due period transitions for every stored budget. Running it
// periodically keeps recurring budgets current without a scheduler in the hot
// path, though reserving also advances implicitly.
func (e *Engine) AdvanceAll(ctx context.Context) ([]ledger.Period, error) {
	defs, err := e.ledger.Definitions(ctx)
	if err != nil {
		return nil, err
	}
	var out []ledger.Period
	for _, def := range defs {
		ps, err := e.Advance(ctx, def.ID)
		if err != nil {
			return out, err
		}
		out = append(out, ps...)
	}
	return out, nil
}

// Recover reclaims expired holds for a budget and its descendants so that a
// crashed process cannot strand headroom. Callers should run this periodically
// and at startup.
func (e *Engine) Recover(ctx context.Context, budgetID string) ([]ledger.Reservation, error) {
	return e.ledger.RecoverExpired(ctx, budgetID, e.clock())
}

// RecoverAll reclaims expired holds across every stored budget.
func (e *Engine) RecoverAll(ctx context.Context) ([]ledger.Reservation, error) {
	defs, err := e.ledger.Definitions(ctx)
	if err != nil {
		return nil, err
	}
	now := e.clock()
	// A hold against a child is reclaimable through any ancestor, so recovering
	// every budget would return it several times. Roots cover the whole forest.
	seen := map[string]bool{}
	var out []ledger.Reservation
	for _, def := range defs {
		if def.ParentID != "" {
			continue
		}
		rs, err := e.ledger.RecoverExpired(ctx, def.ID, now)
		if err != nil {
			return out, err
		}
		for _, r := range rs {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
	}
	return out, nil
}
