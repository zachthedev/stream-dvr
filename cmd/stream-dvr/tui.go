package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/urfave/cli/v3"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/daemon"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/notify"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/post"
	"zach.tools/go/stream-dvr/internal/retention"
	"zach.tools/go/stream-dvr/internal/service"
	"zach.tools/go/stream-dvr/internal/space"
	"zach.tools/go/stream-dvr/internal/store"
	"zach.tools/go/stream-dvr/internal/tui"
)

// controllerAdapter narrows a service.Manager to the model's Controller.
type controllerAdapter struct {
	manager service.Manager
}

// libraryAdapter answers the model's read side, opening the database the
// first time something asks for it.
//
// Opening lazily is what lets the calendar start against a library no recorder
// has written to yet: the refusal reaches the error pane, the month still
// draws, and r retries because a failed open is not cached.
//
// It also carries the budget question the store cannot answer alone. The cap
// comes from the configuration and the free space from the volume, and the
// database knows neither.
type libraryAdapter struct {
	path   string
	root   string
	limits space.Limits

	mu    sync.Mutex
	store *store.Store
}

// purgeAdapter ranks the library and moves what the operator chose.
//
// The organizer lives here rather than in the model because trashing takes
// its per-recording lock, and that lock is the only thing stopping a purge
// racing the sweep's retry of the same row.
//
// It carries a context because tui.Purge takes none: the model has no
// context to give, and every call is made from a command the Bubble Tea
// runtime already ties to the program's lifetime.
type purgeAdapter struct {
	ctx    context.Context
	reader *libraryAdapter
	policy retention.Policy
	build  func(*store.Store) *organize.Organizer

	mu        sync.Mutex
	organizer *organize.Organizer
	shared    *organize.Organizer
}

