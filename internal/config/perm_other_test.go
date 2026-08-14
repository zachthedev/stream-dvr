//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"zach.tools/go/stream-dvr/internal/paths"
)

// assertOwnerOnly reports whether nothing but the owner may reach the file.
func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if mode := info.Mode().Perm(); mode != configMode {
		t.Errorf("mode = %o, want %o so nothing but the owner may read it", mode, configMode)
	}
}

func TestSave_CreatesTheDataDirectoryToItsOwner(t *testing.T) {
	// The directory is what protects the credential file beside the config,
	// and MkdirAll does nothing to one that is already there. So the mode
	// every writer asks for has to be the narrow one, or whichever command
	// an operator ran first decides who can traverse it.
	path := filepath.Join(t.TempDir(), "stream-dvr", "config.toml")
	if err := Save(path, validConfig()); err != nil {
		t.Fatalf("Save() err = %v, want nil", err)
	}

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat %s: %v", filepath.Dir(path), err)
	}
	if mode := info.Mode().Perm(); mode != paths.DataDirMode {
		t.Errorf("mode = %o, want %o so nothing but the owner may traverse it", mode, paths.DataDirMode)
	}
}

func TestSave_DoesNotKeepAWiderModeOnAConfigAlreadyThere(t *testing.T) {
	// A config can hold a webhook URL whose path is the only thing
	// authorizing a post, so a wider mode on a file already at the path must
	// not survive the save. This holds the end state only:
	// fsretry.RestrictToOwner runs last and repairs the mode whatever the
	// writer did before it. The stronger guarantee, a mode landing before
	// the first byte, is held in internal/secret and in the twitch
	// provider's auth config tests.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("half a config"), 0o666); err != nil {
		t.Fatalf("writing the config already there: %v", err)
	}

	if err := Save(path, validConfig()); err != nil {
		t.Fatalf("Save() err = %v, want nil", err)
	}
	assertOwnerOnly(t, path)
}
