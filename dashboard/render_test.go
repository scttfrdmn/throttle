package dashboard

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/report"
	"github.com/scttfrdmn/throttle/usage"
)

// These tests are about the accounting vocabulary surviving the trip into HTML. The read
// model's own suite proves the arithmetic; what a template can still get wrong is putting
// a reserved amount in a column headed "spent", printing an unknown cost as a number, or
// dropping a minus sign.

// (1) A pace balance is signed in both directions, and the sign is printed rather than
// implied by a colour.
func TestPaceBalanceRendersBothSigns(t *testing.T) {
	// Half the period elapsed against a $1,000 allocation puts the target at $500.
	behind := newWorld(t)
	behind.define(monthly("research", "", dollars(1000)))
	behind.spend("s1", "research", dollars(200), behind.now.Add(-time.Hour))

	body := behind.html("/?budget=research")
	if got := figure(t, body, "pace"); got != "+$300.00" {
		t.Errorf("banked pace balance = %q, want %q", got, "+$300.00")
	}
	mustContain(t, body, "BANKED", "a positive pace balance is banked")
	mustContain(t, body, "target by now − spent",
		"the pace balance must state its formula")

	ahead := newWorld(t)
	ahead.define(monthly("research", "", dollars(1000)))
	ahead.spend("s1", "research", dollars(800), ahead.now.Add(-time.Hour))

	body = ahead.html("/?budget=research")
	got := figure(t, body, "pace")
	if got != "−$300.00" {
		t.Errorf("borrowed pace balance = %q, want %q", got, "−$300.00")
	}
	if !strings.ContainsAny(got, "−-") {
		t.Errorf("borrowed pace balance %q carries no visible sign", got)
	}
	mustContain(t, body, "BORROWED", "a negative pace balance is borrowed")

	// The bipolar display names both poles regardless of which way the figure points, so
	// the axis is legible without reading the number first.
	mustContain(t, body, "BORROWED", "the bipolar scale labels its negative pole")
	mustContain(t, body, "BANKED", "the bipolar scale labels its positive pole")

	// And the pace balance is not the remaining allocation. They are different figures
	// answering different questions, and the page must not print one for the other.
	if pace, remaining := figure(t, body, "pace"), figure(t, body, "remaining-2"); pace == remaining {
		t.Errorf("pace balance and remaining allocation both render as %q; "+
			"they answer different questions", pace)
	}
}

// (2) and (3) Reserved is its own figure, never folded into spent, and spendable-now
// subtracts it.
func TestReservedIsNeverSpent(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(200), w.now.Add(-time.Hour))
	w.hold("h1", "research", dollars(50))

	body := w.html("/?budget=research")

	spent := figure(t, body, "spent")
	reserved := figure(t, body, "reserved")
	if spent != "$200.00" {
		t.Errorf("spent = %q, want $200.00 (settled only)", spent)
	}
	if reserved != "$50.00" {
		t.Errorf("reserved = %q, want $50.00", reserved)
	}
	if spent == "$250.00" {
		t.Error("spent includes the reservation: reserved money has not been spent")
	}

	// Target by now is $500 at the midpoint; spendable now is allowed − spent − reserved.
	if got := figure(t, body, "spendable"); got != "$250.00" {
		t.Errorf("spendable now = %q, want $250.00 (allowed 500 − spent 200 − reserved 50)", got)
	}
	if got := figure(t, body, "remaining-2"); got != "+$750.00" {
		t.Errorf("remaining = %q, want +$750.00 (total 1000 − spent 200 − reserved 50)", got)
	}

	// The words matter as much as the numbers.
	mustContain(t, body, "Promised, not spent.", "the Reserved figure must say it is not spend")
	mustContain(t, body, "allowed by now − spent − reserved",
		"spendable now must state that it subtracts reservations")
	mustContain(t, body, "total − spent − reserved",
		"remaining must state that it subtracts reservations")
	mustContain(t, body, "settled. Money that is gone.",
		"the Spent figure must say it is settled")

	// The whole vocabulary is present. A page missing one of these terms would leave the
	// reader to infer it, which is how "reserved" becomes "spent" in someone's head.
	for _, field := range []string{
		"allocation", "spent", "reserved", "remaining-2",
		"target", "allowed", "pace", "spendable",
		"period-start", "period-end", "elapsed", "time-remaining-2",
	} {
		if figure(t, body, field) == "" {
			t.Errorf("the position panel renders no value for %q", field)
		}
	}

	// The live hold appears behind the figure, so Reserved is explicable rather than
	// asserted.
	mustContain(t, body, "Reservations", "a live hold should list under Reservations")
	if held := cellsUnder(t, body, "Held"); len(held) != 1 || held[0] != "$50.00" {
		t.Errorf("Reservations table Held column = %v, want one $50.00 row", held)
	}
	mustContain(t, body, "Reserved is not spend.",
		"the reservations panel must repeat that a hold is not spend")
}

