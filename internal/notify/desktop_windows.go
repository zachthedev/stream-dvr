package notify

// The rest of this platform's implementation is in tray_windows.go: the
// notification area icon a balloon is raised over, and the hidden window
// that owns it.

// ///////////////////////////////////////////////
// Platform
// ///////////////////////////////////////////////

// desktopAvailable reports that the recorder cannot raise a notification
// from where Windows runs it.
//
// The recorder is a scheduled task with an S4U logon and a boot trigger, so
// it runs non-interactively in session 0. Session 0 has its own window
// station with no visible desktop, and the notification platform is
// per-session, so there is nothing there to post to.
//
// It also cannot start a helper that would be in the right session:
// placing a process in another session needs a token from
// WTSQueryUserToken, which requires SE_TCB_NAME, and a task running as the
// operator rather than as the machine does not have it. The helper has to
// be started by the session, which is what the notify agent is.
//
// So the recorder publishes on the bus and the agent raises what it reads.
// This says so by name, and a caller tells it apart from a platform that
// simply cannot.
func desktopAvailable() error {
	return ErrNeedsAgent
}
