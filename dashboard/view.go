package dashboard

import (
	"fmt"
	"math"
	"strings"
	"time"

	"throttle/money"
	"throttle/report"
)

// The view model is where geometry happens: pixels, arcs, and path strings.
//
// Money arrives here already decided and already formatted. Nothing in this file adds,
// subtracts, or compares two amounts to produce a third figure for display -- the one
// exception is scaling an amount into a pixel coordinate, which is presentation
// arithmetic and is deliberately done in float64 because a y coordinate is not
// accounting. No float64 value in this file is ever rendered as a currency figure.

// Page is everything one dashboard render needs.
type Page struct {
	// Title is the browser title.
	Title string

	// Budget is the selected budget, empty on the first-run page.
	Budget string

	// At is the instant every figure was computed for.
	At        time.Time
	AtDisplay string

	// Budgets is the hierarchy selector, and Periods the period selector.
	Budgets []BudgetOption
	Periods []PeriodOption

	// PeriodID is the period the figures describe, always populated once a budget is
	// selected.
	PeriodID string

	// Historic reports that a period other than the current one is on screen, so the
	// page can stop implying that its rates and gauge describe a live workload.
	Historic bool

	// Empty reports the no-budgets-defined state, which is a legitimate first-run
	// page rather than an error.
	Empty bool

	// HasActivity reports whether request history is configured at all. False is a
	// different fact from an empty table and is said differently.
	HasActivity bool

	// Version is the build, shown in the footer so a screenshot identifies itself.
	Version string

	// Notices are conditions the reader must see: a degraded activity store, an
	// unstarted period, a disagreement between the stores.
	Notices []Notice

	Position PositionView
	Pressure GaugeView
	Bank     BankView
	Rates    RatesView
	EndState EndStateView
	Chart    ChartView

	// Hierarchy is the budget tree with each node's own position.
	Hierarchy []NodeView

	// Holds are the live and expired reservations behind the Reserved figure.
	Holds []HoldView

	// Activity is the recent request table.
	Activity ActivityView

	// Breakdowns are spend grouped by each identity and attribution facet.
	Breakdowns []BreakdownView

	// Recon is the reconciliation panel.
	Recon ReconView

	// Rollover is shown only when rollover is configured, so the ordinary case stays
	// uncluttered.
	Rollover *RolloverView
}

// Notice is a message the page displays inline.
type Notice struct {
	// Level is "warn" for something an operator should act on, "info" for something
	// they should merely know.
	Level string
	Text  string
}

// BudgetOption is one entry in the budget selector, indented by its depth.
type BudgetOption struct {
	ID       string
	Name     string
	Depth    int
	Indent   string
	Selected bool
}

// PeriodOption is one entry in the period selector.
type PeriodOption struct {
	PeriodID string
	Label    string
	Current  bool
	Selected bool
	State    string
}

// PositionView is the budget summary panel: the full accounting vocabulary, with no
// term standing in for another.
type PositionView struct {
	BudgetID string
	Name     string

	Allocation string
	CarryIn    string
	HasCarry   bool
	Total      string

	Spent    string
	Reserved string

	// Remaining is Total-Spent-Reserved, signed, and Overspent says the sign matters.
	Remaining string
	Overspent bool

	TargetByNow  string
	AllowedByNow string
	HasBorrow    bool
	BorrowWindow string

	// PaceBalance is TargetByNow-Spent. Signed, and never folded together with
	// Remaining: they answer different questions.
	PaceBalance string

	SpendableNow string

	PeriodStart string
	PeriodEnd   string
	PeriodState string
	Provisional bool

	Elapsed       string
	TimeRemaining string
	ElapsedPct    string

	LiveHolds int

	// Binding names the ancestor that would refuse the next request, when it is not
	// this budget itself.
	Binding string
}

