package dashboard

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/report"
)

// Every figure on a page is rendered by a function in this file. Templates cannot
// format money themselves -- they have no arithmetic -- and the JavaScript is handed
// finished strings for the same reason: a second implementation of "how do we write an
// unknown cost" is how an unknown cost becomes $0.00 in one of the two places.

// usd renders an amount to the cent, grouped, e.g. "$1,234.56".
//
// Cents are right for a budget figure. A per-request cost is often a fraction of a
// cent, which is what usdPrecise is for: rounding a $0.0004 request to "$0.00" would
// make a real charge look free.
func usd(m money.Money) string { return formatUSD(m, 2) }

// usdPrecise renders an amount to four decimal places, for per-request costs.
func usdPrecise(m money.Money) string { return formatUSD(m, 4) }

// signedUSD renders an amount with an explicit sign, because the sign is the message.
//
// A pace balance of $40 and one of -$40 are opposite facts, and a display that dropped
// the minus -- or that showed a magnitude beside a colour -- would turn "borrowed $40"
// into "banked $40" for any reader who is not colour-sensitive.
func signedUSD(m money.Money) string {
	switch {
	case m > 0:
		return "+" + formatUSD(m, 2)
	case m < 0:
		// A true minus sign rather than a hyphen: at small sizes a hyphen in front of a
		// dollar figure is easy to lose.
		return "−" + formatUSD(-m, 2)
	default:
		return formatUSD(0, 2)
	}
}

// formatUSD renders microdollars at a given number of decimal places, rounding half
// away from zero, with thousands separators.
//
// Integer arithmetic throughout. Rounding happens once, here, at the point of display,
// which is the same rule the pricing package follows.
func formatUSD(m money.Money, decimals int) string {
	neg := m < 0
	mag := magnitude(m)

	// One microdollar is 1e-6, so scale is how many microdollars one displayed unit is.
	scale := uint64(1)
	for i := decimals; i < 6; i++ {
		scale *= 10
	}
	units := (mag + scale/2) / scale

	pow := uint64(1)
	for i := 0; i < decimals; i++ {
		pow *= 10
	}
	whole, frac := units/pow, units%pow

	var b strings.Builder
	if neg && units != 0 {
		// A rounded-to-zero negative amount is written as zero rather than as "-$0.00",
		// which reads as a defect.
		b.WriteString("-")
	}
	b.WriteString("$")
	b.WriteString(group(whole))
	if decimals > 0 {
		b.WriteString(".")
		b.WriteString(fmt.Sprintf("%0*d", decimals, frac))
	}
	return b.String()
}

// magnitude is the absolute value of an amount as a uint64, which is correct even for
// money.Min, whose negation does not fit in an int64.
func magnitude(m money.Money) uint64 {
	if m < 0 {
		return uint64(-(m + 1)) + 1
	}
	return uint64(m)
}