// inProcessRecorder runs the daemon inside the calendar's own process.
//
// It owns the migrating store handle, because a recorder is what owns the
// schema, and the calendar's own handle deliberately refuses to migrate.
// The organizer it builds is handed to the purge for as long as it runs, so
// a purge and this recorder's sweep contend on one lock table rather than
// two that know nothing of each other.
type inProcessRecorder struct {
	configPath string
	purge      *purgeAdapter
	// feed carries what a run reports to the calendar's own pane. It
	// outlives any one run, because the pane keeps showing what the last
	// recorder said after it stops.
	feed chan tui.FeedEvent

	// running answers socketFeed without taking the lock below. The goroutine
	// reading the socket asks on every event, and Start holds that lock
	// across opening a database, which retries for seconds against a busy
	// one. Blocking there stops the reader long enough for the bus to drop
	// it, and holds up the wait on the way out.
	running atomic.Bool

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// feedNotifier sends what a recorder reports to the calendar's own pane.
//
// It is the sink that exists only while the calendar is running the
// recorder itself. A recorder in another process reaches the same pane
// through socketFeed.
type feedNotifier struct {
	feed chan tui.FeedEvent
}

// socketFeed carries another process's events into the calendar's pane.
//
// A recorder installed as a service publishes to the notification socket and
// knows nothing about this window. Following it is what makes the pane show
// the same events whether the recorder runs here or somewhere else.
//
// running is what keeps one event from being shown twice. A recorder in this
// window publishes to that socket as well, so during an in-process run every
// line arrives once through feedNotifier and once back through the socket.
// The two recorders cannot both exist: the library's session row admits one,
// so anything read while this window records is its own bus returning.
type socketFeed struct {
	feed    chan<- tui.FeedEvent
	running func() bool
}

// configFileStore is the settings pane's view of the config file.
//
// It holds the path the command resolved rather than resolving it again, so
// a --config the operator passed is the file the pane edits.
type configFileStore struct {
	path string
}

// ///////////////////////////////////////////////
// Limits
// ///////////////////////////////////////////////

// maxFeedField bounds one field of an event read off the notification
// socket, in bytes.
//
// Comfortably longer than a broadcast title and far short of the bus's
// 64KB line, which is what a peer would have to be told to keep to for the
// pane to stay one line per event.
const maxFeedField = 200

// ///////////////////////////////////////////////
// tui
// ///////////////////////////////////////////////

// runRoot opens the calendar, which is what the binary does when no
// subcommand is named.
func runRoot(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() > 0 {
		return unknownCommand(cmd)
	}

	weekStart := time.Sunday
	if cmd.Bool("monday") {
		weekStart = time.Monday
	}
	return runTUI(ctx, configFile(cmd), weekStart)
}

// runTUI opens the library read-side and hands it to the calendar.
func runTUI(ctx context.Context, configPath string, weekStart time.Weekday) (err error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	lib, err := library.Open(cfg.Library.Root)
	if err != nil {
		return err
	}

	location, err := cfg.Location()
	if err != nil {
		return err
	}

	// The daemon may be writing while this reads. WAL mode serves readers
	// from the log without blocking, so both run at once.
	reader := &libraryAdapter{
		path: lib.DatabasePath(),
		root: lib.Root(),
		limits: space.Limits{
			MaxSize: cfg.Space.MaxSize,
			MinFree: cfg.Space.MinFree,
		},
	}
	defer reader.Close()

	template, err := cfg.Template()
	if err != nil {
		return err
	}

	purge := &purgeAdapter{
		ctx:    ctx,
		reader: reader,
		policy: retention.Policy{
			WatchedWeight:     cfg.Space.Purge.WatchedWeight,
			AgeWeight:         cfg.Space.Purge.AgeWeight,
			RefetchableWeight: cfg.Space.Purge.RefetchableWeight,
			ProtectFor:        cfg.Space.Purge.ProtectFor.Std(),
		},
		build: func(db *store.Store) *organize.Organizer {
			return organize.New(lib, db, template, post.New(), organize.Options{
				Container: cfg.Capture.Container,
				Location:  location,
			})
		},
	}

	// Buffered so a recorder reporting while the calendar is mid-redraw
	// never waits, and bounded so a burst is dropped rather than queued
	// against a pane that shows the last few anyway.
	feed := make(chan tui.FeedEvent, 32)
	recorder := &inProcessRecorder{configPath: configPath, purge: purge, feed: feed}
	defer func() {
		// The alternate screen is gone by the time this runs, so a recorder
		// that died on the way out has nowhere to report but the exit code.
		if stopErr := recorder.Stop(); stopErr != nil && err == nil {
			err = stopErr
		}
	}()

	// Read once here, as the recorder's bus reads it once at each start.
	// Turning desktop notifications off takes this subscription down at the
	// next launch rather than the moment the settings pane saves.
	if cfg.Notify.Desktop {
		var following sync.WaitGroup
		followCtx, stopFollowing := context.WithCancel(ctx)
		defer func() {
			stopFollowing()
			following.Wait()
		}()

		subscriber := socketFeed{feed: feed, running: recorder.running.Load}
		following.Go(func() {
			// Nothing here may write to the terminal, which the calendar owns
			// for as long as the alternate screen is up. Follow's own messages
			// are the dial failures that are the ordinary state anyway, and it
			// returns nothing but the cancellation above.
			_ = notify.Follow(followCtx, paths.NotifySocket(lib.Root()),
				slog.New(slog.DiscardHandler), subscriber.deliver)
		})
	}

	model := tui.New(tui.Options{
		Library:        reader,
		Actions:        reader,
		Purge:          purge,
		Settings:       configFileStore{path: configPath},
		Recorder:       recorder,
		Feed:           feed,
		Controller:     serviceController(),
		ServiceKey:     serviceName,
		Location:       location,
		WeekStart:      weekStart,
		StatusInterval: tui.DefaultStatusInterval,
	})

	// The alternate screen is a property of the view the model returns, not
	// a program option.
	program := tea.NewProgram(model, tea.WithContext(ctx))
	_, err = program.Run()
	return err
}

// serviceController adapts the platform service manager to what the model
// needs, or nil when no manager is available.
//
// A missing manager is not fatal: the calendar is still worth reading on a
// machine where the recorder was never registered.
func serviceController() tui.Controller {
	manager, err := service.New()
	if err != nil {
		return nil
	}
	return controllerAdapter{manager: manager}
}

// open returns the database, opening it the first time it is needed.
//
// OpenClient rather than Open: the recorder owns the schema, and a calendar
// from a newer build must not migrate a library underneath a daemon already
// running against it. A failure is not cached, so the next refresh retries.
func (a *libraryAdapter) open() (*store.Store, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.store != nil {
		return a.store, nil
	}

	opened, err := store.OpenClient(a.path)
	if err != nil {
		return nil, libraryAdvice(err)
	}
	a.store = opened
	return a.store, nil
}

// libraryAdvice turns a refusal to open into the line the error pane shows.
//
// Its whole job is to name the next step, because the calendar is what a new
// install runs first and every one of these is recoverable.
func libraryAdvice(err error) error {
	if errors.Is(err, store.ErrNoDatabase) {
		return fmt.Errorf("%w; run 'stream-dvr serve' once to create it", err)
	}

	mismatch, ok := errors.AsType[*store.SchemaMismatchError](err)
	switch {
	case !ok:
		return err
	case mismatch.Got > mismatch.Want:
		return fmt.Errorf("%w; a newer stream-dvr wrote this library, so upgrade this one", err)
	default:
		return fmt.Errorf("%w; run 'stream-dvr serve' once to migrate it", err)
	}
}

// Close releases the database, if anything ever opened it.
func (a *libraryAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.store == nil {
		return nil
	}
	closing := a.store
	a.store = nil
	return closing.Close()
}

// Channels implements tui.Library.
func (a *libraryAdapter) Channels() ([]store.Channel, error) {
	db, err := a.open()
	if err != nil {
		return nil, err
	}
	return db.Channels()
}

// CoverageBetween implements tui.Library.
func (a *libraryAdapter) CoverageBetween(channelID int64, from, to time.Time,
	loc *time.Location,
) ([]store.Day, error) {
	db, err := a.open()
	if err != nil {
		return nil, err
	}
	return db.CoverageBetween(channelID, from, to, loc)
}

// RecordingsForChannel implements tui.Library.
func (a *libraryAdapter) RecordingsForChannel(channelID int64, from, to time.Time) ([]store.Recording, error) {
	db, err := a.open()
	if err != nil {
		return nil, err
	}
	return db.RecordingsForChannel(channelID, from, to)
}

// MarkWatched implements tui.Actions.
func (a *libraryAdapter) MarkWatched(id int64, at *time.Time) error {
	db, err := a.open()
	if err != nil {
		return err
	}
	return db.MarkWatched(id, at)
}

// SetPinned implements tui.Actions.
func (a *libraryAdapter) SetPinned(id int64, pinned bool) error {
	db, err := a.open()
	if err != nil {
		return err
	}
	return db.SetPinned(id, pinned)
}

// Start implements tui.Recorder.
//
// Everything the run needs is built here rather than at construction, so a
// config the operator has just fixed in the settings pane is the one the
// recorder starts with.
func (r *inProcessRecorder) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.done != nil {
		return errors.New("a recorder is already running in this window")
	}

	cfg, err := config.Load(r.configPath)
	if err != nil {
		return err
	}
	lib, err := library.Open(cfg.Library.Root)
	if err != nil {
		return err
	}

	// Never teed to stderr: the calendar owns the terminal, and a log line
	// written over the alternate screen corrupts what is on it.
	log, closeLog, err := buildLogger(lib, false)
	if err != nil {
		return err
	}

	db, err := store.Open(lib.DatabasePath())
	if err != nil {
		closeLog.Close()
		return err
	}

	// The calendar's own pane is a sink beside the configured ones, so a
	// recorder in this window reports into the window running it.
	sinks, closeSinks := buildNotifiers(cfg, lib, log)
	sinks = append(sinks, feedNotifier{feed: r.feed})

	recorder, organizer, err := buildDaemon(cfg, lib, db.WithLogger(log), log, sinks, nil)
	if err != nil {
		closeSinks()
		db.Close()
		closeLog.Close()
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.cancel, r.done, r.err = cancel, done, nil
	r.running.Store(true)
	r.purge.useOrganizer(organizer)

	go func() {
		// Registered first so it runs last, after closeSinks has flushed
		// what the bus still held. Clearing it any earlier would let the
		// tail of a run reach the pane through the socket as well.
		defer r.running.Store(false)
		defer close(done)
		defer closeLog.Close()
		defer db.Close()
		// The sinks drain before the log closes, so a failure that stopped
		// the run is still delivered.
		defer closeSinks()

		err := recorder.Run(ctx)
		r.finish(err)
	}()
	return nil
}

// Stop cancels the run and waits for it to unwind.
//
// It waits rather than signalling and returning, because the caller is
// usually the quit key: a capture that is still killing streamlink and
// sizing its file has to finish before the process goes away.
func (r *inProcessRecorder) Stop() error {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()

	if done == nil {
		return nil
	}
	cancel()
	<-done

	return r.Err()
}

// Running reports whether a run is in flight.
func (r *inProcessRecorder) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.done == nil {
		return false
	}
	select {
	case <-r.done:
		return false
	default:
		return true
	}
}

