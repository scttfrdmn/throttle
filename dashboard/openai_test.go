package dashboard

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

// These tests ask one question: can the dashboard display an OpenAI request without
// learning anything about OpenAI?
//
// Nothing here imports the adapter or its SDK. The records are built from the same
// normalized durable facts the adapter writes -- an access provider string, a publisher
// string, a verbatim provider model ID, dimensional usage, a three-valued cost -- and
// the assertion is that the existing UI renders them correctly with no new code. If a
// column had to be added, a facet special-cased, or a dimension taught to the formatter,
// that would be evidence the reporting abstraction was shaped around Bedrock rather than
// being provider-neutral, and the right response would be to fix the abstraction rather
// than to add an OpenAI branch.

// openAIIdentity is the identity the Responses adapter records.
//
// Three things about it differ from the Bedrock shape, and each is deliberate. The
// access provider and the publisher are the same string, because OpenAI publishes the
// models it serves -- which is a fact about this provider, not a reason to collapse two
// fields. CanonicalModel is empty, because there is nothing structural to parse out of
// "gpt-5.1" and inventing a canonical name would be a guess. And ServiceTier is
// populated, which Bedrock's fixtures never exercise.
func openAIIdentity() usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "openai",
		Publisher:       "openai",
		ProviderModelID: "gpt-5.1",
		Operation:       "responses",
		ServiceTier:     "priority",
	}
}

// openAIRecord is an ordinary priced Responses request, with the dimensional usage the
// Responses API actually reports: fresh input, cached input, visible output, and
// reasoning, all disjoint after the adapter's subtractive normalization.
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
			// Never exact: OpenAI does not document its count endpoint as the billed
			// count, and the output half is a ceiling either way.
			Quality: usage.QualityConservative,
		},
		Quote: pricing.CapturedQuote{
			AccessProvider:  "openai",
			ProviderModelID: "gpt-5.1",
			ServiceTier:     "priority",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(2)),
			},
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
		// A Response carries no latency field, so the adapter attributes none. The
		// display must cope with the absence rather than showing a zero as a measurement.
		ProviderLatency: 0,
		Metadata:        map[string]string{"openai.served_model": "gpt-5.1-2026-07-14"},
	}
}

// An OpenAI request renders with its own three identity facets, and the model falls back
// to the exact provider ID because OpenAI names no canonical model.
func TestOpenAIRequestRendersItsIdentity(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(6), at)
	w.record(openAIRecord("r1", "research", p.ID, cents(6), at))

	body := w.html("/?budget=research")

	access := cellsUnder(t, body, "Access provider")
	publisher := cellsUnder(t, body, "Publisher")
	model := cellsUnder(t, body, "Model")
	if len(access) == 0 || len(publisher) == 0 || len(model) == 0 {
		t.Fatalf("identity columns are missing: access=%v publisher=%v model=%v",
			access, publisher, model)
	}
	if access[0] != "openai" {
		t.Errorf("access provider = %q, want openai", access[0])
	}
	if publisher[0] != "openai" {
		t.Errorf("publisher = %q, want openai", publisher[0])
	}
	// The exact string the caller sent and the bill will name, not a prettified one.
	if model[0] != "gpt-5.1" {
		t.Errorf("model = %q, want the exact provider model ID gpt-5.1", model[0])
	}
	// Access provider and publisher coinciding is a fact about OpenAI, and the columns
	// stay separate anyway: a reader asking "how much did I spend on OpenAI-published
	// models, wherever they were reached?" needs the publisher field to exist.
	mustContain(t, body, "three independent",
		"the page must still explain that the identity fields are independent")

	// The model is marked unmapped rather than being dressed up as a canonical name.
	mustContain(t, body, "unmapped",
		"a model with no canonical name must be marked as showing a raw provider ID")

	if ops := cellsUnder(t, body, "Operation"); len(ops) == 0 || ops[0] != "responses" {
		t.Errorf("Operation = %v, want responses: the operation is what tells a later "+
			"reader which OpenAI API a record belongs to", ops)
	}

	detail := w.html("/request/r1")
	mustContain(t, detail, "gpt-5.1", "the detail page must name the provider model ID")
	mustContain(t, detail, "not in the catalog, so the exact provider ID is shown",
		"the detail page must explain the fallback for an OpenAI model")
	mustContain(t, detail, "developers.openai.com/api/docs/pricing",
		"the detail page must attribute the price to its published source")
	mustContain(t, detail, "2026-08-12-fixture-1",
		"a hand-entered fixture version must be visible rather than passing for live pricing")
}

