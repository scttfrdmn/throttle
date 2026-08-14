package pricing_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// geoIdentity is a model priced by inference geography rather than by tier: the
// shape a provider that re-rates every token dimension by where the work happened
// produces.
func geoIdentity() usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "anthropic",
		ProviderModelID: "claude-geo-test",
		Operation:       "messages",
	}
}

// geoCatalog prices one model in two geographies and in no particular tier, with the
// rates far enough apart that pricing one at the other's rates cannot hide in
// rounding.
func geoCatalog(t *testing.T) *pricing.Static {
	t.Helper()
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  geoIdentity().AccessProvider,
			ProviderModelID: geoIdentity().ProviderModelID,
			InferenceGeo:    "global",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "5.00")),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "25.00")),
			},
			Provenance: pricing.Provenance{Source: "test-global", Currency: "USD"},
		},
		pricing.Price{
			AccessProvider:  geoIdentity().AccessProvider,
			ProviderModelID: geoIdentity().ProviderModelID,
			InferenceGeo:    "us",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "5.50")),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "27.50")),
			},
			Provenance: pricing.Provenance{Source: "test-us", Currency: "USD"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	return cat
}

// A geography the provider served the request in is priced from rates frozen at
// admission, exactly as a substituted service tier is.
//
// This is #30's rule on its second axis. The request asks for one geography, a
// workspace default or a capacity decision serves it in another, and the response is
// the authority on which. Both admissible bands are captured in one read so
// settlement stays a replay rather than a re-query.
func TestServedInferenceGeoSettlesFromTheCapturedAlternate(t *testing.T) {
	cat := geoCatalog(t)

	asked := geoIdentity()
	asked.InferenceGeo = "global"
	quote, err := cat.Capture(asked, at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if quote.InferenceGeo != "global" {
		t.Errorf("InferenceGeo = %q, want the geography the captured rates are qualified for", quote.InferenceGeo)
	}

	u := usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  1_000_000,
		usage.OutputTokens: 1_000_000,
	})

	// The geography that was asked for prices at its own rates.
	global, err := quote.For(asked)
	if err != nil {
		t.Fatalf("For(global): %v", err)
	}
	pricedGlobal, err := global.Price(u)
	if err != nil {
		t.Fatalf("Price(global): %v", err)
	}
	if want := dollars(t, "30.00"); !pricedGlobal.Cost.Known() || pricedGlobal.Cost.Amount != want {
		t.Fatalf("global priced %s (%s), want a known %s",
			pricedGlobal.Cost.Amount, pricedGlobal.Cost.State(), want)
	}

	// Served in the other geography: priced from the alternate frozen at admission,
	// which is a different figure and must not be the one above.
	served := asked
	served.InferenceGeo = "us"
	applicable, err := quote.For(served)
	if err != nil {
		t.Fatalf("For(us): %v, want the captured us alternate", err)
	}
	if applicable.InferenceGeo != "us" {
		t.Errorf("alternate InferenceGeo = %q, want us", applicable.InferenceGeo)
	}
	pricedUS, err := applicable.Price(u)
	if err != nil {
		t.Fatalf("Price(us): %v", err)
	}
	if want := dollars(t, "33.00"); !pricedUS.Cost.Known() || pricedUS.Cost.Amount != want {
		t.Errorf("us priced %s (%s), want a known %s", pricedUS.Cost.Amount, pricedUS.Cost.State(), want)
	}
	if pricedUS.Cost.Amount == pricedGlobal.Cost.Amount {
		t.Errorf("both geographies priced %s: the geography is not selecting a price sheet", pricedUS.Cost.Amount)
	}
	if pricedUS.Provenance.Source != "test-us" {
		t.Errorf("provenance = %q, want the us row's own provenance", pricedUS.Provenance.Source)
	}

	// And it survives persistence, or a restart loses the ability to price the
	// geography the request actually ran in on its original basis.
	back, err := pricing.UnmarshalQuote(mustMarshalQuote(t, quote))
	if err != nil {
		t.Fatalf("UnmarshalQuote: %v", err)
	}
	restored, err := back.For(served)
	if err != nil {
		t.Fatalf("For(us) after restart: %v", err)
	}
	after, err := restored.Price(u)
	if err != nil {
		t.Fatalf("Price after restart: %v", err)
	}
	if after.Cost.Amount != pricedUS.Cost.Amount {
		t.Errorf("us cost after restart = %s, want %s", after.Cost.Amount, pricedUS.Cost.Amount)
	}
}

