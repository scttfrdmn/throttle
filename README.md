<p align="center">
  <img src="assets/throttle-logo.png" alt="throttle" width="420">
</p>

# throttle

**throttle turns a spending budget into a continuously adjusting rate limit for AI usage.**

Bank what you don't use. Burst when you need to. Borrow from future budget when policy
allows. Stay on pace automatically.

## Why

A monthly limit tells you nothing on the 12th. A rate limit measured in requests per second
does not know what a request costs. Neither answers the question anyone actually has: *given
what I have spent so far, how fast can I spend right now?*

throttle answers it continuously. It measures actual spend from measured usage and provider
rates, compares that against where the period's pacing curve says you should be, and turns the
difference into an admission decision on the next request — allow, wait, or deny. A budget
becomes a rate limit denominated in money instead of requests.

> Given a budget and a time period, throttle accurately measures AI spend, shows whether usage
> is ahead or behind pace, and governs requests against the spending envelope.

## Bank, burst, and borrow

For an allocation `B` over a period `[start, end]`, the default pacing curve is linear. At
time `t`:

```text
target spend = carry + B * elapsed / duration
bank         = target spend - actual spend
```

**Banking** is what underspend earns you. A quiet week leaves the bank positive: you are
behind the curve, and that headroom is yours to spend later in the period. **Bursting** is
spending it — a positive bank is exactly how much you can spend right now above the even
split without going over for the period. A negative bank means the opposite: you have
consumed allocation the curve had not reached yet.

**Borrowing** is expressed as time, not dollars, which is the part worth reading twice.
`borrow: 72h` lets a workload spend as though it were three days further into the period than
it is. It does not raise the allocation, and it never invents money: the period total is
unchanged, and everything pulled forward is unavailable later. It exists because an even
split across a month refuses a legitimate Monday-morning batch job that the month as a whole
affords comfortably.

**Rollover** is the same idea across a boundary: unspent allocation carries into the next
period, optionally capped, so a slow month funds a busy one.

## What works today

Governed calls work end to end against two providers: AWS Bedrock — `Converse`,
`ConverseStream`, Agents Classic `InvokeAgent`, and AgentCore `InvokeAgentRuntime` — and
OpenAI, both its Responses API (streaming and non-streaming) and its older Chat
Completions API (non-streaming). Around them:

- **Budgets** with arbitrary envelopes — `monthly` is shorthand for the same period rule a
  two-year grant uses — plus linear pacing, banking, time-based borrowing, rollover with an
  optional cap, and hierarchical sub-budgets where a child's spend consumes its ancestors'
  headroom.
- **Accounting** as `estimate → reserve → execute → reconcile`, in integer microdollars, with
  concurrency-safe reservations, leases that survive a crashed process, and a repair command
  for bookkeeping left half-finished.
- **Pricing** as data with provenance and effective dates, quoted once at admission and
  replayed at settlement, so a price change mid-request cannot rewrite what a request cost.
- **Enforcement** as monitor, enforce, or wait, chosen by the process doing the spending.
- **A local dashboard** for burn rate, bank and debt, pacing, model and provider breakdowns,
  and recent requests. Read-only, loopback by default, no authentication.
- **A local SQLite ledger and activity store.** No server, no account, no network calls of
  throttle's own.

Not yet: streaming Chat Completions, Anthropic direct, and Gemini. Those adapters do
not exist — nothing here supports them today. Pricing ships as a versioned fixture catalog
rather than a live price-list sync, and the worker that ingests delayed AgentCore
runtime-resource usage is not written, though the data model and join keys for it are.

