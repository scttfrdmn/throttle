package report

import (
	"context"
	"time"

	"throttle/activity"
	"throttle/money"
	"throttle/usage"
)

// Detail is one request examined closely: the event, its compound agent steps if it
// had any, its hosted-runtime linkage if it was one, and its reconciliation trail.
//
// Everything here is a measurement, an identifier, a state, or a timestamp. No field
// can hold a prompt, a response, a trace rationale, or a runtime payload, because
// nothing upstream stores them.
type Detail struct {
	Event Event

	// Agent is the compound-transaction detail: the internal model invocations
	// observed beneath one governed request.
	Agent *AgentDetail

	// Runtime is the hosted-runtime linkage: which runtime and session ran, what
	// exposure was held, and whether an out-of-band cost observation has arrived.
	Runtime *RuntimeDetail

	// Repairs is the append-only reconciliation trail, oldest first.
	Repairs []Repair

	// Quote identifies the immutable rates captured at admission, so a reader can see
	// which catalog version priced the request. It is provenance, not a rate card.
	QuoteSource     string
	QuoteVersion    string
	QuoteCapturedAt time.Time
}

// AgentDetail is a managed agent turn, decomposed.
//
// The turn is ONE transaction with ONE reservation and ONE charge. These steps are
// accounting detail beneath it, not transactions of their own: throttle admitted the
// turn, and inventing a child transaction for a model call it never admitted would
// misrepresent what was governed.
type AgentDetail struct {
	AgentID string
	AliasID string
	Version string

	// SessionID groups turns of one conversation. It groups records; it does not
	// scope money.
	SessionID string

	Steps []AgentStep

	// Events are non-model activities counted by kind -- a tool invocation, a
	// knowledge-base lookup, a guardrail evaluation. They are counts and nothing
	// else, because the provider reports no billable quantity for them: their cost
	// lands on the provider's bill outside throttle's view.
	Events map[string]int

	// Total is the request-level charge: the authoritative figure.
	Total Amount

	// StepSum is the sum of the individually rounded step amounts.
	StepSum money.Money

	// RoundingGap is Total-StepSum, normally zero and occasionally a microdollar or
	// two.
	//
	// The turn is accumulated exactly and rounded once, so rounding each step for
	// display can leave the displayed steps not summing to the displayed total. When
	// that is visible it is explained rather than papered over -- adjusting a step to
	// make the column add up would mean displaying a number that is not the step's
	// cost.
	//
	// A gap larger than rounding can account for is a different fact, and
	// GapExplainedByRounding is what separates the two.
	RoundingGap money.Money

	// Note records an accounting limitation of the turn, e.g. that unpriceable
	// non-model activity occurred.
	Note string
}

// GapVisible reports whether the gap between the step column and the request charge is
// large enough to show up in a cents-rounded column.
func (a AgentDetail) GapVisible() bool {
	const halfCent = money.Money(5_000)
	if a.RoundingGap < 0 {
		return -a.RoundingGap >= halfCent
	}
	return a.RoundingGap >= halfCent
}

// GapExplainedByRounding reports whether display rounding can account for the whole gap.
//
// Step costs and the request charge are exact microdollar figures; the only slack rounding
// introduces is in the last place a display shows. At four decimals that is half of one
// hundredth of a cent per figure, so N steps and one total bound the explainable gap at
// (N+1) half-units -- fractions of a cent, never dollars.
//
// The distinction is load-bearing rather than pedantic. A gap of two dollars described as
// per-step rounding would be a display explaining away an accounting disagreement, which
// is the one thing the step table exists not to do: the provider's per-step figures then
// genuinely do not account for the charge, and a reader needs to know that rather than be
// reassured.
func (a AgentDetail) GapExplainedByRounding() bool {
	// Half of $0.0001, in microdollars.
	const halfDisplayUnit = money.Money(50)
	gap := a.RoundingGap
	if gap < 0 {
		gap = -gap
	}
	return gap <= money.Money(len(a.Steps)+1)*halfDisplayUnit
}

