package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/providers"
	"zach.tools/go/stream-dvr/internal/record"
	"zach.tools/go/stream-dvr/internal/space"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// CycleResult reports what one watch cycle did.
type CycleResult struct {
	// Live reports whether the channel was broadcasting.
	Live bool
	// Refused reports that the budget declined the recording, which is
	// the only path where a broadcast is knowingly missed.
	Refused bool
	// RecordingID identifies the recording, when one was made.
	RecordingID int64
	// Bytes is what the capture wrote.
	Bytes int64
	// Duration is how long the capture ran.
	Duration time.Duration
	// TooShort reports a recording below the configured minimum, which is
	// kept on disk but not named into the library.
	TooShort bool
	// StoppedForSpace reports a capture ended early because the library
	// reached its limit mid-broadcast.
	StoppedForSpace bool
	// Stalled reports a capture ended because its output stopped growing,
	// which is a process that is alive and no longer recording anything.
	Stalled bool
}

// LiveBroadcast is what a platform says is on air for a channel now.
//
// It carries what identifies the broadcast as well as what describes it,
// because the caller has to decide whether this is the same broadcast it is
// capturing before it takes the title.
type LiveBroadcast struct {
	// StreamID is the live session the platform names, empty when it named
	// none.
	StreamID string
	// StartedAt is when the platform says this session began.
	StartedAt time.Time
	// Title is the broadcast title as the streamer wrote it. Untrusted.
	Title string
	// Category is the game or section it is filed under.
	Category string
}

// expectedDuration is how long a broadcast is assumed to run when sizing
// the admission check. It is a planning figure, not a limit: a longer
// broadcast is still recorded, and the watermark check stops it if the
// library actually fills.
const expectedDuration = 6 * time.Hour

// ///////////////////////////////////////////////
// Space watermark
// ///////////////////////////////////////////////

// spaceCheckInterval is how often a running capture rechecks the budget.
const spaceCheckInterval = 30 * time.Second

// maxFailureBackoff caps how far a failing channel's poll is stretched. It
// is short enough that a channel whose failure clears is recording again
// well inside a broadcast.
const maxFailureBackoff = 15 * time.Minute

// broadcastStartTimeout bounds the lookup of a broadcast's real start.
//
// The answer only improves a timestamp, so nothing waits long for it. The
// capture is the urgent half of this cycle, and every second here is a
// second of the broadcast nobody is recording.
const broadcastStartTimeout = 8 * time.Second

// maxJoinLateness bounds how far back a resolved start may move an anchor.
//
// A channel that never goes offline reports a start days old, and the hole
// filed from it spans the whole stretch. The patcher sizes a download from
// that span, so an unbounded one estimates more than any library holds,
// refuses the broadcast entire, and takes every real gap inside it down with
// the refusal. Past this the answer is discarded and the moment the poll saw
// the channel stands, which is what a recorder with no answer at all uses.
const maxJoinLateness = 6 * time.Hour

// captureStallLimit is how long a capture's output may go without growing
// before the capture is ended.
//
// A stream stalls legitimately: a platform hiccup, a stretch the recorder
// skips, a reconnect. None of those lasts ten minutes without a byte
// reaching the file. What does is a write blocked on a volume that stopped
// answering, or a hang inside the tool, and neither ends on its own: the
// process is alive, so nothing cancels it, RunCycle never returns, and that
// channel is never polled again for the life of the daemon.
const captureStallLimit = 10 * time.Minute

// spaceBlindLimit is how many consecutive unreadable disk checks turn a
// warning into a report. The watermark is the only thing standing between a
// long broadcast and a full volume, and one that cannot read the disk is
// guarding nothing.
const spaceBlindLimit = 5

// refusalLimit is how many consecutive refusals on one channel turn into a
// report of their own.
//
// The credential check asks Twitch's validation endpoint, and the recorder
// asks its playback endpoint. The two are different judges: a token issued
// to another application validates cleanly and offers no streams at all. In
// that state the recorder captures nothing indefinitely while the hourly
// check confirms the credential is healthy, so the refusals themselves are
// the only evidence and have to say something.
const refusalLimit = 5

// sweepFailureLimit is how many sweeps in a row one recording may fail
// before it is reported as stuck. A warning every quarter of an hour reads
// exactly like the routine waiting-on-a-lock message, so a recording that
// will never finish has to say something else.
//
// It governs the failures nothing abandons on its own: a stage that stops
// retrying a recording says so in its error and is reported the moment it
// does.
const sweepFailureLimit = 3

// sameBroadcastWindow is how much later than a capture's own start a
// platform's live session may begin and still be taken for the same one.
//
// It is the guard that works when the session id is unknown, which is the
// ordinary case here: the id and the title arrive in one metadata block, so
// a block empty enough to need this lookup carried no id either. A channel
// that ended one broadcast and opened another reports a start well after
// the one being captured, and its title must not be written onto it.
const sameBroadcastWindow = 15 * time.Minute

// metadataLookupLimit is how many lookups in a row may answer nothing
// before the platform stops being asked for the rest of the capture.
//
// An API that cannot say what is on air, because nobody authorized it or
// its session was revoked, answers the same way every tick. The recovery
// round fills the title in afterwards either way, so the cost of giving up
// here is a title that arrives later rather than one that never arrives.
const metadataLookupLimit = 5

