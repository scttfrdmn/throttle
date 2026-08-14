package pricing_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

const (
	sonnetID = "anthropic.claude-sonnet-4-20250514-v1:0"
	haikuID  = "anthropic.claude-haiku-4-5-20251001-v1:0"
	novaID   = "amazon.nova-lite-v1:0"
)

// agentCatalog prices three models, as a catalog serving a managed agent turn
// would: the turn names an agent, so any of them may be invoked.
func agentCatalog(t *testing.T) *pricing.Static {
	t.Helper()
	price := func(id, in, out string) pricing.Price {
		return pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: id,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, in)),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, out)),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "v1"},
		}
	}
	cat, err := pricing.NewStatic(
		price(sonnetID, "3.00", "15.00"),
		price(haikuID, "1.00", "5.00"),
		price(novaID, "0.06", "0.24"),
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	return cat
}

// agentIdentity is a model identity under the access dimensions a capture narrows
// to. It differs from identity in naming a region, which is what CaptureSet keys on.
func agentIdentity(modelID string) usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: modelID,
		Operation:       "invoke-agent",
		Region:          "us-east-1",
	}
}

// step is one observed model invocation.
func step(modelID string, in, out int64) pricing.Component {
	return pricing.Component{
		Identity: agentIdentity(modelID),
		Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: in, usage.OutputTokens: out}),
	}
}

// captureSet freezes the whole candidate rate set, as admission does.
func captureSet(t *testing.T, cat pricing.RateSource) pricing.QuoteSet {
	t.Helper()
	set, err := pricing.CaptureSet(cat, "aws-bedrock", "us-east-1", at)
	if err != nil {
		t.Fatalf("CaptureSet: %v", err)
	}
	if !set.Valid() {
		t.Fatalf("the captured set prices nothing: %s", set.Note)
	}
	return set
}

// The core guarantee, widened from one model to a set: a price change landing
// between admission and settlement must not change what an in-flight compound
// request costs, for any model the agent turns out to have invoked.
func TestQuoteSetSurvivesCatalogChange(t *testing.T) {
	cat := agentCatalog(t)
	set := captureSet(t, cat)

	// Reprice every model a hundredfold, after the capture.
	for _, p := range []struct{ id, in, out string }{
		{sonnetID, "300.00", "1500.00"},
		{haikuID, "100.00", "500.00"},
		{novaID, "6.00", "24.00"},
	} {
		if err := cat.Override(pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: p.id,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, p.in)),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, p.out)),
			},
		}); err != nil {
			t.Fatalf("Override %s: %v", p.id, err)
		}
	}

	// A turn that invoked all three: haiku for routing, sonnet for orchestration,
	// nova for a cheap step.
	cost, components, err := set.PriceComponents([]pricing.Component{
		step(haikuID, 200, 20),    // $0.0002 + $0.0001
		step(sonnetID, 1000, 100), // $0.003 + $0.0015
		step(novaID, 5000, 500),   // $0.0003 + $0.00012
	})
	if err != nil {
		t.Fatalf("PriceComponents: %v", err)
	}
	if !cost.Known() {
		t.Fatalf("cost = %s, want a known cost", cost)
	}
	want := dollars(t, "0.00522")
	if cost.Amount != want {
		t.Errorf("cost = %s, want %s from the rates frozen at capture", cost, want)
	}
	for i, c := range components {
		if !c.Priced {
			t.Errorf("component %d not priced: %s", i, c.Reason)
		}
	}
}

