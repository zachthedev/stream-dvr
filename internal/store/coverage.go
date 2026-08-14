package store

import (
	"fmt"
	"iter"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Coverage is what the calendar paints for one day.
type Coverage string

// Day is one day's coverage for one channel.
type Day struct {
	// Date is midnight of the day, in the location the query used.
	Date time.Time
	// State is the day's overall coverage.
	State Coverage
	// Broadcasts is how many broadcasts started that day.
	Broadcasts int
	// Captured is how many of them have at least one recording.
	Captured int
	// Bytes is the disk held by that day's recordings.
	Bytes int64
	// Watched reports that a recorder session covered part of the day, so
	// an absence of broadcasts is evidence rather than ignorance.
	Watched bool
	// Degraded reports that a row in the range could not be read, so this
	// day's tally may be short. An unreadable row cannot be attributed to a
	// day, so every day in the range carries the flag.
	Degraded bool
}

// evidence is what one recording's state proves about the day it started on.
type evidence int

// provenance is the weakest thing known about how a day's recordings were
// made.
//
// The fields accumulate rather than replace, because a day is only as
// trustworthy as its least trustworthy recording. One imported file among
// ten live captures still means the day rests partly on a reading of a
// filename.
type provenance struct {
	// atRisk reports a capture that has not reached the library.
	atRisk bool
	// imported reports a recording whose metadata was read back from its
	// own name rather than recorded when it was made.
	imported bool
	// recovered reports a copy downloaded from an archive, whose audio the
	// platform may have muted.
	recovered bool
}

// dayTally is what one day's broadcasts amount to, before that becomes a
// Coverage.
type dayTally struct {
	broadcasts int
	captured   int
	origins    provenance
	// holed reports a captured broadcast still missing a stretch of itself,
	// which counting broadcasts against captures cannot see.
	holed bool
}

// dayLayout renders the calendar day a coverage map is keyed by.
//
// Both the day walk and the bucketing of recordings key off this, and they
// have to agree exactly: a walk producing keys the buckets do not use would
// report every day as quiet while holding a full set of recordings.
const dayLayout = "2006-01-02"

// maxCoverageDays bounds one query's result. A decade of daily cells is
// already far more than a calendar renders, and the storable range allows
// roughly 214,000 of them, each one an allocation.
const maxCoverageDays = 3660

// minWatched is how long a recorder has to have been up before its session
// is evidence that a day was watched.
//
// A session row is written the moment a recorder starts, stamping its
// heartbeat at the start time, so an instant of uptime otherwise claims a
// whole day. One heartbeat interval is the shortest span that proves the
// recorder was alive rather than merely launched.
const minWatched = time.Minute

// Coverage values.
const (
	// CoverageUnknown means nothing was watching that day and no archive
	// has been consulted, so whether a broadcast happened is genuinely
	// unknown.
	//
	// This is distinct from CoverageNoStream and the difference is the
	// point: reporting a day the recorder was switched off as "no stream"
	// reads as reassurance, when it is exactly the kind of day a
	// broadcast could have been missed on.
	CoverageUnknown Coverage = "unknown"
	// CoverageNoStream means the recorder was running and no broadcast
	// happened.
	CoverageNoStream Coverage = "no_stream"
	// CoverageLive means every broadcast that day was captured live.
	CoverageLive Coverage = "live"
	// CoverageRecovered means every broadcast was captured, but at least
	// one came from an archive rather than live. Recovered audio can be
	// muted where live audio never is.
	CoverageRecovered Coverage = "recovered"
	// CoverageImported means every broadcast was captured, but at least one
	// recording is a file that was found in the library rather than made by
	// the recorder.
	//
	// It ranks below recovered because less is known, not more. An imported
	// recording's date and title were read back from its own filename, so
	// the claim that it holds this day's broadcast is a reading rather than
	// an observation. Painting such a day live would promise the recorder
	// watched a broadcast it never saw.
	CoverageImported Coverage = "imported"
	// CoveragePartial means at least one broadcast was captured and at
	// least one was not.
	CoveragePartial Coverage = "partial"
	// CoverageAtRisk means every broadcast was captured but at least one
	// capture is still outside the library: mid-recording, or held in one
	// of the awaiting states.
	//
	// The file exists and is playable, so this is not a gap. It is also not
	// finished, and painting it the same as a verified day would hide a
	// recording that has been stuck for a week behind a reassuring colour.
	CoverageAtRisk Coverage = "at_risk"
	// CoverageMissed means a broadcast happened and nothing captured it.
	// This is the state the calendar exists to make obvious.
	CoverageMissed Coverage = "missed"
)

// Evidence values, in ascending order of what they prove. Coverage keeps the
// strongest evidence any one recording offers, so the order matters.
const (
	// evidenceNone is a recording that proves nothing about the day:
	// capture gave up, or the operator purged it.
	evidenceNone evidence = iota
	// evidenceAtRisk is a file that exists but has not reached the library.
	evidenceAtRisk
	// evidenceCaptured is a recording that is named, verified, and final.
	evidenceCaptured
)

// Coverages lists every state a day can be in, worst first.
//
// It is the one definition of what the set contains. Anything that paints,
// labels, or counts states reads it, so a state added above reaches all of
// them instead of only the ones somebody remembered. A second list kept by
// hand agrees right up until it does not, and the day it stops agreeing is
// the day a new state renders as a blank cell.
func Coverages() []Coverage {
	return []Coverage{
		CoverageMissed,
		CoveragePartial,
		CoverageAtRisk,
		CoverageImported,
		CoverageRecovered,
		CoverageLive,
		CoverageNoStream,
		CoverageUnknown,
	}
}

// String returns the coverage state's stored form.
func (c Coverage) String() string { return string(c) }

// stateEvidence classifies what a recording's state proves.
//
// The default is deliberately evidenceNone. An unrecognised state, which is
// what a database written by a newer build looks like, must never be read as
// proof that a day was captured.
func stateEvidence(state State) evidence {
	switch state {
	case StateComplete:
		return evidenceCaptured
	case StateCapturing, StateAwaitingFinalize, StateAwaitingMetadata, StateAwaitingFile:
		return evidenceAtRisk
	default:
		return evidenceNone
	}
}

// NeedsRecovery reports whether a broadcast's recordings leave anything to
// fetch.
//
// It is the per-broadcast form of the rule the calendar applies per day,
// built on the same stateEvidence so the two cannot drift. Grid.Gaps names
// the candidate days, and this names the candidate broadcasts inside one.
//
// A recording that is capturing right now, or on its way into the library,
// counts as evidence and stops recovery. That is one of the guards keeping
// backfill and live capture off one broadcast. Refetching what is being
// captured would replace a live copy with an archive copy that platforms
// mute after the fact.
//
// A broadcast whose every recording failed or was trashed is a candidate,
// and so is one with no recordings at all. Neither holds evidence that the
// broadcast was kept.
func NeedsRecovery(recordings []Recording) bool {
	for _, recording := range recordings {
		if stateEvidence(recording.State) != evidenceNone {
			return false
		}
	}
	return true
}

// ///////////////////////////////////////////////
// Coverage
// ///////////////////////////////////////////////

// CoverageBetween reports per-day coverage for a channel over a date range.
//
// Days are bucketed in loc, not UTC, because a calendar is read in local
// time and a broadcast starting at 23:30 belongs to the evening the viewer
// remembers. The range is inclusive of from's day and exclusive of to's.
//
// Days with no known broadcast are returned as CoverageNoStream rather than
// omitted, so the caller renders a complete grid without filling holes.
func (s *Store) CoverageBetween(channelID int64, from, to time.Time, loc *time.Location) ([]Day, error) {
	if loc == nil {
		loc = time.UTC
	}
	start := startOfDay(from, loc)
	end := startOfDay(to, loc)
	if !end.After(start) {
		return nil, fmt.Errorf("coverage range ends %s at or before it starts %s", end, start)
	}

	// Refused before anything is queried. The cap is checked again while
	// the days are built, which is what bounds the result, but reaching it
	// there means four queries and a session walk have already run and
	// their rows are already in memory. A subtraction answers the same
	// question first.
	if days := end.Sub(start) / (24 * time.Hour); days > maxCoverageDays {
		return nil, fmt.Errorf("coverage range covers more than %d days", maxCoverageDays)
	}

	// The day holding the earliest storable instant begins before it, and the
	// day holding the latest ends after it, so a query taking the raw day
	// bound would fail on the first and last day the calendar can show.
	queryStart, queryEnd := clampStorable(start), clampStorable(end)

	broadcasts, lostBroadcasts, err := s.broadcastsBetween(channelID, queryStart, queryEnd)
	if err != nil {
		return nil, err
	}
	recordings, lostRecordings, err := s.recordingsForChannel(channelID, queryStart, queryEnd)
	if err != nil {
		return nil, err
	}
	sessions, lostSessions, err := s.sessionsBetween(queryStart, queryEnd)
	if err != nil {
		return nil, err
	}
	holedRecordings, lostGaps, err := s.openGapRecordings(channelID, queryStart, queryEnd)
	if err != nil {
		return nil, err
	}
	degraded := lostBroadcasts+lostRecordings+lostSessions+lostGaps > 0
	watched := watchedDays(sessions, start, end, loc)

	// A recording counts toward the broadcast it belongs to, and is also
	// evidence about the day it started on. Those are not always the same
	// day: a capture running past midnight starts on the next one, and a
	// live upgrade can move a broadcast's start across a day boundary while
	// its recording stays where it was.
	capturedByBroadcast := make(map[int64][]Recording, len(recordings))
	recordingsByDay := make(map[string][]Recording)
	bytesByDay := make(map[string]int64)

	for _, recording := range recordings {
		key := dayKey(recording.StartedAt, loc)
		bytesByDay[key] += recording.Bytes
		recordingsByDay[key] = append(recordingsByDay[key], recording)
		if recording.BroadcastID != nil {
			capturedByBroadcast[*recording.BroadcastID] = append(capturedByBroadcast[*recording.BroadcastID], recording)
		}
	}

	tallies := tallyBroadcasts(broadcasts, capturedByBroadcast, holedRecordings, loc)

	var result []Day
	for key, cursor := range walkDays(start, loc) {
		if !cursor.Before(end) {
			break
		}
		if len(result) == maxCoverageDays {
			return nil, fmt.Errorf("coverage range covers more than %d days", maxCoverageDays)
		}

		// A quiet day only means "no broadcast" if something was watching.
		// Otherwise it means nobody looked, and saying so is the whole
		// reason the calendar exists. A skipped row could belong to any day
		// in the range, so while one is outstanding no day is quiet.
		quiet := CoverageUnknown
		if watched[key] && !degraded {
			quiet = CoverageNoStream
		}
		day := Day{
			Date:     cursor,
			State:    quiet,
			Bytes:    bytesByDay[key],
			Watched:  watched[key],
			Degraded: degraded,
		}

		if entry := tallies[key]; entry != nil {
			day.Broadcasts = entry.broadcasts
			day.Captured = entry.captured
			day.State = captureState(entry.broadcasts, entry.captured,
				entry.origins, entry.holed, degraded)
		} else if state, counted := recordedState(recordingsByDay[key], day.Bytes); state != CoverageUnknown {
			// No broadcast started this day, so its own recordings are the
			// only witness it has.
			day.State = state
			day.Captured = counted
		}
		result = append(result, day)
	}
	return result, nil
}

// clampStorable pulls a query bound inside the range Unix nanoseconds can
// name, so a calendar day extending past either end still queries.
func clampStorable(t time.Time) time.Time {
	switch {
	case t.Before(minStorable):
		return minStorable
	case t.After(maxStorable):
		return maxStorable
	default:
		return t
	}
}

// tallyBroadcasts groups broadcasts by the day they started on in loc,
// recording what their captures prove about it.
func tallyBroadcasts(broadcasts []Broadcast, captures map[int64][]Recording,
	holedRecordings map[int64]bool, loc *time.Location,
) map[string]*dayTally {
	tallies := make(map[string]*dayTally)

	for _, broadcast := range broadcasts {
		key := dayKey(broadcast.StartedAt, loc)
		if tallies[key] == nil {
			tallies[key] = &dayTally{}
		}
		entry := tallies[key]
		entry.broadcasts++

		// A broadcast counts as captured on the strongest evidence any one
		// of its recordings offers. A failed capture offers none, so a
		// broadcast backed only by failures is still a missed one.
		best := evidenceNone
		for _, capture := range captures[broadcast.ID] {
			// A hole is a statement about the broadcast: a stretch of it
			// reached nobody. Which recording happens to carry the gap,
			// and what state that row is in, says nothing about whether
			// the footage is missing. Read before the evidence test, so
			// trashing the first half of a reconnected capture cannot
			// retire the hole between the halves.
			if holedRecordings[capture.ID] {
				entry.holed = true
			}

			weight := stateEvidence(capture.State)
			if weight == evidenceNone {
				continue
			}
			if weight > best {
				best = weight
			}
			entry.origins.note(capture.Origin)
		}
		if best == evidenceNone {
			continue
		}
		entry.captured++
		if best == evidenceAtRisk {
			entry.origins.atRisk = true
		}
	}
	return tallies
}

// captureState reports what a day's broadcast tally amounts to.
//
// The order is worst first. A day still missing a broadcast is reported as
// partial even when another capture is stranded, because the hole is the
// more urgent fact. At risk outranks recovered for the same reason: a
// stranded file is something to act on, and where the bytes came from is
// not.
//
// A hole inside a captured broadcast is the same statement as a broadcast
// with nothing at all, made at a finer grain, so it lands at the same
// partial rather than below the states that describe whole coverage. A day
// counting one broadcast against one capture is otherwise fully covered by
// arithmetic while an hour of it was never received.
// A degraded range lands at partial for the same reason. Live states that
// every broadcast of the day was caught, and a range that could not read
// all of its rows cannot say that: the row it skipped could name a
// broadcast on any day it covers.
func captureState(broadcasts, captured int, origins provenance, holed, degraded bool) Coverage {
	switch {
	case captured == 0:
		return CoverageMissed
	case captured < broadcasts:
		return CoveragePartial
	case holed:
		return CoveragePartial
	case degraded:
		return CoveragePartial
	default:
		return origins.state()
	}
}

// note folds one recording's origin into what is known about the day.
//
// It is the one place an origin becomes evidence, so a value added to Origin
// is answered here or nowhere. Live sets nothing, because live is what the
// absence of every weaker signal already means.
func (p *provenance) note(origin Origin) {
	switch origin {
	case OriginImported:
		p.imported = true
	case OriginRecovered:
		p.recovered = true
	case OriginLive:
	default:
		// A row written by a newer build, or edited by hand, carries an
		// origin this one cannot weigh. It lands on the weakest rung rather
		// than the strongest: state() returns live when nothing is set, and
		// live is the one answer that claims the recorder saw the broadcast
		// happen.
		p.imported = true
	}
}

// state ranks a fully covered day by the weakest thing known about it.
//
// One ladder, read by both the broadcast tally and the day that has only its
// own recordings to go on. Two copies of this order would let the same
// library answer differently depending on whether a broadcast row happened
// to exist.
//
// Worst first, and each rung is something the operator can act on. A
// stranded file is the most urgent, and where bytes came from is not. An
// imported recording sits below a recovered one because its metadata is a
// reading of a filename: the day may hold the broadcast the name claims, and
// nothing here witnessed that. Live is the default because it is the only
// rung that claims the recorder saw the broadcast happen, and it must be
// reachable only when nothing weaker is known.
func (p provenance) state() Coverage {
	switch {
	case p.atRisk:
		return CoverageAtRisk
	case p.imported:
		return CoverageImported
	case p.recovered:
		return CoverageRecovered
	default:
		return CoverageLive
	}
}

// watchedDays returns the days a recorder session covered, keyed the same
// way coverage buckets recordings.
//
// A session spans its start to its last heartbeat rather than to its stop
// time, because a crashed session has no stop time and its heartbeat is the
// last moment it was known alive.
func watchedDays(sessions []Session, start, end time.Time, loc *time.Location) map[string]bool {
	watched := make(map[string]bool)

	for _, session := range sessions {
		from := session.StartedAt
		if from.Before(start) {
			from = start
		}
		until := session.HeartbeatAt
		if until.After(end) {
			until = end
		}
		if until.Sub(from) < minWatched {
			// A session that barely existed did not watch anything. Its
			// row is written the moment a recorder starts, with the
			// heartbeat stamped at the start time, so a daemon that dies
			// on startup every morning would otherwise report each of
			// those days as one where nothing aired. Unknown is the
			// honest answer, and it is the state this whole distinction
			// exists to keep reachable.
			continue
		}

		for key, cursor := range walkDays(from, loc) {
			if cursor.After(until) {
				break
			}
			watched[key] = true
		}
	}
	return watched
}

// recordedState classifies a day from the recordings that started on it, and
// reports how many count as evidence. CoverageUnknown means they prove
// nothing and the day keeps whatever the session history already said.
//
// This runs only for a day no broadcast started on, where the recordings are
// the whole record: a capture that ran past midnight, one whose broadcast
// was never discovered, or one whose broadcast moved to another day when a
// more precise source upgraded its start time.
//
// Bytes count as evidence on their own. A day whose recordings all failed
// counts none of them, and the bytes they wrote still prove a broadcast
// happened, so the day is a miss rather than a quiet one. Reporting a day
// holding gigabytes as no_stream or unknown is the most reassuring lie the
// calendar can tell.
func recordedState(recordings []Recording, bytes int64) (Coverage, int) {
	var (
		counted int
		origins provenance
	)
	for _, recording := range recordings {
		weight := stateEvidence(recording.State)
		if weight == evidenceNone {
			continue
		}
		counted++
		if weight == evidenceAtRisk {
			origins.atRisk = true
		}
		origins.note(recording.Origin)
	}

	switch {
	case counted == 0 && bytes > 0:
		return CoverageMissed, 0
	case counted == 0:
		return CoverageUnknown, 0
	default:
		return origins.state(), counted
	}
}

// walkDays yields one key and midnight per calendar day, starting with the
// day containing from, in loc. It never ends, so callers stop at their own
// bound.
//
// The walk advances a UTC anchor and rebuilds each day in loc. Advancing in
// loc carries a normalised hour forward: where the clocks go forward at
// midnight that midnight does not exist, time.Date lands on 23:00 the day
// before, and every later step inherits the offset. The visible result is a
// seven day range returning eight rows, with one date repeated and a
// recording counted on both.
func walkDays(from time.Time, loc *time.Location) iter.Seq2[string, time.Time] {
	return func(yield func(string, time.Time) bool) {
		first := from.In(loc)
		anchor := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.UTC)

		for {
			year, month, day := anchor.Date()
			if !yield(anchor.Format(dayLayout), startOfDayOn(year, month, day, loc)) {
				return
			}
			anchor = anchor.AddDate(0, 0, 1)
		}
	}
}

