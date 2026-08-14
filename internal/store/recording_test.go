package store

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// newRecording creates a capturing recording for a channel.
func newRecording(t *testing.T, store *Store, channelID int64, path string) Recording {
	t.Helper()

	recording, err := store.CreateRecording(Recording{
		ChannelID: channelID,
		Path:      path,
		State:     StateCapturing,
		Origin:    OriginLive,
		StartedAt: broadcastStart,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	return recording
}

// ///////////////////////////////////////////////
// Unreadable rows
// ///////////////////////////////////////////////

func TestRecordingsByState_SkipsAnUnreadableRowAndReportsIt(t *testing.T) {
	// One row nothing can read must not blank the query. The sweep that
	// retries pending work reads exactly this list, and an empty one tells
	// it there is nothing left to retry.
	var log bytes.Buffer
	store := newStore(t).WithLogger(slog.New(slog.NewTextHandler(&log, nil)))
	channel := newChannel(t, store)

	for _, path := range []string{"first.ts", "second.ts"} {
		if _, err := store.CreateRecording(Recording{
			ChannelID: channel.ID, Path: path, State: StateAwaitingFile,
			Origin: OriginLive, StartedAt: broadcastStart,
		}); err != nil {
			t.Fatalf("CreateRecording() err = %v, want nil", err)
		}
	}

	// SQLite stores what it is given, so a timestamp column can hold text
	// no scan will accept. A hand-edited database is how this arrives.
	if _, err := store.db.Exec(`
		INSERT INTO recordings
			(channel_id, path, state, origin, started_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'not a timestamp', ?, ?)`,
		channel.ID, "broken.ts", string(StateAwaitingFile), string(OriginLive),
		encodeTime(broadcastStart), encodeTime(broadcastStart)); err != nil {
		t.Fatalf("seeding an unreadable row err = %v, want nil", err)
	}

	recordings, err := store.RecordingsByState(StateAwaitingFile)
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}
	if len(recordings) != 2 {
		t.Errorf("RecordingsByState() returned %d recordings, want the 2 readable ones", len(recordings))
	}
	// Skipping quietly would turn a damaged row into a recording that
	// simply stopped existing, so the skip has to say so.
	if !strings.Contains(log.String(), "skipping unreadable recording row") {
		t.Errorf("the skipped row went unreported, log = %q", log.String())
	}
	// An operator told a row was skipped needs to be able to find it.
	if !strings.Contains(log.String(), "recording_id=3") {
		t.Errorf("the report does not name the row, log = %q", log.String())
	}
}

func TestTotalBytes_CountsOnlyTheRowsTheListingShows(t *testing.T) {
	// Summing in SQL counts a row no scanner can read, so the space budget
	// and the list of recordings the operator is shown disagree by exactly
	// the rows neither of them can explain.
	store := newStore(t)
	channel := newChannel(t, store)

	readable, err := store.CreateRecording(Recording{
		ChannelID: channel.ID, Path: "readable.mkv", State: StateComplete,
		Origin: OriginLive, StartedAt: broadcastStart, Bytes: 1000,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	unreadable, err := store.CreateRecording(Recording{
		ChannelID: channel.ID, Path: "unreadable.mkv", State: StateComplete,
		Origin: OriginLive, StartedAt: broadcastStart, Bytes: 500,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	breakRecording(t, store, unreadable.ID)

	listed, err := store.RecordingsForChannel(channel.ID,
		broadcastStart.Add(-time.Hour), broadcastStart.Add(time.Hour))
	if err != nil {
		t.Fatalf("RecordingsForChannel() err = %v, want nil", err)
	}
	if len(listed) != 1 || listed[0].ID != readable.ID {
		t.Fatalf("RecordingsForChannel() returned %d recordings, want only the readable one", len(listed))
	}

	total, err := store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes() err = %v, want nil", err)
	}
	if total != 1000 {
		t.Errorf("TotalBytes() = %d, want 1000 to match the %d bytes the listing accounts for", total, 1000)
	}
}

func TestFinishRecording_RejectsWhatWouldStrandOrMisdescribeTheFile(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		bytes    int64
		duration time.Duration
		endedAt  time.Time
	}{
		{
			// An empty path erases the only link between the row and the
			// file, and a second one collides on the UNIQUE constraint.
			name: "an empty path", path: "", bytes: 1000,
			duration: time.Hour, endedAt: broadcastStart.Add(time.Hour),
		},
		{
			name: "negative bytes", path: "done.mkv", bytes: -1,
			duration: time.Hour, endedAt: broadcastStart.Add(time.Hour),
		},
		{
			name: "a negative duration", path: "done.mkv", bytes: 1000,
			duration: -time.Hour, endedAt: broadcastStart.Add(time.Hour),
		},
		{
			name: "an end before its start", path: "done.mkv", bytes: 1000,
			duration: time.Hour, endedAt: broadcastStart.Add(-time.Minute),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)
			recording := newRecording(t, store, channel.ID, "capture.ts")

			if err := store.FinishRecording(
				recording.ID, StateComplete, tt.path, tt.bytes, tt.duration, tt.endedAt); err == nil {
				t.Fatal("FinishRecording() err = nil, want a refusal")
			}

			// The row must be left as it was, not half updated.
			after, err := store.Recording(recording.ID)
			if err != nil {
				t.Fatalf("Recording() err = %v, want nil", err)
			}
			if after.Path != "capture.ts" || after.State != StateCapturing {
				t.Errorf("recording = {path %q, state %q}, want it untouched at {%q, %q}",
					after.Path, after.State, "capture.ts", StateCapturing)
			}
		})
	}
}

func TestRecordingsByState_RejectsAStateItDoesNotKnow(t *testing.T) {
	// The sweep asks for the states a recording can be stuck in. A typo
	// there returns an empty list, which reads as no work pending.
	store := newStore(t)

	if _, err := store.RecordingsByState(StateAwaitingFile, State("awating_file")); err == nil {
		t.Error("RecordingsByState() err = nil, want a refusal for an unknown state")
	}
}

// ///////////////////////////////////////////////
// CreateRecording
// ///////////////////////////////////////////////

func TestCreateRecording(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	got := newRecording(t, store, channel.ID, "incoming/twitch-examplechannel-1.ts")
	if got.ID == 0 {
		t.Error("CreateRecording() returned a zero id")
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("CreateRecording() left timestamps zero")
	}
}

func TestCreateRecording_Rejects(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	tests := []struct {
		name      string
		recording Recording
	}{
		{
			name:      "unknown state",
			recording: Recording{ChannelID: channel.ID, Path: "a.ts", State: "melting", Origin: OriginLive},
		},
		{
			name:      "unknown origin",
			recording: Recording{ChannelID: channel.ID, Path: "a.ts", State: StateCapturing, Origin: "telepathy"},
		},
		{
			name:      "empty path",
			recording: Recording{ChannelID: channel.ID, Path: "", State: StateCapturing, Origin: OriginLive},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.CreateRecording(tt.recording); err == nil {
				t.Error("CreateRecording() err = nil, want a rejection")
			}
		})
	}
}

