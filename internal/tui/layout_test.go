package tui

import (
	"testing"
)

// ///////////////////////////////////////////////
// The two drawn sizes
// ///////////////////////////////////////////////

func TestLayout_MatchesTheDesignAt120x40(t *testing.T) {
	// The wide arrangement, drawn: two panels and a gutter over a recorder
	// strip, with the day modal centred on the panels alone so the app bar,
	// the strip and the status line stay visible behind it.
	f := layout(120, 40, 2)

	if !f.Wide {
		t.Fatal("120 columns did not reach the wide arrangement")
	}
	assertRect(t, "calendar", f.Calendar, rect{X: 0, Y: 1, W: 47, H: 28})
	assertRect(t, "side", f.Side, rect{X: 48, Y: 1, W: 72, H: 28})
	assertRect(t, "recorder", f.Recorder, rect{X: 0, Y: 29, W: 120, H: 8})
	assertRect(t, "status", f.Status, rect{X: 0, Y: 37, W: 120, H: 1})
	assertRect(t, "chips", f.Chips, rect{X: 0, Y: 38, W: 120, H: 2})
	assertRect(t, "modal", f.Modal, rect{X: 15, Y: 2, W: 90, H: 25})
}

func TestLayout_MatchesTheDesignAt80x24(t *testing.T) {
	// Height binds here, not width. Twenty-four rows is one app bar, a
	// twenty-row panel, a status line and two chip rows, and the panel holds
	// the grid, the side column and the recovery queue together.
	f := layout(80, 24, 2)

	if f.Wide {
		t.Fatal("80 columns reached the wide arrangement, which does not fit")
	}
	assertRect(t, "calendar", f.Calendar, rect{X: 0, Y: 1, W: 80, H: 20})
	assertRect(t, "side", f.Side, rect{X: 46, Y: 2, W: 32, H: 13})
	assertRect(t, "status", f.Status, rect{X: 0, Y: 21, W: 80, H: 1})
	assertRect(t, "chips", f.Chips, rect{X: 0, Y: 22, W: 80, H: 2})
	assertRect(t, "modal", f.Modal, rect{X: 3, Y: 2, W: 74, H: 18})

	if !f.Recorder.empty() {
		t.Errorf("recorder = %+v, want it dropped where there is no room", f.Recorder)
	}
}

// ///////////////////////////////////////////////
// The breakpoint
// ///////////////////////////////////////////////

func TestLayout_TheBreakpointIsWhereBothPanelsFit(t *testing.T) {
	// The calendar panel is fixed at 47, the gutter is 1, and the right
	// panel's floor is its widest row: a recovery queue entry at 65 columns
	// of content inside 4 of chrome. One column under that and the two
	// cannot stand side by side.
	if got := layout(wideBreakpoint, 40, 2); !got.Wide {
		t.Errorf("%d columns is not wide, want the breakpoint itself to fit",
			wideBreakpoint)
	}
	if got := layout(wideBreakpoint-1, 40, 2); got.Wide {
		t.Errorf("%d columns is wide, want one column under the breakpoint to fall back",
			wideBreakpoint-1)
	}
	if wideBreakpoint != 117 {
		t.Errorf("the breakpoint moved to %d; the design derives 117", wideBreakpoint)
	}
}

func TestLayout_TheSidePanelNeverFallsUnderItsFloor(t *testing.T) {
	// Whatever the terminal, a right panel that exists is wide enough for
	// the row it was sized around.
	for width := wideBreakpoint; width <= 400; width++ {
		f := layout(width, 40, 2)
		if !f.Wide {
			t.Fatalf("%d columns is not wide, though it is over the breakpoint", width)
		}
		if f.Side.W < sidePanelFloor {
			t.Fatalf("%d columns gave the side panel %d, under the %d floor",
				width, f.Side.W, sidePanelFloor)
		}
	}
}

// ///////////////////////////////////////////////
// Every size in between
// ///////////////////////////////////////////////

