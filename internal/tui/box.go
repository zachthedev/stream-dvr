package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"

	"charm.land/lipgloss/v2"

	"zach.tools/go/stream-dvr/internal/calendar"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// weight is how heavily a lattice segment is drawn.
//
// The cursor is a heavy cell border inside a light lattice, which is the same
// signal the panel frame uses for focus one level up. It needs no colour, so
// it survives NO_COLOR and a monochrome terminal.
type weight int

// arms are the four directions leaving a lattice intersection.
type arms struct {
	Up, Right, Down, Left weight
}

// face is what the renderer needs to draw one cell's interior.
//
// Text is exactly gridInterior columns wide and already escaped. Style is
// applied to the interior alone: a shared vertical belongs to two cells and
// cannot carry either one's state, so the lattice is never coloured.
type face struct {
	Text  string
	Style lipgloss.Style
}

// cursor names the cell the heavy border is drawn around.
type cursor struct {
	Week, Day int
}

// ///////////////////////////////////////////////
// Weights
// ///////////////////////////////////////////////

const (
	// none is an arm that is not there, at the edge of the lattice.
	none weight = iota
	light
	heavy
)

// ///////////////////////////////////////////////
// Glyphs
// ///////////////////////////////////////////////

// The straight runs between intersections.
const (
	lightHorizontal  = '─'
	heavyHorizontal  = '━'
	lightVertical    = '│'
	heavyVertical    = '┃'
	dashedHorizontal = '┄'
	dashedVertical   = '┆'
)

// junctions maps a set of arms to the glyph that draws it.
//
// All seventy-two two-or-more-arm combinations of light and heavy exist in
// U+2500..U+257F, which is what lets a heavy cursor sit inside a light
// lattice without any junction having to be approximated. The table is read
// out of the Unicode names rather than typed from a chart.
var junctions = map[arms]rune{
	{none, none, light, light}:   '┐',
	{none, none, light, heavy}:   '┑',
	{none, none, heavy, light}:   '┒',
	{none, none, heavy, heavy}:   '┓',
	{none, light, none, light}:   '─',
	{none, light, none, heavy}:   '╾',
	{none, light, light, none}:   '┌',
	{none, light, heavy, none}:   '┎',
	{none, heavy, none, light}:   '╼',
	{none, heavy, none, heavy}:   '━',
	{none, heavy, light, none}:   '┍',
	{none, heavy, heavy, none}:   '┏',
	{light, none, none, light}:   '┘',
	{light, none, none, heavy}:   '┙',
	{light, none, light, none}:   '│',
	{light, none, heavy, none}:   '╽',
	{light, light, none, none}:   '└',
	{light, heavy, none, none}:   '┕',
	{heavy, none, none, light}:   '┚',
	{heavy, none, none, heavy}:   '┛',
	{heavy, none, light, none}:   '╿',
	{heavy, none, heavy, none}:   '┃',
	{heavy, light, none, none}:   '┖',
	{heavy, heavy, none, none}:   '┗',
	{none, light, light, light}:  '┬',
	{none, light, light, heavy}:  '┭',
	{none, light, heavy, light}:  '┰',
	{none, light, heavy, heavy}:  '┱',
	{none, heavy, light, light}:  '┮',
	{none, heavy, light, heavy}:  '┯',
	{none, heavy, heavy, light}:  '┲',
	{none, heavy, heavy, heavy}:  '┳',
	{light, none, light, light}:  '┤',
	{light, none, light, heavy}:  '┥',
	{light, none, heavy, light}:  '┧',
	{light, none, heavy, heavy}:  '┪',
	{light, light, none, light}:  '┴',
	{light, light, none, heavy}:  '┵',
	{light, light, light, none}:  '├',
	{light, light, heavy, none}:  '┟',
	{light, heavy, none, light}:  '┶',
	{light, heavy, none, heavy}:  '┷',
	{light, heavy, light, none}:  '┝',
	{light, heavy, heavy, none}:  '┢',
	{heavy, none, light, light}:  '┦',
	{heavy, none, light, heavy}:  '┩',
	{heavy, none, heavy, light}:  '┨',
	{heavy, none, heavy, heavy}:  '┫',
	{heavy, light, none, light}:  '┸',
	{heavy, light, none, heavy}:  '┹',
	{heavy, light, light, none}:  '┞',
	{heavy, light, heavy, none}:  '┠',
	{heavy, heavy, none, light}:  '┺',
	{heavy, heavy, none, heavy}:  '┻',
	{heavy, heavy, light, none}:  '┡',
	{heavy, heavy, heavy, none}:  '┣',
	{light, light, light, light}: '┼',
	{light, light, light, heavy}: '┽',
	{light, light, heavy, light}: '╁',
	{light, light, heavy, heavy}: '╅',
	{light, heavy, light, light}: '┾',
	{light, heavy, light, heavy}: '┿',
	{light, heavy, heavy, light}: '╆',
	{light, heavy, heavy, heavy}: '╈',
	{heavy, light, light, light}: '╀',
	{heavy, light, light, heavy}: '╃',
	{heavy, light, heavy, light}: '╂',
	{heavy, light, heavy, heavy}: '╉',
	{heavy, heavy, light, light}: '╄',
	{heavy, heavy, light, heavy}: '╇',
	{heavy, heavy, heavy, light}: '╊',
	{heavy, heavy, heavy, heavy}: '╋',
}

// ///////////////////////////////////////////////
// Intersections
// ///////////////////////////////////////////////

