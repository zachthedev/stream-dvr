package backfill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/fetch"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/record"
	"zach.tools/go/stream-dvr/internal/space"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// fakeDownloader reports a canned result for one download.
type fakeDownloader struct {
	path    string
	err     error
	request fetch.Request
	calls   int
}

// failure is one charged fetch failure.
type failure struct {
	reason  string
	retryAt time.Time
}

// fakeClaims records what the fetcher claimed, wrote, and charged.
type fakeClaims struct {
	claimed bool
	// claimCalls counts claims taken, which is what says whether a broadcast
	// spent one of its attempts.
	claimCalls int
	existing   []store.Recording
	created    []store.Recording
	createErr  error
	released   []store.FetchState
	failures   []failure
	nextID     int64
	finalized  []int64
	// attempts is what a claim has already counted.
	attempts int
	// abandoned counts broadcasts handed back untried, which is what a
	// cancelled fetch must do rather than hold its claim for the lease.
	abandoned int
	// previous is the fetch row as it stood before the failure under test,
	// which is where the wait a retry escalates from is read.
	previous store.Fetch
	// muted records what each recording was told about how much of it the
	// platform silenced.
	muted map[int64]time.Duration
}

// exampleChannel is the fixture every case fetches for.
var exampleChannel = Channel{ID: 1, Name: "examplechannel", Source: "twitch"}

// SetMutedDuration implements Claims.
func (f *fakeClaims) SetMutedDuration(recordingID int64, muted time.Duration) error {
	if f.muted == nil {
		f.muted = map[int64]time.Duration{}
	}
	f.muted[recordingID] = muted
	return nil
}

// Download implements Downloader.
func (f *fakeDownloader) Download(_ context.Context, request fetch.Request) (fetch.Result, error) {
	f.calls++
	f.request = request
	if f.err != nil {
		return fetch.Result{}, f.err
	}
	return fetch.Result{Path: f.path}, nil
}

// ClaimFetch implements Claims.
func (f *fakeClaims) ClaimFetch(int64, int64, time.Time, time.Duration) (bool, error) {
	f.claimCalls++
	return f.claimed, nil
}

// ReleaseFetch implements Claims.
func (f *fakeClaims) ReleaseFetch(_ int64, state store.FetchState, _ time.Time) error {
	f.released = append(f.released, state)
	return nil
}

// RecordFetchFailure implements Claims.
func (f *fakeClaims) RecordFetchFailure(_ int64, reason string, _, retryAt time.Time) error {
	f.failures = append(f.failures, failure{reason: reason, retryAt: retryAt})
	return nil
}

// AbandonFetch implements Claims, recording that the broadcast was handed
// back without a try.
func (f *fakeClaims) AbandonFetch(int64, int64, time.Time) error {
	f.abandoned++
	return nil
}

// RecordingsForBroadcast implements Claims, answering with a copy.
func (f *fakeClaims) RecordingsForBroadcast(int64) ([]store.Recording, error) {
	return append([]store.Recording(nil), f.existing...), nil
}

// FetchFor implements Claims, reporting the row as it stood before this
// failure: what a claim has counted, and how long the last try waited.
func (f *fakeClaims) FetchFor(broadcastID int64) (store.Fetch, error) {
	row := f.previous
	row.BroadcastID = broadcastID
	row.Attempts = f.attempts
	return row, nil
}

// CreateRecording implements Claims.
func (f *fakeClaims) CreateRecording(r store.Recording) (store.Recording, error) {
	if f.createErr != nil {
		return store.Recording{}, f.createErr
	}
	// recordings.channel_id is NOT NULL REFERENCES channels(id) and the
	// database runs with foreign keys on, so a fake that takes any row lets
	// a caller that never sets the channel pass every test and fail against
	// every real library.
	if r.ChannelID == 0 {
		return store.Recording{}, errors.New("constraint failed: FOREIGN KEY constraint failed (787)")
	}
	f.nextID++
	r.ID = f.nextID
	f.created = append(f.created, r)
	return r, nil
}

// newFetcher wires a fetcher over a temporary library, returning it with
// the incoming directory a download is expected to land in.
func newFetcher(t *testing.T, downloader *fakeDownloader, claims *fakeClaims) (*Fetcher, string) {
	t.Helper()

	root := t.TempDir()
	incoming := paths.IncomingDir(root)
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		t.Fatalf("creating the incoming directory: %v", err)
	}

	claims.claimed = true
	finalize := func(_ context.Context, id int64) error {
		claims.finalized = append(claims.finalized, id)
		return nil
	}
	return NewFetcher(downloader, claims, finalize, nil, root, 1, FetchOptions{}, quiet()), incoming
}

