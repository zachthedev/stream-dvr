package backfill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
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

// Downloader fetches one broadcast.
type Downloader interface {
	Download(ctx context.Context, request fetch.Request) (fetch.Result, error)
}

// Claims records who is fetching what, and how it went.
type Claims interface {
	ClaimFetch(broadcastID, claimedBy int64, at time.Time, lease time.Duration) (bool, error)
	ReleaseFetch(broadcastID int64, state store.FetchState, at time.Time) error
	RecordFetchFailure(broadcastID int64, reason string, at, retryAt time.Time) error
	// AbandonFetch gives a broadcast back untouched, for a claim that ended
	// without trying anything. It answers ErrNotFound when the caller no
	// longer holds the claim, which is what a run that already recorded an
	// outcome leaves behind.
	AbandonFetch(broadcastID, claimedBy int64, at time.Time) error
	RecordingsForBroadcast(broadcastID int64) ([]store.Recording, error)
	CreateRecording(r store.Recording) (store.Recording, error)
	// SetMutedDuration records how much of a fetched copy the platform
	// silenced, which nothing in the file itself says.
	SetMutedDuration(recordingID int64, muted time.Duration) error
	// FetchFor reports how a broadcast's fetching has gone so far, which is
	// what says whether it has spent its attempts.
	FetchFor(broadcastID int64) (store.Fetch, error)
}

// Fetcher turns a candidate into a recording in the library.
type Fetcher struct {
	downloader Downloader
	claims     Claims
	finalize   func(ctx context.Context, recordingID int64) error
	// admit reports whether the library has room for a download of about
	// this many bytes. Nil leaves downloads unbudgeted.
	admit func(estimate int64) error
	// recover reports where a copy's silenced stretches can be fetched with
	// the audio as broadcast. Nil leaves a silenced copy silenced.
	recover func(ctx context.Context, broadcastURL string, spans []store.MutedSpan) (string, bool, error)
	// measure reads back what a recovered download actually holds, so a copy
	// is called recovered because it sounds recovered rather than because it
	// came from the right address.
	measure  Measurer
	incoming string
	logger   *slog.Logger
	session  int64
	options  FetchOptions
}

