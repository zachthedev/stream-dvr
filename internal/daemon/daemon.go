// Package daemon supervises recording: it watches channels, admits
// captures against the disk budget, finalizes what it captured, and keeps a
// heartbeat so an outage is visible rather than silent.
//
// The heartbeat is not decoration. A recorder that dies and stays dead
// looks exactly like a channel that never went live, and the difference is
// only recoverable if something wrote down that the recorder was supposed
// to be running. Each session records its start, a periodic heartbeat, and
// a stop time on clean shutdown. The next start compares against the
// previous heartbeat and reports the gap.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"slices"
	"sync"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/post"
	"zach.tools/go/stream-dvr/internal/record"
	"zach.tools/go/stream-dvr/internal/space"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Finalizer turns a finished capture into a library file.
// *organize.Organizer satisfies it.
type Finalizer interface {
	Finalize(ctx context.Context, recordingID int64) (organize.Outcome, error)
	// Release deletes a recording the operator purged, once its grace
	// period has expired. It refuses anything not already in the trash,
	// which is what keeps the daemon unable to delete on its own.
	Release(ctx context.Context, recordingID int64) error
	// Recompress re-encodes a recording in place through encode, which is
	// what actually drives the encoder.
	Recompress(ctx context.Context, recordingID int64, keepOriginal bool,
		encode func(ctx context.Context, source, output string) error) error
}

// Recompressor is what the daemon needs to re-encode a recording.
//
// It is a separate interface from Finalizer because the answer to "what can
// this machine encode with" is about hardware rather than about the
// library.
type Recompressor interface {
	// Encoders lists what this machine can actually encode with, probed
	// rather than assumed.
	Encoders(ctx context.Context) ([]post.Encoder, error)
	// Transcode re-encodes source into output.
	Transcode(ctx context.Context, source, output string, encoder post.Encoder, quality int) error
}

// Daemon supervises the recording loop.
type Daemon struct {
	config    config.Config
	library   *library.Library
	store     *store.Store
	engine    record.Engine
	finalizer Finalizer
	encoder   Recompressor
	// credential asks the provider whether the stored credential still
	// works.
	credential func(ctx context.Context) error
	// broadcastStart reports when a broadcast the recorder joined part way
	// through actually began. Nil, or an answer of false, leaves the moment
	// the poll saw the channel as the anchor.
	broadcastStart func(ctx context.Context, channelURL, streamID string) (time.Time, bool)
	// liveMetadata reports what the platform says about a channel's current
	// broadcast, for a probe that carried no metadata of its own.
	liveMetadata func(ctx context.Context, channelURL string) (LiveBroadcast, bool)
	// recovery fetches what the recorder missed and reports what it did.
	// Nil turns automatic recovery off.
	recovery func(ctx context.Context, round Round) (RoundResult, error)
	// started announces that the claim is held and recording has begun.
	started  func()
	notifier Notifier
	logger   *slog.Logger

	// now and freeSpace are injected so the supervisor's decisions can be
	// tested without a clock or a real volume.
	now       func() time.Time
	freeSpace func(string) (int64, error)

	// captureSlots bounds simultaneous recordings. Run allocates it. A nil
	// channel means unbounded, which is what direct RunCycle callers get.
	captureSlots chan struct{}

	// spaceInterval is how often a running capture rechecks the budget.
	// It is a field so a test need not wait out the production cadence.
	spaceInterval time.Duration

	// idleSpaceInterval is how often the fill level is evaluated with no
	// capture running. A field for the same reason as spaceInterval.
	idleSpaceInterval time.Duration

	// heartbeatInterval is how often liveness is recorded. A field for the
	// same reason as spaceInterval.
	heartbeatInterval time.Duration

	// captureStall is how long a capture's output may go without growing
	// before the capture is ended. A field for the same reason as
	// spaceInterval.
	captureStall time.Duration

	// session is the daemon_sessions row this process is beating into, and
	// sessionMu guards it. The heartbeat loop replaces the row when a freeze
	// makes the old one a lie, so every holder reads it here rather than
	// keeping the id Run started with.
	sessionMu sync.Mutex
	session   int64

	// sessions holds what the daemon remembers about each channel that is
	// currently broadcasting. Keying by channel bounds it by the number of
	// channels configured rather than by uptime.
	//
	// capturing names the recordings this process has an engine writing to
	// right now, under the same lock. It is what tells a row that is really
	// being written from one a crash stranded, which look identical in the
	// database.
	sessionsMu sync.Mutex
	sessions   map[int64]*liveSession
	capturing  map[int64]bool

	// credentialMu guards checkedAt, which is what keeps a burst of
	// channels all failing on the same dead token from becoming a burst of
	// validation requests against the provider. It guards the degraded
	// latch beside it for the same reason: both are read from every
	// channel's goroutine.
	credentialMu sync.Mutex
	checkedAt    time.Time
	// credentialDegraded holds from a rejection until a credential validates
	// again, and credentialReportedAt is when the operator was last told.
	//
	// The state lives here rather than being re-derived from the file
	// because the rejection deletes the file, and an absent one is
	// indistinguishable from a fresh install once it is gone.
	credentialDegraded   bool
	credentialReportedAt time.Time

	// sweepFailures counts how many sweeps in a row each recording has
	// failed, so one that never finishes is reported rather than warned
	// about every quarter hour forever.
	sweepMu       sync.Mutex
	sweepFailures map[int64]int

	// recoveryMu guards the pending round and the reachability the loop
	// judges it against. Every field under it is written from a watcher's
	// goroutine, one per channel, and read from the recovery loop.
	//
	// recoveryFrom is the earliest instant the pending round must cover and
	// recoveryWhy says what asked for it, which is also what says whether
	// there is one: an empty reason means no round is queued.
	//
	// outages dates each channel's current run of unanswered probes, and a
	// channel absent from it answered its last one. probed holds that last
	// answer per channel alongside the platform it was put to, and a
	// channel absent from it has not been probed at all, which is a
	// different thing from being unreachable.
	//
	// All three are keyed by channel, so they are bounded by how many are
	// configured rather than by how long the daemon has been up.
	recoveryMu   sync.Mutex
	recoveryFrom time.Time
	recoveryWhy  string
	// recoveryMissingSince is the earliest instant a measured gap left
	// uncovered, and zero means nothing measured one: the routine round
	// asks for as far back as it may reach rather than for a stretch
	// anybody watched go missing.
	//
	// An instant rather than a length, and coalesced by its own earliest
	// rather than alongside the window that wins. The request is clamped to
	// the horizon and widened by the margin before it is queued, so neither
	// the loss nor the fact that there was one survives in it. Comparing
	// where the round ends up against where coverage stopped is what states
	// the trim exactly, and it holds whichever request the coalescing kept.
	// recoveryMissingWhy is what measured it, and travels with the instant
	// rather than with the window, because the warning the pair feeds is
	// about the measurement.
	recoveryMissingSince time.Time
	recoveryMissingWhy   string
	outages              map[int64]time.Time
	probed               map[int64]probe
	// roundRanAt is when the last round finished, and zero means none has.
	// It is what paces rounds apart, so a link that keeps dropping and
	// returning cannot drive one per drop.
	roundRanAt time.Time
	// roundBackoff is the least time before the next round, doubled each
	// time one fails and reset when one comes back clean.
	roundBackoff time.Duration
	// heldWhy is what is currently stopping a queued round, heldSince is
	// when rounds started being held, and heldReported is when the operator
	// was last told. Reported again on an interval rather than once, since a
	// hold that never clears is exactly the one an operator most needs
	// reminding of.
	heldWhy      string
	heldSince    time.Time
	heldReported time.Time

	// roundMu guards roundCancel, which stops the round now in flight and
	// is nil when there is none.
	//
	// A mutex of its own rather than recoveryMu, because the caller that
	// needs it is noteCapturing, which already holds sessionsMu. Reaching
	// for recoveryMu there would nest two locks that every other path takes
	// one after the other.
	roundMu     sync.Mutex
	roundCancel context.CancelFunc
	// recoveryWake releases the loop between ticks, so a round asked for by
	// an outage that just ended does not wait out the routine cadence.
	recoveryWake chan struct{}

	// recoveryInterval is how often a round runs with nothing having gone
	// wrong. A field for the same reason as spaceInterval.
	recoveryInterval time.Duration
}

