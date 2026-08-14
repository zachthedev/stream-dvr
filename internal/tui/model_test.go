package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Fakes
// ///////////////////////////////////////////////

// fakeLibrary serves scripted coverage without a database.
type fakeLibrary struct {
	channels   []store.Channel
	days       []store.Day
	recordings []store.Recording

	space Space
	gaps  map[int64][]store.Gap

	channelsErr error
	coverageErr error
	spaceErr    error

	lastChannelID int64
	lastFrom      time.Time
	lastTo        time.Time
	coverageCalls int
}

// fakeActions records writes and applies them to the library behind it, so
// a refresh reads back what a write stored rather than what the test
// assumed.
type fakeActions struct {
	library *fakeLibrary

	watchedID    int64
	watchedAt    *time.Time
	watchedCalls int

	pinnedID int64
	pinned   bool

	err error
}

// fakeRecorder stands in for a daemon running inside this process.
type fakeRecorder struct {
	running  bool
	starts   int
	stops    int
	startErr error
	runErr   error
}

// fakeRecovery records what a recovery request named.
type fakeRecovery struct {
	channelID int64
	day       time.Time
	calls     int
	err       error
}

// fakeController records start, stop, and status calls.
//
// Status is counted because every query reaches the platform's service
// manager on the real controller, so how often the model asks is part of
// its contract.
type fakeController struct {
	status  Status
	starts  int
	stops   int
	queries int

	failErr   error
	statusErr error
	block     chan struct{}
}

// ///////////////////////////////////////////////
// Harness
// ///////////////////////////////////////////////

// awaitTimeout is how long the harness waits for one command before
// treating it as still in flight. Long enough that a slow machine does not
// make a prompt command look pending, short enough that a suite of
// subscriptions costs a moment rather than a minute.
const awaitTimeout = 200 * time.Millisecond

// fixedNow is the clock every test runs against.
var fixedNow = time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

// The reads all clone. The real store scans fresh rows every query, so a
// fake that handed out its own storage would let a write reach the model
// without a refresh and certify a missing reload as present.

func (f *fakeLibrary) Channels() ([]store.Channel, error) {
	return slices.Clone(f.channels), f.channelsErr
}

func (f *fakeLibrary) CoverageBetween(channelID int64, from, to time.Time, _ *time.Location) ([]store.Day, error) {
	f.coverageCalls++
	f.lastChannelID = channelID
	f.lastFrom, f.lastTo = from, to
	return slices.Clone(f.days), f.coverageErr
}

func (f *fakeLibrary) RecordingsForChannel(int64, time.Time, time.Time) ([]store.Recording, error) {
	return slices.Clone(f.recordings), nil
}

func (f *fakeLibrary) SpaceUsage() (Space, error) {
	return f.space, f.spaceErr
}

// GapsFor implements Library.
func (f *fakeLibrary) GapsFor(recordingID int64) ([]store.Gap, error) {
	return f.gaps[recordingID], nil
}

// setRecording applies a change to a stored recording, so a write made
// through fakeActions is what the next refresh reads back.
func (f *fakeLibrary) setRecording(id int64, apply func(*store.Recording)) {
	for i := range f.recordings {
		if f.recordings[i].ID == id {
			apply(&f.recordings[i])
			return
		}
	}
}

func (f *fakeActions) MarkWatched(id int64, at *time.Time) error {
	f.watchedCalls++
	f.watchedID, f.watchedAt = id, at
	if f.err != nil {
		return f.err
	}

	f.library.setRecording(id, func(r *store.Recording) { r.WatchedAt = at })
	return nil
}

func (f *fakeRecorder) Start() error {
	if f.startErr != nil {
		return f.startErr
	}

	f.starts++
	f.running = true
	return nil
}

func (f *fakeRecorder) Stop() error {
	f.stops++
	f.running = false
	return nil
}

func (f *fakeRecorder) Running() bool { return f.running }
func (f *fakeRecorder) Err() error    { return f.runErr }

func (f *fakeRecovery) Request(channelID int64, day time.Time) error {
	f.calls++
	f.channelID, f.day = channelID, day
	return f.err
}

func (f *fakeActions) SetPinned(id int64, pinned bool) error {
	f.pinnedID, f.pinned = id, pinned
	if f.err != nil {
		return f.err
	}

	f.library.setRecording(id, func(r *store.Recording) { r.Pinned = pinned })
	return nil
}

func (f *fakeController) Start(string) error {
	f.starts++
	f.wait()
	if f.failErr != nil {
		return f.failErr
	}
	f.status.State = "running"
	return nil
}

func (f *fakeController) Stop(string) error {
	f.stops++
	f.wait()
	if f.failErr != nil {
		return f.failErr
	}
	f.status.State = "installed"
	return nil
}

func (f *fakeController) Status(string) (Status, error) {
	f.queries++
	if f.statusErr != nil {
		return Status{}, f.statusErr
	}
	return f.status, nil
}

// wait holds a control call open, standing in for the seconds a service
// manager can take.
func (f *fakeController) wait() {
	if f.block != nil {
		<-f.block
	}
}

// holdControl makes every start and stop block for a while, standing in for
// the seconds a service manager can take.
//
// The latch opens on a deadline rather than on a call from the test. A build
// that runs the control inside Update stalls there, so a test that had to
// reach a close() would deadlock instead of reporting the defect.
func holdControl(t *testing.T, controller *fakeController) {
	t.Helper()

	controller.block = make(chan struct{})
	timer := time.AfterFunc(50*time.Millisecond, func() { close(controller.block) })
	t.Cleanup(func() { timer.Stop() })
}

// newModel builds a loaded model over a fake library.
func newModel(t *testing.T, library *fakeLibrary, controller Controller) *Model {
	t.Helper()

	return newOptionsModel(t, Options{Library: library, Controller: controller})
}

