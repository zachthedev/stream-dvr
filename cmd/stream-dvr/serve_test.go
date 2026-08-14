package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/daemon"
	"zach.tools/go/stream-dvr/internal/fetch"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// buildBroadcastStart
// ///////////////////////////////////////////////

// lockedBuffer collects output a running daemon writes from its own
// goroutine while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// fakeInfo answers with one scripted listing.
type fakeInfo struct {
	listing fetch.Listing
	err     error
}

func (f fakeInfo) Info(context.Context, string) (fetch.Listing, error) {
	return f.listing, f.err
}

func TestBuildBroadcastStart_AnswersOnlyForABroadcastOnAirNow(t *testing.T) {
	// The resolver anchors a broadcast the recorder is recording right now.
	// A finished archive reports a precise timestamp too, so without the
	// liveness gate the newest past broadcast is handed back as this one's
	// start, and the hole filed from it spans everything in between.
	live := time.Date(2026, 3, 4, 20, 35, 0, 0, time.UTC)

	tests := []struct {
		name    string
		listing fetch.Listing
		err     error
		want    bool
	}{
		{
			name:    "a broadcast on air now",
			listing: fetch.Listing{StartedAt: live, IsLive: true, Precise: true},
			want:    true,
		},
		{
			name:    "a finished archive",
			listing: fetch.Listing{StartedAt: live, IsLive: false, Precise: true},
			want:    false,
		},
		{
			name:    "a date rather than a moment",
			listing: fetch.Listing{StartedAt: live, IsLive: true, Precise: false},
			want:    false,
		},
		{
			name:    "no start at all",
			listing: fetch.Listing{IsLive: true, Precise: true},
			want:    false,
		},
		{
			name: "the tool could not answer",
			err:  errors.New("no such channel"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolve := buildBroadcastStart(
				fakeInfo{listing: tt.listing, err: tt.err}, nil,
				slog.New(slog.DiscardHandler))

			got, ok := resolve(context.Background(), "https://twitch.tv/examplechannel", "stream-1")
			if ok != tt.want {
				t.Fatalf("resolved = %v, want %v", ok, tt.want)
			}
			if ok && !got.Equal(live) {
				t.Errorf("start = %s, want %s", got, live)
			}
		})
	}
}

// ///////////////////////////////////////////////
// describeWatch
// ///////////////////////////////////////////////

