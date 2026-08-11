package bedrock_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"throttle/activity"
	activitysqlite "throttle/activity/sqlite"
	"throttle/engine"
	"throttle/pricing"
	"throttle/provider/bedrock"
	"throttle/usage"
)

// withActivity attaches a durable activity store to a harness and returns it, so
// the tests can assert on what was persisted rather than only on what was returned.
func withActivity(t *testing.T, path string) (*activitysqlite.Store, func(*bedrock.Config)) {
	t.Helper()
	store, err := activitysqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("activity open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, func(c *bedrock.Config) { c.Activity = store }
}

func getRecord(t *testing.T, s *activitysqlite.Store, requestID string) activity.Record {
	t.Helper()
	rec, err := s.Get(context.Background(), requestID)
	if err != nil {
		t.Fatalf("activity Get(%q): %v", requestID, err)
	}
	return rec
}

// The complete durable lifecycle for a successful call: estimate, capture a quote,
// reserve, execute, settle, and persist a content-free record of all of it.
func TestConverseTraversesTheDurableLifecycle(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", opt)

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID:  "team",
		RequestID: "req-1",
		Input:     request(sonnetID, aws.Int32(2000)),
		Metadata:  map[string]string{"workload": "nightly-eval"},
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if !res.Settled {
		t.Fatal("the request should have settled")
	}

	rec := getRecord(t, acts, "req-1")

	// Identity: the access provider and the exact provider model ID are independent
	// facts, and both must be recorded.
	if rec.Identity.AccessProvider != "aws-bedrock" {
		t.Errorf("AccessProvider = %q, want aws-bedrock", rec.Identity.AccessProvider)
	}
	if rec.Identity.ProviderModelID != sonnetID {
		t.Errorf("ProviderModelID = %q, want %q", rec.Identity.ProviderModelID, sonnetID)
	}
	if rec.Identity.Operation != "converse" {
		t.Errorf("Operation = %q, want converse", rec.Identity.Operation)
	}
	if rec.Identity.Region != "us-east-1" {
		t.Errorf("Region = %q, want us-east-1", rec.Identity.Region)
	}

	// The posture that actually governed the call. Nothing else in the database
	// remembers it, because posture is runtime policy and not part of the durable
	// budget definition.
	if rec.EnforcementMode != engine.ModeEnforce {
		t.Errorf("EnforcementMode = %q, want enforce", rec.EnforcementMode)
	}

	if rec.Status != activity.StatusSettled {
		t.Errorf("Status = %q, want settled", rec.Status)
	}
	if rec.Outcome != activity.OutcomeSuccess {
		t.Errorf("Outcome = %q, want success", rec.Outcome)
	}
	if !rec.ActualCost.Known() {
		t.Errorf("ActualCost = %s, want a known cost", rec.ActualCost)
	}
	if rec.ActualCost.Amount != res.Cost.Amount {
		t.Errorf("persisted cost %s != returned cost %s", rec.ActualCost, res.Cost)
	}
	if rec.ActualCost.Amount != res.Charge.ActualCost {
		t.Errorf("persisted cost %s != ledger charge %s", rec.ActualCost, res.Charge.ActualCost)
	}

	// Usage, both estimated and actual, keyed by canonical dimension constants.
	if got := rec.ActualUsage.Count(usage.InputTokens); got != 1000 {
		t.Errorf("actual input tokens = %d, want 1000", got)
	}
	if got := rec.ActualUsage.Count(usage.OutputTokens); got != 500 {
		t.Errorf("actual output tokens = %d, want 500", got)
	}
	if got := rec.Estimate.Usage.Count(usage.OutputTokens); got != 2000 {
		t.Errorf("estimated output tokens = %d, want the requested 2000 ceiling", got)
	}
	if rec.Estimate.Quality == "" {
		t.Error("estimate quality must be recorded, or accuracy cannot be measured later")
	}

	// The reservation and its scope legs.
	if rec.ReservationID == "" {
		t.Error("ReservationID must be recorded")
	}
	if rec.Reserved != res.Estimate.Cost.Amount {
		t.Errorf("Reserved = %s, want the estimated %s", rec.Reserved, res.Estimate.Cost.Amount)
	}
	if len(rec.Scopes) == 0 || rec.Scopes[0].BudgetID != "team" {
		t.Errorf("Scopes = %+v, want the team leg", rec.Scopes)
	}
	if rec.Scopes[0].PeriodID == "" {
		t.Error("a scope must name its period, or spend cannot be attributed to one")
	}

	// The captured quote, with provenance, and it must still price the same.
	if !rec.Quote.Valid() {
		t.Fatal("the captured quote must be persisted")
	}
	if rec.Quote.Provenance.Source == "" {
		t.Error("the quote must carry its provenance")
	}
	priced, err := rec.Quote.Price(rec.ActualUsage)
	if err != nil {
		t.Fatalf("re-pricing from the persisted quote: %v", err)
	}
	if priced.Cost.Amount != rec.ActualCost.Amount {
		t.Errorf("replayed cost = %s, want the recorded %s", priced.Cost, rec.ActualCost)
	}

	// Timings and attribution.
	if rec.StartedAt.IsZero() || rec.CompletedAt.IsZero() {
		t.Error("both timestamps must be recorded")
	}
	if rec.ProviderLatency == 0 {
		t.Error("the provider's own reported latency must be recorded")
	}
	if rec.Metadata["workload"] != "nightly-eval" {
		t.Errorf("Metadata = %v, want the caller's attribution", rec.Metadata)
	}

	// And no content, anywhere. The prompt text used by the test harness must not
	// appear in the record.
	assertNoContent(t, rec)
}

// Usage and cost observability must remain content-free: knowing what a request
// cost does not require knowing what it said.
func assertNoContent(t *testing.T, rec activity.Record) {
	t.Helper()
	const prompt = "airspeed velocity"
	const reply = "hello"
	for _, s := range []string{
		rec.Error, rec.RequestID, rec.ReservationID, rec.BudgetID,
		rec.Identity.ProviderModelID, rec.Identity.CanonicalModel,
		rec.Estimate.Note, string(rec.Status), string(rec.Outcome),
		rec.ActualCost.Reason, rec.Quote.Provenance.Source,
	} {
		if strings.Contains(s, prompt) || strings.Contains(s, reply) {
			t.Errorf("record field %q contains request or response content", s)
		}
	}
	for k, v := range rec.Metadata {
		if strings.Contains(v, prompt) || strings.Contains(v, reply) {
			t.Errorf("metadata %q = %q contains content", k, v)
		}
	}
}

// A record must survive the process that wrote it. The unresolved liability this
// test leaves behind is exactly what a later reconciliation reads.
func TestActivitySurvivesProcessRestart(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/activity.db"

	store, opt := withActivity(t, path)
	h := newHarness(t, "1000", opt)

	if _, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	}); err != nil {
		t.Fatalf("Converse: %v", err)
	}
	before := getRecord(t, store, "req-1")
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// A wholly new handle on the same file, as after a restart.
	reopened, err := activitysqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	after, err := reopened.Get(context.Background(), "req-1")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if after.ActualCost.Amount != before.ActualCost.Amount || !after.ActualCost.Known() {
		t.Errorf("cost after restart = %s, want %s", after.ActualCost, before.ActualCost)
	}
	if after.Identity != before.Identity {
		t.Errorf("identity after restart = %+v, want %+v", after.Identity, before.Identity)
	}
	if after.EnforcementMode != before.EnforcementMode {
		t.Errorf("mode after restart = %q, want %q", after.EnforcementMode, before.EnforcementMode)
	}
	if !after.Quote.Valid() {
		t.Error("the quote must survive the restart")
	}
}

