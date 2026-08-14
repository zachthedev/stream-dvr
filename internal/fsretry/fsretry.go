// Package fsretry runs filesystem operations that another program may be
// holding open.
//
// On Windows a process that opens a file without FILE_SHARE_DELETE blocks
// every rename, delete, and write of that file until it closes the handle.
// Backup agents, search indexers, and antivirus scanners all read files
// that way, so a finished recording can be untouchable for as long as the
// other program takes with it. Reads are unaffected, which is why only the
// operations that move, replace, or remove a file go through here.
//
// Waiting is deliberately brief. Backing up a multi-gigabyte recording can
// hold it for hours, and no call blocks that long. A lock outliving the
// window becomes a LockedError with the file untouched. Callers with
// somewhere to park the work reschedule rather than fail.
package fsretry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// LockedError reports an operation abandoned because another program held
// the file for the whole retry window.
//
// The file is untouched and the operation is safe to repeat.
type LockedError struct {
	// Op is the operation that gave up: "rename" or "remove".
	Op string
	// Path is the file another program holds.
	Path string
	// Attempts is how many times the operation ran.
	Attempts int
	// Waited is how long the call spent before giving up.
	Waited time.Duration
	// Err is the last error the operating system returned.
	Err error
}

// ///////////////////////////////////////////////
// Timing
// ///////////////////////////////////////////////

const (
	// firstDelay is the pause after the first refusal. A scanner reading a
	// file usually releases it well inside this.
	firstDelay = 100 * time.Millisecond
	// maxDelay caps the backoff so a long wait still retries regularly.
	maxDelay = 5 * time.Second
	// window bounds the total wait. It is far shorter than a backup of a
	// large recording takes, which is the point: the caller reschedules
	// rather than holding a goroutine for hours.
	window = 30 * time.Second
)

// ///////////////////////////////////////////////
// Variables
// ///////////////////////////////////////////////

// restrictToOwner is the ownership call, indirected so a test can make it
// fail. No filesystem a runner has refuses one, and what this package does
// when it is refused is the contract most worth holding.
var restrictToOwner = RestrictToOwner

// Error implements error.
func (e *LockedError) Error() string {
	return fmt.Sprintf("%s %s: another program held it across %d attempts over %s: %v",
		e.Op, e.Path, e.Attempts, e.Waited.Round(time.Millisecond), e.Err)
}

// Unwrap exposes the operating system's error.
func (e *LockedError) Unwrap() error { return e.Err }

// ///////////////////////////////////////////////
// Operations
// ///////////////////////////////////////////////

// Rename moves a file, waiting out a lock another program holds on it.
func Rename(ctx context.Context, source, target string) error {
	return run(ctx, "rename", source, func() error {
		return os.Rename(source, target)
	})
}

// RenameNew moves a file to a name that must not already be taken,
// reporting fs.ErrExist when it is.
//
// os.Rename replaces its destination on both platforms: on Windows it is
// MoveFileEx with MOVEFILE_REPLACE_EXISTING, and on Unix rename(2) is
// specified to do the same. Checking that a name is free and then renaming
// is therefore a race that silently destroys whatever landed in between.
// Narrowing the window does not make it safe. Claiming the name with an
// exclusive create turns the race into an error the caller can resolve by
// picking another name.
func RenameNew(ctx context.Context, source, target string) error {
	claim, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := claim.Close(); err != nil {
		return fmt.Errorf("closing the claim on %s: %w", target, err)
	}

	// The rename replaces the empty file just claimed, which is this
	// call's own and safe to lose.
	//
	// It goes through replace rather than Rename because it is always a
	// replacement, and Windows refuses one onto a file any handle is open
	// on. A scanner reading the claim moments after it appears is the
	// ordinary case in a media library, and it answers with a denied access
	// that a plain lock check does not recognize.
	if err := replace(ctx, source, target); err != nil {
		os.Remove(target) // the rename failure is what matters
		return err
	}
	return nil
}

// Remove deletes a file, waiting out a lock another program holds on it.
// A file that is already absent reports the same error os.Remove does.
func Remove(ctx context.Context, path string) error {
	return run(ctx, "remove", path, func() error {
		return os.Remove(path)
	})
}

// WriteFileAtomic replaces a file's contents in one step, waiting out a
// lock another program holds on it.
//
// The data goes to a uniquely named temporary file beside the target and is
// flushed to disk before the rename. Every reader then sees either the whole
// previous file or the whole new one. That matters for a file that is the
// record of something rather than a cache of it: a write that truncates in
// place is readable half-finished, and a crash part way through leaves it
// that way for good.
func WriteFileAtomic(ctx context.Context, path string, data []byte, perm os.FileMode) error {
	return writeFile(ctx, path, data, perm, false)
}

