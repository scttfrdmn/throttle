package budget

import (
	"errors"
	"testing"
	"time"

	"throttle/money"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("timezone %q unavailable: %v", name, err)
	}
	return loc
}

func monthlyDef(anchor time.Time) Definition {
	return Definition{
		ID:         "research",
		Allocation: dollars(4000),
		Recurrence: RecurMonthly,
		AnchorAt:   anchor,
	}
}

func TestDefinitionValidate(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		def     Definition
		wantErr bool
	}{
		{"monthly", monthlyDef(anchor), false},
		{"missing id", Definition{Allocation: 1, Recurrence: RecurMonthly, AnchorAt: anchor}, true},
		{"missing anchor", Definition{ID: "a", Recurrence: RecurMonthly}, true},
		{"missing recurrence", Definition{ID: "a", AnchorAt: anchor}, true},
		{"unknown recurrence", Definition{ID: "a", Recurrence: "yearly", AnchorAt: anchor}, true},
		{"negative allocation", Definition{ID: "a", Allocation: -1, Recurrence: RecurMonthly, AnchorAt: anchor}, true},
		{"negative borrow", Definition{ID: "a", Borrow: -time.Hour, Recurrence: RecurMonthly, AnchorAt: anchor}, true},
		{"self parent", Definition{ID: "a", ParentID: "a", Recurrence: RecurMonthly, AnchorAt: anchor}, true},
		{"none without end", Definition{ID: "a", Recurrence: RecurNone, AnchorAt: anchor}, true},
		{"none with end", Definition{ID: "a", Recurrence: RecurNone, AnchorAt: anchor, EndAt: anchor.AddDate(1, 0, 0)}, false},
		{"duration without every", Definition{ID: "a", Recurrence: RecurDuration, AnchorAt: anchor}, true},
		{"duration with every", Definition{ID: "a", Recurrence: RecurDuration, Every: 6 * time.Hour, AnchorAt: anchor}, false},
		{"end before anchor", Definition{ID: "a", Recurrence: RecurMonthly, AnchorAt: anchor, EndAt: anchor.AddDate(-1, 0, 0)}, true},
		{
			"both cap forms",
			Definition{ID: "a", Recurrence: RecurMonthly, AnchorAt: anchor,
				Rollover: RolloverPolicy{Mode: RolloverCredit, Cap: dollars(100), CapBasisPoints: 2500}},
			true,
		},
		{
			"percentage cap alone",
			Definition{ID: "a", Recurrence: RecurMonthly, AnchorAt: anchor,
				Rollover: RolloverPolicy{Mode: RolloverCredit, CapBasisPoints: 2500}},
			false,
		},
		{
			"negative basis points",
			Definition{ID: "a", Recurrence: RecurMonthly, AnchorAt: anchor,
				Rollover: RolloverPolicy{Mode: RolloverCredit, CapBasisPoints: -1}},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.def.Validate(); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestMonthlyBoundsFollowCalendar is the reason Definition owns recurrence: a
// month of budget must be the user's month, not 30 fixed days.
func TestMonthlyBoundsFollowCalendar(t *testing.T) {
	def := monthlyDef(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	want := []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	for seq, days := range want {
		start, end, err := def.Bounds(seq)
		if err != nil {
			t.Fatalf("Bounds(%d): %v", seq, err)
		}
		if got := int(end.Sub(start) / (24 * time.Hour)); got != days {
			t.Errorf("period %d (%s) = %d days, want %d", seq, start.Format("2006-01"), got, days)
		}
	}

	// 2028 is a leap year: February must be 29 days.
	leap := monthlyDef(time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC))
	start, end, err := leap.Bounds(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(end.Sub(start) / (24 * time.Hour)); got != 29 {
		t.Errorf("February 2028 = %d days, want 29", got)
	}
}

// TestMonthlyBoundsTileWithoutGaps guarantees no money can hide in a crack
// between periods and no instant is charged to two periods.
func TestMonthlyBoundsTileWithoutGaps(t *testing.T) {
	def := monthlyDef(time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC))

	_, prevEnd, err := def.Bounds(0)
	if err != nil {
		t.Fatal(err)
	}
	for seq := 1; seq < 40; seq++ {
		start, end, err := def.Bounds(seq)
		if err != nil {
			t.Fatalf("Bounds(%d): %v", seq, err)
		}
		if !start.Equal(prevEnd) {
			t.Fatalf("period %d starts at %s but period %d ended at %s",
				seq, start.Format(time.RFC3339), seq-1, prevEnd.Format(time.RFC3339))
		}
		if !end.After(start) {
			t.Fatalf("period %d is empty", seq)
		}
		prevEnd = end
	}
}

// TestMonthEndAnchorDoesNotDrift covers the AddDate normalization trap: January
// 31 plus one month is March 3 unless the day is clamped, which would silently
// move every later boundary.
func TestMonthEndAnchorDoesNotDrift(t *testing.T) {
	def := monthlyDef(time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC))

	want := []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30", "2026-05-31"}
	for seq, w := range want {
		start, _, err := def.Bounds(seq)
		if err != nil {
			t.Fatalf("Bounds(%d): %v", seq, err)
		}
		if got := start.Format("2006-01-02"); got != w {
			t.Errorf("period %d starts %s, want %s", seq, got, w)
		}
	}
}

// TestDSTPeriodLengths checks that calendar recurrence in a DST zone yields
// periods that are genuinely an hour short or long, rather than pretending every
// day is 24 hours.
func TestDSTPeriodLengths(t *testing.T) {
	ny := mustLoc(t, "America/New_York")

	// Daily periods across the 2026 spring-forward (March 8) and fall-back
	// (November 1) transitions.
	spring := Definition{
		ID: "d", Allocation: dollars(10), Recurrence: RecurDaily,
		Location: ny, AnchorAt: time.Date(2026, 3, 8, 0, 0, 0, 0, ny),
	}
	if _, _, err := spring.Bounds(0); err != nil {
		t.Fatal(err)
	}
	s, e, _ := spring.Bounds(0)
	if got := e.Sub(s); got != 23*time.Hour {
		t.Errorf("spring-forward day = %v, want 23h", got)
	}

	fall := Definition{
		ID: "d", Allocation: dollars(10), Recurrence: RecurDaily,
		Location: ny, AnchorAt: time.Date(2026, 11, 1, 0, 0, 0, 0, ny),
	}
	s, e, _ = fall.Bounds(0)
	if got := e.Sub(s); got != 25*time.Hour {
		t.Errorf("fall-back day = %v, want 25h", got)
	}

	// A month containing a transition is likewise an hour off a whole number of
	// days, and pacing must use the real duration.
	march := Definition{
		ID: "m", Allocation: dollars(100), Recurrence: RecurMonthly,
		Location: ny, AnchorAt: time.Date(2026, 3, 1, 0, 0, 0, 0, ny),
	}
	s, e, _ = march.Bounds(0)
	if got, want := e.Sub(s), 31*24*time.Hour-time.Hour; got != want {
		t.Errorf("March in New York = %v, want %v", got, want)
	}
}

func TestPeriodForRoundTrips(t *testing.T) {
	defs := []Definition{
		monthlyDef(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		monthlyDef(time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)),
		{ID: "w", Allocation: dollars(10), Recurrence: RecurWeekly, AnchorAt: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)},
		{ID: "d", Allocation: dollars(10), Recurrence: RecurDaily, AnchorAt: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)},
		{ID: "h", Allocation: dollars(10), Recurrence: RecurDuration, Every: 6 * time.Hour, AnchorAt: time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)},
		{
			ID: "tz", Allocation: dollars(10), Recurrence: RecurMonthly,
			Location: mustLoc(t, "America/New_York"),
			AnchorAt: time.Date(2026, 1, 1, 0, 0, 0, 0, mustLoc(t, "America/New_York")),
		},
	}

	for _, def := range defs {
		t.Run(def.ID+"/"+string(def.Recurrence), func(t *testing.T) {
			for seq := 0; seq < 30; seq++ {
				start, end, err := def.Bounds(seq)
				if err != nil {
					t.Fatalf("Bounds(%d): %v", seq, err)
				}
				// The start, the interior, and the last instant must all map back
				// to this period; the end must belong to the next one.
				for _, at := range []time.Time{start, start.Add(end.Sub(start) / 2), end.Add(-time.Nanosecond)} {
					got, err := def.PeriodFor(at)
					if err != nil {
						t.Fatalf("PeriodFor(%s): %v", at.Format(time.RFC3339Nano), err)
					}
					if got != seq {
						t.Errorf("PeriodFor(%s) = %d, want %d", at.Format(time.RFC3339Nano), got, seq)
					}
				}
				if got, err := def.PeriodFor(end); err == nil && got != seq+1 {
					t.Errorf("PeriodFor(end of %d) = %d, want %d", seq, got, seq+1)
				}
			}
		})
	}
}