// group inserts thousands separators.
func group(n uint64) string {
	s := strconv.FormatUint(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteString(",")
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// amount renders a report.Amount to the cent, keeping its completeness semantics.
func amount(a report.Amount) string { return a.Text(usd) }

// amountPrecise renders a report.Amount to four decimal places.
//
// The "+" on a floor and the word "unknown" come from report.Amount itself: this
// function supplies digits and nothing else, so there is no way for a display to
// accidentally render an unknown cost as a number.
func amountPrecise(a report.Amount) string { return a.Text(usdPrecise) }

// duration renders a span in the largest two units that are useful, e.g. "12d 6h".
func duration(d time.Duration) string {
	if d <= 0 {
		return "none"
	}
	days := int64(d / (24 * time.Hour))
	hours := int64(d/time.Hour) % 24
	mins := int64(d/time.Minute) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm", mins)
	default:
		return "under a minute"
	}
}

// latency renders a request duration at the resolution a reader can use.
func latency(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}

// unitName is the display name of a rate's time unit.
func unitName(d time.Duration) string {
	switch d {
	case time.Hour:
		return "hour"
	case 24 * time.Hour:
		return "day"
	case 7 * 24 * time.Hour:
		return "week"
	default:
		return duration(d)
	}
}

// rate renders a rate in its suggested unit.
//
// An unknown rate is "—", never "$0.00/day". No elapsed time is not a rate of zero and
// no remaining time is not a sustainable rate of zero; both would read as facts about
// spending rather than about the clock.
func rate(r report.Rate) string {
	if !r.Known {
		return "—"
	}
	return usd(r.Suggested()) + "/" + unitName(r.Per)
}

// rateNote explains an absent or unreliable rate in words.
func rateNote(r report.Rate) string {
	if !r.Known {
		return "no rate: there is no elapsed time to measure over"
	}
	if r.Confidence == report.ConfidenceLow {
		return "very little of the period has elapsed, so this average is not yet meaningful"
	}
	return ""
}

// elapsedPct is how far through the period the clock is, as a CSS width.
//
// The ratio is taken in seconds rather than nanoseconds. Multiplying a nanosecond count
// by 10,000 overflows int64 at about ten and a half days, and a month-long budget is the
// ordinary case, not the edge one -- so the naive expression would silently produce a
// negative width for almost every real period.
func elapsedPct(elapsed, total time.Duration) string {
	if total <= 0 {
		// A zero-length period has no meaningful position within it. Reporting 0% is a
		// deliberate choice over 100%: nothing has passed, and dividing would panic.
		return "0.00"
	}
	if elapsed <= 0 {
		return "0.00"
	}
	if elapsed >= total {
		return "100.00"
	}
	// Seconds keep the product inside int64 for any period shorter than about 29,000
	// years; sub-second precision is not visible in an 8-pixel bar.
	bp := int64(elapsed.Round(time.Second)/time.Second) * 10_000 /
		max64(int64(total.Round(time.Second)/time.Second), 1)
	if bp < 0 {
		bp = 0
	}
	if bp > 10_000 {
		bp = 10_000
	}
	return fmt.Sprintf("%d.%02d", bp/100, bp%100)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// percent renders basis points as a percentage, exactly as far as basis points go.
func percent(bp int64) string {
	neg := bp < 0
	if neg {
		bp = -bp
	}
	s := fmt.Sprintf("%d.%02d%%", bp/100, bp%100)
	if neg {
		return "-" + s
	}
	return s
}

// periodStateText is the period's lifecycle position in words.
//
// A prospective envelope has no row and therefore no state, and printing an empty
// string would read as a rendering fault. It says what is true instead: the definition
// describes this envelope and the ledger has not written it yet, which happens for
// every budget until something first spends against it.
func periodStateText(pos report.Position) string {
	if pos.Prospective {
		if pos.At.Before(pos.PeriodStart) {
			return "not started"
		}
		return "not yet materialized"
	}
	return string(pos.Period.State)
}

// pressureText is the gauge's reading in words, which is the only form four of its
// five states have.
func pressureText(p report.Pressure) string {
	switch p.State {
	case report.PressureMeasured:
		return percent(p.BasisPoints)
	case report.PressureNotStarted:
		return "no reading"
	case report.PressureEnded:
		return "period over"
	case report.PressureNoHeadroom:
		return "pegged"
	default:
		return "no reading"
	}
}

// pressureWhy explains the reading, and is displayed permanently rather than on hover.
func pressureWhy(p report.Pressure) string {
	switch p.State {
	case report.PressureMeasured:
		switch {
		case p.BasisPoints > 10_000:
			return "burning faster than the remaining allocation sustains"
		case p.BasisPoints == 10_000:
			return "exactly on track to finish at the allocation"
		case p.BasisPoints == 0:
			return "nothing has been spent over real elapsed time"
		default:
			return "on track to finish under the allocation"
		}
	case report.PressureNotStarted:
		return "no time has elapsed yet, so there is no rate to measure"
	case report.PressureEnded:
		return "the period is over: there is no remaining time to sustain a rate across"
	case report.PressureNoHeadroom:
		return "no remaining allocation, so no sustainable rate exists to compare against"
	default:
		return ""
	}
}

// confidenceNote qualifies a figure derived from very little elapsed time.
func confidenceNote(c report.Confidence) string {
	if c == report.ConfidenceLow {
		return "low confidence: very little of the period has elapsed"
	}
	return ""
}

// projectionText renders the straight-line projection.
func projectionText(p report.Projection) string {
	if !p.Known {
		return "—"
	}
	return usd(p.Amount)
}

// projectionNote states the method every time the figure appears.
//
// The label is not decoration. A projection shown without its method invites a reader
// to assume a forecast, and this one is spent x duration / elapsed and nothing else.
func projectionNote(p report.Projection) string {
	if !p.Known {
		return "no projection: no time has elapsed to extrapolate from"
	}
	base := "assumes the average rate to date continues; not a forecast"
	if p.Confidence == report.ConfidenceLow {
		return base + ", and very little of the period has elapsed"
	}
	return base
}

// timestamp renders an instant for a table cell, in UTC because that is what is
// stored.
func timestamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04:05Z")
}

// clockTime is the short form for a chart axis.
func clockTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("Jan 2")
}

// dayStamp is a date without a time, for period bounds.
func dayStamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format("2006-01-02 15:04Z")
}

// count renders an integer with thousands separators.
func count(n int) string { return group(uint64(abs64(int64(n)))) }

// abs is the magnitude of an amount, for rendering a difference whose sign is already
// stated in words.
func abs(m money.Money) money.Money {
	if m < 0 {
		return -m
	}
	return m
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// present renders a field the provider did not report as an explicit absence rather
// than as an empty cell, which is indistinguishable from a rendering bug.
func present(s string) string {
	if strings.TrimSpace(s) == "" {
		return "not reported"
	}
	return s
}

// costStateLabel names a cost state for a legend or a title attribute.
func costStateLabel(s report.CostState) string {
	switch s {
	case report.CostKnown:
		return "complete"
	case report.CostPartial:
		return "a floor: some usage could not be priced"
	case report.CostUnknown:
		return "unknown: nothing could be priced"
	case report.CostUnresolved:
		return "unresolved: still awaiting the information that would price it"
	case report.CostNone:
		return "no cost applies"
	default:
		return "unknown"
	}
}
