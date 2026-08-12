package config

import (
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/budget"
)

// Comparing a file to the ledger. Every case here is a pure function over two definition
// sets: Compare opens nothing and decides nothing, which is what lets "config check" report
// a difference without the report itself being a change.

func monthly(id string, alloc int64) budget.Definition {
	return budget.Definition{
		ID:         id,
		Allocation: dollars(alloc),
		Recurrence: budget.RecurMonthly,
		Location:   time.UTC,
		AnchorAt:   time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Rollover:   budget.RolloverPolicy{Mode: budget.RolloverNone},
	}
}

// one finds the single drift for a budget or fails.
func one(t *testing.T, drifts []Drift, id string) Drift {
	t.Helper()
	for _, d := range drifts {
		if d.BudgetID == id {
			return d
		}
	}
	t.Fatalf("no drift reported for %q", id)
	return Drift{}
}

// (17) An identical definition is unchanged, which is what makes re-running "define"
// idempotent rather than a conflict.
func TestCompareUnchanged(t *testing.T) {
	def := monthly("research", 4000)
	drifts := Compare([]budget.Definition{def}, []budget.Definition{def})
	if got := one(t, drifts, "research").Kind; got != DriftUnchanged {
		t.Errorf("kind = %q, want unchanged", got)
	}
}

// The two spellings of "no rollover" are the same definition.
//
// This is the case that failed in practice: a config file that omits the rollover block
// produces the zero-value mode, the ledger stores the canonical "none", and before
// Fingerprint normalized the mode, "config check" run straight after "define" reported the
// budget as differing from its own stored copy -- with "file none, stored none" as the
// explanation.
func TestCompareRolloverSpellings(t *testing.T) {
	fromFile := monthly("research", 4000)
	fromFile.Rollover.Mode = "" // as an omitted rollover block compiles

	stored := monthly("research", 4000)
	stored.Rollover.Mode = budget.RolloverNone // as the ledger reads it back

	if got := one(t, Compare([]budget.Definition{fromFile}, []budget.Definition{stored}), "research").Kind; got != DriftUnchanged {
		t.Errorf("kind = %q, want unchanged", got)
	}
}

// (18) A changed definition is reported, never applied. Which field changed is named,
// because "allocation differs" does not say which of the two numbers is the intended one.
func TestCompareChanged(t *testing.T) {
	stored := monthly("research", 4000)

	tests := []struct {
		name  string
		edit  func(*budget.Definition)
		field string
	}{
		{"allocation", func(d *budget.Definition) { d.Allocation = dollars(5000) }, "allocation"},
		{"parent", func(d *budget.Definition) { d.ParentID = "institute" }, "parent"},
		{"borrow", func(d *budget.Definition) { d.Borrow = 72 * time.Hour }, "borrow"},
		{"recurrence", func(d *budget.Definition) { d.Recurrence = budget.RecurWeekly }, "recurrence"},
		{"timezone", func(d *budget.Definition) { d.Location = mustLoad(t, "America/New_York") }, "timezone"},
		{"anchor", func(d *budget.Definition) { d.AnchorAt = d.AnchorAt.AddDate(0, 1, 0) }, "anchor"},
		{"end", func(d *budget.Definition) { d.EndAt = d.AnchorAt.AddDate(1, 0, 0) }, "end"},
		{"rollover", func(d *budget.Definition) { d.Rollover.Mode = budget.RolloverCredit }, "rollover"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			edited := stored
			tt.edit(&edited)

			d := one(t, Compare([]budget.Definition{edited}, []budget.Definition{stored}), "research")
			if d.Kind != DriftChanged {
				t.Fatalf("kind = %q, want changed", d.Kind)
			}
			if !contains(d.Fields, tt.field) {
				t.Errorf("fields = %v, want %q among them", d.Fields, tt.field)
			}
			// The fallback exists for a fingerprint difference nothing compared explains.
			// Reaching it here would mean changedFields has fallen behind Fingerprint.
			if contains(d.Fields, "fingerprint") {
				t.Errorf("fields = %v, want the specific field rather than the fallback", d.Fields)
			}
		})
	}
}

// (19) A rename fingerprints identical, so it is reported as a rename rather than as a
// conflict -- and the report says it is not applied, because Fingerprint deliberately
// excludes Name and PutDefinition therefore does nothing. That gap is issue #22.
func TestCompareRenamed(t *testing.T) {
	stored := monthly("research", 4000)
	stored.Name = "Research group"

	renamed := stored
	renamed.Name = "Research programme"

	d := one(t, Compare([]budget.Definition{renamed}, []budget.Definition{stored}), "research")
	if d.Kind != DriftRenamed {
		t.Fatalf("kind = %q, want renamed", d.Kind)
	}
	if renamed.Fingerprint() != stored.Fingerprint() {
		t.Error("a rename changed the fingerprint, so it is a conflict rather than a rename")
	}
}

// A budget in the file and not in the ledger is new, which is the one case a command can
// safely apply: creating it takes nothing away from anyone.
func TestCompareNew(t *testing.T) {
	d := one(t, Compare([]budget.Definition{monthly("research", 4000)}, nil), "research")
	if d.Kind != DriftNew {
		t.Errorf("kind = %q, want new", d.Kind)
	}
	if d.Stored.ID != "" {
		t.Error("a new budget reported a stored definition")
	}
}

// A budget in the ledger and not in the file is left alone.
//
// Reported, never removed. A budget absent from a file is far more often a file that does not
// describe everything than an instruction to delete accounting history.
func TestCompareUnmanaged(t *testing.T) {
	d := one(t, Compare(nil, []budget.Definition{monthly("legacy", 100)}), "legacy")
	if d.Kind != DriftUnmanaged {
		t.Errorf("kind = %q, want unmanaged", d.Kind)
	}
	if d.Config.ID != "" {
		t.Error("an unmanaged budget reported a config definition")
	}
}

// Results are ordered by id, so two runs over the same pair report in the same sequence.
func TestCompareIsOrdered(t *testing.T) {
	cfg := []budget.Definition{monthly("zebra", 1), monthly("alpha", 1)}
	stored := []budget.Definition{monthly("mid", 1)}

	drifts := Compare(cfg, stored)
	for i := 1; i < len(drifts); i++ {
		if drifts[i-1].BudgetID > drifts[i].BudgetID {
			t.Fatalf("unordered: %v then %v", drifts[i-1].BudgetID, drifts[i].BudgetID)
		}
	}
	if len(drifts) != 3 {
		t.Errorf("got %d drifts, want 3", len(drifts))
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return loc
}