// liveSession is what the daemon remembers about a channel for as long as
// it keeps broadcasting.
//
// A poll that re-derives everything treats each look at one broadcast as a
// fresh discovery, and the decisions it makes drift apart from the ones it
// made a poll ago.
type liveSession struct {
	// startedAt anchors the broadcast, so every poll of it reaches the same
	// row. A start that moves with the clock stops matching the row it
	// created once it drifts past the store's overlap window.
	startedAt time.Time
	// streamID is the live session the platform reported when this one
	// opened. A different one is a different broadcast, however continuous
	// the channel looked.
	streamID string
	// broadcastID is the row this session writes to, so the poll that finds
	// the channel offline knows which broadcast to close out. Zero until the
	// first poll of the session has upserted one.
	broadcastID int64
	// refusedFor is the broadcast the operator was last told the library
	// was too full for, and refused says whether there is one. A live
	// channel against a full library refuses on every poll, and the history
	// explaining how it filled is worth more than thousands of copies of
	// the consequence.
	refusedFor int64
	refused    bool
	// haltedFor is the broadcast whose capture the watermark stopped before
	// it was long enough to keep, and halted says whether there is one.
	haltedFor int64
	halted    bool
	// bitrate estimates the channel's byte rate, and bitrateKnown says
	// whether it has been derived yet. Only a completed recording changes
	// the answer and this channel is still broadcasting, so deriving it
	// once per session spares months of rows on every poll.
	bitrate      int64
	bitrateKnown bool
}

// Options supplies a Daemon's collaborators.
type Options struct {
	Config    config.Config
	Library   *library.Library
	Store     *store.Store
	Engine    record.Engine
	Finalizer Finalizer
	// Recompressor drives the re-encode. Nil disables the rung outright,
	// whatever the configuration says.
	Recompressor Recompressor
	// Credential checks whether the stored credential still works, and
	// reports what it did. Nil turns the loop off.
	//
	// It is a function rather than an interface because the daemon's part
	// is only deciding when to ask. What asking means belongs to the
	// provider package, which the daemon must not import.
	Credential func(ctx context.Context) error
	// BroadcastStart reports when a broadcast actually began, for a channel the
	// recorder joined part way through. False means nobody could answer, and
	// the moment the poll saw the channel stands as the anchor.
	//
	// streamID is the live session the probe named. It is passed so an answer
	// describing a different session can be refused: a channel that ends one
	// broadcast and starts another between the probe and the lookup would
	// otherwise anchor this row to the wrong one.
	//
	// A function for the same reason as Recover: answering means asking a
	// platform API and a download tool, and the daemon must import neither.
	BroadcastStart func(ctx context.Context, channelURL, streamID string) (time.Time, bool)
	// LiveMetadata reports the title and category the platform holds for a
	// channel's current broadcast. False means nobody could answer, which
	// leaves whatever the probe carried.
	//
	// It exists because the download tool is one source of a title and not
	// the authoritative one. A probe that answers with an empty metadata
	// block leaves a capture with no title, and a recording with no title
	// is never named into the library.
	//
	// It answers what identifies the broadcast as well as what describes
	// it, because whether this is the session being captured is the
	// recorder's question: it holds the row, and this reports what the
	// platform sees.
	//
	// A function for the same reason as BroadcastStart: answering means
	// asking a platform API, and the daemon must not import one.
	LiveMetadata func(ctx context.Context, channelURL string) (LiveBroadcast, bool)
	// Recover fetches everything the recorder missed since the given
	// instant. Nil turns automatic recovery off, which leaves the backfill
	// command as the only way to fill a gap.
	//
	// The round carries the claim this process holds at call time rather
	// than one captured here, because a freeze mid-run replaces the row and
	// a fetch recorded against a closed session leaves a claim nothing will
	// release.
	//
	// A function rather than a dependency, so the daemon decides when a
	// round runs without depending on the engine that runs it. What a round
	// does belongs to the recovery packages, and the wiring that joins the
	// two lives with the command.
	//
	// It reports what the round achieved, so the log says what a round did
	// rather than only that it ended, and so a round whose work all failed
	// keeps the window it was covering instead of consuming it.
	Recover func(ctx context.Context, round Round) (RoundResult, error)
	// Started runs once the library is claimed and before any watcher does.
	// It is where a caller announces that recording began, because until
	// the claim is taken there is nothing to announce: a run refused
	// because another recorder holds the library gets this far and no
	// further. Nil skips it.
	Started   func()
	Notifier  Notifier
	Logger    *slog.Logger
	Now       func() time.Time
	FreeSpace func(string) (int64, error)
}

// Downtime describes the gap between two sessions.
type Downtime struct {
	// Since is when the previous session was last known alive.
	Since time.Duration
	// Crashed reports that the previous session never recorded a clean
	// stop, which is what an unnoticed outage looks like.
	Crashed bool
}

// ///////////////////////////////////////////////
// Channels
// ///////////////////////////////////////////////

// Target pairs a configured channel with its stored row.
type Target struct {
	// Entry is the channel as configured.
	Entry config.Channel
	// Channel is its database row.
	Channel store.Channel
}

// ///////////////////////////////////////////////
// Run
// ///////////////////////////////////////////////

// unclaimedFile is a file in the incoming directory that no recording row
// names.
type unclaimedFile struct {
	// relPath is the file's library-relative path, in the forward-slash
	// form the store holds.
	relPath string
	bytes   int64
	modTime time.Time
}

// heartbeatInterval is how often the daemon records that it is alive. It
// bounds how much of an outage goes unattributed: a crash is only
// measurable back to the last heartbeat.
const heartbeatInterval = time.Minute

// strandedAfter is how old a capturing row has to be before a sweep will
// treat it as one a crash left behind.
//
// The window it covers is between a row reaching the database and the
// capture registering it in memory, which is microseconds. A minute is
// several orders of magnitude above that and far below the age of anything
// a real crash leaves, so it separates the two without a lock held across
// a write.
const strandedAfter = time.Minute

// maxPlausibleDowntime is the longest outage a stored session may describe
// before the row is read as corrupt rather than as a measurement.
//
// Not a limit on recovery, which the horizon bounds, and not a threshold
// anything acts on. It exists so the length quoted to an operator is one a
// recorder could have been down for. A zeroed timestamp column decodes to
// 1970 and reports fifty-six years, which reads as a bug rather than as the
// outage it stands for. Set far past any outage worth measuring, so a
// machine that spent years switched off is still reported as it was.
const maxPlausibleDowntime = 10 * 365 * 24 * time.Hour

// StaleAfter is how long a recorder may go silent before another may claim
// the library.
//
// Shorter double-starts a daemon the operating system paused, which is what
// a laptop lid does. Longer blocks recovery after a crash for no gain: the
// row is already there and nothing is going to clean it up.
//
// Exported because a command that claims the library asks the same question
// this claim asks. Two constants for one invariant drift apart, and a
// command that called a library free while StartSession still held it would
// refuse for a reason nothing on screen explains.
const StaleAfter = 3 * heartbeatInterval

// credentialFloor is the least time between two credential checks.
//
// A dead token fails every channel at once, so without a floor a
// twenty-channel config would send twenty validation requests inside a
// second. One is enough to learn the answer.
const credentialFloor = time.Minute

// credentialInterval is how often the stored credential is rechecked.
//
// Twitch documents hourly as the requirement for an application
// maintaining an OAuth session, so this is compliance rather than a guess.
const credentialInterval = time.Hour

// credentialReportInterval is how often a credential that stays dead is
// reported again.
//
// It never resolves on its own: every recording made until the operator
// replaces it is captured at public quality. Saying so once relies on one
// best-effort delivery, and hourly would be the flood that teaches the
// operator to skip the line. Daily is what carries a condition that is still
// costing something a week later.
const credentialReportInterval = 24 * time.Hour

// sweepInterval is how often parked recordings are retried.
const sweepInterval = 15 * time.Minute

// missingSweepInterval is how often rows are reconciled against the volume.
//
// It stats every complete recording, and what it is looking for is an
// operator deleting files by hand, which happens on a human timescale. A
// library of thousands of recordings pays for this pass, so it is rare.
const missingSweepInterval = 6 * time.Hour

// reasonFileGone is what a broadcast's refused fetch row says once its last
// recording's file left the volume without the purge that would explain it.
const reasonFileGone = "the operator deleted every recording of this broadcast"

// abandonedAfter is how long a file in the incoming directory that no
// recording row names may sit there before it is removed.
//
// It is longer than any download this program starts, so nothing in flight
// is ever inside it. A shorter threshold would delete a fetch that is still
// running, which is the one mistake this sweep must not make.
const abandonedAfter = 24 * time.Hour

// recompressInterval is how often recordings past their age are re-encoded.
//
// Slow, because the work it schedules runs for hours and the thing it reads
// changes over weeks. A pass that found nothing on the last tick will not
// find much on the next one either.
const recompressInterval = 6 * time.Hour

// idleSpaceInterval is how often the library's fill level is evaluated
// with no capture running.
//
// It is far slower than the watermark a capture carries, because nothing
// between broadcasts writes at a stream's rate: the library moves when a
// finalize lands or the operator deletes something. What this cadence
// buys is that the level is known at all while the recorder sits idle.
const idleSpaceInterval = 5 * time.Minute

