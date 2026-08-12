# Data model

## Identity dimensions

Do not collapse these fields (`usage.ModelIdentity`):

```text
access_provider    aws-bedrock          # required: the path the request took
provider_model_id  anthropic.claude-... # required: verbatim, never rewritten
operation          converse             # required: the provider call made

publisher          anthropic            # enrichment
family             anthropic.claude     # enrichment
canonical_model                         # enrichment; empty = unrecognized
region                                  # access dimension, if meaningful
service_tier                            # access dimension, if meaningful
inference_profile                       # cross-region / ARN profile, if any
endpoint                                # specific deployment, if any
```

This permits both questions:

- "How much did I spend through Bedrock?"
- "How much did I spend on Claude across every access path?"

Only the first three are required. The provider model ID is authoritative raw
identity and the rest is enrichment, so a model released after a given throttle
build still works: an unrecognized model has an empty `canonical_model`, which is
a legitimate state rather than an error. A catalog that has never heard of a model
cannot stop a request.

Workload, principal, and `budget_id` are attribution rather than identity, and
live on the reservation/charge and on activity records — a single model can be
called by several workloads against several budgets.

## Usage

Normalized usage is a **map of billable dimension to count**, not a struct of
named token fields:

```go
type Dimension string
type Usage struct { /* Dimension -> int64 */ }
```

A struct would mean that a dimension throttle has not been taught about gets
dropped, and a dropped dimension is a real charge that silently disappears. So an
unrecognized dimension is stored and priced like any other, and the JSON form is
flat, so a new dimension needs no migration.

Shared vocabulary, not an exhaustive list:

```text
input_tokens
output_tokens
reasoning_tokens
cache_read_tokens
cache_write_tokens
```

