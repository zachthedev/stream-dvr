package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"zach.tools/go/stream-dvr/internal/calendar"
	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/escape"
)

// ///////////////////////////////////////////////
// The screen
// ///////////////////////////////////////////////

// View implements tea.Model.
//
// The alternate screen and the mouse are properties of the view rather than
// program options, so a model driven directly by a test renders the same
// content without a terminal to switch buffers on. Cell motion is what
// reports a click, a release and a wheel; nothing here needs hover.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// render draws the whole screen.
//
// Every row belongs to exactly one band, so the screen is built band by band
// and never overwritten. The one exception is a modal, which is composed over
// the finished screen by a real cell buffer.
func (m *Model) render() string {
	if m.quit {
		return ""
	}

	m.hits.reset()
	m.frame = layout(m.width, m.height, chipHeight(m.chips(), m.width))
	if m.frame.TooSmall {
		return m.tooSmall()
	}

	if !m.dayOpen {
		return m.screen()
	}

	// The backdrop renders through a dimmed style set passed down, never by
	// rewriting the escape sequences of a screen already rendered. Parsing
	// back out of rendered text is where a dimmer starts disagreeing with
	// the renderer about what it is looking at.
	full := m.styles
	m.styles = full.dimmed()
	backdrop := m.screen()
	m.styles = full

	return m.composeModal(backdrop)
}

// screen draws everything but the modal.
func (m *Model) screen() string {
	lines := make([]string, 0, m.height)
	lines = append(lines, m.appBar())
	lines = append(lines, m.panels()...)
	lines = append(lines, m.recorderStrip()...)
	lines = append(lines, drawStatus(m.standing(), m.activeToast(), m.frame.Status, m.styles))
	lines = append(lines, drawChips(m.chips(), m.frame.Chips, m.styles)...)

	return strings.Join(lines, "\n")
}

// tooSmall says what the screen needs rather than drawing a broken one.
func (m *Model) tooSmall() string {
	// Wrapped and padded like every other screen. The one message whose
	// whole job is to say the terminal is too narrow must not be wider
	// than the terminal, and a single line where the rest of the program
	// returns m.height leaves whatever was drawn before it on screen.
	width := max(m.width, 1)
	lines := strings.Split(ansi.Wordwrap(m.text(), width, " -"), "\n")
	for i, line := range lines {
		lines[i] = pad(line, width, cutEnd)
	}
	for len(lines) < m.height {
		lines = append(lines, strings.Repeat(" ", width))
	}
	return strings.Join(lines[:max(m.height, 1)], "\n")
}

// text is what tooSmall has to say.
func (m *Model) text() string {
	return fmt.Sprintf("the calendar needs %d columns and %d rows; this terminal is %dx%d",
		floorWidth, floorHeight+1, m.width, m.height)
}

// composeModal lays the day over the finished screen.
//
// A real cell buffer, so a wide rune under the modal is handled by the
// compositor rather than by counting bytes. The backdrop renders through a
// dimmed style set on the way in, never by rewriting escape sequences on the
// way out.
func (m *Model) composeModal(screen string) string {
	modal := strings.Join(m.dayModal(), "\n")

	// Through a compositor rather than by nesting layers. A layer draws its
	// own content and nothing else: only the compositor flattens a hierarchy
	// and applies each layer's position, so a nested child is never drawn.
	return lipgloss.NewCanvas(m.width, m.height).Compose(
		lipgloss.NewCompositor(
			lipgloss.NewLayer(screen),
			lipgloss.NewLayer(modal).X(m.frame.Modal.X).Y(m.frame.Modal.Y).Z(1),
		),
	).Render()
}

// ///////////////////////////////////////////////
// The app bar
// ///////////////////////////////////////////////

// appBar renders the top row: the name, the channel tabs, and the volume.
func (m *Model) appBar() string {
	tabs := make([]string, 0, len(m.channels))
	column := chipMargin + lipgloss.Width(appName) + 3

	for i, channel := range m.channels {
		tab := drawTab(i+1, escape.Text(channel.Name), i == m.channel, m.styles)
		tabs = append(tabs, tab)

		// A tab click is the digit key, so the bar and the keyboard reach
		// the same handler and cannot drift apart.
		m.hits.add(m.frame.AppBar.Y, column, lipgloss.Width(tab), string(rune('1'+i)))
		column += lipgloss.Width(tab) + 3
	}

	return drawAppBar(appName, tabs, m.volume(), m.frame.AppBar, m.styles)
}