// ///////////////////////////////////////////////
// Watch loop
// ///////////////////////////////////////////////

// Watch polls one channel until ctx is cancelled.
//
// The loop is deliberately thin. Everything that can go wrong lives in
// RunCycle, which takes no timers and can be tested directly.
func (d *Daemon) Watch(ctx context.Context, entry config.Channel, channel store.Channel) {
	interval := d.config.Capture.PollInterval.Std()
	failures := 0
	// Counted per channel and held here rather than on the daemon, because
	// this loop is the only reader and there is exactly one per channel.
	refusals := 0

	// One timer for the whole loop, stopped on the way out, so a cancelled
	// watch leaves nothing counting down behind it.
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		if _, err := d.RunCycle(ctx, entry, channel); err != nil {
			if ctx.Err() != nil {
				return
			}
			// A failed cycle must not end the watch. A channel that errors
			// once and is never polled again is the silent failure this
			// tool exists to prevent.
			failures++
			// A refused credential yields no streams at all, so it costs
			// every channel every poll until it is replaced. Confirming it
			// now rather than at the hourly tick shortens that from an hour
			// to one poll interval. The check itself decides, and this only
			// prompts it. A floor keeps every channel from asking.
			if errors.Is(err, record.ErrUnauthorized) {
				refusals++
				d.recheckCredential(ctx)
				d.reportRefusals(ctx, entry, refusals)
			}
			d.logger.ErrorContext(ctx, "watch cycle failed",
				slog.String("channel", entry.Name),
				slog.Int("consecutive_failures", failures),
				slog.Any("error", err))
			// Only the opening failure of a run is reported. An expired
			// token fails every poll for as long as it takes the operator
			// to notice, and one notification per outage is what carries
			// that. A hundred an hour is what buries it.
			if failures == 1 {
				d.notify(ctx, Event{
					Kind: EventFailure, Channel: entry.Name,
					Detail: err.Error(),
				})
			}
		} else {
			failures, refusals = 0, 0
		}

		timer.Reset(pollDelay(interval, failures))
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// reportRefusals tells the operator about a credential the platform keeps
// refusing for playback, whatever validation says about it.
//
// Reported as the count crosses the line and not on every poll after, so the
// escalation does not become the flood the per-outage failure report already
// avoids.
func (d *Daemon) reportRefusals(ctx context.Context, entry config.Channel, refusals int) {
	if refusals != refusalLimit {
		return
	}

	d.logger.ErrorContext(ctx, "the platform keeps refusing the credential for playback, "+
		"so nothing is being recorded on this channel",
		slog.String("channel", entry.Name),
		slog.Int("consecutive_refusals", refusals))
	d.notify(ctx, Event{
		Kind: EventCredentialDead, Channel: entry.Name,
		Detail: fmt.Sprintf("the platform refused the credential on %d polls in a row, so this channel is "+
			"recording nothing; a token that validates can still be one the platform will not play, "+
			"so run 'stream-dvr auth twitch' with the browser cookie", refusals),
	})
}

// pollDelay stretches the poll while a channel keeps failing, doubling per
// consecutive failure up to the backoff ceiling.
//
// A probe that fails once usually keeps failing for a while, and running it
// at full cadence re-runs a subprocess and an outbound notification for a
// channel that is not going to answer. An interval already longer than the
// ceiling is left alone: the operator chose a slower poll than the backoff
// would impose.
func pollDelay(interval time.Duration, failures int) time.Duration {
	ceiling := max(interval, maxFailureBackoff)
	delay := interval

	for range max(failures-1, 0) {
		if delay >= ceiling {
			return ceiling
		}
		delay *= 2
	}
	return min(delay, ceiling)
}

// RunCycle performs one probe and, if the channel is live, one capture.
//
// It returns when the broadcast ends, so a cycle can last hours.
func (d *Daemon) RunCycle(ctx context.Context, entry config.Channel, channel store.Channel) (CycleResult, error) {
	probe, err := d.engine.Probe(ctx, channelURL(entry))
	// The probe is where reachability is decided, because it is the only
	// step that asks the platform anything. Everything after it can fail
	// over a disk or a database while the network is perfectly fine.
	//
	// A cancelled probe says nothing about the platform: it failed because
	// the recorder is stopping, and counting that as an outage would have
	// every shutdown queue a round for the next start to run.
	//
	// A refused credential is the platform answering, and answering with a
	// refusal. Recovery fetches public archives and carries no credential at
	// all, so reading that as unreachable would disable the one mechanism
	// still able to get the content, for exactly as long as the token stayed
	// dead.
	if ctx.Err() == nil {
		answered := err == nil || errors.Is(err, record.ErrUnauthorized)
		d.noteProbe(channel.ID, entry.Platform, answered)
	}
	if err != nil {
		return CycleResult{}, err
	}
	if !probe.Live {
		// The broadcast is over, so the next one is anchored, estimated,
		// and reported from scratch.
		d.closeBroadcast(ctx, entry, d.endSession(channel.ID))
		return CycleResult{}, nil
	}

	result := CycleResult{Live: true}
	started := d.now().UTC()

	// The display name arrives with the probe, so a channel recorded before
	// its name was known gets it on the next broadcast.
	if probe.Metadata.Author != "" {
		if channel, err = d.store.UpsertChannel(entry.Platform, entry.Name, probe.Metadata.Author); err != nil {
			return result, err
		}
	}

	// The broadcast is anchored to when this channel was first seen live
	// rather than to this poll, so every poll of one session reaches the
	// row the first one created. The recording still keeps its own start:
	// that is when capture began, and it is what names the file.
	// The probe reports the live session's id, which is a different
	// namespace from the video id the archive publishes afterward. Storing
	// it as the stream id is what lets the VOD listing find this row instead
	// of inserting a second one for the same broadcast.
	broadcast, err := d.store.UpsertBroadcast(store.Broadcast{
		ChannelID: channel.ID,
		StreamID:  probe.Metadata.ID,
		StartedAt: d.anchorFor(ctx, entry, channel.ID, probe.Metadata.ID, started),
		Title:     probe.Metadata.Title,
		Category:  probe.Metadata.Category,
		Source:    store.SourceLive,
	})
	if err != nil {
		return result, err
	}
	d.noteBroadcast(channel.ID, broadcast.ID)

	// A capture of this broadcast that the watermark stopped before it was
	// long enough to keep left a failed row and an orphan file. Starting
	// another produces one more of each per poll for the rest of the
	// broadcast, and the operator was already told the library is at its
	// limit. A capture that ran long enough to keep is a different case: it
	// reached the library, and room made afterwards records the rest.
	if d.haltedForSpace(channel.ID, broadcast.ID) {
		result.Refused = true
		return result, nil
	}

	if err := d.admit(channel.ID, expectedDuration); err != nil {
		var refusal *space.RefusalError
		if !errors.As(err, &refusal) {
			return result, err
		}
		// Nothing is deleted to make room. The broadcast is missed and the
		// operator is told, loudly, but only once for this broadcast.
		result.Refused = true
		if d.firstRefusal(channel.ID, broadcast.ID) {
			d.logger.ErrorContext(ctx, "recording refused, library is full",
				slog.String("channel", entry.Name), slog.Any("error", refusal))
			d.notify(ctx, Event{
				Kind: EventLibraryFull, Channel: entry.Name, Title: probe.Metadata.Title,
				Detail: refusal.Error(),
			})
		}
		return result, nil
	}
	// A broadcast that got in ends the latch. Filling the library a second
	// time costs a second recording, and the operator hears about it only
	// if the refusal that follows is reported.
	d.clearRefusal(channel.ID)

	return d.capture(ctx, entry, channel, broadcast, started, result)
}

// anchorFor returns the start the broadcast's row is anchored to.
//
// A channel already broadcasting when the recorder first polls it began
// before this cycle, and seen is only when it was noticed. Asking the
// platform is the one route to the real moment, and it is what keeps the
// row, the sidecar, and every gap offset right from the first poll rather
// than from a correction hours later.
//
// The lookup runs only where there is no session yet, so it costs one
// request per session rather than one per poll. A channel reading offline
// between polls ends its session and pays again on the next one, which is
// the price of treating a flap as a new broadcast. A session opened
// meanwhile wins: sessionStart returns the anchor already agreed, and the
// answer resolved here is dropped rather than moving a row mid-broadcast.
//
// The answer is bounded on both sides before it is used. A recorder cannot
// see a broadcast before it begins, so a time at or after seen is a clock
// disagreeing rather than a better reading, and one further back than
// maxJoinLateness is a channel that never goes offline rather than a
// broadcast this recorder joined. Both leave seen standing, and both must be
// refused here: the anchor is latched into the session and reused for the
// rest of the broadcast, so a value rejected further down would wedge the
// channel instead of falling back.
func (d *Daemon) anchorFor(ctx context.Context, entry config.Channel,
	channelID int64, streamID string, seen time.Time,
) time.Time {
	if anchor, ok := d.sessionAnchor(channelID, streamID); ok {
		return anchor
	}

	started := seen
	if d.broadcastStart != nil {
		lookup, cancel := context.WithTimeout(ctx, broadcastStartTimeout)
		defer cancel()

		resolved, ok := d.broadcastStart(lookup, channelURL(entry), streamID)
		switch {
		case ok && resolved.Before(seen) && seen.Sub(resolved) <= maxJoinLateness:
			started = resolved.UTC()
			d.logger.InfoContext(ctx, "joined a broadcast already in progress",
				slog.String("channel", entry.Name),
				slog.Duration("missed", seen.Sub(started)))
		case ok:
			d.logger.WarnContext(ctx, "ignored an implausible broadcast start",
				slog.String("channel", entry.Name),
				slog.Time("reported", resolved.UTC()))
		}
	}
	return d.sessionStart(channelID, streamID, started)
}

// closeBroadcast stamps the end on the broadcast a finished session was
// writing to.
//
// The offline poll is the only moment the daemon learns a broadcast is over,
// and the end is what the settle window and gap patching both wait for. A
// session that never reached an upsert has nothing to close, which is the
// ordinary state of a channel that was already offline when the daemon
// started.
//
// A failure here is logged rather than returned: the broadcast is over
// either way, and discovery writes the same end from the VOD's duration.
func (d *Daemon) closeBroadcast(ctx context.Context, entry config.Channel, session *liveSession) {
	if session == nil || session.broadcastID == 0 {
		return
	}
	if err := d.store.SetBroadcastEnd(session.broadcastID, d.now().UTC()); err != nil {
		d.logger.WarnContext(ctx, "recording the broadcast's end failed",
			slog.String("channel", entry.Name),
			slog.Int64("broadcast", session.broadcastID),
			slog.Any("error", err))
	}
}

// capture records a live broadcast and finalizes it.
func (d *Daemon) capture(ctx context.Context, entry config.Channel, channel store.Channel,
	broadcast store.Broadcast, started time.Time, result CycleResult,
) (CycleResult, error) {
	// A slot is held for the whole capture, so the concurrency cap counts
	// recordings in flight rather than recordings started.
	if err := d.acquireSlot(ctx); err != nil {
		return result, err
	}
	defer d.releaseSlot()

	relPath := filepath.Join(paths.IncomingDirName, record.CaptureName(entry.Platform, entry.Name, started))

	recording, err := d.store.CreateRecording(store.Recording{
		ChannelID:   channel.ID,
		BroadcastID: &broadcast.ID,
		Path:        relPath,
		State:       store.StateCapturing,
		Origin:      store.OriginLive,
		StartedAt:   started,
	})
	if err != nil {
		return result, err
	}
	result.RecordingID = recording.ID

	// Held for as long as an engine is writing to this row, so the sweep can
	// tell it from a row a crash stranded. The two are identical in the
	// database, and finalizing one that is still recording would remux a
	// file that is still growing.
	d.noteCapturing(recording.ID)
	defer d.forgetCapturing(recording.ID)

	d.notify(ctx, Event{
		Kind: EventRecordingStarted, Channel: entry.Name, Title: broadcast.Title,
		Detail: relPath,
	})

	// Cancelling this context ends the capture. Both background watchers
	// below can trigger it, and both must be stopped before the result is
	// judged.
	captureCtx, stopCapture := context.WithCancel(ctx)
	defer stopCapture()

	// Metadata is polled on its own schedule for the life of the capture,
	// so a title set after the broadcast opened is still recorded and a
	// title that was unavailable at the start can still arrive.
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		d.pollMetadata(captureCtx, entry, broadcast, probeInterval(d.config))
	}()

	// A broadcast can run for hours, so a library that had room at the start
	// need not have room at the end, and a capture that was writing at the
	// start need not still be writing.
	var stoppedForSpace, stalled atomic.Bool
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		d.watchCapture(captureCtx, entry, relPath, &stoppedForSpace, &stalled, stopCapture)
	}()

	captureResult, captureErr := d.engine.Capture(captureCtx, record.Request{
		URL:       channelURL(entry),
		Qualities: d.config.QualityFor(entry),
		Output:    d.library.RelPath(relPath),
		LogPath:   paths.LogCapture.Path(d.library.StateDir()),
	})

	stopCapture()
	<-polled
	<-watched

	result.Bytes = captureResult.Bytes
	result.Duration = captureResult.Duration()
	result.StoppedForSpace = stoppedForSpace.Load()
	result.Stalled = stalled.Load()

	// A capture this daemon ended is complete as far as it goes. It is
	// deliberately not a failure: a whole three-hour recording beats a
	// corrupt four-hour one, and the bytes are worth naming and keeping.
	if result.StoppedForSpace || result.Stalled {
		captureErr = nil
	}

	// Capture being over does not make the recording complete: it still has
	// to be remuxed, named, and moved. Marking it complete here and
	// finalizing afterwards means every failure in between leaves a row
	// claiming the recording is in the library when it is sitting in
	// incoming/, outside the sweep, counted as covered on the calendar.
	// Only the organizer knows when it is really done.
	state := store.StateAwaitingFinalize
	switch {
	case captureErr != nil:
		state = store.StateFailed
	case captureResult.Duration() < d.config.Capture.MinDuration.Std():
		// Short recordings are kept on disk and left out of the library.
		// Nothing here deletes them.
		state = store.StateFailed
		result.TooShort = true
	}

	if result.StoppedForSpace && result.TooShort {
		d.noteSpaceHalt(channel.ID, broadcast.ID)
	}

	// A size nobody could read is about to be written down as zero. Anything
	// that reaches the library is sized again when it lands there, but a
	// failed capture never gets that far, so this line is the only account
	// of a file the budget will not see.
	if captureResult.SizeUnknown {
		d.logger.WarnContext(ctx, "could not measure what the capture wrote",
			slog.String("channel", entry.Name), slog.String("path", relPath))
	}

	// Never before the start. Both timestamps are wall clock and the store
	// refuses an end that precedes its start, so a backward step leaves the
	// row stuck capturing with its file in incoming and nothing to move it.
	// A clamped end costs an understated duration; the alternative costs the
	// recording.
	if err := d.store.FinishRecording(recording.ID, state, relPath,
		captureResult.Bytes, captureResult.Duration(), laterOf(d.now().UTC(), started)); err != nil {
		return result, err
	}

	if captureErr != nil {
		d.notify(ctx, Event{
			Kind: EventFailure, Channel: entry.Name, Title: broadcast.Title,
			Detail: captureErr.Error(),
		})
		return result, fmt.Errorf("capturing %s: %w", entry.Name, captureErr)
	}
	if result.TooShort {
		d.logger.InfoContext(ctx, "recording below the minimum length, not organized",
			slog.String("channel", entry.Name), slog.Duration("duration", captureResult.Duration()))
		return result, nil
	}

	// Finalizing shells out to ffmpeg, so a cancelled context fails it
	// immediately. The row already says the work is outstanding, so leaving
	// it for the next start's sweep beats a guaranteed failure and the
	// notification that would come with it on every shutdown.
	if ctx.Err() != nil {
		d.logger.InfoContext(ctx, "shutting down, leaving the recording for the next sweep",
			slog.String("channel", entry.Name), slog.String("path", relPath))
		return result, ctx.Err()
	}

	// A non-zero exit means the engine stopped for a reason of its own
	// rather than because the broadcast ended. The bytes are worth keeping
	// and the recording is still organized, but a capture that quit two
	// hours into a six-hour broadcast and was filed as complete leaves the
	// operator with no sign that four hours are missing.
	// A stalled capture reports here too, because the broadcast really did
	// outlive it and the hole is real. That is the opposite of a capture the
	// watermark stopped, where the library is full and there is nowhere to
	// put what is missing.
	if (captureResult.ExitCode != 0 || result.Stalled) && !result.StoppedForSpace {
		d.reportEarlyStop(ctx, entry, broadcast, result, captureResult)
	}

	return d.finalize(ctx, entry, broadcast, result)
}

