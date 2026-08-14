package anthropic_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	anth "github.com/anthropics/anthropic-sdk-go"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	anthropic "github.com/scttfrdmn/throttle/provider/anthropic"
	"github.com/scttfrdmn/throttle/usage"
)

// Inference geography is the price modifier Anthropic actually has, and it is the one
// axis on this provider where a request's rates are selected by something the response
// reports rather than by the model alone.
//
// Which makes it the harder half of the geography rule. On OpenAI the axis exists in the
// shared code and no request populates it, so those tests prove an absence. Here the
// multiplier is real -- the pricing page states 1.1x on all token categories for US-only
// inference -- and every assertion below is about selecting the frozen sheet the provider
// says served the call, never about multiplying anything at settlement.

// A request served in the US prices from the US sheet, and the difference is visible.
//
// Anti-vacuous by construction: the two sheets differ by exactly the published 1.1x, so
// the test names the global figure as the failure. A settlement that quietly used the
// unqualified rates -- the easy bug, because the request named no geography and admission
// captured the unqualified sheet -- lands on that number.
func TestUSInferenceGeoPricesFromTheUSSheet(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000000, "output_tokens": 200000, "inference_geo": "us"
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-geo-us", Params: request(opus5, 200000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if res.Identity.InferenceGeo != "us" {
		t.Errorf("InferenceGeo = %q, want %q from the response's own usage object",
			res.Identity.InferenceGeo, "us")
	}
	// opus-5 us: $5.50/M input, $27.50/M output. 1M input plus 200k output.
	want := dollars(t, "11")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (US rates)", res.Charge.ActualCost, want)
	}
	if global := dollars(t, "10"); res.Charge.ActualCost == global {
		t.Errorf("ActualCost = %s: the request was served in the US and priced at global rates. "+
			"The captured US sheet was not selected", global)
	}
	if !res.Cost.Known() {
		t.Errorf("a geography the fixtures price must settle known: %s", res.Cost.Reason)
	}
}

// The same request served globally prices from the global sheet, which is what makes the
// previous test a comparison rather than an assertion about one number.
func TestGlobalInferenceGeoPricesFromTheGlobalSheet(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000000, "output_tokens": 200000, "inference_geo": "global"
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-geo-global", Params: request(opus5, 200000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if want := dollars(t, "10"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (global rates)", res.Charge.ActualCost, want)
	}
	if us := dollars(t, "11"); res.Charge.ActualCost == us {
		t.Errorf("ActualCost = %s: a globally served request was charged the US multiplier", us)
	}
}

// A request that asked for US inference is admitted against the US sheet, so the hold
// covers what the request will actually cost.
//
// The geography is request-known here, which is the whole reason the estimate can reflect
// it: reserving at global rates for a request that will bill 1.1x would under-hold every
// US request by ten percent, and a budget that admits ten percent more than it can afford
// is not governing an envelope.
func TestRequestedGeographyIsReflectedInTheEstimate(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 200000)
	in.InferenceGeo = anth.String("us")

	est, err := h.client.Estimate(context.Background(), in)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Identity.InferenceGeo != "us" {
		t.Errorf("estimate InferenceGeo = %q, want %q from the request parameter",
			est.Identity.InferenceGeo, "us")
	}
	if !est.Cost.Known() {
		t.Fatalf("a priced geography must yield a known estimate: %s", est.Cost.Reason)
	}

	global := request(opus5, 200000)
	globalEst, err := h.client.Estimate(context.Background(), global)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Cost.Amount <= globalEst.Cost.Amount {
		t.Errorf("the US estimate is %s and the unqualified one is %s: a request that will bill "+
			"1.1x must not reserve the base amount", est.Cost.Amount, globalEst.Cost.Amount)
	}
}

