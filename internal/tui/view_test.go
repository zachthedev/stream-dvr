package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"zach.tools/go/stream-dvr/internal/calendar"
	"zach.tools/go/stream-dvr/internal/store"
)

// errAlwaysFails is a fixed error for view assertions.
type errAlwaysFails struct{}

// cellVerticals is the light run between two cells, which every cell row has
// one of per column.
const cellVerticals = string(lightVertical)

// cursorCue is the heavy vertical the cursor draws to the left of its cell.
//
// Border weight rather than reverse video, so it survives a terminal with no
// colour at all. It is also what the panel frame uses for focus one level up,
// so the screen needs no second vocabulary for the same idea.
const cursorCue = string(heavyVertical)

// daysInFixtureMonth is how many days the calendar fixture's month holds.
const daysInFixtureMonth = 31

// markedCaret is what a list draws beside the row its keys act on.
const markedCaret = "❯ "

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// coverageDay builds a March 2026 coverage entry.
func coverageDay(dayOfMonth int, state store.Coverage) store.Day {
	return store.Day{
		Date:  time.Date(2026, 3, dayOfMonth, 0, 0, 0, 0, time.UTC),
		State: state,
	}
}

// render returns the model's view with colour stripped, so assertions read
// the text rather than the escape codes around it.
func render(t *testing.T, model *Model) string {
	t.Helper()

	view := model.View().Content
	if view == "" {
		t.Fatal("View() is empty")
	}
	return stripANSI(view)
}

// stripANSI removes escape sequences from rendered output.
//
// Through the same parser the renderer writes with. A hand-rolled version
// that skipped from an escape byte to the next "m" swallowed everything
// between a sequence that does not end in one and the next style, which
// reads exactly like a pane that rendered nothing.
func stripANSI(text string) string {
	return ansi.Strip(text)
}

// renderDay opens the day modal and returns what it drew.
//
// The day is a modal over the two panels rather than a pane beside them, so
// a test that wants to read a recording opens it the way an operator does.
func renderDay(t *testing.T, model *Model) string {
	t.Helper()

	pressNamed(t, model, tea.KeyEnter)
	if !model.dayOpen {
		t.Fatal("enter did not open the day")
	}
	return render(t, model)
}

// squash collapses runs of spaces, for an assertion about what a line pairs
// rather than about the columns it pairs them in.
func squash(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// ///////////////////////////////////////////////
// Untrusted stored text
// ///////////////////////////////////////////////

func TestView_ControlCharactersInAStoredPathNeverReachTheTerminal(t *testing.T) {
	// A path is stored text, not text this build just produced.
	// internal/naming strips control characters from every name it renders,
	// but that is one route into the library and not the only one, and a
	// filename may legally carry an escape byte on Linux.
	lib := library()
	lib.days = []store.Day{coverageDay(4, store.CoverageLive)}
	lib.recordings = []store.Recording{{
		ID: 1, ChannelID: 1, Path: "evil\x1b[2Jname\a.mkv",
		State: store.StateComplete, Origin: store.OriginLive,
		StartedAt: time.Date(2026, 3, 4, 20, 0, 0, 0, time.UTC),
	}}

	// The raw view, not the stripped one: stripANSI would remove exactly
	// the bytes this test is about.
	model := newModel(t, lib, nil)
	pressNamed(t, model, tea.KeyEnter)
	view := model.View().Content

	// Checked against the raw view, because stripANSI skips to the next 'm'
	// and a clear-screen sequence has none, so it would swallow the rest of
	// the pane and make an unescaped path look like a missing one.
	if !strings.Contains(view, "name") {
		t.Fatalf("the recording did not render at all, so this proved nothing:\n%q", view)
	}
	if strings.Contains(view, "\x1b[2J") {
		t.Error("a clear-screen sequence from a stored path reached the rendered view")
	}
	if strings.Contains(view, "\a") {
		t.Error("a bell from a stored path reached the rendered view")
	}
}

func TestView_ControlCharactersInAChannelNameNeverReachTheTerminal(t *testing.T) {
	// A channel's platform and name are seeded from a config file and carried
	// in a library database designed to travel between machines, so the
	// header is no more trustworthy than the recording path two lines below
	// it, which is already escaped.
	lib := library()
	lib.channels = []store.Channel{{
		ID: 1, Platform: "twitch", Name: "examplechannel\x1b]0;PWNED\a\x1b[2J",
	}}

	// The raw view, not the stripped one: stripANSI skips to the next 'm',
	// and neither an OSC title sequence nor a clear-screen has one, so it
	// would swallow the rest of the pane and hide the answer.
	view := newModel(t, lib, nil).View().Content

	if !strings.Contains(view, "examplechannel") {
		t.Fatalf("the channel did not render at all, so this proved nothing:\n%q", view)
	}
	if strings.Contains(view, "\x1b]0;") {
		t.Error("an OSC window-title sequence from a channel name reached the rendered view")
	}
	if strings.Contains(view, "\x1b[2J") {
		t.Error("a clear-screen sequence from a channel name reached the rendered view")
	}
	if strings.Contains(view, "\a") {
		t.Error("a bell from a channel name reached the rendered view")
	}
}

func TestView_ControlCharactersInAnErrorNeverReachTheTerminal(t *testing.T) {
	// An error string can carry a stored path or a remote message, so the
	// error pane is no more trustworthy than the recording list.
	lib := library()
	lib.channelsErr = errors.New("opening \x1b[2Jevil\a.mkv: permission denied")

	view := newModel(t, lib, nil).View().Content

	if !strings.Contains(view, "permission denied") {
		t.Fatalf("the error did not render at all, so this proved nothing:\n%q", view)
	}
	if strings.Contains(view, "\x1b[2J") {
		t.Error("a clear-screen sequence from an error reached the rendered view")
	}
	if strings.Contains(view, "\a") {
		t.Error("a bell from an error reached the rendered view")
	}
}

// ///////////////////////////////////////////////
// Structure
// ///////////////////////////////////////////////

func TestView_ShowsTheMonthAndChannel(t *testing.T) {
	view := render(t, newModel(t, library(), nil))

	for _, want := range []string{"March 2026", "twitch/examplechannel"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() missing %q:\n%s", want, view)
		}
	}
}

