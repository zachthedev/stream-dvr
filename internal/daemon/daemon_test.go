package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
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
// Fakes
// ///////////////////////////////////////////////

// fakeEngine returns scripted probe and capture results.
type fakeEngine struct {
	mu sync.Mutex

	probe    record.Probe
	probeErr error
	probes   int
	// probeScript answers each probe in turn, with the last answer
	// repeating. It is what lets a test drive a channel that fails, comes
	// back, and fails again without depending on when the poll lands.
	probeScript []error

	captureResult    record.Result
	captureErr       error
	captures         int
	lastRequest      record.Request
	blockUntilCancel bool
	// lingerOnCancel is how long a cancelled capture takes to unwind, so a
	// caller that fails to wait for it is caught rather than raced.
	lingerOnCancel time.Duration
	// captureReturned records that a capture finished. Run claims to return
	// only once every watcher has stopped, and this is what proves it.
	captureReturned bool
	// onCapture runs on the caller's goroutine as the capture starts, so a
	// test can move the clock across a capture, or write the bytes a real
	// engine would be writing, the way a real one does.
	onCapture func(record.Request)
	// capturing is closed once a capture has begun, so a test blocks on one
	// rather than watching for one.
	//
	// A poll loop here competes with the goroutine it is waiting on. On two
	// shared cores under coverage instrumentation that is the difference
	// between a capture starting late and a test reporting one that never
	// started. told is what keeps the close to once.
	capturing chan struct{}
	told      bool
}

// fakeFinalizer returns a scripted organizer outcome.
type fakeFinalizer struct {
	// store lets the fake finish a recording the way the real organizer
	// does. The daemon deliberately leaves a recording pending until the
	// organizer completes it, so a fake that skipped this would let a
	// daemon test pass while nothing ever reached the library.
	store   *store.Store
	outcome organize.Outcome
	err     error
	calls   []int64

	// released records what the trash release deleted, and releaseErr
	// drives the path where a purged recording will not delete.
	released   []int64
	releaseErr error
	// onCall runs before the scripted outcome, so a test can cancel the
	// context the way a shutdown does with a finalize already under way.
	onCall func()

	// recompressed records what was re-encoded and what each call kept,
	// and recompressErr drives the path where an encode fails.
	recompressed  []int64
	keptOriginals []bool
	recompressErr error
}

// countingEncoder counts how often the encoder probe ran.
type countingEncoder struct {
	*fakeEncoder
	listed int
}

// fakeEncoder stands in for the machine's encoders.
type fakeEncoder struct {
	available []post.Encoder
	listErr   error
	encodeErr error

	transcoded []string
	quality    int
}

// recordingNotifier collects every event it is given.
type recordingNotifier struct {
	mu     sync.Mutex
	events []Event

	// waiters holds one channel per kind something is waiting on, closed
	// when an event of that kind arrives. A waiter is what lets a test
	// block on an event rather than watch for one.
	waiters map[EventKind]chan struct{}
}

type harness struct {
	t         *testing.T
	daemon    *Daemon
	root      string
	store     *store.Store
	engine    *fakeEngine
	finalizer *fakeFinalizer
	encoder   *fakeEncoder
	notifier  *recordingNotifier
	entry     config.Channel
	channel   store.Channel
	free      int64

	// The clock is read from whichever goroutine the daemon runs a decision
	// on, so a test that moves it mid-capture has to take the lock too.
	clockMu sync.Mutex
	clock   time.Time

	// broadcastStart answers when a broadcast really began. Nil answers that
	// nobody could, which is a machine with no metadata session watching a
	// channel that publishes no archive.
	//
	// startMu guards it and the count, because the lookup runs on whichever
	// goroutine polled the channel.
	startMu                sync.Mutex
	broadcastStart         func(ctx context.Context, channelURL, streamID string) (time.Time, bool)
	broadcastStartCalls    int
	broadcastStartStreamID string
}

// ///////////////////////////////////////////////
// Harness
// ///////////////////////////////////////////////

// eventWaitBudget bounds how long a test waits for a running daemon to
// report an event.
//
// A backstop against a daemon that never reports, not a claim about how
// fast one does. Everything these tests wait on is decided on the first
// pass of a loop, so a passing run spends milliseconds here whatever the
// number is. What sets it is the slowest machine the suite runs on: two
// shared cores, coverage instrumentation on every statement, and a dozen
// packages under test at once, where a goroutine can wait tens of seconds
// for the scheduler. Half a minute has proved short enough to fail there
// on a daemon that was working.
const eventWaitBudget = 90 * time.Second

// now is a fixed clock so decisions are decidable.
var now = time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

// captureStarted returns a channel closed once a capture has begun, closed
// already if one has.
func (f *fakeEngine) captureStarted() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.captures > 0 {
		f.tellCapturing()
	}
	if f.capturing == nil {
		f.capturing = make(chan struct{})
	}
	return f.capturing
}

// tellCapturing releases anything waiting on a capture. The caller holds mu.
func (f *fakeEngine) tellCapturing() {
	if f.told {
		return
	}
	if f.capturing == nil {
		f.capturing = make(chan struct{})
	}
	f.told = true
	close(f.capturing)
}

func (f *fakeEngine) Probe(context.Context, string) (record.Probe, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++

	if len(f.probeScript) > 0 {
		return f.probe, f.probeScript[min(f.probes-1, len(f.probeScript)-1)]
	}
	return f.probe, f.probeErr
}

func (f *fakeEngine) Capture(ctx context.Context, req record.Request) (record.Result, error) {
	f.mu.Lock()
	f.captures++
	f.tellCapturing()
	f.lastRequest = req
	block := f.blockUntilCancel
	hook := f.onCapture
	result, err := f.captureResult, f.captureErr
	f.mu.Unlock()

	if hook != nil {
		hook(req)
	}

	// A real capture runs until the broadcast ends or the context is
	// cancelled, which is what the watermark relies on to stop it.
	if block {
		<-ctx.Done()
	}

	f.mu.Lock()
	linger := f.lingerOnCancel
	f.mu.Unlock()
	time.Sleep(linger)

	f.mu.Lock()
	f.captureReturned = true
	f.mu.Unlock()
	return result, err
}

// finishedCapturing reports whether a capture has run to its end.
func (f *fakeEngine) finishedCapturing() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.captureReturned
}

func (f *fakeEngine) captureCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.captures
}

func (f *fakeFinalizer) Finalize(_ context.Context, recordingID int64) (organize.Outcome, error) {
	f.calls = append(f.calls, recordingID)
	if f.onCall != nil {
		f.onCall()
	}

	if f.store != nil && f.err == nil && !f.outcome.Parked {
		if err := f.store.SetState(recordingID, store.StateComplete); err != nil {
			return organize.Outcome{}, err
		}
	}
	return f.outcome, f.err
}

// Release deletes the row the way the organizer does, so a test driving
// the trash release sees the budget move as it would in production.
func (f *fakeFinalizer) Release(_ context.Context, recordingID int64) error {
	f.released = append(f.released, recordingID)
	if f.releaseErr != nil {
		return f.releaseErr
	}
	if f.store == nil {
		return nil
	}
	return f.store.DeleteRecording(recordingID)
}

// Recompress records the call and marks the row the way the real organizer
// does, so a second pass does not offer the same recording again.
func (f *fakeFinalizer) Recompress(_ context.Context, recordingID int64, keepOriginal bool,
	encode func(ctx context.Context, source, output string) error,
) error {
	f.recompressed = append(f.recompressed, recordingID)
	f.keptOriginals = append(f.keptOriginals, keepOriginal)

	if err := encode(context.Background(), "source.mkv", "source.mkv.recompressing"); err != nil {
		return err
	}
	if f.recompressErr != nil {
		return f.recompressErr
	}
	if f.store == nil {
		return nil
	}
	return f.store.MarkRecompressed(recordingID, now, 1<<29)
}

func (f *fakeEncoder) Encoders(context.Context) ([]post.Encoder, error) {
	return f.available, f.listErr
}

func (c *countingEncoder) Encoders(ctx context.Context) ([]post.Encoder, error) {
	c.listed++
	return c.fakeEncoder.Encoders(ctx)
}

func (f *fakeEncoder) Transcode(_ context.Context, source, _ string,
	_ post.Encoder, quality int,
) error {
	f.transcoded = append(f.transcoded, source)
	f.quality = quality
	return f.encodeErr
}

func (n *recordingNotifier) Notify(_ context.Context, event Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, event)
	if waiter, ok := n.waiters[event.Kind]; ok {
		close(waiter)
		delete(n.waiters, event.Kind)
	}
	return nil
}

// signal returns a channel closed once an event of this kind has arrived,
// closed already if one has.
//
// A waiter is registered before the daemon starts, so an event reported on
// its first pass cannot land in the gap between starting it and beginning
// to wait.
func (n *recordingNotifier) signal(kind EventKind) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()

	if existing, ok := n.waiters[kind]; ok {
		return existing
	}
	waiter := make(chan struct{})
	for _, event := range n.events {
		if event.Kind == kind {
			close(waiter)
			return waiter
		}
	}
	if n.waiters == nil {
		n.waiters = map[EventKind]chan struct{}{}
	}
	n.waiters[kind] = waiter
	return waiter
}

func (n *recordingNotifier) kinds() []EventKind {
	n.mu.Lock()
	defer n.mu.Unlock()
	kinds := make([]EventKind, 0, len(n.events))
	for _, event := range n.events {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

// all returns every event raised so far, for a test that reads a detail
// rather than only a kind.
func (n *recordingNotifier) all() []Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.events)
}

func (n *recordingNotifier) has(kind EventKind) bool {
	return n.count(kind) > 0
}

