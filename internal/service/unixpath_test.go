//go:build darwin || linux

package service

import (
	"slices"
	"strings"
	"testing"
)

func TestSearchPath(t *testing.T) {
	// Both home shapes, because one list serves both platforms and a rule
	// that only runs on one host is a rule nothing checks until someone is
	// on the other wondering why the daemon cannot find streamlink.
	homes := []struct {
		name string
		home string
	}{
		{name: "linux", home: "/home/operator"},
		{name: "macos", home: "/Users/operator"},
	}

	for _, tt := range homes {
		t.Run(tt.name, func(t *testing.T) {
			entries := strings.Split(searchPath(tt.home), ":")

			t.Run("covers every directory deps scans", func(t *testing.T) {
				// doctor resolves a tool through that scan. A directory it
				// searches and the unit does not is a tool doctor reports
				// present and the daemon then cannot run.
				for _, want := range []string{
					"/opt/homebrew/bin",
					"/usr/local/bin",
					"/usr/bin",
					"/opt/local/bin",
					"/snap/bin",
					"/var/lib/flatpak/exports/bin",
					"/home/linuxbrew/.linuxbrew/bin",
					tt.home + "/.local/bin",
					tt.home + "/.nix-profile/bin",
				} {
					if !slices.Contains(entries, want) {
						t.Errorf("PATH is missing %q: %v", want, entries)
					}
				}
			})

			t.Run("prefers a native build over a translated one", func(t *testing.T) {
				// Homebrew on Apple silicon leads /usr/local/bin, which is
				// where a Rosetta x86_64 build lands. Taking the translated
				// one runs every transcode through emulation on a machine
				// that has a native build.
				native := slices.Index(entries, "/opt/homebrew/bin")
				translated := slices.Index(entries, "/usr/local/bin")
				if native < 0 || translated < 0 {
					t.Fatalf("PATH lost one of the two homebrew directories: %v", entries)
				}
				if native > translated {
					t.Errorf("/opt/homebrew/bin is at %d and /usr/local/bin at %d, want the native one first",
						native, translated)
				}
			})

			t.Run("lets a machine-wide install win", func(t *testing.T) {
				// deps searches the per-user directories last for this
				// reason, and a PATH that ordered them first would resolve a
				// different build than doctor reported.
				shared := slices.Index(entries, "/usr/bin")
				personal := slices.Index(entries, tt.home+"/.local/bin")
				if shared < 0 || personal < 0 {
					t.Fatalf("PATH lost one of the two directories: %v", entries)
				}
				if personal < shared {
					t.Errorf("%s is at %d and /usr/bin at %d, want the machine-wide one first",
						tt.home+"/.local/bin", personal, shared)
				}
			})

			t.Run("is rooted at the caller's home", func(t *testing.T) {
				// A unit written for one account must not send another
				// account's daemon looking in the first one's bin directory.
				if strings.Contains(searchPath("/home/other"), tt.home+"/.local") {
					t.Error("searchPath() ignored its argument")
				}
			})

			t.Run("has no empty entry", func(t *testing.T) {
				// An empty PATH element means the current directory, which
				// for a daemon is whatever the service manager handed it.
				if slices.Contains(entries, "") {
					t.Errorf("PATH has an empty element: %v", entries)
				}
			})
		})
	}
}
