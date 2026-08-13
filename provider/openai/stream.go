package openai

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/usage"
)

// ErrNoStreamClient means RespondStreaming was called on a client built without a
// streaming client. Streaming is opt-in -- wiring it requires the Streaming adapter
// -- so a caller who only makes non-streaming calls should not have to.
var ErrNoStreamClient = errors.New("openai: no streaming Responses client configured")

// EventStream is the reading surface of an OpenAI Responses event stream: exactly
// the four methods *ssestream.Stream exposes.
//
// A consumer-defined interface for the usual reason -- it makes the governed
// streaming path testable without credentials or a network -- and the SDK's own
// stream type satisfies it directly, with the SDK's own event type in the signature.
// Nothing is translated at this seam.
type EventStream interface {
	// Next advances to the next event, blocking on the provider. It returns false at
	// the end of the stream or on an error.
	Next() bool

	// Current returns the event Next advanced to, by value.
	Current() responses.ResponseStreamEventUnion

	// Err reports the error the stream ended with, or nil after a clean end. It is
	// also where a failure to establish the stream at all appears, because
	// NewStreaming has no error return.
	Err() error

	// Close releases the underlying response body.
	Close() error
}

// StreamAPI is the slice of the OpenAI client this adapter uses for streaming.
//
// The single method mirrors the SDK exactly, including the absence of an error
// return: responses.NewStreaming carries an establishment failure inside the stream,
// reported by Err. Preserving that shape here is deliberate -- an interface that
// invented an error return would be a different API from the one throttle actually
// calls, and the adapter's handling of the real shape is what needs testing.
type StreamAPI interface {
	NewStreaming(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) EventStream
}

// The SDK's own stream satisfies EventStream, checked at compile time rather than
// left to the wrapper below to discover.
//
// This assertion is the evidence that EventStream is a description of the official
// surface rather than a parallel invention: if a future SDK changes the shape of a
// Responses stream, this stops compiling instead of quietly letting throttle govern a
// stream type nobody ships.
var _ EventStream = (*ssestream.Stream[responses.ResponseStreamEventUnion])(nil)

// Streaming adapts a real OpenAI client to StreamAPI.
func Streaming(c *oai.Client) StreamAPI { return streamingClient{c: c} }

type streamingClient struct{ c *oai.Client }

func (s streamingClient) NewStreaming(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) EventStream {
	return s.c.Responses.NewStreaming(ctx, body, opts...)
}

// StreamRequest is a governed streaming Responses call.
type StreamRequest struct {
	// BudgetID is the budget to charge. Required.
	BudgetID string

	// Params is the SDK request, passed to OpenAI unchanged. Required.
	//
	// Unchanged is meant as literally as it is for Request. In particular throttle
	// does not set `stream`: the SDK's NewStreaming sets it itself, and a caller who
	// cannot predict what reaches OpenAI cannot reason about their own bill.
	Params responses.ResponseNewParams

	// RequestID identifies this call for reconciliation, exactly as on Request.
	// Generated if empty.
	RequestID string

	// Metadata is recorded on the reservation and charge. Prompt and generated
	// content is never recorded.
	Metadata map[string]string

	// Options are passed through to the SDK call.
	Options []option.RequestOption
}

// streamEnd is why a stream stopped. There is exactly one per stream.
type streamEnd int

const (
	endNone streamEnd = iota

	// endTerminal means an authoritative terminal Response was observed. It is the
	// only end state from which throttle can settle money.
	endTerminal

	// endEOF means the provider's stream ended without a terminal Response. It is
	// emphatically not success: see Stream.Next.
	endEOF

	// endStreamErr means the stream reported an error before any terminal Response.
	endStreamErr

	// endCtx means the caller's context was cancelled or timed out.
	endCtx

	// endClosed means the caller called Close.
	endClosed

	// endStalled means the caller stopped consuming events without closing. See
	// Config.StreamStallTimeout.
	endStalled
)

