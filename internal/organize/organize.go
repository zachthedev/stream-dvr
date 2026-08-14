// Package organize owns the library's files and serializes the work done
// on any one recording.
//
// Finalize runs two independent stages, and keeping them independent is the
// whole design. Remuxing needs no metadata, so it always runs. Naming needs
// metadata, so it can be blocked. A blocked name leaves a playable file
// under its capture name in the incoming directory. The recording is never
// renamed to something partial and never discarded. Calling Finalize again
// once metadata arrives completes it.
//
// Trash is here for the lock rather than for the subject. A purge and a
// finalize can reach the same recording at the same instant, and the
// recordings an operator may purge include two the organizer is still
// retrying, so both paths have to claim the same recording before touching
// its file.
package organize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"zach.tools/go/stream-dvr/internal/deps"
	"zach.tools/go/stream-dvr/internal/fsretry"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/naming"
	"zach.tools/go/stream-dvr/internal/retention"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Processor performs the media steps the organizer needs. *post.Pipeline
// satisfies it.
type Processor interface {
	// Remux repackages a capture into its final container.
	Remux(ctx context.Context, source, output string) error
	// ReplaceVerified runs step, records the verified output through
	// commit, and removes source only after that.
	ReplaceVerified(ctx context.Context, source, output string, keepSource bool,
		step, commit func() error) error
	// Duration reports how much media a finished file holds, which a clock
	// around the capture cannot know.
	Duration(ctx context.Context, path string) (time.Duration, error)
}

// Organizer finalizes recordings into the library.
type Organizer struct {
	library   *library.Library
	store     *store.Store
	template  *naming.Template
	processor Processor
	container string
	location  *time.Location

	// remuxBudget stops a remux that fails the same way every sweep from
	// rewriting a multi-gigabyte file forever.
	remuxBudget *failureBudget

	// finalizing names the recordings a call is already working on. The
	// capture goroutine finalizes what it just recorded, and the sweep
	// enumerates the same row on its next tick. Two calls on one recording
	// run two remuxes at one output path, and each can move a file the
	// other already named.
	finalizingMu sync.Mutex
	finalizing   map[int64]bool
}

// Options configures an Organizer.
type Options struct {
	// Container is the extension finished recordings are remuxed into.
	Container string
	// Location is the timezone dates render in.
	Location *time.Location
}

// Outcome describes what Finalize did.
type Outcome struct {
	// Path is the recording's path relative to the library root, in the
	// forward-slash form the store holds, so it compares directly against
	// a Recording.Path. For a parked recording this is still the capture
	// path.
	Path string
	// Remuxed reports whether this call performed the remux.
	Remuxed bool
	// Renamed reports whether the recording reached its library name on
	// this call.
	Renamed bool
	// Parked reports that the recording is not final yet and the file is
	// waiting under its capture name.
	Parked bool
	// Reason says why the recording parked, so an operator looking at a
	// stuck recording is told what to change. Only meaningful when Parked
	// is set.
	Reason string
	// Missing names the placeholders that blocked naming. It is empty for a
	// park that no new metadata will resolve, such as a title too long to
	// fit a path component. Only meaningful when Parked is set.
	Missing []string
	// Locked reports that another program holds the file, which is what
	// blocked the move. Only meaningful when Parked is set.
	Locked bool
	// Fallbacks names placeholders that resolved through a fallback, so a
	// silently degraded name stays visible.
	Fallbacks []string
}

