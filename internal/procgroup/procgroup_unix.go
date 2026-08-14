//go:build !windows

package procgroup

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
)

// group is a process group holding a command and its descendants.
//
// The id is guarded because os/exec calls Cancel from the goroutine
// watching the context, which can reach it while Run is still forming the
// group.
type group struct {
	mu   sync.Mutex
	pgid int
}

// newGroup arranges for the command to lead a process group of its own and
// points the command's cancellation at that group.
func newGroup(cmd *exec.Cmd) (*group, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// The child becomes a group leader, so its own pid names the group that
	// every process it starts inherits.
	cmd.SysProcAttr.Setpgid = true

	g := &group{}
	// A tool told to stop can close its output and exit, which leaves a
	// file that plays. Close sends the signal that does not ask.
	cmd.Cancel = func() error {
		if pgid := g.id(); pgid != 0 {
			return syscall.Kill(-pgid, syscall.SIGTERM)
		}
		// The group is not formed until the child is running, and a
		// cancellation can arrive before that.
		return cmd.Process.Kill()
	}
	return g, nil
}

// attach records the group the started process leads.
//
// Setpgid makes the child its own group leader, so the pid Start reported
// is the group id. Asking the kernel instead would race a tool that has
// already exited, which is the common case for a version banner.
func (g *group) attach(cmd *exec.Cmd) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pgid = cmd.Process.Pid
	return nil
}

// id returns the group id, or zero before the group is formed.
func (g *group) id() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pgid
}

// Close kills whatever is left of the group.
func (g *group) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pgid <= 0 || g.pgid == syscall.Getpgrp() {
		// A group id matching this process's own would aim the signal at
		// the daemon and everything else sharing its group.
		return nil
	}

	// The command has been waited for, so the group holds only processes it
	// started and abandoned. An empty group reports ESRCH, which is the
	// ordinary case rather than a failure.
	if err := syscall.Kill(-g.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
