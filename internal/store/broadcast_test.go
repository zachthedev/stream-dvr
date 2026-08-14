package store

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// broadcastStart is a fixed start time for broadcast fixtures.
var broadcastStart = time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

// ///////////////////////////////////////////////
// Read-then-write pairs
// ///////////////////////////////////////////////

func TestUpsertBroadcast_ConcurrentDiscoveriesOfOneSessionCollapseToOneRow(t *testing.T) {
	// A broadcast is discovered live, again from the platform's VOD list,
	// and again from a tracker. Matching and writing apart lets two
	// discoveries both find nothing and both insert, which shows up as
	// broadcasts the channel never made.
	store := newStore(t)
	channel := newChannel(t, store)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, StartedAt: broadcastStart, Source: SourceLive,
			}); err != nil {
				t.Errorf("UpsertBroadcast() err = %v, want nil", err)
			}
		})
	}
	wg.Wait()

	broadcasts, err := store.BroadcastsBetween(channel.ID,
		broadcastStart.Add(-time.Hour), broadcastStart.Add(time.Hour))
	if err != nil {
		t.Fatalf("BroadcastsBetween() err = %v, want nil", err)
	}
	if len(broadcasts) != 1 {
		t.Errorf("8 discoveries of one session produced %d rows, want 1", len(broadcasts))
	}
}

func TestObserveTitle_ConcurrentIdenticalReadingsAppendOnce(t *testing.T) {
	// The poller runs on a fixed interval, so most readings repeat. Reading
	// the last observation and appending apart from it lets identical
	// readings each find no match and each append.
	store := newStore(t)
	channel := newChannel(t, store)
	broadcast := seedBroadcast(t, store, channel.ID, broadcastStart, "one")

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := store.ObserveTitle(broadcast.ID, broadcastStart, "Building a DVR", "Software"); err != nil {
				t.Errorf("ObserveTitle() err = %v, want nil", err)
			}
		})
	}
	wg.Wait()

	history, err := store.TitleHistory(broadcast.ID)
	if err != nil {
		t.Fatalf("TitleHistory() err = %v, want nil", err)
	}
	if len(history) != 1 {
		t.Errorf("16 identical readings produced %d observations, want 1", len(history))
	}
}

// ///////////////////////////////////////////////
// UpsertBroadcast
// ///////////////////////////////////////////////

func TestUpsertBroadcast_Insert(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	got, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID,
		RemoteID:  "stream-1",
		StartedAt: broadcastStart,
		Title:     "Midnight Build Stream",
		Category:  "Just Chatting",
		Source:    SourceLive,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	if got.ID == 0 {
		t.Error("UpsertBroadcast() returned a zero id")
	}
	if got.DiscoveredAt.IsZero() {
		t.Error("DiscoveredAt is zero, want it defaulted")
	}
}

func TestUpsertBroadcast_RejectsUnknownSource(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	_, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID,
		StartedAt: broadcastStart,
		Source:    "rumor",
	})
	if err == nil {
		t.Error("UpsertBroadcast() err = nil, want a rejection for an unknown source")
	}
}

func TestUpsertBroadcast_MatchesByRemoteID(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	first, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, RemoteID: "stream-1",
		StartedAt: broadcastStart, Source: SourceLive,
	})
	if err != nil {
		t.Fatalf("first UpsertBroadcast() err = %v, want nil", err)
	}

	// A VOD listing reports the same broadcast hours later with a start
	// time that drifted well past the overlap window.
	second, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, RemoteID: "stream-1",
		StartedAt: broadcastStart.Add(3 * time.Hour), Source: SourceAPI,
		Title: "recovered title",
	})
	if err != nil {
		t.Fatalf("second UpsertBroadcast() err = %v, want nil", err)
	}
	if second.ID != first.ID {
		t.Errorf("second id = %d, want the same row %d", second.ID, first.ID)
	}
}

