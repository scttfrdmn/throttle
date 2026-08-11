package bedrock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime"
	agenttypes "github.com/aws/aws-sdk-go-v2/service/bedrockagentruntime/types"

	"throttle/activity"
	"throttle/engine"
	"throttle/ledger"
	"throttle/money"
	"throttle/pricing"
	"throttle/usage"
)

// ErrNoAgentClient means InvokeAgent was called on a client built without an
// Agents Classic client. Like streaming, it is opt-in: wiring it requires the Agent
// adapter, and a caller who only invokes models directly should not have to.
var ErrNoAgentClient = errors.New("bedrock: no InvokeAgent client configured")

// AgentEventStream is the reading surface of an InvokeAgent response stream:
// exactly the three methods *bedrockagentruntime.InvokeAgentEventStream exposes.
//
// The seam is the stream rather than the output for the same reason it is for
// ConverseStream: InvokeAgentOutput.eventStream is unexported with no setter, so a
// live one cannot be constructed outside the SDK package.
type AgentEventStream interface {
	Events() <-chan agenttypes.ResponseStream
	Close() error
	Err() error
}

// InvokeAgentAPI is the slice of the Bedrock Agent Runtime client this adapter
// uses. Wrap a real client with Agent to satisfy it.
type InvokeAgentAPI interface {
	InvokeAgent(ctx context.Context, in *bedrockagentruntime.InvokeAgentInput, optFns ...func(*bedrockagentruntime.Options)) (AgentEventStream, error)
}

// Agent adapts a real Bedrock Agent Runtime client to InvokeAgentAPI.
//
// The whole adaptation is unwrapping InvokeAgentOutput to the stream it holds.
// Nothing about the request or the events is reinterpreted.
func Agent(c *bedrockagentruntime.Client) InvokeAgentAPI { return agentClient{c: c} }

type agentClient struct{ c *bedrockagentruntime.Client }

func (a agentClient) InvokeAgent(ctx context.Context, in *bedrockagentruntime.InvokeAgentInput, optFns ...func(*bedrockagentruntime.Options)) (AgentEventStream, error) {
	out, err := a.c.InvokeAgent(ctx, in, optFns...)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	// A concrete nil check, so a missing stream is a nil interface rather than a
	// non-nil interface holding a nil pointer.
	es := out.GetStream()
	if es == nil {
		return nil, nil
	}
	return es, nil
}

// AgentRequest is a governed InvokeAgent call.
type AgentRequest struct {
	// BudgetID is the budget to charge. Required.
	BudgetID string

	// Input is the SDK request. Required.
	//
	// It is sent to Bedrock as the caller wrote it but for one field: EnableTrace is
	// set on a copy, because the trace is the only place Agents Classic reports what
	// its internal model invocations consumed. See InvokeAgent.
	Input *bedrockagentruntime.InvokeAgentInput

	// RequestID identifies this call for reconciliation, exactly as on Request.
	// Generated if empty.
	RequestID string

	// MaxCost is the caller's declared ceiling for this invocation, and the amount
	// reserved.
	//
	// It exists because throttle cannot estimate a managed agent turn: the runtime API
	// takes an agent identifier, the agent decides how many models to invoke and with
	// what prompts, and none of that is knowable in advance. Counting tokens does not
	// help -- the prompts throttle would count are not the prompts the agent builds.
	//
	// It is a declaration, not a guarantee. AWS will not stop the agent at throttle's
	// number, so the reservation is a budget hold rather than an enforced cap, and the
	// actual cost may exceed it. The overrun is recorded, as everywhere else.
	//
	// Zero means no ceiling, which makes the cost unknown before the call. Enforce
	// mode then refuses the invocation, because it cannot honestly govern spend it
	// cannot measure; monitor mode admits it with a zero hold and records the gap.
	MaxCost money.Money

	// Metadata is recorded on the reservation and charge. Prompt, response, and trace
	// content is never recorded.
	Metadata map[string]string

	// Options are passed through to the SDK call.
	Options []func(*bedrockagentruntime.Options)
}