// waitFor runs the daemon until it reports an event, and reports whether one
// arrived.
//
// The daemon is cancelled the moment the event lands, so an ordinary run
// finishes in milliseconds. The deadline is generous because it is a
// backstop against never, not a measurement of how long the work takes: a
// fixed window instead asserts that a machine running the whole test suite in
// parallel scheduled this goroutine promptly, which is not a claim about the
// daemon at all.
func (n *recordingNotifier) waitFor(t *testing.T, daemon *Daemon, kind EventKind) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), eventWaitBudget)
	defer cancel()

	// Registered before the daemon starts, so the wait cannot begin after
	// the event it is waiting for.
	//
	// This blocks rather than polling. A poll loop here spent a timer and a
	// wakeup every millisecond for as long as the wait ran, which on a
	// two-core runner already loaded by coverage instrumentation competed
	// with the daemon goroutines it was waiting on. The event then arrived
	// late or not at all, and the test reported a daemon that never spoke
	// rather than a machine that never scheduled it.
	waiter := n.signal(kind)

	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()

	select {
	case <-waiter:
		cancel()
		<-done
		return
	case err := <-done:
		// Run returned first, so whatever it was going to report it has
		// reported. One last look before answering no.
		if err != nil && !errors.Is(err, context.DeadlineExceeded) &&
			!errors.Is(err, context.Canceled) {
			t.Fatalf("Run() err = %v, want nil", err)
		}
		if n.has(kind) {
			return
		}
		// The two ways of not getting an event are worth telling apart. A
		// daemon that ran out of budget was still running and may simply
		// never have been scheduled, where one that returned first decided
		// against reporting. Both read as "events = []" otherwise, and only
		// the second is about this code.
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("no %s within %s; events = %v. The daemon was still running when the budget ran out",
				kind, eventWaitBudget, n.kinds())
		}
		t.Fatalf("no %s; events = %v. The daemon stopped without reporting one", kind, n.kinds())
	}
}

func (n *recordingNotifier) count(kind EventKind) int {
	total := 0
	for _, got := range n.kinds() {
		if got == kind {
			total++
		}
	}
	return total
}

// newHarness builds a daemon over a temp library with one twitch channel.
func newHarness(t *testing.T, mutate func(*config.Config)) *harness {
	t.Helper()

	root := filepath.Join(t.TempDir(), "library")
	lib, err := library.Create(root, "test")
	if err != nil {
		t.Fatalf("library.Create() err = %v, want nil", err)
	}

	db, err := store.OpenMemory()
	if err != nil {
		t.Fatalf("store.OpenMemory() err = %v, want nil", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := config.DefaultConfig()
	cfg.Library.Root = root
	cfg.Notify.OnRecordingStart = true
	cfg.Channels = []config.Channel{
		{Platform: config.PlatformTwitch, Name: "examplechannel", Enabled: true},
	}
	if mutate != nil {
		mutate(&cfg)
	}

	h := &harness{
		t:         t,
		root:      root,
		store:     db,
		engine:    &fakeEngine{},
		finalizer: &fakeFinalizer{store: db},
		notifier:  &recordingNotifier{},
		clock:     now,
		free:      900 * config.Gigabyte.Bytes(),
		encoder: &fakeEncoder{available: []post.Encoder{
			{Name: "hevc_nvenc", Codec: config.CodecHEVC, Hardware: true},
		}},
	}
	// A test may clear the channel list to exercise an empty configuration.
	if len(cfg.Channels) > 0 {
		h.entry = cfg.Channels[0]
	}

	daemon, err := New(Options{
		Config:       cfg,
		Library:      lib,
		Store:        db,
		Engine:       h.engine,
		Finalizer:    h.finalizer,
		Recompressor: h.encoder,
		Notifier:     h.notifier,
		Now: func() time.Time {
			h.clockMu.Lock()
			defer h.clockMu.Unlock()
			return h.clock
		},
		BroadcastStart: func(ctx context.Context, channelURL, streamID string) (time.Time, bool) {
			h.startMu.Lock()
			h.broadcastStartCalls++
			h.broadcastStartStreamID = streamID
			resolver := h.broadcastStart
			h.startMu.Unlock()

			if resolver == nil {
				return time.Time{}, false
			}
			return resolver(ctx, channelURL, streamID)
		},
		FreeSpace: func(string) (int64, error) { return h.free, nil },
	})
	if err != nil {
		t.Fatalf("New() err = %v, want nil", err)
	}
	h.daemon = daemon

	channel, err := db.UpsertChannel(config.PlatformTwitch, "examplechannel", "ExampleChannel")
	if err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
	h.channel = channel
	return h
}

// startLookups reports how many times a broadcast's real start was asked
// for, which is what separates once per broadcast from once per poll.
func (h *harness) startLookups() int {
	h.startMu.Lock()
	defer h.startMu.Unlock()
	return h.broadcastStartCalls
}

// clockNow reads the fixed clock, for a test that moved it.
func (h *harness) clockNow() time.Time {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	return h.clock
}

// at moves the fixed clock.
func (h *harness) at(t time.Time) {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	h.clock = t
}

// interrupted registers a capture the way a crash mid-broadcast leaves it:
// a row still marked capturing, with whatever bytes reached disk.
func (h *harness) interrupted(relPath, content string) store.Recording {
	h.t.Helper()

	if content != "" {
		full := filepath.Join(h.root, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			h.t.Fatalf("creating the capture directory: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			h.t.Fatalf("writing the capture: %v", err)
		}
	}

	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: relPath,
		State: store.StateCapturing, Origin: store.OriginLive, StartedAt: now.Add(-2 * time.Hour),
	})
	if err != nil {
		h.t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	return recording
}

// live scripts the engine as broadcasting with metadata.
func (h *harness) live(title string) {
	h.engine.probe = record.Probe{
		Live:      true,
		Qualities: []string{"best"},
		Metadata:  record.Metadata{ID: "stream-1", Author: "ExampleChannel", Title: title, Category: "Just Chatting"},
	}
}

// captured scripts a successful capture of the given size and length.
func (h *harness) captured(bytes int64, duration time.Duration) {
	h.engine.captureResult = record.Result{
		Bytes: bytes, StartedAt: now, EndedAt: now.Add(duration),
	}
}

// ///////////////////////////////////////////////
// Construction
// ///////////////////////////////////////////////

func TestNew_RequiresCollaborators(t *testing.T) {
	root := filepath.Join(t.TempDir(), "library")
	lib, err := library.Create(root, "test")
	if err != nil {
		t.Fatalf("library.Create() err = %v, want nil", err)
	}
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatalf("store.OpenMemory() err = %v, want nil", err)
	}
	defer db.Close()

	tests := []struct {
		name string
		opts Options
	}{
		{name: "no library", opts: Options{Store: db, Engine: &fakeEngine{}, Finalizer: &fakeFinalizer{}}},
		{name: "no store", opts: Options{Library: lib, Engine: &fakeEngine{}, Finalizer: &fakeFinalizer{}}},
		{name: "no engine", opts: Options{Library: lib, Store: db, Finalizer: &fakeFinalizer{}}},
		{name: "no finalizer", opts: Options{Library: lib, Store: db, Engine: &fakeEngine{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.opts); err == nil {
				t.Error("New() err = nil, want a rejection")
			}
		})
	}
}

// ///////////////////////////////////////////////
// Sessions and downtime
// ///////////////////////////////////////////////

func TestStartSession_FirstRunHasNoDowntime(t *testing.T) {
	h := newHarness(t, nil)

	_, downtime, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if downtime != nil {
		t.Errorf("Downtime = %+v, want nil on a first run", downtime)
	}
}

func TestStartSession_ReportsACrashGap(t *testing.T) {
	// A recorder that dies without recording a stop leaves no other trace:
	// the channels simply stop being recorded, and the gap can run for days
	// before anyone notices.
	h := newHarness(t, nil)

	crashed, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("first StartSession() err = %v, want nil", err)
	}
	h.at(now.Add(2 * time.Hour))
	if err := h.daemon.Heartbeat(crashed.ID); err != nil {
		t.Fatalf("Heartbeat() err = %v, want nil", err)
	}

	h.at(now.Add(96 * time.Hour))
	_, downtime, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("second StartSession() err = %v, want nil", err)
	}

	if downtime == nil {
		t.Fatal("Downtime = nil, want the gap reported")
	}
	if !downtime.Crashed {
		t.Error("Downtime.Crashed = false, want true for a session with no clean stop")
	}
	if downtime.Since != 94*time.Hour {
		t.Errorf("Downtime.Since = %s, want %s", downtime.Since, 94*time.Hour)
	}
	if !h.notifier.has(EventDowntime) {
		t.Errorf("events = %v, want a downtime notification", h.notifier.kinds())
	}
}

func TestStartSession_BoundsADowntimeAHeartbeatCouldNotHaveMeasured(t *testing.T) {
	// A duration counts nanoseconds in an int64, so two instants more than
	// 292 years apart subtract to a saturated value. The store accepts any
	// timestamp from 1677 to 2262, so a heartbeat well inside what it will
	// hold is already far enough to overflow, and the operator is then told
	// the recorder was down for 292 years. The previous session's own start
	// is a real measurement whatever the heartbeat says, and it bounds the
	// gap.
	h := newHarness(t, nil)

	first, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("first StartSession() err = %v, want nil", err)
	}
	h.at(time.Date(1700, time.January, 1, 0, 0, 0, 0, time.UTC))
	if err := h.daemon.Heartbeat(first.ID); err != nil {
		t.Fatalf("Heartbeat() err = %v, want nil", err)
	}

	h.at(now.Add(96 * time.Hour))
	_, downtime, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("second StartSession() err = %v, want nil", err)
	}

	if downtime == nil {
		t.Fatal("Downtime = nil, want the gap reported")
	}
	if downtime.Since != 96*time.Hour {
		t.Errorf("Downtime.Since = %s, want it bounded to %s", downtime.Since, 96*time.Hour)
	}
}

