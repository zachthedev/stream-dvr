package tui

import (
	"zach.tools/go/stream-dvr/internal/calendar"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// rect is a region of the screen, in cells, from the upper left.
type rect struct {
	X, Y, W, H int
}

// frame is where every region of the screen goes.
//
// One value computed from the terminal's size, handed to the render
// functions, which each draw inside the rect they are given and cannot
// exceed it. The branching lives here so the drawing is straight-line, and
// this is testable from 40 columns to 400 without a terminal.
type frame struct {
	// Wide reports that the calendar and the month's work stand as two
	// panels side by side. Below the breakpoint they share one panel, and
	// Side is a column inside Calendar rather than a panel of its own.
	Wide bool
	// TooSmall reports that no arrangement fits, so the screen says what it
	// needs instead of drawing a broken one.
	TooSmall bool

	AppBar   rect
	Calendar rect
	Side     rect
	Recorder rect
	Status   rect
	Chips    rect
	// Modal is centred over the panels only, so the app bar, the recorder
	// and the status line stay visible and the recorder keeps reporting
	// while a day is read.
	Modal rect
}

// ///////////////////////////////////////////////
// Measurements
// ///////////////////////////////////////////////

// The grid is six weeks of boxed cells sharing their borders: seven cells of
// five interior columns plus the eight verticals between and around them,
// and seven rows of one interior line plus the eight horizontals. The
// weekday labels are inlaid into the top rule rather than taking a row.
const (
	gridInterior = 5
	gridColumns  = calendar.DaysPerWeek*gridInterior + calendar.DaysPerWeek + 1
	gridRows     = 6*2 + 1
)

// A panel spends one column and one row on each border, and one column of
// padding inside each vertical.
const (
	panelBorder  = 1
	panelPadding = 1
	panelChrome  = 2 * (panelBorder + panelPadding)
	panelRows    = 2 * panelBorder
)

// The calendar panel's body, top to bottom: the grid, a blank, the legend, a
// blank, the degraded banner, a blank.
//
// The banner's row is held whether or not it fires, so the legend does not
// jump down the screen when a read fails partway through a month.
const (
	legendRows   = 8
	calendarBody = gridRows + 1 + legendRows + 1 + 1 + 1
	calendarWide = calendarBody + panelRows
)

// The wide arrangement is two panels and a gutter. The calendar panel is
// fixed: 47 outer is the only width in range where the grid's seven cells
// divide evenly, at five interior columns each. 46 gives 4.857.
//
// The right panel's floor comes from its widest row, a recovery queue entry
// at 65 columns of content. Below 117 the two do not fit and the screen
// falls back to one panel.
const (
	calendarPanelWide = gridColumns + panelChrome
	panelGutter       = 1
	sidePanelFloor    = 65 + panelChrome
	wideBreakpoint    = calendarPanelWide + panelGutter + sidePanelFloor
)

// The recorder strip is a fixed six rows of feed inside its own border. It
// is dropped rather than shrunk when the terminal is too short for it,
// because a strip with one row of feed says less than the row it costs.
const recorderRows = 6 + panelRows

// Below these the arrangement cannot be drawn at all: the grid alone is 43
// columns and 13 rows, and the app bar, status line and chip rows are what
// stop content from pushing the footer off the screen.
const (
	floorWidth  = gridColumns + panelChrome
	floorHeight = 1 + gridRows + panelRows + 1 + 1
)

// The day modal at its full size, and the smallest that still holds a
// recordings table above a fields block.
const (
	modalWide   = 90
	modalTall   = 25
	modalFloor  = 40
	modalShort  = 8
	modalMargin = 3
)

// ///////////////////////////////////////////////
// Layout
// ///////////////////////////////////////////////

// layout places every region for a terminal of this size.
//
// chipRows is how many rows the footer's key chips wrap onto, which depends
// on the width and on which pane is open. It is passed in rather than
// derived here so this stays a function of the geometry alone.
func layout(width, height, chipRows int) frame {
	chipRows = max(chipRows, 1)
	if width < floorWidth || height < floorHeight+chipRows {
		return frame{TooSmall: true}
	}

	f := frame{Wide: width >= wideBreakpoint}
	f.AppBar = rect{X: 0, Y: 0, W: width, H: 1}
	f.Chips = rect{X: 0, Y: height - chipRows, W: width, H: chipRows}
	f.Status = rect{X: 0, Y: f.Chips.Y - 1, W: width, H: 1}

	// Everything between the app bar and the status line, which is what the
	// panels and the recorder strip divide between them.
	const bodyTop = 1
	bodyRows := f.Status.Y - bodyTop

	// The recorder strip is the first thing to go, because the calendar and
	// the month's work are what the screen is for and the recorder's
	// condition also reads from the app bar and the status line.
	panelHeight := bodyRows
	if bodyRows >= calendarWide+recorderRows {
		panelHeight = bodyRows - recorderRows
		f.Recorder = rect{Y: bodyTop + panelHeight, W: width, H: recorderRows}
	}

	if f.Wide {
		f.Calendar = rect{Y: bodyTop, W: calendarPanelWide, H: panelHeight}
		f.Side = rect{
			X: calendarPanelWide + panelGutter,
			Y: bodyTop,
			W: width - calendarPanelWide - panelGutter,
			H: panelHeight,
		}
	} else {
		// One panel. Side is a column inside it, to the right of the grid,
		// holding the legend and the month's totals; the recovery queue
		// runs full width under both.
		f.Calendar = rect{Y: bodyTop, W: width, H: panelHeight}
		f.Side = rect{
			X: panelBorder + panelPadding + gridColumns + panelGutter,
			Y: bodyTop + panelBorder,
			W: width - panelChrome - gridColumns - panelGutter,
			H: gridRows,
		}
	}

	f.Modal = centred(rect{Y: bodyTop, W: width, H: panelHeight})
	return f
}

// centred places the day modal over a region.
//
// Over the panels rather than the whole screen, so the app bar, the recorder
// strip and the status row stay visible and the recorder keeps reporting
// while a day is read.
func centred(over rect) rect {
	width := max(min(modalWide, over.W-2*modalMargin), modalFloor)
	height := max(min(modalTall, over.H-2), modalShort)

	return rect{
		X: over.X + max((over.W-width)/2, 0),
		Y: over.Y + max((over.H-height)/2, 0),
		W: min(width, over.W),
		H: min(height, over.H),
	}
}

// inner returns the region inside a panel's border and padding.
func (r rect) inner() rect {
	return rect{
		X: r.X + panelBorder + panelPadding,
		Y: r.Y + panelBorder,
		W: max(r.W-panelChrome, 0),
		H: max(r.H-panelRows, 0),
	}
}

// empty reports whether a region has no room to draw in.
func (r rect) empty() bool {
	return r.W <= 0 || r.H <= 0
}
