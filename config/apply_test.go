package config

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"throttle/budget"
	"throttle/ledger"
	"throttle/ledger/sqlite"
)

// Applying a plan.
//
// Against the real ledger rather than a fake. The properties that matter here -- that a
// concurrent edit is detected instead of overwritten, that two identical applies converge,
// that a cycle is rejected -- are properties of the store's transactions. A fake that
// answered the way this package hopes would test the hope.

func newStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// storedIn reads every definition with its revision, the way the CLI does.
func storedIn(t *testing.T, store *sqlite.Store) []Stored {
	t.Helper()
	ctx := context.Background()
	defs, err := store.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	out := make([]Stored, 0, len(defs))
	for _, def := range defs {
		_, revision, err := store.Definition(ctx, def.ID)
		if err != nil {
			t.Fatalf("Definition(%q): %v", def.ID, err)
		}
		out = append(out, Stored{Definition: def, Revision: revision})
	}
	return out
}

// applyTo plans against what is stored and applies it, which is what the CLI does in one
// command.
func applyTo(t *testing.T, store *sqlite.Store, cfg []budget.Definition, now time.Time) (Result, error) {
	t.Helper()
	return Apply(context.Background(), store, NewPlan(cfg, storedIn(t, store), now))
}

func mustApply(t *testing.T, store *sqlite.Store, cfg []budget.Definition, now time.Time) Result {
	t.Helper()
	res, err := applyTo(t, store, cfg, now)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return res
}

func definitionOf(t *testing.T, store *sqlite.Store, id string) (budget.Definition, int) {
	t.Helper()
	def, revision, err := store.Definition(context.Background(), id)
	if err != nil {
		t.Fatalf("Definition(%q): %v", id, err)
	}
	return def, revision
}

// (2, 16) A create is applied, and the file then agrees with the ledger.
func TestApplyCreatesAndConverges(t *testing.T) {
	store := newStore(t)
	parent := monthly("research", 4000)
	child := monthly("chat", 1000)
	child.ParentID = "research"

	cfg := []budget.Definition{child, parent} // Deliberately child first.
	res := mustApply(t, store, cfg, planNow)

	if len(res.Created) != 2 {
		t.Errorf("Created = %v, want both budgets", res.Created)
	}

	// The hierarchy is stored, including the link.
	def, _ := definitionOf(t, store, "chat")
	if def.ParentID != "research" {
		t.Errorf("chat's parent = %q, want research", def.ParentID)
	}

	// (16) Applying and then diffing again reports nothing to do. A tool whose apply does
	// not converge asks its user to run it forever.
	after := NewPlan(cfg, storedIn(t, store), planNow)
	if after.Mutates() {
		var b strings.Builder
		_ = after.Render(&b)
		t.Errorf("a second plan still wants to change something:\n%s", b.String())
	}
	if got := after.Counts()[ActionNoop]; got != 2 {
		t.Errorf("noop count = %d, want 2", got)
	}
}

// (3) A second apply of the same file writes nothing and says so.
func TestApplyIsIdempotent(t *testing.T) {
	store := newStore(t)
	cfg := []budget.Definition{monthly("research", 4000)}

	mustApply(t, store, cfg, planNow)
	_, firstRevision := definitionOf(t, store, "research")

	res := mustApply(t, store, cfg, planNow)
	if len(res.Created) != 0 || len(res.Updated) != 0 {
		t.Errorf("a repeated apply wrote something: %+v", res)
	}
	if res.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1", res.Unchanged)
	}
	if _, revision := definitionOf(t, store, "research"); revision != firstRevision {
		t.Errorf("revision moved from %d to %d without a change", firstRevision, revision)
	}
}