// The rounding boundary is the charge, not the step. A compound turn made of many
// tiny invocations is accumulated exactly and rounded once; rounding each step and
// summing drifts upward by up to a unit per step.
func TestQuoteSetRoundsOncePerCompoundCharge(t *testing.T) {
	set := captureSet(t, agentCatalog(t))

	// Nine nova-lite input tokens at $0.06/M is 0.54 microdollars: over half a unit,
	// so a step rounded alone becomes 1. Twenty of them is 10.8 exactly, which rounds
	// to 11.
	const steps = 20
	in := make([]pricing.Component, 0, steps)
	for range steps {
		in = append(in, step(novaID, 9, 0))
	}

	cost, components, err := set.PriceComponents(in)
	if err != nil {
		t.Fatalf("PriceComponents: %v", err)
	}
	if cost.Amount != 11 {
		t.Errorf("cost = %d microdollars, want 11 from one rounding of the exact 10.8", cost.Amount)
	}

	var sum money.Money
	for _, c := range components {
		sum += c.Amount
	}
	if sum != 20 {
		t.Fatalf("per-step amounts summed to %d, want 20; the fixture no longer exercises drift", sum)
	}
	if cost.Amount == sum {
		t.Error("the charge equals the sum of the separately rounded steps, so rounding is happening per step")
	}
}

// Many invocations each too small to round to a microdollar still cost something
// together. Rounding per step would charge nothing at all.
func TestQuoteSetManyTinyStepsDoNotRoundAwayToNothing(t *testing.T) {
	set := captureSet(t, agentCatalog(t))

	// One nova-lite input token is 0.06 microdollars, which rounds to zero alone.
	// A hundred of them is 6.
	in := make([]pricing.Component, 0, 100)
	for range 100 {
		in = append(in, step(novaID, 1, 0))
	}
	cost, components, err := set.PriceComponents(in)
	if err != nil {
		t.Fatalf("PriceComponents: %v", err)
	}
	if cost.Amount != 6 {
		t.Errorf("cost = %d microdollars, want 6", cost.Amount)
	}
	for _, c := range components {
		if c.Amount != 0 {
			t.Fatalf("a step rounded to %d; the fixture no longer has sub-unit steps", c.Amount)
		}
	}
}

// A model the set has no quote for makes the whole compound charge incomplete, even
// though the steps around it priced cleanly. The priced subset is a floor, and
// reporting it as the total would understate real spend.
func TestQuoteSetOneUnpriceableStepMakesTheWholeChargePartial(t *testing.T) {
	set := captureSet(t, agentCatalog(t))

	cost, components, err := set.PriceComponents([]pricing.Component{
		step(sonnetID, 1000, 100),
		step("anthropic.claude-unknown-v9:0", 2000, 300),
	})
	if !errors.Is(err, pricing.ErrNoRate) {
		t.Fatalf("err = %v, want ErrNoRate", err)
	}
	if cost.Known() {
		t.Errorf("cost = %s, want an incomplete cost", cost)
	}
	if cost.Completeness != usage.CostPartial {
		t.Errorf("completeness = %q, want partial: one step did price", cost.Completeness)
	}
	// The amount is the priced floor: 1000 at $3/M plus 100 at $15/M.
	if want := dollars(t, "0.0045"); cost.AtLeast() != want {
		t.Errorf("floor = %s, want %s", cost, want)
	}
	// The unpriced step's dimensions are named, so a reconciliation knows what the
	// catalog is missing.
	if len(cost.Unpriced) != 2 {
		t.Errorf("Unpriced = %v, want both dimensions of the unpriceable step named", cost.Unpriced)
	}
	if !strings.Contains(cost.Reason, "no captured price for anthropic.claude-unknown-v9:0") {
		t.Errorf("reason = %q, want it to name the model that had no captured price", cost.Reason)
	}
	if components[0].Priced == components[1].Priced {
		t.Error("the components must distinguish the step that priced from the one that did not")
	}
}