func TestUpsertBroadcast_MatchesByOverlappingStart(t *testing.T) {
	tests := []struct {
		name      string
		offset    time.Duration
		wantMerge bool
	}{
		{name: "same minute", offset: 0, wantMerge: true},
		{name: "a few minutes late", offset: 4 * time.Minute, wantMerge: true},
		{name: "a few minutes early", offset: -4 * time.Minute, wantMerge: true},
		{name: "at the window edge", offset: 14 * time.Minute, wantMerge: true},
		{name: "beyond the window", offset: 30 * time.Minute, wantMerge: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)

			first, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, StartedAt: broadcastStart, Source: SourceLive,
			})
			if err != nil {
				t.Fatalf("first UpsertBroadcast() err = %v, want nil", err)
			}

			second, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID,
				StartedAt: broadcastStart.Add(tt.offset),
				Source:    SourceTracker,
			})
			if err != nil {
				t.Fatalf("second UpsertBroadcast() err = %v, want nil", err)
			}

			merged := second.ID == first.ID
			if merged != tt.wantMerge {
				t.Errorf("merged = %t, want %t (offset %s)", merged, tt.wantMerge, tt.offset)
			}
		})
	}
}

func TestUpsertBroadcast_KeepsBroadcastsWithDifferentRemoteIDsApart(t *testing.T) {
	// A channel that drops and comes back inside the overlap window is two
	// broadcasts, and the platform issues a VOD for each. Merging them keeps
	// one title, flips the identity to whichever discovery landed last, and
	// leaves the calendar reporting one broadcast on a day that held two.
	tests := []struct {
		name        string
		firstRemote string
		lastRemote  string
		wantMerge   bool
	}{
		{name: "two identifiers that disagree", firstRemote: "vod-AAA", lastRemote: "vod-BBB", wantMerge: false},
		{name: "the same identifier twice", firstRemote: "vod-AAA", lastRemote: "vod-AAA", wantMerge: true},
		{name: "an identifier claiming an unidentified row", firstRemote: "", lastRemote: "vod-BBB", wantMerge: true},
		{name: "an unidentified discovery of an identified row", firstRemote: "vod-AAA", lastRemote: "", wantMerge: true},
		{name: "neither identified", firstRemote: "", lastRemote: "", wantMerge: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)

			first, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, RemoteID: tt.firstRemote, StartedAt: broadcastStart,
				Title: "part one", Source: SourceAPI,
			})
			if err != nil {
				t.Fatalf("first UpsertBroadcast() err = %v, want nil", err)
			}

			second, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, RemoteID: tt.lastRemote,
				StartedAt: broadcastStart.Add(6 * time.Minute),
				Title:     "part two", Source: SourceAPI,
			})
			if err != nil {
				t.Fatalf("second UpsertBroadcast() err = %v, want nil", err)
			}

			if merged := second.ID == first.ID; merged != tt.wantMerge {
				t.Fatalf("merged = %t, want %t", merged, tt.wantMerge)
			}
			if tt.wantMerge {
				return
			}

			// Two rows is only half of it. The first must still name its own
			// broadcast, or re-ingesting it flips the identity back.
			kept, err := store.Broadcast(first.ID)
			if err != nil {
				t.Fatalf("Broadcast() err = %v, want nil", err)
			}
			if kept.RemoteID != tt.firstRemote {
				t.Errorf("first broadcast remote id = %q, want %q", kept.RemoteID, tt.firstRemote)
			}
			if kept.Title != "part one" {
				t.Errorf("first broadcast title = %q, want %q", kept.Title, "part one")
			}
			if second.Title != "part two" {
				t.Errorf("second broadcast title = %q, want %q", second.Title, "part two")
			}
		})
	}
}

func TestUpsertBroadcast_MergesALiveRowWithItsVodRow(t *testing.T) {
	// The live poller learns the stream id and the VOD listing learns the
	// video id. They are different namespaces and never collide, so without
	// a column of its own for the stream the two discoveries of one
	// broadcast become two rows, and the second holds no recording.
	store := newStore(t)
	channel := newChannel(t, store)

	live, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, StreamID: "48211557693",
		StartedAt: broadcastStart, Title: "live title", Source: SourceLive,
	})
	if err != nil {
		t.Fatalf("live UpsertBroadcast() err = %v, want nil", err)
	}
	if live.RemoteID != "" {
		t.Errorf("RemoteID = %q, want the live row to carry no video id", live.RemoteID)
	}

	// Twitch's Get Videos answers with the same stream id the recorder saw,
	// alongside the video id the download is addressed by.
	vod, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, StreamID: "48211557693", RemoteID: "2847353784",
		StartedAt: broadcastStart.Add(90 * time.Second), Source: SourceAPI,
	})
	if err != nil {
		t.Fatalf("vod UpsertBroadcast() err = %v, want nil", err)
	}
	if vod.ID != live.ID {
		t.Fatalf("vod id = %d, want the live row %d", vod.ID, live.ID)
	}
	if vod.StreamID != "48211557693" || vod.RemoteID != "2847353784" {
		t.Errorf("merged row = stream %q video %q, want both ids held by one row",
			vod.StreamID, vod.RemoteID)
	}

	broadcasts, err := store.BroadcastsBetween(channel.ID,
		broadcastStart.Add(-time.Hour), broadcastStart.Add(time.Hour))
	if err != nil {
		t.Fatalf("BroadcastsBetween() err = %v, want nil", err)
	}
	if len(broadcasts) != 1 {
		t.Errorf("a live row and its VOD row produced %d rows, want 1", len(broadcasts))
	}
}

