//go:build !windows

package service

// newAgentAutostart reports that this platform needs no notification
// helper.
//
// macOS runs the recorder as a launchd agent in the gui domain and Linux
// as a lingering systemd user unit, so both already sit in a session with
// a notification service to call. There is nothing for a helper to do that
// the recorder is not doing itself.
func newAgentAutostart() (AgentAutostart, error) { return nil, ErrNoAgent }
