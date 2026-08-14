// Package procgroup runs an external tool so that everything the tool
// starts dies with it.
//
// A capture engine muxes separate video and audio streams by starting
// ffmpeg, so ending the engine alone leaves a process still writing to the
// capture file. What follows differs by platform and neither outcome is
// acceptable. On Windows the survivor holds the output open, so the rename
// that finalizes the recording fails. On Unix the rename and the unlink
// both succeed, and the survivor keeps writing into an inode no directory
// entry names: the bytes stay charged against the filesystem, no path
// accounts for them, and the space reads as simply gone until the process
// exits.
//
// Both are closed by binding the tool's descendants to the tool. Run is the
// entry point, because the binding is only complete once the child is
// running and that is work only an owner of Start can do.
//
// Output belongs here for the same reason rather than a second one. What a
// tool's lifetime costs this process and what its output costs this process
// are one subject: a child that cannot outlive its parent can still grow it
// to the size of whatever it decides to print.
package procgroup

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// WaitDelay bounds how long a cancelled command may take to be reaped.
//
// Callers collect a tool's output through a pipe, which os/exec drains on a
// goroutine. A process the tool started holds the write end of that pipe
// open after the tool itself is gone, and Wait does not return while
// anything still holds it.
const WaitDelay = 10 * time.Second

// RefuseOption rejects a value a tool would read as an option rather than as
// the operand it is meant to be.
//
// Every operand this project passes a tool is ultimately remote: a channel
// name, a broadcast title, a video id. An option-shaped one reaches whatever
// the tool's own option set allows, and streamlink and yt-dlp both have
// options that name a program to run. The terminator each driver passes
// covers the position. This covers the value, so it holds whatever the
// argument order and whichever tool is spawned.
//
// It lives here because this is the package a driver goes through to spawn
// anything, so a new driver meets the guard on its way to the process rather
// than being expected to remember it.
func RefuseOption(what, value string) error {
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s %q starts with a dash, which a tool reads as an option", what, value)
	}
	return nil
}

// Run starts cmd, holds every process it starts alongside it, waits for it
// to finish, and then ends whatever is left of the group.
//
// It reports what cmd.Run reports, so an *exec.ExitError still carries the
// tool's exit status.
func Run(cmd *exec.Cmd) error {
	cmd.WaitDelay = WaitDelay

	group, err := newGroup(cmd)
	if err != nil {
		return err
	}
	// Closing the group is what ends a process the tool started and never
	// waited for, so it runs on every path out of here.
	defer group.Close()

	if err := cmd.Start(); err != nil {
		return err
	}

	if err := group.attach(cmd); err != nil {
		// The child is running with nothing binding its own children to
		// it, which is the state this package exists to prevent.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("holding %s in a process group: %w", cmd.Path, err)
	}
	return cmd.Wait()
}
