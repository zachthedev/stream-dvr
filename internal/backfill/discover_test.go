package backfill

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/fetch"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// fakeLister answers with canned listings.
type fakeLister struct {
	listings []fetch.Listing
	details  map[string]fetch.Listing
	listErr  error
	infoErr  error
	infoCall int
	listCall int
}

// fakeEnricher is a Lister whose site can describe a channel in one request.
type fakeEnricher struct {
	fakeLister
	// described is what Describe answers.
	described []fetch.Listing
	// describeErr makes the one-request path fail, which is what a site API
	// that is down or has revoked a credential looks like.
	describeErr error
	// askedFor records the horizon Describe was given.
	askedFor time.Time
	// describeCall counts the one-request path.
	describeCall int
}

// fakeRecorder records what discovery wrote.
type fakeRecorder struct {
	written  []store.Broadcast
	writeErr error
}

// examplechannel is the fixture channel every test discovers.
var examplechannel = Channel{ID: 1, Source: "twitch", URL: "https://example.com/examplechannel/videos"}

// farHorizon reaches back further than any fixture, so a case that is not
// about the horizon is never trimmed by it.
var farHorizon = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// quiet is a logger whose output nothing reads.
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Describe implements Enricher.
func (f *fakeEnricher) Describe(_ context.Context, _ string, since time.Time) ([]fetch.Listing, error) {
	f.describeCall++
	f.askedFor = since
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return append([]fetch.Listing(nil), f.described...), nil
}

// List implements Lister.
func (f *fakeLister) List(context.Context, string) ([]fetch.Listing, error) {
	f.listCall++
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Returned as a copy. A fake that hands back its own slice lets the
	// code under test mutate the fixture, and a test that shares storage
	// with what it is testing proves less than it looks like it does.
	return append([]fetch.Listing(nil), f.listings...), nil
}

// Info implements Lister.
func (f *fakeLister) Info(_ context.Context, url string) (fetch.Listing, error) {
	f.infoCall++
	if f.infoErr != nil {
		return fetch.Listing{}, f.infoErr
	}
	if described, ok := f.details[url]; ok {
		return described, nil
	}
	return fetch.Listing{}, errors.New("no detail for " + url)
}

// UpsertBroadcast implements Recorder.
func (f *fakeRecorder) UpsertBroadcast(b store.Broadcast) (store.Broadcast, error) {
	if f.writeErr != nil {
		return store.Broadcast{}, f.writeErr
	}
	f.written = append(f.written, b)
	return b, nil
}

// ///////////////////////////////////////////////
// Trust
// ///////////////////////////////////////////////

func TestDiscover_RecordsATimestampedListingAsAPI(t *testing.T) {
	// A platform that reported a real start time is trusted at the API
	// level, which is below live and above a tracker.
	started := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	lister := &fakeLister{
		listings: []fetch.Listing{{ID: "v100001", URL: "https://example.com/videos/v100001"}},
		details: map[string]fetch.Listing{
			"https://example.com/videos/v100001": {
				ID: "v100001", Title: "a broadcast", StartedAt: started, Precise: true,
			},
		},
	}
	recorder := &fakeRecorder{}

	written, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, started.Add(time.Hour), farHorizon)
	if err != nil {
		t.Fatalf("Discover() err = %v, want nil", err)
	}
	if written != 1 {
		t.Fatalf("Discover() wrote %d, want 1", written)
	}
	if got := recorder.written[0].Source; got != store.SourceAPI {
		t.Errorf("Source = %q, want %q", got, store.SourceAPI)
	}
	if !recorder.written[0].StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", recorder.written[0].StartedAt, started)
	}
}

func TestDiscover_RecordsADatedListingAsATracker(t *testing.T) {
	// The rule that protects every live recording. A listing carrying only
	// a date has no start time, so storing it at API trust would let it
	// displace one the recorder watched happen and move the broadcast to
	// another day.
	started := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	lister := &fakeLister{
		listings: []fetch.Listing{{ID: "EXAMPLEVID01", URL: "https://example.com/watch/EXAMPLEVID01"}},
		details: map[string]fetch.Listing{
			"https://example.com/watch/EXAMPLEVID01": {
				ID: "EXAMPLEVID01", StartedAt: started, Precise: false,
			},
		},
	}
	recorder := &fakeRecorder{}

	if _, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, started.Add(time.Hour), farHorizon); err != nil {
		t.Fatalf("Discover() err = %v, want nil", err)
	}
	if got := recorder.written[0].Source; got != store.SourceTracker {
		t.Errorf("Source = %q, want %q for a listing with only a date", got, store.SourceTracker)
	}
}