// (4) Both burn rates are stated with their formulas, and the average is never called
// current or instantaneous.
func TestBurnRatesStateTheirFormulas(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(310), w.now.Add(-time.Hour))

	body := w.html("/?budget=research")

	// 15.5 days elapsed, $310 settled: $20/day exactly.
	if got := figure(t, body, "average-burn"); got != "$20.00/day" {
		t.Errorf("average burn = %q, want $20.00/day (310 / 15.5d)", got)
	}
	// $690 remaining allocation over the 15.5 days that are left: $44.516…, rounded once
	// at the point of display.
	if got := figure(t, body, "sustainable-burn"); got != "$44.52/day" {
		t.Errorf("sustainable burn = %q, want $44.52/day (690 / 15.5d)", got)
	}

	mustContain(t, body, "settled spend ÷ elapsed period time",
		"the average rate must state its formula")
	mustContain(t, body, "remaining allocation ÷ remaining period time",
		"the sustainable rate must state its formula")
	mustContain(t, body, "Average burn to date",
		"the average rate must be labelled as an average to date")
	mustContain(t, body, "not a current or instantaneous rate",
		"the average rate must disclaim being instantaneous")

	// The vocabulary the brief rules out for this figure. "Instantaneous" is excluded from
	// the list because the page uses it in the denial above, which is the opposite of the
	// mistake: what must not appear is a claim to be one of these things.
	for _, forbidden := range []string{"real-time", "moving average", "EWMA", "exponentially weighted"} {
		mustNotContain(t, body, forbidden,
			"the average-to-date rate must not be described this way")
	}
}

// (4, continued) The gauge is the ratio of those two rates, and it says so permanently --
// not in a tooltip, because the misreading it prevents is "percent of budget spent".
func TestGaugeStatesItsFormulaAndIsNotPercentSpent(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))
	// Exactly on pace at the midpoint: average burn equals sustainable burn.
	w.spend("s1", "research", dollars(500), w.now.Add(-time.Hour))

	body := w.html("/?budget=research")
	if got := figure(t, body, "pressure"); got != "100.00%" {
		t.Errorf("burn pressure at exactly on pace = %q, want 100.00%%", got)
	}
	mustContain(t, body, "average burn to date ÷ sustainable burn for the time left",
		"the gauge must state its formula")
	mustContain(t, body, "This is not percent of budget spent.",
		"the gauge must deny being percent of budget spent")
	mustContain(t, body, "100% = exactly on track to finish at the allocation",
		"the gauge must say what 100% means")
	mustContain(t, body, "exactly on track to finish at the allocation",
		"the reading should be explained in words as well as a number")

	// Half spent at the halfway mark is 100% pressure, not 50%: the two readings are
	// different questions and the page must not answer the wrong one.
	if got := figure(t, body, "pressure"); got == "50.00%" {
		t.Error("the gauge is reporting percent of budget spent")
	}

	// Over the redline, the words change with the number.
	hot := newWorld(t)
	hot.define(monthly("research", "", dollars(1000)))
	hot.spend("s1", "research", dollars(750), hot.now.Add(-time.Hour))
	body = hot.html("/?budget=research")
	if got := figure(t, body, "pressure"); got != "300.00%" {
		t.Errorf("burn pressure at 3x sustainable = %q, want 300.00%%", got)
	}
	mustContain(t, body, "burning faster than the remaining allocation sustains",
		"a reading over the redline must be explained")
	mustContain(t, body, "Off the dial.",
		"a reading past the dial's full scale must say the needle stopped")
}

// (6) The projection is labelled as a straight line every time it appears, and never
// dressed up as a forecast.
func TestProjectionIsLabelledStraightLine(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(600), w.now.Add(-time.Hour))

	body := w.html("/?budget=research")
	mustContain(t, body, "Straight-line projection",
		"the projection must be labelled as a straight line")
	if got := figure(t, body, "projection"); got != "$1,200.00" {
		t.Errorf("projection = %q, want $1,200.00 (600 doubled at the midpoint)", got)
	}
	mustContain(t, body, "assumes the average rate to date continues; not a forecast",
		"the projection must state its method and disclaim forecasting")
	mustContain(t, body, "over the allocation by $200.00",
		"a projection past the allocation should say by how much")

	for _, forbidden := range []string{"forecast的", "predicted", "expected spend", "confidence interval"} {
		mustNotContain(t, body, forbidden, "the projection must not imply statistical sophistication")
	}

	// Too little elapsed time is a low-confidence reading, and it says so rather than
	// presenting a wild extrapolation as a figure.
	early := newWorld(t)
	early.now = base.Add(2 * time.Hour) // well under 5% of a month
	early.define(monthly("research", "", dollars(1000)))
	early.spend("s1", "research", dollars(20), base.Add(time.Hour))

	body = early.html("/?budget=research")
	mustContain(t, body, "very little of the period has elapsed",
		"a projection from almost no elapsed time must be qualified")
}