// A price refresh landing between admission and settlement must not change what an
// in-flight request costs. The catalog is mutated while the provider call is
// blocked, which is the real shape of the race.
func TestSettlementUsesTheQuoteCapturedAtAdmission(t *testing.T) {
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "15.00")),
		},
		Provenance: pricing.Provenance{Source: "test"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}

	acts, actOpt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", actOpt, func(c *bedrock.Config) { c.Catalog = cat })

	// The provider call blocks until the catalog has been refreshed underneath it.
	release := make(chan struct{})
	h.api.block = release
	h.api.out = response(1_000_000, 1_000_000)

	done := make(chan error, 1)
	var res *bedrock.Result
	go func() {
		var err error
		res, err = h.client.Converse(context.Background(), bedrock.Request{
			BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(1_000_000)),
		})
		done <- err
	}()

	// Wait until the request is actually admitted and in the provider call, then
	// change the prices out from under it.
	waitFor(t, func() bool { return h.api.callCount() > 0 })
	if err := cat.Override(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "300.00")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "1500.00")),
		},
	}); err != nil {
		t.Fatalf("Override: %v", err)
	}
	// The refresh must actually have landed, or this test proves nothing: the live
	// catalog now says $1800 for the same usage.
	live, err := cat.Quote(context.Background(), usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		Operation:       "converse",
		Region:          "us-east-1",
	}, usage.New(map[usage.Dimension]int64{
		usage.InputTokens:  1_000_000,
		usage.OutputTokens: 1_000_000,
	}), now)
	if err != nil {
		t.Fatalf("live Quote: %v", err)
	}
	if live.Cost.Amount != dollars(t, "1800.00") {
		t.Fatalf("the live catalog says %s, want $1800.00: the refresh did not land, so this test is vacuous",
			live.Cost)
	}
	close(release)

	if err := <-done; err != nil {
		t.Fatalf("Converse: %v", err)
	}

	// $3 + $15 at the captured rates. The refreshed catalog would have said $1800.
	if want := dollars(t, "18.00"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s from the rates captured at admission",
			res.Charge.ActualCost, want)
	}
	rec := getRecord(t, acts, "req-1")
	if rec.ActualCost.Amount != dollars(t, "18.00") {
		t.Errorf("persisted cost = %s, want the captured basis", rec.ActualCost)
	}
	// The persisted quote is the pre-refresh one, so the record stays reproducible.
	if r, ok := rec.Quote.Rate(usage.InputTokens); !ok || r.PerUnit != dollars(t, "3.00") {
		t.Errorf("persisted input rate = %+v, want the $3.00 rate captured at admission", r)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the provider call to start")
		}
		time.Sleep(time.Millisecond)
	}
}

