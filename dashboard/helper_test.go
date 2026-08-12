package dashboard

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"throttle/activity"
	activitysqlite "throttle/activity/sqlite"
	"throttle/budget"
	"throttle/engine"
	"throttle/ledger"
	ledgersqlite "throttle/ledger/sqlite"
	"throttle/money"
	"throttle/pricing"
	"throttle/report"
	"throttle/usage"
)

// The dashboard is tested against a real read model over real SQLite stores, for the
// same reason the read model is: its job is to render what the stores actually say, and
// a fake reporter would return whatever this package expected and prove nothing about
// the rendering of real data.
//
// What these tests assert is deliberately narrow. The read model's own suite already
// pins the arithmetic; the questions here are whether those properties survive the trip
// through a template and a JSON encoder -- whether an unknown cost can become "$0.00" on
// the way to a browser, whether "reserved" can end up in a column headed "spent", and
// whether a page load can move money.

func dollars(d int64) money.Money { return money.Money(d) * money.PerDollar }

func cents(c int64) money.Money { return money.Money(c) * money.PerDollar / 100 }

// base is the reference instant: the start of a month, so envelope bounds are round.
var base = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// monthDuration is the length of the August 2026 period the fixtures use.
const monthDuration = 31 * 24 * time.Hour

func monthly(id, parent string, allocation money.Money) budget.Definition {
	return budget.Definition{
		ID:         id,
		ParentID:   parent,
		Name:       id,
		Allocation: allocation,
		Recurrence: budget.RecurMonthly,
		AnchorAt:   base,
	}
}

// week is a one-week non-recurring envelope: a demo budget, or a grant. Monthly is not
// special in this UI and the tests say so by not only using monthly.
func week(id string, allocation money.Money) budget.Definition {
	return budget.Definition{
		ID:         id,
		Name:       id,
		Allocation: allocation,
		Recurrence: budget.RecurNone,
		AnchorAt:   base,
		EndAt:      base.Add(7 * 24 * time.Hour),
	}
}

// world is a ledger, an activity store, a dashboard over both, and a movable clock.
type world struct {
	t    *testing.T
	ctx  context.Context
	led  *ledgersqlite.Store
	acts *activitysqlite.Store
	rep  *report.Reporter
	srv  *Server

	now time.Time
}

func newWorld(t *testing.T) *world { return newWorldIn(t, t.TempDir(), true) }

// newLedgerOnlyWorld is the configuration a dashboard runs in when no activity database
// exists: every budget figure answerable, no request history at all.
func newLedgerOnlyWorld(t *testing.T) *world { return newWorldIn(t, t.TempDir(), false) }

func newWorldIn(t *testing.T, dir string, withActivity bool) *world {
	t.Helper()
	ctx := context.Background()

	led, err := ledgersqlite.Open(ctx, filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { led.Close() })

	w := &world{t: t, ctx: ctx, led: led, now: base.Add(monthDuration / 2)}
	cfg := report.Config{Ledger: led, Clock: func() time.Time { return w.now }}

	if withActivity {
		acts, err := activitysqlite.Open(ctx, filepath.Join(dir, "activity.db"))
		if err != nil {
			t.Fatalf("open activity store: %v", err)
		}
		t.Cleanup(func() { acts.Close() })
		w.acts = acts
		cfg.Activity = acts
	}

	rep, err := report.New(cfg)
	if err != nil {
		t.Fatalf("report.New: %v", err)
	}
	w.rep = rep

	srv, err := New(Config{Reporter: rep, Version: "test"})
	if err != nil {
		t.Fatalf("dashboard.New: %v", err)
	}
	w.srv = srv
	return w
}

// at moves the clock, which is what makes "before the period", "midway through", and
// "after it ended" reachable without waiting.
//
// The period a budget is defined into is the one containing the clock at define time, so
// a short envelope has to be defined from inside itself and only then travelled away
// from.
func (w *world) at(t time.Time) *world {
	w.now = t
	return w
}