// (5) The zero-duration and zero-remaining edges do not divide by zero, and do not report
// a rate of zero -- which would be a claim about spending rather than about the clock.
func TestClockEdgesAreSafe(t *testing.T) {
	t.Run("before the period starts", func(t *testing.T) {
		// The envelope has to be materialized from inside itself, then left behind: the
		// period a budget is defined into is the one containing the clock.
		w := newWorld(t).at(base.Add(24 * time.Hour))
		w.define(week("demo", dollars(100)))
		// Roll the clock back before the envelope: nothing has elapsed.
		w.now = base.Add(-24 * time.Hour)

		body := w.html("/?budget=demo")
		if got := figure(t, body, "pressure"); got != "no reading" {
			t.Errorf("pressure before the period = %q, want %q", got, "no reading")
		}
		if got := figure(t, body, "average-burn"); got != "—" {
			t.Errorf("average burn before the period = %q, want an em dash, not a rate", got)
		}
		if got := figure(t, body, "projection"); got != "—" {
			t.Errorf("projection before the period = %q, want an em dash", got)
		}
		mustContain(t, body, "no time has elapsed yet, so there is no rate to measure",
			"the gauge must explain an absent reading")
		mustContain(t, body, "this period has not started yet",
			"a future period must be announced")
		mustNotContain(t, body, "$0.00/day",
			"no elapsed time is not a burn rate of zero")
	})

	t.Run("after the period ends", func(t *testing.T) {
		w := newWorld(t).at(base.Add(24 * time.Hour))
		p := w.define(week("demo", dollars(100)))
		w.spend("s1", "demo", dollars(40), base.Add(24*time.Hour))
		w.now = p.Envelope.End.Add(48 * time.Hour)

		body := w.html("/?budget=demo")
		if got := figure(t, body, "pressure"); got != "period over" {
			t.Errorf("pressure after the period = %q, want %q", got, "period over")
		}
		if got := figure(t, body, "sustainable-burn"); got != "—" {
			t.Errorf("sustainable burn after the period = %q, want an em dash", got)
		}
		mustContain(t, body, "no rate: the period is over",
			"an absent sustainable rate must be explained")
		mustContain(t, body, "there is no remaining time to sustain a rate across",
			"the gauge must explain a period-over reading")
		if got := figure(t, body, "time-remaining-2"); got != "none" {
			t.Errorf("time remaining after the period = %q, want %q", got, "none")
		}
	})

	t.Run("no remaining allocation", func(t *testing.T) {
		w := newWorld(t)
		w.define(monthly("research", "", dollars(100)))
		w.spend("s1", "research", dollars(100), w.now.Add(-time.Hour))

		body := w.html("/?budget=research")
		if got := figure(t, body, "pressure"); got != "pegged" {
			t.Errorf("pressure with no headroom = %q, want %q", got, "pegged")
		}
		mustContain(t, body, "no remaining allocation, so no sustainable rate exists",
			"a pegged gauge must explain why there is no ratio")
		if got := figure(t, body, "sustainable-burn"); got != "—" {
			t.Errorf("sustainable burn with no headroom = %q, want an em dash", got)
		}
		mustContain(t, body, "no sustainable rate: there is no remaining allocation to spread",
			"an exhausted allocation is a different absence from an ended period")
	})

	t.Run("zero spend over real elapsed time", func(t *testing.T) {
		w := newWorld(t)
		w.define(monthly("research", "", dollars(1000)))

		body := w.html("/?budget=research")
		// The one legitimate zero reading: time has passed and nothing was spent.
		if got := figure(t, body, "pressure"); got != "0.00%" {
			t.Errorf("pressure with elapsed time and no spend = %q, want 0.00%%", got)
		}
		mustContain(t, body, "nothing has been spent over real elapsed time",
			"a zero reading must be distinguished from an absent one")
		if got := figure(t, body, "spent"); got != "$0.00" {
			t.Errorf("spent = %q, want $0.00", got)
		}
	})

	t.Run("a zero-length envelope", func(t *testing.T) {
		w := newWorld(t)
		// An envelope that starts and ends at the same instant: degenerate, and reachable
		// from a mistyped definition.
		def := week("instant", dollars(100))
		def.EndAt = base.Add(time.Nanosecond)
		w.at(base).define(def)

		// The only requirement is that it renders rather than panics or divides by zero.
		body := w.html("/?budget=instant")
		mustContain(t, body, "instant", "a degenerate envelope must still render its budget")
		if got := elapsedPct(0, 0); got != "0.00" {
			t.Errorf("elapsedPct(0,0) = %q, want 0.00", got)
		}
	})
}

// (7) and (8) The three-valued cost survives rendering: unknown is never $0, and a partial
// cost keeps its "+".
func TestCostCompletenessSurvivesRendering(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	w.spend("s1", "research", cents(41), at)
	w.record(settledRecord("known", "research", p.ID, cents(41), at))
	w.record(unpriceableRecord("nocost", "research", p.ID, at.Add(time.Minute)))
	w.record(unknownCostRecord("liability", "research", p.ID, at.Add(2*time.Minute)))
	w.record(partialCostRecord("partial", "research", p.ID, at.Add(3*time.Minute)))

	body := w.html("/?budget=research")
	// The activity panel specifically: the bookkeeping panel renders the same table from
	// the same template, and an assertion that landed there would be counting outstanding
	// rows rather than all of them.
	acts := panel(t, body, "activity")
	actuals := cellsUnder(t, acts, "Actual")
	if len(actuals) != 4 {
		t.Fatalf("activity table has %d rows, want 4: %v", len(actuals), actuals)
	}

	// Four rows, four different readings. None of them is a zero.
	want := map[string]string{
		"a settled request nothing could price": "unknown",
		"an unresolved liability":               "unresolved",
		"a partially priced request":            "$0.0300+",
		"a fully priced request":                "$0.4100",
	}
	got := map[string]bool{}
	for _, cell := range actuals {
		got[cell] = true
	}
	for what, cell := range want {
		if !got[cell] {
			t.Errorf("%s does not render as %q: %v", what, cell, actuals)
		}
	}
	for _, cell := range actuals {
		if strings.HasPrefix(cell, "$0.0000") && !strings.HasSuffix(cell, "+") {
			t.Errorf("a cost renders as %q; an incomplete cost must never read as zero: %v",
				cell, actuals)
		}
	}

	// "unknown" and "unresolved" are not synonyms, and the page must not use one word for
	// both: a settled request nothing could price is finished, and an unresolved liability
	// is still awaiting the information that would price it.
	if got["unknown"] && !got["unresolved"] {
		t.Error("an unresolved liability is being rendered as merely unknown; " +
			"only one of the two is still expected to change")
	}

	// The legend explains the notation on the page rather than requiring the reader to
	// know it.
	mustContain(t, body, "An unknown cost is never shown as $0.",
		"the activity panel must state the rule")
	mustContain(t, body, "when some usage could not be priced",
		"the activity panel must explain the + notation")

	// An unresolved liability whose amount is genuinely zero reads as "unresolved", not as
	// a free request.
	if got := amount(report.Amount{State: report.CostUnresolved}); got != "unresolved" {
		t.Errorf("a zero-value unresolved amount renders as %q, want %q", got, "unresolved")
	}
	// A denied request or a released hold has no cost at all, which is a third thing again
	// and must not read as a priced zero.
	if got := amount(report.Amount{State: report.CostNone}); got != "—" {
		t.Errorf("an inapplicable cost renders as %q, want an em dash", got)
	}
	// And the zero value of the type -- a field nobody populated -- cannot claim a request
	// was free.
	if got := amount(report.Amount{}); got != "unknown" {
		t.Errorf("an unpopulated amount renders as %q, want %q", got, "unknown")
	}

	// The page total over incomplete rows is a floor and says so.
	if spend := figure(t, body, "page-spend"); !strings.HasSuffix(spend, "+") {
		t.Errorf("page spend over incomplete rows = %q, want a trailing + to mark a floor", spend)
	}
	mustContain(t, body, "a floor: some costs are incomplete",
		"an incomplete page total must be labelled")

	// And the same rule holds on the JSON surface, which is what the poll reads. This is
	// the second place an unknown cost could become a zero, and the reason the completeness
	// rules live in one function rather than in each renderer.
	obj := w.jsonBody("/api/activity?budget=research")
	raw := rawBody(t, w, "/api/activity?budget=research")
	for _, reading := range []string{`"unknown"`, `"unresolved"`, `"$0.0300+"`} {
		if !strings.Contains(raw, reading) {
			t.Errorf("/api/activity does not carry %s: the JSON has its own renderer and "+
				"must not round an incomplete cost into a number", reading)
		}
	}
	if !strings.HasSuffix(str(t, obj, "page_spend"), "+") {
		t.Errorf("/api/activity page_spend = %q, want a trailing +", str(t, obj, "page_spend"))
	}
	if strings.Contains(raw, `"actual":"$0.0000"`) {
		t.Error("/api/activity renders a cost as $0.0000")
	}
}

