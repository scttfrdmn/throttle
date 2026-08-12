package bedrock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/usage"
)

// ErrNoStreamClient means ConverseStream was called on a client built without a
// streaming client. It is separate from ErrNoClient because streaming is opt-in:
// wiring it requires the Streaming adapter, and a caller who only makes
// non-streaming calls should not have to.
var ErrNoStreamClient = errors.New("bedrock: no ConverseStream client configured")

// EventStream is the reading surface of a Bedrock event stream: exactly the three
// methods *bedrockruntime.ConverseStreamEventStream exposes.
//
// It is a consumer-defined interface for the usual reason -- it makes the governed
// streaming path testable without credentials or a network -- but here there is a
// second, harder reason. ConverseStreamOutput.eventStream is unexported and has no
// setter, so a *ConverseStreamOutput carrying a live stream cannot be constructed
// outside the SDK package. An interface returning that type would be unmockable,
// so the seam is the stream itself.
type EventStream interface {
	Events() <-chan types.ConverseStreamOutput
	Close() error
	Err() error
}

// ConverseStreamAPI is the slice of the Bedrock Runtime client this adapter uses
// for streaming. Wrap a real client with Streaming to satisfy it.
type ConverseStreamAPI interface {
	ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (EventStream, error)
}

// Streaming adapts a real Bedrock Runtime client to ConverseStreamAPI.
//
// The whole adaptation is unwrapping ConverseStreamOutput to the stream it holds.
// Nothing about the request or the events is reinterpreted.
func Streaming(c *bedrockruntime.Client) ConverseStreamAPI { return streamingClient{c: c} }

type streamingClient struct{ c *bedrockruntime.Client }

func (s streamingClient) ConverseStream(ctx context.Context, in *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (EventStream, error) {
	out, err := s.c.ConverseStream(ctx, in, optFns...)
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, nil
	}
	// Returned as a concrete nil check rather than directly, so a missing stream is
	// a nil interface rather than a non-nil interface holding a nil pointer.
	es := out.GetStream()
	if es == nil {
		return nil, nil
	}
	return es, nil
}

// StreamRequest is a governed ConverseStream call.
type StreamRequest struct {
	// BudgetID is the budget to charge. Required.
	BudgetID string

	// Input is the SDK request, passed to Bedrock unchanged. Required.
	Input *bedrockruntime.ConverseStreamInput

	// RequestID identifies this call for reconciliation, exactly as on Request.
	// Generated if empty.
	RequestID string

	// Metadata is recorded on the reservation and charge. Prompt and generated
	// content is never recorded.
	Metadata map[string]string

	// Options are passed through to the SDK call.
	Options []func(*bedrockruntime.Options)
}

// termination is why a stream stopped. There is exactly one per stream.
type termination int

const (
	termNone termination = iota

	// termEnded means the provider's event channel closed: either the stream
	// finished or it failed. Err() distinguishes them.
	termEnded

	// termClosed means the caller called Close.
	termClosed

	// termCtx means the caller's context was cancelled or timed out.
	termCtx

	// termStalled means the caller stopped consuming events without closing. See
	// Config.StreamStallTimeout.
	termStalled
)