// A step whose model was never named is a different problem from a catalog gap, and
// the reason must not report it as one: nobody can add a price for an unnamed model.
func TestQuoteSetUnnamedModelIsDistinguishedFromACatalogGap(t *testing.T) {
	set := captureSet(t, agentCatalog(t))

	unnamed := pricing.Component{
		Identity: usage.ModelIdentity{AccessProvider: "aws-bedrock", Operation: "invoke-agent"},
		Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 1200, usage.OutputTokens: 180}),
	}
	cost, components, err := set.PriceComponents([]pricing.Component{step(sonnetID, 400, 40), unnamed})
	if !errors.Is(err, pricing.ErrNoRate) {
		t.Fatalf("err = %v, want ErrNoRate", err)
	}
	if cost.Completeness != usage.CostPartial {
		t.Errorf("completeness = %q, want partial", cost.Completeness)
	}
	if !strings.Contains(components[1].Reason, "without naming the model") {
		t.Errorf("reason = %q, want it to say the provider never named the model", components[1].Reason)
	}
	if strings.Contains(components[1].Reason, "no captured price") {
		t.Errorf("reason = %q, want it not to read as a catalog gap", components[1].Reason)
	}
	// Its usage is preserved regardless: it is the only evidence of the spend.
	if got := components[1].Usage.Count(usage.InputTokens); got != 1200 {
		t.Errorf("preserved input = %d, want 1200", got)
	}
}

// Nothing priceable at all is unknown, not zero. A turn whose every model is
// unpriceable spent real money.
func TestQuoteSetNothingPriceableIsUnknownNotZero(t *testing.T) {
	set := captureSet(t, agentCatalog(t))

	cost, _, err := set.PriceComponents([]pricing.Component{
		step("model-a", 1000, 100),
		step("model-b", 2000, 200),
	})
	if !errors.Is(err, pricing.ErrNoPrice) {
		t.Fatalf("err = %v, want ErrNoPrice", err)
	}
	if cost.Completeness != usage.CostUnknown {
		t.Errorf("completeness = %q, want unknown", cost.Completeness)
	}
	if cost.Known() {
		t.Errorf("cost = %s, want an explicit unknown", cost)
	}
	if cost.String() == usage.KnownCost(0).String() {
		t.Errorf("an unpriceable turn rendered as %q, indistinguishable from a free one", cost.String())
	}
	if len(cost.Unpriced) == 0 {
		t.Error("the unpriced dimensions must be named")
	}
}

// A dimension with no rate spoils completeness even when the model itself is
// priced: a known model billed for something the price sheet does not cover is
// still spend throttle cannot name.
func TestQuoteSetUnpricedDimensionSpoilsCompleteness(t *testing.T) {
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
		},
		Provenance: pricing.Provenance{Source: "test", Version: "input-only"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	set := captureSet(t, cat)

	cost, components, err := set.PriceComponents([]pricing.Component{step(sonnetID, 1000, 100)})
	if !errors.Is(err, pricing.ErrNoRate) {
		t.Fatalf("err = %v, want ErrNoRate", err)
	}
	if cost.Completeness != usage.CostPartial {
		t.Errorf("completeness = %q, want partial", cost.Completeness)
	}
	if want := dollars(t, "0.003"); cost.AtLeast() != want {
		t.Errorf("floor = %s, want the %s the input tokens did cost", cost, want)
	}
	if len(cost.Unpriced) != 1 || cost.Unpriced[0] != usage.OutputTokens {
		t.Errorf("Unpriced = %v, want the output dimension", cost.Unpriced)
	}
	// The step contributed a floor but is not "priced": completeness is a property of
	// the whole step, not of whether any of it costed.
	if components[0].Priced {
		t.Error("a partially priced step must not report itself as priced")
	}
	if len(components[0].Unpriced) != 1 {
		t.Errorf("component Unpriced = %v, want the output dimension named on the step too",
			components[0].Unpriced)
	}
}

// No observed invocations is unknown, not free. A managed agent turn that reported
// no model usage may have invoked several models with the trace suppressed.
func TestQuoteSetNoStepsIsUnknownNotFree(t *testing.T) {
	set := captureSet(t, agentCatalog(t))

	cost, components, err := set.PriceComponents(nil)
	if err != nil {
		t.Fatalf("PriceComponents: %v", err)
	}
	if cost.Known() {
		t.Errorf("cost = %s, want an unknown: an unobserved turn is not a free one", cost)
	}
	if !strings.Contains(cost.Reason, "no model invocations were observed") {
		t.Errorf("reason = %q, want it to state that nothing was observed", cost.Reason)
	}
	if len(components) != 0 {
		t.Errorf("components = %v, want none", components)
	}
}

