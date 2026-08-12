package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"throttle/activity"
	"throttle/budget"
	"throttle/money"
	"throttle/report"
)

// buildPosition renders the budget summary panel.
//
// Every term in the vocabulary gets its own field. Reserved is never folded into Spent
// and never called spent; the pace balance and the remaining allocation are separate
// figures because they answer different questions; and anything that can legitimately
// be negative is rendered with its sign.
func buildPosition(pos report.Position, binding string, borrow time.Duration) PositionView {
	v := PositionView{
		BudgetID:     pos.BudgetID,
		Name:         pos.Name,
		Allocation:   usd(pos.Allocation),
		CarryIn:      signedUSD(pos.CarryIn),
		HasCarry:     pos.CarryIn != 0,
		Total:        usd(pos.Total),
		Spent:        usd(pos.Spent),
		Reserved:     usd(pos.Reserved),
		Remaining:    signedUSD(pos.RemainingAllocation),
		Overspent:    pos.Overspent(),
		TargetByNow:  usd(pos.TargetByNow),
		AllowedByNow: usd(pos.AllowedByNow),
		HasBorrow:    borrow > 0,
		PaceBalance:  signedUSD(pos.PaceBalance),
		SpendableNow: usd(pos.SpendableNow),
		PeriodStart:  dayStamp(pos.PeriodStart),
		PeriodEnd:    dayStamp(pos.PeriodEnd),
		PeriodState:  string(pos.Period.State),
		Provisional:  pos.Period.Provisional(),

		Elapsed:       duration(pos.Elapsed),
		TimeRemaining: duration(pos.TimeRemaining),

		LiveHolds: pos.LiveHolds,
	}
	if borrow > 0 {
		v.BorrowWindow = duration(borrow)
	}
	v.ElapsedPct = elapsedPct(pos.Elapsed, pos.PeriodDuration)
	if binding != "" && binding != pos.BudgetID {
		v.Binding = binding
	}
	return v
}

// buildRates renders the two explicit burn rates.
//
// The first is labelled "average to date" and never "current": it is an average across
// the whole elapsed period and is not sensitive to the last five minutes.
func buildRates(pos report.Position) RatesView {
	return RatesView{
		AverageToDate:   rate(pos.AverageBurn),
		AverageNote:     rateNote(pos.AverageBurn),
		Sustainable:     rate(pos.SustainableBurn),
		SustainableNote: sustainableNote(pos),
		Unit:            unitName(pos.AverageBurn.Per),
	}
}

// sustainableNote explains an absent sustainable rate, which is a different absence
// from an absent average.
func sustainableNote(pos report.Position) string {
	switch {
	case pos.SustainableBurn.Known:
		return ""
	case pos.TimeRemaining <= 0:
		return "no rate: the period is over"
	case pos.RemainingAllocation <= 0:
		return "no sustainable rate: there is no remaining allocation to spread"
	default:
		return "no rate available"
	}
}

// buildEndState renders the range panel.
func buildEndState(pos report.Position) EndStateView {
	v := EndStateView{
		RemainingAllocation: signedUSD(pos.RemainingAllocation),
		Overspent:           pos.Overspent(),
		TimeRemaining:       duration(pos.TimeRemaining),
		SustainableBurn:     rate(pos.SustainableBurn),
		Projection:          projectionText(pos.Projection),
		ProjectionNote:      projectionNote(pos.Projection),
	}
	// Only the side that applies is shown, and only when the projection exists at all:
	// "under by" on a figure that was never computed would be a claim about the future
	// made from no measurement.
	if pos.Projection.Known {
		if pos.Projection.OverBy > 0 {
			v.OverBy = usd(pos.Projection.OverBy)
		} else if pos.Projection.UnderBy > 0 {
			v.UnderBy = usd(pos.Projection.UnderBy)
		}
	}
	return v
}