// Stream is a governed ConverseStream response.
//
// It presents the same three methods as the SDK's own event stream, so a caller's
// read loop is unchanged:
//
//	stream, err := client.ConverseStream(ctx, bedrock.StreamRequest{...})
//	if err != nil { return err }
//	defer stream.Close()
//	for ev := range stream.Events() {
//		switch v := ev.(type) { ... }
//	}
//	if err := stream.Close(); err != nil { ... }
//
// # Why it is a proxy and not the raw stream
//
// Usage arrives only in the terminal metadata event, so throttle has to see the
// end of the stream to account for the request at all. It reads the provider's
// stream and forwards events to the caller one at a time over an unbuffered
// channel: the caller gets ordinary backpressure and incremental delivery, and
// nothing is accumulated, replayed, or inspected beyond the metadata event.
//
// There is deliberately no accessor for the underlying stream. An unobserved read
// path is an unaccounted request, and the point of the shim is that there is no
// such path.
//
// # Ownership
//
// Exactly one goroutine -- the pump -- reads the provider's stream, forwards
// events, and performs the terminal accounting under a sync.Once. Every way a
// stream can end (drained, provider error, Close, context cancellation, an
// abandoned reader) funnels into that one goroutine, so a stream reaches exactly
// one terminal accounting state no matter how many of those happen at once.
type Stream struct {
	client *Client
	tx     *engine.Transaction

	// up is the provider's stream. Only the pump touches it.
	up EventStream

	// events is the caller's channel. Unbuffered, matching the SDK's own reader: a
	// slow caller stalls the pump, which stalls the provider's reader, and nothing
	// accumulates in throttle.
	events chan types.ConverseStreamOutput

	// ctx is the caller's context, watched by the pump directly rather than by a
	// separate goroutine.
	ctx context.Context

	// done is closed to ask for termination. finished is closed once the terminal
	// accounting is durable, which is what makes Close a synchronization point.
	done      chan struct{}
	closeOnce sync.Once
	finished  chan struct{}

	// stopKeepAlive ends the lease renewal goroutine at the terminal state, so an
	// abandoned stream cannot renew forever.
	stopKeepAlive context.CancelFunc

	// stall bounds a single forward to a caller that has stopped reading.
	stall stall

	// settleCtx is detached from the caller's, so bookkeeping survives a cancelled
	// request.
	settleCtx context.Context

	// rec is the activity record in progress. Only the pump writes it.
	rec activity.Record

	// pending is the accounting result being assembled. Only the pump writes it,
	// and it is published to res once the stream is terminal, so a caller cannot
	// read a half-filled record.
	pending *Result

	// mu guards the published terminal state and the renewal error, which is the
	// one field written from outside the pump.
	mu       sync.Mutex
	res      *Result
	termErr  error
	term     bool
	renewErr error
}

// Events returns the channel of provider events, in order, as they arrive.
//
// The channel is closed when the stream reaches its terminal state, so ranging
// over it terminates. Closing happens before the terminal accounting is written,
// so a caller is never made to wait on a database to receive the last event.
func (s *Stream) Events() <-chan types.ConverseStreamOutput { return s.events }

// Close ends the stream and blocks until its accounting is complete.
//
// It is idempotent and safe to call concurrently from any number of goroutines:
// the underlying provider stream is closed exactly once, and the transaction
// reaches exactly one terminal state. Calling it after the stream has already
// finished on its own just returns that outcome.
//
// A Close before the terminal metadata event does not release the reservation. A
// caller that stopped reading is not evidence that the provider stopped
// generating, so the hold stays and the request is recorded with an unknown
// outcome. See ConverseStream.
func (s *Stream) Close() error {
	s.closeOnce.Do(func() { close(s.done) })
	<-s.finished
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.termErr
}

// Err reports the error the stream ended with, or nil.
//
// Before the stream is terminal it reports the provider's own error, matching the
// SDK. Afterwards it reports throttle's terminal error, which may additionally
// describe an accounting outcome the provider knows nothing about.
func (s *Stream) Err() error {
	s.mu.Lock()
	if s.term {
		defer s.mu.Unlock()
		return s.termErr
	}
	s.mu.Unlock()
	return s.up.Err()
}

// Result reports what throttle recorded, or nil if the stream is not terminal yet.
// Call Close first: it blocks until the result is available.
func (s *Stream) Result() *Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res
}

// ReservationID is the hold this stream is running under.
func (s *Stream) ReservationID() string { return s.tx.Reservation().ID }

// Decision is the admission decision that authorized the stream.
func (s *Stream) Decision() engine.Decision { return s.tx.Decision() }