func TestView_ShowsEveryDayOfTheMonth(t *testing.T) {
	view := render(t, newModel(t, library(), nil))

	// The header and footer carry digits too, so this checks the grid rows
	// specifically.
	grid := gridLines(view)
	joined := strings.Join(grid, " ")
	for day := 1; day <= 31; day++ {
		if !strings.Contains(joined, itoa(day)) {
			t.Errorf("grid is missing day %d:\n%s", day, strings.Join(grid, "\n"))
		}
	}
}

// gridLines returns the rendered calendar rows.
//
// A cell row carries the eight verticals of the lattice plus the panel's own
// two, which is what separates it from a rule row with none and from a legend
// line that also holds digits.
func gridLines(view string) []string {
	var rows []string
	for line := range strings.SplitSeq(view, "\n") {
		if strings.Count(line, cellVerticals) >= calendar.DaysPerWeek+1 {
			rows = append(rows, line)
		}
	}
	return rows
}

// itoa renders a day number.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func TestView_ShowsWeekdayHeadings(t *testing.T) {
	view := render(t, newModel(t, library(), nil))

	for _, want := range []string{"Su", "Mo", "Sa"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() missing weekday heading %q", want)
		}
	}
}

func TestView_ListsTheKeys(t *testing.T) {
	view := render(t, newModel(t, library(), nil))

	for _, want := range []string{"month", "to recover", "quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() missing key hint %q", want)
		}
	}
}

// ///////////////////////////////////////////////
// Reading the grid without colour
// ///////////////////////////////////////////////

func TestView_TheCursorSurvivesATerminalWithNoColour(t *testing.T) {
	// Reverse video is the only thing that marked the selection, and lipgloss
	// emits nothing at all under NO_COLOR, through a pipe, or on a 1-bit
	// terminal. An operator who cannot see which day they are about to act on
	// has an unusable calendar rather than a plain one.
	lib := library()
	model := newModel(t, lib, nil)

	row := selectedRow(t, model)
	if !strings.Contains(row, cursorCue+"  4") {
		t.Errorf("the selected day carries no character cue:\n%s", row)
	}

	// Moving the cursor has to move the cue with it, or it is decoration.
	pressNamed(t, model, tea.KeyRight)

	row = selectedRow(t, model)
	if strings.Contains(row, cursorCue+"  4") {
		t.Errorf("the cue stayed on the old day after the cursor moved:\n%s", row)
	}
	if !strings.Contains(row, cursorCue+"  5") {
		t.Errorf("the cue did not follow the cursor:\n%s", row)
	}
}