// Stream is a governed streaming Responses call.
//
// It presents the same four methods the SDK's own stream does, so a caller's read
// loop is unchanged and the events are the SDK's own:
//
//	stream, err := client.RespondStreaming(ctx, openai.StreamRequest{...})
//	if err != nil { return err }
//	defer stream.Close()
//	for stream.Next() {
//		switch ev := stream.Current().AsAny().(type) { ... }
//	}
//	if err := stream.Err(); err != nil { ... }
//
// # Why this is not Bedrock's stream
//
// Bedrock's SDK pushes events onto a channel, so throttle's Bedrock stream owns a
// goroutine that reads them and forwards them on. OpenAI's SDK is a pull iterator,
// and wrapping a pull iterator in a channel would add a goroutine, a copy, and a
// second stall-detection problem to solve nothing. So the caller's own goroutine
// drives the provider's stream through Next, and throttle observes the events as
// they pass.
//
// Provider-neutral semantics do not require identical mechanics. What both streams
// share is everything that decides money: the admission path, the frozen quote, the
// usage normalization, the rule that only authoritative provider usage settles a
// request, and the rule that a hold is never released on the strength of this side
// of the connection going quiet.
//
// # Ownership
//
// Terminal accounting runs under a sync.Once, synchronously, on whichever goroutine
// gets there first -- the reader inside Next, a caller in Close, or the supervisor
// on cancellation or consumer abandonment. Once.Do makes it exactly one accounting
// action for the stream, and it makes Close a synchronization point: when Close
// returns, the accounting is durable no matter who performed it.
//
// One supervisor goroutine owns the consumer-idle bound and the lease renewal, and
// every terminal path stops it.
type Stream struct {
	client *Client
	tx     *engine.Transaction

	// up is the provider's stream. The reader goroutine drives it; the supervisor
	// and Close only close it.
	up EventStream

	// ctx is the caller's context. The supervisor watches it, and the reader checks
	// it, so a cancellation is noticed whether or not anybody is reading.
	ctx context.Context

	// settleCtx is detached from the caller's, so bookkeeping survives a cancelled
	// request.
	settleCtx context.Context

	// once guards the single terminal accounting action.
	once sync.Once

	// closeUp closes the provider's stream exactly once, from whichever goroutine
	// gets there. Closing the body is also what unblocks a reader parked inside the
	// provider's Next, which is how cancellation and the idle bound take effect.
	closeUp sync.Once

	// stopSupervisor ends the supervisor and the lease renewal at the terminal
	// state, so an abandoned stream cannot renew forever.
	stopSupervisor context.CancelFunc

	// inNext reports whether the caller is currently blocked in Next, waiting on the
	// provider. It is the difference between a slow provider and a stopped consumer.
	inNext atomic.Bool

	// ticket advances on every entry to and exit from Next. The supervisor watches it
	// rather than a timestamp, so the idle bound is about the caller doing nothing
	// rather than about wall clock.
	ticket atomic.Uint64

	// exposure is the classification from the request, which decides whether a fully
	// priced token cost is nonetheless a floor. Read at the terminal state.
	exposure exposure

	// rec is the activity record in progress. Written before the stream is handed
	// out and then only inside the terminal accounting, which runs once.
	rec activity.Record

	// pending is the accounting result being assembled, published to res at the
	// terminal state so a caller cannot read a half-filled record.
	pending *Result

	start time.Time
	first bool

	// mu guards the published terminal state, the last error the reader observed,
	// and the renewal error.
	mu        sync.Mutex
	res       *Result
	term      bool
	termErr   error
	streamErr error
	renewErr  error
}

