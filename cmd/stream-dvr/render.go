package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"golang.org/x/term"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// outcome is what a result row reports.
//
// The glyph and the colour both follow from it, so a row cannot claim one
// thing with its mark and another with its colour.
type outcome int

// row is one result: a mark, what it is about, and everything else.
//
// Trailer is one string rather than further columns. A row that answers
// "which of these needs me" is read down the mark and the label, and detail
// that varies in length between rows sets no column width when it is last.
//
// Label and Trailer are printed as given, so text from anywhere but this
// build is escaped before it reaches here.
type row struct {
	State   outcome
	Label   string
	Trailer string
	// Path is a filesystem path the trailer ends with, cut in the middle
	// rather than at the end. Its last component is what identifies the
	// file, so a trailer elided as one string would drop the half that
	// answers which file this is.
	Path string
}

// progress is one step of a counted run.
type progress struct {
	State   outcome
	Index   int
	Total   int
	Subject string
	When    string
	Detail  string
}

// runner prints the rows of a counted run as they happen.
//
// A pass over a month of broadcasts takes minutes, so its rows cannot wait
// for the run to end and be laid out together. The columns are settled from
// what is known before the first row instead: the channels in scope, and the
// widest count the run could reach.
type runner struct {
	out      io.Writer
	subjects int
	budget   int
	total    int
	index    int
}

// sized is an output that knows how many columns it has.
//
// A test drives a real command through one of these to pin a width, which is
// the only way to prove elision without a terminal.
type sized interface {
	Width() int
}

// ///////////////////////////////////////////////
// Glyphs
// ///////////////////////////////////////////////

// The mark on a result row. glyphWarn is for a row that succeeded into a
// state the operator still has to act on, and glyphNote for one that reports
// rather than passes or fails.
const (
	glyphOK   = "✓"
	glyphBad  = "✗"
	glyphWarn = "!"
	glyphNote = "·"
)

// ellipsis stands for what was cut. One cell wide in every terminal that can
// draw it, unlike three periods, which cost three.
const ellipsis = "…"

// ///////////////////////////////////////////////
// Widths
// ///////////////////////////////////////////////

const (
	// outcomePass and the rest name what a row reports.
	outcomePass outcome = iota
	outcomeFail
	outcomeWarn
	outcomeNote
)

const (
	// rowIndent is the margin every row inside a section carries.
	rowIndent = 2
	// columnGap separates one column from the next.
	columnGap = 2
	// glyphColumn is the width of the mark, which is one cell.
	glyphColumn = 1
)

const (
	// assumedWidth is what a destination that is not a terminal gets. It is
	// the width every terminal has at least, so output that fits it fits
	// anywhere, including a pipe into a file someone opens later.
	assumedWidth = 80
	// widestUseful caps how far a row will stretch. A dim trailer running
	// the full width of a 300-column terminal is a line nobody tracks back
	// to its label, and the eye has to return to column zero either way.
	widestUseful = 110
	// narrowest is the floor below which eliding stops helping, because
	// what survives says less than the fact that something was cut.
	narrowest = 40
)

// ///////////////////////////////////////////////
// Styles
// ///////////////////////////////////////////////

// Render always emits ANSI, whatever it is rendering for, so the decision of
// whether the destination can take it belongs to the writer. Every command
// entry point passes its writer through styled() before rendering anything.
var (
	styleDim  = lipgloss.NewStyle().Faint(true)
	styleOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleBad  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleRow  = lipgloss.NewStyle().PaddingRight(columnGap)
)

// ///////////////////////////////////////////////
// The destination
// ///////////////////////////////////////////////

// styled reconciles a writer with what its destination can display.
//
// The profile covers a pipe, a redirect, NO_COLOR, TERM=dumb and a 16-colour
// terminal alike: a truecolor value is downsampled where it has to be and
// every sequence is stripped where none can be shown. Applied at each entry
// point rather than where os.Stdout is named, so a caller holding a buffer
// gets the same contract a terminal does.
func styled(out io.Writer) io.Writer {
	return colorprofile.NewWriter(out, os.Environ())
}

// columns reports how wide a line may be before it wraps.
//
// A destination that is not a terminal has no width to ask for, and the
// answer there has to be a constant rather than the writer's own idea of
// one: output redirected into a file is read later at whatever width the
// reader has.
func columns(out io.Writer) int {
	for {
		switch dest := out.(type) {
		case sized:
			return clampWidth(dest.Width())
		case *colorprofile.Writer:
			out = dest.Forward
		case *os.File:
			width, _, err := term.GetSize(int(dest.Fd()))
			if err != nil || width <= 0 {
				return assumedWidth
			}
			return clampWidth(width)
		default:
			return assumedWidth
		}
	}
}

// clampWidth holds a measured width inside what the layout can use.
func clampWidth(width int) int {
	return max(min(width, widestUseful), narrowest)
}

// ///////////////////////////////////////////////
// Fitting
// ///////////////////////////////////////////////

