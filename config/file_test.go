package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/money"
)

// writeConfig puts a document in a temp directory and returns its path.
//
// t.TempDir throughout, and an Env whose Home is that directory: nothing here can reach the
// real user's config or data directories even if a path resolution goes wrong.
func writeConfig(t *testing.T, body string) (path string, env Env) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path, testEnv(t, "linux", dir)
}

// load reads a document that is expected to be valid.
func load(t *testing.T, body string) Config {
	t.Helper()
	path, env := writeConfig(t, body)
	cfg, err := LoadFile(path, env)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return cfg
}

// loadErr reads a document that is expected to fail, returning the rendered message.
func loadErr(t *testing.T, body string) string {
	t.Helper()
	path, env := writeConfig(t, body)
	cfg, err := LoadFile(path, env)
	if err == nil {
		t.Fatalf("LoadFile succeeded, want an error; got %d budget(s)", len(cfg.Budgets))
	}
	return err.Error()
}

// def finds a budget by id or fails the test.
func def(t *testing.T, cfg Config, id string) budget.Definition {
	t.Helper()
	d, ok := cfg.Definition(id)
	if !ok {
		t.Fatalf("budget %q not in config", id)
	}
	return d
}

// (1) A minimal valid config: one budget, an amount, an anchor, nothing else.
func TestMinimalConfig(t *testing.T) {
	cfg := load(t, `
version: 1
budgets:
  default:
    amount: $100
    period:
      anchor: 2026-09-01
`)

	d := def(t, cfg, "default")
	if d.Allocation != dollars(100) {
		t.Errorf("allocation = %s, want $100.00", d.Allocation.CentsString())
	}
	// Monthly is the default recurrence, which is what "a budget" means to most people.
	if d.Recurrence != budget.RecurMonthly {
		t.Errorf("recurrence = %q, want monthly", d.Recurrence)
	}
	if d.Location != time.UTC {
		t.Errorf("location = %v, want UTC", d.Location)
	}
	// Defaults survive a file that says nothing about them.
	if cfg.Enforcement != engine.ModeEnforce {
		t.Errorf("enforcement = %q, want enforce", cfg.Enforcement)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("listen = %q, want %q", cfg.Listen, DefaultListen)
	}
	if err := d.Validate(); err != nil {
		t.Errorf("the loaded definition does not validate: %v", err)
	}
}

// (2) The shipped example parses under the real loader.
//
// It is documentation people copy, so a stale example is a first run that fails. This is the
// test that keeps examples/throttle.yaml executable rather than a sketch.
func TestExampleConfigParses(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "examples", "throttle.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	cfg := load(t, string(body))

	// The example is meant to demonstrate a specific shape: a parent, two children, and a
	// fixed grant. If one disappears, the prose around it no longer describes the file.
	for _, id := range []string{"research", "chat", "agents", "award"} {
		d := def(t, cfg, id)
		if err := d.Validate(); err != nil {
			t.Errorf("%s does not validate: %v", id, err)
		}
	}
	if got := def(t, cfg, "chat").ParentID; got != "research" {
		t.Errorf("chat parent = %q, want research", got)
	}
	if got := def(t, cfg, "award").Recurrence; got != budget.RecurNone {
		t.Errorf("award recurrence = %q, want none", got)
	}
	if cfg.DefaultBudget != "research" {
		t.Errorf("default budget = %q, want research", cfg.DefaultBudget)
	}
	// Parents before children, so the slice can be persisted in order: PutDefinition
	// requires the parent row to exist already.
	if idx := indexOf(cfg.Budgets, "research"); idx > indexOf(cfg.Budgets, "chat") {
		t.Error("children are ordered before their parent")
	}
}

// The example describes budgets that are live today, not ones that have not started or have
// expired.
//
// This is a staleness alarm, and it is deliberately tied to the wall clock. A file whose
// anchor is in the future parses perfectly and then reports nothing: "throttle status" on a
// budget that has not begun has no position to give, which is a baffling first result from a
// file the reader just copied. A fixed grant that has ended does the same. Executable
// documentation has to stay executable, and the only thing that notices the calendar moving
// past a hard-coded date is a test that looks at the calendar.
func TestExampleConfigIsCurrent(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "examples", "throttle.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	cfg := load(t, string(body))

	now := time.Now()
	for _, d := range cfg.Budgets {
		if _, err := d.PeriodFor(now); err != nil {
			t.Errorf("examples/throttle.yaml is stale: budget %q has no period covering now "+
				"(%v).\nMove its anchor back, or its end forward: a copied example that reports "+
				"no position is a broken first experience.", d.ID, err)
		}
	}
}

