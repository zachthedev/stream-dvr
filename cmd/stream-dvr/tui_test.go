package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"zach.tools/go/stream-dvr/internal/notify"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/service"
	"zach.tools/go/stream-dvr/internal/store"
	"zach.tools/go/stream-dvr/internal/tui"
)

// ///////////////////////////////////////////////
// Fakes
// ///////////////////////////////////////////////

// fakeManager is a service.Manager that records what it was asked to do.
type fakeManager struct {
	status    service.Status
	statusErr error
	started   int
	stopped   int
}

func (f *fakeManager) Install(service.Definition) error { return nil }
func (f *fakeManager) Uninstall(string) error           { return nil }
func (f *fakeManager) Mechanism() string                { return "fake" }

func (f *fakeManager) Start(string) error {
	f.started++
	return nil
}

func (f *fakeManager) Stop(string) error {
	f.stopped++
	return nil
}

func (f *fakeManager) Status(string) (service.Status, error) {
	return f.status, f.statusErr
}

// ///////////////////////////////////////////////
// controllerAdapter
// ///////////////////////////////////////////////

func TestControllerAdapter_TranslatesStatus(t *testing.T) {
	manager := &fakeManager{
		status: service.Status{State: service.StateRunning, Detail: "stream-dvr"},
	}
	adapter := controllerAdapter{manager: manager}

	got, err := adapter.Status("stream-dvr")
	if err != nil {
		t.Fatalf("Status() err = %v, want nil", err)
	}
	if got.State != string(service.StateRunning) {
		t.Errorf("State = %q, want %q", got.State, service.StateRunning)
	}
	if got.Detail != "stream-dvr" {
		t.Errorf("Detail = %q, want %q", got.Detail, "stream-dvr")
	}
}

func TestControllerAdapter_PropagatesAStatusFailure(t *testing.T) {
	manager := &fakeManager{statusErr: errors.New("scheduler unreachable")}
	adapter := controllerAdapter{manager: manager}

	if _, err := adapter.Status("stream-dvr"); err == nil {
		t.Error("Status() err = nil, want the failure propagated")
	}
}

func TestControllerAdapter_ForwardsControl(t *testing.T) {
	manager := &fakeManager{}
	adapter := controllerAdapter{manager: manager}

	if err := adapter.Start("stream-dvr"); err != nil {
		t.Fatalf("Start() err = %v, want nil", err)
	}
	if err := adapter.Stop("stream-dvr"); err != nil {
		t.Fatalf("Stop() err = %v, want nil", err)
	}
	if manager.started != 1 || manager.stopped != 1 {
		t.Errorf("started/stopped = %d/%d, want 1/1", manager.started, manager.stopped)
	}
}

func TestControllerAdapter_SatisfiesTheModelsInterface(t *testing.T) {
	// The model owns the interface, so this is what stops the adapter
	// drifting away from it.
	var _ tui.Controller = controllerAdapter{manager: &fakeManager{}}
}

// ///////////////////////////////////////////////
// libraryAdapter
// ///////////////////////////////////////////////

func TestLibraryAdapter_SatisfiesTheModelsInterface(t *testing.T) {
	var _ tui.Library = &libraryAdapter{}
}

