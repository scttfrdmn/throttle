package config

import (
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/budget"
)

// Flag overrides go through the same parsers a file does. These tests exist mostly to catch
// the two paths drifting apart: two implementations of "what is $4,000" is one more than the
// microdollar rule allows.

func TestApplyDefinitionOverrides(t *testing.T) {
	base := budget.Definition{ID: "research"}

	def, err := ApplyDefinitionOverrides(base, DefinitionOverrides{
		Amount:   ptr("$4,000"),
		Borrow:   ptr("3d"),
		Recur:    ptr("monthly"),
		Timezone: ptr("America/New_York"),
		Anchor:   ptr("2026-09-01"),
		Name:     ptr("Research group"),
	})
	if err != nil {
		t.Fatalf("ApplyDefinitionOverrides: %v", err)
	}

	if def.Allocation != dollars(4000) {
		t.Errorf("allocation = %s, want $4,000.00", def.Allocation.CentsString())
	}
	if def.Borrow != 72*time.Hour {
		t.Errorf("borrow = %v, want 72h", def.Borrow)
	}
	if def.Location.String() != "America/New_York" {
		t.Errorf("timezone = %v, want America/New_York", def.Location)
	}
	// The timezone is applied before the dates: parsing the anchor first would place it in
	// UTC and shift every period boundary by the offset.
	if got := def.AnchorAt.In(def.Location).Format(time.RFC3339); got != "2026-09-01T00:00:00-04:00" {
		t.Errorf("anchor = %s, want midnight in New York", got)
	}
}

// The same amount written to a flag and to a file produces the same definition, byte for
// byte in the fingerprint. This is what "one canonical configuration model" means in
// practice.
func TestFlagAndFileAgree(t *testing.T) {
	fromFile := def(t, load(t, `
budgets:
  research:
    name: Research group
    amount: $4,000
    borrow: 3d
    period:
      recur: monthly
      timezone: America/New_York
      anchor: 2026-09-01
    rollover:
      mode: credit
      cap:
        percent: 25
`), "research")

	fromFlags, err := ApplyDefinitionOverrides(budget.Definition{ID: "research"}, DefinitionOverrides{
		Name:       ptr("Research group"),
		Amount:     ptr("$4,000"),
		Borrow:     ptr("3d"),
		Recur:      ptr("monthly"),
		Timezone:   ptr("America/New_York"),
		Anchor:     ptr("2026-09-01"),
		Rollover:   ptr("credit"),
		CapPercent: ptr("25"),
	})
	if err != nil {
		t.Fatalf("ApplyDefinitionOverrides: %v", err)
	}

	if fromFile.Fingerprint() != fromFlags.Fingerprint() {
		t.Errorf("a file and flags describing the same budget produce different definitions:\n"+
			"file:  %+v\nflags: %+v", fromFile, fromFlags)
	}
}

// The anchor asymmetry is deliberate, and worth a test because it looks like an
// inconsistency.
//
// A file requires an anchor: it is read repeatedly, and a definition's fingerprint covers
// its anchor, so an anchor from the clock would make the same file describe a different
// budget in September than in October. A command line is typed once, so refusing to run
// without -anchor would be pedantry, and the first of the current month is what "monthly"
// means to the person typing it.
func TestFlagAnchorDefaultsToMonthStart(t *testing.T) {
	def, err := ApplyDefinitionOverrides(budget.Definition{ID: "research"}, DefinitionOverrides{
		Amount: ptr("$100"),
	})
	if err != nil {
		t.Fatalf("ApplyDefinitionOverrides: %v", err)
	}
	if def.AnchorAt.IsZero() {
		t.Fatal("no anchor was defaulted")
	}
	if got := def.AnchorAt.Day(); got != 1 {
		t.Errorf("anchor day = %d, want the first of the month", got)
	}
	if h, m, s := def.AnchorAt.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("anchor at %02d:%02d:%02d, want midnight", h, m, s)
	}
	// A file, by contrast, is refused. TestAnchorRequired covers that side; this asserts
	// the two really do differ, so a future change that "fixes the inconsistency" has to
	// choose deliberately.
	if _, err := LoadFile(mustWrite(t, "budgets:\n  a:\n    amount: $1\n")); err == nil {
		t.Error("a file with no anchor was accepted; the asymmetry has been lost")
	}
}

// mustWrite writes a config to a temp dir and returns the arguments LoadFile takes.
func mustWrite(t *testing.T, body string) (string, Env) {
	t.Helper()
	return writeConfig(t, body)
}

// Setting one cap form clears the other, because a definition carrying both is rejected by
// Validate and a flag that silently failed to take effect is worse than one that says why.
func TestCapFormsAreExclusive(t *testing.T) {
	withAmount, err := ApplyDefinitionOverrides(budget.Definition{ID: "a"}, DefinitionOverrides{
		Amount:     ptr("$100"),
		Rollover:   ptr("credit"),
		CapPercent: ptr("25"),
	})
	if err != nil {
		t.Fatalf("percent cap: %v", err)
	}
	replaced, err := ApplyDefinitionOverrides(withAmount, DefinitionOverrides{CapAmount: ptr("$10")})
	if err != nil {
		t.Fatalf("amount cap: %v", err)
	}
	if replaced.Rollover.CapBasisPoints != 0 {
		t.Errorf("basis points = %d, want cleared by the absolute cap", replaced.Rollover.CapBasisPoints)
	}
	if replaced.Rollover.Cap != dollars(10) {
		t.Errorf("cap = %s, want $10.00", replaced.Rollover.Cap.CentsString())
	}

	// Both at once on one command line is contradictory and is refused.
	_, err = ApplyDefinitionOverrides(budget.Definition{ID: "a"}, DefinitionOverrides{
		Amount:     ptr("$100"),
		Rollover:   ptr("credit"),
		CapAmount:  ptr("$10"),
		CapPercent: ptr("25"),
	})
	if err == nil {
		t.Fatal("want an error for both cap forms on one command line")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error does not say why: %v", err)
	}
}

// A calendar unit on a duration flag is refused, which is why these are strings and not
// time.Duration flags: a time.Duration flag would have accepted 720h for a month.
func TestOverrideRejectsCalendarDuration(t *testing.T) {
	_, err := ApplyDefinitionOverrides(budget.Definition{ID: "a"}, DefinitionOverrides{
		Amount: ptr("$100"),
		Borrow: ptr("1mo"),
	})
	if err == nil {
		t.Fatal("want an error for a calendar unit")
	}
	if !strings.Contains(err.Error(), "calendar unit") {
		t.Errorf("error does not explain why: %v", err)
	}
}

// Bad flag values are reported together and named as the reader typed them.
func TestOverrideErrorsAggregate(t *testing.T) {
	_, err := ApplyDefinitionOverrides(budget.Definition{ID: "a"}, DefinitionOverrides{
		Amount: ptr("$4O00"),
		Recur:  ptr("fortnightly"),
		Anchor: ptr("Sept 1"),
	})
	if err == nil {
		t.Fatal("want errors for three bad flags")
	}
	msg := err.Error()
	for _, want := range []string{"-budget", "-recur", "-anchor"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %s:\n%s", want, msg)
		}
	}
}

// An override never produces a definition the engine would reject: Validate is the last word
// on both paths.
func TestOverrideValidates(t *testing.T) {
	// recur: none with no end has a period with no bound, so pacing has no denominator.
	_, err := ApplyDefinitionOverrides(budget.Definition{ID: "a"}, DefinitionOverrides{
		Amount: ptr("$100"),
		Recur:  ptr("none"),
	})
	if err == nil {
		t.Fatal("want an error for a fixed term with no end")
	}
}