func TestDescribeWatch(t *testing.T) {
	tests := []struct {
		name     string
		channels []config.Channel
		want     []string
	}{
		{
			name: "one channel",
			channels: []config.Channel{
				{Platform: config.PlatformTwitch, Name: "examplechannel", Enabled: true},
			},
			want: []string{"1 channel", "twitch/examplechannel", "30s"},
		},
		{
			name: "several channels",
			channels: []config.Channel{
				{Platform: config.PlatformTwitch, Name: "examplechannel", Enabled: true},
				{Platform: config.PlatformYouTube, Name: "someone", Enabled: true},
			},
			want: []string{"2 channels", "twitch/examplechannel", "youtube/someone"},
		},
		{
			// A channel kept in config but switched off must not read as
			// something the daemon is watching.
			name: "disabled channels are not counted",
			channels: []config.Channel{
				{Platform: config.PlatformTwitch, Name: "examplechannel", Enabled: true},
				{Platform: config.PlatformTwitch, Name: "idle", Enabled: false},
			},
			want: []string{"1 channel", "twitch/examplechannel"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Channels = tt.channels

			got := describeWatch(cfg)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("describeWatch() = %q, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestDescribeWatch_NoChannelsSaysWhatToDo(t *testing.T) {
	// A daemon with nothing to watch is a configuration mistake, so the line
	// must say how to fix it rather than report an empty list.
	cfg := config.DefaultConfig()
	cfg.Channels = nil

	got := describeWatch(cfg)
	if !strings.Contains(got, "channels") {
		t.Errorf("describeWatch() = %q, want it to point at the channels block", got)
	}
}

func TestDescribeWatch_ReportsThePollInterval(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Capture.PollInterval = config.Duration(90 * time.Second)
	cfg.Channels = []config.Channel{
		{Platform: config.PlatformTwitch, Name: "examplechannel", Enabled: true},
	}

	if got := describeWatch(cfg); !strings.Contains(got, "1m30s") {
		t.Errorf("describeWatch() = %q, want the poll interval", got)
	}
}

// ///////////////////////////////////////////////
// joinNames
// ///////////////////////////////////////////////

func TestJoinNames(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{name: "empty", names: nil, want: ""},
		{name: "one", names: []string{"a"}, want: "a"},
		{name: "several", names: []string{"a", "b", "c"}, want: "a, b, c"},
		{
			// A long watch list would otherwise bury the rest of the line.
			name:  "trimmed past five",
			names: []string{"a", "b", "c", "d", "e", "f", "g"},
			want:  "a, b, c, d, e and 2 more",
		},
		{
			name:  "exactly five is not trimmed",
			names: []string{"a", "b", "c", "d", "e"},
			want:  "a, b, c, d, e",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinNames(tt.names); got != tt.want {
				t.Errorf("joinNames(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
	}
}

func TestJoinComma(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{name: "empty", names: nil, want: ""},
		{name: "one", names: []string{"only"}, want: "only"},
		{name: "two", names: []string{"first", "second"}, want: "first, second"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := joinComma(tt.names); got != tt.want {
				t.Errorf("joinComma(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Backfill outcomes
// ///////////////////////////////////////////////

func TestBuildCredentialCheck_SurvivesAnAbsentCredential(t *testing.T) {
	// An absent credential file is not a permanent state. On Linux and
	// macOS the service starts before the operator has run the auth
	// command, and the daemon itself creates that state whenever it deletes
	// a rejected token. Answering nil turns the whole credential loop off
	// for the life of the process, so a token stored an hour later is never
	// checked again and nothing ever reports one that dies.
	t.Setenv(paths.EnvDataDir, t.TempDir())

	if got := buildCredentialCheck(); got == nil {
		t.Error("buildCredentialCheck() = nil with no credential stored, want a check that can see one appear")
	}
}

func TestBuildCredentialCheck_TellsAnAbsentCredentialFromAWorkingOne(t *testing.T) {
	// A refusal deletes the derived file, so from then on a dead credential
	// looks exactly like a fresh install. The daemon keeps reporting one and
	// says nothing about the other, so the two answers cannot be the same.
	t.Setenv(paths.EnvDataDir, t.TempDir())

	err := buildCredentialCheck()(t.Context())

	if !errors.Is(err, daemon.ErrCredentialAbsent) {
		t.Errorf("credential check err = %v, want it to wrap %v", err, daemon.ErrCredentialAbsent)
	}
	if errors.Is(err, daemon.ErrCredentialRejected) {
		t.Error("an absent credential reported as a rejected one, which would notify a fresh install")
	}
}

// TestReportServeStart_ControlCharactersInARootNeverReachTheTerminal covers
// every line serve prints that carries the library root.
//
// The root reaches two of them directly and the third by way of the log path,
// which StateDir builds from it. Each case asserts on the rendered line, so a
// literal standing in for an escaped value fails here rather than sending a
// control byte to the terminal.
func TestReportServeStart_ControlCharactersInARootNeverReachTheTerminal(t *testing.T) {
	const root = "/library\x1b[2Jroot"

	cases := []struct {
		name    string
		root    string
		watch   string
		logPath string
		want    string
	}{
		{
			name:    "the root on the recording line",
			root:    root,
			watch:   "one channel",
			logPath: "/state/daemon.log",
			want:    "root",
		},
		{
			name:    "a channel name in the watch summary",
			root:    "/library",
			watch:   "watching ch\x1b[2Jannel",
			logPath: "/state/daemon.log",
			want:    "annel",
		},
		{
			name:    "the root reaching the log path through StateDir",
			root:    "/library",
			watch:   "one channel",
			logPath: root + "/.dvr/daemon.log",
			want:    "daemon.log",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			reportServeStart(&out, tt.root, tt.watch, tt.logPath)

			if !strings.Contains(out.String(), tt.want) {
				t.Fatalf("the value did not render at all, so this proved nothing:\n%q", out.String())
			}
			if strings.Contains(out.String(), "\x1b[2J") {
				t.Errorf("an escape sequence reached the rendered output:\n%q", out.String())
			}
		})
	}
}

// ///////////////////////////////////////////////
// Announcing the start
// ///////////////////////////////////////////////

func TestRunServe_SaysNothingWhenTheLibraryIsAlreadyHeld(t *testing.T) {
	// The report used to run before the claim, so every refusal printed a
	// green tick above it and told the operator recording had begun. The
	// claim is the first thing the daemon takes, so nothing may be
	// announced until it holds.
	configPath := backfillConfig(t)
	heldLibrary(t, configPath)

	var out bytes.Buffer
	err := runServe(context.Background(), &out, configPath, false)

	if err == nil {
		t.Fatal("runServe() err = nil against a held library, want a refusal")
	}
	if out.Len() != 0 {
		t.Errorf("runServe() printed %q before failing, want nothing", out.String())
	}
}

// Write implements io.Writer.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what has been written so far.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunServe_AnnouncesOnceItHoldsTheLibrary(t *testing.T) {
	// The other half. A report that never fires would trade a false success
	// for no feedback at all, and the operator would have no way to tell a
	// running recorder from a hung one.
	configPath := backfillConfig(t)

	ctx, cancel := context.WithCancel(context.Background())
	out := &lockedBuffer{}

	done := make(chan error, 1)
	go func() { done <- runServe(ctx, out, configPath, false) }()

	// Cancelled as soon as the claim is visible, so the daemon stops
	// without waiting out a poll interval.
	deadline := time.Now().Add(10 * time.Second)
	for out.String() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(out.String(), "recording to") {
		t.Errorf("runServe() = %q, want it to report the library it claimed", out.String())
	}
}

// ///////////////////////////////////////////////
// Automatic recovery
// ///////////////////////////////////////////////

// memoryStore opens a store with no file behind it, for a test whose whole
// subject is a decision made before anything is written.
func memoryStore(t *testing.T) *store.Store {
	t.Helper()

	db, err := store.OpenMemory()
	if err != nil {
		t.Fatalf("store.OpenMemory() err = %v, want nil", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRecoveryWindow_ReachesBackExactlyAsFarAsItWasAsked(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Backfill.Settle = config.Duration(90 * time.Minute)
	at := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

	window, ok := recoveryWindow(cfg, time.UTC, at.Add(-30*time.Hour), at)

	if !ok {
		t.Fatal("recoveryWindow() reported no window, want one")
	}
	if window.Lookback != 30*time.Hour {
		t.Errorf("Lookback = %s, want %s", window.Lookback, 30*time.Hour)
	}
	if window.Settle != 90*time.Minute {
		t.Errorf("Settle = %s, want the configured settle", window.Settle)
	}
	if window.Location != time.UTC {
		t.Errorf("Location = %v, want the configured zone", window.Location)
	}
}

func TestRecoveryWindow_RefusesAWindowThatEndsBeforeItStarts(t *testing.T) {
	// The planner reads a lookback of zero or less as unset and substitutes
	// a month. Passing one on would turn a clock that stepped backwards
	// into a month of unattended downloads.
	// Each offset is measured from now, so a positive one is a window
	// asked to start after the moment it would end.
	cases := []struct {
		name   string
		offset time.Duration
	}{
		{name: "the clock stepped back", offset: 2 * time.Hour},
		{name: "no width at all", offset: 0},
	}

	cfg := config.DefaultConfig()
	at := time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			window, ok := recoveryWindow(cfg, time.UTC, at.Add(testCase.offset), at)

			if ok {
				t.Errorf("recoveryWindow() reported a window of %s, want none", window.Lookback)
			}
		})
	}
}

func TestBackfillChannels_TakesOnlyTheChannelsThatOptedIn(t *testing.T) {
	// A round downloads hours of video from somebody else's service, so
	// backfill = true is the whole permission for it. An enabled channel
	// without it is recorded live and never fetched.
	cfg := config.DefaultConfig()
	cfg.Channels = []config.Channel{
		{Platform: config.PlatformTwitch, Name: "optedin", Enabled: true, Backfill: true},
		{Platform: config.PlatformTwitch, Name: "recordonly", Enabled: true},
		{Platform: config.PlatformTwitch, Name: "turnedoff", Backfill: true},
	}

	db := memoryStore(t)
	channels, dropped := backfillChannels(cfg, db, slog.New(slog.DiscardHandler))

	if len(channels) != 1 {
		t.Fatalf("channels = %d, want only the one that opted in", len(channels))
	}
	if channels[0].Name != "optedin" {
		t.Errorf("channel = %q, want optedin", channels[0].Name)
	}
	if channels[0].ID == 0 {
		t.Error("channel has no stored row, want one created so a round can plan against it")
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want none: every channel resolved", dropped)
	}
}

func TestBackfillChannels_TakesNothingWhenNobodyOptedIn(t *testing.T) {
	// The ordinary state. A recorder whose channels are all record-only
	// runs its rounds against an empty set and fetches nothing, rather
	// than treating the absence as a reason to fetch everything.
	cfg := config.DefaultConfig()
	cfg.Channels = []config.Channel{
		{Platform: config.PlatformTwitch, Name: "recordonly", Enabled: true},
	}

	db := memoryStore(t)

	channels, dropped := backfillChannels(cfg, db, slog.New(slog.DiscardHandler))
	if len(channels) != 0 {
		t.Errorf("channels = %d, want none", len(channels))
	}
	// None dropped, so a round reads this as nothing to do rather than as a
	// failure worth asking again about.
	if dropped != 0 {
		t.Errorf("dropped = %d, want none: nobody opted in", dropped)
	}
}

func TestBuildLiveMetadata_AnswersFalseWithNoMetadataClient(t *testing.T) {
	// The ordinary state of an install that has not authorized the metadata
	// API. It has to answer "nobody could say" rather than an empty title,
	// because the caller keeps whatever the probe carried on a false and
	// would overwrite it with nothing on a true.
	lookup := buildLiveMetadata(nil, slog.New(slog.DiscardHandler))

	live, ok := lookup(context.Background(), "https://twitch.tv/examplechannel")

	if ok {
		t.Errorf("lookup() ok = true with no client, want false")
	}
	if live.Title != "" || live.Category != "" {
		t.Errorf("lookup() = %q, %q; want both empty", live.Title, live.Category)
	}
}

func TestAutomaticRecovery_HandsBackNothingWhenTheOperatorTurnedItOff(t *testing.T) {
	// The daemon reads a nil hook as "do not start rounds", so the switch
	// is enforced by there being nothing to call rather than by a flag
	// every path has to remember to check.
	tests := []struct {
		name      string
		automatic bool
		wantNil   bool
	}{
		{name: "off", wantNil: true},
		{name: "on", automatic: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Backfill.Automatic = tt.automatic

			recover := automaticRecovery(cfg, nil, nil, slog.New(slog.DiscardHandler),
				nil, nil, nil, time.UTC)

			if (recover == nil) != tt.wantNil {
				t.Errorf("automaticRecovery() nil = %v, want %v", recover == nil, tt.wantNil)
			}
		})
	}
}
