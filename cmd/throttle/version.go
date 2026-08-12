package main

import (
	"runtime/debug"
	"strings"
)

// The version throttle reports.
//
// One source, read by "throttle version" and by the dashboard footer, because a footer that
// disagrees with the command is a footer nobody can use to file a bug report.
//
// Three cases, in order of authority:
//
//   - A release build says what it was told: -ldflags "-X main.version=v0.1.0".
//   - "go install ...@v0.1.0" already knows, because the module version is in the binary.
//   - A build from a checkout says the development version and the commit it came from.
//
// Nothing here is generated, so an ordinary "go build ./cmd/throttle" produces a useful
// answer with no extra step.

// version is set at link time for a release build and is empty in every other build.
var version string

// devVersion is what an unreleased build calls itself: the version being worked towards,
// marked as not being it.
const devVersion = "0.1.0-dev"

// buildVersion is the version to report.
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return devVersion
	}
	if v := info.Main.Version; isRelease(v) {
		return v
	}
	return devVersion + revisionSuffix(info)
}

// isRelease reports whether a recorded module version names a real release.
//
// "(devel)" is what a build from a working tree reported for years. A toolchain that derives
// a version from VCS instead reports a v0.0.0- pseudo-version when no tag is reachable, which
// looks like a release and is not one -- and, until this project is tagged, is what every
// build produces. Neither should be printed as though somebody released it.
func isRelease(v string) bool {
	return v != "" && v != "(devel)" && !strings.HasPrefix(v, "v0.0.0-")
}

// revisionSuffix names the commit a development build came from, so two builds from
// different days are distinguishable. Empty when the build carries no VCS information,
// which is normal for a build from an extracted archive.
func revisionSuffix(info *debug.BuildInfo) string {
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		return ""
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified {
		// Uncommitted changes, so the commit alone does not describe this binary.
		return "+" + revision + ".dirty"
	}
	return "+" + revision
}