func TestView_CoverageSurvivesATerminalWithNoColour(t *testing.T) {
	// Colour alone leaves every day identical without it, and missed against
	// live is ANSI 1 against ANSI 2, the pair a deuteranopic reader is most
	// likely to read the same even with colour.
	for _, coverage := range coverageStates {
		t.Run(string(coverage), func(t *testing.T) {
			lib := library()
			lib.days = []store.Day{coverageDay(10, coverage)}

			row := gridRowContaining(t, render(t, newModel(t, lib, nil)), "10")
			want := "10 " + glyphFor(coverage)
			if !strings.Contains(row, want) {
				t.Errorf("day 10 does not carry %q for %q:\n%s", want, coverage, row)
			}
		})
	}
}

func TestView_EveryCoverageStateHasItsOwnGlyph(t *testing.T) {
	// A shared glyph is no better than a shared colour: two states the
	// operator has to act on differently would read the same.
	seen := make(map[string]store.Coverage, len(coverageStates))
	for _, coverage := range coverageStates {
		glyph := glyphFor(coverage)
		if before, clash := seen[glyph]; clash {
			t.Errorf("%q and %q both render as %q", before, coverage, glyph)
			continue
		}
		seen[glyph] = coverage
	}
}

func TestView_PaddingDaysAreMarkedWithoutColour(t *testing.T) {
	// A row reading "26 27 28 29 30 31 1" gives an operator nothing to
	// separate the end of one month from the start of the next.
	view := render(t, newModel(t, library(), nil))

	last := gridLines(view)[len(gridLines(view))-1]
	if !strings.Contains(last, "(") || !strings.Contains(last, ")") {
		t.Errorf("days from the next month carry no character cue:\n%s", last)
	}
	if strings.Count(last, "(") != 4 {
		t.Errorf("marked %d days as belonging elsewhere, want the 4 April days:\n%s",
			strings.Count(last, "("), last)
	}
}

func TestView_TheLegendNamesEveryGlyph(t *testing.T) {
	// The legend is where an operator learns which mark means which state,
	// so a glyph missing from it is a mark with no meaning attached.
	// One day per state, built from the list rather than typed out. A
	// fixture extended by hand stops covering the newest state on the day
	// somebody adds one, which is the day this test exists for.
	lib := library()
	for i, coverage := range coverageStates {
		lib.days = append(lib.days, coverageDay(2+i, coverage))
	}

	view := squash(render(t, newModel(t, lib, nil)))
	for _, coverage := range coverageStates {
		// The count is asserted rather than skipped over. A line pairing the
		// right glyph with the right label and reporting zero days is exactly
		// what a state missing from the count table looks like, and a pattern
		// matching any digit passes on it.
		//
		// One day each, and every day the fixture does not name falls to
		// unknown. Derived rather than typed, so a state added to the store
		// moves the unknown count with no help from anybody.
		count := 1
		if coverage == store.CoverageUnknown {
			count = daysInFixtureMonth - (len(coverageStates) - 1)
		}
		want := fmt.Sprintf("%s %d %s", glyphFor(coverage), count, labelFor(coverage))
		if !strings.Contains(view, want) {
			t.Errorf("the legend does not carry %q:\n%s", want, view)
		}
	}
}

func TestView_TheDetailPaneCarriesTheGlyph(t *testing.T) {
	lib := library()
	lib.days = []store.Day{coverageDay(4, store.CoverageMissed)}

	view := renderDay(t, newModel(t, lib, nil))
	want := glyphFor(store.CoverageMissed) + " " + labelFor(store.CoverageMissed)
	if !strings.Contains(view, want) {
		t.Errorf("the detail pane does not carry %q:\n%s", want, view)
	}
}

// selectedRow returns the grid row holding the cursor.
func selectedRow(t *testing.T, model *Model) string {
	t.Helper()

	for _, row := range gridLines(render(t, model)) {
		if strings.Contains(row, cursorCue) {
			return row
		}
	}
	t.Fatalf("no grid row carries the cursor:\n%s", render(t, model))
	return ""
}

