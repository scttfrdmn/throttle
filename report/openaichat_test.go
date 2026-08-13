package report

import (
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/usage"
)

// This file asks the read-model half of #28: can reporting tell OpenAI's two API families
// apart, and can it render a Chat Completions request honestly, without learning anything
// about OpenAI or gaining a field?
//
// The same discipline as openai_test.go, one step further. There it was one provider
// against another; here it is two API families of the *same* provider, which is the case
// where a schema is most tempted to grow an "api" or "api_family" column. It must not. The
// operation field already carries the distinction, and it carries it as an opaque
// provider-call string, which is why "converse", "responses", and "chat-completions" can
// coexist in one column without the column meaning anything provider-specific.
//
// Nothing here imports the adapter or openai-go. Every record is built from normalized
// durable facts alone.

// chatIdentity is what the Chat Completions adapter records: the same access provider,
// publisher, and model as the Responses adapter, differing only in the operation.
//
// Deliberately identical in every other field. A test that varied the model too could
// pass while the operation was being ignored, because the model alone would separate the
// rows.
func chatIdentity() usage.ModelIdentity {
	id := openAIIdentity()
	id.Operation = "chat-completions"
	return id
}

// chatRecord is an ordinary settled Chat Completions request.
//
// Its usage is the Chat Completions decomposition: fresh input, cached input, visible
// output, and reasoning, disjoint after normalization. The shape coincides with the
// Responses one because the tokens genuinely are the same tokens -- which is the point of
// #28 and not a shortcut in the fixture.
func chatRecord(requestID, budgetID, periodID string, cost money.Money, at time.Time) activity.Record {
	rec := openAIRecord(requestID, budgetID, periodID, cost, at)
	rec.Identity = chatIdentity()
	rec.Estimate.Identity = rec.Identity
	return rec
}

// audioChatRecord is a Chat Completions request that carried audio, priced as a floor.
//
// This is what the adapter writes when it knows authoritative text rates and no audio
// rates: four disjoint dimensions, a partial cost whose amount is the text arithmetic, and
// the two audio dimensions named as unpriced. The reader's whole task is to not turn that
// into either a total or a zero.
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
		"no captured audio-token rates for gpt-5.1 on openai: input_audio_tokens and "+
			"output_audio_tokens are unpriced, so the text cost is a floor")
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeUnpriced
	rec.Reserved = floor * 4
	return rec
}

// The three OpenAI operations group into three rows of the operation facet, and no other
// facet is disturbed by the distinction.
//
// The load-bearing test of the read-model half. If the operation facet collapsed the
// families, a reader could not answer "how much is still going through Chat Completions?"
// -- which is the question a migration is measured by. And if any other facet split them,
// the adapter would have been writing the API family into a field that does not mean that.
func TestOperationFacetSeparatesOpenAIAPIFamilies(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	// One of each, with distinct amounts so a row cannot pass by coincidence.
	resp := openAIRecord("resp1", "research", p.ID, cents(6), at)
	stream := openAIRecord("stream1", "research", p.ID, cents(3), at.Add(time.Minute))
	stream.Identity.Operation = "responses-stream"
	stream.Estimate.Identity = stream.Identity
	chat := chatRecord("chat1", "research", p.ID, cents(11), at.Add(2*time.Minute))

	w.spend("s1", "research", cents(6), at)
	w.spend("s2", "research", cents(3), at.Add(time.Minute))
	w.spend("s3", "research", cents(11), at.Add(2*time.Minute))
	w.record(resp)
	w.record(stream)
	w.record(chat)

	q := ActivityQuery{BudgetID: "research"}
	b, err := w.rep.Breakdown(w.ctx, FacetOperation, q)
	if err != nil {
		t.Fatalf("Breakdown(operation): %v", err)
	}
	if len(b.Rows) != 3 {
		t.Fatalf("the operation breakdown has %d rows, want 3: %v", len(b.Rows), keys(b.Rows))
	}
	want := map[string]money.Money{
		"responses":        cents(6),
		"responses-stream": cents(3),
		"chat-completions": cents(11),
	}
	for _, row := range b.Rows {
		spend, ok := want[row.Key]
		if !ok {
			t.Errorf("the operation breakdown has an unexpected row %q", row.Key)
			continue
		}
		if row.Spend != spend {
			t.Errorf("operation %q spent %s, want %s", row.Key, row.Spend, spend)
		}
		if row.Requests != 1 {
			t.Errorf("operation %q covers %d requests, want 1", row.Key, row.Requests)
		}
		delete(want, row.Key)
	}
	for key := range want {
		t.Errorf("the operation breakdown has no row for %q: a reader cannot see how much still "+
			"goes through that API", key)
	}

	// And every other identity facet still sees one provider, one publisher, one model.
	// The API family is recorded in exactly one place, which is what keeps it from
	// contaminating the questions the other facets answer.
	for _, facet := range []Facet{FacetAccessProvider, FacetPublisher, FacetModel, FacetProviderModel} {
		got, err := w.rep.Breakdown(w.ctx, facet, q)
		if err != nil {
			t.Fatalf("Breakdown(%s): %v", facet, err)
		}
		if len(got.Rows) != 1 {
			t.Errorf("Breakdown(%s) has %d rows, want 1: two API families of one provider are "+
				"still one provider, one publisher, and one model; rows = %v",
				facet, len(got.Rows), keys(got.Rows))
			continue
		}
		if got.Rows[0].Requests != 3 || got.Rows[0].Spend != cents(20) {
			t.Errorf("Breakdown(%s)[%s] = %s over %d requests, want %s over 3",
				facet, got.Rows[0].Key, got.Rows[0].Spend, got.Rows[0].Requests, cents(20))
		}
	}

	// The facet list is unchanged. A new column here would mean the neutral schema had
	// grown a concept to describe one vendor's product history.
	if len(Facets) != 9 {
		t.Errorf("there are %d facets, want the 9 that existed before Chat Completions: %v",
			len(Facets), Facets)
	}
	for _, f := range Facets {
		// FacetFamily is a model family -- claude-sonnet -- and predates all of this.
		if strings.Contains(string(f), "api") || (f != FacetFamily && strings.Contains(string(f), "family")) {
			t.Errorf("facet %q looks like an API-family field; the operation carries that "+
				"distinction already", f)
		}
	}
}

