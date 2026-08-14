//go:build windows

package procgroup

import (
	"context"
	"testing"

	"golang.org/x/sys/windows"
)

// ///////////////////////////////////////////////
// Job object
// ///////////////////////////////////////////////

func TestNewGroup_CreatesAJobAndAimsCancellationAtIt(t *testing.T) {
	cmd := helperCommand(context.Background(), "immediate")

	g, err := newGroup(cmd)
	if err != nil {
		t.Fatalf("newGroup() err = %v, want a job object", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	if g.handle == 0 || g.handle == windows.InvalidHandle {
		t.Errorf("newGroup() handle = %v, want a usable job object", g.handle)
	}
	// Killing only the child leaves Wait blocked on the output pipes its
	// own children inherited until WaitDelay expires.
	if cmd.Cancel == nil {
		t.Error("cmd.Cancel = nil, want cancellation to end the whole job")
	}
}

func TestClose_LeavesNothingForALaterCallToReach(t *testing.T) {
	// os/exec calls Cancel from the goroutine watching the context, which
	// can reach the group after Run has closed it. Windows reissues handle
	// values, so a call that late would act on something else entirely.
	cmd := helperCommand(context.Background(), "immediate")

	g, err := newGroup(cmd)
	if err != nil {
		t.Fatalf("newGroup() err = %v, want a job object", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("Close() err = %v, want nil", err)
	}

	if g.handle != windows.InvalidHandle {
		t.Errorf("handle = %v after Close(), want it marked unusable", g.handle)
	}
	if err := g.Close(); err != nil {
		t.Errorf("second Close() err = %v, want nil", err)
	}
	if err := g.terminate(); err != nil {
		t.Errorf("terminate() after Close() err = %v, want it to do nothing", err)
	}
}

func TestAttach_DoesNothingOnceTheGroupIsClosed(t *testing.T) {
	cmd := helperCommand(context.Background(), "immediate")

	g, err := newGroup(cmd)
	if err != nil {
		t.Fatalf("newGroup() err = %v, want a job object", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() err = %v, want the helper running", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })

	if err := g.Close(); err != nil {
		t.Fatalf("Close() err = %v, want nil", err)
	}
	if err := g.attach(cmd); err != nil {
		t.Errorf("attach() after Close() err = %v, want nil rather than a call on a reissued handle", err)
	}
}

func TestAttach_AcceptsAToolThatAlreadyFinished(t *testing.T) {
	// A version banner answers in milliseconds, and a job refuses a process
	// that has already exited. Reporting that as a failure would turn every
	// fast tool into an error.
	cmd := helperCommand(context.Background(), "immediate")

	g, err := newGroup(cmd)
	if err != nil {
		t.Fatalf("newGroup() err = %v, want a job object", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() err = %v, want the helper running", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	awaitExit(t, cmd.Process.Pid)

	if err := g.attach(cmd); err != nil {
		t.Errorf("attach() err = %v, want a finished tool accepted", err)
	}
}

func TestExited_ReportsWhetherTheProcessIsDone(t *testing.T) {
	cmd := helperCommand(context.Background(), "immediate")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() err = %v, want the helper running", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })

	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatalf("OpenProcess() err = %v", err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(process) })

	if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
		t.Fatalf("WaitForSingleObject() err = %v", err)
	}

	tests := []struct {
		name   string
		handle windows.Handle
		want   bool
	}{
		{name: "a process that has finished", handle: process, want: true},
		{
			// A handle the system will not answer for says nothing about
			// any process, so it must not read as one that finished.
			name:   "a handle that answers for nothing",
			handle: windows.InvalidHandle,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exited(tt.handle); got != tt.want {
				t.Errorf("exited() = %t, want %t", got, tt.want)
			}
		})
	}
}

// awaitExit blocks until the process with pid has finished, without reaping
// it, so a test can act on a command os/exec still considers running.
func awaitExit(t *testing.T, pid int) {
	t.Helper()

	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("OpenProcess(%d) err = %v", pid, err)
	}
	defer func() { _ = windows.CloseHandle(process) }()

	if _, err := windows.WaitForSingleObject(process, windows.INFINITE); err != nil {
		t.Fatalf("WaitForSingleObject() err = %v", err)
	}
}

func TestTerminate_EndsTheJob(t *testing.T) {
	cmd := helperCommand(context.Background(), "immediate")

	g, err := newGroup(cmd)
	if err != nil {
		t.Fatalf("newGroup() err = %v, want a job object", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() err = %v, want the helper running", err)
	}
	if err := g.attach(cmd); err != nil {
		t.Fatalf("attach() err = %v, want the process held", err)
	}
	if err := g.terminate(); err != nil {
		t.Errorf("terminate() err = %v, want the job ended", err)
	}
	_ = cmd.Wait()
}
