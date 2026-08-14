//go:build !windows

package procgroup

import (
	"context"
	"syscall"
	"testing"
)

// ///////////////////////////////////////////////
// Process group
// ///////////////////////////////////////////////

func TestNewGroup_MakesTheChildLeadItsOwnGroup(t *testing.T) {
	// Without this the child shares the daemon's own process group, and a
	// kill aimed at the group reaches the daemon and every capture it runs.
	cmd := helperCommand(context.Background(), "immediate")

	g, err := newGroup(cmd)
	if err != nil {
		t.Fatalf("newGroup() err = %v, want nil", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid is not set, so the child would share this process's group")
	}
	if cmd.Cancel == nil {
		t.Error("cmd.Cancel = nil, want cancellation to reach the group")
	}
}

func TestNewGroup_KeepsAttributesTheCallerAlreadySet(t *testing.T) {
	cmd := helperCommand(context.Background(), "immediate")
	attrs := &syscall.SysProcAttr{}
	cmd.SysProcAttr = attrs

	g, err := newGroup(cmd)
	if err != nil {
		t.Fatalf("newGroup() err = %v, want nil", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	if cmd.SysProcAttr != attrs {
		t.Error("SysProcAttr was replaced, want the caller's own settings kept")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("Setpgid is not set on the caller's attributes")
	}
}

func TestAttach_RecordsTheLeadersPid(t *testing.T) {
	cmd := helperCommand(context.Background(), "immediate")

	g, err := newGroup(cmd)
	if err != nil {
		t.Fatalf("newGroup() err = %v, want nil", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() err = %v, want the helper running", err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })

	if err := g.attach(cmd); err != nil {
		t.Fatalf("attach() err = %v, want nil", err)
	}
	if got := g.id(); got != cmd.Process.Pid {
		t.Errorf("group id = %d, want the leader's pid %d", got, cmd.Process.Pid)
	}
}

func TestClose_DeclinesAGroupItMustNotSignal(t *testing.T) {
	tests := []struct {
		name string
		pgid int
	}{
		{
			// Reached whenever cancellation beats the group being formed.
			name: "before the group is formed",
			pgid: 0,
		},
		{
			// A negative id is what the kill takes, so passing one through
			// would negate twice and name a single process.
			name: "a negative id",
			pgid: -1,
		},
		{
			// This is the daemon's own group, holding every capture it runs
			// and the daemon itself.
			name: "this process's own group",
			pgid: syscall.Getpgrp(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &group{pgid: tt.pgid}
			if err := g.Close(); err != nil {
				t.Errorf("Close() err = %v, want it to decline silently", err)
			}
		})
	}
}

func TestClose_ToleratesAGroupThatIsAlreadyEmpty(t *testing.T) {
	// Every capture that ends on its own reaches Close with the tool
	// reaped and nothing left behind, which the kernel reports as ESRCH.
	cmd := helperCommand(context.Background(), "immediate")

	g, err := newGroup(cmd)
	if err != nil {
		t.Fatalf("newGroup() err = %v, want nil", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start() err = %v, want the helper running", err)
	}
	if err := g.attach(cmd); err != nil {
		t.Fatalf("attach() err = %v, want nil", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait() err = %v, want nil", err)
	}

	if err := g.Close(); err != nil {
		t.Errorf("Close() err = %v, want an empty group treated as the ordinary case", err)
	}
}