// (4, 10, 11, 17) An update changes future periods and leaves the materialized one alone.
//
// This is the invariant the whole design turns on: money already spent in this period was
// governed by the old terms, and a YAML edit does not retroactively change what a budget was
// allowed to spend.
func TestApplyUpdateLeavesTheCurrentPeriodAlone(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	mustApply(t, store, []budget.Definition{monthly("research", 4000)}, planNow)

	// Materialize the current period, as any status query would.
	before, err := store.EnsurePeriod(ctx, "research", planNow)
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	if before.Envelope.Allocation != dollars(4000) {
		t.Fatalf("the materialized period allocates %s, want $4000", before.Envelope.Allocation)
	}

	res := mustApply(t, store, []budget.Definition{monthly("research", 5000)}, planNow)
	if len(res.Updated) != 1 {
		t.Fatalf("Updated = %v, want research", res.Updated)
	}

	// (10) The materialized period is byte-for-byte the envelope it was created with.
	after, err := store.Period(ctx, before.ID)
	if err != nil {
		t.Fatalf("Period: %v", err)
	}
	if after.Envelope.Allocation != dollars(4000) {
		t.Errorf("the current period now allocates %s; an applied definition rewrote history",
			after.Envelope.Allocation)
	}
	if !after.Envelope.Start.Equal(before.Envelope.Start) || !after.Envelope.End.Equal(before.Envelope.End) {
		t.Error("the current period's bounds moved")
	}

	// (11) The next period uses the new definition.
	next, err := store.EnsurePeriod(ctx, "research", before.Envelope.End.Add(time.Hour))
	if err != nil {
		t.Fatalf("EnsurePeriod for the next period: %v", err)
	}
	if next.ID == before.ID {
		t.Fatal("the next period is the same period")
	}
	if next.Envelope.Allocation != dollars(5000) {
		t.Errorf("the next period allocates %s, want the new $5000", next.Envelope.Allocation)
	}

	// (17) And no unrelated period was brought into existence by applying: only the one the
	// test materialized itself, plus the successor it just asked for.
	periods, err := store.Periods(ctx, "research")
	if err != nil {
		t.Fatalf("Periods: %v", err)
	}
	if len(periods) != 2 {
		t.Errorf("the ledger holds %d periods, want the 2 this test materialized", len(periods))
	}
}

// (17) Apply materializes nothing. A definition says what a budget's rules are; it does not
// decide that its first period has begun.
func TestApplyMaterializesNoPeriods(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	parent := monthly("research", 4000)
	child := monthly("chat", 1000)
	child.ParentID = "research"
	mustApply(t, store, []budget.Definition{parent, child}, planNow)

	for _, id := range []string{"research", "chat"} {
		periods, err := store.Periods(ctx, id)
		if err != nil {
			t.Fatalf("Periods(%q): %v", id, err)
		}
		if len(periods) != 0 {
			t.Errorf("%s has %d materialized periods after apply, want 0", id, len(periods))
		}
	}
}

// (5) Apply never deletes. A budget missing from the file keeps its definition and its
// revision.
func TestApplyNeverDeletes(t *testing.T) {
	store := newStore(t)
	mustApply(t, store, []budget.Definition{monthly("legacy", 100), monthly("current", 200)}, planNow)
	_, legacyRevision := definitionOf(t, store, "legacy")

	// The file now mentions only one of them, as an incomplete file does.
	res := mustApply(t, store, []budget.Definition{monthly("current", 200)}, planNow)
	if len(res.Created) != 0 || len(res.Updated) != 0 {
		t.Errorf("apply wrote something: %+v", res)
	}

	def, revision := definitionOf(t, store, "legacy")
	if def.Allocation != dollars(100) {
		t.Errorf("legacy now allocates %s, want the stored $100", def.Allocation)
	}
	if revision != legacyRevision {
		t.Errorf("legacy's revision moved from %d to %d", legacyRevision, revision)
	}
}

// (6) A name-only difference is not applied by apply, at the ledger level and not merely in
// the plan.
func TestApplyDoesNotRename(t *testing.T) {
	store := newStore(t)
	original := monthly("research", 4000)
	original.Name = "Research group"
	mustApply(t, store, []budget.Definition{original}, planNow)

	renamed := original
	renamed.Name = "Research programme"
	res := mustApply(t, store, []budget.Definition{renamed}, planNow)
	if len(res.Updated) != 0 {
		t.Errorf("Updated = %v, want nothing: a rename is explicit", res.Updated)
	}

	if def, _ := definitionOf(t, store, "research"); def.Name != "Research group" {
		t.Errorf("the stored name is now %q; apply renamed a budget", def.Name)
	}
}

