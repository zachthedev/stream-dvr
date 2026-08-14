package notify

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/godbus/dbus/v5"
)

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// The freedesktop notification service, which every desktop environment
// worth the name owns on the session bus.
const (
	notifyService = "org.freedesktop.Notifications"
	notifyPath    = "/org/freedesktop/Notifications"
	notifyMethod  = "org.freedesktop.Notifications.Notify"
)

// notifyTimeoutMS is how long the notification stays up. Minus one asks for
// the server's own default, which is what an operator configured in their
// desktop settings.
const notifyTimeoutMS = -1

// ///////////////////////////////////////////////
// Platform
// ///////////////////////////////////////////////

// desktopAvailable reports whether a session bus is reachable.
//
// The recorder is a lingering systemd user unit, so it shares the one
// per-user bus with the desktop and can reach the notification service
// directly. What it cannot do is invent a bus: on a machine with no
// graphical session there is nothing to connect to, and that is the answer
// rather than a fault.
func desktopAvailable() error {
	connection, err := sessionBus()
	if err != nil {
		return err
	}
	defer connection.Close()
	return nil
}

// sessionBus connects to the per-user bus.
//
// DBUS_SESSION_BUS_ADDRESS is normally exported into the user manager's
// environment, but a session that never ran dbus-update-activation-environment
// leaves it unset. The socket is at a known path under XDG_RUNTIME_DIR, so
// that is the fallback rather than a failure.
func sessionBus() (*dbus.Conn, error) {
	if connection, err := dbus.SessionBus(); err == nil {
		return connection, nil
	}

	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		return nil, errors.New("no session bus is reachable, so no desktop notification can be raised")
	}

	connection, err := dbus.Connect("unix:path=" + runtime + "/bus")
	if err != nil {
		return nil, errors.New("no session bus is reachable, so no desktop notification can be raised")
	}
	return connection, nil
}

// raise posts one notification over the session bus.
//
// Nothing here is quoted or escaped for a shell, because there is no shell:
// the values are typed strings in a marshalled message. That is the whole
// reason this goes over the bus rather than through notify-send.
func raise(ctx context.Context, title, body string) error {
	connection, err := sessionBus()
	if err != nil {
		return err
	}
	defer connection.Close()

	call := connection.Object(notifyService, notifyPath).CallWithContext(
		ctx, notifyMethod, 0,
		"stream-dvr", // the application name
		uint32(0),    // replaces nothing
		"",           // no icon
		title, body,
		[]string{}, // no actions
		map[string]dbus.Variant{},
		int32(notifyTimeoutMS))
	if call.Err != nil {
		return fmt.Errorf("the notification service: %w", call.Err)
	}
	return nil
}

// sessionAvailable reports whether this process can raise a notification.
//
// The recorder already runs inside the session here, so the agent asks the
// same question it does and gets the same answer.
func sessionAvailable() error { return desktopAvailable() }

// closeDesktop releases nothing: a notification here is one call, with no
// handle held between them.
func closeDesktop() error { return nil }

// hideConsole does nothing here. Only Windows hands a console to a program
// that never asked for one.
func hideConsole() {}