// GaugeView is the BURN PRESSURE dial.
//
// The subtitle is a permanent field rather than a tooltip because the confusion it
// prevents -- reading this as percent of budget spent -- is the whole reason the label
// is not "THROTTLE %".
type GaugeView struct {
	Reading  string
	Why      string
	Subtitle string

	// Width, Height, and Face are the dial's own geometry, computed from the same
	// constants as the needle rather than written into the template, so the face and
	// the graduations cannot drift apart.
	Width, Height float64
	Face          string

	// Measured reports whether there is a numeric reading at all. When false the dial
	// shows no needle and the reading is words.
	Measured bool

	// Pegged reports that the ratio is off the top of the dial. The needle stops; the
	// printed percentage does not.
	Pegged bool

	// State is the machine-readable state, for a CSS hook.
	State string

	// Over reports a reading past the redline.
	Over bool

	LowConfidence bool
	Confidence    string

	// NeedleX and NeedleY are the tip of the needle on the dial, and Arc is the swept
	// path from zero to the reading.
	NeedleX, NeedleY float64
	Arc              string

	// RedlineX1, RedlineY1, RedlineX2, RedlineY2 mark the 100% tick, which is the
	// condition the gauge exists to show and so is drawn as a redline rather than as
	// the end of the scale.
	RedlineX1, RedlineY1, RedlineX2, RedlineY2 float64

	// Ticks are the labelled dial graduations.
	Ticks []GaugeTick
}

// GaugeTick is one labelled graduation.
type GaugeTick struct {
	Label                  string
	X1, Y1, X2, Y2, TX, TY float64
	Redline                bool
}

// BankView is the bipolar bank/borrow display: BORROWED <-- 0 --> BANKED.
type BankView struct {
	Amount string

	// Banked and Borrowed are mutually exclusive, and Zero is its own state: exactly
	// on pace is a real answer, not a missing one.
	Banked   bool
	Borrowed bool
	Zero     bool

	// Label is BANKED, BORROWED, or ON PACE.
	Label string

	// FillPct is the bar's extent from the centre, as a percentage of the half-width.
	FillPct float64

	// Explain states what the figure is not, because it is easy to mistake for
	// remaining allocation.
	Explain string
}

// RatesView is the two explicit burn rates.
type RatesView struct {
	AverageToDate string
	AverageNote   string

	Sustainable     string
	SustainableNote string

	// Unit names the display unit both rates are shown in.
	Unit string
}

// EndStateView is the range panel: what is left, and where it lands.
type EndStateView struct {
	RemainingAllocation string
	Overspent           bool
	TimeRemaining       string
	SustainableBurn     string

	Projection     string
	ProjectionNote string

	// OverBy and UnderBy are shown only when the projection is known, and only the
	// one that applies.
	OverBy  string
	UnderBy string
}

// ChartView is the budget-over-time SVG.
type ChartView struct {
	// Width and Height are the viewBox dimensions the paths were computed in.
	Width, Height float64

	// Target, Actual, and Allowed are SVG path strings. Actual is a step function
	// built from persisted charge timestamps -- no interpolation between charges, and
	// no synthesized request.
	Target  string
	Actual  string
	Allowed string

	HasBorrow bool

	// PlotLeft, PlotRight, and PlotBottom bound the drawing area inside the axis
	// labels, so a grid line does not run out from under the y-axis figures and the
	// now marker does not strike through the dates.
	PlotLeft, PlotRight, PlotBottom float64

	// NowX is the current-time marker.
	NowX float64

	// ReservedTop, ReservedHeight, and ReservedWidth bound the reservation band, which
	// is drawn from the now marker rightward rather than stacked over history: a hold
	// is not a charge that happened at a past instant.
	ReservedTop, ReservedHeight, ReservedWidth float64
	HasReserved                                bool
	ReservedLabel                              string

	// GapY1 and GapY2 bracket the vertical distance between the two lines at the now
	// marker, which IS the pace balance.
	GapY1, GapY2 float64

	// GapLabelX and GapLabelY place the label beside the bracket.
	GapLabelX, GapLabelY float64
	GapLabel             string
	GapBanked            bool
	ShowGap              bool

	// YTicks and XTicks are the axes.
	YTicks []ChartTick
	XTicks []ChartTick

	Charges   int
	Truncated bool

	// Note explains a chart that cannot be drawn.
	Note string
}

// ChartTick is one axis graduation.
type ChartTick struct {
	Label string
	X, Y  float64
}

// NodeView is one budget in the hierarchy table.
type NodeView struct {
	BudgetID string
	Name     string
	Depth    int
	Indent   string
	Selected bool

	Allocation string
	Spent      string
	Reserved   string
	Remaining  string
	Overspent  bool

	PaceBalance string
	Banked      bool
	Borrowed    bool

	Pressure string
	State    string

	// Error explains a node whose position could not be read, which is normal on a
	// budget with no materialized period yet.
	Error string
}

