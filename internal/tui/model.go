// Package tui presents the recording library: a coverage calendar per
// channel, the detail behind any day, and control over the recorder.
//
// The model holds no rendering decisions that depend on a live terminal and
// no I/O of its own beyond the queries it issues, so its navigation and
// state transitions are testable without one.
package tui

import (
	"errors"
	"fmt"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"zach.tools/go/stream-dvr/internal/calendar"
	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/retention"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Library is what the model needs from the store. Narrowing it to these
// three calls keeps the model testable without a database.
type Library interface {
	// Channels lists every known channel.
	Channels() ([]store.Channel, error)
	// CoverageBetween reports per-day coverage for a channel.
	CoverageBetween(channelID int64, from, to time.Time, loc *time.Location) ([]store.Day, error)
	// RecordingsForChannel lists a channel's recordings in a window.
	RecordingsForChannel(channelID int64, from, to time.Time) ([]store.Recording, error)
	// SpaceUsage reports where the library sits against its budget.
	SpaceUsage() (Space, error)
	// GapsFor lists the holes in one recording, and what became of each.
	GapsFor(recordingID int64) ([]store.Gap, error)
}

// Actions is what the model needs to change a recording.
//
// It is separate from Library because Library is the read side, and the two
// carry different consequences: a failed read leaves a stale screen, and a
// failed write leaves the operator believing something happened.
type Actions interface {
	// MarkWatched records that a recording was watched, which raises its
	// purge score. Passing nil clears the mark.
	MarkWatched(id int64, at *time.Time) error
	// SetPinned protects a recording from the purge list, or releases it.
	SetPinned(id int64, pinned bool) error
}

// Recovery is what the model needs to ask for a missed day to be fetched
// from a public archive.
//
// This is the seam, not the engine. Persisting a request and acting on it
// belong to whatever the caller supplies here.
type Recovery interface {
	// Request asks for one channel's broadcasts on one day.
	Request(channelID int64, day time.Time) error
}

// Recorder is a daemon this process runs itself, as an alternative to the
// one the operating system starts.
//
// Err is what makes it not a fire-and-forget goroutine. A recorder that
// died on a configuration fault would otherwise read as running for as long
// as the calendar stayed open.
type Recorder interface {
	Start() error
	Stop() error
	Running() bool
	Err() error
}

// Controller is what the model needs to run and stop the recorder.
type Controller interface {
	Start(name string) error
	Stop(name string) error
	Status(name string) (Status, error)
}

// Status mirrors service.Status without importing it, so the model can be
// tested without a platform-specific package. It is exported because
// Controller is: an unexported type in an exported interface would make
// the interface unimplementable from outside this package.
type Status struct {
	// State is the recorder's condition, such as "running".
	State string
	// Detail names the underlying object, for display.
	Detail string
}

// Space is where the library sits against its budget.
//
// Mirrored rather than taken from internal/space for the reason Status is
// mirrored: the model stays testable without the packages that read a real
// disk, and an unexported type inside an exported interface could not be
// implemented from outside this package.
type Space struct {
	// Held is what the library's recordings occupy.
	Held int64
	// Cap is the configured ceiling. Zero means uncapped.
	Cap int64
	// Free is what remains on the volume.
	Free int64
	// Level is the fill level: "ok", "low", or "critical".
	Level string
}

// pane names which part of the calendar has focus.
//
// Keys dispatch on it before they dispatch on the key itself, so one arrow
// moves a day on the grid and a recording in the day pane without either
// handler knowing about the other.
type pane int

// Model is the calendar application's state.
type Model struct {
	library        Library
	actions        Actions
	purge          Purge
	settings       Settings
	recovery       Recovery
	recorder       Recorder
	controller     Controller
	serviceKey     string
	location       *time.Location
	weekStart      time.Weekday
	statusInterval time.Duration
	now            func() time.Time

	channels []store.Channel
	channel  int

	pane   pane
	month  time.Time
	cursor time.Time
	grid   calendar.Grid

	recordings []store.Recording
	recording  int

	candidates []retention.Candidate
	candidate  int
	selected   map[int64]bool
	confirming bool
	// purging reports a trash already issued and not yet answered, so a
	// second enter cannot issue another against the same selection.
	purging  bool
	purged   int
	purgeErr error

	editor   config.Config
	fields   []field
	field    int
	editing  bool
	input    textinput.Model
	commit   func(string) error
	problems []config.Problem
	saved    bool
	saveErr  error
	// dirty reports an edit made and not yet written, which is the only
	// work the calendar holds that exists nowhere else.
	dirty bool
	// confirmQuit reports a quit already refused once over unsaved edits,
	// so the next press goes through.
	confirmQuit bool

	status    Status
	statusErr error
	space     Space
	spaceErr  error

	loading     bool
	reload      bool
	polling     bool
	controlling bool

	feed   Feed
	events []FeedEvent

	// queue is the month's recoverable days, oldest first, which is what
	// Grid.Gaps computes and the old screen showed nowhere useful.
	queue       []calendar.Cell
	queueAt     int
	queueOffset int

	// dayOpen is the modal over the two panels. The day left the right
	// panel because at 80 columns there is no right panel to put it in, and
	// once that modal exists a second renderer for the same payload is pure
	// duplication.
	dayOpen   bool
	dayOffset int
	// settingsOffset and purgeOffset are where each scrolled list sits. A
	// config of thirty fields and a ranking of every recording both outgrow
	// any panel, and neither said so before.
	settingsOffset int
	purgeOffset    int
	// gaps are the holes in a capture, loaded for the selected recording
	// when the modal opens. Keyed by recording, so paging through a day's
	// captures does not re-query one already read.
	gaps map[int64][]store.Gap

	width  int
	height int
	styles styles
	// frame is where the last render put every region, which is what a
	// click is resolved against.
	frame frame
	// hits is what is clickable, rebuilt each render. A region that is no
	// longer drawn cannot be clicked, which is how a modal takes the whole
	// screen: with one open, the cells beneath are never registered.
	hits *registry
	// focused is which panel takes the keys, carried by border weight.
	focused focus
	toasts  []toast

	err    error
	notice string
	quit   bool
}

// focus is which panel the keys go to.
type focus int

// Options configure a Model.
type Options struct {
	// Library supplies coverage and recordings.
	Library Library
	// Actions changes a recording. It may be nil, in which case the keys
	// that would write report that the library is not writable.
	Actions Actions
	// Purge ranks and trashes recordings. It may be nil, in which case the
	// purge key reports that the library cannot be purged from here.
	Purge Purge
	// Settings reads and writes the config file. It may be nil, in which
	// case the settings key reports that no config file is reachable.
	Settings Settings
	// Recovery asks for a missed day to be fetched from an archive. It may
	// be nil, in which case the recovery key reports that none is wired.
	Recovery Recovery
	// Recorder runs the daemon inside this process. It may be nil, in which
	// case the key reports that only the installed service is reachable.
	Recorder Recorder
	// Feed carries what the recorder reports, for the pane that shows it.
	// It may be nil, in which case the pane never appears.
	Feed Feed
	// Controller runs and stops the recorder. It may be nil, in which
	// case the control keys report that no registration is reachable.
	Controller Controller
	// ServiceKey names the registration the controller acts on.
	ServiceKey string
	// Location is the timezone days are bucketed in.
	Location *time.Location
	// WeekStart is the weekday each calendar row begins on.
	WeekStart time.Weekday
	// StatusInterval is how often the recorder's condition is polled.
	// Zero polls at startup and after a control key and never on a timer,
	// which is what a test or a one-shot render wants.
	StatusInterval time.Duration
	// Now supplies the current time, for tests.
	Now func() time.Time
}

// ///////////////////////////////////////////////
// Messages
// ///////////////////////////////////////////////

// loadedMsg carries a completed refresh.
type loadedMsg struct {
	channels   []store.Channel
	days       []store.Day
	recordings []store.Recording
	space      Space
	spaceErr   error
	err        error
}

// statusMsg carries a completed recorder query.
type statusMsg struct {
	status Status
	err    error
}

// statusTickMsg asks for the next recorder query.
type statusTickMsg struct{}

// controlMsg carries the result of a start or a stop, with the condition
// read back afterwards so the header does not lag the action.
type controlMsg struct {
	status Status
	err    error
	query  error
}

// ///////////////////////////////////////////////
// Construction
// ///////////////////////////////////////////////

// DefaultStatusInterval is how often a Model that runs in front of an
// operator polls the recorder's condition.
//
// It is a timer rather than a side effect of navigation because each query
// costs a call into the platform's service manager, and key autorepeat on
// an arrow would otherwise start one per repeat.
const DefaultStatusInterval = 5 * time.Second

// The fill levels Space.Level may carry, matching the names
// internal/space reports. They are strings rather than a mirrored type so
// an implementer outside this package needs nothing from here to fill the
// field in.
const (
	spaceLow      = "low"
	spaceCritical = "critical"
)

// stateRunning is the recorder condition every key that acts on the recorder
// compares against. It mirrors service.StateRunning without importing it,
// for the same reason Status does.
const stateRunning = "running"

// stateDisabled is a registration that exists and will never start. It
// mirrors service.StateDisabled, the way stateRunning does.
const stateDisabled = "disabled"

// The panes, in the order an operator reaches them. The calendar is the
// zero value because it is where a Model opens.
const (
	paneCalendar pane = iota
	panePurge
	paneSettings
)

// The two panels that take keys on the month view. The caret marks the
// selected day in both at once; border weight is what says which one the
// arrows move in.
const (
	focusCalendar focus = iota
	focusQueue
)

// New returns a Model positioned on the current month.
func New(opts Options) *Model {
	model := &Model{
		library:        opts.Library,
		actions:        opts.Actions,
		purge:          opts.Purge,
		settings:       opts.Settings,
		recovery:       opts.Recovery,
		recorder:       opts.Recorder,
		feed:           opts.Feed,
		selected:       make(map[int64]bool),
		controller:     opts.Controller,
		serviceKey:     opts.ServiceKey,
		location:       opts.Location,
		weekStart:      opts.WeekStart,
		statusInterval: opts.StatusInterval,
		now:            opts.Now,
		// A terminal that never answers the background query keeps this
		// set, so the greys are the dark-scheme ones until told otherwise.
		// Dark is the majority and the wrong guess costs contrast, not
		// meaning: every state also carries a glyph.
		styles: newStyles(true),
		hits:   newRegistry(),
	}
	if model.location == nil {
		model.location = time.Local
	}
	if model.now == nil {
		model.now = time.Now
	}

	today := model.today()
	model.month = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, model.location)
	model.cursor = today

	// The first View runs before the first query returns, and a zero Grid
	// renders its zero time as "December -0001". An empty month reads as
	// every day unknown, which is all the model knows at that point.
	model.grid = calendar.Build(today.Year(), today.Month(), model.weekStart, nil, model.location)
	return model
}

