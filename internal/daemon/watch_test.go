package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/record"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// countingLog collects the consecutive_failures counts a loop reported.
//
// The count is the only place a reset is observable from outside, and a
// reset that never happens is exactly the defect that lets a capture fail
// silently for hours.
type countingLog struct {
	mu     sync.Mutex
	counts []int64
}

// ///////////////////////////////////////////////
// Offline
// ///////////////////////////////////////////////

func TestRunCycle_OfflineDoesNothing(t *testing.T) {
	h := newHarness(t, nil)

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if got.Live {
		t.Error("Live = true, want false")
	}
	if h.engine.captureCount() != 0 {
		t.Error("an offline channel triggered a capture")
	}
	if len(h.notifier.kinds()) != 0 {
		t.Errorf("events = %v, want none for an offline channel", h.notifier.kinds())
	}
}

func TestRunCycle_ProbeFailureIsReported(t *testing.T) {
	// An auth failure must surface. Silently treating it as offline is how
	// a channel stops recording without anyone noticing.
	h := newHarness(t, nil)
	h.engine.probeErr = errors.New("token expired")

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err == nil {
		t.Error("RunCycle() err = nil, want the probe failure surfaced")
	}
}

// ///////////////////////////////////////////////
// The recording path
// ///////////////////////////////////////////////

func TestRunCycle_RecordsAndFinalizes(t *testing.T) {
	h := newHarness(t, nil)
	h.live("Midnight Build Stream")
	h.captured(13_450_000_000, 4*time.Hour+36*time.Minute)
	h.finalizer.outcome = organize.Outcome{Path: "ExampleChannel/2026/named.mkv", Renamed: true}

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	if !got.Live || got.RecordingID == 0 {
		t.Fatalf("CycleResult = %+v, want a recording made", got)
	}
	if got.Bytes != 13_450_000_000 {
		t.Errorf("Bytes = %d, want the captured size", got.Bytes)
	}

	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.State != store.StateComplete {
		t.Errorf("State = %q, want %q", recording.State, store.StateComplete)
	}
	if recording.BroadcastID == nil {
		t.Error("BroadcastID = nil, want the recording linked to its broadcast")
	}

	if len(h.finalizer.calls) != 1 || h.finalizer.calls[0] != got.RecordingID {
		t.Errorf("finalizer calls = %v, want exactly the new recording", h.finalizer.calls)
	}
	if !h.notifier.has(EventRecordingStarted) {
		t.Errorf("events = %v, want a start notification", h.notifier.kinds())
	}
}

func TestRunCycle_StampsTheEndWhenTheChannelGoesOffline(t *testing.T) {
	// A broadcast with no recorded end reads as still running forever, and
	// every rule that waits for one, the settle window and gap patching
	// among them, then skips it on every pass.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1000, time.Hour)

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("first RunCycle() err = %v, want nil", err)
	}
	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.BroadcastID == nil {
		t.Fatal("BroadcastID = nil, want the recording linked to its broadcast")
	}

	ended := now.Add(2 * time.Hour)
	h.at(ended)
	h.engine.probe = record.Probe{}

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Fatalf("second RunCycle() err = %v, want nil", err)
	}

	broadcast, err := h.store.Broadcast(*recording.BroadcastID)
	if err != nil {
		t.Fatalf("Broadcast() err = %v, want nil", err)
	}
	if broadcast.EndedAt == nil {
		t.Fatal("EndedAt = nil, want the end stamped once the channel went offline")
	}
	if !broadcast.EndedAt.Equal(ended) {
		t.Errorf("EndedAt = %s, want %s", broadcast.EndedAt, ended)
	}
}

func TestRunCycle_StoresTheProbedIDAsTheStreamID(t *testing.T) {
	// The probe names the live session, not the video the archive publishes
	// later. Storing it as the remote id puts two namespaces in one column,
	// and the VOD listing then files a second row for the same broadcast.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1000, time.Hour)

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.BroadcastID == nil {
		t.Fatal("BroadcastID = nil, want the recording linked to its broadcast")
	}

	broadcast, err := h.store.Broadcast(*recording.BroadcastID)
	if err != nil {
		t.Fatalf("Broadcast() err = %v, want nil", err)
	}
	if broadcast.StreamID != "stream-1" {
		t.Errorf("StreamID = %q, want the id the probe reported", broadcast.StreamID)
	}
	if broadcast.RemoteID != "" {
		t.Errorf("RemoteID = %q, want it left for the archive listing", broadcast.RemoteID)
	}
}

// ///////////////////////////////////////////////
// Joining a broadcast already in progress
// ///////////////////////////////////////////////

// broadcastOf returns the broadcast a recording belongs to.
func broadcastOf(t *testing.T, h *harness, recordingID int64) store.Broadcast {
	t.Helper()

	recording, err := h.store.Recording(recordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.BroadcastID == nil {
		t.Fatal("BroadcastID = nil, want the recording linked to its broadcast")
	}

	broadcast, err := h.store.Broadcast(*recording.BroadcastID)
	if err != nil {
		t.Fatalf("Broadcast() err = %v, want nil", err)
	}
	return broadcast
}

func TestRunCycle_AnchorsToTheResolvedBroadcastStart(t *testing.T) {
	// A channel already broadcasting when the recorder first polls it began
	// before this cycle. Anchoring the row to the poll asserts the missing
	// stretch never happened, so nothing ever files a hole for it.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1000, time.Hour)

	broadcastStart := now.Add(-40 * time.Minute)
	h.broadcastStart = func(context.Context, string, string) (time.Time, bool) { return broadcastStart, true }

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	if broadcast := broadcastOf(t, h, got.RecordingID); !broadcast.StartedAt.Equal(broadcastStart) {
		t.Errorf("StartedAt = %s, want the true start %s", broadcast.StartedAt, broadcastStart)
	}

	// The recording keeps the moment capture began. It is a different fact
	// from when the broadcast began, and it is the one that names the file.
	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if !recording.StartedAt.Equal(now) {
		t.Errorf("recording StartedAt = %s, want the moment capture began %s",
			recording.StartedAt, now)
	}
}

func TestRunCycle_ALaterPollDoesNotMoveTheStart(t *testing.T) {
	// The revert this guards against is quiet. SourceLive outranks
	// SourceAPI, so a corrected start that is re-resolved on a later poll of
	// the same session is overwritten by whatever the resolver says then,
	// and the row is right for one poll interval and wrong afterwards.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1000, time.Hour)

	broadcastStart := now.Add(-40 * time.Minute)
	answer := broadcastStart
	h.broadcastStart = func(context.Context, string, string) (time.Time, bool) { return answer, true }

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	// A second poll of the same broadcast, with the resolver now answering
	// something else entirely. The anchor already agreed must stand.
	h.at(now.Add(30 * time.Second))
	answer = now.Add(-5 * time.Minute)
	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	if broadcast := broadcastOf(t, h, got.RecordingID); !broadcast.StartedAt.Equal(broadcastStart) {
		t.Errorf("StartedAt = %s, want the anchor from the first poll %s",
			broadcast.StartedAt, broadcastStart)
	}
}

func TestRunCycle_ResolvesTheStartOncePerBroadcast(t *testing.T) {
	// The lookup costs a request. Paying it on every poll of a six hour
	// broadcast is the difference between one request and hundreds.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1000, time.Hour)
	h.broadcastStart = func(context.Context, string, string) (time.Time, bool) {
		return now.Add(-40 * time.Minute), true
	}

	for poll := range 3 {
		h.at(now.Add(time.Duration(poll) * 30 * time.Second))
		if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
			t.Fatalf("RunCycle() err = %v, want nil", err)
		}
	}

	if got := h.startLookups(); got != 1 {
		t.Errorf("lookups = %d, want 1 for three polls of one broadcast", got)
	}
}

func TestRunCycle_UnresolvedStartFallsBackToWhenItWasSeen(t *testing.T) {
	// A machine with no metadata session watching a channel that publishes
	// no archive gets no answer. That is the ordinary case, and it must
	// still record.
	tests := []struct {
		name     string
		resolver func(context.Context, string, string) (time.Time, bool)
	}{
		{
			name:     "nobody could answer",
			resolver: func(context.Context, string, string) (time.Time, bool) { return time.Time{}, false },
		},
		{
			// A start after the poll saw the channel is a clock
			// disagreeing, not a better reading of the same broadcast.
			name: "an answer later than the poll",
			resolver: func(context.Context, string, string) (time.Time, bool) {
				return now.Add(10 * time.Minute), true
			},
		},
		{
			// A channel that never goes offline reports a start days old.
			// Anchoring there files a hole spanning the whole stretch, and
			// the patcher sizes a download from it that no library can hold,
			// so the broadcast is refused entire and every real gap inside
			// it goes down with the refusal.
			name: "an answer further back than a broadcast runs",
			resolver: func(context.Context, string, string) (time.Time, bool) {
				return now.Add(-30 * 24 * time.Hour), true
			},
		},
		{
			// Unstorable, and it must be refused before the anchor is
			// latched. A value that reaches the session wedges the channel:
			// every later poll returns the cached anchor, the upsert keeps
			// failing, and nothing is ever recorded.
			name: "an answer outside the storable range",
			resolver: func(context.Context, string, string) (time.Time, bool) {
				return time.Time{}, true
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.live("a title")
			h.captured(1000, time.Hour)
			h.broadcastStart = tt.resolver

			got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
			if err != nil {
				t.Fatalf("RunCycle() err = %v, want nil", err)
			}
			if h.engine.captureCount() != 1 {
				t.Errorf("captures = %d, want the capture to run regardless",
					h.engine.captureCount())
			}
			if broadcast := broadcastOf(t, h, got.RecordingID); !broadcast.StartedAt.Equal(now) {
				t.Errorf("StartedAt = %s, want the moment the poll saw it %s",
					broadcast.StartedAt, now)
			}
		})
	}
}

