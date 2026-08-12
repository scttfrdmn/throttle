package report

import (
	"testing"
	"time"

	"throttle/activity"
	"throttle/money"
	"throttle/usage"
)

// mixedWorld records three requests with deliberately different identities: two
// Anthropic models reached through Bedrock, and one OpenAI model reached directly.
func mixedWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	sonnet := settledRecord("r1", "research", p.ID, dollars(6), w.now)
	w.record(sonnet)

	haiku := settledRecord("r2", "research", p.ID, dollars(1), w.now.Add(time.Second))
	haiku.Identity = usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		Publisher:       "anthropic",
		Family:          "claude-haiku",
		CanonicalModel:  "claude-haiku-4",
		ProviderModelID: "anthropic.claude-haiku-4-20250514-v1:0",
		Operation:       "converse",
		Region:          "us-west-2",
	}
	w.record(haiku)

	direct := settledRecord("r3", "research", p.ID, dollars(3), w.now.Add(2*time.Second))
	direct.Identity = usage.ModelIdentity{
		AccessProvider:  "openai",
		Publisher:       "openai",
		Family:          "gpt-5",
		CanonicalModel:  "gpt-5",
		ProviderModelID: "gpt-5-2025-08-07",
		Operation:       "responses",
	}
	direct.Estimate.Identity = direct.Identity
	w.record(direct)

	return w
}

func rowsByKey(b Breakdown) map[string]BreakdownRow {
	out := make(map[string]BreakdownRow, len(b.Rows))
	for _, r := range b.Rows {
		out[r.Key] = r
	}
	return out
}

// The three identity facets answer three different questions, and each answers its own.
func TestBreakdownsKeepTheThreeIdentityFacetsSeparate(t *testing.T) {
	w := mixedWorld(t)

	bs, err := w.rep.Breakdowns(w.ctx,
		[]Facet{FacetAccessProvider, FacetPublisher, FacetModel, FacetFamily, FacetProviderModel},
		ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdowns: %v", err)
	}
	if len(bs) != 5 {
		t.Fatalf("got %d breakdowns, want 5", len(bs))
	}

	// "How much went through Bedrock?" -- $7 of the $10.
	access := rowsByKey(bs[0])
	if got := access["aws-bedrock"].Spend; got != dollars(7) {
		t.Errorf("aws-bedrock spend = %s, want %s", got, dollars(7))
	}
	if got := access["openai"].Spend; got != dollars(3) {
		t.Errorf("openai access spend = %s, want %s", got, dollars(3))
	}
	if access["aws-bedrock"].Requests != 2 {
		t.Errorf("aws-bedrock requests = %d, want 2", access["aws-bedrock"].Requests)
	}

	// "How much did I spend on Anthropic models?" -- the same $7, but this is a
	// different question with a different answer shape, and both rows exist.
	pub := rowsByKey(bs[1])
	if got := pub["anthropic"].Spend; got != dollars(7) {
		t.Errorf("anthropic publisher spend = %s, want %s", got, dollars(7))
	}
	if _, ok := pub["aws-bedrock"]; ok {
		t.Error("the publisher breakdown has an aws-bedrock row; the facets are collapsed")
	}

	// "Which model cost the most?" -- three rows, one per model.
	models := rowsByKey(bs[2])
	if len(models) != 3 {
		t.Errorf("model breakdown has %d rows, want 3: %+v", len(models), bs[2].Rows)
	}
	if got := models["claude-sonnet-4"].Spend; got != dollars(6) {
		t.Errorf("claude-sonnet-4 spend = %s, want %s", got, dollars(6))
	}

	// Family and exact provider ID are separate again.
	fam := rowsByKey(bs[3])
	if len(fam) != 3 {
		t.Errorf("family breakdown has %d rows, want claude-sonnet, claude-haiku, gpt-5", len(fam))
	}
	provider := rowsByKey(bs[4])
	if _, ok := provider["anthropic.claude-sonnet-4-20250514-v1:0"]; !ok {
		t.Errorf("the provider-model breakdown lost the exact identifier: %+v", bs[4].Rows)
	}

	for _, b := range bs {
		if b.Total != dollars(10) {
			t.Errorf("%s total = %s, want %s", b.Facet, b.Total, dollars(10))
		}
		if !b.Complete {
			t.Errorf("%s is incomplete with three fully priced requests", b.Facet)
		}
		if b.Requests != 3 {
			t.Errorf("%s counted %d requests, want 3", b.Facet, b.Requests)
		}
	}
}

