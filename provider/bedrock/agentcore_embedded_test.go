package bedrock_test

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/scttfrdmn/throttle/activity"
	"github.com/scttfrdmn/throttle/engine"
	"github.com/scttfrdmn/throttle/provider/bedrock"
)

// The inner half of AgentCore governance, proved rather than described.
//
// A hosted agent has two accounting positions. The outer one is the edge: throttle
// wraps InvokeAgentRuntime, sees an invocation of a runtime, and learns its resource
// cost later and out of band (agentcore.go). The inner one is the agent's own code,
// which invokes models directly.
//
// The architectural claim is that the inner position needs nothing new: hosting
// location does not change model-spend governance. There is no AgentCore budget, no
// AgentCore model ledger, no second provider path, and no AgentCore-specific
// enforcement engine. Code inside a runtime governs its model calls with the same
// engine, the same adapter, and the same records as code anywhere else.
//
// That claim is cheap to state and easy to erode, so it is a test. What follows is
// the shape of a real hosted agent: a handler that runs inside AgentCore, holds
// throttle's ordinary client, and is invoked per request.

// hostedAgent is the agent code, as it would be deployed to an AgentCore runtime.
//
// It holds a *bedrock.Client and nothing else from throttle. Note what is absent:
// no runtime ARN, no session identifier, no reference to the AgentCore package, no
// awareness that it is hosted at all. That absence is the point -- the same struct
// runs unchanged in a Lambda, a container, or a laptop.
type hostedAgent struct {
	client   *bedrock.Client
	budgetID string
}

// handle is the agent's per-invocation entry point: the function an AgentCore
// handler calls with a decoded request payload.
//
// Every model call it makes is governed in real time, before it happens: estimated,
// reserved against the budget chain, executed, and settled on the provider's own
// reported usage. That is stronger than anything the outer edge can offer, and it is
// the mechanism for real-time LLM budget enforcement inside a hosted agent.
func (a *hostedAgent) handle(ctx context.Context, requestID, prompt string) (*bedrock.Result, error) {
	in := request(sonnetID, aws.Int32(2000))
	in.Messages[0].Content = []brtypes.ContentBlock{&brtypes.ContentBlockMemberText{Value: prompt}}
	return a.client.Converse(ctx, bedrock.Request{
		BudgetID:  a.budgetID,
		RequestID: requestID,
		Input:     in,
		Metadata:  map[string]string{"hosted-in": "agentcore"},
	})
}

// Code hosted inside an AgentCore runtime governs its model calls through the
// ordinary core, and the resulting record is indistinguishable from one made outside.
func TestEmbeddedAgentGovernsModelCallsThroughTheOrdinaryCore(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")

	// Exactly the wiring any throttle caller does. There is no AgentCore variant of
	// engine.New, budget.Definition, bedrock.New, or activity.Store to reach for --
	// which is the finding this test exists to hold in place.
	h := newHarness(t, "1000", opt)
	agent := &hostedAgent{client: h.client, budgetID: "team"}

	res, err := agent.handle(context.Background(), "inner-1", "what is the airspeed velocity of an unladen swallow?")
	if err != nil {
		t.Fatalf("the hosted agent's model call: %v", err)
	}

	// Real-time enforcement, settled on measured usage -- not deferred, not unknown.
	if !res.Settled {
		t.Fatal("a model call inside a hosted agent must settle like any other")
	}
	if !res.Cost.Known() {
		t.Errorf("Cost = %v, want known: the model reported its usage synchronously", res.Cost)
	}
	if res.Mode != engine.ModeEnforce {
		t.Errorf("Mode = %q, want enforce", res.Mode)
	}
	if want := dollars(t, "0.0105"); res.Charge.ActualCost != want {
		t.Errorf("ActualCost = %s, want %s", res.Charge.ActualCost, want)
	}

	// The money landed in the same ledger, against the same budget, as any other call.
	tot := h.totals(t)
	if tot.Spent != dollars(t, "0.0105") {
		t.Errorf("Spent = %s, want the model call charged to the ordinary budget", tot.Spent)
	}
	if tot.Reserved != 0 {
		t.Errorf("Reserved = %s, want 0: an inner model call settles rather than staying encumbered", tot.Reserved)
	}

	rec := getRecord(t, acts, "inner-1")
	if rec.Status != activity.StatusSettled {
		t.Errorf("Status = %q, want settled", rec.Status)
	}
	// The record is an ordinary Converse record. In particular it carries no
	// hosted-runtime linkage: the inner call is not an edge invocation, and marking it
	// as one would double-count the same work in two accounting positions.
	if rec.Identity.Operation != bedrock.OperationConverse {
		t.Errorf("Operation = %q, want converse: hosting changes nothing about the call", rec.Identity.Operation)
	}
	if !rec.Runtime.Empty() {
		t.Errorf("Runtime = %+v, want empty: an inner model call is not a runtime invocation", rec.Runtime)
	}
	if !rec.Agent.Empty() {
		t.Errorf("Agent = %+v, want empty", rec.Agent)
	}
	if rec.Metadata["hosted-in"] != "agentcore" {
		t.Error("hosting location belongs in caller metadata, which is where the agent put it")
	}
}

