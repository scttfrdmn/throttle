package anthropic

import (
	"context"
	"errors"
	"fmt"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/usage"
)

// The governed lifecycle for direct Anthropic Messages.
//
// # Why this is a copy of a shape and not a copy of code
//
// This file walks the same seven steps the Bedrock and OpenAI adapters walk --
// identify, estimate, capture a quote, reserve, execute, normalize, price, settle --
// because those steps are properties of throttle's accounting model rather than of an
// API. Every state a governed request can end in is one of the same states, resolved
// the same way, against the same engine and the same ledger. There is no
// Anthropic-specific ledger path, no Anthropic-specific settlement rule, and no
// Anthropic branch inside the engine.
//
// What is deliberately *not* shared with the other adapters is anything shaped by the
// provider: the request type, the usage decomposition, the exposure classifier, the
// error reduction. Factoring those together across providers is the temptation this
// slice exists to resist. Anthropic's usage counters are additive where OpenAI's are
// inclusive, its cache writes are priced by lifetime where OpenAI's are not priced at
// all, and its tool billing escapes the usage object in ways OpenAI's does not. A
// shared normalizer parameterized over those differences would be a generic LLM
// accounting API in a different costume, and the first thing it would smuggle in is
// the assumption that identically-named counters mean the same thing -- which is
// precisely the assumption that would have priced a 100,000-token cache read as fifty
// tokens.
//
// The measure of success is that this file is boring. Nothing in it needed inventing.

// admission is everything reserving a request produces, in provider-neutral terms.
type admission struct {
	tx  *engine.Transaction
	rec activity.Record

	// settleCtx is detached from the caller's context, so bookkeeping survives a
	// cancelled or timed-out request. A request whose caller went away still has to
	// resolve its own hold.
	settleCtx context.Context
}

// admit runs the admission half of the governed lifecycle: build the durable record,
// reserve against the budget chain, and write the pre-call evidence.
//
// The pre-call write is not incidental. A process that dies between reserving and
// settling has to leave a record saying money may have moved, or a crash is
// indistinguishable from a request that never happened. It is also what makes
// reconciliation possible without ever calling Anthropic again: the record carries the
// captured quote, so a later repair reprices from frozen rates rather than repeating
// the request.
//
// A denial is recorded and returned. That includes the unpriced denial -- the
// enforce-mode refusal of a request whose complete monetary exposure is not known,
// which for Anthropic means an unpriced model, an inference geography no rate was
// captured for, or a code-execution tool billed by container time the response cannot
// report. Recording it as OutcomeUnpriced rather than OutcomeBudgetDenied keeps "the
// budget was full" distinguishable from "throttle could not price this", and they call
// for entirely different operator action.
func (c *Client) admit(ctx context.Context, budgetID, requestID string, est usage.Estimate, acc *Accounting, metadata map[string]string) (*admission, error) {
	rec := activity.Record{
		RequestID: requestID,
		BudgetID:  budgetID,
		Identity:  est.Identity,
		Estimate:  est,
		Quote:     acc.Quote,
		Status:    activity.StatusPending,
		StartedAt: c.engine.Now(),
		Metadata:  metadata,
	}

	tx, dec, err := c.engine.Begin(ctx, engine.Request{
		BudgetID:  budgetID,
		RequestID: requestID,
		Estimate:  est,
		Identity:  est.Identity,
		Metadata:  metadata,
	})
	acc.Decision = dec
	acc.Mode = dec.Mode
	rec.EnforcementMode = dec.Mode
	if err != nil {
		rec.Status = activity.StatusDenied
		rec.Outcome = activity.OutcomeBudgetDenied
		if errors.Is(err, engine.ErrCostUnknown) {
			rec.Outcome = activity.OutcomeUnpriced
			rec.ActualCost = est.Cost
		}
		rec.Error = err.Error()
		rec.CompletedAt = c.engine.Now()
		c.record(context.WithoutCancel(ctx), rec, false)
		return nil, err
	}

	acc.ReservationID = tx.Reservation().ID
	rec.ReservationID = acc.ReservationID
	rec.Reserved = tx.Reservation().Amount
	rec.Scopes = scopesOf(tx.Reservation())

	settleCtx := context.WithoutCancel(ctx)
	c.record(settleCtx, rec, true)

	return &admission{tx: tx, rec: rec, settleCtx: settleCtx}, nil
}