// define stores a definition and materializes the period containing the clock.
func (w *world) define(def budget.Definition) ledger.Period {
	w.t.Helper()
	if err := w.led.PutDefinition(w.ctx, def); err != nil {
		w.t.Fatalf("PutDefinition(%q): %v", def.ID, err)
	}
	p, err := w.led.EnsurePeriod(w.ctx, def.ID, w.now)
	if err != nil {
		w.t.Fatalf("EnsurePeriod(%q): %v", def.ID, err)
	}
	return p
}

// ensure materializes the period containing the clock for a budget already defined, which
// is how a second period comes into existence for a recurring definition.
func (w *world) ensure(budgetID string) ledger.Period {
	w.t.Helper()
	p, err := w.led.EnsurePeriod(w.ctx, budgetID, w.now)
	if err != nil {
		w.t.Fatalf("EnsurePeriod(%q): %v", budgetID, err)
	}
	return p
}

func unlimited(ids ...string) map[string]money.Money {
	out := make(map[string]money.Money, len(ids))
	for _, id := range ids {
		out[id] = money.Max
	}
	return out
}

// spend reserves and settles, which is what puts settled money in the ledger.
func (w *world) spend(id, budgetID string, amount money.Money, at time.Time, chain ...string) ledger.Charge {
	w.t.Helper()
	if len(chain) == 0 {
		chain = []string{budgetID}
	}
	rv, err := w.led.Reserve(w.ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID: id, BudgetID: budgetID, RequestID: "req-" + id,
			Amount: amount, EstimatedCost: amount, CreatedAt: at,
			ExpiresAt: at.Add(time.Hour), Lease: time.Hour,
			Identity: bedrockIdentity(),
		},
		Ceilings: unlimited(chain...),
		Now:      at,
	})
	if err != nil {
		w.t.Fatalf("Reserve(%q): %v", id, err)
	}
	c, err := w.led.Settle(w.ctx, ledger.Settlement{
		ReservationID: rv.ID, ActualCost: amount, CompletedAt: at,
	})
	if err != nil {
		w.t.Fatalf("Settle(%q): %v", id, err)
	}
	return c
}

// hold reserves without settling, leaving headroom encumbered by a live lease.
//
// The lease runs from the clock rather than from the charge instant, because a hold
// created in the past with a one-hour lease would have lapsed by the time the page
// renders, and a lapsed hold is a different fixture from a live one.
func (w *world) hold(id, budgetID string, amount money.Money, chain ...string) ledger.Reservation {
	w.t.Helper()
	if len(chain) == 0 {
		chain = []string{budgetID}
	}
	at := w.now.Add(-time.Minute)
	rv, err := w.led.Reserve(w.ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID: id, BudgetID: budgetID, RequestID: "req-" + id,
			Amount: amount, EstimatedCost: amount, CreatedAt: at,
			ExpiresAt: at.Add(2 * time.Hour), Lease: 2 * time.Hour,
			Identity: bedrockIdentity(),
		},
		Ceilings: unlimited(chain...),
		Now:      at,
	})
	if err != nil {
		w.t.Fatalf("Reserve(%q): %v", id, err)
	}
	return rv
}

// expiredHold reserves with a lease that has already lapsed by the time the page renders.
//
// A lapsed lease is a different fixture from a live one, and the difference is the whole
// point: its headroom has already dropped out of Reserved, and the request behind it may
// nonetheless have been served and billed.
func (w *world) expiredHold(id, budgetID string, amount money.Money, chain ...string) ledger.Reservation {
	w.t.Helper()
	if len(chain) == 0 {
		chain = []string{budgetID}
	}
	at := w.now.Add(-4 * time.Hour)
	rv, err := w.led.Reserve(w.ctx, ledger.ReserveRequest{
		Reservation: ledger.Reservation{
			ID: id, BudgetID: budgetID, RequestID: "req-" + id,
			Amount: amount, EstimatedCost: amount, CreatedAt: at,
			ExpiresAt: at.Add(time.Hour), Lease: time.Hour,
			Identity: bedrockIdentity(),
		},
		Ceilings: unlimited(chain...),
		Now:      at,
	})
	if err != nil {
		w.t.Fatalf("Reserve(%q): %v", id, err)
	}
	return rv
}