// (12) A definition that changed between planning and applying fails safely.
//
// The plan was computed against terms that are no longer stored, so its update is a decision
// about superseded facts. Last-write-wins here would let a stale process silently restore an
// allocation somebody had just corrected.
func TestApplyRefusesAStalePlan(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	mustApply(t, store, []budget.Definition{monthly("research", 4000)}, planNow)

	// A plan computed now, before anything else happens.
	plan := NewPlan([]budget.Definition{monthly("research", 5000)}, storedIn(t, store), planNow)

	// Somebody else changes the definition in between.
	stored, revision := definitionOf(t, store, "research")
	stored.Allocation = dollars(4500)
	if err := store.UpdateDefinition(ctx, stored, revision); err != nil {
		t.Fatalf("the interloping update: %v", err)
	}

	_, err := Apply(ctx, store, plan)
	if !errors.Is(err, ledger.ErrRevisionMismatch) {
		t.Fatalf("error = %v, want ErrRevisionMismatch", err)
	}
	// The message has to say what to do, or a user faced with it has no next move.
	if !strings.Contains(err.Error(), "config diff") {
		t.Errorf("the error does not say to re-run diff:\n%v", err)
	}

	// And the interloper's value stands: nothing was overwritten.
	if def, _ := definitionOf(t, store, "research"); def.Allocation != dollars(4500) {
		t.Errorf("allocation = %s, want the interloper's $4500 intact", def.Allocation)
	}
}

// (13) Two processes applying the same configuration converge.
//
// Identical creation attempts are idempotent at the store, so both succeed and neither
// clobbers the other. This runs under -race, which is the point: the plan is shared, the
// applies are concurrent, and the outcome must be one definition with the configured terms.
func TestConcurrentIdenticalAppliesConverge(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	parent := monthly("research", 4000)
	child := monthly("chat", 1000)
	child.ParentID = "research"
	cfg := []budget.Definition{child, parent}

	const workers = 4
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each worker plans against nothing, as a first-run process would, so all
			// four attempt the same creates.
			_, err := Apply(ctx, store, NewPlan(cfg, nil, planNow))
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}()
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Errorf("a concurrent apply of an identical configuration failed: %v", err)
		}
	}

	defs, err := store.Definitions(ctx)
	if err != nil {
		t.Fatalf("Definitions: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("the ledger holds %d definitions, want 2", len(defs))
	}
	for _, def := range defs {
		if def.ID == "research" && def.Allocation != dollars(4000) {
			t.Errorf("research allocates %s, want $4000", def.Allocation)
		}
	}
	// Created once, never rewritten: converging must not mean four successive writes.
	if _, revision := definitionOf(t, store, "research"); revision != 1 {
		t.Errorf("revision = %d, want 1: converging applies rewrote the definition", revision)
	}
}

// A create that races a *different* definition under the same id is a conflict, loudly.
func TestApplyReportsAConflictingCreate(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	// A plan that believes nothing is stored.
	plan := NewPlan([]budget.Definition{monthly("research", 4000)}, nil, planNow)

	// Something else gets there first, with different terms.
	if err := store.PutDefinition(ctx, monthly("research", 9000)); err != nil {
		t.Fatalf("PutDefinition: %v", err)
	}

	_, err := Apply(ctx, store, plan)
	if !errors.Is(err, ledger.ErrDefinitionConflict) {
		t.Fatalf("error = %v, want ErrDefinitionConflict", err)
	}
	if def, _ := definitionOf(t, store, "research"); def.Allocation != dollars(9000) {
		t.Errorf("allocation = %s, want the existing $9000 untouched", def.Allocation)
	}
}

// (14) A cycle is rejected by the ledger, and apply surfaces it rather than papering over it.
//
// Configuration validation catches cycles too, so reaching this means somebody assembled a
// plan another way. It must still be refused: a cycle makes "the set of scopes a request
// consumes" undefined.
func TestApplyRejectsAParentCycle(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	a := monthly("a", 100)
	b := monthly("b", 100)
	b.ParentID = "a"
	mustApply(t, store, []budget.Definition{a, b}, planNow)

	// a becomes a child of b, closing the loop. Planned directly, since Compare would
	// classify it as a parent change and refuse it earlier.
	cycle := a
	cycle.ParentID = "b"
	_, revision := definitionOf(t, store, "a")
	plan := Plan{Steps: []Step{{
		BudgetID: "a", Action: ActionUpdate, Config: cycle, Stored: a, Revision: revision,
		Fields: []string{"parent"},
	}}}

	if _, err := Apply(ctx, store, plan); !errors.Is(err, ledger.ErrCycle) {
		t.Fatalf("error = %v, want ErrCycle", err)
	}
	if def, _ := definitionOf(t, store, "a"); def.ParentID != "" {
		t.Errorf("a's parent = %q, want none: the cycle was written", def.ParentID)
	}
}

