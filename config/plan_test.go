package config

import (
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/budget"
)

// Planning what apply would do.
//
// Every case here is a pure function of two definition sets and a clock. NewPlan takes no
// store and no context: there is no argument it could be given that would let it write
// anything, which is what makes rendering a plan safe and makes "config diff" a dry run
// rather than a promise.

// storedAt wraps a definition with the revision the ledger holds it at.
func storedAt(def budget.Definition, revision int) Stored {
	return Stored{Definition: def, Revision: revision}
}

// step finds the planned step for a budget or fails.
func step(t *testing.T, p Plan, id string) Step {
	t.Helper()
	for _, s := range p.Steps {
		if s.BudgetID == id {
			return s
		}
	}
	t.Fatalf("no step planned for %q, plan has %d steps", id, len(p.Steps))
	return Step{}
}

// planNow is the instant plans are computed at: mid-September, so the September period is
// current and its end is a date an assertion can name.
var planNow = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// (2) A budget in the file and not in the ledger plans a create. The only action that takes
// nothing away from anyone.
func TestPlanCreatesNewBudget(t *testing.T) {
	p := NewPlan([]budget.Definition{monthly("research", 4000)}, nil, planNow)

	s := step(t, p, "research")
	if s.Action != ActionCreate {
		t.Errorf("action = %q, want create", s.Action)
	}
	if !p.Mutates() {
		t.Error("a plan that creates a budget must report that it mutates")
	}
	if s.Revision != 0 {
		t.Errorf("Revision = %d, want 0: there is nothing stored to have a revision", s.Revision)
	}
}

// (3) An identical definition plans nothing.
func TestPlanNoopsUnchangedBudget(t *testing.T) {
	def := monthly("research", 4000)
	p := NewPlan([]budget.Definition{def}, []Stored{storedAt(def, 1)}, planNow)

	s := step(t, p, "research")
	if s.Action != ActionNoop {
		t.Errorf("action = %q, want noop", s.Action)
	}
	if p.Mutates() {
		t.Error("a plan over an already-applied file must not report that it mutates")
	}
}

// (4) A semantically different definition plans an update, carrying the stored revision so
// apply can detect a concurrent change.
func TestPlanUpdatesChangedBudget(t *testing.T) {
	stored := monthly("research", 4000)
	edited := monthly("research", 5000)

	p := NewPlan([]budget.Definition{edited}, []Stored{storedAt(stored, 7)}, planNow)

	s := step(t, p, "research")
	if s.Action != ActionUpdate {
		t.Fatalf("action = %q, want update", s.Action)
	}
	if !contains(s.Fields, "allocation") {
		t.Errorf("Fields = %v, want the allocation named", s.Fields)
	}
	if s.Revision != 7 {
		t.Errorf("Revision = %d, want the stored 7: an update without it is last-write-wins", s.Revision)
	}
	if s.Config.Allocation != edited.Allocation {
		t.Error("the step must carry the file's definition, which is what apply writes")
	}
}

// (5) A stored budget absent from the file plans nothing at all.
//
// The single most destructive thing this code could do, and the one it must never do:
// disappearing from a YAML file is not an instruction to delete accounting history. There is
// no Action that means delete, so this is checked by asserting the action is a leave and that
// nothing about the plan claims to mutate.
func TestPlanNeverDeletesAnUnmanagedBudget(t *testing.T) {
	p := NewPlan(nil, []Stored{storedAt(monthly("legacy", 100), 3)}, planNow)

	s := step(t, p, "legacy")
	if s.Action != ActionLeave {
		t.Errorf("action = %q, want leave", s.Action)
	}
	if p.Mutates() {
		t.Error("a plan over a file missing a stored budget must not mutate anything")
	}

	// And no rendering of it suggests otherwise.
	var b strings.Builder
	if err := p.Render(&b); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, word := range []string{"delete", "remove", "prune", "drop"} {
		if strings.Contains(strings.ToLower(b.String()), word) {
			t.Errorf("the plan output uses %q:\n%s", word, b.String())
		}
	}
}

// (6) A name-only difference is reported and never applied.
//
// throttle does not infer a rename from two definitions happening to match financially
// (issue #22): two distinct budgets can have identical terms, and relabelling one of them
// because a file listed a different name is a mistake nobody asked for.
func TestPlanNeverSilentlyRenames(t *testing.T) {
	stored := monthly("research", 4000)
	stored.Name = "Research group"
	renamed := stored
	renamed.Name = "Research programme"

	p := NewPlan([]budget.Definition{renamed}, []Stored{storedAt(stored, 1)}, planNow)

	s := step(t, p, "research")
	if s.Action != ActionSkipRename {
		t.Fatalf("action = %q, want skip-rename", s.Action)
	}
	if p.Mutates() {
		t.Error("a name-only difference must not make the plan mutating")
	}

	// The output names the explicit command, or the difference is a dead end for the reader.
	var b strings.Builder
	if err := p.Render(&b); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(b.String(), "throttle rename research") {
		t.Errorf("the plan does not say how to apply the rename:\n%s", b.String())
	}
}