func TestLibraryAdapter_ReportsAMissingDatabaseWithoutCreatingOne(t *testing.T) {
	// The calendar is what a new install opens first. A library with no
	// database yet is a state to report, not a reason to refuse to start.
	root := t.TempDir()
	adapter := &libraryAdapter{path: filepath.Join(root, "library.db"), root: root}
	defer adapter.Close()

	_, err := adapter.Channels()
	if err == nil {
		t.Fatal("Channels() err = nil, want a missing database reported")
	}
	if !errors.Is(err, store.ErrNoDatabase) {
		t.Errorf("Channels() err = %v, want ErrNoDatabase", err)
	}
	if !strings.Contains(err.Error(), "stream-dvr serve") {
		t.Errorf("Channels() err = %v, want it to name the command that fixes it", err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	if len(entries) != 0 {
		t.Errorf("the adapter left %d entries in the library, want none", len(entries))
	}
}

func TestLibraryAdapter_RetriesAnOpenThatFailed(t *testing.T) {
	// r is what an operator presses after starting the recorder. Caching the
	// failure would make the calendar stay empty until it was restarted.
	root := t.TempDir()
	path := filepath.Join(root, "library.db")
	adapter := &libraryAdapter{path: path, root: root}
	defer adapter.Close()

	if _, err := adapter.Channels(); err == nil {
		t.Fatal("Channels() err = nil, want a missing database reported")
	}

	created, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	created.Close()

	if _, err := adapter.Channels(); err != nil {
		t.Errorf("Channels() err = %v after the recorder created the database, want nil", err)
	}
}

func TestLibraryAdapter_OpensOnce(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "library.db")
	created, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	created.Close()

	adapter := &libraryAdapter{path: path, root: root}
	defer adapter.Close()

	if _, err := adapter.Channels(); err != nil {
		t.Fatalf("Channels() err = %v, want nil", err)
	}
	first := adapter.store

	if _, err := adapter.Channels(); err != nil {
		t.Fatalf("Channels() err = %v, want nil", err)
	}
	if adapter.store != first {
		t.Error("the adapter reopened the database, so every refresh leaks a handle")
	}
}

func TestLibraryAdapter_CloseWithoutAnOpenIsHarmless(t *testing.T) {
	// runTUI defers Close, and a calendar quit before anything asked for the
	// database never opened one.
	adapter := &libraryAdapter{path: filepath.Join(t.TempDir(), "library.db")}

	if err := adapter.Close(); err != nil {
		t.Errorf("Close() err = %v, want nil", err)
	}
}

func TestLibraryAdvice_NamesWhatToDo(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "no database yet",
			err:  fmt.Errorf("opening database: %w", store.ErrNoDatabase),
			want: "run 'stream-dvr serve' once to create it",
		},
		{
			name: "a library from a newer build",
			err:  &store.SchemaMismatchError{Want: 1, Got: 2},
			want: "upgrade this one",
		},
		{
			name: "a library this build would migrate",
			err:  &store.SchemaMismatchError{Want: 2, Got: 1},
			want: "run 'stream-dvr serve' once to migrate it",
		},
		{
			name: "anything else passes through",
			err:  errors.New("disk went away"),
			want: "disk went away",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := libraryAdvice(tc.err)

			if !strings.Contains(got.Error(), tc.want) {
				t.Errorf("libraryAdvice() = %v, want it to name %q", got, tc.want)
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("libraryAdvice() dropped the cause it was given: %v", got)
			}
		})
	}
}

// ///////////////////////////////////////////////
// inProcessRecorder
// ///////////////////////////////////////////////

func TestInProcessRecorder_SatisfiesTheModelsInterface(t *testing.T) {
	var _ tui.Recorder = &inProcessRecorder{}
}

func TestInProcessRecorder_ReportsAStartItCouldNotMake(t *testing.T) {
	// The config is read at Start rather than at construction, so a config
	// the operator has just broken is refused here and not swallowed.
	recorder := &inProcessRecorder{
		configPath: filepath.Join(t.TempDir(), "absent.toml"),
		purge:      &purgeAdapter{},
	}

	err := recorder.Start()

	if err == nil {
		t.Fatal("Start() err = nil, want the missing config reported")
	}
	if recorder.Running() {
		t.Error("Running() = true after a start that failed")
	}
	if err := recorder.Stop(); err != nil {
		t.Errorf("Stop() err = %v after a start that failed, want nil", err)
	}
}

func TestInProcessRecorder_StopWithoutAStartIsHarmless(t *testing.T) {
	// runTUI defers Stop, and a calendar quit with no recorder never
	// started one.
	recorder := &inProcessRecorder{purge: &purgeAdapter{}}

	if err := recorder.Stop(); err != nil {
		t.Errorf("Stop() err = %v, want nil", err)
	}
	if recorder.Running() {
		t.Error("Running() = true with nothing ever started")
	}
}

func TestPurgeAdapter_RoutesThroughARunningRecordersOrganizer(t *testing.T) {
	// The lock that keeps a purge and a sweep off the same recording is
	// held per organizer, so both have to go through one while a recorder
	// runs in this process.
	root := t.TempDir()
	path := filepath.Join(root, "library.db")
	created, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	created.Close()

	reader := &libraryAdapter{path: path, root: root}
	defer reader.Close()

	adapter := &purgeAdapter{
		reader: reader,
		build:  func(*store.Store) *organize.Organizer { return nil },
	}

	shared := &organize.Organizer{}
	adapter.useOrganizer(shared)
	got, err := adapter.organize()
	if err != nil {
		t.Fatalf("organize() err = %v, want nil", err)
	}
	if got != shared {
		t.Error("a purge did not route through the running recorder's organizer")
	}

	adapter.useOrganizer(nil)
	if got, _ := adapter.organize(); got == shared {
		t.Error("a purge still routes through an organizer whose recorder stopped")
	}
}

// ///////////////////////////////////////////////
// serviceController
// ///////////////////////////////////////////////

func TestServiceController_IsUsableOnThisPlatform(t *testing.T) {
	// A missing manager is not fatal: the calendar is still worth reading
	// on a machine where the recorder was never registered. It just has to
	// be nil rather than a broken value.
	controller := serviceController()
	if controller == nil {
		t.Skip("no service manager on this platform")
	}

	if _, err := controller.Status(serviceName); err != nil {
		t.Errorf("Status() err = %v, want nil", err)
	}
}

