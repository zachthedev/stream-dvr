package backfill

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Gaps reads a broadcast's recordings and files the holes between them.
type Gaps interface {
	RecordingsForBroadcast(broadcastID int64) ([]store.Recording, error)
	AddGap(recordingID int64, start, end time.Duration, reason string) (store.Gap, error)
}

// span is one hole, offset from the broadcast's start.
type span struct {
	start  time.Duration
	end    time.Duration
	reason string
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// Gap reasons, as stored.
const (
	// ReasonLateStart is coverage missing from the broadcast's start,
	// because the recorder joined after it began.
	ReasonLateStart = "late start"
	// ReasonReconnect is coverage missing between two recordings, because
	// a capture ended and the next poll started another.
	ReasonReconnect = "reconnect"
	// ReasonEarlyStop is coverage missing from the broadcast's end, because
	// the recorder stopped while the broadcast ran on. A reboot part way
	// through leaves a recording that looks whole, and this is the only
	// thing that says otherwise.
	ReasonEarlyStop = "early stop"
	// ReasonShortMedia is content missing from inside one recording, because
	// the recorder never received the segments an ad replaced.
	//
	// It is reported and never patched. A duration says how much is missing
	// and never where, so the span covers the whole recording, and
	// downloading that span would refetch the entire broadcast to recover
	// minutes.
	ReasonShortMedia = "short media"
)

// shortMediaFloor and shortMediaShare bound how far a recording's media may
// fall behind its wall span before the difference is content rather than
// container rounding.
//
// The larger of the two applies. A container reports a length to the frame,
// a capture that started or ended mid-segment loses a moment at each edge,
// and a re-encode shifts the total slightly. The proportional part is what
// keeps a six-hour recording from being reported for a rounding error the
// floor would let through on a ten-minute one.
const (
	shortMediaFloor = time.Minute
	shortMediaShare = 50 // one fiftieth, which is two percent
)

// minGap is the shortest hole worth recording.
//
// A poll interval means a recorder joins a stream some seconds after it
// starts, and a reconnect costs a moment. Filing those would fill the
// database with gaps nobody would patch and no archive could patch
// accurately, because a stored copy's own timeline is whole-second.
const minGap = 30 * time.Second

// ///////////////////////////////////////////////
// Detection
// ///////////////////////////////////////////////

// Detect files every hole in one broadcast's coverage.
//
// It is repeatable. The detector re-derives every gap from the recordings
// each time it runs, and the store's unique span is what turns a second
// pass into the same rows rather than duplicates of them.
//
// Only recordings that hold something count. A failed capture left no
// bytes, so a hole where one sits is not a hole between two recordings, it
// is part of the surrounding one.
func Detect(gaps Gaps, broadcast store.Broadcast) ([]store.Gap, error) {
	recordings, err := gaps.RecordingsForBroadcast(broadcast.ID)
	if err != nil {
		return nil, fmt.Errorf("reading recordings of broadcast %d: %w", broadcast.ID, err)
	}

	held := make([]store.Recording, 0, len(recordings))
	for _, recording := range recordings {
		if store.HoldsBytes(recording.State) {
			held = append(held, recording)
		}
	}
	if len(held) == 0 {
		// Nothing was captured, so the whole broadcast is missing rather
		// than gapped. That is a candidate for a fetch, not a patch, and
		// there is no recording to attach a gap to.
		return nil, nil
	}

	slices.SortFunc(held, func(a, b store.Recording) int {
		return a.StartedAt.Compare(b.StartedAt)
	})

	// Every gap attaches to the earliest recording, which is the row that
	// survives however many reconnects followed and the one a reader looks
	// at to ask what this broadcast is missing.
	anchor := held[0].ID

	// One span the store refuses does not hide the rest. The spans are
	// derived in order and the last of them is the early stop, the hole
	// nothing else can see, so abandoning the run on the first refusal
	// loses the ones behind it every pass and identically every time.
	var filed []store.Gap
	var refused []error
	for _, span := range spans(broadcast, held) {
		gap, err := gaps.AddGap(anchor, span.start, span.end, span.reason)
		if err != nil {
			refused = append(refused, fmt.Errorf("filing a %s gap in broadcast %d: %w",
				span.reason, broadcast.ID, err))
			continue
		}
		filed = append(filed, gap)
	}
	return filed, errors.Join(refused...)
}

// spans derives every hole in a sorted set of recordings.
func spans(broadcast store.Broadcast, held []store.Recording) []span {
	var found []span

	// A late start sits before the first recording begins, which is the
	// case no offset from a recording's own start can express.
	if lateBy := held[0].StartedAt.Sub(broadcast.StartedAt); lateBy >= minGap {
		found = append(found, span{start: 0, end: lateBy, reason: ReasonLateStart})
	}

	for i := 1; i < len(held); i++ {
		previous, current := held[i-1], held[i]
		if previous.EndedAt == nil {
			// Still running, or ended without recording when. Either way
			// there is no bound to measure a hole from.
			continue
		}
		if gap := current.StartedAt.Sub(*previous.EndedAt); gap >= minGap {
			found = append(found, span{
				start:  previous.EndedAt.Sub(broadcast.StartedAt),
				end:    current.StartedAt.Sub(broadcast.StartedAt),
				reason: ReasonReconnect,
			})
		}
	}

	// The recorder stopping before the broadcast did is the case nothing else
	// here can see. A reboot part way through leaves a recording the crash
	// recovery finishes and the sweep files, so the row reads whole and the
	// day paints as captured live while the archive still holds the rest.
	//
	// Both bounds have to be known. A recording still running has no end to
	// measure from, and a broadcast nobody closed has no end to measure to.
	found = append(found, shortfalls(broadcast, held)...)

	last := held[len(held)-1]
	if last.EndedAt == nil || broadcast.EndedAt == nil {
		return found
	}
	if stoppedEarlyBy := broadcast.EndedAt.Sub(*last.EndedAt); stoppedEarlyBy >= minGap {
		found = append(found, span{
			start:  last.EndedAt.Sub(broadcast.StartedAt),
			end:    broadcast.EndedAt.Sub(broadcast.StartedAt),
			reason: ReasonEarlyStop,
		})
	}
	return found
}

// shortfalls reports each recording holding less broadcast than its wall
// span claims.
//
// This is the only hole nothing else here can find. Every other span is
// derived from the boundaries between recordings, which structurally cannot
// see media missing from the middle of one file, and the recorder skips the
// segments an ad replaced without the row ever saying so.
//
// The span covers the whole recording because a duration says how much is
// missing and never where, and it is reported rather than patched for the
// same reason.
func shortfalls(broadcast store.Broadcast, held []store.Recording) []span {
	var found []span

	for _, recording := range held {
		// Zero is the column's "nobody measured this" value, and an unmeasured
		// recording is not a short one.
		if recording.MediaDuration <= 0 || recording.EndedAt == nil {
			continue
		}

		wall := recording.EndedAt.Sub(recording.StartedAt)
		if wall <= 0 {
			continue
		}
		if wall-recording.MediaDuration < max(shortMediaFloor, wall/shortMediaShare) {
			continue
		}

		// Offsets are measured from the broadcast's start, and a recording
		// can precede it: upgrading a tracker's rounded timestamp to the
		// platform's own moves the broadcast later than a recording
		// already linked to it. The hole is still the part of the
		// broadcast this file does not hold, so it starts where the
		// broadcast does.
		start := max(recording.StartedAt.Sub(broadcast.StartedAt), 0)
		end := recording.EndedAt.Sub(broadcast.StartedAt)
		if end <= start {
			continue
		}

		found = append(found, span{
			start:  start,
			end:    end,
			reason: ReasonShortMedia,
		})
	}
	return found
}

// ///////////////////////////////////////////////
// Patching
// ///////////////////////////////////////////////

// Section renders a gap as the range yt-dlp downloads.
//
// The form is "*start-end" in seconds from the start of the stored copy. A
// gap's own offsets run from the broadcast row's start, and the two origins
// are not the same instant: the platform starts recording at its own moment,
// and the row's start moves as better sources describe the broadcast. The
// conversion goes through wall clock, which is the only thing both timelines
// share.
//
// An unknown anchor is refused rather than assumed. A range indexed from the
// wrong origin downloads a stretch the recorder already holds, and the
// patcher marks the hole filled behind it.
func Section(broadcast store.Broadcast, gap store.Gap) (string, error) {
	start, end, err := vodRange(broadcast, gap)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("*%d-%d", int64(start.Seconds()), int64(end.Seconds())), nil
}

// vodRange places a gap on the stored copy's own timeline.
//
// The two origins are not the same instant, and wall clock is the only thing
// both share. Every reader that has to reason about where a hole sits in the
// stored copy goes through here, so a range that is downloaded and a range
// that is checked against a mute cannot describe different stretches.
func vodRange(broadcast store.Broadcast, gap store.Gap) (time.Duration, time.Duration, error) {
	if broadcast.VodStartedAt == nil {
		return 0, 0, fmt.Errorf("%w: broadcast %d", ErrNoAnchor, broadcast.ID)
	}

	start := broadcast.StartedAt.Add(gap.Start).Sub(*broadcast.VodStartedAt)
	end := broadcast.StartedAt.Add(gap.End).Sub(*broadcast.VodStartedAt)
	if end <= 0 {
		return 0, 0, fmt.Errorf("the hole closes before the stored copy of broadcast %d begins",
			broadcast.ID)
	}
	// A hole that opens before the stored copy does is only recoverable from
	// where the copy starts, which is exactly offset zero.
	return max(start, 0), end, nil
}

// mutedWithin reports the first stretch the platform silenced that overlaps
// a range of the stored copy.
//
// Both are half-open, so a mute that ends exactly where a hole opens is not
// an overlap. Nothing is inferred from a nil list: a platform nobody could
// ask has silenced nothing as far as this is concerned, and the alternative
// is refusing every patch on every machine with no platform session.
func mutedWithin(muted []store.MutedSpan, start, end time.Duration) (store.MutedSpan, bool) {
	for _, span := range muted {
		if span.Offset < end && start < span.Offset+span.Duration {
			return span, true
		}
	}
	return store.MutedSpan{}, false
}
