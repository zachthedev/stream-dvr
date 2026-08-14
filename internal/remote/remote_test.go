package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// ///////////////////////////////////////////////
// Test Helpers
// ///////////////////////////////////////////////

// setOwnerRepo overrides the package-level owner and repo for testing.
// It first triggers ensureInit so the sync.Once is consumed (preventing
// git commands from running during test), then sets the desired values.
// Original values are restored via t.Cleanup.
func setOwnerRepo(t *testing.T, o, r string) {
	t.Helper()

	// Ensure initOnce is consumed so ensureInit is a no-op.
	ensureInit()

	origOwner, origRepo := owner, repo
	owner = o
	repo = r

	t.Cleanup(func() {
		owner = origOwner
		repo = origRepo
	})
}

// resetInit restores package state so ensureInit's sync.Once fires afresh.
// Tests using this helper must not run in parallel with each other.
func resetInit(t *testing.T) {
	t.Helper()
	initOnce = sync.Once{}
	owner = ""
	repo = ""
	ldOwner = ""
	ldRepo = ""
	t.Cleanup(func() {
		initOnce = sync.Once{}
		owner = ""
		repo = ""
		ldOwner = ""
		ldRepo = ""
	})
}

// ///////////////////////////////////////////////
// githubRemoteRe
// ///////////////////////////////////////////////

func TestGithubRemoteRe_Matches(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantOwner string
		wantRepo  string
	}{
		{
			name:      "HTTPS URL",
			input:     "https://github.com/user/repo",
			wantOwner: "user",
			wantRepo:  "repo",
		},
		{
			name:      "HTTPS URL with .git",
			input:     "https://github.com/user/repo.git",
			wantOwner: "user",
			wantRepo:  "repo",
		},
		{
			name:      "SSH URL",
			input:     "git@github.com:user/repo.git",
			wantOwner: "user",
			wantRepo:  "repo",
		},
		{
			name:      "SSH URL without .git",
			input:     "git@github.com:user/repo",
			wantOwner: "user",
			wantRepo:  "repo",
		},
		{
			name:      "HTTPS with org name",
			input:     "https://github.com/my-org/my-project",
			wantOwner: "my-org",
			wantRepo:  "my-project",
		},
		{
			name:      "SSH with org name",
			input:     "git@github.com:my-org/my-project.git",
			wantOwner: "my-org",
			wantRepo:  "my-project",
		},
		{
			name:      "HTTPS dotted repo name",
			input:     "https://github.com/vuejs/vue.js",
			wantOwner: "vuejs",
			wantRepo:  "vue.js",
		},
		{
			name:      "SSH dotted repo name with .git",
			input:     "git@github.com:vuejs/vue.js.git",
			wantOwner: "vuejs",
			wantRepo:  "vue.js",
		},
		{
			name:      "HTTPS dotted org name",
			input:     "https://github.com/my.company/my-project",
			wantOwner: "my.company",
			wantRepo:  "my-project",
		},
		{
			name:      "HTTPS dotted org and repo",
			input:     "https://github.com/my.company/my.project",
			wantOwner: "my.company",
			wantRepo:  "my.project",
		},
		{
			name:      "SSH dotted org and repo with .git",
			input:     "git@github.com:my.company/my.project.git",
			wantOwner: "my.company",
			wantRepo:  "my.project",
		},
		{
			name:      "HTTPS trailing newline",
			input:     "https://github.com/user/repo\n",
			wantOwner: "user",
			wantRepo:  "repo",
		},
		{
			name:      "HTTPS dotted repo trailing newline",
			input:     "https://github.com/user/vue.js\n",
			wantOwner: "user",
			wantRepo:  "vue.js",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := githubRemoteRe.FindStringSubmatch(tt.input)
			if len(m) != 3 {
				t.Fatalf("expected 3 groups, got %d: %v", len(m), m)
			}
			if m[1] != tt.wantOwner {
				t.Errorf("owner = %q, want %q", m[1], tt.wantOwner)
			}
			if m[2] != tt.wantRepo {
				t.Errorf("repo = %q, want %q", m[2], tt.wantRepo)
			}
		})
	}
}

func TestGithubRemoteRe_NoMatch(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"GitLab HTTPS", "https://gitlab.com/user/repo"},
		{"GitLab SSH", "git@gitlab.com:user/repo.git"},
		{"Bitbucket HTTPS", "https://bitbucket.org/user/repo"},
		{"random string", "just some text"},
		{"empty string", ""},
		{"partial URL", "github.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := githubRemoteRe.FindStringSubmatch(tt.input)
			if len(m) == 3 {
				t.Errorf("expected no match for %q, but got owner=%q repo=%q", tt.input, m[1], m[2])
			}
		})
	}
}

// ///////////////////////////////////////////////
// ensureInit
// ///////////////////////////////////////////////

// TestEnsureInit_LdFlagsTakePrecedence covers the ldflags-set branch: when
// both ldOwner and ldRepo carry values (as they do in a release build),
// ensureInit adopts them and skips the git subprocess entirely.
func TestEnsureInit_LdFlagsTakePrecedence(t *testing.T) {
	resetInit(t)
	ldOwner = "ld-owner"
	ldRepo = "ld-repo"

	ensureInit()

	if owner != "ld-owner" {
		t.Errorf("owner = %q, want %q", owner, "ld-owner")
	}
	if repo != "ld-repo" {
		t.Errorf("repo = %q, want %q", repo, "ld-repo")
	}
}