// New returns a Daemon, filling in defaults for anything not supplied.
func New(opts Options) (*Daemon, error) {
	if opts.Library == nil {
		return nil, fmt.Errorf("daemon needs a library")
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("daemon needs a store")
	}
	if opts.Engine == nil {
		return nil, fmt.Errorf("daemon needs a capture engine")
	}
	if opts.Finalizer == nil {
		return nil, fmt.Errorf("daemon needs a finalizer")
	}

	daemon := &Daemon{
		config:         opts.Config,
		library:        opts.Library,
		store:          opts.Store,
		engine:         opts.Engine,
		finalizer:      opts.Finalizer,
		encoder:        opts.Recompressor,
		credential:     opts.Credential,
		broadcastStart: opts.BroadcastStart,
		liveMetadata:   opts.LiveMetadata,
		recovery:       opts.Recover,
		started:        opts.Started,
		notifier:       opts.Notifier,
		logger:         opts.Logger,
		now:            opts.Now,
		freeSpace:      opts.FreeSpace,
	}
	if daemon.notifier == nil {
		daemon.notifier = DiscardNotifier{}
	}
	if daemon.logger == nil {
		daemon.logger = slog.New(slog.DiscardHandler)
	}
	if daemon.now == nil {
		daemon.now = time.Now
	}
	if daemon.freeSpace == nil {
		daemon.freeSpace = space.Free
	}
	daemon.outages = make(map[int64]time.Time)
	daemon.probed = make(map[int64]probe)
	daemon.recoveryWake = make(chan struct{}, 1)
	daemon.spaceInterval = spaceCheckInterval
	daemon.idleSpaceInterval = idleSpaceInterval
	daemon.heartbeatInterval = heartbeatInterval
	daemon.recoveryInterval = recoveryInterval
	daemon.captureStall = captureStallLimit
	return daemon, nil
}

// Run supervises recording until ctx is cancelled.
//
// It returns only once every watcher has stopped, so a caller that cancels
// on a signal knows the daemon is finished rather than merely asked to
// finish.
func (d *Daemon) Run(ctx context.Context) (err error) {
	// The library is claimed first, so a start refused because another
	// recorder holds it writes nothing at all. Registering channels before
	// the claim would leave a refused start having edited the database.
	session, downtime, err := d.StartSession(ctx)
	if err != nil {
		return err
	}
	d.setSession(session.ID)
	// Announced here rather than by the caller, because this is the first
	// point at which recording is a fact. A refused start returns above
	// this line and says nothing that suggests otherwise.
	if d.started != nil {
		d.started()
	}
	// The session is closed however Run ends, and with a live context, so a
	// cancelled run and a failed startup both still distinguish themselves
	// from a crash next time. The id is read back rather than captured,
	// because a freeze mid-run replaces the row this is closing.
	defer func() { err = d.recordStop(d.currentSession(), err) }()

	if downtime != nil {
		if downtime.Crashed {
			d.logger.WarnContext(ctx, "previous session ended without a clean shutdown",
				slog.Duration("down_for", downtime.Since))
		}
		// Queued before any watcher runs, so the first answered probe
		// releases it. Nothing is fetched yet: the round waits until the
		// platform has proved reachable.
		d.requestDowntimeRecovery(ctx, *downtime, session.StartedAt)
	}

	targets, err := d.SyncChannels()
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		d.logger.WarnContext(ctx, "no channels are enabled, nothing to record")
	}

	// Interrupted recordings are reconciled before any watcher runs, so the
	// first sweep already sees them and no capture starts alongside a row
	// that still claims one is in flight.
	if _, reconcileErr := d.reconcileInterrupted(ctx); reconcileErr != nil {
		return reconcileErr
	}

	// Straight after the reconcile, so every row that is going to name a
	// file already does and the sweep judges against the full set. A
	// directory it cannot read is not a reason to stop recording.
	if _, sweepErr := d.SweepIncoming(ctx); sweepErr != nil && ctx.Err() == nil {
		d.logger.WarnContext(ctx, "the incoming directory could not be swept", slog.Any("error", sweepErr))
	}

	d.captureSlots = make(chan struct{}, max(d.config.Capture.MaxConcurrent, 1))

	var wg sync.WaitGroup
	wg.Go(func() { d.heartbeatLoop(ctx) })
	wg.Go(func() { d.sweepLoop(ctx) })
	wg.Go(func() { d.idleSpaceLoop(ctx) })
	wg.Go(func() { d.recompressLoop(ctx) })
	wg.Go(func() { d.credentialLoop(ctx) })
	wg.Go(func() { d.recoveryLoop(ctx) })
	for _, target := range targets {
		wg.Go(func() { d.Watch(ctx, target.Entry, target.Channel) })
	}
	wg.Wait()
	return nil
}

// reconcileInterrupted moves every recording still marked capturing into
// the pending queue, and reports how many it recovered.
//
// A row stays capturing for the whole broadcast and leaves that state only
// when the capture returns, so a power loss, a kill, or a reboot part way
// through a broadcast leaves it there with a playable capture sitting in
// the incoming directory. No sweep looks at capturing, so the recording is
// stranded and its bytes never count against the disk budget. Every such
// row is stale by definition here: this process has not started a capture
// yet.
func (d *Daemon) reconcileInterrupted(ctx context.Context) (int, error) {
	interrupted, err := d.store.RecordingsByState(store.StateCapturing)
	if err != nil {
		return 0, err
	}

	recovered := 0
	for _, recording := range interrupted {
		if ctx.Err() != nil {
			return recovered, ctx.Err()
		}
		if err := d.recoverInterrupted(ctx, recording); err != nil {
			// One unreadable row must not stop the daemon recording
			// everything else.
			d.logger.ErrorContext(ctx, "could not recover an interrupted recording",
				slog.Int64("recording", recording.ID),
				slog.String("path", recording.Path),
				slog.Any("error", err))
			continue
		}
		recovered++
	}
	return recovered, nil
}

// recoverInterrupted files one interrupted recording for the sweep, or
// fails it when no bytes survived.
func (d *Daemon) recoverInterrupted(ctx context.Context, recording store.Recording) error {
	info, err := os.Stat(d.library.RelPath(recording.Path))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A volume that has not come back yet is not a recording that
		// failed. The row keeps its state and the next start retries it.
		return err
	}
	if err != nil || info.Size() == 0 {
		d.logger.WarnContext(ctx, "an interrupted recording left no bytes",
			slog.Int64("recording", recording.ID), slog.String("path", recording.Path))
		return d.store.SetState(recording.ID, store.StateFailed)
	}

	// The last write to the capture is when the recorder stopped, and it is
	// the only evidence the interruption left behind. The size matters as
	// much as the state: a row still holding the zero bytes it was created
	// with makes the library read smaller than it is, and the size cap
	// drifts further with every crash.
	// Clamped forward, because a file whose mtime precedes the row's start
	// is what a clock step or a restored volume leaves behind, and the store
	// refuses that end inside its own transaction. Left unclamped the row
	// stays capturing across every restart and only this log line ever
	// mentions it.
	endedAt := laterOf(info.ModTime().UTC(), recording.StartedAt)
	duration := max(endedAt.Sub(recording.StartedAt), 0)

	if err := d.store.FinishRecording(recording.ID, store.StateAwaitingFinalize,
		recording.Path, info.Size(), duration, endedAt); err != nil {
		return err
	}
	d.logger.InfoContext(ctx, "recovered a recording interrupted by an unclean exit",
		slog.Int64("recording", recording.ID),
		slog.String("path", recording.Path),
		slog.Int64("bytes", info.Size()))
	return nil
}

// unclaimedIncoming lists the files in the incoming directory that no
// recording row names.
//
// Both readers of the incoming directory go through it, so a file counted
// against the budget and a file eligible for removal are decided by one
// rule. The directory is flat: every writer aimed at it, the capture engine
// and both download paths, is told exactly one path to write.
func (d *Daemon) unclaimedIncoming() ([]unclaimedFile, error) {
	entries, err := os.ReadDir(paths.IncomingDir(d.library.Root()))
	if errors.Is(err, os.ErrNotExist) {
		// No capture has ever run against this library.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the incoming directory: %w", err)
	}

	recorded, err := d.store.RecordingPaths()
	if err != nil {
		return nil, err
	}
	named := make(map[string]bool, len(recorded))
	for _, recording := range recorded {
		named[path.Base(recording)] = true
	}

	var unclaimed []unclaimedFile
	for _, entry := range entries {
		if entry.IsDir() || named[entry.Name()] {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			// A capture that finished between the listing and this stat.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("sizing %s: %w", entry.Name(), err)
		}
		unclaimed = append(unclaimed, unclaimedFile{
			relPath: path.Join(paths.IncomingDirName, entry.Name()),
			bytes:   info.Size(),
			modTime: info.ModTime(),
		})
	}
	return unclaimed, nil
}

