package anthropic_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	anth "github.com/anthropics/anthropic-sdk-go"

	"github.com/scttfrdmn/throttle/activity"
	anthropic "github.com/scttfrdmn/throttle/provider/anthropic"
	"github.com/scttfrdmn/throttle/usage"
)

// This file is the adversarial half of #29.
//
// Anthropic's usage counters are additive and disjoint. OpenAI's are inclusive and have
// to be subtracted. Both APIs call the field input_tokens, both report a cached count
// beside it, and the two mean opposite things -- so an adapter written by analogy to the
// other one compiles, passes a smoke test, and misprices cache-heavy traffic by orders
// of magnitude. Every test here is built so that the wrong reading fails with a number
// nobody could mistake for rounding.

// The primary adversarial case: a request whose input is almost entirely a cache read.
//
// Anthropic states the relationship twice. The SDK's own Usage doc: "Total input tokens
// in a request is the summation of `input_tokens`, `cache_creation_input_tokens`, and
// `cache_read_input_tokens`." And the API docs, explaining why: "The `input_tokens`
// field represents only the tokens that come after the last cache breakpoint in your
// request -- not all the input tokens you sent."
//
// So a request serving a 100,000-token cached prefix with 50 fresh tokens really did
// consume 100,050 input tokens, and reports 50. An adapter that treats input_tokens as
// the inclusive total -- OpenAI's rule -- has only 50 tokens to apportion, and every
// consistent way of apportioning them is wrong by a factor of thousands.
func TestCacheReadIsAddedToInputNotSubtractedFromIt(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 50,
		"cache_read_input_tokens": 100000,
		"output_tokens": 100
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-cache-read", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	// Copied, not adjusted. 50 is the whole of the fresh input and 100,000 is the whole
	// of the cache read; neither figure is derived from the other.
	if got, ok := res.Usage.Get(usage.InputTokens); !ok || got != 50 {
		t.Errorf("InputTokens = %d (present %v), want 50: Anthropic's input_tokens is already "+
			"exclusive of cache reads, so nothing may be subtracted from it", got, ok)
	}
	if got, ok := res.Usage.Get(usage.CacheReadTokens); !ok || got != 100000 {
		t.Errorf("CacheReadTokens = %d (present %v), want 100000", got, ok)
	}

	// The identity Anthropic publishes: the dimensions sum to the true input total.
	fresh, _ := res.Usage.Get(usage.InputTokens)
	cached, _ := res.Usage.Get(usage.CacheReadTokens)
	if total := fresh + cached; total != 100050 {
		t.Errorf("the normalized input dimensions sum to %d, want 100050 (50 fresh + 100000 read). "+
			"Under OpenAI's inclusive reading the accounted total would be 50 -- two thousand "+
			"times too few tokens", total)
	}

	// 50 fresh at $5.00/M = $0.00025; 100,000 read at $0.50/M = $0.05; 100 output at
	// $25.00/M = $0.0025.
	want := dollars(t, "0.05275")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}

	// The subtractive reading, priced out, so a regression fails with a diagnosis rather
	// than merely a different number. Treating input_tokens as the inclusive total leaves
	// 50 tokens to divide between fresh and cached, so at most 50 tokens can be charged at
	// either input rate: $0.000025 of cache read, no fresh input, plus the same $0.0025 of
	// output. That is $0.002525 -- the input side of the bill understated twentyfold and
	// the cache read itself understated two thousandfold.
	if subtractive := dollars(t, "0.002525"); res.Charge.ActualCost == subtractive {
		t.Errorf("ActualCost = %s: cache reads were subtracted from input_tokens as though "+
			"Anthropic's counters overlapped. They do not -- the total is their sum", subtractive)
	}
	if !res.Cost.Known() {
		t.Errorf("a cache read is a priced dimension, so the cost is fully known: %s", res.Cost.Reason)
	}
}

// Cache reads are not ordinary input at the ordinary input rate. They are a tenth of it,
// and at this scale the difference is dollars rather than rounding.
func TestCacheReadsAreNotPricedAsFreshInput(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 0,
		"cache_read_input_tokens": 2000000,
		"output_tokens": 100
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-cache-rate", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	// 2,000,000 read at $0.50/M = $1.00; nothing fresh; 100 output at $25/M = $0.0025.
	want := dollars(t, "1.0025")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	// Charged at the base input rate the same tokens would cost $10.00 -- a tenfold
	// overcharge, and $9 of it on a single request.
	if atInputRate := dollars(t, "10.0025"); res.Charge.ActualCost == atInputRate {
		t.Errorf("ActualCost = %s: cache reads were charged at the base input rate. Anthropic "+
			"prices a cache read at a tenth of fresh input", atInputRate)
	}
}