// The inner and outer positions are separate transactions against the same budget,
// and neither reinterprets the other.
//
// An operator asking "what did this agent cost?" gets a complete answer for the model
// spend, and an explicitly incomplete one for the runtime resource -- which is the
// honest shape, because AgentCore reports the latter later and describes it as
// approximate. The alternative would be a single figure that looks complete and is
// not.
func TestEmbeddedModelSpendAndOuterInvocationShareOneBudget(t *testing.T) {
	acts, opt := withActivity(t, t.TempDir()+"/activity.db")
	h := newRuntimeHarness(t, "1000", opt)

	// Outer: the edge invocation, whose runtime cost is unknown until reconciled.
	s, err := h.invokeRuntime(t, context.Background(), "outer-1", dollars(t, "1.00"))
	if err != nil {
		t.Fatalf("InvokeAgentRuntime: %v", err)
	}
	readRuntime(t, s)

	// Inner: a model call the hosted agent made, governed in real time. In production
	// this happens in the runtime's process; here it is the same client, which is
	// precisely the claim.
	agent := &hostedAgent{client: h.client, budgetID: "team"}
	if _, err := agent.handle(context.Background(), "inner-1", "hello"); err != nil {
		t.Fatalf("the hosted agent's model call: %v", err)
	}

	outer, inner := getRecord(t, acts, "outer-1"), getRecord(t, acts, "inner-1")

	// Two transactions, two reservations, one budget.
	if outer.ReservationID == inner.ReservationID {
		t.Error("the edge invocation and the inner model call must be distinct transactions")
	}
	if outer.BudgetID != inner.BudgetID {
		t.Error("both must charge the same budget: throttle has no AgentCore-specific budget")
	}

	// The inner spend is complete; the outer is explicitly not.
	innerAmount, innerComplete := inner.Spent()
	if !innerComplete || innerAmount != dollars(t, "0.0105") {
		t.Errorf("inner spend = %s complete=%v, want a known $0.0105", innerAmount, innerComplete)
	}
	outerAmount, outerComplete := outer.Spent()
	if outerComplete {
		t.Error("the edge invocation's spend must report itself incomplete")
	}
	if outerAmount != 0 {
		t.Errorf("outer spend floor = %s, want 0", outerAmount)
	}

	// So a dashboard sums a known model figure plus an encumbered, unpriced runtime
	// liability -- and knows which is which.
	tot := h.totals(t)
	if tot.Spent != dollars(t, "0.0105") {
		t.Errorf("Spent = %s, want only the priced model call", tot.Spent)
	}
	if tot.Reserved != dollars(t, "1.00") {
		t.Errorf("Reserved = %s, want the runtime exposure still encumbered", tot.Reserved)
	}
}