// newActingModel returns a drained model whose library accepts writes.
func newActingModel(t *testing.T, library *fakeLibrary, actions Actions) *Model {
	t.Helper()

	return newOptionsModel(t, Options{Library: library, Actions: actions})
}

// newOptionsModel fills in the fixtures every test shares and drains Init,
// so the clock and the timezone have one definition rather than one per
// constructor.
func newOptionsModel(t *testing.T, opts Options) *Model {
	t.Helper()

	opts.ServiceKey = "stream-dvr"
	opts.Location = time.UTC
	opts.WeekStart = time.Sunday
	opts.Now = func() time.Time { return fixedNow }

	model := New(opts)
	// The screen is laid out from the terminal's size, and a model never
	// told one has nothing to draw inside. This is the size the design was
	// drawn at.
	model.width, model.height = 120, 40
	drain(t, model, model.Init())
	return model
}

// drain runs a command and feeds its message back, which is what the Bubble
// Tea runtime does, including unpacking a batch into its members.
func drain(t *testing.T, model *Model, cmd tea.Cmd) {
	t.Helper()

	pending := []tea.Cmd{cmd}
	for step := 0; step < 16 && len(pending) > 0; step++ {
		next := pending[0]
		pending = pending[1:]
		if next == nil {
			continue
		}

		message, answered := await(next)
		if !answered {
			continue
		}

		switch msg := message.(type) {
		case nil:
		case tea.BatchMsg:
			pending = append(pending, msg...)
		default:
			_, follow := model.Update(msg)
			pending = append(pending, follow)
		}
	}
}

// await runs a command off this goroutine and reports whether it answered.
//
// The runtime runs every command in its own goroutine, so a command that
// waits is in flight rather than stuck. The feed subscription is exactly
// that: it blocks until the recorder says something, which in most tests is
// never. Running it inline would hang the suite instead of leaving it
// pending the way the runtime does.
func await(cmd tea.Cmd) (tea.Msg, bool) {
	answer := make(chan tea.Msg, 1)
	go func() { answer <- cmd() }()

	select {
	case msg := <-answer:
		return msg, true
	case <-time.After(awaitTimeout):
		return nil, false
	}
}

// keyPress builds the press a terminal reports for a printable character.
//
// Code carries the character and Text what it produced, which is the pair
// that separates a typed "s" from ctrl+s arriving on the same physical key.
func keyPress(text string) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: []rune(text)[0], Text: text}
}

// press sends a key and drains any command it produces.
func press(t *testing.T, model *Model, key string) {
	t.Helper()

	_, cmd := model.Update(keyPress(key))
	drain(t, model, cmd)
}

// pressNamed sends a named key such as "left".
func pressNamed(t *testing.T, model *Model, key rune) {
	t.Helper()

	_, cmd := model.Update(tea.KeyPressMsg{Code: key})
	drain(t, model, cmd)
}

// library returns a fake with one channel.
func library() *fakeLibrary {
	return &fakeLibrary{
		channels: []store.Channel{
			{ID: 1, Platform: "twitch", Name: "examplechannel", DisplayName: "ExampleChannel"},
		},
	}
}

// dayLibrary returns a fake holding two recordings on the selected day, so
// the day pane has a cursor with somewhere to move.
func dayLibrary() *fakeLibrary {
	lib := library()
	lib.recordings = []store.Recording{
		{ID: 1, Path: "first.mkv", StartedAt: fixedNow.Add(-2 * time.Hour), Bytes: 1 << 30},
		{ID: 2, Path: "second.mkv", StartedAt: fixedNow.Add(-time.Hour), Bytes: 2 << 30},
	}
	return lib
}

// ///////////////////////////////////////////////
// Startup
// ///////////////////////////////////////////////

func TestNew_OpensOnToday(t *testing.T) {
	model := newModel(t, library(), nil)

	if got := model.Cursor().Format("2006-01-02"); got != "2026-03-04" {
		t.Errorf("Cursor() = %s, want today", got)
	}
	if got := model.Month().Format("2006-01"); got != "2026-03" {
		t.Errorf("Month() = %s, want the current month", got)
	}
}

func TestNew_RendersTheCurrentMonthBeforeTheFirstQueryReturns(t *testing.T) {
	// The runtime draws once before Init's commands deliver anything. A zero
	// Grid carries a zero date, which renders as December of the year -1.
	model := New(Options{
		Library:    library(),
		ServiceKey: "stream-dvr",
		Location:   time.UTC,
		WeekStart:  time.Sunday,
		Now:        func() time.Time { return fixedNow },
	})
	model.width, model.height = 120, 40

	got := model.View().Content

	if want := "March 2026"; !strings.Contains(got, want) {
		t.Errorf("View() before the first query does not name %q:\n%s", want, got)
	}
	if strings.Contains(got, "-0001") {
		t.Errorf("View() before the first query renders the zero month:\n%s", got)
	}
}

func TestInit_LoadsCoverageAroundTheMonth(t *testing.T) {
	// The grid shows padding days from the neighbouring months, so the
	// query has to reach past the month or those cells read as unknown
	// when their coverage is actually known.
	lib := library()
	newModel(t, lib, nil)

	monthStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if !lib.lastFrom.Before(monthStart) {
		t.Errorf("coverage queried from %s, want it to reach before the 1st", lib.lastFrom)
	}
	monthEnd := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if !lib.lastTo.After(monthEnd) {
		t.Errorf("coverage queried to %s, want it to reach past the month end", lib.lastTo)
	}
}

func TestInit_SurfacesALoadFailure(t *testing.T) {
	lib := library()
	lib.channelsErr = errors.New("database is locked")

	model := newModel(t, lib, nil)
	if model.Err() == nil {
		t.Error("Err() = nil, want the load failure surfaced")
	}
}