func TestRunCycle_OfflineWithNoBroadcastOfItsOwnStampsNothing(t *testing.T) {
	// A daemon that has never seen this channel live has no broadcast to
	// close, and inventing one would put an end on a row it never opened.
	h := newHarness(t, nil)

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	broadcasts, err := h.store.BroadcastsBetween(h.channel.ID,
		now.Add(-24*time.Hour), now.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("BroadcastsBetween() err = %v, want nil", err)
	}
	if len(broadcasts) != 0 {
		t.Errorf("broadcasts = %+v, want none written by an offline poll", broadcasts)
	}
}

func TestRunCycle_CaptureNameNeedsNoMetadata(t *testing.T) {
	// The capture path is derived from platform, channel, and start time,
	// none of which can fail. A probe with no title at all still yields a
	// usable filename.
	h := newHarness(t, nil)
	h.engine.probe.Live = true
	h.engine.probe.Qualities = []string{"best"}
	h.captured(1000, time.Hour)

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if !strings.HasPrefix(recording.Path, "incoming") {
		t.Errorf("Path = %q, want it under incoming", recording.Path)
	}
	if !strings.Contains(recording.Path, "twitch-examplechannel-") {
		t.Errorf("Path = %q, want the metadata-free capture name", recording.Path)
	}
}

func TestRunCycle_LearnsTheDisplayName(t *testing.T) {
	// A channel whose display name was unknown gets it from the probe, so
	// naming stops falling back on the next broadcast.
	h := newHarness(t, nil)
	if _, err := h.store.UpsertChannel(config.PlatformTwitch, "nameless", ""); err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
	nameless, err := h.store.Channel(config.PlatformTwitch, "nameless")
	if err != nil {
		t.Fatalf("Channel() err = %v, want nil", err)
	}

	h.live("a title")
	h.captured(1000, time.Hour)
	entry := config.Channel{Platform: config.PlatformTwitch, Name: "nameless", Enabled: true}

	if _, err := h.daemon.RunCycle(context.Background(), entry, nameless); err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	got, err := h.store.Channel(config.PlatformTwitch, "nameless")
	if err != nil {
		t.Fatalf("Channel() err = %v, want nil", err)
	}
	if got.DisplayName != "ExampleChannel" {
		t.Errorf("DisplayName = %q, want it learned from the probe", got.DisplayName)
	}
}

func TestRunCycle_PassesTheQualityLadder(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Channels[0].Quality = []string{"720p", "best"}
	})
	h.live("a title")
	h.captured(1000, time.Hour)

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if len(h.engine.lastRequest.Qualities) != 2 || h.engine.lastRequest.Qualities[0] != "720p" {
		t.Errorf("capture qualities = %v, want the channel override", h.engine.lastRequest.Qualities)
	}
}

// ///////////////////////////////////////////////
// The budget
// ///////////////////////////////////////////////

// fillLibrary records a complete recording large enough to reach a 10 GB cap.
func fillLibrary(t *testing.T, h *harness) {
	t.Helper()

	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "full.mkv",
		State: store.StateComplete, Origin: store.OriginLive, StartedAt: now.Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	if err := h.store.FinishRecording(recording.ID, store.StateComplete, "full.mkv",
		10*config.Gigabyte.Bytes(), time.Hour, now.Add(-47*time.Hour)); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}
}

func TestRunCycle_AFullLibraryIsReportedOncePerBroadcast(t *testing.T) {
	// A live channel against a full library is polled for as long as it
	// stays live, and every poll refuses again. Reporting each one buries
	// the history explaining how the library filled under copies of the
	// consequence.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 10 * config.Gigabyte
	})
	h.free = 900 * config.Gigabyte.Bytes()
	h.live("a title")
	fillLibrary(t, h)

	for range 5 {
		got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
		if err != nil {
			t.Fatalf("RunCycle() err = %v, want nil", err)
		}
		if !got.Refused {
			t.Fatalf("CycleResult = %+v, want every cycle refused", got)
		}
	}

	if got := h.notifier.count(EventLibraryFull); got != 1 {
		t.Errorf("5 refused polls of one broadcast reported %d times, want 1", got)
	}
}

func TestRunCycle_ANewBroadcastReportsAFullLibraryAgain(t *testing.T) {
	// Latching must not silence the next broadcast. A refusal nobody is
	// told about costs the same broadcast as no refusal at all.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 10 * config.Gigabyte
	})
	h.free = 900 * config.Gigabyte.Bytes()
	h.live("a title")
	fillLibrary(t, h)

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Fatalf("first RunCycle() err = %v, want nil", err)
	}

	// A separate broadcast: a new remote id, and far enough past the
	// overlap window that it cannot merge into the first.
	h.at(now.Add(4 * time.Hour))
	h.engine.probe.Metadata.ID = "stream-2"

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Fatalf("second RunCycle() err = %v, want nil", err)
	}

	if got := h.notifier.count(EventLibraryFull); got != 2 {
		t.Errorf("two refused broadcasts reported %d times, want 2", got)
	}
}

func TestRunCycle_AContinuousBroadcastWithNoRemoteIDStaysOneBroadcast(t *testing.T) {
	// Some sources report no broadcast id at all, and the store then matches
	// on start time. A start taken from the clock stops matching the row it
	// created as soon as it drifts past the overlap window, so one unbroken
	// session becomes a phantom broadcast every window and each one re-arms
	// the refusal latch.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 10 * config.Gigabyte
	})
	h.free = 900 * config.Gigabyte.Bytes()
	h.live("a title")
	h.engine.probe.Metadata.ID = ""
	fillLibrary(t, h)

	const poll = 30 * time.Second
	for elapsed := time.Duration(0); elapsed < 3*time.Hour; elapsed += poll {
		h.at(now.Add(elapsed))
		got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
		if err != nil {
			t.Fatalf("RunCycle() at %s err = %v, want nil", elapsed, err)
		}
		if !got.Refused {
			t.Fatalf("CycleResult at %s = %+v, want every poll refused", elapsed, got)
		}
	}

	broadcasts, err := h.store.BroadcastsBetween(h.channel.ID, now.Add(-time.Hour), now.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("BroadcastsBetween() err = %v, want nil", err)
	}
	if len(broadcasts) != 1 {
		t.Errorf("three hours of one broadcast wrote %d rows, want 1", len(broadcasts))
	}
	if got := h.notifier.count(EventLibraryFull); got != 1 {
		t.Errorf("three hours of one refused broadcast reported %d times, want 1", got)
	}
}

func TestRunCycle_AnAdmittedBroadcastRearmsTheRefusalReport(t *testing.T) {
	// The operator frees space, the broadcast gets in, the library fills
	// again, and the next refusal costs a second recording. A latch that
	// only clears on a different broadcast never tells them.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 10 * config.Gigabyte
	})
	h.free = 900 * config.Gigabyte.Bytes()
	h.live("a title")
	h.captured(1000, time.Hour)
	fillLibrary(t, h)

	if got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil || !got.Refused {
		t.Fatalf("first RunCycle() = %+v, err = %v, want it refused", got, err)
	}

	// The operator makes room, so the same broadcast is admitted.
	h.daemon.config.Space.MaxSize = 100 * config.Gigabyte
	if got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil || got.Refused {
		t.Fatalf("second RunCycle() = %+v, err = %v, want it recorded", got, err)
	}

	// And it fills again.
	h.daemon.config.Space.MaxSize = 10 * config.Gigabyte
	if got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil || !got.Refused {
		t.Fatalf("third RunCycle() = %+v, err = %v, want it refused again", got, err)
	}

	if got := h.notifier.count(EventLibraryFull); got != 2 {
		t.Errorf("two refusals of one broadcast reported %d times, want both", got)
	}
}

func TestRunCycle_CaptureLogsToItsOwnFile(t *testing.T) {
	// A capture child holds its log open for the length of a broadcast. On
	// Windows that blocks the rename lumberjack rotates with, and a
	// rotation that cannot rename discards the records it was moving.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(config.Gigabyte.Bytes(), time.Hour)

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	got := filepath.ToSlash(h.engine.lastRequest.LogPath)
	if strings.HasSuffix(got, paths.LogDaemon.RelPath) {
		t.Errorf("capture was pointed at the daemon's own rotating log %s", got)
	}
	if !strings.HasSuffix(got, paths.LogCapture.RelPath) {
		t.Errorf("capture LogPath = %s, want it to end in %s", got, paths.LogCapture.RelPath)
	}
}

