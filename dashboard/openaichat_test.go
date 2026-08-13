package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/usage"
)

// These tests ask the display half of #28: can one page show both of OpenAI's API families,
// tell them apart, and render an audio request honestly -- with no new column, no
// provider-specific branch, and no import of the adapter or its SDK?
//
// The interesting pressure is different from openai_test.go's. There the question was
// whether a second provider fitted the abstraction. Here both records come from the same
// provider and differ in one field, which is where a UI is most tempted to grow an "API"
// column or to special-case a formatter. It must not: the operation column already exists
// and already carries an opaque provider-call string.

// chatIdentity is what the Chat Completions adapter records -- identical to the Responses
// identity in every field but the operation, which is what makes the operation the thing
// under test rather than a coincidence of the fixture.
func chatIdentity() usage.ModelIdentity {
	id := openAIIdentity()
	id.Operation = "chat-completions"
	return id
}

// chatRecord is an ordinary settled Chat Completions request.
func chatRecord(requestID, budgetID, periodID string, cost money.Money, at time.Time) activity.Record {
	rec := openAIRecord(requestID, budgetID, periodID, cost, at)
	rec.Identity = chatIdentity()
	rec.Estimate.Identity = rec.Identity
	return rec
}

// audioChatRecord is a Chat Completions request that carried audio the catalog has no rates
// for: four disjoint token dimensions, a partial cost whose amount is the text arithmetic,
// and the two audio dimensions named as unpriced.
func audioChatRecord(requestID, budgetID, periodID string, floor money.Money, at time.Time) activity.Record {
	rec := chatRecord(requestID, budgetID, periodID, floor, at)
	rec.ActualUsage = usage.New(map[usage.Dimension]int64{
		usage.InputTokens:       200,
		usage.InputAudioTokens:  800,
		usage.OutputTokens:      100,
		usage.OutputAudioTokens: 400,
	})
	rec.ActualCost = usage.PartialCost(floor,
		[]usage.Dimension{usage.InputAudioTokens, usage.OutputAudioTokens},
		"no captured audio-token rates for gpt-5.1 on openai, so the text cost is a floor")
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeUnpriced
	rec.Reserved = dollars(1)
	return rec
}

// Both OpenAI API families render on one page, distinguished by the Operation column that
// already existed, with the column count unchanged.
//
// The column count is asserted because that is the failure this test exists to catch: a
// display that had to grow a cell to tell Chat Completions from Responses would be evidence
// that the neutral record needed a field it does not have.
func TestBothOpenAIFamiliesRenderInTheOperationColumn(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	// The column count as it stands with a single family on the page, measured rather than
	// hard-coded, so this test cannot be satisfied by a stale number.
	w.spend("s1", "research", cents(6), at)
	w.record(openAIRecord("resp1", "research", p.ID, cents(6), at))
	before := len(headersOf(t, panel(t, w.html("/?budget=research"), "activity")))
	if before == 0 {
		t.Fatal("the activity table has no header row; this test cannot measure anything")
	}

	w.spend("s2", "research", cents(3), at.Add(time.Minute))
	w.spend("s3", "research", cents(11), at.Add(2*time.Minute))
	stream := openAIRecord("stream1", "research", p.ID, cents(3), at.Add(time.Minute))
	stream.Identity.Operation = "responses-stream"
	stream.Estimate.Identity = stream.Identity
	w.record(stream)
	w.record(chatRecord("chat1", "research", p.ID, cents(11), at.Add(2*time.Minute)))

	body := w.html("/?budget=research")
	acts := panel(t, body, "activity")

	if after := len(headersOf(t, acts)); after != before {
		t.Errorf("the activity table grew from %d columns to %d when a second OpenAI API family "+
			"appeared: the operation column already carries the distinction", before, after)
	}

	// Three rows, three operations, newest first.
	ops := cellsUnder(t, acts, "Operation")
	want := []string{"chat-completions", "responses-stream", "responses"}
	if len(ops) != 3 {
		t.Fatalf("the activity table has %d rows, want 3: %v", len(ops), ops)
	}
	for i, w := range want {
		if ops[i] != w {
			t.Errorf("Operation row %d = %q, want %q", i, ops[i], w)
		}
	}

	// And the identity columns still say one provider, one publisher, one model for all
	// three, because two API families of one provider are still one provider.
	for _, col := range []string{"Access provider", "Publisher", "Model"} {
		cells := cellsUnder(t, acts, col)
		if len(cells) != 3 {
			t.Fatalf("%s has %d cells, want 3: %v", col, len(cells), cells)
		}
		if cells[0] != cells[1] || cells[1] != cells[2] {
			t.Errorf("%s = %v, want the same value in all three rows: the API family is not an "+
				"identity fact about the provider, the publisher, or the model", col, cells)
		}
	}

	// The operation breakdown separates them, which is how a reader answers "how much still
	// goes through Chat Completions?" -- the question a migration is measured by.
	bd := breakdownPanel(t, body, "Operation")
	for _, op := range want {
		mustContain(t, bd, op, "the operation breakdown must have a row for "+op)
	}

	// The JSON surface carries the same three, since it is what the poll reads.
	raw := rawBody(t, w, "/api/breakdown?facet=operation&budget=research")
	for _, op := range want {
		if !strings.Contains(raw, op) {
			t.Errorf("/api/breakdown?facet=operation does not carry %q", op)
		}
	}
}

