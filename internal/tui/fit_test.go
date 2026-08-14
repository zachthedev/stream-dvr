package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"

	"zach.tools/go/stream-dvr/internal/escape"
)

func TestFit(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		width int
		mode  elision
		want  string
	}{
		{name: "it already fits", text: "abcde", width: 5, mode: cutEnd, want: "abcde"},
		{name: "the tail goes", text: "abcdefgh", width: 5, mode: cutEnd, want: "abcd…"},
		{name: "the head goes", text: "abcdefgh", width: 5, mode: cutStart, want: "…efgh"},
		{name: "the middle goes", text: "abcdefgh", width: 5, mode: cutMiddle, want: "ab…gh"},
		{
			// The tail takes the larger half: a directory prefix is often
			// shared with its neighbours and the file's own name is what
			// tells them apart.
			name: "an odd middle favours the tail", text: "abcdefghij",
			width: 6, mode: cutMiddle, want: "ab…hij",
		},
		{name: "one column is the mark alone", text: "abcde", width: 1, mode: cutEnd, want: "…"},
		{name: "no columns is nothing", text: "abcde", width: 0, mode: cutEnd, want: ""},
		{name: "a negative width is nothing", text: "abcde", width: -3, mode: cutEnd, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fit(tt.text, tt.width, tt.mode); got != tt.want {
				t.Errorf("fit(%q, %d) = %q, want %q", tt.text, tt.width, got, tt.want)
			}
		})
	}
}

func TestFit_NeverExceedsItsWidth(t *testing.T) {
	// Every render function draws inside a rect and cannot exceed it. One
	// cell over splits the panel border beside it from that row down.
	texts := []string{
		"", "a", "abcdefghijklmnop",
		"日本語のタイトル", // two cells per rune
		"a日b本c語d",
		"café latte", // a combining accent, which is no cells of its own
	}

	for _, text := range texts {
		for width := range 20 {
			for _, mode := range []elision{cutEnd, cutMiddle, cutStart} {
				got := ansi.StringWidth(fit(text, width, mode))
				if got > width {
					t.Errorf("fit(%q, %d, %d) is %d cells wide", text, width, mode, got)
				}
			}
		}
	}
}

func TestFit_CutsTheEscapedFormNotTheRawOne(t *testing.T) {
	// escape.Text expands one control byte into four characters and may
	// return a quoted literal, so a string cut before it is escaped grows
	// back past the width it was cut to.
	raw := "name\x1b[2Jhere.mkv"
	escaped := escape.Text(raw)

	if got := ansi.StringWidth(fit(escaped, 12, cutEnd)); got > 12 {
		t.Errorf("the escaped form fits to %d cells, want at most 12", got)
	}

	// The other order, stated as the thing not to do: cutting first and
	// escaping after produces something wider than it was cut to.
	if got := ansi.StringWidth(escape.Text(fit(raw, 12, cutEnd))); got <= 12 {
		t.Skip("this build's escape.Text does not expand this input, so the ordering is moot")
	}
}

func TestPad_FillsOutToTheWidth(t *testing.T) {
	// A cell short of its column leaves whatever the frame drew there last
	// showing through, which is how a panel border ends up inside a table.
	for width := range 12 {
		for _, text := range []string{"", "ab", "abcdefghijkl"} {
			if got := ansi.StringWidth(pad(text, width, cutEnd)); got != width {
				t.Errorf("pad(%q, %d) is %d cells, want exactly %d", text, width, got, width)
			}
			if got := ansi.StringWidth(padLeft(text, width, cutEnd)); got != width {
				t.Errorf("padLeft(%q, %d) is %d cells, want exactly %d", text, width, got, width)
			}
		}
	}
}

func TestPadLeft_RightAligns(t *testing.T) {
	// A count reads down its last digit, so the column is filled from the
	// left.
	if got := padLeft("7", 4, cutEnd); got != "   7" {
		t.Errorf("padLeft() = %q, want the number against the right edge", got)
	}
}
