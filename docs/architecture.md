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
provider adapter estimates billable usage
      │
      ▼
pricing CAPTURES a quote — the rates are frozen here
      │
      ▼
estimated cost
      │
      ▼
budget engine calculates current envelope headroom
      │
      ▼
ledger atomically reserves estimate
      │
      ├── unavailable now but later affordable → wait/queue
      ├── impossible within envelope → deny
      ├── cost unpriceable and posture enforces → deny
      ▼
activity record written (pending) — before the provider is called
      │
      ▼
request goes to provider
      │
      ▼
adapter observes returned usage/metadata
      │
      ▼
the CAPTURED quote converts usage to actual cost
      │
      ├── cost not fully priceable → hold stays encumbered, marked unresolved
      ▼
ledger settles reservation to actual cost
      │
      ▼
activity record resolved (status, outcome, usage, cost, mode)
```

The quote is captured before admission and replayed at settlement rather than
re-queried, so a price refresh landing mid-request cannot change the basis a
request was accounted on. See [Pricing](#pricing).

The activity record is written before the provider call and updated after it, so a
process that dies mid-request still leaves evidence that money may have moved.

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
budget, so it is not part of the durable budget definition. Within a chain the
strictest posture wins, and a monitored budget never causes a refusal at any depth.

The posture that *actually governed a given request* is nevertheless recorded on
that request's activity record. Spend admitted under monitor mode was never subject
to a ceiling, and no reader could infer that afterwards from the budget alone. This
is a fact about one request, not a rule about the budget, which is why it lives on
the activity record and nowhere else.

## Money

All persisted and compared cost values are integer microdollars:

```go
type Money int64
```

Human-readable decimal conversion happens only at boundaries.

## Provider boundary

Provider adapters may know SDK-specific request and response types. `budget`,
`ledger`, `money`, `pricing`, `usage`, `engine`, and `activity` may not. The invariant
is enforced by a test over the real dependency graph (`provider/boundary_test.go`), not
just by convention.

`activity` is on that list for a second reason beyond neutrality. A managed agent
turn's detail is normalized out of a provider trace carrying prompts, responses, and
reasoning; serializing the SDK's own trace object would be the shortest path to writing
that content to disk. An SDK import in `activity` would be exactly that mistake, so it
is a build failure rather than a review comment.

The chain the boundary exists to protect:

```text
provider response → normalized Usage → pricing → money.Money
```

The budget engine only ever sees the right-hand side. It does not know whether a
dollar was spent on tokens, images, or seconds of audio.

The native SDK remains the user's mental model; throttle wraps and intercepts
rather than inventing a new universal generation API. There is deliberately no
common `Client` interface for adapters to implement: a caller who wants Bedrock's
`Converse` gets Bedrock's `Converse`, with governance around it.

### Streaming responses

A streaming call is the same lifecycle with one fact changed: usage arrives only in
a terminal metadata event, so the adapter has to observe the *end* of the response to
account for the request at all.

throttle therefore proxies the event path. It reads the provider's stream and
forwards each event to the caller over an unbuffered channel, so delivery stays
incremental and a slow caller gets ordinary backpressure. Nothing is accumulated,
replayed, or inspected beyond the metadata event, and no content is persisted. There
is deliberately no accessor for the raw stream: an unobserved read path is an
unaccounted request.

Exactly one goroutine owns the response — reading, closing the provider's stream,
renewing the lease, and performing the terminal accounting under a `sync.Once`. Every
way a stream can end (drained, provider error, caller `Close`, context cancellation,
an abandoned reader) funnels into that owner, so a stream reaches exactly one
terminal state regardless of how many of those happen at once.

The accounting consequence is asymmetric on purpose. A stream whose metadata event
was observed settles from the captured quote, exactly as a non-streaming response
does, even if the stream then failed — reported usage is authoritative. A stream that
ended before that event keeps its reservation encumbered and records the outcome as
unknown. Releasing would assert that nothing was spent, and a caller who closed,
cancelled, or walked away has demonstrated something about the caller, not about the
model. Only a call that failed before any stream existed releases its hold.

Because a stream can outlive a reservation lease, the hold is renewed on a timer at a
fraction of the lease quantum while the stream is alive, and the renewal stops at the
terminal state. A renewal failure does not tear the stream down: an expired
reservation can still settle, so killing the stream would forfeit the usage still to
come and recover nothing.

### Managed agent invocations are compound transactions

A managed agent invocation — AWS Bedrock Agents Classic `InvokeAgent` — changes two
more facts. The caller names an *agent*, not a model, and the service invokes
foundation models several times internally: preprocessing, one or more orchestration
steps, a routing classifier, any collaborator it delegates to, postprocessing.

throttle governs the outer invocation. That is the granularity at which it actually
admitted spend, so the transaction, the reservation, and the charge are all singular.
The internal model invocations are recorded as **accounting detail beneath one
transaction**, not as transactions of their own: throttle never admitted them
independently, and a child reservation for a call throttle did not admit would
misrepresent what was governed.

Per-invocation usage is only reported in the service's trace, so throttle enables the
trace — on a copy of the caller's input, never the caller's own struct. The trace also
carries prompts, model responses, reasoning, rationale, action inputs and outputs,
retrieved passages, and collaborator messages. All of it is forwarded to the caller and
**none of it is persisted**. Raw trace objects are never serialized; the adapter
extracts normalized accounting metadata — step kind, trace ID, collaborator name, model
identity, usage, cost, timing — into types that have nowhere to put content.

Model identity is joined across two events: the input event names the foundation model,
the output event reports the usage, and a trace ID is the only thing linking them. A
step whose usage arrives without a matching input event has real spend and no identity.
That is recorded as an unnamed model, which makes the turn unpriceable and therefore
unresolved. Substituting the agent's configured model would be a guess presented as a
measurement.

Non-model activity — action groups, knowledge-base lookups, guardrail evaluations,
code interpretation — is **counted by kind and never priced**. The service reports no
billable quantity for it, so its real cost lands on the provider's bill outside
throttle's view. The activity record says so in a note rather than implying zero. The
first goal is foundation-model spend; the dimensional usage design leaves the extension
open.

Pre-admission estimation is fundamentally weaker here than for a direct model call.
throttle cannot know how many models the managed agent will invoke, so there is no
token count to bound. The estimate is the caller's declared ceiling, labelled a
**heuristic** rather than a bound: AWS will not stop the agent at throttle's number,
and claiming otherwise would be false. With no declared ceiling the pre-cost is
unknown, which flows into the existing posture rules — enforce denies before the
provider is called, monitor admits with a zero hold.

Returning control to the caller is a **successful protocol outcome**, not an error.
Whatever the agent consumed getting there settles, and a follow-up `InvokeAgent` is a
new throttle transaction. The session identifier is retained as a telemetry dimension
so several turns can be grouped, but the ledger is not session-scoped: each API
invocation remains its own transaction against the budget.

### A hosted agent has two accounting positions

A hosted agent runtime — AWS Bedrock AgentCore `InvokeAgentRuntime` — is not a
managed agent with a compound transaction. The service runs *the caller's own code*,
in the caller's own container, and bills for the compute it consumed. It reports no
model usage at all, because it does not know which models that code called.

That splits governance into two positions, and the distinction is architectural
rather than an implementation detail of one adapter:

**The outer position is the edge.** throttle wraps the invocation, so it sees that a
runtime ran, for which budget, in which session. It governs *admission* of the
invocation and preserves the identity a later resource observation can be joined to.
It does **not** see the model calls made inside the agent, and it must not pretend
otherwise. The outer position is worth having for admission, attribution, streaming
lifecycle, and delayed resource reconciliation — not for real-time model enforcement.

**The inner position is the agent's own code**, which invokes models directly and is
governed by the ordinary core: the same engine, the same provider adapters, the same
records. This is the **preferred mechanism for real-time LLM budget enforcement**,
because throttle sees each model transaction *before* it happens — which is strictly
more than any edge wrapper can offer.

There is deliberately **no AgentCore-specific model-governance engine**: no hosted
budget type, no hosted model ledger, no second provider path. Hosting location does
not change model-spend governance, and the provider-neutral core works identically
inside a runtime and outside one. That claim is load-bearing, so it is held in place
by a test rather than by this paragraph
(`provider/bedrock/agentcore_embedded_test.go`).

Both positions charge the same budget and remain distinct transactions. Neither
reinterprets the other, so an operator gets a complete figure for model spend and an
explicitly incomplete one for runtime resource — rather than one number that looks
complete and is not.

### Hosted runtime cost is unknown at the edge, always

The outer invocation returns an opaque HTTP body and no usage of any kind. Resource
consumption arrives later, out of band, through provider observability, and the
provider itself describes those figures as approximate.

So an edge invocation's cost is **unknown on every path** — success included. It is
never zero, and it is never derived from something merely available:

- not from wall-clock latency, because billing commonly excludes time the agent
  spends waiting on a model or a tool,
- not from configured CPU and memory, because allocation is not consumption,
- not from response size, which has no relationship to compute at all.

Any of those would be an invented charge wearing a measurement's clothes. Unknown is
the honest state, and throttle already represents it as a first-class one.

Enforcement therefore falls back to the existing cost-unknown posture rules: under
enforcement the caller must declare an exposure for the invocation to be admitted;
under monitoring it is admitted and flagged. What the caller declares is a
**reservation exposure** — how much budget headroom to encumber — and pointedly not a
cost, an estimate, or a ceiling the provider will honor. AWS will not stop a runtime
at throttle's number. The API field is named `MaxExposure` so that its name cannot
imply a guarantee that does not exist.

Because the cost is never knowable at settlement, every invocation that reached the
runtime is retained as an unresolved liability: the exposure stays encumbered under
the ordinary lease, and reconciliation closes it if authoritative usage ever arrives.
The one path that releases instead is a call that failed before the runtime ran, where
nothing was consumed.

### Delayed resource usage is reconciliation, not a second mechanism

Normalized runtime usage can be reconciled onto an existing activity record long
after the invocation closed. The record keeps the identity needed to join a later
observation — runtime, endpoint qualifier, session, region, provider request and
trace IDs — and reconciled usage and cost land in **separate fields** from the actual
ones, so an enriched estimate can never be mistaken for what was measured at the time.

v0.1 builds the seam and not the ingestion: there is no observability polling, no log
consumer, no billing-export reader, and the adapter has no dependency on any
observability API. The general shape — *unresolved transaction → later authoritative
observation → reconciliation* — is the same one crash recovery needs, and the two are
expected to share a mechanism rather than each grow their own.

One constraint is deliberate and permanent: where a provider reports resource
consumption per *session* rather than per invocation, that granularity is preserved
as reported. A session's bill is **never apportioned across the invocations that
shared it**, because a computed share would be indistinguishable in the record from a
measured cost. How a session-granular figure should be attributed is an open product
decision, not something an adapter gets to settle.

### Usage is dimensional

Normalized usage is a map of billable dimension to count, not a struct of named
token fields. A provider that bills for something throttle has never heard of is
still representable, because dropping an unrecognized dimension would silently drop
a real charge. `input_tokens`, `output_tokens`, and the cache dimensions are shared
vocabulary, not an exhaustive list.

Cache reads and writes are separate dimensions rather than adjustments to input,
because they carry separate prices.

### Every dimension has a canonical integer unit

Usage counts are `int64`, and hosted-runtime resource dimensions are the first case
where the provider reports a *decimal* — `0.5` vCPU-hours — rather than a count. The
invariant holds anyway, and no float enters the data model: each dimension fixes one
canonical integer atomic unit where it is defined, and the provider's decimal is
converted into it exactly.

For resource-time the atomic unit is one **billionth of the unit the provider prices
in** (`runtime_vcpu_nano_hours`, `runtime_memory_nano_gb_hours`). Conversion is exact
rational arithmetic truncated toward zero, so error is bounded by one nano-unit per
observation and never accumulates in the wrong direction.

The scale is a billionth of the *provider's* unit rather than a physically atomic one
such as a byte-second, and that is the substantive choice. A byte-second would force
throttle to decide whether a "GB-hour" means 10⁹ or 2³⁰ bytes — something providers
routinely leave implicit — and guessing wrong would scale every memory charge by
several percent, permanently, inside the durable unit. Nano-units of the provider's
own unit never need to know.

Pricing converts canonical integer units into money; it does not receive a generic
floating-point quantity. If a future dimension cannot be represented exactly this
way, the answer is to design its unit, not to widen usage to `float64`.

### Identity is dimensional too

Model identity is a set of independent fields, not a `Provider → Model` tree:

- **access provider** — the path the request took (`aws-bedrock`)
- **publisher** — who made the model (`anthropic`)
- **provider model ID** — the exact string sent to the provider, verbatim
- **operation** — the call made (`converse`)
- optional **family**, **canonical model**, **region**, **service tier**,
  **inference profile**, **endpoint**

Access provider and publisher are independent because both questions are real:
"how much did I spend through Bedrock?" and "how much did I spend on Claude,
everywhere?"

Only access provider, provider model ID, and operation are required. **The provider
model ID is authoritative raw identity; canonical naming is enrichment.** A model
released this morning must remain usable by a build from last month, so an
unrecognized model is a legitimate state and never an error.

## Pricing

Pricing is data with provenance, not business logic. A quote records its source,
version, and effective date, so a surprising number can be traced rather than
argued about. Rates are integer numerators over integer units — a $3.00/million
rate is `PerUnit=3_000_000, Unit=1_000_000` — and costs are computed in exact
arithmetic. No floating point appears anywhere in the path.

Adapters never embed prices. They report what was consumed; pricing decides what it
was worth. That is what makes a local price override possible without touching an
adapter.

### One charge, one rounding

Every priced dimension of a single charge accumulates into one exact rational
(`math/big.Rat`), and that total is rounded to microdollars exactly once, half away
from zero and symmetric across sign. Rounding per dimension instead would drift
systematically in one direction: a hundred half-microdollar lines would round to
one microdollar each and bill double, and forty tenth-microdollar lines would round
to nothing each and bill zero. The rounding boundary is the charge, not the line.

The per-dimension breakdown is still reported for auditing, and each of its entries
*is* rounded individually — so the breakdown may not sum exactly to the total. That
discrepancy is the drift the charge-level total exists to avoid, and the total is
the authoritative number.

### A quote is captured, then replayed

Pricing is queried once, at admission, and the applicable rates are **frozen into
an immutable captured quote** that is reserved against, persisted, and replayed to
price the actual usage at settlement.

Settlement never re-queries the live catalog. If it did, a price refresh landing
between admission and settlement would let a request be reserved against one price
sheet and charged against another, with nothing in the record to show it happened.
A request must remain reproducibly priceable after catalog updates, price changes,
and application restarts, which means the quote is throttle's own value type
carrying its own rates and provenance — never a serialized provider SDK object.

A provider may serve a request on a service tier the caller did not ask for, and
that tier can price differently. The captured quote therefore carries the model's
other priced tiers as **alternates**, collected in the same catalog read, and
settlement selects among them. That is still a replay of frozen rates rather than a
re-query.

### When the model is not knowable at admission: a captured quote set

A captured quote assumes the model is known at admission, which holds whenever the
caller names one. A managed agent invocation does not: the caller names an agent, and
which foundation models it invokes is discovered from the response, after the money has
been spent.

The tempting fix — look up each model's price as its usage arrives — is exactly what a
captured quote exists to forbid. So the whole candidate rate set is frozen instead, at
admission, in **one catalog read at one instant**, narrowed to the access dimensions the
request will run under. Settlement is a lookup in that frozen set. It is the same
guarantee, widened from one model to a bounded set of them; a model the set does not
contain is unpriceable, which flows into the ordinary partial/unresolved semantics
rather than into a re-query.

Only the quotes covering invocations that actually happened are retained on the record.
A catalog of several hundred models would otherwise be written to every agent request,
and the models a request never touched are not part of its accounting story.

A catalog that cannot enumerate its models yields an empty set with an explanation, not
an error. Enumeration is optional and asserted for, so a user-supplied catalog stays
cheap to implement and still prices every direct model invocation perfectly.

### One compound charge, still one rounding

A compound charge extends the single-rounding rule across invocations as well as
dimensions. Every observed model invocation's exact cost accumulates into one rational
and the sum is rounded once, so a turn made of twenty small model calls is charged like
one charge. Rounding each invocation would drift upward by up to a microdollar per step.

Per-step amounts are reported for auditing and are each rounded individually, so the
steps may not sum to the turn's total — the same discrepancy the per-dimension breakdown
has, for the same reason. **The aggregate is what settles.** If one internal invocation
cannot be priced, the whole outer transaction's cost becomes partial and the reservation
stays encumbered: the priced subset is a floor and is never reported as the total.

### Unknown cost is representable

When the catalog cannot price a model, throttle reports an explicitly **unknown**
cost. It does not guess, does not map the model to a similar known one, and does
not treat it as free — a zero would understate spend and corrupt every aggregate
built on it.

So `usage: known, cost: unknown` is a first-class state. Completeness is
three-valued (known / partial / unknown), because a single boolean cannot
distinguish "priced everything" from "priced most of it", and a partial amount is a
**floor** that names the dimensions it could not price.

What throttle does in that state (issue #17, decided):

- **At admission, under enforcement:** the request is **denied**, before the
  provider is called. Throttle cannot honestly enforce a dollar budget against
  exposure it cannot determine, and refusing after the call would cost real money.
- **At admission, under monitoring:** the request is **admitted** with a zero hold,
  and the decision carries an explicit cost-unknown flag. Monitor mode observes; it
  never blocks. The zero hold means "no amount was knowable", not "this is free" —
  the distinction lives in the flag and the activity record.
- **At settlement, when actual usage cannot be fully priced:** the usage is
  preserved in full, the reservation is **not** released, its amount stays
  encumbered as headroom already spent, and the request is marked unresolved for
  later reconciliation. Throttle does not settle only the dimensions it understands
  and report that figure as the actual total.
- **Never:** guess a price, borrow a similar model's price, or silently price as
  zero. A local or negotiated override may turn an otherwise unknown model into a
  known priced one, and that provenance is preserved on the quote.

### Unresolved liabilities

The mechanism is deliberately minimal: marking a transaction unresolved performs
**no ledger write at all**. The pending reservation already *is* the encumbrance
record, so the money stays claimed without inventing a second kind of entry.
`Release` refuses an unresolved transaction, because releasing would hand
already-spent money back as available. The ordinary lease still applies, so a
liability cannot freeze headroom permanently, and once a price arrives the ordinary
`Settle` path closes it.

This is not an accounting-corrections or refunds feature. It is the minimum needed
to retain an unresolved charge without lying about available budget.

See [`data-model.md`](data-model.md) for the representation and
[`decisions/0005-dimensional-usage-and-the-adapter-shim.md`](decisions/0005-dimensional-usage-and-the-adapter-shim.md)
for the reasoning.

## Crash reconciliation

A governed request touches two durable stores that cannot commit together — the
ledger holds the money, the activity store holds the record — with a paid provider
call between them. A process that dies in the wrong microsecond leaves the two
telling different stories, and nothing running is left to finish the sentence:
`engine.Transaction` is in-memory, so its notion of "this request is mid-flight"
dies with the process.

The `reconcile` package reconstructs intent from durable state alone and completes
the bookkeeping — but only where the stores already contain enough authoritative
information to complete it truthfully.

### Three classes, one of which is repairable

An incomplete record is in one of three genuinely different situations:

| | situation | what reconciliation does |
|---|---|---|
| **A** | bookkeeping is incomplete and authoritative information exists to finish it | repair it |
| **B** | the provider outcome is genuinely unknown | classify, leave it |
| **C** | the accounting is intentionally unresolved, awaiting a later external observation | classify, leave it |

Only A becomes money. Turning B or C into a guessed cost to make the database look
tidy would be worse than doing nothing, because the result would then look correct
while being wrong. Every automatic repair is a transcription of a fact one store
already holds, never a judgement about what probably happened.

Class C is not crash damage. A hosted runtime invocation whose resource consumption
arrives out of band, later, is in its designed terminal state; the reconciler
distinguishes it by a typed reason (`crash-repairable`, `provider-outcome-unknown`,
`pricing-unresolved`, `awaiting-external-usage`) so a permanently healthy state never
appears in a damage report.

### Repair rules

Automatic repair happens in exactly these cases:

- the ledger settled and the activity write never landed → the record is transcribed
  from the charge; no second charge
- the ledger released and the activity write never landed → same, from the release
- durable usage plus the quote captured at admission, with the hold still standing →
  the settlement is replayed and the hold settles exactly once
- a durably recorded provider refusal with a known zero cost and no usage, with the
  hold still standing → the release is completed

And never in these:

- no durable usage exists → the request may have been served and billed; the record
  becomes explicitly outcome-unknown and the hold stays encumbered. **Not zero.**
- usage is known and the captured quote cannot price it → unresolved, pending
  pricing data rather than repair
- an out-of-band observation has not arrived → awaiting external usage
- one store holds a record the other does not → reported as orphaned; provider or
  model identity that was never durably recorded is **not** fabricated

### Guarantees

- **Historical pricing.** A repair prices recorded usage with the immutable quote
  captured at admission. `reconcile` imports no catalog and holds no reference to
  one, so a price change after the crash *cannot* rewrite history — a structural
  guarantee rather than a rule to remember.
- **The ledger stays authoritative for money.** Activity is repaired from the
  ledger, never the reverse. The two exceptions running the other way are both
  guarded on facts an adapter observed from the provider.
- **Exactly one monetary transition wins.** Concurrency safety comes from the
  ledger's own uniqueness constraints (`ErrAlreadyResolved` and the unique charge
  per reservation), not from an application-level lock, so several throttle
  processes may reconcile the same stores. A loser converges on the authoritative
  state rather than erroring.
- **An expired lease is never evidence of zero spend.** Expiry stops a hold blocking
  headroom and says nothing about what the provider billed; late settlement keeps
  working.
- **The authorizing period owns the charge.** A recovered charge lands in the period
  that authorized the reservation, never the period in which the repair happened.
  The ledger already refuses to close a period holding a settleable reservation, so
  this needed no weakening of period invariants.
- **No content.** Reconciliation reads and writes only statuses, counts, amounts,
  identifiers, and timestamps. Nothing about it requires prompts, responses,
  streamed text, trace rationale, or runtime payloads to be persisted.
- **Provider-neutral.** Classification reads normalized fields adapters already
  write; there is no operation or provider conditional in any repair rule.

### Shape

`Reconcile(ctx, requestID)` for one request, `ReconcilePending(ctx)` for a bounded
sweep of both stores, and `throttle reconcile` with `-dry-run` for an operator. It is
explicit rather than scheduled: an operator running a pass at start-up or after an
incident is sufficient for v0.1, and a daemon would have to decide on its own when to
touch money. Every result carries a class — `repaired`, `already_consistent`,
`unresolved`, `awaiting_external_usage`, `orphaned`, `failed` — and the summary counts
them separately, because folding unresolved records into either the repaired or the
failed count misreports the system in opposite directions. A truncated pass says so.

Each repair appends an entry to the record's append-only reconciliation trail: what
state was observed, what was produced, whether money moved, and which immutable quote
priced it. The observed state is not recoverable from the row afterwards, and it is
the first thing anybody investigating a crash asks for.

## Local first

v0.1 should work with no central service:

```text
application → throttle library → SQLite ledger
                           ├────→ SQLite activity store
                           └────→ local dashboard/API
```

The activity store is a separate database handle from the ledger by design.
Activity is observability: a failure to record it must never be able to fail or
slow the transaction that governs real money, so an adapter treats a recording
error as a non-event and a nil store as "do not record". The two may point at the
same file, but they never share a transaction.

A later team version can swap in a shared ledger/service without changing budget semantics.