func TestModel_NoChannelsIsNotAnError(t *testing.T) {
	// An empty library is the state before the first channel is
	// configured, not a fault.
	model := newModel(t, &fakeLibrary{}, nil)

	if model.Err() != nil {
		t.Errorf("Err() = %v, want nil", model.Err())
	}
	if _, ok := model.Channel(); ok {
		t.Error("Channel() reported a channel, want none")
	}
	if got := model.ChannelLabel(); got == "" {
		t.Error("ChannelLabel() is empty, want it to say there are none")
	}
}

// ///////////////////////////////////////////////
// Navigation
// ///////////////////////////////////////////////

func TestModel_MovesByDay(t *testing.T) {
	tests := []struct {
		name string
		key  rune
		want string
	}{
		{name: "left is a day back", key: tea.KeyLeft, want: "2026-03-03"},
		{name: "right is a day forward", key: tea.KeyRight, want: "2026-03-05"},
		{name: "up is a week back", key: tea.KeyUp, want: "2026-02-25"},
		{name: "down is a week forward", key: tea.KeyDown, want: "2026-03-11"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newModel(t, library(), nil)
			pressNamed(t, model, tt.key)

			if got := model.Cursor().Format("2006-01-02"); got != tt.want {
				t.Errorf("Cursor() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestModel_FollowsTheCursorAcrossAMonthBoundary(t *testing.T) {
	// Walking off the end of a month has to page the view, or the cursor
	// leaves the grid and the selection disappears.
	model := newModel(t, library(), nil)

	for range 28 {
		pressNamed(t, model, tea.KeyRight)
	}

	if got := model.Cursor().Format("2006-01-02"); got != "2026-04-01" {
		t.Fatalf("Cursor() = %s, want it to have crossed into April", got)
	}
	if got := model.Month().Format("2006-01"); got != "2026-04" {
		t.Errorf("Month() = %s, want the view to have followed", got)
	}
	if _, ok := model.SelectedCell(); !ok {
		t.Error("the cursor is outside the grid after crossing a month")
	}
}

func TestModel_PagesMonths(t *testing.T) {
	model := newModel(t, library(), nil)

	press(t, model, "[")
	if got := model.Month().Format("2006-01"); got != "2026-02" {
		t.Errorf("Month() = %s, want February", got)
	}

	press(t, model, "]")
	press(t, model, "]")
	if got := model.Month().Format("2006-01"); got != "2026-04" {
		t.Errorf("Month() = %s, want April", got)
	}
}

func TestModel_ClampsTheDayWhenAMonthIsShorter(t *testing.T) {
	// Paging from the 31st into a 30-day month has to land somewhere real.
	model := newModel(t, library(), nil)
	for range 27 {
		pressNamed(t, model, tea.KeyRight)
	}
	if got := model.Cursor().Format("2006-01-02"); got != "2026-03-31" {
		t.Fatalf("Cursor() = %s, want March 31", got)
	}

	press(t, model, "]")

	if got := model.Cursor().Format("2006-01-02"); got != "2026-04-30" {
		t.Errorf("Cursor() = %s, want it clamped to the last day of April", got)
	}
}

func TestModel_TodayReturnsHome(t *testing.T) {
	model := newModel(t, library(), nil)
	press(t, model, "[")
	press(t, model, "[")

	press(t, model, "t")

	if got := model.Cursor().Format("2006-01-02"); got != "2026-03-04" {
		t.Errorf("Cursor() = %s, want today", got)
	}
	if got := model.Month().Format("2006-01"); got != "2026-03" {
		t.Errorf("Month() = %s, want the current month", got)
	}
}

// ///////////////////////////////////////////////
// Channels
// ///////////////////////////////////////////////

func TestModel_PicksAChannelByItsNumber(t *testing.T) {
	// By number rather than by cycling, because the app bar shows the
	// numbers and tab is spent moving focus between the two panels.
	lib := library()
	lib.channels = append(lib.channels,
		store.Channel{ID: 2, Platform: "youtube", Name: "someone"})

	model := newModel(t, lib, nil)

	press(t, model, "2")
	if channel, _ := model.Channel(); channel.Name != "someone" {
		t.Errorf("Channel() = %q, want the second channel", channel.Name)
	}
	if lib.lastChannelID != 2 {
		t.Errorf("coverage queried channel %d, want 2", lib.lastChannelID)
	}

	press(t, model, "1")
	if channel, _ := model.Channel(); channel.Name != "examplechannel" {
		t.Errorf("Channel() = %q, want the first channel", channel.Name)
	}
}

func TestModel_ANumberNoChannelIsUnderDoesNothing(t *testing.T) {
	// An operator on a one-channel library pressing 3 has asked for
	// something that is not there, and moving anywhere would be a guess.
	model := newModel(t, library(), nil)

	press(t, model, "3")

	if channel, _ := model.Channel(); channel.Name != "examplechannel" {
		t.Errorf("Channel() = %q, want it left alone", channel.Name)
	}
}

func TestModel_CyclingWithNoChannelsIsHarmless(t *testing.T) {
	model := newModel(t, &fakeLibrary{}, nil)

	pressNamed(t, model, tea.KeyTab)
	if model.Err() != nil {
		t.Errorf("Err() = %v, want nil", model.Err())
	}
}

func TestChannelLabel_ShowsThePositionWhenThereAreSeveral(t *testing.T) {
	lib := library()
	lib.channels = append(lib.channels, store.Channel{ID: 2, Platform: "youtube", Name: "someone"})

	model := newModel(t, lib, nil)
	if got := model.ChannelLabel(); got == "twitch/examplechannel" {
		t.Errorf("ChannelLabel() = %q, want it to show the position too", got)
	}
}

// ///////////////////////////////////////////////
// Recorder control
// ///////////////////////////////////////////////

func TestModel_ControlStartsWhenIdle(t *testing.T) {
	controller := &fakeController{status: Status{State: "installed"}}
	model := newModel(t, library(), controller)

	press(t, model, "s")

	if controller.starts != 1 {
		t.Errorf("Start called %d times, want 1", controller.starts)
	}
	if controller.stops != 0 {
		t.Errorf("Stop called %d times, want 0", controller.stops)
	}
}

func TestModel_ControlStopsWhenRunning(t *testing.T) {
	controller := &fakeController{status: Status{State: "running"}}
	model := newModel(t, library(), controller)

	press(t, model, "s")

	if controller.stops != 1 {
		t.Errorf("Stop called %d times, want 1", controller.stops)
	}
	if controller.starts != 0 {
		t.Errorf("Start called %d times, want 0", controller.starts)
	}
}

func TestModel_ControlReportsAFailure(t *testing.T) {
	controller := &fakeController{
		status:  Status{State: "installed"},
		failErr: errors.New("access is denied"),
	}
	model := newModel(t, library(), controller)

	press(t, model, "s")

	if model.Err() == nil {
		t.Error("Err() = nil, want the control failure surfaced")
	}
}

func TestModel_ControlWithoutARegistrationSaysWhatToDo(t *testing.T) {
	// Pressing start on a machine that never ran install has to point at
	// install rather than failing silently.
	model := newModel(t, library(), nil)

	press(t, model, "s")

	if model.Err() == nil {
		t.Fatal("Err() = nil, want an explanation")
	}
	if got := model.Err().Error(); got == "" {
		t.Error("Err() is empty")
	}
}

func TestModel_ControlDoesNotBlockTheEventLoop(t *testing.T) {
	// Start and Stop reach the platform's service manager, which takes
	// hundreds of milliseconds to seconds. Running that inside Update
	// freezes every key including quit for as long as it takes.
	controller := &fakeController{status: Status{State: "installed"}}
	model := newModel(t, library(), controller)
	holdControl(t, controller)

	_, cmd := model.Update(keyPress("s"))
	if cmd == nil {
		t.Fatal("pressing s returned no command, so the control ran inside Update")
	}
	if controller.starts != 0 {
		t.Errorf("Start ran during Update, want it deferred to the command")
	}

	// The loop is still live: quit answers while the control is held open.
	_, _ = model.Update(keyPress("q"))
	if !model.Quit() {
		t.Error("quit did not answer while a control was in flight")
	}

	drain(t, model, cmd)
	if controller.starts != 1 {
		t.Errorf("Start called %d times once the command ran, want 1", controller.starts)
	}
}

func TestModel_ControlIgnoresARepeatWhileOneIsInFlight(t *testing.T) {
	// Holding the key would otherwise start one call into the service
	// manager per autorepeat.
	controller := &fakeController{status: Status{State: "installed"}}
	model := newModel(t, library(), controller)
	holdControl(t, controller)

	_, first := model.Update(keyPress("s"))
	_, second := model.Update(keyPress("s"))
	if second != nil {
		t.Error("a second control started while the first was in flight")
	}

	drain(t, model, first)
	if controller.starts != 1 {
		t.Errorf("Start called %d times, want 1", controller.starts)
	}
}

func TestModel_ControlShowsTheConditionItProduced(t *testing.T) {
	// The header has to reflect the action rather than the state before it,
	// or the operator presses start and reads "installed".
	controller := &fakeController{status: Status{State: "installed"}}
	model := newModel(t, library(), controller)

	press(t, model, "s")

	if got := model.status.State; got != "running" {
		t.Errorf("status.State = %q after starting, want %q", got, "running")
	}
}

// ///////////////////////////////////////////////
// Querying the recorder
// ///////////////////////////////////////////////

func TestModel_NavigationNeverQueriesTheRecorder(t *testing.T) {
	// Every query reaches the platform's service manager. Bound to
	// navigation, key autorepeat starts one per repeat, each in its own
	// goroutine with nothing coalescing them.
	tests := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{name: "a day back", key: tea.KeyPressMsg{Code: tea.KeyLeft}},
		{name: "a day forward, crossing no month", key: tea.KeyPressMsg{Code: tea.KeyRight}},
		{name: "a week back, crossing a month", key: tea.KeyPressMsg{Code: tea.KeyUp}},
		{name: "a month back", key: keyPress("[")},
		{name: "a month forward", key: keyPress("]")},
		{name: "today", key: keyPress("t")},
		{name: "the next channel", key: tea.KeyPressMsg{Code: tea.KeyTab}},
		{name: "an explicit refresh", key: keyPress("r")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &fakeController{status: Status{State: "installed"}}
			model := newModel(t, library(), controller)
			before := controller.queries

			for range 20 {
				_, cmd := model.Update(tt.key)
				drain(t, model, cmd)
			}

			if got := controller.queries - before; got != 0 {
				t.Errorf("20 presses ran %d recorder queries, want none", got)
			}
		})
	}
}

func TestModel_PollsTheRecorderOnATick(t *testing.T) {
	// The condition still has to keep up with a recorder that stops on its
	// own, so it is polled on a timer rather than not at all.
	controller := &fakeController{status: Status{State: "installed"}}
	model := newModel(t, library(), controller)
	before := controller.queries

	drain(t, model, func() tea.Msg { return statusTickMsg{} })

	if got := controller.queries - before; got != 1 {
		t.Errorf("a tick ran %d recorder queries, want 1", got)
	}
	if model.status.State != "installed" {
		t.Errorf("status.State = %q, want the polled condition", model.status.State)
	}
}

func TestModel_ATickIsDroppedWhileAQueryIsInFlight(t *testing.T) {
	controller := &fakeController{status: Status{State: "installed"}}
	model := newModel(t, library(), controller)

	// The first tick's query is started but never completes, so the model
	// still believes one is in flight when the second arrives.
	_, _ = model.Update(statusTickMsg{})
	before := controller.queries

	_, cmd := model.Update(statusTickMsg{})
	drain(t, model, cmd)

	if got := controller.queries - before; got != 0 {
		t.Errorf("a tick during an in-flight query ran %d more queries, want none", got)
	}
}

func TestModel_ATickAlwaysSchedulesTheNextOne(t *testing.T) {
	// A dropped tick that also dropped its successor would freeze the
	// condition on display for the rest of the session without saying so.
	model := New(Options{
		Library:        library(),
		Controller:     &fakeController{status: Status{State: "installed"}},
		ServiceKey:     "stream-dvr",
		Location:       time.UTC,
		WeekStart:      time.Sunday,
		StatusInterval: time.Millisecond,
		Now:            func() time.Time { return fixedNow },
	})
	drain(t, model, model.Init())

	// A query is left in flight, which is the case that drops the poll.
	_, _ = model.Update(statusTickMsg{})

	_, cmd := model.Update(statusTickMsg{})
	if cmd == nil {
		t.Fatal("a dropped tick scheduled nothing, so polling stops for good")
	}
	if _, ok := cmd().(statusTickMsg); !ok {
		t.Error("a tick did not schedule the next one")
	}
}

func TestModel_NoTimerWhenTheIntervalIsZero(t *testing.T) {
	model := New(Options{
		Library:    library(),
		ServiceKey: "stream-dvr",
		Location:   time.UTC,
		WeekStart:  time.Sunday,
		Now:        func() time.Time { return fixedNow },
	})

	if cmd := model.scheduleStatusPoll(); cmd != nil {
		t.Error("scheduleStatusPoll() returned a timer for a zero interval")
	}
}

// ///////////////////////////////////////////////
// Reloading
// ///////////////////////////////////////////////

func TestRefresh_OnlyOneRunsAtATime(t *testing.T) {
	// A held arrow repeats at the terminal's autorepeat rate, and each repeat
	// that crosses a month asks for a reload. Nothing coalesced them.
	lib := library()
	model := newModel(t, lib, nil)
	before := lib.coverageCalls

	var issued int
	for range 20 {
		if cmd := model.refresh(); cmd != nil {
			issued++
		}
	}

	if issued != 1 {
		t.Errorf("20 requests started %d loads, want 1 with the rest folded in", issued)
	}
	if got := lib.coverageCalls - before; got != 0 {
		t.Errorf("%d queries ran before the first command was executed", got)
	}
}

func TestRefresh_ARequestDuringALoadIsNotLost(t *testing.T) {
	// Dropping it would leave the grid showing one month's coverage under
	// another month's dates, which is worse than the pile-up it avoids.
	lib := library()
	model := newModel(t, lib, nil)

	// Page forward twice without letting the first load land, so the second
	// request arrives while the first is still in flight.
	_, first := model.Update(keyPress("]"))
	_, second := model.Update(keyPress("]"))
	if second != nil {
		t.Fatal("the second page started its own load, want it folded into the first")
	}

	drain(t, model, first)

	if got := model.Month().Format("2006-01"); got != "2026-05" {
		t.Fatalf("Month() = %s, want May", got)
	}
	if got := lib.lastFrom.Format("2006-01"); got != "2026-04" {
		t.Errorf("the last coverage query started in %s, want the reload to have covered May's grid", got)
	}
}

// ///////////////////////////////////////////////
// Panes
// ///////////////////////////////////////////////

func TestModel_EnterOpensTheDayPane(t *testing.T) {
	model := newModel(t, dayLibrary(), nil)

	pressNamed(t, model, tea.KeyEnter)

	if !model.dayOpen {
		t.Error("enter did not open the day")
	}
	if !strings.Contains(stripANSI(model.View().Content), "esc close") {
		t.Error("the footer still lists the calendar's keys inside the day modal")
	}
}

func TestModel_EscPopsOneLevelAndNeverQuits(t *testing.T) {
	// One level per press, all the way down, and the bottom says how to
	// leave rather than leaving. An esc that quit from the calendar makes
	// every pane a trap door for an operator who reached for it out of
	// habit.
	model := newModel(t, dayLibrary(), nil)

	press(t, model, "tab")
	pressNamed(t, model, tea.KeyEnter)
	if !model.dayOpen {
		t.Fatal("enter did not open the day")
	}

	pressNamed(t, model, tea.KeyEscape)
	if model.dayOpen {
		t.Fatal("esc did not close the day")
	}
	if model.focused != focusQueue {
		t.Fatal("esc popped two levels at once, past the focused queue")
	}

	pressNamed(t, model, tea.KeyEscape)
	if model.focused != focusCalendar {
		t.Fatal("esc did not return focus to the calendar")
	}

	pressNamed(t, model, tea.KeyEscape)
	if model.Quit() {
		t.Fatal("esc quit from the calendar, which only q may do")
	}
	if !strings.Contains(stripANSI(model.View().Content), "q quits") {
		t.Error("esc at the last level did nothing at all, and said nothing either")
	}
}

func TestModel_QuitsFromEveryPane(t *testing.T) {
	// The companion to esc never quitting. q has to work wherever an
	// operator is, or the only way out is the window manager.
	tests := []struct {
		name string
		open func(*testing.T, *Model)
	}{
		{name: "the calendar", open: func(*testing.T, *Model) {}},
		{
			name: "the day modal",
			open: func(t *testing.T, m *Model) { pressNamed(t, m, tea.KeyEnter) },
		},
		{
			name: "the recovery queue",
			open: func(t *testing.T, m *Model) { press(t, m, "tab") },
		},
		{
			name: "purge",
			open: func(t *testing.T, m *Model) { press(t, m, "x") },
		},
		{
			name: "settings",
			open: func(t *testing.T, m *Model) { press(t, m, "e") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newModel(t, dayLibrary(), nil)
			tt.open(t, model)

			press(t, model, "q")

			if !model.Quit() {
				t.Error("q did not quit")
			}
		})
	}
}

func TestModel_ArrowsInTheDayPaneLeaveTheDayAlone(t *testing.T) {
	// The pane is worth entering only because the arrows change meaning
	// inside it. Moving the day as well would make the recording cursor
	// point at a list that is no longer on screen.
	model := newModel(t, dayLibrary(), nil)
	pressNamed(t, model, tea.KeyEnter)
	before := model.Cursor()

	pressNamed(t, model, tea.KeyDown)
	pressNamed(t, model, tea.KeyDown)

	if !model.Cursor().Equal(before) {
		t.Errorf("Cursor() = %s after two downs in the day pane, want %s",
			model.Cursor().Format("2006-01-02"), before.Format("2006-01-02"))
	}
	if model.recording != 1 {
		t.Errorf("recording = %d after two downs over two recordings, want 1", model.recording)
	}
}

func TestModel_DayPaneCursorStopsAtEachEnd(t *testing.T) {
	cases := []struct {
		name  string
		key   rune
		times int
		want  int
	}{
		{name: "up from the first", key: tea.KeyUp, times: 3, want: 0},
		{name: "down past the last", key: tea.KeyDown, times: 5, want: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := newModel(t, dayLibrary(), nil)
			pressNamed(t, model, tea.KeyEnter)

			for range tc.times {
				pressNamed(t, model, tc.key)
			}

			if model.recording != tc.want {
				t.Errorf("recording = %d, want %d", model.recording, tc.want)
			}
		})
	}
}

func TestModel_ARefreshThatShortensTheDayMovesTheCursorIn(t *testing.T) {
	// A purge or a finalize can drop a row while the pane is open, and the
	// cursor is what every later action reads.
	lib := dayLibrary()
	model := newModel(t, lib, nil)
	pressNamed(t, model, tea.KeyEnter)
	pressNamed(t, model, tea.KeyDown)

	lib.recordings = lib.recordings[:1]
	press(t, model, "r")

	if model.recording != 0 {
		t.Errorf("recording = %d after the list shrank to one, want 0", model.recording)
	}
	if _, ok := model.SelectedRecording(); !ok {
		t.Error("SelectedRecording() reports nothing under a cursor the day still holds")
	}
}

func TestSelectedRecording_ReportsNothingOnAnEmptyDay(t *testing.T) {
	// A missed day is exactly where recovery is requested, so the pane opens
	// on one with nothing in it.
	model := newModel(t, library(), nil)
	pressNamed(t, model, tea.KeyEnter)

	if _, ok := model.SelectedRecording(); ok {
		t.Error("SelectedRecording() found a recording on a day that has none")
	}
}

// ///////////////////////////////////////////////
// Watched and pinned
// ///////////////////////////////////////////////

func TestModel_MarksTheRecordingUnderTheCursorWatched(t *testing.T) {
	lib := dayLibrary()
	actions := &fakeActions{library: lib}
	model := newActingModel(t, lib, actions)
	pressNamed(t, model, tea.KeyEnter)
	pressNamed(t, model, tea.KeyDown)

	press(t, model, "w")

	if actions.watchedID != 2 {
		t.Errorf("MarkWatched called for recording %d, want the one under the cursor", actions.watchedID)
	}
	if actions.watchedAt == nil {
		t.Fatal("MarkWatched was passed nil, want the current time")
	}
	if !actions.watchedAt.Equal(fixedNow) {
		t.Errorf("MarkWatched at %s, want %s", actions.watchedAt, fixedNow)
	}
	marked := markedRecordings(render(t, model))
	if len(marked) != 1 {
		t.Fatalf("%d recordings carry the cursor, want 1", len(marked))
	}
	if !strings.Contains(marked[0], "20:15") {
		t.Errorf("the cursor line is %q, want it on the 20:15 recording", marked[0])
	}
	if !strings.Contains(marked[0], glyphWatched) {
		t.Errorf("the cursor line is %q, want it to show the watched mark", marked[0])
	}
}

func TestModel_WatchedTogglesBackOff(t *testing.T) {
	// The mark is what raises a purge score, so an operator who set it by
	// mistake needs the same key to take it off.
	lib := dayLibrary()
	lib.recordings[0].WatchedAt = &fixedNow
	actions := &fakeActions{library: lib}
	model := newActingModel(t, lib, actions)
	pressNamed(t, model, tea.KeyEnter)

	press(t, model, "w")

	if actions.watchedCalls != 1 {
		t.Fatalf("MarkWatched called %d times, want 1", actions.watchedCalls)
	}
	if actions.watchedAt != nil {
		t.Errorf("MarkWatched at %s, want nil to clear the mark", actions.watchedAt)
	}
}

func TestModel_PinsTheRecordingUnderTheCursor(t *testing.T) {
	cases := []struct {
		name   string
		pinned bool
		want   bool
	}{
		{name: "an unpinned recording is pinned", pinned: false, want: true},
		{name: "a pinned recording is released", pinned: true, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lib := dayLibrary()
			lib.recordings[0].Pinned = tc.pinned
			actions := &fakeActions{library: lib}
			model := newActingModel(t, lib, actions)
			pressNamed(t, model, tea.KeyEnter)

			press(t, model, "p")

			if actions.pinnedID != 1 {
				t.Errorf("SetPinned called for recording %d, want 1", actions.pinnedID)
			}
			if actions.pinned != tc.want {
				t.Errorf("SetPinned(%t), want %t", actions.pinned, tc.want)
			}
		})
	}
}

