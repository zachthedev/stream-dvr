package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Helper process
// ///////////////////////////////////////////////

// heartbeatInterval is how often the grandchild touches its file. It is
// short enough that a process still running is certain to advance the file
// inside the window a test watches.
const heartbeatInterval = 100 * time.Millisecond

// helperLifetime bounds every helper process, so a test that fails to reap
// one leaves nothing behind after the suite ends.
const helperLifetime = 30 * time.Second

// TestHelperProcess stands in for a capture tool. It is not a test: it runs
// only when the parent re-invokes this binary.
//
// The "spawn" mode is the shape that matters. A capture engine muxing two
// streams starts a second process and outlives its own output, so the
// helper starts a grandchild of its own and then sleeps.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		t.Skip("helper process, invoked only by helperCommand")
	}

	switch os.Getenv("FAKE_MODE") {
	case "spawn":
		grandchild := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--") // G204: the only program started is this test binary
		grandchild.Env = append(os.Environ(), "FAKE_MODE=heartbeat")
		if err := grandchild.Start(); err != nil {
			os.Exit(1)
		}
		// Announced once the grandchild is running, so a test that waits
		// for this is cancelling a tree that is certainly formed.
		os.WriteFile(os.Getenv("FAKE_READY"), []byte("spawned"), 0o644)
		time.Sleep(helperLifetime)

	case "spawn_and_exit":
		// The engine finishes on its own and its muxer does not. Nothing
		// cancels here, so only the group closing reaches the survivor.
		grandchild := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--") // G204: the only program started is this test binary
		grandchild.Env = append(os.Environ(), "FAKE_MODE=heartbeat",
			"FAKE_HEARTBEAT="+os.Getenv("FAKE_HEARTBEAT"))
		if err := grandchild.Start(); err != nil {
			os.Exit(1)
		}
		// Exits only once the muxer has written something, so the test has a
		// size to compare rather than an absent file.
		for range 500 {
			if _, err := os.Stat(os.Getenv("FAKE_HEARTBEAT")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

	case "heartbeat":
		beat := os.Getenv("FAKE_HEARTBEAT")
		for range int(helperLifetime / heartbeatInterval) {
			file, err := os.OpenFile(beat, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				os.Exit(1)
			}
			file.WriteString("beat\n")
			file.Close()
			time.Sleep(heartbeatInterval)
		}

	case "exit_code":
		os.Exit(3)

	case "immediate":
	}
	os.Exit(0)
}

// helperCommand builds a command that re-invokes this binary in one of the
// helper's modes.
func helperCommand(ctx context.Context, mode string, env ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess", "--") // G204: the only program started is this test binary
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "FAKE_MODE="+mode)
	cmd.Env = append(cmd.Env, env...)
	return cmd
}

// waitForFile blocks until path exists, failing the test if it never does.
func waitForFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// ///////////////////////////////////////////////
// Group lifetime
// ///////////////////////////////////////////////

func TestRun_CancellingEndsWhatTheChildStarted(t *testing.T) {
	// Killing the capture engine alone leaves the muxer it started writing
	// to the recording. On Windows that survivor holds the file open and
	// the finalizing rename fails. On Unix the rename and the unlink both
	// succeed and the survivor writes on into an unlinked inode, so the
	// space is spent with no path naming it.
	dir := t.TempDir()
	heartbeat := filepath.Join(dir, "heartbeat")
	ready := filepath.Join(dir, "ready")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := helperCommand(ctx, "spawn", "FAKE_HEARTBEAT="+heartbeat, "FAKE_READY="+ready)

	done := make(chan error, 1)
	go func() { done <- Run(cmd) }()

	waitForFile(t, ready)
	waitForFile(t, heartbeat)
	cancel()

	select {
	case <-done:
	case <-time.After(WaitDelay + 10*time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}

	before, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatalf("reading the heartbeat: %v", err)
	}

	// Observed through the file rather than through a pid, because a pid
	// that has been reused answers for a process this test never started.
	time.Sleep(10 * heartbeatInterval)

	after, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatalf("reading the heartbeat: %v", err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the heartbeat grew from %d bytes to %d after the capture was cancelled, want a process the group took with it",
			before.Size(), after.Size())
	}
}