// An audio Chat Completions request with no audio rates renders as a floor, shows its audio
// tokens as tokens, names the two missing rates, and never renders as free.
//
// The display half of Scott's audio decision, and the case where a UI does the most damage
// by being tidy: $0.0013 in a cost column looks like a settled figure, and the whole
// difference between a measurement and a guess is the "+" and the reason behind it.
func TestAudioChatRequestRendersAsAFloorNotAsFree(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	// 200 input at $1.25/M plus 100 output at $10.00/M. Anti-vacuous baseline first: the
	// same amount, settled, renders as a plain figure -- so every assertion below is about
	// completeness and not about a table that cannot print small money.
	const floor = money.Money(1250)
	const wouldHaveRendered = "$0.0013"
	w.spend("s1", "research", floor, at)
	settled := chatRecord("settled", "research", p.ID, floor, at)
	w.record(settled)
	if got := cellsUnder(t, panel(t, w.html("/?budget=research"), "activity"), "Actual"); len(got) != 1 ||
		got[0] != wouldHaveRendered {
		t.Fatalf("the settled baseline renders as %v, want [%s]: this test cannot prove the display "+
			"marks a figure incomplete if it cannot print the figure", got, wouldHaveRendered)
	}

	w.record(audioChatRecord("audio1", "research", p.ID, floor, at.Add(time.Minute)))

	body := w.html("/?budget=research")
	acts := panel(t, body, "activity")

	actuals := cellsUnder(t, acts, "Actual")
	if len(actuals) != 2 {
		t.Fatalf("the activity table has %d rows, want 2: %v", len(actuals), actuals)
	}
	// Newest first, so the audio row is on top.
	if actuals[0] != wouldHaveRendered+"+" {
		t.Errorf("the audio request renders as %q, want %q: the text arithmetic is real and it is "+
			"not the whole bill", actuals[0], wouldHaveRendered+"+")
	}
	if actuals[0] == wouldHaveRendered {
		t.Error("the audio request renders as a complete figure; the audio component was never priced")
	}
	if actuals[0] == "$0.0000" || actuals[0] == "—" {
		t.Errorf("the audio request renders as %q: it consumed 1,200 audio tokens and was not free",
			actuals[0])
	}

	// The audio counts are shown, and shown as audio. A cell reading "1,000 in / 500 out"
	// would be arithmetically true and would make an audio request indistinguishable from a
	// text request costing an order of magnitude less.
	usageCells := cellsUnder(t, acts, "Usage")
	if len(usageCells) != 2 {
		t.Fatalf("the Usage column has %d cells, want 2: %v", len(usageCells), usageCells)
	}
	if !strings.Contains(usageCells[0], "audio") {
		t.Errorf("the audio request's usage cell = %q, should name audio: the same token count "+
			"costs several times more when it is audio", usageCells[0])
	}
	if usageCells[0] == usageCells[1] {
		t.Errorf("the audio request and the text request render the same usage cell %q",
			usageCells[0])
	}

	// The page total inherits the incompleteness rather than absorbing it.
	if spend := figure(t, body, "page-spend"); !strings.HasSuffix(spend, "+") {
		t.Errorf("page spend = %q, want a trailing + to mark a floor", spend)
	}

	// The bookkeeping panel names the two rates an operator has to add. This is the panel's
	// whole purpose: turning an unpriced dimension into a specific piece of work.
	recon := panel(t, body, "recon")
	for _, dim := range []string{"input_audio_tokens", "output_audio_tokens"} {
		mustContain(t, recon, dim,
			"the bookkeeping panel must name the audio rate the catalog is missing")
	}

	// And the detail page shows all four dimensions with the two unpriced ones flagged, so
	// the disjointness that makes the floor valid is auditable rather than asserted.
	detail := w.html("/request/audio1")
	for dim, count := range map[string]string{
		"input_tokens": "200", "input_audio_tokens": "800",
		"output_tokens": "100", "output_audio_tokens": "400",
	} {
		mustContain(t, detail, dim, "the usage table must show the "+dim+" dimension")
		mustContain(t, detail, count, "the usage table must show the recorded count for "+dim)
	}
	mustContain(t, detail, "no price for this dimension",
		"the detail page must flag the dimensions that blocked a full price")
	// Audio here is token-billed, so it must not be badged as a non-token unit: that badge
	// tells a reader the figure needs no per-token rate, which is the rate whose absence
	// made this a floor in the first place.
	if strings.Count(detail, "non-token") != 0 {
		t.Error("an audio token dimension is badged non-token; the provider reported tokens and " +
			"bills per token, at its own rate")
	}
	// Duration never appears anywhere. The provider reported tokens.
	mustNotContain(t, detail, "audio_seconds",
		"a duration figure the provider never reported must not appear")
	mustNotContain(t, detail, "total_tokens",
		"total_tokens is not the billing primitive and is not a stored dimension")
}

