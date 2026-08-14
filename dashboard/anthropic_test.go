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

// These tests ask the display half of #29, and the question is sharper than #28's. Anthropic
// brings three dimensions no page has rendered before -- two TTL-specific cache writes and a
// count of web searches, which is not a token at all -- plus a Claude reached directly rather
// than through Bedrock. All four must appear through machinery that already exists.
//
// The pressure is on the phrase "existing dimensional usage UI". A page that had to learn the
// word "anthropic" to show a one-hour cache write would be evidence that the neutral record
// is not neutral, and a page that quietly folded 1,000 searches into "in / out" would be
// arithmetically absurd in a way no reader could catch.

// anthropicIdentity is a Claude reached directly. Bedrock's Claude carries the same
// publisher and a different access provider, which is the distinction these tests are built
// around.
func anthropicIdentity() usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "anthropic",
		Publisher:       "anthropic",
		ProviderModelID: "claude-opus-5",
		Operation:       "messages",
	}
}

// anthropicRecord is a settled Messages request with the usage Anthropic actually reports:
// fresh input, a cache read, and the two TTL-specific writes, all disjoint and all additive.
func anthropicRecord(requestID, budgetID, periodID string, cost money.Money, at time.Time) activity.Record {
	id := anthropicIdentity()
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
			AccessProvider:  "anthropic",
			ProviderModelID: "claude-opus-5",
			Rates: map[usage.Dimension]pricing.Rate{
				usage.InputTokens: pricing.PerMillion(usage.InputTokens, dollars(5)),
			},
			Provenance: pricing.Provenance{
				Source:      "throttle-fixture:platform.claude.com/docs/en/about-claude/pricing",
				Version:     "2026-08-12-fixture-1",
				RetrievedAt: at,
				Currency:    "USD",
			},
			CapturedAt: at,
		},
		Reserved: cost * 2,
		ActualUsage: usage.New(map[usage.Dimension]int64{
			usage.InputTokens:        2000,
			usage.CacheReadTokens:    100000,
			usage.CacheWrite5mTokens: 40000,
			usage.CacheWrite1hTokens: 8000,
			usage.OutputTokens:       1500,
		}),
		ActualCost:      usage.KnownCost(cost),
		EnforcementMode: engine.ModeEnforce,
		Status:          activity.StatusSettled,
		Outcome:         activity.OutcomeSuccess,
		StartedAt:       at,
		CompletedAt:     at.Add(4 * time.Second),
		Latency:         4 * time.Second,
		Metadata:        map[string]string{"anthropic.served_model": "claude-opus-5"},
	}
}

// A direct Anthropic request renders its identity through the columns that already exist,
// with the exact provider model ID and no new column.
func TestAnthropicRequestRendersItsIdentity(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(6), at)
	w.record(anthropicRecord("m1", "research", p.ID, cents(6), at))

	body := w.html("/?budget=research")
	acts := panel(t, body, "activity")

	for col, want := range map[string]string{
		"Access provider": "anthropic",
		"Publisher":       "anthropic",
		"Model":           "claude-opus-5",
		"Operation":       "messages",
	} {
		cells := cellsUnder(t, acts, col)
		if len(cells) == 0 {
			t.Fatalf("%s column is missing", col)
		}
		if cells[0] != want {
			t.Errorf("%s = %q, want %q", col, cells[0], want)
		}
	}

	// A model the catalog maps to no canonical name shows the exact provider ID, marked as
	// raw rather than dressed up -- because that string is what the invoice will say.
	mustContain(t, body, "unmapped",
		"a model with no canonical name must be marked as showing a raw provider ID")

	// No provider branch. The word "anthropic" appears as data in the cells above and must
	// not appear as presentation vocabulary anywhere in the markup.
	for _, phrase := range []string{
		"Anthropic Messages", "anthropic-only", "Claude cache", "anthropic cache",
	} {
		mustNotContain(t, body, phrase,
			"ordinary presentation must not name the provider: "+phrase)
	}
}

