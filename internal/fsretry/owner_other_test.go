//go:build !windows

package fsretry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// ///////////////////////////////////////////////
// RestrictToOwner
// ///////////////////////////////////////////////

func TestRestrictToOwner_NarrowsAWiderMode(t *testing.T) {
	// A file already at the path keeps the access it carries, and a config
	// or a credential restored from an archive is the ordinary way one
	// arrives wider than the caller asked for.
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("{}"), 0o666); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	if err := RestrictToOwner(path, 0o600); err != nil {
		t.Fatalf("RestrictToOwner() err = %v, want nil", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600 so nothing but the owner may read it", mode)
	}
}

func TestRestrictToOwner_ReportsAMissingFile(t *testing.T) {
	if err := RestrictToOwner(filepath.Join(t.TempDir(), "absent.json"), 0o600); err == nil {
		t.Error("RestrictToOwner() err = nil, want an error for a file that is not there")
	}
}

// ///////////////////////////////////////////////
// WriteFilePrivate
// ///////////////////////////////////////////////

func TestWriteFilePrivate_LandsOwnerOnlyEvenOverAWiderFile(t *testing.T) {
	// The rename replaces whatever stood at the path, so a file already
	// there must not lend its access to the one that replaces it.
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("old"), 0o666); err != nil {
		t.Fatalf("writing the file already there: %v", err)
	}

	if err := WriteFilePrivate(context.Background(), path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFilePrivate() err = %v, want nil", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode = %o, want 600", mode)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(body) != "new" {
		t.Errorf("contents = %q, want %q", body, "new")
	}
}