// Cache writes are priced by lifetime, so the TTL split is a financial fact rather than
// an observability detail.
//
// The figures are deliberately lopsided: 200,000 tokens written for five minutes and
// 800,000 for an hour. Collapsing them in either direction moves the bill by dollars,
// which is what makes this test say something.
func TestCacheWriteTTLsArePricedSeparately(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 100,
		"cache_creation_input_tokens": 1000000,
		"cache_creation": {
			"ephemeral_5m_input_tokens": 200000,
			"ephemeral_1h_input_tokens": 800000
		},
		"output_tokens": 100
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-ttl", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	if got, ok := res.Usage.Get(usage.CacheWrite5mTokens); !ok || got != 200000 {
		t.Errorf("CacheWrite5mTokens = %d (present %v), want 200000", got, ok)
	}
	if got, ok := res.Usage.Get(usage.CacheWrite1hTokens); !ok || got != 800000 {
		t.Errorf("CacheWrite1hTokens = %d (present %v), want 800000", got, ok)
	}

	// 200,000 at $6.25/M = $1.25; 800,000 at $10.00/M = $8.00; 100 input at $5/M =
	// $0.0005; 100 output at $25/M = $0.0025.
	want := dollars(t, "9.253")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Errorf("both cache-write lifetimes carry rates, so the cost is fully known: %s", res.Cost.Reason)
	}

	// Both collapses, named, because either one is a plausible mistake and neither is
	// within rounding of the truth.
	if all5m := dollars(t, "6.253"); res.Charge.ActualCost == all5m {
		t.Errorf("ActualCost = %s: every cache write was priced at the five-minute rate, "+
			"understating the bill by $3", all5m)
	}
	if all1h := dollars(t, "10.003"); res.Charge.ActualCost == all1h {
		t.Errorf("ActualCost = %s: every cache write was priced at the one-hour rate, "+
			"overstating the bill by 75 cents", all1h)
	}
}

// cache_creation_input_tokens is the sum of its per-TTL children, not an additional
// charge. Pricing both would bill the same tokens twice at a plausible-looking figure.
func TestAggregateCacheWriteIsNotChargedAlongsideItsChildren(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 100,
		"cache_creation_input_tokens": 1000000,
		"cache_creation": {
			"ephemeral_5m_input_tokens": 200000,
			"ephemeral_1h_input_tokens": 800000
		},
		"output_tokens": 100
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-aggregate", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	// The aggregate is fully decomposed, so it is not recorded at all: recording it
	// alongside its children is what would let a catalog price the same tokens twice.
	if n, ok := res.Usage.Get(usage.CacheWriteTokens); ok {
		t.Errorf("CacheWriteTokens = %d is present: cache_creation_input_tokens is the sum of "+
			"its TTL children, so recording it as well would double-count %d tokens", n, n)
	}
	// And the cost is complete, which it could not be if an undifferentiated remainder
	// had been invented.
	if !res.Cost.Known() {
		t.Errorf("a fully decomposed cache write prices completely: %s", res.Cost.Reason)
	}
	if len(res.Cost.Unpriced) != 0 {
		t.Errorf("Unpriced = %v, want empty", res.Cost.Unpriced)
	}
}

