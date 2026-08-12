package config

import (
	"context"
	"errors"
	"fmt"

	"throttle/budget"
	"throttle/ledger"
)

// Executing a plan.
//
// The only mutating path in this package. Everything else -- loading, validating,
// comparing, planning -- is pure, and this is deliberately the one file where that stops
// being true, so "does reading configuration write anything" has a one-word answer.

// Writer is the ledger operations apply needs.
//
// Defined here, by the consumer, and deliberately narrow: three methods, none of which can
// delete a definition or touch a period. A package that cannot express "remove this budget"
// cannot remove one by accident.
type Writer interface {
	PutDefinition(ctx context.Context, def budget.Definition) error
	UpdateDefinition(ctx context.Context, def budget.Definition, expectRevision int) error
	Definition(ctx context.Context, budgetID string) (budget.Definition, int, error)
}

// Result is what applying a plan did.
type Result struct {
	Created []string
	Updated []string

	// Unchanged counts definitions that were already identical, including ones another
	// process created concurrently between planning and applying.
	Unchanged int
}

// Apply executes the plan's create and update steps.
//
// Steps that are not creates or updates are skipped by construction rather than by
// filtering: a no-op writes nothing, an unmanaged definition is left alone, a name-only
// difference needs the explicit rename command, and a refusal is a refusal.
//
// A plan containing a refusal is not applied at all. Applying the safe half of a
// configuration and reporting the rest as an error leaves the ledger in a state matching
// neither the file nor what it was before, and the operator has to work out which half
// landed. Better to change the file and re-run.
func Apply(ctx context.Context, w Writer, plan Plan) (Result, error) {
	if refusals := plan.Refusals(); len(refusals) > 0 {
		return Result{}, refusalError(refusals)
	}

	var res Result
	for _, step := range plan.Steps {
		switch step.Action {
		case ActionCreate:
			// PutDefinition is idempotent for identical semantics and conflicts
			// otherwise, so two processes applying the same file both succeed and
			// neither overwrites the other. A conflict here means somebody stored a
			// different definition under this id between plan and apply.
			err := w.PutDefinition(ctx, step.Config)
			switch {
			case err == nil:
				res.Created = append(res.Created, step.BudgetID)
			case errors.Is(err, ledger.ErrDefinitionConflict):
				return res, conflictError(step, err)
			default:
				return res, fmt.Errorf("create %q: %w", step.BudgetID, err)
			}

		case ActionUpdate:
			err := w.UpdateDefinition(ctx, step.Config, step.Revision)
			switch {
			case err == nil:
				res.Updated = append(res.Updated, step.BudgetID)
			case errors.Is(err, ledger.ErrRevisionMismatch):
				// The stored definition moved between plan and apply. Not retried and
				// not overwritten: the plan was computed against a definition that no
				// longer exists, so its update is a decision about superseded facts.
				return res, staleError(step, err)
			default:
				return res, fmt.Errorf("update %q: %w", step.BudgetID, err)
			}

		case ActionNoop:
			res.Unchanged++
		}
	}
	return res, nil
}

// conflictError explains a create that lost a race, distinguishing the harmless case.
//
// Two processes applying the same configuration is expected and converges: PutDefinition
// treats an identical definition as already done. A conflict means the definition that
// arrived is genuinely different, which is the case worth a loud error.
func conflictError(step Step, err error) error {
	return fmt.Errorf("%w\n\nbudget %q was created by something else while this was running, "+
		"with different terms.\nRe-run \"throttle config diff\" to see what it now says.",
		err, step.BudgetID)
}

// staleError explains a revision mismatch in terms of what to do next.
func staleError(step Step, err error) error {
	return fmt.Errorf("%w\n\nbudget %q changed while this was running, so the planned update "+
		"was computed\nagainst terms that are no longer stored. Nothing was written for it.\n"+
		"Re-run \"throttle config diff\" and apply again.", err, step.BudgetID)
}

func refusalError(refusals []Step) error {
	var b []byte
	b = append(b, "the configuration asks for changes apply will not make:\n"...)
	for _, s := range refusals {
		b = append(b, fmt.Sprintf("  %s: %s\n", s.BudgetID, s.Reason)...)
	}
	b = append(b, "\nNothing was applied. Edit the file, or make the change explicitly."...)
	return fmt.Errorf("%w\n%s", ErrRefused, b)
}

// Rename changes a budget's display name and nothing else.
//
// A name is metadata, not financial semantics: Fingerprint excludes it, so this changes
// what the budget is called and not what it may spend. It goes through UpdateDefinition
// with an expected revision like any other edit, so a concurrent change is detected rather
// than clobbered.
//
// Nothing historical is rewritten, because nothing historical stores the name: activity
// records and reservation legs key on the durable budget id, and reports read the current
// name at render time. So a renamed budget's history stays attached to it, and no past
// record is retroactively relabelled.
//
// Parent links are untouched -- children reference the id -- and no period is materialized
// or rewritten, because a display name is not part of an envelope.
func Rename(ctx context.Context, w Writer, budgetID, name string) error {
	if budgetID == "" {
		return errors.New("rename: a budget id is required")
	}
	def, revision, err := w.Definition(ctx, budgetID)
	if err != nil {
		return err
	}
	if def.Name == name {
		return nil // Idempotent.
	}

	before := def.Fingerprint()
	def.Name = name
	if after := def.Fingerprint(); after != before {
		// Belt and braces. A rename that changed the fingerprint would mean Name had
		// become semantic, which would make this operation a silent change to what a
		// budget may spend. Refusing beats discovering it later from the ledger.
		return fmt.Errorf("rename: renaming %q would change its financial identity "+
			"(%s to %s); refusing", budgetID, before, after)
	}
	return w.UpdateDefinition(ctx, def, revision)
}