func TestStartSession_SubstitutesTheHorizonForASessionDatedImpossibly(t *testing.T) {
	// When the previous session's own start is not a measurement either,
	// there is nothing left to bound the gap with. Both orderings the store
	// cannot honestly produce end the same way: the horizon stands in, and
	// the row that forced it is named, because the substituted figure is
	// what the operator is then told an outage was.
	tests := []struct {
		name      string
		startedAt time.Time
		wantSince time.Duration
	}{
		// Timestamps decode through time.Unix, so a zeroed column reads as
		// 1970 rather than as absent, and describes fifty-six years of
		// downtime without overflowing anything. Nothing is left to measure
		// against, so the horizon stands in.
		{
			name:      "a zeroed start column",
			startedAt: time.Unix(0, 0).UTC(),
			wantSince: recoveryHorizon,
		},
		// LastSession orders by id, not by time, so a clock stepping back
		// between runs leaves a previous session that starts after this one.
		// The recorder was up either side of the step, so nothing was
		// missed and the substituted bound never binds.
		{
			name:      "a start after this session began",
			startedAt: now.Add(48 * time.Hour),
			wantSince: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			messages := &messageLog{}
			h.daemon.logger = slog.New(messages)

			h.at(tt.startedAt)
			first, _, err := h.daemon.StartSession(context.Background())
			if err != nil {
				t.Fatalf("first StartSession() err = %v, want nil", err)
			}
			// Closed, so the next start reads the row rather than refusing
			// to claim a library a session dated in the future still holds.
			if err := h.daemon.StopSession(first.ID); err != nil {
				t.Fatalf("StopSession() err = %v, want nil", err)
			}

			h.at(now)
			_, downtime, err := h.daemon.StartSession(context.Background())
			if err != nil {
				t.Fatalf("second StartSession() err = %v, want nil", err)
			}

			if downtime == nil {
				t.Fatal("Downtime = nil, want the gap reported")
			}
			if downtime.Since != tt.wantSince {
				t.Errorf("Downtime.Since = %s, want %s", downtime.Since, tt.wantSince)
			}
			if !messages.mentions("dated impossibly") {
				t.Error("a session dated impossibly was corrected silently, want it named")
			}
		})
	}
}

func TestStartSession_CleanShutdownIsNotACrash(t *testing.T) {
	h := newHarness(t, nil)

	session, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	h.at(now.Add(time.Hour))
	if err := h.daemon.StopSession(session.ID); err != nil {
		t.Fatalf("StopSession() err = %v, want nil", err)
	}

	h.at(now.Add(2 * time.Hour))
	_, downtime, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if downtime == nil {
		t.Fatal("Downtime = nil, want a gap reported")
	}
	if downtime.Crashed {
		t.Error("Downtime.Crashed = true, want false after a clean stop")
	}
	if h.notifier.has(EventDowntime) {
		t.Error("a clean shutdown raised a downtime notification, want none")
	}
}

// ///////////////////////////////////////////////
// Run
// ///////////////////////////////////////////////

func TestRun_RecordsACleanStop(t *testing.T) {
	// A run that ends on cancellation must still record a clean stop, or
	// every intentional shutdown looks like a crash on the next start.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Capture.PollInterval = config.Duration(10 * time.Millisecond)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := h.daemon.Run(ctx); err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}

	h.at(now.Add(time.Hour))
	_, downtime, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if downtime == nil {
		t.Fatal("Downtime = nil, want the previous session found")
	}
	if downtime.Crashed {
		t.Error("Downtime.Crashed = true, want a cancelled run to stop cleanly")
	}
}

func TestRecordStop_ASessionRowThatIsGoneIsNotAFailedShutdown(t *testing.T) {
	// The database is documented as a cache over the library, rebuildable
	// from the sidecars, so losing the session row mid-run is a supported
	// thing to happen. A shutdown that cannot find the row it opened has
	// still shut down, and returning an error would make an orderly stop
	// exit non-zero and read to the next start as the crash it is not.
	//
	// Run defers exactly one call to recordStop, so this drives the whole of
	// the decision. An id no row carries is what a rebuild leaves the daemon
	// holding, and nothing in the store's public surface deletes a session,
	// so the absent id stands in rather than the store gaining a delete no
	// production code would call.
	h := newHarness(t, nil)
	session, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	absent := session.ID + 1
	if stopErr := h.daemon.StopSession(absent); !errors.Is(stopErr, store.ErrNotFound) {
		t.Fatalf("StopSession(absent) err = %v, want ErrNotFound; the premise of this test is gone", stopErr)
	}

	if err := h.daemon.recordStop(absent, nil); err != nil {
		t.Errorf("recordStop() err = %v, want a lost session row not to fail the shutdown", err)
	}

	// A real failure from the run still wins, so the tolerance above cannot
	// swallow one.
	runErr := errors.New("the run itself failed")
	if got := h.daemon.recordStop(absent, runErr); !errors.Is(got, runErr) {
		t.Errorf("recordStop() err = %v, want the run's own error preserved", got)
	}
}

func TestRun_ShutsDownCleanlyWithACaptureInFlight(t *testing.T) {
	// This is the whole graceful path, exercised the way a stop signal
	// cancels it. Every claim Run makes has to hold with a broadcast being
	// recorded at the moment it arrives: the watchers stop, the recording
	// is left in a state the next start retries, the session records a
	// clean stop, and none of it goes out as a failure.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Capture.PollInterval = config.Duration(5 * time.Millisecond)
	})
	h.live("a title")
	h.captured(1<<30, 4*time.Hour)
	h.engine.blockUntilCancel = true
	// A real capture takes a moment to unwind after the signal: streamlink
	// is killed, its exit is collected, and the file is sized.
	h.engine.lingerOnCancel = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.daemon.Run(ctx) }()

	// Wait for a capture to be under way, so the cancellation lands on one.
	select {
	case <-h.engine.captureStarted():
	case <-time.After(eventWaitBudget):
		cancel()
		t.Fatal("no capture started, want the cancellation to land mid-recording")
	}
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() err = %v, want nil", err)
		}
	case <-time.After(eventWaitBudget):
		t.Fatal("Run() did not return, want every watcher stopped before it does")
	}

	// Returning while a capture is still unwinding would leave the caller
	// closing the database out from under it.
	if !h.engine.finishedCapturing() {
		t.Error("Run() returned with a capture still running, want it to wait for every watcher")
	}

	queued, err := h.store.RecordingsByState(store.PendingStates...)
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}
	if len(queued) != 1 {
		t.Errorf("sweep queue holds %d recordings, want the interrupted one left for the next start", len(queued))
	}
	if h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want an orderly shutdown reported as no failure", h.notifier.kinds())
	}

	h.at(now.Add(time.Hour))
	_, downtime, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if downtime == nil {
		t.Fatal("Downtime = nil, want the previous session found")
	}
	if downtime.Crashed {
		t.Error("Downtime.Crashed = true, want a signalled shutdown recorded as a clean stop")
	}
}

func TestRun_WithNoChannelsStillRuns(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Channels = nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := h.daemon.Run(ctx); err != nil {
		t.Errorf("Run() err = %v, want nil", err)
	}
}

func TestRun_RefusesALibraryAnotherRecorderHolds(t *testing.T) {
	// The installed service and a plain serve are the same daemon against
	// the same library, so whichever starts second has to be refused.
	h := newHarness(t, nil)

	if _, err := h.store.StartSession(now, StaleAfter); err != nil {
		t.Fatalf("seeding a live recorder: %v", err)
	}

	err := h.daemon.Run(context.Background())

	if !errors.Is(err, store.ErrRecorderRunning) {
		t.Fatalf("Run() err = %v, want ErrRecorderRunning", err)
	}
	// The claim comes before anything else, so a refused start leaves the
	// library exactly as it found it.
	if got := h.engine.captureCount(); got != 0 {
		t.Errorf("the refused run started %d captures, want none", got)
	}
	if len(h.finalizer.calls) != 0 {
		t.Errorf("the refused run finalized %v, want nothing", h.finalizer.calls)
	}
}

// ///////////////////////////////////////////////
// Recompress
// ///////////////////////////////////////////////

// recompressHarness returns a daemon with the rung on and one recording
// past its age.
func recompressHarness(t *testing.T, mutate func(*config.Config)) (*harness, store.Recording) {
	t.Helper()

	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.Recompress.Enabled = true
		cfg.Space.Recompress.After = config.Duration(30 * 24 * time.Hour)
		cfg.Space.Recompress.Codec = config.CodecHEVC
		if mutate != nil {
			mutate(cfg)
		}
	})

	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID,
		Path:      "ExampleChannel/2026/old.mkv",
		State:     store.StateComplete,
		Origin:    store.OriginLive,
		Bytes:     1 << 30,
		StartedAt: now.Add(-90 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	return h, recording
}

func TestRecompressPass_ReEncodesWhatIsPastItsAge(t *testing.T) {
	h, recording := recompressHarness(t, nil)

	if err := h.daemon.RecompressPass(context.Background()); err != nil {
		t.Fatalf("RecompressPass() err = %v, want nil", err)
	}

	if !slices.Contains(h.finalizer.recompressed, recording.ID) {
		t.Errorf("Recompress ran for %v, want it to include %d",
			h.finalizer.recompressed, recording.ID)
	}
	if len(h.encoder.transcoded) != 1 {
		t.Errorf("Transcode ran %d times, want 1", len(h.encoder.transcoded))
	}
	if h.encoder.quality != h.daemon.config.Space.Recompress.Quality {
		t.Errorf("Transcode at quality %d, want the configured %d",
			h.encoder.quality, h.daemon.config.Space.Recompress.Quality)
	}
}

func TestRecompressPass_DoesNothingUntilTheOperatorTurnsItOn(t *testing.T) {
	// A re-encode without a hardware encoder runs under realtime, so a
	// four-hour broadcast can cost sixteen hours of CPU. That is a price
	// only the machine's owner can agree to.
	h, _ := recompressHarness(t, func(cfg *config.Config) {
		cfg.Space.Recompress.Enabled = false
	})

	if err := h.daemon.RecompressPass(context.Background()); err != nil {
		t.Fatalf("RecompressPass() err = %v, want nil", err)
	}

	if len(h.finalizer.recompressed) != 0 {
		t.Errorf("the disabled rung re-encoded %v", h.finalizer.recompressed)
	}
	if len(h.encoder.transcoded) != 0 {
		t.Errorf("the disabled rung ran %d transcodes", len(h.encoder.transcoded))
	}
}