// Err returns why the last run ended, if it ended badly.
func (r *inProcessRecorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.err
}

// finish records how a run ended and releases the shared organizer.
//
// A cancelled run is how every deliberate stop ends, so it is not a failure
// worth showing.
func (r *inProcessRecorder) finish(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err != nil && !errors.Is(err, context.Canceled) {
		r.err = err
	}
	// done is not cleared here. It is closed by the goroutine's last defer,
	// after the sinks, the database handle and the log have been released,
	// and closing it is what Stop waits on. Nilling it makes Stop return
	// while all three are still closing, and the process exits underneath
	// them: a capture still killing streamlink and sizing its file has to
	// finish before the process goes away, which is what this method's own
	// doc promises.
	r.cancel = nil
	r.purge.useOrganizer(nil)
}

// Notify implements daemon.Notifier.
//
// It drops rather than blocking, for the same reason the webhook does: this
// runs on the recording path, and a calendar the operator is not looking at
// must never delay a capture.
func (f feedNotifier) Notify(_ context.Context, event daemon.Event) error {
	select {
	case f.feed <- tui.FeedEvent{
		Kind:    string(event.Kind),
		Channel: event.Channel,
		Detail:  event.Detail,
		At:      time.Now(),
	}:
	default:
	}
	return nil
}

