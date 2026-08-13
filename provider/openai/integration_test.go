//go:build integration

// This file is behind a build tag because it talks to real OpenAI and spends real
// money. The default `go test ./...` never builds it, so the suite needs no
// credentials, no network, and no OpenAI account.
//
// It also requires an explicit opt-in flag on top of the build tag. A build tag alone
// would mean that anyone running `go test -tags=integration ./...` for the Bedrock
// suite silently starts paying OpenAI too. Spending someone's money should take a
// deliberate act, so:
//
//	go test -tags=integration ./provider/openai/ \
//	    -throttle.openai.spend -throttle.openai.model=gpt-5-mini
//
// It requires OPENAI_API_KEY in the environment -- resolved by the official SDK, not
// by throttle -- and it will incur a genuine (tiny) charge. Without the flag or
// without credentials it skips cleanly.
package openai_test

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing/fixtures"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/usage"
)

var (
	// spendMoney is the explicit opt-in. Defaulting to false means the build tag alone
	// cannot cost anybody anything.
	spendMoney = flag.Bool("throttle.openai.spend", false,
		"actually call the OpenAI API, which costs real money")

	integrationModel = flag.String("throttle.openai.model", "gpt-5-mini",
		"OpenAI model to invoke in the integration test")
)

// integrationClient builds a governed client against real OpenAI on a deliberately
// tiny budget, so a runaway loop in these tests cannot spend meaningfully.
//
// Note what is absent: nothing here reads, sets, or logs an API key. The SDK's
// NewClient resolves OPENAI_API_KEY from the environment on its own, which is the whole
// reason throttle does not need to handle the credential at all.
func integrationClient(t *testing.T) *openai.Client {
	t.Helper()

	if !*spendMoney {
		t.Skip("skipping: pass -throttle.openai.spend to make real, billable OpenAI calls")
	}
	// Checked, never read into a variable and never printed.
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("skipping: OPENAI_API_KEY is not set")
	}

	ctx := context.Background()
	client := oai.NewClient()

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
	if err := eng.Register(ctx, budget.Definition{
		ID:         "integration",
		Allocation: limit,
		Recurrence: budget.RecurMonthly,
		AnchorAt:   time.Now().UTC().Truncate(24 * time.Hour),
	}, engine.ModeEnforce); err != nil {
		t.Fatalf("Register: %v", err)
	}

	cat, err := fixtures.Catalog()
	if err != nil {
		t.Fatalf("fixtures.Catalog: %v", err)
	}

	c, err := openai.New(openai.Config{
		Client:  openai.Responses(&client),
		Counter: openai.Counter(&client),
		// Wired here rather than in a second constructor so the streaming test governs
		// the same budget, the same catalog, and the same ledger as the non-streaming one.
		StreamClient: openai.Streaming(&client),
		// Both API families against one budget, one engine, one ledger, and one catalog.
		// That is the arrangement #28 claims is possible, and configuring it here is the
		// only place the claim is made against the real SDK's own service values rather
		// than against a fake.
		ChatClient: openai.ChatCompletions(&client),
		Engine:     eng,
		Catalog:    cat,
	})
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	return c
}

// TestIntegrationRespond exercises the governed path against real OpenAI.
//
// Its purpose is to confirm what a fake cannot: that the shapes this adapter depends on
// are what the live service actually returns. Specifically, that a Response carries
// usage at all, that its input_tokens really is inclusive of the cached breakdown, that
// service_tier comes back populated, and that the input-count endpoint exists and
// answers.
func TestIntegrationRespond(t *testing.T) {
	ctx := context.Background()
	client := integrationClient(t)

	res, err := client.Respond(ctx, openai.Request{
		BudgetID: "integration",
		Params: responses.ResponseNewParams{
			Model: shared.ResponsesModel(*integrationModel),
			Input: responses.ResponseNewParamsInputUnion{
				OfString: param.NewOpt("Reply with exactly the word: ok"),
			},
			MaxOutputTokens: param.NewOpt(int64(16)),
		},
		Metadata: map[string]string{"workload": "throttle-integration-test"},
	})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}

	// The live service must report usage; the whole accounting model depends on it.
	if res.Usage.Count(usage.InputTokens) == 0 {
		t.Error("OpenAI reported no input tokens")
	}
	if res.Usage.Count(usage.OutputTokens) == 0 && res.Usage.Count(usage.ReasoningTokens) == 0 {
		t.Error("OpenAI reported no output or reasoning tokens")
	}
	if !res.Settled {
		t.Fatalf("the request did not settle; cost = %s (%s)", res.Cost, res.Cost.Reason)
	}
	if res.Charge.ActualCost <= 0 {
		t.Errorf("ActualCost = %s, want a positive charge", res.Charge.ActualCost)
	}
	if res.Response == nil || res.Response.ID == "" {
		t.Error("the live response should carry an ID")
	}

	// The tier the service reports is what settlement priced on. Logging it is how a
	// change in OpenAI's tier behaviour would surface here.
	t.Logf("model requested %q, served %q, tier %q, usage %v, cost %s",
		res.Identity.ProviderModelID, res.ServedModelID, res.Identity.ServiceTier,
		res.Usage, res.Charge.ActualCost)
}