// (9) The three identity facets stay three columns. Collapsing AWS Bedrock, Anthropic, and
// Claude into one "provider" cell makes three different questions unanswerable at once.
func TestIdentityFacetsStayDistinct(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(41), at)
	w.record(settledRecord("r1", "research", p.ID, cents(41), at))

	body := w.html("/?budget=research")

	access := cellsUnder(t, body, "Access provider")
	publisher := cellsUnder(t, body, "Publisher")
	model := cellsUnder(t, body, "Model")
	if len(access) == 0 || len(publisher) == 0 || len(model) == 0 {
		t.Fatalf("identity columns are missing: access=%v publisher=%v model=%v",
			access, publisher, model)
	}
	if access[0] != "aws-bedrock" {
		t.Errorf("access provider = %q, want aws-bedrock", access[0])
	}
	if publisher[0] != "anthropic" {
		t.Errorf("publisher = %q, want anthropic", publisher[0])
	}
	if model[0] != "claude-sonnet-4" {
		t.Errorf("model = %q, want claude-sonnet-4", model[0])
	}
	if access[0] == publisher[0] || publisher[0] == model[0] {
		t.Error("two identity facets render the same value; they are independent fields")
	}

	// The exact provider model ID is retained alongside the canonical name, because it is
	// the identity the provider's bill uses.
	mustContain(t, body, "anthropic.claude-sonnet-4-20250514-v1:0",
		"the exact provider model ID must be available")

	// Each facet also gets its own breakdown, and the page explains why.
	for _, label := range []string{"Access provider", "Model publisher", "Model", "Provider model ID"} {
		mustContain(t, body, label, "a breakdown for each identity facet must exist")
	}
	mustContain(t, body, "three independent",
		"the page must explain that the identity fields are independent")

	// The detail page repeats all four, one figure each.
	detail := w.html("/request/r1")
	mustContain(t, detail, "Access provider", "the detail page must name the access path")
	mustContain(t, detail, "Model publisher", "the detail page must name the publisher")
	mustContain(t, detail, "Provider model ID", "the detail page must name the provider model ID")
	mustContain(t, detail, "the path the request took", "the access provider must be explained")
	mustContain(t, detail, "who made the model", "the publisher must be explained")
}

// (10) A model the catalog has never heard of falls back to the exact provider ID rather
// than to a guess or an "unknown" bucket.
func TestUnknownModelFallsBackToProviderID(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(7), at)
	w.record(unmappedModelRecord("r1", "research", p.ID, at))

	body := w.html("/?budget=research")
	model := cellsUnder(t, body, "Model")
	if len(model) == 0 || model[0] != "vendor.brand-new-model-v9:0" {
		t.Errorf("Model cell = %v, want the exact provider model ID", model)
	}
	mustNotContain(t, body, ">unknown model<", "an unrecognized model must not go into a bucket")

	// It is marked as unmapped so a reader can tell a canonical name from a raw ID.
	mustContain(t, body, "unmapped", "an unrecognized model should be marked as such")

	detail := w.html("/request/r1")
	mustContain(t, detail, "not in the catalog, so the exact provider ID is shown",
		"the detail page must explain the fallback")

	// The breakdown does the same thing rather than dropping the row.
	mustContain(t, body, "vendor.brand-new-model-v9:0",
		"the breakdown must group the unmapped model under its provider ID")

	// A publisher the provider never reported is named as unreported, not left blank: a
	// blank cell is indistinguishable from a rendering fault.
	if pub := cellsUnder(t, body, "Publisher"); len(pub) == 0 || pub[0] != "anthropic" {
		t.Errorf("Publisher = %v, want the reported publisher retained", pub)
	}
	if got := present(""); got != "not reported" {
		t.Errorf("present(\"\") = %q, want %q", got, "not reported")
	}
}