// junction returns the glyph for an intersection of four arms.
//
// A combination with fewer than two arms has no junction to draw and answers
// with a space, which keeps the lattice rectangular rather than short.
func junction(up, right, down, left weight) rune {
	if glyph, ok := junctions[arms{up, right, down, left}]; ok {
		return glyph
	}
	return ' '
}

// at reports whether the cursor sits on a cell.
func (c cursor) at(week, day int) bool {
	return c.Week == week && c.Day == day
}

// verticalWeight is how heavily the line at a column between two rules is
// drawn.
//
// It is the shared edge of the cells either side of it, so either one being
// the cursor makes it heavy.
func verticalWeight(c cursor, week, column int) weight {
	if c.at(week, column-1) || c.at(week, column) {
		return heavy
	}
	return light
}

// horizontalWeight is how heavily the segment over a cell column on a rule is
// drawn: the shared edge of the cells above and below it.
func horizontalWeight(c cursor, rule, column int) weight {
	if c.at(rule-1, column) || c.at(rule, column) {
		return heavy
	}
	return light
}

// ///////////////////////////////////////////////
// The grid
// ///////////////////////////////////////////////

// drawGrid renders a month as boxed cells sharing their borders.
//
// The lattice carries two signals and no colour. The cursor is a heavy cell
// border. A degraded range, where a row could not be read and the tallies may
// be short, dashes the outer frame: degraded is a property of the whole
// range, so a per-cell mark would claim to know which day lost a row.
//
// Corners and junctions stay solid when dashed, because Unicode has no dashed
// junctions and an approximated one would read as a different weight.
func drawGrid(headings []string, faces [][]face, at cursor, degraded bool) []string {
	weeks := len(faces)
	if weeks == 0 {
		return nil
	}

	lines := make([]string, 0, weeks*2+1)
	for rule := range weeks + 1 {
		lines = append(lines, ruleLine(rule, weeks, headings, at, degraded))
		if rule < weeks {
			lines = append(lines, cellLine(faces[rule], rule, at, degraded))
		}
	}
	return lines
}

// ruleLine renders one horizontal line of the lattice.
//
// The weekday labels are inlaid into the top rule rather than taking a row of
// their own, which is what makes six weeks of boxed cells fit in thirteen
// rows at both terminal sizes.
func ruleLine(rule, weeks int, headings []string, at cursor, degraded bool) string {
	outer := rule == 0 || rule == weeks

	var b strings.Builder
	for column := range calendar.DaysPerWeek + 1 {
		b.WriteRune(intersection(rule, weeks, column, at))
		if column == calendar.DaysPerWeek {
			break
		}

		w := horizontalWeight(at, rule, column)
		b.WriteString(run(w, true, outer && degraded, label(rule, headings, column)))
	}
	return b.String()
}

// cellLine renders one row of cell interiors between two rules.
func cellLine(week []face, row int, at cursor, degraded bool) string {
	var b strings.Builder
	for column, cell := range week {
		b.WriteRune(edge(verticalWeight(at, row, column), degraded && column == 0))
		b.WriteString(cell.Style.Render(cell.Text))
	}
	b.WriteRune(edge(verticalWeight(at, row, len(week)), degraded))
	return b.String()
}

// intersection returns the glyph where a rule meets a column.
func intersection(rule, weeks, column int, at cursor) rune {
	up, down, left, right := none, none, none, none
	if rule > 0 {
		up = verticalWeight(at, rule-1, column)
	}
	if rule < weeks {
		down = verticalWeight(at, rule, column)
	}
	if column > 0 {
		left = horizontalWeight(at, rule, column-1)
	}
	if column < calendar.DaysPerWeek {
		right = horizontalWeight(at, rule, column)
	}
	return junction(up, right, down, left)
}

// run renders the interior columns of one cell's horizontal segment, with a
// weekday label inlaid where the top rule carries one.
func run(w weight, horizontal, dashed bool, inlaid string) string {
	glyph := edgeGlyph(w, horizontal, dashed)
	if inlaid == "" {
		return strings.Repeat(string(glyph), gridInterior)
	}

	// One fill, the label, then the rest: "─Su──". The label is two columns,
	// so the run reads as a rule the label sits on rather than as a heading
	// with a rule beside it.
	// The label's width in columns, not its length in bytes, and never a
	// negative repeat: strings.Repeat panics on one, and a heading wider
	// than the cell it sits in would be the input that produced it.
	fill := max(gridInterior-1-ansi.StringWidth(inlaid), 0)
	return string(glyph) + inlaid + strings.Repeat(string(glyph), fill)
}

// edge returns the glyph for a vertical segment beside a cell.
func edge(w weight, dashed bool) rune {
	return edgeGlyph(w, false, dashed)
}

// edgeGlyph picks the straight run for a weight.
//
// A heavy run is never dashed. Heavy means the cursor and dashed means a
// degraded range, and a glyph carrying both would say neither clearly.
func edgeGlyph(w weight, horizontal, dashed bool) rune {
	switch {
	case w == heavy && horizontal:
		return heavyHorizontal
	case w == heavy:
		return heavyVertical
	case dashed && horizontal:
		return dashedHorizontal
	case dashed:
		return dashedVertical
	case horizontal:
		return lightHorizontal
	default:
		return lightVertical
	}
}

// label returns the weekday heading inlaid at a column of the top rule.
func label(rule int, headings []string, column int) string {
	if rule != 0 || column >= len(headings) {
		return ""
	}
	return headings[column]
}