// RespondStreaming makes a governed streaming Responses call.
//
// Admission is identical to Respond, by construction rather than by convention: the
// same estimate, the same captured quote, the same atomic reservation across the
// budget chain, the same pre-call activity record. Only the operation on the identity
// differs. The returned Stream then owns the request until it reaches exactly one
// terminal state.
//
// # Terminality is a Response status, not an event name
//
// The stream is financially complete when throttle has observed enough authoritative
// provider state to settle the real cost, or to record explicitly that the real cost
// cannot be determined. In this API that state is a Response: an event is an
// accounting boundary only if it carries one whose own status is terminal. A
// response.output_text.done, a function-call-arguments.done, an mcp_call.completed
// are content lifecycle events and prove nothing about the bill.
//
// This is also why terminality is not read from the event's type name. The pinned
// SDK enumerates no response.cancelled event at all -- cancellation reaches a client
// only as a Response status, and only for background responses -- so a rule keyed on
// type names would have a hole where a rule keyed on status does not.
//
// # Terminal states
//
//   - A terminal Response with usage: settled from the quote captured at admission,
//     exactly once. This includes an incomplete or failed response, because usage
//     the provider reported is usage the provider billed.
//   - A terminal Response with usage that is not fully priceable: marked unresolved,
//     so the hold stays encumbered pending reconciliation. Same rule as Respond.
//   - A terminal Response with no usage: the hold is NOT released. Generation
//     demonstrably ran -- its events came through this code -- so releasing would
//     assert zero spend against direct evidence to the contrary, and throttle does
//     not reconstruct the count from what it saw.
//   - The stream ended, cleanly or with an error, without a terminal Response: the
//     outcome is unknown and the hold stays. An EOF is not a completion.
//   - The caller closed, cancelled, or abandoned the stream: the hold stays, for the
//     same reason it does on Bedrock. The caller stopping is a fact about the caller.
//   - The stream could not be established: the hold is released. Nothing was
//     generated. This is the one release path.
//
// # Lease
//
// The hold is renewed at a third of the lease quantum while the stream is alive, and
// the renewal stops at the terminal state. Renewal is tied to the request being in
// flight, not to text having arrived recently. A renewal failure does not kill the
// stream: an expired hold can still settle, and killing the stream would forfeit the
// authoritative usage still to come without recovering a cent.
func (c *Client) RespondStreaming(ctx context.Context, req StreamRequest) (*Stream, error) {
	if c.streamAPI == nil {
		return nil, ErrNoStreamClient
	}
	if req.BudgetID == "" {
		return nil, errors.New("openai: budget id is required")
	}
	if modelOf(req.Params.Model) == "" {
		return nil, errors.New("openai: a request with a model is required")
	}

	requestID := req.RequestID
	if requestID == "" {
		id, err := newRequestID()
		if err != nil {
			return nil, fmt.Errorf("openai: generating a request id: %w", err)
		}
		requestID = id
	}

	p := streamParams(req.Params)
	est, quote, err := c.estimate(ctx, req.Params, p)
	if err != nil {
		return nil, err
	}

	// Classified before the call, from the request rather than from the events. A
	// hosted tool's surcharge is incurred by asking for the tool, and seeing a
	// streamed tool event would not make it priceable.
	exposure := classifyTools(req.Params.Tools)

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

	res := &Result{Accounting: Accounting{
		Identity:      est.Identity,
		Estimate:      est,
		Quote:         quote,
		Mode:          dec.Mode,
		Decision:      dec,
		ReservationID: tx.Reservation().ID,
	}}
	rec.ReservationID = res.ReservationID
	rec.Reserved = tx.Reservation().Amount
	rec.Scopes = scopesOf(tx.Reservation())

	settleCtx := context.WithoutCancel(ctx)

	// Written before the call, so a process that dies mid-stream leaves evidence that
	// money may have moved. The operation on the identity is what identifies such a
	// record as a stream rather than a single round trip.
	c.record(settleCtx, rec, true)

	up := c.streamAPI.NewStreaming(ctx, req.Params, req.Options...)
	rec.StreamEstablished = c.engine.Now().Sub(start)

	if up == nil {
		// A streaming client that produced no stream at all is not something the SDK
		// does, and throttle cannot tell whether the model ran. The hold stays:
		// guessing "free" here would be a silent understatement.
		return nil, c.streamNotEstablished(settleCtx, tx, rec, res, start)
	}

	// NewStreaming has no error return: an establishment failure is carried inside
	// the stream. So this, not a returned error, is where "the request never reached
	// the model" is detected.
	if callErr := up.Err(); callErr != nil {
		_ = up.Close()
		return nil, c.streamRefused(settleCtx, tx, rec, res, start, callErr)
	}

	s := &Stream{
		client:    c,
		tx:        tx,
		up:        up,
		ctx:       ctx,
		settleCtx: settleCtx,
		exposure:  exposure,
		rec:       rec,
		pending:   res,
		start:     start,
		first:     true,
	}
	s.startSupervisor(tx.Reservation().Lease)
	return s, nil
}

// streamRefused releases the hold for a request that never reached the model.
//
// This is the one release path in the streaming lifecycle, and it is safe for a
// reason that had to be established rather than assumed: the pinned SDK's retry loop
// runs inside its request execution, before the response body is handed to the SSE
// decoder, so a retry can only happen before a stream exists. A refused
// establishment therefore cannot be a generation that already happened.
func (c *Client) streamRefused(settleCtx context.Context, tx *engine.Transaction, rec activity.Record, res *Result, start time.Time, callErr error) error {
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
		return err
	}
	// Reduced, never dumped: an establishment failure is an *oai.Error whose body can
	// quote the prompt, and a stream error carries the raw SSE payload.
	rec.Error = redactProviderError(callErr)
	c.record(settleCtx, rec, false)
	return fmt.Errorf("%w: %w", ErrProvider, callErr)
}

// streamNotEstablished records a streaming call that returned no stream and no
// error, which leaves the outcome unknowable rather than free.
func (c *Client) streamNotEstablished(settleCtx context.Context, tx *engine.Transaction, rec activity.Record, res *Result, start time.Time) error {
	rec.Status = activity.StatusOutstanding
	rec.Outcome = activity.OutcomeAccountingError
	rec.ActualCost = usage.UnknownCost("the provider returned no event stream")
	rec.Latency = c.engine.Now().Sub(start)
	rec.CompletedAt = c.engine.Now()
	err := fmt.Errorf("%w: the provider returned no event stream, so reservation %s is left outstanding",
		ErrAccounting, res.ReservationID)
	rec.Error = err.Error()
	c.record(settleCtx, rec, false)
	return err
}