func TestCreateRecording_PathIsUnique(t *testing.T) {
	// Two rows claiming one file would let a purge delete bytes another
	// row still believes it owns.
	store := newStore(t)
	channel := newChannel(t, store)
	newRecording(t, store, channel.ID, "incoming/same.ts")

	_, err := store.CreateRecording(Recording{
		ChannelID: channel.ID, Path: "incoming/same.ts",
		State: StateCapturing, Origin: OriginLive, StartedAt: broadcastStart,
	})
	if err == nil {
		t.Error("CreateRecording() with a duplicate path err = nil, want a uniqueness violation")
	}
}

// ///////////////////////////////////////////////
// Lifecycle
// ///////////////////////////////////////////////

func TestFinishRecording(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	recording := newRecording(t, store, channel.ID, "incoming/twitch-examplechannel-1.ts")

	ended := broadcastStart.Add(4*time.Hour + 36*time.Minute)
	final := "ExampleChannel/2026/ExampleChannel - 2026-03-04 21-15 - title.mkv"
	if err := store.FinishRecording(recording.ID, StateComplete, final, 13_450_000_000, 4*time.Hour, ended); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}

	got, err := store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if got.State != StateComplete {
		t.Errorf("State = %q, want %q", got.State, StateComplete)
	}
	if got.Path != final {
		t.Errorf("Path = %q, want %q", got.Path, final)
	}
	if got.Bytes != 13_450_000_000 {
		t.Errorf("Bytes = %d, want 13450000000", got.Bytes)
	}
	if got.Duration != 4*time.Hour {
		t.Errorf("Duration = %s, want %s", got.Duration, 4*time.Hour)
	}
	if got.EndedAt == nil || !got.EndedAt.Equal(ended) {
		t.Errorf("EndedAt = %v, want %s", got.EndedAt, ended)
	}
}

func TestStorablePath_StoresOneSeparatorWhateverTheHostUses(t *testing.T) {
	// A row is read back on whatever host opens the library, and the two
	// spellings of one relative path are not the same string. Every writer
	// hands its path through filepath.Join, so on Windows the value arriving
	// here carries backslashes and the value a Linux host wrote does not.
	//
	// The assertion is vacuous on Linux and load-bearing on Windows, which
	// is the platform the drift lives on.
	tests := []struct {
		name  string
		given string
		want  string
	}{
		{
			name:  "a capture name",
			given: filepath.Join("incoming", "twitch-examplechannel-1.ts"),
			want:  "incoming/twitch-examplechannel-1.ts",
		},
		{
			name:  "a rendered library name",
			given: filepath.Join("ExampleChannel", "2026", "ExampleChannel - 2026-03-04 21-15 - title.mkv"),
			want:  "ExampleChannel/2026/ExampleChannel - 2026-03-04 21-15 - title.mkv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("CreateRecording", func(t *testing.T) {
				store := newStore(t)
				channel := newChannel(t, store)

				created := newRecording(t, store, channel.ID, tt.given)
				if created.Path != tt.want {
					t.Errorf("CreateRecording() Path = %q, want %q", created.Path, tt.want)
				}

				got, err := store.Recording(created.ID)
				if err != nil {
					t.Fatalf("Recording() err = %v, want nil", err)
				}
				if got.Path != tt.want {
					t.Errorf("Path read back = %q, want %q", got.Path, tt.want)
				}
			})

			t.Run("SetPath", func(t *testing.T) {
				store := newStore(t)
				channel := newChannel(t, store)
				recording := newRecording(t, store, channel.ID, "incoming/seed.ts")

				if err := store.SetPath(recording.ID, tt.given); err != nil {
					t.Fatalf("SetPath() err = %v, want nil", err)
				}

				got, err := store.Recording(recording.ID)
				if err != nil {
					t.Fatalf("Recording() err = %v, want nil", err)
				}
				if got.Path != tt.want {
					t.Errorf("Path read back = %q, want %q", got.Path, tt.want)
				}
			})

			t.Run("FinishRecording", func(t *testing.T) {
				store := newStore(t)
				channel := newChannel(t, store)
				recording := newRecording(t, store, channel.ID, "incoming/seed.ts")

				ended := broadcastStart.Add(time.Hour)
				if err := store.FinishRecording(recording.ID, StateComplete, tt.given, 100, time.Hour, ended); err != nil {
					t.Fatalf("FinishRecording() err = %v, want nil", err)
				}

				got, err := store.Recording(recording.ID)
				if err != nil {
					t.Fatalf("Recording() err = %v, want nil", err)
				}
				if got.Path != tt.want {
					t.Errorf("Path read back = %q, want %q", got.Path, tt.want)
				}
			})
		})
	}
}

