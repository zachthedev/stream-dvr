package store

import (
	"errors"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// seedBroadcast records a broadcast on a given day at a given hour.
func seedBroadcast(t *testing.T, store *Store, channelID int64, at time.Time, remoteID string) Broadcast {
	t.Helper()

	broadcast, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channelID, RemoteID: remoteID, StartedAt: at, Source: SourceLive,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	return broadcast
}

// seedCapture records a completed recording of a broadcast.
func seedCapture(t *testing.T, store *Store, channelID int64, broadcastID *int64, at time.Time, origin Origin, path string) {
	t.Helper()

	recording, err := store.CreateRecording(Recording{
		ChannelID: channelID, BroadcastID: broadcastID, Path: path,
		State: StateComplete, Origin: origin, StartedAt: at, Bytes: 1000,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	if err := store.FinishRecording(recording.ID, StateComplete, path, 1000, time.Hour, at.Add(time.Hour)); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}
}

// seedCaptureInState records a recording left in a given state, for cases
// where the state is the subject rather than a finished capture.
func seedCaptureInState(t *testing.T, store *Store, channelID int64, broadcastID *int64, at time.Time, state State, path string) {
	t.Helper()

	if _, err := store.CreateRecording(Recording{
		ChannelID: channelID, BroadcastID: broadcastID, Path: path,
		State: state, Origin: OriginLive, StartedAt: at, Bytes: 1000,
	}); err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
}

// dayAt builds a UTC timestamp on a day of August 2026.
func dayAt(day, hour int) time.Time {
	return time.Date(2026, 8, day, hour, 0, 0, 0, time.UTC)
}

// dayAtMinute builds a UTC timestamp on a day of August 2026, to the minute,
// for the cases that turn on which side of midnight something lands.
func dayAtMinute(day, hour, minute int) time.Time {
	return time.Date(2026, 8, day, hour, minute, 0, 0, time.UTC)
}

// findDay returns the entry for a date, failing if absent.
func findDay(t *testing.T, days []Day, day int) Day {
	t.Helper()

	for _, d := range days {
		if d.Date.Day() == day {
			return d
		}
	}
	t.Fatalf("no coverage entry for August %d in %v", day, days)
	return Day{}
}

// ///////////////////////////////////////////////
// CoverageBetween
// ///////////////////////////////////////////////

// seedSession records a recorder session covering a day range, so quiet
// days read as evidence rather than ignorance.
func seedSession(t *testing.T, store *Store, from, to time.Time) {
	t.Helper()

	session, err := store.StartSession(from, sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if err := store.Heartbeat(session.ID, to); err != nil {
		t.Fatalf("Heartbeat() err = %v, want nil", err)
	}
}

func TestCoverageBetween_ADayWithAHoleIsNotFullyCovered(t *testing.T) {
	// Counting broadcasts against captures cannot see a hole. One broadcast
	// with one recording is fully covered by arithmetic while an hour of it
	// was never received, and a day painted live over a missing hour tells
	// the operator to stop looking for footage that is not on disk.
	tests := []struct {
		name string
		fill bool
		want Coverage
	}{
		{
			name: "a hole nothing has filled",
			fill: false,
			want: CoveragePartial,
		},
		{
			// Closing the hole makes the day whole again. Where the bytes
			// came from is a separate fact, carried by the origin of the
			// recording a patch writes.
			name: "a hole a patch filled",
			fill: true,
			want: CoverageLive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)
			seedSession(t, store, dayAt(1, 0), dayAt(28, 0))

			broadcast := seedBroadcast(t, store, channel.ID, dayAt(9, 20), "b-9")
			seedCapture(t, store, channel.ID, &broadcast.ID, dayAt(9, 20), OriginLive, "held.mkv")

			recordings, err := store.RecordingsForChannel(channel.ID, dayAt(9, 0), dayAt(10, 0))
			if err != nil {
				t.Fatalf("RecordingsForChannel() err = %v, want nil", err)
			}
			if len(recordings) != 1 {
				t.Fatalf("seeded %d recordings, want 1", len(recordings))
			}

			gap, err := store.AddGap(recordings[0].ID, 0, time.Hour, "late_start")
			if err != nil {
				t.Fatalf("AddGap() err = %v, want nil", err)
			}
			if tt.fill {
				if err := store.FillGap(gap.ID, dayAt(10, 0)); err != nil {
					t.Fatalf("FillGap() err = %v, want nil", err)
				}
			}

			days, err := store.CoverageBetween(channel.ID, dayAt(1, 0), dayAt(28, 0), time.UTC)
			if err != nil {
				t.Fatalf("CoverageBetween() err = %v, want nil", err)
			}

			ninth := days[8]
			if ninth.Date.Day() != 9 {
				t.Fatalf("days[8] is day %d, want the 9th", ninth.Date.Day())
			}
			if ninth.State != tt.want {
				t.Errorf("State = %q, want %q", ninth.State, tt.want)
			}
			// The arithmetic still says one of one, which is exactly why the
			// hole has to be read from somewhere else.
			if ninth.Broadcasts != 1 || ninth.Captured != 1 {
				t.Errorf("broadcasts/captured = %d/%d, want 1/1",
					ninth.Broadcasts, ninth.Captured)
			}
		})
	}
}

func TestCoverageBetween_NeverPaintsADayHoldingBytesAsQuiet(t *testing.T) {
	// A day the library holds recorded bytes for is never "nobody was
	// looking" or "no broadcast happened". Both readings tell the operator
	// to stop looking for a recording that is sitting on disk.
	store := newStore(t)
	channel := newChannel(t, store)

	// A session across the whole month, so a quiet day reads as no_stream:
	// the more reassuring of the two lies, and the harder to notice.
	seedSession(t, store, dayAt(1, 0), dayAt(28, 0))

	// A capture that ran past midnight, filed against the previous day's
	// broadcast.
	overnight := seedBroadcast(t, store, channel.ID, dayAtMinute(9, 23, 50), "b-9")
	seedCapture(t, store, channel.ID, &overnight.ID, dayAtMinute(10, 0, 30), OriginLive, "overnight.mkv")

	// A broadcast the live poller moved onto the next day, leaving its
	// recording behind on this one.
	tracked, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, StartedAt: dayAtMinute(12, 23, 55), Source: SourceTracker,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	seedCapture(t, store, channel.ID, &tracked.ID, dayAtMinute(12, 23, 56), OriginLive, "upgraded.mkv")
	if _, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, StartedAt: dayAtMinute(13, 0, 5), Source: SourceLive,
	}); err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}

	// A capture that gave up part way, and one the operator purged. Neither
	// counts as coverage, and both left bytes behind.
	seedCaptureInState(t, store, channel.ID, nil, dayAt(16, 20), StateFailed, "gave-up.mkv")
	seedCaptureInState(t, store, channel.ID, nil, dayAt(18, 20), StateTrashed, "purged.mkv")

	// A recording whose broadcast was never discovered at all.
	seedCapture(t, store, channel.ID, nil, dayAt(20, 14), OriginLive, "orphan.mkv")

	days, err := store.CoverageBetween(channel.ID, dayAt(1, 0), dayAt(28, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}

	held := 0
	for _, day := range days {
		if day.Bytes == 0 {
			continue
		}
		held++
		if day.State == CoverageUnknown || day.State == CoverageNoStream {
			t.Errorf("August %d holds %d bytes and reads %q, want a state that admits the recording exists",
				day.Date.Day(), day.Bytes, day.State)
		}
	}
	if held != 5 {
		t.Fatalf("%d days held bytes, want 5; the seeds are not reaching the query", held)
	}
}