// AgentResult is the outcome of a governed agent invocation.
type AgentResult struct {
	// Identity is the outer invocation: access provider, operation invoke-agent, and
	// the region. It carries no provider model ID, because the outer call is not a
	// model call -- the models it used are on Agent.Steps, discovered from the trace.
	Identity usage.ModelIdentity

	// Estimate is what was predicted. Its usage is empty and its quality is
	// heuristic: a declared ceiling is not a measurement.
	Estimate usage.Estimate

	// Usage is every observed step's usage, summed. It is the turn's usage, and the
	// basis the charge was computed from.
	Usage usage.Usage

	// Cost is the compound cost: every observed invocation priced under the frozen
	// quote set, accumulated exactly, rounded once. A partial cost's amount is a
	// floor, never a total.
	Cost usage.Cost

	// Quotes is the retained subset of the frozen quote set covering the models this
	// turn actually invoked, so the record stays reproducibly priceable.
	Quotes pricing.QuoteSet

	// Steps is the per-step accounting detail, for auditing. The step amounts are
	// rounded individually and may not sum to Cost.Amount; Cost is authoritative.
	Steps []pricing.Component

	// Agent is the durable compound-invocation detail: identifiers, normalized steps,
	// counted non-model activity, and any accounting limitation.
	Agent activity.Agent

	// ReturnedControl reports that the agent handed control back to the caller rather
	// than completing the turn itself. It is a successful protocol outcome: whatever
	// the agent consumed getting there is settled, and the caller's follow-up
	// InvokeAgent is a separate transaction.
	ReturnedControl bool

	// Unresolved reports that the turn spent money throttle could not fully name. Its
	// reservation stays encumbered.
	Unresolved bool

	Mode          engine.Mode
	Decision      engine.Decision
	ReservationID string

	// Charge is the durable record, valid when Settled is true.
	Charge  ledger.Charge
	Settled bool

	// Latency is the caller's wall clock for the whole turn. ProviderLatency is what
	// the agent reported for the invocation operation.
	Latency         time.Duration
	ProviderLatency time.Duration
}

// AgentStream is a governed InvokeAgent response.
//
// It presents the same three methods the SDK's own event stream does, so a caller's
// read loop is unchanged:
//
//	stream, err := client.InvokeAgent(ctx, bedrock.AgentRequest{...})
//	if err != nil { return err }
//	defer stream.Close()
//	for ev := range stream.Events() {
//		switch v := ev.(type) { ... }
//	}
//	if err := stream.Close(); err != nil { ... }
//
// # Why it is a proxy
//
// Usage arrives spread across the trace events, one pair of events per internal
// model invocation, so throttle has to observe the whole stream to account for the
// turn at all. Events are forwarded one at a time over an unbuffered channel: the
// caller gets ordinary backpressure and incremental delivery, and nothing is
// accumulated beyond the normalized accounting metadata. There is deliberately no
// accessor for the underlying stream, because an unobserved read path is an
// unaccounted request.
//
// # Ownership
//
// Exactly one goroutine -- the pump -- reads the provider's stream, forwards
// events, folds trace parts into the accounting state, and performs the terminal
// accounting under a sync.Once. Every way the turn can end funnels into that one
// goroutine, so it reaches exactly one terminal accounting state.
type AgentStream struct {
	client *Client
	tx     *engine.Transaction

	// up is the provider's stream. Only the pump touches it.
	up AgentEventStream

	// events is the caller's channel. Unbuffered, matching the SDK's own reader.
	events chan agenttypes.ResponseStream

	ctx context.Context

	done      chan struct{}
	closeOnce sync.Once
	finished  chan struct{}

	stopKeepAlive context.CancelFunc

	stall stall

	// settleCtx is detached from the caller's, so bookkeeping survives cancellation.
	settleCtx context.Context

	// quotes is the rate set frozen at admission. Settlement looks every observed
	// model up in it and never consults the live catalog.
	quotes pricing.QuoteSet

	// account is the normalized accounting state. Only the pump writes it.
	account *agentAccount

	agentID, aliasID, sessionID string

	// rec is the activity record in progress, and pending the result being
	// assembled. Only the pump writes either.
	rec     activity.Record
	pending *AgentResult

	mu       sync.Mutex
	res      *AgentResult
	termErr  error
	term     bool
	renewErr error
}