// A request that named no geography and came back reporting one still prices from frozen
// rates. The alternate was captured at admission for exactly this, so nothing is looked up
// after the call.
//
// This is the common case rather than an edge: inference_geo is optional and a workspace
// can default it, so a request that names nothing is routinely served somewhere priced.
func TestUnrequestedGeographySelectsAFrozenAlternate(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, sonnet5, `{
		"input_tokens": 1000000, "output_tokens": 200000, "inference_geo": "us"
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-geo-alt", Params: request(sonnet5, 200000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	// The request named nothing, so admission captured the unqualified sheet.
	if res.Quote.InferenceGeo != "" {
		t.Errorf("captured InferenceGeo = %q, want empty: the request named no geography",
			res.Quote.InferenceGeo)
	}
	// And the US sheet came along as an alternate, which is what settlement selected.
	if _, ok := res.Quote.Alternates[""]; !ok && len(res.Quote.Alternates) == 0 {
		t.Fatal("no alternates were captured, so settlement had no frozen US sheet to select " +
			"and could only have re-queried the catalog or mispriced the request")
	}
	// sonnet-5 us: $2.20/M input, $11.00/M output.
	if want := dollars(t, "4.4"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s (US rates from the captured alternate)",
			res.Charge.ActualCost, want)
	}
	if base := dollars(t, "4"); res.Charge.ActualCost == base {
		t.Errorf("ActualCost = %s: the unqualified sheet priced a request served in the US", base)
	}
	// The durable record carries both, so a reconciler makes the same selection.
	rec := h.record(t, "req-geo-alt")
	if rec.Identity.InferenceGeo != "us" {
		t.Errorf("recorded InferenceGeo = %q, want %q: it is the pricing selector and a "+
			"reconciliation cannot re-derive it", rec.Identity.InferenceGeo, "us")
	}
	if !rec.Quote.Valid() {
		t.Error("the record must carry the captured quote it was priced from")
	}
}

// A geography no rate was captured for settles unresolved. Falling back to the base sheet
// would be pricing a modifier throttle cannot see, and the direction of that error is
// always downward.
func TestUncapturedGeographyIsUnresolvedNotBaseRated(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000000, "output_tokens": 200000, "inference_geo": "eu-sovereign-2027"
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-geo-unknown", Params: request(opus5, 200000),
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved", err)
	}
	if res.Cost.Known() {
		t.Fatalf("cost = %s, known: a geography with no captured rate prices nothing",
			res.Cost.Amount)
	}
	if base := dollars(t, "10"); res.Cost.Amount == base {
		t.Errorf("cost = %s: an unpriced geography fell back to base rates, which understates "+
			"every request served in a geography carrying a premium", base)
	}
	if !strings.Contains(res.Cost.Reason, "eu-sovereign-2027") {
		t.Errorf("reason %q must name the geography that served the call, since adding that row "+
			"is the operator action this record is asking for", res.Cost.Reason)
	}
	// The usage itself is authoritative and kept, which is what makes the record
	// resolvable once the row exists.
	if got, _ := res.Usage.Get(usage.InputTokens); got != 1000000 {
		t.Errorf("InputTokens = %d, want 1000000: an unpriced geography does not make usage "+
			"unknown", got)
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: real money was spent in a geography throttle cannot price, so " +
			"the hold must stay encumbered")
	}
}

// Geography is never inferred. Not from the machine, not from an IP, not from a timezone,
// not from an AWS region -- only from Anthropic's own request parameter or usage field.
//
// A response that reports no geography leaves the field empty, and empty prices through
// the unqualified sheet rather than through a default throttle chose. "global" is
// Anthropic's documented default and still not throttle's to assume, because a workspace
// can default to us and the response is the only thing that says which one ran.
func TestAbsentGeographyIsNotDefaulted(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{"input_tokens": 1000000, "output_tokens": 200000}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-geo-absent", Params: request(opus5, 200000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if res.Identity.InferenceGeo != "" {
		t.Errorf("InferenceGeo = %q, want empty: nothing in this response named a geography",
			res.Identity.InferenceGeo)
	}
	if want := dollars(t, "10"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s from the unqualified sheet", res.Charge.ActualCost, want)
	}
}

// Web search is not multiplied by geography, because the published multiplier enumerates
// what it applies to and web search is not in it.
//
// Anti-vacuous the same way as the rest: the search count is large enough that a 1.1x
// applied to it would show up as a whole dollar rather than as rounding.
func TestGeographyDoesNotMultiplyWebSearch(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000000, "output_tokens": 200000, "inference_geo": "us",
		"server_tool_use": {"web_search_requests": 1000}
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-geo-search", Params: request(opus5, 200000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	// Tokens at US rates ($11) plus 1,000 searches at $10/1,000 ($10), unmultiplied.
	if want := dollars(t, "21"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: tokens carry the US multiplier and the search price "+
			"does not", res.Charge.ActualCost, want)
	}
	if multiplied := dollars(t, "22"); res.Charge.ActualCost == multiplied {
		t.Errorf("ActualCost = %s: the geography multiplier was applied to web search, which the "+
			"pricing page does not qualify by geography", multiplied)
	}
}

// A geography-qualified catalog cannot price a request whose geography is unknown, and
// under enforce that means the call does not happen.
//
// The inverse of the fixture's three-row design. A catalog stating only US rates prices
// nothing at admission, because a request that names no geography may be served globally
// -- and admitting it against the US sheet would be reserving against a price sheet
// throttle has no reason to think applies.
func TestGeoOnlyCatalogRefusesAGeographylessRequest(t *testing.T) {
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "anthropic",
		ProviderModelID: opus5,
		InferenceGeo:    "us",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "5.50")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "27.50")),
		},
		Provenance: pricing.Provenance{Source: "test-us-only"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	h := newHarness(t, "1000", func(c *anthropic.Config) { c.Catalog = cat })

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-geo-only", Params: request(opus5, 2000),
	})
	if err == nil {
		t.Fatalf("NewMessage succeeded at %s: a US-only sheet must not price a request whose "+
			"geography is unknown", res.Charge.ActualCost)
	}
	if h.api.callCount() != 0 {
		t.Error("Anthropic was called for a request enforce could not bound")
	}
	if got := h.totals(t).Spent; got != 0 {
		t.Errorf("Spent = %s, want 0", got)
	}
}

// Adding the geography axis left Bedrock's and OpenAI's regional pricing untouched, and
// this is the Anthropic-side half of that: a request with no geography, on a provider
// whose fixtures price three sheets per model, still settles at the ordinary rate.
//
// Worth its own test because the failure mode is quiet. Narrowing wholesale rather than
// per-axis would make every unqualified request on this provider resolve to nothing the
// moment any row named a geography -- and the symptom would be ordinary traffic going
// unresolved, not an obviously wrong number.
func TestOrdinaryRequestsAreUnaffectedByThePricedGeographyAxis(t *testing.T) {
	for _, model := range []string{opus5, sonnet5, haiku45} {
		t.Run(model, func(t *testing.T) {
			h := newHarness(t, "1000")
			h.api.out = message(t, model, 1000, 500)

			res, err := h.client.NewMessage(context.Background(), anthropic.Request{
				BudgetID: "team", RequestID: "req-geo-plain-" + model, Params: request(model, 2000),
			})
			if err != nil {
				t.Fatalf("NewMessage: %v", err)
			}
			if !res.Cost.Known() {
				t.Fatalf("an ordinary request must settle known: %s", res.Cost.Reason)
			}
			if res.Charge.ActualCost == 0 {
				t.Error("ActualCost = 0")
			}
		})
	}
}

// The captured quote is enough to reprice a geography-modified request with no catalog
// and no second Anthropic call, which is what reconciliation depends on.
func TestCapturedQuoteRepricesAGeographyWithoutTheCatalog(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, fmt.Sprintf(`{
		"input_tokens": 1000000, "output_tokens": 200000, "inference_geo": %q
	}`, "us"))

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-geo-replay", Params: request(opus5, 200000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	// Replayed from the frozen quote alone: no catalog, no client, no network.
	rec := h.record(t, "req-geo-replay")
	sheet, err := rec.Quote.For(rec.Identity)
	if err != nil {
		t.Fatalf("the captured quote cannot price the geography that served the call, so a "+
			"reconciler would have to consult the live catalog -- which #30 forbids: %v", err)
	}
	priced, err := sheet.Price(rec.ActualUsage)
	if err != nil {
		t.Fatalf("Price from the frozen sheet: %v", err)
	}
	if priced.Cost.Amount != res.Charge.ActualCost {
		t.Errorf("replayed cost = %s but the request settled at %s: reconciliation must reach the "+
			"same figure from the record alone", priced.Cost.Amount, res.Charge.ActualCost)
	}
}

// A price refresh landing mid-request cannot change what that request costs, including
// its geography rates.
//
// The direction the frozen-quote tests above do not cover. They prove the captured quote
// is present and that it selects the right sheet; this proves settlement actually reads
// from it rather than re-asking a catalog that has since moved. Those are different
// claims, and only this one fails if settlement quietly consults the live catalog --
// which is #30's rule, and the more dangerous half of it here, because Anthropic's
// geography alternates make a live lookup look like it is doing something reasonable.
func TestAPriceRefreshMidRequestCannotChangeTheCost(t *testing.T) {
	cat := &mutableAnthropicCatalog{}
	cat.set(t, "5", "25") // per million, matching the opus-5 fixture

	h := newHarness(t, "1000", func(c *anthropic.Config) { c.Catalog = cat })
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000000, "output_tokens": 200000, "inference_geo": "us"
	}`)
	h.api.block = make(chan struct{})

	type outcome struct {
		res *anthropic.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := h.client.NewMessage(context.Background(), anthropic.Request{
			BudgetID: "team", RequestID: "req-refresh", Params: request(opus5, 200000),
		})
		done <- outcome{res, err}
	}()

	// Admission has happened once the provider has been called, so raising every rate a
	// hundredfold from here must not reach this request.
	waitFor(t, func() bool { return h.api.callCount() == 1 })
	cat.set(t, "500", "2500")
	close(h.api.block)

	got := <-done
	if got.err != nil {
		t.Fatalf("NewMessage: %v", got.err)
	}
	// 1M input plus 200k output at the captured US rates: $5.50/M and $27.50/M.
	if want := dollars(t, "11"); got.res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s: settlement must replay the rates captured at "+
			"admission, not whatever the catalog says now", got.res.Charge.ActualCost, want)
	}
	if refreshed := dollars(t, "1100"); got.res.Charge.ActualCost == refreshed {
		t.Errorf("ActualCost = %s: the request was priced from the refreshed catalog, so every "+
			"in-flight request during a price update would be charged the new rates", refreshed)
	}
	// And the frozen basis is durable, so a reconciler reaches the same figure.
	rec := h.record(t, "req-refresh")
	if !rec.Quote.Valid() {
		t.Fatal("the record must carry the quote it was priced from")
	}
	sheet, err := rec.Quote.For(rec.Identity)
	if err != nil {
		t.Fatalf("the captured quote cannot price the geography that served the call: %v", err)
	}
	priced, err := sheet.Price(rec.ActualUsage)
	if err != nil {
		t.Fatalf("Price from the frozen sheet: %v", err)
	}
	if priced.Cost.Amount != got.res.Charge.ActualCost {
		t.Errorf("replayed cost = %s but the request settled at %s", priced.Cost.Amount,
			got.res.Charge.ActualCost)
	}
}

