package tui

import (
	"fmt"
	"strings"
	"time"

	"zach.tools/go/stream-dvr/internal/calendar"
	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Columns
// ///////////////////////////////////////////////

// The recordings table's fixed columns. Only the file column varies, so it
// takes whatever the modal has left and a long name is cut in the middle
// rather than at the extension.
const (
	recordingStart = 5
	recordingHeld  = 7
	recordingHolds = 7
	recordingSize  = 9
	recordingMarks = 2
	recordingFixed = queueCaret + recordingStart + recordingHeld +
		recordingHolds + recordingSize + recordingMarks
	recordingGaps = 5 * queueColumnGap
	// fieldLabelCols is wide enough for the longest label a recording
	// carries, so none of them runs into the value beside it.
	fieldLabelCols = 13
	// recordingsTop is how many rows the day modal draws above its first
	// recording: the summary, a blank, and the two header rows.
	//
	// Read by the renderer and by the click registry, so the row a
	// recording is drawn on and the row a click resolves to cannot
	// disagree.
	recordingsTop = 4
)

// ///////////////////////////////////////////////
// The day
// ///////////////////////////////////////////////

// dayModal renders the selected day over the panels.
//
// The day left the right panel because at 80 columns there is no right panel.
// Once that modal exists, a 120-column panel holding the same content is
// duplication: two renderers for one payload. The smaller size forces the
// design the larger one should have had.
func (m *Model) dayModal() []string {
	r := m.frame.Modal
	inner := r.inner()

	cell, ok := m.SelectedCell()
	if !ok {
		return drawPanel("no day selected", nil, r, true, m.styles)
	}

	// The glyph travels with the label, because the legend is where an
	// operator learns which mark means which state and this is where they
	// look it up.
	title := cell.Date.Format("Monday 2 January") + " · " +
		glyphFor(cell.Coverage) + " " + labelFor(cell.Coverage)
	body := m.dayLines(inner.W)

	// A day with recordings does not fit a table, a field block and the
	// gaps in sixteen rows, so it scrolls rather than opening a second
	// stacked modal over the first.
	m.dayOffset = max(min(m.dayOffset, max(len(body)-inner.H, 0)), 0)
	visible := scrolled(body, m.dayOffset, inner.H, inner.W, m.styles)

	m.registerModal(r)
	return drawPanel(title, visible, r, true, m.styles)
}

// dayLines is everything the modal has to say about a day.
func (m *Model) dayLines(width int) []string {
	cell, _ := m.SelectedCell()
	recordings := m.SelectedRecordings()

	// The summary, a blank, then the two header rows. recordingsTop counts
	// them, and the click registry counts the same rows from the same
	// constant: a reader that disagrees by one sells every click as the
	// recording below the one under the pointer.
	lines := []string{m.daySummary(cell, width), ""}
	if len(recordings) == 0 {
		lines = append(lines, m.styles.dim.Render("nothing was captured on this day"))
		return append(lines, m.standingFault()...)
	}

	lines = append(lines, m.recordingHeader(width)...)
	for i, recording := range recordings {
		lines = append(lines, m.recordingRow(i, recording, width))
	}

	if m.recording < len(recordings) {
		lines = append(lines, "")
		lines = append(lines, m.recordingFields(recordings[m.recording], width)...)
		lines = append(lines, m.gapLines(recordings[m.recording], width)...)
	}
	return append(lines, m.standingFault()...)
}

// daySummary is the one line that answers what happened on this day.
//
// Whether anything was watching is the fact that decides what a gap means: a
// day with no broadcast and no recorder are the same picture and opposite
// problems.
func (m *Model) daySummary(cell calendar.Cell, width int) string {
	parts := []string{fmt.Sprintf("%d of %d %s captured",
		cell.Captured, cell.Broadcasts, plural(cell.Broadcasts, "broadcast", "broadcasts"))}

	if cell.Bytes > 0 {
		parts = append(parts, config.Size(cell.Bytes).String())
	}
	if cell.Watched {
		parts = append(parts, "a recorder was watching")
	} else {
		parts = append(parts, "nothing was watching this day")
	}
	return fit(strings.Join(parts, " · "), width, cutEnd)
}

// standingFault is a condition that stays true, stated where the action it
// blocks is offered.
//
// Inside the modal rather than as a toast, because a toast expires and this
// does not. A recover chip over a build with no archive client is the
// lifetime rule applied to a new surface.
func (m *Model) standingFault() []string {
	var lines []string

	// A write made from in here can fail, and the reason belongs where the
	// key was pressed. The banner row behind the modal is covered by it.
	if m.err != nil {
		lines = append(lines, "", m.styles.err.Render(escape.Text(m.err.Error())))
	}
	if m.recovery == nil {
		lines = append(lines, "",
			m.styles.warn.Render("! recovery is not wired up: no archive client is configured"))
	}
	return lines
}

// ///////////////////////////////////////////////
// The recordings table
// ///////////////////////////////////////////////

// recordingHeader labels the table's columns and rules them off.
func (m *Model) recordingHeader(width int) []string {
	file := max(width-recordingFixed-recordingGaps, 12)

	head := strings.Repeat(" ", queueCaret) +
		pad("start", recordingStart, cutEnd) + gap() +
		pad("ran", recordingHeld, cutEnd) + gap() +
		pad("holds", recordingHolds, cutEnd) + gap() +
		padLeft("size", recordingSize, cutEnd) + gap() +
		pad("mk", recordingMarks, cutEnd) + gap() +
		pad("file", file, cutEnd)

	underline := strings.Repeat(" ", queueCaret) +
		rule(recordingStart) + gap() + rule(recordingHeld) + gap() +
		rule(recordingHolds) + gap() + rule(recordingSize) + gap() +
		rule(recordingMarks) + gap() + rule(file)

	return []string{m.styles.dim.Render(head), m.styles.dim.Render(underline)}
}

// recordingRow renders one capture.
//
// The leaf of the path only. The directory is the same for every row and is
// stated once under the table, which is what gives the widest column back to
// the part that differs.
func (m *Model) recordingRow(index int, recording store.Recording, width int) string {
	file := max(width-recordingFixed-recordingGaps, 12)

	caret := "  "
	if index == m.recording {
		caret = m.styles.caret.Render("❯ ")
	}

	// Split the stored path, then escape the part being shown. Cutting an
	// escaped path at a byte index lands inside whatever escape.Text
	// produced for it.
	leaf := recording.Path
	if cut := strings.LastIndex(leaf, "/"); cut >= 0 {
		leaf = leaf[cut+1:]
	}
	leaf = escape.Text(leaf)

	return caret +
		pad(recording.StartedAt.In(m.location).Format("15:04"), recordingStart, cutEnd) + gap() +
		pad(shortDuration(recording.Duration), recordingHeld, cutEnd) + gap() +
		pad(shortDuration(recording.MediaDuration), recordingHolds, cutEnd) + gap() +
		padLeft(config.Size(recording.Bytes).String(), recordingSize, cutEnd) + gap() +
		pad(marksOn(recording), recordingMarks, cutEnd) + gap() +
		m.styles.dim.Render(pad(leaf, file, cutMiddle))
}

// ///////////////////////////////////////////////
// The selected recording
// ///////////////////////////////////////////////

// recordingFields render every stored field of the selected capture.
//
// MediaDuration states its shortfall in words, because the number alone means
// nothing: two hours of media inside a two-and-a-quarter-hour capture is an
// ad break, and the reader has to be told that is what they are looking at.
func (m *Model) recordingFields(recording store.Recording, width int) []string {
	// Split first, escape after. escape.Text renders a path carrying
	// anything unprintable as a quoted literal, and cutting that at a byte
	// index lands inside it: what comes back opens like plain text, closes
	// with a stray quote, and no longer reads back as the name it
	// describes. The whole point of the three shapes is that a reader can
	// tell them apart by the first byte.
	directory := "."
	if cut := strings.LastIndex(recording.Path, "/"); cut >= 0 {
		directory = recording.Path[:cut]
	}
	directory = escape.Text(directory)

	lines := []string{
		m.styles.dim.Render("selected"),
		"  " + fit(escape.Text(recording.Path), width-2, cutMiddle),
		"  " + m.styles.dim.Render(fit("in "+directory, width-2, cutMiddle)),
		"",
	}

	for _, pair := range [][2]string{
		{"state", stateLabel(recording.State)},
		{"origin", string(recording.Origin)},
		{"ran", m.ranAt(recording)},
		{"wall", longDuration(recording.Duration)},
		{"media", m.mediaLine(recording)},
		{"muted", mutedLine(recording)},
		{"watched", m.stampOr(recording.WatchedAt, "no")},
		{"recompressed", m.stampOr(recording.RecompressedAt, "no")},
		{"pinned", yesNo(recording.Pinned)},
		{"refetchable", yesNo(recording.Refetchable)},
	} {
		lines = append(lines, "  "+m.styles.dim.Render(pad(pair[0], fieldLabelCols, cutEnd))+
			fit(escape.Text(pair[1]), max(width-2-fieldLabelCols, 1), cutEnd))
	}

	if recording.Note != "" {
		lines = append(lines, "  "+m.styles.dim.Render(pad("note", fieldLabelCols, cutEnd))+
			fit(escape.Text(recording.Note), max(width-2-fieldLabelCols, 1), cutEnd))
	}
	return lines
}

// ranAt renders the window a capture covered.
func (m *Model) ranAt(recording store.Recording) string {
	started := recording.StartedAt.In(m.location).Format("15:04:05")
	if recording.EndedAt == nil {
		return started + " to now"
	}
	return started + " to " + recording.EndedAt.In(m.location).Format("15:04:05")
}

// mediaLine states how much broadcast a file holds, and what it is short of.
func (m *Model) mediaLine(recording store.Recording) string {
	if recording.MediaDuration <= 0 {
		return "not measured"
	}

	text := longDuration(recording.MediaDuration)
	if short := recording.Duration - recording.MediaDuration; short > time.Minute {
		text += "   " + longDuration(short) + " short of the wall clock"
	}
	return text
}

// stateLabel says what a recording's state means for the operator.
//
// A parked capture is the one state that needs acting on, so it says why it
// is parked rather than naming the row's enum value.
func stateLabel(state store.State) string {
	switch state {
	case store.StateAwaitingMetadata:
		return "waiting on metadata"
	case store.StateAwaitingFile:
		return "waiting on a locked file"
	case store.StateAwaitingFinalize:
		return "not filed yet"
	case store.StateFailed:
		return "failed"
	case store.StateCapturing:
		return "recording now"
	case store.StateTrashed:
		return "in trash"
	case store.StateMissing:
		return "the file is gone"
	default:
		return string(state)
	}
}

// mutedLine reports what a platform silenced.
//
// Nil renders as "not measured" and never as none. Nobody asked is not the
// same answer as nobody found any, and the difference decides whether a
// recovery is worth running.
func mutedLine(recording store.Recording) string {
	if recording.MutedDuration == nil {
		return "not measured"
	}
	if *recording.MutedDuration == 0 {
		return "none"
	}
	return longDuration(*recording.MutedDuration)
}

// stampOr renders a time, or what its absence means.
func (m *Model) stampOr(at *time.Time, absent string) string {
	if at == nil {
		return absent
	}
	return at.In(m.location).Format("2 Jan 15:04")
}

// ///////////////////////////////////////////////
// Gaps
// ///////////////////////////////////////////////

// gapLines list the holes in a capture and what became of each.
func (m *Model) gapLines(recording store.Recording, width int) []string {
	gaps := m.gaps[recording.ID]
	if len(gaps) == 0 {
		return nil
	}

	lines := []string{"", m.styles.dim.Render("gaps")}
	for _, hole := range gaps {
		state := m.styles.warn.Render("not filled")
		if hole.FilledAt != nil {
			state = m.styles.ok.Render("filled from the archive")
		}
		lines = append(lines, "  "+fit(fmt.Sprintf("%s to %s   %s   ",
			longDuration(hole.Start), longDuration(hole.End),
			pad(escape.Text(hole.Reason), 14, cutEnd)), max(width-2, 1), cutEnd)+state)
	}
	return lines
}

// ///////////////////////////////////////////////
// Clicks
// ///////////////////////////////////////////////

// registerModal makes the backdrop close the modal and its own whitespace
// swallow a click.
//
// Order is the mechanism: the full-screen region goes down first, the modal's
// own rectangle over it, and later regions win.
func (m *Model) registerModal(r rect) {
	m.hits.addBlock(0, 0, m.width, m.height, "esc")
	m.hits.addBlock(r.Y, r.X, r.W, r.H, keyNoop)

	inner := r.inner()
	recordings := m.SelectedRecordings()
	for i := range recordings {
		row := inner.Y + recordingsTop + i - m.dayOffset
		if row < inner.Y || row >= inner.Y+inner.H {
			continue
		}
		// The scroller writes its counts over the first and last visible
		// rows, so those two carry a hint rather than the recording whose
		// place they took. Registering them anyway would sell a click on
		// "3 more above" as a click on a recording, and the next w or p
		// would write to it.
		if row == inner.Y && m.dayOffset > 0 {
			continue
		}
		if row == inner.Y+inner.H-1 && len(recordings)-m.dayOffset > inner.H {
			continue
		}
		m.hits.addValue(row, inner.X, inner.W, 1, "recording", fmt.Sprintf("%d", i))
	}
}

// ///////////////////////////////////////////////
// Parts
// ///////////////////////////////////////////////

// rule renders a column's underline.
func rule(width int) string {
	return strings.Repeat(string(lightHorizontal), max(width, 0))
}

// marksOn renders what an operator set on a recording, in a fixed column so
// the sizes beside it stay in line.
func marksOn(recording store.Recording) string {
	watched := " "
	if recording.WatchedAt != nil {
		watched = glyphWatched
	}

	pinned := " "
	if recording.Pinned {
		pinned = glyphPinned
	}
	return watched + pinned
}

// shortDuration renders a span for a table column.
func shortDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// longDuration renders a span to the second, for a field.
func longDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dh%02dm%02ds", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

// yesNo renders a flag as a word.
func yesNo(set bool) string {
	if set {
		return "yes"
	}
	return "no"
}