// fitEnd cuts the tail, marking that it did.
//
// For text whose beginning identifies it: a version string, a reason, a
// broadcast title.
func fitEnd(text string, width int) string {
	switch {
	case width <= 0:
		return ""
	case ansi.StringWidth(text) <= width:
		return text
	case width == 1:
		return ellipsis
	}
	return ansi.Truncate(text, width-1, ellipsis)
}

// fitMiddle keeps both ends, cutting the middle.
//
// For a path, where the leading directories say where it lives and the last
// component says which file it is. Neither end can go.
func fitMiddle(text string, width int) string {
	switch {
	case width <= 0:
		return ""
	case ansi.StringWidth(text) <= width:
		return text
	case width <= 2:
		return ellipsis
	}

	// The tail takes the larger half, because the file's own name is what
	// identifies it and a directory prefix is often shared with its
	// neighbours.
	keep := width - 1
	head := keep / 2
	tail := keep - head
	return ansi.Truncate(text, head, "") + ellipsis +
		ansi.TruncateLeft(text, ansi.StringWidth(text)-tail, "")
}

// ///////////////////////////////////////////////
// Blocks
// ///////////////////////////////////////////////

// section prints a labelled block of result rows.
//
// The label is dim and the rows follow it directly, so the block reads as one
// thing. A blank line above separates it from what came before; a blank line
// below it would separate the label from what it names.
//
// Column widths come from the renderer. The one width computed here is the
// trailer's budget, which is whatever the terminal has left once the fixed
// columns are spoken for.
func section(out io.Writer, label string, rows []row) {
	if len(rows) == 0 {
		return
	}

	labels := 0
	for _, r := range rows {
		labels = max(labels, ansi.StringWidth(r.Label))
	}
	budget := columns(out) - rowIndent - glyphColumn - columnGap - labels - columnGap

	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		cells = append(cells, []string{
			mark(r.State), r.Label, styleDim.Render(detail(r, budget)),
		})
	}

	fmt.Fprintf(out, "\n%s\n", styleDim.Render(label))
	writeRows(out, cells)
}

// detail renders a row's trailer, fitting a path into whatever the rest of
// the trailer leaves.
//
// The path is measured against the remainder rather than against a share of
// the budget, because the words before it are the ones that say what the path
// is for. A trailer long enough to leave nothing shows the path cut to
// nothing, which is still the truth.
func detail(r row, budget int) string {
	if r.Path == "" {
		return fitEnd(r.Trailer, budget)
	}
	if r.Trailer == "" {
		return fitMiddle(r.Path, budget)
	}

	text := fitEnd(r.Trailer, budget)
	return text + " " + fitMiddle(r.Path, budget-ansi.StringWidth(text)-1)
}

// steps prints a counted run, one row per step.
//
// The count is what says a run is progressing rather than stuck, and it is
// the reason a step that failed can be found again in a long scroll. A run
// whose steps are all undated drops the date column rather than leaving a
// gap where one would be.
func steps(out io.Writer, label string, entries []progress) {
	if len(entries) == 0 {
		return
	}

	counts, subjects, whens := 0, 0, 0
	for _, entry := range entries {
		counts = max(counts, ansi.StringWidth(count(entry)))
		subjects = max(subjects, ansi.StringWidth(entry.Subject))
		whens = max(whens, ansi.StringWidth(entry.When))
	}
	budget := columns(out) - rowIndent - glyphColumn - columnGap - counts - columnGap - subjects - columnGap
	if whens > 0 {
		budget -= whens + columnGap
	}

	cells := make([][]string, 0, len(entries))
	for _, entry := range entries {
		row := []string{mark(entry.State), count(entry), entry.Subject}
		if whens > 0 {
			row = append(row, entry.When)
		}
		cells = append(cells, append(row, styleDim.Render(fitEnd(entry.Detail, budget))))
	}

	fmt.Fprintf(out, "\n%s\n", styleDim.Render(label))
	writeRows(out, cells)
}

// newRunner opens a counted run over a known set of subjects.
func newRunner(out io.Writer, label string, subjects []string, total int) *runner {
	widest := 0
	for _, subject := range subjects {
		widest = max(widest, ansi.StringWidth(subject))
	}

	counts := ansi.StringWidth(count(progress{Index: total, Total: total}))
	fmt.Fprintf(out, "\n%s\n", styleDim.Render(label))
	return &runner{
		out:      out,
		subjects: widest,
		budget: columns(out) - rowIndent - glyphColumn - columnGap -
			counts - columnGap - widest - columnGap,
		total: total,
	}
}

// step prints one row of the run.
func (r *runner) step(state outcome, subject, detail string) {
	r.index++
	entry := progress{Index: r.index, Total: r.total}
	writeRows(r.out, [][]string{{
		mark(state),
		count(entry),
		fitEnd(subject, r.subjects),
		styleDim.Render(fitEnd(detail, r.budget)),
	}})
}