// (10, 11) An update takes effect at the next period boundary, and the plan says so.
//
// The materialized period keeps the terms it was created with, because money has already
// been spent under them. Saying this in the output is the difference between a user who
// knows their $5,000 starts in October and one who thinks it started today.
func TestPlanUpdateTakesEffectAtTheNextBoundary(t *testing.T) {
	stored := monthly("research", 4000) // anchored 2026-09-01, monthly
	edited := monthly("research", 5000)

	p := NewPlan([]budget.Definition{edited}, []Stored{storedAt(stored, 1)}, planNow)
	s := step(t, p, "research")

	want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if !s.EffectiveAt.Equal(want) {
		t.Errorf("EffectiveAt = %s, want %s", s.EffectiveAt, want)
	}

	var b strings.Builder
	if err := p.Render(&b); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "current period unchanged") {
		t.Errorf("the plan does not say the current period is unchanged:\n%s", out)
	}
	if !strings.Contains(out, "2026-10-01") {
		t.Errorf("the plan does not say when the new definition applies:\n%s", out)
	}
}

// The boundary comes from real period semantics rather than an assumed month.
//
// A weekly budget, a six-hour window, and a fixed grant are all budgets somebody will
// configure, and each answers "when does this take effect" differently.
func TestPlanBoundaryUsesRealPeriodSemantics(t *testing.T) {
	anchor := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		of   func() budget.Definition
		want time.Time
	}{
		{
			name: "weekly",
			of: func() budget.Definition {
				d := monthly("w", 100)
				d.Recurrence = budget.RecurWeekly
				return d
			},
			// planNow is 15 September; the week from the 1st containing it starts on
			// the 15th and ends on the 22nd.
			want: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "six hours",
			of: func() budget.Definition {
				d := monthly("h", 100)
				d.Recurrence = budget.RecurDuration
				d.Every = 6 * time.Hour
				return d
			},
			// planNow is 12:00 on the 15th, which is the start of a window.
			want: time.Date(2026, 9, 15, 18, 0, 0, 0, time.UTC),
		},
		{
			name: "fixed grant",
			of: func() budget.Definition {
				d := monthly("g", 100)
				d.Recurrence = budget.RecurNone
				d.EndAt = anchor.AddDate(1, 0, 0)
				return d
			},
			want: anchor.AddDate(1, 0, 0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := tt.of()
			edited := stored
			edited.Allocation = dollars(999)

			p := NewPlan([]budget.Definition{edited}, []Stored{storedAt(stored, 1)}, planNow)
			s := step(t, p, stored.ID)
			if s.Action != ActionUpdate {
				t.Fatalf("action = %q, want update", s.Action)
			}
			if !s.EffectiveAt.Equal(tt.want) {
				t.Errorf("EffectiveAt = %s, want %s", s.EffectiveAt, tt.want)
			}
		})
	}
}

// A budget with no current period has no fixed terms to protect, and the plan says that
// rather than inventing a boundary.
func TestPlanUpdateBeforeTheAnchorHasNoBoundary(t *testing.T) {
	stored := monthly("later", 100)
	stored.AnchorAt = time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	edited := stored
	edited.Allocation = dollars(200)

	p := NewPlan([]budget.Definition{edited}, []Stored{storedAt(stored, 1)}, planNow)
	s := step(t, p, "later")
	if s.Action != ActionUpdate {
		t.Fatalf("action = %q, want update", s.Action)
	}
	if !s.EffectiveAt.IsZero() {
		t.Errorf("EffectiveAt = %s, want zero for a budget that has not started", s.EffectiveAt)
	}

	var b strings.Builder
	if err := p.Render(&b); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(b.String(), "no current period") {
		t.Errorf("the plan does not explain the absent boundary:\n%s", b.String())
	}
}

// (15) A parent change is refused, explicitly and with a reason.
//
// A reparent moves which envelopes a request consumes. Spend already attributed to the old
// ancestor stays there, correctly, so afterwards no single figure describes what the child
// has cost its parent. Refusing is a limitation somebody can work around; a migration nobody
// designed is not.
func TestPlanRefusesParentChange(t *testing.T) {
	tests := []struct {
		name         string
		from, to     string
		wantMentions []string
	}{
		{"acquires a parent", "", "institute", []string{"institute"}},
		{"loses a parent", "institute", "", []string{"institute"}},
		{"moves parent", "institute", "faculty", []string{"institute", "faculty"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored := monthly("research", 4000)
			stored.ParentID = tt.from
			edited := stored
			edited.ParentID = tt.to

			p := NewPlan([]budget.Definition{edited}, []Stored{storedAt(stored, 1)}, planNow)
			s := step(t, p, "research")
			if s.Action != ActionRefuse {
				t.Fatalf("action = %q, want refuse", s.Action)
			}
			for _, want := range tt.wantMentions {
				if !strings.Contains(s.Reason, want) {
					t.Errorf("Reason = %q, want %q named", s.Reason, want)
				}
			}
			if p.Mutates() {
				t.Error("a refused plan must not report that it mutates")
			}
			if len(p.Refusals()) != 1 {
				t.Errorf("Refusals() has %d steps, want 1", len(p.Refusals()))
			}
		})
	}
}