// settleUsage prices normalized usage and resolves the reservation against it.
//
// This is the one place a governed Messages request turns tokens into a ledger entry.
// It takes normalized usage rather than an *anthropic.Message precisely so that it
// can be: by the time it is called, every Anthropic type is gone.
//
// The exposure argument carries what throttle could not fully account for -- a
// server-side tool billed in units the response does not report, an unknown usage
// counter, a container whose runtime is invisible. It is applied after pricing because
// it is not a pricing fact: the tokens may have priced perfectly and still not be the
// whole bill. Downgrading a known cost to a floor here is the point, since a request
// carrying unbounded provider exposure must never be reported as fully priced.
//
// Three endings, and the middle one is why this is not a one-liner:
//
//   - the cost is fully known: the reservation settles and the charge is recorded.
//   - the cost is partial or unknown: the reservation stays encumbered via
//     MarkUnresolved. Releasing would report spent money as available; settling a
//     floor as a total would understate real spend. This is where a cache-write total
//     its own TTL breakdown does not account for lands, and where an inference
//     geography no rate was frozen for lands.
//   - settlement itself fails: the hold is left outstanding, because releasing it
//     would erase spend that happened.
//
// callErr is Anthropic's own error, when it returned one alongside billable usage. A
// partially served request Anthropic billed for is real spend, so the usage is
// accounted either way and the caller is told both facts.
func (c *Client) settleUsage(acc *Accounting, rec *activity.Record, settleCtx context.Context, tx *engine.Transaction, u usage.Usage, exposure exposure, callErr error) error {
	acc.Usage = u
	rec.Identity = acc.Identity
	rec.ActualUsage = u
	if acc.ServedModelID != "" {
		rec.Metadata = withServedModel(rec.Metadata, acc.ServedModelID)
	}

	actual := usage.Actual{Identity: acc.Identity, Usage: u}

	cost, quoteErr := c.priceActual(settleCtx, acc.Quote, acc.Identity, u)
	if !exposure.complete() {
		cost = exposure.downgrade(cost)
	}
	actual.Cost = cost
	acc.Cost = cost
	rec.ActualCost = cost

	if !cost.Known() {
		if markErr := tx.MarkUnresolved(settleCtx, actual); markErr != nil {
			rec.Status = activity.StatusOutstanding
			rec.Error = markErr.Error()
			c.record(settleCtx, *rec, false)
			return fmt.Errorf("%w: %w", ErrAccounting, markErr)
		}
		acc.Unresolved = true
		rec.Status = activity.StatusUnresolved
		rec.Outcome = activity.OutcomeUnpriced
		err := fmt.Errorf("%w: %s: reservation %s stays encumbered pending reconciliation",
			ErrCostUnresolved, cost.Reason, acc.ReservationID)
		rec.Error = err.Error()
		c.record(settleCtx, *rec, false)
		if callErr != nil {
			return fmt.Errorf("%w (the provider also returned: %v)", err, callErr)
		}
		return err
	}

	charge, setErr := tx.Settle(settleCtx, actual)
	if setErr != nil {
		rec.Status = activity.StatusOutstanding
		rec.Outcome = activity.OutcomeAccountingError
		err := fmt.Errorf("%w: reservation %s is left outstanding: %w", ErrAccounting, acc.ReservationID, setErr)
		if quoteErr != nil {
			err = fmt.Errorf("%w (pricing: %v)", err, quoteErr)
		}
		rec.Error = err.Error()
		c.record(settleCtx, *rec, false)
		if callErr != nil {
			return fmt.Errorf("%w (the provider also returned: %v)", err, callErr)
		}
		return err
	}
	acc.Charge = charge
	acc.Settled = true
	rec.Status = activity.StatusSettled
	rec.Outcome = activity.OutcomeSuccess

	if callErr != nil {
		rec.Outcome = activity.OutcomeProviderError
		rec.Error = redactProviderError(callErr)
		c.record(settleCtx, *rec, false)
		return fmt.Errorf("%w: %w (usage was recorded)", ErrProvider, callErr)
	}
	return nil
}

