package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// ///////////////////////////////////////////////
// Channels
// ///////////////////////////////////////////////

// selectChannel switches to a channel by its position in the app bar.
//
// By number rather than by cycling, because the bar shows the numbers and tab
// is spent moving focus between the panels. A number nothing is under does
// nothing, which is what an operator on a two-channel library pressing 3
// should get.
func (m *Model) selectChannel(index int) tea.Cmd {
	if index < 0 || index >= len(m.channels) || index == m.channel {
		return nil
	}
	m.channel = index
	return m.refresh()
}

// ///////////////////////////////////////////////
// The recovery queue
// ///////////////////////////////////////////////

// moveQueue steps through the month's recoverable days, carrying the
// calendar cursor with it.
//
// The two are one selection shown twice. A queue that moved on its own would
// leave the grid pointing at a day the modal would not open.
func (m *Model) moveQueue(step int) tea.Cmd {
	if len(m.queue) == 0 {
		return nil
	}

	m.queueAt = max(min(m.queueAt+step, len(m.queue)-1), 0)
	m.cursor = m.queue[m.queueAt].Date
	return nil
}

// enterQueue moves the selection onto the nearest day worth acting on.
//
// A day with nothing to recover is not in the queue, so moving focus there
// from a settled day lands on the next thing that needs attention rather than
// on nothing. That is what the queue is for.
func (m *Model) enterQueue() tea.Cmd {
	if len(m.queue) == 0 {
		m.queueAt = 0
		return nil
	}

	want := m.cursor.Format("2006-01-02")
	m.queueAt = len(m.queue) - 1
	for i, cell := range m.queue {
		if cell.Date.Format("2006-01-02") >= want {
			m.queueAt = i
			break
		}
	}

	m.cursor = m.queue[m.queueAt].Date
	return nil
}

// pointQueueAt moves the queue's caret to a day, leaving the cursor alone.
//
// The caret marks the selection in both panels at once, so a day picked on
// the grid carries it in the queue too. A day the queue does not hold leaves
// the caret where it was: the queue is a list of what needs work, not a
// second cursor.
func (m *Model) pointQueueAt(date time.Time) {
	want := date.Format("2006-01-02")
	for i, cell := range m.queue {
		if cell.Date.Format("2006-01-02") == want {
			m.queueAt = i
			return
		}
	}
}

// selectDay moves the cursor to a date the grid holds.
//
// The date arrives as text from a click, so it is parsed rather than trusted:
// a region carrying anything else moves nothing.
func (m *Model) selectDay(stamp string) tea.Cmd {
	date, err := time.ParseInLocation("2006-01-02", stamp, m.location)
	if err != nil {
		return nil
	}
	if _, ok := m.grid.Find(date); !ok {
		return nil
	}

	m.cursor = date
	m.pointQueueAt(date)
	return nil
}
