// Package calendar lays a channel's coverage out as a month grid.
//
// It holds no rendering and no framework types, only the arithmetic of
// which day lands in which cell. That separation keeps the part most likely
// to be wrong, the padding and the week alignment around month boundaries,
// testable without a terminal.
package calendar

import (
	"time"

	"zach.tools/go/stream-dvr/internal/store"
)

// Cell is one square in the grid.
type Cell struct {
	// Date is the day this cell represents, at midnight in the grid's
	// location.
	Date time.Time
	// InMonth reports whether the day belongs to the month on display.
	// Padding cells from the adjacent months are still real days, so they
	// carry their own coverage rather than being blanked.
	InMonth bool
	// Coverage is the day's state.
	Coverage store.Coverage
	// Broadcasts is how many broadcasts started that day.
	Broadcasts int
	// Captured is how many of them were recorded.
	Captured int
	// Bytes is the disk that day's recordings hold.
	Bytes int64
	// Watched reports that a recorder session covered part of the day, so
	// an absence of broadcasts is evidence rather than ignorance. It is the
	// fact that separates a day nothing aired on from a day nothing was
	// listening on, which look identical and are opposite problems.
	Watched bool
	// Degraded reports that a row in the range could not be read, so this
	// day's tally may be short. An unreadable row cannot be attributed to a
	// day, so every day in the range carries the flag.
	Degraded bool
}

// Grid is a month laid out as rows of seven days.
type Grid struct {
	// Year and Month name the month on display.
	Year  int
	Month time.Month
	// WeekStart is the weekday each row begins on.
	WeekStart time.Weekday
	// Weeks are the rows, each exactly seven cells wide.
	Weeks [][]Cell
}

// Summary counts a grid's days by state.
type Summary struct {
	// Missed is the count that matters most: a broadcast happened and
	// nothing captured it.
	Missed int
	// Partial is days where some broadcasts were captured and some were
	// not.
	Partial int
	// AtRisk is days fully captured whose files have not reached the
	// library yet.
	AtRisk int
	// Live, Recovered and Imported are fully covered days, split by how.
	// Imported rests on a filename rather than on anything the recorder
	// saw, so it is counted apart from the two the recorder made.
	Live      int
	Recovered int
	Imported  int
	// Unknown is days nothing was watching, which are not the same as
	// days with no broadcast.
	Unknown int
	// NoStream is days the recorder was running and nothing aired.
	NoStream int
	// Bytes is the disk held by the month's recordings.
	Bytes int64
	// Watched is in-month days a recorder covered part of.
	Watched int
	// Degraded reports that some row in the month could not be read, so
	// every count here may be short.
	Degraded bool
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// DaysPerWeek is the width of every grid row.
//
// Exported because a renderer sizes its columns from it, and two constants
// that must agree is one more than there should be.
const DaysPerWeek = 7

// ///////////////////////////////////////////////
// Construction
// ///////////////////////////////////////////////

// Build lays days out as a month grid.
//
// Days outside the requested month still appear, as padding, because a week
// row has to be seven wide. They keep whatever coverage they were given, so
// a missed broadcast on the 1st is visible from the previous month's view
// rather than hidden until the page turns.
//
// A day with no entry in days becomes store.CoverageUnknown, which is the
// honest answer for a range nothing has looked at.
func Build(year int, month time.Month, weekStart time.Weekday, days []store.Day, loc *time.Location) Grid {
	if loc == nil {
		loc = time.UTC
	}

	byDay := make(map[string]store.Day, len(days))
	for _, day := range days {
		byDay[key(day.Date, loc)] = day
	}

	first := time.Date(year, month, 1, 0, 0, 0, 0, loc)
	start := alignToWeek(first, weekStart)
	end := first.AddDate(0, 1, 0)

	grid := Grid{Year: year, Month: month, WeekStart: weekStart}

	// The walk steps a UTC anchor and rebuilds each day in loc. In a zone
	// whose clocks go forward at midnight, adding a day to a local time
	// lands inside the day it left: the date repeats and every later cell
	// carries the previous evening's hour. A grid that paints a day as
	// missed while the fetch queries the wrong 24 hours finds nothing and
	// reports no error. store.CoverageBetween walks the same way, so the
	// cell and the query agree on which day they mean.
	anchor := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	last := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)

	// Rows run until the month is exhausted and the final week completed,
	// so the grid is always whole weeks.
	for anchor.Before(last) || anchor.Weekday() != weekStart {
		week := make([]Cell, 0, DaysPerWeek)
		for range DaysPerWeek {
			cursorYear, cursorMonth, cursorDay := anchor.Date()
			day := store.StartOfDayOn(cursorYear, cursorMonth, cursorDay, loc)
			week = append(week, cell(day, month, byDay, loc))
			anchor = anchor.AddDate(0, 0, 1)
		}
		grid.Weeks = append(grid.Weeks, week)

		if !anchor.Before(last) {
			break
		}
	}
	return grid
}

