package config

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/scttfrdmn/throttle/budget"
)

// Planning what applying a configuration file would do.
//
// One planner, two commands: "config diff" plans and renders, "config apply" plans and
// executes. The alternative -- diff describing one thing and apply doing another -- is a
// tool whose dry run cannot be trusted, which is worse than having no dry run at all.
//
// A plan is a pure function of two definition sets and a clock. It opens nothing and
// writes nothing, so rendering one is always safe.

// Action is what applying a plan would do to one budget.
type Action string

const (
	// ActionCreate stores a definition the ledger does not have. The only action that
	// takes nothing away from anyone.
	ActionCreate Action = "create"

	// ActionNoop is a definition already stored with identical semantics.
	ActionNoop Action = "noop"

	// ActionUpdate changes a stored definition's rules for future periods. The
	// materialized current period keeps the terms it was created with.
	ActionUpdate Action = "update"

	// ActionSkipRename is a name-only difference, deliberately not applied.
	//
	// A display name is metadata and not financial semantics (issue #22), but throttle
	// does not infer a rename from two definitions happening to match financially, and
	// applying a name change silently is how a mislabelled budget gets relabelled without
	// anyone deciding to. "throttle budget rename" is the explicit operation.
	ActionSkipRename Action = "skip-rename"

	// ActionLeave is a stored definition absent from the configuration file.
	//
	// Left alone, always. A budget missing from a file is far more often an incomplete
	// file than an instruction to discard accounting history, and disappearing from a
	// file is not deletion.
	ActionLeave Action = "leave"

	// ActionRefuse is a change apply will not make, with a reason. Currently: a parent
	// change. See refuseParentChange.
	ActionRefuse Action = "refuse"
)

// Step is one budget's planned action.
type Step struct {
	BudgetID string
	Action   Action

	// Fields names what differs, for ActionUpdate and ActionRefuse.
	Fields []string

	// Config and Stored are the two definitions. Config is zero for ActionLeave,
	// Stored is zero for ActionCreate.
	Config budget.Definition
	Stored budget.Definition

	// Revision is the stored definition's revision, and the value apply passes as the
	// expected revision. A definition that moved between plan and execute fails rather
	// than overwriting whatever arrived in between.
	Revision int

	// EffectiveAt is when an update's new terms begin: the end of the currently
	// materialized period. Zero when the budget has no current period, in which case
	// there is nothing whose terms are already fixed.
	EffectiveAt time.Time

	// Reason explains an ActionRefuse in a sentence a reader can act on.
	Reason string
}

// Stored is a definition as the ledger holds it, with the revision that guards updates.
type Stored struct {
	Definition budget.Definition
	Revision   int
}

// Plan is what applying a configuration would do.
type Plan struct{ Steps []Step }

// Plan computes the actions that would reconcile stored definitions with cfg.
//
// now is passed rather than read so a plan is reproducible: the only thing the clock
// affects is which period is current, and a test that cannot fix that cannot test it.
func NewPlan(cfg []budget.Definition, stored []Stored, now time.Time) Plan {
	byID := make(map[string]Stored, len(stored))
	defs := make([]budget.Definition, 0, len(stored))
	for _, s := range stored {
		byID[s.Definition.ID] = s
		defs = append(defs, s.Definition)
	}

	steps := make([]Step, 0, len(cfg)+len(stored))
	for _, d := range Compare(cfg, defs) {
		step := Step{BudgetID: d.BudgetID, Fields: d.Fields, Config: d.Config, Stored: d.Stored}
		if s, ok := byID[d.BudgetID]; ok {
			step.Revision = s.Revision
		}

		switch d.Kind {
		case DriftNew:
			step.Action = ActionCreate
		case DriftUnchanged:
			step.Action = ActionNoop
		case DriftRenamed:
			step.Action = ActionSkipRename
		case DriftUnmanaged:
			step.Action = ActionLeave
		case DriftChanged:
			if reason := refuseParentChange(d); reason != "" {
				step.Action = ActionRefuse
				step.Reason = reason
				break
			}
			step.Action = ActionUpdate
			step.EffectiveAt = nextBoundary(d.Stored, now)
		}
		steps = append(steps, step)
	}

	return Plan{Steps: orderForApply(steps)}
}