// reportEarlyStop tells the operator about a capture the engine ended on
// its own, and records the hole it left when the broadcast outlived it.
func (d *Daemon) reportEarlyStop(ctx context.Context, entry config.Channel, broadcast store.Broadcast,
	result CycleResult, captureResult record.Result,
) {
	d.logger.ErrorContext(ctx, "capture ended before the broadcast did",
		slog.String("channel", entry.Name),
		slog.Int("exit_code", captureResult.ExitCode),
		slog.Duration("recorded", result.Duration))
	d.notify(ctx, Event{
		Kind: EventFailure, Channel: entry.Name, Title: broadcast.Title,
		Detail: fmt.Sprintf("the capture engine exited with status %d after %s, before the broadcast ended",
			captureResult.ExitCode, roundDuration(result.Duration)),
	})

	// The hole only has an end once the broadcast is known to have outlived
	// the capture. A channel that positively answers offline stopped when the
	// capture did, and nothing is missing.
	//
	// A probe that could not answer proves nothing about the broadcast, and
	// the capture's own non-zero exit is already evidence it was interrupted,
	// so the hole is filed. The two are told apart deliberately: a credential
	// that dies mid-capture fails the capture and this probe alike, and
	// reading that as "the channel went offline" is the conflation
	// internal/record names as skipping a broadcast silently.
	if probe, err := d.engine.Probe(ctx, channelURL(entry)); err == nil && !probe.Live {
		return
	}

	// Both bounds are offsets from the broadcast, which is what a gap means
	// everywhere else: the store's own contract, the detector, and the
	// patcher that renders one into a download range all read them that way.
	// This cycle's poll only coincides with the broadcast's start on a
	// session's first capture.
	start := captureResult.EndedAt.Sub(broadcast.StartedAt)
	end := d.now().UTC().Sub(broadcast.StartedAt)
	if start < 0 || end <= start {
		return
	}

	if _, err := d.store.AddGap(d.gapAnchor(broadcast.ID, result.RecordingID), start, end,
		"the capture engine stopped while the broadcast was still running"); err != nil {
		d.logger.WarnContext(ctx, "recording the gap failed",
			slog.String("channel", entry.Name), slog.Any("error", err))
	}
}