// A geography no rate was frozen for is unresolved, and specifically is not priced at
// the other geography's rates.
//
// Anti-vacuous in the same two halves as the tier test: first that the admitted rates
// would have produced a confident figure, then that the lookup refuses to produce it.
// A geography re-rates every dimension, so the captured bands bound the real cost in
// neither direction and falling back is unsafe in both.
func TestUncapturedInferenceGeoIsUnresolvedRatherThanAdmittedRates(t *testing.T) {
	cat := geoCatalog(t)

	asked := geoIdentity()
	asked.InferenceGeo = "global"
	quote, err := cat.Capture(asked, at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 10_000_000})

	admitted, err := quote.Price(u)
	if err != nil {
		t.Fatalf("pricing at the admitted rates: %v", err)
	}
	if want := dollars(t, "50.00"); !admitted.Cost.Known() || admitted.Cost.Amount != want {
		t.Fatalf("the admitted rates priced %s (%s); this test needs them to price a known %s",
			admitted.Cost.Amount, admitted.Cost.State(), want)
	}

	served := asked
	served.InferenceGeo = "eu-invented-later"

	applicable, err := quote.For(served)
	if err == nil {
		t.Fatalf("For(%q) returned a quote for %s, want a refusal: pricing a geography by rates "+
			"it was not captured for is the bug", served.InferenceGeo, applicable.ProviderModelID)
	}
	if !errors.Is(err, pricing.ErrRatesNotCaptured) {
		t.Errorf("err = %v, want ErrRatesNotCaptured", err)
	}

	var rateErr *pricing.RateNotCapturedError
	if !errors.As(err, &rateErr) {
		t.Fatalf("err = %v, want a *RateNotCapturedError", err)
	}
	if rateErr.InferenceGeo != served.InferenceGeo {
		t.Errorf("InferenceGeo = %q, want the geography that served the call", rateErr.InferenceGeo)
	}
	if rateErr.ServiceTier != "" {
		t.Errorf("ServiceTier = %q, want empty: no tier was the reason for this miss", rateErr.ServiceTier)
	}
	if !slices.Contains(rateErr.Captured, "|geo=global") || !slices.Contains(rateErr.Captured, "|geo=us") {
		t.Errorf("Captured = %v, want it to name the geographies that were frozen", rateErr.Captured)
	}
	for _, want := range []string{served.InferenceGeo, "global", "us", "inference geography"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("reason %q does not mention %q", err.Error(), want)
		}
	}

	if applicable.Valid() {
		t.Fatal("the refused lookup returned a usable quote")
	}
	ignored, err := applicable.Price(u)
	if err == nil {
		t.Error("pricing the refused quote succeeded")
	}
	if ignored.Cost.Known() {
		t.Errorf("ignoring the error produced a known cost of %s", ignored.Cost.Amount)
	}
	if ignored.Cost.Amount == admitted.Cost.Amount {
		t.Errorf("ignoring the error produced the admitted amount %s", ignored.Cost.Amount)
	}
}

