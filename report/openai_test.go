package report

import (
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// This file asks whether reporting is provider-neutral in fact rather than in
// intention. Every test here builds an OpenAI record out of nothing but the normalized
// durable facts the adapter writes, and then requires the existing reporter to answer
// ordinary questions about it: what did it cost, which model was it, how does it group
// against a Bedrock request in the same budget.
//
// The package imports no adapter and no provider SDK, and it contains no provider name
// outside of test data. If any assertion below needed a new field, a new facet, or a
// branch on the access provider, the abstraction would be wrong rather than incomplete.

// openAIIdentity is what the Responses adapter records. It differs from the Bedrock
// shape in three ways that matter to reporting: access provider and publisher coincide,
// CanonicalModel is empty because OpenAI names no canonical model, and ServiceTier is
// populated where Bedrock's is not.
func openAIIdentity() usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "openai",
		Publisher:       "openai",
		ProviderModelID: "gpt-5.1",
		Operation:       "responses",
		ServiceTier:     "priority",
	}
}

// openAIRecord is an ordinary settled Responses request, carrying the four disjoint
// token dimensions the adapter's subtractive normalization produces.
func openAIRecord(requestID, budgetID, periodID string, cost money.Money, at time.Time) activity.Record {
	id := openAIIdentity()
	return activity.Record{
		RequestID:     requestID,
		ReservationID: "res-" + requestID,
		BudgetID:      budgetID,
		Scopes:        []activity.Scope{{BudgetID: budgetID, PeriodID: periodID}},
		Identity:      id,
		Estimate: usage.Estimate{
			Identity: id,
			Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 10000}),
			Cost:     usage.KnownCost(cost * 2),
			Quality:  usage.QualityConservative,
		},
		Quote: pricing.CapturedQuote{
			AccessProvider:  "openai",
			ProviderModelID: "gpt-5.1",
			ServiceTier:     "priority",
			Provenance: pricing.Provenance{
				Source:      "throttle-fixture:developers.openai.com/api/docs/pricing",
				Version:     "2026-08-12-fixture-1",
				RetrievedAt: at,
				Currency:    "USD",
			},
			CapturedAt: at,
		},
		Reserved: cost * 2,
		ActualUsage: usage.New(map[usage.Dimension]int64{
			usage.InputTokens:     8000,
			usage.CacheReadTokens: 2000,
			usage.OutputTokens:    5000,
			usage.ReasoningTokens: 300,
		}),
		ActualCost:      usage.KnownCost(cost),
		EnforcementMode: engine.ModeEnforce,
		Status:          activity.StatusSettled,
		Outcome:         activity.OutcomeSuccess,
		StartedAt:       at,
		CompletedAt:     at.Add(3 * time.Second),
		Latency:         3 * time.Second,
	}
}

// An OpenAI event reports its three identity facets, its exact provider model ID, and
// every usage dimension -- with no provider-specific code anywhere in this package.
func TestOpenAIEventReportsItsIdentityAndUsage(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(6), at)
	w.record(openAIRecord("r1", "research", p.ID, cents(6), at))

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(page.Events))
	}
	e := page.Events[0]

	if e.AccessProvider != "openai" || e.Publisher != "openai" {
		t.Errorf("access provider / publisher = %q / %q, want openai / openai",
			e.AccessProvider, e.Publisher)
	}
	// The fields coincide for this provider and stay separate anyway. Collapsing them
	// would break the question the publisher facet exists to answer, which is "how much
	// went to models this vendor published, wherever they were reached".
	if e.Family != "" {
		t.Errorf("Family = %q, want empty: OpenAI's model IDs carry no family throttle can "+
			"parse, and inventing one would be a guess dressed as a fact", e.Family)
	}
	// No canonical name exists, so the exact provider ID displays and says so.
	if e.Model != "gpt-5.1" {
		t.Errorf("Model = %q, want the exact provider model ID gpt-5.1", e.Model)
	}
	if e.ModelKnown {
		t.Error("ModelKnown = true for a model with no canonical mapping: the flag is what tells " +
			"a display it is showing a raw provider ID")
	}
	if e.ProviderModelID != "gpt-5.1" {
		t.Errorf("ProviderModelID = %q, want gpt-5.1: this is the string the bill will use",
			e.ProviderModelID)
	}
	if e.Operation != "responses" {
		t.Errorf("Operation = %q, want responses", e.Operation)
	}
	// Region is genuinely absent rather than blank-by-omission: OpenAI's Responses API
	// is not addressed by region, and the reporter renders the absence as unreported.
	if e.Region != "" {
		t.Errorf("Region = %q, want empty for a provider with no regional addressing", e.Region)
	}

	// Every dimension appears, including reasoning, which no Bedrock record produces.
	got := map[string]int64{}
	for _, u := range e.Usage {
		got[u.Dimension] = u.Count
		if !u.Token {
			t.Errorf("%s is not marked as a token dimension", u.Dimension)
		}
	}
	for dim, want := range map[string]int64{
		"input_tokens": 8000, "cache_read_tokens": 2000,
		"output_tokens": 5000, "reasoning_tokens": 300,
	} {
		if got[dim] != want {
			t.Errorf("%s = %d, want %d", dim, got[dim], want)
		}
	}
	// total_tokens is not a stored dimension. The reporter cannot invent it, and a
	// display that showed it would be reintroducing the figure pricing refuses to bill on.
	if _, ok := got["total_tokens"]; ok {
		t.Error("the reporter surfaced total_tokens, which is not a dimension throttle stores")
	}

	if e.Actual.State != CostKnown || e.Actual.Value != cents(6) {
		t.Errorf("Actual = %v/%s, want a known %s", e.Actual.State, e.Actual.Value, cents(6))
	}
	if e.EstimateQuality != string(usage.QualityConservative) {
		t.Errorf("EstimateQuality = %q, want conservative: OpenAI documents no billed-count "+
			"guarantee, so the estimate cannot claim to be exact", e.EstimateQuality)
	}
}