// inconsistentUsage records a provider whose own usage figures contradict each other.
//
// The hold stays outstanding rather than being released. The request ran, and a usage
// object that fails its own arithmetic still proves tokens were consumed -- so there
// is nothing trustworthy to charge and nothing honest to give back.
//
// For Messages this is a negative counter or a TTL breakdown that exceeds the total it
// breaks down. A breakdown that merely falls *short* of its total is not inconsistent:
// it is incomplete, which is a priceable floor plus an unresolved remainder, and it
// goes through settleUsage. See normalizeUsage.
func (c *Client) inconsistentUsage(acc *Accounting, rec activity.Record, settleCtx context.Context, u usage.Usage, normErr error) error {
	acc.Usage = u
	rec.ActualUsage = u
	rec.Status = activity.StatusOutstanding
	rec.Outcome = activity.OutcomeAccountingError
	rec.ActualCost = usage.UnknownCost(normErr.Error())
	acc.Cost = rec.ActualCost
	err := fmt.Errorf("%w: %w, so reservation %s is left outstanding", ErrAccounting, normErr, acc.ReservationID)
	rec.Error = err.Error()
	c.record(settleCtx, rec, false)
	return err
}

// interrupted records a call the caller abandoned before any usage came back.
//
// The outcome is genuinely unknown, not zero: Anthropic cannot cancel a synchronous
// message server-side, so the request may well have been served and billed before the
// caller gave up, and no response came back to prove it either way. The hold is left
// outstanding, which is the only reading that does not either invent a charge or erase
// one.
func (c *Client) interrupted(acc *Accounting, rec activity.Record, settleCtx context.Context, ctxErr, callErr error) error {
	rec.Status = activity.StatusOutstanding
	rec.Outcome = activity.OutcomeCancelled
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		rec.Outcome = activity.OutcomeTimeout
	}
	rec.ActualCost = usage.UnknownCost("the call was interrupted before any usage was reported")
	acc.Cost = rec.ActualCost
	err := fmt.Errorf("%w: the call was interrupted (%v), so reservation %s is left outstanding (the provider returned: %v)",
		ErrOutcomeUnknown, ctxErr, acc.ReservationID, callErr)
	rec.Error = err.Error()
	c.record(settleCtx, rec, false)
	return err
}

// providerErrorWithoutUsage releases the hold for a call that failed with nothing
// billed.
//
// This is where an Anthropic 429 or a 529 overloaded lands, and it is recorded as a
// provider error rather than a budget denial. Conflating the two would send a caller
// to look at the wrong system: throttle's own refusal means spend more slowly, and
// Anthropic's rate limit means retry.
//
// It is also where the SDK's own local pre-flight refusal lands -- an unstreamed
// request whose max_tokens implies more than ten minutes of generation is rejected
// before any HTTP request is made, so there is genuinely nothing to charge and the
// full hold goes back.
func (c *Client) providerErrorWithoutUsage(acc *Accounting, rec activity.Record, settleCtx context.Context, tx *engine.Transaction, callErr error) error {
	rec.Status = activity.StatusReleased
	rec.Outcome = activity.OutcomeProviderError
	rec.ActualCost = usage.KnownCost(0)
	acc.Cost = rec.ActualCost
	if relErr := tx.Release(settleCtx); relErr != nil {
		err := fmt.Errorf("%w: %w (releasing reservation %s also failed: %v)",
			ErrProvider, callErr, acc.ReservationID, relErr)
		rec.Error = err.Error()
		c.record(settleCtx, rec, false)
		return err
	}
	rec.Error = redactProviderError(callErr)
	c.record(settleCtx, rec, false)
	return fmt.Errorf("%w: %w", ErrProvider, callErr)
}

// noUsageMetadata records a call that came back apparently fine but reported no usage
// at all.
//
// Not something Anthropic should produce: usage is a required field of the Messages
// response schema. The honest answer is that the accounting is unresolvable, with the
// hold left outstanding -- rather than a zero-cost request, which would quietly assert
// the provider served this for free.
func (c *Client) noUsageMetadata(acc *Accounting, rec activity.Record, settleCtx context.Context) error {
	rec.Status = activity.StatusOutstanding
	rec.Outcome = activity.OutcomeAccountingError
	rec.ActualCost = usage.UnknownCost("the provider reported no usage metadata")
	acc.Cost = rec.ActualCost
	err := fmt.Errorf("%w: the provider returned no usage metadata, so reservation %s is left outstanding",
		ErrAccounting, acc.ReservationID)
	rec.Error = err.Error()
	c.record(settleCtx, rec, false)
	return err
}
