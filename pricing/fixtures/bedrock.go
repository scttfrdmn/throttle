// Package fixtures provides a small static price catalog for development and
// tests.
//
// These are fixtures, not a price feed. They cover a handful of models so that
// the governed path can be exercised end to end, and their provenance says
// exactly that. A deployment that cares about accurate numbers should supply its
// own catalog or overrides; keeping that obvious is the point of recording
// provenance on every quote.
//
// Deliberately absent: any attempt to be comprehensive. A model missing here
// produces an explicit unknown cost, which is the correct outcome and is what the
// unknown-cost tests rely on.
package fixtures

import (
	"time"

	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// Source and FixtureVersion identify these numbers as hand-entered fixtures, so a
// surprising cost can be traced to them rather than mistaken for a live price.
const (
	Source         = "throttle-fixture"
	FixtureVersion = "2026-08-fixture-1"
)

// effectiveFrom is early enough that fixtures apply to any test timestamp a suite
// is likely to use, since a price is not applied to a request that predates it.
var effectiveFrom = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// dollars parses a fixture literal. The figures are written as strings and parsed
// exactly rather than being float constants, so that no part of the pricing path
// -- not even the fixtures that seed it -- introduces a float rounding error. A
// malformed literal is a programming error in this file, so it panics at init
// rather than silently pricing something wrong.
func dollars(s string) money.Money {
	m, err := money.Parse(s)
	if err != nil {
		panic("fixtures: malformed price literal " + s + ": " + err.Error())
	}
	return m
}

// price builds a fixture price from per-million-token dollar figures. An empty
// cache figure means the model has no separately-priced cache dimension.
func price(modelID string, in, out, cacheRead, cacheWrite string) pricing.Price {
	rates := map[usage.Dimension]pricing.Rate{
		usage.InputTokens:  pricing.PerMillion(usage.InputTokens, dollars(in)),
		usage.OutputTokens: pricing.PerMillion(usage.OutputTokens, dollars(out)),
	}
	if cacheRead != "" {
		rates[usage.CacheReadTokens] = pricing.PerMillion(usage.CacheReadTokens, dollars(cacheRead))
	}
	if cacheWrite != "" {
		rates[usage.CacheWriteTokens] = pricing.PerMillion(usage.CacheWriteTokens, dollars(cacheWrite))
	}
	return pricing.Price{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: modelID,
		Rates:           rates,
		Provenance: pricing.Provenance{
			Source:        Source,
			Version:       FixtureVersion,
			EffectiveFrom: effectiveFrom,
			Currency:      "USD",
		},
	}
}

// Bedrock returns fixture prices for a few Bedrock models, including the
// cross-region variants, since the geography prefix is part of the model ID the
// bill refers to.
func Bedrock() []pricing.Price {
	type entry struct {
		id                             string
		in, out, cacheRead, cacheWrite string
	}
	entries := []entry{
		{"anthropic.claude-sonnet-4-5-20250929-v1:0", "3.00", "15.00", "0.30", "3.75"},
		{"anthropic.claude-haiku-4-5-20251001-v1:0", "1.00", "5.00", "0.10", "1.25"},
		{"amazon.nova-pro-v1:0", "0.80", "3.20", "0.20", ""},
		{"amazon.nova-lite-v1:0", "0.06", "0.24", "0.015", ""},
	}
	out := make([]pricing.Price, 0, len(entries)*2)
	for _, e := range entries {
		out = append(out, price(e.id, e.in, e.out, e.cacheRead, e.cacheWrite))
		// Cross-region inference profiles bill at the same rate but under a
		// distinct model ID, and pricing keys on the ID verbatim.
		for _, geo := range []string{"us.", "eu.", "apac."} {
			out = append(out, price(geo+e.id, e.in, e.out, e.cacheRead, e.cacheWrite))
		}
	}
	return out
}

// AgentCoreRuntimeModelID is the pseudo-model ID that AgentCore Runtime resource
// consumption is priced under.
//
// Runtime CPU and memory are billed against a hosted runtime, not against a
// foundation model, so there is no real model ID to key on. Pricing keys on
// (access provider, provider model ID), and rather than widen that key for one
// dimension, the resource is named as what the bill calls it. It is not a model and
// nothing treats it as one: no identity built from it claims a publisher or a
// canonical model.
const AgentCoreRuntimeModelID = "agentcore-runtime"

// agentCoreRuntime returns the per-region Runtime resource prices.
//
// Rates are published per vCPU-hour and per GB-hour while usage is normalized to
// nano-units of each, so PerNanoUnit carries the scale as an exact denominator.
//
// Regions are listed explicitly rather than defaulted, because they genuinely
// differ: GovCloud is dearer, and a region-less fallback entry would silently price
// it at the commercial rate. A region absent here is unpriced, which is the correct
// outcome -- and the honest one for sa-east-1, where AWS publishes no Runtime
// consumption rate at all.
func agentCoreRuntime() []pricing.Price {
	type entry struct {
		regions        []string
		vcpuHour, gbHr string
	}
	entries := []entry{
		{
			regions: []string{
				"us-east-1", "us-east-2", "us-west-2", "ca-central-1",
				"eu-west-1", "eu-west-2", "eu-west-3", "eu-central-1", "eu-north-1",
				"ap-northeast-1", "ap-northeast-2", "ap-south-1",
				"ap-southeast-1", "ap-southeast-2",
			},
			vcpuHour: "0.0895", gbHr: "0.00945",
		},
		{regions: []string{"us-gov-west-1"}, vcpuHour: "0.1074", gbHr: "0.01134"},
	}

	out := make([]pricing.Price, 0, 16)
	for _, e := range entries {
		for _, region := range e.regions {
			out = append(out, pricing.Price{
				AccessProvider:  "aws-bedrock",
				ProviderModelID: AgentCoreRuntimeModelID,
				Region:          region,
				Rates: map[usage.Dimension]pricing.Rate{
					usage.RuntimeVCPUNanoHours:     pricing.PerNanoUnit(usage.RuntimeVCPUNanoHours, dollars(e.vcpuHour)),
					usage.RuntimeMemoryNanoGBHours: pricing.PerNanoUnit(usage.RuntimeMemoryNanoGBHours, dollars(e.gbHr)),
				},
				Provenance: pricing.Provenance{
					Source:        Source,
					Version:       FixtureVersion,
					EffectiveFrom: agentCoreEffectiveFrom,
					Currency:      "USD",
				},
			})
		}
	}
	return out
}

// agentCoreEffectiveFrom is the effective date AWS publishes for these rates, kept
// distinct from the token fixtures' backdated one: this figure is real provenance
// rather than a convenience for tests.
var agentCoreEffectiveFrom = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// Catalog returns a static catalog of every fixture in this package: the Bedrock
// models, AgentCore Runtime resource rates, and the OpenAI models.
//
// One catalog across access providers rather than one per provider, because that is
// how a real deployment reaches more than one: prices key on (access provider,
// provider model ID), so entries for different providers cannot collide, and
// nothing that enumerates the catalog is confused by the mixture -- pricing.CaptureSet
// filters by access provider before capturing.
func Catalog() (*pricing.Static, error) {
	prices := make([]pricing.Price, 0, 64)
	prices = append(prices, Bedrock()...)
	prices = append(prices, agentCoreRuntime()...)
	prices = append(prices, OpenAI()...)
	return pricing.NewStatic(prices...)
}