// newRecoveringFetcher builds a fetcher that can reach a copy's original
// audio, and reports what it measures afterwards.
func newRecoveringFetcher(t *testing.T, downloader *fakeDownloader, claims *fakeClaims,
	measure Measurer, recovery func(context.Context, string, []store.MutedSpan) (string, bool, error),
) (*Fetcher, string) {
	t.Helper()

	root := t.TempDir()
	incoming := paths.IncomingDir(root)
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		t.Fatalf("creating the incoming directory: %v", err)
	}

	claims.claimed = true
	finalize := func(_ context.Context, id int64) error {
		claims.finalized = append(claims.finalized, id)
		return nil
	}
	return NewFetcher(downloader, claims, finalize, measure, root, 1, FetchOptions{
		OriginalAudio: recovery,
	}, quiet()), incoming
}

// aSilencedCandidate is a broadcast the platform muted in two stretches.
func aSilencedCandidate() Candidate {
	candidate := aCandidate()
	candidate.Broadcast.Muted = []store.MutedSpan{
		{Offset: 30 * time.Minute, Duration: 3 * time.Minute},
		{Offset: 90 * time.Minute, Duration: 3 * time.Minute},
	}
	return candidate
}

// onlyMuted returns the silence figure recorded for the one recording a
// fetch created.
func onlyMuted(t *testing.T, claims *fakeClaims) time.Duration {
	t.Helper()

	if len(claims.muted) != 1 {
		t.Fatalf("recorded a silence figure for %d recordings, want exactly 1", len(claims.muted))
	}
	for _, muted := range claims.muted {
		return muted
	}
	return 0
}

// servesOriginal answers that the original audio is reachable at address.
func servesOriginal(address string) func(context.Context, string, []store.MutedSpan) (string, bool, error) {
	return func(context.Context, string, []store.MutedSpan) (string, bool, error) {
		return address, true, nil
	}
}

// downloaded points a fake at the file a well-behaved tool would write.
//
// The stem comes from record.CaptureStem rather than being spelled out
// here, because that is what the fetcher asks for and internal/record's
// own tests are what pin its shape. A copy here would be a second source
// of truth for a name that must match exactly.
func downloaded(incoming string) string {
	return filepath.Join(incoming, captureStem()+".mp4")
}

// captureStem is the name the fixture broadcast is fetched under.
func captureStem() string {
	return record.CaptureStem(exampleChannel.Source, exampleChannel.Name, aCandidate().Broadcast.StartedAt)
}

// aCandidate returns a candidate for the fixture broadcast.
func aCandidate() Candidate {
	return Candidate{Broadcast: store.Broadcast{
		ID:        7,
		ChannelID: 1,
		RemoteID:  "v100001",
		URL:       "https://example.com/videos/v100001",
		StartedAt: time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC),
	}}
}

// ///////////////////////////////////////////////
// The address a fetch is pointed at
// ///////////////////////////////////////////////

