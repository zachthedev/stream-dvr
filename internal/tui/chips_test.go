package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"zach.tools/go/stream-dvr/internal/store"
)

func TestChips_EveryKeyOfferedIsOneThePaneAccepts(t *testing.T) {
	// Footer drift at the root. A chip carries the literal key its handler
	// switches on, so this walks every pane's set against what that pane
	// actually does with each key. A chip naming a key the pane ignores is
	// an instruction to press something that does nothing.
	panes := []struct {
		name string
		open func(*testing.T, *Model)
	}{
		{name: "the calendar", open: func(*testing.T, *Model) {}},
		{
			// On the second entry, so the arrows have somewhere to go in
			// both directions.
			name: "the recovery queue",
			open: func(t *testing.T, m *Model) {
				press(t, m, "tab")
				press(t, m, "down")
			},
		},
		{
			name: "the day modal",
			open: func(t *testing.T, m *Model) {
				pressNamed(t, m, tea.KeyEnter)
				press(t, m, "down")
			},
		},
		{
			name: "purge",
			open: func(t *testing.T, m *Model) { press(t, m, "x") },
		},
		{
			name: "settings",
			open: func(t *testing.T, m *Model) { press(t, m, "e") },
		},
	}

	for _, pane := range panes {
		t.Run(pane.name, func(t *testing.T) {
			model := everyPane(t)
			pane.open(t, model)

			for _, c := range model.chips() {
				if len(c.Keys) == 0 {
					t.Errorf("the %q chip names no key at all", c.Label)
				}
				for _, key := range c.Keys {
					if !accepts(t, pane.open, key) {
						t.Errorf("the footer offers %q (%s), which this pane ignores", key, c.Hint)
					}
				}
			}
		})
	}
}

// everyPane is a model each pane can actually be opened in.
//
// A pane that never opened answers with the calendar's keys, and every chip
// would then read as drift.
func everyPane(t *testing.T) *Model {
	t.Helper()

	lib := dayLibrary()
	lib.days = []store.Day{
		{Date: time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), State: store.CoverageMissed, Broadcasts: 1},
		{Date: time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC), State: store.CoverageMissed, Broadcasts: 1},
		{Date: time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC), State: store.CoveragePartial, Broadcasts: 2, Captured: 1},
	}

	return newOptionsModel(t, Options{
		Library:  lib,
		Actions:  &fakeActions{library: lib},
		Purge:    rankedPurge(),
		Settings: settingsFixture(),
		Recovery: &fakeRecovery{},
	})
}

// accepts reports whether a pane's handler has a case for a key.
//
// Tried from several starting positions, because a key at the end of a list
// is a legitimate no-op and the chip that names it is still right. What the
// test is after is a chip naming a key the pane routes nowhere at all.
//
// Freshly opened each time, because a key that acts changes the pane it was
// pressed in and the next attempt would be answered by somewhere else.
func accepts(t *testing.T, open func(*testing.T, *Model), key string) bool {
	t.Helper()

	for _, priming := range [][]string{nil, {"down"}, {"up"}, {"space"}, {"down", "space"}} {
		fresh := everyPane(t)
		open(t, fresh)
		for _, prime := range priming {
			press(t, fresh, prime)
		}

		before := fresh.View().Content
		cmd := fresh.handleKey(key)
		drain(t, fresh, cmd)

		if cmd != nil || fresh.View().Content != before {
			return true
		}
	}
	return false
}
