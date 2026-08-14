package fsretry

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// lockedCause is the operating system's own error for a held file. It is
// comparable, so errors.Is can match it through any wrapping.
func lockedCause() error { return windows.ERROR_SHARING_VIOLATION }

// lockedErr returns an error isLocked recognizes, shaped the way the
// standard library delivers it, so the platform-neutral tests can drive the
// retry loop without holding a real file.
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

// hold opens a file the way a backup agent does: readable by others, but
// withholding FILE_SHARE_DELETE so no one can rename, delete, or write it.
// It returns a release function.
func hold(t *testing.T, path string) func() {
	t.Helper()

	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("encoding %s: %v", path, err)
	}
	handle, err := windows.CreateFile(name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		t.Fatalf("holding %s: %v", path, err)
	}

	var released bool
	release := func() {
		if released {
			return
		}
		released = true
		if err := windows.CloseHandle(handle); err != nil {
			t.Errorf("releasing %s: %v", path, err)
		}
	}
	t.Cleanup(release)
	return release
}

// ///////////////////////////////////////////////
// isLocked
// ///////////////////////////////////////////////

func TestIsLocked_Windows(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "sharing violation",
			err:  &fs.PathError{Err: windows.ERROR_SHARING_VIOLATION},
			want: true,
		},
		{
			name: "lock violation",
			err:  &fs.PathError{Err: windows.ERROR_LOCK_VIOLATION},
			want: true,
		},
		{
			// Waiting cannot help when the file is not there.
			name: "missing file",
			err:  &fs.PathError{Err: windows.ERROR_FILE_NOT_FOUND},
			want: false,
		},
		{
			// A denied ACL does not change on its own either.
			name: "access denied",
			err:  &fs.PathError{Err: windows.ERROR_ACCESS_DENIED},
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

func TestIsLocked_DoesNotRelyOnErrPermission(t *testing.T) {
	// A sharing violation is not a permission error to Go, which is the
	// whole reason this package exists: a caller checking fs.ErrPermission
	// sees an ordinary failure and gives up on a file that would have been
	// free moments later.
	path := seed(t, "held.ts", "payload")
	hold(t, path)

	err := os.Remove(path)
	if err == nil {
		t.Fatal("os.Remove() err = nil, want the held file to refuse deletion")
	}
	if errors.Is(err, fs.ErrPermission) {
		t.Error("os.Remove() err matched fs.ErrPermission; the package's premise no longer holds")
	}
	if !isLocked(err) {
		t.Errorf("isLocked(%v) = false, want the sharing violation recognized", err)
	}
}

// ///////////////////////////////////////////////
// Rename, Remove, WriteFileAtomic against a genuinely held file
// ///////////////////////////////////////////////

func TestRun_ReportsAHeldFileWithoutTouchingIt(t *testing.T) {
	tests := []struct {
		name   string
		op     string
		invoke func(context.Context, string) error
	}{
		{
			name: "rename",
			op:   "rename",
			invoke: func(ctx context.Context, path string) error {
				return Rename(ctx, path, path+".moved")
			},
		},
		{name: "remove", op: "remove", invoke: Remove},
		{
			// An atomic write publishes its result by renaming over the
			// target, so a held target blocks that rename.
			name: "write",
			op:   "rename",
			invoke: func(ctx context.Context, path string) error {
				return WriteFileAtomic(ctx, path, []byte("replacement"), 0o600)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := seed(t, "held.ts", "payload")
			hold(t, path)

			// The window outlasts any test, so the deadline stands in
			// for it.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			err := tt.invoke(ctx, path)
			if err == nil {
				t.Fatalf("%s() err = nil, want a held file to be reported", tt.name)
			}

			// Either outcome proves it waited rather than failing at once.
			// Which one depends on whether the window or the deadline lands
			// first, and the contract is that the file survives untouched.
			var locked *LockedError
			if !errors.As(err, &locked) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s() err = %v, want a LockedError or the deadline", tt.name, err)
			}
			if locked != nil {
				if locked.Op != tt.op {
					t.Errorf("LockedError.Op = %q, want %q", locked.Op, tt.op)
				}
				if locked.Attempts < 2 {
					t.Errorf("LockedError.Attempts = %d, want it to have retried", locked.Attempts)
				}
			}

			content, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("the held file went missing: %v", readErr)
			}
			if string(content) != "payload" {
				t.Errorf("held file content = %q, want it untouched", content)
			}
		})
	}
}

func TestRun_SucceedsOnceTheHolderReleases(t *testing.T) {
	// The point of the package. A lock that clears mid-wait must let the
	// operation through rather than having already failed.
	tests := []struct {
		name   string
		invoke func(context.Context, string) error
		verify func(*testing.T, string)
	}{
		{
			name: "rename",
			invoke: func(ctx context.Context, path string) error {
				return Rename(ctx, path, path+".moved")
			},
			verify: func(t *testing.T, path string) {
				if _, err := os.Stat(path + ".moved"); err != nil {
					t.Errorf("target missing after the lock cleared: %v", err)
				}
			},
		},
		{
			name:   "remove",
			invoke: Remove,
			verify: func(t *testing.T, path string) {
				if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("Stat() err = %v, want the file gone", err)
				}
			},
		},
		{
			name: "write",
			invoke: func(ctx context.Context, path string) error {
				return WriteFileAtomic(ctx, path, []byte("replacement"), 0o600)
			},
			verify: func(t *testing.T, path string) {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading back: %v", err)
				}
				if string(content) != "replacement" {
					t.Errorf("content = %q, want %q", content, "replacement")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := seed(t, "held.ts", "payload")
			release := hold(t, path)

			// Outlasts the first backoff pause, so the operation has to
			// retry at least once to see the file free.
			timer := time.AfterFunc(3*firstDelay, release)
			defer timer.Stop()

			if err := tt.invoke(context.Background(), path); err != nil {
				t.Fatalf("%s() err = %v, want it to succeed once the holder released", tt.name, err)
			}
			tt.verify(t, path)
		})
	}
}