// The Responses usage shape renders with no new formatting code: four disjoint token
// dimensions, including reasoning, which no Bedrock fixture produces.
func TestOpenAIUsageDimensionsRenderWithoutProviderCode(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(6), at)
	w.record(openAIRecord("r1", "research", p.ID, cents(6), at))

	body := w.html("/?budget=research")

	// The summary column groups the cache read with input and reasoning with output,
	// which is what the provider bills them as. 8,000 + 2,000 in; 5,000 + 300 out.
	if got := cellsUnder(t, body, "Usage"); len(got) == 0 || got[0] != "10,000 in / 5,300 out" {
		t.Errorf("Usage cell = %v, want %q", got, "10,000 in / 5,300 out")
	}

	// The detail page shows every dimension as recorded, so the breakdown that avoided
	// double-charging is auditable rather than merely asserted in a unit test.
	detail := w.html("/request/r1")
	for dim, count := range map[string]string{
		"input_tokens":      "8,000",
		"cache_read_tokens": "2,000",
		"output_tokens":     "5,000",
		"reasoning_tokens":  "300",
	} {
		mustContain(t, detail, dim, "the usage table must show the "+dim+" dimension as recorded")
		mustContain(t, detail, count, "the usage table must show the recorded count for "+dim)
	}
	// Reasoning tokens are accounting metadata and appear as a count. Reasoning content
	// is content, is never stored, and so cannot appear anywhere.
	mustNotContain(t, detail, "reasoning_text",
		"reasoning content must not reach a page, because it is not stored")
	mustNotContain(t, detail, "encrypted_content",
		"an encrypted reasoning payload must not reach a page")

	// total_tokens is not a dimension throttle stores, and a display that invented it
	// would be reintroducing the number the pricing path deliberately refuses to bill on.
	mustNotContain(t, detail, "total_tokens",
		"total_tokens is not the billing primitive and is not a stored dimension")
}

// A hosted OpenAI tool bills outside the response's usage object, so the token cost is a
// floor. It renders as a floor and never as a total, and never as zero.
func TestOpenAIHostedToolCostRendersAsAFloor(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(3), at)

	rec := openAIRecord("tooled", "research", p.ID, cents(3), at)
	// The tokens priced perfectly. The point is that they are not the whole bill: OpenAI
	// charges web search per call, on the pricing page and nowhere in the response.
	rec.ActualCost = usage.PartialCost(cents(3), nil,
		"OpenAI bills web_search outside the response's usage object (per call, per stored "+
			"GB, or per container session), so the token cost is a floor rather than a total")
	w.record(rec)

	body := w.html("/?budget=research")
	acts := panel(t, body, "activity")
	actuals := cellsUnder(t, acts, "Actual")
	if len(actuals) != 1 {
		t.Fatalf("activity table has %d rows, want 1: %v", len(actuals), actuals)
	}
	if actuals[0] != "$0.0300+" {
		t.Errorf("a hosted-tool request renders as %q, want %q: the token cost is a floor",
			actuals[0], "$0.0300+")
	}

	// The page total over it is a floor too, and says so.
	if spend := figure(t, body, "page-spend"); !strings.HasSuffix(spend, "+") {
		t.Errorf("page spend = %q, want a trailing + to mark a floor", spend)
	}

	// The reason travels to the reader rather than being swallowed, because "why is this
	// a floor?" is the question the notation raises.
	detail := w.html("/request/tooled")
	mustSay(t, detail, "outside the response's usage object",
		"the detail page must explain why an OpenAI hosted-tool cost is incomplete")
}

