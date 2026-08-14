package backfill

import (
	"errors"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// fakeCoverage answers the four questions candidate selection asks.
type fakeCoverage struct {
	days       []store.Day
	broadcasts []store.Broadcast
	recordings map[int64][]store.Recording
	fetches    map[int64]store.Fetch
	// askedFrom and askedTo record the window coverage was read over, which
	// is what decides whether today has a cell to be a candidate on.
	askedFrom time.Time
	askedTo   time.Time
}

// failingCoverage fails the first question it is asked.
type failingCoverage struct{ err error }

// planDay is the day every case in this file is built around.
var planDay = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)

// CoverageBetween implements Coverage.
func (f *fakeCoverage) CoverageBetween(_ int64, from, to time.Time, _ *time.Location) ([]store.Day, error) {
	f.askedFrom, f.askedTo = from, to
	return append([]store.Day(nil), f.days...), nil
}

// BroadcastsBetween implements Coverage, returning what falls in the range.
func (f *fakeCoverage) BroadcastsBetween(_ int64, from, to time.Time) ([]store.Broadcast, error) {
	var found []store.Broadcast
	for _, broadcast := range f.broadcasts {
		if !broadcast.StartedAt.Before(from) && broadcast.StartedAt.Before(to) {
			found = append(found, broadcast)
		}
	}
	return found, nil
}

// RecordingsForBroadcast implements Coverage.
func (f *fakeCoverage) RecordingsForBroadcast(broadcastID int64) ([]store.Recording, error) {
	return append([]store.Recording(nil), f.recordings[broadcastID]...), nil
}

// FetchFor implements Coverage.
func (f *fakeCoverage) FetchFor(broadcastID int64) (store.Fetch, error) {
	if fetch, ok := f.fetches[broadcastID]; ok {
		return fetch, nil
	}
	return store.Fetch{}, store.ErrNotFound
}

// oneDay builds a coverage set holding a single day in one state, with one
// broadcast on it whose recordings decide whether it needs recovering.
func oneDay(coverage store.Coverage, recordings []store.Recording) *fakeCoverage {
	started := planDay.Add(20 * time.Hour)
	return &fakeCoverage{
		days:       []store.Day{{Date: planDay, State: coverage, Broadcasts: 1}},
		broadcasts: []store.Broadcast{{ID: 1, ChannelID: 1, StartedAt: started}},
		recordings: map[int64][]store.Recording{1: recordings},
	}
}

// afterSettling is a time far enough past the fixture broadcast that the
// settle window has elapsed.
func afterSettling() time.Time { return planDay.Add(48 * time.Hour) }

// ///////////////////////////////////////////////
// Day-level filtering
// ///////////////////////////////////////////////

