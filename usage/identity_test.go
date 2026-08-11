package usage_test

import (
	"strings"
	"testing"

	"throttle/usage"
)

// Valid is the minimum needed to account for a request honestly. Canonical
// identity is not part of it, because an unrecognized model must remain usable.
func TestModelIdentityValid(t *testing.T) {
	full := usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "anthropic.claude-sonnet-4-5-20250929-v1:0",
		Operation:       "converse",
	}
	if !full.Valid() {
		t.Error("an identity with provider, model id, and operation should be valid")
	}
	if full.Known() {
		t.Error("Known() should be false without a canonical model")
	}

	for _, c := range []struct {
		name   string
		mutate func(*usage.ModelIdentity)
	}{
		{"no access provider", func(m *usage.ModelIdentity) { m.AccessProvider = "" }},
		{"no provider model id", func(m *usage.ModelIdentity) { m.ProviderModelID = "" }},
		{"no operation", func(m *usage.ModelIdentity) { m.Operation = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := full
			c.mutate(&m)
			if m.Valid() {
				t.Error("should not be valid")
			}
		})
	}
}

// A model throttle cannot name is a legitimate state, not an error: raw identity
// is authoritative and canonical naming is enrichment.
func TestModelIdentityUnknownIsUsable(t *testing.T) {
	m := usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		ProviderModelID: "somepublisher.brand-new-model-v1:0",
		Operation:       "converse",
	}
	if !m.Valid() {
		t.Error("an unrecognized model must still be valid")
	}
	if m.Known() {
		t.Error("Known() should report that enrichment did not resolve")
	}
	// The provider ID must appear in output regardless, since it is what the bill
	// refers to.
	if !strings.Contains(m.Describe(), "somepublisher.brand-new-model-v1:0") {
		t.Errorf("Describe() hid the provider model id: %s", m.Describe())
	}
}

// Access provider and publisher are independent fields. Collapsing them would make
// it impossible to ask either "how much through Bedrock?" or "how much on Claude?".
func TestModelIdentityDescribeKeepsBothIdentities(t *testing.T) {
	m := usage.ModelIdentity{
		AccessProvider:  "aws-bedrock",
		Publisher:       "anthropic",
		CanonicalModel:  "anthropic.claude-sonnet-4-5-20250929-v1",
		ProviderModelID: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Operation:       "converse",
		Region:          "us-east-1",
		ServiceTier:     "priority",
	}
	got := m.Describe()
	for _, want := range []string{
		"aws-bedrock", // access path
		"anthropic",   // publisher
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0", // raw identity, never hidden
		"us-east-1",
		"priority",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}
	if !m.Known() {
		t.Error("Known() should be true with a canonical model")
	}
}