func indexOf(defs []budget.Definition, id string) int {
	for i, d := range defs {
		if d.ID == id {
			return i
		}
	}
	return -1
}

// (3) A monthly recurring budget compiles to the durable model, with no month-shaped
// special case: the same fields a fixed grant uses.
func TestMonthlyRecurring(t *testing.T) {
	cfg := load(t, `
budgets:
  research:
    amount: $4,000
    period:
      recur: monthly
      timezone: America/New_York
      anchor: 2026-09-01
`)

	d := def(t, cfg, "research")
	if d.Recurrence != budget.RecurMonthly {
		t.Fatalf("recurrence = %q, want monthly", d.Recurrence)
	}
	if d.Every != 0 {
		t.Errorf("every = %v, want zero for a calendar recurrence", d.Every)
	}
	if d.Location.String() != "America/New_York" {
		t.Fatalf("location = %v, want America/New_York", d.Location)
	}
	// The anchor is midnight in the budget's own zone, not midnight UTC -- which would be
	// 20:00 the previous day there and would shift every period boundary.
	if got := d.AnchorAt.In(d.Location).Format(time.RFC3339); got != "2026-09-01T00:00:00-04:00" {
		t.Errorf("anchor = %s, want midnight in New York", got)
	}

	// A month is whatever the calendar says, so consecutive periods differ in length. This
	// is the property a config layer that parsed "monthly" as 720h would destroy.
	sep, oct, err := d.Bounds(0)
	if err != nil {
		t.Fatalf("Bounds(0): %v", err)
	}
	if got := oct.Sub(sep); got != 30*24*time.Hour {
		t.Errorf("September = %v, want 720h", got)
	}
	_, nov, err := d.Bounds(1)
	if err != nil {
		t.Fatalf("Bounds(1): %v", err)
	}
	if got := nov.Sub(oct); got != 31*24*time.Hour {
		t.Errorf("October = %v, want 744h", got)
	}
	_, dec, err := d.Bounds(2)
	if err != nil {
		t.Fatalf("Bounds(2): %v", err)
	}
	if got := dec.Sub(nov); got != 30*24*time.Hour+time.Hour {
		// DST ends on 2026-11-01 in New York, so November is thirty days plus an hour. The
		// budget's month is the user's month, hour for hour.
		t.Errorf("November = %v, want 721h (30 days plus the DST hour)", got)
	}
}

// (4) An arbitrary fixed grant period: $125,000 from 2026-09-01 through 2028-08-31.
func TestFixedGrantPeriod(t *testing.T) {
	cfg := load(t, `
budgets:
  award:
    amount: $125,000
    period:
      recur: none
      anchor: 2026-09-01
      end: 2028-08-31
`)

	d := def(t, cfg, "award")
	if d.Allocation != dollars(125_000) {
		t.Errorf("allocation = %s, want $125,000.00", d.Allocation.CentsString())
	}
	// "Through 2028-08-31" includes the 31st. Reading a bare end date as the start of that
	// day would expire the grant a day early, and nothing about the result would look wrong.
	if got := d.EndAt.UTC().Format(time.RFC3339); got != "2028-09-01T00:00:00Z" {
		t.Errorf("end = %s, want the end of 2028-08-31", got)
	}
	// One envelope, and no second one.
	if _, _, err := d.Bounds(1); err == nil {
		t.Error("a fixed grant produced a second period")
	}
}