func TestCoverageBetween_CountsARecordingOnTheDayItStarted(t *testing.T) {
	// A recording is evidence about its own day even when its broadcast
	// belongs to another one.
	tests := []struct {
		name     string
		seed     func(t *testing.T, store *Store, channelID int64)
		day      int
		want     Coverage
		captured int
	}{
		{
			name: "a capture running past midnight",
			seed: func(t *testing.T, store *Store, channelID int64) {
				broadcast := seedBroadcast(t, store, channelID, dayAtMinute(9, 23, 50), "b-9")
				seedCapture(t, store, channelID, &broadcast.ID, dayAtMinute(10, 0, 30), OriginLive, "overnight.mkv")
			},
			day: 10, want: CoverageLive, captured: 1,
		},
		{
			name: "a broadcast the live poller moved to the next day",
			seed: func(t *testing.T, store *Store, channelID int64) {
				tracked, err := store.UpsertBroadcast(Broadcast{
					ChannelID: channelID, StartedAt: dayAtMinute(10, 23, 55), Source: SourceTracker,
				})
				if err != nil {
					t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
				}
				seedCapture(t, store, channelID, &tracked.ID, dayAtMinute(10, 23, 56), OriginLive, "upgraded.mkv")
				if _, err := store.UpsertBroadcast(Broadcast{
					ChannelID: channelID, StartedAt: dayAtMinute(11, 0, 5), Source: SourceLive,
				}); err != nil {
					t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
				}
			},
			day: 10, want: CoverageLive, captured: 1,
		},
		{
			name: "a capture still running past midnight",
			seed: func(t *testing.T, store *Store, channelID int64) {
				broadcast := seedBroadcast(t, store, channelID, dayAtMinute(9, 23, 50), "b-9")
				seedCaptureInState(t, store, channelID, &broadcast.ID, dayAtMinute(10, 0, 30), StateCapturing, "live.ts")
			},
			day: 10, want: CoverageAtRisk, captured: 1,
		},
		{
			name: "a day whose only capture gave up",
			seed: func(t *testing.T, store *Store, channelID int64) {
				seedCaptureInState(t, store, channelID, nil, dayAt(10, 20), StateFailed, "gave-up.mkv")
			},
			day: 10, want: CoverageMissed, captured: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)
			seedSession(t, store, dayAt(9, 0), dayAt(12, 0))
			tt.seed(t, store, channel.ID)

			days, err := store.CoverageBetween(channel.ID, dayAt(tt.day, 0), dayAt(tt.day+1, 0), time.UTC)
			if err != nil {
				t.Fatalf("CoverageBetween() err = %v, want nil", err)
			}

			got := findDay(t, days, tt.day)
			if got.State != tt.want {
				t.Errorf("August %d state = %q, want %q", tt.day, got.State, tt.want)
			}
			if got.Captured != tt.captured {
				t.Errorf("August %d captured = %d, want %d", tt.day, got.Captured, tt.captured)
			}
			if got.Bytes == 0 {
				t.Errorf("August %d bytes = 0, want the recording's bytes counted", tt.day)
			}
		})
	}
}