// ConverseStream makes a governed streaming Converse call.
//
// Admission is identical to Converse -- estimate, capture a quote, reserve across
// the budget chain, write the pre-call activity record -- and the returned Stream
// then owns the response until it reaches exactly one terminal state.
//
// # Why streaming needs different reconciliation
//
// Bedrock reports token usage only in the terminal metadata event. There is no
// incremental usage, and generated text cannot be turned back into a token count,
// so a stream that ends before that event leaves a request that certainly ran and
// probably cost money throttle cannot measure. Every failure mode below follows
// from that one fact.
//
// # Terminal states
//
//   - Metadata observed, stream drained: settled from the quote captured at
//     admission, exactly once.
//   - Metadata observed, then the stream failed: still settled. Usage the provider
//     reported is authoritative regardless of what happened afterwards.
//   - Metadata observed but not fully priceable: marked unresolved, so the hold
//     stays encumbered pending reconciliation. Same rule as Converse.
//   - The stream failed before metadata: the hold is NOT released. A stream error
//     is not evidence that nothing was generated or billed, so the outcome is
//     recorded as unknown.
//   - The caller closed, cancelled, or abandoned the stream before metadata: the
//     hold is NOT released either, for the same reason. The caller stopping is a
//     fact about the caller, not about the provider.
//   - The ConverseStream call itself failed and no stream exists: the hold is
//     released. Nothing was generated, exactly as for a Converse error with no
//     usage.
//
// Only the last case releases. Everything else either settles real usage or keeps
// the hold, because the alternative -- handing already-spent headroom back to the
// next caller -- is the one outcome that corrupts the budget.
//
// # Lease
//
// A stream can outlive the reservation lease, so the hold is renewed on a timer at
// a third of the lease quantum while the stream is alive, and the renewal stops at
// the terminal state. A renewal failure does not kill the stream: the usual cause
// is a lease that already lapsed and was reclaimed, an expired hold can still
// settle, and killing the stream would neither recover the money nor the usage.
func (c *Client) ConverseStream(ctx context.Context, req StreamRequest) (*Stream, error) {
	if c.streamAPI == nil {
		return nil, ErrNoStreamClient
	}
	if req.BudgetID == "" {
		return nil, errors.New("bedrock: budget id is required")
	}
	if req.Input == nil || req.Input.ModelId == nil || *req.Input.ModelId == "" {
		return nil, errors.New("bedrock: a ConverseStream input with a model id is required")
	}

	requestID := req.RequestID
	if requestID == "" {
		id, err := newRequestID()
		if err != nil {
			return nil, fmt.Errorf("bedrock: generating a request id: %w", err)
		}
		requestID = id
	}

	est, quote, err := c.estimate(ctx, streamParams(req.Input))
	if err != nil {
		return nil, err
	}

	start := c.engine.Now()
	rec := activity.Record{
		RequestID: requestID,
		BudgetID:  req.BudgetID,
		Identity:  est.Identity,
		Estimate:  est,
		Quote:     quote,
		Status:    activity.StatusPending,
		StartedAt: start,
		Metadata:  req.Metadata,
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

	res := &Result{
		Identity:      est.Identity,
		Estimate:      est,
		Quote:         quote,
		Mode:          dec.Mode,
		Decision:      dec,
		ReservationID: tx.Reservation().ID,
	}
	rec.ReservationID = res.ReservationID
	rec.Reserved = tx.Reservation().Amount
	rec.Scopes = scopesOf(tx.Reservation())

	settleCtx := context.WithoutCancel(ctx)

	// Written before the call, so a process that dies mid-stream leaves evidence
	// that money may have moved. The operation on the identity is what identifies
	// such a record as a stream rather than a single round trip.
	c.record(settleCtx, rec, true)

	up, callErr := c.streamAPI.ConverseStream(ctx, req.Input, req.Options...)
	rec.StreamEstablished = c.engine.Now().Sub(start)

	if callErr != nil {
		// The request was refused before a stream existed, so nothing was generated
		// and nothing was billed. This is the one release path.
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
		// A success that produced no stream is not something Bedrock should do, and
		// throttle cannot tell whether the model ran. The hold stays: guessing "free"
		// here would be a silent understatement.
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

	s := &Stream{
		client:    c,
		tx:        tx,
		up:        up,
		events:    make(chan types.ConverseStreamOutput),
		ctx:       ctx,
		done:      make(chan struct{}),
		finished:  make(chan struct{}),
		stall:     stall{timeout: c.stallBound(tx.Reservation().Lease)},
		settleCtx: settleCtx,
		rec:       rec,
		pending:   res,
	}
	s.startKeepAlive(tx.Reservation().Lease)
	go s.pump(start)
	return s, nil
}

// startKeepAlive renews the hold while the stream is alive.
//
// The interval is a third of the lease quantum, never the lease itself: renewing
// at the boundary would race the reclaim it exists to prevent. A ledger with
// leases disabled needs no renewal, since nothing expires.
func (s *Stream) startKeepAlive(lease time.Duration) {
	interval := lease / 3
	if interval <= 0 {
		return
	}
	// Detached from the caller's context: a cancelled caller still has a stream to
	// wind down, and dropping the lease first would let another request take
	// headroom this one has probably already spent.
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.ctx))
	s.stopKeepAlive = cancel
	go func() {
		if err := s.tx.KeepAlive(ctx, interval); err != nil {
			s.renewFailed(err)
		}
	}()
}

