package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// ///////////////////////////////////////////////
// Scrolling
// ///////////////////////////////////////////////

// scrollBuffer is how many rows stay visible past the cursor.
//
// One above and one below, so the counted hint that replaces the first or
// last visible row never lands on the row the cursor is on.
const scrollBuffer = 1

// follow moves a window so the cursor stays inside it.
//
// The buffer is dropped on a window too short to hold one, because a list
// three rows tall showing two hints and one row says nothing at all.
func follow(offset, at, height, total int) int {
	if height < 1 {
		return 0
	}

	buffer := scrollBuffer
	if height < 3 {
		buffer = 0
	}

	offset = min(offset, at-buffer)
	if at+buffer >= offset+height {
		offset = at + buffer - height + 1
	}
	return max(min(offset, max(total-height, 0)), 0)
}

// scrolled returns the visible rows of a list, with what was cut named and
// counted.
//
// Truncation is explicit and counted, never a silent cut. A list that ran off
// the bottom with no sign of it is a list an operator reads as complete.
func scrolled(rows []string, offset, height, width int, set styles) []string {
	if height < 1 {
		return nil
	}
	offset = max(min(offset, max(len(rows)-height, 0)), 0)

	visible := make([]string, 0, height)
	for row := range height {
		if index := offset + row; index < len(rows) {
			visible = append(visible, rows[index])
			continue
		}
		visible = append(visible, "")
	}

	if offset > 0 {
		visible[0] = set.dim.Render(centre(fmt.Sprintf("↑ %d more above", offset), width))
	}
	if below := len(rows) - offset - height; below > 0 {
		visible[height-1] = set.dim.Render(centre(fmt.Sprintf("↓ %d more below", below), width))
	}
	return visible
}

// centre places text in the middle of a width.
func centre(text string, width int) string {
	text = fit(text, width, cutEnd)
	left := max((width-ansi.StringWidth(text))/2, 0)
	return strings.Repeat(" ", left) + pad(text, max(width-left, 0), cutEnd)
}
