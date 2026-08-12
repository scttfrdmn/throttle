<p align="center">
  <img src="assets/throttle-logo.png" alt="throttle" width="420">
</p>

# throttle

**Turn an AI spending budget into a continuously adjusting rate limit.**

`throttle` is a provider-neutral spending governor for LLM and AI API usage. Give it a budget and a time envelope; it tracks actual usage, banks underspend, permits controlled bursts, optionally borrows from future budget, and prevents spend from outrunning the envelope.

The initial proving ground is AWS Bedrock chat and agents, but the core is intentionally independent of Bedrock, OpenAI, Anthropic, Gemini, or any other provider.

## Core idea

For an allocation `B` over a period `[start, end]`, the default pacing curve is linear. At time `t`:

```text
target spend = carry + B * elapsed / duration
bank         = target spend - actual spend
```

A positive bank means the workload is ahead of plan (underspent). A negative bank means it has consumed future allocation.

Borrowing is expressed as time, not dollars. `borrow: 72h` allows the workload to pull forward up to three days of the pacing curve without changing the total period allocation.

## v0.1 promise

> Given a budget and a time period, throttle accurately measures AI spend, shows whether consumption is ahead or behind pace, and governs requests against the spending envelope.

The first release should focus on:

- arbitrary budget envelopes; monthly is shorthand/default
- linear pacing
- rollover on/off with an optional cap
- time-based borrowing
- hierarchical sub-budgets
- estimate → reserve → execute → reconcile accounting
- integer microdollar money representation
- concurrent-safe reservations
- Bedrock `Converse`, `ConverseStream`, and `InvokeAgent`
- model identity below the access provider (e.g. Anthropic Claude via Bedrock)
- provider-neutral usage and pricing records
- local SQLite ledger
- local dashboard for burn, bank/debt, range/forecast, model/provider/workload use, and live activity
- monitor, allow, wait/queue, and deny enforcement

Scope for the first release is the [`v0.1.0` milestone](https://github.com/scttfrdmn/throttle/milestone/1). Deliberately deferred ideas are parked on the [future policy issue](https://github.com/scttfrdmn/throttle/issues/3).

## Status

The local core is implemented and tested, and governed AWS Bedrock `Converse`,
`ConverseStream`, Agents Classic `InvokeAgent`, and AgentCore `InvokeAgentRuntime`
calls work end to end.

Working today:

- `money` — integer microdollar arithmetic with explicit overflow reporting
- `budget` — envelopes, linear pacing, banking, time-based borrowing, rollover, recurrence rules
- `ledger` — the reservation contract, plus a conformance suite that is its executable specification
- `ledger/sqlite` — durable budget definitions, materialized periods, atomic hierarchical reservations, lease renewal, crash recovery
- `engine` — admission decisions, `estimate → reserve → execute → reconcile`, bounded waiting, period advancement
- `usage` — dimensional usage and model identity, where an unrecognized model is a representable state rather than an error, and a cost is known, partly known, or explicitly unknown — never silently zero; every dimension has a canonical integer unit, so a provider's decimal resource quantity is converted exactly rather than stored as a float
- `pricing` — exact integer rates with provenance, effective dates, and local overrides, quoted once at admission and replayed at settlement so a price change mid-request cannot rewrite what a request cost; for a request whose models are unknowable in advance, the whole candidate rate set is frozen in one read instead
- `activity`, `activity/sqlite` — durable, content-free per-request records: usage, cost and its completeness, the captured quote, the compound detail of an agent turn, the reconciliation linkage of a hosted runtime invocation, and the enforcement posture that actually governed the call
- `provider/bedrock` — governed `Converse`, `ConverseStream`, Agents Classic `InvokeAgent`, and AgentCore `InvokeAgentRuntime`: preflight estimation, response reconciliation, durable activity, and explicit behavior for cancellation, provider errors, unpriceable costs, and ambiguous outcomes
- `reconcile` — repairs bookkeeping a crashed process left half-finished, from durable state alone: it completes a stalled transition when the two stores already hold enough authoritative information to complete it truthfully, prices a replayed settlement with the quote captured at admission rather than any current catalog, and leaves a genuinely unknown cost explicitly unknown instead of tidying it into a zero
- `dashboard` — a local, read-only view of burn, bank and debt, pacing, model and provider breakdowns, and live activity; loopback by default, because it has no authentication
- `config` — one configuration model shared by every command, with a documented precedence and a `config check` that validates without touching the ledger
- `cmd/throttle` — define and inspect budgets, run the dashboard, and repair stranded bookkeeping

Not yet: providers other than Bedrock, and the worker that ingests delayed AgentCore
runtime-resource usage — the data model and the join keys for it exist, the ingestion
does not. Pricing ships as a versioned fixture catalog rather than a live AWS Price
List sync.

## Getting started

```bash
go build -o throttle ./cmd/throttle
```

`throttle init` writes a starter configuration file and nothing else — no databases, no
credentials, no cloud resources:

```bash
./throttle init    # writes the config file, prints where, and says what to do next
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
./throttle config check    # parse, validate, resolve, and compare to the ledger. Writes nothing.
./throttle config show     # what is in effect, and where each value came from
./throttle config diff     # what "apply" would change, in detail. Still writes nothing.
./throttle config apply    # store the definitions from the file
./throttle status          # uses defaults.budget when no -id is given
./throttle serve           # the dashboard, on 127.0.0.1:7654
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
./throttle define -id research -budget '$400' -borrow 72h -rollover credit

# Sub-budgets are real entities, and a child's spend consumes its ancestors too.
./throttle define -id agents -parent research -budget '$150'

# Is a $2.50 request admissible right now, and if not, which budget said no?
./throttle status -id agents -chain -estimate 2.50

# After a crash: what bookkeeping is half-finished, and what would repairing it do?
./throttle reconcile -dry-run
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

Governing a Bedrock call is a shim around the real client, not a replacement for
it — the request and response are the SDK's own types. In full, against a budget
`config apply` already stored:

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

### A hosted agent: two places to govern, and only one of them is real-time

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

## Product boundary

The intended product model is simple:

- **single user:** free/open-source and functionally complete for personal use
- **teams:** shared budgets and collaboration, commercial but intentionally inexpensive
- **enterprise:** institutional identity, governance, audit, deployment, support, and policy administration

Do not introduce licensing gates into the core while building v0.1.

## License

TBD before public release. Do not add a license without an explicit project decision.