// Sidecar is the JSON written beside each recording.
//
// It carries everything the database holds about a recording, so a library
// that loses its database can be rebuilt by scanning the directory. The
// database is a cache; these files and the media are the record.
type Sidecar struct {
	SchemaVersion int        `json:"schema_version"`
	Platform      string     `json:"platform"`
	Channel       string     `json:"channel"`
	Author        string     `json:"author"`
	Title         string     `json:"title"`
	Category      string     `json:"category,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	DurationMS    int64      `json:"duration_ms"`
	// MediaDurationMS is how much broadcast the file actually holds, taken
	// from the media rather than from a clock around the capture. A fetched
	// copy has no clock around it, so this is the only length it carries.
	MediaDurationMS int64  `json:"media_duration_ms"`
	Bytes           int64  `json:"bytes"`
	Origin          string `json:"origin"`
	RemoteID        string `json:"remote_id,omitempty"`
	// MutedMS is how much of this file the platform silenced. Absent means
	// nobody asked, which is every live capture, and is not the same as an
	// answer of none.
	MutedMS      *int64         `json:"muted_ms,omitempty"`
	TitleHistory []SidecarTitle `json:"title_history,omitempty"`
	Gaps         []SidecarGap   `json:"gaps,omitempty"`
}

// SidecarTitle is one title observation in a sidecar.
type SidecarTitle struct {
	ObservedAt time.Time `json:"observed_at"`
	Title      string    `json:"title"`
	Category   string    `json:"category,omitempty"`
}

// SidecarGap is one hole in the recording.
type SidecarGap struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Reason  string `json:"reason"`
	Filled  bool   `json:"filled"`
}

// failureBudget counts how many times in a row each recording failed a
// stage.
type failureBudget struct {
	mu     sync.Mutex
	counts map[int64]int
}

// SidecarVersion is the sidecar schema version this build writes and is
// able to read.
//
// It is exported because a reader has to know what it can account for. A
// sidecar stamped higher was written by a build that knew fields this one
// does not, and reading it as though it were current would import a
// recording while silently dropping whatever the newer build recorded.
const SidecarVersion = 1

// sidecarSuffix is appended to a media path to reach its sidecar.
const sidecarSuffix = ".json"

// The names a recompress works under, all of them beside the recording so
// the rename that installs the result stays on one filesystem.
//
// Each is a suffix rather than an infix so the recording's own extension is
// not what tells the media apart from the work in progress. A half-written
// re-encode named .mkv is one an operator can open by accident.
const (
	recompressSuffix = ".recompressing"
	supersededSuffix = ".superseded"
	originalSuffix   = ".original"
)

// reasonPurged is what a broadcast's refused fetch row says, and it is
// written for an operator reading that row months later.
const reasonPurged = "the operator purged every recording of this broadcast"

// maxCollisionRetries bounds how many times a claim may lose a name to
// another finalize before giving up. Losing even twice needs two other
// recordings rendering the same name in the same instant.
const maxCollisionRetries = 8

// adoptionSlack is how much longer than the clock around a capture its own
// file may measure and still be recognised as that capture.
//
// A container states its length in its own units and a remux can round the
// last frames up, so the two disagree by a little on a file that is
// genuinely the same media. It is small because the value it separates is
// large: an unrelated file that happens to sit at the rendered name.
const adoptionSlack = time.Minute

// maxRemuxAttempts bounds how many times in a row a recording may fail the
// remux stage before it is parked out of the sweep.
//
// The sweep retries every pending recording on a timer, and a remux that
// will not succeed costs a full pass over a multi-gigabyte file each time
// it comes round. Three is enough to tell a program still holding the file
// from a file that cannot be remuxed at all, and the count is of failures
// in a row so a recording that gets through is not carrying a tally.
const maxRemuxAttempts = 3

// ErrBusy reports that another call is already finalizing the recording.
// Finalize is repeatable across calls that run one after another, and this
// is what it answers to one that overlaps.
var ErrBusy = errors.New("the recording is already being finalized")

// ErrNotPurgeable reports that a recording is not in a state the operator
// may purge. It exists so a caller acting on a stale ranking is told what
// happened rather than seeing a move fail for no stated reason.
var ErrNotPurgeable = errors.New("the recording is not in a state that may be purged")

// ErrNotTrashed reports that a recording is not in the trash. Release
// answers with it rather than deleting, so the one operation that removes
// a recording can only ever finish a decision the operator already made.
var ErrNotTrashed = errors.New("the recording is not in the trash")

// ErrNotRecompressable reports a recording that must not be re-encoded: one
// the organizer has not finished with, or one already re-encoded once.
//
// Re-encoding what is already re-encoded costs hours and loses picture for
// nothing, which is why it is refused rather than merely skipped.
var ErrNotRecompressable = errors.New("the recording is not in a state that may be re-encoded")

// ErrGaveUp reports a recording the organizer will not attempt again.
//
// It is the last error a caller ever sees for that recording: the same
// moment puts it in StateFailed, which takes it out of PendingStates and out
// of every sweep, so a caller keeping its own count of failures never
// reaches a limit of its own.
var ErrGaveUp = errors.New("giving up on the recording")

// renameFile moves a finished recording to its library path. Tests
// substitute it to drive the parked-on-lock path, which a real lock could
// only reach by holding a file for the whole of fsretry's window.
var renameFile = fsretry.RenameNew

// writeSidecarFile persists a sidecar. Tests substitute it to fail the
// write after the move has already happened, which is the case that decides
// whether the database can be left naming a file that has already moved.
var writeSidecarFile = fsretry.WriteFileAtomic

// New returns an Organizer.
func New(lib *library.Library, st *store.Store, template *naming.Template, processor Processor, opts Options) *Organizer {
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.Container == "" {
		opts.Container = "mkv"
	}
	return &Organizer{
		library:     lib,
		store:       st,
		template:    template,
		processor:   processor,
		container:   strings.TrimPrefix(opts.Container, "."),
		location:    opts.Location,
		remuxBudget: &failureBudget{counts: map[int64]int{}},
		finalizing:  map[int64]bool{},
	}
}

// ///////////////////////////////////////////////
// Finalize
// ///////////////////////////////////////////////

// Finalize remuxes a finished capture and names it into the library.
//
// It is safe to call repeatedly. A recording already remuxed skips that
// stage, and a recording parked for missing metadata retries naming. That
// repeatability is what lets the daemon sweep parked recordings on a timer
// rather than needing a separate retry path.
//
// Overlapping calls on one recording are not repetition, and the second one
// returns ErrBusy rather than working alongside the first.
func (o *Organizer) Finalize(ctx context.Context, recordingID int64) (Outcome, error) {
	if !o.beginFinalize(recordingID) {
		return Outcome{}, fmt.Errorf("recording %d: %w", recordingID, ErrBusy)
	}
	defer o.endFinalize(recordingID)

	recording, err := o.store.Recording(recordingID)
	if err != nil {
		return Outcome{}, err
	}
	channel, err := o.channelFor(recording)
	if err != nil {
		return Outcome{}, err
	}

	outcome := Outcome{Path: recording.Path}

	if remuxed, err := o.remuxIfNeeded(ctx, &recording); err != nil {
		return outcome, err
	} else if remuxed {
		outcome.Remuxed = true
		outcome.Path = recording.Path
	}

	fields, remoteID, err := o.fieldsFor(recording, channel)
	if err != nil {
		return outcome, err
	}

	rendered, err := o.template.Render(fields)
	if err != nil {
		// Every naming failure parks, not just a missing field. A title
		// that sanitizes away to nothing, or one too long to fit a path
		// component, leaves a recording every bit as intact as a missing
		// one does. Returning an error instead would mark it complete and
		// drop it out of the sweep, which loses the file to the operator
		// while it sits on disk.
		if stateErr := o.store.SetState(recording.ID, store.StateAwaitingMetadata); stateErr != nil {
			return outcome, stateErr
		}
		outcome.Parked = true
		outcome.Reason = err.Error()

		if missing, ok := errors.AsType[*naming.MissingFieldError](err); ok {
			outcome.Missing = missing.Placeholders
		}
		return outcome, nil
	}

	outcome.Fallbacks = rendered.Fallbacks
	finalPath, err := o.moveIntoLibrary(ctx, recording, rendered.Path)
	if err != nil {
		if _, ok := errors.AsType[*fsretry.LockedError](err); !ok {
			return outcome, err
		}
		// Another program is reading the file, most often a backup agent
		// working through a capture that just finished. The recording is
		// intact under its capture name, so this parks exactly like a
		// missing title does and the sweep moves it once the hold ends.
		if stateErr := o.store.SetState(recording.ID, store.StateAwaitingFile); stateErr != nil {
			return outcome, stateErr
		}
		outcome.Parked = true
		outcome.Locked = true
		return outcome, nil
	}
	outcome.Path = filepath.ToSlash(finalPath)
	outcome.Renamed = !samePath(finalPath, recording.Path)

	// The path is recorded before the sidecar is written, because the
	// database must never lag the filesystem. A sidecar failure with the
	// old path still stored leaves the row pointing at a file that has
	// already moved, and every retry then fails on the missing source
	// forever. A missing sidecar is recoverable; a lost path is not.
	if err := o.store.SetPath(recording.ID, finalPath); err != nil {
		return outcome, err
	}
	if err := o.measureFile(ctx, recording.ID, o.library.RelPath(finalPath)); err != nil {
		return outcome, err
	}
	// Re-read, because measureFile has just written the size and the media
	// length into the row, and the sidecar has to carry what the row holds.
	// The value loaded above knows neither.
	measured, err := o.store.Recording(recording.ID)
	if err != nil {
		return outcome, err
	}
	if err := o.writeSidecar(ctx, measured, channel, fields, remoteID, finalPath); err != nil {
		return outcome, err
	}
	if err := o.store.SetState(recording.ID, store.StateComplete); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// ///////////////////////////////////////////////
// Recompress
// ///////////////////////////////////////////////

// Recompress re-encodes a recording in place, through encode.
//
// The encoder itself is the caller's, because which encoder this machine
// has is a question about hardware and configuration rather than about the
// library. What is here is everything that can lose a recording: the lock,
// the state check, and the swap.
//
// The output is verified against the source before anything moves. The
// source is then renamed aside rather than deleted, so every failure after
// that point leaves a file to put back, and it is removed only once the
// store has recorded the new one.
func (o *Organizer) Recompress(ctx context.Context, recordingID int64, keepOriginal bool,
	encode func(ctx context.Context, source, output string) error,
) error {
	if !o.beginFinalize(recordingID) {
		return fmt.Errorf("recording %d: %w", recordingID, ErrBusy)
	}
	defer o.endFinalize(recordingID)

	recording, err := o.store.Recording(recordingID)
	if err != nil {
		return err
	}
	// Asked again here rather than trusted from the list that proposed it.
	// A recompress runs for hours, and the sweep, a purge, and the operator
	// all reach the same row in that time.
	if recording.State != store.StateComplete || recording.RecompressedAt != nil {
		return fmt.Errorf("recording %d in state %q: %w", recordingID, recording.State, ErrNotRecompressable)
	}

	source := o.library.RelPath(recording.Path)
	output := source + recompressSuffix

	return o.processor.ReplaceVerified(ctx, source, output, true,
		func() error { return encode(ctx, source, output) },
		func() error { return o.commitRecompress(ctx, recordingID, source, output, keepOriginal) })
}

// commitRecompress puts the verified output in the source's place.
//
// Nothing is deleted until the store names the file that replaced it. The
// source is renamed aside first, so a failure at any later step has a file
// to restore rather than a hole where a broadcast was.
func (o *Organizer) commitRecompress(ctx context.Context, recordingID int64,
	source, output string, keepOriginal bool,
) error {
	aside := source + supersededSuffix
	if err := fsretry.Rename(ctx, source, aside); err != nil {
		return fmt.Errorf("setting the original of recording %d aside: %w", recordingID, err)
	}

	if err := fsretry.Rename(ctx, output, source); err != nil {
		return errors.Join(
			fmt.Errorf("installing the re-encode of recording %d: %w", recordingID, err),
			fsretry.Rename(ctx, aside, source))
	}

	info, err := os.Stat(source)
	if err != nil {
		return errors.Join(
			fmt.Errorf("measuring the re-encode of recording %d: %w", recordingID, err),
			restoreOriginal(ctx, source, aside))
	}
	if err := o.store.MarkRecompressed(recordingID, time.Now(), info.Size()); err != nil {
		return errors.Join(err, restoreOriginal(ctx, source, aside))
	}

	// The store now names the re-encode, so the original is either the
	// operator's own safety copy or a duplicate of a file that is recorded.
	if keepOriginal {
		return fsretry.Rename(ctx, aside, source+originalSuffix)
	}
	return removeIfPresent(aside)
}

// restoreOriginal puts a set-aside recording back, discarding whatever
// replaced it.
func restoreOriginal(ctx context.Context, source, aside string) error {
	if err := removeIfPresent(source); err != nil {
		return err
	}
	return fsretry.Rename(ctx, aside, source)
}

// ///////////////////////////////////////////////
// Purge
// ///////////////////////////////////////////////

// Trash moves a recording the operator purged out of the library and into
// the trash directory, and returns its new library-relative path.
//
// It lives here rather than in a package of its own because of the lock.
// The sweep retries parked recordings on a timer, so a purge and a
// finalize can reach the same row at the same instant, and the recordings
// the operator can purge include two the organizer is still retrying. The
// lock that makes Finalize safe against itself is what keeps a purge off a
// row this organizer is working on, and it is held here.
//
// That lock is process-local, so it covers a purge and a recorder running
// in the same process and nothing else. A recorder in another process is
// answered by the state re-read below, which happens under the write lock
// the DSN takes as a transaction begins, and by the exclusive create the
// move itself makes.
//
// The bytes are not freed. A trashed recording still counts against the
// budget because its file is still on the volume, and it is the release
// that returns them once the grace expires.
//
// Nothing here decides that a recording goes. Trash acts on one the
// operator already chose.
func (o *Organizer) Trash(ctx context.Context, recordingID int64) (string, error) {
	if !o.beginFinalize(recordingID) {
		return "", fmt.Errorf("recording %d: %w", recordingID, ErrBusy)
	}
	defer o.endFinalize(recordingID)

	recording, err := o.store.Recording(recordingID)
	if err != nil {
		return "", err
	}
	// Asked again here rather than trusted from the list that proposed
	// it. Between the operator reading a ranking and confirming it, the
	// recorder can start writing to that row and the operator can pin it
	// from another pane.
	if !retention.Purgeable(recording) {
		return "", fmt.Errorf("recording %d in state %q: %w", recordingID, recording.State, ErrNotPurgeable)
	}

	// Before the file moves, so a refusal that cannot be written leaves the
	// recording where it is rather than purging it into a library that then
	// downloads it back.
	if err := o.refuseFetchWithoutThisCopy(recording); err != nil {
		return "", err
	}

	// The state is recorded last. A move that succeeds with a state that
	// does not leaves a row naming a file in the trash that still reads as
	// part of the library, which the next sweep tries to finalize. The
	// reverse leaves a row saying trashed with the file still in place,
	// which nothing ever cleans up because the release only deletes what
	// the row points at.
	moved, err := o.moveToTrash(ctx, recording)
	if err != nil {
		return "", err
	}
	if err := o.store.SetPath(recordingID, moved); err != nil {
		return "", err
	}
	if err := o.store.SetState(recordingID, store.StateTrashed); err != nil {
		return "", err
	}
	// A trashed recording is not one the remux stage will see again, which
	// is the condition the budget is cleared on everywhere else. Without
	// this a recording that failed once and was then purged keeps its
	// entry for the life of the process.
	o.remuxBudget.clear(recordingID)
	return moved, nil
}

// Release completes a purge the operator already made, deleting the file
// and the row.
//
// This is the only thing in the project that removes a recording, and it
// may only ever run against a row the operator condemned themselves.
// Trash is what makes that decision; Release only decides when it
// finishes. It refuses anything not already in the trash, so no caller
// can turn it into a delete.
//
// The file goes first and the row last. That order leaves a row naming a
// file that is gone, which an operator can see and act on, rather than
// dropping the only record that the file was ever there.
func (o *Organizer) Release(ctx context.Context, recordingID int64) error {
	if !o.beginFinalize(recordingID) {
		return fmt.Errorf("recording %d: %w", recordingID, ErrBusy)
	}
	defer o.endFinalize(recordingID)

	recording, err := o.store.Recording(recordingID)
	if err != nil {
		return err
	}
	if recording.State != store.StateTrashed {
		return fmt.Errorf("recording %d in state %q: %w", recordingID, recording.State, ErrNotTrashed)
	}

	// Trash already refused this broadcast for a purge it went through, so
	// this covers a row that reached the trash by any other route. Writing
	// it twice is one upsert.
	if err := o.refuseFetchWithoutThisCopy(recording); err != nil {
		return err
	}

	// Waited out rather than attempted once. This is the cheapest rung of
	// the space ladder and it runs under pressure, so a backup agent
	// reading a trashed recording would otherwise defeat it and the daemon
	// would log a warning per tick while reclaiming nothing. Every other
	// move, delete and write in this package already goes through fsretry.
	path := o.library.RelPath(recording.Path)
	if err := removeWithRetry(ctx, path); err != nil {
		return fmt.Errorf("releasing recording %d: %w", recordingID, err)
	}
	// A recording purged before its sidecar was written has none, so a
	// missing one is the ordinary case rather than a failure.
	if err := removeWithRetry(ctx, path+sidecarSuffix); err != nil {
		return fmt.Errorf("releasing the sidecar for recording %d: %w", recordingID, err)
	}
	o.remuxBudget.clear(recordingID)
	return o.store.DeleteRecording(recordingID)
}

// refuseFetchWithoutThisCopy marks a broadcast unfetchable when the
// recording leaving the library is the last one holding bytes.
//
// Deleting every copy of a broadcast is a decision about that broadcast.
// Without this the recovery pass reads the day as missed, downloads the
// platform's copy back inside one backfill interval, and spends the space
// the purge freed on a version the platform mutes where it hears
// copyrighted audio.
//
// Purging one of two files says nothing about the other, so a broadcast
// keeping a copy stays recoverable.
func (o *Organizer) refuseFetchWithoutThisCopy(recording store.Recording) error {
	if recording.BroadcastID == nil {
		return nil
	}

	held, err := o.store.RecordingsForBroadcast(*recording.BroadcastID)
	if err != nil {
		return fmt.Errorf("reading the recordings of broadcast %d: %w", *recording.BroadcastID, err)
	}
	for _, other := range held {
		if other.ID != recording.ID && store.HoldsBytes(other.State) {
			return nil
		}
	}
	return o.store.RefuseFetch(*recording.BroadcastID, reasonPurged, time.Now().UTC())
}

// statOf reports a path's file info, and whether it is there at all.
//
// The absence is an answer rather than a failure: this runs before the
// sidecar exists on every ordinary finalize, and a path that is not there
// cannot be the same file as another one.
func statOf(path string) (os.FileInfo, bool) {
	info, err := os.Stat(path)
	return info, err == nil
}

// removeIfPresent deletes a path, treating an absent one as done.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// removeWithRetry deletes a file, waiting out a program that is holding it.
//
// A file that is not there is the answer this wants, so its absence is
// success rather than a failure to report.
func removeWithRetry(ctx context.Context, path string) error {
	if err := fsretry.Remove(ctx, path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// moveToTrash moves the media and its sidecar, returning the media's new
// library-relative path.
//
// The trash is flat while the library is nested, so the recording's id
// leads the name: two recordings can render identical library names, and
// in the trash there is no channel or year directory left to separate
// them. The id also makes the claim collision-free, so a name taken here
// means something is wrong rather than that a retry is needed.
//
// A missing sidecar is not a failure. It is written after the media moves
// during finalize, so a recording purged between those two steps has
// none, and refusing to purge it would strand exactly the recording an
// operator is most likely to be clearing out.
func (o *Organizer) moveToTrash(ctx context.Context, recording store.Recording) (string, error) {
	base := filepath.Base(filepath.FromSlash(recording.Path))
	if base == "." || base == string(filepath.Separator) {
		return "", fmt.Errorf("recording %d has no file name in %q", recording.ID, recording.Path)
	}

	target := filepath.Join(o.library.TrashDir(), fmt.Sprintf("%d-%s", recording.ID, base))
	source := o.library.RelPath(recording.Path)

	// The sidecar moves first, so the media is the last thing that can
	// fail. The caller records the new path only once this returns, so a
	// failure after the media moved would leave the row naming a file that
	// is already in the trash, where nothing looks again: releasing walks
	// trashed rows and this one still reads complete. Ordered this way the
	// media is still where the row says on every failing path, and a retry
	// finds the sidecar already moved and carries on.
	sidecar := source + sidecarSuffix
	if _, err := os.Stat(sidecar); err == nil {
		if err := renameFile(ctx, sidecar, target+sidecarSuffix); err != nil {
			return "", fmt.Errorf("moving the sidecar for recording %d to the trash: %w", recording.ID, err)
		}
	}

	if err := renameFile(ctx, source, target); err != nil {
		return "", fmt.Errorf("moving recording %d to the trash: %w", recording.ID, err)
	}

	relative, err := filepath.Rel(o.library.Root(), target)
	if err != nil {
		return "", fmt.Errorf("locating the trashed recording %d under the library: %w", recording.ID, err)
	}
	return filepath.ToSlash(relative), nil
}

// beginFinalize reserves a recording for this call, reporting whether it
// got it.
func (o *Organizer) beginFinalize(recordingID int64) bool {
	o.finalizingMu.Lock()
	defer o.finalizingMu.Unlock()

	if o.finalizing[recordingID] {
		return false
	}
	o.finalizing[recordingID] = true
	return true
}

// endFinalize releases a recording for the next call.
func (o *Organizer) endFinalize(recordingID int64) {
	o.finalizingMu.Lock()
	defer o.finalizingMu.Unlock()
	delete(o.finalizing, recordingID)
}

// channelFor loads the channel a recording belongs to.
func (o *Organizer) channelFor(recording store.Recording) (store.Channel, error) {
	channels, err := o.store.Channels()
	if err != nil {
		return store.Channel{}, err
	}
	for _, channel := range channels {
		if channel.ID == recording.ChannelID {
			return channel, nil
		}
	}
	return store.Channel{}, fmt.Errorf("channel %d for recording %d: %w",
		recording.ChannelID, recording.ID, store.ErrNotFound)
}

// ///////////////////////////////////////////////
// Remux stage
// ///////////////////////////////////////////////

// remuxIfNeeded converts a capture to the final container, in place in the
// incoming directory.
//
// The output keeps the capture's metadata-free stem, so this stage never
// waits on a title. A recording already in the final container is left
// alone, which is what makes Finalize repeatable.
func (o *Organizer) remuxIfNeeded(ctx context.Context, recording *store.Recording) (bool, error) {
	// A file already in the container needs nothing. Anything else is
	// remuxed, whatever it arrived as: the name this recording is about to
	// be given carries the container's extension, so a file left in
	// another one would have its bytes renamed onto a path claiming to be
	// something they are not.
	if strings.EqualFold(strings.TrimPrefix(filepath.Ext(recording.Path), "."), o.container) {
		return false, nil
	}

	source := o.library.RelPath(recording.Path)
	if _, err := os.Stat(source); err != nil {
		return false, fmt.Errorf("capture file for recording %d: %w", recording.ID, err)
	}

	relOutput := strings.TrimSuffix(recording.Path, filepath.Ext(recording.Path)) + "." + o.container
	output := o.library.RelPath(relOutput)

	// A leftover at the output path is unverified by construction: the path
	// is recorded only once the output passes, and this stage returns early
	// once the row names the container, so a file here while the row still
	// names the capture came from a remux that never finished. The remux
	// refuses a path something already holds, so leaving it makes every
	// later sweep fail identically until the recording is abandoned.
	if err := removeIfPresent(output); err != nil {
		return false, fmt.Errorf("discarding an unfinished remux of recording %d: %w", recording.ID, err)
	}

	// The path is recorded between verifying the output and removing the
	// capture, so a removal that fails leaves a row naming a file that
	// exists and is verified. Recording it afterwards leaves the row naming
	// the capture with the finished file beside it, and every later sweep
	// refuses to write over that file.
	err := o.processor.ReplaceVerified(ctx, source, output, false,
		func() error { return o.processor.Remux(ctx, source, output) },
		func() error { return o.store.SetPath(recording.ID, relOutput) })
	if err != nil {
		return false, o.chargeRemux(recording.ID, err)
	}

	o.remuxBudget.clear(recording.ID)
	recording.Path = relOutput
	return true, nil
}

// measureFile records the size and the media length of the finished file.
//
// One measurement point for every origin. A download that arrives already in
// the configured container skips the remux entirely, so anything measured
// inside that stage is measured for some recordings and not others, and the
// ones it misses carry a zero for the rest of their lives.
//
// It runs against the path in the library rather than the capture, because
// that is the file the row will name from here on and the only one a later
// reader can check the row against. A length that cannot be read is returned
// rather than stored as zero: zero is the column's "nobody measured this"
// value, and writing it would have the detector report the whole recording
// missing.
//
// The state is still not complete when this runs, so a failure leaves the
// recording in the sweep and the next pass measures it.
func (o *Organizer) measureFile(ctx context.Context, recordingID int64, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("measuring recording %d: %w", recordingID, err)
	}
	if err := o.store.SetBytes(recordingID, info.Size()); err != nil {
		return err
	}

	measured, err := o.processor.Duration(ctx, path)
	if err != nil {
		return fmt.Errorf("measuring the media of recording %d: %w", recordingID, err)
	}
	return o.store.SetMediaDuration(recordingID, measured)
}

// chargeRemux counts a failed remux against the recording's budget and
// parks it out of the sweep once the same failure has spent it.
//
// StateFailed leaves the capture on disk and puts the recording somewhere
// an operator can see it, which a recording retried forever never reaches.
func (o *Organizer) chargeRemux(recordingID int64, cause error) error {
	failure := fmt.Errorf("remuxing recording %d: %w", recordingID, cause)

	// A tool that is not installed says nothing about this recording's
	// media, and it fails every recording equally. Charging it would abandon
	// the whole pending queue in three sweeps over something the operator
	// fixes by installing one program.
	if errors.Is(cause, deps.ErrNotFound) {
		return failure
	}
	if !o.remuxBudget.spent(recordingID) {
		return failure
	}

	// The recording leaves the sweep here, so its count has nothing left to
	// govern and the daemon runs for months.
	o.remuxBudget.clear(recordingID)
	if err := o.store.SetState(recordingID, store.StateFailed); err != nil {
		return errors.Join(failure, err)
	}
	return fmt.Errorf("%w: %w after %d attempts", failure, ErrGaveUp, maxRemuxAttempts)
}

// spent counts one failure and reports whether the budget is now used up.
func (b *failureBudget) spent(id int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.counts[id]++
	return b.counts[id] >= maxRemuxAttempts
}

// clear forgets a recording's failures.
//
// It runs wherever a recording stops being one the remux stage will see
// again, so what the budget holds is the recordings failing right now
// rather than every recording that ever failed.
func (b *failureBudget) clear(id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.counts, id)
}

// tracking reports how many recordings the budget is counting.
func (b *failureBudget) tracking() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.counts)
}

// ///////////////////////////////////////////////
// Naming stage
// ///////////////////////////////////////////////

// fieldsFor assembles the values a naming template can reference.
func (o *Organizer) fieldsFor(recording store.Recording, channel store.Channel) (naming.Fields, string, error) {
	fields := naming.Fields{
		Platform:  channel.Platform,
		Channel:   channel.Name,
		Author:    channel.DisplayName,
		StartedAt: recording.StartedAt.In(o.location),
		Extension: o.container,
	}

	if recording.BroadcastID == nil {
		return fields, "", nil
	}

	broadcast, err := o.store.Broadcast(*recording.BroadcastID)
	if errors.Is(err, store.ErrNotFound) {
		return fields, "", nil
	}
	if err != nil {
		return naming.Fields{}, "", err
	}

	fields.Title = broadcast.Title
	fields.Category = broadcast.Category
	// A live capture's own start is exact, so it wins over the broadcast
	// row, which a lower-precision source may have supplied.
	if recording.Origin == store.OriginRecovered {
		fields.StartedAt = broadcast.StartedAt.In(o.location)
	}
	return fields, broadcast.RemoteID, nil
}

// moveIntoLibrary renames the file to its final path, resolving collisions.
//
// The move claims its target rather than checking that it is free, because
// one organizer serves every channel watcher and the sweep at once. Two
// recordings that render the same name is the ordinary case for a broadcast
// a reconnect split in two, and every part of that name comes from the
// stream. A losing claim re-enters deduplication and never overwrites.
func (o *Organizer) moveIntoLibrary(ctx context.Context, recording store.Recording, relTarget string) (string, error) {
	source := o.library.RelPath(recording.Path)
	if _, err := os.Stat(source); err != nil {
		if adopted, ok := o.orphanOf(ctx, recording, relTarget); ok {
			// The adopted path is checked too. Adopting a file still stores
			// a path, and a stored path is what Release later deletes.
			if err := o.refuseReserved(adopted); err != nil {
				return "", err
			}
			return adopted, nil
		}
		return "", fmt.Errorf("recording %d file: %w", recording.ID, err)
	}

	taken := map[string]bool{}
	for range maxCollisionRetries {
		unique, err := naming.Deduplicate(relTarget, func(candidate string) bool {
			// The file's current path is not a collision with itself.
			if samePath(candidate, recording.Path) {
				return false
			}
			if taken[candidate] {
				return true
			}
			// The sidecar shares the recording's stem, so a free media
			// name whose sidecar is taken would destroy that sidecar. The
			// pair moves to the next suffix together.
			return o.exists(candidate) || o.exists(SidecarPath(candidate))
		})
		if err != nil {
			return "", err
		}
		if samePath(unique, recording.Path) {
			return unique, nil
		}

		if err := o.claim(ctx, recording, source, unique); err != nil {
			if errors.Is(err, fs.ErrExist) {
				// Something took the name between deciding on it and
				// claiming it. Rule it out and pick the next one.
				taken[unique] = true
				continue
			}
			return "", err
		}
		return unique, nil
	}
	return "", fmt.Errorf("moving recording %d into the library: %d names were taken as they were claimed",
		recording.ID, maxCollisionRetries)
}

// exists reports whether a library-relative path is present.
func (o *Organizer) exists(relPath string) bool {
	_, err := os.Stat(o.library.RelPath(relPath))
	return err == nil
}

// samePath reports whether two library-relative paths name one file.
//
// The two sides reach a comparison from different builders: a candidate
// comes from the name renderer and joins segments with the host separator,
// while a stored path carries the store's forward slashes. Compared as
// text, the two spellings of one path disagree on Windows.
func samePath(a, b string) bool { return filepath.ToSlash(a) == filepath.ToSlash(b) }

// orphanOf returns the name an interrupted finalize already moved this
// recording to, and whether one was found.
//
// The move happens before the path reaches the database, so a failure
// between the two leaves the row naming a source that has already moved,
// and every retry dies on it. Adopting the file is what turns that into a
// recording the sweep finishes rather than one stalled forever.
//
// The whole deduplication series is searched, not just the rendered name.
// Two recordings of one broadcast render one name, which a reconnect makes
// ordinary, so the interrupted move is as likely to have landed on
// " (2)" as on the bare name. Searching only the bare name leaves that
// recording with no row naming its file and no stage able to find it.
//
// A candidate must carry no sidecar and must hold the recording's own
// media. The sidecar is written after the path, so its absence marks a
// finalize that did not get that far; a name whose sidecar is present is
// somebody's finished recording. Position alone is not enough to adopt on,
// because every segment of the rendered name comes from the stream, so a
// title can be chosen to land on a file the operator put there.
func (o *Organizer) orphanOf(ctx context.Context, recording store.Recording, relTarget string) (string, bool) {
	for candidate := range naming.Candidates(relTarget) {
		if !o.exists(candidate) || o.exists(SidecarPath(candidate)) {
			continue
		}
		if o.holdsRecording(ctx, recording, candidate) {
			return candidate, true
		}
	}
	return "", false
}

// holdsRecording reports whether a file is the one this recording made.
//
// Either measure settles it, because each covers one of the two ways a
// recording reaches this point.
//
// The size answers for a capture that went into the library as it was
// written: the row carries the size the recorder measured, and moving a
// file does not change it.
//
// The length answers for one that was remuxed on the way. That rewrites the
// file, so its size stops matching a row whose byte count is not measured
// again until after the move being recovered here. The media is the same
// media. A capture holds what the recorder received while it ran, so a hole
// leaves the file shorter than the clock around it and nothing makes it
// longer.
func (o *Organizer) holdsRecording(ctx context.Context, recording store.Recording, candidate string) bool {
	full := o.library.RelPath(candidate)

	if info, err := os.Stat(full); err == nil && recording.Bytes > 0 && info.Size() == recording.Bytes {
		return true
	}

	if recording.Duration <= 0 {
		return false
	}
	held, err := o.processor.Duration(ctx, full)
	if err != nil {
		// Not media, or media nothing can read. Either way it is not a
		// capture this recorder finished.
		return false
	}
	return held > 0 && held <= recording.Duration+adoptionSlack
}

// claim moves the recording onto a name no one else holds.
func (o *Organizer) claim(ctx context.Context, recording store.Recording, source, unique string) error {
	target := o.library.RelPath(unique)
	if err := o.refuseReserved(unique); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}

	if err := renameFile(ctx, source, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return err
		}
		return fmt.Errorf("moving recording %d into the library: %w", recording.ID, err)
	}
	return nil
}

// refuseReserved rejects a path that resolves outside the library or inside
// the library's own state directory.
//
// naming already prefixes a segment matching a reserved directory, so this
// is the backstop for a caller that renders a path some other way. The state
// directory holds the ownership marker, and a recording or sidecar landing on
// it makes the whole library unopenable. Outside the root there is no bound
// at all on what a rendered name can overwrite.
func (o *Organizer) refuseReserved(relPath string) error {
	// A rendered name is a path inside the library and nothing else, so
	// anything absolute or climbing above the root is refused before the
	// root is even consulted.
	if !filepath.IsLocal(relPath) {
		return fmt.Errorf("%s resolves outside the library", relPath)
	}

	state, err := filepath.Abs(o.library.StateDir())
	if err != nil {
		return fmt.Errorf("resolving the state directory: %w", err)
	}
	target, err := filepath.Abs(o.library.RelPath(relPath))
	if err != nil {
		return fmt.Errorf("resolving %s: %w", relPath, err)
	}

	if rel, err := filepath.Rel(state, target); err == nil && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s resolves inside the library state directory", relPath)
	}

	// Both reserved directories, not one. naming reserves the same pair, so
	// a backstop covering half of it is a backstop for half the cases: a
	// display name matching the capture directory would file recordings
	// among the files a capture is still writing.
	incoming, err := filepath.Abs(o.library.IncomingDir())
	if err != nil {
		return fmt.Errorf("resolving the incoming directory: %w", err)
	}
	if rel, err := filepath.Rel(incoming, target); err == nil && !strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s resolves inside the library capture directory", relPath)
	}
	return nil
}

// ///////////////////////////////////////////////
// Sidecar
// ///////////////////////////////////////////////

// writeSidecar records everything known about a recording beside it.
func (o *Organizer) writeSidecar(ctx context.Context, recording store.Recording, channel store.Channel,
	fields naming.Fields, remoteID, relPath string,
) error {
	sidecar := Sidecar{
		SchemaVersion:   SidecarVersion,
		Platform:        channel.Platform,
		Channel:         channel.Name,
		Author:          fields.Author,
		Title:           fields.Title,
		Category:        fields.Category,
		StartedAt:       recording.StartedAt,
		EndedAt:         recording.EndedAt,
		DurationMS:      recording.Duration.Milliseconds(),
		MediaDurationMS: recording.MediaDuration.Milliseconds(),
		Bytes:           recording.Bytes,
		Origin:          string(recording.Origin),
		RemoteID:        remoteID,
	}
	if recording.MutedDuration != nil {
		muted := recording.MutedDuration.Milliseconds()
		sidecar.MutedMS = &muted
	}

	if recording.BroadcastID != nil {
		history, err := o.store.TitleHistory(*recording.BroadcastID)
		if err != nil {
			return err
		}
		for _, observation := range history {
			sidecar.TitleHistory = append(sidecar.TitleHistory, SidecarTitle{
				ObservedAt: observation.ObservedAt,
				Title:      observation.Title,
				Category:   observation.Category,
			})
		}
	}

	gaps, err := o.store.Gaps(recording.ID)
	if err != nil {
		return err
	}
	for _, gap := range gaps {
		sidecar.Gaps = append(sidecar.Gaps, SidecarGap{
			StartMS: gap.Start.Milliseconds(),
			EndMS:   gap.End.Milliseconds(),
			Reason:  gap.Reason,
			Filled:  gap.FilledAt != nil,
		})
	}

	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding sidecar for recording %d: %w", recording.ID, err)
	}
	data = append(data, '\n')

	media := o.library.RelPath(relPath)
	path := SidecarPath(media)
	if err := refuseSelfOverwrite(media, path); err != nil {
		return fmt.Errorf("writing the sidecar for recording %d: %w", recording.ID, err)
	}
	if err := writeSidecarFile(ctx, path, data, 0o644); err != nil {
		return fmt.Errorf("writing sidecar %s: %w", path, err)
	}
	return nil
}

// SidecarPath returns the sidecar path for a media file.
//
// The suffix is appended rather than substituted for the extension, so the
// sidecar can never land on the recording it describes and two recordings
// that differ only in container keep separate sidecars.
func SidecarPath(mediaPath string) string {
	return mediaPath + sidecarSuffix
}

// refuseSelfOverwrite rejects a sidecar path that names the media file it
// describes.
//
// Writing it truncates the recording, replaces it with a few hundred bytes
// of its own metadata, and reports success. The comparison ignores case
// because a filesystem that does reaches the same file either way.
func refuseSelfOverwrite(mediaPath, sidecarPath string) error {
	// Compared on the resolved paths rather than as text. SidecarPath
	// always appends a suffix, so two strings of different lengths can
	// never be equal and the guard could not fire on anything: it read as
	// a check on the one path that would truncate a recording, and was
	// not one. os.SameFile answers what the filesystem thinks, which is
	// what a link or a second spelling of one file would hide.
	if strings.EqualFold(mediaPath, sidecarPath) {
		return fmt.Errorf("the sidecar path is the recording at %s", mediaPath)
	}

	// A path that is not there cannot be the same file as another, and this
	// runs before the sidecar exists on every ordinary finalize. The
	// absence is the answer, not a failure to get one.
	media, mediaOK := statOf(mediaPath)
	sidecar, sidecarOK := statOf(sidecarPath)
	if mediaOK && sidecarOK && os.SameFile(media, sidecar) {
		return fmt.Errorf("the sidecar path is the recording at %s", mediaPath)
	}
	return nil
}