func TestRecompressPass_LeavesARecordingInsideItsAge(t *testing.T) {
	h, _ := recompressHarness(t, func(cfg *config.Config) {
		cfg.Space.Recompress.After = config.Duration(365 * 24 * time.Hour)
	})

	if err := h.daemon.RecompressPass(context.Background()); err != nil {
		t.Fatalf("RecompressPass() err = %v, want nil", err)
	}

	if len(h.finalizer.recompressed) != 0 {
		t.Errorf("a recording inside its age was re-encoded: %v", h.finalizer.recompressed)
	}
}

func TestRecompressPass_OffersEachRecordingOnce(t *testing.T) {
	// Re-encoding what is already re-encoded costs hours and loses picture
	// for nothing.
	h, _ := recompressHarness(t, nil)

	for range 2 {
		if err := h.daemon.RecompressPass(context.Background()); err != nil {
			t.Fatalf("RecompressPass() err = %v, want nil", err)
		}
	}

	if len(h.finalizer.recompressed) != 1 {
		t.Errorf("Recompress ran for %v, want one recording once", h.finalizer.recompressed)
	}
}

func TestRecompressPass_ProbesTheEncodersOncePerPass(t *testing.T) {
	// The probe runs a subprocess per candidate encoder, and the answer
	// cannot change between two recordings in one pass.
	h, _ := recompressHarness(t, nil)
	for i := range 3 {
		if _, err := h.store.CreateRecording(store.Recording{
			ChannelID: h.channel.ID,
			Path:      fmt.Sprintf("ExampleChannel/2026/old-%d.mkv", i),
			State:     store.StateComplete,
			Origin:    store.OriginLive,
			StartedAt: now.Add(-90 * 24 * time.Hour),
		}); err != nil {
			t.Fatalf("CreateRecording() err = %v, want nil", err)
		}
	}

	counting := &countingEncoder{fakeEncoder: h.encoder}
	h.daemon.encoder = counting

	if err := h.daemon.RecompressPass(context.Background()); err != nil {
		t.Fatalf("RecompressPass() err = %v, want nil", err)
	}

	if len(h.finalizer.recompressed) != 4 {
		t.Fatalf("Recompress ran for %v, want all four", h.finalizer.recompressed)
	}
	if counting.listed != 1 {
		t.Errorf("Encoders() probed %d times for one pass, want 1", counting.listed)
	}
}

func TestRecompressPass_CarriesOnAfterOneRecordingFails(t *testing.T) {
	// A pass is the answer to a filling library. Stopping at the first
	// recording whose encode fails frees nothing.
	h, _ := recompressHarness(t, nil)
	if _, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID,
		Path:      "ExampleChannel/2026/older.mkv",
		State:     store.StateComplete,
		Origin:    store.OriginLive,
		StartedAt: now.Add(-120 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	h.encoder.encodeErr = errors.New("the encoder went away")

	err := h.daemon.RecompressPass(context.Background())

	if err == nil {
		t.Fatal("RecompressPass() err = nil, want the failures reported")
	}
	if len(h.finalizer.recompressed) != 2 {
		t.Errorf("Recompress ran for %v, want both attempted", h.finalizer.recompressed)
	}
}

func TestRecompressPass_ReportsAnEncoderItCannotList(t *testing.T) {
	h, _ := recompressHarness(t, nil)
	h.encoder.listErr = errors.New("ffmpeg is not on the path")

	err := h.daemon.RecompressPass(context.Background())

	if err == nil || !strings.Contains(err.Error(), "not on the path") {
		t.Errorf("RecompressPass() err = %v, want the probe failure reported", err)
	}
}

func TestRecompressPass_PassesKeepOriginalThrough(t *testing.T) {
	h, _ := recompressHarness(t, func(cfg *config.Config) {
		cfg.Space.Recompress.KeepOriginal = true
	})

	if err := h.daemon.RecompressPass(context.Background()); err != nil {
		t.Fatalf("RecompressPass() err = %v, want nil", err)
	}

	if len(h.finalizer.keptOriginals) != 1 || !h.finalizer.keptOriginals[0] {
		t.Errorf("keepOriginal reached the organizer as %v, want true", h.finalizer.keptOriginals)
	}
}

func TestRecompressPass_StopsOnCancellation(t *testing.T) {
	h, _ := recompressHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := h.daemon.RecompressPass(ctx); err != nil {
		t.Errorf("RecompressPass() err = %v on a cancelled context, want nil", err)
	}
	if len(h.finalizer.recompressed) != 0 {
		t.Errorf("a cancelled pass re-encoded %v", h.finalizer.recompressed)
	}
}

func TestDaemon_BoundsCaptureConcurrency(t *testing.T) {
	// Each recording costs bandwidth and disk throughput, so unbounded
	// concurrency degrades every capture rather than only the newest.
	h := newHarness(t, nil)
	h.daemon.captureSlots = make(chan struct{}, 1)

	if err := h.daemon.acquireSlot(context.Background()); err != nil {
		t.Fatalf("acquireSlot() err = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.daemon.acquireSlot(ctx); err == nil {
		t.Error("acquireSlot() err = nil while the only slot was held, want it to block until cancelled")
	}

	h.daemon.releaseSlot()
	if err := h.daemon.acquireSlot(context.Background()); err != nil {
		t.Errorf("acquireSlot() after release err = %v, want nil", err)
	}
}

func TestDaemon_UnboundedCaptureSlots(t *testing.T) {
	// A direct RunCycle caller has no Run to allocate slots, and must not
	// deadlock waiting for one.
	h := newHarness(t, nil)
	h.daemon.captureSlots = nil

	if err := h.daemon.acquireSlot(context.Background()); err != nil {
		t.Errorf("acquireSlot() err = %v, want nil when slots are unbounded", err)
	}
	h.daemon.releaseSlot()
}

// ///////////////////////////////////////////////
// Recordings an unclean exit interrupted
// ///////////////////////////////////////////////

func TestRun_RecoversARecordingInterruptedByAnUncleanExit(t *testing.T) {
	// A row stays capturing for the whole broadcast, so a power loss or a
	// reboot part way through leaves it there with a playable capture in
	// incoming. Nothing sweeps capturing, so without this the recording is
	// stranded for good and its bytes never count against the disk budget.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Capture.PollInterval = config.Duration(10 * time.Millisecond)
	})
	recording := h.interrupted(filepath.Join("incoming", "twitch-examplechannel-1.ts"), "captured bytes")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := h.daemon.Run(ctx); err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}

	stored, err := h.store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if stored.State == store.StateCapturing {
		t.Error("State = capturing, want the interrupted recording moved out of it")
	}
	if stored.Bytes != int64(len("captured bytes")) {
		t.Errorf("Bytes = %d, want the %d that reached disk, or the library reads smaller than it is",
			stored.Bytes, len("captured bytes"))
	}
	if !slices.Contains(h.finalizer.calls, recording.ID) {
		t.Errorf("finalizer calls = %v, want the interrupted recording organized", h.finalizer.calls)
	}
}

func TestRun_AnInterruptedRecordingWithNoBytesIsFailed(t *testing.T) {
	// A capture that died before writing anything has nothing to organize.
	// Leaving it pending would put it back in front of every sweep forever.
	tests := []struct {
		name    string
		content string
	}{
		{name: "no file at all", content: ""},
		{name: "an empty file", content: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(cfg *config.Config) {
				cfg.Capture.PollInterval = config.Duration(10 * time.Millisecond)
			})
			recording := h.interrupted(filepath.Join("incoming", "twitch-examplechannel-1.ts"), tt.content)

			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			if err := h.daemon.Run(ctx); err != nil {
				t.Fatalf("Run() err = %v, want nil", err)
			}

			stored, err := h.store.Recording(recording.ID)
			if err != nil {
				t.Fatalf("Recording() err = %v, want nil", err)
			}
			if stored.State != store.StateFailed {
				t.Errorf("State = %q, want %q", stored.State, store.StateFailed)
			}
			if len(h.finalizer.calls) != 0 {
				t.Errorf("finalizer calls = %v, want a recording with no bytes left alone", h.finalizer.calls)
			}
		})
	}
}

func TestRun_RecordsTheStopWhenStartupFails(t *testing.T) {
	// A startup that fails after opening the session row still has to record
	// the stop. Leaving stopped_at null would have the next start report a
	// crash and fire a downtime notification for an orderly failure.
	h := newHarness(t, nil)
	h.interrupted(filepath.Join("incoming", "twitch-examplechannel-1.ts"), "captured bytes")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.daemon.Run(ctx); err == nil {
		t.Fatal("Run() err = nil, want the cancelled startup reported")
	}

	h.at(now.Add(time.Hour))
	_, downtime, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if downtime == nil {
		t.Fatal("Downtime = nil, want the previous session found")
	}
	if downtime.Crashed {
		t.Error("Downtime.Crashed = true, want a startup failure recorded as a clean stop")
	}
	if h.notifier.has(EventDowntime) {
		t.Error("a failed startup raised a downtime notification, want none")
	}
}

func TestSyncChannels(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Channels = []config.Channel{
			{Platform: config.PlatformTwitch, Name: "examplechannel", Enabled: true},
			{Platform: config.PlatformYouTube, Name: "someone", Enabled: true},
			{Platform: config.PlatformTwitch, Name: "idle", Enabled: false},
		}
	})

	channels, err := h.daemon.SyncChannels()
	if err != nil {
		t.Fatalf("SyncChannels() err = %v, want nil", err)
	}
	if len(channels) != 2 {
		t.Fatalf("SyncChannels() registered %d channels, want the 2 enabled ones", len(channels))
	}
}

func TestSyncChannels_DoesNotEraseAKnownDisplayName(t *testing.T) {
	// Config carries no display name, so syncing must not overwrite one
	// learned from a probe, or naming would fall back for no reason.
	h := newHarness(t, nil)

	if _, err := h.daemon.SyncChannels(); err != nil {
		t.Fatalf("SyncChannels() err = %v, want nil", err)
	}
	got, err := h.store.Channel(config.PlatformTwitch, "examplechannel")
	if err != nil {
		t.Fatalf("Channel() err = %v, want nil", err)
	}
	if got.DisplayName != "ExampleChannel" {
		t.Errorf("DisplayName = %q, want the known %q preserved", got.DisplayName, "ExampleChannel")
	}
}

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