// Events returns the channel of provider events, in order, as they arrive.
//
// Trace events are forwarded too. The governed path enables the trace to account for
// the turn, so a caller who did not ask for it receives a superset of what they
// asked for rather than losing anything.
//
// The channel is closed when the stream reaches its terminal state, so ranging over
// it terminates. Closing happens before the terminal accounting is written.
func (s *AgentStream) Events() <-chan agenttypes.ResponseStream { return s.events }

// Close ends the invocation and blocks until its accounting is complete.
//
// Idempotent and safe to call concurrently: the provider stream is closed exactly
// once and the transaction reaches exactly one terminal state.
//
// A Close before the trace reported any usage does not release the reservation. See
// InvokeAgent.
func (s *AgentStream) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	<-s.finished
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.termErr
}

// Err reports the error the invocation ended with, or nil. Before the terminal state
// it reports the provider's own error, matching the SDK.
func (s *AgentStream) Err() error {
	s.mu.Lock()
	if s.term {
		defer s.mu.Unlock()
		return s.termErr
	}
	s.mu.Unlock()
	return s.up.Err()
}

// Result reports what throttle recorded, or nil if the invocation is not terminal
// yet. Call Close first: it blocks until the result is available.
func (s *AgentStream) Result() *AgentResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res
}

// ReservationID is the hold this invocation is running under.
func (s *AgentStream) ReservationID() string { return s.tx.Reservation().ID }

// Decision is the admission decision that authorized the invocation.
func (s *AgentStream) Decision() engine.Decision { return s.tx.Decision() }