// A zero count needs no rate: a dimension the provider reported as zero costs
// nothing whether the price sheet covers it or not.
func TestQuoteSetZeroCountDimensionNeedsNoRate(t *testing.T) {
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "15.00")),
		},
		Provenance: pricing.Provenance{Source: "test", Version: "v1"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	set := captureSet(t, cat)

	s := step(sonnetID, 1000, 100)
	s.Usage.Set("cache-read-tokens", 0) // A dimension with no rate, reported as zero.

	cost, _, err := set.PriceComponents([]pricing.Component{s})
	if err != nil {
		t.Fatalf("PriceComponents: %v", err)
	}
	if !cost.Known() {
		t.Errorf("cost = %s, want a known cost: a zero count cannot need a price", cost)
	}
	if want := dollars(t, "0.0045"); cost.Amount != want {
		t.Errorf("cost = %s, want %s", cost, want)
	}
}

// The set is captured under the access dimensions the request will run under, since
// prices vary by region. A rate captured for the wrong region would be worse than
// no rate at all.
func TestCaptureSetNarrowsToTheRequestsRegion(t *testing.T) {
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			Region:          "us-east-1",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "us"},
		},
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			Region:          "eu-west-1",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "3.30")),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "eu"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	eu, err := pricing.CaptureSet(cat, "aws-bedrock", "eu-west-1", at)
	if err != nil {
		t.Fatalf("CaptureSet: %v", err)
	}
	id := agentIdentity(sonnetID)
	id.Region = "eu-west-1"
	cost, _, err := eu.PriceComponents([]pricing.Component{{
		Identity: id,
		Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000}),
	}})
	if err != nil {
		t.Fatalf("PriceComponents: %v", err)
	}
	if want := dollars(t, "3.30"); cost.Amount != want {
		t.Errorf("cost = %s, want the European rate %s", cost, want)
	}
}

// Only models on the requested access path are captured. A set that mixed providers
// could price a Bedrock invocation with an OpenAI rate.
func TestCaptureSetExcludesOtherAccessProviders(t *testing.T) {
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "v1"},
		},
		pricing.Price{
			AccessProvider:  "anthropic-api",
			ProviderModelID: "claude-sonnet-4-20250514",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "v1"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	set, err := pricing.CaptureSet(cat, "aws-bedrock", "us-east-1", at)
	if err != nil {
		t.Fatalf("CaptureSet: %v", err)
	}
	if got := set.Models(); len(got) != 1 || got[0] != sonnetID {
		t.Errorf("captured %v, want only the aws-bedrock model", got)
	}
	if set.AccessProvider != "aws-bedrock" {
		t.Errorf("AccessProvider = %q, want the path it was captured for", set.AccessProvider)
	}
}

// Every member of the set carries the same capture instant. That is the evidence it
// is one read of one price sheet, rather than an accumulation of several -- which is
// the whole claim the set makes.
func TestCaptureSetIsOneReadAtOneInstant(t *testing.T) {
	set := captureSet(t, agentCatalog(t))

	if !set.CapturedAt.Equal(at) {
		t.Errorf("CapturedAt = %s, want %s", set.CapturedAt, at)
	}
	if len(set.Quotes) != 3 {
		t.Fatalf("captured %d quotes, want 3", len(set.Quotes))
	}
	for id, q := range set.Quotes {
		if !q.CapturedAt.Equal(at) {
			t.Errorf("%s captured at %s, want the set's single instant %s", id, q.CapturedAt, at)
		}
		if !q.Valid() {
			t.Errorf("%s captured an invalid quote", id)
		}
	}
	// Models is stable, so a record's retained set reads the same every time.
	first := set.Models()
	for range 3 {
		got := set.Models()
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("Models is unstable: %v then %v", first, got)
			}
		}
	}
}