// headersOf returns the header cells of the first table in body, so a test can assert that
// a column was not added without hard-coding how many there are.
func headersOf(t *testing.T, body string) []string {
	t.Helper()
	for _, table := range between(body, "<table", "</table>") {
		head, ok := first(table, "<thead", "</thead>")
		if !ok {
			continue
		}
		return stripTags(between(head, "<th", "</th>"))
	}
	t.Fatal("no table in the markup has a header row")
	return nil
}

// breakdownPanel returns the markup of the breakdown panel titled label.
//
// Anchored on the heading rather than on position, because the breakdowns render in facet
// order and a test that took the sixth panel would silently follow a reordering. Bounded by
// the next panel rather than by the first </div>, since each share bar is itself a div.
func breakdownPanel(t *testing.T, body, label string) string {
	t.Helper()
	const marker = `class="panel breakdown"`
	for rest := body; ; {
		i := strings.Index(rest, marker)
		if i < 0 {
			break
		}
		rest = rest[i+len(marker):]
		chunk := rest
		if j := strings.Index(chunk, marker); j >= 0 {
			chunk = chunk[:j]
		} else if j := strings.Index(chunk, "</section>"); j >= 0 {
			chunk = chunk[:j]
		}
		if strings.Contains(chunk, "<h3>"+label+"</h3>") {
			return chunk
		}
	}
	t.Fatalf("no breakdown panel is titled %q", label)
	return ""
}