// InvokeAgent makes a governed Agents Classic InvokeAgent call.
//
// # One transaction, many observed model invocations
//
// A single InvokeAgent request may invoke a foundation model many times --
// preprocessing, a routing classifier, several orchestration turns, postprocessing,
// and any collaborator the agent delegates to. throttle treats the outer invocation
// as one compound transaction: one estimate, one reservation, one charge.
//
// The internal model calls are accounting detail beneath that transaction, not
// transactions of their own. Inventing a child reservation for each would claim
// throttle admitted calls it never saw before they happened, and it did not: the
// managed agent decided to make them. What throttle can do, and does, is observe
// them, price them, and preserve them individually on the activity record.
//
// # Estimation
//
// There is nothing to estimate from. The runtime input names an agent, not a model,
// and the number and size of the internal invocations is the agent's decision.
// Counting tokens does not help, because the prompts the agent builds are not the
// prompt the caller supplied. So the estimate is AgentRequest.MaxCost when the caller
// declared one, labelled heuristic because a declaration is not a bound, and unknown
// when they did not -- which enforce mode refuses and monitor mode admits with a zero
// hold, exactly as for any other unpriceable request.
//
// # Pricing
//
// The models are unknowable at admission, so the whole candidate rate set is frozen
// instead: one catalog read, one instant, before the provider is called. Settlement
// looks each observed model up in that frozen set and never re-queries the catalog.
// A model absent from the set is unpriceable, which makes the turn's cost partial or
// unknown rather than triggering a mid-settlement price lookup. Only the quotes
// actually used are retained on the record.
//
// # Trace
//
// EnableTrace is set on a copy of the caller's input, because the trace is the only
// place Agents Classic reports per-invocation usage. Without it throttle would have
// to either refuse the call or claim an accuracy it does not have. The caller's own
// input is not mutated, and trace events are forwarded to them as well.
//
// Raw trace objects are never retained. Only identifiers, usage, cost, and timing
// are extracted: no prompt, model response, reasoning content, rationale, action
// input or output, knowledge-base query or retrieved passage, guardrail assessment,
// or collaborator message reaches a durable table. See agentAccount.
//
// # Terminal states
//
//   - The turn completed and every observed invocation priced: settled, once.
//   - The agent returned control to the caller: a successful protocol outcome,
//     settled on what was observed. The follow-up InvokeAgent is a new transaction.
//   - Some invocation could not be priced: marked unresolved, the hold stays
//     encumbered. The priced subset is never reported as the total.
//   - The stream failed after usage was observed: still settled. Usage the provider
//     reported is authoritative regardless of what happened afterwards.
//   - The stream failed, was closed, cancelled, or abandoned before any usage was
//     observed: the hold is NOT released. The caller stopping is not evidence that
//     the agent stopped.
//   - The InvokeAgent call itself failed and no stream exists: the hold is released.
//     Nothing ran.
//
// # What this cannot account for
//
// Agents Classic reports usage for model invocations only. An action-group Lambda, a
// knowledge-base lookup, a code-interpreter run, a guardrail evaluation: the trace
// says each happened and reports its duration, and carries no billable quantity for
// any of them. Their cost is real and lands on the AWS bill outside throttle's view.
// They are counted on the activity record and named in its note rather than priced
// at an invented number.
//
// Nor can throttle stop a managed agent at a dollar boundary. The reservation is a
// budget hold taken before the call; AWS provides no control that halts an
// in-progress agent turn at an internal spend figure, and this adapter does not
// pretend otherwise.
func (c *Client) InvokeAgent(ctx context.Context, req AgentRequest) (*AgentStream, error) {
	if c.agentAPI == nil {
		return nil, ErrNoAgentClient
	}
	if req.BudgetID == "" {
		return nil, errors.New("bedrock: budget id is required")
	}
	if req.Input == nil || req.Input.AgentId == nil || *req.Input.AgentId == "" ||
		req.Input.AgentAliasId == nil || *req.Input.AgentAliasId == "" ||
		req.Input.SessionId == nil || *req.Input.SessionId == "" {
		return nil, errors.New("bedrock: an InvokeAgent input with an agent id, alias id, and session id is required")
	}
	if req.MaxCost < 0 {
		return nil, errors.New("bedrock: max cost cannot be negative")
	}

	requestID := req.RequestID
	if requestID == "" {
		id, err := newRequestID()
		if err != nil {
			return nil, fmt.Errorf("bedrock: generating a request id: %w", err)
		}
		requestID = id
	}

	agentID, aliasID := *req.Input.AgentId, *req.Input.AgentAliasId
	sessionID := *req.Input.SessionId

	est := c.estimateAgent(req.MaxCost)

	// One catalog read, one instant, before the provider is called. A capture failure
	// is not fatal: it leaves no priceable models, which is the existing unknown-cost
	// condition and is handled by posture rather than by refusing here.
	at := c.engine.Now()
	quotes, _ := pricing.CaptureSet(c.rates, AccessProvider, c.region, at)

	start := c.engine.Now()
	rec := activity.Record{
		RequestID: requestID,
		BudgetID:  req.BudgetID,
		Identity:  est.Identity,
		Estimate:  est,
		Status:    activity.StatusPending,
		StartedAt: start,
		Metadata:  req.Metadata,
		Agent: activity.Agent{
			AgentID:   agentID,
			AliasID:   aliasID,
			SessionID: sessionID,
		},
	}

	tx, dec, err := c.engine.Begin(ctx, engine.Request{
		BudgetID:  req.BudgetID,
		RequestID: requestID,
		Estimate:  est,
		Identity:  est.Identity,
		Metadata:  req.Metadata,
	})
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

	res := &AgentResult{
		Identity:      est.Identity,
		Estimate:      est,
		Mode:          dec.Mode,
		Decision:      dec,
		ReservationID: tx.Reservation().ID,
	}
	rec.ReservationID = res.ReservationID
	rec.Reserved = tx.Reservation().Amount
	rec.Scopes = scopesOf(tx.Reservation())

	settleCtx := context.WithoutCancel(ctx)

	// Written before the call, so a process that dies mid-turn leaves evidence that
	// money may have moved.
	c.record(settleCtx, rec, true)

	up, callErr := c.agentAPI.InvokeAgent(ctx, traced(req.Input), req.Options...)
	rec.StreamEstablished = c.engine.Now().Sub(start)

	if callErr != nil {
		// Refused before a stream existed, so no model ran and nothing was billed.
		// This is the one release path.
		rec.Status = activity.StatusReleased
		rec.Outcome = activity.OutcomeProviderError
		rec.ActualCost = usage.KnownCost(0)
		rec.Latency = c.engine.Now().Sub(start)
		rec.CompletedAt = c.engine.Now()
		if relErr := tx.Release(settleCtx); relErr != nil {
			err := fmt.Errorf("%w: %w (releasing reservation %s also failed: %v)",
				ErrProvider, callErr, res.ReservationID, relErr)
			rec.Error = err.Error()
			c.record(settleCtx, rec, false)
			return nil, err
		}
		rec.Error = callErr.Error()
		c.record(settleCtx, rec, false)
		return nil, fmt.Errorf("%w: %w", ErrProvider, callErr)
	}

	if up == nil {
		// A success with no stream is not something the API should do, and throttle
		// cannot tell whether the agent ran. The hold stays: guessing "free" would be a
		// silent understatement.
		rec.Status = activity.StatusOutstanding
		rec.Outcome = activity.OutcomeAccountingError
		rec.ActualCost = usage.UnknownCost("the provider returned no event stream")
		rec.Latency = c.engine.Now().Sub(start)
		rec.CompletedAt = c.engine.Now()
		err := fmt.Errorf("%w: the provider returned no event stream, so reservation %s is left outstanding",
			ErrAccounting, res.ReservationID)
		rec.Error = err.Error()
		c.record(settleCtx, rec, false)
		return nil, err
	}

	s := &AgentStream{
		client:    c,
		tx:        tx,
		up:        up,
		events:    make(chan agenttypes.ResponseStream),
		ctx:       ctx,
		done:      make(chan struct{}),
		finished:  make(chan struct{}),
		stall:     stall{timeout: c.stallBound(tx.Reservation().Lease)},
		settleCtx: settleCtx,
		quotes:    quotes,
		account:   newAgentAccount(c.region),
		agentID:   agentID,
		aliasID:   aliasID,
		sessionID: sessionID,
		rec:       rec,
		pending:   res,
	}
	s.startKeepAlive(tx.Reservation().Lease)
	go s.pump(start)
	return s, nil
}

