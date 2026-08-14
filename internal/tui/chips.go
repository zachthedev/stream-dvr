package tui

import (
	"slices"
	"strings"

	"zach.tools/go/stream-dvr/internal/escape"
)

// ///////////////////////////////////////////////
// Identity
// ///////////////////////////////////////////////

// appName is what the app bar leads with.
const appName = "stream-dvr"

// ///////////////////////////////////////////////
// The keys of each pane
// ///////////////////////////////////////////////

// chips lists the keys the focused pane accepts.
//
// Only those keys. A footer naming every key in the application is what
// leaves an operator pressing one that does nothing here. Each chip carries
// the literal key its handler switches on, so a click and a keystroke reach
// the same place and a test can walk every chip against the keys its pane
// takes.
func (m *Model) chips() []chip {
	switch {
	case m.editing:
		return []chip{
			{Keys: []string{"enter"}, Label: "enter", Hint: "accept"},
			{Keys: []string{"esc"}, Label: "esc", Hint: "cancel"},
		}

	case m.dayOpen:
		return m.dayChips()

	case m.pane == paneSettings && m.onChannel():
		return []chip{
			{Keys: []string{"up", "down"}, Label: "↑↓", Hint: "row"},
			{Keys: []string{"enter"}, Label: "enter", Hint: "watch"},
			{Keys: []string{"b"}, Label: "b", Hint: "backfill"},
			{Keys: []string{"P"}, Label: "P", Hint: "platform"},
			{Keys: []string{"n"}, Label: "n", Hint: "name"},
			{Keys: []string{"a"}, Label: "a", Hint: "add"},
			{Keys: []string{"d"}, Label: "d", Hint: "delete"},
			{Keys: []string{"ctrl+s"}, Label: "ctrl+s", Hint: "save"},
			{Keys: []string{"esc"}, Label: "esc", Hint: "back"},
		}

	case m.pane == paneSettings:
		return []chip{
			{Keys: []string{"up", "down"}, Label: "↑↓", Hint: "row"},
			{Keys: []string{"enter"}, Label: "enter", Hint: "change"},
			{Keys: []string{"a"}, Label: "a", Hint: "add channel"},
			{Keys: []string{"ctrl+s"}, Label: "ctrl+s", Hint: "save"},
			{Keys: []string{"r"}, Label: "r", Hint: "reload"},
			{Keys: []string{"esc"}, Label: "esc", Hint: "back"},
			{Keys: []string{"q"}, Label: "q", Hint: "quit"},
		}

	case m.pane == panePurge && m.confirming:
		return []chip{
			{Keys: []string{"enter"}, Label: "enter", Hint: "confirm"},
			{Keys: []string{"esc"}, Label: "esc", Hint: "cancel"},
		}

	case m.pane == panePurge:
		return []chip{
			{Keys: []string{"up", "down"}, Label: "↑↓", Hint: "candidate"},
			{Keys: []string{"space"}, Label: "space", Hint: "select"},
			{Keys: []string{"enter"}, Label: "enter", Hint: "purge"},
			{Keys: []string{"r"}, Label: "r", Hint: "refresh"},
			{Keys: []string{"esc"}, Label: "esc", Hint: "back"},
			{Keys: []string{"q"}, Label: "q", Hint: "quit"},
		}
	}
	return m.calendarChips()
}

// calendarChips are the month view's keys.
func (m *Model) calendarChips() []chip {
	toggle := "start service"
	if m.status.State == stateRunning {
		toggle = "stop service"
	}

	here := "record here"
	if m.recorder != nil && m.recorder.Running() {
		here = "stop here"
	}

	focus := "to recover"
	if m.focused == focusQueue {
		focus = "to calendar"
	}

	return []chip{
		{Keys: []string{"left", "right", "up", "down"}, Label: "←→↑↓", Hint: "day"},
		{Keys: []string{"[", "]"}, Label: "[ ]", Hint: "month"},
		{Keys: []string{"t"}, Label: "t", Hint: "today"},
		{Keys: []string{"enter"}, Label: "enter", Hint: "open day"},
		{Keys: []string{"tab"}, Label: "tab", Hint: focus},
		{Keys: []string{"e"}, Label: "e", Hint: "settings"},
		{Keys: []string{"x"}, Label: "x", Hint: "purge"},
		{Keys: []string{"r"}, Label: "r", Hint: "refresh"},
		{Keys: []string{"s"}, Label: "s", Hint: toggle},
		{Keys: []string{"d"}, Label: "d", Hint: here},
		{Keys: []string{"q"}, Label: "q", Hint: "quit"},
	}
}