// An event of each family reports its own operation and is otherwise identical, so a
// reader can tell which API a record came from and nothing else changed.
func TestChatCompletionsEventReportsItsOperation(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)
	w.spend("s1", "research", cents(6), at)
	w.spend("s2", "research", cents(6), at.Add(time.Minute))
	w.record(openAIRecord("resp1", "research", p.ID, cents(6), at))
	w.record(chatRecord("chat1", "research", p.ID, cents(6), at.Add(time.Minute)))

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(page.Events))
	}
	byID := map[string]Event{}
	for _, e := range page.Events {
		byID[e.RequestID] = e
	}
	chat, resp := byID["chat1"], byID["resp1"]

	if chat.Operation != "chat-completions" {
		t.Errorf("Operation = %q, want chat-completions", chat.Operation)
	}
	if resp.Operation != "responses" {
		t.Errorf("Operation = %q, want responses", resp.Operation)
	}
	// Everything else about the two coincides, which is the evidence that the operation is
	// doing the distinguishing rather than something else happening to differ.
	for _, pair := range []struct {
		field      string
		chat, resp string
	}{
		{"AccessProvider", chat.AccessProvider, resp.AccessProvider},
		{"Publisher", chat.Publisher, resp.Publisher},
		{"Model", chat.Model, resp.Model},
		{"ProviderModelID", chat.ProviderModelID, resp.ProviderModelID},
	} {
		if pair.chat != pair.resp {
			t.Errorf("%s differs between the two families: %q vs %q; only the operation should",
				pair.field, pair.chat, pair.resp)
		}
	}
	if chat.Actual.State != CostKnown || chat.Actual.Value != cents(6) {
		t.Errorf("Actual = %v/%s, want a known %s: the same tokens cost the same through either API",
			chat.Actual.State, chat.Actual.Value, cents(6))
	}
}

