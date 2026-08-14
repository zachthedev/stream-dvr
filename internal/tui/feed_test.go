package tui

import (
	"strings"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Harness
// ///////////////////////////////////////////////

// fedModel returns a drained model reading from a feed the test controls.
func fedModel(t *testing.T, feed chan FeedEvent) *Model {
	t.Helper()

	return newOptionsModel(t, Options{Library: library(), Feed: feed})
}

// deliver applies one event.
//
// The command it returns is the next read, which blocks until the recorder
// says something more. It is deliberately not run: a test that waited on it
// would wait out the harness timeout for every event it delivered.
func deliver(t *testing.T, model *Model, event FeedEvent) {
	t.Helper()

	if _, cmd := model.Update(feedMsg{event: event, open: true}); cmd == nil {
		t.Fatal("an event did not schedule the next read, so the feed stops after one")
	}
}

// ///////////////////////////////////////////////
// Subscription
// ///////////////////////////////////////////////

func TestModel_FeedShowsWhatTheRecorderReported(t *testing.T) {
	model := fedModel(t, make(chan FeedEvent, 4))

	deliver(t, model, FeedEvent{
		Kind:    "library_full",
		Channel: "twitch/examplechannel",
		Detail:  "at the cap",
		At:      fixedNow,
	})

	view := render(t, model)
	for _, want := range []string{"recorder", "the library is full", "twitch/examplechannel", "21:15"} {
		if !strings.Contains(view, want) {
			t.Errorf("the feed does not show %q:\n%s", want, view)
		}
	}
}

func TestModel_FeedIsAbsentUntilSomethingIsReported(t *testing.T) {
	// An empty pane would take a row from the calendar for nothing.
	model := fedModel(t, make(chan FeedEvent, 4))

	if strings.Contains(render(t, model), "recorder\n") {
		t.Error("the feed pane appears before anything was reported")
	}
}

func TestModel_FeedShowsTheNewestFirst(t *testing.T) {
	model := fedModel(t, make(chan FeedEvent, 8))

	for i, kind := range []string{"recording_started", "failure"} {
		deliver(t, model, FeedEvent{Kind: kind, At: fixedNow.Add(time.Duration(i) * time.Minute)})
	}

	view := render(t, model)
	failed, started := strings.Index(view, "failed"), strings.Index(view, "recording started")
	if failed < 0 || started < 0 {
		t.Fatalf("the feed is missing an event:\n%s", view)
	}
	if failed > started {
		t.Errorf("the feed shows the oldest first:\n%s", view)
	}
}

func TestModel_FeedKeepsOnlyWhatItCanShow(t *testing.T) {
	// A recorder left running for a month would otherwise grow this
	// without limit, and nothing below the last screenful is read.
	model := fedModel(t, make(chan FeedEvent, 4))

	for range feedDepth + 20 {
		deliver(t, model, FeedEvent{Kind: "failure", At: fixedNow})
	}

	if got := len(model.events); got != feedDepth {
		t.Errorf("the feed holds %d events, want at most %d", got, feedDepth)
	}
}

func TestModel_FeedEscapesWhatAStrangerWrote(t *testing.T) {
	// A stream title and a failure message both reach this line, and
	// neither is text this build produced.
	model := fedModel(t, make(chan FeedEvent, 4))

	deliver(t, model, FeedEvent{Kind: "failure", Detail: "went\x1b[2Jaway", At: fixedNow})

	if strings.ContainsRune(model.View().Content, 0x1b) {
		// The view is styled, so escape codes are expected; what must not
		// appear is the one the event carried.
		if strings.Contains(model.View().Content, "\x1b[2J") {
			t.Error("the feed sent a clear-screen sequence to the terminal")
		}
	}
}

func TestModel_FeedStopsWhenTheRecorderDoes(t *testing.T) {
	// What it already said stays on screen. There is just nothing more
	// coming, and the subscription must not spin on a closed channel.
	model := fedModel(t, make(chan FeedEvent, 4))
	deliver(t, model, FeedEvent{Kind: "failure", At: fixedNow})

	_, cmd := model.Update(feedMsg{open: false})

	if cmd != nil {
		t.Error("a closed feed scheduled another read")
	}
	if model.feed != nil {
		t.Error("a closed feed is still subscribed")
	}
	if !strings.Contains(render(t, model), "failed") {
		t.Error("a closed feed dropped what the recorder had already said")
	}
}

func TestModel_FeedWithNoRecorderIsNotSubscribed(t *testing.T) {
	// cmd passes no feed when nothing is running in this window.
	model := newModel(t, library(), nil)

	if cmd := model.watchFeed(); cmd != nil {
		t.Error("watchFeed() scheduled a read with no feed to read")
	}
}

// ///////////////////////////////////////////////
// Rendering
// ///////////////////////////////////////////////

func TestFeedLabel_NamesEveryKind(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{kind: "recording_started", want: "recording started"},
		{kind: "failure", want: "failed"},
		{kind: "library_full", want: "the library is full"},
		{kind: "downtime", want: "the recorder was not running"},
		// A kind added to the recorder still reaches the pane before this
		// list catches up.
		{kind: "something_new", want: "something_new"},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			if got := stripANSI(feedLabel(tc.kind, newStyles(true))); got != tc.want {
				t.Errorf("feedLabel(%q) = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}
