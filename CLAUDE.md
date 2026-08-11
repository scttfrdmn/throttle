# CLAUDE.md — throttle build instructions

You are implementing **throttle**, a Go project that turns a spending budget into a continuously adjusting rate limit for LLM/AI usage.

This is a three-way collaboration:

- **Scott** is product owner and makes scope/product decisions.
- **Claude Code** implements, tests, refactors, and keeps the repository coherent.
- **ChatGPT** is a design/review collaborator and will periodically review architecture, UX, APIs, and implementation choices.

Treat repository documents as the shared design record. When an implementation decision changes an architectural assumption, update the relevant document in the same change.

## Read first

Before making substantive changes, read:

1. `README.md`
2. `docs/v0.1-scope.md`
3. `docs/architecture.md`
4. `docs/data-model.md`
5. `docs/future-policy.md`
6. `docs/collaboration.md`
7. `PROJECT_STATE.md`

## Core product promise

> Given a budget and a time period, throttle accurately measures AI spend, shows whether usage is ahead or behind pace, and governs requests against the spending envelope.

Everything in v0.1 should strengthen that promise.

## Hard architectural invariants

- Go is the implementation language.
- The budget engine is provider-neutral and must not import provider SDKs.
- Money is stored as integer microdollars (`int64`). Never use float64 for accounting or persistence.
- The operational transaction is `estimate → reserve → execute → reconcile`.
- Reservations must be concurrency-safe and eventually atomic across processes when SQLite is added.
- A failed/cancelled request must release its reservation unless actual billable usage is known.
- Actual cost may exceed an estimate/reservation; record reality rather than hiding the overrun.
- Access provider and underlying model identity are independent fields.
- Provider pricing is data with provenance/effective dates, not hard-coded business logic.
- Preserve native provider SDK request/response behavior as much as possible.
- Do not build a new generic LLM API as the primary developer interface.
- Do not implement automatic model substitution, provider routing, prompt economy policies, reasoning controls, or other future policy unless Scott explicitly promotes them into scope.
- Leave clean seams for those future features; do not foreclose them.
- Do not claim managed Bedrock Agent invocations can be stopped at an exact internal dollar boundary unless the API actually provides that control.

## v0.1 build sequence

Work in small vertical slices. Recommended order:

1. **Core correctness**
   - strengthen `money`
   - strengthen `budget.Envelope` and pacing tests
   - define rollover semantics precisely
   - define borrowing semantics precisely
   - add admission/wait-time calculations

2. **Ledger contract**
   - finish reservation/settlement semantics
   - add SQLite implementation using transactions
   - test concurrent reservation contention
   - support stale reservation recovery

3. **Normalized usage + pricing**
   - model access provider, publisher, family, model/version, region/tier
   - represent token and non-token billable dimensions without assuming every provider bills identically
   - add versioned pricing catalog contract and fixtures

4. **AWS Bedrock adapter**
   - AWS SDK for Go v2
   - `Converse`
   - `ConverseStream`
   - `InvokeAgent`
   - preflight estimation where supported
   - response usage reconciliation
   - trace-based agent accounting where available
   - preserve underlying model/publisher identity

5. **Local service + dashboard**
   - local HTTP API
   - live request activity
   - budget/bank/debt/forecast views
   - provider/model/workload breakdowns
   - no framework-heavy frontend unless justified

6. **Second providers**
   - OpenAI
   - Anthropic direct
   - use these to validate that the core really is provider-neutral

## Engineering style

- Prefer small interfaces defined by consumers.
- Prefer standard library unless a dependency clearly earns its place.
- Keep packages cohesive and unsurprising.
- Avoid premature plugin systems or reflection-heavy abstraction.
- Use `context.Context` through request paths.
- Make cancellation behavior explicit.
- Keep timestamps in UTC internally; budget envelopes may retain a timezone for calendar recurrence semantics later.
- Add tests for boundary times, month lengths, leap years, negative bank, rollover caps, borrow windows, and concurrency.
- All commits/changes should leave `go test ./...` and `go vet ./...` clean.

## Collaboration protocol

For each meaningful slice:

1. State the slice and acceptance criteria before editing.
2. Implement the smallest coherent version.
3. Run tests/vet.
4. Summarize files changed and any design decisions.
5. Record unresolved questions in `PROJECT_STATE.md` rather than silently choosing product behavior.
6. If a decision touches deferred policy, add a note to `docs/future-policy.md` but do not implement it.

When Scott asks for a ChatGPT review, prepare a concise handoff with:

- what changed
- key interfaces/data structures
- tests added
- known limitations
- open decisions
- exact files that deserve review

## First task

Do **not** begin by wiring every provider.

Start by reviewing the supplied skeleton for correctness and improving the core budget/ledger contracts. The first externally useful milestone is a concurrency-safe local engine that can answer:

- What should have been spent by now?
- How much is banked or borrowed?
- How much can be committed right now?
- Can this estimated request be reserved atomically?
- If not, when will it become affordable?
- What happens at the period boundary with rollover enabled/disabled?

Once those answers are precise and heavily tested, move to Bedrock.