func TestLayout_TheRegionsTileTheScreenExactly(t *testing.T) {
	// Chrome is pinned and content scrolls, so the rows have to add up. A
	// gap leaves a blank line nobody put there; an overlap pushes the footer
	// off the screen, which is the one thing this layout exists to stop.
	for width := floorWidth; width <= 400; width += 7 {
		for height := floorHeight + 1; height <= 120; height += 3 {
			for chips := 1; chips <= 3; chips++ {
				f := layout(width, height, chips)
				if f.TooSmall {
					continue
				}

				rows := f.AppBar.H + f.Calendar.H + f.Recorder.H + f.Status.H + f.Chips.H
				if rows != height {
					t.Fatalf("%dx%d with %d chip rows tiles %d rows, want %d",
						width, height, chips, rows, height)
				}
				if f.Wide && f.Calendar.W+panelGutter+f.Side.W != width {
					t.Fatalf("%dx%d: the panels and the gutter span %d columns, want %d",
						width, height, f.Calendar.W+panelGutter+f.Side.W, width)
				}
			}
		}
	}
}

func TestLayout_NoRegionEscapesTheScreen(t *testing.T) {
	for width := floorWidth; width <= 400; width += 11 {
		for height := floorHeight + 1; height <= 120; height += 5 {
			f := layout(width, height, 2)
			if f.TooSmall {
				continue
			}

			named := map[string]rect{
				"app bar": f.AppBar, "calendar": f.Calendar, "side": f.Side,
				"recorder": f.Recorder, "status": f.Status, "chips": f.Chips,
				"modal": f.Modal,
			}
			for name, r := range named {
				if r.empty() {
					continue
				}
				if r.X < 0 || r.Y < 0 || r.X+r.W > width || r.Y+r.H > height {
					t.Fatalf("%dx%d: %s at %+v leaves the screen", width, height, name, r)
				}
			}
		}
	}
}

func TestLayout_TheGridAlwaysHasRoom(t *testing.T) {
	// The grid is the screen. A layout that reports itself usable and then
	// cannot hold thirteen rows of lattice has drawn a broken calendar.
	for width := floorWidth; width <= 200; width += 3 {
		for height := floorHeight; height <= 60; height++ {
			f := layout(width, height, 2)
			if f.TooSmall {
				continue
			}
			if inner := f.Calendar.inner(); inner.W < gridColumns || inner.H < gridRows {
				t.Fatalf("%dx%d: the calendar panel's inside is %dx%d, want at least %dx%d",
					width, height, inner.W, inner.H, gridColumns, gridRows)
			}
		}
	}
}

// ///////////////////////////////////////////////
// The floor
// ///////////////////////////////////////////////

func TestLayout_SaysSoWhenNothingFits(t *testing.T) {
	// Below the floor the screen states what it needs rather than drawing a
	// broken arrangement.
	tests := []struct {
		name          string
		width, height int
	}{
		{name: "too narrow for the grid", width: floorWidth - 1, height: 40},
		{name: "too short for the grid", width: 120, height: floorHeight},
		{name: "nothing at all", width: 10, height: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := layout(tt.width, tt.height, 2); !got.TooSmall {
				t.Errorf("layout(%d, %d) drew an arrangement, want it refused",
					tt.width, tt.height)
			}
		})
	}
}

func TestLayout_AChipRowIsAlwaysReserved(t *testing.T) {
	// Zero chip rows would put the status line on the last row and hide the
	// keys entirely, which is the drift the pinned footer exists to stop.
	f := layout(120, 40, 0)

	if f.Chips.H < 1 {
		t.Errorf("chips = %+v, want at least one row reserved", f.Chips)
	}
	if f.Status.Y+f.Status.H != f.Chips.Y {
		t.Errorf("the status line at %+v does not sit directly above the chips at %+v",
			f.Status, f.Chips)
	}
}

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// assertRect compares a placed region with where the design puts it.
func assertRect(t *testing.T, name string, got, want rect) {
	t.Helper()

	if got != want {
		t.Errorf("%s = %+v, want %+v", name, got, want)
	}
}