// buildRollover renders the carry configuration, or nothing when rollover is off.
func buildRollover(pos report.Position) *RolloverView {
	p := pos.Rollover
	mode := p.Mode
	if mode == "" {
		mode = budget.RolloverNone
	}
	if mode == budget.RolloverNone && pos.CarryIn == 0 {
		// Nothing to say, and the primary display stays uncluttered.
		return nil
	}
	v := &RolloverView{
		Mode:    string(mode),
		CarryIn: signedUSD(pos.CarryIn),
	}
	switch mode {
	case budget.RolloverCredit:
		v.Explain = "unspent allocation carries into the next period as a credit"
	case budget.RolloverBalance:
		v.Explain = "the signed balance carries into the next period, including a deficit"
	default:
		v.Explain = "rollover is off, but this period inherited a carry"
	}
	if p.Cap > 0 {
		v.HasCap, v.Cap = true, usd(p.Cap)
	}
	if p.CapBasisPoints > 0 {
		v.HasCap = true
		v.CapPct = fmt.Sprintf("%.2f%% of the allocation", float64(p.CapBasisPoints)/100)
	}
	return v
}

// buildTree renders the hierarchy table.
func buildTree(t report.Tree, selected string) []NodeView {
	var out []NodeView
	for _, fn := range t.Flatten() {
		pos := fn.Position
		n := NodeView{
			BudgetID:    pos.BudgetID,
			Name:        pos.Name,
			Depth:       fn.Depth,
			Indent:      strings.Repeat("　", fn.Depth),
			Selected:    pos.BudgetID == selected,
			Allocation:  usd(pos.Allocation),
			Spent:       usd(pos.Spent),
			Reserved:    usd(pos.Reserved),
			Remaining:   signedUSD(pos.RemainingAllocation),
			Overspent:   pos.Overspent(),
			PaceBalance: signedUSD(pos.PaceBalance),
			Banked:      pos.Banked(),
			Borrowed:    pos.Borrowed(),
			Pressure:    pressureText(pos.Pressure),
			State:       string(pos.Pressure.State),
			Error:       t.Errors[pos.BudgetID],
		}
		out = append(out, n)
	}
	return out
}

// buildBudgetOptions renders the budget selector, indented to show the hierarchy.
func buildBudgetOptions(t report.Tree, selected string) []BudgetOption {
	var out []BudgetOption
	for _, fn := range t.Flatten() {
		out = append(out, BudgetOption{
			ID:       fn.Position.BudgetID,
			Name:     fn.Position.Name,
			Depth:    fn.Depth,
			Indent:   strings.Repeat("— ", fn.Depth),
			Selected: fn.Position.BudgetID == selected,
		})
	}
	return out
}

// buildPeriodOptions renders the period selector.
//
// Monthly is not special: these are whatever envelopes the definition generated, so a
// one-week demo and an academic grant list the same way.
func buildPeriodOptions(opts []report.PeriodOption, selected string) []PeriodOption {
	out := make([]PeriodOption, 0, len(opts))
	for _, o := range opts {
		label := dayStamp(o.Start) + " → " + dayStamp(o.End)
		if o.Current {
			label += " (current)"
		}
		out = append(out, PeriodOption{
			PeriodID: o.PeriodID,
			Label:    label,
			Current:  o.Current,
			Selected: o.PeriodID == selected,
			State:    string(o.State),
		})
	}
	return out
}

// buildHolds renders the reservations behind the Reserved figure.
func buildHolds(holds []report.Hold) []HoldView {
	out := make([]HoldView, 0, len(holds))
	for _, h := range holds {
		out = append(out, HoldView{
			ReservationID: h.ReservationID,
			RequestID:     h.RequestID,
			BudgetID:      h.BudgetID,
			Amount:        usd(h.Amount),
			Estimated:     usd(h.EstimatedCost),
			Age:           duration(h.Age),
			Expires:       timestamp(h.ExpiresAt),
			Expired:       h.Expired,
			Model:         present(h.Model),
			ModelKnown:    h.ModelKnown,
			Operation:     present(h.Operation),
			Renewals:      h.RenewCount,
		})
	}
	return out
}

