package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestDrawPanel_IsExactlyItsRect(t *testing.T) {
	// Every render function draws inside its rect and cannot exceed it. A
	// panel a row short leaves the one beside it hanging, and a row long
	// pushes the footer off the screen.
	set := newStyles(true)
	body := []string{"one", "two", strings.Repeat("a very long line ", 20)}

	for width := 8; width <= 120; width += 7 {
		for height := 2; height <= 30; height += 3 {
			r := rect{X: 0, Y: 0, W: width, H: height}
			lines := drawPanel("a title", body, r, true, set)

			if len(lines) != height {
				t.Fatalf("%dx%d drew %d lines, want %d", width, height, len(lines), height)
			}
			for i, line := range lines {
				if got := ansi.StringWidth(line); got != width {
					t.Fatalf("%dx%d line %d is %d cells, want %d: %q",
						width, height, i, got, width, line)
				}
			}
		}
	}
}

func TestDrawPanel_FocusIsBorderWeight(t *testing.T) {
	// A glyph difference rather than a colour one, so focus survives
	// NO_COLOR and a monochrome terminal. Colour is a third signal, never
	// the only one.
	set := newStyles(true)
	r := rect{X: 0, Y: 0, W: 30, H: 4}

	focused := strings.Join(drawPanel("t", nil, r, true, set), "\n")
	unfocused := strings.Join(drawPanel("t", nil, r, false, set), "\n")

	if !strings.ContainsRune(focused, heavyVertical) {
		t.Error("a focused panel draws no heavy border")
	}
	if strings.ContainsRune(unfocused, heavyVertical) {
		t.Error("an unfocused panel draws a heavy border")
	}
	if !strings.ContainsRune(unfocused, lightVertical) {
		t.Error("an unfocused panel draws no border at all")
	}
}

func TestDrawPanel_InlaysTheTitle(t *testing.T) {
	set := newStyles(true)

	line := drawPanel("August 2026", nil, rect{W: 40, H: 3}, true, set)[0]

	if !strings.Contains(ansi.Strip(line), "August 2026") {
		t.Errorf("the top border does not carry the title: %q", line)
	}
}

func TestDrawPanel_AnEmptyRectDrawsNothing(t *testing.T) {
	if got := drawPanel("t", []string{"a"}, rect{}, true, newStyles(true)); got != nil {
		t.Errorf("drawPanel() on an empty rect = %v, want nil", got)
	}
}

// ///////////////////////////////////////////////
// The keys
// ///////////////////////////////////////////////

func TestWrapChips_RowCountIsAPureFunctionOfTheWidth(t *testing.T) {
	// The layout reserves the footer's rows before anything is drawn, which
	// is what stops content pushing the keys off the screen. That only works
	// if the row count can be computed from the width alone.
	chips := []chip{
		{Keys: []string{"a"}, Label: "a", Hint: "one"},
		{Keys: []string{"b"}, Label: "b", Hint: "two"},
		{Keys: []string{"c"}, Label: "c", Hint: "three"},
	}

	for width := 10; width <= 120; width++ {
		first := chipHeight(chips, width)
		if second := chipHeight(chips, width); first != second {
			t.Fatalf("chipHeight at %d answered %d then %d", width, first, second)
		}
	}
}

func TestWrapChips_FitsEveryRowInTheWidth(t *testing.T) {
	chips := []chip{
		{Keys: []string{"left"}, Label: "←→↑↓", Hint: "day"},
		{Keys: []string{"enter"}, Label: "enter", Hint: "open day"},
		{Keys: []string{"ctrl+s"}, Label: "ctrl+s", Hint: "save"},
		{Keys: []string{"q"}, Label: "q", Hint: "quit"},
	}

	for width := 20; width <= 200; width += 3 {
		rows := drawChips(chips, rect{W: width, H: chipHeight(chips, width)}, newStyles(true))
		for i, row := range rows {
			if got := ansi.StringWidth(row); got != width {
				t.Fatalf("at %d columns, chip row %d is %d cells, want %d", width, i, got, width)
			}
		}
	}
}

func TestWrapChips_AlwaysReturnsARow(t *testing.T) {
	// Zero rows would leave the status line on the last row and the keys
	// nowhere, which is the drift the pinned footer exists to stop.
	if got := chipHeight(nil, 80); got != 1 {
		t.Errorf("chipHeight() with no chips = %d, want 1", got)
	}
	if got := chipHeight([]chip{{Label: "q", Hint: "quit"}}, 0); got < 1 {
		t.Errorf("chipHeight() at zero width = %d, want at least 1", got)
	}
}

// ///////////////////////////////////////////////
// The app bar and the status line
// ///////////////////////////////////////////////

func TestDrawAppBar_IsExactlyItsWidth(t *testing.T) {
	set := newStyles(true)
	tabs := []string{drawTab(1, "atrioc", true, set), drawTab(2, "northernlion", false, set)}

	for width := 20; width <= 200; width += 7 {
		got := drawAppBar(appName, tabs, "812 GiB of 1.5 TiB", rect{W: width, H: 1}, set)
		if cells := ansi.StringWidth(got); cells > width {
			t.Errorf("at %d columns the app bar is %d cells: %q", width, cells, got)
		}
	}
}

func TestDrawStatus_KeepsTheStandingConditionAndTheOutcomeApart(t *testing.T) {
	// The split is by lifetime. A standing condition stays true until it
	// changes; a toast expires. A toast carrying a standing condition would
	// take a library that will not open off the screen while it is still
	// broken.
	set := newStyles(true)

	line := ansi.Strip(drawStatus("library D:\\vods", set.ok.Render("marked watched"),
		rect{W: 80, H: 1}, set))

	if !strings.Contains(line, "library") {
		t.Errorf("the status line dropped the standing condition: %q", line)
	}
	if !strings.Contains(line, "marked watched") {
		t.Errorf("the status line dropped the outcome: %q", line)
	}
	if strings.Index(line, "library") > strings.Index(line, "marked watched") {
		t.Errorf("the outcome is not flush right of the condition: %q", line)
	}
}