func TestDiscover_CarriesTheStreamIDAndNormalisesTheVideoID(t *testing.T) {
	// The stream id is what reunites an archive listing with the row the
	// recorder opened live. The video id has two spellings across the two
	// sources, and leaving both in play mints a prefixed twin of every row
	// the other source already wrote.
	started := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	lister := &fakeLister{
		listings: []fetch.Listing{{ID: "v2847353784", URL: "https://example.com/videos/v2847353784"}},
		details: map[string]fetch.Listing{
			"https://example.com/videos/v2847353784": {
				ID: "v2847353784", StreamID: "48211557693", StartedAt: started, Precise: true,
			},
		},
	}
	recorder := &fakeRecorder{}

	if _, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, started.Add(48*time.Hour), farHorizon); err != nil {
		t.Fatalf("Discover() err = %v, want nil", err)
	}
	if len(recorder.written) != 1 {
		t.Fatalf("Discover() wrote %d broadcasts, want 1", len(recorder.written))
	}
	if got := recorder.written[0].StreamID; got != "48211557693" {
		t.Errorf("StreamID = %q, want the live session the listing named", got)
	}
	if got := recorder.written[0].RemoteID; got != "2847353784" {
		t.Errorf("RemoteID = %q, want the leading v stripped", got)
	}
}