// A hosted OpenAI tool bills outside the response's usage object, so the token figure is
// a floor. It reports as partial, carries its reason, and never reads as a total.
func TestOpenAIHostedToolCostReportsAsAFloor(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(3), at)

	const why = "OpenAI bills web_search per call, outside the response's usage object"
	rec := openAIRecord("tooled", "research", p.ID, cents(3), at)
	rec.ActualCost = usage.PartialCost(cents(3), nil, why)
	w.record(rec)

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	e := page.Events[0]
	if e.Actual.State != CostPartial {
		t.Errorf("Actual.State = %q, want partial: the tokens priced exactly but they are not "+
			"the whole bill", e.Actual.State)
	}
	if !e.Actual.Floor() {
		t.Error("a hosted-tool cost must report as a floor, which is what puts the + on the end")
	}
	if !strings.Contains(e.Actual.Reason, "outside the response's usage object") {
		t.Errorf("Actual.Reason = %q, should explain why the figure is incomplete", e.Actual.Reason)
	}
	if txt := e.Actual.Text(money.Money.String); !strings.HasSuffix(txt, "+") {
		t.Errorf("rendered as %q, want a trailing +", txt)
	}

	// And the page total over it is a floor as well, because a total that silently
	// absorbed an incomplete figure would be a claim rather than a sum.
	if page.Summary.Complete {
		t.Error("a page containing a partially priced request must not report a complete total")
	}
}

// An OpenAI model absent from the catalog reports an unknown cost that cannot render as
// zero, while still reporting its identity and usage in full.
func TestUnpricedOpenAIModelIsNeverZero(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	const model = "gpt-6-turbo-2027-01-01"
	rec := openAIRecord("newmodel", "research", p.ID, 0, at)
	rec.Identity.ProviderModelID = model
	rec.Estimate.Identity = rec.Identity
	rec.Estimate.Cost = usage.UnknownCost("no price for " + model + " on openai")
	rec.Quote = pricing.CapturedQuote{}
	rec.ActualCost = usage.UnknownCost("no price for " + model + " on openai")
	rec.Reserved = dollars(1)
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeUnpriced
	w.record(rec)

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	e := page.Events[0]

	// A brand-new model is a valid identity, not a missing one.
	if e.Model != model || e.ProviderModelID != model {
		t.Errorf("Model / ProviderModelID = %q / %q, want %q for both", e.Model, e.ProviderModelID, model)
	}
	if n := len(e.Usage); n != 4 {
		t.Errorf("got %d usage dimensions, want 4: usage is known even when the price is not", n)
	}

	// Unresolved rather than unknown, because the hold still stands and the price may
	// yet be supplied.
	if e.Actual.State != CostUnresolved {
		t.Errorf("Actual.State = %q, want unresolved", e.Actual.State)
	}
	if txt := e.Actual.Text(money.Money.CentsString); txt != "unresolved" {
		t.Errorf("an unpriced OpenAI request renders as %q, want %q: a model throttle has never "+
			"heard of still consumed real tokens", txt, "unresolved")
	}
	// A zero-valued unresolved amount is still "displayable" -- an unresolved figure with
	// a floor above zero does render -- so the protection against $0.00 lives in Text,
	// which is the single place that decides what an incomplete figure looks like.
	if strings.HasPrefix(e.Actual.Text(money.Money.CentsString), "$") {
		t.Error("an unpriced request rendered as a currency figure")
	}
	if !strings.Contains(e.Actual.Reason, model) {
		t.Errorf("Actual.Reason = %q, should name the model it could not price", e.Actual.Reason)
	}
	// The hold is reported separately and is never folded into spend.
	if e.Reserved != dollars(1) {
		t.Errorf("Reserved = %s, want %s", e.Reserved, dollars(1))
	}
}