// deliver shows one event a recorder in another process reported.
//
// It carries the recorder's own timestamp rather than stamping a new one.
// The event crossed a socket and may have waited on a reconnect, so the
// moment it arrived here is not the moment it happened.
func (s socketFeed) deliver(event notify.Event) {
	if s.running() {
		return
	}

	// Dropped rather than queued, for the reason feedNotifier drops: this
	// runs on the goroutine reading the socket, and a blocked reader is what
	// the bus's write deadline disconnects.
	select {
	case s.feed <- tui.FeedEvent{
		Kind:    clipField(event.Kind),
		Channel: clipField(event.Channel),
		Detail:  clipField(event.Detail),
		At:      event.At,
	}:
	default:
	}
}

// clipField bounds one field of an event read off the socket.
//
// Anything running as this operator may write to that socket, and the pane
// renders what it says on one line. A field the length of the bus's own line
// bound wraps over the calendar and the footer and stays there, because the
// pane holds what it was told for the rest of the session. The in-process
// sink needs no such bound: it carries the daemon's own text.
func clipField(text string) string {
	if len(text) <= maxFeedField {
		return text
	}
	// Cut on a rune bound so the escaping below never sees half of one.
	clipped := text[:maxFeedField]
	for len(clipped) > 0 && !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped
}

