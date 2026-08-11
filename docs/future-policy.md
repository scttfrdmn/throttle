# Future policy ideas — deliberately deferred

The design should preserve clean seams for these ideas without implementing them in v0.1.

## Principle

> throttle measures and governs spend. It may eventually optimize spend, but optimization must remain separable from accounting and enforcement.

## Candidate future controls

- optional system instruction encouraging concise responses
- adaptive output-token limits
- reasoning-effort controls
- economy mode based on budget pressure
- preferred/allowed model sets
- smaller-model fallback
- equivalent-model substitution
- alternate access-provider routing
- cheapest-equivalent route selection
- latency/quality/cost optimization
- Bedrock service-tier selection
- cache-aware policy
- workload priorities
- dynamic queue priority
- overlapping budget constraints
- organization policy inheritance
- approval workflows

## Surfaced while building the local engine

These came up as tempting additions during the core budget/ledger work and were
deliberately not built. They are recorded here so the reasoning is not lost.

- **Priority-aware admission.** When a budget is under pressure, admit
  high-priority work and make low-priority work wait, instead of first-come
  first-served. `Decision` already carries a `Reason`, so a priority input is a
  natural extension — but choosing whose request loses is policy, not accounting.
- **Fair-share queueing across callers.** `engine.Wait` currently races: every
  waiter re-checks and the fastest wins, so one hot loop can starve others. A
  real queue would need an ordering policy, which is deferred.
- **Adaptive reservation sizing from history.** Charges record both estimate and
  actual, so throttle could learn a per-model correction factor and reserve more
  accurately. This is optimization, and it must not become a way to quietly
  reserve less than a request might cost.
- **Automatic period rollover on a schedule.** `Envelope.Next` computes the next
  period, but nothing runs it. A scheduler implies opinions about calendars,
  timezones, and what happens to in-flight holds at the boundary.
- **Spend forecasting beyond linear extrapolation.** `Status.ProjectedSpend`
  scales observed spend over elapsed time. Day-of-week or diurnal patterns would
  forecast better, but a wrong forecast that looks sophisticated is worse than an
  obviously simple one.
- **Refund/credit records.** Providers occasionally credit back usage. The ledger
  has no negative charge concept, and adding one touches every aggregate, so it
  needs deliberate design rather than an ad-hoc negative row.

## Surfaced while building the durable core

The second milestone made definitions, periods, leases, and sub-budgets durable.
These were the tempting adjacent features, again deliberately not built.

- **Overlapping, non-hierarchical constraints.** `reservation_legs` /
  `charge_legs` already express "this hold consumes N scopes atomically", so a
  per-model, per-workload, or per-provider cap is an additional leg rather than a
  new mechanism. The seam is deliberate; the policy is not. Building it needs
  answers about how a request discovers which constraints apply to it and what a
  cap means for an identity the ledger has never seen before.
- **Persisted enforcement posture.** Mode lives in the engine, so the process
  doing the spending decides. That is defensible but means two services can govern
  the same budget differently and the CLI cannot show how a budget is really
  enforced at the call site. A `budgets.default_mode` a process may override is
  the obvious middle ground, and it is a product decision.
- **Mid-period definition re-snapshotting.** A materialized period keeps its own
  allocation/borrow/rollover snapshot, so raising a budget mid-month has no effect
  until the next period. Applying an increase immediately is defensible, but it
  moves the pacing curve under in-flight requests, and lowering a budget below
  what is already committed has no obvious correct behavior.
- **Fairness in `Wait`.** Bounded re-evaluation means several waiters wake and
  race, so a large request can be starved by a stream of small ones. Recorded
  above as fair-share queueing; the durable core did not change the conclusion,
  it only made starvation easier to observe.
- **Scheduled period advancement.** `Advance` / `AdvanceAll` exist and are
  idempotent, but something still has to call them. A daemon or cron entry is
  packaging, not budget semantics, and it belongs with the local service.
- **Cross-period reservation splitting.** A hold is authorized by exactly one
  period per scope, chosen when the reservation is taken. A long-running request
  that genuinely spans a boundary could in principle be apportioned across both
  periods by time. That is a materially different accounting rule, so it is a
  product decision rather than an implementation detail.

## Important seams to retain now

- adapters should have a request-preparation interception point even if it is a no-op initially
- charge records should allow `policy_actions: []`
- model identity and access provider must remain distinct
- budget engine output should be an explicit decision object, not a bare boolean
- pricing must be separable from provider invocation

## Non-goal

Do not turn throttle into a general agent framework or universal LLM SDK. Future policy operates around native provider calls; it does not replace them.