func TestUpsertBroadcast_KeepsTwoRealBroadcastsApart(t *testing.T) {
	// A channel that drops and restarts inside the overlap window makes two
	// broadcasts under two stream ids. Merging them reports one broadcast on
	// a day that held two.
	tests := []struct {
		name        string
		firstStream string
		lastStream  string
		wantMerge   bool
	}{
		{name: "two stream ids that disagree", firstStream: "48211557693", lastStream: "48211557694", wantMerge: false},
		{name: "the same stream id twice", firstStream: "48211557693", lastStream: "48211557693", wantMerge: true},
		{name: "a stream id claiming a row with none", firstStream: "", lastStream: "48211557694", wantMerge: true},
		{name: "a discovery with no stream id", firstStream: "48211557693", lastStream: "", wantMerge: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)

			first, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, StreamID: tt.firstStream,
				StartedAt: broadcastStart, Title: "part one", Source: SourceLive,
			})
			if err != nil {
				t.Fatalf("first UpsertBroadcast() err = %v, want nil", err)
			}

			second, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, StreamID: tt.lastStream,
				StartedAt: broadcastStart.Add(6 * time.Minute),
				Title:     "part two", Source: SourceLive,
			})
			if err != nil {
				t.Fatalf("second UpsertBroadcast() err = %v, want nil", err)
			}

			if merged := second.ID == first.ID; merged != tt.wantMerge {
				t.Fatalf("merged = %t, want %t", merged, tt.wantMerge)
			}
			if tt.wantMerge {
				return
			}
			kept, err := store.Broadcast(first.ID)
			if err != nil {
				t.Fatalf("Broadcast() err = %v, want nil", err)
			}
			if kept.StreamID != tt.firstStream {
				t.Errorf("first broadcast stream id = %q, want %q", kept.StreamID, tt.firstStream)
			}
		})
	}
}

func TestUpsertBroadcast_TakesAnEarlierPreciseStartOverALiveOne(t *testing.T) {
	// The rule is directional, and that is what makes it safe. A daemon
	// cannot observe a broadcast before it begins, so a live-observed start
	// is always at or after the true one. An earlier precise start is
	// therefore strictly better information about the same broadcast, and
	// refusing it loses the hours before the daemon joined. A later one
	// proves nothing and is still refused.
	tests := []struct {
		name      string
		source    Source
		offset    time.Duration
		wantStart time.Duration
	}{
		{
			name:      "an earlier precise start corrects a live one",
			source:    SourceAPI,
			offset:    -2 * time.Hour,
			wantStart: -2 * time.Hour,
		},
		{
			name:      "a later precise start does not move a live one",
			source:    SourceAPI,
			offset:    3 * time.Minute,
			wantStart: 0,
		},
		{
			name:      "a tracker never moves a live one, however early",
			source:    SourceTracker,
			offset:    -2 * time.Hour,
			wantStart: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)

			if _, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, StreamID: "48211557693",
				StartedAt: broadcastStart, Title: "live title", Source: SourceLive,
			}); err != nil {
				t.Fatalf("live UpsertBroadcast() err = %v, want nil", err)
			}

			got, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, StreamID: "48211557693",
				StartedAt: broadcastStart.Add(tt.offset), Title: "listed title", Source: tt.source,
			})
			if err != nil {
				t.Fatalf("second UpsertBroadcast() err = %v, want nil", err)
			}

			want := broadcastStart.Add(tt.wantStart)
			if !got.StartedAt.Equal(want) {
				t.Errorf("StartedAt = %s, want %s", got.StartedAt, want)
			}
			// The row is still one the recorder watched happen, and every
			// later discovery must keep being judged against that.
			if got.Source != SourceLive {
				t.Errorf("Source = %q, want it to stay %q", got.Source, SourceLive)
			}
			if got.Title != "live title" {
				t.Errorf("Title = %q, want the live title to stand", got.Title)
			}
		})
	}
}