// gridRowContaining returns the grid row holding a day number.
func gridRowContaining(t *testing.T, view, day string) string {
	t.Helper()

	for _, row := range gridLines(view) {
		if strings.Contains(row, day) {
			return row
		}
	}
	t.Fatalf("no grid row holds day %s:\n%s", day, view)
	return ""
}

// ///////////////////////////////////////////////
// The legend
// ///////////////////////////////////////////////

func TestView_AlwaysShowsTheMissedCount(t *testing.T) {
	// A month with nothing missed must still say so. Hiding the count
	// when it is zero means the absence of the word "missed" has to be
	// read as good news, which is exactly backwards.
	view := render(t, newModel(t, library(), nil))

	if !strings.Contains(view, "missed") {
		t.Errorf("View() omits the missed count:\n%s", view)
	}
}

func TestView_CountsCoverageStates(t *testing.T) {
	lib := library()
	lib.days = []store.Day{
		coverageDay(3, store.CoverageMissed),
		coverageDay(4, store.CoverageMissed),
		coverageDay(5, store.CoverageLive),
	}

	view := squash(render(t, newModel(t, lib, nil)))

	if !strings.Contains(view, "2 missed") {
		t.Errorf("View() does not report 2 missed days:\n%s", view)
	}
	if !strings.Contains(view, "1 recorded live") {
		t.Errorf("View() does not report the recorded day:\n%s", view)
	}
}

func TestView_DistinguishesUnwatchedFromQuiet(t *testing.T) {
	// The whole point of the unknown state is that it does not read as
	// reassurance, so the label has to say what it means.
	lib := library()
	lib.days = []store.Day{coverageDay(3, store.CoverageNoStream)}

	view := squash(render(t, newModel(t, lib, nil)))

	if !strings.Contains(view, "not watched") {
		t.Errorf("View() does not label unwatched days:\n%s", view)
	}
	if !strings.Contains(view, "no broadcast") {
		t.Errorf("View() does not label quiet days:\n%s", view)
	}
}

// ///////////////////////////////////////////////
// The detail pane
// ///////////////////////////////////////////////

func TestView_DescribesTheSelectedDay(t *testing.T) {
	lib := library()
	lib.days = []store.Day{{
		Date:       time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC),
		State:      store.CoveragePartial,
		Broadcasts: 2,
		Captured:   1,
	}}

	view := renderDay(t, newModel(t, lib, nil))

	if !strings.Contains(view, "Wednesday 4 March") {
		t.Errorf("View() does not name the selected day:\n%s", view)
	}
	if !strings.Contains(view, "1 of 2 broadcasts captured") {
		t.Errorf("View() does not summarize the day's capture:\n%s", view)
	}
}

func TestView_ListsTheDaysRecordings(t *testing.T) {
	lib := library()
	lib.recordings = []store.Recording{{
		ID:        1,
		Path:      "ExampleChannel/2026/named.mkv",
		State:     store.StateComplete,
		Bytes:     13_450_000_000,
		StartedAt: time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC),
	}}

	view := renderDay(t, newModel(t, lib, nil))

	for _, want := range []string{"21:15", "named.mkv", "13.45GB"} {
		if !strings.Contains(view, want) {
			t.Errorf("View() missing %q:\n%s", want, view)
		}
	}
}

func TestView_FlagsARecordingWaitingOnMetadata(t *testing.T) {
	// A parked recording is the one state the operator has to act on, so
	// it says why rather than showing a bare path.
	lib := library()
	lib.recordings = []store.Recording{{
		ID:        1,
		Path:      "incoming/twitch-examplechannel-1.mkv",
		State:     store.StateAwaitingMetadata,
		StartedAt: time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC),
	}}

	view := renderDay(t, newModel(t, lib, nil))

	if !strings.Contains(view, "waiting on metadata") {
		t.Errorf("View() does not flag the parked recording:\n%s", view)
	}
}

func TestView_MarksTheRecordingUnderTheDayPaneCursor(t *testing.T) {
	// The marker is the only thing on screen that says which recording w, p
	// and the purge will act on.
	model := newModel(t, dayLibrary(), nil)

	before := render(t, model)
	if strings.Contains(before, markedCaret) {
		t.Errorf("the calendar marks a recording it cannot act on:\n%s", before)
	}

	pressNamed(t, model, tea.KeyEnter)
	pressNamed(t, model, tea.KeyDown)
	after := render(t, model)

	marked := markedRecordings(after)
	if len(marked) != 1 {
		t.Fatalf("%d recordings are marked, want exactly 1:\n%s", len(marked), after)
	}
	if !strings.Contains(marked[0], "second.mkv") {
		t.Errorf("the marker sits on %q, want the second recording", marked[0])
	}
}