// AgentStep is one observed model invocation inside a turn.
type AgentStep struct {
	Seq int

	// Kind is the phase the provider attributed the call to: preprocessing,
	// orchestration, postprocessing, and so on, in the provider's own vocabulary.
	Kind string

	// Collaborator names a delegated agent that performed the step, when the provider
	// reported one. It is a visibility dimension, not a sub-budget.
	Collaborator string

	// Model is the canonical name when known and the exact provider ID otherwise.
	// Both may be empty: a provider that reports usage for a step without naming the
	// model leaves a real charge with no identity, and guessing one would be worse.
	Model      string
	ModelKnown bool

	ProviderModelID string
	Publisher       string

	Usage []UsageItem

	// Cost is this step's contribution, rounded for display. See
	// AgentDetail.RoundingGap.
	Cost Amount

	Latency time.Duration
	At      time.Time
}

// RuntimeDetail is an invocation of an agent hosted on a managed runtime.
//
// Its defining property: the cost of the compute is not in the response and is not
// knowable at the time of the call. It arrives later, through the platform's
// observability, keyed by a session identifier -- and a session's bill includes
// start-up, idle time, and platform overhead that no single invocation caused.
type RuntimeDetail struct {
	RuntimeID string
	Qualifier string
	Account   string

	// SessionID is the load-bearing identifier: on AgentCore it is the only one
	// present both on the API call and in the resource-usage telemetry.
	SessionID string

	// ProviderRequestID and TraceID are the platform's identifiers for this single
	// call. They are precise, and absent from the resource-usage telemetry, so they
	// join to logs and spans rather than to CPU and memory.
	ProviderRequestID string
	TraceID           string

	// StatusCode is the HTTP status the runtime returned. A hosted runtime reports a
	// failure inside the caller's own agent as a non-error response with a failing
	// status: the platform ran, the resource was consumed, and this number is the only
	// evidence.
	StatusCode  int
	ContentType string

	// PayloadBytes and ResponseBytes are sizes, not payloads.
	PayloadBytes  int64
	ResponseBytes int64

	// MaxExposure is the headroom that was held. It is not a cost, not an estimate,
	// and not a limit the platform enforces.
	MaxExposure money.Money

	// Reconciled reports whether authoritative resource usage has been attributed to
	// this record. False is the normal state immediately after a call.
	Reconciled bool

	// ReconciledUsage and ReconciledCost are the eventual observation, kept separate
	// from the record's own usage and cost so a later figure can never be mistaken for
	// something measured at the time of the call.
	ReconciledUsage []UsageItem
	ReconciledCost  Amount
	ReconciledAt    time.Time
	ReconciledFrom  string

	// SessionScoped states, in the UI, the decision recorded on #20: a session-level
	// runtime charge is never divided across the invocations that shared the session.
	// This invocation's own runtime cost is unknown, and a computed share would be
	// indistinguishable from a measurement.
	SessionScoped bool

	Note string
}

// Repair is one entry from a record's reconciliation trail.
type Repair struct {
	At time.Time

	// Class is the monetary outcome the reconciler recorded, and Reason its durable
	// classification -- "crash-repairable", "awaiting-external-usage", and so on. Both
	// are the reconciler's vocabulary, carried through verbatim.
	Class  string
	Reason string

	ObservedStatus      string
	ObservedReservation string
	ProducedStatus      string

	// Money is "settled", "released", or empty when the repair moved none.
	Money  string
	Amount money.Money

	// QuoteSource and QuoteVersion prove which immutable quote priced a replayed
	// settlement, rather than whatever the catalog said at repair time.
	QuoteSource  string
	QuoteVersion string

	Detail string
}

// Detail returns everything the read model knows about one request.
func (r *Reporter) Detail(ctx context.Context, requestID string) (Detail, error) {
	if r.acts == nil {
		return Detail{}, errNotConfigured("request detail")
	}
	rec, err := r.acts.Get(ctx, requestID)
	if err != nil {
		return Detail{}, err
	}

	d := Detail{Event: eventOf(rec)}
	d.QuoteSource = rec.Quote.Provenance.Source
	d.QuoteVersion = rec.Quote.Provenance.Version
	d.QuoteCapturedAt = rec.Quote.Provenance.RetrievedAt

	if !rec.Agent.Empty() {
		d.Agent = agentDetail(rec)
	}
	if !rec.Runtime.Empty() {
		d.Runtime = runtimeDetail(rec)
	}
	for _, rep := range rec.Repairs {
		d.Repairs = append(d.Repairs, Repair{
			At:                  rep.At,
			Class:               rep.Class,
			Reason:              rep.Reason,
			ObservedStatus:      string(rep.ObservedStatus),
			ObservedReservation: rep.ObservedReservation,
			ProducedStatus:      string(rep.ProducedStatus),
			Money:               rep.Money,
			Amount:              rep.Amount,
			QuoteSource:         rep.QuoteSource,
			QuoteVersion:        rep.QuoteVersion,
			Detail:              rep.Detail,
		})
	}
	return d, nil
}