// A request served on a service tier that was never priced reports neither zero nor the
// rate that was priced.
//
// The reporting half of #30. This record is the harder case than an unpriced model: the
// captured quote is valid, it carries rates, and those rates would produce a concrete
// figure. Reporting has to show that the amount is not known rather than showing the one
// number that happens to be at hand, because the tier that actually ran re-prices every
// dimension and the frozen rates bound the real cost in neither direction.
func TestUncapturedServiceTierRendersAsUnresolvedNotTheKnownRate(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	// What the frozen rates would have charged, if reporting were to reach for them.
	// Asserted against below, so this test fails if the refused figure ever surfaces.
	priced := cents(6)

	const served = "turbo-2027"
	reason := `gpt-5.1 was served on service tier "` + served + `", which was not among the ` +
		`tiers priced when this request was admitted (captured: priority)`

	rec := openAIRecord("uncaptured-tier", "research", p.ID, priced, at)
	rec.Identity.ServiceTier = served // the tier that ran, not the one that was priced
	rec.Estimate.Cost = usage.KnownCost(priced * 2)
	rec.ActualCost = usage.UnknownCost(reason)
	rec.Reserved = priced * 2
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeUnpriced

	// Anti-vacuous: the same record, priced from the frozen rates as the old fallback
	// would have, renders as a plain figure. So the assertions below are about the
	// completeness of this cost and not about a formatter that cannot print money.
	fallback := amountOf(usage.KnownCost(priced), activity.StatusSettled)
	if got := fallback.Text(money.Money.CentsString); got != priced.CentsString() {
		t.Fatalf("the pre-#30 fallback amount renders as %q, want %q: this test cannot prove the new "+
			"behavior refuses a figure it was never able to produce", got, priced.CentsString())
	}

	w.record(rec)

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	e := page.Events[0]

	if n := len(e.Usage); n != 4 {
		t.Errorf("got %d usage dimensions, want 4: the call happened and its usage is known", n)
	}

	if e.Actual.State != CostUnresolved {
		t.Errorf("Actual.State = %q, want unresolved: the tier that served this call had no frozen rate",
			e.Actual.State)
	}
	txt := e.Actual.Text(money.Money.CentsString)
	if txt != "unresolved" {
		t.Errorf("an uncaptured-tier request renders as %q, want %q", txt, "unresolved")
	}
	// Item 11: not zero. Item 12: not the amount the priced tier would have charged.
	if strings.Contains(txt, "0.00") {
		t.Errorf("rendered as %q: a request that consumed tokens must not read as free", txt)
	}
	if strings.Contains(txt, priced.CentsString()) {
		t.Errorf("rendered as %q, which is the %s the *priced* tier would have charged: the rate for "+
			"the tier that actually ran was never captured", txt, priced.CentsString())
	}
	if e.Actual.Value != 0 {
		t.Errorf("Actual.Value = %s, want 0: no part of this cost is established", e.Actual.Value)
	}
	if !strings.Contains(e.Actual.Reason, served) {
		t.Errorf("Actual.Reason = %q, should name the tier it could not price", e.Actual.Reason)
	}

	// The hold stays encumbered and stays out of spend, so the budget does not hand the
	// same headroom out twice for money already spent.
	if e.Reserved != priced*2 {
		t.Errorf("Reserved = %s, want %s: the hold on an unresolved cost stands", e.Reserved, priced*2)
	}
	if page.Summary.Complete {
		t.Error("a page holding an unresolved request must not report a complete total")
	}
	if page.Summary.Unresolved != 1 {
		t.Errorf("Summary.Unresolved = %d, want 1: an uncaptured tier is a liability to chase, "+
			"not a settled row", page.Summary.Unresolved)
	}

	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Position.Spent != 0 {
		t.Errorf("Position.Spent = %s, want 0: an unresolved cost is not spend", sum.Position.Spent)
	}
	if sum.Health.Unresolved != 1 {
		t.Errorf("Health.Unresolved = %d, want 1: the budget must show that one request's cost is "+
			"outstanding", sum.Health.Unresolved)
	}
}

