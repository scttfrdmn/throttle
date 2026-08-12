package bedrock_test

import (
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/pricing/fixtures"
	"github.com/scttfrdmn/throttle/provider/bedrock"
)

// The README's integration examples, compiled.
//
// A getting-started example that no longer builds is worse than none: it is the first code a
// new user copies, and the first thing they conclude is broken is throttle. These are Example
// functions with no // Output: comment, so the toolchain type-checks them against the real
// API on every "go test" and never dials AWS.
//
// Keep these in step with README.md. If a signature here has to change, the README has to
// change with it -- that is the whole point of the file.

// The full Converse integration, as the README's first Go block presents it.
func ExampleClient_Converse_readme() {
	ctx := context.Background()

	// Normal AWS SDK configuration: profiles, environment, IMDS, SSO. throttle adds
	// nothing here and holds no credentials of its own.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return
	}

	led, err := sqlite.Open(ctx, "/path/to/ledger.db") // the store "throttle config apply" wrote
	if err != nil {
		return
	}
	defer led.Close()

	eng, err := engine.New(engine.Config{Ledger: led})
	if err != nil {
		return
	}
	cat, err := fixtures.Catalog() // versioned price fixtures, with provenance
	if err != nil {
		return
	}

	brc := bedrockruntime.NewFromConfig(awsCfg)
	client, err := bedrock.New(bedrock.Config{
		Client:  brc,
		Counter: brc, // optional: preflight token counts
		Engine:  eng,
		Catalog: cat,
		Region:  awsCfg.Region,
	})
	if err != nil {
		return
	}

	res, err := client.Converse(ctx, bedrock.Request{
		BudgetID: "agents", // a budget id from the config file
		Input:    &bedrockruntime.ConverseInput{ /* unchanged, passed through verbatim */ },
	})
	if err != nil {
		return
	}
	// res.Output is Bedrock's own *ConverseOutput.
	// res.Estimate, res.Usage, res.Cost, and res.Charge are the accounting.
	_, _, _, _ = res.Output, res.Estimate, res.Cost, res.Charge
}

// Streaming: the SDK's own event types, forwarded as they arrive.
func ExampleClient_ConverseStream_readme() {
	ctx := context.Background()
	awsCfg, eng, cat := readmePrerequisites(ctx)

	client, err := bedrock.New(bedrock.Config{
		Client:       bedrockruntime.NewFromConfig(awsCfg),
		Engine:       eng,
		Catalog:      cat,
		Region:       awsCfg.Region,
		StreamClient: bedrock.Streaming(bedrockruntime.NewFromConfig(awsCfg)),
	})
	if err != nil {
		return
	}

	stream, err := client.ConverseStream(ctx, bedrock.StreamRequest{
		BudgetID: "agents",
		Input:    &bedrockruntime.ConverseStreamInput{ /* passed through verbatim */ },
	})
	if err != nil {
		return
	}
	defer stream.Close()

	for ev := range stream.Events() { // Bedrock's own event types, as they arrive
		_ = ev
	}
	if err := stream.Close(); err != nil { // idempotent; settles, then reports
		return
	}
	_ = stream.Result() // the accounting, available once the stream is terminal.
}

// A managed agent turn: one governed transaction, many internal model invocations.
func ExampleClient_InvokeAgent_readme() {
	ctx := context.Background()
	awsCfg, eng, cat := readmePrerequisites(ctx)
	maxCost, err := money.Parse("$5.00")
	if err != nil {
		return
	}

	client, err := bedrock.New(bedrock.Config{
		Client:      bedrockruntime.NewFromConfig(awsCfg),
		Engine:      eng,
		Catalog:     cat,
		Region:      awsCfg.Region,
		AgentClient: bedrock.Agent(bedrockagentruntime.NewFromConfig(awsCfg)),
	})
	if err != nil {
		return
	}

	stream, err := client.InvokeAgent(ctx, bedrock.AgentRequest{
		BudgetID: "agents",
		Input:    &bedrockagentruntime.InvokeAgentInput{ /* passed through verbatim */ },
		MaxCost:  maxCost, // the declared ceiling, and the amount held
	})
	if err != nil {
		return
	}
	defer stream.Close()

	for ev := range stream.Events() { // chunks, traces, return-control -- all of them
		_ = ev
	}
	if err := stream.Close(); err != nil {
		return
	}
	// stream.Result().Cost is the whole turn. Result().Steps and Result().Agent.Steps
	// are the individual model invocations beneath it.
	res := stream.Result()
	_, _, _ = res.Cost, res.Steps, res.Agent.Steps
}

// A hosted runtime at the edge: admission and identity, with an opaque body forwarded.
func ExampleClient_InvokeAgentRuntime_readme() {
	ctx := context.Background()
	awsCfg, eng, cat := readmePrerequisites(ctx)
	maxExposure, err := money.Parse("$5.00")
	if err != nil {
		return
	}
	var w io.Writer = io.Discard

	client, err := bedrock.New(bedrock.Config{
		Client:        bedrockruntime.NewFromConfig(awsCfg),
		Engine:        eng,
		Catalog:       cat,
		Region:        awsCfg.Region,
		RuntimeClient: bedrockagentcore.NewFromConfig(awsCfg),
	})
	if err != nil {
		return
	}

	stream, err := client.InvokeAgentRuntime(ctx, bedrock.RuntimeRequest{
		BudgetID:    "agents",
		Input:       &bedrockagentcore.InvokeAgentRuntimeInput{ /* passed through verbatim */ },
		MaxExposure: maxExposure, // how much budget headroom to encumber
	})
	if err != nil {
		return
	}
	defer stream.Close()

	io.Copy(w, stream) // the runtime's own bytes, forwarded as they arrive
	if err := stream.Close(); err != nil {
		return
	}
}

// The one-line integration the README shows for an agent running inside AgentCore: hosting
// location does not change model-spend governance.
func ExampleClient_Converse_insideAgentCore() {
	ctx := context.Background()
	awsCfg, eng, cat := readmePrerequisites(ctx)
	client, err := bedrock.New(bedrock.Config{
		Client:  bedrockruntime.NewFromConfig(awsCfg),
		Engine:  eng,
		Catalog: cat,
		Region:  awsCfg.Region,
	})
	if err != nil {
		return
	}
	in := &bedrockruntime.ConverseInput{}

	// This is the whole integration. It runs unchanged in AgentCore, in Lambda, or on a laptop.
	res, err := client.Converse(ctx, bedrock.Request{BudgetID: "agents", Input: in})
	_, _ = res, err
}

// readmePrerequisites is the "...as above" the README's later examples elide, so each of them
// can show only the lines it is actually about.
func readmePrerequisites(ctx context.Context) (awsCfg aws.Config, eng *engine.Engine, cat *pricing.Static) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return
	}
	led, err := sqlite.Open(ctx, "/path/to/ledger.db")
	if err != nil {
		return
	}
	if eng, err = engine.New(engine.Config{Ledger: led}); err != nil {
		return
	}
	cat, _ = fixtures.Catalog()
	return
}