// traced returns a copy of the input with the trace enabled.
//
// A copy because the caller's request is theirs: mutating a struct they may reuse,
// inspect, or share across goroutines would be a surprising side effect of asking
// throttle to govern one call. The copy is shallow, which is enough -- only a
// top-level scalar changes, and nothing here reads or rewrites the nested session
// state.
func traced(in *bedrockagentruntime.InvokeAgentInput) *bedrockagentruntime.InvokeAgentInput {
	out := *in
	t := true
	out.EnableTrace = &t
	return &out
}

// estimateAgent builds the pre-admission estimate for an agent turn.
//
// Usage is deliberately empty: no token count is knowable before the agent decides
// what to invoke, and a fabricated one would be measured against real usage later
// and found to mean nothing. An empty usage with a known cost is a legitimate state
// for a declared ceiling.
//
// The quality is heuristic rather than conservative. QualityConservative promises a
// genuine upper bound on reality, and a caller's declared ceiling is not one: AWS
// will not stop the agent at throttle's number. Labelling it conservative would
// invite a reader to trust it as a bound.
func (c *Client) estimateAgent(maxCost money.Money) usage.Estimate {
	id := usage.ModelIdentity{
		AccessProvider: AccessProvider,
		Operation:      OperationInvokeAgent,
		Region:         c.region,
	}
	est := usage.Estimate{Identity: id, Quality: usage.QualityHeuristic}
	if maxCost > 0 {
		est.Cost = usage.KnownCost(maxCost)
		est.Note = "reserved against the caller's declared ceiling; a managed agent's internal model invocations are not knowable in advance, and AWS does not stop the agent at this figure"
		return est
	}
	est.Cost = usage.UnknownCost("a managed agent invocation's cost is not knowable in advance and no ceiling was declared; set AgentRequest.MaxCost to reserve against one")
	return est
}

