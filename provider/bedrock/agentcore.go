package bedrock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/bedrockagentcore"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/money"
	"github.com/scttfrdmn/throttle/pricing"
	"github.com/scttfrdmn/throttle/usage"
)

// ErrNoRuntimeClient means InvokeAgentRuntime was called on a client built without
// an AgentCore client. Like streaming and Agents Classic, it is opt-in.
var ErrNoRuntimeClient = errors.New("bedrock: no AgentCore InvokeAgentRuntime client configured")

// AgentCoreRuntimeResource is the provider model ID that AgentCore Runtime resource
// consumption is priced under.
//
// Runtime CPU and memory are billed against a hosted runtime rather than a
// foundation model, so there is no model ID to key on. Pricing keys on (access
// provider, provider model ID), and rather than widen that key for one resource, the
// resource is named as the bill names it. Nothing treats it as a model: the identity
// built from it claims no publisher and no canonical model.
//
// A price catalog must use this exact string for a reconciled runtime observation to
// be priceable. pricing/fixtures declares the same value on the neutral side, and a
// test asserts the two agree, because a silent divergence would make every
// reconciled runtime cost unpriceable for a reason nobody would think to look for.
const AgentCoreRuntimeResource = "agentcore-runtime"

// RuntimeAPI is the slice of the AgentCore data-plane client this adapter uses.
//
// Unlike ConverseStreamAPI and InvokeAgentAPI, this needs no wrapper:
// InvokeAgentRuntimeOutput.Response is a plain io.ReadCloser -- the HTTP body -- not
// an event stream hidden behind an unexported field. *bedrockagentcore.Client
// satisfies this directly, and a test fake is a reader rather than an event source.
//
// That difference is not cosmetic. AgentCore's response format is defined by the
// agent the caller deployed, so there are no typed events to inspect, no metadata
// member, and no usage figure anywhere in the response. See InvokeAgentRuntime.
type RuntimeAPI interface {
	InvokeAgentRuntime(ctx context.Context, in *bedrockagentcore.InvokeAgentRuntimeInput, optFns ...func(*bedrockagentcore.Options)) (*bedrockagentcore.InvokeAgentRuntimeOutput, error)
}

// RuntimeRequest is a governed AgentCore InvokeAgentRuntime call.
type RuntimeRequest struct {
	// BudgetID is the budget to charge. Required.
	BudgetID string

	// Input is the SDK request, sent to AgentCore exactly as the caller wrote it.
	// Required.
	//
	// Nothing is added, removed, or rewritten -- not even the trace flag Agents
	// Classic needs, because AgentCore has no equivalent to enable. The payload is
	// opaque bytes in a format the caller's own agent defines, and throttle does not
	// parse it.
	Input *bedrockagentcore.InvokeAgentRuntimeInput

	// RequestID identifies this call for reconciliation, exactly as on Request.
	// Generated if empty.
	RequestID string

	// MaxExposure is the amount to hold against the budget for this invocation.
	//
	// # Why this is not called MaxCost
	//
	// It is not a cost, an estimate, or a ceiling. AgentCore returns no billable
	// quantity with an invocation: CPU and memory consumption arrive later through the
	// platform's observability, so at admission throttle has no cost figure at all and
	// cannot form one. AgentRequest.MaxCost at least becomes comparable to a real
	// figure when the Agents Classic trace reports what the turn consumed; this number
	// may never be compared with anything.
	//
	// What it is: the caller's declaration of how much budget headroom this invocation
	// should occupy while its true cost is unknown. AWS will not stop the agent when
	// its resource consumption reaches this figure, and neither will throttle -- there
	// is no API that does. Calling it a maximum cost would invite exactly the reading
	// the name has to prevent.
	//
	// Zero means no exposure is declared, which leaves the pre-call cost unknown.
	// Enforce mode then refuses the invocation, because it cannot govern spend it
	// cannot measure; monitor mode admits it with a zero hold and records the gap.
	MaxExposure money.Money

	// Metadata is recorded on the reservation and charge. Payload content is never
	// recorded.
	Metadata map[string]string

	// Options are passed through to the SDK call.
	Options []func(*bedrockagentcore.Options)
}