func TestRecording_AwaitingMetadataKeepsItsCaptureName(t *testing.T) {
	// The naming guard puts a recording here when a required field is
	// missing. The bytes are intact and the file keeps the name capture
	// gave it, so nothing is lost while metadata is retried.
	store := newStore(t)
	channel := newChannel(t, store)
	capturePath := "incoming/twitch-examplechannel-1772658900.ts"
	recording := newRecording(t, store, channel.ID, capturePath)

	ended := broadcastStart.Add(time.Hour)
	if err := store.FinishRecording(recording.ID, StateAwaitingMetadata, capturePath, 100, time.Hour, ended); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}

	got, err := store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if got.State != StateAwaitingMetadata {
		t.Errorf("State = %q, want %q", got.State, StateAwaitingMetadata)
	}
	if got.Path != capturePath {
		t.Errorf("Path = %q, want the capture name %q", got.Path, capturePath)
	}
	if got.Bytes == 0 {
		t.Error("Bytes = 0, want the captured bytes recorded")
	}
}

func TestRecording_Mutators(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	t.Run("SetState", func(t *testing.T) {
		recording := newRecording(t, store, channel.ID, "incoming/state.ts")
		if err := store.SetState(recording.ID, StateFailed); err != nil {
			t.Fatalf("SetState() err = %v, want nil", err)
		}
		got, _ := store.Recording(recording.ID)
		if got.State != StateFailed {
			t.Errorf("State = %q, want %q", got.State, StateFailed)
		}
	})

	t.Run("SetState rejects an unknown state", func(t *testing.T) {
		recording := newRecording(t, store, channel.ID, "incoming/badstate.ts")
		if err := store.SetState(recording.ID, "vibes"); err == nil {
			t.Error("SetState() err = nil, want a rejection")
		}
	})

	t.Run("SetPath", func(t *testing.T) {
		recording := newRecording(t, store, channel.ID, "incoming/path.ts")
		if err := store.SetPath(recording.ID, "ExampleChannel/2026/named.mkv"); err != nil {
			t.Fatalf("SetPath() err = %v, want nil", err)
		}
		got, _ := store.Recording(recording.ID)
		if got.Path != "ExampleChannel/2026/named.mkv" {
			t.Errorf("Path = %q, want the renamed path", got.Path)
		}
	})

	t.Run("SetPath rejects an empty path", func(t *testing.T) {
		recording := newRecording(t, store, channel.ID, "incoming/emptypath.ts")
		if err := store.SetPath(recording.ID, ""); err == nil {
			t.Error("SetPath() err = nil, want a rejection")
		}
	})

	t.Run("MarkWatched sets and clears", func(t *testing.T) {
		recording := newRecording(t, store, channel.ID, "incoming/watched.ts")
		when := broadcastStart.Add(24 * time.Hour)

		if err := store.MarkWatched(recording.ID, &when); err != nil {
			t.Fatalf("MarkWatched() err = %v, want nil", err)
		}
		got, _ := store.Recording(recording.ID)
		if got.WatchedAt == nil || !got.WatchedAt.Equal(when) {
			t.Errorf("WatchedAt = %v, want %s", got.WatchedAt, when)
		}

		if err := store.MarkWatched(recording.ID, nil); err != nil {
			t.Fatalf("MarkWatched(nil) err = %v, want nil", err)
		}
		got, _ = store.Recording(recording.ID)
		if got.WatchedAt != nil {
			t.Errorf("WatchedAt = %v, want nil after clearing", got.WatchedAt)
		}
	})

	t.Run("SetBroadcast links and clears", func(t *testing.T) {
		// Capture starts before the broadcast is necessarily known, so the
		// link is made afterward.
		recording := newRecording(t, store, channel.ID, "incoming/link.ts")
		broadcast, err := store.UpsertBroadcast(Broadcast{
			ChannelID: channel.ID, StartedAt: broadcastStart, RemoteID: "link", Source: SourceLive,
		})
		if err != nil {
			t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
		}

		if err := store.SetBroadcast(recording.ID, &broadcast.ID); err != nil {
			t.Fatalf("SetBroadcast() err = %v, want nil", err)
		}
		got, _ := store.Recording(recording.ID)
		if got.BroadcastID == nil || *got.BroadcastID != broadcast.ID {
			t.Errorf("BroadcastID = %v, want %d", got.BroadcastID, broadcast.ID)
		}

		if err := store.SetBroadcast(recording.ID, nil); err != nil {
			t.Fatalf("SetBroadcast(nil) err = %v, want nil", err)
		}
		got, _ = store.Recording(recording.ID)
		if got.BroadcastID != nil {
			t.Errorf("BroadcastID = %v, want nil after clearing", got.BroadcastID)
		}
	})

	t.Run("SetPinned", func(t *testing.T) {
		recording := newRecording(t, store, channel.ID, "incoming/pinned.ts")
		if err := store.SetPinned(recording.ID, true); err != nil {
			t.Fatalf("SetPinned() err = %v, want nil", err)
		}
		got, _ := store.Recording(recording.ID)
		if !got.Pinned {
			t.Error("Pinned = false, want true")
		}
	})

	t.Run("SetRefetchable", func(t *testing.T) {
		recording := newRecording(t, store, channel.ID, "incoming/refetch.ts")
		if err := store.SetRefetchable(recording.ID, true); err != nil {
			t.Fatalf("SetRefetchable() err = %v, want nil", err)
		}
		got, _ := store.Recording(recording.ID)
		if !got.Refetchable {
			t.Error("Refetchable = false, want true")
		}
	})

	t.Run("SetBytes", func(t *testing.T) {
		recording := newRecording(t, store, channel.ID, "incoming/sized.ts")
		if err := store.SetBytes(recording.ID, 4096); err != nil {
			t.Fatalf("SetBytes() err = %v, want nil", err)
		}
		got, _ := store.Recording(recording.ID)
		if got.Bytes != 4096 {
			t.Errorf("Bytes = %d, want 4096", got.Bytes)
		}
	})

	// The schema carries CHECK (bytes >= 0), so a negative size is refused
	// either way. What the guard adds is a message naming the value, which
	// a constraint violation from the driver does not carry.
	t.Run("SetBytes names the negative size it refused", func(t *testing.T) {
		recording := newRecording(t, store, channel.ID, "incoming/negative.ts")
		err := store.SetBytes(recording.ID, -1)
		if err == nil {
			t.Fatal("SetBytes() err = nil, want a rejection")
		}
		if !strings.Contains(err.Error(), "-1 must not be negative") {
			t.Errorf("SetBytes() err = %v, want it to name the refused size", err)
		}
	})
}