// Direct Claude and Bedrock Claude are separate access providers on one page, and the
// breakdown separates them.
//
// The display half of one of throttle's model-identity invariants. A reader asking "how much
// of my Claude spend goes through Bedrock?" is asking a question only two distinct access
// providers can answer -- and the publisher column has to say the same thing for both, or
// the invariant has collapsed the other way.
func TestDirectClaudeAndBedrockClaudeAreDistinctAccessProviders(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	// Column count measured with one family on the page, so a grown column is caught
	// against a number this test derived rather than one it hard-coded.
	w.spend("s1", "research", cents(6), at)
	w.record(anthropicRecord("direct1", "research", p.ID, cents(6), at))
	before := len(headersOf(t, panel(t, w.html("/?budget=research"), "activity")))
	if before == 0 {
		t.Fatal("the activity table has no header row; this test cannot measure anything")
	}

	// The same publisher's model reached through Bedrock instead.
	w.spend("s2", "research", cents(9), at.Add(time.Minute))
	viaBedrock := anthropicRecord("bedrock1", "research", p.ID, cents(9), at.Add(time.Minute))
	viaBedrock.Identity.AccessProvider = "bedrock"
	viaBedrock.Identity.ProviderModelID = "anthropic.claude-opus-5-v1:0"
	viaBedrock.Identity.Region = "us-west-2"
	viaBedrock.Identity.Operation = "converse"
	viaBedrock.Estimate.Identity = viaBedrock.Identity
	w.record(viaBedrock)

	body := w.html("/?budget=research")
	acts := panel(t, body, "activity")

	if after := len(headersOf(t, acts)); after != before {
		t.Errorf("the activity table grew from %d columns to %d when a direct Anthropic request "+
			"joined a Bedrock one: both are ordinary records in the existing columns", before, after)
	}

	access := cellsUnder(t, acts, "Access provider")
	publisher := cellsUnder(t, acts, "Publisher")
	if len(access) != 2 || len(publisher) != 2 {
		t.Fatalf("access=%v publisher=%v, want two rows each", access, publisher)
	}
	// Newest first: Bedrock on top.
	if access[0] != "bedrock" || access[1] != "anthropic" {
		t.Errorf("access providers = %v, want [bedrock anthropic]: two paths to one publisher's "+
			"model are two access providers", access)
	}
	if publisher[0] != "anthropic" || publisher[1] != "anthropic" {
		t.Errorf("publishers = %v, want anthropic twice: the publisher is the same company "+
			"however the model was reached", publisher)
	}

	// And the breakdown answers the migration question in both directions.
	bd := breakdownPanel(t, body, "Access provider")
	for _, ap := range []string{"anthropic", "bedrock"} {
		mustContain(t, bd, ap, "the access-provider breakdown must have a row for "+ap)
	}
	raw := rawBody(t, w, "/api/breakdown?facet=access-provider&budget=research")
	for _, ap := range []string{"anthropic", "bedrock"} {
		if !strings.Contains(raw, ap) {
			t.Errorf("/api/breakdown?facet=access-provider does not carry %q", ap)
		}
	}
}

// The two TTL-specific cache writes render as tokens, distinctly, through the existing
// dimensional usage UI.
//
// Anti-vacuous by size: 40,000 five-minute writes and 8,000 one-hour writes are priced
// differently enough that a cell collapsing them into one figure would be describing money
// that does not exist. And they must not be badged non-token, because they are tokens billed
// per token -- the badge would tell a reader no per-token rate is needed.
func TestCacheWriteTTLRendersThroughTheDimensionalUI(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(60), at)
	w.record(anthropicRecord("cache1", "research", p.ID, cents(60), at))

	body := w.html("/?budget=research")
	acts := panel(t, body, "activity")

	// The summary cell names both lifetimes, because they carry different prices.
	cells := cellsUnder(t, acts, "Usage")
	if len(cells) != 1 {
		t.Fatalf("the Usage column has %d cells, want 1: %v", len(cells), cells)
	}
	for _, want := range []string{"5m", "1h"} {
		if !strings.Contains(cells[0], want) {
			t.Errorf("usage cell = %q, should distinguish the %s cache lifetime: a one-hour write "+
				"costs materially more than a five-minute one", cells[0], want)
		}
	}
	// Input is the sum, because Anthropic's counters are disjoint and additive. 2,000 fresh
	// plus 100,000 read plus 40,000 plus 8,000 written.
	if !strings.Contains(cells[0], "150,000 in") {
		t.Errorf("usage cell = %q, should show 150,000 input tokens: Anthropic's input counters "+
			"are disjoint, so the total is their sum", cells[0])
	}

	// The detail page shows every dimension with its exact count, which is where the
	// disjointness is auditable.
	detail := w.html("/request/cache1")
	for dim, count := range map[string]string{
		"input_tokens":          "2,000",
		"cache_read_tokens":     "100,000",
		"cache_write_5m_tokens": "40,000",
		"cache_write_1h_tokens": "8,000",
		"output_tokens":         "1,500",
	} {
		mustContain(t, detail, dim, "the usage table must show the "+dim+" dimension")
		mustContain(t, detail, count, "the usage table must show the recorded count for "+dim)
	}
	// A TTL-specific write is a token dimension. Badging it otherwise is a false statement
	// about the unit.
	if strings.Count(detail, "non-token") != 0 {
		t.Error("a cache-write token dimension is badged non-token; the provider reported tokens " +
			"and bills per token, at a rate that depends on the lifetime")
	}
	// The aggregate the provider also reports must not appear: it is the sum of the two
	// children and pricing it beside them would double-charge every cache write.
	mustNotContain(t, detail, "cache_creation_input_tokens",
		"the provider's aggregate cache-creation counter is not a stored dimension; its "+
			"TTL-specific children are, and they decompose it exactly")
	mustNotContain(t, detail, "total_tokens",
		"total_tokens is not the billing primitive and is not a stored dimension")
}