// today returns the current date at midnight in the model's location.
func (m *Model) today() time.Time {
	now := m.now().In(m.location)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, m.location)
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.refresh(),
		m.pollStatus(),
		m.scheduleStatusPoll(),
		m.watchFeed(),
		// Asking costs one command and answers a question no fixed palette
		// can: the terminal's background decides which greys clear 3:1.
		tea.RequestBackgroundColor,
	)
}

// refresh reloads everything the current view shows.
//
// It runs as a command rather than inline so a slow query cannot block key
// handling, which is what makes a busy library still feel responsive. Only
// one runs at a time: a held arrow key repeats at the terminal's autorepeat
// rate, and each repeat that crosses a month asks for a reload. A request
// that arrives while one is in flight is remembered rather than dropped, so
// the grid still ends up showing the month the cursor is on.
func (m *Model) refresh() tea.Cmd {
	if m.loading {
		m.reload = true
		return nil
	}
	m.loading = true

	library := m.library
	channelIndex := m.channel
	month := m.month
	location := m.location

	return func() tea.Msg {
		msg := loadedMsg{}

		channels, err := library.Channels()
		if err != nil {
			msg.err = err
			return msg
		}
		msg.channels = channels
		if len(channels) == 0 {
			return msg
		}
		if channelIndex >= len(channels) {
			channelIndex = 0
		}

		from := month
		to := month.AddDate(0, 1, 0)

		// The grid shows padding days from the neighbouring months, so the
		// query has to reach a week either side or those cells would read
		// as unknown when their coverage is known.
		days, err := library.CoverageBetween(channels[channelIndex].ID,
			from.AddDate(0, 0, -7), to.AddDate(0, 0, 7), location)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.days = days

		recordings, err := library.RecordingsForChannel(channels[channelIndex].ID, from, to)
		if err != nil {
			msg.err = err
			return msg
		}
		msg.recordings = recordings

		// A budget that cannot be read is its own line in the header
		// rather than a failed refresh. The calendar is what the operator
		// opened, and it is still entirely readable without the gauge.
		msg.space, msg.spaceErr = library.SpaceUsage()
		return msg
	}
}

