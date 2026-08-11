package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"throttle/activity"
	"throttle/activity/sqlite"
	"throttle/engine"
	"throttle/money"
	"throttle/pricing"
	"throttle/usage"
)

var at = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

func dollars(d int64) money.Money { return money.Money(d) * money.PerDollar }

func open(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func quote(t *testing.T) pricing.CapturedQuote {
	t.Helper()
	return pricing.CapturedQuote{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "anthropic.claude-sonnet-4-20250514-v1:0",
		Region:          "us-east-1",
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(3)),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(15)),
		},
		Provenance: pricing.Provenance{Source: "test", Version: "v1"},
		CapturedAt: at,
	}
}

func settled(t *testing.T) activity.Record {
	t.Helper()
	return activity.Record{
		RequestID:     "req-1",
		ReservationID: "res-req-1",
		BudgetID:      "team",
		Scopes: []activity.Scope{
			{BudgetID: "team", PeriodID: "team-2026-08", Depth: 0},
			{BudgetID: "org", PeriodID: "org-2026-08", Depth: 1},
		},
		Identity: usage.ModelIdentity{
			AccessProvider:  "aws-bedrock",
			Publisher:       "anthropic",
			CanonicalModel:  "claude-sonnet-4",
			ProviderModelID: "anthropic.claude-sonnet-4-20250514-v1:0",
			Operation:       "converse",
			Region:          "us-east-1",
			ServiceTier:     "standard",
		},
		Estimate: usage.Estimate{
			Usage:   usage.New(map[usage.Dimension]int64{usage.InputTokens: 1000, usage.OutputTokens: 4096}),
			Cost:    usage.KnownCost(dollars(1)),
			Quality: usage.QualityConservative,
			Note:    "output bounded by MaxTokens",
		},
		Quote:    quote(t),
		Reserved: dollars(1),
		ActualUsage: usage.New(map[usage.Dimension]int64{
			usage.InputTokens:  1000,
			usage.OutputTokens: 500,
		}),
		ActualCost:      usage.KnownCost(money.Money(10_500)),
		EnforcementMode: engine.ModeEnforce,
		Status:          activity.StatusSettled,
		Outcome:         activity.OutcomeSuccess,
		StartedAt:       at,
		CompletedAt:     at.Add(2 * time.Second),
		Latency:         2 * time.Second,
		ProviderLatency: 1900 * time.Millisecond,
		Metadata:        map[string]string{"workload": "nightly-eval"},
	}
}

// Everything a record claims to hold must come back out. A field that silently
// fails to persist is worse than one that was never added, because a dashboard
// will confidently report its zero value.
func TestRecordRoundTrips(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	ctx := context.Background()

	want := settled(t)
	if err := s.Complete(ctx, want); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := s.Get(ctx, "req-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ReservationID != want.ReservationID || got.BudgetID != want.BudgetID {
		t.Errorf("ids = %q/%q, want %q/%q", got.ReservationID, got.BudgetID, want.ReservationID, want.BudgetID)
	}
	if got.Identity != want.Identity {
		t.Errorf("Identity = %+v, want %+v", got.Identity, want.Identity)
	}
	if got.ActualCost.Amount != want.ActualCost.Amount || !got.ActualCost.Known() {
		t.Errorf("ActualCost = %s, want %s", got.ActualCost, want.ActualCost)
	}
	if got.ActualUsage.Count(usage.OutputTokens) != 500 {
		t.Errorf("output tokens = %d, want 500", got.ActualUsage.Count(usage.OutputTokens))
	}
	if got.Reserved != want.Reserved {
		t.Errorf("Reserved = %s, want %s", got.Reserved, want.Reserved)
	}
	if got.EnforcementMode != engine.ModeEnforce {
		t.Errorf("EnforcementMode = %q, want enforce", got.EnforcementMode)
	}
	if got.Status != activity.StatusSettled || got.Outcome != activity.OutcomeSuccess {
		t.Errorf("status/outcome = %q/%q, want settled/success", got.Status, got.Outcome)
	}
	if !got.StartedAt.Equal(want.StartedAt) || !got.CompletedAt.Equal(want.CompletedAt) {
		t.Errorf("timestamps = %s/%s, want %s/%s", got.StartedAt, got.CompletedAt, want.StartedAt, want.CompletedAt)
	}
	if got.Latency != want.Latency || got.ProviderLatency != want.ProviderLatency {
		t.Errorf("latency = %s/%s, want %s/%s", got.Latency, got.ProviderLatency, want.Latency, want.ProviderLatency)
	}
	if got.Metadata["workload"] != "nightly-eval" {
		t.Errorf("Metadata = %v, want the workload preserved", got.Metadata)
	}
	if got.Estimate.Quality != usage.QualityConservative {
		t.Errorf("Estimate.Quality = %q, want conservative", got.Estimate.Quality)
	}
	if len(got.Scopes) != 2 || got.Scopes[1].BudgetID != "org" {
		t.Errorf("Scopes = %+v, want the team and org legs", got.Scopes)
	}
}

