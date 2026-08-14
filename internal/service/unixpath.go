//go:build darwin || linux

package service

import (
	"path/filepath"
	"strings"
)

// searchPath returns the PATH the recorder runs with.
//
// A service manager hands its child a minimal PATH with none of the places
// streamlink, ffmpeg and yt-dlp are actually installed. doctor resolves them
// from an interactive shell and the daemon then cannot, so every recording
// fails naming a tool the operator can see is present.
//
// The directories and their order match the fallback scan in internal/deps,
// because the two answer the same question and disagreeing means doctor
// finds one build and the daemon runs another. Homebrew on Apple silicon
// leads /usr/local/bin, where a Rosetta x86_64 build lands, and the per-user
// directories come last so a personal install does not shadow a
// machine-wide one. The standard system directories follow.
//
// Both platforms take one list, the way internal/deps does. macOS-only and
// Linux-only entries cost a lookup in a directory that is not there.
func searchPath(home string) string {
	dirs := []string{
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/opt/local/bin",
		"/snap/bin",
		"/var/lib/flatpak/exports/bin",
		"/home/linuxbrew/.linuxbrew/bin",
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".nix-profile", "bin"),
		"/opt/homebrew/sbin",
		"/usr/local/sbin",
		"/usr/sbin",
		"/bin",
		"/sbin",
	}
	return strings.Join(dirs, ":")
}
