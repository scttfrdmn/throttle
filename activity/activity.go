// Package activity is the durable, content-free record of governed requests.
//
// It answers "what did we spend, on what, under which posture, and is that number
// complete?" — for every request, including the ones that failed or could not be
// priced. The ledger records money; activity records the request that moved it,
// plus the requests that moved money the ledger could not yet name.
//
// # No content
//
// Prompts, messages, and generated responses are never stored. Knowing what a
// request cost does not require knowing what it said, and a store that holds
// transcripts is a different product with different obligations. Every field here
// is a measurement or an identifier.
//
// # Provider-neutral
//
// Nothing in this package knows about any provider SDK. An adapter normalizes its
// own response and hands the result over, which is what lets one dashboard read
// spend across providers.
package activity

import (
	"time"

	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// Status is how a governed request ended.
//
// The set exists to keep "we know this cost $2.10" distinct from "we know this
// cost something we cannot name". Both are completed requests that spent real
// money; only one can be added up.
type Status string

const (
	// StatusSettled means the request completed and its full cost was recorded.
	StatusSettled Status = "settled"

	// StatusDenied means the budget refused the request. Nothing was called and
	// nothing was spent.
	StatusDenied Status = "denied"

	// StatusReleased means the request failed with no billable usage, so its hold
	// was returned.
	StatusReleased Status = "released"

	// StatusUnresolved means the request ran and incurred cost that could not be
	// determined -- an unpriced model, or a billable dimension the captured quote
	// has no rate for. Its reservation stays encumbered pending reconciliation.
	// This is the status that makes a budget total "incomplete" rather than wrong.
	StatusUnresolved Status = "unresolved"

	// StatusOutstanding means the outcome is genuinely unknown: the call was
	// interrupted and no response came back to prove whether the provider served
	// and billed it. The hold is deliberately left in place.
	StatusOutstanding Status = "outstanding"

	// StatusPending means the request is in flight. A record in this state after a
	// restart is a request whose process died mid-call.
	StatusPending Status = "pending"
)

// Outcome classifies why a request ended as it did, independently of status. A
// released hold caused by a provider error and one caused by caller cancellation
// are the same status but different operational stories.
type Outcome string

const (
	OutcomeSuccess       Outcome = "success"
	OutcomeBudgetDenied  Outcome = "budget-denied"
	OutcomeProviderError Outcome = "provider-error"
	OutcomeCancelled     Outcome = "cancelled"
	OutcomeTimeout       Outcome = "timeout"

	// OutcomeAccountingError means the provider answered but throttle could not
	// record it.
	OutcomeAccountingError Outcome = "accounting-error"

	// OutcomeUnpriced means the request succeeded and its usage is known, but its
	// cost is not.
	OutcomeUnpriced Outcome = "unpriced"
)

// Record is one governed request.
//
// It is written once when the request is admitted and updated once when it
// resolves, so a request whose process dies mid-call leaves evidence rather than
// nothing. The pre-call write is what makes a crashed request visible at all.
type Record struct {
	// RequestID is the caller's identifier and the primary key. Reusing one is how
	// a retry after an ambiguous failure updates the same record instead of
	// creating a second.
	RequestID string

	// ReservationID is the hold the request was made under. Empty for a denied
	// request, which never got one.
	ReservationID string

	// BudgetID is the budget named by the caller. Scopes lists every scope the
	// hold actually consumed, including ancestors, so attribution survives even
	// after the reservation is gone.
	BudgetID string
	Scopes   []Scope

	// Identity is what was called: access provider, publisher, exact provider
	// model ID, operation, and the access dimensions. CanonicalModel may be empty
	// for a model throttle does not recognize, which is a normal state.
	Identity usage.ModelIdentity

	// Estimate is the prediction and its quality, retained so estimate accuracy can
	// be measured against ActualUsage rather than assumed.
	Estimate usage.Estimate

	// Quote is the immutable rate set captured at admission. It is what makes this
	// record re-priceable: a later reconciliation prices ActualUsage with these
	// rates, not with whatever the catalog says today.
	Quote pricing.CapturedQuote

	// Quotes is the same guarantee for a request whose models were not knowable at
	// admission. A managed agent turn names an agent, not a model, and discovers what
	// it invoked from the response — so the whole candidate rate set is frozen in one
	// read instead of a single quote, and the subset covering the invocations that
	// actually happened is retained here.
	//
	// Exactly one of Quote and Quotes is populated: which one is a property of the
	// operation, not a fallback. Both replay frozen rates and neither consults a live
	// catalog at settlement.
	Quotes pricing.QuoteSet

	// Reserved is the amount actually held, which can differ from the estimate and
	// is zero for a request admitted in monitor mode without a price.
	Reserved money.Money

	// ActualUsage is what the provider reported consuming, including dimensions
	// throttle does not recognize. ActualCost is that usage priced, and carries its
	// own completeness: a partial cost's amount is a floor, not a total.
	ActualUsage usage.Usage
	ActualCost  usage.Cost

	// EnforcementMode is the posture that actually governed the request: the
	// strictest in the budget chain, not necessarily the named budget's own.
	//
	// Recorded because it cannot be reconstructed later. Posture is runtime policy
	// and deliberately not part of the durable budget definition, so nothing else in
	// the database remembers it -- and spend admitted under monitor mode was never
	// subject to a ceiling. A reader who cannot tell the difference will mistake
	// unenforced spend for spend that fit.
	EnforcementMode engine.Mode

	Status  Status
	Outcome Outcome

	// Error is the provider or accounting error text, when there was one. It is an
	// error message, never request or response content.
	Error string

	StartedAt   time.Time
	CompletedAt time.Time

	// Latency is the caller's wall clock, including transport and SDK retries.
	// ProviderLatency is what the provider itself reported.
	Latency         time.Duration
	ProviderLatency time.Duration

	// StreamEstablished is how long the provider took to accept a streaming request
	// and hand back a stream, and StreamFirstEvent is how long until the first event
	// arrived. Both are zero for a non-streaming request.
	//
	// They are separate from Latency because a stream's total duration says nothing
	// about its responsiveness: a slow stream that answered immediately and a fast
	// one that stalled at the start look identical otherwise. StreamFirstEvent is
	// time-to-first-token in practice, and it is a duration rather than content --
	// nothing about the event itself is recorded.
	StreamEstablished time.Duration
	StreamFirstEvent  time.Duration

	// Agent carries the compound-invocation detail when this request was a managed
	// agent turn: which agent ran, and the individual model invocations observed
	// beneath the one transaction. Zero for an ordinary single-model request.
	Agent Agent

	// Runtime carries the linkage detail when this request invoked an agent hosted on
	// a managed runtime, whose resource consumption is reported out of band and long
	// after the call. Zero for every other kind of request.
	Runtime HostedRuntime

	// Repairs is the audit trail of automatic reconciliation applied to this record
	// after the fact, oldest first.
	//
	// It is append-only. A repair that overwrote the evidence of the state it found
	// would make the postmortem of a crash impossible, which is the one question
	// anybody asks about a record that needed repairing at all.
	Repairs []Reconciliation

	// Metadata attributes spend to a workload, a principal, or anything else the
	// caller chooses. It is caller-supplied strings; adapters must not put content
	// in it.
	Metadata map[string]string
}

// Reconciliation is one automatic repair of this record's durable bookkeeping.
//
// A crash between the ledger write and the activity write leaves the two stores
// telling different stories, and repairing that is a state change nobody watched
// happen. This is the evidence: what was observed, what was produced, whether money
// moved, and which frozen quote priced it.
//
// Class and Reason are strings rather than typed constants because the reconciler
// imports this package, not the other way around. Their vocabulary is the
// reconciler's.
type Reconciliation struct {
	// At is when reconciliation ran.
	At time.Time `json:"at"`

	// Class is the monetary outcome, e.g. "repaired" or "unresolved". Reason is the
	// durable classification, e.g. "crash-repairable" or "awaiting-external-usage".
	Class  string `json:"class"`
	Reason string `json:"reason,omitempty"`

	// ObservedStatus and ObservedReservation are the state reconciliation found, and
	// ProducedStatus the state it wrote. Keeping the observed pair is the whole point
	// of the record: it is not recoverable from the row afterwards.
	ObservedStatus      Status `json:"observed_status,omitempty"`
	ObservedReservation string `json:"observed_reservation,omitempty"`
	ProducedStatus      Status `json:"produced_status,omitempty"`

	// Money is "settled", "released", or empty when the repair moved none. Amount is
	// the settled cost, valid only when Money is "settled".
	Money  string      `json:"money,omitempty"`
	Amount money.Money `json:"amount,omitempty"`

	// QuoteSource, QuoteVersion, and QuoteCapturedAt identify the immutable quote a
	// replayed settlement priced with, so it is provable that the historical rates
	// were used and not whatever the catalog said at repair time.
	QuoteSource     string    `json:"quote_source,omitempty"`
	QuoteVersion    string    `json:"quote_version,omitempty"`
	QuoteCapturedAt time.Time `json:"quote_captured_at,omitempty"`

	// Detail explains the repair in one sentence, for operators.
	Detail string `json:"detail,omitempty"`
}

// Agent is the compound detail of a managed agent invocation.
//
// A managed agent turn is ONE governed request that internally invokes a
// foundation model several times. The transaction, the reservation, and the charge
// are all singular — throttle admitted the turn, not the individual model calls,
// and inventing a child reservation for a call throttle never admitted would
// misrepresent what was governed. This type is what makes those internal calls
// visible anyway: accounting detail beneath one transaction, not transactions of
// their own.
//
// It holds identifiers, usage, cost, and timing. Everything the provider's trace
// carries beyond that — prompts, model responses, reasoning, action inputs and
// outputs, retrieved passages, collaborator messages — is deliberately absent, and
// no field here can hold it.
type Agent struct {
	// AgentID, AliasID, and Version identify what ran, as the provider named it.
	AgentID string `json:"agent_id,omitempty"`
	AliasID string `json:"alias_id,omitempty"`
	Version string `json:"version,omitempty"`

	// SessionID is the provider's conversation identifier, retained as a telemetry
	// dimension so several turns of one conversation can be grouped.
	//
	// It groups records; it does not scope money. Each invocation remains its own
	// transaction against the budget, because that is the granularity at which
	// throttle actually admitted spend.
	SessionID string `json:"session_id,omitempty"`

	// Steps are the observed internal model invocations, in the order they were
	// reported.
	Steps []AgentStep `json:"steps,omitempty"`

	// Events are non-model activities observed during the turn — a tool or action
	// invocation, a knowledge-base lookup, a guardrail evaluation — counted by kind.
	//
	// They are counts and nothing else, because that is all the provider gives:
	// these carry no billable quantity throttle can price. Their real cost lands on
	// the provider's bill outside throttle's view, and Note says so rather than
	// implying zero.
	Events map[string]int `json:"events,omitempty"`

	// Note records an accounting limitation of this invocation, e.g. that
	// unpriceable non-model activity occurred.
	Note string `json:"note,omitempty"`
}

// Empty reports whether there is no agent detail at all.
func (a Agent) Empty() bool {
	return a.AgentID == "" && a.AliasID == "" && a.SessionID == "" &&
		len(a.Steps) == 0 && len(a.Events) == 0
}

// AgentStep is one observed model invocation inside an agent turn.
type AgentStep struct {
	// Seq orders the steps within the turn, from 1.
	Seq int `json:"seq"`

	// Kind is the phase the provider attributed the invocation to, e.g.
	// preprocessing, orchestration, or postprocessing. It is the provider's own
	// vocabulary, normalized to lower case with hyphens.
	Kind string `json:"kind,omitempty"`

	// TraceID is the provider's correlation identifier for this step. It is an
	// opaque identifier, retained so a step can be tied back to a provider-side
	// trace the operator may still have. It is never content.
	TraceID string `json:"trace_id,omitempty"`

	// Collaborator names the delegated agent that performed this step, when the
	// provider reported one. It is a dimension for visibility, not a sub-budget:
	// spend still lands on the one budget that admitted the turn.
	Collaborator string `json:"collaborator,omitempty"`

	// Identity is the model this step ran on.
	//
	// ProviderModelID may be empty. A provider that reports usage for a step
	// without naming the model leaves throttle with a real charge and no identity,
	// and recording the turn's configured model instead would be a guess presented
	// as a fact. An unnamed model is a representable state; a wrong one is not.
	Identity usage.ModelIdentity `json:"identity"`

	// Usage is what this step consumed.
	Usage usage.Usage `json:"usage"`

	// Cost is this step's contribution, rounded for reporting.
	//
	// The steps may not sum exactly to the request's ActualCost, and the request's
	// figure is the authoritative one. The turn is a single charge accumulated
	// exactly and rounded once, so rounding each step here is presentation; summing
	// the presented figures would reintroduce the per-line drift that single
	// rounding exists to avoid.
	Cost usage.Cost `json:"cost"`

	// Latency is what the provider reported for this individual invocation.
	Latency time.Duration `json:"latency_ns,omitempty"`

	// At is when the provider reported the step occurring.
	At time.Time `json:"at,omitempty"`
}

// HostedRuntime is the reconciliation linkage for an invocation of an agent hosted
// on a managed runtime.
//
// # Why this type exists at all
//
// A hosted runtime executes the caller's own agent code, and bills for the compute
// that code consumes -- CPU time and memory-time -- rather than for tokens. None of
// that is in the invocation's response. It arrives later, through the platform's
// observability, keyed by identifiers the response does happen to carry.
//
// So this type holds no cost. It holds exactly the identifiers needed to recognize
// a later observation as belonging to this request, plus the outcome of the call it
// belongs to. It is a join key with provenance.
//
// # No payload
//
// A hosted runtime's request and response payloads are opaque bytes in a
// caller-declared format: the platform imposes no message structure and neither does
// throttle. Nothing here can hold them. PayloadBytes and ResponseBytes are sizes,
// which is what an operator needs to see a runaway payload without the payload being
// stored to see it.
type HostedRuntime struct {
	// RuntimeID identifies the hosted runtime that ran, as the platform named it --
	// normally an ARN. Qualifier is the endpoint or version alias within it, when the
	// caller targeted one.
	RuntimeID string `json:"runtime_id,omitempty"`
	Qualifier string `json:"qualifier,omitempty"`

	// Account is the account owning the runtime, when the caller named the runtime by
	// ID rather than by ARN. It is the piece an ARN would have carried, retained
	// because a resource observation is scoped to an account and a bare ID is not.
	Account string `json:"account,omitempty"`

	// SessionID is the runtime session this invocation belonged to.
	//
	// It is the load-bearing field here, and the reason for the whole type: on AWS
	// AgentCore it is the ONLY identifier present both on the API call and in the
	// resource-usage telemetry. The usage records carry no request ID and no trace ID,
	// so a session is the finest granularity at which delayed CPU and memory usage can
	// be attributed to anything at all.
	//
	// A session may span several invocations, and its resource bill includes start-up,
	// idle time, and platform overhead that no single invocation caused. throttle
	// therefore does not divide a session's cost across the invocations inside it: it
	// records which session each invocation belonged to and leaves the attribution
	// rule to be decided once, deliberately, rather than implied by an adapter.
	SessionID string `json:"session_id,omitempty"`

	// RequestID and TraceID are the platform's own identifiers for this single call.
	//
	// They are precise but, on AgentCore, absent from the resource-usage telemetry --
	// they join to application logs and spans, not to CPU and memory. Retained because
	// they are the identifiers an operator will quote when asking the provider what
	// happened, not because they can price anything.
	RequestID string `json:"request_id,omitempty"`
	TraceID   string `json:"trace_id,omitempty"`

	// StatusCode is the HTTP status the runtime returned. It is recorded because a
	// hosted runtime reports a failure inside the caller's own agent as a non-error
	// response with a failing status: the platform ran, the resource was consumed, and
	// the only evidence of the failure is this number.
	StatusCode int `json:"status_code,omitempty"`

	// ContentType is the MIME type the runtime declared for its response. A type, not
	// a payload.
	ContentType string `json:"content_type,omitempty"`

	// PayloadBytes and ResponseBytes are sizes in bytes: what was sent, and what was
	// read back before the stream ended. ResponseBytes is a byte count of a body that
	// was forwarded and never retained.
	PayloadBytes  int64 `json:"payload_bytes,omitempty"`
	ResponseBytes int64 `json:"response_bytes,omitempty"`

	// Reconciled reports whether authoritative resource usage has since been
	// attributed to this record. False means the runtime cost is still unknown, which
	// is the normal state immediately after a call.
	Reconciled bool `json:"reconciled,omitempty"`

	// ReconciledUsage and ReconciledCost are the runtime resource consumption
	// eventually observed, and that usage priced. Both are zero until Reconciled.
	//
	// They are separate from the record's ActualUsage and ActualCost so that a
	// reconciled figure can never be mistaken for something measured at the time of
	// the call, and so that a platform describing its own telemetry as approximate
	// cannot silently overwrite a figure throttle observed directly.
	ReconciledUsage usage.Usage `json:"reconciled_usage,omitempty"`
	ReconciledCost  usage.Cost  `json:"reconciled_cost,omitempty"`

	// ReconciledAt is when the delayed observation was applied, and ReconciledFrom
	// names its source, e.g. a metric or log stream. Provenance for a number that did
	// not come from the request itself.
	ReconciledAt   time.Time `json:"reconciled_at,omitempty"`
	ReconciledFrom string    `json:"reconciled_from,omitempty"`

	// Note records an accounting limitation, e.g. that runtime resource consumption is
	// not reported synchronously and this record's cost covers none of it.
	Note string `json:"note,omitempty"`
}

// Empty reports whether there is no hosted-runtime detail at all.
func (h HostedRuntime) Empty() bool {
	return h.RuntimeID == "" && h.SessionID == "" && h.RequestID == "" &&
		h.TraceID == "" && h.StatusCode == 0 && !h.Reconciled
}

// Scope is a (budget, period) pair the request consumed.
type Scope struct {
	BudgetID string `json:"budget_id"`
	PeriodID string `json:"period_id"`
	Depth    int    `json:"depth"`
}

// Unresolved reports whether this record represents money spent that throttle
// cannot yet name. These are the records that make a period's total incomplete.
func (r Record) Unresolved() bool { return r.Status == StatusUnresolved }

// Spent reports the amount this record contributes to a spend total, and whether
// that contribution is complete.
//
// A settled record contributes its full cost. An unresolved one contributes the
// priced floor of its partial cost -- which may be zero -- and reports
// incompleteness, which is what turns a dashboard total into "$812.41+". A denied
// or released record contributes nothing.
func (r Record) Spent() (amount money.Money, complete bool) {
	switch r.Status {
	case StatusSettled:
		return r.ActualCost.Amount, r.ActualCost.Known()
	case StatusUnresolved, StatusOutstanding:
		return r.ActualCost.AtLeast(), false
	default:
		return 0, true
	}
}

// Summary aggregates records into the figures a dashboard needs, including the
// ones that report their own incompleteness.
type Summary struct {
	// Spend is the sum of what is known plus the floors of what is not. When
	// Complete is false this is a lower bound and must be rendered as such.
	Spend money.Money

	// Complete reports whether Spend is the whole story. False means at least one
	// request spent money throttle could not fully price.
	Complete bool

	// Unresolved is the number of requests with unresolved or unknown cost, and
	// Encumbered is the headroom their reservations still hold.
	Unresolved int
	Encumbered money.Money

	// Requests is the total number of records counted, and UnpricedDimensions names
	// every dimension that blocked a price, so an operator knows what to add to the
	// catalog.
	Requests           int
	UnpricedDimensions []usage.Dimension
}