func agentDetail(rec activity.Record) *AgentDetail {
	a := rec.Agent
	out := &AgentDetail{
		AgentID:   a.AgentID,
		AliasID:   a.AliasID,
		Version:   a.Version,
		SessionID: a.SessionID,
		Events:    a.Events,
		Total:     amountOf(rec.ActualCost, rec.Status),
		Note:      a.Note,
	}
	for _, s := range a.Steps {
		model, known := displayModel(s.Identity)
		out.Steps = append(out.Steps, AgentStep{
			Seq:             s.Seq,
			Kind:            s.Kind,
			Collaborator:    s.Collaborator,
			Model:           model,
			ModelKnown:      known,
			ProviderModelID: s.Identity.ProviderModelID,
			Publisher:       s.Identity.Publisher,
			Usage:           usageItems(s.Usage, s.Cost.Unpriced),
			Cost:            amountOf(s.Cost, activity.StatusSettled),
			Latency:         s.Latency,
			At:              s.At,
		})
		if v, ok := money.Add(out.StepSum, s.Cost.AtLeast()); ok {
			out.StepSum = v
		}
	}
	if rec.ActualCost.Known() {
		if v, ok := money.Sub(rec.ActualCost.Amount, out.StepSum); ok {
			out.RoundingGap = v
		}
	}
	return out
}

func runtimeDetail(rec activity.Record) *RuntimeDetail {
	h := rec.Runtime
	out := &RuntimeDetail{
		RuntimeID:         h.RuntimeID,
		Qualifier:         h.Qualifier,
		Account:           h.Account,
		SessionID:         h.SessionID,
		ProviderRequestID: h.RequestID,
		TraceID:           h.TraceID,
		StatusCode:        h.StatusCode,
		ContentType:       h.ContentType,
		PayloadBytes:      h.PayloadBytes,
		ResponseBytes:     h.ResponseBytes,
		MaxExposure:       rec.Reserved,
		Reconciled:        h.Reconciled,
		ReconciledAt:      h.ReconciledAt,
		ReconciledFrom:    h.ReconciledFrom,
		SessionScoped:     h.SessionID != "",
		Note:              h.Note,
	}
	if h.Reconciled {
		out.ReconciledUsage = usageItems(h.ReconciledUsage, h.ReconciledCost.Unpriced)
		out.ReconciledCost = amountOf(h.ReconciledCost, activity.StatusSettled)
	} else {
		// Not zero, and not an empty cell: the runtime cost of this call is genuinely
		// not known yet, and it is the normal state immediately after an invocation.
		out.ReconciledCost = Amount{
			State:  CostUnresolved,
			Reason: "hosted runtime resource usage is reported out of band and has not arrived",
		}
	}
	return out
}

// Unresolved lists the records that make a total incomplete, for the reconciliation
// panel.
//
// It is a read. Opening a dashboard must never run a monetary repair: reconciliation
// moves money, and money should move because an operator decided it should, not
// because a browser polled.
func (r *Reporter) Unresolved(ctx context.Context, q ActivityQuery) (ActivityPage, error) {
	q.UnresolvedOnly = true
	q.Statuses = []activity.Status{
		activity.StatusUnresolved,
		activity.StatusOutstanding,
		activity.StatusPending,
	}
	return r.Activity(ctx, q)
}

// unpricedNames renders a cost's unpriced dimensions as strings.
func unpricedNames(c usage.Cost) []string {
	if len(c.Unpriced) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.Unpriced))
	for _, d := range c.Unpriced {
		out = append(out, string(d))
	}
	return out
}