// A model the catalog has never heard of gets its own row under its real provider ID,
// rather than being lumped into an "unknown" bucket with every other one.
func TestUnknownModelsGetTheirOwnBreakdownRows(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	for i, id := range []string{"vendor.new-a-v1:0", "vendor.new-b-v1:0"} {
		rec := settledRecord("r"+string(rune('1'+i)), "research", p.ID, dollars(2), w.now)
		rec.Identity = usage.ModelIdentity{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: id,
			Operation:       "converse",
		}
		w.record(rec)
	}

	b, err := w.rep.Breakdown(w.ctx, FacetModel, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(b.Rows) != 2 {
		t.Fatalf("got %d rows, want one per unrecognized model: %+v", len(b.Rows), b.Rows)
	}
	rows := rowsByKey(b)
	for _, id := range []string{"vendor.new-a-v1:0", "vendor.new-b-v1:0"} {
		if rows[id].Spend != dollars(2) {
			t.Errorf("row %q spend = %s, want %s", id, rows[id].Spend, dollars(2))
		}
	}
	if _, ok := rows[""]; ok {
		t.Error("an empty-keyed row appeared; unrecognized models were bucketed")
	}

	// Publisher, by contrast, does not fall back. An absent dimension is reported
	// absent rather than filled in from a neighbouring field.
	pb, err := w.rep.Breakdown(w.ctx, FacetPublisher, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(pb.Rows) != 1 || pb.Rows[0].Key != "" {
		t.Errorf("publisher rows = %+v, want a single empty key rather than an invented publisher", pb.Rows)
	}
}

// A breakdown containing an unpriceable request adds up to a floor, and says so.
func TestBreakdownTotalIsAFloorWhenARequestIsUnpriced(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	w.record(settledRecord("r1", "research", p.ID, dollars(4), w.now))

	partial := settledRecord("r2", "research", p.ID, dollars(0), w.now.Add(time.Second))
	partial.ActualCost = usage.PartialCost(dollars(1),
		[]usage.Dimension{usage.CacheWriteTokens}, "no rate for cache writes")
	w.record(partial)

	b, err := w.rep.Breakdown(w.ctx, FacetAccessProvider, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if b.Complete {
		t.Error("Complete = true with an unpriced dimension in the set")
	}
	if b.Total != dollars(5) {
		t.Errorf("Total = %s, want the floor %s", b.Total, dollars(5))
	}
	if len(b.Rows) != 1 || b.Rows[0].Complete {
		t.Errorf("the row does not report itself as a floor: %+v", b.Rows)
	}
}

// Reserved headroom is counted separately and never added into spend.
func TestBreakdownKeepsReservedOutOfSpend(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	w.record(settledRecord("r1", "research", p.ID, dollars(4), w.now))

	stuck := settledRecord("r2", "research", p.ID, dollars(0), w.now.Add(time.Second))
	stuck.Status = activity.StatusUnresolved
	stuck.ActualCost = usage.UnknownCost("no rate")
	stuck.Reserved = dollars(3)
	w.record(stuck)

	b, err := w.rep.Breakdown(w.ctx, FacetAccessProvider, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	row := b.Rows[0]
	if row.Spend != dollars(4) {
		t.Errorf("Spend = %s, want only the settled %s", row.Spend, dollars(4))
	}
	if row.Reserved != dollars(3) {
		t.Errorf("Reserved = %s, want %s held separately", row.Reserved, dollars(3))
	}
	if row.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1", row.Unresolved)
	}
	if b.Total != dollars(4) {
		t.Errorf("Total = %s; encumbered money is not spend", b.Total)
	}
}

// Rows are ordered by spend and then by key, so the table does not reshuffle between
// page loads when several groups spent the same amount -- including nothing.
func TestBreakdownRowOrderIsStable(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	for i, name := range []string{"zebra", "alpha", "middle"} {
		rec := settledRecord("r"+string(rune('1'+i)), "research", p.ID, dollars(0), w.now)
		rec.Identity.Operation = name
		w.record(rec)
	}

	first, err := w.rep.Breakdown(w.ctx, FacetOperation, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(first.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(first.Rows))
	}
	want := []string{"alpha", "middle", "zebra"}
	for i, k := range want {
		if first.Rows[i].Key != k {
			t.Errorf("row %d = %q, want %q", i, first.Rows[i].Key, k)
		}
	}

	second, err := w.rep.Breakdown(w.ctx, FacetOperation, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	for i := range first.Rows {
		if first.Rows[i].Key != second.Rows[i].Key {
			t.Errorf("row %d moved between reads: %q -> %q", i, first.Rows[i].Key, second.Rows[i].Key)
		}
	}
}

// A bar width comes from the same integer arithmetic as everything else, and a zero
// total does not divide by zero.
func TestShareBasisPointsIsSafeAndExact(t *testing.T) {
	row := BreakdownRow{Spend: dollars(25)}
	if got := row.ShareBasisPoints(dollars(100)); got != 2_500 {
		t.Errorf("ShareBasisPoints = %d, want 2500", got)
	}
	if got := row.ShareBasisPoints(dollars(25)); got != 10_000 {
		t.Errorf("a row that is the whole total = %d, want 10000", got)
	}
	if got := row.ShareBasisPoints(0); got != 0 {
		t.Errorf("ShareBasisPoints with a zero total = %d, want 0", got)
	}
	if got := row.ShareBasisPoints(-dollars(5)); got != 0 {
		t.Errorf("ShareBasisPoints with a negative total = %d, want 0", got)
	}
	if got := (BreakdownRow{}).ShareBasisPoints(dollars(100)); got != 0 {
		t.Errorf("a row that spent nothing = %d, want 0", got)
	}
}

// Every facet has a human label, so no view renders a raw identifier as a heading.
func TestEveryFacetHasALabel(t *testing.T) {
	for _, f := range Facets {
		if f.Label() == "" || f.Label() == string(f) {
			t.Errorf("facet %q has no human label", f)
		}
	}
	if got := Facet("invented").Label(); got != "invented" {
		t.Errorf("an unknown facet labelled %q, want its own name as a fallback", got)
	}
}

// A breakdown over no records is an empty breakdown, not an error.
func TestBreakdownOfNothingIsEmpty(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	b, err := w.rep.Breakdown(w.ctx, FacetModel, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(b.Rows) != 0 || b.Total != 0 || b.Requests != 0 {
		t.Errorf("breakdown of nothing = %+v, want empty", b)
	}
	if !b.Complete {
		t.Error("Complete = false for a breakdown with nothing incomplete in it")
	}
}

// Statuses are a facet too, which is what lets a reader see that a chunk of the period
// was denied rather than spent.
func TestStatusBreakdownSeparatesDeniedFromSettled(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))

	w.record(settledRecord("r1", "research", p.ID, dollars(4), w.now))

	denied := settledRecord("r2", "research", p.ID, dollars(0), w.now.Add(time.Second))
	denied.Status = activity.StatusDenied
	denied.Outcome = activity.OutcomeBudgetDenied
	denied.ActualCost = usage.Cost{}
	denied.Reserved = 0
	w.record(denied)

	b, err := w.rep.Breakdown(w.ctx, FacetStatus, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	rows := rowsByKey(b)
	if rows[string(activity.StatusSettled)].Spend != dollars(4) {
		t.Errorf("settled spend = %s, want %s", rows[string(activity.StatusSettled)].Spend, dollars(4))
	}
	if got, ok := rows[string(activity.StatusDenied)]; !ok {
		t.Error("the denied request does not appear in the status breakdown")
	} else if got.Spend != 0 {
		t.Errorf("denied spend = %s, want zero", got.Spend)
	}
	if b.Total != dollars(4) {
		t.Errorf("Total = %s; a denied request spent nothing", b.Total)
	}
}

// money.Add saturating behaviour must not silently corrupt a group total. This pins
// the arithmetic contract the aggregation relies on.
func TestBreakdownArithmeticSaturatesRatherThanWrapping(t *testing.T) {
	if _, ok := money.Add(money.Max, money.Money(1)); ok {
		t.Error("money.Add reported success past the maximum; the aggregation relies on it not")
	}
}