// startKeepAlive renews the hold while the turn is live. An agent turn can easily
// outlive a lease quantum: it may invoke several models and call out to Lambdas
// between them.
func (s *AgentStream) startKeepAlive(lease time.Duration) {
	interval := lease / 3
	if interval <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.ctx))
	s.stopKeepAlive = cancel
	go func() {
		if err := s.tx.KeepAlive(ctx, interval); err != nil {
			s.mu.Lock()
			s.renewErr = err
			s.mu.Unlock()
		}
	}()
}

// pump is the single owner of the provider's stream.
func (s *AgentStream) pump(start time.Time) {
	firstEvent := true
	for {
		select {
		case <-s.done:
			s.finish(termClosed, start)
			return
		case <-s.ctx.Done():
			s.finish(termCtx, start)
			return
		case ev, ok := <-s.up.Events():
			if !ok {
				s.finish(termEnded, start)
				return
			}
			if firstEvent {
				firstEvent = false
				s.rec.StreamFirstEvent = s.client.engine.Now().Sub(start)
			}
			// Observed BEFORE forwarding, and deliberately so: a caller that abandons the
			// stream at exactly this instant must not be able to cost throttle usage the
			// provider has already reported.
			s.observe(ev)
			if t := s.forward(ev); t != termNone {
				s.finish(t, start)
				return
			}
		}
	}
}

// observe folds one event into the accounting state. Only the trace and
// return-control members say anything about accounting; the chunk and file members
// are response content and are forwarded untouched.
func (s *AgentStream) observe(ev agenttypes.ResponseStream) {
	switch v := ev.(type) {
	case *agenttypes.ResponseStreamMemberTrace:
		s.account.observe(v.Value)
	case *agenttypes.ResponseStreamMemberReturnControl:
		s.account.returnedControl = true
	}
}

// forward hands one event to the caller, with backpressure.
func (s *AgentStream) forward(ev agenttypes.ResponseStream) termination {
	stalled := s.stall.next()
	select {
	case s.events <- ev:
		return termNone
	case <-s.done:
		return termClosed
	case <-s.ctx.Done():
		return termCtx
	case <-stalled:
		return termStalled
	}
}

