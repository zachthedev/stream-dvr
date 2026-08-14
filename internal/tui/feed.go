package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"zach.tools/go/stream-dvr/internal/escape"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Feed is what the model reads events from.
//
// It is a channel rather than a method because the events arrive when the
// recorder decides, not when the calendar asks. Whoever supplies it owns
// closing it, and a closed feed ends the subscription rather than the
// program.
type Feed <-chan FeedEvent

// FeedEvent is one thing the recorder reported.
//
// It mirrors the daemon's event for the same reason notify.Event does: the
// pane is testable without a daemon, and a rename there is not a change to
// what the calendar shows.
type FeedEvent struct {
	// Kind classifies the event, such as "library_full".
	Kind string
	// Channel is the channel involved, when there is one.
	Channel string
	// Detail explains what happened.
	Detail string
	// At is when it happened.
	At time.Time
}

// feedMsg carries one event into the event loop.
type feedMsg struct {
	event FeedEvent
	// open reports whether the feed is still live. A closed feed stops the
	// subscription rather than delivering a zero event forever.
	open bool
}

// ///////////////////////////////////////////////
// Limits
// ///////////////////////////////////////////////

// feedDepth is how many events the pane keeps.
//
// Bounded because a recorder left running for a month would otherwise grow
// this without limit, and because nothing below the last screenful is read.
const feedDepth = 50

// feedShown is how many the calendar shows. The pane is a glance at what
// the recorder has been doing, and the log is where a history lives.
const feedShown = 5

// ///////////////////////////////////////////////
// Subscription
// ///////////////////////////////////////////////

// watchFeed waits for one event.
//
// One command per event is how Bubble Tea reads a channel: the command
// blocks off the event loop, delivers its message, and applyFeed issues the
// next one. A range inside a single command would deliver only the first.
func (m *Model) watchFeed() tea.Cmd {
	feed := m.feed
	if feed == nil {
		return nil
	}

	return func() tea.Msg {
		event, open := <-feed
		return feedMsg{event: event, open: open}
	}
}

// applyFeed records an event and waits for the next.
func (m *Model) applyFeed(msg feedMsg) tea.Cmd {
	if !msg.open {
		// The recorder that was feeding this stopped. What it already said
		// stays on screen; there is just nothing more coming.
		m.feed = nil
		return nil
	}

	m.events = append(m.events, msg.event)
	if len(m.events) > feedDepth {
		m.events = m.events[len(m.events)-feedDepth:]
	}
	return m.watchFeed()
}

// feedLabel names an event kind, styling the ones worth acting on.
//
// A kind this build has no wording for renders as itself, so an event added
// to the recorder still reaches the pane before this list catches up.
func feedLabel(kind string, set styles) string {
	switch kind {
	case "recording_started":
		return set.ok.Render("recording started")
	case "failure":
		return set.err.Render("failed")
	case "library_full":
		return set.err.Render("the library is full")
	case "downtime":
		return set.err.Render("the recorder was not running")
	case "recovered":
		return set.ok.Render("recovered")
	case "gap_filled":
		return set.ok.Render("filled a gap")
	case "fetch_gave_up":
		return set.err.Render("gave up recovering")
	default:
		return escape.Text(kind)
	}
}
