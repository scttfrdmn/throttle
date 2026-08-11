package usage

import "throttle/money"

type ModelIdentity struct {
	AccessProvider string
	Publisher      string
	Family         string
	Model          string
	Endpoint       string
	Region         string
	ServiceTier    string
}

type Amounts struct {
	InputTokens      int64
	OutputTokens     int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// Other is intentionally open for billable dimensions such as images,
	// audio, searches, tool calls, or requests.
	Other map[string]int64
}

type Estimate struct {
	Identity   ModelIdentity
	Amounts    Amounts
	Cost       money.Money
	Confidence string
}

type Actual struct {
	Identity ModelIdentity
	Amounts  Amounts
	Cost     money.Money
}