// startOfDay truncates a time to the first instant of its day in loc.
//
// time.Truncate cannot do this: it works on absolute duration since the
// zero time and ignores the location, so it lands on the wrong hour for any
// zone that is not UTC.
func startOfDay(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	return startOfDayOn(local.Year(), local.Month(), local.Day(), loc)
}

// StartOfDayOn returns the first instant of a calendar day in loc.
//
// It is exported because a caller walking days has to build each one this
// way. A zone whose clocks move at midnight has a day that does not begin at
// 00:00, so stepping through local time repeats one date and shifts every
// later one. Two callers walking days differently is how a grid and the
// query behind it come to disagree about which day they mean.
func StartOfDayOn(year int, month time.Month, day int, loc *time.Location) time.Time {
	return startOfDayOn(year, month, day, loc)
}

// startOfDayOn returns the first instant of a calendar day in loc.
//
// Where the clocks go forward at midnight, that midnight never happens and
// time.Date answers with an instant belonging to the previous day. The day
// really starts where the clocks landed, so the size of the jump is added
// back. Returning the earlier instant would stamp the day with yesterday's
// date and put its cell on the wrong square of the calendar.
func startOfDayOn(year int, month time.Month, day int, loc *time.Location) time.Time {
	start := time.Date(year, month, day, 0, 0, 0, 0, loc)
	if start.Day() == day {
		return start
	}

	// A day later is past the transition in either direction, and no zone
	// changes twice within one, so this is the offset the day begins under.
	_, before := start.Zone()
	_, after := start.Add(24 * time.Hour).Zone()
	return start.Add(time.Duration(after-before) * time.Second)
}

// dayKey renders the calendar day a timestamp falls on in loc.
func dayKey(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(dayLayout)
}
