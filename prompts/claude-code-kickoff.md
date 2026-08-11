# Claude Code kickoff prompt

You are the primary implementation collaborator for a new Go project named **throttle**. I am the product owner, and ChatGPT is our design/review collaborator. This repository contains the initial skeleton and the design record from our planning conversation.

Start by reading `CLAUDE.md`, `README.md`, `PROJECT_STATE.md`, and every file in `docs/`. Do not immediately start wiring provider SDKs.

The product promise is:

> Given a budget and a time period, throttle accurately measures AI/LLM spend, tells the user whether they are ahead or behind pace, and governs requests against that spending envelope.

Key decisions already made:

- Go only.
- Provider-neutral core; AWS Bedrock is the first proving ground, not the architecture boundary.
- Monthly is the default/convenience period, but the engine works on arbitrary start/end envelopes such as academic grants.
- Underspend is banked automatically relative to the pacing curve.
- Borrowing is expressed as a time window (for example 72h) that allows controlled bursts without changing the total allocation.
- Rollover is configurable.
- Sub-budgets are first-class (chat, agents, experiments, etc.).
- Access provider and underlying model identity are separate dimensions. Example: AWS Bedrock / Anthropic / Claude / specific model.
- Money uses integer microdollars. No floating-point accounting.
- The operational transaction is estimate → atomic reserve → execute → reconcile actual usage.
- v0.1 enforcement is monitor / allow / wait / deny.
- Higher-level policies such as prompt concision, output caps, model substitution, access-path routing, service-tier selection, and economy mode are good future ideas, but are explicitly NOT implementation scope yet. Preserve seams for them; do not build them.
- Single-user is free/open-source. Teams and enterprise are later commercial capabilities. Do not build licensing or team features now.

Your first job is to perform a technical review of this skeleton before expanding it. In particular, challenge the budget math, overflow behavior, money arithmetic, wait-time calculation, reservation contract, and period-boundary semantics. Fix correctness problems you find and add tests before adding features.

Then implement the first milestone:

**A concurrency-safe local budget/reservation engine with SQLite persistence that can precisely answer:**

1. What should have been spent by now?
2. How much is banked or borrowed?
3. How much can be committed right now?
4. Can this estimated request be reserved atomically?
5. If it cannot be spent now but fits the period, when does it become affordable?
6. What happens at the period boundary with rollover enabled or disabled?
7. How are stale reservations recovered after crashes?

Work as a sequence of small, reviewable vertical slices. Before each slice, state the acceptance criteria. After each slice, run `gofmt`, `go vet ./...`, and `go test -race ./...`, summarize changes, and update `PROJECT_STATE.md` with decisions/open questions.

Do not silently make product decisions when semantics are ambiguous. Surface the question to me. If an interesting idea falls outside v0.1, record it in `docs/future-policy.md` rather than implementing it.

Once the local engine is boringly reliable, stop and give me a handoff suitable for review by ChatGPT before proceeding to the Bedrock adapter.
