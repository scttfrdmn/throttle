package report

import (
	"context"
	"sort"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/usage"
)

// CostState is how a cost figure should be read.
//
// It exists so that presentation never has to infer completeness from an amount. A
// zero amount means different things in each of these states, and only one of them
// is "this request was free".
type CostState string

const (
	// CostKnown means the amount is the cost.
	CostKnown CostState = "known"

	// CostPartial means the amount is a floor: some dimensions were priced and at
	// least one was not. Rendered with a trailing "+".
	CostPartial CostState = "partial"

	// CostUnknown means nothing could be priced. Rendered as "unknown" and never as
	// a currency amount -- most emphatically never as $0.00.
	CostUnknown CostState = "unknown"

	// CostUnresolved means the request is still awaiting the information that would
	// price it: an unresolved liability, an unknown outcome, or an out-of-band
	// observation that has not arrived.
	CostUnresolved CostState = "unresolved"

	// CostNone means no cost applies because nothing was spent: a denied request, or
	// a released hold with no billable usage.
	CostNone CostState = "none"
)

// Amount is a money figure together with how it should be read.
//
// Every cost the dashboard displays goes through this type, so there is one place
// where "unknown" is prevented from becoming a zero and one place a template can ask
// whether a "+" belongs on the end.
type Amount struct {
	// Value is the amount. It is meaningless when State is CostUnknown, a floor when
	// CostPartial or CostUnresolved, and the cost when CostKnown.
	Value money.Money

	State CostState

	// Unpriced names the dimensions that blocked a full price, for a tooltip or a
	// detail row.
	Unpriced []string

	// Reason explains an incomplete amount in words.
	Reason string
}

// Displayable reports whether Value should be rendered as a currency figure at all.
func (a Amount) Displayable() bool {
	return a.State != CostUnknown && a.State != ""
}

// Floor reports whether Value is a lower bound rather than a total, which is what
// puts the "+" on the end.
func (a Amount) Floor() bool { return a.State == CostPartial || a.State == CostUnresolved }

// Text renders the amount honestly, using format for the number itself: a figure, a
// floor with a "+", or the word unknown. There is no code path here that turns an
// unknown cost into "$0.00".
//
// Only CostKnown renders as a bare figure. An Amount that never declared its
// completeness has not earned one: the zero value of this type must not be able to
// claim a request was free, because a struct field a caller forgot to populate would
// then read as $0.00 forever.
//
// The number formatting is the caller's because a per-request cost wants more decimal
// places than a period total does. The completeness rules are not the caller's, which
// is why they live here once: a display that reimplemented this switch to gain four
// decimal places would be free to get the unknown case wrong.
func (a Amount) Text(format func(money.Money) string) string {
	switch a.State {
	case CostKnown:
		return format(a.Value)
	case CostUnresolved:
		if a.Value == 0 {
			return "unresolved"
		}
		return format(a.Value) + "+"
	case CostPartial:
		return format(a.Value) + "+"
	case CostNone:
		return "—"
	default:
		return "unknown"
	}
}

// String renders the amount rounded to cents.
func (a Amount) String() string { return a.Text(money.Money.CentsString) }

// knownAmount is a fully determined figure.
func knownAmount(m money.Money) Amount { return Amount{Value: m, State: CostKnown} }

// amountOf converts a three-valued usage.Cost into a display amount, given the
// record status that provides the context an unresolved cost needs.
//
// The record status matters because the same usage.Cost means different things at
// different points in a lifecycle: an unknown cost on a settled record is a pricing
// gap, while an unknown cost on an outstanding record is an unknown outcome. Both are
// unknown; only the second is still expected to change.
func amountOf(c usage.Cost, status activity.Status) Amount {
	a := Amount{Value: c.AtLeast(), Reason: c.Reason}
	for _, d := range c.Unpriced {
		a.Unpriced = append(a.Unpriced, string(d))
	}

	switch status {
	case activity.StatusDenied:
		return Amount{State: CostNone}
	case activity.StatusReleased:
		// A released hold means no billable usage occurred. That is a determined zero,
		// not an unknown one.
		if c.Known() {
			return knownAmount(c.Amount)
		}
		return Amount{State: CostNone}
	case activity.StatusUnresolved, activity.StatusOutstanding, activity.StatusPending:
		a.State = CostUnresolved
		return a
	}

	switch c.State() {
	case usage.CostKnown:
		a.State = CostKnown
	case usage.CostPartial:
		a.State = CostPartial
	default:
		a.State = CostUnknown
	}
	return a
}