// A breakdown that falls short of its aggregate is incomplete, not impossible. The
// decomposed portion is a real floor and the remainder is named rather than assigned a
// lifetime by coin flip -- the two rates differ by 60%.
func TestUndecomposedCacheWriteRemainderIsUnresolvedNotGuessed(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 100,
		"cache_creation_input_tokens": 1000000,
		"cache_creation": {
			"ephemeral_5m_input_tokens": 200000,
			"ephemeral_1h_input_tokens": 700000
		},
		"output_tokens": 100
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-remainder", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved", err)
	}

	// The 100,000 tokens the breakdown did not account for survive, under the dimension
	// that says their lifetime is undetermined.
	if got, ok := res.Usage.Get(usage.CacheWriteTokens); !ok || got != 100000 {
		t.Errorf("CacheWriteTokens = %d (present %v), want 100000: the shortfall is real tokens "+
			"whose lifetime the response did not state", got, ok)
	}
	// The decomposed portion is still priced, so the floor is mathematically valid:
	// 200,000 at $6.25/M = $1.25, 700,000 at $10.00/M = $7.00, plus $0.003 of tokens.
	if res.Cost.State() != usage.CostPartial {
		t.Fatalf("cost state = %v, want CostPartial", res.Cost.State())
	}
	if want := dollars(t, "8.253"); res.Cost.Amount != want {
		t.Errorf("floor = %s, want %s: the priced dimensions are a valid lower bound", res.Cost.Amount, want)
	}
	// And what is missing is named, so a later catalog can resolve it.
	var named bool
	for _, d := range res.Cost.Unpriced {
		if d == usage.CacheWriteTokens {
			named = true
		}
	}
	if !named {
		t.Errorf("Unpriced = %v, want it to name %s", res.Cost.Unpriced, usage.CacheWriteTokens)
	}
	// The hold stays encumbered: money was spent that throttle cannot fully name.
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("Reserved = 0: an unresolved cost must keep its hold encumbered")
	}
	if rec := h.record(t, "req-remainder"); rec.Status != activity.StatusUnresolved {
		t.Errorf("activity status = %q, want %q", rec.Status, activity.StatusUnresolved)
	}
}

// An aggregate smaller than the breakdown it is supposed to be the sum of is impossible.
// Nothing settles, and the hold stays outstanding because the request did run.
func TestCacheBreakdownExceedingItsTotalIsReportedNotClamped(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 100,
		"cache_creation_input_tokens": 500000,
		"cache_creation": {
			"ephemeral_5m_input_tokens": 200000,
			"ephemeral_1h_input_tokens": 700000
		},
		"output_tokens": 100
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-impossible", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrAccounting) {
		t.Fatalf("NewMessage error = %v, want ErrAccounting", err)
	}
	if !errors.Is(err, anthropic.ErrUsageInconsistent) {
		t.Errorf("error %v should identify the inconsistency", err)
	}
	if res.Settled {
		t.Error("nothing may settle when the provider's own figures contradict each other")
	}
	if res.Cost.Known() {
		t.Error("cost must not be known when usage could not be normalized")
	}
	if got := h.totals(t).Reserved; got == 0 {
		t.Error("the reservation should be left outstanding: the request ran and consumed tokens")
	}
	if rec := h.record(t, "req-impossible"); rec.Status != activity.StatusOutstanding {
		t.Errorf("activity status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
}

// An aggregate with no breakdown at all is the same case as a shortfall: every token is
// of undetermined lifetime, and none is guessed at.
func TestAggregateCacheWriteWithoutABreakdownIsWhollyUnresolved(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 100,
		"cache_creation_input_tokens": 400000,
		"output_tokens": 100
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-no-breakdown", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved", err)
	}
	if got, ok := res.Usage.Get(usage.CacheWriteTokens); !ok || got != 400000 {
		t.Errorf("CacheWriteTokens = %d (present %v), want 400000", got, ok)
	}
	for _, d := range []usage.Dimension{usage.CacheWrite5mTokens, usage.CacheWrite1hTokens} {
		if _, ok := res.Usage.Get(d); ok {
			t.Errorf("%s is present, but the response stated no lifetime: a TTL must never be "+
				"inferred when the response is silent", d)
		}
	}
	// The token floor survives -- $0.0005 input plus $0.0025 output -- rather than being
	// discarded or inflated to a guessed write rate.
	if want := dollars(t, "0.003"); res.Cost.Amount != want {
		t.Errorf("floor = %s, want %s", res.Cost.Amount, want)
	}
}

