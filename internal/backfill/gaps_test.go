package backfill

import (
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// filedGap is one call the detector made to AddGap.
type filedGap struct {
	recordingID int64
	start       time.Duration
	end         time.Duration
	reason      string
}

// fakeGaps records what the detector filed, deduplicating by span the way
// the store's unique index does.
type fakeGaps struct {
	recordings []store.Recording
	filed      []filedGap
	nextID     int64
}

// broadcastStart is when the fixture broadcast began.
var broadcastStart = time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)

// RecordingsForBroadcast implements Gaps, answering with a copy.
func (f *fakeGaps) RecordingsForBroadcast(int64) ([]store.Recording, error) {
	return append([]store.Recording(nil), f.recordings...), nil
}

// AddGap implements Gaps with the store's own deduplication, so a test
// that runs the detector twice sees what the database would hold.
func (f *fakeGaps) AddGap(recordingID int64, start, end time.Duration, reason string) (store.Gap, error) {
	for _, existing := range f.filed {
		if existing.recordingID == recordingID && existing.start == start && existing.end == end {
			return store.Gap{RecordingID: recordingID, Start: start, End: end, Reason: existing.reason}, nil
		}
	}
	f.nextID++
	f.filed = append(f.filed, filedGap{recordingID: recordingID, start: start, end: end, reason: reason})
	return store.Gap{ID: f.nextID, RecordingID: recordingID, Start: start, End: end, Reason: reason}, nil
}

// aBroadcast returns the fixture broadcast.
func aBroadcast() store.Broadcast {
	return store.Broadcast{ID: 7, ChannelID: 1, StartedAt: broadcastStart}
}

// at returns a time offset from the broadcast's start.
func at(offset time.Duration) time.Time { return broadcastStart.Add(offset) }

// endedAt returns a pointer to a time offset from the broadcast's start.
func endedAt(offset time.Duration) *time.Time {
	ended := at(offset)
	return &ended
}

// ///////////////////////////////////////////////
// Detection
// ///////////////////////////////////////////////

func TestDetect_FindsAHoleBetweenTwoRecordings(t *testing.T) {
	// The ordinary reconnect: a capture ends and the next poll starts
	// another, and what happened in between was not recorded.
	gaps := &fakeGaps{recordings: []store.Recording{
		{ID: 1, State: store.StateComplete, StartedAt: at(0), EndedAt: endedAt(time.Hour)},
		{ID: 2, State: store.StateComplete, StartedAt: at(90 * time.Minute), EndedAt: endedAt(2 * time.Hour)},
	}}

	filed, err := Detect(gaps, aBroadcast())
	if err != nil {
		t.Fatalf("Detect() err = %v, want nil", err)
	}
	if len(filed) != 1 {
		t.Fatalf("Detect() filed %d gaps, want 1", len(filed))
	}
	if filed[0].Start != time.Hour || filed[0].End != 90*time.Minute {
		t.Errorf("gap = [%v, %v], want [1h, 1h30m] measured from the broadcast", filed[0].Start, filed[0].End)
	}
	if filed[0].Reason != ReasonReconnect {
		t.Errorf("Reason = %q, want %q", filed[0].Reason, ReasonReconnect)
	}
}

func TestDetect_FindsALateStart(t *testing.T) {
	// A hole that sits before the recording begins, so no offset from the
	// recording's own start can describe it.
	gaps := &fakeGaps{recordings: []store.Recording{
		{ID: 1, State: store.StateComplete, StartedAt: at(33 * time.Minute), EndedAt: endedAt(2 * time.Hour)},
	}}

	filed, err := Detect(gaps, aBroadcast())
	if err != nil {
		t.Fatalf("Detect() err = %v, want nil", err)
	}
	if len(filed) != 1 {
		t.Fatalf("Detect() filed %d gaps, want 1", len(filed))
	}
	if filed[0].Start != 0 || filed[0].End != 33*time.Minute {
		t.Errorf("gap = [%v, %v], want [0, 33m]", filed[0].Start, filed[0].End)
	}
	if filed[0].Reason != ReasonLateStart {
		t.Errorf("Reason = %q, want %q", filed[0].Reason, ReasonLateStart)
	}
}