func TestRunCycle_RefusesWhenTheLibraryIsFull(t *testing.T) {
	// Nothing is deleted to make room. The broadcast is missed and the
	// operator is told, which is the behavior chosen over silent eviction.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 10 * config.Gigabyte
	})
	h.free = 900 * config.Gigabyte.Bytes()
	h.live("a title")

	// Fill the library to its cap.
	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "full.mkv",
		State: store.StateComplete, Origin: store.OriginLive, StartedAt: now.Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	if err := h.store.FinishRecording(recording.ID, store.StateComplete, "full.mkv",
		10*config.Gigabyte.Bytes(), time.Hour, now.Add(-47*time.Hour)); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	if !got.Refused {
		t.Fatalf("CycleResult = %+v, want the recording refused", got)
	}
	if h.engine.captureCount() != 0 {
		t.Error("a capture started despite the refusal")
	}
	if !h.notifier.has(EventLibraryFull) {
		t.Errorf("events = %v, want a library-full notification", h.notifier.kinds())
	}
}

func TestRunCycle_RefusesWhenTheVolumeIsNearlyFull(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 0
		cfg.Space.MinFree = 100 * config.Gigabyte
	})
	h.free = 100*config.Gigabyte.Bytes() + 1
	h.live("a title")

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if !got.Refused {
		t.Errorf("CycleResult = %+v, want the recording refused by the free-space floor", got)
	}
}

func TestRunCycle_BudgetFailureIsAnError(t *testing.T) {
	// A volume that cannot be read is not the same as a full one, and
	// treating it as full would stop recording forever.
	h := newHarness(t, nil)
	h.live("a title")
	h.daemon.freeSpace = func(string) (int64, error) { return 0, errors.New("volume gone") }

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err == nil {
		t.Error("RunCycle() err = nil, want an unreadable volume surfaced")
	}
}

// ///////////////////////////////////////////////
// The mid-capture watermark
// ///////////////////////////////////////////////

func TestRunCycle_ReportsAWatermarkItCanNoLongerRead(t *testing.T) {
	// The watermark is the only thing that stops a capture before it fills
	// the volume. A read that keeps failing has switched that guard off, and
	// the operator has to hear that rather than watch the same warning go by
	// every interval for the length of a broadcast.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 0
		cfg.Space.MinFree = 100 * config.Gigabyte
	})
	h.daemon.spaceInterval = 5 * time.Millisecond
	h.live("a title")
	h.captured(5_000_000, time.Hour)
	h.engine.blockUntilCancel = true

	// Readable once so the capture is admitted, then never again.
	var reads int
	h.daemon.freeSpace = func(string) (int64, error) {
		reads++
		if reads == 1 {
			return 900 * config.Gigabyte.Bytes(), nil
		}
		return 0, errors.New("volume gone")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// The capture blocks until cancelled, so the watcher runs for the whole
	// window and the escalation has to happen inside it.
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	if _, err := h.daemon.RunCycle(ctx, h.entry, h.channel); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCycle() err = %v, want nil or cancellation", err)
	}

	// Dozens of failed reads, one report. The count is the point: a warning
	// per read that never escalates is one an operator learns to skip.
	if got := h.notifier.count(EventFailure); got != 1 {
		t.Errorf("%d failure notifications from a persistently unreadable volume, want exactly 1", got)
	}
	if reads < spaceBlindLimit {
		t.Fatalf("only %d reads happened, so the escalation threshold was never reached", reads)
	}
}

func TestRunCycle_StopsACaptureThatFillsTheLibrary(t *testing.T) {
	// A broadcast runs for hours, so a library with room at the start need
	// not have room at the end. Letting it run until the disk is full
	// corrupts the recording and everything else on the volume.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 0
		cfg.Space.MinFree = 100 * config.Gigabyte
	})
	h.daemon.spaceInterval = 10 * time.Millisecond
	h.live("a title")
	h.captured(5_000_000, time.Hour)
	h.engine.blockUntilCancel = true

	// Comfortably above the floor at admission, critically low afterward.
	var reads int
	h.daemon.freeSpace = func(string) (int64, error) {
		reads++
		if reads == 1 {
			return 900 * config.Gigabyte.Bytes(), nil
		}
		return 100 * config.Gigabyte.Bytes(), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	if !got.StoppedForSpace {
		t.Fatalf("CycleResult = %+v, want the capture stopped at the watermark", got)
	}

	// A capture ended at the watermark is complete as far as it goes. A
	// whole three-hour recording beats a corrupt four-hour one, and the
	// bytes are worth naming and keeping.
	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.State != store.StateComplete {
		t.Errorf("State = %q, want %q for a capture stopped cleanly at the limit",
			recording.State, store.StateComplete)
	}
	if len(h.finalizer.calls) != 1 {
		t.Errorf("finalizer calls = %v, want the partial recording still organized", h.finalizer.calls)
	}
	if !h.notifier.has(EventLibraryFull) {
		t.Errorf("events = %v, want a library-full notification", h.notifier.kinds())
	}
}

func TestRunCycle_DoesNotReadmitAfterStoppingForSpace(t *testing.T) {
	// A capture the watermark stops before it is long enough to keep leaves
	// a failed row and an orphan file and nothing else. Starting another one
	// for the same broadcast produces one more of the same per poll, for the
	// whole broadcast.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 0
		cfg.Space.MinFree = 100 * config.Gigabyte
	})
	h.daemon.spaceInterval = 10 * time.Millisecond
	h.live("a title")
	// Under the two-minute minimum, so the fragment is discarded.
	h.captured(5_000_000, 30*time.Second)
	h.engine.blockUntilCancel = true

	// Room at admission, critical while the capture runs, healthy again by
	// the next poll, which is what an operator freeing space mid-broadcast
	// looks like.
	stopped := false
	reads := 0
	h.daemon.freeSpace = func(string) (int64, error) {
		reads++
		if reads == 1 || stopped {
			return 900 * config.Gigabyte.Bytes(), nil
		}
		return 100 * config.Gigabyte.Bytes(), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if !first.StoppedForSpace || !first.TooShort {
		t.Fatalf("CycleResult = %+v, want a capture stopped at the watermark and discarded as too short", first)
	}
	stopped = true
	// The next poll of the same broadcast, so the capture name differs.
	h.at(now.Add(5 * time.Minute))

	second, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if !second.Refused {
		t.Errorf("CycleResult = %+v, want the second poll of the same broadcast refused", second)
	}
	if got := h.engine.captureCount(); got != 1 {
		t.Errorf("captures = %d, want 1: the same broadcast must not be attempted again", got)
	}
}

func TestRunCycle_ACaptureWorthKeepingResumesOnceThereIsRoom(t *testing.T) {
	// A capture the watermark stopped after it was long enough to keep is in
	// the library. Room made mid-broadcast records the rest of it, which is
	// the reconnect shape the gap detector already understands.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 0
		cfg.Space.MinFree = 100 * config.Gigabyte
	})
	h.daemon.spaceInterval = 10 * time.Millisecond
	h.live("a title")
	h.captured(5_000_000, time.Hour)
	h.engine.blockUntilCancel = true

	stopped := false
	reads := 0
	h.daemon.freeSpace = func(string) (int64, error) {
		reads++
		if reads == 1 || stopped {
			return 900 * config.Gigabyte.Bytes(), nil
		}
		return 100 * config.Gigabyte.Bytes(), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if !first.StoppedForSpace || first.TooShort {
		t.Fatalf("CycleResult = %+v, want a capture stopped at the watermark and kept", first)
	}

	stopped = true
	h.engine.blockUntilCancel = false
	// The next poll of the same broadcast, so the capture name differs.
	h.at(now.Add(5 * time.Minute))

	second, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if second.Refused {
		t.Errorf("CycleResult = %+v, want the rest of the broadcast recorded once there is room", second)
	}
	if got := h.engine.captureCount(); got != 2 {
		t.Errorf("captures = %d, want 2: room was made and the broadcast is still running", got)
	}
}

// ///////////////////////////////////////////////
// A capture that stopped writing
// ///////////////////////////////////////////////

func TestCapture_EndsACaptureThatStoppedGrowing(t *testing.T) {
	// Probe has a timeout and Capture has none, so a write blocked on a
	// volume that stopped answering blocks in cmd.Wait forever. RunCycle
	// never returns, that channel is never polled again, nothing is logged
	// or notified, and the row stays capturing for the life of the process,
	// which also stands the recovery pass down. Every broadcast from that
	// moment is missed on the only channel this operator watches.
	h := newHarness(t, nil)
	h.daemon.spaceInterval = 5 * time.Millisecond
	h.daemon.captureStall = 25 * time.Millisecond
	h.live("a title")
	h.captured(4096, time.Hour)
	h.engine.blockUntilCancel = true

	// Opened, written once, and never touched again.
	h.engine.onCapture = func(req record.Request) {
		if err := os.WriteFile(req.Output, []byte("some bytes"), 0o644); err != nil {
			t.Errorf("writing the capture: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil: the bytes already written are worth keeping", err)
	}
	if !got.Stalled {
		t.Fatalf("CycleResult = %+v, want the capture ended for having stopped writing", got)
	}
	if !h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want the operator told the capture stopped writing", h.notifier.kinds())
	}

	// The bytes reach the library. A capture cut off part way is complete as
	// far as it goes, exactly like one stopped at the watermark.
	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.State != store.StateComplete {
		t.Errorf("State = %q, want %q for a capture cut off with bytes on disk",
			recording.State, store.StateComplete)
	}
}

