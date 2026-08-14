package backfill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"time"

	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/fetch"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/record"
	"zach.tools/go/stream-dvr/internal/space"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// PatchStore reads a broadcast's holes and records what filled them.
//
// The first two methods are what Gaps asks for, so a PatchStore satisfies
// it and Detect takes one directly. Filing a hole and patching it are two
// halves of one job: a pass detects before it patches, so what Detect
// writes is what this reads back.
type PatchStore interface {
	RecordingsForBroadcast(broadcastID int64) ([]store.Recording, error)
	AddGap(recordingID int64, start, end time.Duration, reason string) (store.Gap, error)
	Gaps(recordingID int64) ([]store.Gap, error)
	FillGap(id int64, at time.Time) error
	ChargeGap(id int64, limit int, terminal bool) error
	CreateRecording(r store.Recording) (store.Recording, error)
}

// errUnmeasured reports a check that could not be made, as opposed to one
// that was made and refused.
//
// The distinction decides what happens to the bytes: a refusal deletes them
// so the next pass fetches again, and an unanswered question keeps them,
// because nothing has been learned about them.
type errUnmeasured struct{ err error }

// Measurer reports a finished file's media length.
//
// Declared here at the point of use rather than taken from internal/post, so
// the patcher is testable without ffprobe. It is what turns "the tool
// exited zero" into "the range that came back is the range that was asked
// for".
type Measurer interface {
	Duration(ctx context.Context, path string) (time.Duration, error)
	// SilentBetween reports whether a stretch of a file carries no audible
	// audio. A length is not proof of content, and the stretch is what
	// matters rather than the file: a short silence inside a long range is
	// invisible to anything averaged over the whole of it.
	SilentBetween(ctx context.Context, path string, from, to time.Duration) (bool, error)
}

// Patcher fills a hole inside a broadcast the recorder did capture.
//
// # Why a patch is its own recording
//
// It never rewrites the file the hole is in. Splicing a fetched range into
// a captured file means rewriting a multi-gigabyte capture to insert a
// couple of minutes, with the live recording as the thing at risk if
// anything goes wrong. This package's hard rule is that a recovered copy
// never displaces a live one, and a splice breaks it in the worst possible
// place.
//
// So a patch lands as a recording of its own, anchored at the moment the
// hole starts. The calendar's coverage improves because the range is held
// by a row, the original capture is never opened, and an operator who
// wants one file has both pieces to join with whatever they prefer.
type Patcher struct {
	downloader Downloader
	store      PatchStore
	finalize   func(ctx context.Context, recordingID int64) error
	measure    Measurer
	// admit reports whether the library has room for a download of about
	// this many bytes. Nil leaves patches unbudgeted.
	admit func(estimate int64) error
	// recover reports where a copy's silenced stretches can be fetched with
	// the audio as broadcast. Nil refuses every silenced range, which is
	// what a machine with no source for it does.
	recover   func(ctx context.Context, broadcastURL string, spans []store.MutedSpan) (string, bool, error)
	incoming  string
	rateLimit string
	// maxAttempts caps patches of one gap, counting every failure. A hole
	// is a small download and the whole broadcast is still on disk, so
	// giving up on one costs a stretch rather than a recording. Fetching a
	// broadcast the recorder never captured is the opposite trade and is
	// bounded by a growing wait instead.
	maxAttempts int
	logger      *slog.Logger
}

// PatchOptions bound the patcher.
type PatchOptions struct {
	// LibraryRoot is the library a patch downloads into.
	LibraryRoot string
	// RateLimit caps bandwidth so a patch does not saturate the link.
	RateLimit string
	// MaxAttempts caps patches of one gap.
	MaxAttempts int
	// Admit reports whether the library has room for a download of about
	// the given bytes. Nil leaves patches outside the budget.
	Admit func(estimate int64) error
	// OriginalAudio reports where a copy's silenced stretches can be fetched
	// with the audio as broadcast, and whether anything can.
	//
	// The spans are the stretches the holes actually cover, so the answer is
	// about the segments that will be downloaded rather than about the copy
	// in general. Storage that kept some originals and lost others must
	// answer no.
	//
	// Nil refuses every silenced range, which is the behaviour of a machine
	// with no source for it and the safe default. A platform that keeps the
	// original beside the silenced variant is what makes an answer possible
	// at all, and only that platform's package knows how to ask.
	OriginalAudio func(ctx context.Context, broadcastURL string, spans []store.MutedSpan) (string, bool, error)
}