// A model priced by geography and not by tier still settles when the response reports
// a tier, and a model priced by tier and not by geography still settles when the
// response reports a geography.
//
// This is why narrowing is per axis rather than wholesale. Requiring an exact match on
// both axes would make an ordinary request unpriceable because the provider named a
// dimension the catalog never needed to distinguish; requiring neither would let a real
// difference be priced by the wrong sheet. The captured rows are the evidence for which
// axes select between price sheets, and they are evidence one axis at a time.
func TestNarrowingIgnoresAxesTheCatalogDoesNotPrice(t *testing.T) {
	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})

	t.Run("priced by geography, not by tier", func(t *testing.T) {
		cat := geoCatalog(t)
		asked := geoIdentity()
		asked.InferenceGeo = "us"
		quote, err := cat.Capture(asked, at)
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		for _, tier := range []string{"", "standard", "priority", "a-tier-from-2030"} {
			served := asked
			served.ServiceTier = tier
			applicable, err := quote.For(served)
			if err != nil {
				t.Fatalf("For(tier %q) = %v, want the geography to decide the price alone", tier, err)
			}
			priced, err := applicable.Price(u)
			if err != nil {
				t.Fatalf("Price(tier %q): %v", tier, err)
			}
			if want := dollars(t, "5.50"); !priced.Cost.Known() || priced.Cost.Amount != want {
				t.Errorf("tier %q priced %s (%s), want a known %s",
					tier, priced.Cost.Amount, priced.Cost.State(), want)
			}
		}

		// The geography still has to match, or the per-axis narrowing has gone too far
		// and made the axis that does select prices stop selecting them.
		wrong := asked
		wrong.ServiceTier = "priority"
		wrong.InferenceGeo = "eu-invented-later"
		if _, err := quote.For(wrong); !errors.Is(err, pricing.ErrRatesNotCaptured) {
			t.Errorf("For(unknown geography) = %v, want ErrRatesNotCaptured", err)
		}
	})

	t.Run("priced by tier, not by geography", func(t *testing.T) {
		cat, err := pricing.NewStatic(
			pricing.Price{
				AccessProvider:  "openai",
				ProviderModelID: "gpt-axis-test",
				ServiceTier:     "default",
				Rates: map[usage.Dimension]pricing.Rate{
					usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "1.00")),
				},
				Provenance: pricing.Provenance{Source: "test-default"},
			},
			pricing.Price{
				AccessProvider:  "openai",
				ProviderModelID: "gpt-axis-test",
				ServiceTier:     "flex",
				Rates: map[usage.Dimension]pricing.Rate{
					usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "0.50")),
				},
				Provenance: pricing.Provenance{Source: "test-flex"},
			},
		)
		if err != nil {
			t.Fatalf("NewStatic: %v", err)
		}
		asked := usage.ModelIdentity{
			AccessProvider:  "openai",
			ProviderModelID: "gpt-axis-test",
			ServiceTier:     "default",
		}
		quote, err := cat.Capture(asked, at)
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}
		if quote.InferenceGeo != "" {
			t.Errorf("InferenceGeo = %q, want empty: no row prices this model by geography", quote.InferenceGeo)
		}
		for _, geo := range []string{"", "global", "us", "a-geography-from-2030"} {
			served := asked
			served.InferenceGeo = geo
			applicable, err := quote.For(served)
			if err != nil {
				t.Fatalf("For(geo %q) = %v, want the tier to decide the price alone", geo, err)
			}
			priced, err := applicable.Price(u)
			if err != nil {
				t.Fatalf("Price(geo %q): %v", geo, err)
			}
			if want := dollars(t, "1.00"); !priced.Cost.Known() || priced.Cost.Amount != want {
				t.Errorf("geo %q priced %s (%s), want a known %s",
					geo, priced.Cost.Amount, priced.Cost.State(), want)
			}
		}

		// The tier still selects, whatever geography the response reports alongside it.
		served := asked
		served.ServiceTier = "flex"
		served.InferenceGeo = "a-geography-from-2030"
		applicable, err := quote.For(served)
		if err != nil {
			t.Fatalf("For(flex): %v", err)
		}
		priced, err := applicable.Price(u)
		if err != nil {
			t.Fatalf("Price(flex): %v", err)
		}
		if want := dollars(t, "0.50"); priced.Cost.Amount != want {
			t.Errorf("flex priced %s, want %s", priced.Cost.Amount, want)
		}
	})
}

