//go:build integration

// This file is behind a build tag because it talks to real Anthropic and spends real
// money. The default `go test ./...` never builds it, so the suite needs no credentials,
// no network, and no Anthropic account.
//
// It also requires an explicit opt-in flag on top of the build tag. A build tag alone
// would mean that anyone running `go test -tags=integration ./...` for the Bedrock or
// OpenAI suites silently starts paying Anthropic too. Spending someone's money should
// take a deliberate act, so:
//
//	go test -tags=integration ./provider/anthropic/ \
//	    -throttle.anthropic.spend -throttle.anthropic.model=claude-haiku-4-5
//
// The gate covers the token-counting endpoint as well as Messages. Anthropic's own
// documentation does not state that count_tokens is free, and an undocumented assumption
// about somebody's bill is not a licence to make a network call: both live paths sit
// behind the same flag until that is proven from a primary source.
//
// It requires an Anthropic credential in the environment -- resolved by the official SDK
// through its own chain, not by throttle -- and it will incur a genuine (tiny) charge.
// Without the flag or without credentials it skips cleanly.
package anthropic_test

import (
	"context"
	"flag"
	"os"
	"testing"
	"time"

	anth "github.com/anthropics/anthropic-sdk-go"

	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing/fixtures"
	anthropic "github.com/scttfrdmn/throttle/provider/anthropic"
	"github.com/scttfrdmn/throttle/usage"
)

var (
	// spendMoney is the explicit opt-in. Defaulting to false means the build tag alone
	// cannot cost anybody anything.
	spendMoney = flag.Bool("throttle.anthropic.spend", false,
		"actually call the Anthropic API, which costs real money")

	integrationModel = flag.String("throttle.anthropic.model", "claude-haiku-4-5",
		"Anthropic model to invoke in the integration test")
)

// integrationClient builds a governed client against real Anthropic on a deliberately
// tiny budget, so a runaway loop in these tests cannot spend meaningfully.
//
// Note what is absent: nothing here reads, sets, or logs an API key, an auth token, a
// profile, or an identity token. anth.NewClient() resolves its own credential chain, which
// is the whole reason throttle does not need to handle any of it. The one environment read
// below is a presence check whose value is never captured.
func integrationClient(t *testing.T) *anthropic.Client {
	t.Helper()

	if !*spendMoney {
		t.Skip("skipping: pass -throttle.anthropic.spend to make real, billable Anthropic calls")
	}
	// Checked, never read into a variable and never printed. Any one of the SDK's
	// credential sources is enough; this covers the common two without throttle taking on
	// the job of resolving the chain.
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		t.Skip("skipping: no Anthropic credential is set in the environment")
	}

	ctx := context.Background()
	client := anth.NewClient()

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

	c, err := anthropic.New(anthropic.Config{
		Client: anthropic.Messages(&client),
		// The counter is wired here because the live count endpoint is one of the things
		// only a real call can confirm exists and answers. It is behind the same gate.
		Counter: anthropic.Counter(&client),
		Engine:  eng,
		Catalog: cat,
	})
	if err != nil {
		t.Fatalf("anthropic.New: %v", err)
	}
	return c
}

