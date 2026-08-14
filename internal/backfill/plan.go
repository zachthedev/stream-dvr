package backfill

import (
	"errors"
	"fmt"
	"time"

	"zach.tools/go/stream-dvr/internal/calendar"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Coverage answers what a channel has and has not captured.
type Coverage interface {
	CoverageBetween(channelID int64, from, to time.Time, loc *time.Location) ([]store.Day, error)
	BroadcastsBetween(channelID int64, from, to time.Time) ([]store.Broadcast, error)
	RecordingsForBroadcast(broadcastID int64) ([]store.Recording, error)
	FetchFor(broadcastID int64) (store.Fetch, error)
}

// Candidate is one broadcast worth fetching.
type Candidate struct {
	// Broadcast is what to fetch.
	Broadcast store.Broadcast
	// Day is the calendar day it sits on, in the operator's location.
	Day time.Time
}

// Window bounds which broadcasts a pass will consider.
type Window struct {
	// Lookback is how far back to search, from now, exactly. A broadcast
	// that started before it is out of range even when the day it sits on
	// is searched, so a range means the hours it says. A first run against
	// a channel with years of history would otherwise try all of it.
	Lookback time.Duration
	// Settle is how long to leave a broadcast alone after it ended. A
	// recording is not published the instant a stream stops, and a
	// broadcast that is still running has no past copy at all.
	Settle time.Duration
	// Location buckets days the way the calendar does, so a broadcast
	// starting at 23:30 belongs to the evening the operator watched.
	Location *time.Location
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// Default bounds for a pass.
const (
	// DefaultLookback is how far back a first pass reaches.
	DefaultLookback = 30 * 24 * time.Hour
	// DefaultSettle is how long after a broadcast ends before it is worth
	// looking for a copy.
	DefaultSettle = 2 * time.Hour
)

// assumedMaxBroadcast is how long a broadcast whose end nobody recorded is
// assumed to run.
//
// It bounds two mistakes against each other. Treating a missing end as
// "finished when it started" declares a six-hour broadcast over at hour two
// and fetches a copy the platform has not published. Treating it as "never
// finished" leaves every row a tracker wrote unreachable for good. A day is
// longer than any broadcast a recorder is likely to meet.
const assumedMaxBroadcast = 24 * time.Hour

// ///////////////////////////////////////////////
// Selection
// ///////////////////////////////////////////////

// Candidates returns the broadcasts of one channel worth fetching.
//
// The day filter comes from the calendar rather than from a rule restated
// here. Grid.Gaps returns missed and partial days and deliberately excludes
// at-risk ones, because an at-risk day's bytes are already on disk and
// refetching would trade a real capture for a muted archive copy. That
// decision belongs in one place, and this is not it.
//
// Inside a gap day each broadcast is judged by store.NeedsRecovery, which
// is the same rule at broadcast scale.
func Candidates(coverage Coverage, channelID int64, now time.Time, window Window) ([]Candidate, error) {
	window = window.withDefaults()

	from := now.Add(-window.Lookback)
	// Read to tomorrow, because coverage buckets whole days and its end is
	// exclusive. A range ending now stops at midnight, so today never gets a
	// cell and a broadcast missed this morning is invisible until tomorrow.
	days, err := coverage.CoverageBetween(channelID, from, now.AddDate(0, 0, 1), window.Location)
	if err != nil {
		return nil, fmt.Errorf("reading coverage for channel %d: %w", channelID, err)
	}

	var candidates []Candidate
	for _, cell := range gapDays(days, from, now, window.Location) {
		found, err := candidatesOn(coverage, channelID, cell, now, window)
		if err != nil {
			return nil, err
		}
		for _, candidate := range found {
			// The search walks whole days, because a day is the unit the
			// calendar calls covered or not. The lookback is not a day, so
			// the oldest day it lands in reaches back further than it does,
			// and every broadcast earlier that day would be fetched by a
			// range that never named it.
			if candidate.Broadcast.StartedAt.Before(from) {
				continue
			}
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

// gapDays returns the days the calendar calls incompletely covered.
//
// The range is walked month by month, because the grid is built per month
// and a lookback of thirty days routinely spans two.
func gapDays(days []store.Day, from, to time.Time, loc *time.Location) []calendar.Cell {
	var gaps []calendar.Cell

	seen := make(map[time.Time]bool)
	// Each step is re-derived rather than accumulated. Where a month's
	// first instant is not midnight, adding a month carries that offset
	// into the next one and the drift compounds until a month falls off
	// the end of the range unbuilt.
	for month := monthOf(from, loc); !month.After(to); month = monthOf(month.AddDate(0, 1, 0), loc) {
		grid := calendar.Build(month.Year(), month.Month(), time.Sunday, days, loc)
		// A cell is a whole day at midnight while the lookback lands at
		// whatever time the pass runs, so the two are compared as days.
		// Comparing a cell against the raw instant drops the oldest day the
		// operator configured, on every pass.
		oldest := startOfDay(from, loc)
		for _, cell := range grid.Gaps() {
			// A grid carries the days either side of its month to fill the
			// first and last weeks, so two months overlap.
			if seen[cell.Date] || cell.Date.Before(oldest) || cell.Date.After(to) {
				continue
			}
			seen[cell.Date] = true
			gaps = append(gaps, cell)
		}
	}
	return gaps
}

// candidatesOn returns the broadcasts worth fetching on one day.
func candidatesOn(coverage Coverage, channelID int64, cell calendar.Cell,
	now time.Time, window Window,
) ([]Candidate, error) {
	dayStart := cell.Date
	broadcasts, err := coverage.BroadcastsBetween(channelID, dayStart, dayStart.AddDate(0, 0, 1))
	if err != nil {
		return nil, fmt.Errorf("reading broadcasts on %s: %w", dayStart.Format(time.DateOnly), err)
	}

	var candidates []Candidate
	for _, broadcast := range broadcasts {
		if !settled(broadcast, now, window.Settle) {
			continue
		}

		recordings, err := coverage.RecordingsForBroadcast(broadcast.ID)
		if err != nil {
			return nil, fmt.Errorf("reading recordings of broadcast %d: %w", broadcast.ID, err)
		}
		if !store.NeedsRecovery(recordings) {
			continue
		}
		if !due(coverage, broadcast.ID, now) {
			continue
		}
		candidates = append(candidates, Candidate{Broadcast: broadcast, Day: dayStart})
	}
	return candidates, nil
}

// settled reports whether enough time has passed since a broadcast to look
// for a copy of it.
//
// It is the one rule both loops read: what a fetch may claim, and what a
// patch may open. A broadcast with no recorded end is measured from its
// start plus assumedMaxBroadcast, so a row nobody ever closed still becomes
// reachable while a broadcast that is plainly still running does not read as
// finished.
func settled(broadcast store.Broadcast, now time.Time, settle time.Duration) bool {
	ended := broadcast.StartedAt.Add(assumedMaxBroadcast)
	if broadcast.EndedAt != nil {
		ended = *broadcast.EndedAt
	}
	return !now.Before(ended.Add(settle))
}

// due reports whether a broadcast is worth attempting a claim for.
//
// It reads the same terms the claim enforces under the write, which is where
// the answer is decided. This is what keeps a pass from spending a round
// trip on every broadcast the claim would refuse.
//
// A broadcast nobody has tried has no fetch row, which is not an error and
// means it is due. Anything unreadable is treated as not due, because
// guessing would offer a broadcast the store may hold terminal.
func due(coverage Coverage, broadcastID int64, now time.Time) bool {
	fetch, err := coverage.FetchFor(broadcastID)
	if err != nil {
		return errorIsNotFound(err)
	}
	if fetch.State == store.FetchTerminal || fetch.State == store.FetchDone {
		return false
	}
	return fetch.NextAttemptAt.IsZero() || !now.Before(fetch.NextAttemptAt)
}

// withDefaults fills a zero window, so a caller that supplied nothing gets
// a bounded pass rather than an unbounded one.
func (w Window) withDefaults() Window {
	if w.Lookback <= 0 {
		w.Lookback = DefaultLookback
	}
	if w.Settle <= 0 {
		w.Settle = DefaultSettle
	}
	if w.Location == nil {
		w.Location = time.UTC
	}
	return w
}

// monthOf returns the first day of a time's month.
//
// Resolved through the store rather than with a bare time.Date, because a
// zone whose clocks move at midnight has no such instant on the days it
// happens and time.Date answers with one belonging to the day before. The
// store owns that correction and the calendar already reads days through
// it, so a second rule here would put the two out of step and walk a month
// this one never builds.
func monthOf(at time.Time, loc *time.Location) time.Time {
	local := at.In(loc)
	return store.StartOfDayOn(local.Year(), local.Month(), 1, loc)
}

// startOfDay returns the first instant of a time's day, which is how a
// calendar cell is dated.
func startOfDay(at time.Time, loc *time.Location) time.Time {
	local := at.In(loc)
	return store.StartOfDayOn(local.Year(), local.Month(), local.Day(), loc)
}

// errorIsNotFound reports a broadcast with no fetch row yet.
func errorIsNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
