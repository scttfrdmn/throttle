package bedrock

import (
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/scttfrdmn/throttle/usage"
)

// normalizeTokens converts Bedrock's TokenUsage into throttle's provider-neutral
// dimensions.
//
// This function is the whole point of the adapter boundary: everything above it
// sees dimensions and money, and nothing above it imports an AWS type.
//
// Two details matter for correctness:
//
//   - TotalTokens is ignored. It is a provider convenience that sums dimensions
//     carrying different prices, so costing it would be wrong.
//   - Cache reads and writes are separate dimensions, not adjustments to input.
//     Bedrock reports them alongside InputTokens, and they are priced differently
//     from fresh input -- reads cheaper, writes dearer.
func normalizeTokens(tu *types.TokenUsage) usage.Usage {
	var u usage.Usage
	if tu == nil {
		return u
	}
	if tu.InputTokens != nil {
		u.Set(usage.InputTokens, int64(*tu.InputTokens))
	}
	if tu.OutputTokens != nil {
		u.Set(usage.OutputTokens, int64(*tu.OutputTokens))
	}
	// Absent is distinct from zero: a model with no prompt caching never mentions
	// these, and recording a zero would imply the provider priced them at nothing.
	if tu.CacheReadInputTokens != nil {
		u.Set(usage.CacheReadTokens, int64(*tu.CacheReadInputTokens))
	}
	if tu.CacheWriteInputTokens != nil {
		u.Set(usage.CacheWriteTokens, int64(*tu.CacheWriteInputTokens))
	}
	return u
}

// tierOf reads the service tier out of a request or response, which affects price
// on Bedrock. The response's tier is authoritative: a request may ask for one tier
// and be served by another.
func tierOf(st *types.ServiceTier) string {
	if st == nil {
		return ""
	}
	return string(st.Type)
}
