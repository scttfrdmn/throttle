package bedrock

import "testing"

// Identity is derived structurally from Bedrock's documented ID convention, not
// from a lookup table, so a model released after this code was written still
// yields a publisher and family. The cases that matter most are the last two:
// unknown models must remain usable.
func TestIdentify(t *testing.T) {
	cases := []struct {
		name      string
		modelID   string
		publisher string
		family    string
		canonical string
		profile   string
	}{
		{
			name:      "plain foundation model",
			modelID:   "anthropic.claude-sonnet-4-5-20250929-v1:0",
			publisher: "anthropic",
			family:    "anthropic.claude-sonnet",
			canonical: "anthropic.claude-sonnet-4-5-20250929-v1",
		},
		{
			name:      "cross-region inference profile",
			modelID:   "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
			publisher: "anthropic",
			family:    "anthropic.claude-sonnet",
			canonical: "anthropic.claude-sonnet-4-5-20250929-v1",
			// The geography prefix means the request was routed, which is worth
			// recording -- but the raw ID keeps it, since the bill does.
			profile: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		},
		{
			name:      "european routing",
			modelID:   "eu.anthropic.claude-haiku-4-5-20251001-v1:0",
			publisher: "anthropic",
			family:    "anthropic.claude-haiku",
			canonical: "anthropic.claude-haiku-4-5-20251001-v1",
			profile:   "eu.anthropic.claude-haiku-4-5-20251001-v1:0",
		},
		{
			name:      "amazon nova",
			modelID:   "amazon.nova-pro-v1:0",
			publisher: "amazon",
			family:    "amazon.nova-pro",
			canonical: "amazon.nova-pro-v1",
		},
		{
			name:      "meta llama",
			modelID:   "meta.llama3-70b-instruct-v1:0",
			publisher: "meta",
			family:    "meta.llama",
			canonical: "meta.llama3-70b-instruct-v1",
		},
		{
			name:      "an unrecognized publisher still parses structurally",
			modelID:   "newvendor.someline-2-v1:0",
			publisher: "newvendor",
			family:    "newvendor.someline",
			// Not in knownFamilies, so throttle declines to claim it recognizes it.
			canonical: "",
		},
		{
			name:      "a model id that follows no convention is still usable",
			modelID:   "completely-opaque-identifier",
			publisher: "",
			family:    "",
			canonical: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := Identify(c.modelID, "us-east-1", "")

			// Raw identity is authoritative and must be verbatim, always.
			if id.ProviderModelID != c.modelID {
				t.Errorf("ProviderModelID = %q, want the verbatim %q", id.ProviderModelID, c.modelID)
			}
			if !id.Valid() {
				t.Error("every identify result must be valid enough to account for a request")
			}
			if id.AccessProvider != AccessProvider {
				t.Errorf("AccessProvider = %q, want %q", id.AccessProvider, AccessProvider)
			}
			if id.Operation != OperationConverse {
				t.Errorf("Operation = %q, want %q", id.Operation, OperationConverse)
			}
			if id.Region != "us-east-1" {
				t.Errorf("Region = %q, want us-east-1", id.Region)
			}

			if id.Publisher != c.publisher {
				t.Errorf("Publisher = %q, want %q", id.Publisher, c.publisher)
			}
			if id.Family != c.family {
				t.Errorf("Family = %q, want %q", id.Family, c.family)
			}
			if id.CanonicalModel != c.canonical {
				t.Errorf("CanonicalModel = %q, want %q", id.CanonicalModel, c.canonical)
			}
			if id.InferenceProfile != c.profile {
				t.Errorf("InferenceProfile = %q, want %q", id.InferenceProfile, c.profile)
			}
		})
	}
}

// A provisioned-throughput or custom-model ARN is a valid model reference. Identity
// must not fall apart on it, and the ARN itself is the profile.
func TestIdentifyARN(t *testing.T) {
	const arn = "arn:aws:bedrock:us-east-1:123456789012:provisioned-model/abc123"
	id := Identify(arn, "us-east-1", "")

	if id.ProviderModelID != arn {
		t.Errorf("ProviderModelID = %q, want the verbatim ARN", id.ProviderModelID)
	}
	if id.InferenceProfile != arn {
		t.Errorf("InferenceProfile = %q, want the ARN", id.InferenceProfile)
	}
	if !id.Valid() {
		t.Error("an ARN-identified model must still be valid")
	}

	// An inference-profile ARN whose resource segment carries a model reference
	// should still yield a publisher.
	const profileARN = "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	pid := Identify(profileARN, "us-east-1", "")
	if pid.Publisher != "anthropic" {
		t.Errorf("Publisher = %q, want anthropic from the ARN's resource segment", pid.Publisher)
	}
	if pid.ProviderModelID != profileARN {
		t.Error("the ARN must be retained verbatim as raw identity")
	}
}

// Tier is part of identity because it changes the price.
func TestIdentifyRecordsTier(t *testing.T) {
	id := Identify("amazon.nova-lite-v1:0", "eu-west-1", "priority")
	if id.ServiceTier != "priority" {
		t.Errorf("ServiceTier = %q, want priority", id.ServiceTier)
	}
}

func TestFamilyOf(t *testing.T) {
	cases := []struct{ in, family, base string }{
		{"claude-sonnet-4-5-20250929-v1:0", "claude-sonnet", "claude"},
		{"nova-pro-v1:0", "nova-pro", "nova"},
		{"llama3-70b-instruct-v1:0", "llama", "llama"},
		{"titan-text-express-v1", "titan-text-express", "titan"},
		{"4-only-digits", "", ""},
	}
	for _, c := range cases {
		family, base := familyOf(c.in)
		if family != c.family || base != c.base {
			t.Errorf("familyOf(%q) = (%q, %q), want (%q, %q)", c.in, family, base, c.family, c.base)
		}
	}
}
