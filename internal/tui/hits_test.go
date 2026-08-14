package tui

import (
	"testing"
)

// keyAt is the key under a point, or empty where nothing is.
func keyAt(hits *registry, x, y int) string {
	if r, ok := hits.find(x, y); ok {
		return r.Key
	}
	return ""
}

func TestRegistry_AClickInsideARegionIsThatKey(t *testing.T) {
	// The whole mouse model. No handler exists for anything that already has
	// a keyboard binding, which is what stops the two drifting apart.
	hits := newRegistry()
	hits.add(3, 10, 5, "q")

	tests := []struct {
		name string
		x, y int
		want string
	}{
		{name: "the first cell", x: 10, y: 3, want: "q"},
		{name: "the last cell", x: 14, y: 3, want: "q"},
		{name: "one past the end", x: 15, y: 3, want: ""},
		{name: "one before the start", x: 9, y: 3, want: ""},
		{name: "the row above", x: 12, y: 2, want: ""},
		{name: "the row below", x: 12, y: 4, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keyAt(hits, tt.x, tt.y); got != tt.want {
				t.Errorf("find(%d, %d) = %q, want %q", tt.x, tt.y, got, tt.want)
			}
		})
	}
}

func TestRegistry_LaterRegionsWin(t *testing.T) {
	// Render order is click priority, which is how a modal takes a click
	// that lands over a calendar cell: the modal is drawn second.
	hits := newRegistry()
	hits.addBlock(0, 0, 20, 10, "esc")
	hits.addBlock(2, 2, 5, 5, keyNoop)

	if got := keyAt(hits, 3, 3); got != keyNoop {
		t.Errorf("find() = %q, want the region drawn over the other", got)
	}
	if got := keyAt(hits, 15, 8); got != "esc" {
		t.Errorf("find() = %q, want the region underneath where nothing covers it", got)
	}
}

func TestRegistry_AValueRegionCarriesWhatItSelects(t *testing.T) {
	// A calendar cell picks a day, and there is no keystroke that means
	// "the fourteenth".
	hits := newRegistry()
	hits.addValue(1, 1, 6, 1, "day", "2026-08-14")

	hit, ok := hits.find(3, 1)
	if !ok {
		t.Fatal("find() reported nothing under a registered cell")
	}
	if hit.Key != "day" || hit.Value != "2026-08-14" {
		t.Errorf("find() = %+v, want the day and its date", hit)
	}
}

func TestRegistry_RefusesARegionThatCannotBeClicked(t *testing.T) {
	// A zero-width region and one with no key are both silent no-ops rather
	// than entries that can never match.
	hits := newRegistry()
	hits.add(0, 0, 0, "q")
	hits.add(0, 0, 5, "")
	hits.addBlock(0, 0, 5, 0, "q")

	if len(hits.regions) != 0 {
		t.Errorf("the registry holds %d unclickable regions, want none", len(hits.regions))
	}
}

func TestRegistry_ResetDropsTheLastFrame(t *testing.T) {
	// Rebuilt on every render, so a region no longer drawn cannot be
	// clicked. That is how a modal hides the cells beneath it.
	hits := newRegistry()
	hits.add(0, 0, 5, "q")
	hits.reset()

	if got := keyAt(hits, 2, 0); got != "" {
		t.Errorf("find() = %q after a reset, want nothing", got)
	}
}