func TestDetect_MeasuresAReconnectFromTheBroadcastNotTheRecording(t *testing.T) {
	// The anchor only shows when the two differ. A first recording that
	// begins exactly when the broadcast did makes the broadcast-anchored
	// and recording-anchored offsets identical, and proves nothing.
	gaps := &fakeGaps{recordings: []store.Recording{
		{ID: 1, State: store.StateComplete, StartedAt: at(20 * time.Minute), EndedAt: endedAt(time.Hour)},
		{ID: 2, State: store.StateComplete, StartedAt: at(90 * time.Minute), EndedAt: endedAt(2 * time.Hour)},
	}}

	filed, err := Detect(gaps, aBroadcast())
	if err != nil {
		t.Fatalf("Detect() err = %v, want nil", err)
	}

	var reconnect *store.Gap
	for i, gap := range filed {
		if gap.Reason == ReasonReconnect {
			reconnect = &filed[i]
		}
	}
	if reconnect == nil {
		t.Fatalf("Detect() filed %d gaps with no reconnect among them", len(filed))
	}
	// Anchored to the recording these would read [40m, 70m].
	if reconnect.Start != time.Hour || reconnect.End != 90*time.Minute {
		t.Errorf("reconnect = [%v, %v], want [1h, 1h30m] measured from the broadcast",
			reconnect.Start, reconnect.End)
	}
}

func TestDetect_AnchorsEveryGapToTheEarliestRecording(t *testing.T) {
	// The earliest recording is the row that survives however many
	// reconnects followed, and the one a reader looks at to ask what this
	// broadcast is missing.
	gaps := &fakeGaps{recordings: []store.Recording{
		{ID: 2, State: store.StateComplete, StartedAt: at(90 * time.Minute), EndedAt: endedAt(2 * time.Hour)},
		{ID: 1, State: store.StateComplete, StartedAt: at(30 * time.Minute), EndedAt: endedAt(time.Hour)},
	}}

	if _, err := Detect(gaps, aBroadcast()); err != nil {
		t.Fatalf("Detect() err = %v, want nil", err)
	}
	for _, gap := range gaps.filed {
		if gap.recordingID != 1 {
			t.Errorf("gap attached to recording %d, want the earliest, 1", gap.recordingID)
		}
	}
}

func TestDetect_IsRepeatable(t *testing.T) {
	// The detector runs on a timer and re-derives every gap each pass.
	// Without the store's unique span that is one duplicate row per pass.
	gaps := &fakeGaps{recordings: []store.Recording{
		{ID: 1, State: store.StateComplete, StartedAt: at(0), EndedAt: endedAt(time.Hour)},
		{ID: 2, State: store.StateComplete, StartedAt: at(90 * time.Minute), EndedAt: endedAt(2 * time.Hour)},
	}}

	for range 3 {
		if _, err := Detect(gaps, aBroadcast()); err != nil {
			t.Fatalf("Detect() err = %v, want nil", err)
		}
	}
	if len(gaps.filed) != 1 {
		t.Errorf("three passes left %d gaps, want 1", len(gaps.filed))
	}
}

func TestDetect_IgnoresAHoleTooShortToPatch(t *testing.T) {
	// A poll interval means the recorder joins some seconds after a stream
	// starts. Filing those fills the database with gaps nobody would patch
	// and no archive could patch accurately.
	gaps := &fakeGaps{recordings: []store.Recording{
		{ID: 1, State: store.StateComplete, StartedAt: at(5 * time.Second), EndedAt: endedAt(time.Hour)},
		{ID: 2, State: store.StateComplete, StartedAt: at(time.Hour + 3*time.Second), EndedAt: endedAt(2 * time.Hour)},
	}}

	filed, err := Detect(gaps, aBroadcast())
	if err != nil {
		t.Fatalf("Detect() err = %v, want nil", err)
	}
	if len(filed) != 0 {
		t.Errorf("Detect() filed %d gaps for holes under %v, want 0", len(filed), minGap)
	}
}

func TestDetect_IgnoresARecordingHoldingNothing(t *testing.T) {
	// A failed capture left no bytes, so a hole where one sits is not a
	// hole between two recordings: it is part of the surrounding one.
	// Counting it as a boundary would file a gap over a stretch the
	// surrounding recording already holds.
	// The failed row sits inside the hole deliberately. Counted as a
	// boundary it would split one gap into two, so the count is what tells
	// the two behaviours apart.
	gaps := &fakeGaps{recordings: []store.Recording{
		{ID: 1, State: store.StateComplete, StartedAt: at(0), EndedAt: endedAt(time.Hour)},
		{ID: 2, State: store.StateFailed, StartedAt: at(90 * time.Minute), EndedAt: endedAt(90 * time.Minute)},
		{ID: 3, State: store.StateComplete, StartedAt: at(2 * time.Hour), EndedAt: endedAt(3 * time.Hour)},
	}}

	filed, err := Detect(gaps, aBroadcast())
	if err != nil {
		t.Fatalf("Detect() err = %v, want nil", err)
	}
	if len(filed) != 1 {
		t.Fatalf("Detect() filed %d gaps, want 1 unbroken hole", len(filed))
	}
	if filed[0].Start != time.Hour || filed[0].End != 2*time.Hour {
		t.Errorf("gap = [%v, %v], want [1h, 2h] undivided by the failed recording",
			filed[0].Start, filed[0].End)
	}
}

