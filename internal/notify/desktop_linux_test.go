package notify

import (
	"testing"
)

func TestSessionBus_ReportsAMachineWithNoBus(t *testing.T) {
	// A build machine or a headless server has no session bus, and that is
	// a fact about the machine rather than a fault. What matters is that it
	// is answered rather than retried forever.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", "")

	connection, err := sessionBus()
	if err == nil {
		connection.Close()
		t.Skip("this machine has a session bus reachable without either variable")
	}

	if got := err.Error(); got != "no session bus is reachable, so no desktop notification can be raised" {
		t.Errorf("sessionBus() err = %q, want it to say no bus is reachable", got)
	}
}

func TestSessionBus_FallsBackToTheRuntimeDirectory(t *testing.T) {
	// A session that never ran dbus-update-activation-environment leaves
	// DBUS_SESSION_BUS_ADDRESS unset in the user manager's environment,
	// while the socket is still at its known path.
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	connection, err := sessionBus()
	if err == nil {
		// A machine that reaches a bus without either variable, through
		// autolaunch or a system-wide address, cannot show what the
		// fallback path does: the connection proves only that some route
		// exists. CI runners are such machines, so this is skipped rather
		// than failed.
		connection.Close()
		t.Skip("this machine has a session bus reachable without either variable")
	}

	// The point is that it tried the fallback path and reported the same
	// answer, rather than failing differently for want of a variable.
	if got := err.Error(); got != "no session bus is reachable, so no desktop notification can be raised" {
		t.Errorf("sessionBus() err = %q, want the fallback to report the same answer", got)
	}
}