// minMeasurable is the shortest stretch worth asking about the audio of.
//
// Below this a window holds too little to tell a silenced stretch from a
// pause between words, and the answer would decide whether a whole patch is
// kept. Anything shorter is left unmeasured, which keeps the patch: the
// probe before the download is what rules on the audio, and this only
// catches a copy that changed under it.
const minMeasurable = 3 * time.Second

// ///////////////////////////////////////////////
// Constructor
// ///////////////////////////////////////////////

// Error implements error.
func (e errUnmeasured) Error() string { return e.err.Error() }

// Unwrap exposes the reason the check could not be made.
func (e errUnmeasured) Unwrap() error { return e.err }

// NewPatcher returns a patcher writing into a library's incoming directory.
func NewPatcher(downloader Downloader, patches PatchStore,
	finalize func(context.Context, int64) error, measure Measurer,
	options PatchOptions, logger *slog.Logger,
) *Patcher {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Patcher{
		downloader:  downloader,
		store:       patches,
		finalize:    finalize,
		measure:     measure,
		admit:       options.Admit,
		recover:     options.OriginalAudio,
		incoming:    paths.IncomingDir(options.LibraryRoot),
		rateLimit:   options.RateLimit,
		maxAttempts: options.MaxAttempts,
		logger:      logger,
	}
}

// ///////////////////////////////////////////////
// Patching
// ///////////////////////////////////////////////

// Patch fills every unfilled hole in one broadcast, reporting how many it
// filled.
//
// Detection runs first and is repeatable, so a broadcast whose holes were
// already filed yields the same rows rather than duplicates of them. A
// broadcast nothing captured has no hole to patch: it is missing whole, and
// that is a fetch.
func (p *Patcher) Patch(ctx context.Context, broadcast store.Broadcast,
	channel Channel, now time.Time,
) (int, error) {
	// Detection runs whether or not anything can be downloaded. A hole that
	// is filed is one the sidecar and the calendar report, and it is what a
	// later pass patches once the platform publishes the broadcast.
	if _, err := Detect(p.store, broadcast); err != nil {
		return 0, err
	}
	// Both refusals are for the whole broadcast rather than charged per gap:
	// every gap here has the same reason, and each charge would spend an
	// attempt on a question the next discovery pass answers.
	if broadcast.URL == "" {
		return 0, fmt.Errorf("broadcast %d: %w", broadcast.ID, ErrNoAddress)
	}
	if broadcast.VodStartedAt == nil {
		return 0, fmt.Errorf("broadcast %d: %w", broadcast.ID, ErrNoAnchor)
	}

	gaps, err := p.patchable(broadcast)
	if err != nil {
		return 0, err
	}

	// Resolved once for the broadcast rather than once per hole. The
	// expensive half is resolving the copy's address and reading its
	// playlist, and every hole in one broadcast shares both answers.
	//
	// A failure is not fatal here. It leaves recovered empty, which refuses
	// the silenced holes exactly as a machine with no route would, and the
	// holes that overlap nothing silenced still get patched.
	recovered, answered := p.resolveOriginalAudio(ctx, broadcast, gaps, channel)

	filled := 0
	for _, gap := range gaps {
		if ctx.Err() != nil {
			return filled, ctx.Err()
		}
		// A hole over silenced audio, on a pass where nothing could say
		// whether the original survives. Left alone entirely rather than
		// refused, so no attempt is spent on a question that was never
		// answered.
		if !answered && p.silenced(broadcast, gap) {
			continue
		}
		// A patch writes into the same volume a capture does, and its
		// bytes are invisible to the size cap until the file is claimed.
		// Nothing is charged for a refusal: a full library is the
		// operator's to resolve, not a hole the platform will not serve.
		if p.admit != nil {
			if err := p.admit(space.Estimate(space.DefaultBitrate, gap.End-gap.Start)); err != nil {
				p.logger.WarnContext(ctx, "no room to patch a gap",
					slog.String("channel", escape.Field(channel.Name)),
					slog.String("error", escape.Field(err.Error())))
				return filled, fmt.Errorf("broadcast %d: %w: %w", broadcast.ID, ErrNoRoom, err)
			}
		}
		if err := p.patch(ctx, gap, broadcast, channel, now, recovered); err != nil {
			// A download the shutdown killed answers like any other
			// failure, and charging it spends one of the gap's attempts
			// on the operator's own reboot. Five of those abandon a hole
			// the platform would have served.
			if ctx.Err() != nil {
				return filled, ctx.Err()
			}
			// One hole that will not download does not stop the rest.
			//
			// A silenced range is terminal for the same reason a removed
			// video is: no later pass changes the answer, and retrying
			// spends a whole-range download every pass for good.
			var toolErr *fetch.ToolError
			terminal := errors.Is(err, ErrMuted) ||
				(errors.As(err, &toolErr) && toolErr.Failure.Terminal())

			if chargeErr := p.store.ChargeGap(gap.ID, p.maxAttempts, terminal); chargeErr != nil {
				return filled, chargeErr
			}
			p.logger.WarnContext(ctx, "could not patch a gap",
				slog.String("channel", escape.Field(channel.Name)),
				slog.String("reason", escape.Field(gap.Reason)),
				slog.Int("attempt", gap.Attempts+1),
				slog.Bool("terminal", terminal),
				slog.String("error", escape.Field(err.Error())))
			continue
		}
		filled++
	}
	return filled, nil
}