// buildActivity renders the request table.
func buildActivity(page report.ActivityPage, available bool, errText string) ActivityView {
	v := ActivityView{
		Available:    available,
		Error:        errText,
		Truncated:    page.Truncated,
		Limit:        page.Limit,
		PageSpend:    usd(page.Summary.Spend),
		PageComplete: page.Summary.Complete,
		Requests:     page.Summary.Requests,
	}
	if !page.Summary.Complete {
		// The page total is a floor, and it says so in the figure itself rather than
		// only in a footnote.
		v.PageSpend += "+"
	}
	for _, e := range page.Events {
		v.Events = append(v.Events, buildEvent(e))
	}
	return v
}

// buildEvent renders one request row.
//
// Twelve columns, and the three identity facets stay three columns. Collapsing AWS
// Bedrock, Anthropic, and Claude into one "provider" cell would make three different
// questions unanswerable at once.
func buildEvent(e report.Event) EventView {
	when := e.StartedAt
	if when.IsZero() {
		when = e.CompletedAt
	}
	v := EventView{
		RequestID:       e.RequestID,
		Time:            when.UTC().Format("15:04:05"),
		TimeFull:        timestamp(when),
		BudgetID:        e.BudgetID,
		Scopes:          scopeList(e.Scopes),
		Operation:       present(e.Operation),
		AccessProvider:  present(e.AccessProvider),
		Publisher:       present(e.Publisher),
		Model:           present(e.Model),
		ModelKnown:      e.ModelKnown,
		ProviderModelID: e.ProviderModelID,
		Usage:           usageSummary(e.Usage),
		UsageTitle:      usageDetail(e.Usage),
		Estimated:       amountPrecise(e.Estimated),
		Reserved:        usdPrecise(e.Reserved),
		Actual:          amountPrecise(e.Actual),
		ActualState:     string(e.Actual.State),
		ActualTitle:     actualTitle(e.Actual),
		Mode:            string(e.EnforcementMode),
		Latency:         latency(e.Latency),
		Status:          string(e.Status),
		Outcome:         string(e.Outcome),
		Error:           e.Error,
	}
	if e.Overrun > 0 {
		// An actual cost above the reservation is recorded, not hidden: the estimate was
		// wrong and the money is gone regardless.
		v.HasOverrun, v.Overrun = true, usdPrecise(e.Overrun)
	}
	if e.Compound {
		v.Flags = append(v.Flags, fmt.Sprintf("agent · %d steps", e.StepCount))
	}
	if e.HostedRuntime {
		v.Flags = append(v.Flags, "hosted runtime")
	}
	if e.AwaitingExternal {
		v.Flags = append(v.Flags, "awaiting external usage")
	}
	if e.Repaired {
		v.Flags = append(v.Flags, "reconciled")
	}
	return v
}

// actualTitle explains an incomplete cost in words, for the cell's title attribute.
func actualTitle(a report.Amount) string {
	parts := []string{costStateLabel(a.State)}
	if a.Reason != "" {
		parts = append(parts, a.Reason)
	}
	if len(a.Unpriced) > 0 {
		parts = append(parts, "unpriced: "+strings.Join(a.Unpriced, ", "))
	}
	return strings.Join(parts, " — ")
}

func scopeList(scopes []activity.Scope) string {
	if len(scopes) == 0 {
		return "—"
	}
	ids := make([]string, 0, len(scopes))
	for _, s := range scopes {
		ids = append(ids, s.BudgetID)
	}
	return strings.Join(ids, " ← ")
}

// usageSummary is the compact usage cell.
//
// Tokens are summarized as in/out because that is what a reader scans, and any other
// dimension is named rather than dropped: not all activity is token-based, and a
// dimension throttle has never seen must not vanish because the UI predates it.
func usageSummary(items []report.UsageItem) string {
	if len(items) == 0 {
		return "—"
	}
	var in, out int64
	var other []string
	haveTokens := false
	for _, it := range items {
		switch it.Dimension {
		case "input_tokens", "cache_read_tokens", "cache_write_tokens":
			in += it.Count
			haveTokens = true
		case "output_tokens", "reasoning_tokens":
			out += it.Count
			haveTokens = true
		default:
			other = append(other, fmt.Sprintf("%s %s", group(uint64(abs64(it.Count))), shortDimension(it.Dimension)))
		}
	}
	var parts []string
	if haveTokens {
		parts = append(parts, fmt.Sprintf("%s in / %s out", group(uint64(in)), group(uint64(out))))
	}
	parts = append(parts, other...)
	return strings.Join(parts, " · ")
}