// A shorter arbitrary envelope: the same model at a different scale.
func TestDurationPeriod(t *testing.T) {
	cfg := load(t, `
budgets:
  experiment:
    amount: $50
    period:
      recur: duration
      every: 6h
      anchor: 2026-09-01T00:00:00Z
`)

	d := def(t, cfg, "experiment")
	if d.Every != 6*time.Hour {
		t.Errorf("every = %v, want 6h", d.Every)
	}
	start, end, err := d.Bounds(3)
	if err != nil {
		t.Fatalf("Bounds(3): %v", err)
	}
	if got := end.Sub(start); got != 6*time.Hour {
		t.Errorf("period length = %v, want 6h", got)
	}
}

// (5) A child resolves its parent by the name in the file. Nobody has to know a UUID.
func TestSubBudgetInheritance(t *testing.T) {
	cfg := load(t, `
budgets:
  research:
    amount: $4,000
    period:
      recur: monthly
      timezone: America/New_York
      anchor: 2026-09-01
    borrow: 72h
  chat:
    parent: research
    amount: $1,000
  agents:
    parent: research
    amount: $2,500
    period:
      recur: weekly
      anchor: 2026-09-07
`)

	parent := def(t, cfg, "research")
	chat := def(t, cfg, "chat")

	// The period is inherited, which is what makes the parent's and child's numbers
	// comparable: a child's spend consumes the parent's headroom.
	if chat.Recurrence != parent.Recurrence {
		t.Errorf("chat recurrence = %q, want the parent's %q", chat.Recurrence, parent.Recurrence)
	}
	if chat.Location.String() != "America/New_York" {
		t.Errorf("chat timezone = %v, want the parent's", chat.Location)
	}
	if !chat.AnchorAt.Equal(parent.AnchorAt) {
		t.Errorf("chat anchor = %v, want the parent's %v", chat.AnchorAt, parent.AnchorAt)
	}

	// Allocation is never inherited: a child whose amount was silently the parent's would
	// double the apparent commitment.
	if chat.Allocation != dollars(1000) {
		t.Errorf("chat allocation = %s, want $1,000.00", chat.Allocation.CentsString())
	}
	// Nor is borrow.
	if chat.Borrow != 0 {
		t.Errorf("chat borrow = %v, want zero", chat.Borrow)
	}

	// A child that states a period gets it rather than the parent's.
	agents := def(t, cfg, "agents")
	if agents.Recurrence != budget.RecurWeekly {
		t.Errorf("agents recurrence = %q, want weekly", agents.Recurrence)
	}
	// An explicit period does not drag the parent's timezone away, though: the child said
	// nothing about the zone, so it still inherits it.
	if agents.Location.String() != "America/New_York" {
		t.Errorf("agents timezone = %v, want the parent's", agents.Location)
	}
}

// A child that explicitly sets a field to the zero value keeps it, rather than inheriting.
func TestChildExplicitTimezoneBeatsInheritance(t *testing.T) {
	cfg := load(t, `
budgets:
  research:
    amount: $4,000
    period:
      timezone: America/New_York
      anchor: 2026-09-01
  chat:
    parent: research
    amount: $1,000
    period:
      timezone: UTC
`)

	// timezone: UTC is a choice, not an omission, and inheritance must not overwrite it.
	if got := def(t, cfg, "chat").Location.String(); got != "UTC" {
		t.Errorf("chat timezone = %q, want UTC", got)
	}
	// The anchor was still omitted, so it is still inherited -- and it is read in the
	// parent's zone, which is where the parent's boundaries fall.
	if got := def(t, cfg, "chat").AnchorAt; !got.Equal(def(t, cfg, "research").AnchorAt) {
		t.Errorf("chat anchor = %v, want the parent's", got)
	}
}

// (6) A missing parent names what is available, because the likely cause is a typo.
func TestMissingParent(t *testing.T) {
	msg := loadErr(t, `
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
  chat:
    parent: researchx
    amount: $1,000
`)

	for _, want := range []string{"budgets.chat.parent", `"researchx"`, "research"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q:\n%s", want, msg)
		}
	}
}

// (7) A parent cycle is reported as a path, because "there is a cycle" does not tell the
// reader which line to delete.
func TestParentCycle(t *testing.T) {
	msg := loadErr(t, `
budgets:
  a:
    parent: b
    amount: $1
    period:
      anchor: 2026-09-01
  b:
    parent: a
    amount: $1
    period:
      anchor: 2026-09-01
`)

	if !strings.Contains(msg, "cycle") {
		t.Errorf("error does not mention a cycle:\n%s", msg)
	}
	if !strings.Contains(msg, "->") {
		t.Errorf("error does not show the cycle path:\n%s", msg)
	}
}