func TestModel_AWriteFailureReachesTheErrorPane(t *testing.T) {
	// A write can lose to the daemon's own write lock past busy_timeout.
	// Swallowing it would leave the operator believing the mark was set.
	lib := dayLibrary()
	actions := &fakeActions{library: lib, err: errors.New("database is locked")}
	model := newActingModel(t, lib, actions)
	pressNamed(t, model, tea.KeyEnter)

	press(t, model, "w")

	if model.Err() == nil {
		t.Fatal("Err() = nil after a failed write, want the failure surfaced")
	}
	if !strings.Contains(render(t, model), "database is locked") {
		t.Error("the error pane does not show why the write failed")
	}
}

func TestModel_WritingWithoutAWritableLibrarySaysSo(t *testing.T) {
	model := newModel(t, dayLibrary(), nil)
	pressNamed(t, model, tea.KeyEnter)

	press(t, model, "w")

	if model.Err() == nil {
		t.Fatal("Err() = nil with no Actions, want the key to report why it did nothing")
	}
	if !strings.Contains(model.Err().Error(), "not writable") {
		t.Errorf("Err() = %v, want it to say the library is not writable", model.Err())
	}
}

func TestModel_WritingOnAnEmptyDayIsHarmless(t *testing.T) {
	lib := library()
	actions := &fakeActions{library: lib}
	model := newActingModel(t, lib, actions)
	pressNamed(t, model, tea.KeyEnter)

	press(t, model, "w")
	press(t, model, "p")

	if actions.watchedCalls != 0 || actions.pinnedID != 0 {
		t.Error("a day with no recordings still wrote to the library")
	}
	if model.Err() != nil {
		t.Errorf("Err() = %v on an empty day, want nil", model.Err())
	}
}