func TestUpsertBroadcast_MatchesNearTheStorableBounds(t *testing.T) {
	// Adding the overlap window to a timestamp near either bound wraps, and
	// a low bound above the high one matches nothing, so deduplication
	// switches itself off without saying so.
	tests := []struct {
		name string
		at   time.Time
	}{
		{name: "at the earliest storable instant", at: minStorable},
		{name: "at the latest storable instant", at: maxStorable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)

			first, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, StartedAt: tt.at, Source: SourceLive,
			})
			if err != nil {
				t.Fatalf("first UpsertBroadcast() err = %v, want nil", err)
			}
			second, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, StartedAt: tt.at, Source: SourceTracker,
			})
			if err != nil {
				t.Fatalf("second UpsertBroadcast() err = %v, want nil", err)
			}
			if second.ID != first.ID {
				t.Errorf("second upsert id = %d, want %d; the same instant must not become two rows",
					second.ID, first.ID)
			}
		})
	}
}

func TestUpsertBroadcast_SourcePrecedence(t *testing.T) {
	t.Run("a better source upgrades timing and metadata", func(t *testing.T) {
		store := newStore(t)
		channel := newChannel(t, store)

		// A tracker sees the broadcast first, rounded to the minute.
		rough := broadcastStart.Add(3 * time.Minute)
		if _, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: rough,
			Title: "tracker title", Source: SourceTracker,
		}); err != nil {
			t.Fatalf("tracker UpsertBroadcast() err = %v, want nil", err)
		}

		got, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: broadcastStart,
			Title: "live title", Source: SourceLive,
		})
		if err != nil {
			t.Fatalf("live UpsertBroadcast() err = %v, want nil", err)
		}
		if got.Source != SourceLive {
			t.Errorf("Source = %q, want %q", got.Source, SourceLive)
		}
		if !got.StartedAt.Equal(broadcastStart) {
			t.Errorf("StartedAt = %s, want the live time %s", got.StartedAt, broadcastStart)
		}
		if got.Title != "live title" {
			t.Errorf("Title = %q, want the live title", got.Title)
		}
	})

	t.Run("a worse source does not overwrite", func(t *testing.T) {
		store := newStore(t)
		channel := newChannel(t, store)

		if _, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: broadcastStart,
			Title: "live title", Source: SourceLive,
		}); err != nil {
			t.Fatalf("live UpsertBroadcast() err = %v, want nil", err)
		}

		got, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: broadcastStart.Add(2 * time.Minute),
			Title: "tracker title", Source: SourceTracker,
		})
		if err != nil {
			t.Fatalf("tracker UpsertBroadcast() err = %v, want nil", err)
		}
		if got.Source != SourceLive {
			t.Errorf("Source = %q, want it to stay %q", got.Source, SourceLive)
		}
		if !got.StartedAt.Equal(broadcastStart) {
			t.Errorf("StartedAt = %s, want the live time to stand", got.StartedAt)
		}
		if got.Title != "live title" {
			t.Errorf("Title = %q, want the live title to stand", got.Title)
		}
	})

	t.Run("a worse source still fills blanks", func(t *testing.T) {
		// Live capture knows when a stream started but often not its
		// category, and a tracker fills that in without displacing
		// anything.
		store := newStore(t)
		channel := newChannel(t, store)

		if _, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: broadcastStart,
			Title: "live title", Source: SourceLive,
		}); err != nil {
			t.Fatalf("live UpsertBroadcast() err = %v, want nil", err)
		}

		ended := broadcastStart.Add(4 * time.Hour)
		got, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: broadcastStart,
			Category: "Just Chatting", EndedAt: &ended, Source: SourceTracker,
		})
		if err != nil {
			t.Fatalf("tracker UpsertBroadcast() err = %v, want nil", err)
		}
		if got.Category != "Just Chatting" {
			t.Errorf("Category = %q, want the tracker to fill the blank", got.Category)
		}
		if got.EndedAt == nil || !got.EndedAt.Equal(ended) {
			t.Errorf("EndedAt = %v, want the tracker to fill the blank", got.EndedAt)
		}
	})

	t.Run("a remote id is adopted from any source", func(t *testing.T) {
		store := newStore(t)
		channel := newChannel(t, store)

		if _, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: broadcastStart, Source: SourceLive,
		}); err != nil {
			t.Fatalf("live UpsertBroadcast() err = %v, want nil", err)
		}

		got, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: broadcastStart,
			RemoteID: "vod-77", Source: SourceAPI,
		})
		if err != nil {
			t.Fatalf("api UpsertBroadcast() err = %v, want nil", err)
		}
		if got.RemoteID != "vod-77" {
			t.Errorf("RemoteID = %q, want %q", got.RemoteID, "vod-77")
		}
	})
}