// A budget that is its own parent is the same defect written more compactly.
func TestSelfParent(t *testing.T) {
	msg := loadErr(t, `
budgets:
  a:
    parent: a
    amount: $1
    period:
      anchor: 2026-09-01
`)
	if !strings.Contains(msg, "cycle") {
		t.Errorf("error does not mention a cycle:\n%s", msg)
	}
}

// (8) An absolute rollover cap.
func TestRolloverCapAmount(t *testing.T) {
	cfg := load(t, `
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
    rollover:
      mode: credit
      cap:
        amount: $1,000
`)

	d := def(t, cfg, "research")
	if d.Rollover.Mode != budget.RolloverCredit {
		t.Errorf("mode = %q, want credit", d.Rollover.Mode)
	}
	if d.Rollover.Cap != dollars(1000) {
		t.Errorf("cap = %s, want $1,000.00", d.Rollover.Cap.CentsString())
	}
	if d.Rollover.CapBasisPoints != 0 {
		t.Errorf("cap basis points = %d, want zero", d.Rollover.CapBasisPoints)
	}
}

// (9) A percentage cap, in exact basis points rather than through a float.
func TestRolloverCapPercent(t *testing.T) {
	cfg := load(t, `
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
    rollover:
      mode: credit
      cap:
        percent: 25
`)

	d := def(t, cfg, "research")
	if d.Rollover.CapBasisPoints != 2500 {
		t.Errorf("cap basis points = %d, want 2500", d.Rollover.CapBasisPoints)
	}
	if d.Rollover.Cap != 0 {
		t.Errorf("absolute cap = %s, want zero", d.Rollover.Cap.CentsString())
	}
	// 12.34% is exactly 1234 basis points. A float path would make this 1233 or 1234
	// depending on the platform, and the discrepancy would be in the fourth decimal place
	// where nobody looks.
	cfg = load(t, `
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
    rollover:
      mode: credit
      cap:
        percent: 12.34
`)
	if got := def(t, cfg, "research").Rollover.CapBasisPoints; got != 1234 {
		t.Errorf("12.34%% = %d basis points, want 1234", got)
	}

	// Finer than a basis point is refused rather than rounded: a cap the user typed and a
	// cap throttle stored differing silently is worse than an error.
	msg := loadErr(t, `
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
    rollover:
      mode: credit
      cap:
        percent: 12.345
`)
	if !strings.Contains(msg, "basis point") {
		t.Errorf("error does not explain the precision limit:\n%s", msg)
	}
}

// (10) Both cap forms together is contradictory configuration, caught before anything
// durable is written rather than resolved in an order nobody chose.
func TestRolloverBothCapFormsRejected(t *testing.T) {
	msg := loadErr(t, `
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
    rollover:
      mode: credit
      cap:
        amount: $1,000
        percent: 25
`)

	if !strings.Contains(msg, "budgets.research.rollover.cap") {
		t.Errorf("error does not name the field:\n%s", msg)
	}
	if !strings.Contains(msg, "mutually exclusive") {
		t.Errorf("error does not say why:\n%s", msg)
	}
}

// A cap under mode: none is inert, and inert configuration is a written belief about
// behavior that is not happening.
func TestRolloverCapWithModeNone(t *testing.T) {
	msg := loadErr(t, `
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
    rollover:
      mode: none
      cap:
        percent: 25
`)
	if !strings.Contains(msg, "no effect") {
		t.Errorf("error does not explain that the cap is inert:\n%s", msg)
	}
}

// An empty cap block is a half-finished edit, not a request for uncapped carry.
func TestRolloverEmptyCapRejected(t *testing.T) {
	msg := loadErr(t, `
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
    rollover:
      mode: credit
      cap: {}
`)
	if !strings.Contains(msg, "amount or percent") {
		t.Errorf("error does not say what the cap needs:\n%s", msg)
	}
}