// An OpenAI model the fixture catalog has never heard of produces an unknown cost, which
// renders as unknown and never as a free request.
func TestUnpricedOpenAIModelNeverRendersAsFree(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	rec := openAIRecord("newmodel", "research", p.ID, 0, at)
	// A model OpenAI released this morning. The identity is valid and the usage is
	// known; only the money is not.
	rec.Identity.ProviderModelID = "gpt-6-turbo-2027-01-01"
	rec.Estimate.Identity = rec.Identity
	rec.Estimate.Cost = usage.UnknownCost("no price for gpt-6-turbo-2027-01-01 on openai")
	rec.Quote = pricing.CapturedQuote{}
	rec.ActualCost = usage.UnknownCost("no price for gpt-6-turbo-2027-01-01 on openai")
	rec.Reserved = dollars(1)
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeUnpriced
	w.record(rec)

	body := w.html("/?budget=research")
	acts := panel(t, body, "activity")

	actuals := cellsUnder(t, acts, "Actual")
	if len(actuals) != 1 {
		t.Fatalf("activity table has %d rows, want 1: %v", len(actuals), actuals)
	}
	// Unresolved rather than merely unknown: the reservation is still encumbered and the
	// information that would price it may yet arrive.
	if actuals[0] != "unresolved" {
		t.Errorf("an unpriced OpenAI request renders as %q, want %q", actuals[0], "unresolved")
	}
	for _, cell := range actuals {
		if strings.HasPrefix(cell, "$0.00") {
			t.Errorf("an unpriced request renders as %q: a model with no catalog entry "+
				"consumed real tokens and is not free", cell)
		}
	}

	// The model still displays, under its exact provider ID, because an unrecognized
	// model is a normal state and not an absence.
	if model := cellsUnder(t, acts, "Model"); len(model) == 0 || model[0] != "gpt-6-turbo-2027-01-01" {
		t.Errorf("Model = %v, want the exact provider model ID", model)
	}

	// The headroom it still holds is shown as held, not as spent.
	if held := cellsUnder(t, acts, "Reserved"); len(held) == 0 || held[0] != "$1.0000" {
		t.Errorf("Reserved = %v, want $1.0000: an unresolved liability's hold is still held", held)
	}

	detail := w.html("/request/newmodel")
	mustContain(t, detail, "no price for gpt-6-turbo-2027-01-01 on openai",
		"the detail page must say why the cost is unknown")
}

// Two access providers in one budget group into one breakdown, with no provider-specific
// code and no collision.
//
// This is the read-side counterpart of the pricing key: a mixed deployment is the normal
// case, and "how much went through OpenAI versus Bedrock?" has to be one query over one
// store rather than two views that a reader adds up by hand.
func TestMixedProvidersShareOneBreakdown(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	w.spend("s1", "research", cents(60), at)
	w.spend("s2", "research", cents(6), at.Add(time.Minute))
	w.record(settledRecord("bedrock1", "research", p.ID, cents(60), at))
	w.record(openAIRecord("openai1", "research", p.ID, cents(6), at.Add(time.Minute)))

	body := w.html("/?budget=research")

	// Both providers appear, in one table, ordered by spend.
	for _, want := range []string{"aws-bedrock", "openai", "anthropic", "gpt-5.1"} {
		mustContain(t, body, want, "the breakdown must include "+want)
	}

	// And the publisher facet distinguishes them, which is the whole reason it is a
	// separate field: the same publisher can be reached through more than one access
	// path, and the same access path serves more than one publisher.
	access := cellsUnder(t, body, "Access provider")
	if len(access) != 2 {
		t.Fatalf("the activity table has %d rows, want 2: %v", len(access), access)
	}
	seen := map[string]bool{access[0]: true, access[1]: true}
	if !seen["openai"] || !seen["aws-bedrock"] {
		t.Errorf("access providers = %v, want both openai and aws-bedrock", access)
	}

	// The JSON surface carries the same facts, since it is what the poll reads.
	raw := rawBody(t, w, "/api/breakdown?facet=access-provider&budget=research")
	for _, want := range []string{"openai", "aws-bedrock"} {
		if !strings.Contains(raw, want) {
			t.Errorf("/api/breakdown does not carry %q", want)
		}
	}
}

// A Response reports no latency, so the adapter records none, and the display shows the
// absence as an absence rather than as a measured zero.
func TestAbsentProviderLatencyIsNotShownAsZero(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(6), at)
	w.record(openAIRecord("r1", "research", p.ID, cents(6), at))

	// The wall-clock latency the caller measured is real and is shown.
	body := w.html("/?budget=research")
	if got := cellsUnder(t, body, "Latency"); len(got) == 0 || got[0] != "3.0s" {
		t.Errorf("Latency = %v, want 3.0s", got)
	}
	// A zero provider latency renders as an em dash, not as "0ms", which would be a
	// fabricated measurement.
	if got := latency(0); got != "—" {
		t.Errorf("latency(0) = %q, want an em dash: OpenAI reports no provider-side "+
			"latency, and inventing one would be a fabrication", got)
	}
}