func TestFetch_SendsTheBroadcastAddress(t *testing.T) {
	// A remote id is a bare video id, not an address. Handing one to yt-dlp
	// either routes to another site's extractor or fails to parse, so every
	// backfill fetch fails and the reason reads like a network fault.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = downloaded(incoming)

	candidate := aCandidate()
	if err := fetcher.Fetch(context.Background(), candidate, exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if got, want := downloader.request.URL, candidate.Broadcast.URL; got != want {
		t.Errorf("Request.URL = %q, want the broadcast's address %q", got, want)
	}
}

func TestFetch_RefusesABroadcastWithNoAddress(t *testing.T) {
	// Nothing can be downloaded from a row carrying no address, and claiming
	// it would spend one of the broadcast's attempts on a question the next
	// discovery pass answers.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, _ := newFetcher(t, downloader, claims)

	candidate := aCandidate()
	candidate.Broadcast.URL = ""

	err := fetcher.Fetch(context.Background(), candidate, exampleChannel, time.Now())
	if !errors.Is(err, ErrNoAddress) {
		t.Errorf("Fetch() err = %v, want ErrNoAddress", err)
	}
	if downloader.calls != 0 {
		t.Error("Fetch() downloaded a broadcast with no address")
	}
	if claims.claimCalls != 0 {
		t.Errorf("claimed %d times, want none: a claim spends an attempt", claims.claimCalls)
	}
	if len(claims.failures) != 0 {
		t.Errorf("charged %d failures, want none", len(claims.failures))
	}
}

func TestFetch_RecordsHowMuchOfTheCopyIsSilenced(t *testing.T) {
	// A recovered copy can carry stretches the platform silenced, and nothing
	// in the file says which. Recording the total is what stops the calendar
	// reporting it as recovered with no qualification, and a machine that
	// could not ask has to leave it unknown rather than claim none.
	tests := []struct {
		name  string
		muted []store.MutedSpan
		want  *time.Duration
	}{
		{
			name:  "two silenced stretches",
			muted: []store.MutedSpan{{Duration: 30 * time.Second}, {Duration: 3 * time.Minute}},
			want:  new(3*time.Minute + 30*time.Second),
		},
		{
			name:  "the platform silenced nothing",
			muted: []store.MutedSpan{},
			want:  new(time.Duration(0)),
		},
		{
			name:  "nobody could ask",
			muted: nil,
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &fakeClaims{}
			downloader := &fakeDownloader{}
			fetcher, incoming := newFetcher(t, downloader, claims)
			downloader.path = downloaded(incoming)

			candidate := aCandidate()
			candidate.Broadcast.Muted = tt.muted

			if err := fetcher.Fetch(context.Background(), candidate, exampleChannel, time.Now()); err != nil {
				t.Fatalf("Fetch() err = %v, want nil", err)
			}
			if len(claims.created) != 1 {
				t.Fatalf("Fetch created %d recordings, want 1", len(claims.created))
			}

			got, recorded := claims.muted[claims.created[0].ID]
			switch {
			case tt.want == nil && recorded:
				t.Errorf("recorded %s silenced, want it left unknown", got)
			case tt.want != nil && !recorded:
				t.Errorf("recorded nothing, want %s", *tt.want)
			case tt.want != nil && got != *tt.want:
				t.Errorf("recorded %s silenced, want %s", got, *tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// The overwrite guard
// ///////////////////////////////////////////////

func TestFetch_RefusesABroadcastWithALiveRecording(t *testing.T) {
	// The hard rule of the package. A recovered copy never displaces a
	// live one, because platforms mute a stored copy after the fact and
	// the live recording cannot be got again.
	claims := &fakeClaims{existing: []store.Recording{
		{Origin: store.OriginLive, State: store.StateComplete},
	}}
	downloader := &fakeDownloader{}
	fetcher, _ := newFetcher(t, downloader, claims)

	err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now())

	if !errors.Is(err, ErrAlreadyCaptured) {
		t.Errorf("Fetch() err = %v, want ErrAlreadyCaptured", err)
	}
	if downloader.calls != 0 {
		t.Error("Fetch() downloaded a broadcast that is already in the library")
	}
}

func TestFetch_ProceedsPastAFailedLiveRecording(t *testing.T) {
	// A capture that gave up left nothing behind, which is the case
	// backfill exists for.
	claims := &fakeClaims{existing: []store.Recording{
		{Origin: store.OriginLive, State: store.StateFailed},
	}}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = downloaded(incoming)

	if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if len(claims.created) != 1 {
		t.Errorf("created %d recordings, want 1", len(claims.created))
	}
}

// ///////////////////////////////////////////////
// What lands in the library
// ///////////////////////////////////////////////

func TestFetch_RecordsTheOriginAsRecovered(t *testing.T) {
	// The sidecar and the retention ranking both read this. A recovered
	// copy is refetchable and therefore cheaper to delete than a live one.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = downloaded(incoming)

	if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if got := claims.created[0].Origin; got != store.OriginRecovered {
		t.Errorf("Origin = %q, want %q", got, store.OriginRecovered)
	}
	if got := claims.created[0].State; got != store.StateAwaitingFinalize {
		t.Errorf("State = %q, want %q", got, store.StateAwaitingFinalize)
	}
}

func TestFetch_KeepsTheBroadcastStartRatherThanTheFetchTime(t *testing.T) {
	// The calendar buckets by this. Stamping the fetch time would put a
	// recovered broadcast on whichever day it happened to be fetched.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = downloaded(incoming)

	candidate := aCandidate()
	if err := fetcher.Fetch(context.Background(), candidate, exampleChannel,
		candidate.Broadcast.StartedAt.Add(72*time.Hour)); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if got := claims.created[0].StartedAt; !got.Equal(candidate.Broadcast.StartedAt) {
		t.Errorf("StartedAt = %v, want the broadcast's %v", got, candidate.Broadcast.StartedAt)
	}
}

func TestFetch_AsksForALiteralOutputTemplate(t *testing.T) {
	// The structural defence against a remote title reaching the
	// filesystem: the tool is told exactly what to write, so its own
	// title-derived template never runs.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = downloaded(incoming)

	if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}

	want := captureStem() + ".%(ext)s"
	if got := filepath.Base(downloader.request.Output); got != want {
		t.Errorf("Output = %q, want %q, with only the extension left to the tool", got, want)
	}
}

// ///////////////////////////////////////////////
// Refusing what the tool reports
// ///////////////////////////////////////////////

func TestFetch_RefusesAFileOutsideTheIncomingDirectory(t *testing.T) {
	// A tool that reported somewhere else chose its own name, and trusting
	// it would let a remote title decide a path.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{path: filepath.Join(t.TempDir(), "elsewhere.mp4")}
	fetcher, _ := newFetcher(t, downloader, claims)

	if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now()); err == nil {
		t.Error("Fetch() err = nil for a file outside incoming, want a refusal")
	}
	if len(claims.created) != 0 {
		t.Error("Fetch() created a recording for a file it should have refused")
	}
}

func TestFetch_RefusesANameThisFetchDidNotChoose(t *testing.T) {
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = filepath.Join(incoming, "a-title-the-streamer-wrote.mp4")

	if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now()); err == nil {
		t.Error("Fetch() err = nil for a name it did not choose, want a refusal")
	}
	if len(claims.created) != 0 {
		t.Error("Fetch() created a recording for a name it should have refused")
	}
}

func TestFetch_RefusesWhenTheToolReportedNoFile(t *testing.T) {
	claims := &fakeClaims{}
	fetcher, _ := newFetcher(t, &fakeDownloader{}, claims)

	if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now()); err == nil {
		t.Error("Fetch() err = nil when nothing was reported, want a refusal")
	}
}

// ///////////////////////////////////////////////
// Claiming and charging
// ///////////////////////////////////////////////

func TestFetch_StopsWhenAnotherFetcherHoldsTheBroadcast(t *testing.T) {
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, _ := newFetcher(t, downloader, claims)
	claims.claimed = false

	err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now())

	if !errors.Is(err, ErrNotClaimed) {
		t.Errorf("Fetch() err = %v, want ErrNotClaimed", err)
	}
	if downloader.calls != 0 {
		t.Error("Fetch() downloaded without holding the claim")
	}
}