// TestRecording_SetBytesMovesTheSpaceBudget states the reason SetBytes
// exists. A stage that shrinks a file has bought headroom, and the budget
// only sees it once the row agrees with the disk.
func TestRecording_SetBytesMovesTheSpaceBudget(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	recording := newRecording(t, store, channel.ID, "incoming/budget.ts")
	if err := store.SetBytes(recording.ID, 10_000); err != nil {
		t.Fatalf("SetBytes() err = %v, want nil", err)
	}

	before, err := store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes() err = %v, want nil", err)
	}

	// A remux reclaiming container overhead, then a recompress halving
	// what is left.
	if err := store.SetBytes(recording.ID, 4_000); err != nil {
		t.Fatalf("SetBytes() err = %v, want nil", err)
	}

	after, err := store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes() err = %v, want nil", err)
	}
	if after != before-6_000 {
		t.Errorf("TotalBytes() = %d, want %d: the budget did not follow the file", after, before-6_000)
	}
}

func TestRecording_MutatorsReportMissingRows(t *testing.T) {
	// An update that matches nothing must not read as success, or a caller
	// carries on believing a deleted recording was changed.
	store := newStore(t)

	tests := []struct {
		name string
		call func() error
	}{
		{name: "SetState", call: func() error { return store.SetState(999, StateComplete) }},
		{name: "SetBroadcast", call: func() error { return store.SetBroadcast(999, nil) }},
		{name: "SetPath", call: func() error { return store.SetPath(999, "x.mkv") }},
		{name: "MarkWatched", call: func() error { return store.MarkWatched(999, nil) }},
		{name: "SetPinned", call: func() error { return store.SetPinned(999, true) }},
		{name: "SetRefetchable", call: func() error { return store.SetRefetchable(999, true) }},
		{name: "SetBytes", call: func() error { return store.SetBytes(999, 1) }},
		{
			name: "FinishRecording",
			call: func() error {
				return store.FinishRecording(999, StateComplete, "x.mkv", 1, time.Second, broadcastStart)
			},
		},
		{name: "FillGap", call: func() error { return store.FillGap(999, broadcastStart) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s on a missing row err = %v, want it to wrap ErrNotFound", tt.name, err)
			}
		})
	}
}

func TestRecording_NotFound(t *testing.T) {
	store := newStore(t)

	if _, err := store.Recording(999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Recording() err = %v, want it to wrap ErrNotFound", err)
	}
}

// ///////////////////////////////////////////////
// State
// ///////////////////////////////////////////////

func TestState_Valid(t *testing.T) {
	// Valid gates every SetState, so a state missing from it is a state the
	// database will never accept no matter who sets it.
	tests := []struct {
		name  string
		state State
		want  bool
	}{
		{name: "capturing", state: StateCapturing, want: true},
		{name: "awaiting metadata", state: StateAwaitingMetadata, want: true},
		{name: "awaiting file", state: StateAwaitingFile, want: true},
		{name: "complete", state: StateComplete, want: true},
		{name: "failed", state: StateFailed, want: true},
		{name: "trashed", state: StateTrashed, want: true},
		{name: "invented", state: State("halfway"), want: false},
		{name: "empty", state: State(""), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.Valid(); got != tt.want {
				t.Errorf("State(%q).Valid() = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Queries
// ///////////////////////////////////////////////

func TestRecordingsByState(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	stuck := newRecording(t, store, channel.ID, "incoming/stuck.ts")
	if err := store.SetState(stuck.ID, StateAwaitingMetadata); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}
	newRecording(t, store, channel.ID, "incoming/running.ts")

	got, err := store.RecordingsByState(StateAwaitingMetadata)
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}
	if len(got) != 1 || got[0].ID != stuck.ID {
		t.Errorf("RecordingsByState() = %v, want only the awaiting-metadata recording", got)
	}
}

func TestRecordingsByState_SeveralStates(t *testing.T) {
	// The sweep retries every kind of waiting recording in one pass, so a
	// state added later must not need a second query to be swept.
	store := newStore(t)
	channel := newChannel(t, store)

	noTitle := newRecording(t, store, channel.ID, "incoming/no-title.ts")
	if err := store.SetState(noTitle.ID, StateAwaitingMetadata); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}
	held := newRecording(t, store, channel.ID, "incoming/held.ts")
	if err := store.SetState(held.ID, StateAwaitingFile); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}
	done := newRecording(t, store, channel.ID, "ExampleChannel/2026/done.mkv")
	if err := store.SetState(done.ID, StateComplete); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}

	got, err := store.RecordingsByState(StateAwaitingMetadata, StateAwaitingFile)
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}

	ids := make(map[int64]bool, len(got))
	for _, recording := range got {
		ids[recording.ID] = true
	}
	if len(got) != 2 || !ids[noTitle.ID] || !ids[held.ID] {
		t.Errorf("RecordingsByState() = %v, want both waiting recordings and nothing else", got)
	}
	if ids[done.ID] {
		t.Error("RecordingsByState() returned the complete recording")
	}
}