// cell builds one square.
func cell(date time.Time, month time.Month, byDay map[string]store.Day, loc *time.Location) Cell {
	out := Cell{
		Date:     date,
		InMonth:  date.Month() == month,
		Coverage: store.CoverageUnknown,
	}
	if day, ok := byDay[key(date, loc)]; ok {
		out.Coverage = day.State
		out.Broadcasts = day.Broadcasts
		out.Captured = day.Captured
		out.Bytes = day.Bytes
		out.Watched = day.Watched
		out.Degraded = day.Degraded
	}
	return out
}

// alignToWeek walks back to the most recent weekStart on or before date.
func alignToWeek(date time.Time, weekStart time.Weekday) time.Time {
	// Normalised, because Go's % keeps the dividend's sign: a weekStart
	// outside a weekday's range makes the offset negative, and subtracting
	// a negative walks forward past the first of the month. The grid then
	// starts after the day it is meant to open on and the month loses days
	// off its front with nothing said.
	start := int(weekStart) % DaysPerWeek
	if start < 0 {
		start += DaysPerWeek
	}
	offset := (int(date.Weekday()) - start + DaysPerWeek) % DaysPerWeek
	return date.AddDate(0, 0, -offset)
}

// key renders the map key for a day.
func key(date time.Time, loc *time.Location) string {
	return date.In(loc).Format("2006-01-02")
}

// ///////////////////////////////////////////////
// Queries
// ///////////////////////////////////////////////

// Summarize counts the grid's in-month days by state.
//
// Padding days are excluded so the figures describe the month on display
// rather than the rectangle it happens to be drawn in.
func (g Grid) Summarize() Summary {
	var summary Summary

	for _, week := range g.Weeks {
		for _, cell := range week {
			if !cell.InMonth {
				continue
			}
			summary.Bytes += cell.Bytes
			if cell.Watched {
				summary.Watched++
			}
			if cell.Degraded {
				summary.Degraded = true
			}

			summary.count(cell.Coverage)
		}
	}
	return summary
}

// count adds one day to the tally for its state.
//
// A state this build does not know still happened. Folding it into unknown
// keeps the totals summing to the days on the grid; dropping it silently
// produces a legend of zeros beside a full month and says nothing about why.
func (s *Summary) count(coverage store.Coverage) {
	if into := s.field(coverage); into != nil {
		*into++
		return
	}
	s.Unknown++
}

// Count returns how many days landed in one state.
//
// Reading and writing go through one table, so a caller drawing a figure per
// state cannot be looking at a field Summarize never fills. A second mapping
// written by hand is how a state comes to be painted on the grid and counted
// as zero in the legend beside it.
func (s Summary) Count(coverage store.Coverage) int {
	if from := s.field(coverage); from != nil {
		return *from
	}
	return 0
}

// field points at the tally for one state, or nil for a state this build does
// not know.
func (s *Summary) field(coverage store.Coverage) *int {
	switch coverage {
	case store.CoverageMissed:
		return &s.Missed
	case store.CoveragePartial:
		return &s.Partial
	case store.CoverageAtRisk:
		return &s.AtRisk
	case store.CoverageLive:
		return &s.Live
	case store.CoverageRecovered:
		return &s.Recovered
	case store.CoverageImported:
		return &s.Imported
	case store.CoverageNoStream:
		return &s.NoStream
	case store.CoverageUnknown:
		return &s.Unknown
	}
	return nil
}

// Recoverable reports whether a day is worth fetching from an archive.
//
// A missed day is a broadcast with nothing captured, and a partial day is
// one where something was captured and something was not.
//
// An at-risk day is not recoverable. Its bytes are already on disk, so
// fetching them again would replace a real capture with a muted archive
// copy. What it needs is the organizer unblocked, and the legend is where
// that shows.
//
// It is a method so the work list and the single-day action test one rule.
// A second copy is how a day Gaps skips becomes a day the interface still
// fetches.
func (c Cell) Recoverable() bool {
	return c.Coverage == store.CoverageMissed || c.Coverage == store.CoveragePartial
}

// Gaps returns the in-month days worth acting on, oldest first.
func (g Grid) Gaps() []Cell {
	var gaps []Cell

	for _, week := range g.Weeks {
		for _, cell := range week {
			if cell.InMonth && cell.Recoverable() {
				gaps = append(gaps, cell)
			}
		}
	}
	return gaps
}

// Find returns the cell for a date, and whether the grid holds it.
func (g Grid) Find(date time.Time) (Cell, bool) {
	want := date.Format("2006-01-02")

	for _, week := range g.Weeks {
		for _, cell := range week {
			if cell.Date.Format("2006-01-02") == want {
				return cell, true
			}
		}
	}
	return Cell{}, false
}

// Title renders the month's name for a header.
func (g Grid) Title() string {
	return time.Date(g.Year, g.Month, 1, 0, 0, 0, 0, time.UTC).Format("January 2006")
}

// WeekdayHeadings returns the column labels, starting on the grid's week
// start.
func (g Grid) WeekdayHeadings() []string {
	headings := make([]string, 0, DaysPerWeek)

	for i := range DaysPerWeek {
		day := time.Weekday((int(g.WeekStart) + i) % DaysPerWeek)
		headings = append(headings, day.String()[:2])
	}
	return headings
}