func TestFetch_ChargesFailuresByWhatTheyMean(t *testing.T) {
	// A zero retry time is what the store reads as terminal. A timer
	// cannot make a private video public and cannot supply a login, so
	// retrying either spends a request per pass and changes nothing.
	tests := []struct {
		name     string
		failure  fetch.Failure
		terminal bool
		why      string
	}{
		{
			name:     "permanent",
			failure:  fetch.FailurePermanent,
			terminal: true,
			why:      "a removed video answers the same way forever",
		},
		{
			name:     "auth",
			failure:  fetch.FailureAuth,
			terminal: true,
			why:      "a timer cannot supply a credential",
		},
		{
			name:     "transient",
			failure:  fetch.FailureTransient,
			terminal: false,
			why:      "the platform was having a moment and a retry may work",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &fakeClaims{}
			downloader := &fakeDownloader{err: &fetch.ToolError{
				Failure: tt.failure,
				Excerpt: "ERROR: something the tool said",
				Err:     errors.New("exit status 1"),
			}}
			fetcher, _ := newFetcher(t, downloader, claims)
			now := time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

			if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, now); err == nil {
				t.Fatal("Fetch() err = nil for a failed download, want it reported")
			}
			if len(claims.failures) != 1 {
				t.Fatalf("charged %d failures, want 1", len(claims.failures))
			}

			zero := claims.failures[0].retryAt.IsZero()
			if zero != tt.terminal {
				t.Errorf("retryAt zero = %v, want %v (%s)", zero, tt.terminal, tt.why)
			}
			if !tt.terminal && !claims.failures[0].retryAt.After(now) {
				t.Errorf("retryAt = %v, want it after %v", claims.failures[0].retryAt, now)
			}
		})
	}
}

func TestFetch_RefusesWhenTheLibraryHasNoRoom(t *testing.T) {
	// Nothing here admits a download today, so a recovery pass fills the
	// library the capture budget is guarding. The download runs before any
	// row exists, so the size cap cannot see it while it runs, and when the
	// volume tightens the watermark cancels the live capture while the
	// download that caused the pressure runs on.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = downloaded(incoming)

	refusal := &space.RefusalError{Limit: "library max_size", Need: 1 << 34, Have: 1 << 30}
	fetcher.admit = func(int64) error { return refusal }

	err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel,
		time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC))

	if !errors.Is(err, ErrNoRoom) {
		t.Fatalf("Fetch() err = %v, want it to wrap %v", err, ErrNoRoom)
	}
	if downloader.calls != 0 {
		t.Error("a download ran despite the refusal")
	}
	// Not charged: the library being full is not the platform refusing, and
	// spending attempts on it abandons the broadcast for good once the
	// operator makes room.
	if len(claims.failures) != 0 {
		t.Errorf("charged %v, want nothing for a library with no room", claims.failures)
	}
}