// Every facet groups a mixed-provider budget correctly, and the OpenAI rows land where
// they belong without a single provider-specific branch.
func TestFacetsGroupMixedProviders(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	w.spend("s1", "research", cents(60), at)
	w.spend("s2", "research", cents(6), at.Add(time.Minute))
	w.spend("s3", "research", cents(4), at.Add(2*time.Minute))
	w.record(settledRecord("bedrock1", "research", p.ID, cents(60), at))
	w.record(openAIRecord("openai1", "research", p.ID, cents(6), at.Add(time.Minute)))
	w.record(openAIRecord("openai2", "research", p.ID, cents(4), at.Add(2*time.Minute)))

	q := ActivityQuery{BudgetID: "research"}
	for _, tc := range []struct {
		facet Facet
		key   string
		spend money.Money
		reqs  int
	}{
		// Both OpenAI requests group under one access provider, separately from Bedrock.
		{FacetAccessProvider, "openai", cents(10), 2},
		{FacetAccessProvider, "aws-bedrock", cents(60), 1},
		// OpenAI publishes what it serves; Anthropic's model was reached through Bedrock.
		// Two different access paths, two different publishers, one budget.
		{FacetPublisher, "openai", cents(10), 2},
		{FacetPublisher, "anthropic", cents(60), 1},
		{FacetModel, "gpt-5.1", cents(10), 2},
		{FacetProviderModel, "gpt-5.1", cents(10), 2},
		{FacetOperation, "responses", cents(10), 2},
		{FacetOperation, "converse", cents(60), 1},
	} {
		b, err := w.rep.Breakdown(w.ctx, tc.facet, q)
		if err != nil {
			t.Fatalf("Breakdown(%s): %v", tc.facet, err)
		}
		var row *BreakdownRow
		for i := range b.Rows {
			if b.Rows[i].Key == tc.key {
				row = &b.Rows[i]
			}
		}
		if row == nil {
			t.Errorf("Breakdown(%s) has no row for %q; rows = %v", tc.facet, tc.key, keys(b.Rows))
			continue
		}
		if row.Spend != tc.spend || row.Requests != tc.reqs {
			t.Errorf("Breakdown(%s)[%s] = %s over %d requests, want %s over %d",
				tc.facet, tc.key, row.Spend, row.Requests, tc.spend, tc.reqs)
		}
		if !row.Complete {
			t.Errorf("Breakdown(%s)[%s] reports an incomplete total for fully priced requests",
				tc.facet, tc.key)
		}
	}

	// The region facet is where a mixed deployment shows an honest gap: OpenAI reports
	// no region, and the empty key is what the display renders as "not reported" rather
	// than as a region named "".
	b, err := w.rep.Breakdown(w.ctx, FacetRegion, q)
	if err != nil {
		t.Fatalf("Breakdown(region): %v", err)
	}
	var blank *BreakdownRow
	for i := range b.Rows {
		if b.Rows[i].Key == "" {
			blank = &b.Rows[i]
		}
	}
	if blank == nil {
		t.Fatalf("the region breakdown has no unreported row; rows = %v", keys(b.Rows))
	}
	if blank.Requests != 2 {
		t.Errorf("the unreported region row covers %d requests, want the 2 OpenAI ones",
			blank.Requests)
	}
}

// keys lists the row keys of a breakdown, for failure messages.
func keys(rows []BreakdownRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Key == "" {
			out = append(out, "(unreported)")
			continue
		}
		out = append(out, r.Key)
	}
	return out
}

// The detail view of an OpenAI request carries its frozen pricing basis, which is what
// makes the settled figure auditable after the catalog has moved on.
func TestOpenAIDetailCarriesItsFrozenBasis(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(6), at)
	w.record(openAIRecord("r1", "research", p.ID, cents(6), at))

	d, err := w.rep.Detail(w.ctx, "r1")
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if d.Event.RequestID != "r1" {
		t.Fatalf("Detail returned request %q, want r1", d.Event.RequestID)
	}
	// The provenance travels with the record rather than being looked up again, so a
	// reader can see which published table this cost came from -- and that it came from
	// a hand-entered fixture rather than a live feed.
	if !strings.Contains(d.QuoteSource, "developers.openai.com") {
		t.Errorf("QuoteSource = %q, should name where the rates were read from", d.QuoteSource)
	}
	if d.QuoteVersion != "2026-08-12-fixture-1" {
		t.Errorf("QuoteVersion = %q, want the fixture version: a stale hand-entered rate must "+
			"stay visibly versioned rather than passing for live pricing", d.QuoteVersion)
	}
}