func TestCoverageBetween_MarksTheRangeDegradedWhenARowIsSkipped(t *testing.T) {
	// A row nothing can read cannot be attributed to a day, so no day in
	// the range can be vouched for. Reporting one of them as no_stream
	// claims a broadcast did not happen on the strength of evidence that
	// went missing.
	store := newStore(t)
	channel := newChannel(t, store)
	seedSession(t, store, dayAt(10, 0), dayAt(13, 0))

	broadcast := seedBroadcast(t, store, channel.ID, dayAt(10, 16), "b-10")
	seedCapture(t, store, channel.ID, &broadcast.ID, dayAt(10, 16), OriginLive, "day10.mkv")

	days, err := store.CoverageBetween(channel.ID, dayAt(10, 0), dayAt(13, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}
	if got := findDay(t, days, 11); got.State != CoverageNoStream || got.Degraded {
		t.Fatalf("August 11 = {%q, degraded %t}, want {%q, false} before anything is broken",
			got.State, got.Degraded, CoverageNoStream)
	}

	// The recording holds bytes and a state the calendar reads. Losing it
	// takes a day's evidence with it.
	recordings, err := store.RecordingsForChannel(channel.ID, dayAt(10, 0), dayAt(13, 0))
	if err != nil {
		t.Fatalf("RecordingsForChannel() err = %v, want nil", err)
	}
	breakRecording(t, store, recordings[0].ID)

	days, err = store.CoverageBetween(channel.ID, dayAt(10, 0), dayAt(13, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}
	for _, day := range days {
		if !day.Degraded {
			t.Errorf("August %d is not marked degraded, want every day in the range flagged", day.Date.Day())
		}
		if day.State == CoverageNoStream {
			t.Errorf("August %d reads %q while a row is unreadable, want the range to admit it does not know",
				day.Date.Day(), day.State)
		}
	}
}

func TestCoverageBetween_BoundsTheRange(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	t.Run("queries the first storable day", func(t *testing.T) {
		// That day begins before the earliest instant Unix nanoseconds can
		// name, so a query taking the raw day bound fails on it.
		days, err := store.CoverageBetween(channel.ID, minStorable, minStorable.AddDate(0, 0, 2), time.UTC)
		if err != nil {
			t.Fatalf("CoverageBetween() err = %v, want nil", err)
		}
		if len(days) == 0 {
			t.Error("CoverageBetween() returned no days, want the range walked")
		}
	})

	t.Run("refuses a range longer than the cap", func(t *testing.T) {
		// The storable range allows roughly 214,000 days, each an
		// allocation, and no calendar renders them.
		from := dayAt(1, 0)
		if _, err := store.CoverageBetween(channel.ID, from, from.AddDate(0, 0, maxCoverageDays+1), time.UTC); err == nil {
			t.Error("CoverageBetween() err = nil, want a refusal past the day cap")
		}
	})

	t.Run("allows a range at the cap", func(t *testing.T) {
		from := dayAt(1, 0)
		days, err := store.CoverageBetween(channel.ID, from, from.AddDate(0, 0, maxCoverageDays), time.UTC)
		if err != nil {
			t.Fatalf("CoverageBetween() err = %v, want nil", err)
		}
		if len(days) != maxCoverageDays {
			t.Errorf("CoverageBetween() returned %d days, want %d", len(days), maxCoverageDays)
		}
	})
}

func TestCoverageBetween_States(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	// The recorder was running across the whole window, which is what
	// makes a day with no broadcast mean "no broadcast".
	seedSession(t, store, dayAt(10, 0), dayAt(15, 0))

	// Day 10: recorded live, the ordinary case.
	live := seedBroadcast(t, store, channel.ID, dayAt(10, 16), "b-10")
	seedCapture(t, store, channel.ID, &live.ID, dayAt(10, 16), OriginLive, "day10.mkv")

	// Day 11: the recorder was down and the broadcast was pulled from an
	// archive afterward. Recovered audio can be muted where live is not,
	// so this is worth distinguishing on the calendar.
	recovered := seedBroadcast(t, store, channel.ID, dayAt(11, 16), "b-11")
	seedCapture(t, store, channel.ID, &recovered.ID, dayAt(11, 16), OriginRecovered, "day11.mkv")

	// Day 12: two broadcasts, one captured.
	first := seedBroadcast(t, store, channel.ID, dayAt(12, 9), "b-12a")
	seedBroadcast(t, store, channel.ID, dayAt(12, 20), "b-12b")
	seedCapture(t, store, channel.ID, &first.ID, dayAt(12, 9), OriginLive, "day12.mkv")

	// Day 13: a broadcast happened and nothing captured it. This is the
	// state the calendar exists to surface.
	seedBroadcast(t, store, channel.ID, dayAt(13, 16), "b-13")

	// Day 14: no broadcast at all.

	days, err := store.CoverageBetween(channel.ID, dayAt(10, 0), dayAt(15, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}
	if len(days) != 5 {
		t.Fatalf("CoverageBetween() returned %d days, want 5", len(days))
	}

	tests := []struct {
		day        int
		want       Coverage
		broadcasts int
		captured   int
	}{
		{day: 10, want: CoverageLive, broadcasts: 1, captured: 1},
		{day: 11, want: CoverageRecovered, broadcasts: 1, captured: 1},
		{day: 12, want: CoveragePartial, broadcasts: 2, captured: 1},
		{day: 13, want: CoverageMissed, broadcasts: 1, captured: 0},
		{day: 14, want: CoverageNoStream, broadcasts: 0, captured: 0},
	}

	for _, tt := range tests {
		t.Run(tt.want.String(), func(t *testing.T) {
			got := findDay(t, days, tt.day)
			if got.State != tt.want {
				t.Errorf("August %d state = %q, want %q", tt.day, got.State, tt.want)
			}
			if got.Broadcasts != tt.broadcasts {
				t.Errorf("August %d broadcasts = %d, want %d", tt.day, got.Broadcasts, tt.broadcasts)
			}
			if got.Captured != tt.captured {
				t.Errorf("August %d captured = %d, want %d", tt.day, got.Captured, tt.captured)
			}
		})
	}
}

func TestCoverageBetween_ReturnsEveryDayInRange(t *testing.T) {
	// The caller renders a grid. Omitting quiet days would make it fill
	// holes itself and guess at what a missing day meant.
	store := newStore(t)
	channel := newChannel(t, store)

	days, err := store.CoverageBetween(channel.ID, dayAt(1, 0), dayAt(32, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}
	if len(days) != 31 {
		t.Fatalf("CoverageBetween() returned %d days, want all 31 of August", len(days))
	}
	for i, day := range days {
		if day.Date.Day() != i+1 {
			t.Fatalf("days[%d] is August %d, want August %d", i, day.Date.Day(), i+1)
		}
		// Nothing has ever run, so every day is genuinely unknown rather
		// than a day with no broadcast.
		if day.State != CoverageUnknown {
			t.Errorf("August %d state = %q, want %q with no recorder history", i+1, day.State, CoverageUnknown)
		}
	}
}

func TestCoverageBetween_DistinguishesQuietFromUnwatched(t *testing.T) {
	// Reporting a day the recorder was switched off as "no stream" reads
	// as reassurance, when it is exactly the kind of day a broadcast could
	// have been missed on.
	store := newStore(t)
	channel := newChannel(t, store)

	// Watching on the 10th and 11th only.
	seedSession(t, store, dayAt(10, 0), dayAt(11, 23))

	days, err := store.CoverageBetween(channel.ID, dayAt(10, 0), dayAt(14, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}

	tests := []struct {
		day         int
		want        Coverage
		wantWatched bool
	}{
		{day: 10, want: CoverageNoStream, wantWatched: true},
		{day: 11, want: CoverageNoStream, wantWatched: true},
		{day: 12, want: CoverageUnknown},
		{day: 13, want: CoverageUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.want.String(), func(t *testing.T) {
			got := findDay(t, days, tt.day)
			if got.State != tt.want {
				t.Errorf("August %d state = %q, want %q", tt.day, got.State, tt.want)
			}
			if got.Watched != tt.wantWatched {
				t.Errorf("August %d watched = %t, want %t", tt.day, got.Watched, tt.wantWatched)
			}
		})
	}
}

func TestCoverageBetween_CrashedSessionOnlyCoversUpToItsHeartbeat(t *testing.T) {
	// A crashed session has no stop time, so its last heartbeat is the
	// last moment it was known alive. Days after that are unwatched, which
	// is precisely the outage the calendar has to surface.
	store := newStore(t)
	channel := newChannel(t, store)

	session, err := store.StartSession(dayAt(10, 0), sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if err := store.Heartbeat(session.ID, dayAt(11, 12)); err != nil {
		t.Fatalf("Heartbeat() err = %v, want nil", err)
	}

	days, err := store.CoverageBetween(channel.ID, dayAt(10, 0), dayAt(14, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}

	if got := findDay(t, days, 11); got.State != CoverageNoStream {
		t.Errorf("August 11 state = %q, want %q up to the last heartbeat", got.State, CoverageNoStream)
	}
	if got := findDay(t, days, 12); got.State != CoverageUnknown {
		t.Errorf("August 12 state = %q, want %q after the recorder died", got.State, CoverageUnknown)
	}
}

func TestCoverageBetween_LeavesAFrozenDayUnknown(t *testing.T) {
	// A machine that sleeps and resumes leaves two sessions with a hole
	// between them, and the days inside that hole were not watched. Painting
	// them "no stream" would tell the operator a broadcast could not have
	// been missed on a day nobody was looking.
	store := newStore(t)
	channel := newChannel(t, store)

	// Stopped on the 10th, resumed on the 13th, which is what a lid closing
	// over a long weekend leaves behind.
	session, err := store.StartSession(dayAt(10, 0), sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if err := store.StopSession(session.ID, dayAt(10, 20)); err != nil {
		t.Fatalf("StopSession() err = %v, want nil", err)
	}
	seedSession(t, store, dayAt(13, 6), dayAt(13, 23))

	days, err := store.CoverageBetween(channel.ID, dayAt(10, 0), dayAt(14, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}

	tests := []struct {
		day  int
		want Coverage
	}{
		{day: 10, want: CoverageNoStream},
		{day: 11, want: CoverageUnknown},
		{day: 12, want: CoverageUnknown},
		{day: 13, want: CoverageNoStream},
	}

	for _, tt := range tests {
		got := findDay(t, days, tt.day)
		if got.State != tt.want {
			t.Errorf("August %d state = %q, want %q", tt.day, got.State, tt.want)
		}
	}
}

func TestCoverageBetween_BucketsByLocalDay(t *testing.T) {
	// A broadcast starting late in the evening belongs to the evening the
	// viewer remembers. Bucketing in UTC would file a 23:30 New York stream
	// under the next day.
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}

	store := newStore(t)
	channel := newChannel(t, store)

	lateEvening := time.Date(2026, 8, 12, 23, 30, 0, 0, newYork)
	broadcast := seedBroadcast(t, store, channel.ID, lateEvening, "late")
	seedCapture(t, store, channel.ID, &broadcast.ID, lateEvening, OriginLive, "late.mkv")

	t.Run("local bucketing keeps it on the 12th", func(t *testing.T) {
		days, err := store.CoverageBetween(channel.ID,
			time.Date(2026, 8, 12, 0, 0, 0, 0, newYork),
			time.Date(2026, 8, 14, 0, 0, 0, 0, newYork), newYork)
		if err != nil {
			t.Fatalf("CoverageBetween() err = %v, want nil", err)
		}
		if got := findDay(t, days, 12); got.State != CoverageLive {
			t.Errorf("August 12 state = %q, want %q", got.State, CoverageLive)
		}
		// The subject here is which day the broadcast lands on, so the
		// assertion is that the 13th did not claim it, whatever a quiet
		// day happens to be called.
		if got := findDay(t, days, 13); got.State == CoverageLive {
			t.Error("August 13 claimed the broadcast, want it on the 12th")
		}
	})

	t.Run("utc bucketing moves it to the 13th", func(t *testing.T) {
		// 23:30 New York is 03:30 UTC the next day, which is exactly the
		// off-by-one the location parameter exists to prevent.
		days, err := store.CoverageBetween(channel.ID,
			time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC), time.UTC)
		if err != nil {
			t.Fatalf("CoverageBetween() err = %v, want nil", err)
		}
		if got := findDay(t, days, 13); got.State != CoverageLive {
			t.Errorf("August 13 state in UTC = %q, want %q", got.State, CoverageLive)
		}
	})
}

func TestCoverageBetween_OrphanRecordingCountsAsCoverage(t *testing.T) {
	// A recording adopted from an existing folder has no broadcast row,
	// but it still proves the day was covered.
	store := newStore(t)
	channel := newChannel(t, store)
	seedCapture(t, store, channel.ID, nil, dayAt(10, 16), OriginLive, "adopted.mkv")

	days, err := store.CoverageBetween(channel.ID, dayAt(10, 0), dayAt(11, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}
	got := findDay(t, days, 10)
	if got.State != CoverageLive {
		t.Errorf("state = %q, want %q", got.State, CoverageLive)
	}
	if got.Captured != 1 {
		t.Errorf("captured = %d, want 1", got.Captured)
	}
}

func TestCoverageBetween_ReportsBytesPerDay(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	broadcast := seedBroadcast(t, store, channel.ID, dayAt(10, 16), "b-10")
	seedCapture(t, store, channel.ID, &broadcast.ID, dayAt(10, 16), OriginLive, "a.mkv")
	seedCapture(t, store, channel.ID, &broadcast.ID, dayAt(10, 20), OriginLive, "b.mkv")

	days, err := store.CoverageBetween(channel.ID, dayAt(10, 0), dayAt(11, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}
	if got := findDay(t, days, 10); got.Bytes != 2000 {
		t.Errorf("bytes = %d, want 2000", got.Bytes)
	}
}

func TestCoverageBetween_RejectsAnEmptyRange(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	tests := []struct {
		name     string
		from, to time.Time
	}{
		{name: "to before from", from: dayAt(12, 0), to: dayAt(10, 0)},
		{name: "same day", from: dayAt(12, 0), to: dayAt(12, 6)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.CoverageBetween(channel.ID, tt.from, tt.to, time.UTC); err == nil {
				t.Error("CoverageBetween() err = nil, want a rejection")
			}
		})
	}
}

// ///////////////////////////////////////////////
// Walking days across a clock change
// ///////////////////////////////////////////////

func TestCoverageBetween_GivesEveryDayItsOwnRowAcrossAMidnightClockChange(t *testing.T) {
	// In these zones the clocks go forward at midnight, so that midnight
	// never happens and time.Date answers with 23:00 the day before. A walk
	// stepping from there stays an hour behind for the rest of the month,
	// which returns one extra row and stamps two of them with the same date.
	tests := []struct {
		name  string
		zone  string
		month time.Month
		days  int
	}{
		{name: "havana forward at midnight", zone: "America/Havana", month: time.March, days: 31},
		{name: "santiago forward at midnight", zone: "America/Santiago", month: time.September, days: 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc, err := time.LoadLocation(tt.zone)
			if err != nil {
				t.Skipf("timezone database unavailable: %v", err)
			}

			store := newStore(t)
			channel := newChannel(t, store)

			days, err := store.CoverageBetween(channel.ID,
				time.Date(2026, tt.month, 1, 12, 0, 0, 0, loc),
				time.Date(2026, tt.month+1, 1, 12, 0, 0, 0, loc), loc)
			if err != nil {
				t.Fatalf("CoverageBetween() err = %v, want nil", err)
			}
			if len(days) != tt.days {
				t.Errorf("CoverageBetween() returned %d rows, want %d", len(days), tt.days)
			}

			// Each row must be stamped with the day it reports. A row whose
			// Date belongs to the previous day lands on the wrong square of
			// the month grid, which is what the caller renders.
			for index, day := range days {
				wantDay := index + 1
				if got := day.Date.In(loc).Day(); got != wantDay {
					t.Errorf("row %d is stamped %s, want day %d",
						index, day.Date.In(loc).Format(time.RFC3339), wantDay)
				}
			}
		})
	}
}

func TestCoverageBetween_CountsARecordingOnceAcrossAMidnightClockChange(t *testing.T) {
	// A repeated date carries a real cost. The recording that starts on it
	// lands on both rows, and its bytes count twice.
	loc, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}

	store := newStore(t)
	channel := newChannel(t, store)

	// Noon on the day whose midnight does not exist.
	transitionDay := time.Date(2026, 3, 8, 12, 0, 0, 0, loc)
	broadcast := seedBroadcast(t, store, channel.ID, transitionDay, "transition")
	seedCapture(t, store, channel.ID, &broadcast.ID, transitionDay, OriginLive, "transition.mkv")

	days, err := store.CoverageBetween(channel.ID,
		time.Date(2026, 3, 5, 0, 0, 0, 0, loc),
		time.Date(2026, 3, 12, 0, 0, 0, 0, loc), loc)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}

	var (
		captured int
		bytes    int64
	)
	for _, day := range days {
		captured += day.Captured
		bytes += day.Bytes
	}
	if captured != 1 {
		t.Errorf("recording counted on %d days, want 1", captured)
	}
	if bytes != 1000 {
		t.Errorf("bytes across the range = %d, want 1000", bytes)
	}
}

// ///////////////////////////////////////////////
// What counts as covered
// ///////////////////////////////////////////////

func TestCoverageBetween_ClassifiesADayByItsRecordingStates(t *testing.T) {
	// A day is only as covered as its recordings. A failed capture leaves
	// bytes on disk and proves nothing, and a capture still held outside
	// the library is real but unfinished.
	tests := []struct {
		name  string
		state State
		want  Coverage
	}{
		{name: "complete", state: StateComplete, want: CoverageLive},
		{name: "failed", state: StateFailed, want: CoverageMissed},
		{name: "trashed", state: StateTrashed, want: CoverageMissed},
		{name: "capturing", state: StateCapturing, want: CoverageAtRisk},
		{name: "awaiting finalize", state: StateAwaitingFinalize, want: CoverageAtRisk},
		{name: "awaiting metadata", state: StateAwaitingMetadata, want: CoverageAtRisk},
		{name: "awaiting file", state: StateAwaitingFile, want: CoverageAtRisk},
		{name: "a state this build does not know", state: State("invented"), want: CoverageMissed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)

			at := dayAt(12, 20)
			broadcast := seedBroadcast(t, store, channel.ID, at, "one")
			if _, err := store.CreateRecording(Recording{
				ChannelID: channel.ID, BroadcastID: &broadcast.ID, Path: "one.ts",
				State: StateCapturing, Origin: OriginLive, StartedAt: at, Bytes: 1000,
			}); err != nil {
				t.Fatalf("CreateRecording() err = %v, want nil", err)
			}
			// The state is written directly because SetState validates, and
			// the unknown-state case has to reach the classifier.
			if _, err := store.db.Exec(
				`UPDATE recordings SET state = ? WHERE path = ?`, string(tt.state), "one.ts"); err != nil {
				t.Fatalf("forcing state err = %v, want nil", err)
			}

			days, err := store.CoverageBetween(channel.ID, dayAt(12, 0), dayAt(13, 0), time.UTC)
			if err != nil {
				t.Fatalf("CoverageBetween() err = %v, want nil", err)
			}
			if got := findDay(t, days, 12); got.State != tt.want {
				t.Errorf("a %s recording made August 12 %q, want %q", tt.state, got.State, tt.want)
			}
		})
	}
}

func TestCoverageBetween_PrefersTheStrongestEvidenceForABroadcast(t *testing.T) {
	// One broadcast can have several recordings. A failed attempt beside a
	// complete one must not drag the day down, and a complete one beside a
	// stranded attempt must not paper over nothing: the day is covered.
	store := newStore(t)
	channel := newChannel(t, store)

	at := dayAt(12, 20)
	broadcast := seedBroadcast(t, store, channel.ID, at, "one")
	seedCaptureInState(t, store, channel.ID, &broadcast.ID, at, StateFailed, "failed.ts")
	seedCaptureInState(t, store, channel.ID, &broadcast.ID, at, StateComplete, "good.mkv")

	days, err := store.CoverageBetween(channel.ID, dayAt(12, 0), dayAt(13, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}
	if got := findDay(t, days, 12); got.State != CoverageLive {
		t.Errorf("August 12 state = %q, want %q", got.State, CoverageLive)
	}
}

func TestCoverageBetween_AFailedOrphanIsNotACoveredDay(t *testing.T) {
	// A recording with no broadcast row is still evidence a day was covered.
	// A failed one is not, and reporting it as captured hides the day the
	// recorder was up and produced nothing usable.
	store := newStore(t)
	channel := newChannel(t, store)

	seedSession(t, store, dayAt(12, 0), dayAt(13, 0))
	seedCaptureInState(t, store, channel.ID, nil, dayAt(12, 20), StateFailed, "orphan.ts")

	days, err := store.CoverageBetween(channel.ID, dayAt(12, 0), dayAt(13, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}

	got := findDay(t, days, 12)
	if got.State == CoverageLive || got.State == CoverageRecovered {
		t.Errorf("August 12 state = %q, want a failed orphan not to count as captured", got.State)
	}
	if got.Captured != 0 {
		t.Errorf("August 12 captured = %d, want 0", got.Captured)
	}
}

func TestCoverageBetween_AStrandedOrphanIsAtRisk(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	seedSession(t, store, dayAt(12, 0), dayAt(13, 0))
	seedCaptureInState(t, store, channel.ID, nil, dayAt(12, 20), StateAwaitingFile, "orphan.ts")

	days, err := store.CoverageBetween(channel.ID, dayAt(12, 0), dayAt(13, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}

	got := findDay(t, days, 12)
	if got.State != CoverageAtRisk {
		t.Errorf("August 12 state = %q, want %q", got.State, CoverageAtRisk)
	}
	if got.Captured != 1 {
		t.Errorf("August 12 captured = %d, want 1", got.Captured)
	}
}

// ///////////////////////////////////////////////
// Sub-second range bounds
// ///////////////////////////////////////////////

func TestCoverageBetween_CountsARecordingAFractionPastTheDayBoundary(t *testing.T) {
	// This start time is a fraction of a second past the day's lower bound,
	// so it belongs inside the day. A range bound that sorts it below the
	// bound it comes after paints the day quiet while the recording sits on
	// disk.
	store := newStore(t)
	channel := newChannel(t, store)

	justAfterMidnight := time.Date(2026, 8, 12, 0, 0, 0, 123456700, time.UTC)
	broadcast := seedBroadcast(t, store, channel.ID, justAfterMidnight, "fraction")
	seedCapture(t, store, channel.ID, &broadcast.ID, justAfterMidnight, OriginLive, "fraction.mkv")

	days, err := store.CoverageBetween(channel.ID, dayAt(12, 0), dayAt(13, 0), time.UTC)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}
	if got := findDay(t, days, 12); got.State != CoverageLive {
		t.Errorf("August 12 state = %q, want %q", got.State, CoverageLive)
	}
}

func TestCoverageBetween_NilLocationFallsBackToUTC(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	days, err := store.CoverageBetween(channel.ID, dayAt(10, 0), dayAt(11, 0), nil)
	if err != nil {
		t.Fatalf("CoverageBetween() err = %v, want nil", err)
	}
	if len(days) != 1 {
		t.Fatalf("CoverageBetween() returned %d days, want 1", len(days))
	}
	if days[0].Date.Location() != time.UTC {
		t.Errorf("date location = %s, want UTC", days[0].Date.Location())
	}
}

// ///////////////////////////////////////////////
// NeedsRecovery
// ///////////////////////////////////////////////

func TestNeedsRecovery(t *testing.T) {
	// Every state, because the one that matters most is the one nobody
	// thinks to list. A recording mid-capture must stop recovery, or
	// backfill races the recorder for the same broadcast and replaces a
	// live copy with an archive copy the platform has muted.
	tests := []struct {
		name       string
		recordings []Recording
		want       bool
		why        string
	}{
		{
			name: "no recordings at all",
			want: true,
			why:  "a broadcast nothing captured is exactly what backfill is for",
		},
		{
			name:       "complete",
			recordings: []Recording{{State: StateComplete}},
			want:       false,
			why:        "it is in the library and a recovered copy is worth less",
		},
		{
			name:       "capturing",
			recordings: []Recording{{State: StateCapturing}},
			want:       false,
			why:        "the recorder holds this broadcast right now",
		},
		{
			name:       "awaiting finalize",
			recordings: []Recording{{State: StateAwaitingFinalize}},
			want:       false,
			why:        "the bytes are on disk and on their way into the library",
		},
		{
			name:       "awaiting metadata",
			recordings: []Recording{{State: StateAwaitingMetadata}},
			want:       false,
			why:        "a parked capture is a playable file, not a gap",
		},
		{
			name:       "awaiting file",
			recordings: []Recording{{State: StateAwaitingFile}},
			want:       false,
			why:        "the same, waiting on a file rather than on metadata",
		},
		{
			name:       "failed",
			recordings: []Recording{{State: StateFailed}},
			want:       true,
			why:        "a capture that gave up left nothing behind",
		},
		{
			name:       "trashed",
			recordings: []Recording{{State: StateTrashed}},
			want:       true,
			why:        "the operator removed it, and a gap is a gap",
		},
		{
			name:       "a state this build does not know",
			recordings: []Recording{{State: State("written_by_a_newer_build")}},
			want:       true,
			why:        "an unreadable state is never proof that a broadcast was kept",
		},
		{
			name:       "every recording failed",
			recordings: []Recording{{State: StateFailed}, {State: StateFailed}},
			want:       true,
			why:        "repeated failure is still failure",
		},
		{
			name:       "one of several succeeded",
			recordings: []Recording{{State: StateFailed}, {State: StateComplete}},
			want:       false,
			why:        "the daemon files a fresh recording per attempt, so one success is enough",
		},
		{
			name:       "a failure followed by a capture in flight",
			recordings: []Recording{{State: StateFailed}, {State: StateCapturing}},
			want:       false,
			why:        "the retry is running, and backfill must not race it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeedsRecovery(tt.recordings); got != tt.want {
				t.Errorf("NeedsRecovery() = %v, want %v (%s)", got, tt.want, tt.why)
			}
		})
	}
}

func TestNeedsRecovery_AgreesWithTheDayLevelRule(t *testing.T) {
	// The day filter and the broadcast filter are one rule applied at two
	// scales. If they ever disagree, backfill either refetches a day the
	// calendar calls covered or skips one it calls missed.
	for _, state := range []State{
		StateCapturing, StateAwaitingFinalize, StateAwaitingMetadata,
		StateAwaitingFile, StateComplete, StateFailed, StateTrashed,
	} {
		t.Run(string(state), func(t *testing.T) {
			proves := stateEvidence(state) != evidenceNone
			if needs := NeedsRecovery([]Recording{{State: state}}); needs == proves {
				t.Errorf("NeedsRecovery = %v while stateEvidence proves coverage = %v; "+
					"the two rules disagree about %q", needs, proves, state)
			}
		})
	}
}

func TestProvenance_AnUnknownOriginNeverPaintsADayLive(t *testing.T) {
	// Live is the one rung that claims the recorder saw the broadcast
	// happen. A row written by a newer build, or edited by hand, carries an
	// origin this build cannot weigh, and falling through to live would have
	// the calendar vouch for a capture nothing here witnessed.
	tests := []struct {
		name   string
		origin Origin
		want   Coverage
	}{
		{name: "live", origin: OriginLive, want: CoverageLive},
		{name: "recovered", origin: OriginRecovered, want: CoverageRecovered},
		{name: "imported", origin: OriginImported, want: CoverageImported},
		{name: "a value this build has never heard of", origin: Origin("teleported"), want: CoverageImported},
		{name: "empty", origin: Origin(""), want: CoverageImported},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var origins provenance
			origins.note(tt.origin)

			if got := origins.state(); got != tt.want {
				t.Errorf("state() = %q for origin %q, want %q", got, tt.origin, tt.want)
			}
		})
	}
}

func TestProvenance_KeepsTheWeakestThingKnown(t *testing.T) {
	// A day is only as trustworthy as its least trustworthy recording. One
	// imported file among ten live captures still means the day rests partly
	// on a reading of a filename.
	tests := []struct {
		name    string
		origins []Origin
		atRisk  bool
		want    Coverage
	}{
		{name: "all live", origins: []Origin{OriginLive, OriginLive}, want: CoverageLive},
		{
			name:    "one imported among live",
			origins: []Origin{OriginLive, OriginImported, OriginLive},
			want:    CoverageImported,
		},
		{
			name:    "imported outranks recovered",
			origins: []Origin{OriginRecovered, OriginImported},
			want:    CoverageImported,
		},
		{
			name:    "at risk outranks everything",
			origins: []Origin{OriginImported, OriginRecovered},
			atRisk:  true,
			want:    CoverageAtRisk,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var origins provenance
			origins.atRisk = tt.atRisk
			for _, origin := range tt.origins {
				origins.note(origin)
			}

			if got := origins.state(); got != tt.want {
				t.Errorf("state() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCoverages_ListsEveryStateExactlyOnce(t *testing.T) {
	// Anything that paints, labels, or counts states reads this list, so a
	// state missing from it renders as a blank cell and a state named twice
	// draws two legend rows for one thing.
	seen := make(map[Coverage]bool, len(Coverages()))
	for _, coverage := range Coverages() {
		if seen[coverage] {
			t.Errorf("Coverages() repeats %q", coverage)
		}
		seen[coverage] = true
	}

	// Every state the ladder and the tally can produce has to be in it.
	for _, reachable := range []Coverage{
		CoverageMissed, CoveragePartial, CoverageAtRisk, CoverageImported,
		CoverageRecovered, CoverageLive, CoverageNoStream, CoverageUnknown,
	} {
		if !seen[reachable] {
			t.Errorf("Coverages() omits %q", reachable)
		}
	}
}

func TestRecompressCandidates_LeavesAnImportedRecordingAlone(t *testing.T) {
	// An imported recording's start comes from a filename and is old by
	// construction, so every one heads this queue the moment it is adopted.
	// Recompressing removes the original, and an operator who adopts an
	// archive asked the library to record where their files are.
	db := newStore(t)
	channel, err := db.UpsertChannel("twitch", "atrioc", "")
	if err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, origin := range []Origin{OriginLive, OriginRecovered, OriginImported} {
		if _, err := db.CreateRecording(Recording{
			ChannelID: channel.ID,
			Path:      "atrioc/2020/" + string(origin) + ".mkv",
			State:     StateComplete,
			Origin:    origin,
			Bytes:     1024,
			Duration:  time.Hour,
			StartedAt: old,
		}); err != nil {
			t.Fatalf("CreateRecording(%q) err = %v, want nil", origin, err)
		}
	}

	candidates, err := db.RecompressCandidates(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("RecompressCandidates() err = %v, want nil", err)
	}

	if len(candidates) != 2 {
		t.Fatalf("offered %d recordings, want the 2 the recorder made", len(candidates))
	}
	for _, candidate := range candidates {
		if candidate.Origin == OriginImported {
			t.Errorf("offered an imported recording at %q for re-encoding", candidate.Path)
		}
	}
}

func TestSetPath_NamesAPathAnotherRecordingAlreadyHolds(t *testing.T) {
	// The organizer treats a lost path as unrecoverable and retries forever,
	// so it has to be able to tell a claimed path from a broken database. An
	// import adopting a file an interrupted finalize was about to name is
	// the ordinary way this happens.
	db := newStore(t)
	channel, err := db.UpsertChannel("twitch", "atrioc", "")
	if err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}

	first, err := db.CreateRecording(Recording{
		ChannelID: channel.ID, Path: "atrioc/2026/one.mkv", State: StateComplete,
		Origin: OriginLive, Bytes: 1, Duration: time.Second,
		StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	second, err := db.CreateRecording(Recording{
		ChannelID: channel.ID, Path: "incoming/two.ts", State: StateAwaitingFinalize,
		Origin: OriginLive, Bytes: 1, Duration: time.Second,
		StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}

	err = db.SetPath(second.ID, first.Path)
	if !errors.Is(err, ErrDuplicatePath) {
		t.Errorf("SetPath() err = %v, want ErrDuplicatePath", err)
	}
}
