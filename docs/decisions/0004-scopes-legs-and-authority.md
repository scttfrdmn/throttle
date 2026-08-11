# ADR 0004: Scopes, legs, and where authority lives

Status: accepted

## Decision

A **scope** is `(budget_id, period_id)` — the smallest unit money is counted
against. A reservation is not one amount against one budget: it holds one **leg**
per scope it consumes, and a charge mirrors those legs. The ledger derives the
scope set by walking stored `parent_id` links, not from the caller.

The whole chain reserves inside one immediate transaction or none of it does.
Every scope must be given an explicit ceiling; a missing ceiling is an error
rather than an unlimited default.

The engine's admission calculation is **advisory**; the ledger is
**authoritative**. The ledger re-checks every ceiling inside the writing
transaction, and when it refuses, the engine refreshes totals and recomputes the
real outcome rather than assuming a lost race means "wait".

Enforcement posture (monitor / wait / enforce) is a property of the process doing
the spending, not an accounting fact about the budget, so it is not persisted.
Within a chain the strictest posture wins, and a monitored budget can never be the
reason a request is refused at any depth.

## Consequences

- a child cannot escape an ancestor's cap by failing to mention it
- concurrent children cannot oversubscribe a shared ancestor, because the
  ancestor's leg is written under the same transaction as the child's
- settlement and release each update the whole chain exactly once, enforced by
  composite primary keys rather than by application care
- a refusal can name which budget in the chain actually bound, which is the
  difference between "you are out of money" and "your parent is"
- the legs table is the seam for future overlapping, non-hierarchical constraints
  (per-model, per-workload, per-provider): those are more legs, not a new
  mechanism. None are implemented.
- deep hierarchies cost more work per reservation; depth is bounded (64) and
  cycles are refused
- because posture is not persisted, the CLI cannot report how a budget is really
  governed at the call site. Whether posture should become durable is an open
  product question.
