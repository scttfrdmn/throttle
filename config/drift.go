package config

import (
	"sort"
	"time"

	"throttle/budget"
)

// Comparing what a config file says to what the ledger holds.
//
// This is a pure function over two sets of definitions. It opens nothing, writes nothing,
// and decides nothing: it reports. That is deliberate -- the question of whether a config
// file may rewrite a definition the ledger already holds is a product decision (issue #21),
// and describing the difference is useful and safe whichever way that lands.
//
// The alternative, having a load path quietly reconcile the two, is the destructive option:
// a definition's fingerprint covers its allocation and its period rule, so "make the ledger
// match the file" is a command that can silently change what a running budget is allowed to
// spend, on the strength of somebody having edited a YAML file.

// DriftKind is how one budget differs between a config file and the ledger.
type DriftKind string

const (
	// DriftNew is in the config file and not in the ledger. Creating it takes nothing
	// away from anyone, so this is the one case a command can safely apply.
	DriftNew DriftKind = "new"

	// DriftUnchanged is in both and semantically identical. Re-storing it is idempotent.
	DriftUnchanged DriftKind = "unchanged"

	// DriftRenamed is in both, semantically identical, but with a different display
	// name. Fingerprint deliberately excludes Name, so this is not a conflict -- but it
	// also means the stored name does not currently update, which is issue #22.
	DriftRenamed DriftKind = "renamed"

	// DriftChanged is in both and semantically different: allocation, period rule,
	// borrow, rollover, or parent. Applying it would change what a live budget may
	// spend, so no command does it implicitly.
	DriftChanged DriftKind = "changed"

	// DriftUnmanaged is in the ledger and not in the config file.
	//
	// Reported, never removed. A budget absent from a file is far more often a file that
	// does not describe everything than an instruction to delete accounting history, and
	// deleting is not recoverable from a YAML edit.
	DriftUnmanaged DriftKind = "unmanaged"
)

// Drift is one budget's difference.
type Drift struct {
	BudgetID string
	Kind     DriftKind

	// Fields names what differs, for DriftChanged: "allocation", "parent", "period".
	Fields []string

	// Config and Stored are the two definitions. Config is zero for DriftUnmanaged,
	// Stored is zero for DriftNew.
	Config budget.Definition
	Stored budget.Definition
}

// Compare reports how a config file's budgets differ from the ledger's, by budget id.
func Compare(cfg, stored []budget.Definition) []Drift {
	byID := make(map[string]budget.Definition, len(stored))
	for _, def := range stored {
		byID[def.ID] = def
	}

	var out []Drift
	seen := make(map[string]bool, len(cfg))
	for _, want := range cfg {
		seen[want.ID] = true
		have, ok := byID[want.ID]
		if !ok {
			out = append(out, Drift{BudgetID: want.ID, Kind: DriftNew, Config: want})
			continue
		}
		d := Drift{BudgetID: want.ID, Config: want, Stored: have}
		switch {
		case want.Fingerprint() != have.Fingerprint():
			d.Kind = DriftChanged
			d.Fields = changedFields(want, have)
		case want.Name != have.Name:
			d.Kind = DriftRenamed
			d.Fields = []string{"name"}
		default:
			d.Kind = DriftUnchanged
		}
		out = append(out, d)
	}

	for _, have := range stored {
		if !seen[have.ID] {
			out = append(out, Drift{BudgetID: have.ID, Kind: DriftUnmanaged, Stored: have})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].BudgetID < out[j].BudgetID })
	return out
}

// changedFields names what differs, so a report can say what to look at rather than only
// that something is different.
func changedFields(want, have budget.Definition) []string {
	var fields []string
	if want.ParentID != have.ParentID {
		// Named separately from the rest because it is structural: a parent change moves
		// which envelopes a request consumes, not just how much of one.
		fields = append(fields, "parent")
	}
	if want.Allocation != have.Allocation {
		fields = append(fields, "allocation")
	}
	if want.Borrow != have.Borrow {
		fields = append(fields, "borrow")
	}
	if rolloverKey(want.Rollover) != rolloverKey(have.Rollover) {
		fields = append(fields, "rollover")
	}
	if want.Recurrence != have.Recurrence || want.Every != have.Every {
		fields = append(fields, "recurrence")
	}
	if locName(want.Location) != locName(have.Location) {
		fields = append(fields, "timezone")
	}
	if !want.AnchorAt.Equal(have.AnchorAt) {
		fields = append(fields, "anchor")
	}
	if !want.EndAt.Equal(have.EndAt) {
		fields = append(fields, "end")
	}
	if want.Name != have.Name {
		fields = append(fields, "name")
	}
	if len(fields) == 0 {
		// The fingerprints differ but nothing compared above does. Not expected, and
		// worth saying plainly rather than reporting an empty list as agreement.
		fields = append(fields, "fingerprint")
	}
	return fields
}

// rolloverKey compares two carry policies the way the fingerprint does, with the mode
// normalized. Comparing the structs directly would report "rollover: file none, stored none"
// for a policy whose mode arrived as the zero value on one side.
func rolloverKey(p budget.RolloverPolicy) budget.RolloverPolicy {
	p.Mode = p.Mode.Normalized()
	return p
}

func locName(loc *time.Location) string {
	if loc == nil {
		return "UTC"
	}
	return loc.String()
}
