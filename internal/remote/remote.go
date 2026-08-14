// Package remote centralizes GitHub owner/repo metadata for the project.
//
// Owner and repo are resolved lazily on first access. Values set at build
// time via ldflags take precedence; otherwise the package derives them
// from the local git remote origin. Use this package to build URLs that
// must point back at the canonical project location (issue links, raw
// content, update checks).
package remote

import (
	"context"
	"log/slog"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"zach.tools/go/stream-dvr/internal/procgroup"
)

// remoteLookupTimeout bounds the git call that resolves the remote.
//
// It is there to stop a hung git holding the first caller for good, not to
// measure how fast git answers. `git remote get-url` reads one config value
// and normally takes milliseconds, so any bound tight enough to fail on a
// loaded machine is measuring the machine rather than the lookup. A release
// build never runs it at all: ldflags supply both values, and this is the
// fallback for a build without them.
const remoteLookupTimeout = 10 * time.Second

// maxRemoteOutput bounds what the git fallback reads. A remote URL is one
// line, so anything past this is not one.
const maxRemoteOutput = 4 << 10

// Set at build time via:
//
//	-X <module>/internal/remote.ldOwner=...
//	-X <module>/internal/remote.ldRepo=...
var (
	ldOwner string
	ldRepo  string
)

var (
	initOnce sync.Once
	owner    string
	repo     string
)

// githubRemoteRe extracts owner and repo from GitHub remote URLs.
// Matches both HTTPS (github.com/) and SSH (github.com:) formats.
// Uses a non-greedy second group with an optional .git suffix so that
// dotted repo names (e.g. "vue.js") are captured correctly.
var githubRemoteRe = regexp.MustCompile(`github\.com[:/]([^/\s]+)/([^/\s]+?)(?:\.git)?\s*$`)

// ensureInit lazily resolves owner and repo on first call. Build-time ldflags
// are preferred; otherwise the values are derived from the local git remote origin.
func ensureInit() {
	initOnce.Do(func() {
		if ldOwner != "" && ldRepo != "" {
			owner = ldOwner
			repo = ldRepo
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), remoteLookupTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
		// Bounded, because a remote URL is one short line and .Output()
		// reads whatever the process writes. Which repository answers also
		// depends on the working directory this binary was started in, so
		// the injected values are the answer and this is only a fallback
		// for a developer build run inside a checkout.
		stdout := procgroup.NewOutput(maxRemoteOutput)
		cmd.Stdout = stdout
		if err := cmd.Run(); err != nil {
			slog.Debug("remote: ldflags not set and git remote unavailable", "error", err)
			return
		}
		m := githubRemoteRe.FindStringSubmatch(stdout.String())
		if len(m) == 3 {
			owner = m[1]
			repo = m[2]
		}
	})
}

// Owner returns the GitHub repository owner.
func Owner() string {
	ensureInit()
	return owner
}

// Repo returns the GitHub repository name.
func Repo() string {
	ensureInit()
	return repo
}

// RawURL returns the raw GitHub URL for a file on the main branch.
// Returns empty string if owner/repo could not be determined.
func RawURL(path string) string {
	ensureInit()
	if owner == "" || repo == "" {
		return ""
	}
	return "https://raw.githubusercontent.com/" + owner + "/" + repo + "/main/" + path
}
