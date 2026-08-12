package dashboard

import (
	"testing"
	"time"

	"throttle/activity"
	"throttle/usage"
)

// The detail page is where a compound transaction has to be legible without becoming a
// second set of books. One governed request, one reservation, one charge; the steps and
// the runtime telemetry beneath it are accounting detail, and the page has to say so.

// (12) An agent turn renders its internal steps as measurements, and the request-level
// charge stays the authoritative figure.
func TestAgentStepsRenderAsAccountingDetail(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(91), at)
	w.record(agentRecord("agent1", "research", p.ID, at))

	body := w.html("/request/agent1")

	// The three steps are there, in order, with their phases.
	phases := cellsUnder(t, body, "Phase")
	if len(phases) != 3 {
		t.Fatalf("the step table has %d rows, want 3: %v", len(phases), phases)
	}
	for i, want := range []string{"preprocessing", "orchestration", "postprocessing"} {
		if phases[i] != want {
			t.Errorf("step %d phase = %q, want %q", i+1, phases[i], want)
		}
	}

	// Each step carries its own cost.
	costs := cellsUnder(t, body, "Cost")
	if len(costs) != 3 {
		t.Fatalf("the step table has %d cost cells, want 3: %v", len(costs), costs)
	}
	for i, want := range []string{"$0.1000", "$0.5500", "$0.2500"} {
		if costs[i] != want {
			t.Errorf("step %d cost = %q, want %q", i+1, costs[i], want)
		}
	}

	// The column states its own sum rather than leaving a reader to add three figures and
	// wonder why the answer is not the charge.
	mustContain(t, body, "steps shown sum to", "the step column must state its own sum")
	mustContain(t, body, "$0.9000", "the step sum must be printed")

	// The request charge is a separate figure, labelled as the authoritative one.
	mustContain(t, body, "Request charge", "the request-level charge must be named")
	mustContain(t, body, "the authoritative figure",
		"the request charge must be identified as authoritative")
	mustContain(t, body, "$0.9100", "the request-level charge must be shown")

	// A step whose collaborator the provider named shows it; one it did not shows an
	// absence rather than a blank.
	collabs := cellsUnder(t, body, "Collaborator")
	if len(collabs) != 3 || collabs[1] != "researcher" {
		t.Errorf("Collaborator column = %v, want the named collaborator on step 2", collabs)
	} else if collabs[0] != "—" {
		t.Errorf("a step with no collaborator renders %q, want an em dash", collabs[0])
	}

	// A step whose model the provider never named renders as an explicit absence. Filling
	// it in from the sibling steps would be a fabricated attribution.
	models := cellsUnder(t, body, "Model")
	if len(models) != 3 {
		t.Fatalf("Model column has %d cells, want 3: %v", len(models), models)
	}
	if models[0] != "claude-sonnet-4" {
		t.Errorf("step 1 model = %q, want the canonical name", models[0])
	}
	if models[2] != "not reported" {
		t.Errorf("a step with no model identity renders %q, want %q", models[2], "not reported")
	}

	// Non-model activity is counted, and the counts are explicitly not costs.
	mustContain(t, body, "tool-invocation", "non-model activity must be listed by kind")
	mustContain(t, body, "knowledge-base-lookup", "every reported activity kind must appear")
	mustContain(t, body, "These are counts, not costs",
		"activity counts must not be mistaken for charges")

	// One transaction, and the page says which level throttle governed.
	mustContain(t, body, "throttle admitted the turn, not the individual model calls inside it",
		"the page must state which level was governed")

	// The session groups records; it does not scope money.
	mustContain(t, body, "It groups records; it does not scope money.",
		"a session must not be presented as a budget scope")

	// And the whole point of the step table: measurements, no content.
	mustContain(t, body, "No prompt, response,",
		"the page must say why no content appears rather than leaving its absence unexplained")

	// From the list, the compound shape is visible without opening the request.
	list := w.html("/?budget=research")
	mustContain(t, list, "agent · 3 steps",
		"a compound request must be flagged as one in the activity table")
}

