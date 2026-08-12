package report

import (
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/usage"
)

// --- cost completeness -----------------------------------------------------

// The single most important rendering rule in the dashboard: an unknown cost is
// never a zero. "$0.00" says a request was free; these requests were not.
func TestUnknownCostNeverRendersAsZero(t *testing.T) {
	cases := map[string]activity.Record{
		"settled but unpriceable": {
			Status:     activity.StatusSettled,
			ActualCost: usage.UnknownCost("no rate for this model"),
		},
		"unresolved liability": {
			Status:     activity.StatusUnresolved,
			ActualCost: usage.UnknownCost("model released before the catalog knew it"),
		},
		"outcome unknown": {
			Status:     activity.StatusOutstanding,
			ActualCost: usage.UnknownCost("the process died mid-call"),
		},
		"still in flight": {
			Status:     activity.StatusPending,
			ActualCost: usage.Cost{},
		},
	}
	for name, rec := range cases {
		t.Run(name, func(t *testing.T) {
			a := amountOf(rec.ActualCost, rec.Status)
			if a.State == CostKnown {
				t.Fatalf("State = known for a cost nothing could price")
			}
			s := a.String()
			if strings.Contains(s, "0.00") {
				t.Errorf("String() = %q; an unknown cost must not render as a zero amount", s)
			}
			if a.State == CostUnknown && a.Displayable() {
				t.Error("Displayable() = true for a wholly unknown cost")
			}
		})
	}
}

// A partial cost is a floor, and the "+" is what says so.
func TestPartialCostKeepsItsFloorSemantics(t *testing.T) {
	c := usage.PartialCost(cents(412),
		[]usage.Dimension{usage.CacheWriteTokens},
		"no rate for cache writes")

	a := amountOf(c, activity.StatusSettled)

	if a.State != CostPartial {
		t.Fatalf("State = %q, want partial", a.State)
	}
	if !a.Floor() {
		t.Error("Floor() = false for a partial cost")
	}
	if got, want := a.String(), "$4.12+"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if a.Value != cents(412) {
		t.Errorf("Value = %s, want the priced floor %s", a.Value, cents(412))
	}
	if len(a.Unpriced) != 1 || a.Unpriced[0] != string(usage.CacheWriteTokens) {
		t.Errorf("Unpriced = %v, want the dimension that blocked the price", a.Unpriced)
	}
	if a.Reason == "" {
		t.Error("Reason is empty; an incomplete amount must explain itself")
	}
}

// An unresolved record that priced part of its usage keeps the floor, and one that
// priced none of it says "unresolved" rather than showing a bare zero.
func TestUnresolvedCostRendersAsAFloorOrAsUnresolved(t *testing.T) {
	withFloor := amountOf(
		usage.PartialCost(cents(150), []usage.Dimension{usage.OutputTokens}, "partial"),
		activity.StatusUnresolved)
	if withFloor.State != CostUnresolved {
		t.Fatalf("State = %q, want unresolved", withFloor.State)
	}
	if got, want := withFloor.String(), "$1.50+"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	bare := amountOf(usage.UnknownCost("nothing priced"), activity.StatusUnresolved)
	if got, want := bare.String(), "unresolved"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// A denied request spent nothing and a released hold returned nothing, and both are
// determined -- distinct from a cost nobody could name.
func TestDeniedAndReleasedAreNoCostNotUnknownCost(t *testing.T) {
	denied := amountOf(usage.Cost{}, activity.StatusDenied)
	if denied.State != CostNone {
		t.Errorf("denied State = %q, want none", denied.State)
	}
	if denied.String() == "unknown" {
		t.Error("a denied request rendered as unknown; nothing was called and nothing was spent")
	}

	released := amountOf(usage.Cost{}, activity.StatusReleased)
	if released.State != CostNone {
		t.Errorf("released State = %q, want none", released.State)
	}

	// A release that nonetheless observed billable usage keeps its known figure.
	billed := amountOf(usage.KnownCost(cents(30)), activity.StatusReleased)
	if billed.State != CostKnown || billed.Value != cents(30) {
		t.Errorf("released-with-usage = %s in state %q, want a known $0.30", billed.Value, billed.State)
	}
}

// A settled, fully priced cost is the ordinary case and renders as a plain figure.
func TestKnownCostRendersAsAFigure(t *testing.T) {
	a := amountOf(usage.KnownCost(cents(412)), activity.StatusSettled)
	if a.State != CostKnown {
		t.Fatalf("State = %q, want known", a.State)
	}
	if a.Floor() {
		t.Error("Floor() = true for a complete cost")
	}
	if got, want := a.String(), "$4.12"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// --- identity facets -------------------------------------------------------

// Access provider, publisher, and model are three independent fields. Collapsing
// AWS Bedrock / Anthropic / Claude into one "provider" string makes all three
// questions unanswerable.
func TestBedrockAccessPathPublisherAndModelStayDistinct(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	w.record(settledRecord("r1", "research", p.ID, cents(412), w.now))

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(page.Events))
	}
	e := page.Events[0]

	if e.AccessProvider != "aws-bedrock" {
		t.Errorf("AccessProvider = %q, want aws-bedrock", e.AccessProvider)
	}
	if e.Publisher != "anthropic" {
		t.Errorf("Publisher = %q, want anthropic", e.Publisher)
	}
	if e.Model != "claude-sonnet-4" {
		t.Errorf("Model = %q, want claude-sonnet-4", e.Model)
	}
	if e.Family != "claude-sonnet" {
		t.Errorf("Family = %q, want claude-sonnet", e.Family)
	}
	if e.ProviderModelID != bedrockIdentity().ProviderModelID {
		t.Errorf("ProviderModelID = %q, want the exact provider identifier", e.ProviderModelID)
	}
	for _, pair := range [][2]string{
		{"access provider", e.AccessProvider},
		{"publisher", e.Publisher},
		{"model", e.Model},
	} {
		if pair[1] == "" {
			t.Errorf("%s is empty; the three facets must each be reported", pair[0])
		}
	}
	if e.AccessProvider == e.Publisher || e.Publisher == e.Model {
		t.Error("two of the three identity facets are equal; they have been collapsed")
	}
}

