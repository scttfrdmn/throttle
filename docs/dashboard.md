# Dashboard design notes

The dashboard should explain the budget model visually rather than resemble a generic cloud billing console.

## Primary views

### Throttle gauge

Represents permitted consumption intensity / budget pressure, not percent of total budget consumed.

Key values nearby:

```text
current burn
sustainable burn
projected end-of-period spend
```

### Bank / debt gauge

A bipolar display centered on zero:

```text
BORROWED  <────── 0 ──────>  BANKED
```

Positive means underspent relative to pace. Negative means future allocation has already been consumed.

### Range

Automotive "miles to empty" metaphor:

```text
budget remaining
period remaining
runway at current burn
projected end-of-period spend
```

### Timeline

Plot at minimum:

- target spend curve
- actual cumulative spend
- borrow envelope
- projection

The vertical distance between actual and target communicates bank/debt.

## Activity

Live table/event stream should expose:

```text
time
workload
access provider
publisher/model
input/output usage
estimated cost
reserved cost
actual cost
latency/status
```

Agent calls should be expandable into internal trace-derived usage when available.

## Breakdowns

Support orthogonal filtering by:

- budget/sub-budget
- workload
- access provider
- model publisher
- model family
- model/version
- principal (later teams)
- time range