// A gap between the step column and the request charge is explained when it is visible,
// absent when it is not, and attributed to rounding only when rounding could have caused
// it.
//
// The last is the one that matters. Rounding four-decimal figures can shift a total by
// hundredths of a cent; describing a two-dollar discrepancy that way would be the display
// explaining away a disagreement between the provider's own numbers, which is precisely
// what the step table exists not to do.
func TestAgentStepGapIsExplainedHonestlyOrNotAtAll(t *testing.T) {
	at := base.Add(monthDuration/2 - time.Hour)

	// The fixture's steps sum to $0.90 against a $0.91 charge: a whole cent, which is a
	// hundred times more than rounding three steps and a total can produce.
	material := newWorld(t)
	p := material.define(monthly("research", "", dollars(1000)))
	material.spend("s1", "research", cents(91), at)
	material.record(agentRecord("agent1", "research", p.ID, at))

	shown := material.html("/request/agent1")
	mustContain(t, shown, "a difference of $0.0100 that display rounding cannot account for",
		"a gap larger than rounding must be named as one")
	mustContain(t, shown, "The provider's per-step figures do not add up to what it charged",
		"a material gap must say the provider's figures disagree")
	mustContain(t, shown, "the authoritative figure",
		"a material gap must still name which figure was billed")
	mustNotContain(t, shown, "per-step rounding need not add up",
		"a whole cent must not be attributed to display rounding")
	mustContain(t, shown, `class="notice warn"`,
		"a material gap is a warning, not a footnote")

	// The gap rounding really can produce: within the last displayed place. Invisible in
	// the column, so no note at all.
	rounding := newWorld(t)
	q := rounding.define(monthly("research", "", dollars(1000)))
	near := agentRecord("agent2", "research", q.ID, at)
	near.ActualCost = usage.KnownCost(cents(90) + 120)
	near.Reserved = cents(91)
	rounding.spend("s1", "research", cents(90)+120, at)
	rounding.record(near)

	quiet := rounding.html("/request/agent2")
	mustNotContain(t, quiet, "against a request charge of",
		"a gap inside the last displayed place needs no note")
	mustNotContain(t, quiet, "cannot account for",
		"a rounding-scale gap is not a disagreement")

	// And steps that add up exactly carry no note either. A note that always appeared
	// would train a reader to ignore it.
	exact := newWorld(t)
	r := exact.define(monthly("research", "", dollars(1000)))
	rec := agentRecord("agent3", "research", r.ID, at)
	rec.ActualCost = usage.KnownCost(cents(90))
	rec.Reserved = cents(90)
	rec.Estimate.Cost = usage.KnownCost(cents(90))
	exact.spend("s1", "research", cents(90), at)
	exact.record(rec)

	clean := exact.html("/request/agent3")
	mustNotContain(t, clean, "against a request charge of",
		"steps that sum to the charge must not carry a discrepancy note")
	mustContain(t, clean, "$0.9000", "the charge and the sum are both $0.9000 here")
}

