package config

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/scttfrdmn/throttle/budget"
)

// Rendering a plan.
//
// Both "config diff" and "config apply" print this, so the two describe the same plan in
// the same words. A dry run whose wording differs from the real thing is a dry run whose
// reader has to guess which parts correspond.

// Render writes a plan in human form. A plan with nothing to do is one line.
func (p Plan) Render(w io.Writer) error {
	if len(p.Steps) == 0 {
		_, err := fmt.Fprintln(w, "no budgets configured")
		return err
	}

	counts := p.Counts()
	if !p.Mutates() && len(p.Refusals()) == 0 {
		// The quiet case, and the common one: a file that has already been applied. Worth
		// stating positively -- "unchanged" answers the question the reader asked -- but
		// not worth a per-budget listing that says nothing five times.
		var parts []string
		if n := counts[ActionNoop]; n > 0 {
			// "3 budgets" rather than "3 unchanged", which the line already says.
			parts = append(parts, fmt.Sprintf("%d %s", n, plural(n, "budget")))
		}
		if n := counts[ActionSkipRename]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d name-only %s", n, plural(n, "difference")))
		}
		if n := counts[ActionLeave]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d stored but not in this file", n))
		}
		if _, err := fmt.Fprintf(w, "unchanged: %s\n", strings.Join(parts, ", ")); err != nil {
			return err
		}
		// The name-only and unmanaged cases still get their detail, because each names a
		// specific budget the reader may want to act on.
		return p.renderNotes(w)
	}

	for _, s := range p.Steps {
		switch s.Action {
		case ActionCreate:
			fmt.Fprintf(w, "%s\n  create\n", s.BudgetID)
			fmt.Fprintf(w, "  %s %s\n", s.Config.Allocation.CentsString(), DescribePeriod(s.Config))
			if s.Config.ParentID != "" {
				fmt.Fprintf(w, "  child of %s\n", s.Config.ParentID)
			}

		case ActionUpdate:
			fmt.Fprintf(w, "%s\n  changed: %s\n", s.BudgetID, strings.Join(s.Fields, ", "))
			for _, line := range describeFields(s) {
				fmt.Fprintf(w, "      %s\n", line)
			}
			// The rule that matters most and is least obvious: money already spent in
			// this period was governed by the old terms and stays that way.
			if s.EffectiveAt.IsZero() {
				fmt.Fprintf(w, "  no current period, so the new definition applies from its next one\n")
			} else {
				fmt.Fprintf(w, "  current period unchanged\n")
				fmt.Fprintf(w, "  new definition applies beginning %s\n", effectiveText(s))
			}

		case ActionRefuse:
			fmt.Fprintf(w, "%s\n  refused: %s\n", s.BudgetID, s.Reason)
		}
	}

	return p.renderNotes(w)
}

// renderNotes prints the per-budget lines for actions that change nothing but that a
// reader may still want to know about.
func (p Plan) renderNotes(w io.Writer) error {
	for _, s := range p.Steps {
		switch s.Action {
		case ActionSkipRename:
			fmt.Fprintf(w, "%s\n  name differs: file %q, stored %q\n", s.BudgetID, s.Config.Name, s.Stored.Name)
			fmt.Fprintf(w, "  not applied; \"throttle rename %s %s\" changes it\n",
				s.BudgetID, quoteArg(s.Config.Name))
		case ActionLeave:
			fmt.Fprintf(w, "%s\n  stored but not in this file; left alone\n", s.BudgetID)
		}
	}
	return nil
}

// effectiveText renders when an update takes effect, in the budget's own timezone because
// that is where its boundaries fall.
func effectiveText(s Step) string {
	loc := s.Stored.Location
	if loc == nil {
		loc = time.UTC
	}
	return s.EffectiveAt.In(loc).Format("2006-01-02 15:04 MST")
}

// describeFields renders the specific values that differ, because "allocation differs" does
// not tell the reader which of the two numbers is the one they meant.
func describeFields(s Step) []string {
	var lines []string
	for _, f := range s.Fields {
		switch f {
		case "allocation":
			lines = append(lines, fmt.Sprintf("allocation: file %s, stored %s",
				s.Config.Allocation.CentsString(), s.Stored.Allocation.CentsString()))
		case "parent":
			lines = append(lines, fmt.Sprintf("parent: file %q, stored %q",
				s.Config.ParentID, s.Stored.ParentID))
		case "borrow":
			lines = append(lines, fmt.Sprintf("borrow: file %s, stored %s",
				borrowText(s.Config.Borrow), borrowText(s.Stored.Borrow)))
		case "recurrence":
			lines = append(lines, fmt.Sprintf("period: file %s, stored %s",
				DescribePeriod(s.Config), DescribePeriod(s.Stored)))
		case "anchor":
			lines = append(lines, fmt.Sprintf("starts: file %s, stored %s",
				dateText(s.Config.AnchorAt, s.Config.Location), dateText(s.Stored.AnchorAt, s.Stored.Location)))
		case "end":
			lines = append(lines, fmt.Sprintf("ends: file %s, stored %s",
				endTextOf(s.Config), endTextOf(s.Stored)))
		case "timezone":
			lines = append(lines, fmt.Sprintf("timezone: file %s, stored %s",
				locName(s.Config.Location), locName(s.Stored.Location)))
		case "rollover":
			lines = append(lines, fmt.Sprintf("rollover: file %s, stored %s",
				DescribeRollover(s.Config.Rollover), DescribeRollover(s.Stored.Rollover)))
		case "name":
			lines = append(lines, fmt.Sprintf("name: file %q, stored %q", s.Config.Name, s.Stored.Name))
		}
	}
	return lines
}

// plural pluralizes a count of things whose plural is the -s form, which is all of them
// here. Not worth a dependency, and not worth "1 difference(s)".
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func borrowText(d time.Duration) string {
	if d == 0 {
		return "none"
	}
	return d.String()
}

func dateText(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "-"
	}
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02 15:04 MST")
}

func endTextOf(def budget.Definition) string {
	if def.EndAt.IsZero() {
		return "open-ended"
	}
	return dateText(def.EndAt, def.Location)
}

// quoteArg quotes a name for the suggested command line, so a copied suggestion works for
// a name containing spaces.
func quoteArg(s string) string {
	if s == "" {
		return `''`
	}
	if !strings.ContainsAny(s, " \t'\"") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