// The request's own cache_control markers never determine the recorded lifetime.
//
// Anthropic documents why this would be wrong rather than merely unwise: the system
// inserts its own breakpoint, and "This automatic breakpoint always uses the default
// 5-minute TTL, independent of any TTL you set on your own `cache_control` markers", so
// a request whose every marker is one-hour can genuinely produce five-minute writes.
func TestRequestedTTLDoesNotDetermineTheRecordedOne(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	ttl1h := anth.NewCacheControlEphemeralParam()
	ttl1h.TTL = anth.CacheControlEphemeralTTLTTL1h
	in.System = []anth.TextBlockParam{{
		Text:         "a long standing instruction worth caching",
		CacheControl: ttl1h,
	}}

	// Every marker in the request asked for an hour. The response says five minutes.
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 100,
		"cache_creation_input_tokens": 500000,
		"cache_creation": {
			"ephemeral_5m_input_tokens": 500000,
			"ephemeral_1h_input_tokens": 0
		},
		"output_tokens": 100
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-ttl-request", Params: in,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	if got, _ := res.Usage.Get(usage.CacheWrite5mTokens); got != 500000 {
		t.Errorf("CacheWrite5mTokens = %d, want 500000: the response is authoritative about what "+
			"was written, not the request", got)
	}
	if got, _ := res.Usage.Get(usage.CacheWrite1hTokens); got != 0 {
		t.Errorf("CacheWrite1hTokens = %d, want 0", got)
	}
	// 500,000 at the five-minute rate of $6.25/M = $3.125, plus $0.003 of tokens. Priced
	// at the requested one-hour rate it would have been $5.00 -- 60% more.
	if want := dollars(t, "3.128"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if atRequestedTTL := dollars(t, "5.003"); res.Charge.ActualCost == atRequestedTTL {
		t.Errorf("ActualCost = %s: the lifetime was taken from the request's cache_control "+
			"markers rather than from the response", atRequestedTTL)
	}
}

// Thinking tokens are already inside output_tokens and billed at the output rate, so
// output is priced once and thinking gets no dimension of its own.
//
// The SDK says so in the field's own documentation: "`output_tokens` remains the
// inclusive, authoritative total used for billing. This object provides a read-only
// decomposition for observability."
func TestThinkingTokensArePricedOnceAsOutput(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000,
		"output_tokens": 500,
		"output_tokens_details": {"thinking_tokens": 400}
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-thinking", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	if got, _ := res.Usage.Get(usage.OutputTokens); got != 500 {
		t.Errorf("OutputTokens = %d, want 500: thinking is inside the authoritative total, so "+
			"nothing is subtracted from it", got)
	}
	// No separate dimension, because a field existing in the SDK is not a reason to
	// invent a billing dimension. Splitting it out would either double the cost of every
	// extended-thinking request or make it settle partially priced for no reason.
	for _, d := range res.Usage.Dimensions() {
		if strings.Contains(string(d), "thinking") || d == usage.ReasoningTokens {
			t.Errorf("usage carries a %q dimension: thinking tokens are output tokens", d)
		}
	}
	// Identical to a request with 500 plain output tokens.
	if want := dollars(t, "0.0175"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Errorf("an extended-thinking request prices completely: %s", res.Cost.Reason)
	}
}

