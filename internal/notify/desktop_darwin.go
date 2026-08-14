package notify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"zach.tools/go/stream-dvr/internal/escape"
)

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// notifyScript posts a notification, taking its text from the arguments.
//
// The text never enters the script. Building the AppleScript by
// interpolating a title would make a quote or a backslash in a stream title
// into script the operator did not write, and this project takes every
// stream title as text a stranger chose. The script bytes are a constant in
// this binary and the only variable part is argv.
const notifyScript = `on run argv
	display notification (item 2 of argv) with title (item 1 of argv)
end run`

// osascriptTimeout bounds one call. It runs on a notification, which is
// already off the recording path, but a wedged osascript would otherwise
// hold the sender goroutine for good.
const osascriptTimeout = 10 * time.Second

// ///////////////////////////////////////////////
// Platform
// ///////////////////////////////////////////////

// desktopAvailable reports whether this Mac can raise a notification.
//
// The recorder is a launchd agent in the gui domain, so it is inside the
// session whenever it is running at all. What it needs is osascript, which
// ships with macOS and is only absent on a system somebody has taken apart.
func desktopAvailable() error {
	if _, err := exec.LookPath("osascript"); err != nil {
		return errors.New("osascript is not on PATH, so no desktop notification can be raised")
	}
	return nil
}

// raise posts one notification through osascript.
//
// The script arrives on stdin and the text arrives as operands, so nothing
// a stream titled itself can reach the parser.
func raise(ctx context.Context, title, body string) error {
	ctx, cancel := context.WithTimeout(ctx, osascriptTimeout)
	defer cancel()

	// G204 names remote text reaching a subprocess, and a broadcast title is
	// exactly that. It is safe here because "-" is the program file, so
	// osascript stops parsing options at it and hands everything after it to
	// the script as arguments. A title leading with a dash is an argument,
	// not an option, which is why this refuses nothing: a stream may
	// legitimately be titled that way and dropping the notification would be
	// the worse answer.
	command := exec.CommandContext(ctx, "osascript", "-", title, body) //nolint:gosec // G204: "-" ends option parsing, so the title and body reach the script
	command.Stdin = strings.NewReader(notifyScript)

	output, err := command.CombinedOutput()
	if err != nil {
		// The output can name the reason, such as notification permission
		// being declined, and it is not text this process wrote.
		return fmt.Errorf("osascript: %w: %s", err, escape.Field(string(output)))
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
