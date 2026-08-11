//go:build integration

// This file is behind a build tag because it talks to real AWS and spends real
// money. The default `go test ./...` never builds it, so the suite needs no
// credentials, no network, and no AWS account.
//
// To run it:
//
//	go test -tags=integration ./provider/bedrock/ \
//	    -throttle.model=anthropic.claude-haiku-4-5-20251001-v1:0
//
// It requires ambient AWS credentials with bedrock:InvokeModel, and it will incur
// a genuine (tiny) charge.
package bedrock_test

import (
	"context"
	"flag"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"throttle/budget"
	"throttle/engine"
	"throttle/ledger/sqlite"
	"throttle/money"
	"throttle/pricing/fixtures"
	"throttle/provider/bedrock"
	"throttle/usage"
)

var (
	integrationModel = flag.String("throttle.model", "anthropic.claude-haiku-4-5-20251001-v1:0",
		"Bedrock model id to invoke in the integration test")
	integrationRegion = flag.String("throttle.region", "us-east-1",
		"AWS region for the integration test")
)

// integrationClient builds a governed client against real Bedrock on a deliberately
// tiny budget, so a runaway loop in these tests cannot spend meaningfully.
func integrationClient(t *testing.T) *bedrock.Client {
	t.Helper()
	ctx := context.Background()

	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*integrationRegion))
	if err != nil {
		t.Fatalf("loading AWS config: %v", err)
	}
	brClient := bedrockruntime.NewFromConfig(awsCfg)

	store, err := sqlite.Open(ctx, t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	eng, err := engine.New(engine.Config{Ledger: store})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	limit, err := money.Parse("1.00")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	def := budget.Definition{
		ID:         "integration",
		Allocation: limit,
		Recurrence: budget.RecurMonthly,
		AnchorAt:   time.Now().UTC().Truncate(24 * time.Hour),
	}
	if err := eng.Register(ctx, def, engine.ModeEnforce); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cat, err := fixtures.Catalog()
	if err != nil {
		t.Fatalf("fixtures.Catalog: %v", err)
	}

	client, err := bedrock.New(bedrock.Config{
		Client:  brClient,
		Counter: brClient, // the real client satisfies both interfaces
		Engine:  eng,
		Catalog: cat,
		Region:  *integrationRegion,
		// The streaming half needs the adapter: ConverseStreamOutput hides its
		// stream, so the raw client cannot satisfy ConverseStreamAPI directly.
		StreamClient: bedrock.Streaming(brClient),
	})
	if err != nil {
		t.Fatalf("bedrock.New: %v", err)
	}
	return client
}

// TestIntegrationConverse exercises the governed path against real Bedrock.
//
// Its purpose is to confirm what a fake cannot: that the SDK shapes this adapter
// depends on -- TokenUsage, ConverseMetrics, ServiceTier, CountTokens -- are what
// the live service actually returns.
func TestIntegrationConverse(t *testing.T) {
	ctx := context.Background()
	client := integrationClient(t)

	res, err := client.Converse(ctx, bedrock.Request{
		BudgetID: "integration",
		Input: &bedrockruntime.ConverseInput{
			ModelId: aws.String(*integrationModel),
			Messages: []brtypes.Message{{
				Role: brtypes.ConversationRoleUser,
				Content: []brtypes.ContentBlock{
					&brtypes.ContentBlockMemberText{Value: "Reply with exactly the word: ok"},
				},
			}},
			InferenceConfig: &brtypes.InferenceConfiguration{MaxTokens: aws.Int32(16)},
		},
		Metadata: map[string]string{"workload": "throttle-integration-test"},
	})
	if err != nil {
		t.Fatalf("Converse: %v", err)
	}

	// The live service must report usage; the whole accounting model depends on it.
	if res.Usage.Count(usage.InputTokens) == 0 {
		t.Error("Bedrock reported no input tokens")
	}
	if res.Usage.Count(usage.OutputTokens) == 0 {
		t.Error("Bedrock reported no output tokens")
	}
	if !res.Settled {
		t.Fatalf("the request did not settle; cost = %s (%s)", res.Cost, res.Cost.Reason)
	}
	if res.Charge.ActualCost <= 0 {
		t.Errorf("ActualCost = %s, want a positive charge", res.Charge.ActualCost)
	}
	// A capped request should come in under its reservation.
	if res.Charge.Overrun() != 0 {
		t.Errorf("unexpected overrun of %s against a capped request", res.Charge.Overrun())
	}
	if res.Charge.Usage.ProviderLatency <= 0 {
		t.Error("Bedrock reported no latency in ConverseMetrics")
	}

	// The estimate must not have claimed to be exact, even against the real
	// CountTokens API.
	if res.Estimate.Quality == usage.QualityExact {
		t.Error("no Converse estimate may claim to be exact")
	}

	t.Logf("model=%s estimate=%s (%s) actual=%s latency=%v",
		res.Identity.Describe(), res.Estimate.Cost.Amount, res.Estimate.Quality,
		res.Charge.ActualCost, res.Charge.Usage.ProviderLatency)
}

// TestIntegrationConverseStream is the streaming counterpart, and it exists to
// confirm the one thing no fake can: that live Bedrock really does deliver a
// terminal metadata event carrying usage, on the event types this adapter switches
// on. Every accounting decision in the streaming path rests on that.
func TestIntegrationConverseStream(t *testing.T) {
	ctx := context.Background()
	client := integrationClient(t)

	s, err := client.ConverseStream(ctx, bedrock.StreamRequest{
		BudgetID: "integration",
		Input: &bedrockruntime.ConverseStreamInput{
			ModelId: aws.String(*integrationModel),
			Messages: []brtypes.Message{{
				Role: brtypes.ConversationRoleUser,
				Content: []brtypes.ContentBlock{
					&brtypes.ContentBlockMemberText{Value: "Count from one to five, one word per line."},
				},
			}},
			InferenceConfig: &brtypes.InferenceConfiguration{MaxTokens: aws.Int32(64)},
		},
		Metadata: map[string]string{"workload": "throttle-integration-test"},
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}

	var events, deltas, metadataEvents int
	for ev := range s.Events() {
		events++
		switch ev.(type) {
		case *brtypes.ConverseStreamOutputMemberContentBlockDelta:
			deltas++
		case *brtypes.ConverseStreamOutputMemberMetadata:
			metadataEvents++
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if deltas == 0 {
		t.Error("the live stream delivered no content deltas")
	}
	// The load-bearing assertion: usage arrives exactly once, at the end.
	if metadataEvents != 1 {
		t.Errorf("%d metadata events, want exactly 1 carrying usage", metadataEvents)
	}

	res := s.Result()
	if res == nil {
		t.Fatal("a terminal stream must have a result")
	}
	if !res.Settled {
		t.Fatalf("the stream did not settle; cost = %s (%s)", res.Cost, res.Cost.Reason)
	}
	if res.Usage.Count(usage.InputTokens) == 0 || res.Usage.Count(usage.OutputTokens) == 0 {
		t.Errorf("usage = %s, want both input and output tokens from the metadata event", res.Usage)
	}
	if res.Charge.ActualCost <= 0 {
		t.Errorf("ActualCost = %s, want a positive charge", res.Charge.ActualCost)
	}
	if res.Identity.Operation != bedrock.OperationConverseStream {
		t.Errorf("Operation = %q, want %q", res.Identity.Operation, bedrock.OperationConverseStream)
	}

	t.Logf("model=%s events=%d deltas=%d estimate=%s actual=%s usage=%s",
		res.Identity.Describe(), events, deltas, res.Estimate.Cost.Amount,
		res.Charge.ActualCost, res.Usage)
}