// Load implements tui.Settings.
func (s configFileStore) Load() (config.Config, error) { return config.Load(s.path) }

// Save implements tui.Settings.
func (s configFileStore) Save(cfg config.Config) error { return config.Save(s.path, cfg) }

// Path implements tui.Settings.
func (s configFileStore) Path() string { return s.path }

// Candidates implements tui.Purge.
//
// It asks the store for exactly the states retention offers rather than
// scanning the library, and the state list comes from retention itself so
// there is no second copy of the rule about what may be purged.
func (p *purgeAdapter) Candidates() ([]retention.Candidate, error) {
	db, err := p.reader.open()
	if err != nil {
		return nil, err
	}

	recordings, err := db.RecordingsByState(retention.Offerable()...)
	if err != nil {
		return nil, err
	}
	return retention.Rank(p.policy, recordings, time.Now()), nil
}

// Trash implements tui.Purge.
func (p *purgeAdapter) Trash(recordingID int64) error {
	organizer, err := p.organize()
	if err != nil {
		return err
	}

	_, err = organizer.Trash(p.ctx, recordingID)
	return err
}

// useOrganizer routes purges through a recorder's organizer, or back to
// this adapter's own when that recorder stops.
//
// The lock that keeps a purge and a sweep off the same recording is held
// per organizer, so while a recorder runs in this process both have to go
// through the same one.
func (p *purgeAdapter) useOrganizer(organizer *organize.Organizer) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.shared = organizer
}

// organize returns the organizer a purge must go through.
//
// One per session, not one per call. Its per-recording lock is what keeps
// two purges of the same row apart, and a fresh organizer each time would
// carry a fresh and empty lock table.
//
// A recorder in another process is not covered by any of this. What guards
// that case is organize.Trash re-reading the state under the write lock,
// and the exclusive create the move itself makes.
func (p *purgeAdapter) organize() (*organize.Organizer, error) {
	db, err := p.reader.open()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.shared != nil {
		return p.shared, nil
	}
	if p.organizer == nil {
		p.organizer = p.build(db)
	}
	return p.organizer, nil
}

// GapsFor implements tui.Library.
//
// A gap exposes offsets, a reason from this project's own constants, and
// when it was filled. The id comes from the row the operator selected, so
// nothing here reads text from outside the build.
func (a *libraryAdapter) GapsFor(recordingID int64) ([]store.Gap, error) {
	db, err := a.open()
	if err != nil {
		return nil, err
	}
	return db.Gaps(recordingID)
}

// SpaceUsage implements tui.Library.
//
// Trashed recordings are counted, because their files are still on the
// volume until the grace expires and something releases them. A gauge that
// discounted them would promise headroom the disk does not have.
func (a *libraryAdapter) SpaceUsage() (tui.Space, error) {
	db, err := a.open()
	if err != nil {
		return tui.Space{}, err
	}

	held, err := db.TotalBytes()
	if err != nil {
		return tui.Space{}, err
	}
	free, err := space.Free(a.root)
	if err != nil {
		return tui.Space{}, err
	}

	usage := space.Usage{LibraryBytes: held, FreeBytes: free}
	return tui.Space{
		Held:  held,
		Cap:   a.limits.MaxSize.Bytes(),
		Free:  free,
		Level: string(space.Watch(a.limits, usage)),
	}, nil
}

// Start implements tui.Controller.
func (c controllerAdapter) Start(name string) error { return c.manager.Start(name) }

// Stop implements tui.Controller.
func (c controllerAdapter) Stop(name string) error { return c.manager.Stop(name) }

// Status implements tui.Controller.
func (c controllerAdapter) Status(name string) (tui.Status, error) {
	status, err := c.manager.Status(name)
	if err != nil {
		return tui.Status{}, err
	}
	return tui.Status{State: string(status.State), Detail: status.Detail}, nil
}