// TestEnsureInit_ParsesGithubRemote covers the git-success branch. With no
// ldflags set and `git remote get-url origin` returning a parseable GitHub
// URL, ensureInit fills owner and repo from it.
//
// The scratch repo carries a canned remote, so the assertion does not
// depend on the host checkout's own remote.
func TestEnsureInit_ParsesGithubRemote(t *testing.T) {
	dir := t.TempDir()
	emptyConfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatalf("write empty gitconfig: %v", err)
	}
	run := func(args ...string) {
		t.Helper()
		// Args are test-constant string literals; flagging as tainted is a
		// false positive here.
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		// Isolate from the user's git config so no hooks/signing/identity
		// settings leak into the test invocation.
		cmd.Env = append(cmd.Environ(),
			"GIT_CONFIG_GLOBAL="+emptyConfig,
			"GIT_CONFIG_SYSTEM="+emptyConfig,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("remote", "add", "origin", "https://github.com/testowner/testrepo.git")
	// Asked here as well, so the assertions below are about the parse
	// rather than about whether git works in this directory at all. It also
	// pays the cost of a first access to a fresh repository, which on
	// Windows can mean a virus scanner reading every file in it, before
	// ensureInit's own bounded call runs.
	run("remote", "get-url", "origin")

	t.Chdir(dir)
	resetInit(t)

	ensureInit()

	if owner != "testowner" {
		t.Errorf("owner = %q, want %q", owner, "testowner")
	}
	if repo != "testrepo" {
		t.Errorf("repo = %q, want %q", repo, "testrepo")
	}
}

// TestEnsureInit_GitFails covers the error branch where no ldflags are set
// and `git remote get-url origin` fails (running outside a git checkout).
// Running in a plain temp dir guarantees the failure regardless of whether
// the host repo has an origin configured.
func TestEnsureInit_GitFails(t *testing.T) {
	t.Chdir(t.TempDir())
	resetInit(t)

	ensureInit()

	if owner != "" || repo != "" {
		t.Errorf("owner/repo = %q/%q, want both empty", owner, repo)
	}
}

func TestEnsureInit_NoGitRemote(t *testing.T) {
	// Exercise behavior through setOwnerRepo + Owner()/Repo(). The helper
	// consumes initOnce so ensureInit is a no-op afterwards; setting both
	// to empty simulates "no git remote".
	setOwnerRepo(t, "", "")

	if Owner() != "" {
		t.Errorf("Owner() = %q, want empty", Owner())
	}
	if Repo() != "" {
		t.Errorf("Repo() = %q, want empty", Repo())
	}
}

// ///////////////////////////////////////////////
// Owner
// ///////////////////////////////////////////////

func TestOwner(t *testing.T) {
	setOwnerRepo(t, "myowner", "myrepo")
	if got := Owner(); got != "myowner" {
		t.Errorf("Owner() = %q, want %q", got, "myowner")
	}
}

func TestOwner_Empty(t *testing.T) {
	setOwnerRepo(t, "", "")
	if got := Owner(); got != "" {
		t.Errorf("Owner() = %q, want empty", got)
	}
}

// ///////////////////////////////////////////////
// Repo
// ///////////////////////////////////////////////

func TestRepo(t *testing.T) {
	setOwnerRepo(t, "myowner", "myrepo")
	if got := Repo(); got != "myrepo" {
		t.Errorf("Repo() = %q, want %q", got, "myrepo")
	}
}

func TestRepo_Empty(t *testing.T) {
	setOwnerRepo(t, "", "")
	if got := Repo(); got != "" {
		t.Errorf("Repo() = %q, want empty", got)
	}
}

// ///////////////////////////////////////////////
// RawURL
// ///////////////////////////////////////////////

func TestRawURL_Format(t *testing.T) {
	setOwnerRepo(t, "testowner", "testrepo")

	got := RawURL("data/tiers.json")
	want := "https://raw.githubusercontent.com/testowner/testrepo/main/data/tiers.json"
	if got != want {
		t.Errorf("RawURL = %q, want %q", got, want)
	}
}

func TestRawURL_EmptyWhenNotConfigured(t *testing.T) {
	setOwnerRepo(t, "", "")

	result := RawURL("some/path.json")
	if result != "" {
		t.Errorf("RawURL with empty owner/repo = %q, want empty", result)
	}
}

func TestRawURL_OwnerOnly(t *testing.T) {
	setOwnerRepo(t, "testowner", "")

	got := RawURL("file.txt")
	if got != "" {
		t.Errorf("RawURL with repo empty = %q, want empty", got)
	}
}

func TestRawURL_RepoOnly(t *testing.T) {
	setOwnerRepo(t, "", "testrepo")

	got := RawURL("file.txt")
	if got != "" {
		t.Errorf("RawURL with owner empty = %q, want empty", got)
	}
}

func TestRawURL_EmptyPath(t *testing.T) {
	setOwnerRepo(t, "testowner", "testrepo")

	got := RawURL("")
	want := "https://raw.githubusercontent.com/testowner/testrepo/main/"
	if got != want {
		t.Errorf("RawURL(\"\") = %q, want %q", got, want)
	}
}

func TestRawURL_SpecialChars(t *testing.T) {
	setOwnerRepo(t, "testowner", "testrepo")

	// RawURL does simple concatenation, no URL encoding.
	got := RawURL("data/some file.json")
	want := "https://raw.githubusercontent.com/testowner/testrepo/main/data/some file.json"
	if got != want {
		t.Errorf("RawURL with spaces = %q, want %q", got, want)
	}
}