// Both axes at once: a model priced per tier per geography selects on the combination,
// and a combination that was never priced is unresolved even though each of its halves
// appears somewhere in the catalog.
//
// The half-known case is the one worth pinning. "priority" is priced and "us" is
// priced, so a selector that matched either axis alone would happily return a sheet --
// and it would be the wrong sheet, at a rate off by an amount no rounding could
// explain.
func TestSelectorSelectsOnTheCombinationOfBothAxes(t *testing.T) {
	price := func(tier, geo, rate, source string) pricing.Price {
		return pricing.Price{
			AccessProvider:  "anthropic",
			ProviderModelID: "claude-matrix-test",
			ServiceTier:     tier,
			InferenceGeo:    geo,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, rate)),
			},
			Provenance: pricing.Provenance{Source: source},
		}
	}
	cat, err := pricing.NewStatic(
		price("standard", "global", "5.00", "std-global"),
		price("standard", "us", "5.50", "std-us"),
		price("priority", "global", "10.00", "pri-global"),
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	asked := usage.ModelIdentity{
		AccessProvider:  "anthropic",
		ProviderModelID: "claude-matrix-test",
		ServiceTier:     "standard",
		InferenceGeo:    "global",
	}
	quote, err := cat.Capture(asked, at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})

	for _, c := range []struct{ tier, geo, want string }{
		{"standard", "global", "5.00"},
		{"standard", "us", "5.50"},
		{"priority", "global", "10.00"},
	} {
		served := asked
		served.ServiceTier, served.InferenceGeo = c.tier, c.geo
		applicable, err := quote.For(served)
		if err != nil {
			t.Fatalf("For(%s/%s): %v", c.tier, c.geo, err)
		}
		priced, err := applicable.Price(u)
		if err != nil {
			t.Fatalf("Price(%s/%s): %v", c.tier, c.geo, err)
		}
		if want := dollars(t, c.want); !priced.Cost.Known() || priced.Cost.Amount != want {
			t.Errorf("%s/%s priced %s (%s), want a known %s",
				c.tier, c.geo, priced.Cost.Amount, priced.Cost.State(), want)
		}
	}

	// priority+us was never priced, though both "priority" and "us" are priced
	// separately. Substituting either half's sheet would be a rate error of at least
	// 10%, and in one direction nearly 100%.
	served := asked
	served.ServiceTier, served.InferenceGeo = "priority", "us"
	applicable, err := quote.For(served)
	if !errors.Is(err, pricing.ErrRatesNotCaptured) {
		t.Fatalf("For(priority/us) = %v, want ErrRatesNotCaptured: that combination was never priced", err)
	}
	if applicable.Valid() {
		t.Error("the refused lookup returned a usable quote")
	}

	var rateErr *pricing.RateNotCapturedError
	if !errors.As(err, &rateErr) {
		t.Fatalf("err = %v, want a *RateNotCapturedError", err)
	}
	if rateErr.ServiceTier != "priority" || rateErr.InferenceGeo != "us" {
		t.Errorf("error names tier %q geo %q, want both axes of the combination that served the call",
			rateErr.ServiceTier, rateErr.InferenceGeo)
	}
	if !strings.Contains(err.Error(), "service tier") || !strings.Contains(err.Error(), "inference geography") {
		t.Errorf("reason %q must name both axes so an operator knows which row to add", err.Error())
	}
}