func TestChannelURL(t *testing.T) {
	tests := []struct {
		name  string
		entry config.Channel
		want  string
	}{
		{
			name:  "twitch",
			entry: config.Channel{Platform: config.PlatformTwitch, Name: "examplechannel"},
			want:  "https://twitch.tv/examplechannel",
		},
		{
			name:  "youtube",
			entry: config.Channel{Platform: config.PlatformYouTube, Name: "someone"},
			want:  "https://www.youtube.com/@someone/live",
		},
		{
			name:  "a url channel is passed through",
			entry: config.Channel{Platform: config.PlatformURL, Name: "https://example.com/live"},
			want:  "https://example.com/live",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := channelURL(tt.entry); got != tt.want {
				t.Errorf("channelURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProbeInterval_IsSlowerThanLivenessPolling(t *testing.T) {
	// Every metadata probe is a request to the platform, and a title
	// changes far less often than a channel goes live.
	cfg := config.DefaultConfig()
	got := probeInterval(cfg)

	if got <= cfg.Capture.PollInterval.Std() {
		t.Errorf("probeInterval() = %s, want it slower than the %s liveness poll",
			got, cfg.Capture.PollInterval.Std())
	}
	if got < time.Minute {
		t.Errorf("probeInterval() = %s, want at least a minute", got)
	}
}

func TestRoundDuration(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "long gaps drop stray seconds", in: 94*time.Hour + 20*time.Second, want: 94 * time.Hour},
		{name: "long gaps round up past the half minute", in: 94*time.Hour + 40*time.Second, want: 94*time.Hour + time.Minute},
		{name: "short gaps round to seconds", in: 90*time.Second + 400*time.Millisecond, want: 90 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := roundDuration(tt.in); got != tt.want {
				t.Errorf("roundDuration(%s) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

func TestDaemon_NotifyRespectsConfiguration(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Notify.OnRecordingStart = false
		cfg.Notify.OnFailure = true
		cfg.Notify.OnLibraryFull = true
	})

	h.daemon.notify(context.Background(), Event{Kind: EventRecordingStarted})
	h.daemon.notify(context.Background(), Event{Kind: EventFailure})

	if h.notifier.has(EventRecordingStarted) {
		t.Error("a disabled event was delivered, want it suppressed")
	}
	if !h.notifier.has(EventFailure) {
		t.Error("an enabled event was suppressed, want it delivered")
	}
}

func TestDaemon_NotifySwallowsASinkFailure(t *testing.T) {
	// A webhook that is down must never take a recording down with it.
	// notify swallows the error by design, so the assertion is that the
	// call returns at all rather than panicking or blocking.
	h := newHarness(t, nil)
	h.daemon.notifier = failingNotifier{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.notify(context.Background(), Event{Kind: EventFailure, Detail: "x"})
	}()

	select {
	case <-done:
	case <-time.After(eventWaitBudget):
		t.Error("notify() did not return, want a failing sink to be swallowed")
	}
}

// ///////////////////////////////////////////////
// Bitrate estimation
// ///////////////////////////////////////////////

func TestBitrateFor_UsesTheChannelsOwnHistory(t *testing.T) {
	h := newHarness(t, nil)

	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "a.mkv",
		State: store.StateComplete, Origin: store.OriginLive, StartedAt: now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	if err := h.store.FinishRecording(recording.ID, store.StateComplete, "a.mkv",
		3600*2_000_000, time.Hour, now.Add(-23*time.Hour)); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}

	if got := h.daemon.bitrateFor(h.channel.ID); got != 2_000_000 {
		t.Errorf("bitrateFor() = %d, want 2000000 from the channel's own history", got)
	}
}

func TestBitrateFor_NoHistory(t *testing.T) {
	h := newHarness(t, nil)

	if got := h.daemon.bitrateFor(h.channel.ID); got != 0 {
		t.Errorf("bitrateFor() = %d, want 0 with no history so the default is used", got)
	}
}

func TestBitrateFor_IsDerivedOncePerLiveSession(t *testing.T) {
	// A refused channel is admitted again on every poll for as long as it
	// stays live, and each admission reads six months of rows. Only a
	// completed recording changes the answer, and the channel is still
	// broadcasting.
	h := newHarness(t, nil)

	h.daemon.sessionStart(h.channel.ID, "stream-1", now)
	if got := h.daemon.bitrateFor(h.channel.ID); got != 0 {
		t.Fatalf("bitrateFor() = %d, want 0 before the channel has any history", got)
	}

	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "a.mkv",
		State: store.StateComplete, Origin: store.OriginLive, StartedAt: now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	if err := h.store.FinishRecording(recording.ID, store.StateComplete, "a.mkv",
		3600*2_000_000, time.Hour, now.Add(-23*time.Hour)); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}

	if got := h.daemon.bitrateFor(h.channel.ID); got != 0 {
		t.Errorf("bitrateFor() = %d, want the estimate this session already made", got)
	}

	h.daemon.endSession(h.channel.ID)
	if got := h.daemon.bitrateFor(h.channel.ID); got != 2_000_000 {
		t.Errorf("bitrateFor() = %d, want the new history read once the session ended", got)
	}
}

// ///////////////////////////////////////////////
// The refusal latch
// ///////////////////////////////////////////////

func TestFirstRefusal_SeparatesNeverRefusedFromRefusedForBroadcastZero(t *testing.T) {
	// Recording the refusal as a bare broadcast id makes the map's zero
	// value indistinguishable from a real one, and the report for that
	// broadcast is swallowed.
	h := newHarness(t, nil)
	h.daemon.sessionStart(h.channel.ID, "stream-1", now)

	if !h.daemon.firstRefusal(h.channel.ID, 0) {
		t.Error("firstRefusal() = false on the first refusal, want the report through")
	}
	if h.daemon.firstRefusal(h.channel.ID, 0) {
		t.Error("firstRefusal() = true on the repeat, want it latched")
	}
	if !h.daemon.firstRefusal(h.channel.ID, 1) {
		t.Error("firstRefusal() = false for a different broadcast, want the report through")
	}
}

// ///////////////////////////////////////////////
// Idle space check
// ///////////////////////////////////////////////

// holdBytes records a complete recording of the given size, which is how a
// test puts the library at a chosen point against its cap.
func holdBytes(t *testing.T, h *harness, path string, bytes int64) {
	t.Helper()

	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: path,
		State: store.StateComplete, Origin: store.OriginLive, StartedAt: now.Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	if err := h.store.FinishRecording(recording.ID, store.StateComplete, path,
		bytes, time.Hour, now.Add(-47*time.Hour)); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}
}

// runOneIdleCheck runs exactly one pass of the idle loop's body.
//
// The pass is called directly rather than driven through the loop with a
// cancelled context: the release refuses to start deleting during a
// shutdown, so a done context is exactly the state in which it does
// nothing. Testing it this way needs no clock and no sleep, and the loop's
// own wiring is proved separately by driving Run.
func runOneIdleCheck(h *harness) {
	h.daemon.checkSpace(context.Background(), space.LevelOK, true)
}

func TestIdleSpaceLoop_ReportsAFillingLibraryWithNoCaptureRunning(t *testing.T) {
	// The watermark a capture carries lives only as long as that capture,
	// so nothing else notices a library that fills between broadcasts. The
	// idle check is what reports it before a broadcast is refused.
	tests := []struct {
		name  string
		held  int64
		want  int
		kinds string
	}{
		{name: "room to spare", held: 10, want: 0, kinds: "no report"},
		{name: "inside the warning margin", held: 95, want: 1, kinds: "a warning"},
		{name: "at the limit", held: 99, want: 1, kinds: "a critical report"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(cfg *config.Config) {
				cfg.Space.MaxSize = 100 * config.Gigabyte
			})
			h.free = 900 * config.Gigabyte.Bytes()
			holdBytes(t, h, "held.mkv", tt.held*config.Gigabyte.Bytes())

			runOneIdleCheck(h)

			if got := h.notifier.count(EventLibraryFull); got != tt.want {
				t.Errorf("an idle library holding %d GB of 100 GB reported %d times, want %s",
					tt.held, got, tt.kinds)
			}
			if h.engine.captures != 0 {
				t.Errorf("captures = %d, want 0: the idle check must not need one", h.engine.captures)
			}
		})
	}
}

func TestReportSpaceLevel_ReportsOnlyTransitions(t *testing.T) {
	// A library sitting at its warning level for a week is one fact, not
	// one per tick. An operator who has learned to skip a line that repeats
	// has learned to skip the escalation printed in the same shape.
	tests := []struct {
		name string
		was  space.Level
		now  space.Level
		want int
	}{
		{name: "ok holds", was: space.LevelOK, now: space.LevelOK, want: 0},
		{name: "ok to low reports", was: space.LevelOK, now: space.LevelLow, want: 1},
		{name: "low holds", was: space.LevelLow, now: space.LevelLow, want: 0},
		{name: "low to critical escalates", was: space.LevelLow, now: space.LevelCritical, want: 1},
		{name: "critical holds", was: space.LevelCritical, now: space.LevelCritical, want: 0},
		{name: "critical back to low reports", was: space.LevelCritical, now: space.LevelLow, want: 1},
		// Recovery is logged, never notified. Nothing is at stake in it,
		// and the operator who made the room is the one who would be told.
		{name: "low to ok notifies nobody", was: space.LevelLow, now: space.LevelOK, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)

			got := h.daemon.reportSpaceLevel(context.Background(), tt.was, tt.now, space.Usage{})

			if got != tt.now {
				t.Errorf("reportSpaceLevel() = %q, want %q in force", got, tt.now)
			}
			if n := h.notifier.count(EventLibraryFull); n != tt.want {
				t.Errorf("%s to %s reported %d times, want %d", tt.was, tt.now, n, tt.want)
			}
		})
	}
}

