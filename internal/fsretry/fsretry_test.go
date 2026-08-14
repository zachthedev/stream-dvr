package fsretry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// ///////////////////////////////////////////////
// LockedError
// ///////////////////////////////////////////////

func TestLockedError_SaysWhatIsStuckAndForHowLong(t *testing.T) {
	// This line is what an operator reads to work out why a recording never
	// moved. Without the path and the duration it says only that something
	// somewhere is busy.
	err := &LockedError{
		Op: "rename", Path: `D:\recordings\incoming\twitch-examplechannel-1.mkv`,
		Attempts: 9, Waited: 26300 * time.Millisecond,
		Err: errors.New("used by another process"),
	}

	for _, want := range []string{
		"rename",
		`D:\recordings\incoming\twitch-examplechannel-1.mkv`,
		"9 attempts",
		"26.3s",
		"used by another process",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to contain %q", err, want)
		}
	}
}

// ///////////////////////////////////////////////
// run
// ///////////////////////////////////////////////

func TestRun_ReturnsImmediatelyWhenTheActionSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		attempts := 0

		err := run(t.Context(), "remove", "free.ts", func() error {
			attempts++
			return nil
		})
		if err != nil {
			t.Fatalf("run() err = %v, want nil", err)
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want 1", attempts)
		}
		if elapsed := time.Since(start); elapsed != 0 {
			t.Errorf("run() waited %s, want no delay on success", elapsed)
		}
	})
}

func TestRun_DoesNotRetryAnErrorWaitingCannotFix(t *testing.T) {
	// Retrying a missing file or a denied ACL only delays the report by
	// half a minute, so only a lock earns the wait.
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		want := errors.New("no such file")

		err := run(t.Context(), "remove", "gone.ts", func() error {
			attempts++
			return want
		})
		if !errors.Is(err, want) {
			t.Errorf("run() err = %v, want %v returned unchanged", err, want)
		}
		if attempts != 1 {
			t.Errorf("attempts = %d, want the error reported without a retry", attempts)
		}
	})
}

func TestRun_SucceedsOnceTheLockClears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		attempts := 0

		err := run(t.Context(), "rename", "held.ts", func() error {
			attempts++
			if attempts < 4 {
				return lockedErr()
			}
			return nil
		})
		if err != nil {
			t.Fatalf("run() err = %v, want the operation to go through", err)
		}
		if attempts != 4 {
			t.Errorf("attempts = %d, want 4", attempts)
		}
	})
}

func TestRun_GivesUpWithinTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		attempts := 0

		err := run(t.Context(), "rename", "held.ts", func() error {
			attempts++
			return lockedErr()
		})

		var locked *LockedError
		if !errors.As(err, &locked) {
			t.Fatalf("run() err = %v, want a *LockedError", err)
		}
		if elapsed := time.Since(start); elapsed > window {
			t.Errorf("run() waited %s, want no more than the %s window", elapsed, window)
		}
		if attempts < 2 {
			t.Errorf("attempts = %d, want it to have retried before giving up", attempts)
		}
		if locked.Attempts != attempts {
			t.Errorf("LockedError.Attempts = %d, want %d", locked.Attempts, attempts)
		}
		if locked.Op != "rename" || locked.Path != "held.ts" {
			t.Errorf("LockedError = %s, want it to name the operation and file", locked)
		}
		if !errors.Is(err, lockedCause()) {
			t.Errorf("run() err = %v, want it to unwrap to %v", err, lockedCause())
		}
	})
}