// HoldView is one live or expired reservation.
type HoldView struct {
	ReservationID string
	RequestID     string
	BudgetID      string
	Amount        string
	Estimated     string
	Age           string
	Expires       string
	Expired       bool
	Model         string
	ModelKnown    bool
	Operation     string
	Renewals      int
}

// ActivityView is the recent-requests table.
type ActivityView struct {
	// Available reports whether the activity store could be read. False with an empty
	// table means throttle is not recording history, which is a different fact from
	// "no requests happened".
	Available bool
	Error     string

	Events    []EventView
	Truncated bool
	Limit     int

	// PageSpend is the sum over the rows shown, labelled as such so it is never read
	// as a period total.
	PageSpend    string
	PageComplete bool
	Requests     int
}

// EventView is one row of the activity table.
type EventView struct {
	RequestID string

	Time     string
	TimeFull string

	BudgetID string

	// Scopes lists every budget the hold consumed, so a request against a child is
	// explicable from its parent.
	Scopes string

	Operation string

	// AccessProvider, Publisher, and Model are three columns, never one. AWS Bedrock
	// is the access path, Anthropic the publisher, and the model a third thing again.
	AccessProvider  string
	Publisher       string
	Model           string
	ModelKnown      bool
	ProviderModelID string

	Usage      string
	UsageTitle string

	Estimated string
	Reserved  string

	Actual      string
	ActualState string
	ActualTitle string

	Overrun    string
	HasOverrun bool

	Mode    string
	Latency string

	Status  string
	Outcome string

	// Flags are the badges: compound, hosted runtime, awaiting external data,
	// repaired.
	Flags []string

	Error string
}

// BreakdownView is spend grouped by one facet.
type BreakdownView struct {
	Facet string
	Label string

	Total    string
	Complete bool
	Requests int

	Rows []BreakdownRowView
}

// BreakdownRowView is one group.
type BreakdownRowView struct {
	Key      string
	Reported bool

	Spend    string
	Complete bool

	Requests   int
	Unresolved int
	Reserved   string
	HasHold    bool

	// SharePct is the bar width, from integer basis points.
	SharePct  float64
	ShareText string
}

// ReconView is the reconciliation panel: what is outstanding, and why.
type ReconView struct {
	// Clean reports that nothing is outstanding at all.
	Clean bool

	// Needs reports that something wants an operator's attention. An awaiting record
	// does not: it is waiting for the provider, not for a human.
	Needs bool

	Unresolved       int
	OutcomeUnknown   int
	AwaitingExternal int
	ExpiredHolds     int

	Encumbered    string
	ExpiredAmount string

	Repaired int

	UnpricedDimensions []string

	// Rows are the outstanding requests themselves, so the panel links to evidence
	// rather than to a count.
	Rows []EventView

	// Available reports whether the activity store could be read.
	Available bool
	Error     string

	// Explain states that nothing here has been repaired by opening the page.
	Explain string
}

// RolloverView is the carry configuration, displayed only when rollover is on.
type RolloverView struct {
	Mode    string
	Cap     string
	HasCap  bool
	CapPct  string
	CarryIn string
	Explain string
}

// DetailPage is one request examined closely.
type DetailPage struct {
	Title     string
	At        time.Time
	AtDisplay string
	Version   string

	Event EventView

	// Estimate and cost provenance.
	QuoteSource     string
	QuoteVersion    string
	QuoteCapturedAt string
	EstimateQuality string

	Usage []UsageRow

	// Agent is the compound-transaction view: one governed request, several observed
	// internal model calls beneath it.
	Agent *AgentView

	// Runtime is the hosted-runtime view, whose defining property is that the cost of
	// the compute is not knowable at the time of the call.
	Runtime *RuntimeView

	Repairs []RepairView
}

// UsageRow is one dimension of usage.
type UsageRow struct {
	Dimension string
	Count     string
	Token     bool
	Unpriced  bool
}