// A parent change bundled with an ordinary edit is still a refusal. Applying the safe half of
// a step is how a definition ends up matching neither the file nor what it was before.
func TestPlanRefusesParentChangeEvenAlongsideAnAllocationEdit(t *testing.T) {
	stored := monthly("research", 4000)
	stored.ParentID = "institute"
	edited := stored
	edited.ParentID = "faculty"
	edited.Allocation = dollars(5000)

	p := NewPlan([]budget.Definition{edited}, []Stored{storedAt(stored, 1)}, planNow)
	if got := step(t, p, "research").Action; got != ActionRefuse {
		t.Errorf("action = %q, want refuse", got)
	}
}

// Parents are created before their children, because creating a child whose parent does not
// exist yet fails. Compare returns budgets in id order, which puts "chat" first.
func TestPlanOrdersParentsBeforeChildren(t *testing.T) {
	parent := monthly("research", 4000)
	chat := monthly("chat", 1000)
	chat.ParentID = "research"
	agents := monthly("agents", 500)
	agents.ParentID = "research"

	p := NewPlan([]budget.Definition{chat, agents, parent}, nil, planNow)

	position := map[string]int{}
	for i, s := range p.Steps {
		position[s.BudgetID] = i
	}
	if position["research"] > position["chat"] || position["research"] > position["agents"] {
		var ids []string
		for _, s := range p.Steps {
			ids = append(ids, s.BudgetID)
		}
		t.Errorf("order = %v, want research before its children", ids)
	}
}

// A grandchild comes after its parent, which comes after the root.
func TestPlanOrdersDeepHierarchy(t *testing.T) {
	root := monthly("a-root", 100)
	mid := monthly("z-mid", 100)
	mid.ParentID = "a-root"
	leaf := monthly("m-leaf", 100)
	leaf.ParentID = "z-mid"

	p := NewPlan([]budget.Definition{leaf, mid, root}, nil, planNow)

	var order []string
	for _, s := range p.Steps {
		order = append(order, s.BudgetID)
	}
	want := []string{"a-root", "z-mid", "m-leaf"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// A plan over nothing is one line, not an empty screen a reader has to interpret.
func TestPlanRendersEmptyPlainly(t *testing.T) {
	var b strings.Builder
	if err := (Plan{}).Render(&b); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(b.String(), "no budgets configured") {
		t.Errorf("an empty plan renders as %q", b.String())
	}
}

// A zero-change plan is concise: one line, because the answer to "what would apply do" is
// "nothing" and repeating that per budget says it five times.
func TestPlanRendersNoChangeConcisely(t *testing.T) {
	defs := []budget.Definition{monthly("a", 1), monthly("b", 2), monthly("c", 3)}
	var stored []Stored
	for _, d := range defs {
		stored = append(stored, storedAt(d, 1))
	}

	var b strings.Builder
	if err := NewPlan(defs, stored, planNow).Render(&b); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := strings.TrimSpace(b.String())
	if strings.Count(out, "\n") != 0 {
		t.Errorf("an unchanged plan rendered %d lines:\n%s", strings.Count(out, "\n")+1, out)
	}
	if !strings.Contains(out, "unchanged") || !strings.Contains(out, "3 budgets") {
		t.Errorf("the summary does not say what is unchanged: %q", out)
	}
}

// Counts tallies by action, which is what the CLI summary line is built from.
func TestPlanCounts(t *testing.T) {
	stored := []Stored{
		storedAt(monthly("same", 100), 1),
		storedAt(monthly("edited", 100), 1),
		storedAt(monthly("gone", 100), 1),
	}
	cfg := []budget.Definition{
		monthly("same", 100),
		monthly("edited", 200),
		monthly("fresh", 300),
	}

	counts := NewPlan(cfg, stored, planNow).Counts()
	for action, want := range map[Action]int{
		ActionNoop:   1,
		ActionUpdate: 1,
		ActionCreate: 1,
		ActionLeave:  1,
	} {
		if counts[action] != want {
			t.Errorf("counts[%q] = %d, want %d", action, counts[action], want)
		}
	}
}

// The same inputs plan the same way twice. NewPlan takes the clock as an argument precisely
// so this is testable: a planner reading time.Now internally could not be pinned.
func TestPlanIsReproducible(t *testing.T) {
	cfg := []budget.Definition{monthly("research", 5000), monthly("fresh", 100)}
	stored := []Stored{storedAt(monthly("research", 4000), 2)}

	first := NewPlan(cfg, stored, planNow)
	second := NewPlan(cfg, stored, planNow)

	if len(first.Steps) != len(second.Steps) {
		t.Fatalf("step counts differ: %d and %d", len(first.Steps), len(second.Steps))
	}
	for i := range first.Steps {
		a, b := first.Steps[i], second.Steps[i]
		if a.BudgetID != b.BudgetID || a.Action != b.Action || !a.EffectiveAt.Equal(b.EffectiveAt) {
			t.Errorf("step %d differs: %+v and %+v", i, a, b)
		}
	}
}
