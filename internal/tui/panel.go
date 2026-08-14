package tui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// chip is one key in the footer.
//
// Keys is what the handler switches on, so a chip cannot advertise a key the
// pane does not take: a test walks every pane's chip set against the keys
// that pane accepts. Label is what is printed, which for a set of arrows is
// one glyph run standing for four keys.
type chip struct {
	Keys  []string
	Label string
	Hint  string
}

// ///////////////////////////////////////////////
// Chrome
// ///////////////////////////////////////////////

// The frame around a panel. Focus is border weight, which is a glyph
// difference rather than a colour one, so it survives NO_COLOR and a
// monochrome terminal. Colour is a third signal, never the only one.
const (
	focusedCorners   = "┏┓┗┛"
	unfocusedCorners = "╭╮╰╯"
)

// chipGap separates one footer chip from the next, and chipMargin is the
// left margin every line of chrome starts at.
//
// The margin matches a panel's inner text, one border and one padding column
// in, so the status line and the keys line up with the content above them.
const (
	chipGap    = 3
	chipMargin = 2
)

// ///////////////////////////////////////////////
// Panels
// ///////////////////////////////////////////////

// drawPanel frames a body, inlaying the title into the top border.
//
// It returns exactly r.H lines of exactly r.W cells. A body line short of the
// panel's inside is filled out rather than left ragged, because whatever the
// frame drew there last would otherwise show through.
func drawPanel(title string, body []string, r rect, focused bool, set styles) []string {
	if r.empty() {
		return nil
	}

	corners := []rune(unfocusedCorners)
	horizontal, vertical := lightHorizontal, lightVertical
	if focused {
		corners = []rune(focusedCorners)
		horizontal, vertical = heavyHorizontal, heavyVertical
	}

	inner := r.W - panelChrome
	lines := make([]string, 0, r.H)
	lines = append(lines, topBorder(title, r.W, corners, horizontal, focused, set))

	side := string(vertical)
	for row := range r.H - panelRows {
		text := ""
		if row < len(body) {
			text = body[row]
		}
		lines = append(lines, side+" "+pad(text, inner, cutEnd)+" "+side)
	}

	lines = append(lines, string(corners[2])+
		strings.Repeat(string(horizontal), max(r.W-2, 0))+string(corners[3]))
	return lines
}

// topBorder renders a panel's top edge with its title set into it.
func topBorder(title string, width int, corners []rune, horizontal rune, focused bool, set styles) string {
	rule := string(horizontal)
	if title == "" || width < 8 {
		return string(corners[0]) + strings.Repeat(rule, max(width-2, 0)) + string(corners[1])
	}

	title = fit(title, width-6, cutEnd)
	style := set.dim
	if focused {
		style = set.heading
	}

	head := string(corners[0]) + rule + " " + style.Render(title) + " "
	tail := max(width-ansi.StringWidth(head)-1, 0)
	return head + strings.Repeat(rule, tail) + string(corners[1])
}

// ///////////////////////////////////////////////
// The app bar
// ///////////////////////////////////////////////

// drawAppBar renders the top row: the name, the channel tabs, and what the
// volume holds.
//
// No border. It is one row of orientation above the panels, and a frame
// around a single line would cost two more.
func drawAppBar(name string, tabs []string, right string, r rect, set styles) string {
	var left strings.Builder
	left.WriteString(set.heading.Render(name))
	for _, tab := range tabs {
		left.WriteString("   " + tab)
	}

	gap := r.W - chipMargin - ansi.StringWidth(left.String()) - ansi.StringWidth(right)
	if gap < 1 {
		return pad(strings.Repeat(" ", chipMargin)+left.String(), r.W, cutEnd)
	}
	return strings.Repeat(" ", chipMargin) + left.String() + strings.Repeat(" ", gap) + right
}

// drawTab renders one channel tab as a number and a name sharing a
// background.
func drawTab(index int, name string, active bool, set styles) string {
	number := set.tabNumber
	label := set.tabLabel
	if active {
		number, label = set.tabNumberOn, set.tabLabelOn
	}
	return number.Render(" "+strconv.Itoa(index)+" ") + label.Render(name+" ")
}

// ///////////////////////////////////////////////
// The status line and the keys
// ///////////////////////////////////////////////

// drawStatus renders the standing condition on the left and a transient
// outcome flush right. The toast arrives already styled, because what it
// reports decides whether it reads as an outcome or a refusal.
//
// The split is by lifetime. A standing condition is where the library is and
// how much room is left, which stays true until it changes. A toast is what
// just happened, which expires. A toast that had to carry a standing
// condition would hide a library that will not open.
func drawStatus(standing, toast string, r rect, set styles) string {
	left := set.dim.Render(fit(standing, max(r.W-chipMargin*2, 1), cutMiddle))
	if toast == "" {
		return pad(strings.Repeat(" ", chipMargin)+left, r.W, cutEnd)
	}

	right := toast
	gap := r.W - chipMargin*2 - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 1 {
		return pad(strings.Repeat(" ", chipMargin)+right, r.W, cutEnd)
	}
	return strings.Repeat(" ", chipMargin) + left + strings.Repeat(" ", gap) + right +
		strings.Repeat(" ", chipMargin)
}

// drawChips renders the key rows, wrapped to the width.
func drawChips(chips []chip, r rect, set styles) []string {
	rows := wrapChips(chips, r.W)
	lines := make([]string, 0, r.H)

	for row := range r.H {
		if row >= len(rows) {
			lines = append(lines, strings.Repeat(" ", r.W))
			continue
		}

		var text strings.Builder
		text.WriteString(strings.Repeat(" ", chipMargin))
		for i, c := range rows[row] {
			if i > 0 {
				text.WriteString(strings.Repeat(" ", chipGap))
			}
			text.WriteString(set.heading.Render(c.Label) + " " + set.dim.Render(c.Hint))
		}
		lines = append(lines, pad(text.String(), r.W, cutEnd))
	}
	return lines
}

// wrapChips greedily fills rows with chips that fit the width.
//
// Chip widths are fixed, so how many rows the footer takes is a pure function
// of the width and the set. That is what lets the layout reserve the rows
// before anything is drawn, and it is why nothing can push the footer off the
// screen.
func wrapChips(chips []chip, width int) [][]chip {
	if width < 1 {
		return [][]chip{chips}
	}

	var rows [][]chip
	var row []chip
	column := chipMargin

	for _, c := range chips {
		w := chipWidth(c)
		if len(row) > 0 && column+chipGap+w > width {
			rows = append(rows, row)
			row, column = nil, chipMargin
		}
		if len(row) > 0 {
			column += chipGap
		}
		row = append(row, c)
		column += w
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// chipWidth is how many cells one chip takes, unstyled.
func chipWidth(c chip) int {
	return ansi.StringWidth(c.Label) + 1 + ansi.StringWidth(c.Hint)
}

// chipHeight is how many rows a chip set wraps onto at a width.
func chipHeight(chips []chip, width int) int {
	return max(len(wrapChips(chips, width)), 1)
}