// AgentView is a managed agent turn, decomposed.
type AgentView struct {
	AgentID   string
	AliasID   string
	Version   string
	SessionID string

	// Total is the request-level charge: the authoritative figure. The steps beneath
	// are accounting detail, not transactions of their own.
	Total string

	Steps []AgentStepView

	// Events are non-model activities counted by kind. They are counts because the
	// provider reports no billable quantity for them.
	Events []AgentEventView

	StepSum string

	// GapNote explains displayed steps that do not sum to the displayed total, which
	// happens because the turn is rounded once. Adjusting a step to make the column
	// add up would mean printing a number that is not the step's cost.
	GapNote string

	// GapMaterial marks a gap too large for display rounding to account for. Then the
	// provider's per-step figures genuinely do not add up to what it charged, which is a
	// different statement from a rounding artefact and is rendered as a warning rather
	// than as a footnote.
	GapMaterial bool

	Note string
}

// AgentEventView is one kind of non-model activity and how often it occurred.
type AgentEventView struct {
	Kind  string
	Count int
}

// AgentStepView is one observed model invocation inside a turn.
type AgentStepView struct {
	Seq          int
	Kind         string
	Collaborator string

	Model      string
	ModelKnown bool
	Publisher  string

	Usage string
	Cost  string

	Latency string
	At      string
}

// RuntimeView is an invocation of an agent on a managed runtime.
type RuntimeView struct {
	RuntimeID string
	Qualifier string
	SessionID string

	ProviderRequestID string
	TraceID           string

	StatusCode int
	Sizes      string

	MaxExposure string

	Reconciled     bool
	ReconciledCost string
	ReconciledAt   string
	ReconciledFrom string
	ReconciledUse  []UsageRow

	// Explain states the accounting position: a session-level runtime charge is never
	// divided across the invocations that shared the session, so this invocation's own
	// runtime cost is unknown rather than estimated.
	Explain string

	Note string
}

// RepairView is one entry of the reconciliation trail.
type RepairView struct {
	At     string
	Class  string
	Reason string

	Observed string
	Produced string

	Money  string
	Amount string

	Quote  string
	Detail string
}

// gauge geometry. The dial is a 240-degree sweep, which leaves the bottom open for the
// reading, and spans 0% to 200% with the 100% redline in the middle.
//
// The graduation labels sit outside the arc rather than inside it. Inside, they share
// the middle of the dial with the reading, and a "50%" printed against the digits of a
// "22.03%" is worse than no scale at all.
const (
	gaugeW, gaugeH   = 260.0, 196.0
	gaugeCX, gaugeCY = 130.0, 128.0
	gaugeR           = 86.0
	gaugeLabelR      = gaugeR + 16
	gaugeStart       = 150.0 // degrees, measured clockwise from 3 o'clock
	gaugeSweep       = 240.0
	gaugeFullScale   = 20_000 // basis points at the end of the dial
)

// gaugeView builds the dial.
func gaugeView(p report.Pressure) GaugeView {
	g := GaugeView{
		Reading:  pressureText(p),
		Why:      pressureWhy(p),
		State:    string(p.State),
		Measured: p.Measured(),
		Over:     p.OverRedline(),
		Subtitle: "average burn to date ÷ sustainable burn for the time left. " +
			"100% = exactly on track to finish at the allocation. " +
			"This is not percent of budget spent.",
		LowConfidence: p.Confidence == report.ConfidenceLow,
		Confidence:    confidenceNote(p.Confidence),
		Pegged:        p.State == report.PressureNoHeadroom,
	}

	// The face is drawn from the same constants as everything else on the dial, so the
	// arc, the graduations, the redline, and the needle cannot drift apart.
	g.Width, g.Height = gaugeW, gaugeH
	g.Face = arcPath(gaugeCX, gaugeCY, gaugeR, gaugeAngle(0), gaugeAngle(gaugeFullScale))

	for _, bp := range []int64{0, 5_000, 10_000, 15_000, 20_000} {
		a := gaugeAngle(bp)
		x1, y1 := polar(gaugeCX, gaugeCY, gaugeR-7, a)
		x2, y2 := polar(gaugeCX, gaugeCY, gaugeR+7, a)
		tx, ty := polar(gaugeCX, gaugeCY, gaugeLabelR, a)
		label := fmt.Sprintf("%d%%", bp/100)
		if bp == gaugeFullScale {
			label += "+"
		}
		g.Ticks = append(g.Ticks, GaugeTick{
			Label: label, X1: x1, Y1: y1, X2: x2, Y2: y2, TX: tx, TY: ty,
			Redline: bp == 10_000,
		})
	}

	red := gaugeAngle(10_000)
	g.RedlineX1, g.RedlineY1 = polar(gaugeCX, gaugeCY, gaugeR-9, red)
	g.RedlineX2, g.RedlineY2 = polar(gaugeCX, gaugeCY, gaugeR+9, red)

	if !p.Measured() {
		// No needle at all. A needle resting at zero would claim an idle workload,
		// which is exactly what "no reading" does not mean.
		return g
	}

	bp := p.BasisPoints
	if bp > gaugeFullScale {
		// The dial ends; the number does not. The needle stops at full scale and the
		// exact percentage is still printed in full.
		g.Pegged = true
		bp = gaugeFullScale
	}
	if bp < 0 {
		bp = 0
	}
	a := gaugeAngle(bp)
	g.NeedleX, g.NeedleY = polar(gaugeCX, gaugeCY, gaugeR-14, a)
	g.Arc = arcPath(gaugeCX, gaugeCY, gaugeR, gaugeAngle(0), a)
	return g
}