// (15) A plan containing a refusal applies nothing at all.
//
// Applying the safe half and reporting the rest as an error leaves the ledger matching
// neither the file nor what it was before, and the operator has to work out which half
// landed.
func TestApplyAppliesNothingWhenAnythingIsRefused(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	institute := monthly("institute", 9000)
	research := monthly("research", 4000)
	research.ParentID = "institute"
	mustApply(t, store, []budget.Definition{institute, research}, planNow)

	// One safe create alongside one refused reparent.
	reparented := research
	reparented.ParentID = ""
	fresh := monthly("fresh", 100)

	_, err := applyTo(t, store, []budget.Definition{institute, reparented, fresh}, planNow)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("error = %v, want ErrRefused", err)
	}
	if !strings.Contains(err.Error(), "Nothing was applied") {
		t.Errorf("the error does not say nothing was applied:\n%v", err)
	}

	// The safe create did not land either.
	if _, _, err := store.Definition(ctx, "fresh"); !errors.Is(err, ledger.ErrBudgetNotFound) {
		t.Errorf("Definition(fresh) = %v, want not found: half the plan was applied", err)
	}
	if def, _ := definitionOf(t, store, "research"); def.ParentID != "institute" {
		t.Errorf("research's parent = %q, want institute untouched", def.ParentID)
	}
}

// (7, 8, 9) An explicit rename changes the display name and nothing else.
func TestRenameChangesOnlyTheName(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	parent := monthly("research", 4000)
	parent.Name = "Research group"
	child := monthly("chat", 1000)
	child.ParentID = "research"
	child.Name = "Chat"
	mustApply(t, store, []budget.Definition{parent, child}, planNow)

	// A period exists and holds money, so a rename has something it could damage.
	period, err := store.EnsurePeriod(ctx, "research", planNow)
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	beforeTotals, err := store.Totals(ctx, ledger.Scope{BudgetID: "research", PeriodID: period.ID}, planNow)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}

	before, _ := definitionOf(t, store, "research")
	if err := Rename(ctx, store, "research", "Research programme"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	after, _ := definitionOf(t, store, "research")

	// (7) The durable identity is the same budget.
	if after.ID != before.ID {
		t.Errorf("the budget id changed from %q to %q", before.ID, after.ID)
	}
	if after.Name != "Research programme" {
		t.Errorf("Name = %q, want the new name", after.Name)
	}

	// (8) The financial fingerprint is untouched, which is the formal statement of "a name is
	// metadata".
	if after.Fingerprint() != before.Fingerprint() {
		t.Errorf("the fingerprint changed from %s to %s: a rename altered financial semantics",
			before.Fingerprint(), after.Fingerprint())
	}
	for _, pair := range []struct {
		field string
		a, b  any
	}{
		{"allocation", before.Allocation, after.Allocation},
		{"parent", before.ParentID, after.ParentID},
		{"recurrence", before.Recurrence, after.Recurrence},
		{"anchor", before.AnchorAt, after.AnchorAt},
		{"borrow", before.Borrow, after.Borrow},
	} {
		if pair.a != pair.b {
			t.Errorf("%s changed from %v to %v", pair.field, pair.a, pair.b)
		}
	}

	// (9) The child still points at it. Children reference the id, not the name.
	if def, _ := definitionOf(t, store, "chat"); def.ParentID != "research" {
		t.Errorf("chat's parent = %q, want research", def.ParentID)
	}
	chain, err := store.Chain(ctx, "chat")
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(chain) != 2 || chain[1].ID != "research" {
		t.Errorf("the chain from chat is %v, want [chat research]", idsOf(chain))
	}

	// The current period and its money are untouched: a display name is not part of an
	// envelope.
	same, err := store.Period(ctx, period.ID)
	if err != nil {
		t.Fatalf("Period: %v", err)
	}
	if same.Envelope.Allocation != period.Envelope.Allocation ||
		!same.Envelope.Start.Equal(period.Envelope.Start) ||
		!same.Envelope.End.Equal(period.Envelope.End) {
		t.Error("the materialized period changed under a rename")
	}
	afterTotals, err := store.Totals(ctx, ledger.Scope{BudgetID: "research", PeriodID: period.ID}, planNow)
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	if afterTotals != beforeTotals {
		t.Errorf("totals changed from %+v to %+v under a rename", beforeTotals, afterTotals)
	}

	// And no new period was materialized as a side effect.
	periods, err := store.Periods(ctx, "research")
	if err != nil {
		t.Fatalf("Periods: %v", err)
	}
	if len(periods) != 1 {
		t.Errorf("the ledger holds %d periods after a rename, want 1", len(periods))
	}
}

