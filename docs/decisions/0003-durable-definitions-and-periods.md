# ADR 0003: Durable budget definitions and materialized periods

Status: accepted

## Decision

Split a budget into two persisted things:

- a **definition** — the recurrence rule (allocation per period, borrow window,
  rollover policy, recurrence, timezone, anchor, end), stored once and shared by
  every process using the ledger
- a **period** — one materialized envelope of that definition, carrying its own
  snapshot of allocation, carry, borrow window, and rollover policy

A definition is identified by a fingerprint over its semantic fields (excluding
the display name). Storing one is idempotent for identical semantics and a
conflict otherwise. Changing the rules requires an explicit update against an
expected revision.

Periods advance through `open → draining → closed`. A charge lands in the period
that **authorized** its reservation, even when settlement arrives after the
boundary. A successor may open before its predecessor is final, on a provisional
carry computed as if every outstanding hold settles in full; finalizing the
predecessor can only revise that carry upward.

## Consequences

- budget definitions survive process restart, and two processes cannot silently
  govern the same money by different numbers
- editing a definition cannot retroactively rewrite what a closed period was
  allowed to spend, because the period holds its own snapshot
- correspondingly, raising a budget mid-period has no effect until the next
  period; re-snapshotting an open period is a deferred product decision
- percentage rollover caps resolve to an amount when the next envelope is
  constructed, so no proportional arithmetic reaches the ledger
- work done in August is August's cost and never consumes September's money
- provisional carry can only understate what is available, never overstate it, so
  a boundary is never a window in which a budget over-admits
- gaps are materialized completely rather than skipped, so carry chains through
  unused periods
- a long request that genuinely spans a boundary is attributed wholly to the
  authorizing period; time-apportioned splitting is a different accounting rule
  and is deferred