// Next advances to the next provider event, returning false at the end of the
// stream. It is the SDK's own iteration contract, and like the SDK's it is not safe
// to call from more than one goroutine.
//
// # What throttle does here
//
// Events pass through untouched. throttle inspects each one only for the presence of
// an authoritative terminal Response, from which it reads usage, the served model,
// the tier that actually ran, and the provider's own status -- accounting metadata,
// never content. Deltas, reasoning, tool calls, annotations, and event kinds this
// build has never heard of are forwarded exactly as the SDK presents them, because
// forwarding must not require understanding.
//
// # Why accounting happens before the terminal event is delivered
//
// When this method observes a terminal Response it performs the whole terminal
// accounting before returning true. A caller who sees response.completed and simply
// stops iterating therefore cannot leave a request unaccounted -- there is no window
// in which the money has been decided and not recorded.
//
// # Why a false return is not a completion
//
// The SDK reports a clean end of stream and a successfully finished generation
// identically: Err returns nil for both. So a false return here means only that the
// transport stopped producing events. If no terminal Response was seen, the outcome
// is unknown, the cost is unknown, and the hold stays -- an EOF is never taken as
// evidence that the request succeeded, or that it was free.
func (s *Stream) Next() bool {
	if s.terminated() {
		// Terminal accounting is done. Iterating further could only re-read a stream
		// whose money has already been decided.
		return false
	}

	s.ticket.Add(1)
	s.inNext.Store(true)
	defer func() {
		s.inNext.Store(false)
		s.ticket.Add(1)
	}()

	// A cancelled caller has stopped waiting for the provider, so there is nothing to
	// wait for on their behalf.
	if s.ctx.Err() != nil {
		s.terminate(endCtx, nil)
		s.teardown()
		return false
	}

	if !s.up.Next() {
		s.endOfStream()
		return false
	}

	if s.first {
		s.first = false
		s.rec.StreamFirstEvent = s.client.engine.Now().Sub(s.start)
	}

	if out, ok := terminalResponse(s.up.Current()); ok {
		s.terminate(endTerminal, out)
	}
	return true
}

// endOfStream classifies a stream that stopped producing events.
//
// The distinction that matters is not why the transport stopped but whether an
// authoritative terminal Response was already seen. If it was, the accounting is
// done and this is cleanup: a trailing transport error does not undo a settlement
// the provider's own terminal state authorized. If it was not, every one of these
// endings leaves the outcome unknown.
func (s *Stream) endOfStream() {
	err := s.up.Err()
	s.mu.Lock()
	s.streamErr = err
	s.mu.Unlock()

	switch {
	case s.ctx.Err() != nil:
		// The read failed because the caller's context ended, which the transport
		// surfaces as a stream error. It is a cancellation, not a provider failure.
		s.terminate(endCtx, nil)
	case err != nil:
		s.terminate(endStreamErr, nil)
	default:
		s.terminate(endEOF, nil)
	}
	s.teardown()
}

// Current returns the event Next advanced to, exactly as the SDK produced it.
//
// The SDK returns its event by value, so this is the caller's own copy and throttle
// retains nothing. Nothing is accumulated: there is no buffer of deltas, no
// reconstructed text, and no reason for either, because throttle settles from
// authoritative usage rather than from what it watched go past.
func (s *Stream) Current() responses.ResponseStreamEventUnion { return s.up.Current() }

// Err reports the error the stream ended with, or nil.
//
// Before the terminal state it reports what the last Next observed, matching the
// SDK's own sequential contract. Afterwards it reports throttle's terminal error,
// which may additionally describe an accounting outcome the provider knows nothing
// about -- a settled request whose cost could not be priced, or a stream that ended
// without ever saying what it billed.
//
// It reads throttle's own record of the provider's error rather than calling the
// SDK's Err, so that a caller polling it from another goroutine cannot race the
// reader inside the SDK's Next.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.term {
		return s.termErr
	}
	return s.streamErr
}

// Close ends the stream and blocks until its accounting is complete.
//
// It is idempotent and safe to call concurrently from any number of goroutines: the
// provider's stream is closed exactly once and the transaction reaches exactly one
// terminal state. Closing while another goroutine is blocked in Next unblocks it, by
// closing the response body underneath it.
//
// A Close before a terminal Response does not release the reservation. A caller that
// stopped reading is not evidence that the provider stopped generating, so the hold
// stays and the request is recorded with an unknown outcome. A Close afterwards is
// resource cleanup and cannot account twice.
func (s *Stream) Close() error {
	s.terminate(endClosed, nil)
	s.teardown()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.termErr
}

// Result reports what throttle recorded, or nil if the stream is not terminal yet.
// Call Close first: it returns only once the result is available.
func (s *Stream) Result() *Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res
}

// ReservationID is the hold this stream is running under.
func (s *Stream) ReservationID() string { return s.tx.Reservation().ID }

