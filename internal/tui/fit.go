package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// elision is which end of a string is cut when it will not fit.
type elision int

// ///////////////////////////////////////////////
// Modes
// ///////////////////////////////////////////////

const (
	// cutEnd keeps the beginning, for text that is identified by its start:
	// a reason, a version, a broadcast title.
	cutEnd elision = iota
	// cutMiddle keeps both ends, for a name whose last part identifies it.
	cutMiddle
	// cutStart keeps the end, for a path whose final components say which
	// file it is.
	cutStart
)

// ellipsis stands for what was cut. One cell wide wherever it can be drawn,
// unlike three periods, which cost three.
const ellipsis = "…"

// ///////////////////////////////////////////////
// Fitting
// ///////////////////////////////////////////////

// fit cuts text to a width, marking that it did.
//
// The text handed here is already escaped. escape.Text expands one control
// byte into four characters and may return a quoted literal, so a string cut
// before it is escaped can grow past the width it was cut to, and a string
// measured before escaping is measured wrong.
func fit(text string, width int, mode elision) string {
	switch {
	case width <= 0:
		return ""
	case ansi.StringWidth(text) <= width:
		return text
	case width == 1:
		return ellipsis
	}

	// ansi.Truncate counts its tail inside the length it is given, so the
	// width passed is the whole answer rather than the width less the mark.
	if mode == cutEnd {
		return ansi.Truncate(text, width, ellipsis)
	}

	// A cut that lands inside a wide rune keeps the whole rune, so a first
	// attempt can come back one cell over. Each step gives up one more cell,
	// which is at most one step per wide rune on the boundary.
	total := ansi.StringWidth(text)
	for give := range width {
		keep := width - 1 - give
		if keep < 0 {
			break
		}

		got := ellipsis + ansi.TruncateLeft(text, total-keep, "")
		if mode == cutMiddle {
			// The tail takes the larger half: a directory prefix is often
			// shared with its neighbours, and the file's own name is what
			// tells them apart.
			head := keep / 2
			got = ansi.Truncate(text, head, "") + ellipsis +
				ansi.TruncateLeft(text, total-(keep-head), "")
		}
		if ansi.StringWidth(got) <= width {
			return got
		}
	}
	return ellipsis
}

// pad fits text to a width and then fills it out to exactly that width.
//
// Every render function draws inside a rect and cannot exceed it. A cell
// short of its column leaves whatever the frame drew there last showing
// through, which is how a panel border ends up inside a table.
func pad(text string, width int, mode elision) string {
	text = fit(text, width, mode)
	if short := width - ansi.StringWidth(text); short > 0 {
		return text + strings.Repeat(" ", short)
	}
	return text
}

// padLeft fits text to a width and right-aligns it, for a count or a size.
func padLeft(text string, width int, mode elision) string {
	text = fit(text, width, mode)
	if short := width - ansi.StringWidth(text); short > 0 {
		return strings.Repeat(" ", short) + text
	}
	return text
}