// A quote persisted before the geography axis existed still resolves its alternates.
//
// The alternates map is keyed by a selector encoding whose no-geography form is the
// bare service tier it was keyed by before. This test is written against literal JSON
// rather than against a freshly captured quote on purpose: a stored record from an
// earlier build is exactly what cannot be re-captured, and the compatibility claim is
// about that byte sequence and not about the current encoder agreeing with itself.
func TestStoredTierKeyedAlternatesStillResolve(t *testing.T) {
	const stored = `{
		"access_provider": "openai",
		"provider_model_id": "gpt-legacy-quote",
		"service_tier": "default",
		"rates": {"input_tokens": {"Dimension": "input_tokens", "PerUnit": 1000000, "Unit": 1000000}},
		"provenance": {"Source": "stored-default"},
		"captured_at": "2026-03-01T12:00:00Z",
		"alternates": {
			"flex": {
				"access_provider": "openai",
				"provider_model_id": "gpt-legacy-quote",
				"service_tier": "flex",
				"rates": {"input_tokens": {"Dimension": "input_tokens", "PerUnit": 500000, "Unit": 1000000}},
				"provenance": {"Source": "stored-flex"},
				"captured_at": "2026-03-01T12:00:00Z"
			}
		}
	}`

	quote, err := pricing.UnmarshalQuote(stored)
	if err != nil {
		t.Fatalf("UnmarshalQuote: %v", err)
	}
	if !quote.Valid() {
		t.Fatal("the stored quote did not read back as usable")
	}

	served := usage.ModelIdentity{
		AccessProvider:  "openai",
		ProviderModelID: "gpt-legacy-quote",
		ServiceTier:     "flex",
	}
	applicable, err := quote.For(served)
	if err != nil {
		t.Fatalf("For(flex) on a stored quote = %v: the alternates key encoding is no longer "+
			"compatible with what was persisted before the geography axis existed", err)
	}
	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})
	priced, err := applicable.Price(u)
	if err != nil {
		t.Fatalf("Price: %v", err)
	}
	if want := money.Money(500_000); !priced.Cost.Known() || priced.Cost.Amount != want {
		t.Errorf("stored flex alternate priced %s (%s), want a known %s",
			priced.Cost.Amount, priced.Cost.State(), want)
	}
	if priced.Provenance.Source != "stored-flex" {
		t.Errorf("provenance = %q, want the stored alternate's own", priced.Provenance.Source)
	}

	// The reason an operator sees for a miss is still in the same vocabulary.
	served.ServiceTier = "priority"
	if _, err := quote.For(served); !errors.Is(err, pricing.ErrRatesNotCaptured) {
		t.Errorf("For(priority) = %v, want ErrRatesNotCaptured", err)
	}
}

// A key that a freshly captured tier-only quote writes must be the same bytes a stored
// one used, or the compatibility above holds only for the fixture in the test above.
func TestTierOnlyAlternateKeysCarryNoGeographySuffix(t *testing.T) {
	cat, err := pricing.NewStatic(
		pricing.Price{
			AccessProvider:  "openai",
			ProviderModelID: "gpt-key-test",
			ServiceTier:     "default",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "1.00")),
			},
			Provenance: pricing.Provenance{Source: "test"},
		},
		pricing.Price{
			AccessProvider:  "openai",
			ProviderModelID: "gpt-key-test",
			ServiceTier:     "flex",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(t, "0.50")),
			},
			Provenance: pricing.Provenance{Source: "test-flex"},
		},
	)
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	quote, err := cat.Capture(usage.ModelIdentity{
		AccessProvider:  "openai",
		ProviderModelID: "gpt-key-test",
		ServiceTier:     "default",
	}, at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, ok := quote.Alternates["flex"]; !ok {
		keys := make([]string, 0, len(quote.Alternates))
		for k := range quote.Alternates {
			keys = append(keys, k)
		}
		t.Errorf("alternates keyed %v, want a bare \"flex\": a tier-only key must stay byte-identical "+
			"to what was persisted before geography existed", keys)
	}

	// A geography-qualified capture does add the suffix, so the two cases are actually
	// distinguishable and the test above is not passing for a trivial reason.
	geo, err := geoCatalog(t).Capture(func() usage.ModelIdentity {
		id := geoIdentity()
		id.InferenceGeo = "global"
		return id
	}(), at)
	if err != nil {
		t.Fatalf("Capture(geo): %v", err)
	}
	if _, ok := geo.Alternates["|geo=us"]; !ok {
		keys := make([]string, 0, len(geo.Alternates))
		for k := range geo.Alternates {
			keys = append(keys, k)
		}
		t.Errorf("geography alternates keyed %v, want \"|geo=us\"", keys)
	}
}