// patchable returns a broadcast's holes worth attempting now, oldest
// recording first.
//
// One definition of "worth attempting", because the recovery lookup reads
// the same set the loop does. Two walks that could disagree would resolve a
// route for holes nothing patches, or skip one for a hole it does.
func (p *Patcher) patchable(broadcast store.Broadcast) ([]store.Gap, error) {
	recordings, err := p.store.RecordingsForBroadcast(broadcast.ID)
	if err != nil {
		return nil, fmt.Errorf("reading recordings of broadcast %d: %w", broadcast.ID, err)
	}

	var open []store.Gap
	for _, recording := range recordings {
		gaps, err := p.store.Gaps(recording.ID)
		if err != nil {
			return nil, fmt.Errorf("reading gaps of recording %d: %w", recording.ID, err)
		}
		for _, gap := range gaps {
			if gap.FilledAt != nil {
				continue
			}
			// A shortfall names how much of a recording never arrived and
			// never where, so its span covers the whole recording.
			// Downloading that would refetch the entire broadcast to recover
			// minutes, and it would land beside a capture that already holds
			// most of the same footage.
			if gap.Reason == ReasonShortMedia {
				continue
			}
			// A hole nothing can fill is the ordinary case: a platform
			// deletes a broadcast and the range behind it never returns.
			// Retrying costs a download of that whole range every pass, so
			// the count on the row is what ends it.
			if gap.Attempts >= p.maxAttempts {
				continue
			}
			open = append(open, gap)
		}
	}
	return open, nil
}

// resolveOriginalAudio returns the address a broadcast's silenced stretches
// can be fetched from with the audio as broadcast, or "" when none can.
//
// It asks about the stretches the holes actually cover, so the answer is
// about the segments that will be downloaded rather than about the copy in
// general. A copy whose storage kept some originals and lost others is
// refused rather than assumed whole.
func (p *Patcher) resolveOriginalAudio(ctx context.Context, broadcast store.Broadcast,
	gaps []store.Gap, channel Channel,
) (string, bool) {
	if p.recover == nil {
		return "", true
	}

	spans := mutedAcross(broadcast, gaps)
	if len(spans) == 0 {
		return "", true
	}

	original, ok, err := p.recover(ctx, broadcast.URL, spans)
	switch {
	case err != nil:
		// A copy the platform will never serve again has answered, and the
		// answer is no. Reading that as "ask later" skips the hole on every
		// pass for good, and spends a subprocess each time to be told the
		// same thing.
		var toolErr *fetch.ToolError
		if errors.As(err, &toolErr) && toolErr.Failure.Terminal() {
			return "", true
		}
		// Anything else answered nothing, which is not an answer of no. The
		// silenced holes are left untouched and uncharged so the next pass
		// asks again: a platform having a bad minute must not spend
		// attempts, because enough of those retire a hole it would serve.
		p.logger.WarnContext(ctx, "could not look up a broadcast's original audio",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("error", escape.Field(err.Error())))
		return "", false
	case !ok || original == "":
		return "", true
	}
	return original, true
}

// discard removes a rejected download and returns the reason it was
// rejected.
//
// A rejection is only worth retrying if the retry fetches again. The tool
// passes --no-overwrites, so a file left where it was makes the next pass
// re-measure the same bytes, reach the same verdict, and spend every attempt
// without ever asking the platform a second time. The bytes also sit outside
// the size cap until a row claims them, and no row ever will.
//
// The removal failing is not the caller's problem: the reason for the
// rejection is what matters, and the sweep collects what is left.
func (p *Patcher) discard(ctx context.Context, relPath string, reason error) error {
	// A verdict discards; a failure to reach one does not. A probe that
	// could not run says nothing about the bytes, and deleting them spends
	// the download again on the next pass for no reading.

	if _, ok := errors.AsType[errUnmeasured](reason); ok {
		return reason
	}

	file := filepath.Join(p.incoming, path.Base(relPath))

	if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.logger.WarnContext(ctx, "could not remove a rejected patch",
			slog.String("error", escape.Field(err.Error())))
	}
	return reason
}