func TestCapture_LeavesAGrowingCaptureAlone(t *testing.T) {
	// The companion that stops the stall rule ending an ordinary recording.
	// A file that grows is a capture doing its job however long it runs.
	//
	// The rule counts consecutive samples that saw no growth, so the margin
	// here is a number of samples rather than a duration: 500ms over a 5ms
	// interval is 100 in a row. A shorter limit is not a weaker version of
	// this test, it is a different one, because a loaded runner can starve
	// the writer below for longer than a handful of samples and the rule
	// then fires on a file that really did stop growing.
	h := newHarness(t, nil)
	h.daemon.spaceInterval = 5 * time.Millisecond
	h.daemon.captureStall = 500 * time.Millisecond
	h.live("a title")
	h.captured(4096, time.Hour)
	h.engine.blockUntilCancel = true

	writing := make(chan struct{})
	h.engine.onCapture = func(req record.Request) {
		go func() {
			for {
				select {
				case <-writing:
					return
				default:
				}
				file, err := os.OpenFile(req.Output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					return
				}
				file.WriteString("v")
				file.Close()
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	go func() {
		// Four times the stall window, so the rule had every chance to fire
		// and a capture still running was left alone rather than simply not
		// watched for long enough.
		time.Sleep(2 * time.Second)
		cancel()
	}()
	defer cancel()

	got, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	close(writing)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCycle() err = %v, want nil or cancellation", err)
	}
	if got.Stalled {
		t.Errorf("CycleResult = %+v, want a growing capture left alone", got)
	}
}

func TestUsage_CountsTheBytesACaptureHasAlreadyWritten(t *testing.T) {
	// A capturing row carries no size until the capture ends, so a library
	// position taken from the recordings table alone cannot see the file
	// currently being written into it.
	h := newHarness(t, nil)

	finished, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "done.mkv",
		State: store.StateComplete, Origin: store.OriginLive, StartedAt: now.Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	if err := h.store.FinishRecording(finished.ID, store.StateComplete, "done.mkv",
		1000, time.Hour, now.Add(-47*time.Hour)); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}
	h.interrupted(filepath.Join("incoming", "twitch-examplechannel-1.ts"), strings.Repeat("x", 500))

	got, err := h.daemon.usage()
	if err != nil {
		t.Fatalf("usage() err = %v, want nil", err)
	}
	if got.LibraryBytes != 1500 {
		t.Errorf("LibraryBytes = %d, want 1500: the 1000 already recorded plus the 500 on disk", got.LibraryBytes)
	}
}

func TestUsage_CountsAnUnclaimedFileInIncoming(t *testing.T) {
	// A backfill download writes into the incoming directory before any row
	// names it, so the size cap cannot see it while it runs. min_free is a
	// real statfs and sees it; max_size does not, and the two disagreeing is
	// how a download fills a library the cap reports as having room.
	h := newHarness(t, nil)

	finished, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "done.mkv",
		State: store.StateComplete, Origin: store.OriginLive, StartedAt: now.Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	if err := h.store.FinishRecording(finished.ID, store.StateComplete, "done.mkv",
		1000, time.Hour, now.Add(-47*time.Hour)); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}

	full := filepath.Join(h.root, paths.IncomingDirName, "twitch-examplechannel-1772658900.mp4.part")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating the incoming directory: %v", err)
	}
	if err := os.WriteFile(full, []byte(strings.Repeat("x", 700)), 0o644); err != nil {
		t.Fatalf("writing the download: %v", err)
	}

	got, err := h.daemon.usage()
	if err != nil {
		t.Fatalf("usage() err = %v, want nil", err)
	}
	if got.LibraryBytes != 1700 {
		t.Errorf("LibraryBytes = %d, want 1700: the 1000 recorded plus the 700 a download holds",
			got.LibraryBytes)
	}
}

func TestUsage_DoesNotCountACaptureTwice(t *testing.T) {
	// The companion. A capture in flight is counted through its row, and
	// counting it again as an unclaimed file would double every recording
	// being made and refuse the next broadcast at half the real cap.
	h := newHarness(t, nil)
	h.interrupted(filepath.Join(paths.IncomingDirName, "twitch-examplechannel-1.ts"),
		strings.Repeat("x", 500))

	got, err := h.daemon.usage()
	if err != nil {
		t.Fatalf("usage() err = %v, want nil", err)
	}
	if got.LibraryBytes != 500 {
		t.Errorf("LibraryBytes = %d, want 500 counted once", got.LibraryBytes)
	}
}

func TestUsage_ACaptureWithNoFileYetHoldsNothing(t *testing.T) {
	// The row is created before the engine opens the output, so an absent
	// file is the ordinary opening moment of every capture.
	h := newHarness(t, nil)
	h.interrupted(filepath.Join("incoming", "twitch-examplechannel-1.ts"), "")

	got, err := h.daemon.usage()
	if err != nil {
		t.Fatalf("usage() err = %v, want nil", err)
	}
	if got.LibraryBytes != 0 {
		t.Errorf("LibraryBytes = %d, want 0", got.LibraryBytes)
	}
}

func TestRunCycle_TheSizeCapStopsACaptureItCanSeeGrowing(t *testing.T) {
	// With no free-space floor configured, the size cap is the only limit
	// left, and a cap that cannot see the recording being written under it
	// never fires at all. The volume then fills, which corrupts the
	// recording and everything else sharing the disk.
	// The cap is chosen so an empty library is not already critical and the
	// capture's own growth is what crosses the margin. A cap breached
	// before a byte was written would prove nothing about whether the
	// watermark can see a file being written under it.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Space.MaxSize = 120 * config.Megabyte
		cfg.Space.MinFree = 0
	})
	h.daemon.spaceInterval = 10 * time.Millisecond
	h.live("a title")
	h.captured(70<<20, 2*time.Hour)
	h.engine.blockUntilCancel = true

	// A cheap estimate, so admission passes on a cap this small.
	history, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "history.mkv",
		State: store.StateComplete, Origin: store.OriginLive, StartedAt: now.Add(-48 * time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	if err := h.store.FinishRecording(history.ID, store.StateComplete, "history.mkv",
		3600, time.Hour, now.Add(-47*time.Hour)); err != nil {
		t.Fatalf("FinishRecording() err = %v, want nil", err)
	}

	// The engine puts a file of a capture's size where a real one would.
	// Only its length matters here, so it is sized rather than filled.
	h.engine.onCapture = func(req record.Request) {
		file, err := os.Create(req.Output)
		if err != nil {
			t.Errorf("creating the capture: %v", err)
			return
		}
		if err := file.Truncate(70 << 20); err != nil {
			t.Errorf("sizing the capture: %v", err)
		}
		file.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if !got.StoppedForSpace {
		t.Fatalf("CycleResult = %+v, want the capture stopped by the size cap", got)
	}
	if !h.notifier.has(EventLibraryFull) {
		t.Errorf("events = %v, want a library-full notification", h.notifier.kinds())
	}
}

func TestWatchCapture_ReportsAWatermarkThatCannotReadTheDisk(t *testing.T) {
	// A free-space read that keeps failing turns the only automatic
	// protection against filling the volume off, and a warning every thirty
	// seconds forever is not a report anybody acts on.
	h := newHarness(t, nil)
	h.daemon.spaceInterval = time.Millisecond
	// Long enough that the stall rule never fires inside this test, which is
	// about the watermark rather than about a capture that stopped writing.
	h.daemon.captureStall = time.Hour
	h.daemon.freeSpace = func(string) (int64, error) { return 0, errors.New("the volume is gone") }

	ctx, cancel := context.WithCancel(context.Background())
	stopped, stalled := &atomic.Bool{}, &atomic.Bool{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.watchCapture(ctx, h.entry, "incoming/capture.ts", stopped, stalled, cancel)
	}()

	select {
	case <-h.notifier.signal(EventFailure):
	case <-time.After(eventWaitBudget):
		cancel()
		<-done
		t.Fatal("a watermark blind for the whole capture reported nothing, want it escalated")
	}

	// And it says so once, however long the outage runs.
	time.Sleep(50 * time.Millisecond)
	if got := h.notifier.count(EventFailure); got != 1 {
		t.Errorf("a single outage reported %d times, want 1", got)
	}
	if stopped.Load() {
		t.Error("the capture was stopped, want the recording kept while the operator is told")
	}

	cancel()
	<-done
}

func TestRunCycle_HealthyLibraryRunsToCompletion(t *testing.T) {
	h := newHarness(t, nil)
	h.daemon.spaceInterval = 10 * time.Millisecond
	h.live("a title")
	h.captured(1000, time.Hour)

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if got.StoppedForSpace {
		t.Error("StoppedForSpace = true, want false with room to spare")
	}
}

// ///////////////////////////////////////////////
// Failure and short recordings
// ///////////////////////////////////////////////

func TestRunCycle_CaptureFailureKeepsTheBytes(t *testing.T) {
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(512, time.Minute)
	h.engine.captureErr = errors.New("streamlink died")

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err == nil {
		t.Fatal("RunCycle() err = nil, want the capture failure surfaced")
	}

	recording, storeErr := h.store.Recording(got.RecordingID)
	if storeErr != nil {
		t.Fatalf("Recording() err = %v, want nil", storeErr)
	}
	if recording.State != store.StateFailed {
		t.Errorf("State = %q, want %q", recording.State, store.StateFailed)
	}
	if recording.Bytes != 512 {
		t.Errorf("Bytes = %d, want the bytes that reached disk recorded", recording.Bytes)
	}
	if !h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want a failure notification", h.notifier.kinds())
	}
	if len(h.finalizer.calls) != 0 {
		t.Error("a failed capture was organized into the library, want it left alone")
	}
}