// pollStatus reads the recorder's condition, at most one query at a time.
func (m *Model) pollStatus() tea.Cmd {
	if m.polling {
		return nil
	}
	m.polling = true

	controller, key := m.controller, m.serviceKey
	return func() tea.Msg {
		status, err := serviceState(controller, key)
		return statusMsg{status: status, err: err}
	}
}

// scheduleStatusPoll asks for the next poll. A zero interval leaves the
// condition on display until something the operator did changes it.
func (m *Model) scheduleStatusPoll() tea.Cmd {
	if m.statusInterval <= 0 {
		return nil
	}
	return tea.Tick(m.statusInterval, func(time.Time) tea.Msg { return statusTickMsg{} })
}

// serviceState reads the recorder's condition, tolerating a missing
// controller.
func serviceState(controller Controller, key string) (Status, error) {
	if controller == nil {
		return Status{State: "unavailable"}, nil
	}
	status, err := controller.Status(key)
	if err != nil {
		return Status{State: "unknown"}, err
	}
	return status, nil
}

// ///////////////////////////////////////////////
// Update
// ///////////////////////////////////////////////

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.BackgroundColorMsg:
		m.styles = newStyles(msg.IsDark())
		return m, nil

	case toastMsg:
		return m, m.advanceToasts()

	case gapsMsg:
		m.applyGaps(msg)
		return m, nil

	case tea.MouseClickMsg:
		// A press rather than a release, so a click reads the regions of
		// the frame it landed on rather than the frame drawn since.
		return m, m.handleMouse(msg.Mouse(), false)

	case tea.MouseWheelMsg:
		return m, m.handleMouse(msg.Mouse(), true)

	case loadedMsg:
		return m, m.applyLoaded(msg)

	case statusMsg:
		m.polling = false
		m.status, m.statusErr = msg.status, msg.err
		return m, nil

	case statusTickMsg:
		// The next tick is scheduled whatever happens to this query, so a
		// slow service manager cannot end the chain and freeze the
		// condition on display without ever saying so.
		return m, tea.Batch(m.pollStatus(), m.scheduleStatusPoll())

	case controlMsg:
		return m, m.applyControl(msg)

	case candidatesMsg:
		return m, m.applyCandidates(msg)

	case purgedMsg:
		return m, m.applyPurged(msg)

	case settingsMsg:
		return m, m.applySettings(msg)

	case savedMsg:
		return m, m.applySaved(msg)

	case feedMsg:
		return m, m.applyFeed(msg)

	case tea.KeyPressMsg:
		// The press type rather than the tea.KeyMsg interface, so a release
		// on a terminal that reports them cannot act a second time.
		//
		// A line editor takes the whole key, not its name: it needs the
		// runes to insert them.
		if m.editing {
			return m, m.handleEditKey(msg)
		}
		return m, m.handleKey(msg.String())
	}
	return m, nil
}