// dayChips are the modal's keys.
func (m *Model) dayChips() []chip {
	return []chip{
		{Keys: []string{"up", "down"}, Label: "↑↓", Hint: "recording"},
		{Keys: []string{"w"}, Label: "w", Hint: "watched"},
		{Keys: []string{"p"}, Label: "p", Hint: "pin"},
		{Keys: []string{"R"}, Label: "R", Hint: "recover this day"},
		{Keys: []string{"r"}, Label: "r", Hint: "refresh"},
		{Keys: []string{"esc"}, Label: "esc", Hint: "close"},
		{Keys: []string{"q"}, Label: "q", Hint: "quit"},
	}
}

// ///////////////////////////////////////////////
// The recorder strip
// ///////////////////////////////////////////////

// recorderStrip renders what the recorder has been doing, under the panels.
//
// It keeps reporting while a day is read, which is why the modal is centred
// over the panels alone rather than over the whole screen.
func (m *Model) recorderStrip() []string {
	if m.frame.Recorder.empty() {
		return nil
	}

	inner := m.frame.Recorder.inner()
	body := make([]string, 0, inner.H)
	body = append(body, m.recorderCondition(inner.W))
	body = append(body, "")

	shown := m.events
	if room := inner.H - 2; len(shown) > room {
		shown = shown[len(shown)-room:]
	}
	for _, s := range slices.Backward(shown) {
		body = append(body, m.eventLine(s, inner.W))
	}

	return drawPanel("recorder", body, m.frame.Recorder, false, m.styles)
}

// recorderCondition states both recorders on one line, because which one is
// running decides what stopping does.
func (m *Model) recorderCondition(width int) string {
	service := m.status.State
	if service == "" {
		service = "unknown"
	}
	if m.statusErr != nil {
		service += " (" + escape.Text(m.statusErr.Error()) + ")"
	}

	here := "stopped"
	if m.recorder != nil && m.recorder.Running() {
		here = "running"
		if err := m.recorder.Err(); err != nil {
			here = "failed (" + escape.Text(err.Error()) + ")"
		}
	}

	line := m.styles.dim.Render("service") + "   " + pad(service, 28, cutEnd) +
		m.styles.dim.Render("in this window") + "   " + here
	return fit(line, width, cutEnd)
}

// eventLine renders one thing the recorder reported.
func (m *Model) eventLine(event FeedEvent, width int) string {
	parts := make([]string, 0, 2)
	if event.Channel != "" {
		parts = append(parts, event.Channel)
	}
	if event.Detail != "" {
		parts = append(parts, event.Detail)
	}

	line := event.At.In(m.location).Format("15:04") + "  " +
		pad(feedLabel(event.Kind, m.styles), 24, cutEnd)
	if len(parts) > 0 {
		line += m.styles.dim.Render(escape.Text(strings.Join(parts, ": ")))
	}
	return fit(line, width, cutEnd)
}

// ///////////////////////////////////////////////
// The other panes
// ///////////////////////////////////////////////

// purgeBody is the purge list, laid into the panel.
func (m *Model) purgeBody() []string {
	head, rows, tail, at := m.purgeView()
	return m.scrollBody(head, rows, tail, at, &m.purgeOffset)
}

// settingsBody is the settings list, laid into the panel.
func (m *Model) settingsBody() []string {
	head, rows, tail, at := m.settingsView()
	return m.scrollBody(head, rows, tail, at, &m.settingsOffset)
}

// scrollBody lays a pinned head and tail around a list that scrolls.
//
// Chrome is pinned and content scrolls, so nothing a list grows to can push
// the help under it off the panel.
func (m *Model) scrollBody(head, rows, tail []string, at int, offset *int) []string {
	inner := m.frame.Calendar.inner()
	room := max(inner.H-len(head)-len(tail), 1)

	*offset = follow(*offset, at, room, len(rows))
	body := append([]string{}, head...)
	body = append(body, scrolled(rows, *offset, room, inner.W, m.styles)...)
	return append(body, tail...)
}