func TestRun_BacksOffRatherThanSpinning(t *testing.T) {
	// A tight loop against a file a backup agent holds for minutes burns a
	// core for nothing. Each pause has to grow, up to the cap.
	synctest.Test(t, func(t *testing.T) {
		var gaps []time.Duration
		last := time.Now()

		_ = run(t.Context(), "rename", "held.ts", func() error {
			now := time.Now()
			gaps = append(gaps, now.Sub(last))
			last = now
			return lockedErr()
		})

		// The first attempt runs with no pause before it.
		if len(gaps) < 4 {
			t.Fatalf("attempts = %d, want enough to observe the backoff", len(gaps))
		}
		if gaps[0] != 0 {
			t.Errorf("first attempt waited %s, want none", gaps[0])
		}
		for i := 2; i < len(gaps); i++ {
			if gaps[i] < gaps[i-1] {
				t.Errorf("gap %d = %s, shorter than the %s before it", i, gaps[i], gaps[i-1])
			}
			if gaps[i] > maxDelay {
				t.Errorf("gap %d = %s, want it capped at %s", i, gaps[i], maxDelay)
			}
		}
	})
}

func TestRun_StopsWhenTheContextIsCancelled(t *testing.T) {
	// Shutting the daemon down must not wait out the window first.
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		start := time.Now()
		attempts := 0

		err := run(ctx, "remove", "held.ts", func() error {
			attempts++
			if attempts == 2 {
				cancel()
			}
			return lockedErr()
		})

		if !errors.Is(err, context.Canceled) {
			t.Errorf("run() err = %v, want it to report the cancellation", err)
		}
		// Two attempts means one pause, so anything beyond the opening
		// delay is the loop ignoring the cancellation.
		if elapsed := time.Since(start); elapsed > firstDelay {
			t.Errorf("run() ran for %s after cancellation, want it to stop inside %s", elapsed, firstDelay)
		}
	})
}

// ///////////////////////////////////////////////
// Rename, Remove, WriteFileAtomic on a free file
// ///////////////////////////////////////////////

func TestRename_MovesTheFile(t *testing.T) {
	path := seed(t, "capture.ts", "payload")
	target := filepath.Join(filepath.Dir(path), "final.mkv")

	if err := Rename(t.Context(), path, target); err != nil {
		t.Fatalf("Rename() err = %v, want nil", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("Stat(target) err = %v, want the file moved", err)
	}
}

func TestRenameNew_MovesOntoAFreeName(t *testing.T) {
	path := seed(t, "capture.ts", "payload")
	target := filepath.Join(filepath.Dir(path), "final.mkv")

	if err := RenameNew(t.Context(), path, target); err != nil {
		t.Fatalf("RenameNew() err = %v, want nil", err)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("reading the target: %v", err)
	}
	if string(content) != "payload" {
		t.Errorf("target content = %q, want the source's %q", content, "payload")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(source) err = %v, want it moved", err)
	}
}

func TestRenameNew_RefusesATakenName(t *testing.T) {
	// The whole point. os.Rename replaces its destination, so a caller that
	// checked the name was free and then renamed would destroy whatever
	// arrived in between.
	path := seed(t, "capture.ts", "newcomer")
	target := filepath.Join(filepath.Dir(path), "final.mkv")
	if err := os.WriteFile(target, []byte("irreplaceable"), 0o600); err != nil {
		t.Fatalf("seeding the target: %v", err)
	}

	err := RenameNew(t.Context(), path, target)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("RenameNew() err = %v, want it to report %v", err, fs.ErrExist)
	}

	content, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("reading the target: %v", readErr)
	}
	if string(content) != "irreplaceable" {
		t.Errorf("target content = %q, want it untouched", content)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Stat(source) err = %v, want the source left in place", err)
	}
}

func TestRenameNew_LeavesNoClaimBehindWhenTheMoveFails(t *testing.T) {
	// The claim is an empty file standing in for the recording. Leaving it
	// behind would make the name look taken to the next attempt, so a
	// failed move has to take it back out.
	target := filepath.Join(t.TempDir(), "final.mkv")

	err := RenameNew(t.Context(), filepath.Join(t.TempDir(), "never-existed.ts"), target)
	if err == nil {
		t.Fatal("RenameNew() err = nil, want the missing source reported")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(target) err = %v, want the claim removed", err)
	}
}