Release scope is the [`v0.1.0` milestone](https://github.com/scttfrdmn/throttle/milestone/1).
Deferred ideas are parked on the [future policy issue](https://github.com/scttfrdmn/throttle/issues/3).

## What is stored, and what is not

throttle keeps two SQLite files on your machine and nothing else. The ledger holds budget
definitions, periods, reservations, and charges. The activity store holds one record per
request: which budget and ancestors it consumed, the model identity, measured usage, cost and
whether that cost is complete, the rates quoted at admission, timings, and the enforcement
posture that governed the call.

Deliberately **not** stored, anywhere, ever:

- prompts, and model responses
- reasoning or model rationale
- agent trace payloads — throttle enables the trace because it is the only place per-invocation
  usage is reported, passes all of it to you, and persists none of it
- AgentCore request and response bodies, which it forwards without parsing
- provider credentials or API keys, which stay with each provider SDK's normal mechanisms

There is no telemetry. Nothing reports to us, there is no licence check or user counting, and
the only network calls are the ones your own application makes to its provider.

## When throttle cannot price something

A cost is known, partly known, or explicitly unknown — never silently zero. An unrecognized
model, a stream that ended before its usage metadata, an agent turn whose internal calls are
not all priceable, a hosted runtime that reports resource use later and approximately: each
produces a request whose cost throttle will not invent.

What happens next depends on the posture. **Under enforcement** a request throttle cannot
price is denied before the provider is called, because a dollar budget cannot be honestly
enforced against exposure that cannot be determined. **Under monitoring** it runs, and its
cost is recorded as unknown. Either way the dashboard renders it as unknown rather than
`$0.00`, and a period total containing one is shown as a floor — `$812.41+` — because a total
that quietly omits unpriceable spend is worse than one that admits it is a lower bound.

## Install

```bash
go install github.com/scttfrdmn/throttle/cmd/throttle@latest
```

Or download a prebuilt binary for macOS or Linux from the
[releases page](https://github.com/scttfrdmn/throttle/releases). From a checkout,
`go build -o throttle ./cmd/throttle` works with no further setup.

`throttle init` writes a starter configuration file and nothing else — no databases, no
credentials, no cloud resources:

```bash
throttle init    # writes the config file, prints where, and says what to do next
```

The default location follows the platform's own conventions:

| | config file | ledger and activity stores |
|---|---|---|
| macOS | `~/Library/Application Support/throttle/throttle.yaml` | `~/Library/Application Support/throttle/` |
| Linux | `~/.config/throttle/throttle.yaml` | `~/.local/share/throttle/` |

Linux honours `XDG_CONFIG_HOME` and `XDG_DATA_HOME`. Nothing is written to the current
directory, and `-config`, `-db`, `-activity`, `THROTTLE_CONFIG`, `THROTTLE_LEDGER`, and
`THROTTLE_ACTIVITY` override the defaults for development, CI, and portable installs.

The file is the same budget model the engine uses — `monthly` is convenience syntax that
compiles to the same period rule as a two-year grant. [`examples/throttle.yaml`](examples/throttle.yaml)
is the fuller annotated version, and it parses under the real loader:

```yaml
version: 1

defaults:
  budget: research

budgets:
  research:
    amount: $4,000
    period:
      recur: monthly
      timezone: America/New_York
      anchor: 2026-01-01   # the first day it applies; required, never taken from the clock
    borrow: 72h            # may spend up to three days ahead of its own pacing curve
    rollover:
      mode: credit         # unspent money carries forward...
      cap:
        percent: 25        # ...up to a quarter of the allocation

  chat:                    # a child's spend also consumes its parent's headroom
    parent: research
    amount: $1,000
```

Then check it, see what it would do, store it, and watch it:

```bash
throttle config check    # parse, validate, resolve, and compare to the ledger. Writes nothing.
throttle config show     # what is in effect, and where each value came from
throttle config diff     # what "apply" would change, in detail. Still writes nothing.
throttle config apply    # store the definitions from the file
throttle status          # uses defaults.budget when no -id is given
throttle serve           # the dashboard, on 127.0.0.1:7654
```

`config check` and `config diff` are read-only and exit nonzero on a problem, so they work
as CI steps. Reading a file never changes a stored budget: a stored definition governs money
that has already been spent against it, so `config apply` is the only command that writes
one, and it says what it did.

`apply` is deliberately narrow about what it will do:

```text
research
  changed: allocation
      allocation: file $5000.00, stored $4000.00
  current period unchanged
  new definition applies beginning 2026-09-01 00:00 UTC
```

- Changing a recurring budget does **not** rewrite the period already running. Money spent
  under the old terms was governed by the old terms; the new definition takes effect at the
  next boundary, and `diff` says which date that is.
- A budget that disappears from the file is left alone, not deleted. Nothing here removes a
  durable definition.
- A name that differs is reported, never applied: two budgets with identical financial terms
  are not evidence of a rename. `throttle rename <budget> <new name>` is the explicit
  command, and it changes the display name only.
- If a stored definition changed since the diff, `apply` refuses rather than overwriting it.
  Re-run `diff` and look again.

Anything a flag can say, the file can say too, and the precedence is fixed — later wins,
nothing merged:

```text
built-in defaults  <  config file  <  environment  <  command flag
```

A one-off budget needs no file at all:

```bash
# Define a budget once; it is persisted and shared by every process on the ledger.
throttle define -id research -budget '$400' -borrow 72h -rollover credit

# Sub-budgets are real entities, and a child's spend consumes its ancestors too.
throttle define -id agents -parent research -budget '$150'

# Is a $2.50 request admissible right now, and if not, which budget said no?
throttle status -id agents -chain -estimate 2.50

# After a crash: what bookkeeping is half-finished, and what would repairing it do?
throttle reconcile -dry-run
```

For the test suite and the demo:

```bash
make check
make demo
```

A crash between the ledger write and the activity write leaves the two stores
disagreeing, so `reconcile` finishes the bookkeeping from durable state — and only
where the durable state already says what happened. A request whose outcome is
genuinely unknown stays unknown and stays encumbered; it does not become a tidy zero:

```text
scanned 18 / repaired 3 / consistent 10 / unresolved 3 / awaiting data 2
```

## Governing a call

There are two direct adapters today, **AWS Bedrock** and **OpenAI**. Each is a
shim around that provider's own client rather than a replacement for it: the request and
the response stay the SDK's own types, and there is deliberately no generic
`throttle.Generate` in front of them. What they share is the accounting — one budget
engine, one ledger, one set of money semantics — not an invented common API.

### AWS Bedrock

In full, against a budget `config apply` already stored:

```go
ctx := context.Background()

// Normal AWS SDK configuration: profiles, environment, IMDS, SSO. throttle adds
// nothing here and holds no credentials of its own.
awsCfg, err := awsconfig.LoadDefaultConfig(ctx)

led, err := sqlite.Open(ctx, "/path/to/ledger.db") // the store "throttle config apply" wrote
defer led.Close()

eng, err := engine.New(engine.Config{Ledger: led})
cat, err := fixtures.Catalog() // versioned price fixtures, with provenance

brc := bedrockruntime.NewFromConfig(awsCfg)
client, err := bedrock.New(bedrock.Config{
	Client:  brc,
	Counter: brc, // optional: preflight token counts
	Engine:  eng,
	Catalog: cat,
	Region:  awsCfg.Region,
})

res, err := client.Converse(ctx, bedrock.Request{
	BudgetID: "agents", // a budget id from the config file
	Input:    &bedrockruntime.ConverseInput{ /* unchanged, passed through verbatim */ },
})
// res.Output is Bedrock's own *ConverseOutput.
// res.Estimate, res.Usage, res.Cost, and res.Charge are the accounting.
// res.Cost may be legitimately unknown while res.Usage is known: throttle reports
// that it cannot price a request rather than pricing it at zero.
```

Spend against `agents` also consumes `research`, because that is what the file said.
Add `Activity: acts` (an `activity/sqlite` store) for durable, content-free
per-request records, which is what the dashboard's history and breakdowns read.

Under enforcement, a request throttle cannot price is denied before the provider is
called — a dollar budget cannot be honestly enforced against exposure that cannot
be determined. Under monitoring the same request runs, and its cost is recorded as
explicitly unknown.

Streaming keeps the SDK's read loop and stays streaming — events are forwarded one
at a time with ordinary backpressure, never buffered or replayed:

```go
client, err := bedrock.New(bedrock.Config{
	// ...as above, plus:
	StreamClient: bedrock.Streaming(bedrockruntime.NewFromConfig(awsCfg)),
})

stream, err := client.ConverseStream(ctx, bedrock.StreamRequest{
	BudgetID: "agents",
	Input:    &bedrockruntime.ConverseStreamInput{ /* passed through verbatim */ },
})
if err != nil {
	return err
}
defer stream.Close()

for ev := range stream.Events() { // Bedrock's own event types, as they arrive
	switch v := ev.(type) { /* ... */ }
}
if err := stream.Close(); err != nil { // idempotent; settles, then reports
	return err
}
// stream.Result() is the accounting, available once the stream is terminal.
```

Bedrock reports token usage only in the terminal metadata event, so a stream that
ends before it — closed early, cancelled, abandoned, or broken — leaves a request
that ran and cannot be measured. throttle keeps the reservation encumbered and
records the outcome as unknown rather than releasing it: the caller stopping is a
fact about the caller, not about the model. Only a `ConverseStream` call that failed
before any stream existed releases its hold.

A managed agent turn is one governed transaction that may invoke a foundation model
many times:

```go
client, err := bedrock.New(bedrock.Config{
	// ...as above, plus:
	AgentClient: bedrock.Agent(bedrockagentruntime.NewFromConfig(awsCfg)),
})

stream, err := client.InvokeAgent(ctx, bedrock.AgentRequest{
	BudgetID: "agents",
	Input:    &bedrockagentruntime.InvokeAgentInput{ /* passed through verbatim */ },
	MaxCost:  maxCost, // the declared ceiling, and the amount held
})
if err != nil {
	return err
}
defer stream.Close()

for ev := range stream.Events() { // chunks, traces, return-control — all of them
	switch v := ev.(type) { /* ... */ }
}
if err := stream.Close(); err != nil {
	return err
}
// stream.Result().Cost is the whole turn. Result().Steps and Result().Agent.Steps
// are the individual model invocations beneath it.
```

`MaxCost` is a **declaration, not a cap**: AWS does not stop the agent at throttle's
number, so the estimate is labelled a heuristic and the actual cost may exceed the hold.
throttle cannot estimate an agent turn any other way — the API takes an agent
identifier, the agent decides how many models to invoke and with what prompts, and
counting the caller's tokens would count prompts the agent never sends.

There is **one reservation and one charge**, because throttle admitted the outer
invocation and not the model calls inside it. Those are recorded as accounting detail
beneath the transaction: per-step kind, model, usage, and cost, so an operator can see
where a turn's money went. The turn is accumulated exactly and rounded once, so twenty
small internal calls are charged like one charge rather than rounded twenty times.

throttle enables the service's trace, on a copy of the caller's input, because that is
the only place per-invocation usage is reported. The trace also carries prompts, model
responses, reasoning, action payloads, and retrieved passages. All of it reaches the
caller; **none of it is persisted** — the durable record has nowhere to put it.

Action groups, knowledge-base lookups, and guardrails are counted and never priced. The
service reports no billable quantity for them, so their cost lands on the AWS bill
outside throttle's view, and the record says so rather than implying they were free.

#### A hosted agent: two places to govern, and only one of them is real-time

AgentCore runs *your own* agent code and bills for the compute it consumed, so there
are two distinct accounting positions — and the useful one is probably not the one you
would reach for first.

**Inside the agent** — where the model calls actually happen — you use throttle
exactly as anywhere else. There is no AgentCore budget type, no AgentCore client, and
no special import; hosting location does not change model-spend governance:

```go
// This is the whole integration. It runs unchanged in AgentCore, in Lambda, or on a laptop.
res, err := client.Converse(ctx, bedrock.Request{BudgetID: "agents", Input: in})
```

This is the **preferred mechanism for real-time enforcement**, because throttle sees
each model call before it happens and settles it on measured usage.

**At the edge** — wrapping the invocation itself — throttle governs admission and
records the identity a later resource observation can be joined to:

```go
client, err := bedrock.New(bedrock.Config{
	// ...as above, plus:
	RuntimeClient: bedrockagentcore.NewFromConfig(awsCfg),
})

stream, err := client.InvokeAgentRuntime(ctx, bedrock.RuntimeRequest{
	BudgetID:    "agents",
	Input:       &bedrockagentcore.InvokeAgentRuntimeInput{ /* passed through verbatim */ },
	MaxExposure: maxExposure, // how much budget headroom to encumber
})
if err != nil {
	return err
}
defer stream.Close()

io.Copy(w, stream) // the runtime's own bytes, forwarded as they arrive
if err := stream.Close(); err != nil {
	return err
}
```

The response is an opaque body in whatever format your agent produces. throttle
forwards it and **parses none of it** — not to derive accounting, and not to store.

The edge invocation's cost is **unknown on every path, success included.** AgentCore
reports CPU and memory consumption later, through observability, and calls those
figures approximate. throttle will not manufacture a number from what it does have:
not from latency (billing excludes time your agent waits on a model), not from
configured CPU and memory (allocation is not consumption), and not from response size.
Unknown is recorded as unknown, and it never renders as `$0.00`.

`MaxExposure` is named for what it is: **the amount of budget headroom to hold**, not
a cost, not an estimate, and not a limit AWS will enforce. Nothing stops your runtime
at throttle's number. Under enforcement an invocation with no declared exposure is
denied before the call; under monitoring it runs and is flagged cost-unknown.

Because the cost never becomes knowable, every invocation that reached the runtime
stays **unresolved** with its exposure encumbered, carrying the runtime, endpoint,
session, and trace identifiers that a later reconciliation would need. Where AWS
reports resource usage per *session* rather than per invocation, throttle records the
session and **does not divide its bill** across the invocations that shared it — a
computed share would be indistinguishable from a measurement.

### OpenAI Responses

The same shape, a different SDK. The params are `responses.ResponseNewParams` and
throttle sends them unchanged — it does not adjust `MaxOutputTokens`, set `Store`,
or touch `ServiceTier`. The official client resolves `OPENAI_API_KEY` itself, so
throttle never holds the credential.

Both packages are called `openai`, so one of them needs an alias:

```go
import (
	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/provider/openai"
)
```

```go
client := oai.NewClient() // the official SDK; it reads OPENAI_API_KEY, throttle does not

governed, err := openai.New(openai.Config{
	Client:  openai.Responses(&client),
	Counter: openai.Counter(&client), // optional: preflight token counts
	Engine:  eng,                     // the same engine and catalog as above
	Catalog: cat,
})

res, err := governed.Respond(ctx, openai.Request{
	BudgetID: "agents",
	Params: responses.ResponseNewParams{ // the SDK's own params, sent verbatim
		Model: shared.ResponsesModel("gpt-5.1"),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: param.NewOpt("Summarize this in one sentence."),
		},
	},
})
// res.Response is OpenAI's own *responses.Response.
// res.Estimate, res.Usage, res.Cost, and res.Charge are the accounting.
```

Streaming stays a pull loop, because that is what the OpenAI SDK is; Bedrock's
stays a channel, because that is what its SDK is. throttle does not invent a
common stream type to make the two look alike — only the accounting is shared:

```go
governed, err := openai.New(openai.Config{
	// ...as above, plus:
	StreamClient: openai.Streaming(&client),
})

stream, err := governed.RespondStreaming(ctx, openai.StreamRequest{
	BudgetID: "agents",
	Params:   params, // as above
})
if err != nil {
	return err
}
defer stream.Close()

for stream.Next() { // OpenAI's own event union, one at a time
	ev := stream.Current()
	_ = ev
}
if err := stream.Close(); err != nil { // idempotent; settles, then reports
	return err
}
// stream.Result() is the accounting, available once the stream is terminal.
```

OpenAI reports usage only on the terminal `Response`, so the same rule as Bedrock
applies: a stream that ends without one leaves a request that ran and cannot be
measured, and its reservation stays encumbered rather than being released. A
caller who stops reading without closing or cancelling is treated as abandoning
the stream after `StreamStallTimeout` — a slow provider is not a stalled consumer,
and that bound only measures the gap between one event being delivered and the next
being asked for.

Because a request can be served by a different tier than it asked for, and tiers
price differently, throttle settles on the tier OpenAI *reported* serving. When
that is not observable the request is recorded as pricing-unresolved rather than
priced at the standard rate.

### OpenAI Chat Completions

Responses is the surface to write new code against, and the one above is the example to
copy. Chat Completions is here because applications that already use it should not have to
be rewritten to be governed — so it is a second client on the same `openai.Config`,
sharing one budget, one ledger, and one price catalog:

```go
governed, err := openai.New(openai.Config{
	// ...as above, plus:
	ChatClient: openai.ChatCompletions(&client),
})

res, err := governed.Complete(ctx, openai.ChatRequest{
	BudgetID: "agents",
	Params: oai.ChatCompletionNewParams{ // the SDK's own params, sent verbatim
		Model: shared.ChatModel("gpt-5.1"),
		Messages: []oai.ChatCompletionMessageParamUnion{
			oai.UserMessage("Summarize this in one sentence."),
		},
	},
})
// res.Completion is OpenAI's own *oai.ChatCompletion; the accounting fields are the same
// ones Respond returns.
```

The two API families stay visibly distinct at the SDK boundary — there is no common
request type in front of them — and identical underneath it. Requests are told apart in
the dashboard by their operation, `chat-completions` against `responses`, which is the
existing column rather than a new one. Streaming Chat Completions is not supported yet.

One asymmetry worth knowing: an audio request is priced only as far as its rates go. Audio
input and output are separate token dimensions billed at their own rates, so when the
catalog has no audio rates for the model, the request is refused before the call under
enforcement, and under monitoring it settles as the text cost plus the two unpriced audio
dimensions by name — a floor, never a total, and never zero.

## Architectural rules

1. The budget engine never imports provider SDKs.
2. Money is integer microdollars; floats never enter the ledger.
3. Provider adapters preserve native SDK behavior as much as practical.
4. Access provider, model publisher, model family, model/version, workload, principal, and budget are separate dimensions.
5. Accounting and enforcement come before optimization.
6. Automatic model substitution, prompt rewriting, routing, and other higher-level policy are **not v0.1**.
7. Never invent or silently freeze provider prices. Pricing data must be sourceable, versioned, and timestamped.
8. Managed-agent accounting must not claim hard mid-run enforcement when the upstream platform does not expose such a control boundary.
9. Hosted-runtime resource cost is never inferred from wall-clock time, allocated capacity, or payload size, and a session's bill is never apportioned across the invocations that shared it.

## License

throttle is licensed under the [Apache License 2.0](LICENSE).

Copyright 2026 Scott Friedman.

That is the whole licensing story for this repository. Shared budgets across a team, and
institutional governance on top of them, are planned as commercial editions — but that is a
product boundary rather than a licence term. Nothing in the Apache licence restricts this core
to one person or to non-commercial use, and there is no licence check, user counting, feature
gate, or telemetry in it.