// refuseParentChange reports why a parent change is not applied, or "" if none is proposed.
//
// A parent change is not a rename and not an amount edit: it moves which envelopes a
// request consumes. The ledger would accept it -- UpdateDefinition validates the new edge
// for existence and cycles -- but accepting it is not the same as it being meaningful.
// Spend already attributed to the old ancestor's period stays there, correctly, because
// that is what happened; so after a reparent a child's history is divided across two
// chains and no single figure describes "what this budget has cost its parent". Whether
// that is acceptable, or wants migration, or wants a fresh budget instead, is a product
// question and not one to answer inside an update statement.
//
// So v0.1 refuses and says so. An explicit limitation is recoverable; a migration nobody
// designed is not.
func refuseParentChange(d Drift) string {
	changed := false
	for _, f := range d.Fields {
		if f == "parent" {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}
	from, to := d.Stored.ParentID, d.Config.ParentID
	switch {
	case from == "":
		return fmt.Sprintf("the file gives it the parent %q and the stored budget has none; "+
			"apply does not change parentage", to)
	case to == "":
		return fmt.Sprintf("the file removes its parent %q; apply does not change parentage", from)
	default:
		return fmt.Sprintf("the file moves it from parent %q to %q; apply does not change parentage",
			from, to)
	}
}

// nextBoundary is when an updated definition's terms begin: the end of the period that is
// current now, computed from the stored definition, because that is the one that
// materialized it.
//
// Real period semantics rather than an assumed month: the answer for a weekly budget, a
// six-hour window, and a fixed grant all come from the same Bounds call.
//
// Zero when there is no current period -- a budget that has not started, or one whose term
// has ended -- because then no materialized period's terms are already fixed and there is
// nothing to say "after this one" about.
func nextBoundary(stored budget.Definition, now time.Time) time.Time {
	seq, err := stored.PeriodFor(now)
	if err != nil {
		return time.Time{}
	}
	_, end, err := stored.Bounds(seq)
	if err != nil {
		return time.Time{}
	}
	return end
}

// orderForApply sorts steps so a parent is created before its children.
//
// Compare returns budgets in id order, which puts "chat" before "research" -- and creating
// a child whose parent does not exist yet fails. Configuration validation has already
// rejected cycles and missing parents, so ranking by depth within the configured set is
// well defined. Everything else keeps id order, so output is stable and diffable.
func orderForApply(steps []Step) []Step {
	depth := make(map[string]int, len(steps))
	parent := make(map[string]string, len(steps))
	for _, s := range steps {
		if s.Action == ActionLeave {
			continue
		}
		parent[s.BudgetID] = s.Config.ParentID
	}

	var depthOf func(id string, guard int) int
	depthOf = func(id string, guard int) int {
		if d, ok := depth[id]; ok {
			return d
		}
		p, ok := parent[id]
		if !ok || p == "" || guard <= 0 {
			// No parent, a parent outside this file, or a guard trip. A parent the file
			// does not mention is already stored or the plan would not have validated,
			// so this budget can be created immediately.
			return 0
		}
		d := depthOf(p, guard-1) + 1
		depth[id] = d
		return d
	}

	out := make([]Step, len(steps))
	copy(out, steps)
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := depthOf(out[i].BudgetID, len(steps)), depthOf(out[j].BudgetID, len(steps))
		if di != dj {
			return di < dj
		}
		return out[i].BudgetID < out[j].BudgetID
	})
	return out
}

// Counts tallies the plan by action, for a summary line.
func (p Plan) Counts() map[Action]int {
	out := make(map[Action]int, 6)
	for _, s := range p.Steps {
		out[s.Action]++
	}
	return out
}

// Mutates reports whether applying this plan would write anything.
func (p Plan) Mutates() bool {
	for _, s := range p.Steps {
		if s.Action == ActionCreate || s.Action == ActionUpdate {
			return true
		}
	}
	return false
}

// Refusals returns the steps apply will not perform.
func (p Plan) Refusals() []Step {
	var out []Step
	for _, s := range p.Steps {
		if s.Action == ActionRefuse {
			out = append(out, s)
		}
	}
	return out
}

// ErrRefused reports a plan containing a change apply will not make.
var ErrRefused = errors.New("config: the configuration asks for a change apply will not make")
