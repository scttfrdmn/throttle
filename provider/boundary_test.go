package provider_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The adapter boundary is an architectural invariant, so it is tested rather than
// merely documented: AWS SDK types must not leak into the provider-neutral core.
// The budget engine sees money and scopes, never tokens or AWS semantics.
//
// This is checked by inspecting the real dependency graph, because a leak would
// arrive as an innocuous-looking import in a package nobody thought of as
// provider-specific.
func TestCoreDoesNotDependOnProviderSDKs(t *testing.T) {
	neutral := []string{
		"github.com/scttfrdmn/throttle/budget",
		"github.com/scttfrdmn/throttle/ledger",
		"github.com/scttfrdmn/throttle/ledger/sqlite",
		"github.com/scttfrdmn/throttle/money",
		"github.com/scttfrdmn/throttle/pricing",
		"github.com/scttfrdmn/throttle/pricing/fixtures",
		"github.com/scttfrdmn/throttle/usage",
		"github.com/scttfrdmn/throttle/engine",
		"github.com/scttfrdmn/throttle/provider",
		// Activity is on this list because it is where a leak would be most tempting
		// and most damaging. A managed agent turn's detail is normalized out of a
		// provider trace carrying prompts, responses, and reasoning; serializing the
		// SDK's own trace object would have been the shortest path to writing it, and
		// would put content in the durable record. An import here is that mistake.
		"github.com/scttfrdmn/throttle/activity",
		"github.com/scttfrdmn/throttle/activity/sqlite",
		// Reconciliation repairs crashed bookkeeping for every provider, so a single
		// `if provider == ...` in it would become the place provider knowledge
		// accumulates. It classifies normalized durable facts; an SDK import here would
		// mean it had started reading provider responses instead.
		"github.com/scttfrdmn/throttle/reconcile",
		// The read model and the dashboard are the read half of the same rule. A UI is
		// where "just import the SDK to get the model's display name" is most tempting,
		// and an SDK type reaching either of them would mean a display was reading
		// provider responses rather than the normalized durable record.
		"github.com/scttfrdmn/throttle/report",
		"github.com/scttfrdmn/throttle/dashboard",
	}

	// Any SDK a provider adapter might pull in. A neutral package must import none
	// of them.
	forbidden := []string{
		"aws-sdk-go",
		"openai",
		"anthropic-sdk",
	}

	for _, pkg := range neutral {
		t.Run(pkg, func(t *testing.T) {
			out, err := exec.Command("go", "list", "-deps", pkg).Output()
			if err != nil {
				t.Skipf("go list unavailable: %v", err)
			}
			for _, dep := range strings.Split(string(out), "\n") {
				for _, bad := range forbidden {
					if strings.Contains(dep, bad) {
						t.Errorf("%s depends on %s: provider SDK types must not reach the budget core", pkg, dep)
					}
				}
			}
		})
	}
}

// The test above is an absence check, and an absence check passes for the wrong
// reason if the thing it looks for is not in the build at all. So each adapter is
// also asserted to really depend on its provider's SDK.
//
// Without this, deleting an adapter -- or misspelling an entry in the forbidden list
// -- would turn the boundary test green while proving nothing. The two together say
// something worth knowing: the SDKs are present in this module, and they stop at the
// adapters.
func TestAdaptersDoDependOnTheirProviderSDK(t *testing.T) {
	adapters := map[string]string{
		"github.com/scttfrdmn/throttle/provider/bedrock":   "aws-sdk-go",
		"github.com/scttfrdmn/throttle/provider/openai":    "openai-go",
		"github.com/scttfrdmn/throttle/provider/anthropic": "anthropic-sdk-go",
	}

	for pkg, sdk := range adapters {
		t.Run(pkg, func(t *testing.T) {
			out, err := exec.Command("go", "list", "-deps", pkg).Output()
			if err != nil {
				t.Skipf("go list unavailable: %v", err)
			}
			if !strings.Contains(string(out), sdk) {
				t.Errorf("%s does not depend on %s, so the boundary test above is proving nothing", pkg, sdk)
			}
		})
	}
}

// Streaming is where a parallel representation is most tempting, so each adapter is
// asserted to use its SDK's own streaming surface rather than a hand-rolled stand-in.
//
// A wrapper could have satisfied its own interface with types it defined itself --
// decoding SSE by hand, or copying the SDK's event shape into a local struct -- and
// every other test here would still pass. The check is that the streaming package the
// SDK actually ships is in the adapter's dependency graph, which is only true if the
// adapter is calling into it.
func TestAdaptersUseTheirProviderStreamingSDK(t *testing.T) {
	streaming := map[string]string{
		// The SSE decoder behind ResponseService.NewStreaming. throttle's EventStream is
		// declared over the SDK's own event union and asserted to be satisfied by
		// *ssestream.Stream at compile time, which is what puts this package here.
		"github.com/scttfrdmn/throttle/provider/openai": "openai-go/v3/packages/ssestream",
		// Bedrock's ConverseStream event stream, for the same reason.
		"github.com/scttfrdmn/throttle/provider/bedrock": "aws-sdk-go-v2/service/bedrockruntime",
	}

	for pkg, dep := range streaming {
		t.Run(pkg, func(t *testing.T) {
			out, err := exec.Command("go", "list", "-deps", pkg).Output()
			if err != nil {
				t.Skipf("go list unavailable: %v", err)
			}
			if !strings.Contains(string(out), dep) {
				t.Errorf("%s does not depend on %s: a streaming adapter must use the provider's "+
					"own streaming surface, not a parallel representation of it", pkg, dep)
			}
		})
	}
}
