package pricing

import (
	"context"
	"time"

	"throttle/money"
	"throttle/usage"
)

type Quote struct {
	Cost          money.Money
	Source        string
	EffectiveFrom time.Time
	Currency      string
}

// Catalog prices normalized billable usage. Production implementations must
// retain provenance and effective dates; provider prices must not be silently
// embedded in business logic.
type Catalog interface {
	Quote(ctx context.Context, identity usage.ModelIdentity, amounts usage.Amounts, at time.Time) (Quote, error)
}