// A dimension the response did not mention is absent, not zero. Recording a zero cache
// read would assert throttle knows the provider charged nothing for one.
func TestAbsentCountersAreNotRecordedAsZero(t *testing.T) {
	h := newHarness(t, "1000")

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-absent", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	for _, d := range []usage.Dimension{
		usage.CacheReadTokens, usage.CacheWriteTokens,
		usage.CacheWrite5mTokens, usage.CacheWrite1hTokens, usage.Searches,
	} {
		if _, ok := res.Usage.Get(d); ok {
			t.Errorf("%s is present, but the response never mentioned it", d)
		}
	}

	// A response that sends the cache_creation object but omits a field is the same
	// case: nothing was said about that lifetime.
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 100,
		"cache_creation_input_tokens": 300000,
		"cache_creation": {"ephemeral_1h_input_tokens": 300000},
		"output_tokens": 100
	}`)
	res, err = h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-absent-2", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if _, ok := res.Usage.Get(usage.CacheWrite5mTokens); ok {
		t.Error("a cache_creation object that omits the five-minute field mentions no five-minute " +
			"write, so the dimension must be absent")
	}
	if got, _ := res.Usage.Get(usage.CacheWrite1hTokens); got != 300000 {
		t.Errorf("CacheWrite1hTokens = %d, want 300000", got)
	}
	if _, ok := res.Usage.Get(usage.CacheWriteTokens); ok {
		t.Error("the one-hour child accounted for the whole aggregate, so there is no remainder")
	}
}

// A usage object reporting nothing at all is not a free request. The hold stays
// outstanding rather than settling at zero.
func TestUsageWithNoTokenCountsIsNotFree(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = reply(t, fmt.Sprintf(`{
		"id": "msg_empty", "type": "message", "role": "assistant", "model": %q,
		"content": [], "stop_reason": "end_turn", "usage": {}
	}`, opus5))

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-nousage", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrAccounting) {
		t.Fatalf("NewMessage error = %v, want ErrAccounting", err)
	}
	if res.Settled {
		t.Error("a message reporting no usage must not settle: usage is a required response field")
	}
	if res.Cost.Known() {
		t.Error("cost must not be known -- and specifically must not be a known zero")
	}
	if rec := h.record(t, "req-nousage"); rec.Status != activity.StatusOutstanding {
		t.Errorf("activity status = %q, want %q", rec.Status, activity.StatusOutstanding)
	}
}

// A negative counter cannot be charged or refunded, so it is reported rather than
// clamped into something plausible.
func TestNegativeCountersAreReported(t *testing.T) {
	for name, body := range map[string]string{
		"input":       `{"input_tokens": -1, "output_tokens": 100}`,
		"output":      `{"input_tokens": 100, "output_tokens": -5}`,
		"cache read":  `{"input_tokens": 100, "output_tokens": 100, "cache_read_input_tokens": -20}`,
		"web search":  `{"input_tokens": 100, "output_tokens": 100, "server_tool_use": {"web_search_requests": -3}}`,
		"5m creation": `{"input_tokens": 100, "output_tokens": 100, "cache_creation": {"ephemeral_5m_input_tokens": -7}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "1000")
			h.api.out = usageReply(t, opus5, body)

			res, err := h.client.NewMessage(context.Background(), anthropic.Request{
				BudgetID: "team", RequestID: "req-negative-" + name, Params: request(opus5, 2000),
			})
			if !errors.Is(err, anthropic.ErrUsageInconsistent) {
				t.Fatalf("NewMessage error = %v, want ErrUsageInconsistent", err)
			}
			if res.Settled {
				t.Error("a negative count must not settle")
			}
		})
	}
}

// Web search is a separately billed unit taken from the authoritative counter, priced
// from the captured fixture, and added to the token cost exactly once.
//
// The count is deliberately large: 4,000 searches at $10 per thousand is $40, which no
// amount of rounding could hide.
func TestWebSearchRequestsArePricedFromTheAuthoritativeCounter(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	in.Tools = []anth.ToolUnionParam{{
		OfWebSearchTool20250305: &anth.WebSearchTool20250305Param{},
	}}
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000,
		"output_tokens": 500,
		"server_tool_use": {"web_search_requests": 4000}
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-search", Params: in,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	if got, ok := res.Usage.Get(usage.Searches); !ok || got != 4000 {
		t.Errorf("Searches = %d (present %v), want 4000", got, ok)
	}
	// $40 of searches on top of $0.0175 of tokens. A server-side tool whose billable
	// count the response reports authoritatively is fully accountable, so the cost stays
	// known -- being separately billed is not the same as being unaccountable.
	if want := dollars(t, "40.0175"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Errorf("web search reports its own billable count, so the cost is complete: %s", res.Cost.Reason)
	}
	// Forgetting the search dimension entirely would have charged $0.0175 -- the failure
	// mode this test exists to catch, and it is off by a factor of two thousand.
	if tokensOnly := dollars(t, "0.0175"); res.Charge.ActualCost == tokensOnly {
		t.Errorf("ActualCost = %s: the web searches were not charged at all", tokensOnly)
	}
}

// Searches are counted from usage, never from the response's content blocks. A failed
// search returns a result block inside an HTTP 200 and Anthropic documents that it is
// not billed, so counting blocks would invent charges out of failures.
func TestFailedSearchesAreNotInventedFromContentBlocks(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	in.Tools = []anth.ToolUnionParam{{
		OfWebSearchTool20250305: &anth.WebSearchTool20250305Param{},
	}}
	// Three search result blocks, two of them errors, and a counter saying one search was
	// billed.
	h.api.out = reply(t, fmt.Sprintf(`{
		"id": "msg_search", "type": "message", "role": "assistant", "model": %q,
		"stop_reason": "end_turn",
		"content": [
			{"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search",
			 "input": {"query": "unladen swallow airspeed"}},
			{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1",
			 "content": {"type": "web_search_tool_result_error", "error_code": "max_uses_exceeded"}},
			{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_2",
			 "content": {"type": "web_search_tool_result_error", "error_code": "unavailable"}},
			{"type": "text", "text": "roughly 11 metres per second"}
		],
		"usage": {"input_tokens": 1000, "output_tokens": 500,
			"server_tool_use": {"web_search_requests": 1}}
	}`, opus5))

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-search-failed", Params: in,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	if got, _ := res.Usage.Get(usage.Searches); got != 1 {
		t.Errorf("Searches = %d, want 1: the authoritative counter says one search was billed, "+
			"whatever the content blocks show", got)
	}
	// One search at $10/1000 = $0.01, plus $0.0175 of tokens.
	if want := dollars(t, "0.0275"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
}