func TestRecordingsByState_NoStates(t *testing.T) {
	// An empty list must not read as "every state", which an IN () built
	// without care would do.
	store := newStore(t)
	channel := newChannel(t, store)
	newRecording(t, store, channel.ID, "incoming/running.ts")

	got, err := store.RecordingsByState()
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("RecordingsByState() = %v, want nothing", got)
	}
}

func TestRecordingsForBroadcast(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	broadcast, err := store.UpsertBroadcast(Broadcast{
		ChannelID: channel.ID, StartedAt: broadcastStart, Source: SourceLive,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}

	if _, err := store.CreateRecording(Recording{
		ChannelID: channel.ID, BroadcastID: &broadcast.ID, Path: "linked.ts",
		State: StateComplete, Origin: OriginLive, StartedAt: broadcastStart,
	}); err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	newRecording(t, store, channel.ID, "unlinked.ts")

	got, err := store.RecordingsForBroadcast(broadcast.ID)
	if err != nil {
		t.Fatalf("RecordingsForBroadcast() err = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Path != "linked.ts" {
		t.Errorf("RecordingsForBroadcast() = %v, want only the linked recording", got)
	}
}

func TestTotalBytes(t *testing.T) {
	store := newStore(t)

	t.Run("empty library sums to zero", func(t *testing.T) {
		// SUM over no rows is NULL, which must not become a scan error.
		got, err := store.TotalBytes()
		if err != nil {
			t.Fatalf("TotalBytes() err = %v, want nil", err)
		}
		if got != 0 {
			t.Errorf("TotalBytes() = %d, want 0", got)
		}
	})

	t.Run("sums recorded bytes", func(t *testing.T) {
		channel := newChannel(t, store)
		for i, bytes := range []int64{100, 250} {
			recording := newRecording(t, store, channel.ID, "incoming/sum"+time.Month(i+1).String()+".ts")
			if err := store.FinishRecording(recording.ID, StateComplete, recording.Path,
				bytes, time.Hour, broadcastStart.Add(time.Hour)); err != nil {
				t.Fatalf("FinishRecording() err = %v, want nil", err)
			}
		}

		got, err := store.TotalBytes()
		if err != nil {
			t.Fatalf("TotalBytes() err = %v, want nil", err)
		}
		if got != 350 {
			t.Errorf("TotalBytes() = %d, want 350", got)
		}
	})
}

// ///////////////////////////////////////////////
// Gaps
// ///////////////////////////////////////////////

func TestGaps(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	recording := newRecording(t, store, channel.ID, "incoming/gappy.ts")

	// The 2026-07-01 recovery missed the first 33 minutes of a broadcast.
	// Recording that as a gap is what makes patching it possible later.
	gap, err := store.AddGap(recording.ID, 0, 33*time.Minute, "recorder started late")
	if err != nil {
		t.Fatalf("AddGap() err = %v, want nil", err)
	}

	got, err := store.Gaps(recording.ID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("Gaps() returned %d, want 1", len(got))
	}
	if got[0].End != 33*time.Minute {
		t.Errorf("Gap.End = %s, want %s", got[0].End, 33*time.Minute)
	}
	if got[0].FilledAt != nil {
		t.Errorf("FilledAt = %v, want nil before patching", got[0].FilledAt)
	}

	filled := broadcastStart.Add(48 * time.Hour)
	if err := store.FillGap(gap.ID, filled); err != nil {
		t.Fatalf("FillGap() err = %v, want nil", err)
	}
	got, _ = store.Gaps(recording.ID)
	if got[0].FilledAt == nil || !got[0].FilledAt.Equal(filled) {
		t.Errorf("FilledAt = %v, want %s", got[0].FilledAt, filled)
	}
}

func TestAddGap_RejectsInvertedRange(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	recording := newRecording(t, store, channel.ID, "incoming/inverted.ts")

	// The schema refuses these too, so the assertion is on the message: a
	// caller has to be told what was wrong with its arguments, not handed a
	// constraint name from a layer it never addressed.
	tests := []struct {
		name       string
		start, end time.Duration
		wantErr    string
	}{
		{name: "end before start", start: time.Hour, end: time.Minute, wantErr: "must be after start"},
		{name: "zero length", start: time.Hour, end: time.Hour, wantErr: "must be after start"},
		// Gaps are stored in milliseconds. A span shorter than one rounds
		// away to the zero-length gap the check above exists to refuse.
		{
			name: "shorter than a millisecond", start: 100 * time.Microsecond, end: 200 * time.Microsecond,
			wantErr: "must be after start",
		},
		// An offset runs from the recording's start, so a negative one
		// names no part of the file.
		{name: "starting before the file", start: -10 * time.Hour, end: -time.Hour, wantErr: "must not be negative"},
		{
			name: "starting before the file and ending inside it", start: -time.Hour, end: time.Minute,
			wantErr: "must not be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.AddGap(recording.ID, tt.start, tt.end, "bad")
			if err == nil {
				t.Fatal("AddGap() err = nil, want a rejection")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("AddGap() err = %v, want it to say %q", err, tt.wantErr)
			}
		})
	}
}

func TestAddGap_ReturnsWhatItStored(t *testing.T) {
	// The backfill engine patches from the returned Gap. Returning the
	// nanosecond arguments while storing milliseconds hands it a span the
	// row does not agree with.
	store := newStore(t)
	channel := newChannel(t, store)
	recording := newRecording(t, store, channel.ID, "incoming/rounded.ts")

	gap, err := store.AddGap(recording.ID, 1500*time.Microsecond, 4700*time.Microsecond, "reconnect")
	if err != nil {
		t.Fatalf("AddGap() err = %v, want nil", err)
	}

	stored, err := store.Gaps(recording.ID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(stored) != 1 {
		t.Fatalf("Gaps() returned %d gaps, want 1", len(stored))
	}
	if gap.Start != stored[0].Start || gap.End != stored[0].End {
		t.Errorf("AddGap() returned %s to %s, want the stored %s to %s",
			gap.Start, gap.End, stored[0].Start, stored[0].End)
	}
}

func TestGaps_CascadeWithTheirRecording(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	recording := newRecording(t, store, channel.ID, "incoming/cascade.ts")
	if _, err := store.AddGap(recording.ID, 0, time.Minute, "test"); err != nil {
		t.Fatalf("AddGap() err = %v, want nil", err)
	}

	if _, err := store.db.Exec(`DELETE FROM recordings WHERE id = ?`, recording.ID); err != nil {
		t.Fatalf("deleting recording: %v", err)
	}

	got, err := store.Gaps(recording.ID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("Gaps() returned %d orphaned rows, want none", len(got))
	}
}

// ///////////////////////////////////////////////
// Trash lifecycle
// ///////////////////////////////////////////////

func TestRecompressCandidates(t *testing.T) {
	// The rung exists to buy headroom, so the recordings least likely to be
	// watched again pay for it: oldest first.
	store := newStore(t)
	channel := newChannel(t, store)

	old := completeAt(t, store, channel.ID, "old.mkv", broadcastStart.Add(-90*24*time.Hour))
	older := completeAt(t, store, channel.ID, "older.mkv", broadcastStart.Add(-120*24*time.Hour))
	completeAt(t, store, channel.ID, "recent.mkv", broadcastStart.Add(-time.Hour))

	got, err := store.RecompressCandidates(broadcastStart.Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("RecompressCandidates() err = %v, want nil", err)
	}

	if len(got) != 2 {
		t.Fatalf("RecompressCandidates() returned %d recordings, want 2", len(got))
	}
	if got[0].ID != older.ID {
		t.Errorf("the first candidate is %d, want the oldest %d", got[0].ID, older.ID)
	}
	if got[1].ID != old.ID {
		t.Errorf("the second candidate is %d, want %d", got[1].ID, old.ID)
	}
}

func TestRecompressCandidates_ExcludesWhatMustNotBeReEncoded(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, store *Store, recording Recording)
	}{
		{
			name: "a recording already re-encoded",
			prepare: func(t *testing.T, store *Store, recording Recording) {
				if err := store.MarkRecompressed(recording.ID, broadcastStart, 1<<29); err != nil {
					t.Fatalf("MarkRecompressed() err = %v, want nil", err)
				}
			},
		},
		{
			name: "a recording the organizer has not finished",
			prepare: func(t *testing.T, store *Store, recording Recording) {
				// Its file is about to move, and re-encoding underneath
				// that leaves the move pointing at bytes nothing verified.
				if err := store.SetState(recording.ID, StateAwaitingMetadata); err != nil {
					t.Fatalf("SetState() err = %v, want nil", err)
				}
			},
		},
		{
			name: "a recording already in the trash",
			prepare: func(t *testing.T, store *Store, recording Recording) {
				if err := store.SetState(recording.ID, StateTrashed); err != nil {
					t.Fatalf("SetState() err = %v, want nil", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			channel := newChannel(t, store)
			recording := completeAt(t, store, channel.ID, "old.mkv",
				broadcastStart.Add(-90*24*time.Hour))
			tc.prepare(t, store, recording)

			got, err := store.RecompressCandidates(broadcastStart.Add(-30 * 24 * time.Hour))
			if err != nil {
				t.Fatalf("RecompressCandidates() err = %v, want nil", err)
			}
			if len(got) != 0 {
				t.Errorf("RecompressCandidates() offered %d recordings, want none", len(got))
			}
		})
	}
}

func TestMarkRecompressed_WritesTheMarkAndTheSizeTogether(t *testing.T) {
	// A mark without a size leaves the budget measuring the pre-encode file.
	// A size without a mark offers the recording again next sweep, which
	// spends hours to save nothing.
	store := newStore(t)
	channel := newChannel(t, store)
	recording := completeAt(t, store, channel.ID, "old.mkv", broadcastStart.Add(-90*24*time.Hour))

	at := broadcastStart.Add(time.Hour)
	if err := store.MarkRecompressed(recording.ID, at, 1<<29); err != nil {
		t.Fatalf("MarkRecompressed() err = %v, want nil", err)
	}

	got, err := store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if got.RecompressedAt == nil {
		t.Fatal("RecompressedAt = nil, want the mark recorded")
	}
	if !got.RecompressedAt.Equal(at) {
		t.Errorf("RecompressedAt = %s, want %s", got.RecompressedAt, at)
	}
	if got.Bytes != 1<<29 {
		t.Errorf("Bytes = %d, want the re-encoded size", got.Bytes)
	}
}

func TestMarkRecompressed_RefusesANegativeSize(t *testing.T) {
	// The schema's own CHECK would refuse this too, so the guard's whole
	// contribution is a message naming the value.
	store := newStore(t)
	channel := newChannel(t, store)
	recording := completeAt(t, store, channel.ID, "old.mkv", broadcastStart)

	err := store.MarkRecompressed(recording.ID, broadcastStart, -1)

	if err == nil {
		t.Fatal("MarkRecompressed() err = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "-1") {
		t.Errorf("MarkRecompressed() err = %v, want it to name the value", err)
	}
}

// completeAt seeds a finished recording that started at a given time.
func completeAt(t *testing.T, store *Store, channelID int64, path string, at time.Time) Recording {
	t.Helper()

	recording, err := store.CreateRecording(Recording{
		ChannelID: channelID,
		Path:      path,
		State:     StateComplete,
		Origin:    OriginLive,
		Bytes:     1 << 30,
		StartedAt: at,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	return recording
}

func TestTrashedBefore(t *testing.T) {
	// The release asks which undo windows have been open longest, so the
	// order is by when the purge happened, not by broadcast date.
	store := newStore(t)
	channel := newChannel(t, store)

	older := newRecording(t, store, channel.ID, "trash/1-older.mkv")
	newer := newRecording(t, store, channel.ID, "trash/2-newer.mkv")
	live := newRecording(t, store, channel.ID, "ExampleChannel/2026/live.mkv")

	// Trashed in this order, so updated_at orders them this way.
	for _, id := range []int64{older.ID, newer.ID} {
		if err := store.SetState(id, StateTrashed); err != nil {
			t.Fatalf("SetState() err = %v, want nil", err)
		}
	}

	got, err := store.TrashedBefore(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("TrashedBefore() err = %v, want nil", err)
	}

	if len(got) != 2 {
		t.Fatalf("TrashedBefore() returned %d recordings, want the two trashed ones", len(got))
	}
	if got[0].ID != older.ID || got[1].ID != newer.ID {
		t.Errorf("TrashedBefore() order = %d then %d, want %d then %d: longest in the trash first",
			got[0].ID, got[1].ID, older.ID, newer.ID)
	}
	for _, recording := range got {
		if recording.ID == live.ID {
			t.Error("TrashedBefore() returned a recording that was never purged")
		}
	}
}

func TestTrashedBefore_HonoursTheCutoff(t *testing.T) {
	// The cutoff is the grace period. A recording purged a moment ago is
	// still inside its undo window and must not be offered.
	store := newStore(t)
	channel := newChannel(t, store)

	recording := newRecording(t, store, channel.ID, "trash/1-fresh.mkv")
	if err := store.SetState(recording.ID, StateTrashed); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}

	if got, err := store.TrashedBefore(time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("TrashedBefore() err = %v, want nil", err)
	} else if len(got) != 0 {
		t.Errorf("TrashedBefore() returned %d recordings, want none still inside the grace", len(got))
	}
}

func TestDeleteRecording(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	recording := newRecording(t, store, channel.ID, "trash/1-gone.mkv")

	if err := store.DeleteRecording(recording.ID); err != nil {
		t.Fatalf("DeleteRecording() err = %v, want nil", err)
	}

	if _, err := store.Recording(recording.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Recording() after delete err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestDeleteRecording_ReturnsTheBytesToTheBudget(t *testing.T) {
	// Releasing a purge is the only thing that gives headroom back. A
	// trashed recording still counts, because its file is still there.
	store := newStore(t)
	channel := newChannel(t, store)

	kept := newRecording(t, store, channel.ID, "ExampleChannel/2026/kept.mkv")
	purged := newRecording(t, store, channel.ID, "trash/2-purged.mkv")
	for _, id := range []int64{kept.ID, purged.ID} {
		if err := store.SetBytes(id, 10_000); err != nil {
			t.Fatalf("SetBytes() err = %v, want nil", err)
		}
	}
	if err := store.SetState(purged.ID, StateTrashed); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}

	trashed, err := store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes() err = %v, want nil", err)
	}
	if trashed != 20_000 {
		t.Fatalf("TotalBytes() = %d, want 20000: a trashed file is still on the volume", trashed)
	}

	if err := store.DeleteRecording(purged.ID); err != nil {
		t.Fatalf("DeleteRecording() err = %v, want nil", err)
	}

	after, err := store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes() err = %v, want nil", err)
	}
	if after != 10_000 {
		t.Errorf("TotalBytes() = %d, want 10000: the release did not return the bytes", after)
	}
}

// ///////////////////////////////////////////////
// AddGap deduplication
// ///////////////////////////////////////////////

func TestAddGap_FilingTheSameSpanTwiceYieldsOneRow(t *testing.T) {
	// The detector re-derives every hole in a broadcast each time it runs,
	// so a gap is filed again on every pass. Without the unique span there
	// is one duplicate row per pass and nothing to stop it.
	store := newStore(t)
	channel := newChannel(t, store)
	recordingID := newRecording(t, store, channel.ID, "incoming/gappy.ts").ID

	first, err := store.AddGap(recordingID, time.Minute, 2*time.Minute, "reconnect")
	if err != nil {
		t.Fatalf("first AddGap() err = %v, want nil", err)
	}
	second, err := store.AddGap(recordingID, time.Minute, 2*time.Minute, "reconnect")
	if err != nil {
		t.Fatalf("second AddGap() err = %v, want nil", err)
	}

	if second.ID != first.ID {
		t.Errorf("second AddGap() id = %d, want the existing %d", second.ID, first.ID)
	}

	gaps, err := store.Gaps(recordingID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(gaps) != 1 {
		t.Errorf("filing one gap twice left %d rows, want 1", len(gaps))
	}
}

func TestAddGap_KeepsTheReasonRecordedFirst(t *testing.T) {
	// A later pass has no more information than the first, so it must not
	// overwrite a reason with whatever the current detector happens to
	// call the same hole.
	store := newStore(t)
	channel := newChannel(t, store)
	recordingID := newRecording(t, store, channel.ID, "incoming/gappy.ts").ID

	if _, err := store.AddGap(recordingID, 0, time.Minute, "late start"); err != nil {
		t.Fatalf("first AddGap() err = %v, want nil", err)
	}
	again, err := store.AddGap(recordingID, 0, time.Minute, "reconnect")
	if err != nil {
		t.Fatalf("second AddGap() err = %v, want nil", err)
	}

	if again.Reason != "late start" {
		t.Errorf("Reason = %q, want the first-recorded %q", again.Reason, "late start")
	}
}

func TestAddGap_SeparatesDifferentSpans(t *testing.T) {
	// Two holes in one recording are two gaps. A conflict target that
	// caught them both would silently drop the second.
	store := newStore(t)
	channel := newChannel(t, store)
	recordingID := newRecording(t, store, channel.ID, "incoming/gappy.ts").ID

	if _, err := store.AddGap(recordingID, 0, time.Minute, "late start"); err != nil {
		t.Fatalf("first AddGap() err = %v, want nil", err)
	}
	if _, err := store.AddGap(recordingID, 5*time.Minute, 6*time.Minute, "reconnect"); err != nil {
		t.Fatalf("second AddGap() err = %v, want nil", err)
	}

	gaps, err := store.Gaps(recordingID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(gaps) != 2 {
		t.Errorf("two distinct spans left %d rows, want 2", len(gaps))
	}
}

// ///////////////////////////////////////////////
// What a recovered copy carries
// ///////////////////////////////////////////////

// TestSetMediaDuration covers the length measured from the finished file.
//
// A capture's row records how long the recorder ran, which counts wall clock
// through a dropped connection or an ad break the tool skipped. Only the file
// itself says how much media is in it, and the difference is content missing
// from inside one recording where nothing else looks.
func TestSetMediaDuration(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	recording := newRecording(t, store, channel.ID, "incoming/one.ts")

	if err := store.SetMediaDuration(recording.ID, 90*time.Minute); err != nil {
		t.Fatalf("SetMediaDuration() err = %v, want nil", err)
	}

	stored, err := store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if stored.MediaDuration != 90*time.Minute {
		t.Errorf("MediaDuration = %s, want 1h30m0s", stored.MediaDuration)
	}
}

// TestSetMutedDuration covers how much of a recovered copy the platform
// silenced.
//
// Nothing in the file says which stretches are muted, so a recovered
// recording would otherwise read exactly like one that carries its original
// audio.
func TestSetMutedDuration(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	recording := newRecording(t, store, channel.ID, "incoming/two.ts")

	if err := store.SetMutedDuration(recording.ID, 4*time.Minute); err != nil {
		t.Fatalf("SetMutedDuration() err = %v, want nil", err)
	}

	stored, err := store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if stored.MutedDuration == nil {
		t.Fatal("MutedDuration = nil, want the measured total: nil means nobody could ask")
	}
	if *stored.MutedDuration != 4*time.Minute {
		t.Errorf("MutedDuration = %s, want 4m0s", *stored.MutedDuration)
	}
}

// TestRecordingPaths returns every stored path, which is what a sweep for
// files nothing owns compares the directory against.
func TestRecordingPaths(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)
	newRecording(t, store, channel.ID, "incoming/one.ts")
	newRecording(t, store, channel.ID, "ExampleChannel/2026/two.mkv")

	paths, err := store.RecordingPaths()
	if err != nil {
		t.Fatalf("RecordingPaths() err = %v, want nil", err)
	}
	if len(paths) != 2 {
		t.Fatalf("RecordingPaths() = %v, want both stored paths", paths)
	}
	for _, want := range []string{"incoming/one.ts", "ExampleChannel/2026/two.mkv"} {
		if !slices.Contains(paths, want) {
			t.Errorf("RecordingPaths() = %v, missing %q", paths, want)
		}
	}
}

// TestHoldsBytes reports which states have a file behind them, which is what
// decides whether a row's absence from disk means anything.
func TestHoldsBytes(t *testing.T) {
	tests := []struct {
		state State
		want  bool
	}{
		{StateCapturing, true},
		{StateAwaitingFinalize, true},
		{StateAwaitingMetadata, true},
		{StateAwaitingFile, true},
		{StateComplete, true},
		{StateFailed, false},
		{StateTrashed, false},
		{StateMissing, false},
		{State("nonsense"), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.state), func(t *testing.T) {
			if got := HoldsBytes(tt.state); got != tt.want {
				t.Errorf("HoldsBytes(%q) = %t, want %t", tt.state, got, tt.want)
			}
		})
	}
}