// volume renders what the library holds against what the disk has, flush
// right in the app bar.
func (m *Model) volume() string {
	if m.spaceErr != nil {
		return m.styles.dim.Render("space unknown")
	}

	held := config.Size(m.space.Held).String()
	if m.space.Cap <= 0 {
		text := held + " held  " + config.Size(m.space.Free).String() + " free"
		return m.styles.dim.Render(text) + "  " + m.clock()
	}

	percent := percentOf(m.space.Held, m.space.Cap)
	text := fmt.Sprintf("%s of %s  %d%%", held, config.Size(m.space.Cap), percent)

	// The level is the only part of the bar that can mean a broadcast goes
	// unrecorded, so it is the only part that leaves the dim style, and it
	// says what it means in words as well. A gauge that turned red and said
	// nothing leaves colour as the only signal, which is what the rest of
	// this screen refuses.
	style := m.styles.dim
	switch m.space.Level {
	case spaceCritical:
		style = m.styles.err
		text += "  the next broadcast will be refused"
	case spaceLow:
		style = m.styles.err
		text += "  running low"
	}
	return style.Render(text) + "  " + m.clock()
}

// clock renders the time, which is what says the screen is live.
func (m *Model) clock() string {
	return m.styles.dim.Render(m.now().In(m.location).Format("15:04"))
}

// ///////////////////////////////////////////////
// The panels
// ///////////////////////////////////////////////

// panels renders the calendar and the month's work, side by side where there
// is room for both.
func (m *Model) panels() []string {
	// Purge and settings replace the month rather than sitting beside it,
	// so they take the whole band. Each ranks or edits the library as a
	// whole, and a grid showing one channel in one month next to either
	// would read as the scope of what is about to happen.
	whole := rect{X: 0, Y: m.frame.Calendar.Y, W: m.width, H: m.frame.Calendar.H}
	switch m.pane {
	case panePurge:
		return drawPanel("purge", m.purgeBody(), whole, true, m.styles)
	case paneSettings:
		return drawPanel("settings", m.settingsBody(), whole, true, m.styles)
	}

	if !m.frame.Wide {
		return drawPanel(m.monthTitle()+" · "+m.ChannelLabel(),
			m.narrowBody(), m.frame.Calendar, true, m.styles)
	}

	left := drawPanel(m.monthTitle(), m.calendarBody(m.frame.Calendar.inner()),
		m.frame.Calendar, m.focused == focusCalendar, m.styles)
	right := drawPanel("to recover", m.sideBody(m.frame.Side.inner()),
		m.frame.Side, m.focused == focusQueue, m.styles)

	rows := make([]string, 0, len(left))
	for i := range left {
		rows = append(rows, left[i]+strings.Repeat(" ", panelGutter)+right[i])
	}
	return rows
}

// monthTitle names the month on display.
func (m *Model) monthTitle() string {
	return m.grid.Title()
}

// calendarBody is the wide arrangement's left panel: the grid, the legend,
// and the row the degraded banner is held for.
func (m *Model) calendarBody(inner rect) []string {
	body := make([]string, 0, inner.H)
	body = append(body, m.gridLines(inner)...)
	body = append(body, "")
	body = append(body, m.legendLines()...)
	body = append(body, "")
	body = append(body, m.bannerLine())
	return body
}

// narrowBody is the single panel: the grid with a column beside it, and the
// recovery queue at full width underneath.
func (m *Model) narrowBody() []string {
	region := m.frame.Calendar.inner()

	grid := m.gridLines(region)
	side := m.narrowColumn(m.frame.Side.W)

	body := make([]string, 0, region.H)
	for i := range gridRows {
		line := ""
		if i < len(grid) {
			line = grid[i]
		}
		beside := ""
		if i < len(side) {
			beside = side[i]
		}
		body = append(body, pad(line, gridColumns, cutEnd)+
			strings.Repeat(" ", panelGutter)+beside)
	}

	body = append(body, m.queueLines(rect{
		X: region.X, Y: region.Y + gridRows, W: region.W, H: region.H - gridRows,
	}, region.H-gridRows)...)
	return body
}