// A catalog that cannot enumerate its models yields an empty set with an
// explanation, not an error. The request then has no priceable models, which is the
// existing unknown-cost condition and is a matter for posture, not a failure here.
func TestCaptureSetFromANonEnumerableCatalog(t *testing.T) {
	set, err := pricing.CaptureSet(opaqueCatalog{}, "aws-bedrock", "us-east-1", at)
	if err != nil {
		t.Fatalf("CaptureSet: %v", err)
	}
	if set.Valid() {
		t.Error("a catalog that cannot enumerate cannot yield a priceable set")
	}
	if !strings.Contains(set.Note, "cannot enumerate") {
		t.Errorf("Note = %q, want it to explain why the set is empty", set.Note)
	}
	// And an empty set prices nothing rather than pricing zero.
	cost, _, err := set.PriceComponents([]pricing.Component{step(sonnetID, 1000, 100)})
	if !errors.Is(err, pricing.ErrNoPrice) {
		t.Fatalf("err = %v, want ErrNoPrice", err)
	}
	if cost.Known() {
		t.Errorf("cost = %s, want an unknown", cost)
	}
}

// No catalog at all is an error: it is a misconfiguration, not a priceable state.
func TestCaptureSetWithoutACatalogIsAnError(t *testing.T) {
	set, err := pricing.CaptureSet(nil, "aws-bedrock", "us-east-1", at)
	if err == nil {
		t.Fatal("want an error when no catalog was supplied")
	}
	if set.Valid() {
		t.Error("the set must price nothing")
	}
	if set.Note == "" {
		t.Error("the set must explain itself even on the error path")
	}
}

// A catalog with no models for the requested access path yields an empty set with a
// note naming what was looked for.
func TestCaptureSetWithNoMatchingModels(t *testing.T) {
	set, err := pricing.CaptureSet(agentCatalog(t), "openai", "us-east-1", at)
	if err != nil {
		t.Fatalf("CaptureSet: %v", err)
	}
	if set.Valid() {
		t.Error("want an empty set")
	}
	if !strings.Contains(set.Note, "openai") {
		t.Errorf("Note = %q, want it to name the access provider looked for", set.Note)
	}
}

// Only the quotes a request actually used are retained. The set captured at
// admission may cover hundreds of models, and writing all of them to every record
// would bloat the store without adding to the record's accounting story.
func TestQuoteSetRetainsOnlyWhatTheRequestUsed(t *testing.T) {
	set := captureSet(t, agentCatalog(t))
	if len(set.Quotes) != 3 {
		t.Fatalf("captured %d quotes, want 3", len(set.Quotes))
	}

	steps := []pricing.Component{step(sonnetID, 1000, 100), step(sonnetID, 500, 50)}
	retained := set.Retain(steps)

	if got := retained.Models(); len(got) != 1 || got[0] != sonnetID {
		t.Errorf("retained %v, want only the model the request invoked", got)
	}
	// The retained quote is the same frozen one, so the record replays identically.
	if !retained.CapturedAt.Equal(set.CapturedAt) {
		t.Errorf("retained CapturedAt = %s, want the original %s", retained.CapturedAt, set.CapturedAt)
	}
	full, _, err := set.PriceComponents(steps)
	if err != nil {
		t.Fatalf("PriceComponents on the full set: %v", err)
	}
	sub, _, err := retained.PriceComponents(steps)
	if err != nil {
		t.Fatalf("PriceComponents on the retained set: %v", err)
	}
	if sub.Amount != full.Amount {
		t.Errorf("retained set priced %s, want the same %s the full set did", sub, full)
	}
}

// Retaining an unpriceable step's model does not invent a quote for it.
func TestQuoteSetRetainSkipsModelsItNeverHad(t *testing.T) {
	set := captureSet(t, agentCatalog(t))
	retained := set.Retain([]pricing.Component{step("model-nobody-prices", 1000, 100)})
	if retained.Valid() {
		t.Errorf("retained %v, want nothing: the set never had a quote for that model",
			retained.Models())
	}
	if !retained.CapturedAt.Equal(set.CapturedAt) {
		t.Error("the capture instant must survive even an empty retention")
	}
}