func TestRenameNew_OnlyOneOfManyRacersWins(t *testing.T) {
	// Concurrent finalizes are the ordinary case: one organizer serves
	// every channel watcher and the sweep.
	dir := t.TempDir()
	target := filepath.Join(dir, "contested.mkv")

	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	for i := range racers {
		source := filepath.Join(dir, fmt.Sprintf("source-%d.ts", i))
		if err := os.WriteFile(source, []byte(fmt.Sprintf("racer %d", i)), 0o600); err != nil {
			t.Fatalf("seeding %s: %v", source, err)
		}
		wg.Go(func() {
			results[i] = RenameNew(context.Background(), source, target)
		})
	}
	wg.Wait()

	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, fs.ErrExist):
		default:
			t.Errorf("racer %d err = %v, want nil or %v", i, err, fs.ErrExist)
		}
	}
	if winners != 1 {
		t.Errorf("%d racers took the name, want exactly 1", winners)
	}

	// Every loser must still have its own file to rename elsewhere.
	survivors, err := filepath.Glob(filepath.Join(dir, "source-*.ts"))
	if err != nil {
		t.Fatalf("Glob() err = %v, want nil", err)
	}
	if len(survivors) != racers-1 {
		t.Errorf("%d sources survived, want %d", len(survivors), racers-1)
	}
}

func TestRemove_DeletesTheFile(t *testing.T) {
	path := seed(t, "capture.ts", "payload")

	if err := Remove(t.Context(), path); err != nil {
		t.Fatalf("Remove() err = %v, want nil", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat() err = %v, want the file gone", err)
	}
}

func TestRemove_ReportsAnAbsentFile(t *testing.T) {
	// Callers rely on os.Remove's contract, so absence must not be quietly
	// swallowed here.
	err := Remove(t.Context(), filepath.Join(t.TempDir(), "never-existed.ts"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove() err = %v, want it to report the absent file", err)
	}
}

// ///////////////////////////////////////////////
// Atomic writes
// ///////////////////////////////////////////////

func TestWriteFileAtomic_ReplacesTheContents(t *testing.T) {
	tests := []struct {
		name    string
		seeded  string
		content string
	}{
		{name: "a new file", content: "new"},
		{name: "an existing file", seeded: "old", content: "new"},
		{name: "a shorter payload than the file it replaces", seeded: "a much longer previous body", content: "new"},
		{name: "an empty payload", seeded: "old", content: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sidecar.json")
			if tt.seeded != "" {
				if err := os.WriteFile(path, []byte(tt.seeded), 0o600); err != nil {
					t.Fatalf("seeding: %v", err)
				}
			}

			if err := WriteFileAtomic(t.Context(), path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("WriteFileAtomic() err = %v, want nil", err)
			}

			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading back: %v", err)
			}
			if string(content) != tt.content {
				t.Errorf("content = %q, want %q", content, tt.content)
			}
			if leftovers := siblings(t, path); len(leftovers) != 1 {
				t.Errorf("directory holds %v, want only the target", leftovers)
			}
		})
	}
}

func TestWriteFileAtomic_ReportsAnUnwritableTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-directory", "sidecar.json")

	if err := WriteFileAtomic(t.Context(), path, []byte("body"), 0o600); err == nil {
		t.Error("WriteFileAtomic() err = nil, want the missing directory reported")
	}
}

func TestWriteFileAtomic_AReaderNeverSeesAPartialFile(t *testing.T) {
	// This is the whole reason the sidecar and the ownership marker go
	// through here. Both are the record of something rather than a cache of
	// it, and a write that truncates in place is readable half-finished.
	dir := t.TempDir()
	path := filepath.Join(dir, "sidecar.json")

	const size = 64 * 1024
	bodies := [][]byte{bytes.Repeat([]byte("a"), size), bytes.Repeat([]byte("b"), size)}
	if err := os.WriteFile(path, bodies[0], 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	done := make(chan struct{})
	var writers sync.WaitGroup
	for _, body := range bodies {
		writers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				if err := WriteFileAtomic(context.Background(), path, body, 0o600); err != nil {
					t.Errorf("WriteFileAtomic() err = %v, want nil", err)
					return
				}
			}
		})
	}

	// A read can legitimately fail while the rename swaps the file out from
	// under it. Content that is neither payload cannot happen at all.
	for range 300 {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if !bytes.Equal(content, bodies[0]) && !bytes.Equal(content, bodies[1]) {
			t.Fatalf("read %d bytes starting %.8q, want one of the two whole payloads", len(content), content)
		}
	}

	close(done)
	writers.Wait()
}