// applyLoaded folds a completed refresh into the model.
func (m *Model) applyLoaded(msg loadedMsg) tea.Cmd {
	m.loading = false

	m.err = msg.err
	if msg.err == nil {
		m.channels = msg.channels
		if m.channel >= len(m.channels) {
			m.channel = 0
		}
		m.recordings = msg.recordings
		m.grid = calendar.Build(m.month.Year(), m.month.Month(), m.weekStart, msg.days, m.location)
		// The queue is built where the grid is, from the same value, so the
		// two cannot end up describing different months.
		m.queue = m.grid.Gaps()
		m.queueAt = min(m.queueAt, max(len(m.queue)-1, 0))
		m.space, m.spaceErr = msg.space, msg.spaceErr
		m.clampRecording()
	}

	if !m.reload {
		return nil
	}
	m.reload = false
	return m.refresh()
}

// applyControl folds a completed start or stop into the model.
func (m *Model) applyControl(msg controlMsg) tea.Cmd {
	m.controlling = false
	m.err = msg.err
	m.status, m.statusErr = msg.status, msg.query
	return nil
}

// handleMouse dispatches a click or a wheel.
//
// Gated on the editor the way keys are. A click routed past an open editor
// dispatches whatever key its region names, so a region carrying esc
// leaves the editor open over another pane, invisible, still taking every
// keystroke.
func (m *Model) handleMouse(mouse tea.Mouse, wheel bool) tea.Cmd {
	if m.editing {
		return nil
	}
	if wheel {
		return m.handleWheel(mouse.X, mouse.Y, mouse.Button == tea.MouseWheelUp)
	}
	if mouse.Button == tea.MouseLeft {
		return m.handleClick(mouse.X, mouse.Y)
	}
	return nil
}