// A price known at admission must still be known at settlement. Without the
// captured quote this would depend on the catalog not having changed.
func TestKnownPriceAtBeginRemainsKnownAtSettle(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", opt)

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if !res.Estimate.Cost.Known() {
		t.Fatal("the estimate should have been priced")
	}
	if !res.Cost.Known() {
		t.Error("a request priced at admission must still be priced at settlement")
	}
	rec := getRecord(t, acts, "req-1")
	if !rec.Estimate.Cost.Known() || !rec.ActualCost.Known() {
		t.Errorf("persisted estimate/actual = %s/%s, want both known", rec.Estimate.Cost, rec.ActualCost)
	}
}

const unpricedModelID = "anthropic.model-released-yesterday-v1:0"

// Enforce mode cannot honestly enforce a dollar budget against a request whose
// monetary exposure it cannot determine, so it refuses -- and refuses before
// spending any money, which is the only refusal worth anything.
func TestEnforceRejectsUnknownPriceBeforeCallingTheProvider(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", opt)

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(unpricedModelID, aws.Int32(2000)),
	})
	if !errors.Is(err, engine.ErrCostUnknown) {
		t.Fatalf("error = %v, want engine.ErrCostUnknown", err)
	}
	// The point of refusing at admission: the provider was never called, so nothing
	// was billed.
	if h.api.callCount() != 0 {
		t.Errorf("the provider was called %d times, want 0", h.api.callCount())
	}
	if res.Settled {
		t.Error("a denied request must not report itself settled")
	}
	if tot := h.totals(t); tot.Spent != 0 || tot.Reserved != 0 {
		t.Errorf("spent %s, reserved %s, want an untouched budget", tot.Spent, tot.Reserved)
	}

	// The denial is still recorded: "the budget stopped this" is invisible otherwise.
	rec := getRecord(t, acts, "req-1")
	if rec.Status != activity.StatusDenied {
		t.Errorf("Status = %q, want denied", rec.Status)
	}
	if rec.Outcome != activity.OutcomeUnpriced {
		t.Errorf("Outcome = %q, want unpriced: the reason was a pricing gap, not a spent budget", rec.Outcome)
	}
	if rec.EnforcementMode != engine.ModeEnforce {
		t.Errorf("EnforcementMode = %q, want enforce", rec.EnforcementMode)
	}
	if rec.ActualCost.Known() {
		t.Error("a denied unpriced request must not record a known cost")
	}
	if rec.Error == "" {
		t.Error("a denial must record why")
	}
}