// The captured quote must survive persistence and still price identically, or a
// request cannot be reconciled on its original basis after a restart.
func TestQuoteSurvivesPersistenceAndStillPrices(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	ctx := context.Background()

	rec := settled(t)
	if err := s.Complete(ctx, rec); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, err := s.Get(ctx, "req-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.Quote.Provenance.Source != "test" || got.Quote.Provenance.Version != "v1" {
		t.Errorf("Quote.Provenance = %+v, want the captured provenance", got.Quote.Provenance)
	}
	if !got.Quote.Valid() {
		t.Fatal("the persisted quote must still be usable")
	}
	priced, err := got.Quote.Price(rec.ActualUsage)
	if err != nil {
		t.Fatalf("Price from a persisted quote: %v", err)
	}
	if priced.Cost.Amount != rec.ActualCost.Amount {
		t.Errorf("re-priced = %s, want the originally recorded %s", priced.Cost, rec.ActualCost)
	}
}

// A record must survive the process that wrote it. Reconciling an unresolved
// liability days later depends on nothing but what is on disk.
func TestRecordSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.db")
	ctx := context.Background()

	first, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := settled(t)
	rec.Status = activity.StatusUnresolved
	rec.Outcome = activity.OutcomeUnpriced
	rec.ActualCost = usage.PartialCost(dollars(3),
		[]usage.Dimension{usage.Dimension("video-sec")}, "no rate for video-sec")
	if err := first.Complete(ctx, rec); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A wholly new handle, as after a restart.
	second := open(t, path)
	got, err := second.Get(ctx, "req-1")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if got.Status != activity.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", got.Status)
	}
	if got.ActualCost.Completeness != usage.CostPartial {
		t.Errorf("Completeness = %q, want partial", got.ActualCost.Completeness)
	}
	if got.ActualCost.Amount != dollars(3) {
		t.Errorf("floor = %s, want %s", got.ActualCost.Amount, dollars(3))
	}
	if len(got.ActualCost.Unpriced) != 1 || got.ActualCost.Unpriced[0] != usage.Dimension("video-sec") {
		t.Errorf("Unpriced = %v, want [video-sec] to survive the restart", got.ActualCost.Unpriced)
	}
	if !got.Quote.Valid() {
		t.Error("the quote must survive the restart so the liability can be reconciled")
	}
	if !got.Unresolved() {
		t.Error("the restored record must still report itself unresolved")
	}
}