// pairs prints a key and its value, the value on its own line.
//
// A value here is a path, and a path is the thing an operator copies.
//
// Nothing on these lines is cut. A path that shares a line with other columns
// is cut to fit, because it is there for orientation and a ragged wrap would
// break the columns beside it. A path on its own line is the one that gets
// copied, and a copied path with an ellipsis in the middle of it goes
// nowhere. A terminal too narrow for it wraps instead, and a wrapped path
// still pastes.
func pairs(out io.Writer, label string, keyed [][2]string) {
	if len(keyed) == 0 {
		return
	}

	fmt.Fprintf(out, "\n%s\n", styleDim.Render(label))
	for _, pair := range keyed {
		fmt.Fprintf(out, "  %s\n", styleDim.Render(pair[0]))
		fmt.Fprintf(out, "    %s\n", pair[1])
	}
}

// summary closes a command with what it counted and what to do next.
//
// The next action is the point: a count alone leaves an operator who has
// never run this before at a prompt with nothing to type. Where there is
// genuinely nothing to do next, next is empty and the count stands alone.
func summary(out io.Writer, counted, next string) {
	fmt.Fprintf(out, "\n  %s\n", counted)
	if next != "" {
		fmt.Fprintf(out, "  %s\n", styleDim.Render(next))
	}
	fmt.Fprintln(out)
}

// failure prints a refusal: what happened, and under it why or what to do.
//
// An error is a result row of one, so it takes the same mark every other
// outcome does rather than going to the terminal as bare text.
//
// G705: every caller escapes with escape.Text first, which is the sanitizer. The sink is a terminal, not a web response.
func failure(out io.Writer, cause, hint string) {
	lines := wrapped(cause, columns(out)-3)
	fmt.Fprintf(out, "\n%s  %s\n", styleBad.Render(glyphBad), lines[0])
	for _, line := range lines[1:] {
		fmt.Fprintf(out, "   %s\n", line)
	}
	if hint != "" {
		indented(out, 5, styleDim, hint)
	}
	fmt.Fprintln(out)
}

// banner opens a command that does not return until it is stopped.
//
// The stop instruction is last and dim. It is the one line the operator needs
// once the facts above it stop being new, and a command that blocks without
// ever saying so reads as one that finished.
func banner(out io.Writer, state string, facts []string, stop string) {
	fmt.Fprintf(out, "\n%s  %s\n", styleOK.Render(glyphOK), state)
	// Uncut, for the reason pairs() states: the socket and the log path
	// are what an operator copies out of this.
	for _, fact := range facts {
		fmt.Fprintf(out, "     %s\n", styleDim.Render(fact))
	}
	fmt.Fprintf(out, "\n  %s\n\n", styleDim.Render(stop))
}

// ///////////////////////////////////////////////
// Parts
// ///////////////////////////////////////////////

// wrapped breaks prose at a width, on whole words.
//
// A sentence is not a row. Cutting one loses what it said, so it wraps
// instead, and it wraps here rather than at the terminal's own margin so
// the continuation lines keep the indent of the line they belong to.
func wrapped(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	return strings.Split(ansi.Wordwrap(text, width, ""), "\n")
}

// indented writes each line of a wrapped block under the same margin.
//
// G705: every caller escapes with escape.Text first, which is the sanitizer. The sink is a terminal, not a web response.
func indented(out io.Writer, margin int, style lipgloss.Style, text string) {
	pad := strings.Repeat(" ", margin)
	for _, line := range wrapped(text, columns(out)-margin) {
		fmt.Fprintf(out, "%s%s\n", pad, style.Render(line))
	}
}

// mark renders the glyph an outcome carries.
func mark(state outcome) string {
	switch state {
	case outcomePass:
		return styleOK.Render(glyphOK)
	case outcomeFail:
		return styleBad.Render(glyphBad)
	case outcomeWarn:
		return styleWarn.Render(glyphWarn)
	case outcomeNote:
		return styleDim.Render(glyphNote)
	default:
		return styleDim.Render(glyphNote)
	}
}

// count renders a step's position in the run.
//
// A run whose length is not known until it ends shows the position alone,
// rather than a denominator that would have to be guessed.
func count(entry progress) string {
	if entry.Total <= 0 {
		return fmt.Sprintf("%d", entry.Index)
	}
	return fmt.Sprintf("%d/%d", entry.Index, entry.Total)
}

// trailer joins the parts of a result row's detail into one string.
func trailer(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " · ")
}

// plural picks a word form for a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// writeRows lays out cells in columns and indents the block.
//
// The renderer owns the padding. Column widths that agree across rows are
// what makes a block scannable, and they are not something to compute by
// hand once a cell can hold a wide rune or a styled glyph.
func writeRows(out io.Writer, cells [][]string) {
	rendered := table.New().
		Border(lipgloss.Border{}).
		BorderTop(false).BorderBottom(false).
		BorderLeft(false).BorderRight(false).
		BorderColumn(false).BorderRow(false).BorderHeader(false).
		StyleFunc(func(_, _ int) lipgloss.Style { return styleRow }).
		Rows(cells...).
		Render()

	for line := range strings.SplitSeq(rendered, "\n") {
		fmt.Fprintf(out, "  %s\n", strings.TrimRight(line, " "))
	}
}