// Monitor mode observes rather than governs, so it permits an unpriced request --
// but it must persist the cost as explicitly unknown, never as zero.
func TestMonitorPermitsUnknownPriceAndPersistsCostUnknown(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", opt)
	if err := h.engine.SetMode("team", engine.ModeMonitor); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(unpricedModelID, aws.Int32(2000)),
	})
	if !errors.Is(err, bedrock.ErrCostUnresolved) {
		t.Fatalf("error = %v, want bedrock.ErrCostUnresolved", err)
	}
	if h.api.callCount() != 1 {
		t.Errorf("the provider was called %d times, want 1: monitor mode must not block", h.api.callCount())
	}
	// The response is still handed back: the caller paid for it.
	if res.Output == nil {
		t.Error("the provider response must be returned even when the cost is unknown")
	}
	if res.Cost.Known() {
		t.Errorf("Cost = %s, want an explicit unknown", res.Cost)
	}
	if !res.Unresolved {
		t.Error("Result.Unresolved must be set")
	}
	// Usage is known even though cost is not, which is the whole reason the two are
	// separate fields.
	if got := res.Usage.Count(usage.InputTokens); got != 1000 {
		t.Errorf("input tokens = %d, want 1000: usage is knowable even when price is not", got)
	}

	rec := getRecord(t, acts, "req-1")
	if rec.EnforcementMode != engine.ModeMonitor {
		t.Errorf("EnforcementMode = %q, want monitor: a reader must be able to tell the ceiling did not apply",
			rec.EnforcementMode)
	}
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", rec.Status)
	}
	if rec.ActualCost.State() != usage.CostUnknown {
		t.Errorf("cost completeness = %q, want unknown", rec.ActualCost.State())
	}
	if rec.ActualCost.Reason == "" {
		t.Error("an unknown cost must say why")
	}
	if rec.ActualUsage.Count(usage.OutputTokens) != 500 {
		t.Error("usage must be preserved even when it cannot be priced")
	}
}

// The failure this whole design exists to prevent: an unknown cost showing up as
// $0.00 and quietly understating spend.
func TestUnknownCostNeverAppearsAsZero(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", opt)
	if err := h.engine.SetMode("team", engine.ModeMonitor); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	if _, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(unpricedModelID, aws.Int32(2000)),
	}); !errors.Is(err, bedrock.ErrCostUnresolved) {
		t.Fatalf("error = %v, want ErrCostUnresolved", err)
	}

	rec := getRecord(t, acts, "req-1")
	// A known zero and an unknown cost must not render the same way.
	if rec.ActualCost.String() == usage.KnownCost(0).String() {
		t.Errorf("an unknown cost rendered as %q, indistinguishable from a free request",
			rec.ActualCost.String())
	}
	if rec.ActualCost.Known() {
		t.Error("the cost must not read back as known")
	}

	// And the summary refuses to claim completeness.
	all, err := acts.List(context.Background(), activity.Filter{BudgetID: "team"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sum := activity.Summarize(all)
	if sum.Complete {
		t.Error("a period containing an unpriced request must not report a complete total")
	}
	if sum.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1", sum.Unresolved)
	}
}