// mutableAnthropicCatalog is a catalog whose rates can be replaced mid-flight, standing in
// for a price refresh landing between admission and settlement.
//
// It carries the fixture's three-row-per-model shape, because a stand-in that priced only
// the unqualified sheet could not distinguish "settled from the frozen quote" from "fell
// back to a geographyless lookup" -- the two would give the same number.
type mutableAnthropicCatalog struct {
	mu     sync.RWMutex
	static *pricing.Static
}

func (m *mutableAnthropicCatalog) set(t *testing.T, input, output string) {
	t.Helper()
	// The published US-only multiplier: exactly 11/10 on every token category.
	usRate := func(s string) money.Money { return dollars(t, s) * 11 / 10 }
	price := func(geo string, in, out money.Money) pricing.Price {
		return pricing.Price{
			AccessProvider:  "anthropic",
			ProviderModelID: opus5,
			InferenceGeo:    geo,
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens:  pricing.PerMillion(usage.InputTokens, in),
				usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, out),
			},
			Provenance: pricing.Provenance{Source: "test", Version: "test-1", Currency: "USD"},
		}
	}
	base, out := dollars(t, input), dollars(t, output)
	s, err := pricing.NewStatic(
		price("", base, out),
		price("global", base, out),
		price("us", usRate(input), usRate(output)),
	)
	if err != nil {
		t.Fatalf("pricing.NewStatic: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.static = s
}

func (m *mutableAnthropicCatalog) current() *pricing.Static {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.static
}

func (m *mutableAnthropicCatalog) Quote(ctx context.Context, id usage.ModelIdentity, u usage.Usage, at time.Time) (pricing.Quote, error) {
	return m.current().Quote(ctx, id, u, at)
}

func (m *mutableAnthropicCatalog) Capture(id usage.ModelIdentity, at time.Time) (pricing.CapturedQuote, error) {
	return m.current().Capture(id, at)
}