func TestUpsertBroadcast_KeepsAnUnansweredMuteQuestionApartFromAnAnswerOfNone(t *testing.T) {
	// A machine with no platform session cannot ask what the platform
	// silenced, and that is not the same as a platform saying it silenced
	// nothing. Only the second licenses filling a hole from the stored copy,
	// so the two must survive a round trip apart.
	tests := []struct {
		name  string
		muted []MutedSpan
	}{
		{name: "nobody could ask", muted: nil},
		{name: "the platform silenced nothing", muted: []MutedSpan{}},
		{
			name: "two silenced stretches",
			muted: []MutedSpan{
				{Offset: 2 * time.Minute, Duration: 30 * time.Second},
				{Offset: 2 * time.Hour, Duration: 3 * time.Minute},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)

			written, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, RemoteID: "v100001",
				StartedAt: broadcastStart, Source: SourceAPI, Muted: tt.muted,
			})
			if err != nil {
				t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
			}

			got, err := store.Broadcast(written.ID)
			if err != nil {
				t.Fatalf("Broadcast() err = %v, want nil", err)
			}
			if (got.Muted == nil) != (tt.muted == nil) {
				t.Fatalf("Muted = %#v, want the answered state of %#v", got.Muted, tt.muted)
			}
			if !slices.Equal(got.Muted, tt.muted) {
				t.Errorf("Muted = %+v, want %+v", got.Muted, tt.muted)
			}
		})
	}
}

