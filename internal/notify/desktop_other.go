//go:build !windows && !linux && !darwin

package notify

import (
	"context"
	"errors"
	"runtime"
)

// ///////////////////////////////////////////////
// Platform
// ///////////////////////////////////////////////

// desktopAvailable reports that this build has no way to raise one.
//
// It refuses by name rather than silently doing nothing, the way the
// service package refuses an unsupported platform. An operator on a system
// nobody planned for must be told, not left waiting for a notification that
// was never coming.
func desktopAvailable() error {
	return errors.New("desktop notifications are not supported on " + runtime.GOOS)
}

// raise is unreachable: NewDesktop refuses before a sink exists.
func raise(context.Context, string, string) error {
	return desktopAvailable()
}

// sessionAvailable reports that this build has no way to raise one,
// wherever the process runs.
func sessionAvailable() error { return desktopAvailable() }

// closeDesktop releases nothing, because nothing was ever opened.
func closeDesktop() error { return nil }

// hideConsole does nothing here. Only Windows hands a console to a program
// that never asked for one.
func hideConsole() {}