// Decision is the admission decision that authorized the stream.
func (s *Stream) Decision() engine.Decision { return s.tx.Decision() }

func (s *Stream) terminated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term
}

// teardown closes the provider's stream exactly once.
//
// Separate from terminate because they answer different questions. Terminating is
// about money and happens once per stream; closing the body is about a file
// descriptor and may be reached from a caller's Close, from the supervisor
// unblocking a parked reader, or from the reader itself at the end of the stream.
func (s *Stream) teardown() {
	s.closeUp.Do(func() { _ = s.up.Close() })
}

// terminalResponse reports whether an event carries an authoritative terminal
// Response.
//
// Presence is read from the JSON metadata rather than from the value, because the
// SDK models every stream event as one flat struct: Response is a value type on
// every event, so a zero one is indistinguishable from an absent one on its face.
//
// Terminality is then the Response's own status. That is deliberately narrower than
// "the event's name sounds final": created, queued, and in_progress all carry a
// Response too, and content lifecycle events -- output_text.done,
// function_call_arguments.done, mcp_call.completed -- carry none at all and are not
// accounting boundaries however done they sound.
//
// Reading status rather than event type also means cancellation is covered. The
// pinned SDK enumerates no response.cancelled event; a cancelled background response
// arrives as a Response whose status says so, and this recognizes it.
func terminalResponse(ev responses.ResponseStreamEventUnion) (*responses.Response, bool) {
	if !ev.JSON.Response.Valid() {
		return nil, false
	}
	switch ev.Response.Status {
	case responses.ResponseStatusCompleted,
		responses.ResponseStatusIncomplete,
		responses.ResponseStatusFailed,
		responses.ResponseStatusCancelled:
		out := ev.Response
		return &out, true
	default:
		// in_progress, queued, or a status this build does not know. An unrecognized
		// status is not treated as terminal: the conservative failure is to keep
		// waiting and, if the stream then ends, to record the outcome as unknown.
		return nil, false
	}
}

// terminate performs the one terminal accounting action for this stream.
//
// Every path into it -- the reader observing a terminal Response or the end of the
// stream, a caller's Close, the supervisor on cancellation or consumer abandonment
// -- goes through the same sync.Once, which is what makes double-settlement
// unrepresentable rather than merely unlikely. Once.Do also blocks a late caller
// until the first one finishes, so when Close returns the accounting is durable
// whoever performed it.
func (s *Stream) terminate(end streamEnd, out *responses.Response) {
	s.once.Do(func() {
		// Stopped first, so no lease renewal or idle check can outlive the accounting
		// they exist to protect.
		if s.stopSupervisor != nil {
			s.stopSupervisor()
		}

		now := s.client.engine.Now()
		s.rec.Latency = now.Sub(s.start)
		s.rec.CompletedAt = now

		res := s.pending
		res.Latency = s.rec.Latency

		if out != nil {
			s.settle(out, res)
		} else {
			s.retain(end, res)
		}

		if renewErr := s.renewalError(); renewErr != nil {
			// Recorded, not fatal: an expired hold can still settle, and the usual cause
			// is a lease that lapsed while a long stream was legitimately running.
			s.rec.Error = join(s.rec.Error, fmt.Sprintf("lease renewal failed: %v", renewErr))
		}

		s.client.record(s.settleCtx, s.rec, false)

		s.mu.Lock()
		s.res = res
		s.term = true
		s.mu.Unlock()
	})
}

