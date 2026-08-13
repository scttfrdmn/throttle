package openai_test

import (
	"context"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/pricing/fixtures"
	openai "github.com/scttfrdmn/throttle/provider/openai"
)

// The README's OpenAI examples, compiled.
//
// Same reasoning as provider/bedrock/readme_test.go: a getting-started example that no
// longer builds is worse than none, because it is the first code a new user copies and
// the first thing they conclude is broken is throttle. These are Example functions with
// no // Output: comment, so the toolchain type-checks them against the real SDK on every
// "go test" and never dials OpenAI. Nothing here reads a credential.
//
// Keep these in step with README.md. If a signature here has to change, the README has to
// change with it -- that is the whole point of the file.

// The Responses integration, as the README's OpenAI block presents it.
func ExampleClient_Respond_readme() {
	ctx := context.Background()

	// Normal OpenAI SDK configuration: NewClient resolves OPENAI_API_KEY from the
	// environment itself. throttle holds no credential and reads none.
	client := oai.NewClient()

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

	governed, err := openai.New(openai.Config{
		Client:  openai.Responses(&client),
		Counter: openai.Counter(&client), // optional: preflight token counts
		Engine:  eng,
		Catalog: cat,
	})
	if err != nil {
		return
	}

	res, err := governed.Respond(ctx, openai.Request{
		BudgetID: "agents", // a budget id from the config file
		Params: responses.ResponseNewParams{ // the SDK's own params, sent verbatim
			Model: shared.ResponsesModel("gpt-5.1"),
			Input: responses.ResponseNewParamsInputUnion{
				OfString: param.NewOpt("Summarize this in one sentence."),
			},
		},
	})
	if err != nil {
		return
	}
	// res.Response is OpenAI's own *responses.Response.
	// res.Estimate, res.Usage, res.Cost, and res.Charge are the accounting.
	_, _, _, _ = res.Response, res.Estimate, res.Cost, res.Charge
}

// Streaming: the SDK's own pull loop, kept as a pull loop.
func ExampleClient_RespondStreaming_readme() {
	ctx := context.Background()
	client, eng, cat := readmeOpenAIPrerequisites(ctx)

	governed, err := openai.New(openai.Config{
		Client:       openai.Responses(&client),
		Engine:       eng,
		Catalog:      cat,
		StreamClient: openai.Streaming(&client),
	})
	if err != nil {
		return
	}

	stream, err := governed.RespondStreaming(ctx, openai.StreamRequest{
		BudgetID: "agents",
		Params: responses.ResponseNewParams{
			Model: shared.ResponsesModel("gpt-5.1"),
			Input: responses.ResponseNewParamsInputUnion{
				OfString: param.NewOpt("Count from one to five."),
			},
		},
	})
	if err != nil {
		return
	}
	defer stream.Close()

	for stream.Next() { // OpenAI's own event union, one at a time
		_ = stream.Current()
	}
	if err := stream.Close(); err != nil { // idempotent; settles, then reports
		return
	}
	_ = stream.Result() // the accounting, available once the stream is terminal.
}

// readmeOpenAIPrerequisites is the "...as above" the README's streaming example elides, so
// it can show only the lines it is actually about.
func readmeOpenAIPrerequisites(ctx context.Context) (client oai.Client, eng *engine.Engine, cat *pricing.Static) {
	client = oai.NewClient()
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