// noCacheCatalog prices input and output but not cache tokens, standing in for a
// price sheet that predates a dimension the provider has started billing for.
func noCacheCatalog(t *testing.T) *pricing.Static {
	t.Helper()
	cat, err := pricing.NewStatic(pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: sonnetID,
		Rates: map[usage.Dimension]pricing.Rate{
			usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(t, "3.00")),
			usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(t, "15.00")),
		},
		Provenance: pricing.Provenance{Source: "test", Version: "no-cache-rates"},
	})
	if err != nil {
		t.Fatalf("NewStatic: %v", err)
	}
	return cat
}

// A dimension the provider bills for that the captured quote has no rate for
// leaves an unresolved liability: usage preserved, hold encumbered, request marked
// as owing a price. Settling only the dimensions we understand would report a
// number that is definitely too low as though it were right.
func TestUnexpectedBillableDimensionLeavesAnUnresolvedLiability(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	cat := noCacheCatalog(t)
	h := newHarness(t, "1000", opt, func(c *bedrock.Config) { c.Catalog = cat })

	// Bedrock starts reporting cache tokens for a model whose price sheet has no
	// rate for them.
	out := response(1000, 500)
	out.Usage.CacheWriteInputTokens = aws.Int32(4000)
	h.api.out = out

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if !errors.Is(err, bedrock.ErrCostUnresolved) {
		t.Fatalf("error = %v, want ErrCostUnresolved", err)
	}
	if res.Settled {
		t.Error("a request with an unpriceable dimension must not settle")
	}
	if !res.Unresolved {
		t.Error("Result.Unresolved must be set")
	}

	rec := getRecord(t, acts, "req-1")
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("Status = %q, want unresolved", rec.Status)
	}
	// The usage is preserved in full, including the dimension that could not be
	// priced -- it is the only evidence of what is owing.
	if got := rec.ActualUsage.Count(usage.CacheWriteTokens); got != 4000 {
		t.Errorf("cache write tokens = %d, want 4000 preserved", got)
	}
	if rec.ActualCost.Completeness != usage.CostPartial {
		t.Errorf("completeness = %q, want partial: input and output were priced", rec.ActualCost.Completeness)
	}
	if len(rec.ActualCost.Unpriced) == 0 {
		t.Error("a partial cost must name the dimensions it could not price")
	}
	found := false
	for _, d := range rec.ActualCost.Unpriced {
		if d == usage.CacheWriteTokens {
			found = true
		}
	}
	if !found {
		t.Errorf("Unpriced = %v, want the cache write dimension named", rec.ActualCost.Unpriced)
	}

	// The reserved amount stays encumbered: the money was spent, so the budget must
	// not offer it to the next caller.
	tot := h.totals(t)
	if tot.Reserved != res.Estimate.Cost.Amount {
		t.Errorf("Reserved = %s, want the hold still encumbered at %s", tot.Reserved, res.Estimate.Cost.Amount)
	}
	if tot.Spent != 0 {
		t.Errorf("Spent = %s, want 0: a floor must not be settled as a total", tot.Spent)
	}
}

