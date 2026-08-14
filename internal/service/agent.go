package service

import "errors"

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// AgentAutostart registers a helper to start when the operator signs in.
//
// It is separate from Manager because the two answer different questions.
// Manager registers the recorder, which must run with nobody signed in and
// therefore needs a boot trigger and sometimes an elevated shell. This
// registers a helper that is only useful while somebody is signed in, so
// it hangs off the session and needs no privilege at all.
//
// Only Windows has one. Elsewhere the recorder raises its own
// notifications from the session it already runs in, so there is no helper
// to start.
type AgentAutostart interface {
	// Install registers the helper, replacing an existing registration.
	Install(executable string, args []string) error
	// Uninstall removes it. Removing one that is not there is not an
	// error, so uninstall is safe to repeat.
	Uninstall() error
	// Installed reports whether a registration exists.
	Installed() (bool, error)
	// Mechanism names the platform facility, for output that tells the
	// operator where to look.
	Mechanism() string
}

// ErrNoAgent reports a platform that needs no notification helper.
//
// It is not a failure. The recorder raises its own notifications on macOS
// and Linux, so a caller treats this as "nothing to do" rather than as
// something an operator must act on.
var ErrNoAgent = errors.New("this platform raises notifications from the recorder itself")

// NewAgentAutostart returns the helper registration for the host platform.
func NewAgentAutostart() (AgentAutostart, error) { return newAgentAutostart() }