// gaugeAngle maps basis points onto the dial.
func gaugeAngle(bp int64) float64 {
	frac := float64(bp) / float64(gaugeFullScale)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	return gaugeStart + gaugeSweep*frac
}

// polar converts a centre, radius, and angle in degrees to a point.
func polar(cx, cy, r, deg float64) (float64, float64) {
	rad := deg * math.Pi / 180
	return round2(cx + r*math.Cos(rad)), round2(cy + r*math.Sin(rad))
}

// arcPath is an SVG arc between two angles on a circle.
func arcPath(cx, cy, r, from, to float64) string {
	if to <= from {
		return ""
	}
	x1, y1 := polar(cx, cy, r, from)
	x2, y2 := polar(cx, cy, r, to)
	large := 0
	if to-from > 180 {
		large = 1
	}
	return fmt.Sprintf("M %g %g A %g %g 0 %d 1 %g %g", x1, y1, r, r, large, x2, y2)
}

// round2 keeps path strings short without moving anything a reader could see.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// bankView builds the bipolar bank/borrow display.
//
// The bar is scaled against the pacing target, which is the quantity the balance is a
// deviation from. Scaling it against the allocation instead would make a $40 overrun on
// a $10,000 budget invisible, and the sign of this figure is the point of showing it.
func bankView(pos report.Position) BankView {
	b := BankView{
		Amount: signedUSD(pos.PaceBalance),
		Explain: "target by now minus settled spend. Positive is banked, negative is borrowed " +
			"against the rest of the period. This is not the remaining allocation: " +
			"they answer different questions.",
	}
	switch {
	case pos.PaceBalance > 0:
		b.Banked, b.Label = true, "BANKED"
	case pos.PaceBalance < 0:
		b.Borrowed, b.Label = true, "BORROWED"
	default:
		b.Zero, b.Label = true, "ON PACE"
	}

	// The scale reference is the target, falling back to the envelope total for a
	// period that has not started.
	ref := pos.TargetByNow
	if ref <= 0 {
		ref = pos.Total
	}
	if ref > 0 {
		frac := float64(magnitude(pos.PaceBalance)) / float64(ref)
		if frac > 1 {
			frac = 1
		}
		b.FillPct = round2(frac * 100)
	}
	return b
}

// chart geometry.
const (
	chartW, chartH = 900.0, 270.0

	// The left pad clears the y-axis labels and the right pad keeps the final x-axis
	// label inside the viewBox; without them the first and last graduations are clipped
	// at the edges of the drawing, which reads as a rendering fault.
	chartPadL = 62.0
	chartPadR = 26.0
	chartPadT = 14.0
	chartPadB = 26.0
)