Cache reads and writes are separate dimensions rather than adjustments to input,
because they carry separate prices. Provider-reported totals (Bedrock's
`totalTokens`, OpenAI's `total_tokens`) are display-only and never used for
accounting, since they sum dimensions that are priced differently.

**Dimensions are disjoint.** Every count is the number of units billed at *that*
dimension's rate and at no other, so the cost of a request is the sum over its
dimensions with no correction term. This is a property of throttle's normalized
form, not of any provider's wire format, and providers do report otherwise:
OpenAI's `input_tokens` is inclusive of the cached tokens broken out beneath it,
and its `output_tokens` is inclusive of reasoning tokens, which bill at the output
rate. An adapter facing an inclusive report must therefore **subtract** its way to
the normalized form rather than copying fields across.

Copying an inclusive total into a disjoint dimension double-charges the overlap,
and it does so silently: the resulting cost is plausible, internally consistent,
and wrong by the price of the cached prefix. Which subset is included in which
total is a per-provider fact that changes without notice, so it belongs in an
adapter — pinned by a test that prices a response whose overlap is large enough
that double-charging cannot be mistaken for rounding.

Decomposition never produces a negative count. A report whose parts exceed their
stated total is a provider contract violation rather than a negative dimension, and
is floored at zero.

An **absent dimension is not zero**. "The provider did not report cache reads" and
"the provider reported zero cache reads" are different facts, and only the second
one licenses a claim about caching.

### Canonical integer units

Counts are `int64`, and the invariant that makes that safe is: **every dimension
fixes one canonical integer atomic unit, where the dimension is defined.** Tokens,
requests, and images are naturally integer. Hosted-runtime resource time is not — a
provider reports `0.5` vCPU-hours — so its canonical unit is scaled instead:

```text
runtime_vcpu_nano_hours        # billionths of a vCPU-hour
runtime_memory_nano_gb_hours   # billionths of a GB-hour
```

A provider's decimal is converted once, at the edge, by an exact rational conversion
truncated toward zero:

```go
usage.Nano("0.5")   // -> 500_000_000
const usage.NanoScale = 1_000_000_000
```

Truncation rather than rounding keeps the residual bounded by one nano-unit per
observation and one-directional, which at hosted-runtime rates is many orders of
magnitude below a microdollar. It is toward zero rather than downward, so the same
"never exceed what the provider reported" property holds for a negative quantity:
`-0.0000000015` becomes one nano-unit, not two. The input is decimal *text*, never a
`float64`, because that is how a provider actually delivers it and parsing through a
float would introduce a representation error before any accounting happened.

Two consequences, recorded because they are public semantics rather than
implementation details. Digits below a nano-unit are silently dropped, so a
normalized quantity is a floor and not an exact echo of the telemetry. And exact
parsing accepts everything `big.Rat` does, which includes hexadecimal and rational
spellings no provider emits, so a malformed field that happens to look like one
converts rather than erroring. Tightening either would change behaviour callers can
observe. Pricing quotes these dimensions per nano-unit
(`pricing.PerNanoUnit`), so a published dollars-per-unit rate stays exactly
representable and the dimension count is what gets multiplied.

The scale is a billionth of **the unit the provider prices in**, not a physically
atomic unit such as a byte-second. That is deliberate: a byte-second would require
throttle to decide whether a "GB-hour" means 10⁹ or 2³⁰ bytes, which providers
routinely leave implicit, and guessing wrong would scale every memory charge by
several percent permanently, inside the durable stored unit. Nano-units of the
provider's own unit preserve the provider's meaning verbatim and leave that
definition with whoever bills.

`Usage` therefore stays integer-valued. A future dimension that resists exact integer
representation is a signal to **design its unit**, not to widen the map to `float64`.

## Cost

Cost is not a bare amount, because "unpriced" must not be expressible as zero, and
"partly priced" must not be expressible as a total:

```text
amount        # microdollars; a TOTAL when known, a FLOOR when partial
completeness  # known | partial | unknown
unpriced []   # the nonzero dimensions that had no rate; sorted
reason        # why the cost is not fully known
```

Completeness is three-valued rather than a boolean. A boolean would let "some
dimensions priced" be read as "complete cost known", which is the single most
expensive confusion available in this data model: it turns an understatement into
an apparently authoritative number.

- **known** — every reported nonzero dimension had a rate. `amount` is the total.
- **partial** — some dimensions priced, at least one nonzero dimension did not.
  `amount` is a floor, and `unpriced` names what is missing so a later
  reconciliation knows which prices it needs. This renders as `$812.41+`.

  Partial also covers a charge that is not a *dimension* at all: a provider-hosted
  tool whose fee is levied per call, per stored gigabyte, or per session, and
  reported nowhere in the response. `unpriced` is then empty while the cost is still
  incomplete, because nothing throttle could price was missing a rate — the charge
  simply never appeared in the usage report. `reason` carries what `unpriced` cannot
  say. A request whose provider charges arrive partly outside the usage object is
  never "fully priced", and enforcement must not treat its token cost as its cost.
- **unknown** — nothing could be priced. There is no amount; the JSON form omits
  the field entirely, so nothing that ignores completeness can read a stored zero
  back as a free request.

The **zero value is unknown**, not a free request: a `Cost` nobody filled in is the
most likely accounting bug in the system, so it defaults to the state that cannot
silently understate. Because the zero value of the completeness field is the empty
string, code compares through an accessor that resolves it rather than against the
field directly.

A **known zero and an unknown cost are different facts** and never render the same
way. A dimension reported with a zero count needs no rate — nothing was consumed,
so nothing is owed, and its absence from the quote is harmless.

`usage: known, cost: unknown` is a first-class state, reached when the catalog has
no price for a model or no rate for a dimension the provider actually billed.
Throttle does not guess a price, substitute a similar model, or fall back to zero.
What throttle *does* in that state is settled — see
[`architecture.md`](architecture.md#unknown-cost-is-representable).

## Pricing quote

A **captured quote** is the immutable rate set frozen at admission and replayed to
price actual usage at settlement:

```text
access_provider
provider_model_id        # verbatim
region / service_tier    # the access dimensions the rates were selected for
rates {dimension: rate}  # integer numerator over integer unit; copied, not referenced
provenance               # source, version, effective_from, currency
captured_at
alternates {tier: quote} # the model's other priced tiers, same catalog read
```

It is throttle's own value type, persisted as JSON. **No provider SDK object is
ever serialized into it**, because a historical request must stay reproducibly
priceable across catalog updates, price changes, and application restarts, and an
SDK type is none of those things' concern.

The rates are *copied* out of the catalog rather than referenced, which is the whole
point: a captured quote must not change when the catalog it came from does.
Provenance travels inside the quote, so a rate supplied by a negotiated or local
override is still identifiable as such after the fact.

`alternates` exists because a provider may serve a request on a service tier the
caller did not request, at a different price. The alternates are collected in the
**same catalog read** as the primary quote, so selecting one at settlement is a
replay of frozen rates rather than a fresh query. Alternates are one level deep: an
alternate never carries its own.

Pricing a quote accumulates every dimension of the charge as one exact rational and
rounds once — see
[`architecture.md`](architecture.md#one-charge-one-rounding).

### Pricing quote set

A **captured quote set** is the same guarantee for a request whose model identities are
not knowable before it runs — a managed agent invocation names an agent, and discovers
which foundation models it invoked from the response:

```text
access_provider
quotes {provider_model_id: captured_quote}  # each with its own rates and provenance
captured_at                                 # ONE instant, shared by every member
note                                        # why a set is empty or narrow
```

Every member is taken in **one catalog read at one instant**, narrowed to the region and
tier the request will run under. `captured_at` being shared is the evidence of that: it
is what distinguishes one read from an accumulation of several, which is precisely the
failure a captured quote exists to prevent.

Settlement looks a model up in the frozen set and never re-queries the catalog. A model
absent from the set has no price *for this request*, whatever the catalog says now, and
that flows into the ordinary partial/unresolved semantics.

Exactly one of `quote` and `quotes` is populated on a record. Which one is a property of
the operation, not a fallback: a direct model invocation always knows its model, and a
managed agent invocation never does. Both replay frozen rates.

The set persisted on a record is **only the subset covering invocations that actually
happened**. The full captured set may cover every model the catalog prices, and the
models a request never touched are not part of its accounting story.

Pricing a set of observed invocations accumulates all of them into one exact rational
and rounds once, extending the single-rounding rule across invocations as well as
dimensions — see
[`architecture.md`](architecture.md#one-compound-charge-still-one-rounding).

An **estimate** additionally carries its own quality, so a reservation's
trustworthiness travels with it:

```text
exact         # the provider told us, preflight
conservative  # a genuine upper bound
heuristic     # a guess that can undercount
historical    # derived from past observations
unknown       # no basis
```

**No estimate of a generative request is `exact`, on any provider.** Output tokens
cannot be known before generation, so the output half of any estimate is a declared
ceiling at best. Bedrock's `CountTokens` and OpenAI's input-token count endpoint both
cover the input half only, and neither vendor documents its count as the number that
will be billed — so even the input half is `conservative` rather than exact. `exact`
is reserved for a preflight figure a provider states will be billed, which is not on
offer today.

The label is not cosmetic. It is what a later reader uses to decide whether an
estimate's divergence from actual is a bug or the expected behaviour of a guess, and
calling a count exact because it came from an API would make that judgement
impossible.

An estimate is also only as complete as the modalities it covers. A text tokenizer
says nothing about an image, an audio second, a file the provider parsed, or a
hosted tool's fee, so a request carrying those has monetary exposure the token count
does not describe — an incompleteness of the estimate, distinct from its accuracy.

A managed agent invocation is weaker still. throttle cannot know how many foundation
models the agent will invoke, so no token count is available to bound and `CountTokens`
does not help. The estimate is the caller's **declared ceiling**, and its quality is
`heuristic` rather than `conservative` — AWS does not stop the agent at throttle's
number, and labelling a figure a bound that the provider will not enforce would be
false. With no declared ceiling the estimate is `unknown`, and posture decides:
enforce denies before the provider is called, monitor admits with a zero hold.

## Activity

`activity.Record` is the durable per-request record behind the dashboard. One row
per request, keyed on `request_id`, so a retry after an ambiguous failure updates
its own record rather than creating a rival account of the same call:

```text
request_id               # primary key
reservation_id
budget_id
scopes []                # (budget_id, period_id, depth) per consumed scope

identity                 # the full ModelIdentity; the queryable dimensions are
                         # also columns: access_provider, publisher,
                         # canonical_model, provider_model_id, operation,
                         # region, service_tier

estimate                 # estimated usage, quality, note
estimated_cost           # with completeness
quote                    # the captured quote, with provenance
quotes                   # the captured quote SET, when the models were not
                         # knowable at admission; exactly one of quote/quotes
reserved_cost

actual_usage             # every dimension the provider reported, verbatim
actual_cost              # with completeness and unpriced dimensions
cost_completeness        # denormalized out of actual_cost, and indexed
cost_amount              # denormalized

enforcement_mode         # the posture that ACTUALLY governed this request
status                   # settled | denied | released | unresolved | outstanding | pending
outcome                  # success | budget-denied | unpriced | provider-error |
                         # cancelled | timeout | accounting-error
error                    # why, when something failed

started_at / completed_at
latency_ns               # wall clock, throttle's view
provider_latency_ns      # what the provider itself reported
stream_established_ns    # 0 for a non-streaming request; how long until a stream
stream_first_event_ns    # existed, and how long until it said anything

agent                    # compound detail for a managed agent invocation; the
                         # queryable dimensions are also columns: agent_id,
                         # agent_alias_id, agent_session_id
runtime                  # reconciliation linkage for a hosted runtime invocation;
                         # the queryable dimensions are also columns: runtime_id,
                         # runtime_qualifier, runtime_session_id,
                         # runtime_reconciled
repairs                  # append-only trail of automatic crash reconciliation
metadata                 # caller attribution: workload, principal, ...
```

The two streaming durations are separate from `latency_ns` because a stream's total
duration says nothing about its responsiveness: a slow stream that answered
immediately and a fast one that stalled at the start are indistinguishable
otherwise. `stream_first_event_ns` is time-to-first-token in practice. Both are
durations, never content.

**Enforcement mode is recorded per request** even though it is not a property of
the budget, because spend admitted under monitor mode was never subject to a
ceiling and no reader could infer that afterwards. It remains runtime policy and is
deliberately *not* part of the immutable recurring budget definition.

The statuses are distinguishable because they mean genuinely different things:

- **pending** — written before the provider call. A pending row that never resolved
  is what a crashed process leaves behind, and it is the only evidence that money
  may have moved.
- **settled** / **released** — resolved. A released request was refused by the
  provider with no usage reported, so a known zero cost is the truth.
- **denied** — the budget or a pricing gap stopped it; nothing was spent. Recorded
  anyway, because "the budget stopped this" is invisible otherwise.
- **unresolved** — it ran, its usage is known, its cost is not fully priceable. The
  reservation stays encumbered.
- **outstanding** — it ran and the outcome is genuinely ambiguous (cancelled,
  timed out, or a stream that ended before reporting usage). The hold stays,
  because the provider may have served and billed it.

A managed agent invocation adds no statuses either. It reaches **unresolved** more often
than a direct call does, because a turn is only as priceable as its least priceable
internal invocation: one step on a model the captured set cannot price, or one step whose
model the provider never named, makes the whole outer transaction partial.
`operation` is `invoke-agent`, which is what tells a later reader that a stranded row
belongs to a long-running compound turn.

Streaming uses exactly these statuses and adds none. It reaches **outstanding** more
often than a single round trip does, because Bedrock reports usage only in the
terminal metadata event: a stream that is closed, cancelled, abandoned, or broken
before that event ran and cannot be measured. `operation` distinguishes such a
record — `converse-stream` rather than `converse` — which is what tells a later
reader that a stranded `pending` row belongs to a long-lived stream.

A hosted runtime invocation adds no statuses and reaches **unresolved** on *every*
path that touched the runtime, success included, because its resource cost is never
reported synchronously. `operation` is `invoke-agent-runtime`. That makes unresolved
the normal terminal state here rather than an exception, which is why the reconciled
fields are separate and why `runtime_reconciled` is indexed: the outstanding work is a
query, not an anomaly to be discovered.

### Compound agent detail

A managed agent invocation is **one** governed request that internally invoked a
foundation model several times. The transaction, the reservation, and the charge stay
singular; the internal invocations are recorded as detail beneath them:

```text
agent:
  agent_id / alias_id / version   # what ran, as the provider named it
  session_id                      # telemetry dimension; groups records, not money
  steps []                        # the observed model invocations, in order
  events {kind: count}            # non-model activity, COUNTED and never priced
  note                            # an accounting limitation of this invocation

agent_step:
  seq                             # order within the turn, from 1
  kind                            # pre-processing | orchestration | post-processing |
                                  # routing-classifier | failure
  trace_id                        # the provider's correlation id; an opaque identifier
  collaborator                    # the delegated agent, when one performed the step
  identity                        # the model this step ran on; may be UNNAMED
  usage                           # what this step consumed
  cost                            # this step's contribution, rounded for reporting
  latency / at
```

There are **no child reservations and no child charges**. throttle admitted the outer
invocation, not the individual model calls, and a reservation for a call it never
admitted would misrepresent what was governed.

The step `cost` figures are presentation. The turn is accumulated exactly and rounded
once, so the steps may not sum to `actual_cost`, and `actual_cost` is the authoritative
number — the same relationship the per-dimension breakdown has to a single charge.

A step's `identity.provider_model_id` **may be empty**. Model identity arrives on the
input event and usage on the output event, joined only by trace ID; a step whose usage
arrives without a matching input event is real spend with no identity. Recording the
agent's configured model instead would be a guess presented as a fact. An unnamed model
is a representable state and makes the turn unpriceable, hence unresolved.

`events` are counts and nothing more, because that is all the provider gives: action
groups, knowledge-base lookups, guardrail evaluations, and code interpretation carry no
billable quantity throttle can price. Their real cost lands on the provider's bill
outside throttle's view, and `note` says so rather than implying zero.

**No field here can hold content.** The provider's trace also carries prompts, model
responses, reasoning, rationale, action inputs and outputs, retrieved passages, and
collaborator messages. All of it is forwarded to the caller; none of it is persisted.
Raw trace objects are never serialized — every field above is a measurement or an
identifier, so this is content-free by construction and not by the writer's care.

### Hosted runtime linkage

A hosted runtime invocation is the opposite case from a managed agent: rather than one
transaction with priced detail beneath it, it is one transaction whose cost is **not
known at all** when it closes. So `runtime` holds no cost of its own. It holds the
identifiers a later resource observation can be recognized by, and the outcome of the
call they belong to — a join key with provenance:

```text
runtime:
  runtime_id / qualifier / account  # what ran, and which endpoint alias
  session_id                        # the runtime session; see below
  request_id / trace_id             # the platform's ids for this single call
  status_code / content_type        # a status and a MIME type, not a payload
  payload_bytes / response_bytes    # SIZES of bodies that were never retained
  reconciled                        # has authoritative resource usage arrived
  reconciled_usage / reconciled_cost
  reconciled_at / reconciled_from   # when, and from which source
  note                              # the accounting limitation of this invocation
```

`session_id` is the load-bearing field and the reason the type exists. On AgentCore it
is the only identifier appearing both on the API call and in the resource-usage
telemetry — those records carry no request ID and no trace ID — so a session is the
finest granularity at which delayed CPU and memory usage can be attributed to
anything. `request_id` and `trace_id` are more precise but join to application logs
and spans, not to resource usage; they are what an operator quotes when asking the
provider what happened.

A session may span several invocations, and its resource bill includes start-up, idle
time, and platform overhead that no single invocation caused. throttle therefore
**records which session an invocation belonged to and does not divide the session's
cost across them.** A computed share would be indistinguishable in the record from a
measured cost, and the attribution rule is a product decision to be made once and
deliberately rather than implied by an adapter.

`reconciled_usage` and `reconciled_cost` are deliberately separate from the record's
`actual_usage` and `actual_cost`. A figure that arrived later — from telemetry the
platform itself describes as approximate — must never be readable as something
throttle measured at the time of the call, and must never overwrite something it did.

**No field here can hold a payload.** A hosted runtime's request and response are
opaque bytes in a caller-declared format; the platform imposes no message structure
and neither does throttle. `payload_bytes` and `response_bytes` are counts, which is
what an operator needs to see a runaway payload without the payload being stored in
order to see it.

One field is absent on purpose: the platform's **runtime user ID**. It is forwarded to
the provider verbatim, because throttle does not alter the caller's own request, but it
is not persisted. It is caller-supplied, frequently an end-user identifier, and storing
identifying data merely because an SDK exposes it is not a decision an adapter should
make silently. A caller who wants attribution has `metadata`, where the choice is
explicit and theirs.

### Reconciliation trail

`repairs` is the append-only record of automatic crash reconciliation applied to a
row after the fact:

```text
repairs[]:
  at                                # when the repair ran
  class / reason                    # the outcome, and the durable classification
  observed_status                   # THE STATE RECONCILIATION FOUND
  observed_reservation
  produced_status                   # the state it wrote
  money / amount                    # "settled", "released", or neither
  quote_source / quote_version      # which immutable quote priced a replayed
  quote_captured_at                 # settlement
  detail                            # one sentence, for an operator
```

The observed pair is the point of the whole structure: after a repair overwrites the
row, the state that needed repairing is not recoverable from it, and that is the first
thing anybody investigating a crash asks for. So the trail is appended to and never
rewritten — a repair that destroyed the evidence of what it found would make the
postmortem impossible.

It is JSON on the row rather than a second table because it is only ever read with
its own record, never queried across records, and a repair is meaningless without the
row it repaired. Deliberately not a general audit subsystem.

Every field is a status, an amount, an identifier, or a timestamp. Recovering from a
crash never requires content, which is what makes a content-free activity record
sufficient to be crash-safe.

`scopes` is a separate table, so a parent budget can total its descendants' activity
without re-deriving the hierarchy. `cost_completeness` is denormalized and partially
indexed so that "3 unpriced requests" is a query rather than a JSON scan of every
row.

**Prompt and response content is deliberately absent**, and no column exists that
could hold it. Recording what a request cost does not require recording what it
said; usage and cost observability is content-free by construction rather than by
the writer's discretion.

A summary over records reports incompleteness rather than hiding it — spend as a
floor, a count of unresolved requests, the encumbered amount, and the union of
unpriced dimensions — so a reader can decide whether to trust the total:

```text
Spend:                        $812.41+
Unpriced/unresolved requests: 3
Budget status:                incomplete
```

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

A reservation retained for an **unresolved cost** needs no new representation: it
stays `pending`, and the pending reservation *is* the encumbrance record. Marking a
transaction unresolved therefore writes nothing to the ledger. Releasing such a
reservation is refused, since that would hand already-spent money back as
available; settling it is not, so reconciliation once a price arrives is the
ordinary settlement path.

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

Activity lives in `activity/sqlite` under the same conventions, in `activity` and
`activity_scopes`, on its own database handle. Usage maps, cost objects, quotes, and
metadata are stored as JSON within that schema, deliberately: a dimension or a rate
throttle has never heard of must survive a round trip without a migration.
