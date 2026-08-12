package config

import (
	"time"

	"throttle/budget"
)

// Overriding one budget's fields from the command line.
//
// This exists so that "throttle define -budget '$4,000'" and an amount written in a config
// file reach budget.Definition through the same parsers. The flags used to have their own:
// their own money parsing, their own timezone lookup, their own percentage-to-basis-points
// conversion through a float64. Two implementations of "what is $4,000" is one more than the
// microdollar rule allows.

// DefinitionOverrides are budget fields a command line supplied, nil for the rest.
//
// Strings rather than typed values, because parsing is the part being shared: a
// time.Duration flag would have accepted "720h" for a month, which parseDuration exists to
// refuse.
type DefinitionOverrides struct {
	Parent *string
	Name   *string
	Amount *string
	Borrow *string

	Recur    *string
	Every    *string
	Timezone *string
	Anchor   *string
	End      *string

	Rollover   *string
	CapAmount  *string
	CapPercent *string
}

// ApplyDefinitionOverrides returns def with the supplied fields replaced.
//
// Field paths in the errors are flag names, because that is what the reader typed. Every
// problem found is reported together, for the same reason the file loader aggregates: a
// command line with two bad flags should not need two runs to discover that.
func ApplyDefinitionOverrides(def budget.Definition, over DefinitionOverrides) (budget.Definition, error) {
	problems := &Errors{Source: "flags"}

	if over.Parent != nil {
		def.ParentID = *over.Parent
	}
	if over.Name != nil {
		def.Name = *over.Name
	}
	if over.Amount != nil {
		m, err := parseMoney("-budget", *over.Amount)
		if err != nil {
			problems.add(err)
		} else {
			def.Allocation = m
		}
	}
	if over.Borrow != nil {
		d, err := parseDuration("-borrow", *over.Borrow)
		if err != nil {
			problems.add(err)
		} else {
			def.Borrow = d
		}
	}

	// The timezone is applied before the dates, because a bare date is read as midnight in
	// the budget's own zone: parsing the anchor first would place it in the old zone and
	// shift every period boundary by the difference.
	if over.Timezone != nil {
		loc, err := parseLocation("-tz", *over.Timezone)
		if err != nil {
			problems.add(err)
		} else {
			def.Location = loc
		}
	}
	if def.Location == nil {
		def.Location = time.UTC
	}

	if over.Recur != nil {
		recur := budget.Recurrence(*over.Recur)
		switch recur {
		case budget.RecurNone, budget.RecurDaily, budget.RecurWeekly, budget.RecurMonthly, budget.RecurDuration:
			def.Recurrence = recur
		default:
			problems.add(fieldErr("-recur",
				"unknown period "+*over.Recur+": use monthly, weekly, daily, duration, or none"))
		}
	}
	if def.Recurrence == "" {
		def.Recurrence = budget.RecurMonthly
	}

	if over.Every != nil {
		d, err := parseDuration("-every", *over.Every)
		if err != nil {
			problems.add(err)
		} else {
			def.Every = d
		}
	}
	if over.Anchor != nil {
		t, err := parseDate("-anchor", *over.Anchor, def.Location, false)
		if err != nil {
			problems.add(err)
		} else {
			def.AnchorAt = t
		}
	}
	if over.End != nil {
		t, err := parseDate("-end", *over.End, def.Location, true)
		if err != nil {
			problems.add(err)
		} else {
			def.EndAt = t
		}
	}

	if over.Rollover != nil {
		mode, err := parseRolloverMode("-rollover", *over.Rollover)
		if err != nil {
			problems.add(err)
		} else {
			def.Rollover.Mode = mode
		}
	}
	if over.CapAmount != nil && over.CapPercent != nil {
		problems.add(fieldErr("-rollover-cap",
			"and -rollover-cap-pct are mutually exclusive: a cap is either a fixed sum or a "+
				"proportion of the allocation, not both"))
	} else {
		if over.CapAmount != nil {
			m, err := parseMoney("-rollover-cap", *over.CapAmount)
			if err != nil {
				problems.add(err)
			} else {
				def.Rollover.Cap = m
				// The other form is cleared rather than left standing. A definition
				// carrying both is rejected by Validate, and a flag that silently
				// failed to take effect is worse than one that says why.
				def.Rollover.CapBasisPoints = 0
			}
		}
		if over.CapPercent != nil {
			bp, err := parsePercent("-rollover-cap-pct", *over.CapPercent)
			if err != nil {
				problems.add(err)
			} else {
				def.Rollover.CapBasisPoints = bp
				def.Rollover.Cap = 0
			}
		}
	}

	if def.AnchorAt.IsZero() {
		// Defaulted here, unlike in a config file, where an anchor is required.
		//
		// The difference is deliberate. A file is read repeatedly, so an anchor from the
		// clock would make the same file mean something different each month; a command
		// line is typed once, and refusing to run without -anchor would be pedantry.
		// The first of the current month, in the budget's own zone, is what "monthly"
		// means to the person typing it.
		def.AnchorAt = monthStart(time.Now(), def.Location)
	}

	if err := problems.err(); err != nil {
		return budget.Definition{}, err
	}
	if err := def.Validate(); err != nil {
		return budget.Definition{}, err
	}
	return def, nil
}

// monthStart is the first instant of at's month, in loc.
func monthStart(at time.Time, loc *time.Location) time.Time {
	local := at.In(loc)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
}