func (w *world) record(rec activity.Record) activity.Record {
	w.t.Helper()
	if err := w.acts.Complete(w.ctx, rec); err != nil {
		w.t.Fatalf("Complete(%q): %v", rec.RequestID, err)
	}
	return rec
}

// bedrockIdentity is the three-facet identity: the access path is AWS Bedrock, the
// publisher is Anthropic, and the model is a third thing again.
func bedrockIdentity() usage.ModelIdentity {
	return usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		Publisher:       "anthropic",
		Family:          "claude-sonnet",
		CanonicalModel:  "claude-sonnet-4",
		ProviderModelID: "anthropic.claude-sonnet-4-20250514-v1:0",
		Operation:       "converse",
		Region:          "us-east-1",
	}
}

// settledRecord is an ordinary fully priced request.
func settledRecord(requestID, budgetID, periodID string, cost money.Money, at time.Time) activity.Record {
	return activity.Record{
		RequestID:     requestID,
		ReservationID: "res-" + requestID,
		BudgetID:      budgetID,
		Scopes:        []activity.Scope{{BudgetID: budgetID, PeriodID: periodID}},
		Identity:      bedrockIdentity(),
		Estimate: usage.Estimate{
			Identity: bedrockIdentity(),
			Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 1000}),
			Cost:     usage.KnownCost(cost),
			Quality:  usage.QualityConservative,
		},
		Quote: pricing.CapturedQuote{
			AccessProvider:  "aws-bedrock",
			ProviderModelID: bedrockIdentity().ProviderModelID,
			Provenance: pricing.Provenance{
				Source: "aws-price-list", Version: "2026-08-01", RetrievedAt: at, Currency: "USD",
			},
		},
		Reserved: cost,
		ActualUsage: usage.New(map[usage.Dimension]int64{
			usage.InputTokens:     1200,
			usage.OutputTokens:    400,
			usage.CacheReadTokens: 90,
		}),
		ActualCost:      usage.KnownCost(cost),
		EnforcementMode: engine.ModeEnforce,
		Status:          activity.StatusSettled,
		Outcome:         activity.OutcomeSuccess,
		StartedAt:       at,
		CompletedAt:     at.Add(2 * time.Second),
		Latency:         2 * time.Second,
		ProviderLatency: 1900 * time.Millisecond,
	}
}

// unknownCostRecord ran, incurred cost, and could not be priced at all. Its cell must
// never read as a free request.
func unknownCostRecord(requestID, budgetID, periodID string, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, periodID, 0, at)
	rec.ActualCost = usage.UnknownCost("no price for this model in the catalog")
	rec.Reserved = dollars(1)
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeUnpriced
	return rec
}

// unpriceableRecord completed and was settled, and nothing in the captured quote could
// price it. It is a finished request with an unknown cost, which is a different state from
// an unresolved one: nothing further is expected to arrive.
func unpriceableRecord(requestID, budgetID, periodID string, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, periodID, 0, at)
	rec.ActualCost = usage.UnknownCost("the captured quote has no rate for this model")
	rec.Status = activity.StatusSettled
	rec.Outcome = activity.OutcomeUnpriced
	return rec
}

// partialCostRecord priced some of its usage and not the rest, so its cost is a floor.
func partialCostRecord(requestID, budgetID, periodID string, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, periodID, cents(3), at)
	rec.ActualUsage = usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  1200,
		usage.OutputTokens: 400,
		"widget_calls":     7,
	})
	rec.ActualCost = usage.PartialCost(cents(3),
		[]usage.Dimension{"widget_calls"}, "no price for widget_calls")
	return rec
}

