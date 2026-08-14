package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"zach.tools/go/stream-dvr/internal/store"
)

func TestDayModal_SaysWhetherAnythingWasWatching(t *testing.T) {
	// The fact that decides what a gap means. A day nothing aired on and a
	// day nothing was listening on are the same picture and opposite
	// problems, and the grid cannot tell them apart.
	tests := []struct {
		name    string
		watched bool
		want    string
	}{
		{name: "a recorder covered it", watched: true, want: "a recorder was watching"},
		{name: "nothing did", watched: false, want: "nothing was watching this day"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lib := dayLibrary()
			lib.days = []store.Day{{
				Date:       time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC),
				State:      store.CoverageLive,
				Broadcasts: 1, Captured: 1, Watched: tt.watched,
			}}

			view := renderDay(t, newModel(t, lib, nil))

			if !strings.Contains(view, tt.want) {
				t.Errorf("the modal does not say %q:\n%s", tt.want, view)
			}
		})
	}
}

func TestDayModal_RendersEveryStoredFieldOfTheSelectedRecording(t *testing.T) {
	lib := dayLibrary()
	lib.recordings = []store.Recording{{
		ID: 1, Path: "ExampleChannel/2026/named.mkv",
		State: store.StateComplete, Origin: store.OriginLive,
		Bytes: 4 << 30, Duration: 2*time.Hour + 18*time.Minute,
		MediaDuration: 2*time.Hour + 11*time.Minute,
		StartedAt:     time.Date(2026, 3, 4, 19, 2, 0, 0, time.UTC),
	}}

	view := renderDay(t, newModel(t, lib, nil))

	for _, want := range []string{"state", "origin", "ran", "wall", "media", "muted", "pinned"} {
		if !strings.Contains(view, want) {
			t.Errorf("the modal drops the %q field:\n%s", want, view)
		}
	}
}

func TestDayModal_StatesTheMediaShortfallInWords(t *testing.T) {
	// Two hours of media inside a two-and-a-quarter-hour capture is an ad
	// break, and the number alone does not say that is what it is.
	lib := dayLibrary()
	lib.recordings = []store.Recording{{
		ID: 1, Path: "named.mkv", State: store.StateComplete,
		Duration: 2*time.Hour + 18*time.Minute, MediaDuration: 2*time.Hour + 11*time.Minute,
		StartedAt: time.Date(2026, 3, 4, 19, 2, 0, 0, time.UTC),
	}}

	view := renderDay(t, newModel(t, lib, nil))

	if !strings.Contains(view, "short of the wall clock") {
		t.Errorf("the modal reports the shortfall as a number alone:\n%s", view)
	}
}