func TestNormalizeID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "the yt-dlp spelling", id: "v2847353784", want: "2847353784"},
		{name: "the helix spelling", id: "2847353784", want: "2847353784"},
		{name: "a name that merely starts with v", id: "vODeo42x", want: "vODeo42x"},
		{name: "a bare v", id: "v", want: "v"},
		{name: "no identifier at all", id: "", want: ""},
		{name: "another platform's identifier", id: "EXAMPLEVID01", want: "EXAMPLEVID01"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeID(tt.id); got != tt.want {
				t.Errorf("normalizeID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestDiscover_RecordsTheEndFromTheDuration(t *testing.T) {
	// Both listers already report a duration and discovery threw it away, so
	// every stored broadcast read as still running and nothing that waits on
	// an end ever ran.
	started := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		duration time.Duration
		isLive   bool
		want     *time.Time
	}{
		{
			name:     "a reported duration becomes the end",
			duration: 4*time.Hour + 12*time.Minute,
			want:     new(started.Add(4*time.Hour + 12*time.Minute)),
		},
		{
			name:     "an unreported duration leaves the end unknown",
			duration: 0,
			want:     nil,
		},
		{
			// A broadcast still running reports a length that grows between
			// calls. Reading an end from it settles the broadcast against a
			// copy the platform is still writing, which releases the fetch
			// planner and the patcher onto an incomplete archive.
			name:     "a broadcast still running has no end yet",
			duration: 90 * time.Minute,
			isLive:   true,
			want:     nil,
		},
		{
			// The same copy, from a source that reports no liveness at all.
			// A platform API describing a video need not carry the flag, so
			// the end landing at now is the only thing left to notice it by.
			// Discover runs at started+6h, so a length of 6h ends at now.
			name:     "a length reaching up to now has no end yet",
			duration: 6 * time.Hour,
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lister := &fakeLister{
				listings: []fetch.Listing{{ID: "v100001", URL: "https://example.com/videos/v100001"}},
				details: map[string]fetch.Listing{
					"https://example.com/videos/v100001": {
						ID: "v100001", StartedAt: started, Duration: tt.duration,
						IsLive: tt.isLive, Precise: true,
					},
				},
			}
			recorder := &fakeRecorder{}

			if _, err := NewDiscoverer(lister, recorder, quiet()).
				Discover(context.Background(), examplechannel, started.Add(6*time.Hour), farHorizon); err != nil {
				t.Fatalf("Discover() err = %v, want nil", err)
			}
			if len(recorder.written) != 1 {
				t.Fatalf("Discover() wrote %d broadcasts, want 1", len(recorder.written))
			}

			got := recorder.written[0].EndedAt
			switch {
			case tt.want == nil && got != nil:
				t.Errorf("EndedAt = %s, want nil with no duration reported", got)
			case tt.want != nil && got == nil:
				t.Errorf("EndedAt = nil, want %s", tt.want)
			case tt.want != nil && !got.Equal(*tt.want):
				t.Errorf("EndedAt = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestDiscover_DropsAListingWithNoStartTime(t *testing.T) {
	// Storing it at the time of the scan would put the broadcast on
	// whatever day the daemon happened to look, for good.
	lister := &fakeLister{
		listings: []fetch.Listing{{ID: "v100001", URL: "https://example.com/videos/v100001"}},
		details: map[string]fetch.Listing{
			"https://example.com/videos/v100001": {ID: "v100001", Title: "a broadcast"},
		},
	}
	recorder := &fakeRecorder{}

	written, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, time.Now(), farHorizon)
	if err != nil {
		t.Fatalf("Discover() err = %v, want nil", err)
	}
	if written != 0 || len(recorder.written) != 0 {
		t.Errorf("Discover() wrote %d broadcasts, want none without a start time", len(recorder.written))
	}
}

// ///////////////////////////////////////////////
// Rate limiting
// ///////////////////////////////////////////////

func TestDiscover_ListsEveryTimeItIsCalled(t *testing.T) {
	// Nothing polls this. A pass runs because the operator asked for one,
	// and a second run that served the first run's listing would report
	// nothing found for a broadcast that ended in between, with nothing on
	// screen saying why.
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	started := now.Add(-24 * time.Hour)
	lister := &fakeLister{
		listings: []fetch.Listing{{ID: "v100001", URL: "https://example.com/videos/v100001"}},
		details: map[string]fetch.Listing{
			"https://example.com/videos/v100001": {ID: "v100001", StartedAt: started, Precise: true},
		},
	}
	recorder := &fakeRecorder{}
	discoverer := NewDiscoverer(lister, recorder, quiet())

	for _, run := range []string{"first", "second"} {
		t.Run(run, func(t *testing.T) {
			before := lister.listCall
			if _, err := discoverer.Discover(context.Background(),
				examplechannel, now, farHorizon); err != nil {
				t.Fatalf("Discover() err = %v, want nil", err)
			}
			if lister.listCall == before {
				t.Error("Discover() served an earlier listing rather than asking again")
			}
		})
	}
}

// ///////////////////////////////////////////////
// Resilience
// ///////////////////////////////////////////////

func TestDiscover_KeepsGoingPastOneUndescribableBroadcast(t *testing.T) {
	// One removed video in a channel's history must not cost the whole
	// listing behind it.
	started := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	lister := &fakeLister{
		listings: []fetch.Listing{
			{ID: "gone", URL: "https://example.com/videos/gone"},
			{ID: "v100002", URL: "https://example.com/videos/v100002"},
		},
		details: map[string]fetch.Listing{
			"https://example.com/videos/v100002": {ID: "v100002", StartedAt: started, Precise: true},
		},
	}
	recorder := &fakeRecorder{}

	written, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, started.Add(time.Hour), farHorizon)
	if err != nil {
		t.Fatalf("Discover() err = %v, want nil", err)
	}
	if written != 1 {
		t.Errorf("Discover() wrote %d, want the describable one recorded", written)
	}
}

func TestDiscover_BoundsHowManyItDescribes(t *testing.T) {
	// A channel with years of history would otherwise spend one request
	// per broadcast on every pass.
	started := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	lister := &fakeLister{details: map[string]fetch.Listing{}}
	for i := range maxDescribed * 2 {
		url := "https://example.com/videos/" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		lister.listings = append(lister.listings, fetch.Listing{ID: url, URL: url})
		lister.details[url] = fetch.Listing{ID: url, StartedAt: started, Precise: true}
	}
	recorder := &fakeRecorder{}

	if _, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, started.Add(time.Hour), farHorizon); err != nil {
		t.Fatalf("Discover() err = %v, want nil", err)
	}
	if lister.infoCall > maxDescribed {
		t.Errorf("described %d broadcasts, want at most %d", lister.infoCall, maxDescribed)
	}
}

func TestDiscover_StopsWhenTheContextIsCancelled(t *testing.T) {
	// A backfill pass ends when the daemon does, mid-listing or not.
	started := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	lister := &fakeLister{details: map[string]fetch.Listing{}}
	for i := range 5 {
		url := "https://example.com/videos/" + string(rune('a'+i))
		lister.listings = append(lister.listings, fetch.Listing{ID: url, URL: url})
		lister.details[url] = fetch.Listing{ID: url, StartedAt: started, Precise: true}
	}
	recorder := &fakeRecorder{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(ctx, examplechannel, started.Add(time.Hour), farHorizon); !errors.Is(err, context.Canceled) {
		t.Errorf("Discover() err = %v, want context.Canceled", err)
	}
}

// ///////////////////////////////////////////////
// Enrichment
// ///////////////////////////////////////////////

func TestDiscover_PrefersAListerThatDescribesInOneRequest(t *testing.T) {
	// The whole point of the capability. A flat listing plus one lookup per
	// broadcast costs 1 + N, which a channel with years of history spends on
	// every pass.
	started := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	lister := &fakeEnricher{
		described: []fetch.Listing{{ID: "v100001", Title: "a broadcast", StartedAt: started, Precise: true}},
	}
	recorder := &fakeRecorder{}

	written, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, started.Add(time.Hour), farHorizon)
	if err != nil {
		t.Fatalf("Discover() err = %v, want nil", err)
	}
	if written != 1 {
		t.Fatalf("Discover() wrote %d, want 1", written)
	}
	if lister.infoCall != 0 || lister.listCall != 0 {
		t.Errorf("spent %d listings and %d lookups, want neither once one request answered",
			lister.listCall, lister.infoCall)
	}
	if !recorder.written[0].StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", recorder.written[0].StartedAt, started)
	}
}

func TestDiscover_AsksTheEnricherForTheWholeWindow(t *testing.T) {
	// The bound a listing needs is how far back to reach, not how many of
	// the newest to take. A count reaches roughly a week on a channel that
	// streams daily, so a machine off for a fortnight comes back and never
	// learns the older broadcasts happened: no row, so those days read
	// unknown and nothing fetches them while the archive still holds them.
	lister := &fakeEnricher{}
	recorder := &fakeRecorder{}
	horizon := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	if _, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, time.Now(), horizon); err != nil {
		t.Fatalf("Discover() err = %v, want nil", err)
	}
	if !lister.askedFor.Equal(horizon) {
		t.Errorf("asked back to %v, want the pass's own horizon %v", lister.askedFor, horizon)
	}
}