// A Chat Completions request that carried audio reports a floor, names the two audio
// dimensions as unpriced, and never renders as free.
//
// The reporting half of Scott's audio decision. The record holds a mathematically valid
// text cost and two dimensions with no rate, and reporting has to carry both facts at once:
// the figure is real, and it is not the whole bill.
func TestAudioChatRequestReportsAFloorAndNamesWhatIsMissing(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	// The text floor: 200 input at $1.25/M plus 100 output at $10.00/M.
	floor := micros(1250)

	// Anti-vacuous: the same amount on a settled record renders as a plain figure, so the
	// assertions below are about this cost's completeness and not about a formatter that
	// cannot print money.
	if got := knownAmount(floor).Text(money.Money.String); !strings.HasPrefix(got, "$") {
		t.Fatalf("a known %s renders as %q; this test cannot prove reporting refuses a figure it "+
			"was never able to produce", floor, got)
	}

	w.record(audioChatRecord("audio1", "research", p.ID, floor, at))

	page, err := w.rep.Activity(w.ctx, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	e := page.Events[0]

	// Four dimensions, all reported, all marked as tokens: audio here is token-billed and
	// a "non-token" badge beside a token count would be a false statement about the unit.
	got := map[string]UsageItem{}
	for _, u := range e.Usage {
		got[u.Dimension] = u
	}
	if len(got) != 4 {
		t.Fatalf("got %d usage dimensions, want 4: %v", len(got), e.Usage)
	}
	for dim, want := range map[string]int64{
		"input_tokens": 200, "input_audio_tokens": 800,
		"output_tokens": 100, "output_audio_tokens": 400,
	} {
		it, ok := got[dim]
		if !ok {
			t.Errorf("the reporter dropped the %s dimension", dim)
			continue
		}
		if it.Count != want {
			t.Errorf("%s = %d, want %d", dim, it.Count, want)
		}
		if !it.Token {
			t.Errorf("%s is not marked as a token dimension: this provider reports audio as "+
				"tokens and bills per token, at its own rate", dim)
		}
	}
	// The two dimensions with no rate are flagged, and the two with rates are not. That
	// distinction is what tells an operator which two rows to add to the catalog.
	for dim, blocked := range map[string]bool{
		"input_audio_tokens": true, "output_audio_tokens": true,
		"input_tokens": false, "output_tokens": false,
	} {
		if got[dim].Unpriced != blocked {
			t.Errorf("%s unpriced = %v, want %v", dim, got[dim].Unpriced, blocked)
		}
	}
	// Duration never appears. The provider reported tokens, and a seconds figure would be
	// an invention.
	if _, ok := got["audio_seconds"]; ok {
		t.Error("the reporter surfaced audio_seconds, which the provider never reported")
	}

	// The cost is a floor: an amount that is real, marked as incomplete, and never zero.
	if !e.Actual.Floor() {
		t.Errorf("Actual.State = %q, want a floor: the text portion priced and the audio did not",
			e.Actual.State)
	}
	if e.Actual.Value != floor {
		t.Errorf("Actual.Value = %s, want the text floor %s exactly -- no figure standing in for "+
			"the unpriced audio", e.Actual.Value, floor)
	}
	txt := e.Actual.Text(money.Money.String)
	if !strings.HasSuffix(txt, "+") {
		t.Errorf("rendered as %q, want a trailing + marking a floor", txt)
	}
	if txt == "$0.0000" || txt == "$0.00" {
		t.Errorf("rendered as %q: the audio component is missing, not zero", txt)
	}
	// The unpriced dimensions travel to the reader by name, because "add these two rates"
	// is the action the floor implies.
	if len(e.Actual.Unpriced) != 2 {
		t.Fatalf("Actual.Unpriced = %v, want both audio dimensions", e.Actual.Unpriced)
	}
	for _, want := range []string{"input_audio_tokens", "output_audio_tokens"} {
		var found bool
		for _, d := range e.Actual.Unpriced {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Actual.Unpriced = %v, should name %s", e.Actual.Unpriced, want)
		}
	}

	// The page total inherits the incompleteness rather than absorbing it.
	if page.Summary.Complete {
		t.Error("a page holding an audio request with no audio rates must not report a complete total")
	}
	if page.Summary.Spend != floor {
		t.Errorf("page spend = %s, want the floor %s", page.Summary.Spend, floor)
	}

	// And the bookkeeping panel names the two rates to add, which is the operator's
	// actionable output from all of this.
	sum, err := w.rep.Summary(w.ctx, "research")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if sum.Health.Unresolved != 1 {
		t.Errorf("Health.Unresolved = %d, want 1: an unpriced audio dimension is a liability to "+
			"chase", sum.Health.Unresolved)
	}
	wantDims := []string{"input_audio_tokens", "output_audio_tokens"}
	if len(sum.Health.UnpricedDimensions) != 2 {
		t.Fatalf("UnpricedDimensions = %v, want %v", sum.Health.UnpricedDimensions, wantDims)
	}
	for i, want := range wantDims {
		if sum.Health.UnpricedDimensions[i] != want {
			t.Errorf("UnpricedDimensions[%d] = %q, want %q", i, sum.Health.UnpricedDimensions[i], want)
		}
	}
	// The hold stays a hold: an unresolved cost is not spend, and its headroom is still
	// encumbered until someone supplies the missing rates.
	if sum.Position.Spent != 0 {
		t.Errorf("Position.Spent = %s, want 0: no charge settled", sum.Position.Spent)
	}
}

// Both families group into one breakdown when the reader is asking a question the API
// family has nothing to do with.
//
// The complement of the operation facet: a reader asking "what did gpt-5.1 cost me?" wants
// one row, because the model is the model regardless of which endpoint reached it. Two
// rows here would mean the adapter had written the API family into the model identity.
func TestBothOpenAIFamiliesGroupUnderOneModel(t *testing.T) {
	w := newWorld(t)
	p := w.define(monthly("research", "", dollars(1000)))
	at := w.now.Add(-time.Hour)

	w.spend("s1", "research", cents(6), at)
	w.spend("s2", "research", cents(4), at.Add(time.Minute))
	w.record(openAIRecord("resp1", "research", p.ID, cents(6), at))
	w.record(chatRecord("chat1", "research", p.ID, cents(4), at.Add(time.Minute)))

	b, err := w.rep.Breakdown(w.ctx, FacetProviderModel, ActivityQuery{BudgetID: "research"})
	if err != nil {
		t.Fatalf("Breakdown(provider-model): %v", err)
	}
	if len(b.Rows) != 1 {
		t.Fatalf("the provider-model breakdown has %d rows, want 1: %v", len(b.Rows), keys(b.Rows))
	}
	row := b.Rows[0]
	if row.Key != "gpt-5.1" {
		t.Errorf("row key = %q, want gpt-5.1", row.Key)
	}
	if row.Spend != cents(10) || row.Requests != 2 {
		t.Errorf("gpt-5.1 = %s over %d requests, want %s over 2: the model is the same model "+
			"whichever endpoint reached it", row.Spend, row.Requests, cents(10))
	}
	if !row.Complete {
		t.Error("a row of two fully priced requests reports an incomplete total")
	}
}
