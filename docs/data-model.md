# Data model

## Identity dimensions

Do not collapse these fields:

```text
access_provider   AWS Bedrock
publisher         Anthropic
model_family      Claude
model             provider/model version identifier
endpoint          specific endpoint/deployment/inference profile
region            region if meaningful
service_tier      tier if meaningful
workload          chat / agents / coding / batch / user-defined
principal         local user now; team identity later
budget_id         charge attribution
```

This permits both questions:

- "How much did I spend through Bedrock?"
- "How much did I spend on Claude across every access path?"

## Usage

The normalized structure must not assume all AI costs are simple text tokens.

Initial fields include:

```text
input_tokens
output_tokens
reasoning_tokens
cache_read_tokens
cache_write_tokens
```

It must also permit named billable dimensions for future image/audio/search/tool/request charges.

## Budget: definition versus period

A budget has two representations, and conflating them is the mistake this split
exists to prevent.

A **definition** is the durable recurrence rule — the thing a user writes down
once and every process shares:

```text
id
parent_id
name                     # excluded from the fingerprint; renaming is not a conflict
allocation               # per period
borrow_window
rollover_policy          # mode + cap OR cap_basis_points, never both
recurrence               # monthly / weekly / daily / duration / none
recurrence_length        # only when recurrence = duration
timezone                 # calendar boundaries fall in this zone
anchor_at                # start of the first period
end_at                   # end of the whole budget; required when recurrence = none
fingerprint              # hash of the semantic fields above
revision                 # optimistic-concurrency token for updates
```

Two processes must not be able to govern the same money by different numbers, so
storing a definition is idempotent for identical semantics and a conflict
otherwise. Changing the rules is an explicit update against an expected
`revision`.

A **period** is one materialized envelope of that definition:

```text
id                       # budget_id + sequence
budget_id
seq
start / end
allocation               # snapshot
carry                    # resolved to money; no percentages below this line
borrow_window            # snapshot
rollover_policy          # snapshot
state                    # open | draining | closed
carry_final              # is carry-IN settled, or still provisional?
closing_balance
closed_at
```

The snapshotted fields are what make history stable: editing a definition must
not retroactively rewrite what a closed period was allowed to spend. Percentage
rollover caps resolve to an amount when the next period is constructed, so no
proportional arithmetic reaches the ledger.

Monthly budgets are a recurrence/convenience layer over sequential envelopes;
"month" is not embedded in core pacing math. Gaps are materialized completely
rather than skipped, so carry chains through unused periods.

## Scopes and legs

A **scope** is `(budget_id, period_id)` — the smallest thing money is counted
against. A request against a child budget consumes its own scope *and* every
ancestor's scope, so a reservation is not one amount against one budget: it is
one **leg** per consumed scope, written in a single transaction or not at all.

```text
reservation_leg: reservation_id, budget_id, period_id, depth, amount
charge_leg:      charge_id,      budget_id, period_id, depth, amount
```

`depth` 0 is the named budget, 1 its parent, and so on. Totals for a scope are
sums over legs. The scope set is derived from the stored `parent_id` links rather
than supplied by the caller, so a request cannot escape an ancestor's cap by
failing to mention it.

The legs table is also the seam for future *overlapping*, non-hierarchical
constraints (per-model, per-workload, per-provider caps): those would be
additional legs, not a different mechanism. None are implemented.

## Reservation

A reservation is a **lease**, not an assumption about how long an LLM request
runs. It can be renewed while the work is live, and an expired lease stops
consuming headroom while remaining settleable — a slow-but-alive request really
did cost money.

```text
id
budget_id                # the named budget; the full set of scopes is in legs
request_id
amount                   # reserved
estimated_cost           # what the adapter predicted
created_at
expires_at
lease                    # renewal quantum
renewed_at
renew_count
state                    # pending | settled | released | expired
legs []
model_identity
metadata
```

## Charge

```text
id
reservation_id           # unique: settling twice is unrepresentable
request_id
budget_id
timestamp
estimated_cost
reserved_cost
actual_cost
legs []                  # money lands in the period that AUTHORIZED the request
normalized_usage
model_identity
latency
metadata
policy_actions []   # empty in v0.1, reserved for future visibility
```

`actual_cost` may exceed `reserved_cost`; the overrun is recorded, not hidden.

## Notes on representation

This document describes semantic fields. The SQLite implementation in
`ledger/sqlite/schema.go` normalizes them into `budgets`, `periods`,
`reservations`, `reservation_legs`, `charges`, and `charge_legs`, using STRICT
tables so a wrong column type is a write error rather than a silent conversion —
a REAL column would quietly round money. Money is always integer microdollars and
timestamps are always Unix nanoseconds with 0 meaning unset.
