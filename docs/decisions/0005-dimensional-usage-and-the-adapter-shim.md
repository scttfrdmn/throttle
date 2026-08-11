# ADR 0005: Dimensional usage, unknown cost, and the adapter shim

Status: accepted

## Decision

**Usage is a map of billable dimension to count**, not a struct of named token
fields. Unrecognized dimensions are stored and priced like any other, and the JSON
form is flat so a new dimension needs no migration. An absent dimension is not
zero. Provider-reported totals are display-only, because they sum dimensions that
are priced differently.

**Model identity is a set of independent dimensions**, not a `Provider → Model`
tree. Access provider, provider model ID, and operation are required; publisher,
family, canonical model, region, service tier, inference profile, and endpoint are
enrichment. The exact provider model ID is authoritative raw identity and is never
rewritten. An unrecognized model has empty canonical fields and remains fully
usable.

**Cost carries a `known` flag**, so `usage: known, cost: unknown` is
representable. Throttle never guesses a price, maps an unknown model to a similar
known one, or falls back to zero. An estimate additionally carries its quality —
exact, conservative, heuristic, historical, unknown — and no Bedrock `Converse`
estimate may be labelled exact, because `CountTokens` returns input tokens only.

**Prices are integer numerators over integer units, carrying provenance** (source,
version, effective date). Adapters embed no prices. Catalog lookups key on the
exact provider model ID, more specific matches (region, service tier) win, and a
local override beats a fixture at equal specificity.

**An adapter is a shim, not a replacement API.** It takes and returns the
provider's own SDK types and adds governance around them. There is deliberately no
common `Client` interface for adapters to implement, and translation is strictly
one-directional: provider response → normalized usage → pricing → `money.Money`.

## Consequences

- a provider that starts billing for something throttle has never heard of is
  accounted for rather than silently discounted
- both "spend through Bedrock" and "spend on Claude everywhere" are answerable,
  and a model released after a given build still works
- the budget engine never learns what a token is; the boundary is enforced by a
  test over the real dependency graph rather than by convention
- unpriced usage cannot masquerade as free, so aggregates stay honest — but
  throttle must have a *policy* for that state, which is a product decision
  (issue #17) and is currently a provisional refusal
- a caller keeps the full expressiveness of the provider SDK, including features
  throttle knows nothing about, at the cost of throttle being unable to offer a
  single portable call
- the fixture catalog will drift from real AWS prices; provenance makes the drift
  visible rather than arguable, and live price synchronization is deferred
- reservations are systematically larger than actual spend, because a conservative
  estimate bounds output at the token cap and most responses come in well under it