// handleKey applies a key press and returns any command it triggers.
//
// Quitting is answered before the pane is consulted, so no pane can trap an
// operator by forgetting to handle it. Everything else belongs to whichever
// pane has focus.
func (m *Model) handleKey(key string) tea.Cmd {
	if key == "q" || key == "ctrl+c" {
		// The settings pane is the one place holding work that exists
		// nowhere else, and its own chip set offers save and quit side by
		// side. Quitting straight out of it drops every edit with nothing
		// said. Asked once: the second press goes through, because an
		// operator who meant it should not have to hunt for another key.
		//
		// ctrl+c is exempt. It means stop now, and a prompt in answer to
		// it is a program refusing to close.
		if key == "q" && m.dirty && !m.confirmQuit {
			m.confirmQuit = true
			return m.queueToast(
				"the settings pane has unsaved edits; press q again to discard them", m.styles.warn)
		}

		// A recorder this process started dies with the process, so it is
		// stopped deliberately here rather than left to be killed mid-write.
		m.stopRecorderOnQuit()
		m.quit = true
		return tea.Quit
	}
	// Any other key means the operator moved on, so the next q asks again.
	m.confirmQuit = false

	// A notice reports what the last key did, so the next key clears it.
	// Left standing, it would describe an action the operator has since
	// navigated away from.
	m.notice = ""

	// A modal is the top level, so it answers before the pane under it.
	if m.dayOpen {
		return m.handleDayKey(key)
	}

	switch m.pane {
	case panePurge:
		return m.handlePurgeKey(key)
	case paneSettings:
		return m.handleSettingsKey(key)
	default:
		return m.handleCalendarKey(key)
	}
}

// escape pops one level, and only one.
//
// A key that silently does nothing is its own small bug, so the last level
// says how to leave rather than leaving. Only q and ctrl+c quit.
func (m *Model) escape() tea.Cmd {
	switch {
	case m.dayOpen:
		m.dayOpen = false
	case m.focused == focusQueue:
		m.focused = focusCalendar
	default:
		return m.queueToast("q quits", m.styles.dim)
	}
	return nil
}

