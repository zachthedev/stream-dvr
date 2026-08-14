package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Info
// ///////////////////////////////////////////////

func TestInfo_PrefersInjectedSemver(t *testing.T) {
	semver = "9.9.9+test"
	t.Cleanup(func() { semver = "" })

	if got := Info(); got != "9.9.9+test" {
		t.Errorf("Info() = %q, want injected %q", got, "9.9.9+test")
	}
}

func TestInfo_FallsBackToBuildInfo(t *testing.T) {
	semver = ""
	t.Cleanup(func() { semver = "" })

	got := Info()
	if got == "" {
		t.Fatal("Info() returned empty string")
	}
	// Must start with a digit (major version) regardless of fallback.
	if got[0] < '0' || got[0] > '9' {
		t.Errorf("Info() = %q, expected to start with a digit", got)
	}
}

// ///////////////////////////////////////////////
// fromBuildInfo (via the readBuildInfo indirection)
// ///////////////////////////////////////////////

// stubBuildInfo swaps readBuildInfo for the duration of a test and restores
// it on cleanup. Also clears semver so fromBuildInfo is the path exercised.
func stubBuildInfo(t *testing.T, bi *debug.BuildInfo, ok bool) {
	t.Helper()
	orig := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, ok }
	semver = ""
	t.Cleanup(func() {
		readBuildInfo = orig
		semver = ""
	})
}

func TestFromBuildInfo_ReadBuildInfoFails(t *testing.T) {
	// debug.ReadBuildInfo returns ok=false in binaries stripped of module
	// info (rare, but possible in custom builds). Fall back to "0.0.0-dev".
	stubBuildInfo(t, nil, false)
	if got := Info(); got != "0.0.0-dev" {
		t.Errorf("Info() = %q, want %q", got, "0.0.0-dev")
	}
}

func TestFromBuildInfo_HonorsGoInstallVersion(t *testing.T) {
	// `go install pkg@v1.2.3` populates Main.Version, which is surfaced
	// verbatim after the leading "v" is trimmed.
	stubBuildInfo(t, &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
	}, true)
	if got := Info(); got != "1.2.3" {
		t.Errorf("Info() = %q, want %q", got, "1.2.3")
	}
}

func TestFromBuildInfo_UsesVCSSettings(t *testing.T) {
	// When Main.Version is "(devel)" (the go-test / bare go-build default),
	// fall through to VCS settings. A long SHA is truncated to 7 chars.
	stubBuildInfo(t, &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc1234def56789"},
			{Key: "vcs.modified", Value: "true"},
		},
	}, true)
	want := "0.0.0-dev+gabc1234.dirty"
	if got := Info(); got != want {
		t.Errorf("Info() = %q, want %q", got, want)
	}
}

func TestFromBuildInfo_VCSShortSHA(t *testing.T) {
	// If vcs.revision is shorter than 7 chars (pathological but possible in
	// hand-crafted BuildInfo), keep the whole string rather than slicing.
	stubBuildInfo(t, &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc12"},
		},
	}, true)
	want := "0.0.0-dev+gabc12"
	if got := Info(); got != want {
		t.Errorf("Info() = %q, want %q", got, want)
	}
}

func TestFromBuildInfo_NoVCSNoVersion(t *testing.T) {
	// Clean BuildInfo with no vcs.revision setting: fall all the way to
	// "0.0.0-dev" with no metadata.
	stubBuildInfo(t, &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
	}, true)
	if got := Info(); got != "0.0.0-dev" {
		t.Errorf("Info() = %q, want %q", got, "0.0.0-dev")
	}
}

// ///////////////////////////////////////////////
// DockerTag
// ///////////////////////////////////////////////