func TestCapture_SurvivesAClockStep(t *testing.T) {
	// Both timestamps are wall clock, and the store refuses an end before
	// its start inside the transaction. A backward step then leaves the row
	// stuck capturing across restarts, its size never recorded, its file in
	// incoming, the day painted at risk so backfill will not fetch it, and
	// the recovery pass stood down for the life of the process. A bad RTC or
	// a restored VM snapshot is what does it.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1000, time.Hour)
	h.engine.onCapture = func(record.Request) { h.at(now.Add(-2 * time.Hour)) }

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want the clock step survived", err)
	}

	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.State == store.StateCapturing {
		t.Errorf("State = %q, want the recording moved out of capturing", recording.State)
	}
	if recording.Bytes != 1000 {
		t.Errorf("Bytes = %d, want the capture's size recorded", recording.Bytes)
	}
}

func TestRunCycle_ShortRecordingIsKeptButNotOrganized(t *testing.T) {
	// A connection test must not enter the library, and it must not be
	// deleted either: the daemon never removes a recording.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Capture.MinDuration = config.Duration(2 * time.Minute)
	})
	h.live("a title")
	h.captured(1000, 10*time.Second)

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if !got.TooShort {
		t.Errorf("CycleResult = %+v, want it flagged too short", got)
	}

	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.State != store.StateFailed {
		t.Errorf("State = %q, want %q", recording.State, store.StateFailed)
	}
	if recording.Bytes != 1000 {
		t.Errorf("Bytes = %d, want the recording still accounted for", recording.Bytes)
	}
	if len(h.finalizer.calls) != 0 {
		t.Error("a too-short recording was organized, want it left out of the library")
	}
}

func TestRunCycle_AnEarlyEngineExitIsReportedAndLeavesAGap(t *testing.T) {
	// The engine's exit status is the only sign that a capture stopped for a
	// reason of its own. Filing it as complete makes a two-hour recording of
	// a six-hour broadcast indistinguishable from a whole one: no
	// notification, and a calendar that counts the missing hours as covered.
	h := newHarness(t, nil)
	h.live("a title")
	h.engine.captureResult = record.Result{
		Bytes: 1 << 30, StartedAt: now, EndedAt: now.Add(2 * time.Hour), ExitCode: 2,
	}
	// The broadcast runs on for another hour after the engine quits.
	h.engine.onCapture = func(record.Request) { h.at(now.Add(3 * time.Hour)) }

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want the bytes still kept and organized", err)
	}
	if !h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want the early exit reported", h.notifier.kinds())
	}

	gaps, err := h.store.Gaps(got.RecordingID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want the hole between the capture and the broadcast recorded", gaps)
	}
	if gaps[0].Start != 2*time.Hour || gaps[0].End != 3*time.Hour {
		t.Errorf("gap = %s to %s, want 2h0m0s to 3h0m0s", gaps[0].Start, gaps[0].End)
	}
}

func TestReportEarlyStop_MeasuresFromTheBroadcastStart(t *testing.T) {
	// A gap's offsets are broadcast-relative everywhere else: the store's own
	// contract, the detector, and the patcher all read them that way. Measured
	// from this cycle's poll instead, the second capture of one broadcast
	// files its hole hours out of place, and the patcher downloads footage
	// from the wrong part of the stream and marks the hole filled.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1<<30, time.Hour)

	first, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("first RunCycle() err = %v, want nil", err)
	}

	// The channel never went offline, so this is the same broadcast an hour
	// in. The engine quits at hour two and the broadcast runs on to hour
	// three.
	h.at(now.Add(time.Hour))
	h.engine.captureResult = record.Result{
		Bytes: 1 << 30, StartedAt: now.Add(time.Hour), EndedAt: now.Add(2 * time.Hour), ExitCode: 2,
	}
	h.engine.onCapture = func(record.Request) { h.at(now.Add(3 * time.Hour)) }

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Fatalf("second RunCycle() err = %v, want nil", err)
	}

	// Every gap of one broadcast hangs off its earliest recording, which is
	// the row a reader looks at to ask what the broadcast is missing.
	gaps, err := h.store.Gaps(first.RecordingID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(gaps) != 1 {
		t.Fatalf("gaps on the earliest recording = %+v, want the hole recorded there", gaps)
	}
	if gaps[0].Start != 2*time.Hour || gaps[0].End != 3*time.Hour {
		t.Errorf("gap = %s to %s, want 2h0m0s to 3h0m0s measured from the broadcast's start",
			gaps[0].Start, gaps[0].End)
	}
}

func TestRunCycle_ANormalEndLeavesNoGap(t *testing.T) {
	// A broadcast that ended on its own is whole. A gap on it would report
	// a hole in a recording that has none.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1<<30, 2*time.Hour)
	h.engine.onCapture = func(record.Request) { h.at(now.Add(3 * time.Hour)) }

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want no failure for a broadcast that ended normally", h.notifier.kinds())
	}

	gaps, err := h.store.Gaps(got.RecordingID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps = %+v, want none", gaps)
	}
}

func TestRunCycle_AnEarlyExitAfterTheBroadcastEndedLeavesNoGap(t *testing.T) {
	// A channel that has gone offline stopped when the capture did. The
	// exit still deserves a report, but nothing is missing.
	h := newHarness(t, nil)
	h.live("a title")
	h.engine.captureResult = record.Result{
		Bytes: 1 << 30, StartedAt: now, EndedAt: now.Add(2 * time.Hour), ExitCode: 1,
	}
	h.engine.onCapture = func(record.Request) {
		h.at(now.Add(3 * time.Hour))
		h.engine.mu.Lock()
		defer h.engine.mu.Unlock()
		h.engine.probe.Live = false
	}

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}
	if !h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want the early exit still reported", h.notifier.kinds())
	}

	gaps, err := h.store.Gaps(got.RecordingID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(gaps) != 0 {
		t.Errorf("gaps = %+v, want none for a broadcast that had already ended", gaps)
	}
}

func TestReportEarlyStop_FilesTheGapWhenTheConfirmingProbeFails(t *testing.T) {
	// A credential that dies mid-capture, which a password change or a
	// "sign out everywhere" does, makes streamlink exit non-zero and makes
	// the confirming probe fail on the same dead credential. Folding "could
	// not answer" together with "the channel is offline" writes the hole
	// down nowhere, and the calendar shows the day fully covered.
	h := newHarness(t, nil)
	h.live("a title")
	h.engine.captureResult = record.Result{
		Bytes: 1 << 30, StartedAt: now, EndedAt: now.Add(2 * time.Hour), ExitCode: 1,
	}
	h.engine.onCapture = func(record.Request) {
		h.at(now.Add(3 * time.Hour))
		h.engine.mu.Lock()
		defer h.engine.mu.Unlock()
		h.engine.probeErr = errors.New("Unauthorized: the token is invalid")
	}

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	gaps, err := h.store.Gaps(got.RecordingID)
	if err != nil {
		t.Fatalf("Gaps() err = %v, want nil", err)
	}
	if len(gaps) != 1 {
		t.Fatalf("gaps = %+v, want the hole recorded: a probe that errored proves nothing", gaps)
	}
	if gaps[0].Start != 2*time.Hour || gaps[0].End != 3*time.Hour {
		t.Errorf("gap = %s to %s, want 2h0m0s to 3h0m0s", gaps[0].Start, gaps[0].End)
	}
}

func TestRunCycle_FinalizeFailureIsReported(t *testing.T) {
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1000, time.Hour)
	h.finalizer.err = errors.New("remux failed")

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err == nil {
		t.Error("RunCycle() err = nil, want the finalize failure surfaced")
	}
	if !h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want a failure notification", h.notifier.kinds())
	}
}

func TestRunCycle_AFinalizeCancelledByShutdownIsNotAFailure(t *testing.T) {
	// Finalizing shells out to ffmpeg, so a stop signal arriving while one
	// is running kills it and the finalize reports the death. That is the
	// operator stopping the recorder, not a recording that failed, and the
	// row already says the work is outstanding.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1<<30, 2*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.finalizer.err = errors.New("ffmpeg was killed")
	h.finalizer.onCall = cancel

	got, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCycle() err = %v, want the cancellation rather than the finalize failure", err)
	}
	if h.notifier.has(EventFailure) {
		t.Errorf("events = %v, want no failure raised by an orderly stop", h.notifier.kinds())
	}

	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if !slices.Contains(store.PendingStates, recording.State) {
		t.Errorf("State = %q, want one of %q so the next start finishes it",
			recording.State, store.PendingStates)
	}
}