// silenced reports whether a hole overlaps a stretch the platform silenced.
//
// It is the same question mutedWithin answers inside a patch, asked before
// one starts so a hole nothing could rule on is skipped rather than refused.
func (p *Patcher) silenced(broadcast store.Broadcast, gap store.Gap) bool {
	start, end, err := vodRange(broadcast, gap)
	if err != nil {
		return false
	}
	_, silenced := mutedWithin(broadcast.Muted, start, end)
	return silenced
}

// mutedAcross returns the silenced stretches the given holes cover, in the
// stored copy's own timeline.
//
// A stretch nobody is patching is left out: probing it would refuse a
// broadcast over audio no hole needs.
func mutedAcross(broadcast store.Broadcast, gaps []store.Gap) []store.MutedSpan {
	var spans []store.MutedSpan

	for _, gap := range gaps {
		start, end, err := vodRange(broadcast, gap)
		if err != nil {
			continue
		}
		for _, span := range broadcast.Muted {
			if span.Offset < end && start < span.Offset+span.Duration {
				spans = append(spans, span)
			}
		}
	}
	return spans
}

// patch fetches one hole and files it as a recording of its own.
//
// The gap is marked filled last. Marking it before the file reaches the
// library would leave a hole that reads as patched with nothing behind it,
// and nothing would ever look at it again.
func (p *Patcher) patch(ctx context.Context, gap store.Gap, broadcast store.Broadcast,
	channel Channel, now time.Time, recovered string,
) error {
	// Anchored where the hole starts, not where the broadcast does, so the
	// stem differs per gap and the row sorts into the right place in a day.
	startedAt := broadcast.StartedAt.Add(gap.Start)
	stem := record.CaptureStem(channel.Source, channel.Name, startedAt)

	start, end, err := vodRange(broadcast, gap)
	if err != nil {
		return err
	}
	// Checked before the download. A platform that silenced part of the
	// range serves that part as silence to anyone asking the ordinary way,
	// and FillGap is permanent, so a hole filled with silence and marked
	// done is worse than one left open for an operator to see.
	//
	// Some copies are stored with the audio as broadcast kept beside the
	// silenced variant. Where a source can reach it, the range is fetched
	// from there instead and the hole is genuinely filled.
	source, recoveredAudio := broadcast.URL, false
	if span, silenced := mutedWithin(broadcast.Muted, start, end); silenced {
		if recovered == "" {
			return fmt.Errorf("%w: %s of the range is silenced from %s in",
				ErrMuted, span.Duration.Round(time.Second), span.Offset.Round(time.Second))
		}
		source, recoveredAudio = recovered, true
	}

	section, err := Section(broadcast, gap)
	if err != nil {
		return err
	}

	result, err := p.downloader.Download(ctx, fetch.Request{
		URL: source,
		// Literal apart from the extension, for the reason the whole-file
		// fetch is: a remote title must not reach the filesystem.
		Output:    filepath.Join(p.incoming, stem+".%(ext)s"),
		Sections:  section,
		RateLimit: p.rateLimit,
	})
	if err != nil {
		return err
	}

	relPath, err := producedIn(p.incoming, stem, result.Path)
	if err != nil {
		return err
	}

	// Measured before the row exists, because the row is what makes a second
	// pass read this gap as already patched. A range that came back short was
	// indexed somewhere other than the hole, and FillGap is permanent.
	held, err := p.requireWholeRange(ctx, relPath, gap)
	if err != nil {
		return p.discard(ctx, relPath, err)
	}
	// A length says the range was assembled, not that it holds what was
	// asked for. Only a recovered range is measured: an ordinary patch is
	// allowed to be quiet, and a broadcast can legitimately hold silence.
	if recoveredAudio {
		if err := p.requireAudible(ctx, relPath, broadcast, start, held); err != nil {
			return p.discard(ctx, relPath, err)
		}
	}

	recording, err := p.store.CreateRecording(store.Recording{
		ChannelID:   channel.ID,
		BroadcastID: &broadcast.ID,
		Path:        relPath,
		State:       store.StateAwaitingFinalize,
		Origin:      store.OriginRecovered,
		StartedAt:   startedAt,
	})
	switch {
	case errors.Is(err, store.ErrDuplicatePath):
		// The path is unique and the stem is derived from where the hole
		// starts, so a duplicate means this gap was patched and only the
		// marking below did not land. Fall through and mark it.
	case err != nil:
		return err
	default:
		if err := p.finalize(ctx, recording.ID); err != nil {
			return fmt.Errorf("finalizing patch recording %d: %w", recording.ID, err)
		}
	}

	if err := p.store.FillGap(gap.ID, now); err != nil {
		return fmt.Errorf("marking gap %d filled: %w", gap.ID, err)
	}

	p.logger.InfoContext(ctx, "patched a gap",
		slog.String("channel", escape.Field(channel.Name)),
		slog.String("reason", escape.Field(gap.Reason)),
		slog.Duration("length", gap.End-gap.Start))
	return nil
}