// ///////////////////////////////////////////////
// socketFeed
// ///////////////////////////////////////////////

// socketEvent is the event these tests deliver. Every field is populated, so
// one dropped on the way to the pane shows up as a difference.
func socketEvent() notify.Event {
	return notify.Event{
		Kind:    "recording_started",
		Channel: "examplechannel",
		Title:   "a broadcast title",
		Detail:  "1080p60",
		At:      time.Date(2026, time.March, 2, 9, 30, 0, 0, time.UTC),
	}
}

func TestSocketFeed_ShowsWhatAnotherProcessReported(t *testing.T) {
	// The point of following the socket. A recorder installed as a service
	// knows nothing about this window, and its events still reach the pane.
	feed := make(chan tui.FeedEvent, 1)
	socketFeed{feed: feed, running: func() bool { return false }}.deliver(socketEvent())

	select {
	case got := <-feed:
		want := tui.FeedEvent{
			Kind:    "recording_started",
			Channel: "examplechannel",
			Detail:  "1080p60",
			At:      time.Date(2026, time.March, 2, 9, 30, 0, 0, time.UTC),
		}
		if got != want {
			t.Errorf("delivered %+v, want %+v", got, want)
		}
	default:
		t.Fatal("nothing reached the pane, want the event")
	}
}

func TestSocketFeed_CarriesTheRecordersOwnTimestamp(t *testing.T) {
	// The event crossed a socket and may have waited on a reconnect, so the
	// moment it arrived is not the moment it happened. Stamping it here would
	// put a recording an hour old at the top of the pane as if it were now.
	feed := make(chan tui.FeedEvent, 1)
	event := socketEvent()
	socketFeed{feed: feed, running: func() bool { return false }}.deliver(event)

	got := <-feed
	if !got.At.Equal(event.At) {
		t.Errorf("At = %v, want the recorder's %v", got.At, event.At)
	}
}

func TestSocketFeed_ShowsNothingTwiceWhileThisWindowRecords(t *testing.T) {
	// A recorder in this window publishes to that same socket, so during an
	// in-process run every line would arrive once through feedNotifier and
	// once back through here.
	feed := make(chan tui.FeedEvent, 1)
	socketFeed{feed: feed, running: func() bool { return true }}.deliver(socketEvent())

	select {
	case got := <-feed:
		t.Errorf("delivered %+v while this window records, want it dropped", got)
	default:
	}
}

func TestSocketFeed_ClipsWhatAPeerCanPutOnOneLine(t *testing.T) {
	// Anything running as this operator may write to that socket, and the
	// bus allows a 64KB line. Unclipped, one event wraps over the calendar
	// and stays there for the session, because the pane keeps what it was
	// told.
	feed := make(chan tui.FeedEvent, 1)
	event := socketEvent()
	event.Detail = strings.Repeat("x", 64<<10)
	socketFeed{feed: feed, running: func() bool { return false }}.deliver(event)

	got := <-feed
	if len(got.Detail) > maxFeedField {
		t.Errorf("Detail is %d bytes, want at most %d", len(got.Detail), maxFeedField)
	}
}

func TestClipField_CutsOnARuneBound(t *testing.T) {
	// escape.Text quotes what it is given, so half a rune reaching it
	// renders as an escape rather than as the character it came from.
	clipped := clipField(strings.Repeat("é", maxFeedField))
	if !utf8.ValidString(clipped) {
		t.Errorf("clipField produced invalid UTF-8: %q", clipped)
	}
	if len(clipped) > maxFeedField {
		t.Errorf("clipField returned %d bytes, want at most %d", len(clipped), maxFeedField)
	}
}

func TestClipField_LeavesAFieldThatAlreadyFits(t *testing.T) {
	// The ordinary case, and the one a clip must not touch: a broadcast
	// title is far shorter than the bound.
	title := "a perfectly ordinary broadcast title"
	if got := clipField(title); got != title {
		t.Errorf("clipField(%q) = %q, want it unchanged", title, got)
	}
}

func TestSocketFeed_DropsRatherThanWaitingOnAFullPane(t *testing.T) {
	// deliver runs on the goroutine reading the socket. Blocking here stops
	// that read, and a subscriber that stopped reading is what the bus
	// disconnects on its write deadline.
	feed := make(chan tui.FeedEvent) // Unbuffered, so nothing can be queued.
	done := make(chan struct{})

	go func() {
		defer close(done)
		socketFeed{feed: feed, running: func() bool { return false }}.deliver(socketEvent())
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deliver is still waiting on a pane nobody is reading, want it dropped")
	}
}
