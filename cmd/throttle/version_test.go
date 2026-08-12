package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// The version, at both ends: what the resolver decides, and what a release build reports.
//
// Deliberately not a test that pins the version of the binary running the test suite. That
// value depends on the checkout it was built from -- whether a tag is reachable, whether the
// tree is dirty -- so asserting on it would either encode a moment in this repository's history
// or fail on somebody's machine. What is pinned is the behaviour: which of the three sources
// wins, and that a release build says what it was told.

// A release build reports the version it was given, and does so at the version command and in
// the dashboard footer alike, because both read one variable.
func TestReleaseBuildReportsItsLdflagsVersion(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "throttle")

	build := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.9", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "v9.9.9" {
		t.Errorf("version = %q, want the ldflags value v9.9.9", got)
	}
}

// An ordinary build says something useful without being asked to.
//
// No generated file, no build step, no linker flag: "go build ./cmd/throttle" out of a checkout
// produces a binary that can name itself, because a version that only exists in release
// artifacts is a version nobody can quote in a bug report.
//
// Which useful answer depends on the checkout, so the assertion cannot name one. A tree with no
// reachable tag reports the development version and the commit; a tree checked out *at* a
// release tag reports that release, because it genuinely is that release -- which is what CI
// builds on a tag push. What is asserted is that neither case yields a placeholder, and that a
// commit suffix is attached to the development version rather than to a release.
func TestOrdinaryBuildHasAUsefulVersion(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "throttle")

	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	got := strings.TrimSpace(string(out))

	// Never a placeholder dressed up as a version.
	if got == "" || got == "(devel)" || strings.HasPrefix(got, "v0.0.0-") {
		t.Fatalf("version = %q, which names no code", got)
	}
	if strings.HasPrefix(got, devVersion) {
		// Volatile build metadata is fine in a version string and not in a test expectation:
		// this checks the shape, not the commit.
		if strings.Contains(got, "+") && !strings.HasPrefix(got, devVersion+"+") {
			t.Errorf("version = %q has a suffix that is not attached to the development version", got)
		}
		return
	}
	// Otherwise this checkout sits at a tag and reports it. The toolchain marks a modified tree
	// as "+dirty" of its own accord, which is exactly the disclosure wanted, so a "+" suffix is
	// permitted here -- but the version must still name a release and not a placeholder.
	if !isRelease(got) {
		t.Errorf("version = %q, want either %q with an optional commit suffix, or the release tag this checkout is at", got, devVersion)
	}
}

// buildVersion prefers the linker value over anything the build info says, so a release
// binary cannot be relabelled by whatever happened to be in the module cache.
func TestLdflagsVersionWins(t *testing.T) {
	saved := version
	defer func() { version = saved }()

	version = "v1.2.3"
	if got := buildVersion(); got != "v1.2.3" {
		t.Errorf("buildVersion() = %q, want v1.2.3", got)
	}
}

// isRelease rejects the values that look like releases and are not.
//
// "(devel)" is what a build from a working tree reported for years; a pseudo-version is what a
// VCS-deriving toolchain synthesizes for a commit that is not itself tagged. Printing either as
// a release would put a version in a bug report that names no release.
//
// The v0.1.1-0. case is the one that mattered: checking for a v0.0.0- prefix caught only the
// no-tag-anywhere form, so the moment a commit landed after the v0.1.0 tag every build began
// reporting a confident v0.1.1 that had never been released.
func TestIsRelease(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"v0.1.0", true},
		{"v1.2.3-rc1", true},
		{"v0.1.0+dirty", true},
		{"", false},
		{"(devel)", false},
		{"v0.0.0-20260811120000-abcdef123456", false},
		{"v0.1.1-0.20260812173340-359035f4efbc", false},
		{"v1.2.3-rc2.0.20260812173340-359035f4efbc", false},
	} {
		if got := isRelease(tc.in); got != tc.want {
			t.Errorf("isRelease(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// isPseudoVersion matches the shape the toolchain actually produces, and nothing else.
//
// A release version that merely contains a hyphen and some digits is not a pseudo-version, and
// rejecting one would make a legitimately tagged release report itself as a development build.
func TestIsPseudoVersion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"v0.0.0-20260811120000-abcdef123456", true},
		{"v0.1.1-0.20260812173340-359035f4efbc", true},
		{"v1.2.3-rc2.0.20260812173340-359035f4efbc", true},
		{"v0.1.0", false},
		{"v1.2.3-rc1", false},
		{"v2.0.0-beta.1", false},
		// A timestamp of the wrong length, and a revision of the wrong length: both are
		// somebody's real version string rather than a synthesized one.
		{"v0.1.1-0.2026081217334-359035f4efbc", false},
		{"v0.1.1-0.20260812173340-359035f4efb", false},
		// Upper-case hex is not how the toolchain writes a revision.
		{"v0.1.1-0.20260812173340-359035F4EFBC", false},
		{"", false},
		{"v0.1.1-0.20260812173340-", false},
	} {
		if got := isPseudoVersion(tc.in); got != tc.want {
			t.Errorf("isPseudoVersion(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// revisionSuffix distinguishes two development builds, and marks one with uncommitted changes.
func TestRevisionSuffix(t *testing.T) {
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef0123"},
	}}
	if got := revisionSuffix(info); got != "+0123456789ab" {
		t.Errorf("revisionSuffix = %q, want the abbreviated commit", got)
	}

	info.Settings = append(info.Settings, debug.BuildSetting{Key: "vcs.modified", Value: "true"})
	if got := revisionSuffix(info); got != "+0123456789ab.dirty" {
		t.Errorf("revisionSuffix = %q, want the commit marked dirty", got)
	}

	// A build from an extracted archive carries no VCS information, and inventing one would
	// name a commit that does not describe the binary.
	if got := revisionSuffix(&debug.BuildInfo{}); got != "" {
		t.Errorf("revisionSuffix with no VCS info = %q, want empty", got)
	}
}

// The dashboard footer and the version command are the same value.
//
// A footer with its own literal is a footer that disagrees with the command the moment one of
// them is updated, and a screenshot is the main reason anybody looks at the footer at all.
func TestDashboardFooterUsesTheVersionCommandsValue(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "throttle")
	build := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.9-footer", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	home := t.TempDir()
	env := []string{
		"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TZ=UTC",
		"XDG_CONFIG_HOME=", "XDG_DATA_HOME=",
		"THROTTLE_CONFIG=", "THROTTLE_LEDGER=", "THROTTLE_ACTIVITY=",
	}
	for _, args := range [][]string{{"init"}, {"config", "apply"}} {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	addr := freeAddr(t)
	serve := exec.Command(bin, "serve", "-listen", addr)
	serve.Env = env
	if err := serve.Start(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer func() {
		serve.Process.Kill()
		serve.Wait()
	}()

	base := "http://" + addr
	waitForHTTP(t, base+"/healthz")

	body := fetch(t, base+"/")
	if !strings.Contains(body, "v9.9.9-footer") {
		t.Errorf("the dashboard footer does not carry the build's version:\n%s", truncate(body))
	}
}