func TestWriteFileAtomic_ReplacesASymlinkRatherThanWritingThroughIt(t *testing.T) {
	// A link standing where the file belongs would otherwise carry the
	// write to whatever it names, at whatever access that file holds. The
	// rename claims the link's own path, so the write lands where the caller
	// asked and the mode is the one it asked for.
	dir := t.TempDir()
	decoy := filepath.Join(dir, "decoy")
	if err := os.WriteFile(decoy, []byte("untouched"), 0o600); err != nil {
		t.Fatalf("writing the decoy: %v", err)
	}
	path := filepath.Join(dir, "sidecar.json")
	if err := os.Symlink(decoy, path); err != nil {
		t.Skipf("this filesystem does not take symlinks: %v", err)
	}

	if err := WriteFileAtomic(t.Context(), path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic() err = %v, want nil", err)
	}

	content, err := os.ReadFile(decoy)
	if err != nil {
		t.Fatalf("reading the decoy: %v", err)
	}
	if string(content) != "untouched" {
		t.Errorf("the decoy holds %q, want the write landing beside it rather than through the link", content)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("mode = %v, want the link replaced by a regular file", info.Mode())
	}
	content, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(content) != "new" {
		t.Errorf("content = %q, want %q", content, "new")
	}
}

// siblings lists the names in a path's directory.
func siblings(t *testing.T, path string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Dir(path), err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestWriteFilePrivate_PublishesNothingWhenItCannotProtectTheFile is the
// fail-closed contract.
//
// A filesystem with no access list to set, an exFAT stick or some network
// mounts, would otherwise take a live OAuth token and publish it readable by
// anything on the machine. Reporting that through an error a caller may fold
// into a log line is not enough for a token, so the write is abandoned.
//
// The failure is injected rather than waiting for such a filesystem, because
// no runner has one and the contract has to hold on every platform.
func TestWriteFilePrivate_PublishesNothingWhenItCannotProtectTheFile(t *testing.T) {
	original := restrictToOwner
	t.Cleanup(func() { restrictToOwner = original })
	restrictToOwner = func(string, os.FileMode) error {
		return errors.New("this filesystem has no access list to set")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	err := WriteFilePrivate(context.Background(), path, []byte(`{"refresh":"a-live-token"}`), 0o600)
	if err == nil {
		t.Fatal("WriteFilePrivate() err = nil, want a refusal when the file cannot be protected")
	}

	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("the token was published anyway, which is the outcome this refusal exists to prevent")
	}

	// The staged file holds the same bytes, so leaving one behind would put
	// the token on disk under a name nothing ever cleans up.
	for _, name := range siblings(t, path) {
		t.Errorf("%s was left behind, still holding the token", name)
	}
}

// TestWriteFileAtomic_DoesNotRestrictWhatItWrites keeps the two writers
// apart.
//
// The sidecar exists for a media player to read, and the library marker can
// sit on a volume shared with one. A change that quietly restricted every
// atomic write would break both without failing anything here.
func TestWriteFileAtomic_DoesNotRestrictWhatItWrites(t *testing.T) {
	restricted := false
	original := restrictToOwner
	t.Cleanup(func() { restrictToOwner = original })
	restrictToOwner = func(string, os.FileMode) error {
		restricted = true
		return nil
	}

	path := filepath.Join(t.TempDir(), "recording.json")
	if err := WriteFileAtomic(context.Background(), path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic() err = %v, want nil", err)
	}
	if restricted {
		t.Error("WriteFileAtomic restricted the file, which would hide a sidecar from every media player")
	}
}
