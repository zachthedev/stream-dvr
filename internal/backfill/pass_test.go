package backfill

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// countingDiscovery records which channels were listed, and can refuse.
type countingDiscovery struct {
	mu      sync.Mutex
	seen    []string
	err     error
	written int
	// horizon records how far back the pass asked discovery to reach.
	horizon time.Time
}

// recordingFetcher records what it was asked to fetch, and can refuse.
type recordingFetcher struct {
	mu      sync.Mutex
	fetched []int64
	err     error
	// inFlight tracks how many fetches overlap, so a concurrency bound is
	// observed rather than assumed.
	inFlight atomic.Int32
	peak     atomic.Int32
	// hold blocks each fetch until closed, so overlap is reachable at all.
	hold chan struct{}
}

// recordingPatcher records which broadcasts a pass offered it to patch.
type recordingPatcher struct {
	patched []int64
	err     error
}

// Discover implements Discovery.
func (d *countingDiscovery) Discover(_ context.Context, channel Channel, _, since time.Time) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.seen = append(d.seen, channel.Name)
	d.horizon = since
	return d.written, d.err
}

// Fetch implements Fetching.
func (f *recordingFetcher) Fetch(_ context.Context, candidate Candidate, _ Channel, _ time.Time) error {
	current := f.inFlight.Add(1)
	for {
		peak := f.peak.Load()
		if current <= peak || f.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	defer f.inFlight.Add(-1)

	if f.hold != nil {
		<-f.hold
	}

	f.mu.Lock()
	f.fetched = append(f.fetched, candidate.Broadcast.ID)
	f.mu.Unlock()
	return f.err
}

// fetchedIDs returns what the fetcher was asked for.
func (f *recordingFetcher) fetchedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]int64(nil), f.fetched...)
}

// Patch implements Patching.
func (p *recordingPatcher) Patch(_ context.Context, broadcast store.Broadcast,
	_ Channel, _ time.Time,
) (int, error) {
	p.patched = append(p.patched, broadcast.ID)
	return 0, p.err
}

// passChannels names one channel per stored id, so a case can tell which
// channel a pass acted on.
func passChannels(names ...string) []Channel {
	channels := make([]Channel, 0, len(names))
	for i, name := range names {
		channels = append(channels, Channel{
			ID:     int64(i + 1),
			Name:   name,
			Source: "twitch",
			URL:    "https://twitch.tv/" + name + "/videos",
		})
	}
	return channels
}

// recoverable is a coverage set with one broadcast worth fetching.
func recoverable() *fakeCoverage {
	return oneDay(store.CoverageMissed, nil)
}

// passDeps builds deps around one coverage set.
func passDeps(coverage Coverage, discovery Discovery, fetcher Fetching) PassDeps {
	return PassDeps{
		Coverage: coverage,
		Discover: discovery,
		Fetch:    fetcher,
		Window:   Window{Lookback: 30 * 24 * time.Hour, Location: time.UTC},
	}
}

// ///////////////////////////////////////////////
// One channel's failure never stops the others
// ///////////////////////////////////////////////

func TestPass_ListsEveryChannelWhenOneRefuses(t *testing.T) {
	// A listing is a request against somebody else's service, so a platform
	// that is down or rate limiting takes every channel on it. Stopping at
	// the first would leave the rest unfetched for as long as that lasts,
	// which is the state backfill exists to end.
	discovery := &countingDiscovery{err: errors.New("the platform refused the listing")}
	fetcher := &recordingFetcher{}

	_, err := Pass(context.Background(), passDeps(recoverable(), discovery, fetcher),
		passChannels("first", "second", "third"), afterSettling())
	if err != nil {
		t.Fatalf("Pass() err = %v, want nil", err)
	}

	if got, want := len(discovery.seen), 3; got != want {
		t.Errorf("Pass listed %d channels, want %d: %v", got, want, discovery.seen)
	}
}