func TestFetch_EstimatesFromTheBroadcastsOwnLength(t *testing.T) {
	// A download admitted against a figure of zero is admitted always. The
	// broadcast's own length is what the platform published, so it is the
	// only honest estimate there is.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = downloaded(incoming)

	var asked int64
	fetcher.admit = func(estimate int64) error {
		asked = estimate
		return nil
	}

	candidate := aCandidate()
	ended := candidate.Broadcast.StartedAt.Add(4 * time.Hour)
	candidate.Broadcast.EndedAt = &ended

	if err := fetcher.Fetch(context.Background(), candidate, exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}

	want := space.Estimate(space.DefaultBitrate, 4*time.Hour)
	if asked != want {
		t.Errorf("admitted against %d bytes, want %d for a four hour broadcast", asked, want)
	}
}

func TestFetch_DoesNotChargeAShutdown(t *testing.T) {
	// The claim counts an attempt before the download runs, and the
	// classifier has no marker for a process somebody stopped, so a
	// cancelled download reads as transient. Five reboots during a fetch
	// then abandon the broadcast permanently, even though the tool left the
	// partial file behind and would have resumed from it.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{err: context.Canceled}
	fetcher, _ := newFetcher(t, downloader, claims)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := fetcher.Fetch(ctx, aCandidate(), exampleChannel, time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch() err = %v, want the cancellation reported", err)
	}
	if len(claims.failures) != 0 {
		t.Errorf("charged %v, want nothing: the operator stopped the recorder", claims.failures)
	}
	if errors.Is(err, ErrGaveUp) {
		t.Error("a shutdown was reported as giving up on the broadcast")
	}
}

func TestFetch_DiscardsThePartialWhenItGivesUp(t *testing.T) {
	// yt-dlp leaves a partial file so a retry resumes, which is right for a
	// transient failure and wrong for a terminal one: nothing will ever
	// claim this broadcast again, so the file has no owner, the size cap
	// cannot see it, and nothing else removes it.
	tests := []struct {
		name     string
		failure  fetch.Failure
		wantGone bool
		why      string
	}{
		{
			name:     "terminal",
			failure:  fetch.FailurePermanent,
			wantGone: true,
			why:      "no pass will resume it, so the bytes are abandoned",
		},
		{
			name:    "retryable",
			failure: fetch.FailureTransient,
			why:     "the next attempt resumes from what is already downloaded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &fakeClaims{}
			downloader := &fakeDownloader{err: &fetch.ToolError{
				Failure: tt.failure,
				Excerpt: "ERROR: something the tool said",
				Err:     errors.New("exit status 1"),
			}}
			fetcher, incoming := newFetcher(t, downloader, claims)

			partial := filepath.Join(incoming, captureStem()+".mp4.part")
			if err := os.WriteFile(partial, []byte("half a broadcast"), 0o644); err != nil {
				t.Fatalf("writing the partial download: %v", err)
			}

			if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel,
				time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)); err == nil {
				t.Fatal("Fetch() err = nil for a failed download, want it reported")
			}

			_, statErr := os.Stat(partial)
			gone := errors.Is(statErr, os.ErrNotExist)
			if gone != tt.wantGone {
				t.Errorf("partial removed = %t, want %t: %s", gone, tt.wantGone, tt.why)
			}
		})
	}
}

func TestFetch_LeavesAnotherBroadcastsPartialAlone(t *testing.T) {
	// The stem is what scopes the removal. A rule that emptied the
	// directory would delete a download running for a different broadcast.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{err: &fetch.ToolError{
		Failure: fetch.FailurePermanent,
		Excerpt: "ERROR: Video unavailable",
		Err:     errors.New("exit status 1"),
	}}
	fetcher, incoming := newFetcher(t, downloader, claims)

	other := filepath.Join(incoming, "twitch-anotherchannel-1772658900.mp4.part")
	if err := os.WriteFile(other, []byte("someone else's download"), 0o644); err != nil {
		t.Fatalf("writing the other partial: %v", err)
	}

	if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel,
		time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("Fetch() err = nil for a failed download, want it reported")
	}

	if _, err := os.Stat(other); err != nil {
		t.Errorf("another broadcast's partial was removed: %v", err)
	}
}

// ///////////////////////////////////////////////
// Finalizing
// ///////////////////////////////////////////////