// RuntimeResult is what throttle recorded about a governed runtime invocation.
type RuntimeResult struct {
	// Identity is the outer invocation: access provider aws-bedrock, operation
	// invoke-agent-runtime, the region, and AgentCoreRuntimeResource as the priceable
	// resource. It names no model, because the outer call is not a model call: which
	// models the hosted agent invoked, if any, is invisible from here.
	Identity usage.ModelIdentity

	// Estimate is what was predicted. Its usage is empty, and its quality is unknown
	// unless the caller declared an exposure.
	Estimate usage.Estimate

	// Cost is the runtime cost of this invocation. It is unknown on every path,
	// including complete success, until a delayed resource observation is reconciled
	// against the record. It is never zero: the runtime ran and consumed billable
	// resource, and reporting an unmeasured charge as free would be the one
	// unrecoverable error here.
	Cost usage.Cost

	// Quote is the runtime resource rate set frozen at admission, so a reconciliation
	// weeks later prices the observation at the rates in effect when the invocation
	// happened rather than at whatever the catalog says then. Empty if the catalog
	// prices no runtime resource for this region.
	Quote pricing.CapturedQuote

	// Runtime is the linkage detail persisted for that later reconciliation.
	Runtime activity.HostedRuntime

	// Unresolved reports that the invocation ran with a cost throttle cannot name, so
	// its hold stays encumbered. True for every invocation that reached the runtime.
	Unresolved bool

	// Mode is the posture that actually governed the invocation, and Decision the
	// admission decision that authorized it.
	Mode     engine.Mode
	Decision engine.Decision

	// ReservationID is the hold this invocation ran under, and Reserved the amount
	// held. Reserved is the declared exposure, not a measured cost.
	ReservationID string
	Reserved      money.Money

	// Latency is the caller's wall clock for the whole interaction, from admission to
	// the terminal state. ResponseBytes is how much of the body was read before it
	// ended.
	Latency       time.Duration
	ResponseBytes int64
}

// RuntimeStream is a governed AgentCore runtime invocation in progress.
//
// It is an io.ReadCloser over the runtime's response body. The caller reads it as
// they would read InvokeAgentRuntimeOutput.Response, and the bytes are forwarded
// verbatim: nothing is buffered, parsed, or retained. Output carries the response
// metadata.
//
// Reading to EOF, or calling Close, reaches the invocation's one terminal state.
// Close is idempotent and safe to call concurrently.
type RuntimeStream struct {
	client *Client
	tx     *engine.Transaction

	// body is the SDK's response body, owned solely by this stream.
	body io.ReadCloser

	// out is the SDK response with its body detached, so there is exactly one path to
	// the bytes.
	out *bedrockagentcore.InvokeAgentRuntimeOutput

	ctx context.Context

	// settleCtx is detached from the caller's, so bookkeeping survives cancellation.
	settleCtx context.Context

	stopKeepAlive context.CancelFunc

	// idle abandons an invocation whose caller stops reading without closing.
	idle *idleBound

	once sync.Once

	start time.Time

	mu sync.Mutex

	// rec is the activity record in progress, and pending the result being
	// assembled. Both become authoritative at the terminal transition.
	rec     activity.Record
	pending *RuntimeResult

	res       *RuntimeResult
	bytes     int64
	firstRead bool
	termErr   error
	term      bool
	renewErr  error
}

// Output returns the SDK response metadata: content type, status code, runtime
// session, trace identifiers, and the MCP fields when the caller used them.
//
// Its Response field is nil. The body is this stream, and leaving a second handle to
// the same bytes on the struct would let a caller read around the accounting by
// accident.
func (s *RuntimeStream) Output() *bedrockagentcore.InvokeAgentRuntimeOutput { return s.out }