// (13) A hosted runtime's own cost is not knowable at the time of the call, and the page
// says so instead of showing a zero or dividing a session charge across invocations.
func TestHostedRuntimeCostReadsHonestly(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.record(runtimeRecord("rt1", "research", p.ID, at))

	body := w.html("/request/rt1")
	rt := panel(t, body, "runtime")

	mustContain(t, rt, "Hosted runtime", "a runtime invocation must be identified as one")
	mustContain(t, rt, "arn:aws:bedrock-agentcore", "the runtime identity must be shown")
	mustContain(t, rt, "runtime-session-77", "the session identity must be shown")

	// The reservation is headroom, and is named as such rather than as an estimate or as a
	// limit the platform enforces.
	mustContain(t, rt, "Max exposure held", "the reservation must be labelled as exposure")
	mustContain(t, rt, "$2.00", "the held exposure must be shown")
	mustContain(t, rt, "headroom, not a cost, not an estimate, and not a limit the platform enforces",
		"the exposure must state what it is not")

	// The runtime cost reads as unknown, and no figure in this panel is a zero: a $0.00
	// runtime cost would be a claim that a hosted agent ran for free.
	mustContain(t, rt, "Runtime cost", "the runtime cost must be a named figure")
	mustContain(t, rt, "unresolved", "an unobserved runtime cost must read as unresolved")
	mustContain(t, rt, "not yet observed",
		"an unobserved runtime cost must say it has not been observed")
	mustNotContain(t, rt, "$0.00", "an unobserved runtime cost must never render as zero")

	// And the accounting position: a session charge is not divided across invocations.
	mustContain(t, rt, "it is not divided across the invocations that shared the session",
		"the page must refuse to attribute session cost to one invocation")
	mustContain(t, rt, "this call's own runtime cost is unknown rather than estimated",
		"the page must say the cost is unknown rather than estimated")
	mustContain(t, rt, "session-level resource usage is not divisible across invocations",
		"the provider's own note must be carried through")

	// Sizes are measurements; the payloads they measure are not stored.
	mustContain(t, rt, "sizes, not payloads", "byte counts must be distinguished from payloads")
	mustContain(t, rt, "812 bytes sent / 2,044 received", "the byte counts must be shown")

	// A failing call inside a hosted agent arrives as a successful HTTP response with a
	// failing status, so the status code is evidence rather than decoration.
	mustContain(t, rt, "HTTP status", "the runtime's status code must be shown")
	mustContain(t, rt, "this number is the only evidence",
		"the status code must be explained as the only evidence of an internal failure")

	// The unresolved cost is visible from the list as well as from the detail page, so it
	// is not something an operator has to already know to look for.
	list := w.html("/?budget=research")
	mustContain(t, list, "aws-bedrock-agentcore",
		"a runtime invocation must appear in the activity list")
	mustContain(t, list, "hosted runtime", "a runtime invocation must be flagged as one")
	mustContain(t, list, "awaiting external usage",
		"an unresolved runtime cost must be counted in the bookkeeping panel")
}

// (14) The reconciler's typed reasons are surfaced verbatim, in its own vocabulary.
func TestReconciliationReasonsAreVisible(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(12), at)
	w.record(repairedRecord("r1", "research", p.ID, at))

	// On the detail page, the whole trail entry.
	body := w.html("/request/r1")
	mustContain(t, body, "Reconciliation trail", "the trail must be a named section")
	mustContain(t, body, "crash-repairable",
		"the reconciler's typed reason must appear verbatim rather than paraphrased")
	mustContain(t, body, "aws-price-list 2026-08-01",
		"the immutable quote a replayed settlement priced from must be shown")
	mustContain(t, body, "the ledger held a settled charge the activity record never learned about",
		"the reconciler's own detail must be carried through")
	mustSay(t, body, "A repaired record is not a problem: it is evidence a crash was cleaned up.",
		"a repair must be explained as evidence rather than as a fault")
	mustContain(t, body, "Append-only", "the trail must say it is append-only")

	// The states the reconciler moved between are both named, in its vocabulary.
	for _, col := range []struct{ header, want string }{
		{"Class", "repaired"},
		{"Reason", "crash-repairable"},
		{"Observed", "pending · pending"},
		{"Produced", "settled"},
		{"Money", "settled $0.1200"},
	} {
		got := cellsUnder(t, body, col.header)
		if len(got) != 1 || got[0] != col.want {
			t.Errorf("reconciliation %s column = %v, want [%q]", col.header, got, col.want)
		}
	}

	// And on the dashboard, counted rather than buried. This budget has nothing
	// outstanding, so a repair reported only alongside outstanding work would be invisible
	// on exactly the dashboard where it is the one thing worth knowing.
	page := w.html("/?budget=research")
	book := panel(t, page, "recon")
	mustSay(t, book, "record(s) already repaired by reconciliation",
		"repairs must be counted in the bookkeeping panel")
	mustSay(t, book, "<strong>1</strong> record(s)",
		"the number of repaired records must be shown")
	mustContain(t, book, "Nothing outstanding.",
		"a repaired record is not outstanding work")

	// The panel is read-only, and says so. This is the one panel whose subject is money
	// moving, so the absence of a button is not sufficient: it has to be stated.
	mustContain(t, book, "this panel is read-only: opening it runs no repair",
		"the bookkeeping panel must state that viewing it repairs nothing")
	mustContain(t, book, "throttle reconcile",
		"the panel must name the command that does move money")
}