// A usage counter this build has never heard of survives as a namespaced dimension, so
// a new Anthropic charge cannot be silently discarded by a build that predates it.
//
// The presence check is deliberately not the field's Valid method: an unknown JSON field
// lands in the SDK's extra-fields map with its raw text intact and reports Valid() ==
// false in every case -- number, zero, string, and null alike. A guard written the
// obvious way would skip all of them and restore exactly the silent discard this
// prevents.
func TestUnknownUsageCountersSurviveAsUnpricedDimensions(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000,
		"output_tokens": 500,
		"quantum_entanglement_tokens": 250000,
		"cache_creation": {"ephemeral_1d_input_tokens": 4000},
		"server_tool_use": {"holodeck_requests": 12}
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-unknown-dim", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved: an unpriceable counter must not "+
			"settle as though the bill were complete", err)
	}

	// Every unknown counter survives with an identifier a later catalog can price, and
	// with its value intact.
	for dim, want := range map[usage.Dimension]int64{
		"anthropic.quantum_entanglement_tokens":              250000,
		"anthropic.cache_creation.ephemeral_1d_input_tokens": 4000,
		"anthropic.server_tool_use.holodeck_requests":        12,
	} {
		if got, ok := res.Usage.Get(dim); !ok || got != want {
			t.Errorf("%s = %d (present %v), want %d", dim, got, ok, want)
		}
	}
	// The known tokens are still a valid floor.
	if res.Cost.State() != usage.CostPartial {
		t.Fatalf("cost state = %v, want CostPartial", res.Cost.State())
	}
	if want := dollars(t, "0.0175"); res.Cost.Amount != want {
		t.Errorf("floor = %s, want %s", res.Cost.Amount, want)
	}
	// And the durable record names what could not be priced, which is what makes the
	// request resolvable later without calling Anthropic again.
	rec := h.record(t, "req-unknown-dim")
	if len(rec.ActualCost.Unpriced) == 0 {
		t.Error("the record must name the dimensions it could not price")
	}
	if rec.Status != activity.StatusUnresolved {
		t.Errorf("status = %q, want %q", rec.Status, activity.StatusUnresolved)
	}
}

// An unknown usage field that is not a number cannot become a dimension, but it is still
// evidence the response outgrew this build -- so it makes the cost incomplete rather
// than passing unnoticed.
func TestUnreadableUsageFieldsPreventAFullPrice(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000,
		"output_tokens": 500,
		"container_runtime": {"seconds": 42, "tier": "large"}
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-unreadable", Params: request(opus5, 2000),
	})
	if !errors.Is(err, anthropic.ErrCostUnresolved) {
		t.Fatalf("NewMessage error = %v, want ErrCostUnresolved", err)
	}
	if res.Cost.Known() {
		t.Error("a usage field this build cannot read must not leave the cost fully known")
	}
	if !strings.Contains(res.Cost.Reason, "container_runtime") {
		t.Errorf("cost reason %q should name the field it could not read", res.Cost.Reason)
	}
	// The token floor is intact.
	if want := dollars(t, "0.0175"); res.Cost.Amount != want {
		t.Errorf("floor = %s, want %s", res.Cost.Amount, want)
	}
}