// Read forwards the runtime's response bytes to the caller.
//
// It is an ordinary blocking read with ordinary backpressure: the runtime's body is
// pulled on the caller's goroutine, so a slow consumer simply reads slowly. There is
// no pump goroutine, because AgentCore pushes no events -- a distinction from the
// Converse and Agents Classic streams, whose providers deliver typed events whether
// or not anyone is listening.
//
// The first error or EOF reaches the terminal state. Subsequent reads return that
// same error.
func (s *RuntimeStream) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		s.finish(termCtx, nil)
		return 0, err
	}
	s.mu.Lock()
	done := s.term
	s.mu.Unlock()
	if done {
		return 0, s.readErr()
	}

	s.idle.reset()
	n, err := s.body.Read(p)
	if n > 0 {
		s.mu.Lock()
		s.bytes += int64(n)
		if !s.firstRead {
			s.firstRead = true
			s.rec.StreamFirstEvent = s.client.engine.Now().Sub(s.start)
		}
		s.mu.Unlock()
	}
	switch {
	case err == io.EOF:
		s.finish(termEnded, nil)
		return n, io.EOF
	case err != nil:
		// A cancelled context surfaces here as a transport error. It is classified as
		// cancellation rather than as a provider failure, because that is what happened.
		if ctxErr := s.ctx.Err(); ctxErr != nil {
			s.finish(termCtx, nil)
			return n, err
		}
		s.finish(termEnded, err)
		return n, err
	}
	return n, nil
}

// Close ends the invocation and blocks until its accounting is complete.
//
// Idempotent and safe to call concurrently: the body is closed exactly once and the
// transaction reaches exactly one terminal state.
//
// A Close before the body was fully read does not release the reservation. The
// runtime ran; the caller walking away is not evidence that it stopped. See
// InvokeAgentRuntime.
func (s *RuntimeStream) Close() error {
	s.finish(termClosed, nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.termErr
}

// Result reports what throttle recorded, or nil before the terminal state. Read to
// EOF or call Close first.
func (s *RuntimeStream) Result() *RuntimeResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res
}

// ReservationID is the hold this invocation is running under.
func (s *RuntimeStream) ReservationID() string { return s.tx.Reservation().ID }

// Decision is the admission decision that authorized the invocation.
func (s *RuntimeStream) Decision() engine.Decision { return s.tx.Decision() }

func (s *RuntimeStream) readErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.termErr != nil {
		return s.termErr
	}
	return io.EOF
}

