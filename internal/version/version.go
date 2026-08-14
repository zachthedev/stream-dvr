// Package version provides the build's SemVer 2.0.0 version string.
//
// The string is injected at link time via ldflags and falls back to
// runtime/debug BuildInfo when ldflags are absent (bare `go build`,
// `go install pkg@version`, or execution outside a module).
//
// # Output shape
//
// Strict SemVer 2.0.0: MAJOR.MINOR.PATCH[-prerelease][+metadata]
//
//	Clean release tag v1.2.3:         1.2.3
//	Dirty release tag v1.2.3:         1.2.3+dirty
//	Untagged, clean:                  0.0.0-dev+gabc1234
//	Untagged, dirty:                  0.0.0-dev+gabc1234.dirty
//	5 past v1.2.3, clean:             1.2.3-dev.5+gabc1234
//	5 past v1.2.3, dirty:             1.2.3-dev.5+gabc1234.dirty
//	3 past v2.0.0-rc.1, clean:        2.0.0-rc.1.dev.3+gabc1234
//
// Build metadata (after '+') is ignored by semver comparators, so clean
// and dirty binaries from the same commit compare equal. Dirty is
// provenance, not a version ordering.
package version

import (
	"fmt"
	"os/exec"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

// ///////////////////////////////////////////////
// Link-time injection point
// ///////////////////////////////////////////////

// devVersion is what every path falls back to when git, BuildInfo, or the
// tag itself gives nothing usable.
const devVersion = "0.0.0-dev"

// semver is set at build time via:
//
//	go build -ldflags "-X <module>/internal/version.semver=$(VERSION)"
//
// Leave it empty in bare `go build` and Info() will derive from BuildInfo.
var semver = ""

// semverCharacters are the only bytes FromGit will hand back.
//
// Its result is interpolated into a shell word in the Makefile's -ldflags,
// where a backtick or a $( ) is evaluated rather than passed along. git
// check-ref-format accepts a tag name carrying either, and a space is the
// only form it rejects, so a clone with a hostile tag would run its author's
// commands the first time anyone built it. SemVer 2.0.0 uses nothing outside
// this set, so a legitimate tag never trips it.
var semverCharacters = regexp.MustCompile(`^[0-9A-Za-z.+-]+$`)

// ///////////////////////////////////////////////
// BuildInfo fallback
// ///////////////////////////////////////////////

// readBuildInfo is the BuildInfo source fromBuildInfo reads.
//
// It is a variable so a test can supply metadata the host cannot produce,
// such as a build the stdlib reports no BuildInfo for at all.
var readBuildInfo = debug.ReadBuildInfo

// ///////////////////////////////////////////////
// Runtime API
// ///////////////////////////////////////////////

// Info returns the build's SemVer 2.0.0 version string.
//
// Preference:
//  1. ldflags-injected `semver` (release builds via Makefile)
//  2. BuildInfo-derived fallback (bare `go build` in a git checkout)
//  3. devVersion (no VCS info available, e.g., built outside a module)
func Info() string {
	if semver != "" {
		return semver
	}
	return fromBuildInfo()
}

// DockerTag returns Info() with '+' replaced by '-'. Docker image tags and
// OCI artifact references reject '+' per the distribution spec; this is
// the idiomatic mapping.
//
// Precedence remains stable because build metadata is comparator-ignored in
// SemVer, so collapsing it into the pre-release section with '-' still
// sorts identically against the clean tag.
func DockerTag() string {
	return strings.ReplaceAll(Info(), "+", "-")
}

// ///////////////////////////////////////////////
// Build-time helper
// ///////////////////////////////////////////////

// FromGit shells out to `git describe --tags --match v* --always --dirty`
// and reformats the output into strict SemVer 2.0.0.
//
// Intended for use at build time by internal/tools/version, whose stdout
// is fed into Makefile LDFLAGS. Returns devVersion if git is
// unavailable, the tree is not a repository, or the tag carries anything
// outside the SemVer character set.
func FromGit() string {
	desc, err := runGit("describe", "--tags", "--match", "v*", "--always", "--dirty")
	if err != nil || desc == "" {
		return devVersion
	}

	described := parseDescribe(desc)
	if !semverCharacters.MatchString(described) {
		return devVersion
	}
	return described
}

// ///////////////////////////////////////////////
// Internal parsing
// ///////////////////////////////////////////////

// parseDescribe converts git-describe output into strict SemVer 2.0.0.
// Handles three shapes emitted by `git describe --tags --always`:
//
//	<sha>                no tags in repo
//	v<base>              exact tag
//	v<base>-<N>-g<sha>   past tag
//
// A trailing "-dirty" suffix on any shape is peeled off and re-applied
// in the build metadata section (".dirty") per SemVer convention.
func parseDescribe(desc string) string {
	dirty := strings.HasSuffix(desc, "-dirty")
	desc = strings.TrimSuffix(desc, "-dirty")

	// No tags: git describe with --always emits the short SHA directly.
	if isShortSHA(desc) {
		return buildNoTag(desc, dirty)
	}

	// Both exact-tag and past-tag start with "v".
	desc = strings.TrimPrefix(desc, "v")

	if base, ahead, sha, ok := splitPastTag(desc); ok {
		return buildPastTag(base, ahead, sha, dirty)
	}
	return buildExactTag(desc, dirty)
}

// splitPastTag parses "<base>-<N>-g<sha>" from the right. The base can
// itself contain hyphens, as a prerelease tag such as "2.0.0-rc.1" does,
// so the parse peels from the end.
func splitPastTag(s string) (base string, ahead int, sha string, ok bool) {
	gIdx := strings.LastIndex(s, "-g")
	if gIdx < 0 {
		return "", 0, "", false
	}
	sha = s[gIdx+2:]
	if !isHex(sha) || len(sha) < 7 {
		return "", 0, "", false
	}
	rest := s[:gIdx]
	dashIdx := strings.LastIndex(rest, "-")
	if dashIdx < 0 {
		return "", 0, "", false
	}
	n, err := strconv.Atoi(rest[dashIdx+1:])
	if err != nil || n <= 0 {
		return "", 0, "", false
	}
	return rest[:dashIdx], n, sha, true
}

func buildNoTag(sha string, dirty bool) string {
	meta := "g" + sha
	if dirty {
		meta += ".dirty"
	}
	return devVersion + "+" + meta
}

func buildExactTag(base string, dirty bool) string {
	if !dirty {
		return base
	}
	return base + "+dirty"
}

// buildPastTag appends ".dev.<N>" to the existing prerelease chain (if
// the base has one) or opens a new prerelease with "-dev.<N>" (if the
// base is a clean release). Build metadata carries the commit SHA plus
// an optional ".dirty" tag.
func buildPastTag(base string, ahead int, sha string, dirty bool) string {
	var pre string
	if found := strings.Contains(base, "-"); found {
		pre = fmt.Sprintf("%s.dev.%d", base, ahead)
	} else {
		pre = fmt.Sprintf("%s-dev.%d", base, ahead)
	}
	meta := "g" + sha
	if dirty {
		meta += ".dirty"
	}
	return pre + "+" + meta
}

// fromBuildInfo derives a fallback semver from runtime/debug.BuildInfo.
// Used when `semver` is unset (bare `go build` or `go install pkg@ver`).
func fromBuildInfo() string {
	bi, ok := readBuildInfo()
	if !ok {
		return devVersion
	}
	// `go install pkg@v1.2.3` populates Main.Version. Honor it.
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return strings.TrimPrefix(v, "v")
	}
	var sha string
	var dirty bool
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				sha = s.Value[:7]
			} else {
				sha = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if sha == "" {
		return devVersion
	}
	return buildNoTag(sha, dirty)
}

// ///////////////////////////////////////////////
// Low-level helpers
// ///////////////////////////////////////////////

func runGit(args ...string) (string, error) {
	// G204 names a variable reaching a subprocess. Every caller in this
	// package passes literals, this package builds a version string at build
	// time and is never linked into the shipped binary, and no value here
	// comes from a config, a network answer, or a tool's output.
	out, err := exec.Command("git", args...).Output() //nolint:gosec // G204: every argument is a literal in this package
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// isShortSHA reports whether s is 7-40 chars of lowercase hex. Matches
// the shape `git describe --always` emits when no tags exist.
func isShortSHA(s string) bool {
	return isHex(s) && len(s) >= 7 && len(s) <= 40
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
