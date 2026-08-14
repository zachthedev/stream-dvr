package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"zach.tools/go/stream-dvr/internal/fsretry"
	"zach.tools/go/stream-dvr/internal/paths"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// File is a Store kept in one file the daemon owns.
//
// # Why not the operating system's credential store
//
// streamlink can only read a credential from a file. No environment
// variable, no standard input. So a readable copy exists for as long as the
// recorder runs, whatever else holds the credential. A credential store
// beside it protects nothing the file does not already expose.
//
// Rotation makes a credential store worse, not better. A refreshed token
// must be written back, so the daemon needs write access. A store it cannot
// reliably reach gives two sources of truth that drift apart. One
// authoritative file is the smaller failure.
//
// The reach is the deciding part, and it is not a Windows quirk. A Linux
// daemon with no session bus cannot call Secret Service, and a macOS
// launchd agent runs only after somebody signs in. Two of the three cannot
// depend on a store, and the third only exists when a human is already
// present.
//
// # What protects it
//
// The file mode, and the data directory's own permissions. That is the same
// boundary config.toml sits behind, which already carries a webhook URL
// that is itself a credential. It is not encryption at rest and must not be
// described as though it were.
type File struct {
	path string

	// mu serializes read-modify-write inside one handle. It is not the
	// whole guard: the daemon, an interactive auth command and doctor each
	// build their own File, and two of them can be different processes, so
	// a lock held in memory is a lock neither of them shares.
	mu sync.Mutex
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// FileName is what the credential file is called inside the data directory.
const FileName = "credentials.json"

// fileMode keeps the file to its owner.
//
// On Windows the permission bits carry little and the file inherits the
// data directory's ACL, which comes from the profile root and is user only.
const fileMode = 0o600

// The advisory lock beside the credential file.
//
// Waiting is right where the alternative is losing a one-time token: a
// rotation is a handful of milliseconds, so a caller that waits a moment
// costs nothing next to a session that has to be authorized again. The
// stale bound is what stops a lock a killed process left behind from
// holding the store for the life of the machine.
const (
	lockSuffix = ".lock"
	lockWait   = 5 * time.Second
	lockPoll   = 20 * time.Millisecond
	lockStale  = 30 * time.Second
)

// ///////////////////////////////////////////////
// Constructor
// ///////////////////////////////////////////////

// NewFile returns a Store kept in dir.
func NewFile(dir string) *File {
	return &File{path: filepath.Join(dir, FileName)}
}

// Path reports where the credentials are kept, whether or not any are.
func (f *File) Path() string { return f.path }

// ///////////////////////////////////////////////
// File
// ///////////////////////////////////////////////

// Get implements Store.
func (f *File) Get(account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	stored, err := f.read()
	if err != nil {
		return "", err
	}
	if value, ok := stored[account]; ok {
		return value, nil
	}
	return "", ErrNotFound
}

// Set implements Store.
func (f *File) Set(account, secret string) error {
	if err := checkSize(secret); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	release, err := f.lock()
	if err != nil {
		return err
	}
	defer release()

	stored, err := f.read()
	if err != nil {
		return err
	}
	stored[account] = secret
	return f.write(stored)
}

// Delete implements Store.
//
// An account nothing holds is already in the state the caller asked for,
// so a logout run twice is not a failure.
func (f *File) Delete(account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	release, err := f.lock()
	if err != nil {
		return err
	}
	defer release()

	stored, err := f.read()
	if err != nil {
		return err
	}
	if _, held := stored[account]; !held {
		return nil
	}
	delete(stored, account)
	return f.write(stored)
}

// lock takes an advisory lock over the credential file, returning what
// releases it.
//
// A refresh token is one time use. Two handles reading the same map and
// each writing the whole of it back leaves the loser's rotation on disk:
// the token it names has already been spent at the provider, so the
// session is dead and nothing says so until the next renewal hours later.
// The lock is a file beside the store rather than a mutex, because the
// callers are separate processes as often as they are separate handles.
func (f *File) lock() (func(), error) {
	// The store's own directory may not exist yet, and the lock has to sit
	// beside the file it guards rather than somewhere shared.
	if err := fsretry.MkdirPrivate(filepath.Dir(f.path), paths.DataDirMode); err != nil {
		return nil, fmt.Errorf("creating the credential directory: %w", err)
	}

	path := f.path + lockSuffix
	deadline := time.Now().Add(lockWait)

	for {
		handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
		if err == nil {
			_ = handle.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("locking the credential store: %w", err)
		}

		// A lock a killed process left behind must not hold the store for
		// the life of the machine.
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > lockStale {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the credential store at %s is locked by another process", f.path)
		}
		time.Sleep(lockPoll)
	}
}

// ///////////////////////////////////////////////
// Storage
// ///////////////////////////////////////////////

// read loads the file, answering an empty set when there is none.
//
// A missing file is the ordinary state before anyone authenticates. A file
// that will not parse is NOT treated as empty. Overwriting it would discard
// a credential the operator would have to obtain again, so it is reported.
func (f *File) read() (map[string]string, error) {
	body, err := os.ReadFile(f.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return map[string]string{}, nil
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", f.path, err)
	}

	stored := map[string]string{}
	if err := json.Unmarshal(body, &stored); err != nil {
		return nil, fmt.Errorf("reading %s: %w", f.path, err)
	}
	return stored, nil
}

// write replaces the file atomically.
//
// A rotated credential is the one thing here that cannot be recovered. A
// refresh token is spent when it is used, so a truncated write during
// rotation costs the session. The rename underneath either happens or does
// not. The mode is set before the first byte lands, so the secret is never
// briefly readable by anyone else.
func (f *File) write(stored map[string]string) error {
	if err := fsretry.MkdirPrivate(filepath.Dir(f.path), paths.DataDirMode); err != nil {
		return fmt.Errorf("creating the credential directory: %w", err)
	}

	body, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the credentials: %w", err)
	}

	if err := fsretry.WriteFilePrivate(context.Background(), f.path, body, fileMode); err != nil {
		return fmt.Errorf("replacing %s: %w", f.path, err)
	}
	return nil
}