func TestDiscover_StopsDescribingPastTheHorizon(t *testing.T) {
	// The two-step path costs one subprocess per broadcast, so it stops at
	// the first one older than the window rather than walking a channel's
	// whole history.
	inside := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	outside := time.Date(2026, 1, 1, 20, 0, 0, 0, time.UTC)

	lister := &fakeLister{
		listings: []fetch.Listing{
			{ID: "v1", URL: "https://example.com/videos/v1"},
			{ID: "v2", URL: "https://example.com/videos/v2"},
			{ID: "v3", URL: "https://example.com/videos/v3"},
		},
		details: map[string]fetch.Listing{
			"https://example.com/videos/v1": {ID: "v1", StartedAt: inside, Precise: true},
			"https://example.com/videos/v2": {ID: "v2", StartedAt: outside, Precise: true},
			"https://example.com/videos/v3": {ID: "v3", StartedAt: outside, Precise: true},
		},
	}
	recorder := &fakeRecorder{}
	horizon := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	if _, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, inside.Add(time.Hour), horizon); err != nil {
		t.Fatalf("Discover() err = %v, want nil", err)
	}
	if lister.infoCall != 2 {
		t.Errorf("described %d broadcasts, want 2: one inside the window and the one that proved the bound",
			lister.infoCall)
	}
}

func TestDiscover_FallsBackToTheSubprocessWhenOneRequestFails(t *testing.T) {
	// Enrichment is an optimisation, so a site API that is down, rate
	// limiting, or has revoked a credential must cost a slower listing
	// rather than the listing itself.
	started := time.Date(2026, 3, 1, 20, 0, 0, 0, time.UTC)
	lister := &fakeEnricher{describeErr: errors.New("twitch answered 503")}
	lister.listings = []fetch.Listing{{ID: "v100001", URL: "https://example.com/videos/v100001"}}
	lister.details = map[string]fetch.Listing{
		"https://example.com/videos/v100001": {ID: "v100001", StartedAt: started, Precise: true},
	}
	recorder := &fakeRecorder{}

	written, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(context.Background(), examplechannel, started.Add(time.Hour), farHorizon)
	if err != nil {
		t.Fatalf("Discover() err = %v, want the fallback to carry it", err)
	}
	if written != 1 {
		t.Errorf("Discover() wrote %d, want the fallback to have recorded 1", written)
	}
	if lister.listCall != 1 {
		t.Errorf("listed %d times, want the fallback to have run", lister.listCall)
	}
}

func TestDiscover_DoesNotFallBackWhenTheContextIsCancelled(t *testing.T) {
	// A cancelled context is the shutdown. Retrying through a subprocess
	// would spend a process launch to fail the same way more slowly.
	lister := &fakeEnricher{describeErr: context.Canceled}
	recorder := &fakeRecorder{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewDiscoverer(lister, recorder, quiet()).
		Discover(ctx, examplechannel, time.Now(), farHorizon); !errors.Is(err, context.Canceled) {
		t.Errorf("Discover() err = %v, want context.Canceled", err)
	}
	if lister.listCall != 0 {
		t.Errorf("listed %d times after cancellation, want 0", lister.listCall)
	}
}

func TestDiscover_AsksAnEnrichedChannelEveryTime(t *testing.T) {
	// The enriched path answers list and describe in one request. It is
	// still a request the operator asked for, so it is made every time.
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	lister := &fakeEnricher{
		described: []fetch.Listing{{ID: "v100001", StartedAt: now.Add(-24 * time.Hour), Precise: true}},
	}
	discoverer := NewDiscoverer(lister, &fakeRecorder{}, quiet())

	for run := range 2 {
		if _, err := discoverer.Discover(context.Background(),
			examplechannel, now, farHorizon); err != nil {
			t.Fatalf("Discover() run %d err = %v, want nil", run, err)
		}
	}
	if lister.describeCall != 2 {
		t.Errorf("described %d times across two runs, want 2", lister.describeCall)
	}
}
