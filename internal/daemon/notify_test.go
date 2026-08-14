package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

// failingNotifier always fails, standing in for an unreachable webhook.
type failingNotifier struct{}

// discardLogger returns a logger that writes nothing.
func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func (failingNotifier) Notify(context.Context, Event) error {
	return errors.New("webhook unreachable")
}

// ///////////////////////////////////////////////
// Implementations
// ///////////////////////////////////////////////

func TestDiscardNotifier(t *testing.T) {
	// The default notifier exists so a daemon built without one still runs.
	if err := (DiscardNotifier{}).Notify(context.Background(), Event{Kind: EventFailure}); err != nil {
		t.Errorf("Notify() err = %v, want nil", err)
	}
}

func TestLogNotifier(t *testing.T) {
	tests := []struct {
		name     string
		notifier LogNotifier
	}{
		{name: "with a logger", notifier: LogNotifier{Logger: discardLogger()}},
		{name: "without a logger falls back to the default", notifier: LogNotifier{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.notifier.Notify(context.Background(), Event{
				Kind: EventLibraryFull, Channel: "examplechannel", Detail: "full",
			})
			if err != nil {
				t.Errorf("Notify() err = %v, want nil", err)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Event kinds
// ///////////////////////////////////////////////

func TestEvent_KindsAreDistinct(t *testing.T) {
	// Each kind maps to its own configuration switch, so a duplicate value
	// would silently tie two settings together.
	kinds := []EventKind{
		EventRecordingStarted, EventFailure, EventLibraryFull, EventDowntime,
	}

	seen := make(map[EventKind]bool, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			t.Error("an event kind is empty")
		}
		if seen[kind] {
			t.Errorf("event kind %q is duplicated", kind)
		}
		seen[kind] = true
	}
}
