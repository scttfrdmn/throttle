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

See [`docs/v0.1-scope.md`](docs/v0.1-scope.md) for the boundary and [`docs/future-policy.md`](docs/future-policy.md) for deliberately deferred ideas.

## Status

The local core is implemented and tested; **no provider adapter exists yet**.

Working today:

- `money` — integer microdollar arithmetic with explicit overflow reporting
- `budget` — envelopes, linear pacing, banking, time-based borrowing, rollover, recurrence rules
- `ledger` — the reservation contract, plus a conformance suite that is its executable specification
- `ledger/sqlite` — durable budget definitions, materialized periods, atomic hierarchical reservations, lease renewal, crash recovery
- `engine` — admission decisions, `estimate → reserve → execute → reconcile`, bounded waiting, period advancement
- `cmd/throttle` — define and inspect budgets

Next: the AWS Bedrock adapter (`Converse`, `ConverseStream`, `InvokeAgent`).

```bash
make check
make demo
```

Example:

```bash
# Define a budget once; it is persisted and shared by every process on the ledger.
go run ./cmd/throttle define -id research -budget 400 -borrow 72h -rollover credit

# Sub-budgets are real entities, and a child's spend consumes its ancestors too.
go run ./cmd/throttle define -id agents -parent research -budget 150

# Is a $2.50 request admissible right now, and if not, which budget said no?
go run ./cmd/throttle status -id agents -chain -estimate 2.50
```

## Architectural rules

1. The budget engine never imports provider SDKs.
2. Money is integer microdollars; floats never enter the ledger.
3. Provider adapters preserve native SDK behavior as much as practical.
4. Access provider, model publisher, model family, model/version, workload, principal, and budget are separate dimensions.
5. Accounting and enforcement come before optimization.
6. Automatic model substitution, prompt rewriting, routing, and other higher-level policy are **not v0.1**.
7. Never invent or silently freeze provider prices. Pricing data must be sourceable, versioned, and timestamped.
8. Managed-agent accounting must not claim hard mid-run enforcement when the upstream platform does not expose such a control boundary.

## Product boundary

The intended product model is simple:

- **single user:** free/open-source and functionally complete for personal use
- **teams:** shared budgets and collaboration, commercial but intentionally inexpensive
- **enterprise:** institutional identity, governance, audit, deployment, support, and policy administration

Do not introduce licensing gates into the core while building v0.1.

## License

TBD before public release. Do not add a license without an explicit project decision.
