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
// marked as not being it. Bumped when a release ships, since the version already released is
// no longer the one being worked towards.
const devVersion = "0.2.0-dev"

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
// a version from VCS instead reports a *pseudo-version* for any commit that is not itself
// tagged, which looks like a release and is not one. Neither should be printed as though
// somebody released it.
func isRelease(v string) bool {
	return v != "" && v != "(devel)" && !isPseudoVersion(v)
}

// isPseudoVersion reports whether a version is one the toolchain synthesized for an untagged
// commit.
//
// Matching the trailing shape rather than the leading one is the whole point. Only the first
// of Go's three pseudo-version forms starts with v0.0.0-, and that is the form produced when
// no tag is reachable at all:
//
//	v0.0.0-20260811120000-abcdef123456          no tag anywhere
//	v0.1.1-0.20260812173340-abcdef123456        after the v0.1.0 tag
//	v1.2.3-rc2.0.20260812173340-abcdef123456    after a pre-release tag
//
// The second is what every build of this project produced the moment a commit landed after
// v0.1.0, and checking the prefix let it pass for a release of a version that does not exist.
// What all three share is the ending: a 14-digit UTC timestamp and a 12-character revision.
//
// Note the timestamp's separator differs between the forms -- "-20260811120000" in the first,
// ".20260812173340" in the other two -- so the timestamp is read as the run of digits ending
// the segment before the revision, rather than as a whole hyphen-delimited field.
func isPseudoVersion(v string) bool {
	// Build metadata is not part of the version's identity, and the toolchain appends it: a
	// dirty checkout at an untagged commit records "...-359035f4efbc+dirty". Leaving it on
	// would make the revision segment fail its length check and the whole thing pass for a
	// release, which is the exact bug being fixed.
	if plus := strings.Index(v, "+"); plus >= 0 {
		v = v[:plus]
	}

	i := strings.LastIndex(v, "-")
	if i < 0 {
		return false
	}
	revision := v[i+1:]
	if len(revision) != 12 || !isLowerHex(revision) {
		return false
	}

	rest := v[:i]
	j := strings.LastIndexAny(rest, "-.")
	if j < 0 {
		return false
	}
	timestamp := rest[j+1:]

	return len(timestamp) == 14 && isDigits(timestamp)
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// isLowerHex reports whether s is a non-empty run of lower-case hexadecimal digits, which is
// how the toolchain writes an abbreviated commit.
func isLowerHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return s != ""
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