// ///////////////////////////////////////////////
// The recorder in this window
// ///////////////////////////////////////////////

func TestModel_InProcessRecorderStartsAndStops(t *testing.T) {
	recorder := &fakeRecorder{}
	model := newOptionsModel(t, Options{Library: library(), Recorder: recorder})

	press(t, model, "d")
	if !recorder.Running() {
		t.Fatal("d did not start the recorder in this window")
	}
	view := render(t, model)
	if !strings.Contains(view, "in this window") {
		t.Errorf("the header does not say the recorder runs here:\n%s", view)
	}
	if !strings.Contains(view, "d stop here") {
		t.Errorf("the footer still offers to start a recorder that is running:\n%s", view)
	}

	press(t, model, "d")
	if recorder.Running() {
		t.Error("d did not stop the recorder in this window")
	}
}

func TestModel_InProcessRecorderRefusesBesideTheInstalledService(t *testing.T) {
	// Two recorders against one library is the thing being prevented, and
	// the calendar knows about the installed one before it tries.
	recorder := &fakeRecorder{}
	model := newOptionsModel(t, Options{
		Library:    library(),
		Recorder:   recorder,
		Controller: &fakeController{status: Status{State: stateRunning}},
	})

	press(t, model, "d")

	if recorder.starts != 0 {
		t.Error("the calendar started a second recorder beside the installed one")
	}
	if model.Err() == nil || !strings.Contains(model.Err().Error(), "already running") {
		t.Errorf("Err() = %v, want it to name the installed recorder", model.Err())
	}
}