func TestSweepParked_AShutdownDoesNotCountAgainstARecording(t *testing.T) {
	// A sweep cancelled part way through has not learned anything about the
	// recording it was working on. Counting it would call a recording stuck
	// for the operator stopping the daemon.
	h := newHarness(t, nil)
	if _, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "incoming/pending.mkv",
		State: store.StateAwaitingFinalize, Origin: store.OriginLive, StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.finalizer.err = errors.New("ffmpeg was killed")
	h.finalizer.onCall = cancel

	for range sweepFailureLimit + 1 {
		if _, err := h.daemon.SweepParked(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("SweepParked() err = %v, want the cancellation reported", err)
		}
	}
	if got := h.notifier.count(EventFailure); got != 0 {
		t.Errorf("a cancelled sweep reported %d failures, want none", got)
	}
}

func TestRunCycle_ParkedRecordingIsNotAFailure(t *testing.T) {
	// A recording waiting on metadata is intact. The sweeper retries it,
	// so it must not be reported as a failure.
	h := newHarness(t, nil)
	h.live("a title")
	h.captured(1000, time.Hour)
	h.finalizer.outcome = organize.Outcome{Parked: true, Missing: []string{"title"}}

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Errorf("RunCycle() err = %v, want nil for a parked recording", err)
	}
	if h.notifier.has(EventFailure) {
		t.Error("a parked recording raised a failure notification, want none")
	}
}

// ///////////////////////////////////////////////
// Sweeping
// ///////////////////////////////////////////////

func TestSweepParked(t *testing.T) {
	h := newHarness(t, nil)

	for _, path := range []string{"incoming/a.mkv", "incoming/b.mkv"} {
		recording, err := h.store.CreateRecording(store.Recording{
			ChannelID: h.channel.ID, Path: path,
			State: store.StateAwaitingMetadata, Origin: store.OriginLive, StartedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateRecording() err = %v, want nil", err)
		}
		_ = recording
	}
	h.finalizer.outcome = organize.Outcome{Path: "named.mkv", Renamed: true}

	completed, err := h.daemon.SweepParked(context.Background())
	if err != nil {
		t.Fatalf("SweepParked() err = %v, want nil", err)
	}
	if completed != 2 {
		t.Errorf("SweepParked() completed %d, want 2", completed)
	}
	if len(h.finalizer.calls) != 2 {
		t.Errorf("finalizer calls = %v, want one per parked recording", h.finalizer.calls)
	}
}

func TestRunCycle_AFailedFinalizeLeavesTheRecordingForTheSweep(t *testing.T) {
	// Capture ending does not put a recording in the library. Anything that
	// fails between the two has to leave a row the sweep will retry: a row
	// marked complete is a multi-gigabyte file sitting in incoming/ that
	// nothing ever looks at again, while the calendar counts the day as
	// covered.
	h := newHarness(t, nil)
	h.engine.probe = record.Probe{Live: true}
	h.engine.captureResult = record.Result{Bytes: 1 << 30, StartedAt: now, EndedAt: now.Add(2 * time.Hour)}
	h.finalizer.err = errors.New("ffmpeg is not installed")

	got, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel)
	if err == nil {
		t.Fatal("RunCycle() err = nil, want the finalize failure reported")
	}

	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.State == store.StateComplete {
		t.Error("State = complete, want a recording that never reached the library left pending")
	}
	if !slices.Contains(store.PendingStates, recording.State) {
		t.Errorf("State = %q, want one of %q so the sweep retries it",
			recording.State, store.PendingStates)
	}

	queued, err := h.store.RecordingsByState(store.PendingStates...)
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}
	if len(queued) != 1 || queued[0].ID != got.RecordingID {
		t.Errorf("sweep queue = %v, want the failed recording", queued)
	}
}

func TestRunCycle_ShutdownLeavesTheRecordingForTheNextStart(t *testing.T) {
	// Cancelling kills streamlink, which exits non-zero, which the engine
	// reports as an ordinary finish. Taking the success path from there and
	// finalizing with the cancelled context fails instantly, so the
	// recording has to stay pending rather than be marked complete.
	h := newHarness(t, nil)
	h.engine.probe = record.Probe{Live: true}
	h.engine.captureResult = record.Result{Bytes: 1 << 30, StartedAt: now, EndedAt: now.Add(4 * time.Hour)}
	h.engine.blockUntilCancel = true

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	got, err := h.daemon.RunCycle(ctx, h.entry, h.channel)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCycle() err = %v, want nil or the cancellation", err)
	}
	if got.RecordingID == 0 {
		t.Fatal("no recording was created")
	}

	recording, err := h.store.Recording(got.RecordingID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if recording.State == store.StateComplete {
		t.Error("State = complete, want a recording still in incoming/ left pending")
	}

	queued, err := h.store.RecordingsByState(store.PendingStates...)
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}
	if len(queued) != 1 {
		t.Errorf("sweep queue holds %d recordings, want the interrupted one", len(queued))
	}
}

func TestSweepParked_RetriesEveryPendingState(t *testing.T) {
	// The sweep is the only thing that finishes a recording whose finalize
	// failed, so it has to cover every state that means "not in the library
	// yet". A state left out is a recording nothing looks at again.
	h := newHarness(t, nil)

	ids := make(map[store.State]int64, len(store.PendingStates))
	for i, state := range store.PendingStates {
		recording, err := h.store.CreateRecording(store.Recording{
			ChannelID: h.channel.ID, Path: fmt.Sprintf("incoming/pending-%d.mkv", i),
			State: store.StateCapturing, Origin: store.OriginLive, StartedAt: now,
		})
		if err != nil {
			t.Fatalf("CreateRecording() err = %v, want nil", err)
		}
		if err := h.store.SetState(recording.ID, state); err != nil {
			t.Fatalf("SetState(%q) err = %v, want nil", state, err)
		}
		ids[state] = recording.ID
	}
	h.finalizer.outcome = organize.Outcome{Path: "ExampleChannel/2026/named.mkv", Renamed: true}

	completed, err := h.daemon.SweepParked(context.Background())
	if err != nil {
		t.Fatalf("SweepParked() err = %v, want nil", err)
	}
	if completed != len(store.PendingStates) {
		t.Errorf("SweepParked() completed %d, want all %d pending recordings",
			completed, len(store.PendingStates))
	}

	for state, id := range ids {
		if !slices.Contains(h.finalizer.calls, id) {
			t.Errorf("recording in state %q was never retried, calls = %v", state, h.finalizer.calls)
		}
	}
}

func TestSweepParked_IncludesRecordingsHeldByAnotherProgram(t *testing.T) {
	// A backup agent can hold a large recording for hours, far longer than
	// the in-call wait. The sweep is the only thing that moves it, so a
	// recording parked on a lock has to be swept like any other.
	h := newHarness(t, nil)

	held, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "incoming/held.mkv",
		State: store.StateAwaitingFile, Origin: store.OriginLive, StartedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	h.finalizer.outcome = organize.Outcome{Path: "ExampleChannel/2026/named.mkv", Renamed: true}

	completed, err := h.daemon.SweepParked(context.Background())
	if err != nil {
		t.Fatalf("SweepParked() err = %v, want nil", err)
	}
	if completed != 1 {
		t.Errorf("SweepParked() completed %d, want the held recording retried", completed)
	}
	if len(h.finalizer.calls) != 1 || h.finalizer.calls[0] != held.ID {
		t.Errorf("finalizer calls = %v, want the held recording %d", h.finalizer.calls, held.ID)
	}
}

func TestSweepParked_StillParkedIsNotCounted(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "incoming/a.mkv",
		State: store.StateAwaitingMetadata, Origin: store.OriginLive, StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	h.finalizer.outcome = organize.Outcome{Parked: true, Missing: []string{"title"}}

	completed, err := h.daemon.SweepParked(context.Background())
	if err != nil {
		t.Fatalf("SweepParked() err = %v, want nil", err)
	}
	if completed != 0 {
		t.Errorf("SweepParked() completed %d, want 0 while it is still parked", completed)
	}
}

func TestSweepParked_OneFailureDoesNotStopTheRest(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"incoming/a.mkv", "incoming/b.mkv"} {
		if _, err := h.store.CreateRecording(store.Recording{
			ChannelID: h.channel.ID, Path: path,
			State: store.StateAwaitingMetadata, Origin: store.OriginLive, StartedAt: now,
		}); err != nil {
			t.Fatalf("CreateRecording() err = %v, want nil", err)
		}
	}
	h.finalizer.err = errors.New("disk error")

	completed, err := h.daemon.SweepParked(context.Background())
	if err != nil {
		t.Fatalf("SweepParked() err = %v, want nil", err)
	}
	if completed != 0 {
		t.Errorf("SweepParked() completed %d, want 0", completed)
	}
	if len(h.finalizer.calls) != 2 {
		t.Errorf("finalizer calls = %v, want every parked recording attempted", h.finalizer.calls)
	}
}