// handleCalendarKey applies a key press with the month grid focused.
func (m *Model) handleCalendarKey(key string) tea.Cmd {
	if cmd, handled := m.moveKey(key); handled {
		return cmd
	}

	switch key {
	case "esc":
		return m.escape()

	case "enter":
		m.dayOpen = true
		m.dayOffset = 0
		m.recording = 0
		// Loaded here as well as on a click. The gaps block is the only
		// thing that says whether a capture has holes and whether they were
		// filled, and a keyboard-only operator would otherwise be shown a
		// day that reads as having none. Nobody asked is not the same
		// answer as nobody found any, which is the distinction the modal
		// makes everywhere else.
		return m.loadGaps()

	case "tab", "shift+tab":
		// One key, both directions. A chip that offered the way in but not
		// the way out would name a key that does nothing on the second
		// press.
		if m.focused == focusQueue {
			m.focused = focusCalendar
			return nil
		}
		m.focused = focusQueue
		return m.enterQueue()

	case "t":
		return m.goToday()

	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		return m.selectChannel(int(key[0] - '1'))

	case "r":
		return m.refresh()

	case "s":
		return m.toggleRecorder()

	case "x":
		return m.openPurge()

	case "e":
		return m.openSettings()

	case "d":
		return m.toggleInProcess()
	}
	return nil
}

// moveKey handles the keys that move a selection, and reports whether it took
// the key.
//
// Split out because the arrows mean two different things depending on which
// panel the border weight says is focused: a day in the grid, a row in the
// queue.
func (m *Model) moveKey(key string) (tea.Cmd, bool) {
	if m.focused == focusQueue {
		switch key {
		case "up", "k":
			return m.moveQueue(-1), true
		case "down", "j":
			return m.moveQueue(1), true
		}
	}

	switch key {
	case "left", "h":
		return m.moveCursor(-1), true
	case "right", "l":
		return m.moveCursor(1), true
	case "up", "k":
		return m.moveCursor(-calendar.DaysPerWeek), true
	case "down", "j":
		return m.moveCursor(calendar.DaysPerWeek), true
	case "[", "pgup":
		return m.shiftMonth(-1), true
	case "]", "pgdown":
		return m.shiftMonth(1), true
	}
	return nil, false
}

// handleDayKey applies a key press with the selected day's recordings
// focused.
//
// The arrows move within the day rather than across days, which is what
// makes the pane worth entering: the grid's own arrows are still one key
// press away through esc.
func (m *Model) handleDayKey(key string) tea.Cmd {
	switch key {
	case "esc":
		return m.escape()

	case "up", "k":
		m.moveRecording(-1)
		return m.loadGaps()
	case "down", "j":
		m.moveRecording(1)
		return m.loadGaps()

	case "w":
		return m.toggleWatched()
	case "p":
		return m.togglePinned()

	case "R":
		return m.requestRecovery()

	case "r":
		return m.refresh()
	}
	return nil
}

// toggleInProcess starts or stops the recorder this process runs.
//
// The installed service is checked first, because two recorders against one
// library is the thing being prevented and the service can be running under
// another user. It is a courtesy rather than the guard: the store's session
// claim is what actually refuses, and it holds across machines.
func (m *Model) toggleInProcess() tea.Cmd {
	if m.recorder == nil {
		m.err = errors.New("only the installed recorder is reachable from here")
		return nil
	}

	if m.recorder.Running() {
		if err := m.recorder.Stop(); err != nil {
			m.err = err
			return nil
		}
		m.err, m.notice = nil, "the recorder in this window stopped"
		return m.pollStatus()
	}

	if m.status.State == stateRunning {
		m.err = errors.New("the installed recorder is already running; stop it with s first")
		return nil
	}

	if err := m.recorder.Start(); err != nil {
		m.err = err
		return nil
	}
	m.err, m.notice = nil, "recording in this window; it stops when the calendar closes"
	return m.pollStatus()
}