// Renaming to the name a budget already has writes nothing, so a scripted rename is safe to
// re-run.
func TestRenameIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	def := monthly("research", 4000)
	def.Name = "Research group"
	mustApply(t, store, []budget.Definition{def}, planNow)
	_, before := definitionOf(t, store, "research")

	if err := Rename(ctx, store, "research", "Research group"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, after := definitionOf(t, store, "research"); after != before {
		t.Errorf("revision moved from %d to %d for a no-op rename", before, after)
	}
}

// A rename is revision-safe like any other edit: it reads the current revision and passes it
// through UpdateDefinition, so a definition changing underneath is detected.
func TestRenameIsRevisionSafe(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	mustApply(t, store, []budget.Definition{monthly("research", 4000)}, planNow)

	// A writer whose Definition call reports a revision that is already stale by the time
	// UpdateDefinition runs, which is the race a rename could otherwise lose.
	stale := staleWriter{Writer: store, t: t, store: store}
	if err := Rename(ctx, stale, "research", "Whatever"); !errors.Is(err, ledger.ErrRevisionMismatch) {
		t.Fatalf("error = %v, want ErrRevisionMismatch", err)
	}
	if def, _ := definitionOf(t, store, "research"); def.Name == "Whatever" {
		t.Error("a rename overwrote a concurrently updated definition")
	}
}

// staleWriter makes a concurrent change land between the read and the write, so the rename
// path is exercised against a definition that moved underneath it.
type staleWriter struct {
	Writer
	t     *testing.T
	store *sqlite.Store
}

func (w staleWriter) Definition(ctx context.Context, id string) (budget.Definition, int, error) {
	def, revision, err := w.store.Definition(ctx, id)
	if err != nil {
		return def, revision, err
	}
	// Somebody else edits the definition after this read returns.
	edited := def
	edited.Allocation += dollars(1)
	if err := w.store.UpdateDefinition(ctx, edited, revision); err != nil {
		w.t.Fatalf("the interloping update: %v", err)
	}
	return def, revision, nil
}

// Renaming a budget that is not there says so rather than creating one. A rename is not a
// create by another name.
func TestRenameUnknownBudgetFails(t *testing.T) {
	store := newStore(t)
	err := Rename(context.Background(), store, "absent", "Whatever")
	if !errors.Is(err, ledger.ErrBudgetNotFound) {
		t.Errorf("error = %v, want ErrBudgetNotFound", err)
	}
}

func TestRenameRequiresABudgetID(t *testing.T) {
	store := newStore(t)
	if err := Rename(context.Background(), store, "", "Whatever"); err == nil {
		t.Error("renaming an unnamed budget succeeded")
	}
}

// (24) No money on the apply path passes through a float64.
//
// Asserted on a value float64 cannot hold exactly: it must survive the plan, the write, and
// the read back to the microdollar.
func TestApplyPreservesExactMicrodollars(t *testing.T) {
	store := newStore(t)

	// 12345678901234567 microdollars: more significant digits than a float64 has.
	const exact = 12345678901234567
	def := monthly("research", 0)
	def.Allocation = exact
	mustApply(t, store, []budget.Definition{def}, planNow)

	got, _ := definitionOf(t, store, "research")
	if int64(got.Allocation) != exact {
		t.Errorf("allocation = %d microdollars, want %d", int64(got.Allocation), int64(exact))
	}

	// And an update of the same value is recognized as unchanged, which a float64 round trip
	// would break by a microdollar.
	res := mustApply(t, store, []budget.Definition{def}, planNow)
	if res.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1: the exact amount did not compare equal", res.Unchanged)
	}
}

func idsOf(defs []budget.Definition) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.ID)
	}
	return out
}