// requireWholeRange refuses a download that did not bring back the hole it
// was asked for.
//
// The tool exiting zero says the range was accepted, not that it held what
// the hole needs: a range indexed past the end of the stored copy comes back
// nearly empty, and one indexed from the wrong origin comes back holding
// footage from elsewhere in the broadcast. A length that cannot be read is
// treated the same way, because the point is confirmation and an unread
// length confirms nothing.
func (p *Patcher) requireWholeRange(ctx context.Context, relPath string, gap store.Gap) (time.Duration, error) {
	wanted := gap.End - gap.Start

	measured, err := p.measure.Duration(ctx, filepath.Join(p.incoming, path.Base(relPath)))
	if err != nil {
		// Unmeasured, not wrong. The bytes are kept and the caller is told
		// not to discard them: a probe that could not run says nothing about
		// the range, and deleting a correct download over it spends an
		// attempt and the bandwidth again.
		return 0, errUnmeasured{err: fmt.Errorf("measuring the patch of a %s hole: %w", wanted, err)}
	}
	if shortest := wanted - shortfallAllowed(wanted); measured < shortest {
		return 0, fmt.Errorf("the patch holds %s of the %s hole, which is less than the %s a whole one is",
			measured.Round(time.Second), wanted.Round(time.Second), shortest.Round(time.Second))
	}
	return measured, nil
}

// requireAudible refuses a recovered patch whose silenced stretches came
// back silent anyway.
//
// It measures the stretches that were silenced, not the whole file. A mean
// over the whole range cannot see this: ninety seconds of silence inside a
// fifteen minute range moves the level by less than half a decibel, which no
// threshold separates from an ordinary quiet passage. Measured against the
// silenced stretch itself the difference is the whole dynamic range.
//
// It is a backstop, not the guard. What decides whether a range is fetched
// at all is whether its segments serve, settled before the download.
func (p *Patcher) requireAudible(ctx context.Context, relPath string,
	broadcast store.Broadcast, rangeStart, held time.Duration,
) error {
	file := filepath.Join(p.incoming, path.Base(relPath))

	for _, span := range broadcast.Muted {
		// The file begins where the download did, so a stretch of the
		// stored copy lands at its distance from that point.
		//
		// BOUNDED BY WHAT THE FILE ACTUALLY HOLDS, not by the hole it was
		// asked for. A cut lands on a keyframe, so a whole patch is allowed
		// to come back a little short, and a window past the end of it
		// passes no samples at all. That measures as silence, which would
		// throw away a patch that is fine.
		from := max(span.Offset-rangeStart, 0)
		to := min(span.Offset-rangeStart+span.Duration, held)
		// A sliver is not a measurement. Where a mute straddles the delivered
		// end the surviving window can be milliseconds, and a fade or a zero
		// crossing in it reads as silence, which would throw away a whole
		// patch over the last moment of it.
		if to-from < minMeasurable {
			continue
		}

		silent, err := p.measure.SilentBetween(ctx, file, from, to)
		if err != nil {
			return errUnmeasured{err: fmt.Errorf("measuring the recovered audio of a %s hole: %w",
				span.Duration.Round(time.Second), err)}
		}
		if silent {
			return fmt.Errorf("%w %s into the copy", ErrStillSilent,
				span.Offset.Round(time.Second))
		}
	}
	return nil
}

// shortfallAllowed is how much shorter than the hole a patch may be and
// still count as covering it.
//
// A cut lands on a keyframe, so the range that comes back is never exactly
// the range asked for. The proportional part is what keeps a long hole's
// allowance sane, and the floor is what keeps a hole barely over minGap from
// having an allowance measured in single seconds.
func shortfallAllowed(wanted time.Duration) time.Duration {
	return max(wanted/10, 5*time.Second)
}