// SweepIncoming removes the files in the incoming directory that no
// recording row names and that nothing can still be writing, and reports how
// many it deleted.
//
// A download charged terminal leaves its partial file behind, and nothing
// else in the tree removes one. The size cap cannot see those bytes, so the
// library overruns max_size while reporting room, and the operator never
// sees the files because they sit under a name no listing shows. The same
// shape covers a .recompressing left by a kill and a stranded remux output.
//
// The age threshold is what makes this safe: it is longer than any download
// this program starts, so a file young enough to be in flight is left alone
// whether or not a row names it yet.
func (d *Daemon) SweepIncoming(ctx context.Context) (int, error) {
	unclaimed, err := d.unclaimedIncoming()
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, file := range unclaimed {
		if ctx.Err() != nil {
			return removed, ctx.Err()
		}
		if d.now().Sub(file.modTime) < abandonedAfter {
			continue
		}
		if err := os.Remove(d.library.RelPath(file.relPath)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// One file that will not delete is not a reason to leave the
			// rest, and it is a fact the operator needs.
			d.logger.WarnContext(ctx, "an abandoned file in the incoming directory could not be removed",
				slog.String("path", file.relPath), slog.Any("error", err))
			continue
		}
		removed++
		d.logger.InfoContext(ctx, "removed an abandoned file nothing owns",
			slog.String("path", file.relPath),
			slog.String("bytes", config.Size(file.bytes).String()))
	}
	return removed, nil
}

// SweepMissing moves recordings whose files are gone out of the size cap's
// reckoning, and reports how many it moved.
//
// Nothing else reconciles a row against the volume. An operator who deletes
// a year of recordings in a file manager leaves rows summing to the whole
// cap, so every broadcast after that is refused with "library max_size
// would be breached" while the disk shows a terabyte free. Recording stops
// permanently, loudly, for a reason that contradicts what the operator can
// see.
//
// The whole sweep is guarded on the library still being readable, because
// an unmounted share and an emptied library look identical from a stat and
// only one of them is a reason to rewrite every row.
func (d *Daemon) SweepMissing(ctx context.Context) (int, error) {
	if err := d.library.Verify(); err != nil {
		return 0, fmt.Errorf("reconciling recordings against the volume: %w", err)
	}

	recordings, err := d.store.RecordingsByState(store.StateComplete)
	if err != nil {
		return 0, err
	}

	swept := 0
	for _, recording := range recordings {
		if ctx.Err() != nil {
			return swept, ctx.Err()
		}
		if _, err := os.Stat(d.library.RelPath(recording.Path)); !errors.Is(err, os.ErrNotExist) {
			// Present, or unreadable for a reason that is not absence. A
			// permission error says nothing about whether the file is there.
			continue
		}
		if err := d.store.SetState(recording.ID, store.StateMissing); err != nil {
			d.logger.WarnContext(ctx, "could not record that a recording's file is gone",
				slog.Int64("recording", recording.ID), slog.Any("error", err))
			continue
		}
		swept++
		d.logger.WarnContext(ctx, "a recording's file is gone from the library",
			slog.Int64("recording", recording.ID),
			slog.String("path", recording.Path),
			slog.String("was_bytes", config.Size(recording.Bytes).String()))
		d.refuseFetchIfNothingLeft(ctx, recording.BroadcastID)
	}
	return swept, nil
}

// refuseFetchIfNothingLeft marks a broadcast unfetchable once none of its
// recordings holds bytes any more.
//
// A file the operator deleted must not come back as the platform's muted
// copy on the next recovery pass. Losing one of a broadcast's two files says
// nothing, so the check is over the whole broadcast rather than over the row
// that just changed.
func (d *Daemon) refuseFetchIfNothingLeft(ctx context.Context, broadcastID *int64) {
	if broadcastID == nil {
		return
	}

	recordings, err := d.store.RecordingsForBroadcast(*broadcastID)
	if err != nil {
		d.logger.WarnContext(ctx, "could not read a broadcast's recordings",
			slog.Int64("broadcast", *broadcastID), slog.Any("error", err))
		return
	}
	for _, recording := range recordings {
		if store.HoldsBytes(recording.State) {
			return
		}
	}

	if err := d.store.RefuseFetch(*broadcastID, reasonFileGone, d.now().UTC()); err != nil {
		d.logger.WarnContext(ctx, "could not record that a broadcast must not be fetched again",
			slog.Int64("broadcast", *broadcastID), slog.Any("error", err))
	}
}

// heartbeatLoop records liveness until ctx is cancelled, opening a new
// session whenever a beat lands late enough to prove the recorder was not
// running in between.
//
// One heartbeat column means an interruption inside a session leaves no
// trace of itself: the next beat overwrites the last honest one, and
// coverage reads the whole span as watched. A process frozen by sleep,
// hibernate, or a suspended VM resumes and beats into the same row, so a
// desktop that sleeps overnight reports every slept day as "the recorder was
// running and nothing aired".
func (d *Daemon) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(d.heartbeatInterval)
	defer ticker.Stop()

	beat := d.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		beat = d.beat(ctx, beat)
	}
}

// beat records one heartbeat and returns the moment it was taken.
//
// It reopens the session first when the stretch since the previous beat is
// long enough to count as time the recorder was not running, so the gap
// belongs to neither session rather than being covered by the beat about to
// land.
//
// It is separate from the loop because this is the whole decision, and the
// loop around it is only a ticker. A test drives this directly and settles
// what a freeze does without racing a clock.
func (d *Daemon) beat(ctx context.Context, previous time.Time) time.Time {
	if at := d.now(); at.Sub(previous) > d.freezeAfter() {
		d.reopenSession(ctx, previous, at)
	}

	at := d.now()
	if err := d.Heartbeat(d.currentSession()); err != nil {
		// A beat that does not land means this process may no longer hold
		// the library: the store answers ErrNotFound for a session row
		// that is gone, which is what a rebuilt database leaves behind.
		// Logging once a minute forever would leave the recorder writing
		// into a library it does not own, so the claim is taken again.
		d.logger.ErrorContext(ctx, "heartbeat failed", slog.Any("error", err))
		if errors.Is(err, store.ErrNotFound) {
			d.reclaim(ctx, at)
		}
	}
	return at
}

// reclaim takes the library again after a heartbeat found no session to
// write to.
//
// A refusal is the honest outcome and is left to the next beat: another
// recorder holding the library is exactly what the claim is for, and this
// process has no standing to take it from them.
func (d *Daemon) reclaim(ctx context.Context, at time.Time) {
	session, err := d.store.StartSession(at, StaleAfter)
	if err != nil {
		d.logger.ErrorContext(ctx, "could not take the library again after a heartbeat found no session",
			slog.Any("error", err))
		return
	}
	d.logger.WarnContext(ctx, "took the library again after a heartbeat found no session",
		slog.Int64("session", session.ID))
	d.setSession(session.ID)
}

// freezeAfter is how late a beat has to be before the stretch it spans
// counts as time the recorder was not running.
//
// Three intervals, so an ordinary scheduling delay is never mistaken for an
// outage. It is derived from the interval in force rather than from the
// constant, so the two cannot disagree.
func (d *Daemon) freezeAfter() time.Duration {
	return 3 * d.heartbeatInterval
}

// reopenSession closes the frozen session at its last honest beat and opens
// a new one, so the stretch between them belongs to neither.
//
// The stop is stamped at the last beat rather than at now, because the stop
// also moves the heartbeat and stamping it now would cover the very stretch
// this exists to expose.
//
// A failure leaves the old session in place. That overstates coverage for
// the frozen stretch, which is the state this was already in, and it is
// better than a recorder that stops beating at all.
func (d *Daemon) reopenSession(ctx context.Context, beat, at time.Time) {
	d.logger.WarnContext(ctx, "the recorder was not running, so nothing was watched",
		slog.Duration("for", at.Sub(beat)))
	d.notify(ctx, Event{
		Kind: EventDowntime,
		Detail: fmt.Sprintf("the recorder was frozen for %s, so anything broadcast in that time was missed",
			roundDuration(at.Sub(beat))),
	})

	// Queued before the reopen rather than after it, because the freeze
	// happened whether or not the row can be corrected, and a reopen that
	// failed must not also cost the recovery. A frozen process is the one
	// gap a restart cannot report: this process kept its session, so the
	// next start measures its downtime from a heartbeat written seconds
	// ago and would ask for nothing.
	d.requestDowntimeRecovery(ctx, Downtime{Since: at.Sub(beat), Crashed: true}, at)

	session, err := d.store.ReopenSession(d.currentSession(), beat, at, StaleAfter)
	if err != nil {
		d.logger.ErrorContext(ctx, "could not reopen the session a freeze interrupted", slog.Any("error", err))
		return
	}
	d.setSession(session.ID)
}

// currentSession returns the session row this process is beating into.
func (d *Daemon) currentSession() int64 {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	return d.session
}

