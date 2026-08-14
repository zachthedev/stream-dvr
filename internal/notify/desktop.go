package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"zach.tools/go/stream-dvr/internal/escape"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Desktop raises a native notification in the operator's session.
//
// Where the recorder runs decides whether this can work at all, and the
// answer differs per platform rather than per configuration:
//
//   - macOS: the recorder is a launchd agent in the gui domain, so it runs
//     inside the session and posts its own notification.
//   - Linux: the recorder is a lingering systemd user unit sharing the one
//     per-user bus with the desktop, so it calls the notification service
//     directly. With nobody signed in the name has no owner and the event
//     is dropped, which is the right answer.
//   - Windows: the recorder is a scheduled task in session 0 and cannot
//     reach the desktop at all. It also cannot start a helper that could:
//     placing a process in another session needs SE_TCB_NAME, which a
//     per-user task does not have. The helper has to be started by the
//     session instead, which is the notify agent.
type Desktop struct {
	logger *slog.Logger
}

// ///////////////////////////////////////////////
// Limits
// ///////////////////////////////////////////////

// Every platform truncates a long notification somewhere, and none of them
// says where. Cutting here means one rule rather than three surprises, and
// it keeps a multi-hour stream title from filling the screen.
//
// The sizes come from the tightest field any platform offers, which is
// Windows: 64 UTF-16 units for the title and 256 for the body, each
// including a terminator.
//
// The body counts runes and the field counts UTF-16 units, and a rune
// outside the basic plane costs two of them. A stream title is whatever
// the streamer typed, emoji included, so the body is held to half the
// field and fits even when every rune is a surrogate pair. The title is
// composed here from a fixed set of English phrases, so its runes and
// units are the same thing and it uses the field as it stands.
const (
	maxTitle = 63
	maxBody  = 127
)

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

// ErrNeedsAgent reports that this platform raises notifications from a
// separate process in the operator's session rather than from the recorder.
//
// It is not a misconfiguration, and a caller distinguishes it so that the
// ordinary Windows arrangement is not announced as a fault. The recorder
// publishes on the bus instead, and the agent raises what it reads.
var ErrNeedsAgent = errors.New("this platform raises notifications from the notify agent")

// ///////////////////////////////////////////////
// Lifecycle
// ///////////////////////////////////////////////

// NewDesktop returns a sink for this platform, or reports why there is
// none.
//
// It refuses at construction rather than at the first event, so an operator
// who turned desktop notifications on finds out from doctor or from the
// daemon's first log line instead of from a broadcast nobody told them
// about.
func NewDesktop(logger *slog.Logger) (*Desktop, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := desktopAvailable(); err != nil {
		return nil, err
	}
	return &Desktop{logger: logger}, nil
}

// NewSessionDesktop returns a sink for a process already running inside
// the operator's session.
//
// The notify agent is that process. NewDesktop asks whether the recorder
// can raise one from where its platform starts it, which on Windows is
// session 0 and cannot. The agent is started by the session instead, so
// that question does not apply to it and only "can this platform raise a
// notification at all" remains.
func NewSessionDesktop(logger *slog.Logger) (*Desktop, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := sessionAvailable(); err != nil {
		return nil, err
	}
	return &Desktop{logger: logger}, nil
}

// Close releases whatever the platform holds open to raise notifications.
//
// On Windows that is a tray icon, which outlives the process as a ghost
// until something hovers over it if it is not removed.
func (d *Desktop) Close() error { return closeDesktop() }

// Notify implements a sink.
//
// A notification nobody is there to see is not a failure. The desktop is a
// convenience on top of the log and the webhook, so a session that has gone
// away is reported once and never again escalated.
func (d *Desktop) Notify(ctx context.Context, event Event) error {
	title, body := render(event)
	if err := raise(ctx, title, body); err != nil {
		return fmt.Errorf("raising a desktop notification: %w", err)
	}
	return nil
}

// render turns an event into the two strings every platform wants.
//
// Both halves go through escape.Text first. A stream title is written by
// whoever was streaming, and it reaches a surface that renders text.
func render(event Event) (title, body string) {
	title = "stream-dvr: " + summarize(event.Kind)

	parts := make([]string, 0, 2)
	if event.Channel != "" {
		parts = append(parts, event.Channel)
	}
	if event.Detail != "" {
		parts = append(parts, event.Detail)
	} else if event.Title != "" {
		parts = append(parts, event.Title)
	}

	return clip(escape.Text(title), maxTitle), clip(escape.Text(strings.Join(parts, ": ")), maxBody)
}

// summarize names an event kind in the operator's words.
//
// An unknown kind renders as itself rather than as nothing, so an event
// added to the daemon still reaches the desktop before this list catches up.
func summarize(kind string) string {
	switch kind {
	case "recording_started":
		return "recording started"
	case "failure":
		return "something failed"
	case "library_full":
		return "the library is full"
	case "downtime":
		return "the recorder was not running"
	default:
		return kind
	}
}

// clip shortens text to a rune bound, marking that it did.
func clip(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

// HideConsole hides a console this process was given rather than asked
// for, which is what starting from a Windows Run key does. It does nothing
// on the platforms that do not.
func HideConsole() { hideConsole() }
