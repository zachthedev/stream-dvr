package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFollow_KeepsTheCursorInsideTheWindow(t *testing.T) {
	tests := []struct {
		name                   string
		offset, at, height, of int
		want                   int
	}{
		{name: "already visible", offset: 0, at: 2, height: 10, of: 20, want: 0},
		{name: "past the bottom", offset: 0, at: 12, height: 10, of: 20, want: 4},
		{name: "back above the top", offset: 10, at: 3, height: 10, of: 20, want: 2},
		{name: "the very first row", offset: 5, at: 0, height: 10, of: 20, want: 0},
		{name: "the very last row", offset: 0, at: 19, height: 10, of: 20, want: 10},
		{name: "a list shorter than the window", offset: 0, at: 2, height: 10, of: 4, want: 0},
		{name: "no window at all", offset: 3, at: 2, height: 0, of: 20, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := follow(tt.offset, tt.at, tt.height, tt.of); got != tt.want {
				t.Errorf("follow(%d, %d, %d, %d) = %d, want %d",
					tt.offset, tt.at, tt.height, tt.of, got, tt.want)
			}
		})
	}
}

func TestFollow_LeavesARowOfBufferPastTheCursor(t *testing.T) {
	// The counted hint replaces the first or last visible row, so the cursor
	// must never be on one. One row of buffer at each end is what keeps them
	// apart.
	rows := 40
	for at := range rows {
		offset := follow(0, at, 10, rows)

		top, bottom := offset, offset+10-1
		if at > 0 && at == top && top > 0 {
			t.Errorf("cursor %d sits on the top hint row at offset %d", at, offset)
		}
		if at < rows-1 && at == bottom && bottom < rows-1 {
			t.Errorf("cursor %d sits on the bottom hint row at offset %d", at, offset)
		}
	}
}

func TestScrolled_NamesAndCountsWhatWasCut(t *testing.T) {
	// Truncation is explicit and counted, never a silent cut. A list that
	// ran off the bottom with no sign of it is a list an operator reads as
	// complete.
	rows := make([]string, 20)
	for i := range rows {
		rows[i] = "row"
	}

	got := scrolled(rows, 5, 8, 40, newStyles(true))

	if len(got) != 8 {
		t.Fatalf("scrolled() drew %d rows, want 8", len(got))
	}
	if !strings.Contains(ansi.Strip(got[0]), "5 more above") {
		t.Errorf("the first row does not count what is above it: %q", got[0])
	}
	if !strings.Contains(ansi.Strip(got[7]), "7 more below") {
		t.Errorf("the last row does not count what is below it: %q", got[7])
	}
}

func TestScrolled_SaysNothingWhenNothingIsCut(t *testing.T) {
	rows := []string{"a", "b", "c"}

	got := scrolled(rows, 0, 5, 40, newStyles(true))

	joined := ansi.Strip(strings.Join(got, "\n"))
	if strings.Contains(joined, "more") {
		t.Errorf("a list that fits reports rows it does not have:\n%s", joined)
	}
	if len(got) != 5 {
		t.Errorf("scrolled() drew %d rows, want the window filled to 5", len(got))
	}
}

func TestScrolled_ClampsAnOffsetPastTheEnd(t *testing.T) {
	// A list that shrank under a scrolled window would otherwise draw
	// nothing at all.
	got := scrolled([]string{"a", "b", "c"}, 99, 3, 20, newStyles(true))

	if strings.TrimSpace(ansi.Strip(got[0])) == "" {
		t.Errorf("an offset past the end drew a blank list: %q", got)
	}
}

func TestScrolled_NoRoomDrawsNothing(t *testing.T) {
	if got := scrolled([]string{"a"}, 0, 0, 20, newStyles(true)); got != nil {
		t.Errorf("scrolled() with no height = %v, want nil", got)
	}
}
