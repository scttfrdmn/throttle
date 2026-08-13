package openai_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"

	activitysqlite "github.com/scttfrdmn/throttle/activity/sqlite"
	"github.com/scttfrdmn/throttle/budget"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/ledger"
	"github.com/scttfrdmn/throttle/ledger/sqlite"
	openai "github.com/scttfrdmn/throttle/provider/openai"
	"github.com/scttfrdmn/throttle/usage"
)

// event builds a stream event from JSON rather than by assigning struct fields.
//
// The same reason respond does it: the SDK reports field presence through unexported
// metadata that only its unmarshaller populates, and the streaming path reads presence
// to decide whether an event carries a Response at all. Every event in the union is
// one flat struct with a Response field, so a struct literal would leave every event
// looking like it carried an empty terminal Response -- exercising a state the wire
// cannot produce, and the exact state the terminality check exists to reject.
func event(t *testing.T, body string) responses.ResponseStreamEventUnion {
	t.Helper()
	var ev responses.ResponseStreamEventUnion
	if err := json.Unmarshal([]byte(body), &ev); err != nil {
		t.Fatalf("unmarshalling a stream event fixture: %v", err)
	}
	return ev
}

// createdEvent is the first event of any Responses stream: a Response in progress.
// It carries a full Response and is deliberately not terminal.
func createdEvent(t *testing.T, model string) responses.ResponseStreamEventUnion {
	t.Helper()
	return event(t, fmt.Sprintf(`{
		"type": "response.created", "sequence_number": 0,
		"response": {"id": "resp_stream", "object": "response", "status": "in_progress", "model": %q}
	}`, model))
}

// deltaEvent is an ordinary content event: text on its way to the caller, and nothing
// throttle may either persist or count.
func deltaEvent(t *testing.T, seq int, text string) responses.ResponseStreamEventUnion {
	t.Helper()
	return event(t, fmt.Sprintf(`{
		"type": "response.output_text.delta", "sequence_number": %d,
		"item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": %q
	}`, seq, text))
}

// completedEvent is the terminal accounting boundary of a normal stream.
func completedEvent(t *testing.T, model string, in, out int64) responses.ResponseStreamEventUnion {
	t.Helper()
	return event(t, fmt.Sprintf(`{
		"type": "response.completed", "sequence_number": 99,
		"response": {
			"id": "resp_stream", "object": "response", "status": "completed", "model": %q,
			"usage": {"input_tokens": %d, "output_tokens": %d, "total_tokens": %d}
		}
	}`, model, in, out, in+out))
}

// normalEventStream is the ordinary shape: created, some text, then completed.
func normalEventStream(t *testing.T, model string, in, out int64) []responses.ResponseStreamEventUnion {
	t.Helper()
	return []responses.ResponseStreamEventUnion{
		createdEvent(t, model),
		deltaEvent(t, 1, "the airspeed "),
		deltaEvent(t, 2, "velocity is "),
		completedEvent(t, model, in, out),
	}
}

// fakeEventStream stands in for *ssestream.Stream, and follows its contract exactly:
// Next reports false at the end or on error, Current returns the last event by value,
// Err reports what ended it, and Close releases the body.
//
// Written against the SDK's semantics rather than a convenient approximation, since
// what is under test is throttle's handling of those semantics. In particular Err
// returns nil after a clean end of stream, which is why an EOF cannot be read as
// success.
type fakeEventStream struct {
	mu sync.Mutex

	events []responses.ResponseStreamEventUnion
	i      int
	cur    responses.ResponseStreamEventUnion

	// err is what Err reports once the events run out. Nil means a clean end, which
	// the SDK reports identically to a finished generation.
	err error

	// establishErr, when set, is reported by Err before any event is read -- the shape
	// an establishment failure takes, since NewStreaming has no error return.
	establishErr error

	// block gates the next read, standing in for a provider that is still generating.
	// A test closes it to let the read proceed.
	block chan struct{}

	// blockAt is the event index the block applies to. -1 blocks every read.
	blockAt int

	// bodyClosed makes reads fail after Close, the way closing an HTTP response body
	// under an SSE decoder does.
	bodyClosed bool

	closed  int
	reads   int
	entered chan struct{}
}

func newFakeEventStream(events ...responses.ResponseStreamEventUnion) *fakeEventStream {
	return &fakeEventStream{events: events, blockAt: -1, entered: make(chan struct{}, 64)}
}