func TestModel_RefusesToStartADisabledRegistration(t *testing.T) {
	// A disabled registration is complete and its triggers are set, and the
	// scheduler still will not run it. Asking anyway reports a failure that
	// reads as a broken recorder rather than as a setting to change.
	controller := &fakeController{status: Status{State: stateDisabled}}
	model := newOptionsModel(t, Options{Library: library(), Controller: controller})

	press(t, model, "s")

	if controller.starts != 0 {
		t.Error("the calendar asked the scheduler to run a disabled registration")
	}
	if model.Err() == nil || !strings.Contains(model.Err().Error(), "disabled") {
		t.Errorf("Err() = %v, want it to name the disabled registration", model.Err())
	}
}

func TestModel_SaysWhyADisabledRecorderWillNotStart(t *testing.T) {
	// The header carries the state word, and "disabled" alone reads like a
	// condition that clears on its own.
	model := newOptionsModel(t, Options{
		Library:    library(),
		Controller: &fakeController{status: Status{State: stateDisabled}},
	})
	drain(t, model, model.Init())

	if !strings.Contains(model.View().Content, "enabled again") {
		t.Errorf("View() carries no reason for a disabled recorder:\n%s", model.View().Content)
	}
}

func TestModel_InProcessRecorderSurfacesWhyItDied(t *testing.T) {
	// A recorder that died on a configuration fault would otherwise read as
	// running for as long as the calendar stayed open.
	recorder := &fakeRecorder{startErr: errors.New("no channels are enabled")}
	model := newOptionsModel(t, Options{Library: library(), Recorder: recorder})

	press(t, model, "d")

	if model.Err() == nil || !strings.Contains(model.Err().Error(), "no channels") {
		t.Errorf("Err() = %v, want the failure surfaced", model.Err())
	}
	if recorder.Running() {
		t.Error("a recorder that refused to start reads as running")
	}
}