// TestIntegrationRespondStreaming exercises the governed streaming path against real
// OpenAI, behind the same double gate: the build tag plus -throttle.openai.spend.
//
// It exists to confirm the things only the live service can confirm, all of which the
// streaming accounting model depends on and none of which a fake can establish:
//
//   - A real stream really does deliver a terminal Response, and its status is one of
//     the four the terminality check recognizes. The whole lifecycle hinges on this.
//   - That terminal Response carries usage. If OpenAI ever stopped including it,
//     throttle would be correct to leave holds outstanding, but every streaming request
//     would become unresolvable and this test is where that would show up first.
//   - The terminal event is the last one, or near it. If usage arrived only after
//     several more events, settling on the terminal Response would still be right, but
//     the observation is worth having.
//   - service_tier comes back populated on a stream, exactly as it does on a single
//     round trip, since #30's rule prices on the tier that actually served the call.
//
// Deliberately not asserted: the number or kind of intermediate events. Those are
// OpenAI's business, and throttle forwards what it does not understand.
func TestIntegrationRespondStreaming(t *testing.T) {
	ctx := context.Background()
	client := integrationClient(t)

	s, err := client.RespondStreaming(ctx, openai.StreamRequest{
		BudgetID: "integration",
		Params: responses.ResponseNewParams{
			Model: shared.ResponsesModel(*integrationModel),
			Input: responses.ResponseNewParamsInputUnion{
				OfString: param.NewOpt("Count from one to five, one number per line."),
			},
			MaxOutputTokens: param.NewOpt(int64(64)),
		},
		Metadata: map[string]string{"workload": "throttle-integration-stream"},
	})
	if err != nil {
		t.Fatalf("RespondStreaming: %v", err)
	}
	defer s.Close()

	// The caller's own read loop, exactly as the docs describe it. Events are counted and
	// their types noted; nothing is accumulated, because there is nothing throttle would
	// do with the text.
	var events, terminals int
	var lastType string
	var afterTerminal int
	for s.Next() {
		ev := s.Current()
		events++
		lastType = ev.Type
		if ev.JSON.Response.Valid() {
			switch ev.Response.Status {
			case responses.ResponseStatusCompleted, responses.ResponseStatusIncomplete,
				responses.ResponseStatusFailed, responses.ResponseStatusCancelled:
				terminals++
			}
		}
		if terminals > 0 {
			afterTerminal++
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if events == 0 {
		t.Fatal("the live stream delivered no events at all")
	}
	if terminals == 0 {
		t.Fatalf("the live stream delivered %d events but no terminal Response (last type %q): "+
			"throttle cannot settle a stream that never reports one", events, lastType)
	}
	if terminals > 1 {
		t.Errorf("the live stream carried %d terminal Responses; throttle accounts on the first", terminals)
	}

	res := s.Result()
	if res == nil {
		t.Fatal("the stream reached no terminal state")
	}
	if !res.Settled {
		t.Fatalf("the streamed request did not settle; cost = %s (%s)", res.Cost.Amount, res.Cost.Reason)
	}
	if res.Charge.ActualCost <= 0 {
		t.Errorf("ActualCost = %s, want a positive charge", res.Charge.ActualCost)
	}
	if res.Usage.Count(usage.InputTokens) == 0 {
		t.Error("the terminal Response reported no input tokens")
	}
	if res.Usage.Count(usage.OutputTokens) == 0 && res.Usage.Count(usage.ReasoningTokens) == 0 {
		t.Error("the terminal Response reported no output or reasoning tokens")
	}
	if res.Identity.ServiceTier == "" {
		t.Error("the terminal Response reported no service tier, so #30's pricing rule had nothing to price on")
	}

	// Logged rather than asserted: how many events trailed the terminal Response is
	// OpenAI's choice, and this is the record of what it currently does.
	t.Logf("%d events, %d after the terminal Response (last type %q), served %q, tier %q, usage %v, cost %s",
		events, afterTerminal-1, lastType, res.ServedModelID, res.Identity.ServiceTier,
		res.Usage, res.Charge.ActualCost)
}

// TestIntegrationComplete exercises the governed Chat Completions path against real
// OpenAI, through the same client, budget, and catalog as the Responses tests above.
//
// It confirms the things about this API family that only the live service can confirm,
// and that a fake cannot establish because the fake is written from the same reading of
// the documentation as the adapter:
//
//   - A real completion carries usage at all, since settlement depends on it.
//   - prompt_tokens really is inclusive of its cached detail, and completion_tokens of
//     its reasoning detail. The normalization subtracts on that assumption, and if the
//     relationship were additive instead, throttle would undercount the text portion --
//     or, if the subtraction went negative, refuse to settle. Either failure surfaces
//     here first. Asserted as an inequality rather than by reproducing the arithmetic,
//     because the point is the inclusion relationship and not a particular count.
//   - The two families reach the same accounting through one engine and one ledger.
//     Spend from the Responses tests and from this one accumulate in one budget; if the
//     two had somehow acquired separate bookkeeping, one of the two would not be here.
//   - service_tier comes back populated, since #30 prices on the tier that served.
//
// Deliberately not exercised: audio. The fixture catalog carries no audio-token rates, so
// an audio request is expected to be refused before execution under enforcement -- which
// is behaviour the offline suite pins, and making a live audio call to observe it would
// spend money to learn nothing new.
func TestIntegrationComplete(t *testing.T) {
	ctx := context.Background()
	client := integrationClient(t)

	res, err := client.Complete(ctx, openai.ChatRequest{
		BudgetID: "integration",
		Params: oai.ChatCompletionNewParams{
			Model: shared.ChatModel(*integrationModel),
			Messages: []oai.ChatCompletionMessageParamUnion{
				oai.UserMessage("Reply with exactly the word: ok"),
			},
			MaxCompletionTokens: param.NewOpt(int64(16)),
		},
		Metadata: map[string]string{"workload": "throttle-integration-chat"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Completion == nil || res.Completion.ID == "" {
		t.Error("the live completion should carry an ID")
	}
	if res.Usage.Count(usage.InputTokens) == 0 {
		t.Error("OpenAI reported no input tokens for a Chat Completions request")
	}
	if res.Usage.Count(usage.OutputTokens) == 0 && res.Usage.Count(usage.ReasoningTokens) == 0 {
		t.Error("OpenAI reported no output or reasoning tokens")
	}
	if !res.Settled {
		t.Fatalf("the request did not settle; cost = %s (%s)", res.Cost.Amount, res.Cost.Reason)
	}
	if res.Charge.ActualCost <= 0 {
		t.Errorf("ActualCost = %s, want a positive charge", res.Charge.ActualCost)
	}
	if res.Identity.Operation != openai.OperationChatCompletions {
		t.Errorf("Operation = %q, want %q", res.Identity.Operation, openai.OperationChatCompletions)
	}

	// The inclusion relationship, checked against the raw usage object rather than the
	// normalized one, because the normalization is the thing under test.
	raw := res.Completion.Usage
	if raw.PromptTokens == 0 {
		t.Error("the live usage object reported no prompt_tokens")
	}
	if d := raw.PromptTokensDetails; d.CachedTokens > raw.PromptTokens {
		t.Errorf("cached_tokens (%d) exceeds prompt_tokens (%d): the detail counters are not "+
			"inclusive of the total, and throttle's subtractive normalization is wrong",
			d.CachedTokens, raw.PromptTokens)
	}
	if d := raw.CompletionTokensDetails; d.ReasoningTokens > raw.CompletionTokens {
		t.Errorf("reasoning_tokens (%d) exceeds completion_tokens (%d): the detail counters are "+
			"not inclusive of the total", d.ReasoningTokens, raw.CompletionTokens)
	}
	// And the normalized text figure is what is left after the details, never negative and
	// never larger than the total the provider reported.
	if got := res.Usage.Count(usage.InputTokens); got > raw.PromptTokens {
		t.Errorf("normalized input_tokens (%d) exceeds the reported prompt_tokens (%d)",
			got, raw.PromptTokens)
	}

	t.Logf("model requested %q, served %q, tier %q, usage %v, cost %s",
		res.Identity.ProviderModelID, res.ServedModelID, res.Identity.ServiceTier,
		res.Usage, res.Charge.ActualCost)
}

// TestIntegrationCount confirms the preflight input-count endpoint exists and answers
// for a real request, which is the only way to know the estimate path is more than
// theoretical.
func TestIntegrationCount(t *testing.T) {
	ctx := context.Background()
	client := integrationClient(t)

	est, err := client.Estimate(ctx, responses.ResponseNewParams{
		Model: shared.ResponsesModel(*integrationModel),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: param.NewOpt("Reply with exactly the word: ok"),
		},
		MaxOutputTokens: param.NewOpt(int64(16)),
	})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Usage.Count(usage.InputTokens) == 0 {
		t.Error("the count endpoint reported no input tokens")
	}
	// Counted, but still not exact: OpenAI does not document this count as the billed
	// count, and the output half is a ceiling either way.
	if est.Quality == usage.QualityExact {
		t.Error("a counted estimate must not be labelled exact")
	}
	t.Logf("estimate: %v, quality %s, cost %s, note: %s",
		est.Usage, est.Quality, est.Cost.Amount, est.Note)
}