func TestCandidates_OneDayPerCoverageState(t *testing.T) {
	// The most important rule in the package. An at-risk day's bytes are on
	// disk already, so refetching would replace a real capture with an
	// archive copy the platform has muted. The calendar decides this and
	// backfill must not second-guess it.
	tests := []struct {
		name     string
		coverage store.Coverage
		want     int
		why      string
	}{
		{
			name:     "missed",
			coverage: store.CoverageMissed,
			want:     1,
			why:      "a broadcast happened and nothing captured it",
		},
		{
			name:     "partial",
			coverage: store.CoveragePartial,
			want:     1,
			why:      "some of the day was captured and some was not",
		},
		{
			name:     "at risk",
			coverage: store.CoverageAtRisk,
			want:     0,
			why:      "the bytes are on disk, and an archive copy would be worse",
		},
		{
			name:     "live",
			coverage: store.CoverageLive,
			want:     0,
			why:      "the recorder captured it as it happened, which is the best copy there is",
		},
		{
			name:     "recovered",
			coverage: store.CoverageRecovered,
			want:     0,
			why:      "already fetched once, and fetching again gets the same file",
		},
		{
			name:     "no stream",
			coverage: store.CoverageNoStream,
			want:     0,
			why:      "no broadcast happened, so no copy exists to find",
		},
		{
			name:     "unknown",
			coverage: store.CoverageUnknown,
			want:     0,
			why:      "a day nothing is known about is not evidence that something is missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coverage := oneDay(tt.coverage, []store.Recording{{State: store.StateFailed}})

			candidates, err := Candidates(coverage, 1, afterSettling(), Window{Location: time.UTC})
			if err != nil {
				t.Fatalf("Candidates() err = %v, want nil", err)
			}
			if len(candidates) != tt.want {
				t.Errorf("Candidates() returned %d, want %d (%s)", len(candidates), tt.want, tt.why)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Broadcast-level filtering
// ///////////////////////////////////////////////

func TestCandidates_SkipsABroadcastAlreadyCaptured(t *testing.T) {
	// A partial day holds both captured and missed broadcasts. Fetching
	// the captured one would replace a live recording.
	coverage := oneDay(store.CoveragePartial, []store.Recording{{State: store.StateComplete}})

	candidates, err := Candidates(coverage, 1, afterSettling(), Window{Location: time.UTC})
	if err != nil {
		t.Fatalf("Candidates() err = %v, want nil", err)
	}
	if len(candidates) != 0 {
		t.Errorf("Candidates() returned %d for a captured broadcast, want 0", len(candidates))
	}
}

func TestCandidates_SkipsABroadcastBeingCaptured(t *testing.T) {
	// The recorder holds it right now, and racing it would trade a live
	// recording for a muted one.
	coverage := oneDay(store.CoverageMissed, []store.Recording{{State: store.StateCapturing}})

	candidates, err := Candidates(coverage, 1, afterSettling(), Window{Location: time.UTC})
	if err != nil {
		t.Fatalf("Candidates() err = %v, want nil", err)
	}
	if len(candidates) != 0 {
		t.Errorf("Candidates() returned %d while capturing, want 0", len(candidates))
	}
}

// ///////////////////////////////////////////////
// Settling
// ///////////////////////////////////////////////

func TestCandidates_WaitsForABroadcastToSettle(t *testing.T) {
	// A copy is not published the instant a stream stops, and a broadcast
	// with no recorded end may still be running. Measuring an unclosed row
	// from its start declares a six-hour broadcast finished at hour two and
	// spends a fetch on a range the platform has not published.
	coverage := oneDay(store.CoverageMissed, []store.Recording{{State: store.StateFailed}})
	started := coverage.broadcasts[0].StartedAt

	tests := []struct {
		name    string
		endedAt *time.Time
		at      time.Duration
		want    int
	}{
		{
			name:    "a recorded end, still settling",
			endedAt: new(started.Add(3 * time.Hour)),
			at:      3*time.Hour + 30*time.Minute,
			want:    0,
		},
		{
			name:    "a recorded end, past the settle window",
			endedAt: new(started.Add(3 * time.Hour)),
			at:      5 * time.Hour,
			want:    1,
		},
		{
			name: "no recorded end, and it could still be running",
			at:   2 * time.Hour,
			want: 0,
		},
		{
			name: "no recorded end, past any plausible broadcast",
			at:   26 * time.Hour,
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coverage.broadcasts[0].EndedAt = tt.endedAt

			candidates, err := Candidates(coverage, 1, started.Add(tt.at), Window{
				Settle: time.Hour, Location: time.UTC,
			})
			if err != nil {
				t.Fatalf("Candidates() err = %v, want nil", err)
			}
			if len(candidates) != tt.want {
				t.Errorf("Candidates() returned %d, want %d", len(candidates), tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Backoff and terminal state
// ///////////////////////////////////////////////

func TestCandidates_HonoursTheStoredBackoff(t *testing.T) {
	// The retry schedule lives in the store precisely so it survives a
	// daemon restart. Selecting a broadcast before its time would undo it.
	coverage := oneDay(store.CoverageMissed, []store.Recording{{State: store.StateFailed}})
	now := afterSettling()
	coverage.fetches = map[int64]store.Fetch{
		1: {BroadcastID: 1, State: store.FetchPending, NextAttemptAt: now.Add(time.Hour)},
	}

	candidates, err := Candidates(coverage, 1, now, Window{Location: time.UTC})
	if err != nil {
		t.Fatalf("Candidates() err = %v, want nil", err)
	}
	if len(candidates) != 0 {
		t.Errorf("Candidates() returned %d before the backoff elapsed, want 0", len(candidates))
	}
}

func TestCandidates_NeverSelectsATerminalBroadcast(t *testing.T) {
	// A private or removed video answers the same way forever, so a pass
	// that kept selecting it would spend a request every time.
	coverage := oneDay(store.CoverageMissed, []store.Recording{{State: store.StateFailed}})
	coverage.fetches = map[int64]store.Fetch{1: {BroadcastID: 1, State: store.FetchTerminal}}

	candidates, err := Candidates(coverage, 1, afterSettling(), Window{Location: time.UTC})
	if err != nil {
		t.Fatalf("Candidates() err = %v, want nil", err)
	}
	if len(candidates) != 0 {
		t.Errorf("Candidates() returned %d for a terminal broadcast, want 0", len(candidates))
	}
}

func TestCandidates_SelectsABroadcastNeverTried(t *testing.T) {
	// No fetch row is the ordinary state for a broadcast backfill has not
	// reached, and must not read as a failure.
	coverage := oneDay(store.CoverageMissed, []store.Recording{{State: store.StateFailed}})

	candidates, err := Candidates(coverage, 1, afterSettling(), Window{Location: time.UTC})
	if err != nil {
		t.Fatalf("Candidates() err = %v, want nil", err)
	}
	if len(candidates) != 1 {
		t.Errorf("Candidates() returned %d for an untried broadcast, want 1", len(candidates))
	}
}

// ///////////////////////////////////////////////
// Window
// ///////////////////////////////////////////////

func TestCandidates_IncludesTheOldestDay(t *testing.T) {
	// The lookback lands at whatever time of day the pass runs, while a
	// calendar cell is midnight. Comparing the two drops the oldest day the
	// operator configured, every pass, for good.
	coverage := oneDay(store.CoverageMissed, []store.Recording{{State: store.StateFailed}})

	// Thirty days after the fixture broadcast, at midday, so the lookback
	// lands halfway through the day that broadcast sits on.
	now := planDay.Add(30*24*time.Hour + 12*time.Hour)

	candidates, err := Candidates(coverage, 1, now, Window{
		Lookback: 30 * 24 * time.Hour, Location: time.UTC,
	})
	if err != nil {
		t.Fatalf("Candidates() err = %v, want nil", err)
	}
	if len(candidates) != 1 {
		t.Errorf("Candidates() returned %d on the oldest day in the window, want 1", len(candidates))
	}
}

func TestCandidates_ReadsCoverageThroughToday(t *testing.T) {
	// Coverage buckets by whole days and its end is exclusive, so a range
	// ending now stops at midnight and today never gets a cell. A broadcast
	// this morning that the recorder missed is then invisible until
	// tomorrow, by which time a short archive window may have closed.
	coverage := oneDay(store.CoverageMissed, []store.Recording{{State: store.StateFailed}})
	now := planDay.Add(48*time.Hour + 12*time.Hour)

	if _, err := Candidates(coverage, 1, now, Window{Location: time.UTC}); err != nil {
		t.Fatalf("Candidates() err = %v, want nil", err)
	}

	if !coverage.askedTo.After(now) {
		t.Errorf("coverage read to %v with now at %v, want the range to reach past today",
			coverage.askedTo, now)
	}
}

func TestCandidates_BoundsTheLookback(t *testing.T) {
	// A first pass against a channel with years of history must not try
	// all of it.
	coverage := oneDay(store.CoverageMissed, []store.Recording{{State: store.StateFailed}})

	candidates, err := Candidates(coverage, 1, planDay.AddDate(1, 0, 0), Window{
		Lookback: 7 * 24 * time.Hour, Location: time.UTC,
	})
	if err != nil {
		t.Fatalf("Candidates() err = %v, want nil", err)
	}
	if len(candidates) != 0 {
		t.Errorf("Candidates() returned %d for a day a year back, want 0", len(candidates))
	}
}

func TestCandidates_MeansTheHoursTheLookbackSays(t *testing.T) {
	// The search walks whole days, because a day is what the calendar calls
	// covered or not. The lookback is not a day, so the oldest day it lands
	// in reaches back further than it does, and an operator who asked for
	// the last two days would be handed broadcasts from three.
	//
	// Both broadcasts sit on one uncovered day. Only the later one is
	// inside the range.
	early := planDay.Add(2 * time.Hour)
	late := planDay.Add(20 * time.Hour)
	earlyEnd, lateEnd := early.Add(time.Hour), late.Add(time.Hour)
	coverage := &fakeCoverage{
		days: []store.Day{{Date: planDay, State: store.CoverageMissed, Broadcasts: 2}},
		broadcasts: []store.Broadcast{
			{ID: 1, ChannelID: 1, StartedAt: early, EndedAt: &earlyEnd},
			{ID: 2, ChannelID: 1, StartedAt: late, EndedAt: &lateEnd},
		},
		recordings: map[int64][]store.Recording{
			1: {{State: store.StateFailed}},
			2: {{State: store.StateFailed}},
		},
	}

	// Lands mid-day, so the day is searched and only part of it qualifies.
	now := planDay.Add(30 * time.Hour)
	candidates, err := Candidates(coverage, 1, now, Window{
		Lookback: 20 * time.Hour, Location: time.UTC,
	})
	if err != nil {
		t.Fatalf("Candidates() err = %v, want nil", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("Candidates() returned %d, want only the broadcast inside the range", len(candidates))
	}
	if got := candidates[0].Broadcast.ID; got != 2 {
		t.Errorf("Candidates() chose broadcast %d, want 2: %s is older than the range",
			got, early.Format(time.RFC3339))
	}
}

func TestWindow_FillsAZeroValue(t *testing.T) {
	// A zero window must mean the defaults, not an unbounded pass reaching
	// every broadcast a channel ever made.
	filled := Window{}.withDefaults()

	if filled.Lookback != DefaultLookback {
		t.Errorf("Lookback = %v, want %v", filled.Lookback, DefaultLookback)
	}
	if filled.Settle != DefaultSettle {
		t.Errorf("Settle = %v, want %v", filled.Settle, DefaultSettle)
	}
	if filled.Location == nil {
		t.Error("Location = nil, want a location so days bucket consistently")
	}
}

// ///////////////////////////////////////////////
// Failures
// ///////////////////////////////////////////////

func TestCandidates_ReportsACoverageFailure(t *testing.T) {
	// Selecting nothing and failing to look are different answers, and a
	// caller that could not tell them apart would report a quiet library.
	coverage := &failingCoverage{err: errors.New("database is locked")}

	if _, err := Candidates(coverage, 1, afterSettling(), Window{Location: time.UTC}); err == nil {
		t.Error("Candidates() err = nil when coverage failed, want the failure reported")
	}
}

func (f *failingCoverage) CoverageBetween(int64, time.Time, time.Time, *time.Location) ([]store.Day, error) {
	return nil, f.err
}

func (f *failingCoverage) BroadcastsBetween(int64, time.Time, time.Time) ([]store.Broadcast, error) {
	return nil, f.err
}

func (f *failingCoverage) RecordingsForBroadcast(int64) ([]store.Recording, error) { return nil, f.err }

func (f *failingCoverage) FetchFor(int64) (store.Fetch, error) { return store.Fetch{}, f.err }

func TestMonthOf_LandsOnTheFirstOfTheMonthWhereTheClocksMoveAtMidnight(t *testing.T) {
	// Zones that move their clocks at midnight have no midnight on the day
	// it happens, and time.Date answers with an instant belonging to the
	// day before. A month anchored on the previous month's last day steps
	// past a whole month when a month is added to it, and every missed day
	// in that month goes unseen: no candidate, no fetch, no error.
	tests := []struct {
		name string
		zone string
		at   time.Time
		want string
	}{
		{
			name: "Havana spring forward at midnight",
			zone: "America/Havana",
			at:   time.Date(1990, time.April, 15, 12, 0, 0, 0, time.UTC),
			want: "1990-04-01",
		},
		{
			name: "Asuncion spring forward at midnight",
			zone: "America/Asuncion",
			at:   time.Date(1990, time.October, 15, 12, 0, 0, 0, time.UTC),
			want: "1990-10-01",
		},
		{
			name: "Santiago spring forward at midnight",
			zone: "America/Santiago",
			at:   time.Date(2018, time.August, 15, 12, 0, 0, 0, time.UTC),
			want: "2018-08-01",
		},
		{
			name: "an ordinary zone is unaffected",
			zone: "America/New_York",
			at:   time.Date(2026, time.March, 15, 12, 0, 0, 0, time.UTC),
			want: "2026-03-01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tt.zone)
			if err != nil {
				t.Skipf("zone %s is not available: %v", tt.zone, err)
			}

			got := monthOf(tt.at, loc)
			if stamped := got.Format("2006-01-02"); stamped != tt.want {
				t.Errorf("monthOf() = %s, want the month to start on %s", stamped, tt.want)
			}
			// The instant before it belongs to the month before, which is
			// what makes it the first instant rather than merely a time on
			// the right date.
			if before := got.Add(-time.Nanosecond); before.In(loc).Month() == got.In(loc).Month() {
				t.Errorf("the nanosecond before monthOf() is still in %s, so it is not the month's first instant",
					before.In(loc).Month())
			}
		})
	}
}

func TestGapDays_BuildsEveryMonthInTheRange(t *testing.T) {
	// The walk adds a month at a time. Where a month's first instant is
	// not midnight, carrying that offset forward compounds until a month
	// falls off the end of the range unbuilt.
	loc, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Skipf("zone is not available: %v", err)
	}

	from := time.Date(1990, time.April, 1, 12, 0, 0, 0, loc)
	to := time.Date(1990, time.July, 15, 12, 0, 0, 0, loc)

	seen := map[string]bool{}
	for month := monthOf(from, loc); !month.After(to); month = monthOf(month.AddDate(0, 1, 0), loc) {
		seen[month.Format("2006-01")] = true
	}

	for _, want := range []string{"1990-04", "1990-05", "1990-06", "1990-07"} {
		if !seen[want] {
			t.Errorf("the walk never built %s; it built %v", want, seen)
		}
	}
}