func TestRun_ACompletedCommandLeavesNothingBehind(t *testing.T) {
	// The engine exiting is not the end of what the engine started. A muxer
	// it never waited for keeps appending to the recording that this call's
	// return says is finished, so the group is closed on the way out of a
	// clean run as well as a cancelled one.
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")

	cmd := helperCommand(context.Background(), "spawn_and_exit", "FAKE_HEARTBEAT="+heartbeat)
	if err := Run(cmd); err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}

	before, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatalf("reading the heartbeat: %v", err)
	}

	time.Sleep(10 * heartbeatInterval)

	after, err := os.Stat(heartbeat)
	if err != nil {
		t.Fatalf("reading the heartbeat: %v", err)
	}
	if after.Size() != before.Size() {
		t.Errorf("the heartbeat grew from %d bytes to %d after the command finished, want the group closed behind it",
			before.Size(), after.Size())
	}
}

func TestRun_ReportsWhatTheToolReported(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		wantCode int
	}{
		{name: "a tool that succeeds", mode: "immediate", wantCode: 0},
		{name: "a tool that fails", mode: "exit_code", wantCode: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Run(helperCommand(context.Background(), tt.mode))

			if tt.wantCode == 0 {
				if err != nil {
					t.Fatalf("Run() err = %v, want nil", err)
				}
				return
			}

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("Run() err = %v, want an *exec.ExitError carrying the status", err)
			}
			if exitErr.ExitCode() != tt.wantCode {
				t.Errorf("Run() exit code = %d, want %d", exitErr.ExitCode(), tt.wantCode)
			}
		})
	}
}

func TestRun_ReportsACommandThatCannotStart(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), filepath.Join(t.TempDir(), "absent"))

	if err := Run(cmd); err == nil {
		t.Error("Run() err = nil, want the failure to start reported")
	}
}

func TestRun_BoundsHowLongReapingMayTake(t *testing.T) {
	// A process the tool started holds the inherited output pipe open after
	// the tool is gone, and Wait does not return while anything holds it.
	// Without the delay that wait has no end.
	if WaitDelay <= 0 {
		t.Errorf("WaitDelay = %s, want a bound on reaping a cancelled command", WaitDelay)
	}

	cmd := helperCommand(context.Background(), "immediate")
	if err := Run(cmd); err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}
	if cmd.WaitDelay != WaitDelay {
		t.Errorf("cmd.WaitDelay = %s, want %s", cmd.WaitDelay, WaitDelay)
	}
}

func TestRun_CancellationReachesTheToolItself(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	heartbeat := filepath.Join(dir, "heartbeat")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := helperCommand(ctx, "spawn", "FAKE_HEARTBEAT="+heartbeat, "FAKE_READY="+ready)

	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- Run(cmd) }()

	waitForFile(t, ready)
	cancel()

	select {
	case err := <-done:
		// The helper sleeps far longer than this, so returning at all
		// proves the cancellation reached it rather than the sleep ending.
		if err == nil {
			t.Error("Run() err = nil, want the cancellation reported")
		}
	case <-time.After(WaitDelay + 10*time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
	if elapsed := time.Since(started); elapsed >= helperLifetime {
		t.Errorf("Run() took %s, want it to end well inside the helper's own %s", elapsed, helperLifetime)
	}
}

func TestRun_HoldsACommandThatOutlivesNothing(t *testing.T) {
	// A version banner answers in milliseconds, so the tool can be gone
	// before the group is formed around it. That is not a failure: a
	// process that exited that fast started nothing to hold.
	for range 20 {
		if err := Run(helperCommand(context.Background(), "immediate")); err != nil {
			t.Fatalf("Run() err = %v, want a tool that finishes immediately to run cleanly", err)
		}
	}
}

// TestRefuseOption_RejectsAValueATooolWouldReadAsAnOption covers the guard
// every driver goes through on its way to a process.
//
// Each operand this project passes a tool comes from somewhere remote: a
// channel name, a broadcast title, a video id. streamlink and yt-dlp both
// have options that name a program to run, so an option-shaped operand
// reaches whatever the tool's own option set allows.
func TestRefuseOption_RejectsAValueATooolWouldReadAsAnOption(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		refuse bool
	}{
		{"a long option", "--version", true},
		{"a value-taking option", "--config-locations=/tmp/planted.conf", true},
		{"a short option", "-J", true},
		{"a bare dash", "-", true},
		{"an ordinary address", "https://twitch.tv/examplechannel", false},
		{"a video id", "v2100001", false},
		{"a name with a dash inside it", "example-channel", false},
		{"an empty value", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RefuseOption("channel URL", tt.value)
			if tt.refuse && err == nil {
				t.Errorf("RefuseOption(%q) err = nil, want a refusal", tt.value)
			}
			if !tt.refuse && err != nil {
				t.Errorf("RefuseOption(%q) err = %v, want nil", tt.value, err)
			}
			if tt.refuse && err != nil && !strings.Contains(err.Error(), "channel URL") {
				t.Errorf("err = %v, want it to name what was refused so the operator knows which value", err)
			}
		})
	}
}
