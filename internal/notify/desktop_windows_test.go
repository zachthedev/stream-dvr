package notify

import (
	"errors"
	"testing"
)

// Nothing here calls raise, sessionAvailable, or NewSessionDesktop. Each
// builds the notification area icon, which is a change to the desktop of
// whoever is running the tests rather than a fact about the code.

func TestDesktopAvailable_RoutesToTheAgent(t *testing.T) {
	// The recorder is a scheduled task with an S4U logon, which runs
	// non-interactively in session 0. There is no desktop there to post to,
	// and no amount of configuration changes that, so the agent raises one
	// instead.
	//
	// The sentinel rather than the wording: the daemon branches on this to
	// decide whether an operator has misconfigured something or is running
	// the ordinary Windows arrangement.
	err := desktopAvailable()

	if err == nil {
		t.Fatal("desktopAvailable() err = nil, want the agent named as the mechanism")
	}
	if !errors.Is(err, ErrNeedsAgent) {
		t.Errorf("desktopAvailable() err = %v, want it to match ErrNeedsAgent", err)
	}
}