func (f *fakeEventStream) Next() bool {
	f.mu.Lock()
	if f.establishErr != nil {
		f.mu.Unlock()
		return false
	}
	f.reads++
	i := f.i
	block := f.block
	blockAt := f.blockAt
	f.mu.Unlock()

	select {
	case f.entered <- struct{}{}:
	default:
	}

	if block != nil && (blockAt < 0 || blockAt == i) {
		<-block
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bodyClosed || f.i >= len(f.events) {
		return false
	}
	f.cur = f.events[f.i]
	f.i++
	return true
}

func (f *fakeEventStream) Current() responses.ResponseStreamEventUnion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur
}

func (f *fakeEventStream) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.establishErr != nil {
		return f.establishErr
	}
	// The SDK only reports its terminal error once the stream has stopped producing.
	if f.i < len(f.events) {
		return nil
	}
	return f.err
}

func (f *fakeEventStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	f.bodyClosed = true
	// Unblocks a reader parked inside Next, which is what closing the response body
	// does to a real SSE read.
	f.unblockLocked()
	return nil
}

func (f *fakeEventStream) unblockLocked() {
	if f.block == nil {
		return
	}
	select {
	case <-f.block:
	default:
		close(f.block)
	}
	f.block = nil
}

func (f *fakeEventStream) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeEventStream) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// blockFrom makes the read at index i wait until the returned channel is closed.
func (f *fakeEventStream) blockFrom(i int) chan struct{} {
	ch := make(chan struct{})
	f.mu.Lock()
	f.block = ch
	f.blockAt = i
	f.mu.Unlock()
	return ch
}

// release lets a blocked read proceed, standing in for the provider finally producing
// the event the caller is parked on.
//
// The race tests need this rather than closing the channel themselves, because Close
// unblocks the same reader by the same means: whichever of the two the scheduler runs
// second would be closing an already-closed channel. Deciding that under the stream's
// own lock is the only way to let a test release an event and close the stream at the
// same instant, which is exactly the interleaving those tests exist to explore.
func (f *fakeEventStream) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unblockLocked()
}

// fakeStreamAPI stands in for the OpenAI client's NewStreaming call. Its signature
// mirrors the SDK's, error return included -- which is to say excluded.
type fakeStreamAPI struct {
	mu sync.Mutex

	// stream is returned by the next call. nil means the call returns no stream at
	// all, which is the pathological case the adapter still has to account for.
	stream *fakeEventStream

	// streams, when non-empty, hands out one stream per call, for tests that run
	// several concurrently.
	streams []*fakeEventStream

	// factory, when set, builds a fresh stream per call. Concurrent streams need one
	// each: *ssestream.Stream is not safe for concurrent use, and neither is this, so
	// sharing one would test a state the SDK cannot produce.
	factory func() *fakeEventStream

	// nilStream forces a nil return, distinct from an unset stream.
	nilStream bool

	calls  int
	params []responses.ResponseNewParams
	ctxs   []context.Context
}

func (f *fakeStreamAPI) NewStreaming(ctx context.Context, body responses.ResponseNewParams, _ ...option.RequestOption) openai.EventStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.params = append(f.params, body)
	f.ctxs = append(f.ctxs, ctx)
	if f.nilStream {
		return nil
	}
	if f.factory != nil {
		return f.factory()
	}
	if len(f.streams) > 0 {
		s := f.streams[0]
		f.streams = f.streams[1:]
		return s
	}
	return f.stream
}

func (f *fakeStreamAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeStreamAPI) lastParams() responses.ResponseNewParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.params) == 0 {
		return responses.ResponseNewParams{}
	}
	return f.params[len(f.params)-1]
}

// streamHarness is the Responses harness plus a streaming client.
type streamHarness struct {
	*harness
	stream *fakeStreamAPI
	up     *fakeEventStream
}

// newStreamHarness builds a governed streaming client on the frozen test clock.
//
// generousStall by default: the abandonment bound defaults to the reservation lease,
// and a test that is not about abandonment should not be racing it.
func newStreamHarness(t *testing.T, allocation string, events []responses.ResponseStreamEventUnion, opts ...func(*openai.Config)) *streamHarness {
	t.Helper()
	up := newFakeEventStream(events...)
	api := &fakeStreamAPI{stream: up}
	h := newHarness(t, allocation, append([]func(*openai.Config){withStream(api), generousStall}, opts...)...)
	return &streamHarness{harness: h, stream: api, up: up}
}