// settle accounts a stream whose authoritative terminal Response was observed.
//
// This is the same reconciliation Respond performs, on the same captured quote and
// through the same usage mapper: a terminal event carries the same responses.Response
// a non-streaming call returns, so streaming has no pricing or normalization
// implementation of its own. The live catalog is never consulted.
func (s *Stream) settle(out *responses.Response, res *Result) {
	s.captureTerminalFacts(out, res)

	if !hasUsage(out) {
		s.terminalWithoutUsage(out, res)
		return
	}

	u, normErr := normalizeUsage(out.Usage)
	if normErr != nil {
		// The provider's own figures contradict each other, so there is nothing
		// trustworthy to charge. The hold stays outstanding: the stream ran, and the
		// usage object proves tokens were consumed even though its arithmetic does not
		// add up.
		res.Usage = u
		s.rec.ActualUsage = u
		s.rec.Status = activity.StatusOutstanding
		s.rec.Outcome = activity.OutcomeAccountingError
		s.rec.ActualCost = usage.UnknownCost(normErr.Error())
		res.Cost = s.rec.ActualCost
		s.setErr(fmt.Errorf("%w: %w, so reservation %s is left outstanding",
			ErrAccounting, normErr, res.ReservationID))
		return
	}

	res.Usage = u
	s.rec.ActualUsage = u

	actual := usage.Actual{Identity: res.Identity, Usage: u}

	cost, _ := s.client.priceActual(s.settleCtx, res.Quote, res.Identity, u)

	// A hosted tool whose charge OpenAI bills outside the usage object means the token
	// cost is a floor, however completely the tokens themselves priced. Streaming does
	// not change that: the events said nothing about the surcharge either.
	if !s.exposure.complete() {
		cost = s.exposure.downgrade(cost)
	}
	actual.Cost = cost
	res.Cost = cost
	s.rec.ActualCost = cost

	if !cost.Known() {
		// The request ran and cost money throttle cannot name. The hold stays
		// encumbered: releasing it would report spent money as available, and settling
		// a partial floor as a total would understate real spend.
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
		if !completed(out) {
			s.rec.Error = join(s.rec.Error, responseReason(out))
		}
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

	// A response that stopped early, failed, or was cancelled, yet reported usage, is
	// settled on that usage and the caller is told why it stopped. Recording the
	// reason keeps "we charged for a truncated answer" legible later.
	if status := statusOutcome(out); status != "" {
		s.rec.Outcome = status
		s.rec.Error = join(s.rec.Error, responseReason(out))
		s.setTermErr(statusError(out))
	}
}

// captureTerminalFacts reads what the terminal Response says actually happened.
//
// Split out of settle because the two do different kinds of work: this one only reads
// provider facts and records them, while settle decides money from them. Keeping the
// reading separate makes the privacy boundary auditable in one place -- every field
// taken from the Response is here, and it is an identifier or a tier, never content.
//
// The Response's own identity is authoritative for what ran. The tier that served the
// call can differ from the one requested and can price differently (#30), and the model
// may be an alias the provider resolved to a dated snapshot.
func (s *Stream) captureTerminalFacts(out *responses.Response, res *Result) {
	if tier := tierOf(out.ServiceTier); tier != "" {
		res.Identity.ServiceTier = tier
	}
	if served := modelOf(out.Model); served != "" && served != res.Identity.ProviderModelID {
		res.ServedModelID = served
		s.rec.Metadata = withMeta(s.rec.Metadata, "openai.served_model", served)
	}
	if out.ID != "" {
		// The provider's own identifier for this response, which is what an operator
		// quotes in a support conversation and what ties a throttle record to OpenAI's
		// own logs. An identifier, not content, so it is safe to persist.
		s.rec.Metadata = withMeta(s.rec.Metadata, "openai.response_id", out.ID)
	}
	// The terminal Response is handed back on the Result as well as through the event
	// the caller is about to receive, so a caller who only wants the accounting outcome
	// does not have to have kept the event. It is in memory only: nothing from it but
	// usage figures and identifiers reaches durable state.
	res.Response = out
	s.rec.Identity = res.Identity
}

// terminalWithoutUsage records a terminal Response that reported no usage.
//
// The hold is deliberately not released, and this is where the streaming lifecycle
// parts company with the non-streaming one. A non-streaming failed response is a
// single envelope with nothing in it; here, the generation's own events came through
// this code on their way to the caller, so "nothing was billed" would be a claim
// against direct evidence that work was done. throttle will not reconstruct the count
// from the deltas it saw, and it will not call the request free either. The outcome
// is that the cost is unknown and the hold stands.
func (s *Stream) terminalWithoutUsage(out *responses.Response, res *Result) {
	s.rec.Status = activity.StatusOutstanding

	if completed(out) {
		// A completed response with no usage metadata is not something OpenAI should
		// produce, and admitting the accounting is unresolvable beats recording a
		// zero-cost request.
		s.rec.Outcome = activity.OutcomeAccountingError
		s.rec.ActualCost = usage.UnknownCost("the stream's terminal response reported no usage metadata")
		res.Cost = s.rec.ActualCost
		s.setErr(fmt.Errorf("%w: the stream's terminal response reported no usage metadata, so reservation %s is left outstanding",
			ErrAccounting, res.ReservationID))
		return
	}

	reason := responseReason(out)
	s.rec.Outcome = activity.OutcomeProviderError
	if out.Status == responses.ResponseStatusCancelled {
		s.rec.Outcome = activity.OutcomeCancelled
	}
	s.rec.ActualCost = usage.UnknownCost(fmt.Sprintf(
		"the stream's terminal response was %s and reported no usage (%s), and throttle does not reconstruct usage from streamed events",
		out.Status, reason))
	res.Cost = s.rec.ActualCost
	s.setErr(fmt.Errorf("%w: the stream's terminal response was %s and reported no usage (%s), so reservation %s is left outstanding",
		ErrOutcomeUnknown, out.Status, reason, res.ReservationID))
}

// retain records a stream that ended before any authoritative terminal Response.
//
// The reservation is deliberately left alone in every one of these cases. Usage
// arrives only on a terminal Response, so "no terminal Response" is exactly the state
// in which throttle knows least about what was spent -- and releasing a hold means
// asserting that nothing was. A caller who closed early, a cancelled context, a
// broken stream, an abandoned reader, and a stream that simply stopped are all facts
// about this side of the connection; none of them proves the model stopped
// generating.
func (s *Stream) retain(end streamEnd, res *Result) {
	s.rec.Status = activity.StatusOutstanding
	streamErr := s.observedError()

	switch end {
	case endStreamErr:
		s.rec.Outcome = activity.OutcomeProviderError
		s.rec.ActualCost = usage.UnknownCost("the stream failed before reporting a terminal response")
		s.setErr(fmt.Errorf("%w: the stream failed before reporting a terminal response, so reservation %s is left outstanding: %s",
			ErrOutcomeUnknown, res.ReservationID, redactProviderError(streamErr)))

	case endEOF:
		// The transport stopped producing events and reported no error. That is not a
		// completion: the SDK reports a clean end of stream and a finished generation
		// identically, and only a terminal Response distinguishes them.
		s.rec.Outcome = activity.OutcomeAccountingError
		s.rec.ActualCost = usage.UnknownCost("the stream ended without a terminal response")
		s.setErr(fmt.Errorf("%w: the stream ended without a terminal response, so reservation %s is left outstanding",
			ErrAccounting, res.ReservationID))

	case endCtx:
		s.rec.Outcome = activity.OutcomeCancelled
		reason := "the stream was cancelled before a terminal response arrived"
		if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
			s.rec.Outcome = activity.OutcomeTimeout
			reason = "the stream timed out before a terminal response arrived"
		}
		s.rec.ActualCost = usage.UnknownCost(reason)
		s.setErr(fmt.Errorf("%w: %s (%v), so reservation %s is left outstanding",
			ErrOutcomeUnknown, reason, s.ctx.Err(), res.ReservationID))

	case endStalled:
		s.rec.Outcome = activity.OutcomeCancelled
		s.rec.ActualCost = usage.UnknownCost("the caller stopped consuming the stream before a terminal response arrived")
		s.setErr(fmt.Errorf("%w: the caller stopped consuming the stream for %s, so reservation %s is left outstanding",
			ErrOutcomeUnknown, s.client.streamStall, res.ReservationID))

	default: // endClosed
		s.rec.Outcome = activity.OutcomeCancelled
		s.rec.ActualCost = usage.UnknownCost("the stream was closed before a terminal response arrived")
		s.setErr(fmt.Errorf("%w: the stream was closed before a terminal response arrived, so reservation %s is left outstanding",
			ErrOutcomeUnknown, res.ReservationID))
	}
	res.Cost = s.rec.ActualCost

	if streamErr != nil && s.rec.Outcome != activity.OutcomeProviderError {
		s.rec.Error = join(s.rec.Error, "the provider stream also reported: "+redactProviderError(streamErr))
	}
}

