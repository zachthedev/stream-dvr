//go:build windows

package procgroup

import (
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// group is a job object holding a command and every process it starts.
//
// The handle is guarded because os/exec calls Cancel from the goroutine
// watching the context, which can reach it while Run is on its way out.
type group struct {
	mu     sync.Mutex
	handle windows.Handle
}

// stillActive is the exit code Windows reports for a process that has not
// finished, which is why an exit code alone cannot say a process is done.
const stillActive = 259

// newGroup creates the job the command's tree is held in and points the
// command's cancellation at it.
func newGroup(cmd *exec.Cmd) (*group, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("creating job object: %w", err)
	}

	// Closing the last handle to the job kills everything still inside it,
	// which is what reaches a process the tool started and abandoned.
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(handle, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("setting job object limits: %w", err)
	}

	g := &group{handle: handle}
	// Terminating the job ends the tree at once, which releases the output
	// pipes its members inherited. Killing only the child leaves Wait
	// blocked on those pipes until WaitDelay expires.
	cmd.Cancel = g.terminate
	return g, nil
}

// attach puts the started process into the job.
//
// A process the command starts before this lands inherits no job and
// escapes. streamlink resolves the stream over the network before it spawns
// a muxer, which puts hundreds of milliseconds between Start and the first
// grandchild, so the window is not one a real capture reaches. Closing it
// needs CREATE_SUSPENDED and the process's thread handle, neither of which
// os/exec exposes.
func (g *group) attach(cmd *exec.Cmd) error {
	// os/exec holds an open handle to the child for as long as cmd.Process
	// exists, and Windows keeps a pid reserved while any handle to it is
	// open, so this pid cannot name a different process.
	// The first two rights are what assigning to a job requires. The query
	// right is what distinguishes a refusal because the tool has already
	// finished from one that leaves a running process unheld.
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("opening process %d: %w", cmd.Process.Pid, err)
	}
	defer func() { _ = windows.CloseHandle(process) }()

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.handle == windows.InvalidHandle {
		return nil
	}
	if err := windows.AssignProcessToJobObject(g.handle, process); err != nil {
		// A tool that answered before this landed, which a version banner
		// routinely does, refuses the job because it has already exited.
		// There is nothing left to hold and nothing it could have left
		// behind, so the run stands.
		if exited(process) {
			return nil
		}
		return fmt.Errorf("assigning process %d to the job object: %w", cmd.Process.Pid, err)
	}
	return nil
}

// exited reports whether a process has already finished.
func exited(process windows.Handle) bool {
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err != nil {
		return false
	}
	return code != stillActive
}

// terminate ends every process in the job.
func (g *group) terminate() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.handle == windows.InvalidHandle {
		return nil
	}
	return windows.TerminateJobObject(g.handle, 1)
}

// Close releases the job, killing anything still in it.
func (g *group) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.handle == windows.InvalidHandle {
		return nil
	}

	handle := g.handle
	// Marked closed under the lock so a cancellation arriving after this
	// cannot reach a handle value the system is free to reissue.
	g.handle = windows.InvalidHandle
	return windows.CloseHandle(handle)
}