// Event is one governed request, shaped for display.
//
// It carries measurements, identifiers, and states. It carries no prompt, no
// response, no streamed text, no trace rationale, and no runtime payload, because the
// activity store holds none of those and this type has nowhere to put them.
type Event struct {
	RequestID     string
	ReservationID string

	// BudgetID is the budget the caller named. Scopes lists every scope the hold
	// consumed, so a request against a child is visible from its parent.
	BudgetID string
	Scopes   []activity.Scope

	StartedAt   time.Time
	CompletedAt time.Time

	// Operation is the provider call made, e.g. "converse" or "invoke-agent-runtime".
	Operation string

	// AccessProvider, Publisher, and Model are three independent dimensions, never
	// collapsed into one "provider" field. AWS Bedrock is the access path, Anthropic
	// is the publisher, and the model is a third thing again.
	AccessProvider string
	Publisher      string
	Family         string

	// Model is the canonical name when the catalog recognizes it and the exact
	// provider model ID otherwise, and ModelKnown says which. An unrecognized model
	// is a normal state and must still display something true.
	Model      string
	ModelKnown bool

	// ProviderModelID is always the exact identifier sent to the provider. It is
	// what the provider's bill will refer to, so it is never normalized away.
	ProviderModelID string

	Region string

	// Usage is the dimensional usage, in a stable order, with unrecognized
	// dimensions included.
	Usage []UsageItem

	// Estimated is what was predicted, Reserved what was held, Actual what it cost.
	// The three are separate figures and are never conflated.
	Estimated Amount
	Reserved  money.Money
	Actual    Amount

	// EstimateQuality is how much the estimate could be trusted: exact,
	// conservative, heuristic. A managed agent turn's estimate is a heuristic
	// declaration, and saying so is the difference between an estimate and a promise.
	EstimateQuality string

	// Overrun is how much the actual cost exceeded what was reserved, zero if it did
	// not. A real overrun is recorded rather than clamped to the estimate.
	Overrun money.Money

	Status  activity.Status
	Outcome activity.Outcome

	// EnforcementMode is the posture that actually governed this call. Spend admitted
	// under monitoring was never subject to a ceiling, and a reader who cannot see
	// that will mistake unenforced spend for spend that fit.
	EnforcementMode engine.Mode

	// Error is an error message, never content.
	Error string

	Latency         time.Duration
	ProviderLatency time.Duration

	// StreamFirstEvent is time to first event for a streaming call, zero otherwise.
	StreamFirstEvent time.Duration

	// Compound reports whether this request has agent step detail beneath it.
	Compound  bool
	StepCount int

	// HostedRuntime reports whether this was an invocation of an agent on a managed
	// runtime, whose cost arrives out of band.
	HostedRuntime bool

	// AwaitingExternal reports whether this record is waiting for an out-of-band
	// observation. It is a designed terminal state, not damage.
	AwaitingExternal bool

	// Repaired reports whether reconciliation has touched this record.
	Repaired bool

	// Metadata is the caller's own attribution strings.
	Metadata map[string]string
}

// UsageItem is one dimension of usage.
//
// Dimension is the canonical name as recorded, so a dimension throttle has never
// seen before still appears: a provider that starts billing a new unit must not
// vanish from the dashboard merely because the UI predates it.
type UsageItem struct {
	Dimension string
	Count     int64

	// Token reports whether this is one of the token dimensions, so a display can
	// group the familiar ones and still show the rest.
	Token bool

	// Unpriced reports whether this dimension is what blocked a full price.
	Unpriced bool
}