func TestDockerTag_ReplacesPlus(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no build meta", "1.2.3", "1.2.3"},
		{"single plus", "1.2.3+dirty", "1.2.3-dirty"},
		{"past-tag with build meta", "1.2.3-dev.5+gabc1234", "1.2.3-dev.5-gabc1234"},
		{"past-tag with dirty meta", "1.2.3-dev.5+gabc1234.dirty", "1.2.3-dev.5-gabc1234.dirty"},
		{"dev build", "0.0.0-dev+gabc1234", "0.0.0-dev-gabc1234"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The value under test is injected directly, so the case
			// depends on neither ldflags nor git state.
			semver = tt.input
			t.Cleanup(func() { semver = "" })

			got := DockerTag()
			if got != tt.want {
				t.Errorf("DockerTag() with semver=%q = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// FromGit
// ///////////////////////////////////////////////

// TestFromGit_InRepo bootstraps a scratch git repo with a known tag so the
// happy path (git describe succeeds, parseDescribe normalizes the output)
// exercises deterministically regardless of the host repo's VCS state.
func TestFromGit_InRepo(t *testing.T) {
	dir, git := scratchRepo(t)
	git("init")
	git("commit", "--allow-empty", "-m", "seed")
	git("tag", "v1.2.3")

	t.Chdir(dir)
	if got := FromGit(); got != "1.2.3" {
		t.Errorf("FromGit() in seeded repo = %q, want %q", got, "1.2.3")
	}
}

// TestFromGit_OutsideRepo changes into a scratch directory that is not a
// git checkout, which makes runGit fail. FromGit must fall back to
// "0.0.0-dev".
func TestFromGit_OutsideRepo(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := FromGit(); got != "0.0.0-dev" {
		t.Errorf("FromGit() outside a git repo = %q, want %q", got, "0.0.0-dev")
	}
}

func TestFromGit_RefusesATagThatWouldRunAsAShellWord(t *testing.T) {
	// The result is interpolated into the Makefile's -ldflags, and a
	// double-quoted shell word evaluates a backtick or a $( ) inside it. git
	// check-ref-format accepts a tag name carrying either, so `make build` in
	// a clone with such a tag would run its author's commands.
	//
	// Driven through FromGit against a real tag rather than through the
	// parser, because the defect is a hostile tag reaching the recipe, not a
	// helper mishandling a string.
	tests := []struct {
		name string
		tag  string
	}{
		{name: "a backtick substitution", tag: "v1.2.3`id`"},
		{name: "a dollar substitution", tag: "v1.2.3$(id)"},
		{name: "a command separator", tag: "v1.2.3;id"},
		{name: "a single quote", tag: "v1.2.3'"},
		{name: "a double quote", tag: `v1.2.3"`},
		{name: "a pipe", tag: "v1.2.3|id"},
		{name: "an ampersand", tag: "v1.2.3&id"},
		{name: "a newline", tag: "v1.2.3\nid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, git := scratchRepo(t)
			git("init")
			git("commit", "--allow-empty", "-m", "seed")
			if !git("tag", "--", tt.tag) {
				t.Skipf("git refused the tag %q, so it cannot reach a build", tt.tag)
			}

			t.Chdir(dir)
			got := FromGit()

			if got != "0.0.0-dev" {
				t.Errorf("FromGit() with tag %q = %q, want the fallback", tt.tag, got)
			}
			if strings.ContainsAny(got, "`$;'\"|&\n\r <>(){}") {
				t.Errorf("FromGit() = %q, which the recipe would evaluate rather than pass along", got)
			}
		})
	}
}

func TestFromGit_KeepsAnOrdinaryTag(t *testing.T) {
	// Refusing everything would be a safe answer and a useless one: a real
	// SemVer tag has to survive, prerelease and build metadata included.
	for _, tag := range []string{"v1.2.3", "v2.0.0-rc.1", "v1.0.0-alpha.1+build.7"} {
		t.Run(tag, func(t *testing.T) {
			dir, git := scratchRepo(t)
			git("init")
			git("commit", "--allow-empty", "-m", "seed")
			git("tag", tag)

			t.Chdir(dir)
			if got, want := FromGit(), strings.TrimPrefix(tag, "v"); got != want {
				t.Errorf("FromGit() with tag %q = %q, want %q", tag, got, want)
			}
		})
	}
}

// scratchRepo returns a temporary directory and a git runner for it.
//
// Fully isolated from the operator's git config (no global or system config
// read, signing disabled) so a test cannot trip a GPG agent or inherit an
// environment-specific hook. The runner reports whether git accepted the
// command rather than failing the test, since a tag git itself refuses is
// a tag that can never reach a build.
func scratchRepo(t *testing.T) (string, func(args ...string) bool) {
	t.Helper()

	dir := t.TempDir()
	emptyConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatalf("write empty gitconfig: %v", err)
	}

	return dir, func(args ...string) bool {
		t.Helper()

		flags := []string{"-C", dir, "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}
		cmd := exec.Command("git", append(flags, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_CONFIG_GLOBAL="+emptyConfig,
			"GIT_CONFIG_SYSTEM="+emptyConfig,
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil && args[0] != "tag" {
			t.Skipf("git %v: %v\n%s", args, err, out)
		}
		return err == nil
	}
}

// ///////////////////////////////////////////////
// parseDescribe
// ///////////////////////////////////////////////

func TestParseDescribe_Shapes(t *testing.T) {
	tests := []struct {
		name string
		desc string
		want string
	}{
		// No tags: git describe --always emits the short SHA directly.
		{"no tags clean", "abc1234", "0.0.0-dev+gabc1234"},
		{"no tags dirty", "abc1234-dirty", "0.0.0-dev+gabc1234.dirty"},
		{"no tags long SHA", "abcdef1234567890", "0.0.0-dev+gabcdef1234567890"},

		// Exact tag (HEAD == tag).
		{"exact tag clean", "v1.2.3", "1.2.3"},
		{"exact tag dirty", "v1.2.3-dirty", "1.2.3+dirty"},
		{"exact prerelease tag", "v2.0.0-rc.1", "2.0.0-rc.1"},
		{"exact prerelease dirty", "v2.0.0-rc.1-dirty", "2.0.0-rc.1+dirty"},
		{"double-digit major", "v12.0.0", "12.0.0"},

		// Past tag: base + commit count + short SHA.
		{"5 past tag clean", "v1.2.3-5-gabc1234", "1.2.3-dev.5+gabc1234"},
		{"5 past tag dirty", "v1.2.3-5-gabc1234-dirty", "1.2.3-dev.5+gabc1234.dirty"},
		{"10 past tag clean", "v1.2.3-10-gdef5678", "1.2.3-dev.10+gdef5678"},
		{"past prerelease clean", "v2.0.0-rc.1-3-gabc1234", "2.0.0-rc.1.dev.3+gabc1234"},
		{"past prerelease dirty", "v2.0.0-rc.1-3-gabc1234-dirty", "2.0.0-rc.1.dev.3+gabc1234.dirty"},

		// Long SHAs.
		{"past tag long SHA", "v1.2.3-5-gabcdef1234567890", "1.2.3-dev.5+gabcdef1234567890"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDescribe(tt.desc)
			if got != tt.want {
				t.Errorf("parseDescribe(%q) = %q, want %q", tt.desc, got, tt.want)
			}
		})
	}
}

// TestParseDescribe_PastTagOrdering covers the ordering past ten commits
// ahead, which is where raw git-describe output breaks: "v1.2.3-5-gXXX" and
// "v1.2.3-10-gYYY" compare lexically in the prerelease identifier, so "10"
// sorts before "5".
//
// The reformat puts the commit count in a numeric identifier of its own,
// "dev.5" against "dev.10", which SemVer compares numerically.
func TestParseDescribe_PastTagOrdering(t *testing.T) {
	five := parseDescribe("v1.2.3-5-gabc1234")
	ten := parseDescribe("v1.2.3-10-gdef5678")

	if five != "1.2.3-dev.5+gabc1234" {
		t.Fatalf("5-past = %q", five)
	}
	if ten != "1.2.3-dev.10+gdef5678" {
		t.Fatalf("10-past = %q", ten)
	}
}

// ///////////////////////////////////////////////
// splitPastTag
// ///////////////////////////////////////////////

func TestSplitPastTag_Cases(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantBase  string
		wantAhead int
		wantSHA   string
		wantOK    bool
	}{
		{"simple", "1.2.3-5-gabc1234", "1.2.3", 5, "abc1234", true},
		{"prerelease base", "2.0.0-rc.1-3-gabc1234", "2.0.0-rc.1", 3, "abc1234", true},
		{"double-digit count", "1.2.3-15-gabc1234", "1.2.3", 15, "abc1234", true},
		{"exact tag (no -g suffix)", "1.2.3", "", 0, "", false},
		{"zero count is rejected", "1.2.3-0-gabc1234", "", 0, "", false},
		{"short SHA too short", "1.2.3-5-gabc12", "", 0, "", false},
		{"non-hex SHA rejected", "1.2.3-5-gxyz1234", "", 0, "", false},
		{"no dash before -g", "base-gabc1234", "", 0, "", false},
		{"non-numeric count", "1.2.3-abc-gdef4567", "", 0, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, ahead, sha, ok := splitPastTag(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("splitPastTag(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if base != tt.wantBase || ahead != tt.wantAhead || sha != tt.wantSHA {
				t.Errorf("splitPastTag(%q) = (%q, %d, %q), want (%q, %d, %q)",
					tt.input, base, ahead, sha, tt.wantBase, tt.wantAhead, tt.wantSHA)
			}
		})
	}
}

// ///////////////////////////////////////////////
// runGit
// ///////////////////////////////////////////////

func TestRunGit_InvalidSubcommand(t *testing.T) {
	// A garbage subcommand makes git exit non-zero without needing any
	// particular working directory shape.
	if _, err := runGit("definitely-not-a-real-subcommand"); err == nil {
		t.Fatal("runGit with invalid subcommand should return error")
	}
}

func TestRunGit_Version(t *testing.T) {
	// Happy-path smoke test: --version always succeeds when git is on PATH.
	out, err := runGit("--version")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	if out == "" {
		t.Error("runGit(--version) returned empty output")
	}
}

// ///////////////////////////////////////////////
// isShortSHA
// ///////////////////////////////////////////////

func TestIsShortSHA_LengthBounds(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"6 chars too short", "abc123", false},
		{"7 chars OK", "abc1234", true},
		{"40 chars OK", "abcdef1234567890abcdef1234567890abcdef12", true},
		{"41 chars too long", "abcdef1234567890abcdef1234567890abcdef123", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isShortSHA(tt.input); got != tt.want {
				t.Errorf("isShortSHA(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// isHex
// ///////////////////////////////////////////////

func TestIsHex_Cases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid hex", "abc1234", true},
		{"upper not accepted", "ABC1234", false},
		{"empty rejected", "", false},
		{"has letter g", "abcg123", false},
		{"all digits", "1234567", true},
		{"all letters", "abcdeff", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHex(tt.input); got != tt.want {
				t.Errorf("isHex(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