// newStreamHarnessWithLease builds a streaming client on a real clock with the given
// lease, so a renewal can be observed within a test rather than in fifteen minutes.
//
// The clock stays real because lease expiry is compared against it inside the ledger
// and renewal is driven by a ticker; freezing it would stop the mechanism under test
// rather than control it. Tests get their determinism from waiting for the renewal
// itself, not from sleeping a guessed interval.
//
// A child-of-parent hierarchy, so the same harness serves the ancestor-consumption
// tests.
func newStreamHarnessWithLease(t *testing.T, allocation string, lease time.Duration, events []responses.ResponseStreamEventUnion, opts ...func(*openai.Config)) *streamHarness {
	t.Helper()

	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := func() time.Time { return time.Now().UTC() }

	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock, Lease: lease})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	anchor := clock().Truncate(24 * time.Hour)
	for _, def := range []budget.Definition{
		{ID: "team", Allocation: dollars(t, allocation), Recurrence: budget.RecurMonthly, AnchorAt: anchor},
		{ID: "child", ParentID: "team", Allocation: dollars(t, allocation), Recurrence: budget.RecurMonthly, AnchorAt: anchor},
	} {
		if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", def.ID, err)
		}
	}

	up := newFakeEventStream(events...)
	api := &fakeStreamAPI{stream: up}
	h := buildHarness(t, eng, store, clock, append([]func(*openai.Config){withStream(api)}, opts...)...)
	return &streamHarness{harness: h, stream: api, up: up}
}

// newHierarchicalStreamHarness builds a parent and a child with independent
// allocations, so a test can make the ancestor the only thing that can refuse.
//
// Each streaming call gets its own event stream, since concurrent callers each hold
// their own -- the SDK's stream is not safe for concurrent use, and a shared fake
// would test a state that cannot arise.
func newHierarchicalStreamHarness(t *testing.T, parent, child string, opts ...func(*openai.Config)) *streamHarness {
	t.Helper()

	store, err := sqlite.Open(context.Background(), t.TempDir()+"/throttle.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	clock := func() time.Time { return now }
	eng, err := engine.New(engine.Config{Ledger: store, Clock: clock})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	anchor := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, def := range []budget.Definition{
		{ID: "team", Allocation: dollars(t, parent), Recurrence: budget.RecurMonthly, AnchorAt: anchor},
		{ID: "child", ParentID: "team", Allocation: dollars(t, child), Recurrence: budget.RecurMonthly, AnchorAt: anchor},
	} {
		if err := eng.Register(context.Background(), def, engine.ModeEnforce); err != nil {
			t.Fatalf("Register %s: %v", def.ID, err)
		}
	}

	api := &fakeStreamAPI{}
	// A fresh stream per call, built from the same script.
	api.factory = func() *fakeEventStream { return newFakeEventStream(normalEventStream(t, gpt51, 1000, 500)...) }

	h := buildHarness(t, eng, store, clock,
		append([]func(*openai.Config){withStream(api), generousStall}, opts...)...)
	return &streamHarness{harness: h, stream: api}
}

func withStream(api *fakeStreamAPI) func(*openai.Config) {
	return func(c *openai.Config) { c.StreamClient = api }
}

// generousStall decouples the abandonment bound from the lease.
//
// Config.StreamStallTimeout defaults to the reservation lease, and the lease tests
// shrink the lease to milliseconds so a renewal is observable inside a test -- which
// would shrink the abandonment bound with it. The two answer unrelated questions:
// "does a live request renew before its hold expires?" and "when do we give up on a
// caller who walked away?". Left coupled, a test that pauses between reads to observe
// a renewal is also racing its own abandonment timer, and a loaded machine loses that
// race. Abandonment has its own tests, with their own short bound.
func generousStall(c *openai.Config) { c.StreamStallTimeout = 30 * time.Second }

// leaseQuantum is the lease the renewal tests run with: short enough that a renewal
// happens within a test, long enough that a loaded machine does not lose the race it
// is not supposed to be running.
const leaseQuantum = 600 * time.Millisecond

// withActivity attaches an activity store a test can read back.
func withActivity(t *testing.T, path string) (*activitysqlite.Store, func(*openai.Config)) {
	t.Helper()
	store, err := activitysqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("activity open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, func(c *openai.Config) { c.Activity = store }
}

// tokens reads one dimension's count, treating absence as zero.
//
// The presence bit matters to the adapter and not to these assertions: a dimension
// nobody reported and a dimension reported as zero cost the same, and the tests that
// care about presence check the Cost's own state.
func tokens(u usage.Usage, d usage.Dimension) int64 {
	n, _ := u.Get(d)
	return n
}

// drain reads a stream to its end, discarding events, and reports how many arrived.
func drain(t *testing.T, s *openai.Stream) int {
	t.Helper()
	n := 0
	for s.Next() {
		_ = s.Current()
		n++
	}
	return n
}

// reservationOf reads a reservation back from the ledger.
func (h *harness) reservation(t *testing.T, id string) ledger.Reservation {
	t.Helper()
	r, err := h.ledger.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("ledger.Get(%q): %v", id, err)
	}
	return r
}
