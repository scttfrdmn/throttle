# Architecture

```text
┌──────────────────────────────────────┐
│         future policy layer          │  deferred
│ routing / economy / quality / model  │
├──────────────────────────────────────┤
│            enforcement               │
│      allow / reserve / wait / deny   │
├──────────────────────────────────────┤
│             budgeting                │
│   pace / bank / borrow / rollover    │
├──────────────────────────────────────┤
│              ledger                  │
│ reservations / charges / attribution │
├──────────────────────────────────────┤
│          provider adapters           │
│ Bedrock / OpenAI / Anthropic / ...   │
└──────────────────────────────────────┘
```

The bottom four layers are v0.1.

## Request lifecycle

```text
native SDK request
      │
      ▼
provider adapter estimates billable usage/cost
      │
      ▼
budget engine calculates current envelope headroom
      │
      ▼
ledger atomically reserves estimate
      │
      ├── unavailable now but later affordable → wait/queue
      ├── impossible within envelope → deny
      ▼
request goes to provider
      │
      ▼
adapter observes returned usage/metadata
      │
      ▼
pricing converts usage to actual cost
      │
      ▼
ledger settles reservation to actual cost
```

## Why reservations matter

A check-then-spend design races under concurrency. Multiple requests could all observe the same remaining headroom and simultaneously oversubscribe it. Reservation therefore belongs to the transaction boundary and must be atomic in the durable ledger.

## Where authority lives

The budget engine's admission decision is **advisory**. The ledger is
**authoritative**: it re-checks every ceiling inside the transaction that writes
the reservation, and a caller that loses a race is refused there rather than in
the engine. When that happens the engine refreshes totals and recomputes the real
outcome — a lost race is not automatically a "wait", because the request may have
become impossible for the period.

This is why enforcement sits above the ledger in the layering but does not get to
be the last word. Two processes sharing a ledger cannot coordinate through engine
state; they can only coordinate through the database.

A request against a sub-budget consumes its own scope and every ancestor's scope,
where a scope is `(budget, period)`. All of those are written as legs of one
reservation in a single transaction, or none are. See
[`data-model.md`](data-model.md) for the representation and
[`decisions/0004-scopes-legs-and-authority.md`](decisions/0004-scopes-legs-and-authority.md)
for the reasoning.

Enforcement posture is a property of the process doing the spending, not of the
budget, so it is not persisted. Within a chain the strictest posture wins, and a
monitored budget never causes a refusal at any depth.

## Money

All persisted and compared cost values are integer microdollars:

```go
type Money int64
```

Human-readable decimal conversion happens only at boundaries.

## Provider boundary

Provider adapters may know SDK-specific request and response types. The budget and ledger packages may not.

Adapters are responsible for translating provider facts into normalized:

- estimated billable usage
- actual billable usage
- cost quote inputs
- model/access identity

The native SDK remains the user's mental model; throttle should wrap/intercept rather than invent a new universal generation API.

## Local first

v0.1 should work with no central service:

```text
application → throttle library → SQLite ledger
                           └────→ local dashboard/API
```

A later team version can swap in a shared ledger/service without changing budget semantics.