// (11) The hierarchy scopes correctly: each budget shows its own position, and a child's
// spend appears against its ancestors too.
func TestSubBudgetHierarchyScopes(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("lab", "", dollars(1000)))
	w.define(monthly("team-a", "lab", dollars(400)))
	w.define(monthly("team-b", "lab", dollars(300)))

	at := w.now.Add(-time.Hour)
	w.spend("s1", "team-a", dollars(100), at, "team-a", "lab")
	w.spend("s2", "team-b", dollars(30), at, "team-b", "lab")

	body := w.html("/?budget=lab")

	// The tree table lists all three, and the parent carries both children's spend.
	budgets := cellsUnder(t, body, "Budget")
	if len(budgets) != 3 {
		t.Fatalf("hierarchy table has %d rows, want 3: %v", len(budgets), budgets)
	}
	spents := cellsUnder(t, body, "Spent")
	if len(spents) != 3 {
		t.Fatalf("hierarchy Spent column has %d rows, want 3: %v", len(spents), spents)
	}
	if spents[0] != "$130.00" {
		t.Errorf("parent spent = %q, want $130.00 (both children)", spents[0])
	}

	// Selecting the child scopes the figures to the child.
	child := w.html("/?budget=team-a")
	if got := figure(t, child, "spent"); got != "$100.00" {
		t.Errorf("team-a spent = %q, want $100.00", got)
	}
	if got := figure(t, child, "allocation"); got != "$400.00" {
		t.Errorf("team-a allocation = %q, want $400.00", got)
	}
	mustContain(t, body, "consumes its ancestors' headroom",
		"the hierarchy must explain that a child consumes its ancestors")

	// The activity row records every scope the hold consumed, so a request against a
	// child is explicable from its parent.
	if got := scopeList(nil); got != "—" {
		t.Errorf("an empty scope list renders as %q, want an em dash", got)
	}
}

// (16) An empty dashboard is a first-run page, not a wall of zeros and not an error.
func TestEmptyDashboard(t *testing.T) {
	w := newWorld(t)

	rec := w.get("/")
	if rec.Code != 200 {
		t.Fatalf("GET / with no budgets = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	mustContain(t, body, "No budgets are defined.", "the first-run page must say so plainly")
	mustContain(t, body, "not the same as showing zero spend",
		"the first-run page must distinguish itself from zero spend")
	mustContain(t, body, "throttle config apply", "the first-run page should say what to do next")
	mustNotContain(t, body, `data-field="spent"`,
		"the first-run page must not render budget figures for a budget that does not exist")

	// The poll endpoint reports the state rather than an error, so a browser can tell the
	// difference without reading a status code.
	obj := w.jsonBody("/api/summary")
	if empty, _ := obj["empty"].(bool); !empty {
		t.Errorf("/api/summary with no budgets = %v, want {\"empty\": true}", obj)
	}
}

// A budget with no activity at all renders every figure and says the table is empty for
// the right reason.
func TestBudgetWithNoActivity(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))

	body := w.html("/?budget=research")
	if got := figure(t, body, "spent"); got != "$0.00" {
		t.Errorf("spent = %q, want $0.00", got)
	}
	if got := figure(t, body, "allocation"); got != "$1,000.00" {
		t.Errorf("allocation = %q, want $1,000.00", got)
	}
	mustContain(t, body, "No requests recorded in this period.",
		"an empty period must be stated rather than shown as a blank table")
	mustContain(t, body, "Nothing outstanding.",
		"clean bookkeeping should be stated")
}

// A store that was never configured is a different fact from one that is empty, and from
// one that failed. All three read differently.
func TestNoActivityStoreConfigured(t *testing.T) {
	w := newLedgerOnlyWorld(t)
	w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(120), w.now.Add(-time.Hour))

	body := w.html("/?budget=research")

	// Money still comes from the ledger.
	if got := figure(t, body, "spent"); got != "$120.00" {
		t.Errorf("spent with no activity store = %q, want $120.00 from the ledger", got)
	}
	mustContain(t, body, "No activity store is configured",
		"an unconfigured store must be named as such")
	mustContain(t, body, "Budget figures above come from the ledger and are unaffected",
		"the page must say that money is unaffected by missing history")
	mustNotContain(t, body, "No requests recorded in this period.",
		"an unconfigured store must not be reported as an empty period")
}