// A record must stay reproducibly priceable after a restart, which means the whole
// set round-trips: rates, provenance, and the capture instant.
func TestQuoteSetRoundTripsThroughJSON(t *testing.T) {
	set := captureSet(t, agentCatalog(t))
	steps := []pricing.Component{step(sonnetID, 1000, 100), step(haikuID, 200, 20)}
	before, _, err := set.PriceComponents(steps)
	if err != nil {
		t.Fatalf("PriceComponents: %v", err)
	}

	encoded, err := pricing.MarshalQuoteSet(set.Retain(steps))
	if err != nil {
		t.Fatalf("MarshalQuoteSet: %v", err)
	}
	restored, err := pricing.UnmarshalQuoteSet(encoded)
	if err != nil {
		t.Fatalf("UnmarshalQuoteSet: %v", err)
	}

	after, _, err := restored.PriceComponents(steps)
	if err != nil {
		t.Fatalf("PriceComponents after the round trip: %v", err)
	}
	if after.Amount != before.Amount {
		t.Errorf("restored set priced %s, want %s", after, before)
	}
	if !restored.CapturedAt.Equal(set.CapturedAt) {
		t.Errorf("CapturedAt = %s, want %s", restored.CapturedAt, set.CapturedAt)
	}
	if restored.AccessProvider != set.AccessProvider {
		t.Errorf("AccessProvider = %q, want %q", restored.AccessProvider, set.AccessProvider)
	}
	// Provenance survives, so a reader can tell which price sheet the charge came
	// from.
	q, err := restored.For(agentIdentity(sonnetID))
	if err != nil {
		t.Fatalf("the restored set must still price the model it retained: %v", err)
	}
	if q.Provenance.Version != "v1" || q.Provenance.Source != "test" {
		t.Errorf("provenance = %+v, want it preserved", q.Provenance)
	}
}

// An empty payload is a request that captured no rates, not a corrupt record.
func TestUnmarshalQuoteSetToleratesEmptyPayloads(t *testing.T) {
	for _, in := range []string{"", "{}", "null"} {
		set, err := pricing.UnmarshalQuoteSet(in)
		if err != nil {
			t.Errorf("UnmarshalQuoteSet(%q): %v", in, err)
		}
		if set.Valid() {
			t.Errorf("UnmarshalQuoteSet(%q) yielded a priceable set", in)
		}
	}
	if _, err := pricing.UnmarshalQuoteSet("{not json"); err == nil {
		t.Error("want an error for a malformed payload")
	}
}

// The service tier selects among a quote's alternates, exactly as it does for a
// single captured quote -- so a compound charge and a simple one price identically.
func TestQuoteSetHonoursServiceTier(t *testing.T) {
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "standard"},
		},
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			ServiceTier:     "batch",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "1.50")),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "batch"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	set := captureSet(t, cat)

	priceAt := func(tier string) money.Money {
		t.Helper()
		id := agentIdentity(sonnetID)
		id.ServiceTier = tier
		cost, _, err := set.PriceComponents([]pricing.Component{{
			Identity: id,
			Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000}),
		}})
		if err != nil {
			t.Fatalf("PriceComponents(%q): %v", tier, err)
		}
		return cost.Amount
	}
	if got, want := priceAt(""), dollars(t, "3.00"); got != want {
		t.Errorf("standard tier priced %s, want %s", got, want)
	}
	if got, want := priceAt("batch"), dollars(t, "1.50"); got != want {
		t.Errorf("batch tier priced %s, want %s", got, want)
	}
}

