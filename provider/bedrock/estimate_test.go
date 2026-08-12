package bedrock_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/scttfrdmn/throttle/provider/bedrock"
	"github.com/scttfrdmn/throttle/usage"
)

// The central honesty constraint: no Converse estimate may claim to be exact.
// CountTokens returns input tokens only, and output tokens cannot be known before
// generation, so the best available estimate is conservative -- bounded above by
// the output cap.
func TestEstimateIsNeverExact(t *testing.T) {
	h := newHarness(t, "1000")

	est, err := h.client.Estimate(context.Background(), request(sonnetID, aws.Int32(2000)))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Quality == usage.QualityExact {
		t.Error("a Converse estimate must never claim to be exact: output tokens are unknowable preflight")
	}
	if est.Quality != usage.QualityConservative {
		t.Errorf("Quality = %q, want conservative", est.Quality)
	}
}

// With a tokenizer count for input and a caller-set output cap, the estimate is a
// genuine upper bound, so it is safe to reserve against.
func TestEstimateConservativeWithCounterAndCap(t *testing.T) {
	h := newHarness(t, "1000")
	h.counter.tokens = 1234

	est, err := h.client.Estimate(context.Background(), request(sonnetID, aws.Int32(2000)))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Quality != usage.QualityConservative {
		t.Errorf("Quality = %q, want conservative", est.Quality)
	}
	if got := est.Usage.Count(usage.InputTokens); got != 1234 {
		t.Errorf("input tokens = %d, want the counted 1234", got)
	}
	if got := est.Usage.Count(usage.OutputTokens); got != 2000 {
		t.Errorf("output tokens = %d, want the caller's cap of 2000", got)
	}
	if h.counter.calls != 1 {
		t.Errorf("CountTokens called %d times, want 1", h.counter.calls)
	}

	// $3/M * 1234 + $15/M * 2000 = 3702 + 30000 microdollars.
	if want := dollars(t, "0.033702"); est.Cost.Amount != want {
		t.Errorf("cost = %s, want %s", est.Cost.Amount, want)
	}
}

// Without a caller-set cap, throttle has to assume one. The estimate stays
// conservative against that assumption, but the note must say the number came from
// throttle rather than from the request.
func TestEstimateWithoutMaxTokensNotesTheAssumption(t *testing.T) {
	h := newHarness(t, "1000")

	est, err := h.client.Estimate(context.Background(), request(sonnetID, nil))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got := est.Usage.Count(usage.OutputTokens); got != bedrock.DefaultMaxOutputTokens {
		t.Errorf("output tokens = %d, want the default %d", got, bedrock.DefaultMaxOutputTokens)
	}
	if !strings.Contains(est.Note, "MaxTokens") {
		t.Errorf("the note must disclose that throttle supplied the cap: %q", est.Note)
	}
}

// A CountTokens failure must degrade the estimate, not fail the request -- and the
// estimate must stop calling itself conservative, because the fallback can
// undercount.
func TestEstimateHeuristicWhenCounterFails(t *testing.T) {
	h := newHarness(t, "1000")
	h.counter.err = errors.New("ThrottlingException")

	est, err := h.client.Estimate(context.Background(), request(sonnetID, aws.Int32(2000)))
	if err != nil {
		t.Fatalf("a counting failure must not fail estimation: %v", err)
	}
	if est.Quality != usage.QualityHeuristic {
		t.Errorf("Quality = %q, want heuristic: the input count was guessed", est.Quality)
	}
	if est.Usage.Count(usage.InputTokens) == 0 {
		t.Error("the fallback should still produce an input estimate")
	}
	if !strings.Contains(est.Note, "CountTokens failed") {
		t.Errorf("the note must explain the degradation: %q", est.Note)
	}
}

// CountTokens is a billable extra round trip, so a client that did not opt in must
// not make it -- and must label the resulting estimate heuristic.
func TestEstimateWithoutCounterIsHeuristicAndSilent(t *testing.T) {
	h := newHarness(t, "1000", func(c *bedrock.Config) { c.Counter = nil })

	est, err := h.client.Estimate(context.Background(), request(sonnetID, aws.Int32(2000)))
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if est.Quality != usage.QualityHeuristic {
		t.Errorf("Quality = %q, want heuristic", est.Quality)
	}
	if h.counter.calls != 0 {
		t.Error("CountTokens must not be called when the caller did not opt in")
	}
	if !strings.Contains(est.Note, "Config.Counter") {
		t.Errorf("the note should point at how to improve the estimate: %q", est.Note)
	}
}