// stopRecorderOnQuit shuts an in-process recorder down before the process
// goes away, so a capture in flight is unwound rather than killed.
func (m *Model) stopRecorderOnQuit() {
	if m.recorder == nil || !m.recorder.Running() {
		return
	}
	if err := m.recorder.Stop(); err != nil {
		m.err = err
	}
}

// requestRecovery asks for the selected day to be fetched from an archive.
//
// The day is checked against calendar.Cell.Recoverable, which is the same
// rule the calendar's own work list uses. A day that is merely at risk has
// its bytes on disk already, and fetching it again would replace a real
// capture with a muted archive copy.
func (m *Model) requestRecovery() tea.Cmd {
	if m.recovery == nil {
		m.err = errors.New("recovery is not built yet")
		return nil
	}

	channel, ok := m.Channel()
	if !ok {
		return nil
	}
	cell, ok := m.SelectedCell()
	if !ok {
		return nil
	}

	if !cell.Recoverable() {
		m.err = fmt.Errorf("%s has no gap to recover", cell.Date.Format("Monday 2 January"))
		return nil
	}

	if err := m.recovery.Request(channel.ID, cell.Date); err != nil {
		m.err = err
		return nil
	}

	m.err = nil
	m.notice = "recovery requested for " + cell.Date.Format("Monday 2 January")
	return m.refresh()
}

// toggleWatched marks the recording under the cursor watched, or clears the
// mark.
func (m *Model) toggleWatched() tea.Cmd {
	return m.act(func(recording store.Recording) error {
		if recording.WatchedAt != nil {
			return m.actions.MarkWatched(recording.ID, nil)
		}
		now := m.now()
		return m.actions.MarkWatched(recording.ID, &now)
	})
}

// togglePinned protects the recording under the cursor from the purge list,
// or releases it.
func (m *Model) togglePinned() tea.Cmd {
	return m.act(func(recording store.Recording) error {
		return m.actions.SetPinned(recording.ID, !recording.Pinned)
	})
}

// act runs a write against the recording under the cursor and reloads.
//
// Reloading rather than editing the row in place is what makes the marker on
// screen the stored state. A write can lose to the daemon's own write lock,
// and a model that assumed success would show a mark the library does not
// hold.
func (m *Model) act(write func(store.Recording) error) tea.Cmd {
	if m.actions == nil {
		m.err = errors.New("this library is not writable from here")
		return nil
	}

	recording, ok := m.SelectedRecording()
	if !ok {
		return nil
	}

	if err := write(recording); err != nil {
		m.err = err
		return nil
	}
	m.err = nil
	return m.refresh()
}

// moveRecording walks the day's recordings, stopping at each end rather than
// wrapping. The list is short enough to see whole, so wrapping would hide
// which end it reached and nothing else.
func (m *Model) moveRecording(step int) {
	m.recording += step
	m.clampRecording()
}

// clampRecording keeps the day pane's cursor inside the list.
//
// A refresh can drop a row while the pane is open, so the bound is applied
// here rather than at each path that can shorten the list.
func (m *Model) clampRecording() {
	m.recording = min(max(m.recording, 0), max(len(m.SelectedRecordings())-1, 0))
}

// moveCursor walks the selection by whole days, following it across a month
// boundary so the grid never traps the cursor at an edge.
func (m *Model) moveCursor(days int) tea.Cmd {
	m.cursor = m.cursor.AddDate(0, 0, days)

	if m.cursor.Year() == m.month.Year() && m.cursor.Month() == m.month.Month() {
		return nil
	}
	m.month = time.Date(m.cursor.Year(), m.cursor.Month(), 1, 0, 0, 0, 0, m.location)
	return m.refresh()
}

// shiftMonth pages the calendar, keeping the cursor's day of month where it
// still exists and clamping to the last day where it does not.
func (m *Model) shiftMonth(months int) tea.Cmd {
	m.month = m.month.AddDate(0, months, 0)

	day := m.cursor.Day()
	last := m.month.AddDate(0, 1, -1).Day()
	if day > last {
		day = last
	}
	m.cursor = time.Date(m.month.Year(), m.month.Month(), day, 0, 0, 0, 0, m.location)
	return m.refresh()
}

