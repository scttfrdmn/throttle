package report

import (
	"context"
	"sort"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/money"
)

// Facet is a dimension to break spend down by.
//
// Access provider, publisher, and model are three separate facets rather than one
// "provider" field, because they answer different questions: "how much went through
// Bedrock?", "how much did I spend on Anthropic models anywhere?", and "which model
// cost the most?" are not the same question, and collapsing AWS Bedrock / Anthropic /
// Claude into one string makes all three unanswerable.
type Facet string

const (
	// FacetAccessProvider is the path the request took, e.g. aws-bedrock.
	FacetAccessProvider Facet = "access-provider"

	// FacetPublisher is who made the model, e.g. anthropic.
	FacetPublisher Facet = "publisher"

	// FacetFamily groups model versions.
	FacetFamily Facet = "family"

	// FacetModel is the canonical model where recognized, falling back to the exact
	// provider model ID. The fallback is why an unrecognized model still appears in a
	// breakdown instead of being lumped into a blank row.
	FacetModel Facet = "model"

	// FacetProviderModel is always the exact provider model ID, which is the identity
	// the provider's bill uses.
	FacetProviderModel Facet = "provider-model"

	// FacetOperation is the provider call made.
	FacetOperation Facet = "operation"

	// FacetBudget is the budget the caller named.
	FacetBudget Facet = "budget"

	// FacetRegion is the access region.
	FacetRegion Facet = "region"

	// FacetStatus is how the requests ended.
	FacetStatus Facet = "status"
)

// Facets is every facet a breakdown can group by, in display order.
var Facets = []Facet{
	FacetAccessProvider, FacetPublisher, FacetFamily, FacetModel,
	FacetProviderModel, FacetOperation, FacetBudget, FacetRegion, FacetStatus,
}

// Label is the human name of a facet.
func (f Facet) Label() string {
	switch f {
	case FacetAccessProvider:
		return "Access provider"
	case FacetPublisher:
		return "Model publisher"
	case FacetFamily:
		return "Model family"
	case FacetModel:
		return "Model"
	case FacetProviderModel:
		return "Provider model ID"
	case FacetOperation:
		return "Operation"
	case FacetBudget:
		return "Budget"
	case FacetRegion:
		return "Region"
	case FacetStatus:
		return "Status"
	default:
		return string(f)
	}
}

// Breakdown is spend grouped by one facet.
type Breakdown struct {
	Facet Facet
	Rows  []BreakdownRow

	// Total is the sum of the rows' spend, and Complete reports whether that sum is a
	// total or a floor. A breakdown whose rows include unpriceable requests adds up to
	// a floor, and saying so is the difference between a chart and a claim.
	Total    money.Money
	Complete bool

	// Requests is the number of records counted.
	Requests int
}

// BreakdownRow is one group.
type BreakdownRow struct {
	// Key is the facet value. Empty means the dimension was not reported, which is
	// rendered as an explicit "not reported" rather than as a blank.
	Key string

	// Spend is what this group spent: known amounts plus the floors of what could not
	// be priced.
	Spend money.Money

	// Complete reports whether Spend is a total or a floor for this group.
	Complete bool

	// Requests is how many records are in the group, and Unresolved how many of those
	// have a cost that is not fully known.
	Requests   int
	Unresolved int

	// Reserved is the headroom this group's in-flight and unresolved requests still
	// hold. It is separate from Spend and is never added into it.
	Reserved money.Money
}

// ShareBasisPoints is this row's share of a total, in basis points.
//
// Integer basis points rather than a float percentage, so a bar width is computed
// from the same arithmetic as everything else here. A zero or negative total yields
// zero rather than a division by zero.
func (r BreakdownRow) ShareBasisPoints(total money.Money) int64 {
	if total <= 0 || r.Spend <= 0 {
		return 0
	}
	return int64(r.Spend) * 10_000 / int64(total)
}