func TestMarksOn_KeepsTheColumnWidthWhateverIsSet(t *testing.T) {
	watched := time.Date(2026, 3, 5, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		recording store.Recording
		want      string
	}{
		{name: "nothing set", recording: store.Recording{}, want: "  "},
		{name: "watched", recording: store.Recording{WatchedAt: &watched}, want: glyphWatched + " "},
		{name: "pinned", recording: store.Recording{Pinned: true}, want: " " + glyphPinned},
		{
			name:      "both",
			recording: store.Recording{WatchedAt: &watched, Pinned: true},
			want:      glyphWatched + glyphPinned,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The sizes below the column step out of true the moment one
			// row renders narrower than another.
			if got := marksOn(tc.recording); got != tc.want {
				t.Errorf("marksOn() = %q, want %q", got, tc.want)
			}
		})
	}
}

// markedRecordings returns the rendered lines carrying the day modal's
// cursor.
func markedRecordings(view string) []string {
	var marked []string

	for line := range strings.SplitSeq(view, "\n") {
		if strings.Contains(line, markedCaret) {
			marked = append(marked, line)
		}
	}
	return marked
}

// ///////////////////////////////////////////////
// Errors and controls
// ///////////////////////////////////////////////

func TestView_ShowsAnError(t *testing.T) {
	lib := library()
	lib.channelsErr = errAlwaysFails{}

	view := render(t, newModel(t, lib, nil))

	if !strings.Contains(view, "database is locked") {
		t.Errorf("View() does not surface the error:\n%s", view)
	}
}

func (errAlwaysFails) Error() string { return "database is locked" }

func TestView_ControlKeyReflectsTheRecorderState(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  string
	}{
		{name: "idle offers start", state: "installed", want: "s start"},
		{name: "running offers stop", state: "running", want: "s stop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := newModel(t, library(), &fakeController{status: Status{State: tt.state}})

			if view := render(t, model); !strings.Contains(view, tt.want) {
				t.Errorf("View() does not offer %q:\n%s", tt.want, view)
			}
		})
	}
}

func TestView_ReportsTheRecorderState(t *testing.T) {
	model := newModel(t, library(), &fakeController{status: Status{State: "running"}})

	if view := render(t, model); !strings.Contains(view, "recorder running") {
		t.Errorf("View() does not report the recorder state:\n%s", view)
	}
}

func TestView_SaysWhyTheRecorderConditionIsUnknown(t *testing.T) {
	// "unknown" on its own leaves the operator with a recorder in an unnamed
	// condition and nothing to act on. The service manager said why.
	model := newModel(t, library(), &fakeController{statusErr: errors.New("access is denied")})

	view := render(t, model)
	if !strings.Contains(view, "recorder unknown") {
		t.Fatalf("View() does not report the unknown condition:\n%s", view)
	}
	if !strings.Contains(view, "access is denied") {
		t.Errorf("View() drops the reason the query failed:\n%s", view)
	}
}

func TestView_ControlCharactersInAStatusFailureNeverReachTheTerminal(t *testing.T) {
	// The reason comes from the platform's service manager, so it is no
	// more trustworthy than a stored path.
	model := newModel(t, library(), &fakeController{statusErr: errors.New("denied\x1b[2Jhere\a")})

	view := model.View().Content
	if !strings.Contains(view, "here") {
		t.Fatalf("the reason did not render at all, so this proved nothing:\n%q", view)
	}
	if strings.Contains(view, "\x1b[2J") {
		t.Error("a clear-screen sequence from a status failure reached the rendered view")
	}
	if strings.Contains(view, "\a") {
		t.Error("a bell from a status failure reached the rendered view")
	}
}

func TestView_EmptyAfterQuitting(t *testing.T) {
	// Leaving the alternate screen must not repaint a frame on the way
	// out.
	model := newModel(t, library(), nil)
	_, _ = model.Update(keyPress("q"))

	if got := model.View().Content; got != "" {
		t.Errorf("View() = %q after quitting, want empty", got)
	}
}

// ///////////////////////////////////////////////
// Space gauge
// ///////////////////////////////////////////////