func TestSweepParked_EscalatesARecordingThatKeepsFailing(t *testing.T) {
	// A recording that fails every sweep is stuck, not delayed. Warning
	// about it every quarter of an hour reads exactly like the routine
	// waiting-on-a-lock message, so nobody ever looks.
	h := newHarness(t, nil)
	if _, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "incoming/wedged.mkv",
		State: store.StateAwaitingFinalize, Origin: store.OriginLive, StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	h.finalizer.err = errors.New("file already exists")

	for sweep := 1; sweep < sweepFailureLimit; sweep++ {
		if _, err := h.daemon.SweepParked(context.Background()); err != nil {
			t.Fatalf("SweepParked() err = %v, want nil", err)
		}
		if got := h.notifier.count(EventFailure); got != 0 {
			t.Fatalf("sweep %d reported %d failures, want the retries left quiet", sweep, got)
		}
	}

	if _, err := h.daemon.SweepParked(context.Background()); err != nil {
		t.Fatalf("SweepParked() err = %v, want nil", err)
	}
	if got := h.notifier.count(EventFailure); got != 1 {
		t.Errorf("a recording stuck across %d sweeps reported %d times, want 1", sweepFailureLimit, got)
	}

	// And it keeps quiet after, or the escalation becomes the flood the
	// warning is.
	if _, err := h.daemon.SweepParked(context.Background()); err != nil {
		t.Fatalf("SweepParked() err = %v, want nil", err)
	}
	if got := h.notifier.count(EventFailure); got != 1 {
		t.Errorf("the next sweep reported again, total %d, want 1", got)
	}
}

func TestSweepParked_ReportsARecordingTheOrganizerWillNotRetry(t *testing.T) {
	// A recording the organizer abandons leaves PendingStates in the same
	// sweep, so no later sweep can carry the count to its limit. This is the
	// only account the operator gets of a recording that never reached the
	// library.
	h := newHarness(t, nil)
	if _, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "incoming/abandoned.mkv",
		State: store.StateAwaitingFinalize, Origin: store.OriginLive, StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	h.finalizer.err = fmt.Errorf("remuxing recording 1: %w after 3 attempts", organize.ErrGaveUp)

	if _, err := h.daemon.SweepParked(context.Background()); err != nil {
		t.Fatalf("SweepParked() err = %v, want nil", err)
	}
	if got := h.notifier.count(EventFailure); got != 1 {
		t.Errorf("an abandoned recording reported %d times on the first sweep, want 1", got)
	}
	if got := len(h.daemon.sweepFailures); got != 0 {
		t.Errorf("the sweep is counting %d recordings, want none once one is abandoned", got)
	}
}

func TestSweepParked_LeavesARecordingAnotherCallIsFinalizing(t *testing.T) {
	// The capture goroutine finalizes what it just recorded, and a sweep
	// tick inside that window reaches the same row. It is neither stuck nor
	// this sweep's to finish.
	h := newHarness(t, nil)
	if _, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: "incoming/inflight.mkv",
		State: store.StateAwaitingFinalize, Origin: store.OriginLive, StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	h.finalizer.err = fmt.Errorf("recording 1: %w", organize.ErrBusy)

	for range sweepFailureLimit + 1 {
		completed, err := h.daemon.SweepParked(context.Background())
		if err != nil {
			t.Fatalf("SweepParked() err = %v, want nil", err)
		}
		if completed != 0 {
			t.Errorf("SweepParked() completed %d, want 0 while another call holds it", completed)
		}
	}

	if got := h.notifier.count(EventFailure); got != 0 {
		t.Errorf("a recording another call was finalizing reported %d failures, want none", got)
	}
}

// ///////////////////////////////////////////////
// The watch loop
// ///////////////////////////////////////////////

func TestPollDelay(t *testing.T) {
	// A channel that keeps failing re-runs a probe subprocess and an
	// outbound notification at full cadence for a channel that is not going
	// to answer.
	const poll = 30 * time.Second

	tests := []struct {
		name     string
		interval time.Duration
		failures int
		want     time.Duration
	}{
		{name: "a healthy channel polls at its interval", interval: poll, failures: 0, want: poll},
		{name: "the first failure does not wait longer", interval: poll, failures: 1, want: poll},
		{name: "the second doubles", interval: poll, failures: 2, want: 2 * poll},
		{name: "the fifth is sixteen times", interval: poll, failures: 5, want: 16 * poll},
		{name: "the backoff stops at the ceiling", interval: poll, failures: 20, want: maxFailureBackoff},
		{
			name:     "an interval past the ceiling is left alone",
			interval: 30 * time.Minute,
			failures: 6,
			want:     30 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pollDelay(tt.interval, tt.failures); got != tt.want {
				t.Errorf("pollDelay(%s, %d) = %s, want %s", tt.interval, tt.failures, got, tt.want)
			}
		})
	}
}

func TestWatch_ReportsOneFailurePerOutage(t *testing.T) {
	// A persistently failing channel emits a notification on every poll,
	// each one an outbound webhook. One per outage is what an operator can
	// act on. A hundred an hour is what they mute.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Capture.PollInterval = config.Duration(5 * time.Millisecond)
	})
	// Fails, fails, recovers, then fails again and stays failing. Scripting
	// it by probe rather than by clock keeps the count decidable.
	h.engine.probeScript = []error{
		errors.New("token expired"),
		errors.New("token expired"),
		nil,
		errors.New("token expired again"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	h.daemon.Watch(ctx, h.entry, h.channel)

	h.engine.mu.Lock()
	probes := h.engine.probes
	h.engine.mu.Unlock()
	if probes < 4 {
		t.Fatalf("engine probed %d times, want the whole script driven", probes)
	}
	if got := h.notifier.count(EventFailure); got != 2 {
		t.Errorf("two outages reported %d times, want 2", got)
	}
}

func TestWatch_EscalatesRepeatedRefusals(t *testing.T) {
	// The credential check asks Twitch's validation endpoint and the
	// recorder asks its playback endpoint, and the two are different judges:
	// a token issued to another application validates cleanly and offers no
	// streams at all. In that state the recorder captures nothing
	// indefinitely while the hourly check confirms the credential is
	// healthy, so the refusals themselves have to say something.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Capture.PollInterval = config.Duration(2 * time.Millisecond)
		cfg.Notify.OnFailure = true
	})
	h.engine.probeErr = fmt.Errorf("probing: %w", record.ErrUnauthorized)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	h.daemon.Watch(ctx, h.entry, h.channel)

	h.engine.mu.Lock()
	probes := h.engine.probes
	h.engine.mu.Unlock()
	if probes < refusalLimit {
		t.Fatalf("engine probed %d times, want at least %d so the threshold is reached", probes, refusalLimit)
	}
	if got := h.notifier.count(EventCredentialDead); got != 1 {
		t.Errorf("refusals reported %d times, want exactly 1 however long the outage runs", got)
	}
}

func TestWatch_DoesNotEscalateAnOrdinaryFailure(t *testing.T) {
	// The companion. A network failure is not a credential the platform is
	// refusing, and reporting one as the other sends an operator to
	// re-authenticate over a router that rebooted.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Capture.PollInterval = config.Duration(2 * time.Millisecond)
		cfg.Notify.OnFailure = true
	})
	h.engine.probeErr = errors.New("dial tcp: connection refused")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	h.daemon.Watch(ctx, h.entry, h.channel)

	if got := h.notifier.count(EventCredentialDead); got != 0 {
		t.Errorf("a network outage was reported as a refused credential %d times, want 0", got)
	}
}

func TestWatch_StopsOnCancellation(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Capture.PollInterval = config.Duration(10 * time.Millisecond)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.Watch(ctx, h.entry, h.channel)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(eventWaitBudget):
		t.Error("Watch() did not return after cancellation")
	}
}

func TestWatch_KeepsPollingAfterAFailedCycle(t *testing.T) {
	// A channel that errors once and is never polled again is precisely
	// the silent failure this tool exists to prevent.
	h := newHarness(t, func(cfg *config.Config) {
		cfg.Capture.PollInterval = config.Duration(10 * time.Millisecond)
	})
	h.engine.probeErr = errors.New("transient network failure")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	h.daemon.Watch(ctx, h.entry, h.channel)

	h.engine.mu.Lock()
	probes := h.engine.probes
	h.engine.mu.Unlock()

	if probes < 2 {
		t.Errorf("engine probed %d times, want the loop to continue past a failure", probes)
	}
}

// ///////////////////////////////////////////////
// Reachability
// ///////////////////////////////////////////////

func TestRunCycle_RecordsThatThePlatformAnsweredAProbe(t *testing.T) {
	// Recorded from the probe rather than from the whole cycle. A cycle
	// lasts as long as the broadcast it captures, so a channel that went
	// live an hour ago would otherwise have reported nothing since, and the
	// recovery round would be held against a platform that is plainly
	// answering.
	h := newHarness(t, nil)
	h.live("a broadcast")

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err != nil {
		t.Fatalf("RunCycle() err = %v, want nil", err)
	}

	last, seen := h.probeRecord()
	if !seen {
		t.Fatal("the probe was not recorded, want the platform noted as reachable")
	}
	if !last.answered {
		t.Error("the probe is recorded as unanswered, want it answered")
	}
	if h.outageOpen() {
		t.Error("an outage is open after an answered probe, want none")
	}
}