// setErr publishes the terminal error and records it.
func (s *Stream) setErr(err error) {
	s.rec.Error = join(s.rec.Error, err.Error())
	s.setTermErr(err)
}

func (s *Stream) setTermErr(err error) {
	s.mu.Lock()
	s.termErr = err
	s.mu.Unlock()
}

func (s *Stream) observedError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streamErr
}

func (s *Stream) renewalError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewErr
}

// renewFailed records a lease renewal failure without killing the stream.
func (s *Stream) renewFailed(err error) {
	s.mu.Lock()
	s.renewErr = err
	s.mu.Unlock()
}

// stallBound returns how long a stream tolerates an idle consumer before treating it
// as abandoned.
//
// Zero configuration defaults to the reservation lease: a caller idler than that has
// stopped being distinguishable from one that walked away, and the hold is about to
// be reclaimed anyway. A ledger with leases disabled gets no bound, which is the same
// trade that configuration already made -- and with no lease there is no renewal to
// make immortal either.
func (c *Client) stallBound(lease time.Duration) time.Duration {
	if c.streamStall != 0 {
		return c.streamStall
	}
	return lease
}

// startSupervisor starts the one goroutine that watches a live stream.
//
// It owns both obligations a pull-shaped stream creates, because they have identical
// lifetimes and combining them keeps the ownership auditable: exactly one goroutine
// per stream, stopped by exactly one cancel, on every terminal path.
//
//   - Lease renewal, at a third of the lease quantum. A third rather than the lease
//     itself: renewing at the boundary would race the reclaim it exists to prevent.
//   - The consumer-idle bound, which is what stops an abandoned stream from renewing
//     that hold forever.
//
// # Why a watchdog rather than an event pump
//
// The Bedrock adapter forwards events through a channel, so it can bound each
// forwarding step directly. A pull iterator has no such step to bound: between two
// Next calls, throttle is not running at all. Rather than add a goroutine and a
// channel to manufacture a place to put the check, the supervisor samples the
// caller's progress.
//
// The distinction it has to draw is between a caller who is waiting on OpenAI and a
// caller who has walked away. Both look like "no new event for a while" from outside.
// So the watchdog reads two facts the reader publishes: whether the caller is
// currently inside Next, and a counter that moves on every entry and exit. If the
// caller is inside Next, they are waiting on the provider and the timer is simply
// re-armed -- a slow provider is not a stalled consumer, and a stream generating for
// an hour is fine. Only a caller who is outside Next, and has not entered it since
// the last sample, is abandoning the stream.
//
// The timer is armed at construction, so a caller who takes the stream and never
// calls Next at all is covered by the same mechanism as one who stops midway.
func (s *Stream) startSupervisor(lease time.Duration) {
	idle := s.client.stallBound(lease)
	interval := lease / 3

	if idle <= 0 && interval <= 0 {
		// Nothing to renew and nothing to bound. A stream with neither obligation needs
		// no goroutine, and not starting one is better than starting one that idles.
		s.stopSupervisor = func() {}
		return
	}

	// Detached from the caller's context, for the reason the Bedrock adapter's renewal
	// is: a cancelled caller still has a stream to wind down, and dropping the lease
	// first would hand headroom this request has probably already spent to the next
	// one. Cancellation is noticed separately, below.
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.ctx))
	s.stopSupervisor = cancel

	go s.supervise(ctx, idle, interval)
}

