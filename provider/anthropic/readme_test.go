package anthropic_test

import (
	"context"

	anth "github.com/anthropics/anthropic-sdk-go"

	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/pricing/fixtures"
	anthropic "github.com/scttfrdmn/throttle/provider/anthropic"
)

// The README's direct Anthropic example, compiled.
//
// Same reasoning as provider/openai/readme_test.go and provider/bedrock/readme_test.go: a
// getting-started example that no longer builds is worse than none, because it is the first
// code a new user copies and the first thing they conclude is broken is throttle. This is an
// Example function with no // Output: comment, so the toolchain type-checks it against the
// real SDK on every "go test" and never dials Anthropic. Nothing here reads a credential.
//
// Keep it in step with README.md. If a signature here has to change, the README has to
// change with it -- that is the whole point of the file.
func ExampleClient_NewMessage_readme() {
	ctx := context.Background()

	// Normal Anthropic SDK configuration: NewClient resolves its own credential chain from
	// the environment. throttle holds no credential and reads none.
	client := anth.NewClient()

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

	governed, err := anthropic.New(anthropic.Config{
		Client:  anthropic.Messages(&client),
		Counter: anthropic.Counter(&client), // optional: preflight token counts
		Engine:  eng,
		Catalog: cat,
	})
	if err != nil {
		return
	}

	res, err := governed.NewMessage(ctx, anthropic.Request{
		BudgetID: "agents", // a budget id from the config file
		Params: anth.MessageNewParams{ // the SDK's own params, sent verbatim
			Model:     anth.ModelClaudeOpus5,
			MaxTokens: 1024,
			Messages: []anth.MessageParam{
				anth.NewUserMessage(anth.NewTextBlock("Summarize this in one sentence.")),
			},
		},
	})
	if err != nil {
		return
	}
	// res.Message is Anthropic's own *anth.Message.
	// res.Estimate, res.Usage, res.Cost, and res.Charge are the accounting.
	_, _, _, _ = res.Message, res.Estimate, res.Cost, res.Charge
}