// A model the catalog has never heard of is a normal state. It must still display
// something true: the exact provider model ID, which is authoritative identity.
func TestUnknownModelFallsBackToTheProviderModelID(t *testing.T) {
	id := usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "vendor.brand-new-model-v9:0",
		Operation:       "converse",
	}
	name, known := displayModel(id)
	if known {
		t.Error("ModelKnown = true for a model with no canonical name")
	}
	if name != id.ProviderModelID {
		t.Errorf("display name = %q, want the provider model ID %q", name, id.ProviderModelID)
	}

	// A canonical name, when present, wins.
	name, known = displayModel(bedrockIdentity())
	if !known || name != "claude-sonnet-4" {
		t.Errorf("display name = %q (known=%v), want the canonical name", name, known)
	}

	// A provider that reported usage without naming a model leaves a real charge
	// with no identity. That absence is displayed as an absence, never guessed.
	name, known = displayModel(usage.ModelIdentity{AccessProvider: "aws-bedrock"})
	if name != "" || known {
		t.Errorf("display name = %q (known=%v), want an empty, unknown identity", name, known)
	}
}

// --- usage dimensions ------------------------------------------------------

// Not all activity is token-based, and a dimension the dashboard has never seen
// must not vanish merely because the UI predates it.
func TestUsageItemsKeepUnfamiliarDimensions(t *testing.T) {
	u := usage.New(map[usage.Dimension]int64{
		usage.OutputTokens:             400,
		usage.InputTokens:              1200,
		usage.RuntimeVCPUNanoHours:     123_456_789,
		usage.RuntimeMemoryNanoGBHours: 987_654_321,
		"vendor_new_unit":              7,
	})
	items := usageItems(u, []usage.Dimension{usage.RuntimeVCPUNanoHours})

	if len(items) != 5 {
		t.Fatalf("got %d items, want 5: %+v", len(items), items)
	}
	// Token dimensions first, in the reading order, then the rest alphabetically.
	want := []string{
		string(usage.InputTokens), string(usage.OutputTokens),
		string(usage.RuntimeMemoryNanoGBHours), string(usage.RuntimeVCPUNanoHours),
		"vendor_new_unit",
	}
	for i, w := range want {
		if items[i].Dimension != w {
			t.Errorf("item %d = %q, want %q", i, items[i].Dimension, w)
		}
	}
	if !items[0].Token || items[4].Token {
		t.Error("Token flags are wrong; a display must be able to group the familiar ones")
	}
	for _, it := range items {
		if it.Dimension == string(usage.RuntimeVCPUNanoHours) && !it.Unpriced {
			t.Error("the dimension that blocked the price is not flagged unpriced")
		}
	}
	if items[4].Count != 7 {
		t.Errorf("an unrecognized dimension reported %d, want its real count 7", items[4].Count)
	}
}

func TestUsageItemsOfEmptyUsageIsEmpty(t *testing.T) {
	if got := usageItems(usage.Usage{}, nil); got != nil {
		t.Errorf("usageItems of empty usage = %+v, want nil", got)
	}
}

// --- the activity listing --------------------------------------------------