func TestPeriodForOutsideDefinition(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	def := monthlyDef(anchor)

	if _, err := def.PeriodFor(anchor.Add(-time.Nanosecond)); !errors.Is(err, ErrNoSuchPeriod) {
		t.Errorf("before anchor: error = %v, want ErrNoSuchPeriod", err)
	}

	bounded := def
	bounded.EndAt = anchor.AddDate(0, 3, 0)
	if _, err := bounded.PeriodFor(bounded.EndAt); !errors.Is(err, ErrNoSuchPeriod) {
		t.Errorf("at definition end: error = %v, want ErrNoSuchPeriod", err)
	}
	if _, _, err := bounded.Bounds(3); !errors.Is(err, ErrNoSuchPeriod) {
		t.Errorf("Bounds past end: error = %v, want ErrNoSuchPeriod", err)
	}
}

// TestBoundedDefinitionTruncatesLastPeriod covers a grant that ends mid-month.
func TestBoundedDefinitionTruncatesLastPeriod(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	def := monthlyDef(anchor)
	def.EndAt = time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	_, end, err := def.Bounds(2)
	if err != nil {
		t.Fatal(err)
	}
	if !end.Equal(def.EndAt) {
		t.Errorf("last period ends %s, want the definition end %s",
			end.Format(time.RFC3339), def.EndAt.Format(time.RFC3339))
	}
	if _, _, err := def.Bounds(3); !errors.Is(err, ErrNoSuchPeriod) {
		t.Errorf("Bounds(3): error = %v, want ErrNoSuchPeriod", err)
	}
}