// finish performs the one terminal accounting action for this invocation.
//
// It runs on the pump goroutine and nowhere else, which is what makes
// double-settlement unrepresentable rather than merely unlikely.
func (s *AgentStream) finish(t termination, start time.Time) {
	// The caller's read loop ends here, before any database work.
	close(s.events)
	defer close(s.finished)

	s.stall.stop()
	if s.stopKeepAlive != nil {
		s.stopKeepAlive()
	}

	streamErr := s.up.Err()
	// Closed exactly once, by the owner, whatever ended the stream.
	if closeErr := s.up.Close(); streamErr == nil {
		streamErr = closeErr
	}

	now := s.client.engine.Now()
	s.rec.Latency = now.Sub(start)
	s.rec.CompletedAt = now

	res := s.pending
	res.Latency = s.rec.Latency
	res.ReturnedControl = s.account.returnedControl
	res.ProviderLatency = s.account.operationTotal
	res.Agent = s.account.detail(s.agentID, s.aliasID, s.sessionID)

	s.rec.Agent = res.Agent
	s.rec.ProviderLatency = res.ProviderLatency

	if s.account.observedUsage() {
		s.settle(res, streamErr)
	} else {
		s.retain(t, res, streamErr)
	}

	s.mu.Lock()
	renewErr := s.renewErr
	s.mu.Unlock()
	if renewErr != nil {
		// Recorded, not fatal: an expired hold still settles, and tearing the turn down
		// would forfeit authoritative usage without recovering a cent.
		s.rec.Error = join(s.rec.Error, fmt.Sprintf("lease renewal failed: %v", renewErr))
	}

	s.client.record(s.settleCtx, s.rec, false)

	s.mu.Lock()
	s.res = res
	s.term = true
	s.mu.Unlock()
}

// settle records a turn whose model invocations reported usage.
//
// Every observed step is priced under the quote frozen at admission, accumulated
// into one exact rational, and rounded once at the charge boundary. Rounding each
// step and summing the results would reintroduce exactly the per-invocation drift
// single rounding exists to prevent -- and on a twenty-step turn that drift is not
// theoretical.
func (s *AgentStream) settle(res *AgentResult, streamErr error) {
	cost, components, _ := s.quotes.PriceComponents(s.account.components())

	res.Usage = s.account.aggregate()
	res.Cost = cost
	res.Steps = components
	res.Quotes = s.quotes.Retain(components)

	// The per-step amounts are reported for auditing. They are rounded individually,
	// so they may not sum to the authoritative total.
	for i := range components {
		if i < len(res.Agent.Steps) {
			res.Agent.Steps[i].Cost = componentCost(components[i])
		}
	}

	s.rec.ActualUsage = res.Usage
	s.rec.ActualCost = cost
	s.rec.Agent = res.Agent
	// Only the quotes this turn actually used are persisted. The frozen set covers
	// every model the agent could have invoked, and writing a few hundred of them to
	// every record would say nothing about what happened.
	s.rec.Quotes = res.Quotes

	actual := usage.Actual{
		Identity:        res.Identity,
		Usage:           res.Usage,
		Cost:            cost,
		ProviderLatency: res.ProviderLatency,
	}

	if !cost.Known() {
		// The turn ran and cost money throttle cannot fully name. The hold stays
		// encumbered: releasing it would report spent money as available, and settling
		// the priced floor as a total would understate real spend.
		if markErr := s.tx.MarkUnresolved(s.settleCtx, actual); markErr != nil {
			s.rec.Status = activity.StatusOutstanding
			s.rec.Outcome = activity.OutcomeAccountingError
			s.setErr(fmt.Errorf("%w: %w", ErrAccounting, markErr))
			return
		}
		res.Unresolved = true
		s.rec.Status = activity.StatusUnresolved
		s.rec.Outcome = activity.OutcomeUnpriced
		s.setErr(fmt.Errorf("%w: %s: reservation %s stays encumbered pending reconciliation",
			ErrCostUnresolved, cost.Reason, res.ReservationID))
		return
	}

	charge, setErr := s.tx.Settle(s.settleCtx, actual)
	if setErr != nil {
		s.rec.Status = activity.StatusOutstanding
		s.rec.Outcome = activity.OutcomeAccountingError
		s.setErr(fmt.Errorf("%w: reservation %s is left outstanding: %w",
			ErrAccounting, res.ReservationID, setErr))
		return
	}
	res.Charge = charge
	res.Settled = true
	s.rec.Status = activity.StatusSettled
	s.rec.Outcome = activity.OutcomeSuccess

	if streamErr != nil {
		// The stream broke, but the agent had already reported what it consumed. That
		// usage is real spend and is charged; the caller is told both.
		s.rec.Outcome = activity.OutcomeProviderError
		s.setErr(fmt.Errorf("%w: %w (usage was recorded)", ErrProvider, streamErr))
	}
}