func TestFetch_FinalizesWhatItCreated(t *testing.T) {
	// A fetched file that never finalizes sits in incoming under a capture
	// name, which the library treats as unfinished work forever.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = downloaded(incoming)

	if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if len(claims.finalized) != 1 || claims.finalized[0] != claims.created[0].ID {
		t.Errorf("finalized %v, want the recording it created", claims.finalized)
	}
}

func TestFetch_ReleasesTheClaimOnceDone(t *testing.T) {
	// A claim held past the work blocks the broadcast until the lease runs
	// out, which is hours.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	fetcher, incoming := newFetcher(t, downloader, claims)
	downloader.path = downloaded(incoming)

	if err := fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if len(claims.released) != 1 || claims.released[0] != store.FetchDone {
		t.Errorf("released %v, want one release as %q", claims.released, store.FetchDone)
	}
}

// ///////////////////////////////////////////////
// Attempt cap
// ///////////////////////////////////////////////

// ///////////////////////////////////////////////
// Recovering the audio a platform silenced
// ///////////////////////////////////////////////

func TestFetch_DownloadsASilencedCopyFromItsOriginalAudio(t *testing.T) {
	// A broadcast the recorder missed is downloaded whole, and the playlist
	// carrying the audio as broadcast carries every other segment too. So the
	// whole download moves to it rather than a range of it, and the file
	// arrives whole and audible instead of whole and part silent.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	measure := &fakeMeasurer{}
	original := "https://example.com/vod/index-dvr.m3u8"
	fetcher, incoming := newRecoveringFetcher(t, downloader, claims, measure, servesOriginal(original))
	downloader.path = downloaded(incoming)

	if err := fetcher.Fetch(context.Background(), aSilencedCandidate(), exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if got := downloader.request.URL; got != original {
		t.Errorf("Request.URL = %q, want the original-audio playlist %q", got, original)
	}
}

func TestFetch_LeavesACopyWithNothingSilencedAlone(t *testing.T) {
	// The lookup costs a request per segment covering the silenced stretches.
	// A broadcast with none has nothing to look up, and asking anyway spends
	// that on every fetch for good.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	asked := false
	recovery := func(context.Context, string, []store.MutedSpan) (string, bool, error) {
		asked = true
		return "https://example.com/vod/index-dvr.m3u8", true, nil
	}
	fetcher, incoming := newRecoveringFetcher(t, downloader, claims, &fakeMeasurer{}, recovery)
	downloader.path = downloaded(incoming)

	candidate := aCandidate()
	if err := fetcher.Fetch(context.Background(), candidate, exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if asked {
		t.Error("looked up the original audio for a broadcast with nothing silenced")
	}
	if got := downloader.request.URL; got != candidate.Broadcast.URL {
		t.Errorf("Request.URL = %q, want the broadcast's own address", got)
	}
}

func TestFetch_FallsBackWhenNoOriginalSurvives(t *testing.T) {
	// Most stored copies keep no original. The silenced copy is still the
	// recording the operator missed, so it is fetched rather than refused.
	tests := []struct {
		name     string
		recovery func(context.Context, string, []store.MutedSpan) (string, bool, error)
	}{
		{
			name: "the platform kept none",
			recovery: func(context.Context, string, []store.MutedSpan) (string, bool, error) {
				return "", false, nil
			},
		},
		{
			name: "the lookup could not answer",
			recovery: func(context.Context, string, []store.MutedSpan) (string, bool, error) {
				return "", false, errors.New("connection reset")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &fakeClaims{}
			downloader := &fakeDownloader{}
			fetcher, incoming := newRecoveringFetcher(t, downloader, claims, &fakeMeasurer{}, tt.recovery)
			downloader.path = downloaded(incoming)

			candidate := aSilencedCandidate()
			if err := fetcher.Fetch(context.Background(), candidate, exampleChannel, time.Now()); err != nil {
				t.Fatalf("Fetch() err = %v, want the copy fetched anyway", err)
			}
			if got := downloader.request.URL; got != candidate.Broadcast.URL {
				t.Errorf("Request.URL = %q, want the broadcast's own address", got)
			}
			if got := onlyMuted(t, claims); got != mutedTotal(candidate.Broadcast.Muted) {
				t.Errorf("muted = %s, want the %s the platform reported",
					got, mutedTotal(candidate.Broadcast.Muted))
			}
		})
	}
}

func TestFetch_RecordsNoSilenceOnceTheAudioComesBack(t *testing.T) {
	// The figure describes this file, not the platform's copy. A recovered
	// download holds no silence however much the platform silenced its own,
	// and the sidecar beside it has to say so.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	measure := &fakeMeasurer{silent: false}
	fetcher, incoming := newRecoveringFetcher(t, downloader, claims, measure,
		servesOriginal("https://example.com/vod/index-dvr.m3u8"))
	downloader.path = downloaded(incoming)

	if err := fetcher.Fetch(context.Background(), aSilencedCandidate(), exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if got := onlyMuted(t, claims); got != 0 {
		t.Errorf("muted = %s, want none: the download came back audible", got)
	}
	if len(measure.listened) == 0 {
		t.Error("nothing was measured, so the recovery was taken on trust")
	}
}

func TestFetch_KeepsTheSilenceFigureWhenARecoveredCopyIsStillSilent(t *testing.T) {
	// An address that serves is not audio. A copy served from the wrong place,
	// or served short, would otherwise be recorded as recovered and never
	// looked at again.
	tests := []struct {
		name    string
		measure *fakeMeasurer
	}{
		{name: "it came back silent", measure: &fakeMeasurer{silent: true}},
		{name: "it could not be measured", measure: &fakeMeasurer{silentErr: errors.New("ffmpeg missing")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &fakeClaims{}
			downloader := &fakeDownloader{}
			fetcher, incoming := newRecoveringFetcher(t, downloader, claims, tt.measure,
				servesOriginal("https://example.com/vod/index-dvr.m3u8"))
			downloader.path = downloaded(incoming)

			candidate := aSilencedCandidate()
			if err := fetcher.Fetch(context.Background(), candidate, exampleChannel, time.Now()); err != nil {
				t.Fatalf("Fetch() err = %v, want the recording kept", err)
			}
			if got := onlyMuted(t, claims); got != mutedTotal(candidate.Broadcast.Muted) {
				t.Errorf("muted = %s, want the %s the platform reported",
					got, mutedTotal(candidate.Broadcast.Muted))
			}
			if len(claims.created) != 1 {
				t.Errorf("created %d recordings, want the copy kept even unrecovered", len(claims.created))
			}
		})
	}
}

func TestFetch_BoundsHowManySilencedStretchesItMeasures(t *testing.T) {
	// A long broadcast names hundreds of them, and each measurement spawns a
	// subprocess over a multi-gigabyte file. The question is whether the
	// playlist served originals at all.
	claims := &fakeClaims{}
	downloader := &fakeDownloader{}
	measure := &fakeMeasurer{}
	fetcher, incoming := newRecoveringFetcher(t, downloader, claims, measure,
		servesOriginal("https://example.com/vod/index-dvr.m3u8"))
	downloader.path = downloaded(incoming)

	candidate := aCandidate()
	for i := range 50 {
		candidate.Broadcast.Muted = append(candidate.Broadcast.Muted, store.MutedSpan{
			Offset:   time.Duration(i) * 10 * time.Minute,
			Duration: 3 * time.Minute,
		})
	}

	if err := fetcher.Fetch(context.Background(), candidate, exampleChannel, time.Now()); err != nil {
		t.Fatalf("Fetch() err = %v, want nil", err)
	}
	if len(measure.listened) > maxVerifiedSpans {
		t.Errorf("measured %d stretches, want at most %d", len(measure.listened), maxVerifiedSpans)
	}
	if len(measure.listened) == 0 {
		t.Error("measured nothing at all, so the recovery was taken on trust")
	}
}

// ///////////////////////////////////////////////
// How long a failed fetch waits
// ///////////////////////////////////////////////

func TestFetch_NeverRetiresABroadcastOverAFailureThePlatformDidNotAnswer(t *testing.T) {
	// Terminal is the state no timer moves a row out of and no command
	// resets, so a broadcast retired this way is gone while the copy
	// upstream is fine. A reset connection, a store that could not answer,
	// and a volume that went away are all about this machine.
	tests := []struct {
		name  string
		cause error
	}{
		{
			name: "a reset connection",
			cause: &fetch.ToolError{
				Failure: fetch.FailureTransient,
				Excerpt: "ERROR: unable to download video data",
				Err:     errors.New("exit status 1"),
			},
		},
		{name: "a locked database", cause: errors.New("database is locked (5)")},
		{name: "a volume that went away", cause: errors.New("input/output error")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
			// Far past any cap a reader might expect to apply here.
			claims := &fakeClaims{claimed: true, attempts: 99}
			fetcher := NewFetcher(&fakeDownloader{err: tt.cause}, claims,
				func(context.Context, int64) error { return nil },
				nil, t.TempDir(), 1, FetchOptions{Backoff: time.Hour}, quiet())

			_ = fetcher.Fetch(context.Background(), aCandidate(), exampleChannel, now)

			if len(claims.failures) != 1 {
				t.Fatalf("recorded %d failures, want 1", len(claims.failures))
			}
			if claims.failures[0].retryAt.IsZero() {
				t.Error("the broadcast was retired, want it still reachable")
			}
		})
	}
}

// realClaims builds a store with one broadcast to fetch, and returns it with
// that broadcast's id.
//
// The escalating retry reads a row the claim rewrites as it takes the
// broadcast, so a hand-built fixture can report a shape the real store never
// produces. Only the real thing settles whether the wait actually grows.
func realClaims(t *testing.T) (*store.Store, store.Channel, int64) {
	t.Helper()

	db, err := store.OpenMemory()
	if err != nil {
		t.Fatalf("store.OpenMemory() err = %v, want nil", err)
	}
	t.Cleanup(func() { db.Close() })

	channel, err := db.UpsertChannel(exampleChannel.Source, exampleChannel.Name, "")
	if err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
	broadcast, err := db.UpsertBroadcast(store.Broadcast{
		ChannelID: channel.ID,
		StartedAt: aCandidate().Broadcast.StartedAt,
		URL:       aCandidate().Broadcast.URL,
		RemoteID:  aCandidate().Broadcast.RemoteID,
		Source:    store.SourceAPI,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	return db, channel, broadcast.ID
}

func TestFetch_WaitsLongerEachTimeABroadcastFailsAgain(t *testing.T) {
	// The recorder runs rounds unattended and nothing caps how many times a
	// whole broadcast is fetched, so the wait growing is the only thing
	// bounding what a broadcast nobody can download costs. Driven against a
	// real store, because the claim rewrites the row the delay is derived
	// from and a fake cannot be trusted to reproduce that.
	db, channel, broadcastID := realClaims(t)

	candidate := aCandidate()
	candidate.Broadcast.ID = broadcastID
	candidate.Broadcast.ChannelID = channel.ID
	fetching := Channel{ID: channel.ID, Name: exampleChannel.Name, Source: exampleChannel.Source}

	fetcher := NewFetcher(&fakeDownloader{err: errors.New("the fragment download failed")},
		db, func(context.Context, int64) error { return nil },
		nil, t.TempDir(), 1, FetchOptions{Backoff: time.Hour}, quiet())

	at := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	var waited []time.Duration
	for range 4 {
		_ = fetcher.Fetch(context.Background(), candidate, fetching, at)

		row, err := db.FetchFor(broadcastID)
		if err != nil {
			t.Fatalf("FetchFor() err = %v, want nil", err)
		}
		if row.NextAttemptAt.IsZero() {
			t.Fatalf("the broadcast was retired after %d tries, want it still reachable", len(waited)+1)
		}
		waited = append(waited, row.NextAttemptAt.Sub(at))
		// Forward to the moment the retry is allowed, so the next claim is
		// taken rather than refused by the backoff.
		at = row.NextAttemptAt
	}

	want := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour}
	if !slices.Equal(waited, want) {
		t.Errorf("waits were %v, want them doubling: %v", waited, want)
	}
}

func TestFetch_NeverWaitsLongerThanTheRetryCeiling(t *testing.T) {
	// A broadcast failing for weeks must settle at a cadence rather than
	// being pushed past the window recovery reaches back, where it would
	// never be tried again and never be recorded as given up on either.
	for _, attempts := range []int{8, 20, 1000} {
		if got := retryDelay(time.Hour, attempts); got != retryCeiling {
			t.Errorf("retryDelay(1h, %d) = %s, want it capped at %s", attempts, got, retryCeiling)
		}
	}
}

func TestFetch_GivesBackABroadcastWhoseFetchWasInterrupted(t *testing.T) {
	// A capture beginning cancels the round around it. The fetch downloaded
	// nothing and learned nothing, so leaving it claimed would hold the
	// broadcast for the rest of the lease and leave the attempt the claim
	// counted standing, which is what the retry delay grows from.
	claims := &fakeClaims{claimed: true}
	fetcher := NewFetcher(&fakeDownloader{err: errors.New("signal: killed")}, claims,
		func(context.Context, int64) error { return nil },
		nil, t.TempDir(), 1, FetchOptions{Backoff: time.Hour}, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = fetcher.Fetch(ctx, aCandidate(), exampleChannel, time.Now())

	if claims.abandoned != 1 {
		t.Errorf("handed back %d broadcasts, want the interrupted one given back", claims.abandoned)
	}
	if len(claims.failures) != 0 {
		t.Errorf("charged %d failures, want none for a fetch that never tried", len(claims.failures))
	}
}