// gridLines draws the month as boxed cells.
func (m *Model) gridLines(inner rect) []string {
	at := cursor{Week: -1, Day: -1}
	today := m.today().Format("2006-01-02")
	selected := m.cursor.Format("2006-01-02")

	faces := make([][]face, 0, len(m.grid.Weeks))
	for week, days := range m.grid.Weeks {
		row := make([]face, 0, calendar.DaysPerWeek)
		for day, cell := range days {
			if cell.Date.Format("2006-01-02") == selected {
				at = cursor{Week: week, Day: day}
			}
			row = append(row, m.cellFace(cell, today))

			// The cell's clickable span includes its left border, so the
			// lattice between two days belongs to one of them rather than
			// to neither.
			m.hits.addValue(inner.Y+week*2+1, inner.X+day*(gridInterior+1),
				gridInterior+1, 1, "day", cell.Date.Format("2006-01-02"))
		}
		faces = append(faces, row)
	}

	return drawGrid(m.grid.WeekdayHeadings(), faces, at, m.degraded())
}

// cellFace renders one square's interior: the day, the today marker, and the
// state glyph.
//
// A padding day is parenthesised and keeps its glyph, so a missed broadcast
// on the 1st is visible from the previous month's view. It carries no today
// marker: the parentheses take those columns, and today is never padding in
// the month it belongs to.
func (m *Model) cellFace(cell calendar.Cell, today string) face {
	glyph := glyphFor(cell.Coverage)
	if !cell.InMonth {
		return face{
			Text:  fmt.Sprintf("(%2d)%s", cell.Date.Day(), glyph),
			Style: m.styles.padding,
		}
	}

	marker := " "
	if cell.Date.Format("2006-01-02") == today {
		marker = glyphToday
	}
	return face{
		Text:  fmt.Sprintf(" %2d%s%s", cell.Date.Day(), marker, glyph),
		Style: m.styles.forCell(cell.Coverage),
	}
}

// legendLines name each glyph and count the month, worst first.
func (m *Model) legendLines() []string {
	summary := m.grid.Summarize()

	lines := make([]string, 0, legendRows)
	lines = append(lines, m.styles.dim.Render("coverage"))
	for _, coverage := range legendOrder {
		lines = append(lines, fmt.Sprintf("  %s  %s  %s",
			m.styles.forCoverage(coverage).Render(glyphFor(coverage)),
			padLeft(fmt.Sprintf("%d", summary.Count(coverage)), 2, cutEnd),
			m.styles.dim.Render(labelFor(coverage))))
	}
	return lines
}

// bannerLine holds a row for the standing condition the grid cannot show.
//
// The row is reserved whether or not it fires, so the legend does not jump
// down the screen when a read fails partway through a month.
func (m *Model) bannerLine() string {
	switch {
	case m.degraded():
		return m.styles.warn.Render("! some rows could not be read, so these counts may be short")
	default:
		return ""
	}
}

// degraded reports whether any day in the month lost a row to an unreadable
// record.
//
// Range level, not day level: an unreadable row cannot be attributed to a
// day, so the whole range carries it.
func (m *Model) degraded() bool {
	for _, week := range m.grid.Weeks {
		for _, cell := range week {
			if cell.Degraded {
				return true
			}
		}
	}
	return false
}

// ///////////////////////////////////////////////
// Standing condition
// ///////////////////////////////////////////////

// standing renders what stays true until it changes: where the library is,
// and what the recorder is doing.
func (m *Model) standing() string {
	recorder := m.status.State
	if recorder == "" {
		recorder = "unknown"
	}
	// A query that failed says only "unknown" without its reason, which
	// leaves an operator with a recorder in an unnamed condition and nothing
	// to act on. The reason comes from the platform's service manager, so it
	// is no more trustworthy than a recording label.
	if m.statusErr != nil {
		recorder += " (" + escape.Text(m.statusErr.Error()) + ")"
	}
	// A disabled registration is complete and reads like a recorder between
	// broadcasts. The word alone does not say that no broadcast will ever
	// start it.
	if recorder == stateDisabled {
		recorder += ", it will not start until it is enabled again"
	}
	// The one recorder that stops when this window closes says so where the
	// condition is read, because that is the only place the difference
	// between it and the installed service can matter.
	if m.recorder != nil && m.recorder.Running() {
		recorder = "recording in this window"
		if err := m.recorder.Err(); err != nil {
			recorder += " (" + escape.Text(err.Error()) + ")"
		}
	}

	// An error outranks the recorder's condition. It is the standing thing
	// that needs acting on, and the status line is the widest row on the
	// screen, so the reason arrives whole rather than cut to a panel.
	if m.err != nil {
		return escape.Text(m.err.Error())
	}
	return m.ChannelLabel() + " · recorder " + recorder
}

// percentOf reports held as a whole percentage of limit, rounded down so
// a library one byte short of its cap never reads as a full 100.
func percentOf(held, limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	return min(held*100/limit, 100)
}

// plural picks a word form for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