// renewFailed records a lease renewal failure without killing the stream.
//
// The model may already be generating, and the usual cause is a lease that lapsed
// and was reclaimed -- which does not prevent settlement, because the request
// really happened. Tearing the stream down would forfeit the authoritative usage
// still to come and would not recover a cent.
func (s *Stream) renewFailed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renewErr = err
}

// pump is the single owner of the provider's stream.
func (s *Stream) pump(start time.Time) {
	var meta *types.ConverseStreamMetadataEvent
	firstEvent := true

	for {
		select {
		case <-s.done:
			s.finish(termClosed, meta, start)
			return
		case <-s.ctx.Done():
			s.finish(termCtx, meta, start)
			return
		case ev, ok := <-s.up.Events():
			if !ok {
				s.finish(termEnded, meta, start)
				return
			}
			if firstEvent {
				firstEvent = false
				s.rec.StreamFirstEvent = s.client.engine.Now().Sub(start)
			}
			// Observed BEFORE forwarding, and deliberately so: a caller that abandons
			// the stream at exactly this instant must not be able to cost throttle the
			// only authoritative usage the provider will ever report.
			if m, isMeta := ev.(*types.ConverseStreamOutputMemberMetadata); isMeta {
				v := m.Value
				meta = &v
			}
			if t := s.forward(ev); t != termNone {
				s.finish(t, meta, start)
				return
			}
		}
	}
}