// InvokeAgentRuntime makes a governed AgentCore InvokeAgentRuntime call.
//
// # Two accounting positions, and this is the outer one
//
// A hosted agent has an outside and an inside, and they are governed differently.
//
// Outside -- here -- throttle sees an invocation of a runtime: which runtime, which
// endpoint, which session, when, for how long, and whether it succeeded. It does not
// see the model calls, tool calls, or API calls the agent makes internally. The
// agent is arbitrary code the caller deployed; AgentCore does not report what it did,
// and this adapter does not pretend to know.
//
// Inside, the agent's own code uses throttle's ordinary provider adapters, and every
// model call it makes is governed in real time exactly as it would be anywhere else:
// estimated before it happens, reserved, executed, settled against the same
// provider-neutral engine. That is where real-time LLM budget enforcement lives, and
// it needs nothing AgentCore-specific -- hosting location does not change model-spend
// governance.
//
// So this call is admission control and attribution for the invocation, plus durable
// linkage for the resource bill that arrives later. It is not, and cannot be,
// real-time visibility into what a hosted agent spends on models.
//
// # Runtime cost is unknown, on every path
//
// AgentCore bills Runtime by CPU time and memory-time. Neither figure is in the
// response: they arrive through the platform's observability, delayed, and AWS
// describes them as possibly imprecise. There is nothing here to price.
//
// throttle therefore records the invocation with an explicitly unknown cost and
// leaves the hold encumbered -- the MarkUnresolved path -- rather than settling.
// Everything else would be a fabrication:
//
//   - Settling at zero would report a request that consumed real compute as free.
//   - Deriving CPU from wall-clock latency would invent a number. Providers bill
//     active processing, not elapsed time; an agent waiting on a model consumes
//     little CPU across a long invocation.
//   - Assuming the configured CPU and memory allocation would bill for capacity
//     rather than consumption.
//   - Estimating from response size would price bytes AWS does not charge for.
//
// The hold keeps its lease, so an unreconciled invocation stops blocking headroom the
// same way any other expired hold does. It does not freeze a budget forever.
//
// # Payloads are opaque
//
// The request payload and the response body are bytes in a format the caller's agent
// defines. throttle imposes no message, prompt, or token structure on them, reads
// nothing out of them for accounting, and persists neither. What is recorded is
// their size, which is what an operator needs to spot a runaway payload without the
// payload being stored to spot it.
//
// # Failure semantics
//
//   - Refused by budget: nothing is reserved and nothing is called.
//   - The InvokeAgentRuntime call itself fails: the hold is released. AgentCore
//     refused the invocation before the runtime ran, so nothing was consumed. This is
//     the one release path.
//   - A response came back: the runtime ran, so the hold stays. This holds equally
//     for a complete read, a non-200 status, a mid-body transport failure, caller
//     cancellation, an early Close, and an abandoned reader. Each is recorded with
//     its own outcome and an unknown cost; none is recorded as free.
//   - A success with no body at all: also unknown, also held.
func (c *Client) InvokeAgentRuntime(ctx context.Context, req RuntimeRequest) (*RuntimeStream, error) {
	if c.runtimeAPI == nil {
		return nil, ErrNoRuntimeClient
	}
	if req.BudgetID == "" {
		return nil, errors.New("bedrock: budget id is required")
	}
	if req.Input == nil || req.Input.AgentRuntimeArn == nil || *req.Input.AgentRuntimeArn == "" {
		return nil, errors.New("bedrock: an InvokeAgentRuntime input with an agent runtime arn or agent id is required")
	}
	if req.Input.Payload == nil {
		return nil, errors.New("bedrock: an InvokeAgentRuntime input with a payload is required")
	}
	if req.MaxExposure < 0 {
		return nil, errors.New("bedrock: max exposure cannot be negative")
	}

	requestID := req.RequestID
	if requestID == "" {
		id, err := newRequestID()
		if err != nil {
			return nil, fmt.Errorf("bedrock: generating a request id: %w", err)
		}
		requestID = id
	}

	est := c.estimateRuntime(req.MaxExposure)

	// The runtime resource rates are frozen here, at admission, even though nothing
	// can be priced yet. Reconciliation may not happen for hours, and it must price
	// what this invocation consumed at the rates that applied when it ran. A capture
	// failure is not fatal: it leaves the record unpriceable until an operator adds a
	// rate, which is the existing unknown-cost condition rather than a new one.
	at := c.engine.Now()
	var quote pricing.CapturedQuote
	if c.rates != nil {
		quote, _ = c.rates.Capture(est.Identity, at)
	}

	start := c.engine.Now()
	rt := activity.HostedRuntime{
		RuntimeID:    *req.Input.AgentRuntimeArn,
		Qualifier:    deref(req.Input.Qualifier),
		Account:      deref(req.Input.AccountId),
		SessionID:    deref(req.Input.RuntimeSessionId),
		TraceID:      deref(req.Input.TraceId),
		PayloadBytes: int64(len(req.Input.Payload)),
		Note:         runtimeCostNote,
	}
	rec := activity.Record{
		RequestID: requestID,
		BudgetID:  req.BudgetID,
		Identity:  est.Identity,
		Estimate:  est,
		Quote:     quote,
		Status:    activity.StatusPending,
		StartedAt: start,
		Metadata:  req.Metadata,
		Runtime:   rt,
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

	res := &RuntimeResult{
		Identity:      est.Identity,
		Estimate:      est,
		Cost:          usage.UnknownCost(runtimeCostNote),
		Quote:         quote,
		Runtime:       rt,
		Mode:          dec.Mode,
		Decision:      dec,
		ReservationID: tx.Reservation().ID,
		Reserved:      tx.Reservation().Amount,
	}
	rec.ReservationID = res.ReservationID
	rec.Reserved = res.Reserved
	rec.Scopes = scopesOf(tx.Reservation())

	settleCtx := context.WithoutCancel(ctx)

	// Written before the call, so a process that dies mid-invocation leaves evidence
	// that a runtime may have run and consumed billable resource.
	c.record(settleCtx, rec, true)

	out, callErr := c.runtimeAPI.InvokeAgentRuntime(ctx, req.Input, req.Options...)
	rec.StreamEstablished = c.engine.Now().Sub(start)

	if callErr != nil {
		// AgentCore refused the invocation, so no runtime ran and no resource was
		// consumed. This is the one release path.
		rec.Status = activity.StatusReleased
		rec.Outcome = activity.OutcomeProviderError
		rec.ActualCost = usage.KnownCost(0)
		rec.Runtime.Note = "the invocation was refused before the runtime ran, so no runtime resource was consumed"
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

	if out == nil {
		// A success with no response is not something the API should do, and throttle
		// cannot tell whether the runtime ran. The hold stays: guessing "free" would be
		// a silent understatement.
		rec.Status = activity.StatusOutstanding
		rec.Outcome = activity.OutcomeAccountingError
		rec.ActualCost = usage.UnknownCost("the provider returned no response")
		rec.Latency = c.engine.Now().Sub(start)
		rec.CompletedAt = c.engine.Now()
		err := fmt.Errorf("%w: the provider returned no response, so reservation %s is left outstanding",
			ErrAccounting, res.ReservationID)
		rec.Error = err.Error()
		c.record(settleCtx, rec, false)
		return nil, err
	}

	// The identifiers AgentCore echoes are authoritative over the ones the caller
	// asked for: the SDK fills in a runtime session when the caller supplied none, and
	// the session the service used is the one the resource telemetry will name.
	rec.Runtime = observeRuntime(rec.Runtime, out)
	res.Runtime = rec.Runtime

	// Metadata but no body is a legitimate response: the runtime ran and answered with
	// nothing to read. It becomes an immediate EOF rather than a special case, so the
	// terminal transition has exactly one implementation.
	body := out.Response
	if body == nil {
		body = io.NopCloser(emptyReader{})
	}

	s := &RuntimeStream{
		client:    c,
		tx:        tx,
		body:      body,
		out:       detachBody(out),
		ctx:       ctx,
		settleCtx: settleCtx,
		start:     start,
		rec:       rec,
		pending:   res,
	}
	// The bound and the keep-alive are armed last, after pending is in place: both can
	// reach the terminal transition, and neither may observe a half-built stream.
	lease := tx.Reservation().Lease
	s.idle = newIdleBound(c.stallBound(lease), s.abandon)
	s.startKeepAlive(lease)
	return s, nil
}

// runtimeCostNote is the standing accounting limitation of an outer runtime
// invocation, recorded on the result and on the durable record so that an unknown
// cost is never read as a missing one.
const runtimeCostNote = "AgentCore Runtime reports CPU and memory consumption out of band and after the fact, so this invocation's runtime cost is unknown rather than zero, pending reconciliation"

// estimateRuntime builds the pre-admission estimate for a runtime invocation.
//
// Usage is empty and cost comes only from the declared exposure. There is nothing to
// estimate from: the payload is opaque bytes bound for arbitrary code, and the
// billable dimensions are CPU time and memory-time that nothing knows in advance.
//
// With no exposure declared the quality is genuinely unknown rather than heuristic.
// A heuristic estimate is an informed guess; this would be no guess at all.
func (c *Client) estimateRuntime(maxExposure money.Money) usage.Estimate {
	id := usage.ModelIdentity{
		AccessProvider:  AccessProvider,
		ProviderModelID: AgentCoreRuntimeResource,
		Operation:       OperationInvokeAgentRuntime,
		Region:          c.region,
	}
	est := usage.Estimate{Identity: id}
	if maxExposure > 0 {
		est.Cost = usage.KnownCost(maxExposure)
		est.Quality = usage.QualityHeuristic
		est.Note = "held against the caller's declared exposure; AgentCore reports no cost with the invocation and does not stop the runtime at this figure"
		return est
	}
	est.Quality = usage.QualityUnknown
	est.Cost = usage.UnknownCost("an AgentCore runtime invocation's cost is not reported synchronously and no exposure was declared; set RuntimeRequest.MaxExposure to hold against one")
	return est
}

// observeRuntime folds the response's identifiers into the linkage record.
//
// Only identifiers, a status, a content type, and sizes. Deliberately absent:
// RuntimeUserId, which the caller may send as a header and which AgentCore echoes
// nowhere. It is a claim about a person, it is not needed to attribute a resource
// observation to an invocation, and storing an identifying value in a spend database
// because an SDK exposes it is a product decision rather than an implementation one.
// The MCP method and resource names are absent for the adjacent reason: they describe
// what was asked for, which is closer to content than to accounting.
func observeRuntime(rt activity.HostedRuntime, out *bedrockagentcore.InvokeAgentRuntimeOutput) activity.HostedRuntime {
	if s := deref(out.RuntimeSessionId); s != "" {
		rt.SessionID = s
	}
	if t := deref(out.TraceId); t != "" {
		rt.TraceID = t
	}
	rt.ContentType = deref(out.ContentType)
	if out.StatusCode != nil {
		rt.StatusCode = int(*out.StatusCode)
	}
	// The AWS request ID is what an operator quotes when asking the provider what
	// happened. It joins to logs and traces, not to the resource telemetry.
	if id, ok := awsmiddleware.GetRequestIDMetadata(out.ResultMetadata); ok {
		rt.RequestID = id
	}
	return rt
}

// detachBody returns the response with its body removed, so RuntimeStream is the
// only handle to the bytes. The copy is shallow, which is enough: only one pointer
// field changes and the caller never sees the original.
func detachBody(out *bedrockagentcore.InvokeAgentRuntimeOutput) *bedrockagentcore.InvokeAgentRuntimeOutput {
	copied := *out
	copied.Response = nil
	return &copied
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

// startKeepAlive renews the hold while the invocation is live. A hosted agent can
// easily outlive a lease quantum: it may call several models and tools in one turn.
func (s *RuntimeStream) startKeepAlive(lease time.Duration) {
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

// abandon terminates an invocation whose caller stopped reading without closing.
//
// It exists because a live invocation renews a budget hold, so a caller who neither
// reads, closes, nor cancels would otherwise pin a goroutine and keep headroom
// encumbered indefinitely. Closing the body is what unblocks a reader that is parked
// in the transport.
func (s *RuntimeStream) abandon() { s.finish(termStalled, nil) }

// finish performs the one terminal accounting action for this invocation.
//
// Guarded by a sync.Once, so whichever of EOF, a read error, Close, cancellation, or
// abandonment arrives first owns the transition and the rest are no-ops. A concurrent
// caller blocks inside the Once until the transition completes, which is what makes
// Close's "blocks until accounting is done" contract hold without a second channel.
func (s *RuntimeStream) finish(t termination, readErr error) {
	s.once.Do(func() {
		s.idle.stop()
		if s.stopKeepAlive != nil {
			s.stopKeepAlive()
		}
		closeErr := s.body.Close()

		now := s.client.engine.Now()

		s.mu.Lock()
		res := s.pending
		s.rec.Latency = now.Sub(s.start)
		s.rec.CompletedAt = now
		s.rec.Runtime.ResponseBytes = s.bytes
		res.Latency = s.rec.Latency
		res.ResponseBytes = s.bytes
		s.mu.Unlock()

		s.retain(t, res, readErr, closeErr)

		s.mu.Lock()
		renewErr := s.renewErr
		s.mu.Unlock()
		if renewErr != nil {
			// Recorded, not fatal. An expired hold does not change what happened, and
			// tearing the invocation down would forfeit the linkage without recovering a
			// cent.
			s.rec.Error = join(s.rec.Error, fmt.Sprintf("lease renewal failed: %v", renewErr))
		}

		s.mu.Lock()
		res.Runtime = s.rec.Runtime
		s.mu.Unlock()

		s.client.record(s.settleCtx, s.rec, false)

		s.mu.Lock()
		s.res = res
		s.term = true
		s.mu.Unlock()
	})
}

// retain records an invocation that reached the runtime.
//
// Every case leaves the reservation encumbered, and for one reason: the runtime ran.
// It consumed CPU and memory that AWS will bill, in an amount that is not knowable
// from here. Releasing the hold would assert that nothing was spent; settling zero
// would assert that the spend was free. The status is unresolved because that is
// precisely what the cost is, and the outcome distinguishes the operational stories
// that share it.
func (s *RuntimeStream) retain(t termination, res *RuntimeResult, readErr, closeErr error) {
	s.mu.Lock()
	status := s.rec.Runtime.StatusCode
	s.mu.Unlock()

	var reason string
	switch {
	case t == termEnded && readErr != nil:
		s.rec.Outcome = activity.OutcomeProviderError
		reason = "the runtime response failed mid-body, and AgentCore reports no resource consumption with the invocation"
		s.setErr(fmt.Errorf("%w: reading the runtime response failed: %w", ErrProvider, readErr))

	case t == termEnded && status != 0 && status != 200:
		// The platform ran the caller's agent and the agent failed. AgentCore reports
		// that as a normal response with a failing status, and the resource it consumed
		// getting there is billable all the same.
		s.rec.Outcome = activity.OutcomeProviderError
		reason = fmt.Sprintf("the runtime returned status %d, and AgentCore reports no resource consumption with the invocation", status)
		s.setErr(fmt.Errorf("%w: the runtime returned status %d", ErrProvider, status))

	case t == termEnded:
		s.rec.Outcome = activity.OutcomeSuccess
		reason = runtimeCostNote

	case t == termCtx:
		s.rec.Outcome = activity.OutcomeCancelled
		reason = "the invocation was cancelled; the runtime had already started and its resource consumption is unknown"
		if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
			s.rec.Outcome = activity.OutcomeTimeout
			reason = "the invocation timed out; the runtime had already started and its resource consumption is unknown"
		}
		s.setErr(fmt.Errorf("%w: %s (%v), so reservation %s is left outstanding",
			ErrOutcomeUnknown, reason, s.ctx.Err(), res.ReservationID))

	case t == termStalled:
		s.rec.Outcome = activity.OutcomeCancelled
		reason = "the caller stopped reading the runtime response; the runtime ran and its resource consumption is unknown"
		s.setErr(fmt.Errorf("%w: the caller stopped reading the runtime response for %s, so reservation %s is left outstanding",
			ErrOutcomeUnknown, s.idle.timeout, res.ReservationID))

	default: // termClosed
		s.rec.Outcome = activity.OutcomeCancelled
		reason = "the runtime response was closed before it was fully read; the runtime ran and its resource consumption is unknown"
	}

	cost := usage.UnknownCost(reason)
	res.Cost = cost
	s.rec.ActualCost = cost
	s.mu.Lock()
	s.rec.Runtime.Note = reason
	s.mu.Unlock()

	actual := usage.Actual{Identity: res.Identity, Cost: cost}
	if err := s.tx.MarkUnresolved(s.settleCtx, actual); err != nil {
		s.rec.Status = activity.StatusOutstanding
		s.rec.Outcome = activity.OutcomeAccountingError
		s.setErr(fmt.Errorf("%w: %w", ErrAccounting, err))
	} else {
		res.Unresolved = true
		s.rec.Status = activity.StatusUnresolved
	}

	if closeErr != nil {
		s.rec.Error = join(s.rec.Error, fmt.Sprintf("closing the runtime response also failed: %v", closeErr))
	}
}

func (s *RuntimeStream) setErr(err error) {
	s.rec.Error = join(s.rec.Error, err.Error())
	s.mu.Lock()
	s.termErr = err
	s.mu.Unlock()
}

// idleBound abandons an invocation whose caller stops reading without closing.
//
// It is the pull-based counterpart of stall. stall arms a channel for one forwarding
// step in a select, which is the right shape when a pump goroutine owns the stream;
// a body the caller reads directly has no select to arm, so the bound has to fire on
// its own. Same purpose, same guarantee -- a slow consumer is fine, a stopped one is
// not -- different mechanism, and forcing one type to serve both would obscure that
// these are genuinely different stream shapes.
type idleBound struct {
	timeout time.Duration

	mu    sync.Mutex
	timer *time.Timer
}

// newIdleBound arms the bound. A zero or negative timeout means unbounded, which is
// the same trade stall makes for a ledger with leases disabled.
func newIdleBound(timeout time.Duration, fire func()) *idleBound {
	b := &idleBound{timeout: timeout}
	if timeout > 0 {
		b.timer = time.AfterFunc(timeout, fire)
	}
	return b
}

// reset restarts the bound for another read. Resetting an AfterFunc timer that has
// already fired cannot un-fire it, and does not need to: the terminal transition it
// triggered is idempotent.
func (b *idleBound) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Reset(b.timeout)
	}
}

func (b *idleBound) stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
}
