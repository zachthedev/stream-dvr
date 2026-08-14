package tui

import (
	"maps"

	"charm.land/lipgloss/v2"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Style set
// ///////////////////////////////////////////////

// styles holds every style a screen draws with, resolved once against the
// terminal's background rather than at each use.
//
// The scheme arrives as a tea.BackgroundColorMsg during the event loop, so
// the set is rebuilt on that message. Reading the terminal at package init
// instead would do blocking I/O outside the loop, on a program that may
// never own a terminal at all.
type styles struct {
	dark    bool
	heading lipgloss.Style
	dim     lipgloss.Style
	padding lipgloss.Style
	err     lipgloss.Style
	ok      lipgloss.Style
	warn    lipgloss.Style
	// caret marks the selection in a list, in both panels at once, so a
	// queue row stays tied to the day the grid cursor is on even while the
	// queue does not take the keys.
	caret lipgloss.Style
	// tabNumber and tabLabel are the two halves of a channel chip, sharing
	// one background so the pair reads as one object.
	tabNumber   lipgloss.Style
	tabLabel    lipgloss.Style
	tabNumberOn lipgloss.Style
	tabLabelOn  lipgloss.Style
	// coverage styles a state's name, in the legend and in a label.
	coverage map[store.Coverage]lipgloss.Style
	// cells styles a state inside a grid square, where the three worth
	// acting on are washed.
	cells map[store.Coverage]lipgloss.Style
}

// ///////////////////////////////////////////////
// Glyphs
// ///////////////////////////////////////////////

// glyphToday marks the current date.
const glyphToday = "*"

// glyphElsewhere marks a day belonging to a neighbouring month, which the
// enclosing parentheses of a padding cell carry instead.
const glyphElsewhere = " "

// glyphUnrecognized marks a coverage state this build has no glyph for, so a
// state added to the store still reads as something the operator was not
// told about rather than as a settled day.
const glyphUnrecognized = "?"

// The marks an operator sets on a recording. Both are ASCII, because the day
// pane's columns line up by byte width and a terminal that renders these two
// at double width would step every size below them out of true.
const (
	glyphWatched = "w"
	glyphPinned  = "@"
)

// ///////////////////////////////////////////////
// Palette
// ///////////////////////////////////////////////

// Colors are chosen so the states worth acting on, a missed broadcast, a
// partly captured one, and a capture that has not reached the library, are
// the only warm ones on the grid. Everything settled recedes.
var (
	colorMissed    = lipgloss.Color("1")
	colorPartial   = lipgloss.Color("3")
	colorAtRisk    = lipgloss.Color("5")
	colorLive      = lipgloss.Color("2")
	colorRecovered = lipgloss.Color("6")
	// Imported shares the settled family rather than the warm one. The
	// file is in the library and playable, so it is not a day to act on;
	// what it lacks is a witness, and the glyph carries that.
	colorImported = lipgloss.Color("4")
)

// The greys follow the terminal's scheme because one fixed value has to be
// wrong on either a light or a dark background. Bright black is near #93a1a1
// on Solarized Light's #fdf6e3, under 3:1, and a fixed 256-colour grey
// ignores the scheme altogether.
var (
	quietLight   = lipgloss.Color("240")
	quietDark    = lipgloss.Color("245")
	unknownLight = lipgloss.Color("244")
	unknownDark  = lipgloss.Color("243")
)

// coverageGlyphs give every state a mark that does not depend on colour.
//
// Under NO_COLOR, through a pipe, or on a 1-bit terminal, the writer strips
// every sequence, and colour alone would leave every day on the grid looking
// identical. Colour is the second signal rather than the only one, which also
// answers missed against live: ANSI 1 and ANSI 2 are the pair a deuteranopic
// reader is most likely to read the same.
var coverageGlyphs = map[store.Coverage]string{
	store.CoverageMissed:    "!",
	store.CoveragePartial:   "~",
	store.CoverageAtRisk:    ">",
	store.CoverageLive:      "#",
	store.CoverageRecovered: "+",
	store.CoverageImported:  "=",
	store.CoverageNoStream:  "-",
	store.CoverageUnknown:   ".",
}

// coverageLabels name each state in the legend and detail pane.
//
// Unknown says what it means rather than naming the state, because "no
// recorder history" is the fact and "unknown" is only a label for it.
var coverageLabels = map[store.Coverage]string{
	store.CoverageMissed:    "missed",
	store.CoveragePartial:   "partly captured",
	store.CoverageAtRisk:    "not yet filed",
	store.CoverageLive:      "recorded live",
	store.CoverageRecovered: "recovered from an archive",
	store.CoverageImported:  "imported from its filename",
	store.CoverageNoStream:  "no broadcast",
	store.CoverageUnknown:   "not watched",
}

// legendOrder lists states in the order the legend shows them, worst first,
// so the eye lands on what needs attention.
//
// It is the store's own list rather than a copy. The legend shows every
// state there is, and a hand-kept second list drifts silently: the state it
// forgets is the one that reaches the grid with no key beside it.
var legendOrder = store.Coverages()

// ///////////////////////////////////////////////
// Resolution
// ///////////////////////////////////////////////

// newStyles builds the set for a terminal whose background is dark or light.
//
// Secondary text carries a fact here, a recorder condition or a capture
// count, so it recedes by colour rather than by Faint, which many terminals
// implement as a further opacity cut on top of an already grey foreground.
func newStyles(dark bool) styles {
	lightDark := lipgloss.LightDark(dark)
	quiet := lightDark(quietLight, quietDark)
	unknown := lightDark(unknownLight, unknownDark)

	coverage := map[store.Coverage]lipgloss.Style{
		store.CoverageMissed:    lipgloss.NewStyle().Foreground(colorMissed).Bold(true),
		store.CoveragePartial:   lipgloss.NewStyle().Foreground(colorPartial).Bold(true),
		store.CoverageAtRisk:    lipgloss.NewStyle().Foreground(colorAtRisk).Bold(true),
		store.CoverageLive:      lipgloss.NewStyle().Foreground(colorLive),
		store.CoverageRecovered: lipgloss.NewStyle().Foreground(colorRecovered),
		store.CoverageImported:  lipgloss.NewStyle().Foreground(colorImported),
		store.CoverageNoStream:  lipgloss.NewStyle().Foreground(quiet),
		store.CoverageUnknown:   lipgloss.NewStyle().Foreground(unknown),
	}

	// Only the three worth acting on are washed. Everything settled recedes
	// to a foreground colour, which spends the contrast budget on the days
	// that need attention rather than on the ones that do not.
	//
	// The wash is a reverse rather than a chosen background pair, so its
	// contrast is whatever the terminal already guarantees for that colour
	// against its own background. It also improves the deuteranopia position
	// rather than weakening it: missed against recorded live becomes washed
	// against unwashed, a fill and luminance difference hue blindness does
	// not touch, on top of "!" against "#". Three signals, none of them hue
	// alone.
	cells := make(map[store.Coverage]lipgloss.Style, len(coverage))
	maps.Copy(cells, coverage)
	for _, state := range []store.Coverage{
		store.CoverageMissed, store.CoveragePartial, store.CoverageAtRisk,
	} {
		cells[state] = coverage[state].Reverse(true)
	}

	return styles{
		dark:        dark,
		heading:     lipgloss.NewStyle().Bold(true),
		dim:         lipgloss.NewStyle().Foreground(quiet),
		padding:     lipgloss.NewStyle().Foreground(unknown),
		err:         lipgloss.NewStyle().Foreground(colorMissed),
		ok:          lipgloss.NewStyle().Foreground(colorLive),
		warn:        lipgloss.NewStyle().Foreground(colorPartial),
		caret:       lipgloss.NewStyle().Bold(true),
		tabNumber:   lipgloss.NewStyle().Foreground(unknown),
		tabLabel:    lipgloss.NewStyle().Foreground(quiet),
		tabNumberOn: lipgloss.NewStyle().Reverse(true).Bold(true),
		tabLabelOn:  lipgloss.NewStyle().Reverse(true),
		coverage:    coverage,
		cells:       cells,
	}
}

// dimmed returns the set with every style receded to one grey.
//
// The backdrop behind a modal renders through this rather than by rewriting
// the escape sequences of an already rendered screen. Parsing back out of
// rendered text is where a dimmer starts disagreeing with the renderer about
// what it is looking at.
func (s styles) dimmed() styles {
	flat := styles{
		dark:        s.dark,
		heading:     s.dim,
		dim:         s.dim,
		padding:     s.dim,
		err:         s.dim,
		ok:          s.dim,
		warn:        s.dim,
		caret:       s.dim,
		tabNumber:   s.dim,
		tabLabel:    s.dim,
		tabNumberOn: s.dim,
		tabLabelOn:  s.dim,
		coverage:    make(map[store.Coverage]lipgloss.Style, len(s.coverage)),
		cells:       make(map[store.Coverage]lipgloss.Style, len(s.cells)),
	}
	for state := range s.coverage {
		flat.coverage[state] = s.dim
		flat.cells[state] = s.dim
	}
	return flat
}

// forCell returns the style for a coverage state inside a grid square.
func (s styles) forCell(coverage store.Coverage) lipgloss.Style {
	if style, ok := s.cells[coverage]; ok {
		return style
	}
	return s.dim
}

// forCoverage returns the style for a coverage state, receding for one this
// build has no style for.
func (s styles) forCoverage(coverage store.Coverage) lipgloss.Style {
	if style, ok := s.coverage[coverage]; ok {
		return style
	}
	return s.dim
}

// glyphFor returns the mark for a coverage state.
func glyphFor(coverage store.Coverage) string {
	if glyph, ok := coverageGlyphs[coverage]; ok {
		return glyph
	}
	return glyphUnrecognized
}

// labelFor returns the human label for a coverage state.
func labelFor(coverage store.Coverage) string {
	if label, ok := coverageLabels[coverage]; ok {
		return label
	}
	return string(coverage)
}