// ActivityQuery selects events.
type ActivityQuery struct {
	BudgetID string
	PeriodID string
	From     time.Time
	To       time.Time

	// Statuses limits to the given statuses. Empty means all.
	Statuses []activity.Status

	// UnresolvedOnly limits to records whose cost is not fully known, which is the
	// query behind the reconciliation panel's "show me those rows" link.
	UnresolvedOnly bool

	// Limit caps the rows returned. Zero applies DefaultActivityLimit, because an
	// unbounded activity table on a busy ledger is a page that never renders.
	Limit int
}

// DefaultActivityLimit bounds an activity listing that did not ask for a size.
const DefaultActivityLimit = 200

// ActivityPage is a page of events plus whether it was truncated.
type ActivityPage struct {
	Events []Event

	// Truncated reports that the limit was reached and older events exist. Saying so
	// is the difference between a bounded view and a silently incomplete one.
	Truncated bool

	// Limit is the limit actually applied.
	Limit int

	// Summary aggregates exactly the events on this page, not the whole period, so a
	// reader is never shown a page total labelled as a period total.
	Summary activity.Summary
}

// Activity lists governed requests, most recent first.
func (r *Reporter) Activity(ctx context.Context, q ActivityQuery) (ActivityPage, error) {
	if r.acts == nil {
		return ActivityPage{}, errNotConfigured("request activity")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultActivityLimit
	}

	// One extra row is requested so truncation can be detected without a second
	// count query.
	records, err := r.acts.List(ctx, activity.Filter{
		BudgetID:       q.BudgetID,
		PeriodID:       q.PeriodID,
		From:           q.From,
		To:             q.To,
		Statuses:       q.Statuses,
		UnresolvedOnly: q.UnresolvedOnly,
		Limit:          limit + 1,
	})
	if err != nil {
		return ActivityPage{}, err
	}

	page := ActivityPage{Limit: limit}
	if len(records) > limit {
		page.Truncated = true
		records = records[:limit]
	}
	page.Summary = activity.Summarize(records)
	page.Events = make([]Event, 0, len(records))
	for _, rec := range records {
		page.Events = append(page.Events, eventOf(rec))
	}
	return page, nil
}

// eventOf converts a durable record into a display event.
//
// It is a projection, not a computation: no money is derived here that the record
// does not already hold. The one exception is the overrun, which is a subtraction of
// two recorded figures.
func eventOf(rec activity.Record) Event {
	id := rec.Identity
	e := Event{
		RequestID:        rec.RequestID,
		ReservationID:    rec.ReservationID,
		BudgetID:         rec.BudgetID,
		Scopes:           rec.Scopes,
		StartedAt:        rec.StartedAt,
		CompletedAt:      rec.CompletedAt,
		Operation:        id.Operation,
		AccessProvider:   id.AccessProvider,
		Publisher:        id.Publisher,
		Family:           id.Family,
		ProviderModelID:  id.ProviderModelID,
		Region:           id.Region,
		Reserved:         rec.Reserved,
		Estimated:        amountOf(rec.Estimate.Cost, activity.StatusSettled),
		Actual:           amountOf(rec.ActualCost, rec.Status),
		EstimateQuality:  string(rec.Estimate.Quality),
		Status:           rec.Status,
		Outcome:          rec.Outcome,
		EnforcementMode:  rec.EnforcementMode,
		Error:            rec.Error,
		Latency:          rec.Latency,
		ProviderLatency:  rec.ProviderLatency,
		StreamFirstEvent: rec.StreamFirstEvent,
		Compound:         !rec.Agent.Empty(),
		StepCount:        len(rec.Agent.Steps),
		HostedRuntime:    !rec.Runtime.Empty(),
		AwaitingExternal: rec.Status == activity.StatusUnresolved && awaitingExternal(rec),
		Repaired:         len(rec.Repairs) > 0,
		Metadata:         rec.Metadata,
	}

	// An unrecognized model still displays: the exact provider ID is authoritative
	// identity and the canonical name is enrichment on top of it.
	e.Model, e.ModelKnown = displayModel(id)
	e.Usage = usageItems(rec.ActualUsage, rec.ActualCost.Unpriced)

	if rec.ActualCost.Known() && rec.ActualCost.Amount > rec.Reserved {
		if v, ok := money.Sub(rec.ActualCost.Amount, rec.Reserved); ok {
			e.Overrun = v
		}
	}
	return e
}