// WriteFilePrivate is WriteFileAtomic for a file only its owner may read.
//
// The access is set on the staged file before the first byte reaches it, so
// there is no instant at which the contents exist under a wider access than
// they will end up with. Repairing the published file instead leaves that
// window open, and on Windows it leaves it open on the file every reader can
// already see.
//
// It refuses rather than publishing what it cannot protect. A filesystem
// with no access list to set, an exFAT stick or some network mounts, would
// otherwise take a live OAuth token and leave it readable by anything on the
// machine. The only report would be an error the caller can fold into a log
// line. The staged file is removed, so nothing is left behind either.
func WriteFilePrivate(ctx context.Context, path string, data []byte, perm os.FileMode) error {
	return writeFile(ctx, path, data, perm, true)
}

// MkdirPrivate creates a directory only its owner may enter.
//
// The access is set on creation and never on a directory already there.
// That matches what os.MkdirAll does with its mode, and what an operator
// who widened one deliberately expects. A directory this tool made keeps
// what this tool gave it, and one that was already there is somebody
// else's decision. The files inside are protected in their own right, so
// this is depth rather than the guard the tokens rest on.
func MkdirPrivate(path string, perm os.FileMode) error {
	// G703 traces a caller's path into a filesystem call, which is what
	// every function in this package does by definition. The paths that
	// reach here are the data, config, log and program directories, named by
	// the operator or resolved from the platform, never built from remote
	// metadata. What confines a path built from remote text is
	// naming.SanitizeSegment, organize.refuseReserved and
	// store.storablePath, each of which is tested for it.
	if parent := filepath.Dir(path); parent != path {
		if err := os.MkdirAll(parent, perm); err != nil { // G703: a directory the caller names is this function's argument
			return fmt.Errorf("creating %s: %w", parent, err)
		}
	}

	switch err := os.Mkdir(path, perm); { // G703: same, the leaf of the directory the caller named
	case errors.Is(err, os.ErrExist):
		return nil
	case err != nil:
		return fmt.Errorf("creating %s: %w", path, err)
	}

	if err := restrictToOwner(path, perm); err != nil {
		return err
	}
	return nil
}

// writeFile stages, optionally restricts, and publishes in one rename.
func writeFile(ctx context.Context, path string, data []byte, perm os.FileMode, private bool) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}

	temp, err := os.CreateTemp(dir, base+".tmp*")
	if err != nil {
		return fmt.Errorf("creating a temporary file beside %s: %w", path, err)
	}
	// A rename leaves nothing to remove, so this only fires on a path that
	// failed before it, a refusal to restrict included.
	defer os.Remove(temp.Name()) // best effort cleanup of a failed write

	if err := fill(temp, data, perm, private); err != nil {
		return fmt.Errorf("writing %s: %w", temp.Name(), err)
	}

	return replace(ctx, temp.Name(), path)
}

// replace moves a staged file onto its target, waiting out both a lock on
// the target and, on the platforms that refuse it, a handle held open on
// the file being replaced.
func replace(ctx context.Context, staged, path string) error {
	return runUntil(ctx, "rename", path,
		func(err error) bool { return isLocked(err) || replaceBlocked(path, err) },
		func() error { return os.Rename(staged, path) })
}

// fill writes a temporary file's contents and commits them to disk, so the
// rename that follows can only ever publish complete data.
func fill(file *os.File, data []byte, perm os.FileMode, private bool) (err error) {
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()

	if err := file.Chmod(perm); err != nil {
		return err
	}
	// Before the write, so the bytes never exist under a wider access than
	// the file ends up with.
	if private {
		if err := restrictToOwner(file.Name(), perm); err != nil {
			return err
		}
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

// run repeats action until it succeeds, fails for a reason other than a
// lock, or the window expires.
func run(ctx context.Context, op, path string, action func() error) error {
	return runUntil(ctx, op, path, isLocked, action)
}

// runUntil repeats action until it succeeds, fails for a reason retryable
// rejects, or the window expires.
func runUntil(ctx context.Context, op, path string, retryable func(error) bool, action func() error) error {
	start := time.Now()
	delay := firstDelay

	for attempt := 1; ; attempt++ {
		err := action()
		if err == nil || !retryable(err) {
			return err
		}

		// Stopping before a sleep that would overrun the window bounds the
		// total wait and skips a pause whose result is discarded anyway.
		if time.Since(start)+delay > window {
			return &LockedError{
				Op: op, Path: path,
				Attempts: attempt, Waited: time.Since(start), Err: err,
			}
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%s %s: %w", op, path, ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, maxDelay)
	}
}