func TestRunCycle_DatesAnOutageFromAProbeThePlatformDidNotAnswer(t *testing.T) {
	h := newHarness(t, nil)
	h.engine.probeErr = errors.New("dial tcp: connection refused")

	if _, err := h.daemon.RunCycle(context.Background(), h.entry, h.channel); err == nil {
		t.Fatal("RunCycle() err = nil, want the probe failure surfaced")
	}

	last, seen := h.probeRecord()
	if !seen || last.answered {
		t.Errorf("probe recorded as answered=%v seen=%v, want a failure recorded", last.answered, seen)
	}
	if last.platform != config.PlatformTwitch {
		t.Errorf("probe recorded against %q, want the channel's own platform", last.platform)
	}
	if !h.outageOpen() {
		t.Error("no outage is open after a failed probe, want one dated from it")
	}
}

func TestRunCycle_DoesNotReadAShutdownAsAnOutage(t *testing.T) {
	// A probe fails on the way down because the recorder is stopping, which
	// says nothing about the platform. Counted as an outage it would have
	// every clean shutdown queue a round for the next start to run, and the
	// first thing a restarted recorder did would be to fetch a stretch it
	// had not missed.
	h := newHarness(t, nil)
	h.engine.probeErr = errors.New("context canceled")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.daemon.RunCycle(ctx, h.entry, h.channel); err == nil {
		t.Fatal("RunCycle() err = nil, want the probe failure surfaced")
	}

	if _, seen := h.probeRecord(); seen {
		t.Error("a cancelled probe was recorded, want it ignored")
	}
	if h.outageOpen() {
		t.Error("a cancelled probe opened an outage, want none")
	}
}

// ///////////////////////////////////////////////
// Where a live broadcast's title comes from
// ///////////////////////////////////////////////

// titledBroadcast opens a broadcast for the harness channel to hang titles
// on, and returns it.
func titledBroadcast(t *testing.T, h *harness, streamID string) store.Broadcast {
	t.Helper()

	broadcast, err := h.store.UpsertBroadcast(store.Broadcast{
		ChannelID: h.channel.ID,
		StreamID:  streamID,
		StartedAt: now,
		Source:    store.SourceLive,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	return broadcast
}

func TestPollMetadata_RecordsATitleTheProbeNeverCarried(t *testing.T) {
	// The defect this exists for. A probe that answers with an empty
	// metadata block leaves the broadcast row untitled, and the organizer
	// will not name a recording without one: it waits in the incoming
	// directory indefinitely, which is where a two-hour capture sat.
	h := newHarness(t, nil)
	broadcast := titledBroadcast(t, h, "stream-1")
	h.engine.probe = record.Probe{Live: true, Qualities: []string{"best"}}
	h.daemon.liveMetadata = func(context.Context, string) (LiveBroadcast, bool) {
		return LiveBroadcast{StartedAt: now, Title: "what the platform says", Category: "Just Chatting"}, true
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.daemon.pollMetadata(ctx, h.entry, broadcast, time.Millisecond)
	}()

	deadline := time.Now().Add(10 * time.Second)
	var titled store.Broadcast
	for time.Now().Before(deadline) {
		stored, err := h.store.Broadcast(broadcast.ID)
		if err == nil && stored.Title != "" {
			titled = stored
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if titled.Title != "what the platform says" {
		t.Errorf("broadcast title = %q, want the platform's answer", titled.Title)
	}
}

func TestPollMetadata_StopsAskingAPlatformThatKeepsAnsweringNothing(t *testing.T) {
	// An API nobody authorized answers the same way on every tick, and this
	// runs for the life of the capture. The recovery round supplies the
	// title afterwards either way.
	h := newHarness(t, nil)
	broadcast := titledBroadcast(t, h, "stream-1")
	h.engine.probe = record.Probe{Live: true, Qualities: []string{"best"}}

	var lookups atomic.Int32
	h.daemon.liveMetadata = func(context.Context, string) (LiveBroadcast, bool) {
		lookups.Add(1)
		return LiveBroadcast{}, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.daemon.pollMetadata(ctx, h.entry, broadcast, time.Millisecond)

	if got := lookups.Load(); got != metadataLookupLimit {
		t.Errorf("asked the platform %d times, want it to stop after %d", got, metadataLookupLimit)
	}
}

// Enabled implements slog.Handler.
func (c *countingLog) Enabled(context.Context, slog.Level) bool { return true }

// WithAttrs implements slog.Handler.
func (c *countingLog) WithAttrs([]slog.Attr) slog.Handler { return c }

// WithGroup implements slog.Handler.
func (c *countingLog) WithGroup(string) slog.Handler { return c }

// Handle implements slog.Handler, keeping only the failure counts.
func (c *countingLog) Handle(_ context.Context, record slog.Record) error {
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "consecutive_failures" {
			c.mu.Lock()
			c.counts = append(c.counts, attr.Value.Int64())
			c.mu.Unlock()
		}
		return true
	})
	return nil
}

// seen returns the counts reported so far.
func (c *countingLog) seen() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.counts)
}

func TestPollMetadata_KeepsReportingFailuresAfterAnOfflineReading(t *testing.T) {
	// An offline probe is an answer, not a failure. Left counting over one,
	// the run of failures after it reads as a continuation of a run that had
	// already ended, and a throttle on the count then reports none of them.
	// That is how a capture comes to fail for hours with an empty log.
	h := newHarness(t, nil)
	broadcast := titledBroadcast(t, h, "stream-1")
	counts := &countingLog{}
	h.daemon.logger = slog.New(counts)
	h.engine.probe = record.Probe{Live: false}
	h.engine.probeScript = []error{
		errors.New("the probe failed"),
		nil, // offline, which is an answer and must clear the count
		errors.New("the probe failed again"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.daemon.pollMetadata(ctx, h.entry, broadcast, time.Millisecond)

	// The third scripted answer repeats, so the failures after the offline
	// reading climb from one. Left unreset they would climb from two.
	got := counts.seen()
	if len(got) < 3 {
		t.Fatalf("reported %d failure counts, want at least 3", len(got))
	}
	if want := []int64{1, 1, 2}; !slices.Equal(got[:3], want) {
		t.Errorf("failure counts were %v, want %v", got[:3], want)
	}
}

func TestDescribeBroadcast_RefusesABroadcastThatBeganAfterTheOneBeingCaptured(t *testing.T) {
	// A channel that ends one broadcast and opens another while this
	// capture drains would otherwise have the new title written onto the
	// old recording, and the organizer files it under that name for good.
	//
	// The start is what settles it here. The session id travels in the same
	// metadata block the title does, so a block empty enough to need this
	// lookup carried no id to compare against.
	tests := []struct {
		name    string
		began   time.Duration
		id      string
		liveID  string
		wantErr bool
	}{
		{name: "the same broadcast", began: 0},
		{name: "a moment later, still this one", began: 5 * time.Minute},
		{name: "a broadcast that began afterwards", began: sameBroadcastWindow + time.Minute, wantErr: true},
		{name: "an hour later", began: time.Hour, wantErr: true},
		{name: "ids disagree", id: "stream-1", liveID: "stream-2", wantErr: true},
		{name: "ids agree", id: "stream-1", liveID: "stream-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			broadcast := titledBroadcast(t, h, tt.id)
			h.daemon.liveMetadata = func(context.Context, string) (LiveBroadcast, bool) {
				return LiveBroadcast{
					StreamID:  tt.liveID,
					StartedAt: broadcast.StartedAt.Add(tt.began),
					Title:     "the other broadcast",
				}, true
			}

			title, _ := h.daemon.describeBroadcast(context.Background(), h.entry, broadcast)

			if tt.wantErr && title != "" {
				t.Errorf("took the title %q from a different broadcast, want it refused", title)
			}
			if !tt.wantErr && title == "" {
				t.Error("refused a title from the broadcast being captured, want it taken")
			}
		})
	}
}

func TestDescribeBroadcast_AsksNobodyWithNoLookupWiredUp(t *testing.T) {
	// A daemon built without the hook keeps whatever the probe carried.
	h := newHarness(t, nil)
	broadcast := titledBroadcast(t, h, "stream-1")

	if title, _ := h.daemon.describeBroadcast(context.Background(), h.entry, broadcast); title != "" {
		t.Errorf("describeBroadcast() = %q with no lookup wired up, want empty", title)
	}
}

func TestDescribeBroadcast_AsksNobodyOnTheWayDown(t *testing.T) {
	// A shutdown mid-capture must not be reported as a metadata failure,
	// the noise class the probe leg above already refuses to raise.
	h := newHarness(t, nil)
	broadcast := titledBroadcast(t, h, "stream-1")
	asked := false
	h.daemon.liveMetadata = func(context.Context, string) (LiveBroadcast, bool) {
		asked = true
		return LiveBroadcast{StartedAt: now, Title: "a title"}, true
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.daemon.describeBroadcast(ctx, h.entry, broadcast)

	if asked {
		t.Error("asked the platform while shutting down, want the lookup skipped")
	}
}