// An unresolved liability keeps consuming its reserved headroom, so a budget never
// offers money that has already been spent.
func TestUnresolvedLiabilityKeepsConsumingReservedHeadroom(t *testing.T) {
	_, opt := withActivity(t, t.TempDir()+"/activity.db")
	cat := noCacheCatalog(t)
	h := newHarness(t, "1000", opt, func(c *bedrock.Config) { c.Catalog = cat })

	out := response(1000, 500)
	out.Usage.CacheWriteInputTokens = aws.Int32(4000)
	h.api.out = out

	if _, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	}); !errors.Is(err, bedrock.ErrCostUnresolved) {
		t.Fatalf("error = %v, want ErrCostUnresolved", err)
	}

	before := h.totals(t)
	if before.Reserved == 0 {
		t.Fatal("the unresolved liability must still hold its reservation")
	}

	// A second request runs against the reduced headroom, which proves the first
	// hold was not quietly released.
	h.api.out = response(1000, 500)
	if _, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-2", Input: request(sonnetID, aws.Int32(2000)),
	}); err != nil {
		t.Fatalf("second Converse: %v", err)
	}
	after := h.totals(t)
	if after.Reserved != before.Reserved {
		t.Errorf("Reserved = %s, want the first liability's %s still encumbered",
			after.Reserved, before.Reserved)
	}
	if after.Spent == 0 {
		t.Error("the second, fully priced request should have settled")
	}
}

// A provider error with no usage means nothing was billed, so the hold goes back
// and the record says so.
func TestProviderErrorReleasesAndRecords(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", opt)
	h.api.out = nil
	h.api.err = errors.New("ValidationException: bad request")

	if _, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	}); !errors.Is(err, bedrock.ErrProvider) {
		t.Fatalf("error = %v, want ErrProvider", err)
	}

	rec := getRecord(t, acts, "req-1")
	if rec.Status != activity.StatusReleased {
		t.Errorf("Status = %q, want released", rec.Status)
	}
	if rec.Outcome != activity.OutcomeProviderError {
		t.Errorf("Outcome = %q, want provider-error", rec.Outcome)
	}
	// Nothing was billed, so a known zero is the truth here -- unlike an unpriced
	// request, where zero would be a lie.
	if !rec.ActualCost.Known() || rec.ActualCost.Amount != 0 {
		t.Errorf("ActualCost = %s, want a known zero", rec.ActualCost)
	}
	if _, complete := rec.Spent(); !complete {
		t.Error("a released request is a complete story and must not make a total incomplete")
	}
	if tot := h.totals(t); tot.Reserved != 0 {
		t.Errorf("Reserved = %s, want the hold returned", tot.Reserved)
	}
}

// A cancelled call leaves the outcome genuinely unknown: the provider may have
// served and billed it, and no response came back to prove otherwise. The hold
// stays, and the record says the outcome is unknown rather than free.
func TestCancelledCallRecordsAnOutstandingHold(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", opt)

	block := make(chan struct{})
	defer close(block)
	h.api.block = block

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.client.Converse(ctx, bedrock.Request{
			BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
		})
		done <- err
	}()

	// Cancel only once the provider call is genuinely in flight, so the outcome is
	// ambiguous rather than merely pre-empted.
	waitFor(t, func() bool { return h.api.callCount() > 0 })
	cancel()

	if err := <-done; !errors.Is(err, bedrock.ErrOutcomeUnknown) {
		t.Fatalf("error = %v, want ErrOutcomeUnknown", err)
	}

	rec := getRecord(t, acts, "req-1")
	if rec.Status != activity.StatusOutstanding {
		t.Errorf("Status = %q, want outstanding", rec.Status)
	}
	if rec.Outcome != activity.OutcomeCancelled {
		t.Errorf("Outcome = %q, want cancelled", rec.Outcome)
	}
	if rec.ActualCost.Known() {
		t.Error("an interrupted call must not record a known cost")
	}
	if tot := h.totals(t); tot.Reserved == 0 {
		t.Error("the hold must be left in place when the outcome is ambiguous")
	}
}