// gapAnchor returns the recording a broadcast's holes hang off.
//
// It is the earliest recording holding bytes, which is the rule the detector
// applies, so one hole is described by one row rather than by two that
// disagree. A reconnect makes the capture that just ended a later row, and a
// hole filed against it is a second account of what the earliest row already
// names.
//
// The fallback is the capture that just ended. A read that fails leaves the
// hole recorded somewhere rather than lost, which is the whole point of
// filing it.
func (d *Daemon) gapAnchor(broadcastID, fallback int64) int64 {
	recordings, err := d.store.RecordingsForBroadcast(broadcastID)
	if err != nil {
		return fallback
	}
	for _, recording := range recordings {
		if store.HoldsBytes(recording.State) {
			return recording.ID
		}
	}
	return fallback
}

// finalize organizes a completed recording into the library.
func (d *Daemon) finalize(ctx context.Context, entry config.Channel,
	broadcast store.Broadcast, result CycleResult,
) (CycleResult, error) {
	outcome, err := d.finalizer.Finalize(ctx, result.RecordingID)
	if err != nil {
		if ctx.Err() != nil {
			// Shutting down killed the ffmpeg this was waiting on. The row
			// still says the work is outstanding, so the next start's sweep
			// finishes it. Watch reads the cancellation and stops without
			// reporting anything, which is what an orderly stop deserves.
			d.logger.InfoContext(ctx, "shutting down, leaving the recording for the next sweep",
				slog.String("channel", entry.Name), slog.Int64("recording", result.RecordingID))
			return result, ctx.Err()
		}
		d.notify(ctx, Event{
			Kind: EventFailure, Channel: entry.Name, Title: broadcast.Title,
			Detail: err.Error(),
		})
		return result, err
	}

	switch {
	case outcome.Parked && outcome.Locked:
		// The recording is intact under its capture name and another
		// program is reading it, most often a backup agent. The sweeper
		// retries it, so this is a delay rather than a failure.
		d.logger.WarnContext(ctx, "recording waiting on another program to release the file",
			slog.String("channel", entry.Name),
			slog.String("path", outcome.Path))
	case outcome.Parked:
		// The recording is intact under its capture name. The sweeper
		// retries it, so this is a delay rather than a failure.
		d.logger.WarnContext(ctx, "recording waiting on a usable name",
			slog.String("channel", entry.Name),
			slog.String("reason", outcome.Reason),
			slog.Any("missing", outcome.Missing),
			slog.String("path", outcome.Path))
	case len(outcome.Fallbacks) > 0:
		d.logger.WarnContext(ctx, "recording named using a fallback",
			slog.String("channel", entry.Name),
			slog.Any("fallbacks", outcome.Fallbacks),
			slog.String("path", outcome.Path))
	default:
		d.logger.InfoContext(ctx, "recording complete",
			slog.String("channel", entry.Name), slog.String("path", outcome.Path))
	}
	return result, nil
}