// An omitted rollover block fingerprints the same as an explicit mode: none.
//
// These are the same policy -- Validate accepts both spellings and CarryInto returns zero
// for both -- and the ledger stores the canonical one. When they fingerprinted differently,
// "config check" run straight after "define" reported a budget as drifting from its own
// stored copy.
func TestOmittedRolloverMatchesExplicitNone(t *testing.T) {
	body := `
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
`
	omitted := def(t, load(t, body), "research")
	explicit := def(t, load(t, body+"    rollover:\n      mode: none\n"), "research")

	if omitted.Fingerprint() != explicit.Fingerprint() {
		t.Errorf("an omitted rollover block and mode: none fingerprint differently:\n  %+v\n  %+v",
			omitted.Rollover, explicit.Rollover)
	}
	if got := DescribeRollover(omitted.Rollover); got != "none" {
		t.Errorf("DescribeRollover = %q, want none", got)
	}
}

// (11) A borrow window is a fixed duration, and days are exact multiples of it.
func TestBorrowDuration(t *testing.T) {
	cfg := load(t, `
budgets:
  research:
    amount: $4,000
    borrow: 3d
    period:
      anchor: 2026-09-01
`)
	if got := def(t, cfg, "research").Borrow; got != 72*time.Hour {
		t.Errorf("borrow = %v, want 72h", got)
	}
}

// A calendar unit is refused rather than approximated. This is the "720h is not a month"
// rule at the parsing layer: accepting "1mo" as 720h is how one month of borrow silently
// becomes thirty days in a month that has thirty-one.
func TestCalendarDurationsRejected(t *testing.T) {
	for _, raw := range []string{"1mo", "1month", "2months", "1y", "1yr", "1year"} {
		t.Run(raw, func(t *testing.T) {
			msg := loadErr(t, `
budgets:
  research:
    amount: $4,000
    borrow: `+raw+`
    period:
      anchor: 2026-09-01
`)
			if !strings.Contains(msg, "calendar unit") {
				t.Errorf("error does not explain why %q is refused:\n%s", raw, msg)
			}
			if !strings.Contains(msg, "period.recur") {
				t.Errorf("error does not point at where calendar periods are set:\n%s", msg)
			}
		})
	}
}

// (12) Invalid money never reaches a definition, and the error names the field and the
// value rather than saying "parse error".
func TestInvalidMoney(t *testing.T) {
	msg := loadErr(t, `
budgets:
  research:
    amount: $4O00
    period:
      anchor: 2026-09-01
`)

	if !strings.Contains(msg, "budgets.research.amount") {
		t.Errorf("error does not name the field path:\n%s", msg)
	}
	if !strings.Contains(msg, `invalid money "$4O00"`) {
		t.Errorf("error does not quote the offending value:\n%s", msg)
	}
}

// Amounts that do parse are exact microdollars. A float64 path would make one of these
// land a microdollar off, which is the whole reason money is an integer here.
func TestMoneyIsExact(t *testing.T) {
	tests := []struct {
		raw  string
		want money.Money
	}{
		{"$4,000", dollars(4000)},
		{"4000", dollars(4000)},
		{"$0.10", cents(10)},
		{"$0.07", cents(7)},
		{"$1,234.56", cents(123456)},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			cfg := load(t, `
budgets:
  b:
    amount: "`+tt.raw+`"
    period:
      anchor: 2026-09-01
`)
			if got := def(t, cfg, "b").Allocation; got != tt.want {
				t.Errorf("%s = %d microdollars, want %d", tt.raw, int64(got), int64(tt.want))
			}
		})
	}
}

// A negative allocation is money owed, not money granted.
func TestNegativeAmountRejected(t *testing.T) {
	msg := loadErr(t, `
budgets:
  b:
    amount: -$100
    period:
      anchor: 2026-09-01
`)
	if !strings.Contains(msg, "negative") {
		t.Errorf("error does not say the amount is negative:\n%s", msg)
	}
}