// The CountTokens request must carry the parts of the Converse request that are
// billed as input, or the count understates the reservation.
func TestEstimateCountTokensCarriesTheRequestContent(t *testing.T) {
	h := newHarness(t, "1000")

	in := request(sonnetID, aws.Int32(2000))
	in.System = []brtypes.SystemContentBlock{
		&brtypes.SystemContentBlockMemberText{Value: "you are a helpful assistant"},
	}

	if _, err := h.client.Estimate(context.Background(), in); err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if len(h.counter.inputs) != 1 {
		t.Fatalf("CountTokens inputs = %d, want 1", len(h.counter.inputs))
	}
	got := h.counter.inputs[0]
	if got.ModelId == nil || *got.ModelId != sonnetID {
		t.Error("CountTokens must count against the model actually being called")
	}
	conv, ok := got.Input.(*brtypes.CountTokensInputMemberConverse)
	if !ok {
		t.Fatalf("CountTokens input type = %T, want the Converse member", got.Input)
	}
	if len(conv.Value.Messages) != len(in.Messages) {
		t.Error("messages were not passed to CountTokens")
	}
	if len(conv.Value.System) != 1 {
		t.Error("system content is billed as input and must be counted")
	}
}

// An unpriced model yields a usable usage estimate with an explicitly unknown
// cost. Estimation itself must not fail: usage is known, only its price is not.
func TestEstimateUnknownModelHasUsageButUnknownCost(t *testing.T) {
	h := newHarness(t, "1000")

	est, err := h.client.Estimate(context.Background(), request("newvendor.unknown-v1:0", aws.Int32(2000)))
	if err != nil {
		t.Fatalf("estimating an unpriced model must not fail: %v", err)
	}
	if est.Usage.Count(usage.InputTokens) == 0 {
		t.Error("usage should still be estimated")
	}
	if est.Cost.Known() {
		t.Errorf("cost must not be known, got %s", est.Cost.Amount)
	}
	if est.Cost.Reason == "" {
		t.Error("an unknown cost must explain itself")
	}
	// Not zero: a caller must not be able to read this as free.
	if est.Cost.Or(-1) != -1 {
		t.Error("an unknown cost must not read back as zero")
	}
}

// A longer prompt must estimate more input tokens than a shorter one. The absolute
// number is a guess; the ordering is the property worth relying on.
func TestEstimateHeuristicScalesWithContent(t *testing.T) {
	h := newHarness(t, "1000", func(c *bedrock.Config) { c.Counter = nil })

	short := request(sonnetID, aws.Int32(100))
	long := request(sonnetID, aws.Int32(100))
	long.Messages[0].Content = []brtypes.ContentBlock{
		&brtypes.ContentBlockMemberText{Value: strings.Repeat("a long prompt. ", 500)},
	}

	se, err := h.client.Estimate(context.Background(), short)
	if err != nil {
		t.Fatalf("Estimate(short): %v", err)
	}
	le, err := h.client.Estimate(context.Background(), long)
	if err != nil {
		t.Fatalf("Estimate(long): %v", err)
	}
	if le.Usage.Count(usage.InputTokens) <= se.Usage.Count(usage.InputTokens) {
		t.Errorf("a longer prompt estimated %d tokens, not more than the shorter %d",
			le.Usage.Count(usage.InputTokens), se.Usage.Count(usage.InputTokens))
	}
}

// A non-text block (an image, say) has no byte length that maps to tokens, so it
// must still contribute something rather than being counted as free.
func TestEstimateHeuristicCountsNonTextBlocks(t *testing.T) {
	h := newHarness(t, "1000", func(c *bedrock.Config) { c.Counter = nil })

	withImage := request(sonnetID, aws.Int32(100))
	withImage.Messages[0].Content = []brtypes.ContentBlock{
		&brtypes.ContentBlockMemberImage{Value: brtypes.ImageBlock{
			Format: brtypes.ImageFormatPng,
			Source: &brtypes.ImageSourceMemberBytes{Value: []byte{1, 2, 3}},
		}},
	}

	est, err := h.client.Estimate(context.Background(), withImage)
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	// Three bytes of PNG must not be estimated as one token.
	if got := est.Usage.Count(usage.InputTokens); got < 100 {
		t.Errorf("an image contributed only %d input tokens, which understates it badly", got)
	}
}

func TestEstimateRejectsMissingModelID(t *testing.T) {
	h := newHarness(t, "1000")

	if _, err := h.client.Estimate(context.Background(), nil); err == nil {
		t.Error("a nil input should be rejected")
	}
	if _, err := h.client.Estimate(context.Background(), request("", nil)); err == nil {
		t.Error("an empty model id should be rejected")
	}
}