// watchCapture ends a capture that is about to breach a limit, or one that
// has stopped writing.
//
// Both jobs run on one ticker because both are answered by looking at the
// same disk on the same cadence, and a second ticker would double the work
// to ask half the question.
//
// Letting a recording run until the disk is full corrupts it and takes
// everything else on the volume down with it. Stopping at the watermark
// costs the tail of one broadcast and keeps the rest. A capture that has
// stopped writing costs more than a tail: nothing else ever cancels it, so
// the cycle never returns and the channel is never polled again.
func (d *Daemon) watchCapture(ctx context.Context, entry config.Channel, relPath string,
	stopped, stalled *atomic.Bool, stop context.CancelFunc,
) {
	warned := false
	blind := 0

	// Counted in ticks rather than against a clock, because the cadence is
	// what this loop actually has.
	quiet, quietLimit := 0, max(int(d.captureStall/d.spaceInterval), 1)
	written := int64(-1)

	ticker := time.NewTicker(d.spaceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if size, known := d.captureSize(relPath); known {
			if size > written {
				quiet, written = 0, size
			} else {
				quiet++
			}
			if quiet >= quietLimit {
				stalled.Store(true)
				d.reportStalledCapture(ctx, entry, relPath, written)
				stop()
				return
			}
		}

		usage, err := d.usage()
		if err != nil {
			blind++
			d.reportBlindWatermark(ctx, entry, blind, err)
			continue
		}
		blind = 0

		switch space.Watch(d.limits(), usage) {
		case space.LevelCritical:
			stopped.Store(true)
			d.logger.ErrorContext(ctx, "stopping capture, library is at its limit",
				slog.String("channel", entry.Name))
			d.notify(ctx, Event{
				Kind: EventLibraryFull, Channel: entry.Name,
				Detail: "capture stopped early: the library reached its limit mid-broadcast",
			})
			stop()
			return
		case space.LevelLow:
			if !warned {
				warned = true
				d.logger.WarnContext(ctx, "library is running low",
					slog.String("channel", entry.Name))
				d.notify(ctx, Event{
					Kind: EventLibraryFull, Channel: entry.Name,
					Detail: "library is running low; recording continues for now",
				})
			}
		case space.LevelOK:
		}
	}
}