// forward hands one event to the caller, with backpressure.
func (s *Stream) forward(ev types.ConverseStreamOutput) termination {
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

// finish performs the one terminal accounting action for this stream.
//
// It runs on the pump goroutine and nowhere else, which is what makes
// double-settlement unrepresentable rather than merely unlikely.
func (s *Stream) finish(t termination, meta *types.ConverseStreamMetadataEvent, start time.Time) {
	// The caller's read loop ends here, before any database work: nobody should
	// wait on telemetry to receive the last event they were sent.
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

	if meta != nil {
		s.settle(meta, res, streamErr)
	} else {
		s.retain(t, res, streamErr)
	}

	if renewErr := s.renewalError(); renewErr != nil {
		// Recorded, not fatal: see renewFailed. It goes on the record so an operator
		// can see that a hold was renewed unsuccessfully during a live stream.
		s.rec.Error = join(s.rec.Error, fmt.Sprintf("lease renewal failed: %v", renewErr))
	}

	s.client.record(s.settleCtx, s.rec, false)

	s.mu.Lock()
	s.res = res
	s.term = true
	s.mu.Unlock()
}

// settle records a stream whose authoritative usage was observed.
//
// This is the same reconciliation Converse performs, on the same captured quote:
// the metadata event carries the same *types.TokenUsage and *types.ServiceTier a
// non-streaming response does, so streaming has no pricing implementation of its
// own. The live catalog is never consulted.
func (s *Stream) settle(meta *types.ConverseStreamMetadataEvent, res *Result, streamErr error) {
	// The tier the provider actually served on is authoritative over the requested
	// one, and it can price differently.
	if tier := tierOf(meta.ServiceTier); tier != "" {
		res.Identity.ServiceTier = tier
	}
	res.Usage = normalizeTokens(meta.Usage)
	s.rec.Identity = res.Identity
	s.rec.ActualUsage = res.Usage
	s.rec.ProviderLatency = streamLatency(meta.Metrics)

	actual := usage.Actual{
		Identity:        res.Identity,
		Usage:           res.Usage,
		ProviderLatency: s.rec.ProviderLatency,
	}

	cost, _ := s.client.priceActual(s.settleCtx, res.Quote, res.Identity, res.Usage)
	actual.Cost = cost
	res.Cost = cost
	s.rec.ActualCost = cost

	if !cost.Known() {
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
		// The stream broke, but the provider had already reported what it billed for.
		// That usage is real spend and is charged; the caller is told both.
		s.rec.Outcome = activity.OutcomeProviderError
		s.setErr(fmt.Errorf("%w: %w (usage was recorded)", ErrProvider, streamErr))
	}
}

// retain records a stream that ended before its authoritative usage arrived.
//
// The reservation is deliberately left alone in every one of these cases. Bedrock
// reports usage only at the end, so "we never saw the end" is exactly the state in
// which throttle knows least about what was spent -- and releasing a hold means
// asserting that nothing was. A caller who closed early, a cancelled context, a
// broken stream, and an abandoned reader are all facts about this side of the
// connection; none of them proves the model stopped generating.
func (s *Stream) retain(t termination, res *Result, streamErr error) {
	s.rec.Status = activity.StatusOutstanding

	switch {
	case t == termEnded && streamErr != nil:
		s.rec.Outcome = activity.OutcomeProviderError
		s.rec.ActualCost = usage.UnknownCost("the stream failed before reporting usage")
		s.setErr(fmt.Errorf("%w: the stream failed before reporting usage, so reservation %s is left outstanding: %w",
			ErrOutcomeUnknown, res.ReservationID, streamErr))

	case t == termEnded:
		// The stream ran to completion and never sent a metadata event. Bedrock marks
		// usage required on that event, so this is a provider contract violation, not
		// a free request.
		s.rec.Outcome = activity.OutcomeAccountingError
		s.rec.ActualCost = usage.UnknownCost("the stream ended without a metadata event")
		s.setErr(fmt.Errorf("%w: the stream ended without reporting usage, so reservation %s is left outstanding",
			ErrAccounting, res.ReservationID))

	case t == termCtx:
		s.rec.Outcome = activity.OutcomeCancelled
		reason := "the stream was cancelled before usage was reported"
		if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
			s.rec.Outcome = activity.OutcomeTimeout
			reason = "the stream timed out before usage was reported"
		}
		s.rec.ActualCost = usage.UnknownCost(reason)
		s.setErr(fmt.Errorf("%w: %s (%v), so reservation %s is left outstanding",
			ErrOutcomeUnknown, reason, s.ctx.Err(), res.ReservationID))

	case t == termStalled:
		s.rec.Outcome = activity.OutcomeCancelled
		s.rec.ActualCost = usage.UnknownCost("the caller stopped consuming the stream before usage was reported")
		s.setErr(fmt.Errorf("%w: the caller stopped consuming the stream for %s, so reservation %s is left outstanding",
			ErrOutcomeUnknown, s.stall.timeout, res.ReservationID))

	default: // termClosed
		s.rec.Outcome = activity.OutcomeCancelled
		s.rec.ActualCost = usage.UnknownCost("the stream was closed before usage was reported")
		s.setErr(fmt.Errorf("%w: the stream was closed before usage was reported, so reservation %s is left outstanding",
			ErrOutcomeUnknown, res.ReservationID))
	}

	if streamErr != nil && s.rec.Outcome != activity.OutcomeProviderError {
		s.rec.Error = join(s.rec.Error, fmt.Sprintf("the provider stream also reported: %v", streamErr))
	}
}

// setErr publishes the terminal error and records it.
func (s *Stream) setErr(err error) {
	s.rec.Error = join(s.rec.Error, err.Error())
	s.mu.Lock()
	s.termErr = err
	s.mu.Unlock()
}

func (s *Stream) renewalError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewErr
}

// streamLatency converts the latency Bedrock reports on a streaming metadata
// event. ConverseStreamMetrics is a distinct type from ConverseMetrics, which is
// the only reason this is not providerLatency.
func streamLatency(m *types.ConverseStreamMetrics) time.Duration {
	if m == nil || m.LatencyMs == nil {
		return 0
	}
	return time.Duration(*m.LatencyMs) * time.Millisecond
}