// supervise is the body of the supervisor goroutine. It returns -- always -- when
// stopSupervisor is called, which every terminal path does before accounting.
//
// The two timers are set up as nil channels when their obligation does not apply,
// because a nil channel blocks forever in a select. That is exactly what "no bound"
// and "no renewal" should mean, and it keeps one loop rather than four variants.
func (s *Stream) supervise(ctx context.Context, idle, interval time.Duration) {
	var renew <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		renew = ticker.C
	}

	var watchdog <-chan time.Time
	rearm := func() {}
	if idle > 0 {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		watchdog = timer.C
		// A bare Reset, deliberately. The stop-drain-reset dance this would have needed
		// before Go 1.23 is now not just unnecessary but misleading: Reset guarantees no
		// stale value is received afterwards, so a drain would be dead code implying a
		// hazard that no longer exists. The module requires 1.25.
		rearm = func() { timer.Reset(idle) }
	}

	last := s.ticket.Load()

	for {
		select {
		case <-ctx.Done():
			// The terminal state was reached, or the whole process is winding down. Either
			// way this goroutine's work is over: no lease to renew and no consumer to watch.
			return

		case <-s.ctx.Done():
			// The caller cancelled. Terminalizing here rather than waiting for a reader
			// means a caller who cancels and abandons the stream still gets accounted, and
			// closing the body unblocks a reader parked inside the provider's Next.
			//
			// The hold is deliberately not released: local cancellation does not prove zero
			// provider spend. OpenAI cannot cancel a synchronous response server-side, so
			// the model may well still be generating and billing.
			s.terminate(endCtx, nil)
			s.teardown()
			return

		case <-renew:
			if err := s.tx.Renew(ctx); err != nil {
				if errors.Is(err, ledger.ErrAlreadyResolved) {
					// The transaction finished under us. Nothing left to keep alive.
					return
				}
				// Recorded, not fatal: see renewFailed. An expired hold can still settle, and
				// tearing the stream down would forfeit the authoritative usage still to come
				// without recovering a cent.
				s.renewFailed(err)
			}

		case <-watchdog:
			now := s.ticket.Load()
			switch {
			case s.inNext.Load():
				// The caller is blocked on the provider. That is the request working, not the
				// caller abandoning it, so the bound is re-armed rather than enforced. This is
				// why the timeout cannot impose a maximum generation time.
				last = now
				rearm()
			case now != last:
				// The caller made progress since the last sample -- they entered and left Next
				// -- and is simply between events. Give them another interval.
				last = now
				rearm()
			default:
				// The caller has not touched the stream for a whole interval and is not waiting
				// on the provider. That is abandonment: without this the lease renewal above
				// would keep a hold alive forever behind a caller who has moved on.
				s.terminate(endStalled, nil)
				s.teardown()
				return
			}
		}
	}
}

// withMeta records one provider fact on the activity metadata.
//
// Metadata rather than a column: a served-model or response-id field on the neutral
// Record would be a schema change made for one provider. The caller's map is copied
// rather than written to, since it belongs to them.
func withMeta(m map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out[key] = value
	return out
}