func TestDetect_FilesNothingWhenNothingWasCaptured(t *testing.T) {
	// A broadcast nothing captured is missing rather than gapped. It is a
	// candidate for a fetch, and there is no recording to attach a gap to.
	gaps := &fakeGaps{recordings: []store.Recording{
		{ID: 1, State: store.StateFailed, StartedAt: at(0)},
	}}

	filed, err := Detect(gaps, aBroadcast())
	if err != nil {
		t.Fatalf("Detect() err = %v, want nil", err)
	}
	if len(filed) != 0 {
		t.Errorf("Detect() filed %d gaps with nothing captured, want 0", len(filed))
	}
}

func TestDetect_FilesTheHoleAfterTheLastRecording(t *testing.T) {
	// A reboot at hour two of a six-hour broadcast leaves a complete-looking
	// two-hour recording: the crash recovery stamps its end, the sweep
	// finalizes it, and the day paints as captured live. Nothing compares
	// the last recording's end against the broadcast's, so four hours the
	// archive still holds are reported as covered.
	tests := []struct {
		name       string
		ended      *time.Time
		recordings []store.Recording
		wantStart  time.Duration
		wantEnd    time.Duration
		wantGap    bool
	}{
		{
			name:  "the recorder stopped four hours before the broadcast did",
			ended: endedAt(6 * time.Hour),
			recordings: []store.Recording{
				{ID: 1, State: store.StateComplete, StartedAt: at(0), EndedAt: endedAt(2 * time.Hour)},
			},
			wantStart: 2 * time.Hour,
			wantEnd:   6 * time.Hour,
			wantGap:   true,
		},
		{
			name:  "the recording is still running",
			ended: endedAt(6 * time.Hour),
			recordings: []store.Recording{
				{ID: 1, State: store.StateCapturing, StartedAt: at(0)},
			},
			wantGap: false,
		},
		{
			name:  "nobody recorded when the broadcast ended",
			ended: nil,
			recordings: []store.Recording{
				{ID: 1, State: store.StateComplete, StartedAt: at(0), EndedAt: endedAt(2 * time.Hour)},
			},
			wantGap: false,
		},
		{
			name:  "the recorder held on to the end",
			ended: endedAt(6 * time.Hour),
			recordings: []store.Recording{
				{ID: 1, State: store.StateComplete, StartedAt: at(0), EndedAt: endedAt(6 * time.Hour)},
			},
			wantGap: false,
		},
		{
			name:  "the recorder stopped a few seconds early",
			ended: endedAt(6 * time.Hour),
			recordings: []store.Recording{
				{ID: 1, State: store.StateComplete, StartedAt: at(0), EndedAt: endedAt(6*time.Hour - 3*time.Second)},
			},
			wantGap: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broadcast := aBroadcast()
			broadcast.EndedAt = tt.ended
			gaps := &fakeGaps{recordings: tt.recordings}

			filed, err := Detect(gaps, broadcast)
			if err != nil {
				t.Fatalf("Detect() err = %v, want nil", err)
			}
			if !tt.wantGap {
				if len(filed) != 0 {
					t.Errorf("Detect() filed %+v, want nothing", filed)
				}
				return
			}
			if len(filed) != 1 {
				t.Fatalf("Detect() filed %d gaps, want 1", len(filed))
			}
			if filed[0].Start != tt.wantStart || filed[0].End != tt.wantEnd {
				t.Errorf("gap = [%v, %v], want [%v, %v]",
					filed[0].Start, filed[0].End, tt.wantStart, tt.wantEnd)
			}
			if filed[0].Reason != ReasonEarlyStop {
				t.Errorf("Reason = %q, want %q", filed[0].Reason, ReasonEarlyStop)
			}
		})
	}
}