func TestRun_ReportsAFullLibraryWithNoChannelsEnabled(t *testing.T) {
	// Nothing polls and nothing captures, so the per-capture watermark can
	// never run. The only thing left that can notice a library over its cap
	// is the idle check, so this is what holds Run to starting it.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Channels = nil
		cfg.Space.MaxSize = 100 * config.Gigabyte
	})
	h.free = 900 * config.Gigabyte.Bytes()
	holdBytes(t, h, "held.mkv", 99*config.Gigabyte.Bytes())

	// The wait fails the test itself, naming which way it went wrong.
	h.notifier.waitFor(t, h.daemon, EventLibraryFull)
}

// ///////////////////////////////////////////////
// Trash release
// ///////////////////////////////////////////////

// purged records a recording already in the trash, with a purge time far
// enough back to be outside any grace this test sets.
func purged(t *testing.T, h *harness, path string, bytes int64) store.Recording {
	t.Helper()

	holdBytes(t, h, path, bytes)
	recordings, err := h.store.RecordingsByState(store.StateComplete)
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}
	recording := recordings[len(recordings)-1]
	if err := h.store.SetState(recording.ID, store.StateTrashed); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}

	// These tests set a zero grace, which puts the cutoff at the instant
	// the release runs. A row purged in the same instant is not yet
	// strictly older than that, so without this the fixture races the
	// assertion. In production the grace is a week and this cannot arise.
	// Waiting for the clock to pass the row keeps the test about the
	// policy rather than about how long a query took.
	trashed, err := h.store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	for !time.Now().After(trashed.UpdatedAt) {
		runtime.Gosched()
	}
	return trashed
}

func TestReleaseTrash_OnlyRunsUnderPressure(t *testing.T) {
	// The undo window costs headroom for as long as it stays open, so it
	// is held while the library can afford it and spent when it cannot.
	// A timer would close it on a library with room to spare.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 100 * config.Gigabyte
		cfg.Space.Purge.TrashGrace = 0
	})
	h.free = 900 * config.Gigabyte.Bytes()
	purged(t, h, "trash/1-old.mkv", 10*config.Gigabyte.Bytes())

	runOneIdleCheck(h)

	if len(h.finalizer.released) != 0 {
		t.Errorf("released %v with the library at 10%% of its cap, want the undo window kept",
			h.finalizer.released)
	}
}

func TestReleaseTrash_FreesRoomWhenTheLibraryIsFull(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 100 * config.Gigabyte
		cfg.Space.Purge.TrashGrace = 0
	})
	h.free = 900 * config.Gigabyte.Bytes()
	holdBytes(t, h, "ExampleChannel/2026/kept.mkv", 60*config.Gigabyte.Bytes())
	purged(t, h, "trash/2-old.mkv", 39*config.Gigabyte.Bytes())

	runOneIdleCheck(h)

	if len(h.finalizer.released) != 1 {
		t.Errorf("released %v, want the expired purge released to clear the level", h.finalizer.released)
	}
}

func TestReleaseTrash_StopsOnceTheLevelClears(t *testing.T) {
	// An operator who purged a month of broadcasts must not lose the undo
	// window on all of them to reclaim one broadcast's worth of space.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 100 * config.Gigabyte
		cfg.Space.Purge.TrashGrace = 0
	})
	h.free = 900 * config.Gigabyte.Bytes()
	holdBytes(t, h, "ExampleChannel/2026/kept.mkv", 60*config.Gigabyte.Bytes())
	for _, path := range []string{"trash/2-a.mkv", "trash/3-b.mkv", "trash/4-c.mkv"} {
		purged(t, h, path, 13*config.Gigabyte.Bytes())
	}

	runOneIdleCheck(h)

	if len(h.finalizer.released) != 1 {
		t.Errorf("released %v, want it to stop as soon as the library had room", h.finalizer.released)
	}
}

func TestReleaseTrash_KeepsARecordingInsideItsGraceWhileTheresRoom(t *testing.T) {
	// The grace is the undo window the operator configured, and a library
	// that is merely running low still has room to record into. Nothing is
	// gained by closing the window early here.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 100 * config.Gigabyte
		cfg.Space.Purge.TrashGrace = config.Duration(7 * 24 * time.Hour)
	})
	h.free = 900 * config.Gigabyte.Bytes()
	purged(t, h, "trash/1-fresh.mkv", 95*config.Gigabyte.Bytes())

	runOneIdleCheck(h)

	if len(h.finalizer.released) != 0 {
		t.Errorf("released %v with the library only running low, want the undo window honoured",
			h.finalizer.released)
	}
}

func TestReleaseTrash_IgnoresTheGraceWhenTheLibraryIsCritical(t *testing.T) {
	// The purge pane is the one lever the design offers against a full
	// library. Holding the grace at the limit means the operator selects
	// five broadcasts, confirms, gains nothing, and every broadcast for the
	// next week is refused or cut at the watermark.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 100 * config.Gigabyte
		cfg.Space.Purge.TrashGrace = config.Duration(7 * 24 * time.Hour)
	})
	h.free = 900 * config.Gigabyte.Bytes()
	purged(t, h, "trash/1-fresh.mkv", 99*config.Gigabyte.Bytes())

	runOneIdleCheck(h)

	if len(h.finalizer.released) != 1 {
		t.Errorf("released %v at the limit, want the purge the operator just made to actually free space",
			h.finalizer.released)
	}
}

func TestReleaseTrash_NeverTouchesARecordingNobodyPurged(t *testing.T) {
	// The hard rule: nothing may auto-delete a recording the operator did
	// not condemn. The library is over its cap and every recording in it
	// is live, so the only safe outcome is that nothing is released.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 100 * config.Gigabyte
		cfg.Space.Purge.TrashGrace = 0
	})
	h.free = 900 * config.Gigabyte.Bytes()
	holdBytes(t, h, "ExampleChannel/2026/a.mkv", 99*config.Gigabyte.Bytes())

	runOneIdleCheck(h)

	if len(h.finalizer.released) != 0 {
		t.Errorf("released %v, want nothing: no recording here was ever purged", h.finalizer.released)
	}
}

func TestReleaseTrash_CarriesOnPastOneThatWillNotDelete(t *testing.T) {
	// A recording the organizer is busy with comes round on the next tick,
	// and one that will not delete is a fact rather than a reason to leave
	// the library full.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 100 * config.Gigabyte
		cfg.Space.Purge.TrashGrace = 0
	})
	h.free = 900 * config.Gigabyte.Bytes()
	holdBytes(t, h, "ExampleChannel/2026/kept.mkv", 60*config.Gigabyte.Bytes())
	purged(t, h, "trash/2-a.mkv", 20*config.Gigabyte.Bytes())
	purged(t, h, "trash/3-b.mkv", 19*config.Gigabyte.Bytes())
	h.finalizer.releaseErr = errors.New("another program holds the file")

	runOneIdleCheck(h)

	if len(h.finalizer.released) != 2 {
		t.Errorf("attempted %v, want both tried despite the first failing", h.finalizer.released)
	}
}

// ///////////////////////////////////////////////
// The heartbeat
// ///////////////////////////////////////////////