// Breakdown groups spend by a facet.
//
// It reads the activity store because attribution lives there: the ledger knows a
// charge belongs to a budget and a period, not that it was an Anthropic model reached
// through Bedrock. That is exactly the division of labour this package keeps -- the
// ledger for money, activity for attribution -- so a breakdown's totals are floors
// derived from records and are labelled as such, and the authoritative period total
// stays the one on the Position.
func (r *Reporter) Breakdown(ctx context.Context, facet Facet, q ActivityQuery) (Breakdown, error) {
	if r.acts == nil {
		return Breakdown{}, errNotConfigured("spend breakdowns")
	}
	records, err := r.acts.List(ctx, activity.Filter{
		BudgetID:       q.BudgetID,
		PeriodID:       q.PeriodID,
		From:           q.From,
		To:             q.To,
		Statuses:       q.Statuses,
		UnresolvedOnly: q.UnresolvedOnly,
		Limit:          q.Limit,
	})
	if err != nil {
		return Breakdown{}, err
	}
	return groupBy(facet, records), nil
}

// Breakdowns computes several facets from one read of the store.
//
// One query, many groupings: issuing a separate query per facet would read the same
// rows five times to produce five views of them.
func (r *Reporter) Breakdowns(ctx context.Context, facets []Facet, q ActivityQuery) ([]Breakdown, error) {
	if r.acts == nil {
		return nil, errNotConfigured("spend breakdowns")
	}
	records, err := r.acts.List(ctx, activity.Filter{
		BudgetID:       q.BudgetID,
		PeriodID:       q.PeriodID,
		From:           q.From,
		To:             q.To,
		Statuses:       q.Statuses,
		UnresolvedOnly: q.UnresolvedOnly,
		Limit:          q.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Breakdown, 0, len(facets))
	for _, f := range facets {
		out = append(out, groupBy(f, records))
	}
	return out, nil
}

// groupBy aggregates records by a facet.
func groupBy(facet Facet, records []activity.Record) Breakdown {
	b := Breakdown{Facet: facet, Complete: true, Requests: len(records)}
	rows := map[string]*BreakdownRow{}

	for _, rec := range records {
		key := facetValue(facet, rec)
		row := rows[key]
		if row == nil {
			row = &BreakdownRow{Key: key, Complete: true}
			rows[key] = row
		}
		row.Requests++

		amount, complete := rec.Spent()
		if v, ok := money.Add(row.Spend, amount); ok {
			row.Spend = v
		}
		if v, ok := money.Add(b.Total, amount); ok {
			b.Total = v
		}
		if !complete {
			row.Complete = false
			b.Complete = false
		}
		if rec.Status == activity.StatusUnresolved || rec.Status == activity.StatusOutstanding {
			row.Unresolved++
			if v, ok := money.Add(row.Reserved, rec.Reserved); ok {
				row.Reserved = v
			}
		}
	}

	b.Rows = make([]BreakdownRow, 0, len(rows))
	for _, row := range rows {
		b.Rows = append(b.Rows, *row)
	}
	// Largest spend first, then by key, so the order is stable when several groups
	// spent the same amount -- including when they all spent nothing.
	sort.Slice(b.Rows, func(i, j int) bool {
		if b.Rows[i].Spend != b.Rows[j].Spend {
			return b.Rows[i].Spend > b.Rows[j].Spend
		}
		return b.Rows[i].Key < b.Rows[j].Key
	})
	return b
}

// facetValue extracts one facet from a record.
//
// The three identity facets read three different fields. There is no fallback from
// publisher to access provider or from model to publisher: an absent dimension is
// reported absent, because filling it in from a neighbouring field would invent an
// attribution the provider never made.
func facetValue(facet Facet, rec activity.Record) string {
	id := rec.Identity
	switch facet {
	case FacetAccessProvider:
		return id.AccessProvider
	case FacetPublisher:
		return id.Publisher
	case FacetFamily:
		return id.Family
	case FacetModel:
		// The one deliberate fallback in this function, and it is not a guess: the
		// exact provider model ID is authoritative identity, and canonical naming is
		// enrichment layered on top. A model the catalog has never heard of belongs in
		// its own row under its real ID, not in an "unknown" bucket with every other
		// unrecognized model.
		if id.CanonicalModel != "" {
			return id.CanonicalModel
		}
		return id.ProviderModelID
	case FacetProviderModel:
		return id.ProviderModelID
	case FacetOperation:
		return id.Operation
	case FacetBudget:
		return rec.BudgetID
	case FacetRegion:
		return id.Region
	case FacetStatus:
		return string(rec.Status)
	default:
		return ""
	}
}