func TestNonRecurringHasExactlyOnePeriod(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	def := Definition{
		ID: "grant", Allocation: dollars(50000), Recurrence: RecurNone,
		AnchorAt: anchor, EndAt: anchor.AddDate(1, 0, 0),
	}
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	start, end, err := def.Bounds(0)
	if err != nil {
		t.Fatal(err)
	}
	if !start.Equal(anchor) || !end.Equal(def.EndAt) {
		t.Errorf("bounds = [%s, %s), want the whole definition", start, end)
	}
	if _, _, err := def.Bounds(1); !errors.Is(err, ErrNotRecurring) {
		t.Errorf("Bounds(1): error = %v, want ErrNotRecurring", err)
	}
}

func TestEnvelopeMaterializesPeriod(t *testing.T) {
	def := monthlyDef(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	def.Borrow = 72 * time.Hour
	def.Rollover = RolloverPolicy{Mode: RolloverBalance, CapBasisPoints: 2500}

	env, err := def.Envelope(1, dollars(250))
	if err != nil {
		t.Fatal(err)
	}
	if env.ID != "research@1" {
		t.Errorf("ID = %q, want research@1", env.ID)
	}
	if env.Allocation != dollars(4000) || env.Carry != dollars(250) {
		t.Errorf("allocation/carry = %s/%s", env.Allocation, env.Carry)
	}
	if env.Borrow != 72*time.Hour {
		t.Errorf("Borrow = %v", env.Borrow)
	}
	if env.Rollover != def.Rollover {
		t.Errorf("Rollover = %+v, want %+v", env.Rollover, def.Rollover)
	}
	if got := env.Total(); got != dollars(4250) {
		t.Errorf("Total = %s, want %s", got, dollars(4250))
	}
}

// TestFingerprintDetectsSemanticChange is the mechanism that stops two processes
// from silently sharing a ledger under conflicting definitions.
func TestFingerprintDetectsSemanticChange(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := monthlyDef(anchor)
	base.Name = "Research"

	if base.Fingerprint() != base.Fingerprint() {
		t.Fatal("Fingerprint is not stable across calls")
	}

	// Renaming is a display change, not a semantic conflict.
	renamed := base
	renamed.Name = "Research Group"
	if renamed.Fingerprint() != base.Fingerprint() {
		t.Error("renaming changed the fingerprint; name must not be semantic")
	}

	mutations := map[string]func(*Definition){
		"allocation": func(d *Definition) { d.Allocation = dollars(5000) },
		"parent":     func(d *Definition) { d.ParentID = "org" },
		"borrow":     func(d *Definition) { d.Borrow = time.Hour },
		"mode":       func(d *Definition) { d.Rollover.Mode = RolloverCredit },
		"cap":        func(d *Definition) { d.Rollover.Cap = dollars(10) },
		"capbp":      func(d *Definition) { d.Rollover.CapBasisPoints = 2500 },
		"recurrence": func(d *Definition) { d.Recurrence = RecurWeekly },
		"every":      func(d *Definition) { d.Every = time.Hour },
		"anchor":     func(d *Definition) { d.AnchorAt = anchor.Add(time.Hour) },
		"end":        func(d *Definition) { d.EndAt = anchor.AddDate(1, 0, 0) },
		"timezone":   func(d *Definition) { d.Location = mustLoc(t, "America/New_York") },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if changed.Fingerprint() == base.Fingerprint() {
				t.Errorf("changing %s did not change the fingerprint", name)
			}
		})
	}
}