// goToday returns to the current month and day.
func (m *Model) goToday() tea.Cmd {
	today := m.today()
	m.cursor = today
	m.month = time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, m.location)
	return m.refresh()
}

// toggleRecorder starts the recorder when it is idle and stops it when it
// is running.
//
// The start and the stop run as a command for the same reason a reload does:
// both reach the platform's service manager, which takes hundreds of
// milliseconds to seconds, and doing it inside Update would freeze every key
// including quit for that long. The condition is read back in the same command so the
// header reflects the action rather than the state before it.
func (m *Model) toggleRecorder() tea.Cmd {
	if m.controller == nil {
		m.err = fmt.Errorf("no startup registration is reachable; run 'stream-dvr install' first")
		return nil
	}
	if m.controlling {
		return nil
	}
	// The scheduler will not run a disabled registration, so asking it to
	// would report a failure that reads as a broken recorder rather than as
	// a setting the operator can change.
	if m.status.State == stateDisabled {
		m.err = errors.New("the registration is disabled; enable it before starting it")
		return nil
	}
	m.controlling = true
	m.err = nil

	controller, key := m.controller, m.serviceKey
	stop := m.status.State == stateRunning

	return func() tea.Msg {
		var err error
		if stop {
			err = controller.Stop(key)
		} else {
			err = controller.Start(key)
		}

		status, queryErr := serviceState(controller, key)
		return controlMsg{status: status, err: err, query: queryErr}
	}
}

// ///////////////////////////////////////////////
// Accessors
// ///////////////////////////////////////////////

// Cursor returns the selected day.
func (m *Model) Cursor() time.Time { return m.cursor }

// Month returns the month on display.
func (m *Model) Month() time.Time { return m.month }

// Grid returns the current calendar grid.
func (m *Model) Grid() calendar.Grid { return m.grid }

// Err returns the last error, if any.
func (m *Model) Err() error { return m.err }

// Quit reports whether the model asked to exit.
func (m *Model) Quit() bool { return m.quit }

// Channel returns the selected channel, and whether one exists.
func (m *Model) Channel() (store.Channel, bool) {
	if m.channel >= len(m.channels) {
		return store.Channel{}, false
	}
	return m.channels[m.channel], true
}

// SelectedRecording returns the recording under the day pane's cursor, and
// whether the day holds one at all.
//
// A day with no recordings is still worth opening: a missed day is exactly
// where recovery is requested, and by definition it has nothing in it.
func (m *Model) SelectedRecording() (store.Recording, bool) {
	recordings := m.SelectedRecordings()
	if m.recording < 0 || m.recording >= len(recordings) {
		return store.Recording{}, false
	}
	return recordings[m.recording], true
}

// SelectedRecordings returns the recordings that started on the selected
// day, oldest first.
func (m *Model) SelectedRecordings() []store.Recording {
	var selected []store.Recording

	want := m.cursor.Format("2006-01-02")
	for _, recording := range m.recordings {
		if recording.StartedAt.In(m.location).Format("2006-01-02") == want {
			selected = append(selected, recording)
		}
	}
	return selected
}

// SelectedCell returns the grid cell under the cursor.
func (m *Model) SelectedCell() (calendar.Cell, bool) {
	return m.grid.Find(m.cursor)
}

// ChannelLabel renders the selected channel for a header.
//
// Platform and name are stored text, seeded from a config file and carried in
// a library database that is meant to travel between machines, so they are no
// more trustworthy than a recording path. Rendering happens here rather than
// at the caller so no view can print the raw pair by forgetting.
func (m *Model) ChannelLabel() string {
	channel, ok := m.Channel()
	if !ok {
		return "no channels configured"
	}

	label := escape.Text(channel.Platform + "/" + channel.Name)
	if len(m.channels) > 1 {
		label = fmt.Sprintf("%s  (%d of %d)", label, m.channel+1, len(m.channels))
	}
	return label
}