func TestDetect_FilesAShortfallWhenTheMediaIsShorterThanTheWallSpan(t *testing.T) {
	// streamlink drops the segments an ad replaced, so the file holds less
	// broadcast than the row's wall span claims. Holes between recordings are
	// the only thing derived from wall boundaries, which structurally cannot
	// see media missing from inside one file, and the day paints as captured
	// live while the archive still holds the rest.
	tests := []struct {
		name      string
		wall      time.Duration
		media     time.Duration
		wantSpans int
	}{
		{name: "twenty minutes of the capture never arrived", wall: 6 * time.Hour, media: 5*time.Hour + 40*time.Minute, wantSpans: 1},
		{name: "the media matches the wall span", wall: 6 * time.Hour, media: 6 * time.Hour, wantSpans: 0},
		{name: "a few seconds of container rounding", wall: 6 * time.Hour, media: 6*time.Hour - 20*time.Second, wantSpans: 0},
		{name: "nobody measured the media", wall: 6 * time.Hour, media: 0, wantSpans: 0},
		{name: "the media outruns the wall span", wall: 6 * time.Hour, media: 6*time.Hour + time.Minute, wantSpans: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gaps := &fakeGaps{recordings: []store.Recording{{
				ID: 1, State: store.StateComplete,
				StartedAt: at(0), EndedAt: endedAt(tt.wall),
				MediaDuration: tt.media,
			}}}

			filed, err := Detect(gaps, aBroadcast())
			if err != nil {
				t.Fatalf("Detect() err = %v, want nil", err)
			}
			if len(filed) != tt.wantSpans {
				t.Fatalf("Detect() filed %+v, want %d spans", filed, tt.wantSpans)
			}
			if tt.wantSpans == 0 {
				return
			}
			// The span covers the whole recording, because a duration says
			// how much is missing and never where it was.
			if filed[0].Start != 0 || filed[0].End != tt.wall {
				t.Errorf("shortfall = [%v, %v], want the whole recording [0, %v]",
					filed[0].Start, filed[0].End, tt.wall)
			}
			if filed[0].Reason != ReasonShortMedia {
				t.Errorf("Reason = %q, want %q", filed[0].Reason, ReasonShortMedia)
			}
		})
	}
}

func TestDetect_SkipsARecordingWithNoRecordedEnd(t *testing.T) {
	// A capture still running has no bound to measure a hole from, and
	// guessing one would file a gap that closes as soon as it ends.
	gaps := &fakeGaps{recordings: []store.Recording{
		{ID: 1, State: store.StateCapturing, StartedAt: at(0)},
		{ID: 2, State: store.StateComplete, StartedAt: at(2 * time.Hour), EndedAt: endedAt(3 * time.Hour)},
	}}

	filed, err := Detect(gaps, aBroadcast())
	if err != nil {
		t.Fatalf("Detect() err = %v, want nil", err)
	}
	if len(filed) != 0 {
		t.Errorf("Detect() filed %d gaps against an unbounded recording, want 0", len(filed))
	}
}

// ///////////////////////////////////////////////
// Section
// ///////////////////////////////////////////////

func TestSection_IndexesTheStoredCopysOwnTimeline(t *testing.T) {
	// yt-dlp indexes a download range from the stored copy's own t=0, which
	// is when the platform started recording rather than when the daemon
	// first saw the channel live. A range measured from the broadcast row's
	// start downloads a stretch the recorder already holds, files it stamped
	// at the wrong moment, and marks the hole patched for good.
	tests := []struct {
		name      string
		vodStart  *time.Time
		gap       store.Gap
		want      string
		wantError bool
	}{
		{
			name:     "the stored copy began two minutes before the broadcast row does",
			vodStart: new(at(-2 * time.Minute)),
			gap:      store.Gap{Start: time.Hour, End: 90 * time.Minute},
			want:     "*3720-5520",
		},
		{
			name:     "the two timelines agree",
			vodStart: new(at(0)),
			gap:      store.Gap{Start: 0, End: 33 * time.Minute},
			want:     "*0-1980",
		},
		{
			name:     "rounds down to the second",
			vodStart: new(at(0)),
			gap:      store.Gap{Start: 1500 * time.Millisecond, End: 2500 * time.Millisecond},
			want:     "*1-2",
		},
		{
			name:     "the hole opens before the stored copy does",
			vodStart: new(at(10 * time.Minute)),
			gap:      store.Gap{Start: 0, End: 33 * time.Minute},
			want:     "*0-1380",
		},
		{
			name:      "the hole closes before the stored copy begins",
			vodStart:  new(at(time.Hour)),
			gap:       store.Gap{Start: 0, End: 33 * time.Minute},
			wantError: true,
		},
		{
			name:      "nothing recorded where the stored copy starts",
			vodStart:  nil,
			gap:       store.Gap{Start: time.Hour, End: 90 * time.Minute},
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broadcast := aBroadcast()
			broadcast.VodStartedAt = tt.vodStart

			got, err := Section(broadcast, tt.gap)
			if tt.wantError {
				if err == nil {
					t.Fatalf("Section() = %q, err = nil, want a refusal", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Section() err = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("Section() = %q, want %q", got, tt.want)
			}
		})
	}
}