// TestFingerprintStableForZeroEnd guards a subtle trap: a zero time's UnixNano
// is a huge negative number, so an open-ended definition must be special-cased
// to fingerprint stably.
func TestFingerprintStableForZeroEnd(t *testing.T) {
	def := monthlyDef(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	a := def.Fingerprint()

	// A different zero-valued time.Time must fingerprint identically.
	def.EndAt = time.Time{}
	if b := def.Fingerprint(); a != b {
		t.Errorf("open-ended fingerprint unstable: %s vs %s", a, b)
	}
}

func TestPeriodIDIsOrderedAndUnique(t *testing.T) {
	def := monthlyDef(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	seen := map[string]bool{}
	for seq := 0; seq < 100; seq++ {
		id := def.PeriodID(seq)
		if seen[id] {
			t.Fatalf("duplicate period id %q", id)
		}
		seen[id] = true
	}
	if got := def.PeriodID(7); got != "research@7" {
		t.Errorf("PeriodID(7) = %q, want research@7", got)
	}
}

func TestDaysInMonth(t *testing.T) {
	tests := []struct {
		year  int
		month time.Month
		want  int
	}{
		{2026, time.February, 28},
		{2028, time.February, 29},
		{2000, time.February, 29}, // divisible by 400
		{1900, time.February, 28}, // divisible by 100 but not 400
		{2026, time.January, 31},
		{2026, time.April, 30},
		{2026, time.December, 31},
	}
	for _, tc := range tests {
		if got := daysInMonth(tc.year, tc.month); got != tc.want {
			t.Errorf("daysInMonth(%d, %s) = %d, want %d", tc.year, tc.month, got, tc.want)
		}
	}
}

func TestResolveCap(t *testing.T) {
	tests := []struct {
		name       string
		policy     RolloverPolicy
		allocation money.Money
		want       money.Money
	}{
		{"unset", RolloverPolicy{}, dollars(1000), 0},
		{"absolute", RolloverPolicy{Cap: dollars(250)}, dollars(1000), dollars(250)},
		{"percent 25", RolloverPolicy{CapBasisPoints: 2500}, dollars(1000), dollars(250)},
		{"percent 100", RolloverPolicy{CapBasisPoints: 10000}, dollars(1000), dollars(1000)},
		{"percent 0.01", RolloverPolicy{CapBasisPoints: 1}, dollars(1000), money.Money(100_000)},
		{"percent of zero", RolloverPolicy{CapBasisPoints: 2500}, 0, 0},
		{"absolute wins when bp unset", RolloverPolicy{Cap: dollars(5)}, dollars(1000), dollars(5)},
		// Truncation must round down: a cap that rounds up carries money the
		// policy did not authorize.
		{"truncates down", RolloverPolicy{CapBasisPoints: 3333}, money.Money(3), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.ResolveCap(tc.allocation); got != tc.want {
				t.Errorf("ResolveCap(%s) = %s, want %s", tc.allocation, got, tc.want)
			}
		})
	}
}

func TestResolveCapDoesNotOverflow(t *testing.T) {
	p := RolloverPolicy{CapBasisPoints: 10_000_000} // 1000x
	if got := p.ResolveCap(money.Max); got != money.Max {
		t.Errorf("ResolveCap(Max) = %s, want saturation at Max", got)
	}
}

// TestCapFormsAreEquivalent is a stated acceptance criterion: an absolute cap and
// a percentage cap configured to the same effective value must behave identically.
func TestCapFormsAreEquivalent(t *testing.T) {
	allocations := []money.Money{dollars(1000), dollars(4000), dollars(1), money.Money(999_999)}
	fractions := []int64{0, 1, 250, 2500, 5000, 10000}
	balances := []money.Money{
		0, money.Money(1), dollars(1), dollars(100), dollars(250), dollars(999), dollars(4000),
		-dollars(1), -dollars(500),
	}
	modes := []RolloverMode{RolloverNone, RolloverCredit, RolloverBalance}

	for _, alloc := range allocations {
		for _, bp := range fractions {
			// The absolute equivalent of this percentage for this allocation.
			absolute := RolloverPolicy{CapBasisPoints: bp}.ResolveCap(alloc)
			for _, mode := range modes {
				pct := RolloverPolicy{Mode: mode, CapBasisPoints: bp}
				abs := RolloverPolicy{Mode: mode, Cap: absolute}
				for _, bal := range balances {
					gotPct := pct.CarryInto(bal, alloc)
					gotAbs := abs.CarryInto(bal, alloc)
					if gotPct != gotAbs {
						t.Errorf("mode=%s alloc=%s bp=%d balance=%s: percentage carry %s != absolute carry %s",
							mode, alloc, bp, bal, gotPct, gotAbs)
					}
				}
			}
		}
	}
}

// TestPercentageCapDoesNotCompound is why a percentage resolves against
// allocation rather than total: resolving against total would let each period's
// cap grow from the previous period's carry.
func TestPercentageCapDoesNotCompound(t *testing.T) {
	def := monthlyDef(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	def.Rollover = RolloverPolicy{Mode: RolloverCredit, CapBasisPoints: 2500}

	carry := money.Money(0)
	for seq := 0; seq < 12; seq++ {
		env, err := def.Envelope(seq, carry)
		if err != nil {
			t.Fatal(err)
		}
		// Spend nothing at all, the maximally favourable case for carry growth.
		carry = env.Rollover.CarryInto(env.Close(0), env.Allocation)
		if want := dollars(1000); carry != want {
			t.Fatalf("period %d carry = %s, want a constant %s (cap must not compound)", seq, carry, want)
		}
	}
}

func TestProvisionalCloseIsConservative(t *testing.T) {
	env := Envelope{
		ID: "b", Allocation: dollars(1000),
		Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}

	// With $200 spent and $300 still held, the provisional balance assumes the
	// holds all spend: $1000 - $500.
	if got, want := env.ProvisionalClose(dollars(200), dollars(300)), dollars(500); got != want {
		t.Errorf("ProvisionalClose = %s, want %s", got, want)
	}
	// It can never exceed the final balance, so a successor never overspends.
	if prov, final := env.ProvisionalClose(dollars(200), dollars(300)), env.Close(dollars(200)); prov > final {
		t.Errorf("provisional %s exceeds final %s", prov, final)
	}
	// With nothing outstanding the two agree exactly, which is the common case
	// at a boundary and the reason draining is usually instantaneous.
	if prov, final := env.ProvisionalClose(dollars(200), 0), env.Close(dollars(200)); prov != final {
		t.Errorf("provisional %s != final %s with no holds", prov, final)
	}
}