func TestMutedLine_NobodyAskedIsNotNobodyFound(t *testing.T) {
	// Nil means nobody measured, which is every live capture and every
	// machine with no platform session. Rendering that as none would say a
	// recovery is not worth running.
	none := time.Duration(0)
	some := 4 * time.Minute

	tests := []struct {
		name string
		set  *time.Duration
		want string
	}{
		{name: "nobody asked", set: nil, want: "not measured"},
		{name: "asked and found none", set: &none, want: "none"},
		{name: "asked and found some", set: &some, want: "0h04m00s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mutedLine(store.Recording{MutedDuration: tt.set})
			if got != tt.want {
				t.Errorf("mutedLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStandingFault_SaysRecoveryIsNotWiredWhereItIsOffered(t *testing.T) {
	// A standing condition, stated where the action it blocks is offered. A
	// toast would expire while the condition was still true.
	view := renderDay(t, newModel(t, dayLibrary(), nil))

	if !strings.Contains(view, "recovery is not wired up") {
		t.Errorf("the modal offers a recover key with no archive client and says nothing:\n%s", view)
	}
}

func TestStateLabel_SaysWhatAParkedCaptureIsWaitingOn(t *testing.T) {
	// A parked recording is the one state the operator has to act on, so it
	// says why rather than naming the row's enum value.
	tests := []struct {
		state store.State
		want  string
	}{
		{state: store.StateAwaitingMetadata, want: "waiting on metadata"},
		{state: store.StateAwaitingFile, want: "waiting on a locked file"},
		{state: store.StateAwaitingFinalize, want: "not filed yet"},
		{state: store.StateFailed, want: "failed"},
		{state: store.StateMissing, want: "the file is gone"},
		{state: store.State("invented"), want: "invented"},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := stateLabel(tt.state); got != tt.want {
				t.Errorf("stateLabel(%q) = %q, want %q", tt.state, got, tt.want)
			}
		})
	}
}

func TestDayModal_KeepsTheRecorderStripVisibleBehindIt(t *testing.T) {
	// Centred over the panels alone, so the recorder keeps reporting while a
	// day is read.
	model := newModel(t, dayLibrary(), nil)
	view := renderDay(t, model)

	if !strings.Contains(view, "recorder") {
		t.Errorf("the modal covered the recorder strip:\n%s", view)
	}
	if model.frame.Modal.Y <= model.frame.AppBar.Y {
		t.Errorf("the modal at %+v covers the app bar", model.frame.Modal)
	}
	if bottom := model.frame.Modal.Y + model.frame.Modal.H; bottom > model.frame.Recorder.Y {
		t.Errorf("the modal reaches row %d, past the recorder strip at %d",
			bottom, model.frame.Recorder.Y)
	}
}

func TestShortDuration_AnUnmeasuredSpanIsNotZero(t *testing.T) {
	if got := shortDuration(0); got != "-" {
		t.Errorf("shortDuration(0) = %q, want it to read as unmeasured", got)
	}
	if got := longDuration(0); got != "-" {
		t.Errorf("longDuration(0) = %q, want it to read as unmeasured", got)
	}
}

func TestRegisterModal_ClicksTheRecordingUnderThePointer(t *testing.T) {
	// The registry and the renderer count the rows above the first
	// recording separately. One row of disagreement makes every click
	// select the recording below the one clicked, and w or p then writes
	// to it: pinning is what protects a capture from the purge list, so
	// the operator protects the wrong file and leaves the intended one
	// purgeable.
	model := everyPane(t)
	press(t, model, "enter")
	_ = model.View()

	recordings := model.SelectedRecordings()
	if len(recordings) < 2 {
		t.Skipf("the fixture day holds %d recordings, want at least two", len(recordings))
	}

	inner := model.frame.Modal.inner()
	for i := range recordings {
		row := inner.Y + recordingsTop + i - model.dayOffset
		if row < inner.Y || row >= inner.Y+inner.H {
			continue
		}

		hit, ok := model.hits.find(inner.X+1, row)
		if !ok || hit.Key != "recording" {
			continue
		}
		if got := hit.Value; got != fmt.Sprintf("%d", i) {
			t.Errorf("the region on row %d carries recording %s, want %d", row, got, i)
		}
	}
}

func TestRegisterModal_RegistersNoRecordingOnAScrollHintRow(t *testing.T) {
	// The scroller writes its counts over the first and last visible rows.
	// A region left under one sells a click on "3 more above" as a click
	// on the recording whose place it took.
	model := everyPane(t)
	press(t, model, "enter")
	model.dayOffset = 1
	_ = model.View()

	inner := model.frame.Modal.inner()
	if len(model.SelectedRecordings()) == 0 {
		t.Skip("the fixture day holds no recordings")
	}

	if hit, ok := model.hits.find(inner.X+1, inner.Y); ok && hit.Key == "recording" {
		t.Errorf("the top row carries recording %s, want the scroll hint to own it", hit.Value)
	}
}

func TestRecordingFields_SplitsThePathBeforeEscapingIt(t *testing.T) {
	// escape.Text renders a path carrying anything unprintable as a quoted
	// literal. Cutting that at a byte index lands inside it, and what comes
	// back opens like plain text and closes with a stray quote: a fourth
	// shape, where the whole point of the three is that a reader can tell
	// them apart by the first byte.
	model := everyPane(t)
	recording := store.Recording{
		Path:      "vods/examplechannel/2026-03-04/ep\x1b[2J.mkv",
		StartedAt: model.Cursor(),
		Bytes:     1 << 20,
	}

	for _, line := range model.recordingFields(recording, 120) {
		trimmed := strings.TrimSpace(ansi.Strip(line))
		if !strings.Contains(trimmed, `"`) {
			continue
		}
		// Anything rendered as a quoted literal has to read back as one.
		start := strings.Index(trimmed, `"`)
		if _, err := strconv.Unquote(trimmed[start:]); err != nil {
			t.Errorf("field %q does not read back as a rendering: %v", trimmed, err)
		}
	}
}
