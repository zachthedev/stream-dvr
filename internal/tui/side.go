package tui

import (
	"fmt"
	"strings"

	"zach.tools/go/stream-dvr/internal/calendar"
	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Columns
// ///////////////////////////////////////////////

// The recovery queue's fixed columns. Only the state column varies with the
// panel, so the widths are settled here and the terminal's slack goes to the
// one column that can use it.
const (
	queueCaret       = 2
	queueDay         = 12
	queueBroadcasts  = 10
	queueCaptured    = 8
	queueSize        = 9
	queueColumnGap   = 2
	queueFixedWidth  = queueCaret + queueDay + queueBroadcasts + queueCaptured + queueSize
	queueGaps        = 4 * queueColumnGap
	queueStateFloor  = 17
	queueHeaderRows  = 3
	queueSummaryRows = 4
)

// weekColumns are the by-week table's fixed widths.
const (
	weekRange       = 16
	weekBroadcasts  = 10
	weekCaptured    = 8
	weekMissed      = 6
	weekSizeColumn  = 9
	weekColumnCount = 5
)

// ///////////////////////////////////////////////
// The right panel
// ///////////////////////////////////////////////

// sideBody is the month's work, in three zones that are always present.
//
// A clean month has an empty queue and a full by-week table, so the panel
// never reads as broken for want of anything to recover.
func (m *Model) sideBody(inner rect) []string {
	body := make([]string, 0, inner.H)
	body = append(body, m.queueLines(inner, m.queueHeight(inner.H))...)
	body = append(body, m.styles.dim.Render(strings.Repeat(string(lightHorizontal), inner.W)))
	body = append(body, m.weekLines(inner.W)...)
	body = append(body, m.styles.dim.Render(strings.Repeat(string(lightHorizontal), inner.W)))
	body = append(body, m.monthLines(inner.W)...)
	return body
}

// queueHeight is how many rows the queue gets once the two fixed zones below
// it are paid for.
func (m *Model) queueHeight(height int) int {
	weeks := len(m.grid.Weeks) + 2
	return max(height-weeks-queueSummaryRows-2, queueHeaderRows+1)
}

// ///////////////////////////////////////////////
// To recover
// ///////////////////////////////////////////////

// queueLines renders the days worth acting on, oldest first.
//
// Fed by Grid.Gaps, which is missed days and partly captured ones alike:
// Cell.Recoverable is the one rule the work list and the single-day action
// share, so a day the list skips is a day nothing else offers to fetch.
func (m *Model) queueLines(at rect, height int) []string {
	width := at.W
	lines := make([]string, 0, height)
	heading := m.queueSummary()
	if !m.frame.Wide {
		// One panel, so its title names the month rather than this zone.
		heading = strings.TrimSpace("to recover  " + heading)
	}
	lines = append(lines, m.styles.dim.Render(pad(heading, width, cutEnd)))

	if len(m.queue) == 0 {
		lines = append(lines, "")
		lines = append(lines, m.styles.dim.Render("  nothing in this month needs fetching"))
		return padBlock(lines, height)
	}

	// Column labels only where there is a panel to spare them. On one panel
	// the queue has five rows in total, and three of them spent on a heading
	// and a rule leaves two days on a list of four.
	header := 1
	state := max(width-queueFixedWidth-queueGaps, queueStateFloor)
	if m.frame.Wide {
		lines = append(lines, m.queueHeader(state)...)
		header = queueHeaderRows
	}

	rows := make([]string, 0, len(m.queue))
	for i, cell := range m.queue {
		rows = append(rows, m.queueRow(i, cell, state))
	}

	// The caret marks the selection in both panels at once, so the queue's
	// row for the cursored day carries it even while the calendar takes the
	// keys. Border weight, not the caret, says which panel that is.
	visible := height - header
	m.queueOffset = follow(m.queueOffset, m.queueAt, visible, len(rows))
	lines = append(lines, scrolled(rows, m.queueOffset, visible, width, m.styles)...)

	m.registerQueue(at, visible, header)
	return padBlock(lines, height)
}

// queueSummary counts what the month has left to fetch.
func (m *Model) queueSummary() string {
	if len(m.queue) == 0 {
		return ""
	}

	broadcasts, missing := 0, 0
	for _, cell := range m.queue {
		broadcasts += cell.Broadcasts
		missing += cell.Broadcasts - cell.Captured
	}
	return fmt.Sprintf("%d %s, %d %s missing, oldest first",
		len(m.queue), plural(len(m.queue), "day", "days"),
		missing, plural(missing, "broadcast", "broadcasts"))
}

// queueHeader labels the columns and rules them off.
func (m *Model) queueHeader(state int) []string {
	head := strings.Repeat(" ", queueCaret) +
		pad("day", queueDay, cutEnd) + gap() +
		padLeft("broadcasts", queueBroadcasts, cutEnd) + gap() +
		padLeft("captured", queueCaptured, cutEnd) + gap() +
		padLeft("size", queueSize, cutEnd) + gap() +
		pad("state", state, cutEnd)

	rule := strings.Repeat(" ", queueCaret) +
		strings.Repeat(string(lightHorizontal), queueDay) + gap() +
		strings.Repeat(string(lightHorizontal), queueBroadcasts) + gap() +
		strings.Repeat(string(lightHorizontal), queueCaptured) + gap() +
		strings.Repeat(string(lightHorizontal), queueSize) + gap() +
		strings.Repeat(string(lightHorizontal), state)

	return []string{m.styles.dim.Render(head), m.styles.dim.Render(rule)}
}

// queueRow renders one recoverable day.
func (m *Model) queueRow(index int, cell calendar.Cell, state int) string {
	caret := "  "
	if index == m.queueAt {
		caret = m.styles.caret.Render("❯ ")
	}

	return caret +
		pad(cell.Date.Format("Mon 2 Jan"), queueDay, cutEnd) + gap() +
		padLeft(fmt.Sprintf("%d", cell.Broadcasts), queueBroadcasts, cutEnd) + gap() +
		padLeft(fmt.Sprintf("%d", cell.Captured), queueCaptured, cutEnd) + gap() +
		padLeft(config.Size(cell.Bytes).String(), queueSize, cutEnd) + gap() +
		m.styles.forCoverage(cell.Coverage).Render(pad(labelFor(cell.Coverage), state, cutEnd))
}

// registerQueue makes each visible row clickable.
//
// The rectangle is the one the caller drew into, not the side panel's.
// Below the wide breakpoint there is no side panel: the queue is drawn
// full width under the grid while frame.Side names a column beside it, so
// resolving from the panel puts every row's region on the coverage legend
// and a click there jumps the cursor to an unrelated day.
func (m *Model) registerQueue(at rect, visible, header int) {
	inner := at
	top := inner.Y + header

	for row := range visible {
		index := m.queueOffset + row
		if index >= len(m.queue) {
			break
		}
		m.hits.addValue(top+row, inner.X, inner.W, 1, "day",
			m.queue[index].Date.Format("2006-01-02"))
	}
}

// ///////////////////////////////////////////////
// By week
// ///////////////////////////////////////////////

// weekLines summarize each grid row, in-month days only.
//
// Row three of the table is week row three of the grid, so the two are
// spatially linked and the figures sum to exactly the month below them.
func (m *Model) weekLines(width int) []string {
	lines := make([]string, 0, len(m.grid.Weeks)+2)
	lines = append(lines, m.styles.dim.Render("by week"))

	label := max(width-weekBroadcasts-weekCaptured-weekMissed-weekSizeColumn-
		weekColumnCount*queueColumnGap, weekRange)
	lines = append(lines, m.styles.dim.Render(strings.Repeat(" ", queueCaret)+
		pad("week", label, cutEnd)+gap()+
		padLeft("broadcasts", weekBroadcasts, cutEnd)+gap()+
		padLeft("captured", weekCaptured, cutEnd)+gap()+
		padLeft("missed", weekMissed, cutEnd)+gap()+
		padLeft("size", weekSizeColumn, cutEnd)))

	for _, week := range m.grid.Weeks {
		lines = append(lines, m.weekRow(week, label))
	}
	return lines
}

// weekRow renders one grid row's totals.
func (m *Model) weekRow(week []calendar.Cell, label int) string {
	broadcasts, captured, missed, bytes := 0, 0, 0, int64(0)
	first, last := "", ""

	for _, cell := range week {
		if !cell.InMonth {
			continue
		}
		if first == "" {
			first = cell.Date.Format("2 Jan")
		}
		last = cell.Date.Format("2 Jan")

		broadcasts += cell.Broadcasts
		captured += cell.Captured
		bytes += cell.Bytes
		if cell.Coverage == store.CoverageMissed {
			missed++
		}
	}

	span := first
	if last != first {
		span = first + " to " + last
	}
	if span == "" {
		span = "-"
	}

	return strings.Repeat(" ", queueCaret) +
		m.styles.dim.Render(pad(span, label, cutEnd)) + gap() +
		padLeft(fmt.Sprintf("%d", broadcasts), weekBroadcasts, cutEnd) + gap() +
		padLeft(fmt.Sprintf("%d", captured), weekCaptured, cutEnd) + gap() +
		padLeft(fmt.Sprintf("%d", missed), weekMissed, cutEnd) + gap() +
		padLeft(config.Size(bytes).String(), weekSizeColumn, cutEnd)
}

// ///////////////////////////////////////////////
// The month
// ///////////////////////////////////////////////

// monthLines total the month, split by how each day ended up.
func (m *Model) monthLines(width int) []string {
	summary := m.grid.Summarize()
	recordings := 0
	for _, week := range m.grid.Weeks {
		for _, cell := range week {
			if cell.InMonth {
				recordings += cell.Captured
			}
		}
	}

	covered := []string{
		fmt.Sprintf("%d live", summary.Live),
		fmt.Sprintf("%d recovered", summary.Recovered),
	}
	// Imported is named only where there is one. This line is already cut on
	// a narrow terminal, and a library nobody has imported into would spend
	// that width on a zero.
	if summary.Imported > 0 {
		covered = append(covered, fmt.Sprintf("%d imported", summary.Imported))
	}
	covered = append(covered,
		fmt.Sprintf("%d partly captured", summary.Partial),
		fmt.Sprintf("%d missed", summary.Missed))

	return []string{
		m.styles.dim.Render("month"),
		"  " + fit(fmt.Sprintf("%s across %d %s", config.Size(summary.Bytes),
			recordings, plural(recordings, "recording", "recordings")), width-2, cutEnd),
		"  " + m.styles.dim.Render(fit(strings.Join(covered, " · "), width-2, cutEnd)),
		"  " + m.styles.dim.Render(fit(fmt.Sprintf(
			"%d %s nothing was watching, %d with no broadcast",
			summary.Unknown, plural(summary.Unknown, "day", "days"),
			summary.NoStream), width-2, cutEnd)),
	}
}

// ///////////////////////////////////////////////
// The narrow column
// ///////////////////////////////////////////////

// narrowColumn is the legend and the month's totals, beside the grid where
// there is one panel rather than two.
func (m *Model) narrowColumn(width int) []string {
	lines := m.legendLines()
	lines = append(lines, "")
	lines = append(lines, m.monthLines(width)...)

	for i, line := range lines {
		lines[i] = pad(line, width, cutEnd)
	}
	return padBlock(lines, gridRows)
}

// ///////////////////////////////////////////////
// Parts
// ///////////////////////////////////////////////

// gap is the space between two columns.
func gap() string {
	return strings.Repeat(" ", queueColumnGap)
}

// padBlock fills a block out to a height, so what is under it does not move
// when its content shortens.
func padBlock(lines []string, height int) []string {
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines[:max(min(len(lines), height), 0)]
}