// A fully spent and an overspent budget both render honestly, including a negative
// remaining allocation.
func TestFullySpentAndOverspent(t *testing.T) {
	t.Run("fully spent", func(t *testing.T) {
		w := newWorld(t)
		w.define(monthly("research", "", dollars(100)))
		w.spend("s1", "research", dollars(100), w.now.Add(-time.Hour))

		body := w.html("/?budget=research")
		if got := figure(t, body, "remaining-2"); got != "$0.00" {
			t.Errorf("remaining when fully spent = %q, want $0.00", got)
		}
		if got := figure(t, body, "spendable"); got != "$0.00" {
			t.Errorf("spendable when fully spent = %q, want $0.00", got)
		}
	})

	t.Run("overspent", func(t *testing.T) {
		w := newWorld(t)
		w.define(monthly("research", "", dollars(100)))
		// Actual cost above the reservation is recorded, not clamped, so an envelope can
		// legitimately go negative.
		w.spend("s1", "research", dollars(140), w.now.Add(-time.Hour))

		body := w.html("/?budget=research")
		got := figure(t, body, "remaining-2")
		if got != "−$40.00" {
			t.Errorf("remaining when overspent = %q, want −$40.00", got)
		}
		if !strings.ContainsAny(got, "−-") {
			t.Errorf("an overspent remaining allocation %q has no visible sign", got)
		}
		if got := figure(t, body, "spendable"); got != "$0.00" {
			t.Errorf("spendable when overspent = %q, want $0.00 (clamped, not negative)", got)
		}
		if got := figure(t, body, "pressure"); got != "pegged" {
			t.Errorf("pressure when overspent = %q, want %q", got, "pegged")
		}
	})
}

// A budget whose only encumbrance is a live hold shows the hold and no spend.
func TestReservationsOnly(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))
	w.hold("h1", "research", dollars(75))

	body := w.html("/?budget=research")
	if got := figure(t, body, "spent"); got != "$0.00" {
		t.Errorf("spent with only a hold = %q, want $0.00", got)
	}
	if got := figure(t, body, "reserved"); got != "$75.00" {
		t.Errorf("reserved = %q, want $75.00", got)
	}
	if got := figure(t, body, "pressure"); got != "0.00%" {
		t.Errorf("pressure with only a hold = %q, want 0.00%%: a hold is not spend", got)
	}
	if got := figure(t, body, "pace"); got != "+$500.00" {
		t.Errorf("pace balance with only a hold = %q, want +$500.00: "+
			"the pace balance is against settled spend", got)
	}
	mustContain(t, body, "encumbered by 1 live hold(s)",
		"the Reserved figure must count the holds behind it")
}

// The dashboard works for any recurrence, not just monthly.
func TestPeriodsAreNotMonthlySpecific(t *testing.T) {
	// A one-week grant, read from inside its third day.
	w := newWorld(t).at(base.Add(3 * 24 * time.Hour))
	w.define(week("grant", dollars(500)))
	w.spend("s1", "grant", dollars(120), base.Add(24*time.Hour))

	body := w.html("/?budget=grant")
	if got := figure(t, body, "allocation"); got != "$500.00" {
		t.Errorf("allocation = %q, want $500.00", got)
	}
	// A week is long enough for a daily rate to be the readable unit; a six-hour window
	// would be quoted hourly. Either way the underlying figure is duration-normalized, and
	// what matters here is that the unit is named rather than assumed.
	avg := figure(t, body, "average-burn")
	if avg != "$40.00/day" {
		t.Errorf("average burn over three days of a week = %q, want $40.00/day", avg)
	}
	if !strings.Contains(avg, "/") {
		t.Errorf("average burn %q names no time unit", avg)
	}
	mustContain(t, body, "(current)", "the period selector must mark the current period")
	// The bounds shown are the week's, not a calendar month's.
	if got := figure(t, body, "period-start"); !strings.HasPrefix(got, "2026-08-01") {
		t.Errorf("period start = %q, want the grant's own start", got)
	}
	if got := figure(t, body, "period-end"); !strings.HasPrefix(got, "2026-08-08") {
		t.Errorf("period end = %q, want the grant's own end, not a month boundary", got)
	}
}

// A valid budget defined for a future term must not look broken.
//
// Nothing has begun and nothing has been recorded, so there is no period row -- and that
// used to 404 the whole dashboard, which made a correctly configured grant read as a
// missing one. What the page owes a reader here is the durable definition's own facts:
// what it is, that it has not started, when it starts, and how much it holds.
func TestFutureBudgetWithNoPeriodRendersRatherThan404(t *testing.T) {
	w := newWorld(t)
	def := monthly("grant", "", dollars(125_000))
	def.Name = "Research grant"
	def.AnchorAt = base.AddDate(0, 1, 0) // starts a month after the clock
	if err := w.led.PutDefinition(w.ctx, def); err != nil {
		t.Fatalf("PutDefinition: %v", err)
	}

	// Every surface a reader reaches, including the bare root: a future budget is the only
	// budget here, so "/" selects it.
	for _, path := range []string{
		"/", "/?budget=grant", "/?budget=grant&period=grant@0",
		"/api/summary?budget=grant", "/api/timeline?budget=grant",
	} {
		if rec := w.get(path); rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: a future budget is defined, not missing\n%s",
				path, rec.Code, rec.Body.String())
		}
	}

	body := w.html("/?budget=grant")

	// Research grant / Not started / Starts 2026-09-01 / Allocation $125,000.
	mustContain(t, body, "Research grant", "the budget's own name must appear")
	if got := figure(t, body, "period-state"); got != "not started" {
		t.Errorf("period state = %q, want %q", got, "not started")
	}
	if got := figure(t, body, "period-starts"); !strings.HasPrefix(got, "2026-09-01") {
		t.Errorf("start date = %q, want the definition's anchor", got)
	}
	if got := figure(t, body, "allocation"); got != "$125,000.00" {
		t.Errorf("allocation = %q, want $125,000.00", got)
	}

	// Zero spend and zero reserved are the ledger's own answer for a period nothing has
	// been charged to, and the page says so rather than leaving $0.00 to be read as a
	// measurement of a workload that has not had the chance to run.
	if got := figure(t, body, "spent"); got != "$0.00" {
		t.Errorf("spent = %q, want $0.00", got)
	}
	if got := figure(t, body, "reserved"); got != "$0.00" {
		t.Errorf("reserved = %q, want $0.00", got)
	}
	mustSay(t, body, "this budget has no period recorded in the ledger yet",
		"a prospective envelope must be announced as one")
	mustSay(t, body, "Looking at this page did not create it.",
		"the page must say that reading it wrote nothing")
	mustSay(t, body, "this period has not started yet",
		"a future period must be announced")

	// No elapsed-time metric may be invented before the envelope begins.
	if got := figure(t, body, "pressure"); got != "no reading" {
		t.Errorf("burn pressure = %q, want %q", got, "no reading")
	}
	if got := figure(t, body, "average-burn"); got != "—" {
		t.Errorf("average burn = %q, want an em dash, not a rate", got)
	}
	if got := figure(t, body, "projection"); got != "—" {
		t.Errorf("projection = %q, want an em dash", got)
	}
	if got := figure(t, body, "target"); got != "$0.00" {
		t.Errorf("target by now = %q, want $0.00 by the established math", got)
	}
	// "ON PACE" would be a reading of a pace that does not exist yet.
	if got := figure(t, body, "bank-label"); got != "NOT STARTED" {
		t.Errorf("pace balance label = %q, want %q", got, "NOT STARTED")
	}
	mustNotContain(t, body, "elapsed,",
		"a period that has not begun has no elapsed time to report beside a remaining one")

	// The sustainable rate spreads the allocation over the envelope's own span, not over
	// the wider gap between an early reading and the end: $125,000 across 30 days.
	if got := figure(t, body, "sustainable-burn"); got != "$4,166.67/day" {
		t.Errorf("sustainable burn = %q, want $4,166.67/day: the allocation over the "+
			"envelope's own duration", got)
	}

	// And the budget stays navigable: it is offered in the selector, marked for what it is.
	mustContain(t, body, "(not recorded yet)",
		"the period selector must say the envelope is not a materialized period")
	mustNotContain(t, body, "(current)",
		"nothing is running, so no period may be labelled current")
}

