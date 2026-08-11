package provider

import (
	"context"

	"throttle/usage"
)

// Preparation is a future-facing seam for request controls. v0.1 should mostly
// pass requests through unchanged. Keeping this explicit prevents later policy
// work from contaminating the accounting interfaces.
type Preparation struct {
	Metadata        map[string]string
	MaxOutputTokens *int
	Concise         bool
}

type Adapter interface {
	Name() string
	Estimate(ctx context.Context, request any) (usage.Estimate, error)
	Prepare(ctx context.Context, request any, preparation Preparation) (any, error)
	Observe(ctx context.Context, response any) (usage.Actual, error)
}