// The unresolved states are counted separately, because they imply different actions: an
// unpriced request needs a catalog entry, an unknown outcome needs investigation, and an
// out-of-band cost needs waiting.
func TestUnresolvedStatesAreCountedSeparately(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	w.record(unknownCostRecord("liability", "research", p.ID, at))
	w.record(runtimeRecord("runtime", "research", p.ID, at.Add(time.Minute)))
	w.record(partialCostRecord("partial", "research", p.ID, at.Add(2*time.Minute)))

	outstanding := settledRecord("unknown-outcome", "research", p.ID, 0, at.Add(3*time.Minute))
	outstanding.Status = activity.StatusOutstanding
	outstanding.Outcome = activity.OutcomeCancelled
	outstanding.ActualCost = usage.UnknownCost("the call was interrupted before a response came back")
	outstanding.Reserved = dollars(1)
	w.record(outstanding)

	book := panel(t, w.html("/?budget=research"), "recon")

	for _, want := range []string{
		"unresolved liability",
		"outcome unknown",
		"awaiting external usage",
	} {
		mustContain(t, book, want, "each unresolved state needs its own count and its own words")
	}
	mustNotContain(t, book, "Nothing outstanding.",
		"a budget with unresolved liabilities is not clean")

	// The money those rows are holding is totalled, because that is the figure that says
	// how much of the envelope is waiting on bookkeeping.
	mustContain(t, book, "Still encumbered:",
		"the panel must total the money the unresolved rows are holding")
	mustContain(t, book, "$4.00",
		"the encumbered total is the three unresolved records' held headroom")

	// The rows behind the counts are reachable.
	mustContain(t, book, "outstanding request(s)",
		"the panel should offer the rows behind the counts")

	// The missing catalog entries are named, because adding one is the action the counts
	// imply. A count alone says something is wrong without saying what to do.
	mustContain(t, book, "The pricing catalog is missing",
		"an unpriced dimension should be named rather than merely counted")
	mustContain(t, book, "widget_calls", "the dimension that blocked a price must be named")

	// A clean budget says so rather than showing an empty panel.
	clean := newWorld(t)
	clean.define(monthly("tidy", "", dollars(100)))
	cleanBook := panel(t, clean.html("/?budget=tidy"), "recon")
	mustContain(t, cleanBook, "Nothing outstanding.", "clean bookkeeping must be stated")
	mustContain(t, cleanBook, "Every request in this period has a settled cost.",
		"clean bookkeeping must say what it means")
	mustNotContain(t, cleanBook, "Still encumbered:",
		"a clean budget has no encumbered total to report")
}

// An expired lease is not evidence of zero spend, and the dashboard must not present it as
// resolved. The money may already have been spent; the record is simply not settled yet.
func TestExpiredHoldIsFlaggedRatherThanForgotten(t *testing.T) {
	w := newWorld(t)
	w.define(monthly("research", "", dollars(1000)))
	w.expiredHold("stale", "research", dollars(30))

	body := w.html("/?budget=research")

	// In the reservations table, on the row.
	holds := panel(t, body, "holds")
	mustContain(t, holds, "lapsed", "an expired hold must be flagged in the reservations table")
	mustContain(t, holds, "An expired hold is not evidence of zero spend",
		"a lapsed lease must not be presented as a resolved request")

	// And as a notice, because a row in a table three panels down is not an announcement.
	mustContain(t, body, "expired leases and have not been recovered",
		"expired holds must be announced rather than left in a table")
	mustContain(t, body, "throttle recover", "the page must name the command that resolves them")

	// Its headroom is already out of Reserved, and the page says so rather than leaving a
	// reader to wonder why $30 of holds does not appear in the Reserved figure.
	mustContain(t, body, "Their headroom is already excluded from Reserved",
		"the relationship between an expired hold and the Reserved figure must be stated")
	if got := figure(t, body, "reserved"); got != "$0.00" {
		t.Errorf("Reserved = %q with only an expired hold outstanding, want $0.00", got)
	}

	// The bookkeeping panel counts it, with the amount, so recovery has a visible size.
	book := panel(t, body, "recon")
	mustContain(t, book, "expired hold(s) holding $30.00",
		"the expired headroom must be counted with its amount")
}