// FetchOptions bound one fetch.
type FetchOptions struct {
	// Lease is how long a claim is held before another fetcher may take
	// the broadcast over.
	Lease time.Duration
	// RateLimit caps bandwidth so a fetch does not saturate the link
	// during a live capture.
	RateLimit string
	// Backoff is how long to wait after the first retryable failure. Each
	// failure after it waits twice as long, up to retryCeiling.
	//
	// There is no attempt cap here, unlike the patch path. Retiring a
	// broadcast is permanent, no command undoes it, and the classifier
	// answers "retryable" for any message it does not recognize, so a
	// reworded upstream error would retire everything in range. Backing off
	// bounds the cost instead, and the window automatic recovery reaches
	// back is what finally lets a broadcast go.
	Backoff time.Duration
	// OriginalAudio reports where a copy's silenced stretches can be fetched
	// with the audio as broadcast, and the playlist to fetch them from.
	//
	// A broadcast the recorder missed is downloaded whole, so the answer is
	// used as the source for the whole download rather than for a range: the
	// playlist carrying the originals carries every other segment too. Nil,
	// or an answer of false, downloads from the address the platform serves
	// for playback, which is the silenced one.
	OriginalAudio func(ctx context.Context, broadcastURL string, spans []store.MutedSpan) (string, bool, error)

	// Admit reports whether the library has room for a download of about
	// the given bytes, answering *space.RefusalError when it does not.
	//
	// Nil leaves downloads outside the budget, which is what a caller with
	// no library to guard gets. A download writes into the same volume a
	// capture does and is invisible to the size cap while it runs, so a pass
	// left unbudgeted fills the library the capture budget is guarding.
	Admit func(estimate int64) error
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// DefaultLease is how long one fetcher holds a broadcast.
//
// Long enough for a multi-hour broadcast to download, short enough that a
// fetcher killed mid-download does not hold it for a working day.
const DefaultLease = 6 * time.Hour

// DefaultBackoff is the first wait after a retryable failure. Each failure
// after it waits twice as long, up to retryCeiling.
const DefaultBackoff = 15 * time.Minute

// retryCeiling bounds how far a broadcast that keeps failing is pushed out.
//
// A recorder runs rounds unattended, so the cost of a broadcast nothing can
// download has to be bounded by something. Retiring it would do that and is
// the wrong tool: terminal is the state no timer moves a row out of and no
// command resets, and the classifier's own default for a message it does
// not recognize is "retryable". Backing off to once a day instead leaves
// the row cheap, and the window automatic recovery reaches back retires it
// by aging it out.
const retryCeiling = 24 * time.Hour

// maxVerifiedSpans bounds how many silenced stretches are measured after a
// recovered download.
//
// A long broadcast names hundreds of them and each measurement spawns a
// subprocess over a multi-gigabyte file. The question is whether the
// playlist served originals at all, which one stretch answers and a handful
// answers with room to spare.
const maxVerifiedSpans = 3

// assumedDownload is how long a broadcast with no recorded end is sized at
// for admission. It matches the length a capture is admitted against, so a
// download and the capture it stands aside for are judged the same way.
const assumedDownload = 6 * time.Hour

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

// ErrNotClaimed reports a broadcast another fetcher holds, or one whose
// backoff has not elapsed. It is an ordinary outcome of a pass, not a
// fault.
var ErrNotClaimed = errors.New("another fetcher holds this broadcast")

// ErrAlreadyCaptured reports a broadcast that already has a live recording
// in the library.
//
// The hard rule of this package: a recovered copy never displaces a live
// one. Platforms mute a stored copy after the fact, so the live recording
// is the better file and the only one that cannot be got again.
var ErrAlreadyCaptured = errors.New("the broadcast already has a live recording")

// ErrNoAddress reports a broadcast whose row names no address to fetch
// from.
//
// A broadcast the recorder watched live carries the session it saw and
// nothing more, because an address for the stored copy only exists once the
// platform publishes one. It is an ordinary outcome rather than a fault, and
// nothing is charged for it: the next discovery pass is what supplies the
// address, and spending an attempt here would abandon the broadcast before
// it was ever fetchable.
var ErrNoAddress = errors.New("the broadcast has no address to fetch from yet")

// ErrNoAnchor reports a broadcast whose stored copy's own start is unknown,
// so a hole inside it cannot be turned into a download range.
//
// It is an ordinary outcome for the same reason ErrNoAddress is, and nothing
// is charged for it. Assuming the stored copy's timeline begins where the
// broadcast row does downloads a stretch the recorder already holds and
// marks the hole filled behind it, which is permanent.
var ErrNoAnchor = errors.New("where the stored copy's own timeline begins is unknown")

// ErrMuted reports a hole the platform silenced in its stored copy, with no
// route to the audio as broadcast.
//
// It is terminal. Playback serves silence for the stretch, and the copy no
// longer holds the original beside it, so no later pass gets a different
// answer. Filling the hole anyway would mark it done permanently with
// silence behind it, which is worse than leaving it open where an operator
// can see it.
var ErrMuted = errors.New("the platform silenced this stretch of its stored copy")

// ErrStillSilent reports a range fetched from the copy holding the original
// audio that came back silent anyway.
//
// It is NOT terminal, and that is the whole reason it is separate from
// ErrMuted. Every segment answered before the download, so the claim is
// about one assembled file rather than about the copy: a partial response, a
// decode that stopped, or a shifted keyframe produces it once and the next
// pass gets a different answer. Charged as terminal it would retire a hole
// the platform would have served.
var ErrStillSilent = errors.New("the recovered range came back silent")

// ErrGaveUp reports a broadcast nothing will try again: it spent its
// attempts, or it failed in a way a timer cannot fix.
//
// It wraps the cause, so a caller that wants the detail still has it. This
// is the one fetch outcome worth telling an operator about, because every
// other one resolves itself on a later pass.
var ErrGaveUp = errors.New("giving up on this broadcast")

// ErrNoRoom reports a download the library has no room for.
//
// Nothing is charged for it. The library being full is not the platform
// refusing, and spending attempts on it abandons a broadcast permanently
// over a condition the operator resolves by making room.
var ErrNoRoom = errors.New("the library has no room for this download")

// ///////////////////////////////////////////////
// Constructor
// ///////////////////////////////////////////////

// NewFetcher returns a fetcher writing into a library's incoming directory.
func NewFetcher(downloader Downloader, claims Claims,
	finalize func(context.Context, int64) error, measure Measurer,
	libraryRoot string, session int64, options FetchOptions, logger *slog.Logger,
) *Fetcher {
	if logger == nil {
		logger = slog.Default()
	}
	if options.Lease <= 0 {
		options.Lease = DefaultLease
	}
	if options.Backoff <= 0 {
		options.Backoff = DefaultBackoff
	}
	return &Fetcher{
		downloader: downloader,
		claims:     claims,
		finalize:   finalize,
		admit:      options.Admit,
		recover:    options.OriginalAudio,
		measure:    measure,
		incoming:   paths.IncomingDir(libraryRoot),
		logger:     logger,
		session:    session,
		options:    options,
	}
}

// estimateOf sizes a broadcast's download from its own length.
//
// The platform published the length, so it is the only honest figure there
// is. A broadcast with no recorded end is sized at the planning length a
// capture is admitted against, because admitting an unknown against zero
// admits it always.
func estimateOf(broadcast store.Broadcast) int64 {
	length := assumedDownload
	if broadcast.EndedAt != nil {
		if measured := broadcast.EndedAt.Sub(broadcast.StartedAt); measured > 0 {
			length = measured
		}
	}
	return space.Estimate(space.DefaultBitrate, length)
}

// ///////////////////////////////////////////////
// Fetching
// ///////////////////////////////////////////////

// Fetch downloads one candidate and finalizes it into the library.
//
// The order is deliberate. The address check comes first because a claim
// counts an attempt and there is nothing here to attempt. The claim comes
// next, so two fetchers cannot spend the same bandwidth on one broadcast.
// The live-recording check comes after the claim and before the download,
// because a capture can finish between a pass selecting a candidate and this
// reaching it.
func (f *Fetcher) Fetch(ctx context.Context, candidate Candidate, channel Channel, now time.Time) error {
	broadcast := candidate.Broadcast
	if broadcast.URL == "" {
		return fmt.Errorf("broadcast %d: %w", broadcast.ID, ErrNoAddress)
	}
	// Before the claim, for the same reason the address check is: a claim
	// counts an attempt, and a full library is not an attempt this broadcast
	// has spent.
	if f.admit != nil {
		if err := f.admit(estimateOf(broadcast)); err != nil {
			return fmt.Errorf("broadcast %d: %w: %w", broadcast.ID, ErrNoRoom, err)
		}
	}

	claimed, err := f.claims.ClaimFetch(broadcast.ID, f.session, now, f.options.Lease)
	if err != nil {
		return fmt.Errorf("claiming broadcast %d: %w", broadcast.ID, err)
	}
	if !claimed {
		return ErrNotClaimed
	}

	if err := f.run(ctx, broadcast, channel, now); err != nil {
		// A cancelled fetch downloaded nothing and learned nothing, so the
		// broadcast goes back exactly as it was found. Left held it would
		// sit behind this claim for the rest of the lease, and a capture
		// beginning cancels the round around it, so on a channel that
		// streams most days that is every broadcast, every round.
		if ctx.Err() != nil {
			// ErrNotFound means the run recorded an outcome before it was
			// cancelled, so there is no claim of ours left to give back.
			abandon := f.claims.AbandonFetch(broadcast.ID, f.session, now)
			if abandon != nil && !errors.Is(abandon, store.ErrNotFound) {
				f.logger.Warn("could not give back a broadcast whose fetch was interrupted",
					slog.Int64("broadcast", broadcast.ID), slog.Any("error", abandon))
			}
		}
		return err
	}
	return f.claims.ReleaseFetch(broadcast.ID, store.FetchDone, now)
}

// run does the work a claim protects, recording why it failed.
func (f *Fetcher) run(ctx context.Context, broadcast store.Broadcast, channel Channel, now time.Time) error {
	if err := f.refuseIfCaptured(broadcast.ID); err != nil {
		if !errors.Is(err, ErrAlreadyCaptured) {
			// The read could not answer, which is not the same as
			// answering no. Only a recording this reader actually saw
			// retires a broadcast, because done is the state no timer
			// moves a row out of: a database busy for the moment it was
			// asked would otherwise retire a broadcast the recorder
			// missed, permanently, and say nothing.
			return f.chargeFailure(broadcast.ID, "", err, now)
		}
		// The broadcast is in the library and this is the answer forever.
		if release := f.claims.ReleaseFetch(broadcast.ID, store.FetchDone, now); release != nil {
			return release
		}
		return err
	}

	stem := record.CaptureStem(channel.Source, channel.Name, broadcast.StartedAt)

	// A copy the platform silenced is downloaded from the playlist carrying
	// the audio as broadcast where one survives. That playlist holds every
	// other segment too, so the whole download moves rather than a range of
	// it, and the file arrives whole and audible instead of whole and part
	// silent.
	source := f.audibleSource(ctx, broadcast, channel)

	result, err := f.downloader.Download(ctx, fetch.Request{
		URL: source,
		// Literal apart from the extension, so the tool's default
		// title-derived name never runs and a remote title cannot reach
		// the filesystem. Structural, not a filter.
		Output:    filepath.Join(f.incoming, stem+".%(ext)s"),
		RateLimit: f.options.RateLimit,
	})
	if err != nil {
		// A download the shutdown killed answers like any other failure, and
		// the classifier has no marker for a process somebody stopped, so it
		// reads as transient. The claim already counted the attempt, so five
		// interruptions abandon the broadcast permanently even though the
		// tool left the partial file behind and would have resumed.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return f.chargeFailure(broadcast.ID, stem, err, now)
	}

	relPath, err := f.produced(stem, result.Path)
	if err != nil {
		return f.chargeFailure(broadcast.ID, stem, err, now)
	}

	recording, err := f.claims.CreateRecording(store.Recording{
		ChannelID:   channel.ID,
		BroadcastID: &broadcast.ID,
		Path:        relPath,
		State:       store.StateAwaitingFinalize,
		Origin:      store.OriginRecovered,
		StartedAt:   broadcast.StartedAt,
	})
	if err != nil {
		// The path is unique and the stem is derived from the broadcast's
		// start, so a duplicate means this broadcast was already fetched.
		if errors.Is(err, store.ErrDuplicatePath) {
			return nil
		}
		return f.chargeFailure(broadcast.ID, stem, err, now)
	}

	// Recorded before the file reaches the library, so the sidecar the
	// organizer writes beside it carries the figure and the file describes
	// itself. Nothing in the media says which stretches are silent.
	//
	// The figure describes this file rather than the platform's copy, so a
	// download that came back audible records none however much the platform
	// silenced its own.
	// Both of these charge like every other failure in this function. A
	// bare return would leave the claim held for the whole lease, so every
	// pass inside it refuses a broadcast whose bytes are already on disk,
	// and nothing would record why.
	if broadcast.Muted != nil {
		if err := f.claims.SetMutedDuration(recording.ID,
			f.silenceHeldBy(ctx, relPath, broadcast, source, channel)); err != nil {
			return f.chargeFailure(broadcast.ID, stem, err, now)
		}
	}

	if err := f.finalize(ctx, recording.ID); err != nil {
		return f.chargeFailure(broadcast.ID, stem,
			fmt.Errorf("finalizing recovered recording %d: %w", recording.ID, err), now)
	}
	return nil
}

// audibleSource returns the address to download a broadcast from, choosing
// the copy that carries the audio as broadcast where one survives.
//
// A failure to ask is not a reason to refuse the broadcast. The silenced
// copy is still the recording an operator missed, and arriving with silent
// stretches beats not arriving.
func (f *Fetcher) audibleSource(ctx context.Context, broadcast store.Broadcast, channel Channel) string {
	if f.recover == nil || len(broadcast.Muted) == 0 {
		return broadcast.URL
	}

	original, ok, err := f.recover(ctx, broadcast.URL, broadcast.Muted)
	switch {
	case err != nil:
		f.logger.WarnContext(ctx, "could not look up a broadcast's original audio",
			slog.String("channel", escape.Field(channel.Name)),
			slog.String("error", escape.Field(err.Error())))
		return broadcast.URL
	case !ok || original == "":
		// The ordinary answer. Most stored copies keep no original, and the
		// download proceeds from the silenced one.
		return broadcast.URL
	}
	return original
}

// silenceHeldBy reports how much of the downloaded file is actually silent.
//
// A download from the original playlist is measured rather than trusted. The
// objects are named for carrying the audio as broadcast, and a copy served
// from the wrong place, or served short, would otherwise be recorded as
// recovered and never looked at again.
//
// Anything that cannot be measured falls back to what the platform reported,
// which is the answer that was true before this ran.
func (f *Fetcher) silenceHeldBy(ctx context.Context, relPath string, broadcast store.Broadcast,
	source string, channel Channel,
) time.Duration {
	reported := mutedTotal(broadcast.Muted)
	if source == broadcast.URL || f.measure == nil {
		return reported
	}

	file := filepath.Join(f.incoming, path.Base(relPath))
	checked := 0
	for _, span := range broadcast.Muted {
		if checked == maxVerifiedSpans {
			break
		}
		// A sliver is not a measurement: a fade or a zero crossing inside one
		// reads as silence whatever the file holds.
		if span.Duration < minMeasurable {
			continue
		}

		silent, err := f.measure.SilentBetween(ctx, file, span.Offset, span.Offset+span.Duration)
		if err != nil {
			f.logger.WarnContext(ctx, "could not measure a recovered stretch",
				slog.String("channel", escape.Field(channel.Name)),
				slog.String("error", escape.Field(err.Error())))
			return reported
		}
		if silent {
			f.logger.WarnContext(ctx, "a recovered copy came back silent anyway",
				slog.String("channel", escape.Field(channel.Name)),
				slog.String("at", span.Offset.Round(time.Second).String()))
			return reported
		}
		checked++
	}

	if checked == 0 {
		return reported
	}
	f.logger.InfoContext(ctx, "recovered the audio a platform silenced",
		slog.String("channel", escape.Field(channel.Name)),
		slog.String("recovered", reported.Round(time.Second).String()))
	return 0
}

// mutedTotal adds up how much of a stored copy the platform silenced.
func mutedTotal(spans []store.MutedSpan) time.Duration {
	total := time.Duration(0)
	for _, span := range spans {
		total += span.Duration
	}
	return total
}

// refuseIfCaptured reports a broadcast that already has a live recording.
func (f *Fetcher) refuseIfCaptured(broadcastID int64) error {
	recordings, err := f.claims.RecordingsForBroadcast(broadcastID)
	if err != nil {
		return fmt.Errorf("reading recordings of broadcast %d: %w", broadcastID, err)
	}
	for _, recording := range recordings {
		if recording.Origin == store.OriginLive && recording.State == store.StateComplete {
			return ErrAlreadyCaptured
		}
	}
	return nil
}

// produced turns the path a tool reported into one inside the library.
//
// The tool is told exactly where to write, so anything else it reports is
// a tool that chose its own name. Refusing rather than trusting is what
// keeps a remote title out of the filesystem, and the check is on the
// answer rather than on the request because only the answer is evidence.
func (f *Fetcher) produced(stem, reported string) (string, error) {
	return producedIn(f.incoming, stem, reported)
}

// producedIn turns the path a tool reported into one inside the library.
//
// Shared by the whole-broadcast fetch and the gap patch, because the rule
// is about what a tool is allowed to have written rather than about which
// caller asked it to write.
func producedIn(incomingDir, stem, reported string) (string, error) {
	if reported == "" {
		return "", fmt.Errorf("the download reported no file for %s", stem)
	}

	absolute, err := filepath.Abs(reported)
	if err != nil {
		return "", fmt.Errorf("resolving the downloaded path: %w", err)
	}
	incoming, err := filepath.Abs(incomingDir)
	if err != nil {
		return "", fmt.Errorf("resolving the incoming directory: %w", err)
	}
	if filepath.Dir(absolute) != incoming {
		return "", fmt.Errorf("the download landed outside the incoming directory")
	}

	base := filepath.Base(absolute)
	if !strings.HasPrefix(base, stem+".") {
		return "", fmt.Errorf("the download wrote a name this fetch did not choose")
	}

	// Stored with forward slashes, the way every other library path is.
	return path.Join(paths.IncomingDirName, base), nil
}

// chargeFailure records why a fetch failed and when to try again.
//
// A terminal failure gets a zero retry time. The store reads that as the
// last word, so nothing claims the broadcast again.
// retryDelay returns how long to wait before trying a broadcast again,
// doubling for each attempt already spent.
//
// It reads the attempt counter rather than the wait the last failure chose,
// because the claim rewrites updated_at as it takes the broadcast and leaves
// next_attempt_at where the failure put it. The gap between them is
// therefore zero by the time any retry reads it, and a delay derived from it
// never grows.
//
// Doubled through a comparison rather than by shifting, so a row with an
// implausible count cannot overflow the duration into a negative wait that
// schedules the retry in the past.
func retryDelay(base time.Duration, attempts int) time.Duration {
	if base <= 0 {
		base = DefaultBackoff
	}

	delay := base
	for range max(attempts-1, 0) {
		if delay >= retryCeiling {
			return retryCeiling
		}
		delay *= 2
	}
	return min(delay, retryCeiling)
}

// chargeFailure records why a fetch failed and when to try again.
//
// A terminal failure gets a zero retry time. The store reads that as the
// last word, so nothing claims the broadcast again.
func (f *Fetcher) chargeFailure(broadcastID int64, stem string, cause error, now time.Time) error {
	// The row as it stood before this failure, for what it has already been
	// given and how long its last try waited. A read that fails leaves the
	// zero value, which starts the wait at the base and spends nothing.
	previous, previousErr := f.claims.FetchFor(broadcastID)
	if previousErr != nil && !errors.Is(previousErr, store.ErrNotFound) {
		// The count this read answers is the only thing pacing a broadcast
		// that keeps failing, since the fetch path caps no attempts. A read
		// that cannot answer leaves the wait at its base, so say so rather
		// than letting a broadcast quietly retry at full rate.
		f.logger.Warn("could not read how often this broadcast has been tried, "+
			"so its retry is not backed off",
			slog.Int64("broadcast", broadcastID), slog.Any("error", previousErr))
	}

	// Each failure waits longer than the last. This is what makes a
	// broadcast nothing can download cheap instead of permanent: it decays
	// to one try a day and then ages out of the window automatic recovery
	// reaches, where retiring it would be forever, since no timer moves a
	// row out of terminal and no command resets one.
	retryAt := now.Add(retryDelay(f.options.Backoff, previous.Attempts))

	// Only a classified answer from the platform retires a broadcast. A
	// reset connection, a store that could not answer, and a volume that
	// went away are all about this machine rather than about the broadcast,
	// and a broadcast retired over one is retired for good.
	if toolErr, ok := errors.AsType[*fetch.ToolError](cause); ok {
		f.logger.Warn("a backfill fetch failed",
			slog.Int64("broadcast", broadcastID),
			slog.String("failure", toolErr.Failure.String()),
			slog.String("detail", escape.Field(toolErr.Excerpt)))

		if toolErr.Failure.Terminal() {
			retryAt = time.Time{}
		}
	}

	if err := f.claims.RecordFetchFailure(broadcastID, escape.Field(cause.Error()), now, retryAt); err != nil {
		return err
	}
	// The caller is told outright, rather than having to infer the last word
	// from a failure that looks like any other.
	if retryAt.IsZero() {
		f.discardPartials(stem)
		return fmt.Errorf("%w: %w", ErrGaveUp, cause)
	}
	return cause
}

// discardPartials removes the scratch files a download of this stem left
// behind, once nothing will resume it.
//
// The download tool leaves a partial deliberately, so a retry continues
// rather than starting over. That is only worth keeping while a retry can
// happen. Past the last attempt the file has no owner: no recording row
// names it, the size cap cannot see it, and it sits in the incoming
// directory under a name no listing shows.
//
// Scoped by stem, so a download running for another broadcast is untouched.
// A removal that fails is logged rather than returned, because the fetch
// already failed and that is the answer the caller needs.
func (f *Fetcher) discardPartials(stem string) {
	entries, err := os.ReadDir(f.incoming)
	if err != nil {
		f.logger.Warn("could not read the incoming directory to discard a partial download",
			slog.String("stem", stem), slog.Any("error", err))
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, stem+".") || !isPartial(name) {
			continue
		}
		if err := os.Remove(filepath.Join(f.incoming, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			f.logger.Warn("could not discard a partial download",
				slog.String("file", name), slog.Any("error", err))
			continue
		}
		f.logger.Info("discarded a partial download nothing will resume", slog.String("file", name))
	}
}

// isPartial reports whether a name is download scratch rather than media.
//
// yt-dlp writes the media under a .part suffix while it downloads, numbers
// that suffix per fragment for a fragmented format, and keeps its resume
// state in a .ytdl file beside them. None of the three is playable.
func isPartial(name string) bool {
	return strings.HasSuffix(name, ".ytdl") ||
		strings.HasSuffix(name, ".part") ||
		strings.Contains(name, ".part-")
}