// The pre-call write is what makes a crashed request visible. Without it, a process
// that dies mid-call is indistinguishable from one that never ran.
func TestActivityIsWrittenBeforeTheProviderIsCalled(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", opt)

	block := make(chan struct{})
	h.api.block = block

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.client.Converse(context.Background(), bedrock.Request{
			BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
		})
	}()

	// While the provider call is in flight, a pending record must already exist.
	waitFor(t, func() bool { return h.api.callCount() > 0 })
	waitFor(t, func() bool {
		_, err := acts.Get(context.Background(), "req-1")
		return err == nil
	})

	rec := getRecord(t, acts, "req-1")
	if rec.Status != activity.StatusPending {
		t.Errorf("in-flight Status = %q, want pending", rec.Status)
	}
	if !rec.Quote.Valid() {
		t.Error("the pre-call record must carry the captured quote, or a crashed request cannot be priced")
	}
	if rec.ActualCost.Known() {
		t.Error("an in-flight request has no known cost yet")
	}
	if rec.ReservationID == "" {
		t.Error("the pre-call record must name the hold it is consuming")
	}

	close(block)
	<-done

	if got := getRecord(t, acts, "req-1"); got.Status != activity.StatusSettled {
		t.Errorf("final Status = %q, want settled", got.Status)
	}
}

// A store failure must never fail a provider call the caller has already paid for.
func TestActivityFailureDoesNotFailTheRequest(t *testing.T) {
	h := newHarness(t, "1000", func(c *bedrock.Config) { c.Activity = brokenStore{} })

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("a telemetry failure must not fail the request: %v", err)
	}
	if !res.Settled {
		t.Error("the request must still settle")
	}
}

// brokenStore fails every write, standing in for a misconfigured activity database.
type brokenStore struct{}

func (brokenStore) Begin(context.Context, activity.Record) error { return errors.New("disk on fire") }
func (brokenStore) Complete(context.Context, activity.Record) error {
	return errors.New("disk on fire")
}
func (brokenStore) Get(context.Context, string) (activity.Record, error) {
	return activity.Record{}, errors.New("disk on fire")
}
func (brokenStore) List(context.Context, activity.Filter) ([]activity.Record, error) {
	return nil, errors.New("disk on fire")
}

// A client with no activity store still works. Recording is optional; governing is
// not.
func TestConverseWorksWithoutAnActivityStore(t *testing.T) {
	h := newHarness(t, "1000")
	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}
	if !res.Settled {
		t.Error("the request must settle without an activity store")
	}
}

// Canonical dimension constants must round-trip through the whole path: the
// provider's own field names are normalized into throttle's dimensions once, and
// every layer downstream keys on the constants rather than on string literals.
func TestCanonicalDimensionsRoundTripThroughTheLifecycle(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newHarness(t, "1000", opt)

	h.api.out = &bedrockruntime.ConverseOutput{
		Usage: &brtypes.TokenUsage{
			InputTokens:           aws.Int32(1000),
			OutputTokens:          aws.Int32(500),
			TotalTokens:           aws.Int32(1500),
			CacheReadInputTokens:  aws.Int32(200),
			CacheWriteInputTokens: aws.Int32(0),
		},
		Metrics:    &brtypes.ConverseMetrics{LatencyMs: aws.Int64(10)},
		StopReason: brtypes.StopReasonEndTurn,
	}

	res, err := h.client.Converse(context.Background(), bedrock.Request{
		BudgetID: "team", RequestID: "req-1", Input: request(sonnetID, aws.Int32(2000)),
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}

	for _, d := range []usage.Dimension{usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens} {
		if _, ok := res.Usage.Get(d); !ok {
			t.Errorf("normalized usage is missing the canonical dimension %s", d)
		}
	}
	rec := getRecord(t, acts, "req-1")
	if got := rec.ActualUsage.Count(usage.CacheReadTokens); got != 200 {
		t.Errorf("persisted cache read tokens = %d, want 200 under the canonical key", got)
	}
	// A zero-count dimension the provider reported is preserved as reported through
	// persistence: absent and zero are different facts, and only Get can tell them
	// apart.
	if _, ok := rec.ActualUsage.Get(usage.CacheWriteTokens); !ok {
		t.Error("a dimension the provider reported as zero must not be dropped")
	}
}