// TestIntegrationNewMessage exercises the governed path against real Anthropic.
//
// Its purpose is to confirm what a fake cannot: that the shapes this adapter depends on
// are what the live service actually returns. Above all the additive identity, which is
// the single assumption the whole Anthropic accounting model rests on and the one that
// differs from OpenAI:
//
//	total input = input_tokens + cache_creation_input_tokens + cache_read_input_tokens
//
// A fake can only confirm that throttle implements what the documentation says. This
// confirms the documentation.
func TestIntegrationNewMessage(t *testing.T) {
	ctx := context.Background()
	client := integrationClient(t)

	res, err := client.NewMessage(ctx, anthropic.Request{
		BudgetID: "integration",
		Params: anth.MessageNewParams{
			Model:     anth.Model(*integrationModel),
			MaxTokens: 16,
			Messages: []anth.MessageParam{{
				Role: anth.MessageParamRoleUser,
				Content: []anth.ContentBlockParamUnion{
					{OfText: &anth.TextBlockParam{Text: "Reply with exactly the word: ok"}},
				},
			}},
		},
		Metadata: map[string]string{"workload": "throttle-integration-test"},
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	// The live service must report usage; the whole accounting model depends on it.
	if res.Usage.Count(usage.InputTokens) == 0 {
		t.Error("Anthropic reported no input tokens")
	}
	if res.Usage.Count(usage.OutputTokens) == 0 {
		t.Error("Anthropic reported no output tokens")
	}
	if !res.Settled {
		t.Fatalf("the request did not settle; cost = %s (%s)", res.Cost, res.Cost.Reason)
	}
	if res.Charge.ActualCost <= 0 {
		t.Errorf("ActualCost = %s, want a positive charge", res.Charge.ActualCost)
	}
	if res.Message == nil || res.Message.ID == "" {
		t.Error("the live response should carry a message ID")
	}

	// The additive identity, against the raw SDK object rather than against throttle's
	// normalization -- otherwise this would be asserting that the adapter agrees with
	// itself. If Anthropic ever made input_tokens inclusive of the cache counters the way
	// OpenAI's is, this is where it would surface, and the consequence would be an
	// overcharge on every cached request.
	u := res.Message.Usage
	if got, want := res.Usage.Count(usage.InputTokens), u.InputTokens; got != want {
		t.Errorf("normalized InputTokens = %d, want the raw %d unmodified: Anthropic's counters "+
			"are disjoint and nothing should be subtracted from the fresh-input figure", got, want)
	}
	t.Logf("live usage: input=%d cache_creation=%d (5m=%d 1h=%d) cache_read=%d output=%d "+
		"(thinking=%d) total_input=%d",
		u.InputTokens, u.CacheCreationInputTokens,
		u.CacheCreation.Ephemeral5mInputTokens, u.CacheCreation.Ephemeral1hInputTokens,
		u.CacheReadInputTokens, u.OutputTokens, u.OutputTokensDetails.ThinkingTokens,
		u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens)

	// The TTL-specific children must decompose their aggregate exactly, because throttle
	// prices the children and never the aggregate. If they ever stopped summing, pricing
	// the children would silently undercharge the difference.
	if sum := u.CacheCreation.Ephemeral5mInputTokens + u.CacheCreation.Ephemeral1hInputTokens; //
	u.CacheCreationInputTokens != sum {
		t.Errorf("cache_creation_input_tokens = %d but its TTL children sum to %d: throttle prices "+
			"the children, so a gap here is spend it would never see",
			u.CacheCreationInputTokens, sum)
	}

	// Any usage field the SDK did not model is worth knowing about: an unmodelled counter
	// could be a billable dimension, and throttle's rule is that an unknown dimension makes
	// cost completeness unknown rather than passing through as fully priced.
	if extra := u.JSON.ExtraFields; len(extra) > 0 {
		for name := range extra {
			t.Logf("the live usage object carries a field the SDK does not model: %q", name)
		}
	}

	// Identity, logged rather than asserted where the service is free to choose: the served
	// model may be a dated build of the alias that was requested, and the tier and geography
	// are the service's to assign.
	t.Logf("model requested %q, served %q, tier %q, geo %q, stop %q, cost %s",
		res.Identity.ProviderModelID, res.ServedModelID, res.Identity.ServiceTier,
		res.Identity.InferenceGeo, res.Message.StopReason, res.Charge.ActualCost)
	if res.ServedModelID == "" {
		t.Error("the live response should name the model that served it")
	}
}

// TestIntegrationCountTokens confirms the preflight count endpoint exists, answers, and
// returns a figure in the neighbourhood of what the same request then actually bills.
//
// "Neighbourhood" is deliberate. throttle classifies this endpoint's answer as
// conservative rather than exact, because Anthropic does not document it as the billed
// count, and this test asserts only what that classification claims: a positive figure
// that is close enough to be useful for admission. Asserting equality would be encoding
// a guarantee the provider has not made.
func TestIntegrationCountTokens(t *testing.T) {
	ctx := context.Background()
	client := integrationClient(t)

	in := anth.MessageNewParams{
		Model:     anth.Model(*integrationModel),
		MaxTokens: 16,
		Messages: []anth.MessageParam{{
			Role: anth.MessageParamRoleUser,
			Content: []anth.ContentBlockParamUnion{
				{OfText: &anth.TextBlockParam{Text: "Reply with exactly the word: ok"}},
			},
		}},
	}

	est, err := client.Estimate(ctx, in)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	counted := est.Usage.Count(usage.InputTokens)
	if counted <= 0 {
		t.Fatalf("the live count endpoint reported %d input tokens", counted)
	}
	if est.Quality == usage.QualityExact {
		t.Error("the count endpoint must not be classified exact: Anthropic does not document it " +
			"as equivalent to the billed input count")
	}

	res, err := client.NewMessage(ctx, anthropic.Request{
		BudgetID: "integration", Params: in,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	billed := res.Usage.Count(usage.InputTokens)
	t.Logf("count endpoint said %d input tokens, the request billed %d", counted, billed)
	if billed <= 0 {
		t.Fatal("the request billed no input tokens, so there is nothing to compare against")
	}
	// A count useless for admission would be one off by an order of magnitude. That is the
	// claim being tested, not equality.
	if counted*4 < billed || billed*4 < counted {
		t.Errorf("the count endpoint said %d and the request billed %d: an estimate that far out "+
			"cannot govern an envelope", counted, billed)
	}
}