// captureSize reports how much a capture has written, and whether the answer
// is known.
//
// An output the engine has not opened yet holds nothing, which is the
// ordinary opening moment of every capture and is a real answer. A stat that
// fails for any other reason is not: the file may be growing perfectly well
// behind a permission error, and judging a capture on that would end a
// recording over a question that was never answered.
func (d *Daemon) captureSize(relPath string) (int64, bool) {
	info, err := os.Stat(d.library.RelPath(relPath))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return 0, true
	case err != nil:
		return 0, false
	default:
		return info.Size(), true
	}
}

// reportStalledCapture tells the operator about a capture that stopped
// writing, which is the one capture failure nothing else surfaces.
func (d *Daemon) reportStalledCapture(ctx context.Context, entry config.Channel,
	relPath string, written int64,
) {
	d.logger.ErrorContext(ctx, "stopping capture, it has not written anything for a long time",
		slog.String("channel", entry.Name),
		slog.String("path", relPath),
		slog.Duration("quiet_for", d.captureStall))
	d.notify(ctx, Event{
		Kind: EventFailure, Channel: entry.Name,
		Detail: fmt.Sprintf("the capture stopped writing for %s and was ended at %s; "+
			"the broadcast is picked up again on the next poll",
			roundDuration(d.captureStall), config.Size(written)),
	})
}

