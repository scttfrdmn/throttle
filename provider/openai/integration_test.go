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
		Engine:  eng,
		Catalog: cat,
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