// (13) DST and calendar-period semantics survive the config layer.
//
// The engine already gets this right; the risk a config file introduces is a timezone that
// arrives as a string and is dropped, silently moving every boundary to UTC.
func TestDSTSemanticsPreserved(t *testing.T) {
	cfg := load(t, `
budgets:
  daily:
    amount: $10
    period:
      recur: daily
      timezone: America/New_York
      anchor: 2026-03-07
`)

	d := def(t, cfg, "daily")

	// 2026-03-08 is the spring-forward day in New York: 23 hours long, not 24. A budget
	// paced across it must use the real length, and a definition that lost its zone would
	// report a flat 24.
	start, end, err := d.Bounds(1)
	if err != nil {
		t.Fatalf("Bounds(1): %v", err)
	}
	if got := end.Sub(start); got != 23*time.Hour {
		t.Errorf("2026-03-08 = %v, want 23h (spring forward)", got)
	}
	if got := start.In(d.Location).Format("2006-01-02 15:04"); got != "2026-03-08 00:00" {
		t.Errorf("period starts at %s, want local midnight", got)
	}

	// Every boundary is local midnight, including across the transition -- which is the
	// property a UTC-flattened anchor would break.
	for seq := 0; seq < 5; seq++ {
		s, _, err := d.Bounds(seq)
		if err != nil {
			t.Fatalf("Bounds(%d): %v", seq, err)
		}
		if got := s.In(d.Location).Format("15:04"); got != "00:00" {
			t.Errorf("period %d starts at %s local, want midnight", seq, got)
		}
	}
}

// (25) Errors name the field path and are reported together, because a config file is
// hand-edited and one typo at a time turns five mistakes into five edit-run cycles.
func TestErrorsAreAggregatedAndPathed(t *testing.T) {
	msg := loadErr(t, `
defaults:
  enforcement: pretend
budgets:
  a:
    amount: $4O00
    period:
      anchor: 2026-09-01
  b:
    amount: $100
    borrow: 1mo
    period:
      anchor: 2026-09-01
`)

	for _, want := range []string{
		"defaults.enforcement",
		"budgets.a.amount",
		"budgets.b.borrow",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not report %s:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "3 problem(s)") {
		t.Errorf("error does not count the problems:\n%s", msg)
	}
	// The file is named, so the reader knows which one to open.
	if !strings.Contains(msg, ConfigFileName) {
		t.Errorf("error does not name the file:\n%s", msg)
	}
}

// A required anchor is required, and the message says what to write.
func TestAnchorRequired(t *testing.T) {
	msg := loadErr(t, `
budgets:
  research:
    amount: $4,000
`)
	if !strings.Contains(msg, "budgets.research.period.anchor") {
		t.Errorf("error does not name the anchor field:\n%s", msg)
	}
	if !strings.Contains(msg, "2026-09-01") {
		t.Errorf("error does not show the expected form:\n%s", msg)
	}
}

// A child with no anchor anywhere in its chain gets a message that says where to look,
// since the child itself is not where the fix goes.
func TestChildAnchorErrorPointsAtParent(t *testing.T) {
	msg := loadErr(t, `
budgets:
  research:
    amount: $4,000
  chat:
    parent: research
    amount: $1,000
`)
	if !strings.Contains(msg, "the parent has none either") {
		t.Errorf("error does not explain where a child's anchor comes from:\n%s", msg)
	}
}

// (26) An unknown field is an error, not a shrug.
//
// A silently ignored "alloction:" is a budget quietly running on no allocation at all, and
// the config check that exists to catch exactly that would report the file as clean.
func TestUnknownFieldsRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"top level", "verison: 1\n"},
		{"budget field", "budgets:\n  a:\n    alloction: $100\n    period:\n      anchor: 2026-09-01\n"},
		{"period field", "budgets:\n  a:\n    amount: $100\n    period:\n      anchr: 2026-09-01\n"},
		{"rollover field", "budgets:\n  a:\n    amount: $100\n    period:\n      anchor: 2026-09-01\n    rollover:\n      mode: credit\n      limit: $5\n"},
		{"dashboard field", "dashboard:\n  bind: 127.0.0.1:1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := loadErr(t, tt.body)
			if !strings.Contains(msg, "not a throttle configuration field") &&
				!strings.Contains(msg, "unknown") {
				t.Errorf("error does not identify the unknown field:\n%s", msg)
			}
			// The library's own wording names a Go type, which means nothing to somebody
			// editing YAML.
			if strings.Contains(msg, "config.file") || strings.Contains(msg, "fileBudget") {
				t.Errorf("error leaks an internal Go type name:\n%s", msg)
			}
		})
	}
}

