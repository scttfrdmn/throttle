package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scttfrdmn/throttle/dashboard"
)

// (16) "config show" reveals no secrets.
//
// The guarantee is structural rather than a filter -- Config has no field for a credential --
// but this test is what would catch somebody adding one, or adding a "helpful" dump of the
// environment. A "throttle config show" pasted into an issue is exactly how a credential
// leaks, so the assertion is on the output, not on the type.
func TestShowRevealsNoSecrets(t *testing.T) {
	// Values a real machine would have set. Set in the process environment on purpose:
	// providerNote reads os.Getenv, and the point is that it reads only the names.
	secrets := map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAIOSFODNN7EXAMPLE",
		"AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"AWS_SESSION_TOKEN":     "FwoGZXIvYXdzEBYaDEXAMPLETOKEN",
		"AWS_PROFILE":           "research-admin",
		"AWS_REGION":            "us-west-2",
		"OPENAI_API_KEY":        "sk-proj-EXAMPLE",

		// Every secret the current Anthropic SDK resolves from the environment. throttle
		// stores none of them and reads none of them: the SDK owns its own credential chain,
		// and the only way to keep it that way is for this surface to know no key names.
		"ANTHROPIC_API_KEY":             "sk-ant-api03-EXAMPLE",
		"ANTHROPIC_AUTH_TOKEN":          "Bearer EXAMPLE-AUTH-TOKEN",
		"ANTHROPIC_PROFILE":             "research-anthropic",
		"ANTHROPIC_FEDERATION_RULE_ID":  "fedrule_EXAMPLE",
		"ANTHROPIC_ORGANIZATION_ID":     "org_EXAMPLE",
		"ANTHROPIC_IDENTITY_TOKEN":      "eyJhbGciOiJSUzI1NiEXAMPLE",
		"ANTHROPIC_IDENTITY_TOKEN_FILE": "/var/run/secrets/anthropic/token",
		"ANTHROPIC_WEBHOOK_SIGNING_KEY": "whsec_EXAMPLE",
		"ANTHROPIC_CUSTOM_HEADERS":      "X-Internal-Auth: EXAMPLE",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}

	cfg := load(t, `
store:
  ledger: /tmp/throttle-test/ledger.db
defaults:
  budget: research
budgets:
  research:
    name: Research group
    amount: $4,000
    period:
      timezone: America/New_York
      anchor: 2026-09-01
    rollover:
      mode: credit
      cap:
        percent: 25
`)

	var b strings.Builder
	if err := cfg.Show(&b); err != nil {
		t.Fatalf("Show: %v", err)
	}
	out := b.String()

	for k, v := range secrets {
		if strings.Contains(out, v) {
			t.Errorf("output contains the value of %s", k)
		}
	}
	// The profile's name is a configuration fact and is allowed; what it resolves to is not.
	// The distinction is only safe because presence is all that is ever read.
	if !strings.Contains(out, "AWS_PROFILE") {
		t.Error("output does not mention that AWS_PROFILE is set")
	}
	if !strings.Contains(out, "values not shown") {
		t.Error("output does not say that values are withheld")
	}
	// A key whose name throttle does not enumerate must not appear at all, not even as a
	// name: an environment dump is how the next credential gets printed. That rule is what
	// makes the growing Anthropic list below cost nothing to keep correct -- throttle does
	// not know these names, so it cannot print them, and a new one added by the SDK
	// tomorrow is covered by the same silence.
	for _, name := range []string{
		"OPENAI_API_KEY",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_PROFILE",
		"ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_ORGANIZATION_ID",
		"ANTHROPIC_IDENTITY_TOKEN", "ANTHROPIC_IDENTITY_TOKEN_FILE",
		"ANTHROPIC_WEBHOOK_SIGNING_KEY", "ANTHROPIC_CUSTOM_HEADERS",
	} {
		if strings.Contains(out, name) {
			t.Errorf("output enumerates %s, which throttle neither stores nor reads", name)
		}
	}
	// "ANTHROPIC" at all would mean the surface grew provider credential awareness.
	if strings.Contains(out, "ANTHROPIC") {
		t.Error("output mentions an ANTHROPIC environment variable: the SDK owns that chain")
	}
	if strings.Contains(out, "AWS_SECRET_ACCESS_KEY") || strings.Contains(out, "AWS_SESSION_TOKEN") {
		t.Error("output names credential variables")
	}
}