// A zero-valued unknown counter is not a charge, so it does not make an otherwise
// complete request unresolved. Presence is what is detected; a count of nothing costs
// nothing.
func TestZeroValuedUnknownCounterDoesNotBlockSettlement(t *testing.T) {
	h := newHarness(t, "1000")
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 1000,
		"output_tokens": 500,
		"future_widget_requests": 0
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-zero-unknown", Params: request(opus5, 2000),
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	if !res.Cost.Known() {
		t.Errorf("a counter reporting zero is not an unpriced charge: %s", res.Cost.Reason)
	}
	if want := dollars(t, "0.0175"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
}

// Every priced dimension at once, which is where a mapping that folds counters together
// is most wrong. The dimensions must be disjoint and must sum back to what Anthropic
// reported.
func TestFullUsageBreakdownPricesDisjointDimensions(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(sonnet5, 4000)
	in.Tools = []anth.ToolUnionParam{{
		OfWebSearchTool20250305: &anth.WebSearchTool20250305Param{},
	}}
	h.api.out = usageReply(t, sonnet5, `{
		"input_tokens": 3000,
		"cache_read_input_tokens": 500000,
		"cache_creation_input_tokens": 120000,
		"cache_creation": {
			"ephemeral_5m_input_tokens": 20000,
			"ephemeral_1h_input_tokens": 100000
		},
		"output_tokens": 2000,
		"output_tokens_details": {"thinking_tokens": 1500},
		"server_tool_use": {"web_search_requests": 300, "web_fetch_requests": 7}
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-full", Params: in,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	for _, c := range []struct {
		dim  usage.Dimension
		want int64
	}{
		{usage.InputTokens, 3000},
		{usage.CacheReadTokens, 500000},
		{usage.CacheWrite5mTokens, 20000},
		{usage.CacheWrite1hTokens, 100000},
		{usage.OutputTokens, 2000},
		{usage.Searches, 300},
	} {
		if got, ok := res.Usage.Get(c.dim); !ok || got != c.want {
			t.Errorf("%s = %d (present %v), want %d", c.dim, got, ok, c.want)
		}
	}

	// Anthropic's own identity: the three input dimensions sum to the total input.
	var in3 int64
	for _, d := range []usage.Dimension{usage.InputTokens, usage.CacheReadTokens, usage.CacheWrite5mTokens, usage.CacheWrite1hTokens} {
		n, _ := res.Usage.Get(d)
		in3 += n
	}
	if in3 != 623000 {
		t.Errorf("the input dimensions sum to %d, want 623000 (3000 + 500000 + 20000 + 100000)", in3)
	}

	// Neither the undifferentiated cache-write dimension nor a thinking dimension exists:
	// the first would double-count the decomposed aggregate, the second the output.
	if _, ok := res.Usage.Get(usage.CacheWriteTokens); ok {
		t.Error("the aggregate cache write must not be recorded alongside its children")
	}
	if _, ok := res.Usage.Get(usage.ReasoningTokens); ok {
		t.Error("thinking tokens are output tokens and get no dimension of their own")
	}

	// claude-sonnet-5: input $2/M, output $10/M, cache read $0.20/M, 5m $2.50/M, 1h $4/M,
	// search $10/1000.
	//   3,000 input     -> $0.006
	//   500,000 read    -> $0.10
	//   20,000 at 5m    -> $0.05
	//   100,000 at 1h   -> $0.40
	//   2,000 output    -> $0.02
	//   300 searches    -> $3.00
	want := dollars(t, "3.576")
	if res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Errorf("every dimension here carries a rate: %s", res.Cost.Reason)
	}
}

// web_fetch_requests is a real counter carrying no surcharge, so it is not a billing
// dimension. Minting a $0 rate to make it look supported would assert a price Anthropic
// has not published; recording it as unpriced would make every web-fetch request settle
// partially priced for no monetary reason.
func TestWebFetchIsNotGivenAFabricatedPrice(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(opus5, 2000)
	in.Tools = []anth.ToolUnionParam{{
		OfWebFetchTool20250910: &anth.WebFetchTool20250910Param{},
	}}
	h.api.out = usageReply(t, opus5, `{
		"input_tokens": 50000,
		"output_tokens": 500,
		"server_tool_use": {"web_fetch_requests": 9}
	}`)

	res, err := h.client.NewMessage(context.Background(), anthropic.Request{
		BudgetID: "team", RequestID: "req-fetch", Params: in,
	})
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}

	for _, d := range res.Usage.Dimensions() {
		if strings.Contains(string(d), "fetch") {
			t.Errorf("usage carries a %q dimension: a web fetch costs the tokens its content "+
				"becomes, which input_tokens already reports", d)
		}
	}
	// 50,000 input at $5/M = $0.25, plus 500 output at $25/M = $0.0125. The fetched
	// content is in the input count, which is the whole charge.
	if want := dollars(t, "0.2625"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}
	if !res.Cost.Known() {
		t.Errorf("web fetch is billed in tokens, so the cost is complete: %s", res.Cost.Reason)
	}
}