// usageDetail is every dimension with its exact count, for the title attribute.
func usageDetail(items []report.UsageItem) string {
	if len(items) == 0 {
		return "no usage reported"
	}
	parts := make([]string, 0, len(items))
	for _, it := range items {
		s := fmt.Sprintf("%s: %s", it.Dimension, group(uint64(abs64(it.Count))))
		if it.Unpriced {
			s += " (unpriced)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

// shortDimension abbreviates a dimension name for a narrow cell without inventing one:
// an unrecognized name is shown as itself.
func shortDimension(d string) string {
	switch d {
	case "runtime_vcpu_nano_hours":
		return "vCPU·nh"
	case "runtime_memory_nano_gb_hours":
		return "mem·nGBh"
	default:
		return d
	}
}

// buildUsageRows renders the full dimensional usage table for a detail page.
func buildUsageRows(items []report.UsageItem) []UsageRow {
	out := make([]UsageRow, 0, len(items))
	for _, it := range items {
		out = append(out, UsageRow{
			Dimension: it.Dimension,
			Count:     group(uint64(abs64(it.Count))),
			Token:     it.Token,
			Unpriced:  it.Unpriced,
		})
	}
	return out
}

// buildBreakdowns renders spend grouped by each facet.
func buildBreakdowns(bs []report.Breakdown) []BreakdownView {
	out := make([]BreakdownView, 0, len(bs))
	for _, b := range bs {
		v := BreakdownView{
			Facet:    string(b.Facet),
			Label:    b.Facet.Label(),
			Total:    usd(b.Total),
			Complete: b.Complete,
			Requests: b.Requests,
		}
		if !b.Complete {
			v.Total += "+"
		}
		for _, row := range b.Rows {
			bp := row.ShareBasisPoints(b.Total)
			r := BreakdownRowView{
				// An unreported dimension is named as unreported. A blank cell is
				// indistinguishable from a rendering fault, and inventing a publisher
				// from the access provider would be worse still.
				Key:        present(row.Key),
				Reported:   strings.TrimSpace(row.Key) != "",
				Spend:      usd(row.Spend),
				Complete:   row.Complete,
				Requests:   row.Requests,
				Unresolved: row.Unresolved,
				SharePct:   round2(float64(bp) / 100),
				ShareText:  percent(bp),
			}
			if !row.Complete {
				r.Spend += "+"
			}
			if row.Reserved > 0 {
				r.HasHold, r.Reserved = true, usd(row.Reserved)
			}
			v.Rows = append(v.Rows, r)
		}
		out = append(out, v)
	}
	return out
}

// buildRecon renders the reconciliation panel.
//
// Read-only, and it says so. The panel shows what is outstanding and the reconciler's
// own typed reasons; it does not repair anything, because money should move when an
// operator decides it should and not because a browser polled.
func buildRecon(h report.Health, rows report.ActivityPage, available bool, errText string) ReconView {
	v := ReconView{
		Clean:              h.Clean(),
		Needs:              h.Needs(),
		Unresolved:         h.Unresolved,
		OutcomeUnknown:     h.OutcomeUnknown,
		AwaitingExternal:   h.AwaitingExternal,
		ExpiredHolds:       h.ExpiredHolds,
		Encumbered:         usd(h.Encumbered),
		ExpiredAmount:      usd(h.ExpiredAmount),
		Repaired:           h.Repaired,
		UnpricedDimensions: h.UnpricedDimensions,
		Available:          available,
		Error:              errText,
		Explain: "this panel is read-only: opening it runs no repair. " +
			`Use "throttle reconcile" to move money.`,
	}
	for _, e := range rows.Events {
		v.Rows = append(v.Rows, buildEvent(e))
	}
	return v
}

// buildDetail renders one request's detail page.
func buildDetail(d report.Detail, at time.Time) DetailPage {
	p := DetailPage{
		Title:           "request " + d.Event.RequestID,
		At:              at,
		AtDisplay:       timestamp(at),
		Event:           buildEvent(d.Event),
		QuoteSource:     present(d.QuoteSource),
		QuoteVersion:    present(d.QuoteVersion),
		QuoteCapturedAt: timestamp(d.QuoteCapturedAt),
		EstimateQuality: present(d.Event.EstimateQuality),
		Usage:           buildUsageRows(d.Event.Usage),
	}
	if d.Agent != nil {
		p.Agent = buildAgent(*d.Agent)
	}
	if d.Runtime != nil {
		p.Runtime = buildRuntime(*d.Runtime)
	}
	for _, rep := range d.Repairs {
		p.Repairs = append(p.Repairs, buildRepair(rep))
	}
	return p
}

// buildAgent renders the compound transaction.
//
// One governed request, one reservation, one charge, and beneath it the internal model
// calls the provider reported. The steps are accounting detail, not transactions:
// throttle admitted the turn, and presenting a step as its own governed request would
// misrepresent what was governed. No step carries content, because none is stored.
func buildAgent(a report.AgentDetail) *AgentView {
	v := &AgentView{
		AgentID:   present(a.AgentID),
		AliasID:   present(a.AliasID),
		Version:   present(a.Version),
		SessionID: present(a.SessionID),
		Total:     amountPrecise(a.Total),
		StepSum:   usdPrecise(a.StepSum),
		Note:      a.Note,
	}
	for _, s := range a.Steps {
		v.Steps = append(v.Steps, AgentStepView{
			Seq:          s.Seq,
			Kind:         present(s.Kind),
			Collaborator: s.Collaborator,
			Model:        present(s.Model),
			ModelKnown:   s.ModelKnown,
			Publisher:    present(s.Publisher),
			Usage:        usageSummary(s.Usage),
			Cost:         amountPrecise(s.Cost),
			Latency:      latency(s.Latency),
			At:           timestamp(s.At),
		})
	}
	kinds := make([]string, 0, len(a.Events))
	for k := range a.Events {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		v.Events = append(v.Events, AgentEventView{Kind: k, Count: a.Events[k]})
	}
	if a.GapVisible() {
		// Explained rather than hidden, and certainly rather than fixed by adjusting a
		// step: the charge is accumulated exactly and rounded once, so the displayed
		// steps can legitimately not sum to the displayed total.
		//
		// Which explanation is honest depends on the size of the gap. Rounding accounts
		// for fractions of a cent, so attributing dollars to it would be a display
		// explaining away a disagreement between the provider's own figures.
		if a.GapExplainedByRounding() {
			v.GapNote = fmt.Sprintf(
				"the steps shown sum to %s against a request charge of %s. The turn is "+
					"accumulated exactly and rounded once, so per-step rounding need not add up. "+
					"The request charge is the authoritative figure.",
				usdPrecise(a.StepSum), amountPrecise(a.Total))
		} else {
			v.GapMaterial = true
			v.GapNote = fmt.Sprintf(
				"the steps shown sum to %s against a request charge of %s, a difference of %s "+
					"that display rounding cannot account for. The provider's per-step figures "+
					"do not add up to what it charged for the turn: the steps are incomplete, "+
					"priced differently, or both. The request charge is the authoritative figure "+
					"and is what was billed to the budget.",
				usdPrecise(a.StepSum), amountPrecise(a.Total), usdPrecise(abs(a.RoundingGap)))
		}
	}
	return v
}

// buildRuntime renders a hosted-runtime invocation.
func buildRuntime(r report.RuntimeDetail) *RuntimeView {
	v := &RuntimeView{
		RuntimeID:         present(r.RuntimeID),
		Qualifier:         present(r.Qualifier),
		SessionID:         present(r.SessionID),
		ProviderRequestID: present(r.ProviderRequestID),
		TraceID:           present(r.TraceID),
		StatusCode:        r.StatusCode,
		Sizes: fmt.Sprintf("%s bytes sent / %s received",
			group(uint64(abs64(r.PayloadBytes))), group(uint64(abs64(r.ResponseBytes)))),
		MaxExposure:    usd(r.MaxExposure),
		Reconciled:     r.Reconciled,
		ReconciledCost: amountPrecise(r.ReconciledCost),
		ReconciledFrom: r.ReconciledFrom,
		Note:           r.Note,
		ReconciledUse:  buildUsageRows(r.ReconciledUsage),
	}
	if !r.ReconciledAt.IsZero() {
		v.ReconciledAt = timestamp(r.ReconciledAt)
	}
	if r.SessionScoped {
		v.Explain = "resource usage for a hosted runtime is reported per session, out of band. " +
			"A session's charge includes start-up, idle time, and platform overhead that no " +
			"single invocation caused, so it is not divided across the invocations that " +
			"shared the session: this call's own runtime cost is unknown rather than estimated."
	}
	return v
}

// buildRepair renders one reconciliation trail entry with the reconciler's own
// vocabulary carried through verbatim.
func buildRepair(r report.Repair) RepairView {
	v := RepairView{
		At:       timestamp(r.At),
		Class:    r.Class,
		Reason:   r.Reason,
		Observed: r.ObservedStatus,
		Produced: r.ProducedStatus,
		Money:    r.Money,
		Detail:   r.Detail,
	}
	if r.ObservedReservation != "" {
		v.Observed += " · " + r.ObservedReservation
	}
	if r.Money != "" {
		v.Amount = usdPrecise(r.Amount)
	}
	if r.QuoteSource != "" || r.QuoteVersion != "" {
		v.Quote = strings.TrimSpace(r.QuoteSource + " " + r.QuoteVersion)
	}
	return v
}

// notices are the conditions a reader must see rather than infer from a blank panel.
func notices(sum report.Summary, tl report.Timeline, at time.Time) []Notice {
	var out []Notice

	if !sum.ActivityAvailable {
		text := "request history is unavailable, so the tables below are empty for that " +
			"reason rather than because nothing happened. Budget figures come from the " +
			"ledger and are unaffected."
		if sum.ActivityError != "" {
			text = "the activity database could not be read (" + sum.ActivityError +
				"), so request history is missing. Budget figures come from the ledger and are unaffected."
		}
		out = append(out, Notice{Level: "warn", Text: text})
	}

	if sum.Disagreement.Material() {
		out = append(out, Notice{Level: "warn", Text: fmt.Sprintf(
			"the ledger reports %s settled and request history accounts for %s, a difference of %s. "+
				"The ledger is authoritative. This is what reconciliation exists to resolve.",
			usd(sum.Disagreement.LedgerSpent), usd(sum.Disagreement.ActivitySpent),
			signedUSD(sum.Disagreement.Delta))})
	}

	pos := sum.Position
	switch {
	case at.Before(pos.PeriodStart):
		out = append(out, Notice{Level: "info", Text: "this period has not started yet, so there " +
			"is no elapsed time to measure a rate over. The envelope is shown as configured."})
	case !at.Before(pos.PeriodEnd):
		out = append(out, Notice{Level: "info", Text: "this period has ended. The figures are its " +
			"final position, and the gauge has no reading because there is no remaining time."})
	}

	if pos.Period.Provisional() {
		out = append(out, Notice{Level: "info", Text: "the carry into this period is provisional: " +
			"the preceding period is still draining and may release money, which can only raise it."})
	}

	if pos.ExpiredHolds > 0 {
		out = append(out, Notice{Level: "warn", Text: fmt.Sprintf(
			`%d reservation(s) holding %s have expired leases and have not been recovered. `+
				`Their headroom is already excluded from Reserved. Run "throttle recover".`,
			pos.ExpiredHolds, usd(pos.ExpiredAmount))})
	}

	if tl.Truncated {
		out = append(out, Notice{Level: "info", Text: fmt.Sprintf(
			"the chart is drawn from the most recent %d charges; older ones exist, so the "+
				"line starts partway up.", tl.Charges)})
	}
	return out
}

// summaryJSON is the shape of /api/summary: the figures a poll refreshes, already
// formatted, so the browser never does money arithmetic.
type summaryJSON struct {
	BudgetID string `json:"budget_id"`
	PeriodID string `json:"period_id"`

	// At is machine-readable and AtDisplay is what the page shows. Both are here so a
	// poll can patch the header without the browser reformatting a timestamp into a
	// second convention.
	At        string `json:"at"`
	AtDisplay string `json:"at_display"`

	Allocation string `json:"allocation"`
	CarryIn    string `json:"carry_in"`
	Total      string `json:"total"`

	Spent    string `json:"spent"`
	Reserved string `json:"reserved"`

	Remaining string `json:"remaining_allocation"`
	Overspent bool   `json:"overspent"`

	TargetByNow  string `json:"target_by_now"`
	AllowedByNow string `json:"allowed_by_now"`
	PaceBalance  string `json:"pace_balance"`
	SpendableNow string `json:"spendable_now"`

	PeriodStart string `json:"period_start"`
	PeriodEnd   string `json:"period_end"`

	Elapsed       string `json:"elapsed"`
	TimeRemaining string `json:"time_remaining"`
	ElapsedPct    string `json:"elapsed_pct"`

	AverageBurn     string `json:"average_burn_to_date"`
	SustainableBurn string `json:"sustainable_burn"`

	Pressure      string  `json:"burn_pressure"`
	PressureState string  `json:"burn_pressure_state"`
	PressureBP    int64   `json:"burn_pressure_basis_points"`
	NeedleX       float64 `json:"needle_x"`
	NeedleY       float64 `json:"needle_y"`
	Arc           string  `json:"gauge_arc"`

	BankLabel   string  `json:"bank_label"`
	BankAmount  string  `json:"bank_amount"`
	BankFillPct float64 `json:"bank_fill_pct"`

	Projection     string `json:"straight_line_projection"`
	ProjectionNote string `json:"straight_line_projection_note"`

	LiveHolds    int `json:"live_holds"`
	ExpiredHolds int `json:"expired_holds"`

	Unresolved       int `json:"unresolved"`
	OutcomeUnknown   int `json:"outcome_unknown"`
	AwaitingExternal int `json:"awaiting_external"`

	ActivityAvailable bool   `json:"activity_available"`
	ActivityError     string `json:"activity_error,omitempty"`
}

// buildSummaryJSON renders the poll payload.
//
// Strings, not numbers: the browser is handed finished figures so that no formatting
// rule -- least of all the rule that keeps an unknown cost from printing as $0.00 --
// has a second implementation in JavaScript. The one integer is basis points, which is
// exact.
func buildSummaryJSON(sum report.Summary, page Page) summaryJSON {
	pos := sum.Position
	return summaryJSON{
		BudgetID:  pos.BudgetID,
		PeriodID:  pos.Period.ID,
		At:        pos.At.UTC().Format(time.RFC3339),
		AtDisplay: timestamp(pos.At),

		Allocation: usd(pos.Allocation),
		CarryIn:    signedUSD(pos.CarryIn),
		Total:      usd(pos.Total),

		Spent:    usd(pos.Spent),
		Reserved: usd(pos.Reserved),

		Remaining: signedUSD(pos.RemainingAllocation),
		Overspent: pos.Overspent(),

		TargetByNow:  usd(pos.TargetByNow),
		AllowedByNow: usd(pos.AllowedByNow),
		PaceBalance:  signedUSD(pos.PaceBalance),
		SpendableNow: usd(pos.SpendableNow),

		PeriodStart: dayStamp(pos.PeriodStart),
		PeriodEnd:   dayStamp(pos.PeriodEnd),

		Elapsed:       duration(pos.Elapsed),
		TimeRemaining: duration(pos.TimeRemaining),
		ElapsedPct:    page.Position.ElapsedPct,

		AverageBurn:     rate(pos.AverageBurn),
		SustainableBurn: rate(pos.SustainableBurn),

		Pressure:      pressureText(pos.Pressure),
		PressureState: string(pos.Pressure.State),
		PressureBP:    pos.Pressure.BasisPoints,
		NeedleX:       page.Pressure.NeedleX,
		NeedleY:       page.Pressure.NeedleY,
		Arc:           page.Pressure.Arc,

		BankLabel:   page.Bank.Label,
		BankAmount:  page.Bank.Amount,
		BankFillPct: page.Bank.FillPct,

		Projection:     projectionText(pos.Projection),
		ProjectionNote: projectionNote(pos.Projection),

		LiveHolds:    pos.LiveHolds,
		ExpiredHolds: pos.ExpiredHolds,

		Unresolved:       sum.Health.Unresolved,
		OutcomeUnknown:   sum.Health.OutcomeUnknown,
		AwaitingExternal: sum.Health.AwaitingExternal,

		ActivityAvailable: sum.ActivityAvailable,
		ActivityError:     sum.ActivityError,
	}
}

// timelineJSON is the shape of /api/timeline.
type timelineJSON struct {
	BudgetID string `json:"budget_id"`
	PeriodID string `json:"period_id"`

	Start string `json:"start"`
	End   string `json:"end"`
	Now   string `json:"now"`

	Total     string `json:"envelope_total"`
	Reserved  string `json:"reserved"`
	Committed string `json:"committed"`

	Charges   int  `json:"charges"`
	Truncated bool `json:"truncated"`

	// Target, Actual, and Allowed are the SVG paths, so a poll can replace the lines
	// without the browser recomputing geometry from amounts.
	Target  string `json:"target_path"`
	Actual  string `json:"actual_path"`
	Allowed string `json:"allowed_path,omitempty"`

	NowX float64 `json:"now_x"`

	Points []pointJSON `json:"points"`
}

// pointJSON is one sample of the actual line, as a formatted amount and an instant.
type pointJSON struct {
	At     string `json:"at"`
	Amount string `json:"amount"`
}

func buildTimelineJSON(tl report.Timeline, c ChartView) timelineJSON {
	out := timelineJSON{
		BudgetID:  tl.BudgetID,
		PeriodID:  tl.PeriodID,
		Start:     tl.Start.UTC().Format(time.RFC3339),
		End:       tl.End.UTC().Format(time.RFC3339),
		Now:       tl.Now.UTC().Format(time.RFC3339),
		Total:     usd(tl.Total),
		Reserved:  usd(tl.Reserved),
		Committed: usd(tl.Committed),
		Charges:   tl.Charges,
		Truncated: tl.Truncated,
		Target:    c.Target,
		Actual:    c.Actual,
		Allowed:   c.Allowed,
		NowX:      c.NowX,
	}
	for _, pt := range tl.Actual {
		out.Points = append(out.Points, pointJSON{
			At:     pt.At.UTC().Format(time.RFC3339),
			Amount: usd(pt.Amount),
		})
	}
	return out
}

// activityJSON is the shape of /api/activity.
type activityJSON struct {
	Events    []EventView `json:"events"`
	Truncated bool        `json:"truncated"`
	Limit     int         `json:"limit"`

	// PageSpend is the sum over the rows returned, named so it cannot be read as a
	// period total.
	PageSpend    string `json:"page_spend"`
	PageComplete bool   `json:"page_spend_complete"`
	Requests     int    `json:"requests"`

	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

// breakdownJSON is the shape of /api/breakdown.
type breakdownJSON struct {
	Breakdowns []BreakdownView `json:"breakdowns"`
}

// moneyString is a helper for tests that need the formatter's exact output.
func moneyString(m money.Money) string { return usd(m) }