// Show answers "why is it using that?" by naming each value's origin.
func TestShowNamesOrigins(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(cfgPath, []byte(`
defaults:
  enforcement: monitor
budgets:
  research:
    amount: $4,000
    period:
      anchor: 2026-09-01
`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := Load(
		envWith(t, dir, map[string]string{EnvConfig: cfgPath, EnvLedger: filepath.Join(dir, "env.db")}),
		Overrides{Listen: ptr("127.0.0.1:9000")},
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var b strings.Builder
	if err := cfg.Show(&b); err != nil {
		t.Fatalf("Show: %v", err)
	}
	out := b.String()

	for _, want := range []string{
		string(FromFile), // enforcement
		string(FromEnv),  // ledger
		string(FromFlag), // listen
		cfgPath,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// (22, the display half) A non-loopback address is called out wherever it came from.
//
// serve prints the warning; show prints the same fact before anybody runs serve. The two are
// tested separately because the failure mode is a setting that looks harmless in one place
// and is only explained in the other.
func TestShowFlagsNonLoopback(t *testing.T) {
	cfg := load(t, `
dashboard:
  listen: 0.0.0.0:7654
`)

	var b strings.Builder
	if err := cfg.Show(&b); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !strings.Contains(b.String(), "NOT loopback") {
		t.Errorf("output does not flag a non-loopback address:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "no authentication") {
		t.Errorf("output does not say why that matters:\n%s", b.String())
	}

	// The loopback default gets no scary label, or the label stops meaning anything.
	quiet := load(t, "dashboard:\n  listen: 127.0.0.1:7654\n")
	var q strings.Builder
	if err := quiet.Show(&q); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if strings.Contains(q.String(), "NOT loopback") {
		t.Error("output flags the loopback default as exposed")
	}
}

// config's copy of the loopback test must agree with the dashboard's own.
//
// The duplication exists so config does not import dashboard in its non-test files: serve
// reads configuration, not the reverse. This is the test that makes the copy safe, and it
// imports dashboard freely because a test may.
func TestLoopbackAgreesWithDashboard(t *testing.T) {
	addrs := []string{
		"127.0.0.1:7654",
		"localhost:7654",
		"[::1]:7654",
		"0.0.0.0:7654",
		":7654",
		"192.168.1.10:7654",
		"[2001:db8::1]:7654",
		"example.com:7654",
		"127.0.0.1",
		"garbage",
		"",
	}
	for _, addr := range addrs {
		t.Run(addr, func(t *testing.T) {
			// dashboard.ExposureWarning is empty exactly when the address is safe, which is
			// the same question IsLoopback answers.
			wantSafe := dashboard.ExposureWarning(addr) == ""
			if got := IsLoopback(addr); got != wantSafe {
				t.Errorf("config.IsLoopback(%q) = %v, dashboard considers it safe = %v",
					addr, got, wantSafe)
			}
		})
	}
}

// The defaults config declares must match the dashboard's own, for the same reason.
func TestDashboardDefaultsAgree(t *testing.T) {
	if DefaultListen != dashboard.DefaultListen {
		t.Errorf("config.DefaultListen = %q, dashboard.DefaultListen = %q",
			DefaultListen, dashboard.DefaultListen)
	}
	if DefaultActivityLimit != dashboard.DefaultActivityLimit {
		t.Errorf("config.DefaultActivityLimit = %d, dashboard.DefaultActivityLimit = %d",
			DefaultActivityLimit, dashboard.DefaultActivityLimit)
	}
}

// Percentages render through integer arithmetic, so a cap reads the same everywhere.
func TestPercentText(t *testing.T) {
	tests := []struct {
		bp   int64
		want string
	}{
		{2500, "25"},
		{1234, "12.34"},
		{1200, "12"},
		{1250, "12.5"},
		{5, "0.05"},
		{1_000_000, "10000"},
	}
	for _, tt := range tests {
		if got := percentText(tt.bp); got != tt.want {
			t.Errorf("percentText(%d) = %q, want %q", tt.bp, got, tt.want)
		}
	}
}

// Show renders the normalized definition, not the file's syntax: the question it answers is
// what throttle will do, not what somebody typed.
func TestShowNormalizesBudgets(t *testing.T) {
	cfg := load(t, `
budgets:
  research:
    amount: $4,000
    period:
      recur: monthly
      timezone: America/New_York
      anchor: 2026-09-01
    borrow: 3d
  award:
    amount: $125,000
    period:
      recur: none
      anchor: 2026-09-01
      end: 2028-08-31
`)

	var b strings.Builder
	if err := cfg.Show(&b); err != nil {
		t.Fatalf("Show: %v", err)
	}
	out := b.String()

	for _, want := range []string{
		"monthly (America/New_York)", // the compiled recurrence with the zone its boundaries fall in
		"fixed term",                 // not described as a period, which would read as recurring
		"72h0m0s",                    // 3d normalized to the duration it is
		"$4000.00",
		"$125000.00",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

// A configuration with no budgets says so rather than printing an empty table.
func TestShowWithNoBudgets(t *testing.T) {
	cfg := load(t, "version: 1\n")
	var b strings.Builder
	if err := cfg.Show(&b); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if !strings.Contains(b.String(), "no budgets defined") {
		t.Errorf("output does not say there are no budgets:\n%s", b.String())
	}
}