func TestPass_FetchesFromWhatIsStoredWhenAListingFails(t *testing.T) {
	// The broadcasts the recorder already saw are in the database, and the
	// gaps they leave are fetchable without learning about new ones. Skipping
	// the fetch would make a platform's outage cost the recovery of every
	// broadcast it already knew about.
	discovery := &countingDiscovery{err: errors.New("the platform refused the listing")}
	fetcher := &recordingFetcher{}

	if _, err := Pass(context.Background(), passDeps(recoverable(), discovery, fetcher),
		passChannels("examplechannel"), afterSettling()); err != nil {
		t.Fatalf("Pass() err = %v, want nil", err)
	}

	if len(fetcher.fetchedIDs()) == 0 {
		t.Error("Pass fetched nothing after a failed listing, want the stored gap fetched")
	}
}

func TestPass_KeepsGoingWhenOneFetchFails(t *testing.T) {
	// Same reason as a failed listing, one rung down: a broadcast that will
	// not download must not cost the channels after it.
	fetcher := &recordingFetcher{err: errors.New("yt-dlp exited 1")}

	if _, err := Pass(context.Background(), passDeps(recoverable(), &countingDiscovery{}, fetcher),
		passChannels("first", "second"), afterSettling()); err != nil {
		t.Fatalf("Pass() err = %v, want nil", err)
	}

	if got, want := len(fetcher.fetchedIDs()), 2; got != want {
		t.Errorf("Pass attempted %d fetches, want %d", got, want)
	}
}

// ///////////////////////////////////////////////
// Ordinary outcomes are not failures
// ///////////////////////////////////////////////

