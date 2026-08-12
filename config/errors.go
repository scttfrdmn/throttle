package config

import (
	"fmt"
	"sort"
	"strings"
)

// A config error has to answer three questions: what failed, where, and what to do. A
// message that answers only the first sends the reader back to re-read the whole file
// looking for it.

// FieldError is one problem at one place in the configuration.
type FieldError struct {
	// Path is the dotted field path, e.g. "budgets.research.amount".
	Path string

	// Problem states what is wrong, and where useful, what to write instead.
	Problem string
}

func (e FieldError) Error() string {
	if e.Path == "" {
		return e.Problem
	}
	return e.Path + ": " + e.Problem
}

func fieldErr(path, problem string) error {
	return FieldError{Path: path, Problem: problem}
}

// Errors is every problem found in one pass.
//
// Reporting them together matters more here than in most places. Configuration is edited
// by hand, and a loader that stops at the first mistake turns five typos into five
// edit-run cycles -- each one a fresh chance to introduce a sixth.
//
// Aggregation stops where correctness would suffer. A budget whose amount will not parse
// is not checked for a parent cycle, because there is no budget yet to check; the phases
// are ordered so that each one only aggregates errors its predecessors have made safe.
type Errors struct {
	// Source is the file the problems are in, if they came from a file.
	Source string

	Errs []error
}

func (e *Errors) add(err error) {
	if err != nil {
		e.Errs = append(e.Errs, err)
	}
}

func (e *Errors) len() int { return len(e.Errs) }

// err returns the aggregate as an error, or nil when there are none.
func (e *Errors) err() error {
	if len(e.Errs) == 0 {
		return nil
	}
	return e
}

func (e *Errors) Error() string {
	// Sorted by path so two runs over the same file list problems in the same order,
	// and so problems in one budget appear together rather than interleaved.
	sorted := make([]error, len(e.Errs))
	copy(sorted, e.Errs)
	sort.SliceStable(sorted, func(i, j int) bool {
		return pathOf(sorted[i]) < pathOf(sorted[j])
	})

	var b strings.Builder
	if e.Source != "" {
		fmt.Fprintf(&b, "%s: %d problem(s):\n", e.Source, len(sorted))
	} else {
		fmt.Fprintf(&b, "%d problem(s):\n", len(sorted))
	}
	for _, err := range sorted {
		fmt.Fprintf(&b, "  %s\n", err.Error())
	}
	return strings.TrimRight(b.String(), "\n")
}

// Unwrap exposes the individual problems, so a test can assert on one without matching a
// rendered message and a caller can look for a specific field.
func (e *Errors) Unwrap() []error { return e.Errs }

func pathOf(err error) string {
	var fe FieldError
	if as(err, &fe) {
		return fe.Path
	}
	return ""
}

// as is errors.As for a FieldError, kept local to avoid importing errors purely for one
// type assertion in a sort comparator.
func as(err error, target *FieldError) bool {
	if fe, ok := err.(FieldError); ok {
		*target = fe
		return true
	}
	return false
}