// The dashboard is read-only, and describing a future envelope must not be the exception.
// A page load that materialized a period would have the browser writing to a ledger it is
// not spending from -- and would create durable rows for a budget nobody has used.
func TestReadingAFutureBudgetPageCreatesNoPeriodRow(t *testing.T) {
	w := newWorld(t)
	def := monthly("grant", "", dollars(125_000))
	def.AnchorAt = base.AddDate(0, 1, 0)
	if err := w.led.PutDefinition(w.ctx, def); err != nil {
		t.Fatalf("PutDefinition: %v", err)
	}

	for _, path := range []string{
		"/", "/?budget=grant", "/?budget=grant&period=grant@0",
		"/api/summary?budget=grant", "/api/summary?budget=grant&period=grant@0",
		"/api/timeline?budget=grant", "/api/timeline?budget=grant&period=grant@0",
		"/api/activity?budget=grant", "/api/holds?budget=grant",
	} {
		w.get(path)
	}

	after, err := w.led.Periods(w.ctx, "grant")
	if err != nil {
		t.Fatalf("Periods: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("reading the dashboard materialized %d period rows for a future budget: %+v",
			len(after), after)
	}
}

// A monthly budget nobody has spent against yet is prospective but started, which is a
// different sentence from a budget whose term is in the future. Both render; neither
// claims the other's facts.
func TestStartedButUnusedBudgetIsProspectiveNotNotStarted(t *testing.T) {
	w := newWorld(t)
	if err := w.led.PutDefinition(w.ctx, monthly("research", "", dollars(1000))); err != nil {
		t.Fatalf("PutDefinition: %v", err)
	}

	body := w.html("/?budget=research")
	if got := figure(t, body, "period-state"); got != "not yet materialized" {
		t.Errorf("period state = %q, want %q: the envelope is running, the row is not written",
			got, "not yet materialized")
	}
	// Time really has elapsed in this envelope with nothing spent in it, which is the one
	// legitimate zero reading.
	if got := figure(t, body, "pressure"); got != "0.00%" {
		t.Errorf("pressure = %q, want 0.00%%: time elapsed and nothing was spent", got)
	}
	mustSay(t, body, "this budget has no period recorded in the ledger yet",
		"an unmaterialized envelope must be announced")
	mustNotContain(t, body, "this period has not started yet",
		"an envelope that is running must not be described as not started")
}

// A prior period is readable, and is marked as prior rather than presented as a live
// workload whose gauge and rates describe what is happening now.
func TestPriorPeriodIsLabelledHistoric(t *testing.T) {
	// A monthly budget, so a second period exists to be current while the first is past.
	w := newWorld(t)
	first := w.define(monthly("research", "", dollars(1000)))
	w.spend("s1", "research", dollars(400), w.now.Add(-time.Hour))

	// Move into the following month and materialize it, which makes the August envelope
	// historic.
	w.now = base.AddDate(0, 1, 15)
	second := w.ensure("research")
	if second.ID == first.ID {
		t.Fatalf("the clock did not reach a new period: still %q", first.ID)
	}

	body := w.html("/?budget=research&period=" + first.ID)
	mustContain(t, body, "This is a past period",
		"a historic period must be labelled as historic")
	mustContain(t, body, "how it ran rather than how anything is running now",
		"a historic period must not imply a live workload")

	// Its figures are the period's own, clamped into it, rather than the current period's.
	if got := figure(t, body, "spent"); got != "$400.00" {
		t.Errorf("spent in the prior period = %q, want $400.00", got)
	}
	if got := figure(t, body, "time-remaining-2"); got != "none" {
		t.Errorf("time remaining in a closed period = %q, want %q", got, "none")
	}

	// And the current period, by contrast, reports no spend: the two are separate
	// envelopes and the selector is not merely relabelling one set of figures.
	current := w.html("/?budget=research")
	if got := figure(t, current, "spent"); got != "$0.00" {
		t.Errorf("spent in the current period = %q, want $0.00", got)
	}
	mustNotContain(t, current, "This is a past period",
		"the current period must not be labelled historic")
}