func TestVolume(t *testing.T) {
	// The budget was otherwise visible only at a refusal, which is the one
	// moment it is too late to act on. What the header has to carry is the
	// position, and whether the next broadcast is at risk.
	const gigabyte = 1_000_000_000

	tests := []struct {
		name  string
		space Space
		err   error
		want  []string
		avoid []string
	}{
		{
			name:  "under the cap",
			space: Space{Held: 50 * gigabyte, Cap: 100 * gigabyte, Free: 900 * gigabyte, Level: "ok"},
			want:  []string{"50GB", "100GB", "50%"},
			avoid: []string{"running low", "refused"},
		},
		{
			name:  "running low",
			space: Space{Held: 95 * gigabyte, Cap: 100 * gigabyte, Free: 900 * gigabyte, Level: spaceLow},
			want:  []string{"95%", "running low"},
			avoid: []string{"refused"},
		},
		{
			// Colour is never the only signal. A gauge that turned red and
			// said nothing leaves an operator on a monochrome terminal with
			// no warning at all.
			name:  "at the limit",
			space: Space{Held: 99 * gigabyte, Cap: 100 * gigabyte, Free: 900 * gigabyte, Level: spaceCritical},
			want:  []string{"99%", "the next broadcast will be refused"},
		},
		{
			// An uncapped library has no percentage to report, and a bare
			// "0%" would read as an empty library rather than no cap.
			name:  "no cap set",
			space: Space{Held: 50 * gigabyte, Cap: 0, Free: 900 * gigabyte, Level: "ok"},
			want:  []string{"50GB held", "900GB free"},
			avoid: []string{"%"},
		},
		{
			// The calendar is what the operator opened and it is entirely
			// readable without the gauge, so a failure costs the gauge
			// rather than the screen.
			name: "the budget could not be read",
			err:  errors.New("volume is offline"),
			want: []string{"space unknown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(t, &fakeLibrary{}, nil)
			m.space, m.spaceErr = tt.space, tt.err

			got := stripANSI(m.volume())

			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("volume() = %q, want it to carry %q", got, want)
				}
			}
			for _, avoid := range tt.avoid {
				if strings.Contains(got, avoid) {
					t.Errorf("volume() = %q, want it not to carry %q", got, avoid)
				}
			}
		})
	}
}

func TestPercentOf(t *testing.T) {
	tests := []struct {
		name  string
		held  int64
		limit int64
		want  int64
	}{
		{name: "empty", held: 0, limit: 100, want: 0},
		{name: "half", held: 50, limit: 100, want: 50},
		// Rounded down, so a library one byte short of its cap never reads
		// as the full 100 an operator would take for "already refusing".
		// The gap has to be smaller than a whole percent to say anything:
		// at 99 of 100 both roundings agree, and the row proves nothing.
		{name: "one byte short rounds down", held: 99_999_999_999, limit: 100_000_000_000, want: 99},
		{name: "a fraction over a percent still rounds down", held: 995, limit: 1000, want: 99},
		{name: "exactly full", held: 100, limit: 100, want: 100},
		// Over the cap is reachable: nothing stops a capture that was
		// admitted from finishing above it.
		{name: "over the cap clamps", held: 150, limit: 100, want: 100},
		{name: "no cap", held: 50, limit: 0, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentOf(tt.held, tt.limit); got != tt.want {
				t.Errorf("percentOf(%d, %d) = %d, want %d", tt.held, tt.limit, got, tt.want)
			}
		})
	}
}

func TestView_CarriesTheSpaceGauge(t *testing.T) {
	// The gauge is worth nothing unless a refresh loads it and the header
	// prints it, so this is the test that proves both ends are connected
	// rather than that spaceLine renders in isolation.
	const gigabyte = 1_000_000_000
	library := &fakeLibrary{
		channels: []store.Channel{{ID: 1, Platform: "twitch", Name: "examplechannel"}},
		space:    Space{Held: 95 * gigabyte, Cap: 100 * gigabyte, Free: 900 * gigabyte, Level: spaceLow},
	}

	got := stripANSI(newModel(t, library, nil).View().Content)

	for _, want := range []string{"95GB", "100GB", "running low"} {
		if !strings.Contains(got, want) {
			t.Errorf("View() does not carry %q in its app bar:\n%s", want, got)
		}
	}
}