func TestBroadcast_RefusesASilencedStretchTheArithmeticCannotHold(t *testing.T) {
	// A span whose end wraps negative reads as covering nothing, so every
	// guard against patching silenced audio passes and the hole is filled
	// from the copy that serves silence, permanently. The column is written
	// by whatever build was running at the time, so the read is where this
	// has to hold.
	//
	// Refused rather than dropped. An empty list is a positive answer here,
	// that the platform was asked and silenced nothing, and it is the only
	// answer that licenses a patch. Thinning a list of unreadable spans
	// down to nothing manufactures exactly that licence, so the case where
	// none of them survives is the one that matters.
	tests := []struct {
		name   string
		stored string
	}{
		{
			name:   "a negative offset",
			stored: `[{"offset_ms":-1,"duration_ms":1000}]`,
		},
		{
			name:   "a negative duration",
			stored: `[{"offset_ms":120000,"duration_ms":-1000}]`,
		},
		{
			name:   "an offset whose nanoseconds overflow",
			stored: `[{"offset_ms":9223372036854,"duration_ms":1000}]`,
		},
		{
			name: "one unreadable span beside a good one",
			stored: `[{"offset_ms":-1,"duration_ms":1000},` +
				`{"offset_ms":120000,"duration_ms":30000}]`,
		},
		{
			name: "every span unreadable, which would otherwise read as silenced nothing",
			stored: `[{"offset_ms":-1,"duration_ms":1000},` +
				`{"offset_ms":120000,"duration_ms":-1000},` +
				`{"offset_ms":9223372036854,"duration_ms":1000}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)

			written, err := store.UpsertBroadcast(Broadcast{
				ChannelID: channel.ID, RemoteID: "v100001",
				StartedAt: broadcastStart, Source: SourceAPI,
				Muted: []MutedSpan{{Offset: 2 * time.Minute, Duration: 30 * time.Second}},
			})
			if err != nil {
				t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
			}

			// Written past the column the way an earlier build could have,
			// since a stored row is not re-validated by the writer that
			// put it there.
			if _, err := store.db.Exec(
				`UPDATE broadcasts SET muted_spans = ? WHERE id = ?`,
				tt.stored, written.ID); err != nil {
				t.Fatalf("writing the spans: %v", err)
			}

			if _, err := store.Broadcast(written.ID); err == nil {
				t.Error("Broadcast() read a row holding a span it cannot represent, want a refusal")
			}
		})
	}
}

func TestUpsertBroadcast_RefusesASilencedStretchItCannotStore(t *testing.T) {
	// The write is where a caller still exists to tell. A span accepted
	// here that the read refuses is a broadcast that cannot be read back
	// at all.
	store := newStore(t)
	channel := newChannel(t, store)

	_, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, RemoteID: "v100002",
		StartedAt: broadcastStart, Source: SourceAPI,
		Muted: []MutedSpan{{Offset: -time.Second, Duration: 30 * time.Second}},
	})
	if err == nil {
		t.Error("UpsertBroadcast() stored a span it cannot read back, want a refusal")
	}
}

func TestUpsertBroadcast_DoesNotEraseAMuteAnswerWithSilenceFromASourceThatCannotAsk(t *testing.T) {
	// A tool with no such field says nothing about what is muted, and letting
	// it clear the column would make a hole the platform silenced look
	// patchable again on the next pass.
	store := newStore(t)
	channel := newChannel(t, store)

	answered := []MutedSpan{{Offset: time.Minute, Duration: 30 * time.Second}}
	if _, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, RemoteID: "v100001",
		StartedAt: broadcastStart, Source: SourceAPI, Muted: answered,
	}); err != nil {
		t.Fatalf("first UpsertBroadcast() err = %v, want nil", err)
	}

	got, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, RemoteID: "v100001",
		StartedAt: broadcastStart, Source: SourceAPI, Muted: nil,
	})
	if err != nil {
		t.Fatalf("second UpsertBroadcast() err = %v, want nil", err)
	}
	if !slices.Equal(got.Muted, answered) {
		t.Errorf("Muted = %+v, want the answer to stand: %+v", got.Muted, answered)
	}
}

// ///////////////////////////////////////////////
// SetBroadcastEnd
// ///////////////////////////////////////////////

func TestSetBroadcastEnd(t *testing.T) {
	// The end is what the settle rule, gap detection and the calendar all
	// wait for. An end before the start is read by each of them as a length,
	// so it is refused rather than stored.
	tests := []struct {
		name    string
		offset  time.Duration
		wantErr bool
	}{
		{name: "an end after the start", offset: 4 * time.Hour, wantErr: false},
		{name: "an end at the start", offset: 0, wantErr: false},
		{name: "an end before the start", offset: -time.Minute, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)
			broadcast := seedBroadcast(t, store, channel.ID, broadcastStart, "stream-1")

			ended := broadcastStart.Add(tt.offset)
			err := store.SetBroadcastEnd(broadcast.ID, ended)
			if tt.wantErr {
				if err == nil {
					t.Fatal("SetBroadcastEnd() err = nil, want a refusal for an end before the start")
				}
				return
			}
			if err != nil {
				t.Fatalf("SetBroadcastEnd() err = %v, want nil", err)
			}

			got, err := store.Broadcast(broadcast.ID)
			if err != nil {
				t.Fatalf("Broadcast() err = %v, want nil", err)
			}
			if got.EndedAt == nil || !got.EndedAt.Equal(ended) {
				t.Errorf("EndedAt = %v, want %s", got.EndedAt, ended)
			}
		})
	}
}

func TestSetBroadcastEnd_UnknownBroadcast(t *testing.T) {
	store := newStore(t)

	if err := store.SetBroadcastEnd(999, broadcastStart); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetBroadcastEnd() err = %v, want it to wrap ErrNotFound", err)
	}
}

// ///////////////////////////////////////////////
// Queries
// ///////////////////////////////////////////////

func TestBroadcast_NotFound(t *testing.T) {
	store := newStore(t)

	if _, err := store.Broadcast(999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Broadcast() err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestBroadcastsBetween(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	day := func(d int) time.Time { return time.Date(2026, 3, d, 12, 0, 0, 0, time.UTC) }
	for _, d := range []int{10, 11, 12, 13} {
		if _, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: day(d), Source: SourceLive,
			RemoteID: time.Month(d).String(),
		}); err != nil {
			t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
		}
	}

	got, err := store.BroadcastsBetween(channel.ID, day(11), day(13))
	if err != nil {
		t.Fatalf("BroadcastsBetween() err = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("BroadcastsBetween() returned %d, want 2 (from inclusive, to exclusive)", len(got))
	}
	if !got[0].StartedAt.Before(got[1].StartedAt) {
		t.Error("BroadcastsBetween() is not ordered oldest first")
	}
}

// ///////////////////////////////////////////////
// Title history
// ///////////////////////////////////////////////

func TestObserveTitle(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	broadcast, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, StartedAt: broadcastStart, Source: SourceLive,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}

	// The poller runs on a fixed interval, so most readings repeat. Only
	// changes are worth storing, or the history records how often the poller
	// asked rather than what the broadcaster did.
	observations := []struct {
		offset   time.Duration
		title    string
		category string
	}{
		{0, "starting soon", "Just Chatting"},
		{1 * time.Minute, "starting soon", "Just Chatting"},
		{2 * time.Minute, "starting soon", "Just Chatting"},
		{3 * time.Minute, "Midnight Build Stream", "Just Chatting"},
		{4 * time.Minute, "Midnight Build Stream", "Software and Game Development"},
	}
	for _, o := range observations {
		if err := store.ObserveTitle(broadcast.ID, broadcastStart.Add(o.offset), o.title, o.category); err != nil {
			t.Fatalf("ObserveTitle() err = %v, want nil", err)
		}
	}

	history, err := store.TitleHistory(broadcast.ID)
	if err != nil {
		t.Fatalf("TitleHistory() err = %v, want nil", err)
	}
	if len(history) != 3 {
		t.Fatalf("TitleHistory() returned %d entries, want 3 changes out of 5 readings", len(history))
	}
	if history[0].Title != "starting soon" {
		t.Errorf("history[0].Title = %q, want %q", history[0].Title, "starting soon")
	}
	if history[2].Category != "Software and Game Development" {
		t.Errorf("history[2].Category = %q, want %q", history[2].Category, "Software and Game Development")
	}
	for i := 1; i < len(history); i++ {
		if history[i-1].ObservedAt.After(history[i].ObservedAt) {
			t.Errorf("TitleHistory() is not ordered oldest first at %d", i)
		}
	}
}

func TestObserveTitle_DedupesAgainstThePrecedingObservation(t *testing.T) {
	// Backfill feeds history out of order. Comparing against the newest
	// reading on record rather than the one this observation follows stores
	// a repeat next to its own twin.
	store := newStore(t)
	channel := newChannel(t, store)
	broadcast := seedBroadcast(t, store, channel.ID, broadcastStart, "b-1")

	observations := []struct {
		at    time.Time
		title string
	}{
		{at: broadcastStart, title: "first"},
		{at: broadcastStart.Add(time.Hour), title: "second"},
		// Arrives late and belongs between the two. The reading before it
		// is "first", so it adds nothing.
		{at: broadcastStart.Add(30 * time.Minute), title: "first"},
	}
	for _, observation := range observations {
		if err := store.ObserveTitle(broadcast.ID, observation.at, observation.title, ""); err != nil {
			t.Fatalf("ObserveTitle() err = %v, want nil", err)
		}
	}

	history, err := store.TitleHistory(broadcast.ID)
	if err != nil {
		t.Fatalf("TitleHistory() err = %v, want nil", err)
	}

	want := []string{"first", "second"}
	got := make([]string, 0, len(history))
	for _, observation := range history {
		got = append(got, observation.Title)
	}
	if !slices.Equal(got, want) {
		t.Errorf("TitleHistory() = %v, want %v", got, want)
	}
}

func TestTitleHistory_Empty(t *testing.T) {
	store := newStore(t)

	got, err := store.TitleHistory(999)
	if err != nil {
		t.Fatalf("TitleHistory() err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("TitleHistory() returned %d entries, want none", len(got))
	}
}