// A web-search count renders as a count of searches, not as tokens.
//
// Anti-vacuous by absurdity: 1,000 searches folded into an input-token figure would read as
// 151,000 tokens, and the searches cost $10 on their own. The existing "anything else is
// named rather than dropped" rule is what carries this, and this test is what proves the
// rule was not quietly special-cased away.
func TestWebSearchCountRendersAsItsOwnUnit(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", dollars(21), at)

	rec := anthropicRecord("search1", "research", p.ID, dollars(21), at)
	rec.ActualUsage.Set(usage.Searches, 1000)
	w.record(rec)

	body := w.html("/?budget=research")
	acts := panel(t, body, "activity")

	cells := cellsUnder(t, acts, "Usage")
	if len(cells) != 1 {
		t.Fatalf("the Usage column has %d cells, want 1: %v", len(cells), cells)
	}
	if !strings.Contains(cells[0], "searches") {
		t.Errorf("usage cell = %q, should name the searches: a request that ran 1,000 web searches "+
			"cost $10 for them alone, and a token figure cannot express that", cells[0])
	}
	if strings.Contains(cells[0], "151,000 in") {
		t.Error("the search count was summed into the input tokens, which is not a unit it shares")
	}

	// On the detail page it is a dimension like any other, and badged as the non-token unit
	// it genuinely is -- the opposite call from the cache writes above, for the opposite
	// reason: a search is priced per search.
	detail := w.html("/request/search1")
	mustContain(t, detail, "searches", "the usage table must show the searches dimension")
	mustContain(t, detail, "1,000", "the usage table must show the recorded search count")
	mustContain(t, detail, "non-token",
		"a search is not a token and must be badged as the unit it is, since no per-token "+
			"rate would price it")
}

// A Messages request whose price throttle cannot complete renders as a floor and never as
// free, and the bookkeeping panel names what is missing.
//
// The case Anthropic makes newly reachable: a server-side tool billed in a unit the response
// does not report -- code-execution container time -- leaves the token arithmetic valid and
// the total unknown. The distinction between $0.30 and $0.30+ is the whole difference between
// a measurement and a lower bound.
func TestUnpriceableServerToolRendersAsAFloor(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	// Anti-vacuous baseline: the same amount, settled, prints as a plain figure. So every
	// assertion below is about completeness rather than about a table that cannot print money.
	const floor = money.Money(300000) // $0.30
	const plain = "$0.3000"
	w.spend("s1", "research", floor, at)
	w.record(anthropicRecord("settled", "research", p.ID, floor, at))
	if got := cellsUnder(t, panel(t, w.html("/?budget=research"), "activity"), "Actual"); len(got) != 1 ||
		got[0] != plain {
		t.Fatalf("the settled baseline renders as %v, want [%s]: this test cannot prove the display "+
			"marks a figure incomplete if it cannot print the figure", got, plain)
	}

	unpriced := anthropicRecord("exec1", "research", p.ID, floor, at.Add(time.Minute))
	unpriced.ActualCost = usage.PartialCost(floor,
		[]usage.Dimension{usage.Dimension("container_hours")},
		"code execution container time is not reported in the response, so the token cost is a floor")
	unpriced.Status = activity.StatusUnresolved
	unpriced.Outcome = activity.OutcomeUnpriced
	unpriced.Reserved = dollars(1)
	w.record(unpriced)

	body := w.html("/?budget=research")
	acts := panel(t, body, "activity")

	actuals := cellsUnder(t, acts, "Actual")
	if len(actuals) != 2 {
		t.Fatalf("the activity table has %d rows, want 2: %v", len(actuals), actuals)
	}
	if actuals[0] != plain+"+" {
		t.Errorf("the unpriced request renders as %q, want %q: the token arithmetic is real and it "+
			"is not the whole bill", actuals[0], plain+"+")
	}
	for _, falseZero := range []string{"$0.0000", "—"} {
		if actuals[0] == falseZero {
			t.Errorf("the unpriced request renders as %q: it ran a hosted tool and was not free",
				falseZero)
		}
	}
	// The page total inherits the incompleteness rather than absorbing it.
	if spend := figure(t, body, "page-spend"); !strings.HasSuffix(spend, "+") {
		t.Errorf("page spend = %q, want a trailing + to mark a floor", spend)
	}
	// And the panel turns the gap into a specific piece of work.
	mustContain(t, panel(t, body, "recon"), "container_hours",
		"the bookkeeping panel must name the unit the catalog is missing a rate for")
}