// reportBlindWatermark records a disk check the watermark could not make,
// and reports it once the capture has run unchecked for long enough that
// the operator has to know.
//
// The capture keeps running. Stopping it would trade the rest of a
// broadcast, which is certain, against a volume that may never fill, and
// nothing else here discards a recording it could keep. What the operator
// gets instead is the fact that the only automatic limit on the recording
// is off.
func (d *Daemon) reportBlindWatermark(ctx context.Context, entry config.Channel, blind int, err error) {
	if blind < spaceBlindLimit {
		d.logger.WarnContext(ctx, "could not read disk usage during capture",
			slog.String("channel", entry.Name),
			slog.Int("consecutive_failures", blind),
			slog.Any("error", err))
		return
	}

	d.logger.ErrorContext(ctx, "the disk limit is unenforced, the capture is running unchecked",
		slog.String("channel", entry.Name),
		slog.Int("consecutive_failures", blind),
		slog.Any("error", err))
	// Reported as it crosses the line and not on every check after, so the
	// escalation does not become the flood the warning is.
	if blind == spaceBlindLimit {
		d.notify(ctx, Event{
			Kind: EventFailure, Channel: entry.Name,
			Detail: fmt.Sprintf("the library's disk usage has been unreadable across %d checks, so nothing is stopping this recording filling the volume: %v",
				blind, err),
		})
	}
}

// ///////////////////////////////////////////////
// Metadata polling
// ///////////////////////////////////////////////

// pollMetadata records title and category changes for the life of a
// capture.
//
// It runs independently of the capture process. That separation is the
// whole point: capture writes bytes under a name that needs no metadata,
// and this fills the metadata in as it becomes available.
func (d *Daemon) pollMetadata(ctx context.Context, entry config.Channel,
	broadcast store.Broadcast, interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Counted so a run of failures is measurable in the log. Discarding the
	// error entirely is what left a two-hour capture parked with nothing
	// anywhere to say why.
	failures := 0
	// Consecutive lookups that answered nothing. An API that cannot say
	// what is on air will not start being able to mid-broadcast, and this
	// runs every tick for the length of the capture.
	misses := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		probe, err := d.engine.Probe(ctx, channelURL(entry))
		if err != nil {
			// A cancelled probe is the shutdown rather than a failure, and
			// there is nothing left to poll for.
			if ctx.Err() != nil {
				return
			}
			failures++
			d.logger.WarnContext(ctx, "could not read what a broadcast is titled",
				slog.String("channel", entry.Name),
				slog.Int("consecutive_failures", failures),
				slog.Any("error", err))
			continue
		}
		// Reset on any answered probe. An offline reading is an answer, and
		// leaving the count standing over one would report the next run of
		// failures as a continuation of a run that had already ended.
		failures = 0
		if !probe.Live {
			continue
		}

		// A probe can resolve streams and still carry nothing. The download
		// tool fetches metadata through a separate request and suppresses
		// every failure of it, so an empty block is reported exactly like a
		// broadcast that has no title, with a success status and no error.
		title, category := probe.Metadata.Title, probe.Metadata.Category
		if title == "" && category == "" {
			if misses < metadataLookupLimit {
				title, category = d.describeBroadcast(ctx, entry, broadcast)
			}
			if title == "" && category == "" {
				misses++
				// Said once, because this is the state that leaves a
				// finished recording sitting unnamed, and nothing else
				// reports it: both sources answered without failing.
				if misses == 1 {
					d.logger.WarnContext(ctx, "no title for this broadcast yet, so the "+
						"recording cannot be named until one arrives",
						slog.String("channel", entry.Name))
				}
				continue
			}
		}
		// Cleared for a title from either source, so a probe that recovers
		// and lapses again is asked about afresh rather than being held
		// silent by a budget an earlier lapse spent.
		misses = 0

		if err := d.store.ObserveTitle(broadcast.ID, d.now().UTC(), title, category); err != nil {
			d.logger.WarnContext(ctx, "recording title observation failed",
				slog.String("channel", entry.Name), slog.Any("error", err))
			continue
		}
		// The naming stage reads the broadcast row, so a title that only
		// became available after the broadcast opened has to land there or
		// the recording stays parked forever.
		if err := d.store.SetBroadcastMetadata(broadcast.ID, title, category); err != nil {
			d.logger.WarnContext(ctx, "recording broadcast metadata failed",
				slog.String("channel", entry.Name), slog.Any("error", err))
		}
	}
}

// describeBroadcast asks the platform what a channel is broadcasting now.
//
// The download tool is one source of a title and not the authoritative one.
// A probe that answers with an empty metadata block leaves a capture with no
// title, and a recording with no title is never named into the library: it
// waits in the incoming directory until something supplies one. The API
// already holds what every archived broadcast is titled, so this asks it
// while the broadcast is still on air.
//
// An answer describing a different session is refused. A channel that ends
// one broadcast and opens another while this capture drains would otherwise
// stamp the new title onto the old recording, and the organizer files it
// under that name for good.
//
// Identity is judged on the start rather than on the session id, because
// the id travels in the same metadata block the title does: a block empty
// enough to need this lookup carried no id to compare against. The id is
// still checked where the capture has one, since it settles the question
// outright.
func (d *Daemon) describeBroadcast(ctx context.Context, entry config.Channel,
	broadcast store.Broadcast,
) (string, string) {
	if d.liveMetadata == nil || ctx.Err() != nil {
		return "", ""
	}

	lookup, cancel := context.WithTimeout(ctx, broadcastStartTimeout)
	defer cancel()

	live, ok := d.liveMetadata(lookup, channelURL(entry))
	switch {
	case !ok:
		return "", ""
	case broadcast.StreamID != "" && live.StreamID != "" && live.StreamID != broadcast.StreamID:
		d.logger.DebugContext(ctx, "the platform described a different broadcast",
			slog.String("channel", entry.Name))
		return "", ""
	case live.StartedAt.After(broadcast.StartedAt.Add(sameBroadcastWindow)):
		d.logger.DebugContext(ctx, "the platform described a broadcast that began after this one",
			slog.String("channel", entry.Name),
			slog.Time("capturing_since", broadcast.StartedAt),
			slog.Time("platform_says", live.StartedAt))
		return "", ""
	}
	return live.Title, live.Category
}