// chartView builds the budget-over-time SVG paths.
//
// Time maps to x and money to y. The actual line is emitted as a step function, because
// that is what the persisted charges are: a smooth line through them would draw spend
// at instants when no request was made.
func chartView(tl report.Timeline, pos report.Position) ChartView {
	c := ChartView{
		Width: chartW, Height: chartH,
		PlotLeft: chartPadL, PlotRight: round2(chartW - chartPadR),
		PlotBottom: round2(chartH - chartPadB),
		HasBorrow:  tl.HasBorrow,
		Charges:    tl.Charges,
		Truncated:  tl.Truncated,
	}

	span := tl.End.Sub(tl.Start)
	if span <= 0 {
		c.Note = "this period has no duration, so there is nothing to plot"
		return c
	}

	// The vertical scale is the envelope total, or the highest line if spend has gone
	// past it: an overspend must be visible rather than clipped at the top.
	top := tl.Total
	for _, pt := range tl.Actual {
		if pt.Amount > top {
			top = pt.Amount
		}
	}
	if tl.Committed > top {
		top = tl.Committed
	}
	if top <= 0 {
		top = money.PerDollar
	}

	x := func(at time.Time) float64 {
		frac := float64(at.Sub(tl.Start)) / float64(span)
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		return round2(chartPadL + frac*(chartW-chartPadL-chartPadR))
	}
	y := func(m money.Money) float64 {
		frac := float64(m) / float64(top)
		if frac < 0 {
			frac = 0
		}
		if frac > 1 {
			frac = 1
		}
		return round2(chartH - chartPadB - frac*(chartH-chartPadT-chartPadB))
	}

	c.Target = linePath(tl.Target, x, y)
	c.Actual = stepPath(tl.Actual, x, y)
	if tl.HasBorrow {
		c.Allowed = linePath(tl.Allowed, x, y)
	}
	c.NowX = x(tl.Now)

	// The reservation band: from the now marker rightward, above the actual line.
	if tl.Reserved > 0 {
		bottom := y(lastAmount(tl.Actual))
		top := y(tl.Committed)
		c.HasReserved = true
		c.ReservedTop = top
		c.ReservedHeight = round2(bottom - top)
		c.ReservedWidth = round2(chartW - chartPadR - c.NowX)
		c.ReservedLabel = usd(tl.Reserved) + " reserved, not spent"
	}

	// The gap bracket at the now marker. Its length is the pace balance, which is what
	// makes the chart and the figure beside it the same statement.
	if pos.PaceBalance != 0 {
		c.ShowGap = true
		c.GapBanked = pos.PaceBalance > 0
		c.GapY1 = y(lastAmount(tl.Actual))
		c.GapY2 = y(pos.TargetByNow)
		verb := "banked"
		if pos.PaceBalance < 0 {
			verb = "borrowed"
		}
		c.GapLabel = signedUSD(pos.PaceBalance) + " " + verb
		c.GapLabelX = round2(c.NowX + 6)
		c.GapLabelY = round2((c.GapY1 + c.GapY2) / 2)
	}

	for _, frac := range []float64{0, 0.25, 0.5, 0.75, 1} {
		m := money.Money(float64(top) * frac)
		c.YTicks = append(c.YTicks, ChartTick{Label: usd(m), Y: y(m)})
	}
	steps := 6
	for i := 0; i <= steps; i++ {
		at := tl.Start.Add(time.Duration(int64(span) * int64(i) / int64(steps)))
		c.XTicks = append(c.XTicks, ChartTick{Label: clockTime(at), X: x(at)})
	}
	return c
}

func lastAmount(pts []report.Point) money.Money {
	if len(pts) == 0 {
		return 0
	}
	return pts[len(pts)-1].Amount
}

// linePath draws a polyline through the samples.
func linePath(pts []report.Point, x func(time.Time) float64, y func(money.Money) float64) string {
	if len(pts) == 0 {
		return ""
	}
	var b strings.Builder
	for i, pt := range pts {
		if i == 0 {
			fmt.Fprintf(&b, "M %g %g", x(pt.At), y(pt.Amount))
			continue
		}
		fmt.Fprintf(&b, " L %g %g", x(pt.At), y(pt.Amount))
	}
	return b.String()
}

// stepPath draws the samples as a step function: horizontal to the next charge's
// timestamp, then vertical by its amount.
//
// This is the honest shape for settled charges. A diagonal between two charges would
// draw money leaving the account at instants when nothing was billed.
func stepPath(pts []report.Point, x func(time.Time) float64, y func(money.Money) float64) string {
	if len(pts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "M %g %g", x(pts[0].At), y(pts[0].Amount))
	prev := pts[0]
	for _, pt := range pts[1:] {
		fmt.Fprintf(&b, " L %g %g", x(pt.At), y(prev.Amount))
		fmt.Fprintf(&b, " L %g %g", x(pt.At), y(pt.Amount))
		prev = pt
	}
	return b.String()
}