// setSession records which session row this process is beating into.
func (d *Daemon) setSession(sessionID int64) {
	d.sessionMu.Lock()
	defer d.sessionMu.Unlock()
	d.session = sessionID
}

// sweepLoop retries parked recordings until ctx is cancelled.
//
// The first sweep runs at once. Anything left pending has already waited
// out however long the recorder was down, and making it wait a further
// quarter of an hour serves nothing.
func (d *Daemon) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()

	var reconciled time.Time
	for {
		// Before the parked sweep, so a row this pass recovers is finalized
		// on the same tick rather than waiting out another interval.
		if _, err := d.SweepCapturing(ctx); err != nil && ctx.Err() == nil {
			d.logger.ErrorContext(ctx, "could not recover stranded recordings", slog.Any("error", err))
		}
		if _, err := d.SweepParked(ctx); err != nil && ctx.Err() == nil {
			d.logger.ErrorContext(ctx, "sweep failed", slog.Any("error", err))
		}

		// Far slower than the parked sweep, because it stats every complete
		// recording and what it looks for changes on the operator's
		// timescale rather than the recorder's. The zero time runs it on the
		// first pass, so a daemon starting after a manual deletion
		// reconciles at once.
		if at := d.now(); at.Sub(reconciled) >= missingSweepInterval {
			reconciled = at
			if _, err := d.SweepMissing(ctx); err != nil && ctx.Err() == nil {
				// A library that cannot be read is the ordinary state of an
				// unmounted share rather than a fault to escalate.
				d.logger.WarnContext(ctx, "recordings could not be reconciled against the volume",
					slog.Any("error", err))
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// recompressLoop re-encodes recordings past their age, one at a time.
//
// It does nothing at all unless the operator turned it on for this machine.
// A re-encode without a hardware encoder runs at under realtime, so a
// four-hour broadcast can cost six to sixteen hours of CPU, and that is a
// price only the machine's owner can agree to.
//
// The pass runs on a timer rather than after each capture, because it
// competes with capture for the same CPU and the same disk, and a broadcast
// that goes unrecorded costs more than a re-encode that waits.
func (d *Daemon) recompressLoop(ctx context.Context) {
	if !d.recompressEnabled() {
		return
	}

	ticker := time.NewTicker(recompressInterval)
	defer ticker.Stop()

	for {
		if err := d.RecompressPass(ctx); err != nil && ctx.Err() == nil {
			d.logger.ErrorContext(ctx, "recompress pass failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// credentialLoop asks whether the stored credential still works.
//
// Hourly, and once at startup, which is Twitch's own documented cadence for
// an application maintaining a session.
//
// THE RULE THIS LOOP EXISTS UNDER: it never touches a capture. streamlink
// already holds the playback token for a running session, and a recording
// in progress is worth more than credential hygiene. A dead credential is
// reported and the next capture records public quality. The capture in
// flight runs to its end.
func (d *Daemon) credentialLoop(ctx context.Context) {
	if d.credential == nil {
		return
	}

	ticker := time.NewTicker(credentialInterval)
	defer ticker.Stop()

	for {
		d.checkCredential(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// recheckCredential runs a check prompted by a failure rather than a timer.
//
// A refused credential yields no streams at all, so every poll until the
// hourly tick records nothing. Asking at once shortens that from an hour
// to a poll interval.
//
// The floor is what stops every channel asking. Nothing is lost by
// skipping: the channel that got there first is checking the same token.
func (d *Daemon) recheckCredential(ctx context.Context) {
	if d.credential == nil {
		return
	}

	d.credentialMu.Lock()
	if since := d.now().Sub(d.checkedAt); since < credentialFloor {
		d.credentialMu.Unlock()
		return
	}
	// The read and the stamp are one critical section, so two channels that
	// both find the floor clear cannot both go on to check.
	d.checkedAt = d.now()
	d.credentialMu.Unlock()

	d.checkCredential(ctx)
}

// checkCredential runs one check and reports a credential that has died.
//
// Only a refusal is reported. A network failure means the question could
// not be put, and treating that as a dead token would have a Twitch outage
// delete a working credential on every machine running this.
func (d *Daemon) checkCredential(ctx context.Context) {
	// Stamped before the request rather than after, so the floor is already
	// held while this check is in flight and a burst of channels failing on
	// one dead token finds it held.
	d.credentialMu.Lock()
	d.checkedAt = d.now()
	d.credentialMu.Unlock()

	err := d.credential(ctx)
	switch {
	case ctx.Err() != nil:
	case err == nil:
		// A credential that validates is the only thing that ends the
		// condition, and the only answer that tells a working install from
		// one whose token was deleted after a refusal.
		d.clearCredentialFailure()
	case errors.Is(err, ErrCredentialRejected):
		d.reportDeadCredential(ctx, true)
	case errors.Is(err, ErrCredentialAbsent):
		// The refusal deleted the file, so this is what the same dead
		// credential answers on every check after the first. A fresh install
		// answers the same way with nothing wrong, which is why it only
		// reports while the latch already holds.
		d.reportDeadCredential(ctx, false)
	default:
		// The question could not be put. Nothing is concluded from that.
		d.logger.WarnContext(ctx, "could not check the stored credential",
			slog.Any("error", err))
	}
}

// reportDeadCredential tells the operator that recordings are being made at
// public quality, on the first refusal and daily while it lasts.
func (d *Daemon) reportDeadCredential(ctx context.Context, refused bool) {
	if !d.latchCredential(d.now(), refused) {
		return
	}
	d.logger.ErrorContext(ctx, "the stored credential no longer works; "+
		"recordings will capture public quality until it is replaced")
	d.notify(ctx, Event{
		Kind:   EventCredentialDead,
		Detail: "run 'stream-dvr auth twitch' to store a new token",
	})
}

// latchCredential records a credential that does not work, and reports
// whether this is a moment to say so.
//
// refused says the caller learned it from a refusal, which is news whatever
// the latch held. An absence is news only while the latch already holds.
func (d *Daemon) latchCredential(at time.Time, refused bool) bool {
	d.credentialMu.Lock()
	defer d.credentialMu.Unlock()

	if !refused && !d.credentialDegraded {
		return false
	}
	if d.credentialDegraded && at.Sub(d.credentialReportedAt) < credentialReportInterval {
		return false
	}
	d.credentialDegraded, d.credentialReportedAt = true, at
	return true
}

// clearCredentialFailure forgets a condition a working credential ended, so
// the next one is reported from the start.
func (d *Daemon) clearCredentialFailure() {
	d.credentialMu.Lock()
	defer d.credentialMu.Unlock()
	d.credentialDegraded = false
}

// noteCapturing records that this process has an engine writing to a
// recording.
func (d *Daemon) noteCapturing(recordingID int64) {
	d.sessionsMu.Lock()
	if d.capturing == nil {
		d.capturing = make(map[int64]bool)
	}
	d.capturing[recordingID] = true
	d.sessionsMu.Unlock()

	// A round in flight yields to the capture that just began. It was
	// admitted against a library this recording is now filling, and the
	// check that would have held it off ran once, before it started. Its
	// window is queued again, so stopping costs the round's progress and
	// nothing else.
	//
	// Outside the lock, so the two mutexes are never held at once.
	d.stopRound()
}

// forgetCapturing records that a capture has ended, however it ended.
func (d *Daemon) forgetCapturing(recordingID int64) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	delete(d.capturing, recordingID)
}

// isCapturing reports whether this process is writing to a recording now.
func (d *Daemon) isCapturing(recordingID int64) bool {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	return d.capturing[recordingID]
}

// anyCapturing reports whether this process has an engine writing to any
// recording right now.
func (d *Daemon) anyCapturing() bool {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	return len(d.capturing) > 0
}

// SweepCapturing recovers rows left in the capturing state by a process that
// is no longer writing to them, and reports how many it moved.
//
// A row stays capturing for the whole broadcast and leaves that state only
// when the capture returns. A failure at that moment, a clock step among
// them, leaves the row there with a complete playable file in the incoming
// directory and nothing to move it: no sweep looks at the capturing state,
// and the start-up reconcile has already run. For a recorder that runs for
// months the recording is stranded for months, and the day it sits on paints
// at risk so recovery deliberately leaves it alone too.
//
// Only rows this process is not writing to are touched, which is what keeps
// it from finalizing a broadcast that is still recording.
func (d *Daemon) SweepCapturing(ctx context.Context) (int, error) {
	stranded, err := d.store.RecordingsByState(store.StateCapturing)
	if err != nil {
		return 0, err
	}

	recovered := 0
	for _, recording := range stranded {
		if ctx.Err() != nil {
			return recovered, ctx.Err()
		}
		if d.isCapturing(recording.ID) {
			continue
		}
		// The in-memory set is written just after the row, so a capture
		// that has created its row and not yet registered it reads as
		// stranded. Age settles what the map cannot: a row a crash left
		// behind was written before this process started, while one this
		// process just created is seconds old. Skipping a young row costs
		// one sweep interval on a genuinely stranded recording, and taking
		// it files a capture that is still being written.
		if d.now().Sub(recording.StartedAt) < strandedAfter {
			continue
		}
		if err := d.recoverInterrupted(ctx, recording); err != nil {
			// One unreadable row must not stop the rest being recovered.
			d.logger.ErrorContext(ctx, "could not recover a stranded recording",
				slog.Int64("recording", recording.ID),
				slog.String("path", recording.Path),
				slog.Any("error", err))
			continue
		}
		recovered++
	}
	return recovered, nil
}

// recompressEnabled reports whether the rung may run at all.
func (d *Daemon) recompressEnabled() bool {
	return d.config.Space.Recompress.Enabled && d.encoder != nil && d.finalizer != nil
}

// RecompressPass re-encodes every recording past its age, oldest first.
//
// It selects the encoder once per pass rather than once per recording. The
// probe runs a subprocess per candidate encoder, and the answer cannot
// change between two recordings in one pass.
func (d *Daemon) RecompressPass(ctx context.Context) error {
	if !d.recompressEnabled() {
		return nil
	}

	settings := d.config.Space.Recompress
	cutoff := d.now().Add(-settings.After.Std())
	candidates, err := d.store.RecompressCandidates(cutoff)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}

	available, err := d.encoder.Encoders(ctx)
	if err != nil {
		return fmt.Errorf("listing this machine's encoders: %w", err)
	}
	encoder, software, err := post.SelectEncoder(available, post.TranscodeOptions{
		Codec:          settings.Codec,
		Quality:        settings.Quality,
		PreferHardware: settings.PreferHardware,
	})
	if err != nil {
		return fmt.Errorf("choosing an encoder: %w", err)
	}
	if software {
		// Said out loud once per pass. The operator asked for hardware and
		// is about to spend hours per broadcast instead of one.
		d.logger.WarnContext(ctx, "re-encoding in software, which runs slower than realtime",
			slog.String("encoder", encoder.Name), slog.String("codec", settings.Codec))
	}

	return d.recompressEach(ctx, candidates, encoder, settings)
}

// recompressEach works through the candidates, stopping on cancellation.
//
// One recording at a time whatever max_concurrent says for capture: a
// re-encode saturates the encoder, and running two halves the rate of each
// while doubling the window in which a stop loses work.
func (d *Daemon) recompressEach(ctx context.Context, candidates []store.Recording,
	encoder post.Encoder, settings config.Recompress,
) error {
	var failures []error

	for _, recording := range candidates {
		if stopping(ctx) {
			return nil
		}

		before := recording.Bytes
		err := d.finalizer.Recompress(ctx, recording.ID, settings.KeepOriginal,
			func(ctx context.Context, source, output string) error {
				return d.encoder.Transcode(ctx, source, output, encoder, settings.Quality)
			})
		if err != nil {
			failures = append(failures, fmt.Errorf("recording %d: %w", recording.ID, err))
			continue
		}

		// The resulting size sits beside the original because a re-encode
		// that shrank a recording absurdly is otherwise invisible: the
		// encoder path has no size band of its own, and with keep_original
		// off the original is already gone.
		after := before
		if reencoded, readErr := d.store.Recording(recording.ID); readErr == nil {
			after = reencoded.Bytes
		}
		d.logger.InfoContext(ctx, "re-encoded a recording",
			slog.Int64("recording", recording.ID),
			slog.Int64("was_bytes", before),
			slog.Int64("now_bytes", after),
			slog.String("encoder", encoder.Name))
	}

	// A cancelled pass is how every stop ends. What it collected on the way
	// out describes the shutdown rather than the library, so it goes
	// nowhere.
	if stopping(ctx) {
		return nil
	}
	return errors.Join(failures...)
}

// stopping reports whether a pass must end because the daemon is ending.
//
// It reads the done channel rather than ctx.Err, so that "the operator
// stopped us" never has the shape of an error the caller reports.
func stopping(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// idleSpaceLoop evaluates the library's fill level whether or not a
// capture is running.
//
// The watermark a capture carries only exists for as long as that capture
// does, so a library that fills between broadcasts is silent until the
// next one is refused. Refusal is the worst moment to learn: the
// broadcast is already going unrecorded. This is what turns that into a
// warning with time left to act on it.
//
// The first check runs at once, so a daemon starting against a library
// that is already full says so rather than waiting out the interval.
func (d *Daemon) idleSpaceLoop(ctx context.Context) {
	level := space.LevelOK
	readable := true

	ticker := time.NewTicker(d.idleSpaceInterval)
	defer ticker.Stop()

	for {
		level, readable = d.checkSpace(ctx, level, readable)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// checkSpace evaluates the fill level once, reports a change, and lets
// the cheapest rung of the ladder run if the library is under pressure.
//
// It takes and returns the level and whether the last read succeeded,
// because both are latches: each one exists so a condition that persists
// is reported once rather than on every tick.
func (d *Daemon) checkSpace(ctx context.Context, level space.Level, readable bool) (space.Level, bool) {
	usage, err := d.usage()
	if err != nil {
		// Reported as the reads start failing rather than on every tick
		// after, because a disk that cannot be read stays unreadable and
		// the report would become the flood.
		if readable {
			d.logger.WarnContext(ctx, "the library's size could not be read, so its limits are unenforced",
				slog.Any("error", err))
		}
		return level, false
	}

	current := space.Watch(d.limits(), usage)
	level = d.reportSpaceLevel(ctx, level, current, usage)
	if current != space.LevelOK {
		level = d.releaseTrash(ctx, level)
	}
	return level, true
}

// releaseTrash finishes expired purges until the library has room again,
// and returns the level in force afterwards.
//
// This is the cheapest rung of the ladder and it only runs under
// pressure. A trashed recording still occupies the volume, so the undo
// window costs headroom for as long as it stays open. Holding it while
// there is room and spending it when there is not is what keeps the
// window as long as the library can afford.
//
// Nothing here chooses a recording. Every row it touches is one the
// operator already purged, and the only decision left is when that
// deletion finishes. It stops the moment the level clears, so an operator
// who purged a month of broadcasts does not lose the undo window on all of
// them to reclaim one broadcast's worth of space.
//
// At the limit the grace is overridden, because it is the one lever the
// design offers against a full library and a week-long window makes it
// useless at the moment it is pulled. Oldest first, and only as far as the
// level takes to clear.
func (d *Daemon) releaseTrash(ctx context.Context, level space.Level) space.Level {
	grace := d.config.Space.Purge.TrashGrace.Std()
	if grace < 0 {
		return level
	}
	if level == space.LevelCritical && grace > 0 {
		// Said once, because it closes an undo window the operator chose.
		d.logger.WarnContext(ctx, "library is at its limit, finishing purges before their grace expires",
			slog.Duration("grace", grace))
		grace = 0
	}

	// The wall clock rather than d.now, because the column this is
	// compared against is written by the store from time.Now. An injected
	// clock here would put the two sides of the comparison on different
	// clocks, and the grace period would mean nothing.
	expired, err := d.store.TrashedBefore(time.Now().Add(-grace))
	if err != nil {
		d.logger.WarnContext(ctx, "the trash could not be read", slog.Any("error", err))
		return level
	}

	released := 0
	for _, recording := range expired {
		if ctx.Err() != nil {
			break
		}
		if err := d.finalizer.Release(ctx, recording.ID); err != nil {
			// A recording the organizer is busy with comes round again on
			// the next tick, and one that will not delete is a fact the
			// operator needs rather than a reason to stop on the rest.
			d.logger.WarnContext(ctx, "a purged recording could not be released",
				slog.Int64("recording_id", recording.ID), slog.Any("error", err))
			continue
		}
		released++

		usage, err := d.usage()
		if err != nil {
			break
		}
		if level = space.Watch(d.limits(), usage); level == space.LevelOK {
			break
		}
	}

	if released > 0 {
		d.logger.InfoContext(ctx, "released purged recordings to make room",
			slog.Int("released", released), slog.Int("expired", len(expired)))
	}
	return level
}

// reportSpaceLevel announces a change in the fill level and returns the
// level now in force.
//
// Only a transition is reported. A library sitting at its warning level
// for a week is one fact rather than one per tick, and an operator who
// has learned to skip a line that repeats has learned to skip the
// escalation printed in the same shape.
func (d *Daemon) reportSpaceLevel(ctx context.Context, was, now space.Level, usage space.Usage) space.Level {
	if now == was {
		return now
	}

	held := slog.String("library", config.Size(usage.LibraryBytes).String())
	free := slog.String("free", config.Size(usage.FreeBytes).String())

	switch now {
	case space.LevelCritical:
		d.logger.ErrorContext(ctx, "library is at its limit, the next broadcast will be refused", held, free)
		d.notify(ctx, Event{
			Kind:   EventLibraryFull,
			Detail: "the library is at its limit; the next broadcast will be refused",
		})
	case space.LevelLow:
		d.logger.WarnContext(ctx, "library is running low", held, free)
		d.notify(ctx, Event{
			Kind:   EventLibraryFull,
			Detail: "the library is running low; recording continues for now",
		})
	case space.LevelOK:
		// Recovery carries no notification. Nothing is at stake in it, and
		// the operator who made the room is the one who would be told.
		d.logger.InfoContext(ctx, "library has room again", held, free)
	}
	return now
}

// acquireSlot blocks until a capture slot is free, bounding how many
// recordings run at once. Each one costs bandwidth and disk throughput, so
// unbounded concurrency degrades every recording rather than only the
// newest.
func (d *Daemon) acquireSlot(ctx context.Context) error {
	if d.captureSlots == nil {
		return nil
	}
	select {
	case d.captureSlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseSlot returns a capture slot.
func (d *Daemon) releaseSlot() {
	if d.captureSlots == nil {
		return
	}
	select {
	case <-d.captureSlots:
	default:
	}
}

// ///////////////////////////////////////////////
// Sessions
// ///////////////////////////////////////////////

// StartSession opens a session and reports how long the recorder was down.
//
// A first-ever start reports no downtime. Anything else compares against
// the previous session's last heartbeat, which is the only evidence a
// crashed recorder leaves behind.
func (d *Daemon) StartSession(ctx context.Context) (store.Session, *Downtime, error) {
	session, err := d.store.StartSession(d.now(), StaleAfter)
	if err != nil {
		return store.Session{}, nil, err
	}

	previous, err := d.store.LastSession(session.ID)
	if errors.Is(err, store.ErrNotFound) {
		return session, nil, nil
	}
	if err != nil {
		return session, nil, err
	}

	downtime := &Downtime{
		Since:   session.StartedAt.Sub(previous.HeartbeatAt),
		Crashed: previous.StoppedAt == nil,
	}
	if downtime.Since < 0 {
		downtime.Since = 0
	}
	// A session cannot have started after it last beat, so its own start
	// bounds the gap with a measurement that holds whatever the heartbeat
	// says. Where that start is not a measurement either, the horizon
	// stands in: it is the boundary the operator is told to reach past by
	// hand, which is the one thing still worth saying about a gap this
	// long.
	//
	// Judged on plausibility rather than on arithmetic. Timestamps decode
	// through time.Unix, so a zeroed column reads as 1970 and describes
	// fifty-six years of downtime without overflowing anything, and the
	// store accepts any instant from 1677 on. The ceiling is set far past
	// any outage worth measuring, so a machine that spent years in storage
	// is still reported honestly and only a row that cannot be a
	// measurement is replaced.
	age := session.StartedAt.Sub(previous.StartedAt)
	if age < 0 || age > maxPlausibleDowntime {
		// Neither value the store can produce honestly. Said out loud
		// rather than quietly corrected, because the row is the only thing
		// that explains whatever length is reported next.
		impossible := []any{
			slog.Int64("previous_session", previous.ID),
			slog.Time("previous_started", previous.StartedAt),
			slog.Time("this_started", session.StartedAt),
		}
		age = recoveryHorizon

		// The substituted figure is named only where it is the figure. It
		// is a bound, not a reading, so a heartbeat that measured something
		// shorter is still what the operator is told, and quoting the bound
		// beside it would offer two lengths for one outage.
		if downtime.Since > age {
			downtime.Since = age
			impossible = append(impossible, slog.Duration("assuming", age))
		}
		d.logger.WarnContext(ctx, "a previous session is dated impossibly, "+
			"so how long the recorder was down cannot be measured from it",
			impossible...)
	} else if downtime.Since > age {
		downtime.Since = age
	}

	if downtime.Crashed {
		d.logger.WarnContext(ctx, "recorder was down",
			slog.Duration("for", downtime.Since),
			slog.Int64("previous_session", previous.ID))
		d.notify(ctx, Event{
			Kind:   EventDowntime,
			Detail: fmt.Sprintf("recorder was down for %s and did not shut down cleanly", roundDuration(downtime.Since)),
		})
	}
	return session, downtime, nil
}

// StopSession records a clean shutdown, which is what distinguishes an
// intentional stop from a crash the next time the daemon starts.
func (d *Daemon) StopSession(sessionID int64) error {
	return d.store.StopSession(sessionID, d.now())
}

// Heartbeat records that the daemon is still alive.
func (d *Daemon) Heartbeat(sessionID int64) error {
	return d.store.Heartbeat(sessionID, d.now())
}

// recordStop closes the session and folds the outcome into the run's own
// error, which is what Run defers.
//
// A session row that is no longer there is not a failed shutdown. The
// database is a cache over the library and is meant to be rebuildable from
// the sidecars, so a rebuild mid-run drops the row this session opened.
// Reporting that as an error would make an orderly stop exit non-zero and
// read to the next start as the crash it is not.
func (d *Daemon) recordStop(sessionID int64, runErr error) error {
	switch stopErr := d.StopSession(sessionID); {
	case stopErr == nil:
		d.logger.Info("recorder stopped cleanly")
	case errors.Is(stopErr, store.ErrNotFound):
		d.logger.Warn("session row is gone, so this stop was not recorded",
			slog.Int64("session_id", sessionID))
	case runErr == nil:
		return stopErr
	default:
		// The run failed for its own reason, which is what the caller gets
		// back, and the stop failed too. Without this arm the second one
		// is neither returned nor logged: the session stays open with no
		// stopped_at, the next start reports a crash that did not happen,
		// and nothing anywhere says why.
		d.logger.Error("the session could not be closed, so the next start will report a crash",
			slog.Int64("session_id", sessionID), slog.Any("error", stopErr))
	}
	return runErr
}

// laterOf returns whichever instant is not earlier.
//
// A recording's end is clamped to its own start with it, because both are
// wall clock and the store refuses an end before its start inside the
// transaction. A backward clock step, from a bad real-time clock or a
// restored VM snapshot, otherwise leaves the row stuck in whatever state it
// was in with nothing left to move it.
func laterOf(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// roundDuration trims a duration to something worth reading in a message.
func roundDuration(d time.Duration) time.Duration {
	if d >= time.Hour {
		return d.Round(time.Minute)
	}
	return d.Round(time.Second)
}

// SyncChannels registers every configured channel, so a channel added to
// the config becomes recordable without touching the database by hand.
func (d *Daemon) SyncChannels() ([]Target, error) {
	configured := d.config.EnabledChannels()
	targets := make([]Target, 0, len(configured))

	for _, entry := range configured {
		channel, err := d.store.UpsertChannel(entry.Platform, entry.Name, "")
		if err != nil {
			return nil, err
		}
		targets = append(targets, Target{Entry: entry, Channel: channel})
	}
	return targets, nil
}

// ///////////////////////////////////////////////
// Budget
// ///////////////////////////////////////////////

// limits returns the configured disk bounds.
func (d *Daemon) limits() space.Limits {
	return space.Limits{
		MaxSize: d.config.Space.MaxSize,
		MinFree: d.config.Space.MinFree,
	}
}

// usage reads the library's current disk position.
//
// A capture in flight holds bytes the recordings table cannot account for:
// its row carries a size only once the capture finishes. Reading those from
// disk is what lets the size cap bound a recording while it is being made,
// and without it a capture cannot be stopped by the very limit that is
// watching it.
func (d *Daemon) usage() (space.Usage, error) {
	libraryBytes, err := d.store.TotalBytes()
	if err != nil {
		return space.Usage{}, err
	}
	inFlight, err := d.inFlightBytes()
	if err != nil {
		return space.Usage{}, err
	}
	unclaimed, err := d.unclaimedBytes()
	if err != nil {
		return space.Usage{}, err
	}
	free, err := d.freeSpace(d.library.Root())
	if err != nil {
		return space.Usage{}, err
	}
	return space.Usage{LibraryBytes: libraryBytes + inFlight + unclaimed, FreeBytes: free}, nil
}

// unclaimedBytes sums the files in the incoming directory that no recording
// row names.
//
// A backfill download writes there before any row names it, so the size cap
// cannot see it while it runs. min_free is a real statfs and does see it, so
// the two limits disagree by exactly the download, and one directory walk
// covers a fetch, a patch, and a partial file under one rule rather than
// juggling a row through the capturing state around a download whose
// extension is not known until it finishes.
func (d *Daemon) unclaimedBytes() (int64, error) {
	unclaimed, err := d.unclaimedIncoming()
	if err != nil {
		return 0, err
	}

	total := int64(0)
	for _, file := range unclaimed {
		total += file.bytes
	}
	return total, nil
}

// inFlightBytes sums what the captures still running have written so far.
//
// A capturing row's stored size is zero until the capture ends, so the file
// on disk is the only account of it. Nothing is double counted: a recording
// leaves that state in the same write that records its size.
func (d *Daemon) inFlightBytes() (int64, error) {
	capturing, err := d.store.RecordingsByState(store.StateCapturing)
	if err != nil {
		return 0, err
	}

	total := int64(0)
	for _, recording := range capturing {
		info, err := os.Stat(d.library.RelPath(recording.Path))
		if errors.Is(err, os.ErrNotExist) {
			// The engine has not opened the output yet, so it holds nothing.
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("sizing the capture of recording %d: %w", recording.ID, err)
		}
		total += info.Size()
	}
	return total, nil
}

// sessionAnchor returns the anchor of a channel's open session, and whether
// there is one under this stream id.
//
// It exists so a caller can tell an open session from a new one without
// opening one, which is what keeps the cost of resolving a true start to
// once per broadcast rather than once per poll.
func (d *Daemon) sessionAnchor(channelID int64, streamID string) (time.Time, bool) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	session, ok := d.matchingSession(channelID, streamID)
	if !ok {
		return time.Time{}, false
	}
	return session.startedAt, true
}

// matchingSession returns a channel's open session when it is the same
// broadcast this stream id names.
//
// One definition of "the same session" for every caller, so the rule cannot
// be changed in one place and left behind in another. The caller holds
// sessionsMu.
func (d *Daemon) matchingSession(channelID int64, streamID string) (*liveSession, bool) {
	session, ok := d.sessions[channelID]
	if !ok || session.streamID != streamID {
		return nil, false
	}
	return session, true
}

// sessionStart returns the anchor for the broadcast a channel is in,
// opening a session at startedAt when there is not one already.
//
// A session lasts while the channel keeps broadcasting under one stream id.
// Anchoring the broadcast to it is what keeps every poll of one session on
// one row: a start taken from the clock stops matching the row it created
// once it drifts past the store's overlap window, and the platform reports
// no stream id at all for some sources.
func (d *Daemon) sessionStart(channelID int64, streamID string, startedAt time.Time) time.Time {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	if d.sessions == nil {
		d.sessions = make(map[int64]*liveSession)
	}
	if session, ok := d.matchingSession(channelID, streamID); ok {
		return session.startedAt
	}
	d.sessions[channelID] = &liveSession{startedAt: startedAt, streamID: streamID}
	return startedAt
}

// noteBroadcast records which row a channel's live session writes to.
//
// It is what lets the poll that finds the channel offline stamp the end on
// the broadcast that just finished, rather than searching for one.
func (d *Daemon) noteBroadcast(channelID, broadcastID int64) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	if session, ok := d.sessions[channelID]; ok {
		session.broadcastID = broadcastID
	}
}

// endSession forgets a channel that has stopped broadcasting, so its next
// broadcast is anchored, estimated, and reported afresh.
//
// It answers with what it forgot, or nil when the channel had no session, so
// the caller can close the broadcast out before the only record of which one
// it was is gone.
func (d *Daemon) endSession(channelID int64) *liveSession {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	session := d.sessions[channelID]
	delete(d.sessions, channelID)
	return session
}

// firstRefusal reports whether this is the first refusal for a channel's
// current broadcast, and records it either way.
//
// The daemon polls a live channel for as long as it stays live, so without
// this a full library writes the same refusal every poll interval until the
// broadcast ends.
func (d *Daemon) firstRefusal(channelID, broadcastID int64) bool {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	session, ok := d.sessions[channelID]
	if !ok {
		return true
	}
	if session.refused && session.refusedFor == broadcastID {
		return false
	}
	session.refused, session.refusedFor = true, broadcastID
	return true
}

// clearRefusal forgets a channel's last refusal, so the latch means "not
// twice in a row" rather than "once ever". A broadcast that got in after a
// refusal and filled the library again is a second refusal the operator has
// to hear about.
func (d *Daemon) clearRefusal(channelID int64) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	if session, ok := d.sessions[channelID]; ok {
		session.refused = false
	}
}

// noteSpaceHalt records that the watermark stopped a capture of this
// broadcast before it was long enough to keep.
//
// Such a capture produced nothing: the row is failed and the bytes are an
// orphan in incoming. Another attempt at the same broadcast produces another
// of the same, once per poll, for as long as the channel stays live.
func (d *Daemon) noteSpaceHalt(channelID, broadcastID int64) {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	if session, ok := d.sessions[channelID]; ok {
		session.halted, session.haltedFor = true, broadcastID
	}
}

// haltedForSpace reports whether a capture of this broadcast already ended
// at the watermark with nothing to keep.
//
// It is scoped to one broadcast, so the next one is admitted on its own
// merits however this one went.
func (d *Daemon) haltedForSpace(channelID, broadcastID int64) bool {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	session, ok := d.sessions[channelID]
	return ok && session.halted && session.haltedFor == broadcastID
}

// admit reports whether a broadcast of roughly the given length may be
// recorded.
//
// The estimate uses the channel's own history when there is any, because a
// generic figure either refuses recordings that would have fit or admits
// ones that will not.
func (d *Daemon) admit(channelID int64, expected time.Duration) error {
	usage, err := d.usage()
	if err != nil {
		return err
	}
	return space.Admit(d.limits(), usage, space.Estimate(d.bitrateFor(channelID), expected))
}

// bitrateFor derives a channel's byte rate from its most recent completed
// recording, or zero when it has none.
//
// A live channel keeps the answer for the length of its session. Only a
// completed recording can change it, and the channel is still broadcasting,
// so a refused channel would otherwise reread six months of rows on every
// poll for the same number.
func (d *Daemon) bitrateFor(channelID int64) int64 {
	d.sessionsMu.Lock()
	session, live := d.sessions[channelID]
	if live && session.bitrateKnown {
		defer d.sessionsMu.Unlock()
		return session.bitrate
	}
	d.sessionsMu.Unlock()

	// Read outside the lock. The same mutex guards the capturing set, which
	// sits on the capture path and inside the sweep, and this query walks
	// six months of a channel's rows against a database the store gives one
	// connection: a round trip that waits out the busy timeout would
	// otherwise freeze every capture start and a whole sweep pass with it.
	// Two callers racing compute the same number twice, which costs one
	// query and answers the same either way.
	rate := d.readBitrate(channelID)

	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()
	if session, live := d.sessions[channelID]; live {
		session.bitrate, session.bitrateKnown = rate, true
	}
	return rate
}

// readBitrate reads a channel's byte rate out of its recording history.
//
// The measured media length is preferred over the wall span. Bytes only
// exist for media the recorder actually received, so dividing them by a span
// that includes the stretches it did not understates the rate, and the next
// broadcast is admitted against an estimate that is short by however far the
// last capture diverged.
func (d *Daemon) readBitrate(channelID int64) int64 {
	recordings, err := d.store.RecordingsForChannel(channelID,
		d.now().AddDate(0, -6, 0), d.now().Add(time.Hour))
	if err != nil {
		return 0
	}

	for _, recording := range slices.Backward(recordings) {
		measured := recording.MediaDuration
		if measured <= 0 {
			measured = recording.Duration
		}
		if rate := space.Bitrate(recording.Bytes, measured); rate > 0 {
			return rate
		}
	}
	return 0
}

// notify sends an event, logging rather than failing when the sink is
// unavailable. A notification that cannot be delivered must never take down
// a recording.
func (d *Daemon) notify(ctx context.Context, event Event) {
	if !d.wants(event.Kind) {
		return
	}
	if err := d.notifier.Notify(ctx, event); err != nil {
		d.logger.WarnContext(ctx, "notification failed",
			slog.String("kind", string(event.Kind)), slog.Any("error", err))
	}
}

// wants reports whether the configuration asks for an event kind.
func (d *Daemon) wants(kind EventKind) bool {
	switch kind {
	case EventRecordingStarted:
		return d.config.Notify.OnRecordingStart
	case EventFailure:
		return d.config.Notify.OnFailure
	case EventLibraryFull:
		return d.config.Notify.OnLibraryFull
	case EventDowntime:
		// Downtime is the event the operator most needs and least expects,
		// so it follows the failure setting rather than having its own.
		return d.config.Notify.OnFailure
	case EventRecovered, EventGapFilled:
		// Something reached the library, which is what the start setting is
		// about. Backfill gets no switch of its own: an operator who wants
		// to hear about a capture wants to hear about the copy that stood in
		// for the one they missed.
		return d.config.Notify.OnRecordingStart
	case EventFetchGaveUp, EventCredentialDead:
		// Neither resolves on its own. A dead credential costs quality on
		// every recording after it until the operator replaces it.
		return d.config.Notify.OnFailure
	default:
		return false
	}
}
