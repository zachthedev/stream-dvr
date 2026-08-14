//go:build !windows

package fsretry

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// lockedCause is the operating system's own error for a held file. It is
// comparable, so errors.Is can match it through any wrapping.
func lockedCause() error { return syscall.EBUSY }

// lockedErr returns an error isLocked recognizes, shaped the way the
// standard library delivers it, so the platform-neutral tests can drive the
// retry loop without a real filesystem holding one.
func lockedErr() error {
	return &fs.PathError{Op: "remove", Path: "held.ts", Err: lockedCause()}
}

// seed writes a file and returns its path.
func seed(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seeding %s: %v", path, err)
	}
	return path
}

// ///////////////////////////////////////////////
// isLocked
// ///////////////////////////////////////////////

func TestIsLocked_Other(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			// The share mode a network server enforces, which the local
			// kernel has no concept of.
			name: "busy",
			err:  &fs.PathError{Err: syscall.EBUSY},
			want: true,
		},
		{
			// An NFS handle the server invalidated. Retrying re-resolves it
			// from the path, so the recording is reachable again.
			name: "stale handle",
			err:  &fs.PathError{Err: syscall.ESTALE},
			want: true,
		},
		{
			// A file being executed. It ends when that process does, which
			// is the definition of worth waiting out.
			name: "text file busy",
			err:  &fs.PathError{Err: syscall.ETXTBSY},
			want: true,
		},
		{
			// Waiting cannot help when the file is not there.
			name: "missing file",
			err:  &fs.PathError{Err: syscall.ENOENT},
			want: false,
		},
		{
			// A denied permission does not change on its own either.
			name: "permission denied",
			err:  &fs.PathError{Err: syscall.EACCES},
			want: false,
		},
		{name: "unrelated error", err: errors.New("disk on fire"), want: false},
		{name: "no error", err: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLocked(tt.err); got != tt.want {
				t.Errorf("isLocked(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRemove_UnlinksAnOpenFile(t *testing.T) {
	// A Unix filesystem renames and unlinks a file that is open, so the
	// condition this package waits out cannot arise locally. Holding the
	// file open must therefore cost nothing.
	path := seed(t, "held.ts", "payload")

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer file.Close()

	if err := Remove(t.Context(), path); err != nil {
		t.Errorf("Remove() err = %v, want an open file to be removable", err)
	}
}
