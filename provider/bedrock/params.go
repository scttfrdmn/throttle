package bedrock

import (
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// params is the subset of a Bedrock request that admission needs: the fields that
// determine identity, the input token count, and the output ceiling.
//
// ConverseInput and ConverseStreamInput are distinct SDK types with the same
// relevant fields, and both must be estimated, identified, and quoted by exactly
// one code path -- otherwise the streaming and non-streaming halves of the adapter
// could drift into pricing the same request differently, which is the one
// inconsistency this package cannot tolerate.
//
// It holds references to the caller's slices rather than copies. Nothing here
// mutates them, and copying prompt content to read its length would be pointless
// work on the request path.
type params struct {
	// operation distinguishes converse from converse-stream on the identity.
	operation string

	modelID     string
	serviceTier *types.ServiceTier

	messages    []types.Message
	system      []types.SystemContentBlock
	toolConfig  *types.ToolConfiguration
	extraFields document.Interface

	// maxTokens is the caller's output cap, or nil when they set none. A
	// caller-supplied cap is a real bound on the request; throttle's default is an
	// assumption, and the estimate reports the difference.
	maxTokens *int32
}

// converseParams reads the fields admission needs out of a non-streaming request.
func converseParams(in *bedrockruntime.ConverseInput) params {
	p := params{
		operation:   OperationConverse,
		serviceTier: in.ServiceTier,
		messages:    in.Messages,
		system:      in.System,
		toolConfig:  in.ToolConfig,
		extraFields: in.AdditionalModelRequestFields,
	}
	if in.ModelId != nil {
		p.modelID = *in.ModelId
	}
	if in.InferenceConfig != nil {
		p.maxTokens = in.InferenceConfig.MaxTokens
	}
	return p
}

// streamParams reads the same fields out of a streaming request.
func streamParams(in *bedrockruntime.ConverseStreamInput) params {
	p := params{
		operation:   OperationConverseStream,
		serviceTier: in.ServiceTier,
		messages:    in.Messages,
		system:      in.System,
		toolConfig:  in.ToolConfig,
		extraFields: in.AdditionalModelRequestFields,
	}
	if in.ModelId != nil {
		p.modelID = *in.ModelId
	}
	if in.InferenceConfig != nil {
		p.maxTokens = in.InferenceConfig.MaxTokens
	}
	return p
}

// countTokensInput builds the CountTokens request for these params. CountTokens
// takes a Converse-shaped payload regardless of which operation will follow, so
// streaming gets the same tokenizer count as non-streaming.
func (p params) countTokensInput() *bedrockruntime.CountTokensInput {
	modelID := p.modelID
	return &bedrockruntime.CountTokensInput{
		ModelId: &modelID,
		Input: &types.CountTokensInputMemberConverse{
			Value: types.ConverseTokensRequest{
				Messages:                     p.messages,
				System:                       p.system,
				ToolConfig:                   p.toolConfig,
				AdditionalModelRequestFields: p.extraFields,
			},
		},
	}
}