// unmappedModelRecord names a model the catalog has never heard of, which must still
// display something true: the exact provider model ID.
func unmappedModelRecord(requestID, budgetID, periodID string, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, periodID, cents(7), at)
	rec.Identity.CanonicalModel = ""
	rec.Identity.Family = ""
	rec.Identity.ProviderModelID = "vendor.brand-new-model-v9:0"
	rec.Estimate.Identity = rec.Identity
	return rec
}

// agentRecord is a managed agent turn: ONE governed transaction with internal model
// invocations beneath it, carrying measurements and no content.
func agentRecord(requestID, budgetID, periodID string, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, periodID, cents(91), at)
	rec.Identity.Operation = "invoke-agent"
	rec.Estimate.Quality = usage.QualityHeuristic
	rec.Agent = activity.Agent{
		AgentID:   "AGENT123",
		AliasID:   "TSTALIASID",
		Version:   "3",
		SessionID: "sess-abc",
		Events:    map[string]int{"tool-invocation": 2, "knowledge-base-lookup": 1},
		Note:      "non-model activity occurred that the provider does not price per call",
		Steps: []activity.AgentStep{
			{
				Seq: 1, Kind: "preprocessing", TraceID: "trace-1",
				Identity: bedrockIdentity(),
				Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 300, usage.OutputTokens: 20}),
				Cost:     usage.KnownCost(cents(10)),
				Latency:  200 * time.Millisecond,
				At:       at,
			},
			{
				Seq: 2, Kind: "orchestration", TraceID: "trace-2",
				Collaborator: "researcher",
				Identity:     bedrockIdentity(),
				Usage:        usage.New(map[usage.Dimension]int64{usage.InputTokens: 900, usage.OutputTokens: 250}),
				Cost:         usage.KnownCost(cents(55)),
				Latency:      1200 * time.Millisecond,
				At:           at.Add(300 * time.Millisecond),
			},
			{
				Seq: 3, Kind: "postprocessing",
				// A step whose model the provider never named: a real charge with no
				// identity, which must render as an absence rather than a guess.
				Identity: usage.ModelIdentity{AccessProvider: "aws-bedrock", Operation: "invoke-agent"},
				Usage:    usage.New(map[usage.Dimension]int64{usage.InputTokens: 400, usage.OutputTokens: 60}),
				Cost:     usage.KnownCost(cents(25)),
				Latency:  400 * time.Millisecond,
				At:       at.Add(1600 * time.Millisecond),
			},
		},
	}
	return rec
}

// runtimeRecord is a hosted-runtime invocation whose compute cost arrives out of band
// and has not arrived.
func runtimeRecord(requestID, budgetID, periodID string, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, periodID, 0, at)
	rec.Identity = usage.ModelIdentity{
		AccessProvider: "aws-bedrock-agentcore",
		Operation:      "invoke-agent-runtime",
	}
	rec.ActualUsage = usage.Usage{}
	rec.ActualCost = usage.UnknownCost("hosted runtime resource usage is reported out of band")
	rec.Estimate.Cost = usage.UnknownCost("a hosted runtime's compute cost is not knowable at admission")
	rec.Estimate.Quality = usage.QualityHeuristic
	rec.Reserved = dollars(2)
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeUnpriced
	rec.Runtime = activity.HostedRuntime{
		RuntimeID:     "arn:aws:bedrock-agentcore:us-east-1:123456789012:runtime/my-agent",
		Qualifier:     "DEFAULT",
		Account:       "123456789012",
		SessionID:     "runtime-session-77",
		RequestID:     "provider-req-9",
		TraceID:       "trace-xyz",
		StatusCode:    200,
		ContentType:   "application/json",
		PayloadBytes:  812,
		ResponseBytes: 2044,
		Note:          "session-level resource usage is not divisible across invocations",
	}
	return rec
}

