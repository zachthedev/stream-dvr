package tui

import (
	tea "charm.land/bubbletea/v2"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Messages
// ///////////////////////////////////////////////

// gapsMsg carries the holes in one recording back to the model.
type gapsMsg struct {
	recording int64
	gaps      []store.Gap
	err       error
}

// ///////////////////////////////////////////////
// Clicks
// ///////////////////////////////////////////////

// maxIndexDigits bounds a clickable region's numeric value. A library of a
// million recordings still fits, and nothing this build registers is
// longer.
const maxIndexDigits = 9

// handleClick turns a click into the key it stands for.
//
// A click inside a region is the same as pressing the key that region names,
// which is what stops the mouse and the keyboard drifting apart: a chip
// carries the literal key its handler switches on, and the region carries the
// chip's key.
//
// A region that selects a value rather than pressing a key is dispatched
// here, because there is no keystroke that means "the fourteenth".
func (m *Model) handleClick(x, y int) tea.Cmd {
	hit, ok := m.hits.find(x, y)
	if !ok {
		return nil
	}

	switch hit.Key {
	case keyNoop:
		return nil

	case "day":
		// One click selects and opens. The cell carries a day number and a
		// state and nothing else, so "just looking" tells an operator almost
		// nothing and opening is what they wanted.
		if cmd := m.selectDay(hit.Value); cmd != nil {
			return cmd
		}
		m.dayOpen, m.dayOffset, m.recording = true, 0, 0
		return m.loadGaps()

	case "recording":
		m.recording = atoi(hit.Value)
		return m.loadGaps()
	}
	return m.handleKey(hit.Key)
}

// handleWheel pages the month over the grid and scrolls a list elsewhere.
//
// The wheel over a calendar means the one gesture a calendar has that a list
// does not, which is turning the page.
func (m *Model) handleWheel(x, y int, up bool) tea.Cmd {
	step := 1
	if up {
		step = -1
	}

	switch {
	case m.dayOpen:
		m.dayOffset = max(m.dayOffset+step, 0)
		return nil
	case m.overGrid(x, y):
		return m.shiftMonth(step)
	default:
		return m.moveQueue(step)
	}
}

// overGrid reports whether a point is inside the month grid.
func (m *Model) overGrid(x, y int) bool {
	inner := m.frame.Calendar.inner()
	return x >= inner.X && x < inner.X+gridColumns &&
		y >= inner.Y && y < inner.Y+gridRows
}

// ///////////////////////////////////////////////
// Gaps
// ///////////////////////////////////////////////

// loadGaps reads the holes in the selected recording, once.
//
// Keyed by recording, so paging back to a capture already read costs nothing
// and a day of twenty captures never queries twenty times at once.
func (m *Model) loadGaps() tea.Cmd {
	recordings := m.SelectedRecordings()
	if m.recording >= len(recordings) {
		return nil
	}

	id := recordings[m.recording].ID
	if _, ok := m.gaps[id]; ok {
		return nil
	}

	library := m.library
	return func() tea.Msg {
		gaps, err := library.GapsFor(id)
		return gapsMsg{recording: id, gaps: gaps, err: err}
	}
}

// applyGaps folds a completed gap read into the model.
func (m *Model) applyGaps(msg gapsMsg) {
	if msg.err != nil {
		m.err = msg.err
		return
	}
	if m.gaps == nil {
		m.gaps = make(map[int64][]store.Gap)
	}
	m.gaps[msg.recording] = msg.gaps
}

// ///////////////////////////////////////////////
// Parts
// ///////////////////////////////////////////////

// atoi reads a small index out of a region's value, answering zero for
// anything else.
func atoi(text string) int {
	// Bounded as well as filtered. Every registered value is a small index
	// this build wrote, so a long run of digits is not one of them, and
	// letting it wrap answers a click with a number nobody chose.
	if text == "" || len(text) > maxIndexDigits {
		return 0
	}

	n := 0
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
