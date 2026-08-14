package bedrock_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/provider/bedrock"
	"github.com/scttfrdmn/throttle/usage"
)

// Inference geography is a second axis of the same access-dimension rule service tier
// established, and adding it must not change what Bedrock does.
//
// Bedrock has no inference-geography parameter: it has regions, which are endpoints
// rather than priced constraints on where work may happen, and the adapter never
// populates the field. So the whole of this axis has to be invisible here — which is
// the interesting claim, because the shared narrowing code now consults it on every
// lookup. A regression that made an unset geography select nothing, or select the wrong
// sheet, would show up as an ordinary Converse settling wrong.
//
// The catalog below prices this model by tier and by nothing else, which is the case
// per-axis narrowing exists for: the geography axis must be dropped because no captured
// row names one, while the tier axis must keep selecting.
func TestGeographyAxisDoesNotDisturbTierSelection(t *testing.T) {
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "15.00")),
			},
			Provenance: pricing.Provenance{Source: "test"},
		},
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			ServiceTier:     "flex",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "1.50")),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "7.50")),
			},
			Provenance: pricing.Provenance{Source: "test-flex"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	h := newHarness(t, "1000", func(c *bedrock.Config) { c.Catalog = cat })
	h.api.out = response(1_000_000, 0)

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-geo-silent", Input: request(sonnetID, aws.Int32(10)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if res.Identity.InferenceGeo != "" {
		t.Errorf("InferenceGeo = %q, want empty: Bedrock reports no inference geography, and a "+
			"region is an endpoint rather than a priced constraint on where inference runs",
			res.Identity.InferenceGeo)
	}
	if !res.Cost.Known() {
		t.Fatalf("cost = %s (%s), want known: no captured row prices this model by geography, so "+
			"the axis must not narrow anything: %s", res.Cost.Amount, res.Cost.State(), res.Cost.Reason)
	}
	if want := dollars(t, "3.00"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
}

// The whole fixture catalog still settles known costs, with the geography axis in the
// shared lookup and no Bedrock row naming a geography.
//
// A blunt regression check on the shipped fixtures rather than on a hand-built catalog:
// if narrowing were wholesale rather than per axis, every Bedrock request in the suite
// would go unresolved the moment any provider's rows started naming a geography.
func TestFixturePricedBedrockRequestsStaySettled(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = response(1000, 500)

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-geo-fixtures", Input: request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if !res.Cost.Known() {
		t.Errorf("cost = %s (%s), want known from the shipped fixtures: %s",
			res.Cost.Amount, res.Cost.State(), res.Cost.Reason)
	}
	if res.Charge.ActualCost == 0 {
		t.Error("ActualCost = 0: a priced request must charge something")
	}
	if !res.Quote.Valid() {
		t.Error("the captured quote should be returned")
	}
	if res.Quote.InferenceGeo != "" {
		t.Errorf("captured InferenceGeo = %q, want empty: no Bedrock fixture row prices a "+
			"geography, and the capture must record the row's value rather than the request's",
			res.Quote.InferenceGeo)
	}
}

// A geography that only the catalog knows about does not silently reprice a Bedrock
// request.
//
// The failure this guards against is the direction the per-axis rule makes possible: a
// catalog row naming a geography means the axis now selects between price sheets for
// that model, so a request whose identity carries no geography must not fall through
// onto a geography-qualified sheet. Bedrock never sets the field, so a catalog that
// prices only one geography leaves the request unpriceable rather than priced at the
// geography-specific rate.
func TestGeoQualifiedRowDoesNotPriceAGeographylessBedrockRequest(t *testing.T) {
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		InferenceGeo:    "us",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "9.99")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "99.90")),
		},
		Provenance: pricing.Provenance{Source: "test-us-only"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	h := newHarness(t, "1000", func(c *bedrock.Config) { c.Catalog = cat })
	h.api.out = response(1_000_000, 0)

	// It is refused at admission: the only row this catalog holds is qualified for a
	// geography the request says nothing about, so under enforce there is no bound on
	// what the call could cost and it does not happen.
	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-geo-mismatch", Input: request(sonnetID, aws.Int32(10)),
	})
	if err == nil {
		t.Fatalf("Converse succeeded with cost %s: a row qualified for geography %q must not "+
			"price a request whose geography is unknown", res.Charge.ActualCost, "us")
	}
	if res.Cost.Known() {
		t.Errorf("cost = %s, known: nothing here bounds what the request cost", res.Cost.Amount)
	}
	if res.Charge.ActualCost == dollars(t, "9.99") {
		t.Error("the us-qualified rate was applied to a request with no geography")
	}
	if got := h.api.callCount(); got != 0 {
		t.Errorf("provider calls = %d, want 0: enforce must refuse before spending money it "+
			"cannot bound", got)
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}
}

// A Bedrock request served on a tier no rate was frozen for still reports the tier
// alone, now that the error carries both axes.
//
// The generalized error has an InferenceGeo field, and the wrong version of this change
// would fill it in from the request or leave a stale value in the reason, telling an
// operator to add a geography row for a provider that has no geographies.
func TestUnpricedTierReasonNamesNoGeographyOnBedrock(t *testing.T) {
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: sonnetID,
			ServiceTier:     "default",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			},
			Provenance: pricing.Provenance{Source: "test"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	quote, err := cat.Capture(usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		ServiceTier:     "default",
	}, now)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	served := usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		ServiceTier:     "dedicated-2027",
	}
	_, err = quote.For(served)
	if !errors.Is(err, pricing.ErrRatesNotCaptured) {
		t.Fatalf("For(unpriced tier) = %v, want ErrRatesNotCaptured", err)
	}
	var rateErr *pricing.RateNotCapturedError
	if !errors.As(err, &rateErr) {
		t.Fatalf("err = %v, want a *RateNotCapturedError", err)
	}
	if rateErr.InferenceGeo != "" {
		t.Errorf("InferenceGeo = %q, want empty: no geography was involved in this miss",
			rateErr.InferenceGeo)
	}
	if strings.Contains(err.Error(), "inference geography") {
		t.Errorf("reason %q names a geography for a provider that has none, which would send an "+
			"operator looking for a row that cannot exist", err.Error())
	}
	if !strings.Contains(err.Error(), "service tier") {
		t.Errorf("reason %q must still name the tier axis", err.Error())
	}
}