// repairedRecord carries the reconciler's own typed reason, which the bookkeeping panel
// must surface rather than paraphrase.
func repairedRecord(requestID, budgetID, periodID string, at time.Time) activity.Record {
	rec := settledRecord(requestID, budgetID, periodID, cents(12), at)
	rec.Repairs = []activity.Reconciliation{{
		At:                  at.Add(time.Minute),
		Class:               "repaired",
		Reason:              "crash-repairable",
		ObservedStatus:      activity.StatusPending,
		ObservedReservation: "pending",
		ProducedStatus:      activity.StatusSettled,
		Money:               "settled",
		Amount:              cents(12),
		QuoteSource:         "aws-price-list",
		QuoteVersion:        "2026-08-01",
		Detail:              "the ledger held a settled charge the activity record never learned about",
	}}
	return rec
}

// --- HTTP helpers ----------------------------------------------------------

// get drives the real handler chain, so every assertion below is about what a browser
// would receive rather than about a view struct.
func (w *world) get(path string) *httptest.ResponseRecorder {
	w.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	w.srv.ServeHTTP(rec, req)
	return rec
}

// html fetches a page and fails unless it rendered.
func (w *world) html(path string) string {
	w.t.Helper()
	rec := w.get(path)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("GET %s = %d, want 200\n%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// jsonBody fetches a JSON endpoint and returns the decoded payload.
func (w *world) jsonBody(path string) map[string]any {
	w.t.Helper()
	rec := w.get(path)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("GET %s = %d, want 200\n%s", path, rec.Code, rec.Body.String())
	}
	return decodeObject(w.t, rec.Body)
}

func decodeObject(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	return out
}

// str reads a string field out of a decoded JSON object, failing when it is absent or
// of the wrong type -- a renamed field is a broken contract, not a missing value.
func str(t *testing.T, obj map[string]any, key string) string {
	t.Helper()
	v, ok := obj[key]
	if !ok {
		t.Fatalf("JSON payload has no %q field", key)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("JSON field %q is %T, want string", key, v)
	}
	return s
}

// mustContain fails with the surrounding text rather than with the whole page, because a
// 40KB HTML dump in a test failure hides the thing that went wrong.
//
// The body is unescaped first, for the same reason figure() unescapes: an apostrophe in
// "this call's own runtime cost" reaches the browser as "&#39;" and renders as "'", and a
// test spelling it the first way would be asserting on html/template's encoder rather than
// on the sentence.
func mustContain(t *testing.T, body, want, why string) {
	t.Helper()
	if !strings.Contains(unescape(body), want) {
		t.Errorf("rendered page does not contain %q: %s", want, why)
	}
}

// mustSay asserts a sentence a reader would see, ignoring where the template happened to
// wrap it.
//
// The templates wrap their explanatory copy to fit a source line, so a sentence can carry a
// newline and eight spaces in the middle of a phrase. Asserting on the wrapped form works
// but pins the assertion to the indentation, which then breaks when a paragraph is
// reindented and says nothing about the words.
func mustSay(t *testing.T, body, want, why string) {
	t.Helper()
	if !strings.Contains(flatten(body), flatten(want)) {
		t.Errorf("rendered page does not say %q: %s", want, why)
	}
}

// flatten unescapes entities and collapses every run of whitespace to one space.
func flatten(s string) string {
	return strings.Join(strings.Fields(unescape(s)), " ")
}

func mustNotContain(t *testing.T, body, unwanted, why string) {
	t.Helper()
	body = unescape(body)
	if i := strings.Index(body, unwanted); i >= 0 {
		t.Errorf("rendered page contains %q: %s\n...%s...",
			unwanted, why, excerpt(body, i))
	}
}

func excerpt(body string, at int) string {
	lo, hi := at-120, at+120
	if lo < 0 {
		lo = 0
	}
	if hi > len(body) {
		hi = len(body)
	}
	return body[lo:hi]
}

// cellsUnder returns the text of the column headed name, one entry per row, from the
// first table in body that has such a header.
//
// Crude HTML slicing rather than a parser dependency, and deliberately anchored on the
// header text: a test that asserted on the fourth <td> would keep passing if a column
// were inserted, which is exactly the mistake it exists to catch.
func cellsUnder(t *testing.T, body, name string) []string {
	t.Helper()
	for _, table := range between(body, "<table", "</table>") {
		head, ok := first(table, "<thead", "</thead>")
		if !ok {
			continue
		}
		headers := stripTags(between(head, "<th", "</th>"))
		idx := -1
		for i, h := range headers {
			if h == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		// Body rows only. A tfoot carries a summary line whose cells are spanned and do
		// not line up with the header, and counting it as a row would make a three-step
		// table report four steps.
		rows := table
		if body, ok := first(table, "<tbody", "</tbody>"); ok {
			rows = body
		}
		var out []string
		for _, row := range between(rows, "<tr", "</tr>") {
			if strings.Contains(row, "<th") {
				continue
			}
			cells := stripTags(between(row, "<td", "</td>"))
			if idx < len(cells) {
				out = append(out, cells[idx])
			}
		}
		return out
	}
	t.Fatalf("no table in the page has a column headed %q", name)
	return nil
}

// panel returns the markup of the <section class="panel X"> named by class, so an
// assertion about "the Actual column" can name which table it means. Several panels
// render the same request table, and the first one on the page is not always the one a
// test is about.
// A panel's class list carries state -- "panel recon needs", "panel gauge state-measured"
// -- so the match is on the name within the list rather than on the whole attribute.
func panel(t *testing.T, body, class string) string {
	t.Helper()
	i := -1
	for _, marker := range []string{
		`class="panel ` + class + `"`,
		`class="panel ` + class + ` `,
	} {
		if j := strings.Index(body, marker); j >= 0 && (i < 0 || j < i) {
			i = j
		}
	}
	if i < 0 {
		t.Fatalf("the page has no panel with class %q", class)
	}
	rest := body[i:]
	j := strings.Index(rest, "</section>")
	if j < 0 {
		t.Fatalf("the %q panel is unterminated", class)
	}
	return rest[:j]
}

// figure returns the value rendered for a named data-field, which is how the templates
// mark the figures the poll refreshes.
func figure(t *testing.T, body, field string) string {
	t.Helper()
	marker := `data-field="` + field + `"`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no element carries data-field=%q", field)
	}
	rest := body[i:]
	j := strings.Index(rest, ">")
	if j < 0 {
		t.Fatalf("data-field=%q element is malformed", field)
	}
	rest = rest[j+1:]
	k := strings.Index(rest, "<")
	if k < 0 {
		k = len(rest)
	}
	return unescape(strings.TrimSpace(rest[:k]))
}

// unescape undoes html/template's entity encoding, so an assertion can be written in the
// characters a reader sees. A leading "+" on a banked pace balance reaches the browser as
// "&#43;" and renders as "+", and a test spelling it the second way would be asserting on
// the encoder rather than on the figure.
func unescape(s string) string {
	return html.UnescapeString(s)
}

// between returns each substring bounded by open and close, exclusive of close.
func between(s, open, close string) []string {
	var out []string
	for {
		i := strings.Index(s, open)
		if i < 0 {
			return out
		}
		s = s[i+len(open):]
		j := strings.Index(s, close)
		if j < 0 {
			return out
		}
		out = append(out, s[:j])
		s = s[j+len(close):]
	}
}

func first(s, open, close string) (string, bool) {
	got := between(s, open, close)
	if len(got) == 0 {
		return "", false
	}
	return got[0], true
}

// stripTags removes markup and collapses whitespace, leaving the text a reader sees.
func stripTags(chunks []string) []string {
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		// Each chunk starts partway through its own opening tag.
		if i := strings.Index(c, ">"); i >= 0 {
			c = c[i+1:]
		}
		var b strings.Builder
		depth := 0
		for _, r := range c {
			switch {
			case r == '<':
				depth++
			case r == '>':
				if depth > 0 {
					depth--
				}
			case depth == 0:
				b.WriteRune(r)
			}
		}
		out = append(out, unescape(strings.Join(strings.Fields(b.String()), " ")))
	}
	return out
}