// probeInterval is how often metadata is refreshed during a capture. It is
// slower than liveness polling because a title changes rarely and every
// probe is a request to the platform.
func probeInterval(cfg config.Config) time.Duration {
	interval := max(cfg.Capture.PollInterval.Std()*10, time.Minute)
	return interval
}

// ///////////////////////////////////////////////
// Sweeping
// ///////////////////////////////////////////////

// SweepParked retries every recording that is waiting, whether on metadata
// or on another program to release its file.
//
// Finalize is repeatable, so this needs no separate retry path: a parked
// recording either completes now or stays parked for the next sweep. A
// backup agent holding a large recording outlasts any in-call wait, so this
// sweep is what moves it.
func (d *Daemon) SweepParked(ctx context.Context) (int, error) {
	parked, err := d.store.RecordingsByState(store.PendingStates...)
	if err != nil {
		return 0, err
	}

	completed := 0
	for _, recording := range parked {
		if ctx.Err() != nil {
			return completed, ctx.Err()
		}
		outcome, err := d.finalizer.Finalize(ctx, recording.ID)
		if errors.Is(err, organize.ErrBusy) {
			// The capture goroutine has it. It is neither stuck nor this
			// sweep's to finish.
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				// Shutting down cancelled this finalize. Counting it would
				// call a recording stuck for stopping the daemon.
				return completed, ctx.Err()
			}
			d.reportSweepFailure(ctx, recording, err)
			continue
		}
		d.clearSweepFailure(recording.ID)
		if !outcome.Parked {
			completed++
			d.logger.InfoContext(ctx, "parked recording completed",
				slog.Int64("recording", recording.ID), slog.String("path", outcome.Path))
		}
	}
	return completed, nil
}

// reportSweepFailure records one recording's failed sweep and escalates it
// once it has failed enough of them to be stuck rather than delayed.
func (d *Daemon) reportSweepFailure(ctx context.Context, recording store.Recording, err error) {
	if errors.Is(err, organize.ErrGaveUp) {
		// The organizer will not touch this recording again, so no later
		// sweep can carry a count to its limit. This is the last moment
		// anything can say the recording never reached the library.
		d.clearSweepFailure(recording.ID)
		d.logger.ErrorContext(ctx, "recording abandoned, it will not be retried",
			slog.Int64("recording", recording.ID),
			slog.String("path", recording.Path),
			slog.Any("error", err))
		d.notify(ctx, Event{
			Kind: EventFailure,
			Detail: fmt.Sprintf("recording %s will not be retried and is not reaching the library: %v",
				recording.Path, err),
		})
		return
	}

	d.sweepMu.Lock()
	if d.sweepFailures == nil {
		d.sweepFailures = make(map[int64]int)
	}
	d.sweepFailures[recording.ID]++
	failures := d.sweepFailures[recording.ID]
	d.sweepMu.Unlock()

	if failures < sweepFailureLimit {
		d.logger.WarnContext(ctx, "sweep could not finalize",
			slog.Int64("recording", recording.ID), slog.Any("error", err))
		return
	}

	d.logger.ErrorContext(ctx, "recording is stuck, every sweep has failed",
		slog.Int64("recording", recording.ID),
		slog.String("path", recording.Path),
		slog.Int("attempts", failures),
		slog.Any("error", err))
	// Reported as it crosses the line and not on every sweep after, so a
	// recording nobody attends to does not become the same flood the
	// warning is.
	if failures == sweepFailureLimit {
		d.notify(ctx, Event{
			Kind: EventFailure,
			Detail: fmt.Sprintf("recording %s has failed %d sweeps and is not reaching the library: %v",
				recording.Path, failures, err),
		})
	}
}

// clearSweepFailure forgets a recording that got through, so a later
// failure counts from the start.
func (d *Daemon) clearSweepFailure(recordingID int64) {
	d.sweepMu.Lock()
	defer d.sweepMu.Unlock()
	delete(d.sweepFailures, recordingID)
}

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// channelURL is where the capture engine is pointed for a channel.
//
// An unregistered platform cannot happen: config validates against
// SupportedPlatforms before a channel reaches the daemon. Answering with the
// bare name keeps a drift between those two lists from becoming a panic on
// the recording path.
func channelURL(entry config.Channel) string {
	provider, err := providers.For(entry.Platform)
	if err != nil {
		return entry.Name
	}
	return provider.LiveURL(entry.Name)
}
