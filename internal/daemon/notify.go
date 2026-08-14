package daemon

import (
	"context"
	"errors"
	"log/slog"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// EventKind classifies a notification.
type EventKind string

// Event is one notification.
type Event struct {
	// Kind classifies the event.
	Kind EventKind
	// Channel is the channel involved, when there is one.
	Channel string
	// Title is the broadcast title, when known.
	Title string
	// Detail explains what happened.
	Detail string
}

// Notifier delivers events.
//
// An implementation must not block for long: it runs on the recording
// path, and a slow webhook must not delay a capture.
type Notifier interface {
	Notify(ctx context.Context, event Event) error
}

// ///////////////////////////////////////////////
// Implementations
// ///////////////////////////////////////////////

// DiscardNotifier drops every event. It is the default, so a daemon built
// without a notifier still runs.
type DiscardNotifier struct{}

// LogNotifier writes events to a logger. Useful when no sink is configured
// but the events must still land somewhere readable.
type LogNotifier struct {
	Logger *slog.Logger
}

// Notifiers delivers one event to every sink behind it.
//
// A sink that fails does not stop the ones after it: the point of having
// several is that they fail apart.
type Notifiers []Notifier

// Event kinds.
const (
	// EventRecordingStarted fires when a capture begins.
	EventRecordingStarted EventKind = "recording_started"
	// EventFailure fires when a capture or post-processing step fails.
	EventFailure EventKind = "failure"
	// EventLibraryFull fires when the budget refuses a recording. This is
	// the only event that costs a broadcast.
	EventLibraryFull EventKind = "library_full"
	// EventDowntime fires when the daemon discovers it was not running.
	EventDowntime EventKind = "downtime"
	// EventRecovered fires when backfill fetches a broadcast the recorder
	// missed whole.
	EventRecovered EventKind = "recovered"
	// EventGapFilled fires when backfill patches a hole inside a broadcast
	// that was captured.
	EventGapFilled EventKind = "gap_filled"
	// EventCredentialDead fires when the stored credential stops working.
	// Nothing recorded is lost by it, but everything recorded afterwards is
	// worse, which is exactly the silent degradation this reports.
	//nolint:gosec // G101: an event name, not a credential.
	EventCredentialDead EventKind = "credential_dead"
	// EventFetchGaveUp fires when a broadcast spends its attempts or fails
	// permanently. It is the one backfill event that will not resolve on
	// its own, so it is the one worth telling an operator about.
	EventFetchGaveUp EventKind = "fetch_gave_up"
)

// ErrCredentialRejected reports a credential the provider refused.
//
// It is the one credential outcome the operator is told about. A network
// failure means the question could not be put, and concluding a token is
// dead from that would have an outage delete a working one.
var ErrCredentialRejected = errors.New("the provider rejected the stored credential")

// ErrCredentialAbsent reports that nothing is stored to check.
//
// It is told apart from a credential that validates because the two mean
// opposite things to a recorder that has already seen a rejection: the
// rejection deletes the file, so from then on the same dead credential looks
// exactly like a fresh install, and only a token that positively validates
// says the condition is over.
var ErrCredentialAbsent = errors.New("no credential is stored")

// Notify implements Notifier.
func (DiscardNotifier) Notify(context.Context, Event) error { return nil }

// Notify implements Notifier.
func (n LogNotifier) Notify(ctx context.Context, event Event) error {
	logger := n.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "dvr event",
		slog.String("kind", string(event.Kind)),
		slog.String("channel", event.Channel),
		slog.String("title", event.Title),
		slog.String("detail", event.Detail))
	return nil
}

// Notify implements Notifier.
func (n Notifiers) Notify(ctx context.Context, event Event) error {
	var failures []error

	for _, sink := range n {
		if sink == nil {
			continue
		}
		if err := sink.Notify(ctx, event); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