func TestHeartbeatLoop_OpensANewSessionAfterAFreeze(t *testing.T) {
	// One heartbeat column means an interruption inside a session leaves no
	// trace: only the newest beat survives. A desktop that sleeps overnight
	// resumes, beats into the same row, and every slept day reads as "the
	// recorder was running and nothing aired", which is the one reassurance
	// the calendar must never print falsely.
	h := newHarness(t, nil)
	h.daemon.heartbeatInterval = time.Minute

	session, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	h.daemon.setSession(session.ID)

	// A beat that lands on time, so the row has an honest last beat to be
	// closed at.
	ctx := context.Background()
	beat := h.daemon.beat(ctx, now)

	// An hour passes between two beats, which is what a lid closing looks
	// like from inside the process.
	frozen := now.Add(time.Hour)
	h.at(frozen)
	h.daemon.beat(ctx, beat)

	sessions, err := h.store.SessionsBetween(now.Add(-time.Hour), frozen.Add(time.Hour))
	if err != nil {
		t.Fatalf("SessionsBetween() err = %v, want nil", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2: the frozen stretch belongs to neither", len(sessions))
	}
	if sessions[0].StoppedAt == nil {
		t.Error("the first session has no stop time, want it closed at its last honest beat")
	}
	if sessions[0].HeartbeatAt.After(now.Add(time.Minute)) {
		t.Errorf("the first session's heartbeat is %v, want it left at the last beat before the freeze",
			sessions[0].HeartbeatAt)
	}
	if sessions[1].StartedAt.Before(frozen) {
		t.Errorf("the second session starts at %v, want it after the freeze at %v",
			sessions[1].StartedAt, frozen)
	}
	if got := h.daemon.currentSession(); got != sessions[1].ID {
		t.Errorf("currentSession() = %d, want the new row %d", got, sessions[1].ID)
	}
}

func TestHeartbeatLoop_KeepsOneSessionWhileTheBeatsLandOnTime(t *testing.T) {
	// The companion that stops the freeze rule splitting a healthy run into
	// a session per tick.
	h := newHarness(t, nil)
	h.daemon.heartbeatInterval = time.Minute

	session, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	h.daemon.setSession(session.ID)

	// Beats one interval apart, which is every healthy run. The freeze rule
	// allows three intervals, so none of these may split the session.
	ctx := context.Background()
	beat := now
	for tick := 1; tick <= 5; tick++ {
		h.at(now.Add(time.Duration(tick) * time.Minute))
		beat = h.daemon.beat(ctx, beat)
	}

	sessions, err := h.store.SessionsBetween(now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SessionsBetween() err = %v, want nil", err)
	}
	if len(sessions) != 1 {
		t.Errorf("sessions = %d over an unbroken run, want 1", len(sessions))
	}
}

func TestHeartbeatLoop_TicksUntilTheContextEnds(t *testing.T) {
	// The loop around the decision is only a ticker, and this is the one
	// thing it owns: it beats while the context is alive and returns when it
	// is not, rather than running on or exiting early.
	h := newHarness(t, nil)
	h.daemon.heartbeatInterval = time.Millisecond

	session, _, err := h.daemon.StartSession(context.Background())
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	h.daemon.setSession(session.ID)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.heartbeatLoop(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(eventWaitBudget):
		t.Fatal("heartbeatLoop did not return after its context was cancelled")
	}
}

// ///////////////////////////////////////////////
// The incoming sweep
// ///////////////////////////////////////////////

// incomingFile writes a file into the incoming directory and dates it
// against the harness clock, which is what the sweep measures age with.
func incomingFile(t *testing.T, h *harness, name string, age time.Duration) string {
	t.Helper()

	relPath := filepath.Join(paths.IncomingDirName, name)
	full := filepath.Join(h.root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating the incoming directory: %v", err)
	}
	if err := os.WriteFile(full, []byte(strings.Repeat("x", 64)), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	dated := now.Add(-age)
	if err := os.Chtimes(full, dated, dated); err != nil {
		t.Fatalf("dating %s: %v", name, err)
	}
	return relPath
}

func TestSweepIncoming_RemovesAFileNoRecordingNames(t *testing.T) {
	// A download that goes terminal leaves its partial file behind with no
	// row naming it. The size cap cannot see those bytes and nothing else
	// deletes them, so the library overruns its cap while reporting room.
	tests := []struct {
		name       string
		file       string
		age        time.Duration
		claim      store.State
		wantRemove bool
		why        string
	}{
		{
			name:       "an abandoned partial download",
			file:       "twitch-examplechannel-1772658900.mp4.part",
			age:        48 * time.Hour,
			wantRemove: true,
			why:        "no row names it and no download runs for two days",
		},
		{
			name:  "a file a recording row names",
			file:  "twitch-examplechannel-1772662500.ts",
			age:   48 * time.Hour,
			claim: store.StateAwaitingFinalize,
			why:   "the organizer still has work to do on it",
		},
		{
			name: "a download that may still be running",
			file: "twitch-examplechannel-1772666100.mp4.part",
			age:  time.Hour,
			why:  "a multi-hour broadcast takes a while to fetch",
		},
		{
			name:  "the capture in flight",
			file:  "twitch-examplechannel-1772669700.ts",
			age:   48 * time.Hour,
			claim: store.StateCapturing,
			why:   "a broadcast recording right now is the one thing that cannot be got again",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			relPath := incomingFile(t, h, tt.file, tt.age)
			if tt.claim != "" {
				recording, err := h.store.CreateRecording(store.Recording{
					ChannelID: h.channel.ID, Path: filepath.ToSlash(relPath),
					State: store.StateCapturing, Origin: store.OriginLive, StartedAt: now.Add(-tt.age),
				})
				if err != nil {
					t.Fatalf("CreateRecording() err = %v, want nil", err)
				}
				if err := h.store.SetState(recording.ID, tt.claim); err != nil {
					t.Fatalf("SetState() err = %v, want nil", err)
				}
			}

			removed, err := h.daemon.SweepIncoming(context.Background())
			if err != nil {
				t.Fatalf("SweepIncoming() err = %v, want nil", err)
			}

			_, statErr := os.Stat(filepath.Join(h.root, relPath))
			gone := errors.Is(statErr, os.ErrNotExist)
			if gone != tt.wantRemove {
				t.Errorf("file removed = %t, want %t: %s", gone, tt.wantRemove, tt.why)
			}
			if want := map[bool]int{true: 1, false: 0}[tt.wantRemove]; removed != want {
				t.Errorf("SweepIncoming() = %d, want %d", removed, want)
			}
		})
	}
}

func TestSweepIncoming_LeavesTheStateDirectoryAlone(t *testing.T) {
	// The sweep reads one directory. A rule that reached further would put
	// the database and the ownership marker inside a delete.
	h := newHarness(t, nil)
	incomingFile(t, h, "twitch-examplechannel-1772658900.ts.part", 48*time.Hour)

	// A library file nothing in the incoming directory names, aged past the
	// threshold so only the directory rule keeps it.
	kept := filepath.Join(h.root, "ExampleChannel", "2026", "kept.mkv")
	if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
		t.Fatalf("creating the library directory: %v", err)
	}
	if err := os.WriteFile(kept, []byte("recording"), 0o644); err != nil {
		t.Fatalf("writing the library recording: %v", err)
	}
	if err := os.Chtimes(kept, now.Add(-720*time.Hour), now.Add(-720*time.Hour)); err != nil {
		t.Fatalf("dating the library recording: %v", err)
	}

	if _, err := h.daemon.SweepIncoming(context.Background()); err != nil {
		t.Fatalf("SweepIncoming() err = %v, want nil", err)
	}

	if _, err := os.Stat(paths.MarkerPath(h.root)); err != nil {
		t.Errorf("the ownership marker is gone after a sweep of the incoming directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, "ExampleChannel", "2026", "kept.mkv")); err != nil {
		t.Errorf("a library recording is gone after a sweep of the incoming directory: %v", err)
	}
}

// ///////////////////////////////////////////////
// The missing-file sweep
// ///////////////////////////////////////////////

func TestSweepMissing_StopsCountingAFileThatIsGone(t *testing.T) {
	// The operator deletes a year of recordings in Explorer to make room.
	// Nothing reconciles the rows, so the library still reads 1.9TB against
	// a 2TB cap and every broadcast is refused with "max_size would be
	// breached" while the disk shows a terabyte free.
	h := newHarness(t, nil)

	kept := filepath.Join("ExampleChannel", "2026", "kept.mkv")
	gone := filepath.Join("ExampleChannel", "2026", "gone.mkv")
	for _, relPath := range []string{kept, gone} {
		full := filepath.Join(h.root, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating the library directory: %v", err)
		}
		if err := os.WriteFile(full, []byte("recording"), 0o644); err != nil {
			t.Fatalf("writing the recording: %v", err)
		}
		holdBytes(t, h, filepath.ToSlash(relPath), 10*config.Gigabyte.Bytes())
	}
	if err := os.Remove(filepath.Join(h.root, gone)); err != nil {
		t.Fatalf("removing the recording behind the operator's back: %v", err)
	}

	before, err := h.store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes() err = %v, want nil", err)
	}
	if before != 20*config.Gigabyte.Bytes() {
		t.Fatalf("TotalBytes() = %d before the sweep, want both rows counted", before)
	}

	swept, err := h.daemon.SweepMissing(context.Background())
	if err != nil {
		t.Fatalf("SweepMissing() err = %v, want nil", err)
	}
	if swept != 1 {
		t.Errorf("SweepMissing() = %d, want 1", swept)
	}

	after, err := h.store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes() err = %v, want nil", err)
	}
	if after != 10*config.Gigabyte.Bytes() {
		t.Errorf("TotalBytes() = %d, want only the file that is still there counted", after)
	}
}

func TestSweepMissing_StopsTheBroadcastBeingFetchedAgain(t *testing.T) {
	// A file the operator deleted by hand must not come back as a muted
	// archive copy on the next recovery pass, for the same reason a purged
	// one must not.
	h := newHarness(t, nil)

	broadcast, err := h.store.UpsertBroadcast(store.Broadcast{
		ChannelID: h.channel.ID, StreamID: "stream-1",
		StartedAt: now.Add(-48 * time.Hour), Source: store.SourceLive,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	holdBytes(t, h, "ExampleChannel/2026/gone.mkv", 10*config.Gigabyte.Bytes())
	recordings, err := h.store.RecordingsByState(store.StateComplete)
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}
	if err := h.store.SetBroadcast(recordings[0].ID, &broadcast.ID); err != nil {
		t.Fatalf("SetBroadcast() err = %v, want nil", err)
	}

	if _, err := h.daemon.SweepMissing(context.Background()); err != nil {
		t.Fatalf("SweepMissing() err = %v, want nil", err)
	}

	fetch, err := h.store.FetchFor(broadcast.ID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want the sweep to have refused the broadcast", err)
	}
	if fetch.State != store.FetchTerminal {
		t.Errorf("FetchState = %q, want %q", fetch.State, store.FetchTerminal)
	}
}

func TestSweepMissing_LeavesEverythingAloneWhenTheLibraryIsUnreadable(t *testing.T) {
	// The case that would otherwise destroy a library. An unmounted network
	// share or an unplugged volume reads as a root where every file is
	// missing, and a sweep that trusted that would blank the whole library
	// in one pass.
	h := newHarness(t, nil)
	holdBytes(t, h, "ExampleChannel/2026/a.mkv", 10*config.Gigabyte.Bytes())
	holdBytes(t, h, "ExampleChannel/2026/b.mkv", 10*config.Gigabyte.Bytes())

	if err := os.Remove(paths.MarkerPath(h.root)); err != nil {
		t.Fatalf("removing the ownership marker: %v", err)
	}

	swept, err := h.daemon.SweepMissing(context.Background())
	if err == nil {
		t.Error("SweepMissing() err = nil with the library unreadable, want it refused")
	}
	if swept != 0 {
		t.Errorf("SweepMissing() = %d, want 0: nothing may be concluded from an unreadable library", swept)
	}

	total, err := h.store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes() err = %v, want nil", err)
	}
	if total != 20*config.Gigabyte.Bytes() {
		t.Errorf("TotalBytes() = %d, want both recordings still counted", total)
	}
}

func TestSweepCapturing_RecoversAStrandedRow(t *testing.T) {
	// The start-up reconcile runs once, before any watcher. A failure at the
	// moment a capture finishes leaves the row capturing with a complete
	// playable file in incoming and nothing left to move it: no sweep looks
	// at that state, and the day it sits on paints at risk so recovery
	// deliberately leaves it alone too.
	h := newHarness(t, nil)
	recording := h.interrupted(
		filepath.Join(paths.IncomingDirName, "twitch-examplechannel-1772658900.ts"), "captured bytes")

	recovered, err := h.daemon.SweepCapturing(context.Background())
	if err != nil {
		t.Fatalf("SweepCapturing() err = %v, want nil", err)
	}
	if recovered != 1 {
		t.Fatalf("SweepCapturing() = %d, want 1", recovered)
	}

	stored, err := h.store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if stored.State != store.StateAwaitingFinalize {
		t.Errorf("State = %q, want %q so the parked sweep finishes it",
			stored.State, store.StateAwaitingFinalize)
	}
}

