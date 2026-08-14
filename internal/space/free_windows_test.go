//go:build windows

package space

import (
	"path/filepath"
	"testing"
)

func TestFree(t *testing.T) {
	got, err := Free(t.TempDir())
	if err != nil {
		t.Fatalf("Free() err = %v, want nil", err)
	}
	if got <= 0 {
		t.Errorf("Free() = %d, want a positive figure for a writable directory", got)
	}
}

func TestFree_MissingPath(t *testing.T) {
	// A library root that vanished must surface as an error rather than as
	// zero free space, which would read as a full disk and stop recording.
	if _, err := Free(filepath.Join(t.TempDir(), "absent", "deeper")); err == nil {
		t.Error("Free() err = nil, want an error for a missing path")
	}
}
