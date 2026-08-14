package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// toast is a transient outcome, shown flush right on the status line until
// it expires.
//
// Notifications split by lifetime, not by severity. A transient outcome is a
// toast. A standing condition, a library that will not open or a recovery
// that is not wired up, is a banner inside the panel or the modal it
// concerns. A standing condition shown as a toast disappears while it is
// still true.
type toast struct {
	Text  string
	Style lipgloss.Style
	Until time.Time
}

// toastMsg wakes the model when a toast is due to expire.
type toastMsg struct{}

// ///////////////////////////////////////////////
// Timing
// ///////////////////////////////////////////////

// toastLife is how long an outcome stays on the status line.
//
// Long enough to read a sentence, short enough that the standing condition
// underneath is not hidden for long. Toasts queue rather than overwrite, so
// two outcomes in quick succession are both read.
const toastLife = 4 * time.Second

// ///////////////////////////////////////////////
// The queue
// ///////////////////////////////////////////////

// queueToast adds an outcome behind whatever is already showing.
//
// Each toast starts its life when the one before it ends, so a burst of
// outcomes is read in order rather than collapsing into the last one.
func (m *Model) queueToast(text string, style lipgloss.Style) tea.Cmd {
	if text == "" {
		return nil
	}

	from := m.now()
	if len(m.toasts) > 0 {
		from = m.toasts[len(m.toasts)-1].Until
	}
	m.toasts = append(m.toasts, toast{Text: text, Style: style, Until: from.Add(toastLife)})

	return tea.Tick(m.toasts[0].Until.Sub(m.now()), func(time.Time) tea.Msg {
		return toastMsg{}
	})
}

// advanceToasts drops what has expired and asks to be woken for the next.
func (m *Model) advanceToasts() tea.Cmd {
	now := m.now()

	kept := m.toasts[:0]
	for _, t := range m.toasts {
		if t.Until.After(now) {
			kept = append(kept, t)
		}
	}
	m.toasts = kept

	if len(m.toasts) == 0 {
		return nil
	}
	return tea.Tick(m.toasts[0].Until.Sub(now), func(time.Time) tea.Msg { return toastMsg{} })
}

// activeToast renders what is showing, or nothing.
//
// A notice is the same kind of thing on a shorter fuse: what the last key
// did, cleared by the next one. It shows where a toast would, because both
// answer "what just happened" and only one can be true at a time.
func (m *Model) activeToast() string {
	if len(m.toasts) > 0 {
		return m.toasts[0].Style.Render(m.toasts[0].Text)
	}
	if m.notice != "" {
		return m.styles.ok.Render(m.notice)
	}
	return ""
}