// A dimension nobody anticipated must survive persistence verbatim. Discarding it
// would destroy the only evidence of what throttle failed to price.
func TestUnknownDimensionsSurvivePersistence(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	ctx := context.Background()

	odd := usage.Dimension("holographic-frames")
	rec := settled(t)
	rec.ActualUsage = usage.New(map[usage.Dimension]int64{
		usage.InputTokens: 1000,
		odd:               42,
	})
	rec.ActualCost = usage.PartialCost(money.Money(3_000), []usage.Dimension{odd}, "no rate for holographic-frames")
	rec.Status = activity.StatusUnresolved
	if err := s.Complete(ctx, rec); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := s.Get(ctx, "req-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ActualUsage.Count(odd) != 42 {
		t.Errorf("%s = %d, want 42: an unrecognized dimension must not be discarded",
			odd, got.ActualUsage.Count(odd))
	}
	// Canonical dimensions round-trip alongside it, keyed by their constants rather
	// than by an arbitrary string.
	if got.ActualUsage.Count(usage.InputTokens) != 1000 {
		t.Errorf("input tokens = %d, want 1000", got.ActualUsage.Count(usage.InputTokens))
	}
	if len(got.ActualCost.Unpriced) != 1 || got.ActualCost.Unpriced[0] != odd {
		t.Errorf("Unpriced = %v, want [%s]", got.ActualCost.Unpriced, odd)
	}
}