// A price refresh after admission cannot make a request stranded by its geography
// priceable, for the same reason it cannot for a tier.
func TestCatalogChangesAfterAdmissionCannotPriceAnUncapturedGeo(t *testing.T) {
	cat := geoCatalog(t)
	asked := geoIdentity()
	asked.InferenceGeo = "global"
	quote, err := cat.Capture(asked, at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	served := asked
	served.InferenceGeo = "eu"
	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})

	for i := 0; i < 50; i++ {
		if err := cat.Add(pricing.Price{
			AccessProvider:  asked.AccessProvider,
			ProviderModelID: asked.ProviderModelID,
			InferenceGeo:    "eu",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, money.Money(6_000_000+i)),
			},
			Provenance: pricing.Provenance{Source: "test-later"},
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
		applicable, err := quote.For(served)
		if !errors.Is(err, pricing.ErrRatesNotCaptured) {
			t.Fatalf("round %d: For(eu) = %v, want ErrRatesNotCaptured however much the catalog has learned", i, err)
		}
		if priced, _ := applicable.Price(u); priced.Cost.Known() {
			t.Fatalf("round %d: the request became priceable at %s because the catalog changed", i, priced.Cost.Amount)
		}
	}

	// A request admitted now does see the new geography, which is the difference
	// between a live catalog and a frozen quote.
	fresh, err := cat.Capture(served, at)
	if err != nil {
		t.Fatalf("Capture after the catalog learned the geography: %v", err)
	}
	if _, err := fresh.For(served); err != nil {
		t.Errorf("For(eu) on a freshly captured quote = %v, want the new rates to apply", err)
	}
}

// A quote set resolves the geography axis by the same rules a single quote does, and
// reports a miss with the same structured error, so a compound charge and a simple one
// stay consistent about which combinations they can price.
func TestQuoteSetAppliesTheGeographyAxis(t *testing.T) {
	asked := geoIdentity()
	asked.InferenceGeo = "global"
	quote, err := geoCatalog(t).Capture(asked, at)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// Assembled from the capture rather than via CaptureSet, because CaptureSet
	// narrows by region alone: a model every row of which names an access dimension it
	// does not pass through is absent from such a set, which is true of the tier axis
	// already and is not what this test is about.
	set := pricing.QuoteSet{
		AccessProvider: asked.AccessProvider,
		Quotes:         map[string]pricing.CapturedQuote{asked.ProviderModelID: quote},
		CapturedAt:     at,
	}

	u := usage.New(map[usage.Dimension]int64{usage.InputTokens: 1_000_000})
	served := geoIdentity()
	served.InferenceGeo = "us"

	cost, components, err := set.PriceComponents([]pricing.Component{{Identity: served, Usage: u}})
	if err != nil {
		t.Fatalf("PriceComponents(us): %v", err)
	}
	if want := dollars(t, "5.50"); !cost.Known() || cost.Amount != want {
		t.Errorf("us priced %s (%s), want a known %s", cost.Amount, cost.State(), want)
	}
	if len(components) != 1 || !components[0].Priced {
		t.Errorf("components = %+v, want one priced step", components)
	}

	stranded := geoIdentity()
	stranded.InferenceGeo = "eu-invented-later"
	cost, components, err = set.PriceComponents([]pricing.Component{{Identity: stranded, Usage: u}})
	if !errors.Is(err, pricing.ErrRatesNotCaptured) {
		t.Fatalf("PriceComponents(unknown geography) err = %v, want ErrRatesNotCaptured", err)
	}
	if cost.Known() {
		t.Errorf("cost = %s (%s), want unresolved", cost.Amount, cost.State())
	}
	if cost.Amount != 0 {
		t.Errorf("cost amount = %s, want no invented floor for a request whose whole rate sheet is unknown", cost.Amount)
	}
	if len(components) != 1 {
		t.Fatalf("components = %+v, want one step", components)
	}
	// The step's own reason, not a generic "no captured price": the model *is* priced,
	// just not in the geography that served the call, and those need different fixes.
	if !strings.Contains(components[0].Reason, "inference geography") {
		t.Errorf("step reason = %q, want it to name the geography axis", components[0].Reason)
	}
}