func TestModel_QuittingStopsTheRecorderInThisWindow(t *testing.T) {
	// It dies with the process either way. Stopping it deliberately unwinds
	// a capture in flight rather than leaving it killed mid-write.
	recorder := &fakeRecorder{}
	model := newOptionsModel(t, Options{Library: library(), Recorder: recorder})
	press(t, model, "d")

	press(t, model, "q")

	if !model.Quit() {
		t.Fatal("q did not quit")
	}
	if recorder.stops != 1 {
		t.Errorf("Stop called %d times on quit, want 1", recorder.stops)
	}
}

func TestModel_QuittingWithNoRecorderHereIsHarmless(t *testing.T) {
	recorder := &fakeRecorder{}
	model := newOptionsModel(t, Options{Library: library(), Recorder: recorder})

	press(t, model, "q")

	if recorder.stops != 0 {
		t.Errorf("Stop called %d times with nothing running, want 0", recorder.stops)
	}
}

func TestModel_InProcessRecorderWithoutOneSaysSo(t *testing.T) {
	model := newModel(t, library(), nil)

	press(t, model, "d")

	if model.Err() == nil || !strings.Contains(model.Err().Error(), "only the installed") {
		t.Errorf("Err() = %v, want it to say only the installed recorder is reachable", model.Err())
	}
}