// A compound step served on a tier no rate was frozen for is unpriceable, and the
// charge as a whole is incomplete -- not silently priced at the rates that were
// captured for some other tier.
//
// Same rule as a single charge, reached through the set. A managed agent turn made of
// several internal invocations must not go quietly known because one step's tier was
// substituted for one nobody priced.
func TestQuoteSetRefusesAnUncapturedServiceTier(t *testing.T) {
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "standard"},
		},
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			ServiceTier:     "batch",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "1.50")),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "batch"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	set := captureSet(t, cat)

	served := agentIdentity(sonnetID)
	served.ServiceTier = "turbo-invented-later"
	steps := []pricing.Component{{
		Identity: served,
		Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000}),
	}}

	// Anti-vacuous: the captured tiers do price this usage, to two different confident
	// figures. Neither may be reported for a tier that was not captured.
	for tier, want := range map[string]string{"": "3.00", "batch": "1.50"} {
		known := agentIdentity(sonnetID)
		known.ServiceTier = tier
		cost, _, err := set.PriceComponents([]pricing.Component{{Identity: known, Usage: steps[0].Usage}})
		if err != nil {
			t.Fatalf("PriceComponents(%q): %v", tier, err)
		}
		if !cost.Known() || cost.Amount != dollars(t, want) {
			t.Fatalf("tier %q priced %s (%s); this test needs a known %s", tier, cost.Amount, cost.State(), want)
		}
	}

	if _, err := set.For(served); !errors.Is(err, pricing.ErrRatesNotCaptured) {
		t.Errorf("For = %v, want ErrTierNotCaptured", err)
	}

	cost, priced, err := set.PriceComponents(steps)
	if !errors.Is(err, pricing.ErrRatesNotCaptured) {
		t.Fatalf("err = %v, want ErrTierNotCaptured", err)
	}
	if cost.Known() {
		t.Errorf("cost = %s, want an incomplete cost", cost)
	}
	if cost.Amount != 0 {
		t.Errorf("cost amount = %s, want zero: the captured tiers bound this one in neither direction, "+
			"so there is no floor to report", cost.Amount)
	}
	if priced[0].Priced {
		t.Error("the step reported itself as priced")
	}
	if !strings.Contains(priced[0].Reason, served.ServiceTier) {
		t.Errorf("step reason = %q, want it to name the tier that served the call", priced[0].Reason)
	}
	if !strings.Contains(cost.Reason, served.ServiceTier) {
		t.Errorf("cost reason = %q, want it to name the tier that served the call", cost.Reason)
	}
}

// PriceComponents must not mutate the caller's slice. The adapter holds those
// components as its own observation record, and pricing is a read of them.
func TestPriceComponentsDoesNotMutateTheCallersSteps(t *testing.T) {
	set := captureSet(t, agentCatalog(t))
	steps := []pricing.Component{step(sonnetID, 1000, 100), step("unknown-model", 500, 50)}

	if _, _, err := set.PriceComponents(steps); err == nil {
		t.Fatal("want an error for the unpriceable step")
	}
	for i, s := range steps {
		if s.Priced || s.Amount != 0 || s.Reason != "" || len(s.Unpriced) != 0 {
			t.Errorf("step %d was mutated in place: %+v", i, s)
		}
	}
}

// A zero set prices nothing rather than panicking, since a record written before
// quote sets existed reads back as one.
func TestZeroQuoteSetPricesNothing(t *testing.T) {
	var set pricing.QuoteSet
	if set.Valid() {
		t.Error("a zero set must not report itself as priceable")
	}
	if len(set.Models()) != 0 {
		t.Errorf("Models = %v, want none", set.Models())
	}
	if _, err := set.For(agentIdentity(sonnetID)); !errors.Is(err, pricing.ErrNoPrice) {
		t.Errorf("For on a zero set = %v, want ErrNoPrice: it has no quote for anything", err)
	}
	cost, _, err := set.PriceComponents([]pricing.Component{step(sonnetID, 1000, 100)})
	if !errors.Is(err, pricing.ErrNoPrice) {
		t.Fatalf("err = %v, want ErrNoPrice", err)
	}
	if cost.Known() {
		t.Errorf("cost = %s, want an unknown", cost)
	}
	if retained := set.Retain([]pricing.Component{step(sonnetID, 1000, 100)}); retained.Valid() {
		t.Error("retaining from a zero set must yield nothing")
	}
}

// opaqueCatalog prices models but cannot enumerate them: a user-supplied catalog
// that implements only RateSource, which is all a direct model invocation needs.
type opaqueCatalog struct{}

func (opaqueCatalog) Capture(id usage.ModelIdentity, at time.Time) (pricing.CapturedQuote, error) {
	return pricing.CapturedQuote{}, pricing.ErrNoPrice
}