func TestPass_TreatsAHeldOrCapturedBroadcastAsOrdinary(t *testing.T) {
	// Both mean another actor got there first, which is the design working
	// rather than a fault. A pass that reported them would cry wolf on every
	// tick against a library that is already up to date.
	tests := []struct {
		name string
		err  error
	}{
		{name: "another fetcher holds it", err: ErrNotClaimed},
		{name: "a live capture already has it", err: ErrAlreadyCaptured},
		{name: "the platform has not published it yet", err: ErrNoAddress},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Pass(context.Background(),
				passDeps(recoverable(), &countingDiscovery{}, &recordingFetcher{err: tt.err}),
				passChannels("examplechannel"), afterSettling())
			if err != nil {
				t.Errorf("Pass() err = %v, want nil for %s", err, tt.name)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Patching
// ///////////////////////////////////////////////

func TestPatchChannel_PatchesASettledBroadcast(t *testing.T) {
	// One rule decides whether a broadcast has settled, and both loops read
	// it. A broadcast with no recorded end is neither finished the moment it
	// started nor running for the life of the library.
	tests := []struct {
		name      string
		endedAt   *time.Time
		now       time.Time
		wantPatch bool
	}{
		{
			name:      "a recorded end past the settle window",
			endedAt:   new(planDay.Add(4 * time.Hour)),
			now:       planDay.Add(12 * time.Hour),
			wantPatch: true,
		},
		{
			name:      "a recorded end still inside the settle window",
			endedAt:   new(planDay.Add(4 * time.Hour)),
			now:       planDay.Add(5 * time.Hour),
			wantPatch: false,
		},
		{
			name:      "no recorded end, past any plausible broadcast",
			endedAt:   nil,
			now:       planDay.Add(72 * time.Hour),
			wantPatch: true,
		},
		{
			name:      "no recorded end, and it could still be running",
			endedAt:   nil,
			now:       planDay.Add(6 * time.Hour),
			wantPatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			coverage := &fakeCoverage{
				days: []store.Day{{Date: planDay, State: store.CoverageLive, Broadcasts: 1}},
				broadcasts: []store.Broadcast{
					{ID: 1, ChannelID: 1, StartedAt: planDay, EndedAt: tt.endedAt},
				},
				recordings: map[int64][]store.Recording{1: {{State: store.StateComplete}}},
			}
			patcher := &recordingPatcher{}

			deps := passDeps(coverage, nil, &recordingFetcher{})
			deps.Patch = patcher

			if _, err := Pass(context.Background(), deps, passChannels("examplechannel"), tt.now); err != nil {
				t.Fatalf("Pass() err = %v, want nil", err)
			}

			if got := len(patcher.patched) > 0; got != tt.wantPatch {
				t.Errorf("patched = %t, want %t", got, tt.wantPatch)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Cancellation
// ///////////////////////////////////////////////

func TestPass_StopsOnCancellationRatherThanWalkingEveryChannel(t *testing.T) {
	// A pass runs on a timer inside a daemon that must unwind promptly. It
	// reports the cancellation so the loop above it can tell a shutdown from
	// a round that finished.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	discovery := &countingDiscovery{}
	_, err := Pass(ctx, passDeps(recoverable(), discovery, &recordingFetcher{}),
		passChannels("first", "second"), afterSettling())

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Pass() err = %v, want context.Canceled", err)
	}
	if len(discovery.seen) != 0 {
		t.Errorf("Pass listed %v after cancellation, want none", discovery.seen)
	}
}

// ///////////////////////////////////////////////
// Bounds
// ///////////////////////////////////////////////

func TestPass_FetchesNoMoreAtOnceThanItWasAllowed(t *testing.T) {
	// A fetch competes with recording for the same link and the same disk,
	// which is why the setting exists. Ignoring it would let a channel with a
	// month of gaps saturate both.
	coverage := oneDay(store.CoverageMissed, nil)
	coverage.broadcasts = nil
	coverage.recordings = map[int64][]store.Recording{}
	for id := int64(1); id <= 6; id++ {
		coverage.broadcasts = append(coverage.broadcasts, store.Broadcast{
			ID: id, ChannelID: 1, StartedAt: planDay.Add(time.Duration(id) * time.Hour),
		})
		coverage.recordings[id] = nil
	}
	coverage.days = []store.Day{{Date: planDay, State: store.CoverageMissed, Broadcasts: 6}}

	fetcher := &recordingFetcher{hold: make(chan struct{})}
	deps := passDeps(coverage, &countingDiscovery{}, fetcher)
	deps.MaxConcurrent = 2

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Pass(context.Background(), deps, passChannels("examplechannel"), afterSettling())
	}()

	// Released once every fetch that is going to start has started, so the
	// peak reflects the bound rather than how fast the goroutines were
	// scheduled.
	for fetcher.inFlight.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	close(fetcher.hold)
	<-done

	if peak := fetcher.peak.Load(); peak > 2 {
		t.Errorf("Pass ran %d fetches at once, want at most 2", peak)
	}
	if got, want := len(fetcher.fetchedIDs()), 6; got != want {
		t.Errorf("Pass fetched %d broadcasts, want all %d", got, want)
	}
}

func TestPass_PlansFromStorageWithNoDiscovererWired(t *testing.T) {
	// A platform with no listing tool available still has whatever the
	// recorder saw, so the gaps around it are still fetchable.
	fetcher := &recordingFetcher{}
	deps := passDeps(recoverable(), nil, fetcher)

	if _, err := Pass(context.Background(), deps, passChannels("examplechannel"),
		afterSettling()); err != nil {
		t.Fatalf("Pass() err = %v, want nil", err)
	}
	if len(fetcher.fetchedIDs()) == 0 {
		t.Error("Pass fetched nothing with no discoverer, want the stored gap fetched")
	}
}

// ///////////////////////////////////////////////
// Reporting
// ///////////////////////////////////////////////

func TestPass_ReportsARecoveryToWhoeverIsListening(t *testing.T) {
	// A pass runs unattended on a timer. Without this the only account of a
	// recovery is a log file nobody opens, and the whole notification story
	// stops at the edge of live capture.
	var reported []Outcome
	deps := passDeps(recoverable(), &countingDiscovery{}, &recordingFetcher{})
	deps.Report = func(o Outcome) { reported = append(reported, o) }

	if _, err := Pass(context.Background(), deps, passChannels("examplechannel"),
		afterSettling()); err != nil {
		t.Fatalf("Pass() err = %v, want nil", err)
	}

	if len(reported) != 1 {
		t.Fatalf("reported %d outcomes, want 1: %+v", len(reported), reported)
	}
	if reported[0].Kind != OutcomeRecovered {
		t.Errorf("Kind = %q, want %q", reported[0].Kind, OutcomeRecovered)
	}
	if reported[0].Channel != "examplechannel" {
		t.Errorf("Channel = %q, want %q", reported[0].Channel, "examplechannel")
	}
}

func TestPass_ReportsOnlyTheFailureThatWillNotResolveItself(t *testing.T) {
	// A held broadcast and a transient download failure both come right on
	// a later pass, so telling an operator about them is noise that trains
	// them to ignore the ones that matter.
	tests := []struct {
		name       string
		err        error
		wantReport bool
	}{
		{name: "gave up", err: ErrGaveUp, wantReport: true},
		{name: "another fetcher holds it", err: ErrNotClaimed, wantReport: false},
		{name: "already captured", err: ErrAlreadyCaptured, wantReport: false},
		{name: "no address yet", err: ErrNoAddress, wantReport: false},
		{name: "a transient failure", err: errors.New("connection reset"), wantReport: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reported []Outcome
			deps := passDeps(recoverable(), &countingDiscovery{}, &recordingFetcher{err: tt.err})
			deps.Report = func(o Outcome) { reported = append(reported, o) }

			if _, err := Pass(context.Background(), deps, passChannels("examplechannel"),
				afterSettling()); err != nil {
				t.Fatalf("Pass() err = %v, want nil", err)
			}

			if got := len(reported) > 0; got != tt.wantReport {
				t.Errorf("reported = %v for %s, want %v", got, tt.name, tt.wantReport)
			}
		})
	}
}

func TestPass_SurvivesWithNobodyListening(t *testing.T) {
	// The log is always written, so a nil reporter is a configuration and
	// not a fault.
	deps := passDeps(recoverable(), &countingDiscovery{}, &recordingFetcher{})
	deps.Report = nil

	if _, err := Pass(context.Background(), deps, passChannels("examplechannel"),
		afterSettling()); err != nil {
		t.Errorf("Pass() err = %v, want nil with no reporter", err)
	}
}

// ///////////////////////////////////////////////
// What a Result says about the window
// ///////////////////////////////////////////////

func TestPass_TellsWorkStillOutstandingFromWorkThatIsDone(t *testing.T) {
	// The recorder decides whether to ask for a window again from this. A
	// round where nothing could be taken has not finished the work, and
	// reading it as clean consumes a window having fetched none of it, with
	// the log reporting success.
	tests := []struct {
		name         string
		err          error
		wantFailed   int
		wantDeferred int
	}{
		{name: "fetched", err: nil},
		{
			name:         "something else holds it",
			err:          ErrNotClaimed,
			wantDeferred: 1,
		},
		{
			name: "already in the library",
			err:  ErrAlreadyCaptured,
		},
		{
			name: "no address published yet",
			err:  ErrNoAddress,
		},
		{
			name: "the library is full",
			err:  ErrNoRoom,
		},
		{
			name: "given up on for good",
			err:  ErrGaveUp,
		},
		{
			name:       "something nobody expected",
			err:        errors.New("the volume stopped answering"),
			wantFailed: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := passDeps(recoverable(), &countingDiscovery{}, &recordingFetcher{err: tt.err})

			result, err := Pass(context.Background(), deps, passChannels("examplechannel"), afterSettling())
			if err != nil {
				t.Fatalf("Pass() err = %v, want nil", err)
			}

			if result.Failed != tt.wantFailed {
				t.Errorf("Failed = %d, want %d", result.Failed, tt.wantFailed)
			}
			if result.Deferred != tt.wantDeferred {
				t.Errorf("Deferred = %d, want %d", result.Deferred, tt.wantDeferred)
			}
		})
	}
}
