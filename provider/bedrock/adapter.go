// Package bedrock will adapt the AWS SDK for Go v2 Bedrock Runtime and Agent
// Runtime clients to throttle's normalized accounting model.
package bedrock

import (
	"context"
	"errors"

	"throttle/provider"
	"throttle/usage"
)

var ErrNotImplemented = errors.New("bedrock adapter not implemented")

type Adapter struct{}

func (Adapter) Name() string { return "aws-bedrock" }

func (Adapter) Estimate(context.Context, any) (usage.Estimate, error) {
	return usage.Estimate{}, ErrNotImplemented
}

func (Adapter) Prepare(_ context.Context, request any, _ provider.Preparation) (any, error) {
	return request, nil
}

func (Adapter) Observe(context.Context, any) (usage.Actual, error) {
	return usage.Actual{}, ErrNotImplemented
}