func TestSweepCapturing_LeavesACaptureThisProcessIsRunning(t *testing.T) {
	// The companion that matters more than the sweep itself. Finalizing a
	// recording an engine is still writing to remuxes a file that is still
	// growing, against the one copy that cannot be got again.
	h := newHarness(t, nil)
	recording := h.interrupted(
		filepath.Join(paths.IncomingDirName, "twitch-examplechannel-1772658900.ts"), "captured bytes")
	h.daemon.noteCapturing(recording.ID)

	recovered, err := h.daemon.SweepCapturing(context.Background())
	if err != nil {
		t.Fatalf("SweepCapturing() err = %v, want nil", err)
	}
	if recovered != 0 {
		t.Fatalf("SweepCapturing() = %d, want 0", recovered)
	}

	stored, err := h.store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if stored.State != store.StateCapturing {
		t.Errorf("State = %q, want %q: the engine is still writing to it",
			stored.State, store.StateCapturing)
	}
}

// ///////////////////////////////////////////////
// checkCredential
// ///////////////////////////////////////////////

func TestCheckCredential_ReportsARejectedCredential(t *testing.T) {
	// The silent degradation this whole path exists to end. Nothing already
	// recorded is lost, but everything recorded afterwards is worse, and
	// without this nothing says so.
	h := newHarness(t, func(c *config.Config) { c.Notify.OnFailure = true })
	h.daemon.credential = func(context.Context) error { return ErrCredentialRejected }

	h.daemon.checkCredential(context.Background())

	if !h.notifier.has(EventCredentialDead) {
		t.Errorf("events = %v, want a credential notification", h.notifier.kinds())
	}
}

func TestCredentialLoop_KeepsReportingADeadCredential(t *testing.T) {
	// A rejection deletes the derived file, so every later check finds an
	// absence and says nothing. The one delivery that did happen is
	// best-effort: on Windows the recorder cannot raise a notification
	// itself, and the log copy ages out at 28 days. Every other persistent
	// condition here latches, and this is the only one that degrades every
	// recording made after it.
	h := newHarness(t, func(c *config.Config) { c.Notify.OnFailure = true })

	rejected := true
	h.daemon.credential = func(context.Context) error {
		if rejected {
			rejected = false
			return ErrCredentialRejected
		}
		// What the same dead credential looks like from then on: the
		// rejection removed the file it was reading.
		return ErrCredentialAbsent
	}

	h.daemon.checkCredential(context.Background())
	if got := h.notifier.count(EventCredentialDead); got != 1 {
		t.Fatalf("notifications after the rejection = %d, want 1", got)
	}

	// An hour later, which is the loop's own cadence.
	h.at(now.Add(time.Hour))
	h.daemon.checkCredential(context.Background())
	if got := h.notifier.count(EventCredentialDead); got != 1 {
		t.Errorf("notifications after an hour = %d, want 1: hourly would be a flood", got)
	}

	h.at(now.Add(50 * time.Hour))
	h.daemon.checkCredential(context.Background())
	if got := h.notifier.count(EventCredentialDead); got != 2 {
		t.Errorf("notifications after two days = %d, want 2: the condition never resolves on its own", got)
	}
}

func TestCredentialLoop_StopsReportingOnceACredentialIsStoredAgain(t *testing.T) {
	// The companion. An operator who fixed it must not keep hearing about
	// it, or the report means nothing the next time.
	h := newHarness(t, func(c *config.Config) { c.Notify.OnFailure = true })

	answer := ErrCredentialRejected
	h.daemon.credential = func(context.Context) error { return answer }

	h.daemon.checkCredential(context.Background())
	if got := h.notifier.count(EventCredentialDead); got != 1 {
		t.Fatalf("notifications after the rejection = %d, want 1", got)
	}

	// The operator stores a working token, which validates.
	answer = nil
	h.at(now.Add(time.Hour))
	h.daemon.checkCredential(context.Background())

	// Then it is removed, which is an ordinary logout rather than a
	// rejection, and must not restart the reports.
	answer = ErrCredentialAbsent
	h.at(now.Add(100 * time.Hour))
	h.daemon.checkCredential(context.Background())

	if got := h.notifier.count(EventCredentialDead); got != 1 {
		t.Errorf("notifications = %d, want 1: the condition was resolved", got)
	}
}

func TestCheckCredential_SaysNothingOnAFreshInstall(t *testing.T) {
	// No credential stored is where every install starts. Reporting it
	// daily would train the operator to ignore the one report that matters.
	h := newHarness(t, func(c *config.Config) { c.Notify.OnFailure = true })
	h.daemon.credential = func(context.Context) error { return ErrCredentialAbsent }

	h.daemon.checkCredential(context.Background())
	h.at(now.Add(100 * time.Hour))
	h.daemon.checkCredential(context.Background())

	if len(h.notifier.kinds()) != 0 {
		t.Errorf("events = %v, want none: nothing was ever stored to go bad", h.notifier.kinds())
	}
}

func TestCheckCredential_SaysNothingWhenTheQuestionCouldNotBePut(t *testing.T) {
	// A network failure is not a dead token. Treating it as one would have
	// a provider outage notify every operator that their credential died,
	// and, worse, delete a credential that still works.
	h := newHarness(t, func(c *config.Config) { c.Notify.OnFailure = true })
	h.daemon.credential = func(context.Context) error {
		return errors.New("dial tcp: connection refused")
	}

	h.daemon.checkCredential(context.Background())

	if h.notifier.has(EventCredentialDead) {
		t.Errorf("a network failure was reported as a dead credential: %v", h.notifier.kinds())
	}
}

func TestCheckCredential_SaysNothingWhenTheCredentialIsFine(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.Notify.OnFailure = true })
	h.daemon.credential = func(context.Context) error { return nil }

	h.daemon.checkCredential(context.Background())

	if len(h.notifier.kinds()) != 0 {
		t.Errorf("events = %v, want none for a working credential", h.notifier.kinds())
	}
}

func TestCheckCredential_NeverCancelsACaptureInFlight(t *testing.T) {
	// THE RULE. streamlink already holds the playback token for a running
	// session, and a recording in progress is worth more than credential
	// hygiene. A check that cancelled one would destroy a broadcast to
	// protect a credential that is already spent.
	h := newHarness(t, func(c *config.Config) { c.Notify.OnFailure = true })
	h.daemon.credential = func(context.Context) error { return ErrCredentialRejected }

	capturing, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID,
		Path:      "incoming/twitch-examplechannel-1772658900.ts",
		State:     store.StateCapturing,
		Origin:    store.OriginLive,
		StartedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}

	h.daemon.checkCredential(context.Background())

	after, err := h.store.Recording(capturing.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if after.State != store.StateCapturing {
		t.Errorf("the capture moved to %q, want it left alone at %q",
			after.State, store.StateCapturing)
	}
}

func TestCredentialLoop_DoesNothingWithNoCredentialPathWired(t *testing.T) {
	// A nil credential check must not spin a ticker for the life of the
	// process to decide that on every tick.
	h := newHarness(t, nil)
	h.daemon.credential = nil

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.daemon.credentialLoop(ctx)
}

func TestRecheckCredential_AsksAtOnceRatherThanWaitingForTheTick(t *testing.T) {
	// A refused credential yields no streams at all, so every poll until
	// the hourly tick records nothing. This is what shortens that to a
	// single poll interval.
	h := newHarness(t, func(c *config.Config) { c.Notify.OnFailure = true })
	checks := 0
	h.daemon.credential = func(context.Context) error {
		checks++
		return ErrCredentialRejected
	}

	h.daemon.recheckCredential(context.Background())

	if checks != 1 {
		t.Errorf("ran %d checks, want 1", checks)
	}
	if !h.notifier.has(EventCredentialDead) {
		t.Errorf("events = %v, want a credential notification", h.notifier.kinds())
	}
}

func TestRecheckCredential_LetsOneChannelAskForAllOfThem(t *testing.T) {
	// A dead token fails every channel at once. Without a floor a
	// twenty-channel config would send twenty validation requests inside a
	// second, and the answer is the same every time.
	h := newHarness(t, nil)
	checks := 0
	h.daemon.credential = func(context.Context) error {
		checks++
		return ErrCredentialRejected
	}

	for range 20 {
		h.daemon.recheckCredential(context.Background())
	}

	if checks != 1 {
		t.Errorf("ran %d checks for one dead token, want 1", checks)
	}
}

func TestRecheckCredential_AsksAgainOnceTheFloorHasPassed(t *testing.T) {
	// The floor must not silence the check forever. A token replaced and
	// then dying again is a real sequence.
	h := newHarness(t, nil)
	checks := 0
	h.daemon.credential = func(context.Context) error {
		checks++
		return ErrCredentialRejected
	}

	clock := now
	h.daemon.now = func() time.Time { return clock }

	h.daemon.recheckCredential(context.Background())
	clock = clock.Add(credentialFloor + time.Second)
	h.daemon.recheckCredential(context.Background())

	if checks != 2 {
		t.Errorf("ran %d checks either side of the floor, want 2", checks)
	}
}

func TestCheckCredential_CountsAgainstTheFloorForEveryCaller(t *testing.T) {
	// The floor is the least time between two checks, whoever asks. A
	// channel failing a second after the hourly tick is asking about the
	// token that tick just checked.
	h := newHarness(t, nil)
	checks := 0
	h.daemon.credential = func(context.Context) error {
		checks++
		return ErrCredentialRejected
	}

	clock := now
	h.daemon.now = func() time.Time { return clock }

	h.daemon.checkCredential(context.Background())
	h.daemon.recheckCredential(context.Background())
	if checks != 1 {
		t.Fatalf("ran %d checks inside the floor, want 1", checks)
	}

	clock = clock.Add(credentialFloor + time.Second)
	h.daemon.recheckCredential(context.Background())
	if checks != 2 {
		t.Errorf("ran %d checks either side of the floor, want 2", checks)
	}
}

func TestRecheckCredential_DoesNothingWithNoCredentialPathWired(t *testing.T) {
	h := newHarness(t, nil)
	h.daemon.credential = nil

	h.daemon.recheckCredential(context.Background())
}