// (27, schema version) An omitted version means 1; a version this build does not know is an
// error naming what it does know.
func TestSchemaVersion(t *testing.T) {
	// Omitted: accepted. Requiring a version line in a first hand-written config is
	// friction with nothing behind it.
	load(t, `
budgets:
  a:
    amount: $100
    period:
      anchor: 2026-09-01
`)

	// Explicit and current: accepted.
	load(t, `
version: 1
budgets:
  a:
    amount: $100
    period:
      anchor: 2026-09-01
`)

	msg := loadErr(t, `
version: 2
budgets:
  a:
    amount: $100
    period:
      anchor: 2026-09-01
`)
	if !strings.Contains(msg, "version") {
		t.Errorf("error does not mention the version:\n%s", msg)
	}
	if !strings.Contains(msg, "1") {
		t.Errorf("error does not name the understood version:\n%s", msg)
	}
}

// defaults.budget naming a budget the file does not define is a typo worth catching: every
// command that falls back to the default would otherwise fail one at a time.
func TestDefaultBudgetMustExist(t *testing.T) {
	msg := loadErr(t, `
defaults:
  budget: reserch
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
`)
	if !strings.Contains(msg, "defaults.budget") {
		t.Errorf("error does not name the field:\n%s", msg)
	}
	if !strings.Contains(msg, "research") {
		t.Errorf("error does not list what is defined:\n%s", msg)
	}
}

// An empty listen address means every interface, which is not what an omitted field means.
func TestEmptyListenRejected(t *testing.T) {
	msg := loadErr(t, `
dashboard:
  listen: ""
`)
	if !strings.Contains(msg, "every interface") {
		t.Errorf("error does not explain what an empty address does:\n%s", msg)
	}
}

// A lease of zero would let recovery reclaim a hold the instant it was taken.
func TestZeroLeaseRejected(t *testing.T) {
	msg := loadErr(t, `
defaults:
  lease: 0s
`)
	if !strings.Contains(msg, "defaults.lease") {
		t.Errorf("error does not name the field:\n%s", msg)
	}
}

// A missing file is distinguishable from an unparseable one, because the caller treats the
// two differently: a default location that does not exist is normal.
func TestLoadFileMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadFile(filepath.Join(dir, "absent.yaml"), testEnv(t, "linux", dir))
	if !errors.Is(err, ErrNotExist) {
		t.Errorf("error = %v, want ErrNotExist", err)
	}
}

// Loading records where each value came from, which is what makes "config show" able to
// answer "why is it using that ledger?".
func TestOriginsRecorded(t *testing.T) {
	cfg := load(t, `
store:
  ledger: ~/from-file/ledger.db
defaults:
  enforcement: monitor
dashboard:
  listen: 127.0.0.1:9999
budgets:
  a:
    amount: $100
    period:
      anchor: 2026-09-01
`)

	for _, path := range []string{"store.ledger", "defaults.enforcement", "dashboard.listen"} {
		if got := cfg.source(path); got != FromFile {
			t.Errorf("%s came from %q, want the config file", path, got)
		}
	}
	// Untouched fields still say "default", not "config file".
	if got := cfg.source("store.activity"); got != FromDefault {
		t.Errorf("store.activity came from %q, want the default", got)
	}
	if !cfg.ListenFromFile() {
		t.Error("ListenFromFile is false for an address set in the file")
	}
}

// Parsing has no side effects: no database, no directory, nothing but the file read.
//
// (15) is the CLI-level version of this. Here it is the loader's own guarantee, which is
// what makes "config check" honestly read-only.
func TestLoadingCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	body := `
store:
  ledger: ` + filepath.Join(dir, "sub", "ledger.db") + `
  activity: ` + filepath.Join(dir, "sub", "activity.db") + `
budgets:
  a:
    amount: $100
    period:
      anchor: 2026-09-01
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := LoadFile(path, testEnv(t, "linux", dir)); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != ConfigFileName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("loading created files: %v", names)
	}
}
