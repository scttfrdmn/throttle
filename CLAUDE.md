# CLAUDE.md — throttle build instructions

You are implementing **throttle**, a Go project that turns a spending budget into a continuously adjusting rate limit for LLM/AI usage.

This is a three-way collaboration:

- **Scott** is product owner and makes scope/product decisions.
- **Claude Code** implements, tests, refactors, and keeps the repository coherent.
- **ChatGPT** is a design/review collaborator and will periodically review architecture, UX, APIs, and implementation choices.

**GitHub is the source of truth for work state. Repository markdown is for durable technical and product documentation only.**

Repository documents are the shared design record. When an implementation decision changes an architectural assumption, update the relevant document in the same change.

## Read first

Before making substantive changes, read:

1. `README.md`
2. `docs/architecture.md`
3. `docs/data-model.md`
4. `docs/collaboration.md`
5. The `v0.1.0` milestone and the open issues in the `throttle` GitHub Project

## GitHub workflow

Work is tracked in GitHub, not in markdown:

- Repository: `scttfrdmn/throttle`
- Project: `throttle` (Projects v2, user `scttfrdmn`) — Status: `Backlog` → `Ready` → `In Progress` → `Review` → `Done`
- Release milestone: `v0.1.0`

Standing rules:

- Every meaningful work item has a GitHub issue.
- Put the issue into the `throttle` Project and assign the appropriate milestone.
- Move its Status as work progresses; do not duplicate Status with labels.
- **Identify the issue number before writing code**, and state the acceptance criteria in the issue.
- Record open product decisions as issues labelled `kind:decision` and, while unresolved, `decision-needed`. Surface the question; do not choose product behavior silently.
- Park out-of-scope ideas on the future-policy umbrella issue (label `future`). Open a separate `future` issue only when an idea is concrete enough to discuss independently — do not explode every brainstorm into its own issue.
- Close or update the issue when the slice is complete.
- **Do not create status, roadmap, backlog, or scratch planning markdown files.** If work state seems to need a file, it needs an issue.

Label vocabulary: `kind:bug` `kind:feature` `kind:chore` `kind:decision`; `area:core` `area:budget` `area:ledger` `area:provider` `area:pricing` `area:bedrock` `area:ui` `area:docs`; `future` `blocked` `decision-needed`.

Durable docs are `README.md` (user-facing explanation and getting started), this file (contributor instructions), `docs/architecture.md` (canonical architecture), and `docs/data-model.md` (canonical data model and semantics). Existing ADRs in `docs/decisions/` stand where they capture genuinely durable decisions, but do not add ADRs for ordinary implementation choices: prefer issue discussion plus an update to the canonical architecture or data model once a decision becomes permanent.

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

1. Identify its GitHub issue, state the acceptance criteria in the issue, and move it to `In Progress`.
2. Implement the smallest coherent version.
3. Run `gofmt`, `go vet ./...`, and `go test -race ./...`.
4. Summarize files changed and any design decisions on the issue.
5. Open a `kind:decision` + `decision-needed` issue for unresolved product questions rather than silently choosing behavior.
6. If a decision touches deferred policy, add it to the future-policy umbrella issue but do not implement it.
7. Move the issue to `Review` when the slice is boringly reliable.

When Scott asks for a ChatGPT review, prepare a concise handoff with:

- what changed
- key interfaces/data structures
- tests added
- known limitations
- open decisions
- exact files that deserve review