// ///////////////////////////////////////////////
// Recovery
// ///////////////////////////////////////////////

func TestModel_RecoveryRequestsTheSelectedDay(t *testing.T) {
	lib := library()
	lib.days = []store.Day{coverageDay(4, store.CoverageMissed)}
	recovery := &fakeRecovery{}
	model := newOptionsModel(t, Options{Library: lib, Recovery: recovery})
	pressNamed(t, model, tea.KeyEnter)

	press(t, model, "R")

	if recovery.channelID != 1 {
		t.Errorf("Request for channel %d, want 1", recovery.channelID)
	}
	if got := recovery.day.Format("2006-01-02"); got != "2026-03-04" {
		t.Errorf("Request for %s, want the selected day", got)
	}
	if !strings.Contains(render(t, model), "recovery requested") {
		t.Errorf("the pane does not confirm the request:\n%s", render(t, model))
	}
}

func TestModel_RecoveryIsRefusedWhereThereIsNoGap(t *testing.T) {
	// An at-risk day has its bytes on disk already, so fetching it again
	// would replace a real capture with a muted archive copy. This is
	// calendar.Cell.Recoverable's rule and the pane must not have a second.
	cases := []struct {
		name     string
		coverage store.Coverage
	}{
		{name: "a fully captured day", coverage: store.CoverageLive},
		{name: "a day at risk", coverage: store.CoverageAtRisk},
		{name: "a day with no broadcast", coverage: store.CoverageNoStream},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lib := library()
			lib.days = []store.Day{coverageDay(4, tc.coverage)}
			recovery := &fakeRecovery{}
			model := newOptionsModel(t, Options{Library: lib, Recovery: recovery})
			pressNamed(t, model, tea.KeyEnter)

			press(t, model, "R")

			if recovery.calls != 0 {
				t.Errorf("Request ran %d times on a day with no gap", recovery.calls)
			}
			if model.Err() == nil || !strings.Contains(model.Err().Error(), "no gap") {
				t.Errorf("Err() = %v, want it to say there is no gap", model.Err())
			}
		})
	}
}

func TestModel_RecoveryReportsAFailure(t *testing.T) {
	lib := library()
	lib.days = []store.Day{coverageDay(4, store.CoveragePartial)}
	recovery := &fakeRecovery{err: errors.New("no archive for this channel")}
	model := newOptionsModel(t, Options{Library: lib, Recovery: recovery})
	pressNamed(t, model, tea.KeyEnter)

	press(t, model, "R")

	if !strings.Contains(render(t, model), "no archive") {
		t.Error("the pane does not say why the request failed")
	}
}

func TestModel_RecoveryWithoutAnEngineSaysSo(t *testing.T) {
	// cmd passes no recovery, so this is what the key answers with.
	lib := library()
	lib.days = []store.Day{coverageDay(4, store.CoverageMissed)}
	model := newModel(t, lib, nil)
	pressNamed(t, model, tea.KeyEnter)

	press(t, model, "R")

	if model.Err() == nil || !strings.Contains(model.Err().Error(), "not built yet") {
		t.Errorf("Err() = %v, want it to say recovery is not built", model.Err())
	}
}

func TestModel_ANoticeLastsUntilTheNextKey(t *testing.T) {
	// Left standing, it would describe an action the operator has since
	// navigated away from.
	lib := library()
	lib.days = []store.Day{coverageDay(4, store.CoverageMissed)}
	model := newOptionsModel(t, Options{Library: lib, Recovery: &fakeRecovery{}})
	pressNamed(t, model, tea.KeyEnter)
	press(t, model, "R")

	pressNamed(t, model, tea.KeyEsc)

	if model.notice != "" {
		t.Errorf("notice = %q after another key, want it cleared", model.notice)
	}
}

// ///////////////////////////////////////////////
// Quitting
// ///////////////////////////////////////////////

func TestQuit(t *testing.T) {
	panes := map[string]pane{"calendar": paneCalendar, "day": paneCalendar}

	for name, target := range panes {
		t.Run(name, func(t *testing.T) {
			model := newModel(t, dayLibrary(), nil)
			model.pane = target

			press(t, model, "q")

			if !model.Quit() {
				t.Errorf("q did not quit from the %s pane", name)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Selection
// ///////////////////////////////////////////////

func TestSelectedRecordings_FiltersToTheCursorsDay(t *testing.T) {
	lib := library()
	lib.recordings = []store.Recording{
		{ID: 1, Path: "on-the-day.mkv", StartedAt: time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)},
		{ID: 2, Path: "another-day.mkv", StartedAt: time.Date(2026, 3, 3, 21, 15, 0, 0, time.UTC)},
	}

	model := newModel(t, lib, nil)

	got := model.SelectedRecordings()
	if len(got) != 1 || got[0].Path != "on-the-day.mkv" {
		t.Errorf("SelectedRecordings() = %v, want only the recording from the selected day", got)
	}
}

func TestRefresh_ReloadsOnDemand(t *testing.T) {
	lib := library()
	model := newModel(t, lib, nil)
	before := lib.coverageCalls

	press(t, model, "r")

	if lib.coverageCalls <= before {
		t.Error("r did not reload coverage")
	}
}