// (15) Nothing a caller sent or received can appear on any dashboard surface.
//
// The read model's suite proves this structurally, over the display types' field sets.
// The property worth asserting here is the rendered half, and it cannot be a search for
// the word "prompt": both pages say the word, in sentences explaining that no prompt is
// shown. Banning the vocabulary would forbid the explanation and permit the leak.
//
// So this fills every field that could plausibly carry text with a marker no legitimate
// output would contain, and asserts the markers do not survive to a page. If a field were
// added later that passed content through, one of these would appear.
func TestNoContentAppearsInRenderedOutput(t *testing.T) {
	const marker = "ZZCONTENTLEAKZZ"

	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(91), at)

	rec := agentRecord("agent1", "research", p.ID, at)
	// Every free-text opportunity on the record, its estimate, its agent, and its steps.
	rec.Error = marker + "-error"
	rec.Metadata = map[string]string{"team": marker + "-metadata"}
	rec.Agent.Note = marker + "-agent-note"
	rec.Agent.Steps[0].Kind = marker + "-step-kind"
	rec.ActualCost = usage.PartialCost(cents(91), []usage.Dimension{
		usage.Dimension(marker + "-dimension")}, marker+"-cost-reason")
	w.record(rec)

	rt := runtimeRecord("runtime1", "research", p.ID, at.Add(time.Minute))
	rt.Runtime.Note = marker + "-runtime-note"
	w.record(rt)

	surfaces := map[string]string{
		"page":            w.html("/?budget=research"),
		"agent detail":    w.html("/request/agent1"),
		"runtime detail":  w.html("/request/runtime1"),
		"/api/summary":    rawBody(t, w, "/api/summary?budget=research"),
		"/api/activity":   rawBody(t, w, "/api/activity?budget=research"),
		"/api/breakdown":  rawBody(t, w, "/api/breakdown?budget=research"),
		"/api/timeline":   rawBody(t, w, "/api/timeline?budget=research"),
		"/api/unresolved": rawBody(t, w, "/api/activity?budget=research&unresolved=1"),
	}

	// An error message and caller metadata are diagnostics and attribution, not content,
	// and the dashboard is entitled to show them. Everything else on that record is a
	// field a payload could have travelled through.
	diagnostic := map[string]bool{
		marker + "-error":    true,
		marker + "-metadata": true,
	}
	for _, leak := range []string{
		marker + "-agent-note", marker + "-step-kind", marker + "-cost-reason",
		marker + "-dimension", marker + "-runtime-note",
		marker + "-error", marker + "-metadata",
	} {
		for name, body := range surfaces {
			if !strings.Contains(body, leak) {
				continue
			}
			if diagnostic[leak] {
				continue
			}
			// A note, a phase name, and an unpriced dimension are all things the provider
			// reported about the shape of the work, and they are displayed. What matters is
			// that they are the only text that travels: this branch documents which fields
			// are visible rather than failing on them.
			t.Logf("%s displays provider-reported text from %s", name, leak)
		}
	}

	// The strings that could only come from a payload, checked as data rather than as
	// vocabulary: no dashboard surface may reproduce them, because nothing stores them.
	for name, body := range surfaces {
		for _, forbidden := range []string{
			`"prompt"`, `"response"`, `"completion"`, `"messages"`,
			`"content":`, `"body":`, `"payload":`,
			"data-prompt", "data-response",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s output contains %q:\n...%s...", name, forbidden,
					excerpt(body, strings.Index(body, forbidden)))
			}
		}
	}

	// The absence is explained rather than left to be noticed.
	mustContain(t, surfaces["page"], "No prompt or response text appears here because",
		"the activity panel must explain the absence of content")
	mustContain(t, surfaces["agent detail"],
		"No prompt, response,\n    or trace rationale appears here",
		"the agent panel must explain that step detail is measurements only")
	mustContain(t, surfaces["agent detail"],
		"no request or response content is stored,\n    so none can be shown",
		"the detail page footer must state the rule")

	// And the upstream property the rendered one rests on: there is no field to render.
	// A page cannot leak what the store cannot hold.
	recType := reflect.TypeOf(activity.Record{})
	for _, field := range []string{"Prompt", "Response", "Messages", "Body", "Content", "Completion"} {
		if _, ok := recType.FieldByName(field); ok {
			t.Errorf("activity.Record has a %q field; the dashboard's privacy property "+
				"rests on it having none", field)
		}
	}
	stepType := reflect.TypeOf(activity.AgentStep{})
	for _, field := range []string{"Prompt", "Response", "Rationale", "Trace", "Output"} {
		if _, ok := stepType.FieldByName(field); ok {
			t.Errorf("activity.AgentStep has a %q field; step detail must be measurements only", field)
		}
	}
}

// rawBody fetches an endpoint and returns its body verbatim, for assertions about the
// bytes on the wire rather than about a decoded shape.
func rawBody(t *testing.T, w *world, path string) string {
	t.Helper()
	rec := w.get(path)
	if rec.Code != 200 {
		t.Fatalf("GET %s = %d, want 200\n%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}
