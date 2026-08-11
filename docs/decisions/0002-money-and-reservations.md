# ADR 0002: Integer microdollars and reservations

Status: accepted

## Decision

Represent persisted money as signed integer microdollars and govern concurrent calls with explicit reservations.

## Consequences

- no float rounding drift in the ledger
- a request estimates and atomically reserves cost before execution
- settlement replaces the reservation with actual observed cost
- actual cost is authoritative even when it exceeds the reservation
- persistent stores must make reservation checks atomic