// The twelve display columns the brief names must all be populated from the
// record, including the enforcement posture that actually governed the call.
func TestEventCarriesTheDisplayColumns(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	rec := settledRecord("r1", "research", p.ID, cents(412), w.now)
	w.record(rec)

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	e := page.Events[0]

	if e.RequestID != "r1" {
		t.Errorf("RequestID = %q", e.RequestID)
	}
	if e.BudgetID != "research" {
		t.Errorf("BudgetID = %q", e.BudgetID)
	}
	if e.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
	if e.Operation != "converse" {
		t.Errorf("Operation = %q, want converse", e.Operation)
	}
	if len(e.Usage) == 0 {
		t.Error("Usage is empty; a usage summary is one of the columns")
	}
	if e.Estimated.State != CostKnown {
		t.Errorf("Estimated state = %q, want known", e.Estimated.State)
	}
	if e.Reserved != cents(412) {
		t.Errorf("Reserved = %s, want %s", e.Reserved, cents(412))
	}
	if e.Actual.State != CostKnown || e.Actual.Value != cents(412) {
		t.Errorf("Actual = %s in state %q, want a known %s", e.Actual.Value, e.Actual.State, cents(412))
	}
	if e.EnforcementMode == "" {
		t.Error("EnforcementMode is empty; unenforced spend must be distinguishable")
	}
	if e.Latency == 0 {
		t.Error("Latency is zero")
	}
	if e.Status != activity.StatusSettled {
		t.Errorf("Status = %q, want settled", e.Status)
	}
	if e.EstimateQuality == "" {
		t.Error("EstimateQuality is empty; an estimate's trustworthiness is part of reading it")
	}
}

// Actual cost may exceed what was reserved. The overrun is recorded, not clamped.
func TestEventReportsAnOverrun(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	rec := settledRecord("r1", "research", p.ID, cents(412), w.now)
	rec.Reserved = cents(300)
	w.record(rec)

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if want := cents(112); page.Events[0].Overrun != want {
		t.Errorf("Overrun = %s, want %s", page.Events[0].Overrun, want)
	}
}

// A bounded listing says it is bounded, rather than reading as a complete history.
func TestActivityReportsTruncation(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		w.record(settledRecord("r"+id, "research", p.ID, cents(10), w.now.Add(time.Duration(i)*time.Second)))
	}

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research", Limit: 3})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(page.Events) != 3 {
		t.Fatalf("got %d events, want the requested 3", len(page.Events))
	}
	if !page.Truncated {
		t.Error("Truncated = false with more records than the limit")
	}
	if page.Limit != 3 {
		t.Errorf("Limit = %d, want the applied limit 3", page.Limit)
	}

	full, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research", Limit: 10})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if full.Truncated {
		t.Error("Truncated = true when every record fit")
	}
	if full.Summary.Requests != 5 {
		t.Errorf("Summary.Requests = %d, want 5", full.Summary.Requests)
	}
}

// A page summary aggregates the page, not the period, so a reader is never shown a
// page total labelled as a period total.
func TestActivityPageSummaryCoversThePage(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	for i := 0; i < 4; i++ {
		id := string(rune('a' + i))
		w.record(settledRecord("r"+id, "research", p.ID, dollars(1), w.now.Add(time.Duration(i)*time.Second)))
	}

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research", Limit: 2})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if page.Summary.Requests != 2 {
		t.Errorf("Summary.Requests = %d, want the 2 rows on this page", page.Summary.Requests)
	}
	if want := dollars(2); page.Summary.Spend != want {
		t.Errorf("Summary.Spend = %s, want %s", page.Summary.Spend, want)
	}
}

// A dashboard with no activity store must say so, rather than showing an empty
// table that reads as "no requests happened".
func TestActivityQueriesFailLoudlyWithoutAnActivityStore(t *testing.T) {
	w := newLedgerOnlyWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	if w.rep.HasActivity() {
		t.Fatal("HasActivity() = true on a reporter built without one")
	}
	if _, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"}); err == nil {
		t.Error("Activity returned no error without an activity store")
	}
	if _, err := w.rep.Breakdown(w.ctx, FacetModel, ActivityQuery{}); err == nil {
		t.Error("Breakdown returned no error without an activity store")
	}
	if _, err := w.rep.Detail(w.ctx, "r1"); err == nil {
		t.Error("Detail returned no error without an activity store")
	}

	// The monetary figures still work, because they come from the ledger.
	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.ActivityAvailable {
		t.Error("ActivityAvailable = true without an activity store")
	}
	if sum.Position.Total != dollars(1000) {
		t.Errorf("Total = %s, want the envelope total from the ledger", sum.Position.Total)
	}
}

// Amount's zero value must not read as a free request either.
func TestZeroAmountValueIsNotAKnownZero(t *testing.T) {
	var a Amount
	if a.State == CostKnown {
		t.Error("the zero Amount claims a known cost")
	}
	if s := a.String(); strings.Contains(s, "0.00") {
		t.Errorf("the zero Amount renders as %q", s)
	}
}

// knownAmount is the only constructor that asserts completeness, and a genuine
// zero-cost request is representable through it.
func TestKnownZeroIsRepresentable(t *testing.T) {
	a := knownAmount(money.Money(0))
	if a.State != CostKnown {
		t.Fatalf("State = %q, want known", a.State)
	}
	if got, want := a.String(), "$0.00"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