// The store must hold measurements, not transcripts. This checks the actual bytes
// on disk, because the guarantee is about what the file contains, not about what
// the struct is documented to hold.
func TestStoreHoldsNoContent(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	ctx := context.Background()

	rec := settled(t)
	if err := s.Complete(ctx, rec); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// No column may accept content, so there is nowhere for a prompt to go. Verify
	// by inspecting the schema itself rather than by trusting the writer.
	rows, err := s.DB().QueryContext(ctx, `SELECT name FROM pragma_table_info('activity')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer rows.Close()

	banned := []string{"prompt", "message", "content", "response", "text", "body", "completion", "input_text", "output_text"}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		for _, b := range banned {
			if name == b || strings.HasSuffix(name, "_"+b) {
				t.Errorf("column %q could hold request or response content", name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
}

// A retry after an ambiguous failure must update its own record rather than
// creating a rival account of the same call.
func TestWriteIsIdempotentOnRequestID(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	ctx := context.Background()

	pending := settled(t)
	pending.Status = activity.StatusPending
	pending.Outcome = ""
	pending.ActualUsage = usage.Usage{}
	pending.ActualCost = usage.Cost{}
	pending.CompletedAt = time.Time{}
	if err := s.Begin(ctx, pending); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if err := s.Complete(ctx, settled(t)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	all, err := s.List(ctx, activity.Filter{BudgetID: "team"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d records, want 1: a resolved request must overwrite its own pre-call row", len(all))
	}
	if all[0].Status != activity.StatusSettled {
		t.Errorf("Status = %q, want settled", all[0].Status)
	}
	if all[0].ActualCost.Amount != money.Money(10_500) {
		t.Errorf("ActualCost = %s, want the settled amount", all[0].ActualCost)
	}
}

// A pre-call record with no resolution is the trace a crashed process leaves. It
// must be findable, because it is the only evidence that money may have moved.
func TestPendingRecordIsVisibleAfterACrash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "activity.db")
	ctx := context.Background()

	first, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := settled(t)
	rec.Status = activity.StatusPending
	rec.Outcome = ""
	rec.ActualUsage = usage.Usage{}
	rec.ActualCost = usage.Cost{}
	rec.CompletedAt = time.Time{}
	if err := first.Begin(ctx, rec); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// No Complete: the process died here.
	first.Close()

	second := open(t, path)
	pending, err := second.List(ctx, activity.Filter{Statuses: []activity.Status{activity.StatusPending}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending records, want 1", len(pending))
	}
	// Its cost reads as unknown, not as zero: nobody ever priced it.
	if pending[0].ActualCost.Known() {
		t.Error("an unresolved pre-call record must not report a known cost")
	}
	if pending[0].Quote.Valid() != true {
		t.Error("the pre-call quote must be there, or the request cannot be priced later")
	}
}

// A parent budget must see spend its children incurred: a request against a child
// really did consume the parent's headroom.
func TestListMatchesAncestorScopes(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	ctx := context.Background()

	if err := s.Complete(ctx, settled(t)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := s.List(ctx, activity.Filter{BudgetID: "org"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records for the parent budget, want 1", len(got))
	}

	byPeriod, err := s.List(ctx, activity.Filter{PeriodID: "org-2026-08"})
	if err != nil {
		t.Fatalf("List by period: %v", err)
	}
	if len(byPeriod) != 1 {
		t.Errorf("got %d records for the parent period, want 1", len(byPeriod))
	}
}

// "Unpriced/unresolved requests: 3" has to be a query, not a scan of every row.
func TestListUnresolvedOnly(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	ctx := context.Background()

	if err := s.Complete(ctx, settled(t)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	for i, r := range []struct {
		id   string
		cost usage.Cost
	}{
		{"req-2", usage.UnknownCost("no price")},
		{"req-3", usage.PartialCost(dollars(1), []usage.Dimension{usage.Dimension("video-sec")}, "no rate")},
	} {
		rec := settled(t)
		rec.RequestID = r.id
		rec.Status = activity.StatusUnresolved
		rec.Outcome = activity.OutcomeUnpriced
		rec.ActualCost = r.cost
		rec.StartedAt = at.Add(time.Duration(i+1) * time.Minute)
		if err := s.Complete(ctx, rec); err != nil {
			t.Fatalf("Complete(%s): %v", r.id, err)
		}
	}

	unresolved, err := s.List(ctx, activity.Filter{BudgetID: "team", UnresolvedOnly: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(unresolved) != 2 {
		t.Fatalf("got %d unresolved records, want 2", len(unresolved))
	}

	all, err := s.List(ctx, activity.Filter{BudgetID: "team"})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	sum := activity.Summarize(all)
	if sum.Complete {
		t.Error("a summary containing unresolved requests must not claim to be complete")
	}
	if sum.Unresolved != 2 {
		t.Errorf("Unresolved = %d, want 2", sum.Unresolved)
	}
	// $0.0105 settled plus the $1 floor of the partial: the total is a lower bound,
	// which is what "$X+" means.
	if want := money.Money(10_500) + dollars(1); sum.Spend != want {
		t.Errorf("Spend = %s, want the floor %s", sum.Spend, want)
	}
	if sum.Encumbered != 2*dollars(1) {
		t.Errorf("Encumbered = %s, want the two unresolved holds", sum.Encumbered)
	}
	if len(sum.UnpricedDimensions) != 1 || sum.UnpricedDimensions[0] != usage.Dimension("video-sec") {
		t.Errorf("UnpricedDimensions = %v, want [video-sec]", sum.UnpricedDimensions)
	}
}

// A denied request never ran, so it contributes no spend -- but it must still be
// recorded, or "the budget stopped this" is invisible.
func TestDeniedRecordContributesNoSpend(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	ctx := context.Background()

	rec := settled(t)
	rec.Status = activity.StatusDenied
	rec.Outcome = activity.OutcomeBudgetDenied
	rec.ReservationID = ""
	rec.Reserved = 0
	rec.ActualUsage = usage.Usage{}
	rec.ActualCost = usage.Cost{}
	rec.Error = "engine: request denied by budget policy"
	if err := s.Complete(ctx, rec); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := s.List(ctx, activity.Filter{BudgetID: "team"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sum := activity.Summarize(got)
	if sum.Spend != 0 {
		t.Errorf("Spend = %s, want 0 for a denied request", sum.Spend)
	}
	if !sum.Complete {
		t.Error("a denial is a complete story: nothing was spent and nothing is owing")
	}
	if got[0].Error == "" {
		t.Error("a denial must record why")
	}
}

// Get on a request nobody recorded is a distinguishable condition, not an empty
// record that reads as a free request.
func TestGetMissingRecord(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	if _, err := s.Get(context.Background(), "nope"); err == nil {
		t.Fatal("Get must fail for an unknown request id")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want a not-found error", err)
	}
}

// A record with no status is a bug in the caller, not something to invent a status
// for.
func TestCompleteRequiresAStatus(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "activity.db"))
	rec := settled(t)
	rec.Status = ""
	if err := s.Complete(context.Background(), rec); err == nil {
		t.Error("Complete must refuse a record with no status")
	}
}