// displayModel is the name to show and whether it is the canonical one.
func displayModel(id usage.ModelIdentity) (string, bool) {
	if id.CanonicalModel != "" {
		return id.CanonicalModel, true
	}
	if id.ProviderModelID != "" {
		return id.ProviderModelID, false
	}
	// A provider that reported usage without naming a model leaves a real charge with
	// no identity. Recording the configured model instead would be a guess presented
	// as a fact, so the absence is displayed as an absence.
	return "", false
}

// tokenDimensions are the dimensions a display may group as "tokens". Anything else
// is shown as itself.
//
// Audio tokens belong here because they are tokens: a multimodal model reports a token
// count and bills per token, at its own rate. Only the rate differs, and a rate is not
// what this map is about. Leaving them out would put a "non-token" badge beside a token
// count, which is a false statement about the unit rather than a cosmetic gap -- and it
// would tell a reader the figure needs no per-token rate, which is precisely the rate
// whose absence made the cost a floor.
var tokenDimensions = map[usage.Dimension]bool{
	usage.InputTokens:        true,
	usage.OutputTokens:       true,
	usage.ReasoningTokens:    true,
	usage.CacheReadTokens:    true,
	usage.CacheWriteTokens:   true,
	usage.CacheWrite5mTokens: true,
	usage.CacheWrite1hTokens: true,
	usage.InputAudioTokens:   true,
	usage.OutputAudioTokens:  true,
}

// usageItems flattens usage into a stable ordered list.
//
// Token dimensions come first in a fixed order because that is how a reader scans
// them; everything else follows alphabetically, which is what keeps a dimension the
// dashboard has never seen visible instead of dropped. Not all activity is
// token-based, and a runtime's vCPU-nano-hours must appear here too.
func usageItems(u usage.Usage, unpriced []usage.Dimension) []UsageItem {
	if u.Empty() {
		return nil
	}
	blocked := make(map[usage.Dimension]bool, len(unpriced))
	for _, d := range unpriced {
		blocked[d] = true
	}

	// Audio sits beside the text figure in its own direction, because the two are the
	// disjoint halves of one reported total and a reader checking the arithmetic should
	// not have to scan past the cache rows to find the other half.
	// The cache rows sit together and the TTL-specific writes sit beside the undifferentiated
	// one, because a provider that prices a one-hour write higher than a five-minute write
	// makes the two adjacent figures the thing a reader is comparing.
	preferred := []usage.Dimension{
		usage.InputTokens, usage.InputAudioTokens,
		usage.OutputTokens, usage.OutputAudioTokens,
		usage.ReasoningTokens, usage.CacheReadTokens, usage.CacheWriteTokens,
		usage.CacheWrite5mTokens, usage.CacheWrite1hTokens,
	}
	seen := map[usage.Dimension]bool{}
	var out []UsageItem

	for _, d := range preferred {
		if n, ok := u.Get(d); ok {
			seen[d] = true
			out = append(out, UsageItem{Dimension: string(d), Count: n, Token: true, Unpriced: blocked[d]})
		}
	}
	var rest []usage.Dimension
	for _, d := range u.Dimensions() {
		if !seen[d] {
			rest = append(rest, d)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	for _, d := range rest {
		n, _ := u.Get(d)
		out = append(out, UsageItem{
			Dimension: string(d),
			Count:     n,
			Token:     tokenDimensions[d],
			Unpriced:  blocked[d],
		})
	}
	return out
}