// retain records a turn that ended before any model invocation reported usage.
//
// The reservation is deliberately left alone in every case. The agent may have
// invoked several models before the stream broke or the caller walked away, and
// releasing a hold means asserting that nothing was spent. A closed stream, a
// cancelled context, a provider error, and an abandoned reader are all facts about
// this side of the connection; none of them proves the agent stopped.
func (s *AgentStream) retain(t termination, res *AgentResult, streamErr error) {
	res.Cost = s.rec.ActualCost
	s.rec.Status = activity.StatusOutstanding

	switch {
	case t == termEnded && streamErr != nil:
		s.rec.Outcome = activity.OutcomeProviderError
		s.rec.ActualCost = usage.UnknownCost("the agent invocation failed before any model usage was reported")
		s.setErr(fmt.Errorf("%w: the agent invocation failed before reporting usage, so reservation %s is left outstanding: %w",
			ErrOutcomeUnknown, res.ReservationID, streamErr))

	case t == termEnded:
		// The stream ran to completion and no model invocation reported usage. Either
		// the trace was suppressed or the agent answered without invoking a model; from
		// here those are indistinguishable, and neither is evidence of a free request.
		s.rec.Outcome = activity.OutcomeAccountingError
		s.rec.ActualCost = usage.UnknownCost("the agent invocation completed without reporting any model usage")
		s.setErr(fmt.Errorf("%w: the agent invocation reported no model usage, so reservation %s is left outstanding",
			ErrAccounting, res.ReservationID))

	case t == termCtx:
		s.rec.Outcome = activity.OutcomeCancelled
		reason := "the agent invocation was cancelled before any model usage was reported"
		if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
			s.rec.Outcome = activity.OutcomeTimeout
			reason = "the agent invocation timed out before any model usage was reported"
		}
		s.rec.ActualCost = usage.UnknownCost(reason)
		s.setErr(fmt.Errorf("%w: %s (%v), so reservation %s is left outstanding",
			ErrOutcomeUnknown, reason, s.ctx.Err(), res.ReservationID))

	case t == termStalled:
		s.rec.Outcome = activity.OutcomeCancelled
		s.rec.ActualCost = usage.UnknownCost("the caller stopped consuming the agent stream before any model usage was reported")
		s.setErr(fmt.Errorf("%w: the caller stopped consuming the agent stream for %s, so reservation %s is left outstanding",
			ErrOutcomeUnknown, s.stall.timeout, res.ReservationID))

	default: // termClosed
		s.rec.Outcome = activity.OutcomeCancelled
		s.rec.ActualCost = usage.UnknownCost("the agent stream was closed before any model usage was reported")
		s.setErr(fmt.Errorf("%w: the agent stream was closed before usage was reported, so reservation %s is left outstanding",
			ErrOutcomeUnknown, res.ReservationID))
	}

	res.Cost = s.rec.ActualCost
	if streamErr != nil && s.rec.Outcome != activity.OutcomeProviderError {
		s.rec.Error = join(s.rec.Error, fmt.Sprintf("the provider stream also reported: %v", streamErr))
	}
}

func (s *AgentStream) setErr(err error) {
	s.rec.Error = join(s.rec.Error, err.Error())
	s.mu.Lock()
	s.termErr = err
	s.mu.Unlock()
}

// componentCost turns a priced component into a reportable step cost, preserving
// why an unpriced step had no price.
func componentCost(c pricing.Component) usage.Cost {
	switch {
	case c.Priced:
		return usage.KnownCost(c.Amount)
	case c.Amount > 0:
		return usage.PartialCost(c.Amount, c.Unpriced, c.Reason)
	default:
		cost := usage.UnknownCost(c.Reason)
		cost.Unpriced = c.Unpriced
		return cost
	}
}
