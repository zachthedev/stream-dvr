package organize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/deps"
	"zach.tools/go/stream-dvr/internal/fsretry"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/naming"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/store"
)

// fakeProcessor stands in for the media pipeline. Its remux copies the
// source to the output, which is what a real remux amounts to from the
// organizer's point of view.
type fakeProcessor struct {
	// mu guards the fields this records into. One processor is shared by
	// every Finalize in a test, and the concurrency tests run two at once,
	// so the counters below are written from two goroutines. Reads happen
	// after the WaitGroup, which orders them against these writes.
	mu sync.Mutex

	remuxErr   error
	verifyErr  error
	removeErr  error
	remuxCalls int

	// remuxOutput replaces the copy with fixed content, which is how a
	// test gives the remux a different size than the capture it read.
	remuxOutput []byte

	// entered and release hold a remux open, which is how a test puts two
	// Finalize calls inside the same recording at the same time.
	entered chan struct{}
	release chan struct{}

	// mediaDuration is what the fake measures a finished file as, and
	// durationErr is the machine that cannot measure one at all.
	mediaDuration time.Duration
	durationErr   error
	measured      []string
}

// harness bundles everything a test needs to finalize a recording.
type harness struct {
	t         *testing.T
	root      string
	store     *store.Store
	processor *fakeProcessor
	organizer *Organizer
	channel   store.Channel
}

// defaultTemplate matches the shipped naming default.
const defaultTemplate = "{author}/{year}/{author} - {date} {time} - {title}.{ext}"

// captureLength is how long the fixture capture ran.
const captureLength = 2 * time.Hour

// startedAt is a fixed broadcast start so rendered dates are decidable.
var startedAt = time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)

// Duration implements Processor.
func (f *fakeProcessor) Duration(_ context.Context, path string) (time.Duration, error) {
	f.mu.Lock()
	f.measured = append(f.measured, path)
	f.mu.Unlock()
	return f.mediaDuration, f.durationErr
}

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

func (f *fakeProcessor) Remux(_ context.Context, source, output string) error {
	f.mu.Lock()
	f.remuxCalls++
	f.mu.Unlock()
	if f.entered != nil {
		f.entered <- struct{}{}
		<-f.release
	}
	if f.remuxErr != nil {
		return f.remuxErr
	}
	// The real pipeline refuses a path something already holds, because once
	// the tool has run the file there answers for the tool and for whatever
	// was already at that path. A fake without it lets a caller pass here
	// and fail against every real library.
	if err := paths.RequireAbsent("output", output); err != nil {
		return err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if f.remuxOutput != nil {
		data = f.remuxOutput
	}
	return os.WriteFile(output, data, 0o644)
}

func (f *fakeProcessor) ReplaceVerified(_ context.Context, source, output string, keepSource bool,
	step, commit func() error,
) error {
	if err := step(); err != nil {
		return err
	}
	if f.verifyErr != nil {
		os.Remove(output)
		return f.verifyErr
	}
	// The pipeline records the output before removing the source, so a
	// removal that fails leaves the row naming a file that exists.
	if err := commit(); err != nil {
		os.Remove(output)
		return err
	}
	if keepSource {
		return nil
	}
	if f.removeErr != nil {
		return f.removeErr
	}
	return os.Remove(source)
}

// newHarness builds a library, a store, and an organizer over a temp dir.
func newHarness(t *testing.T, template string) *harness {
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

	channel, err := db.UpsertChannel("twitch", "examplechannel", "ExampleChannel")
	if err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}

	parsed, err := naming.Parse(template)
	if err != nil {
		t.Fatalf("naming.Parse() err = %v, want nil", err)
	}

	// A capture's media matches its wall span unless a case says otherwise,
	// which is what an unremarkable recording looks like.
	processor := &fakeProcessor{mediaDuration: captureLength}
	return &harness{
		t:         t,
		root:      root,
		store:     db,
		processor: processor,
		organizer: New(lib, db, parsed, processor, Options{Container: "mkv", Location: time.UTC}),
		channel:   channel,
	}
}

// capture writes a capture file and registers it.
func (h *harness) capture(relPath string) store.Recording {
	h.t.Helper()

	full := filepath.Join(h.root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatalf("creating capture directory: %v", err)
	}
	if err := os.WriteFile(full, []byte("captured bytes"), 0o644); err != nil {
		h.t.Fatalf("writing capture: %v", err)
	}

	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID,
		Path:      relPath,
		State:     store.StateCapturing,
		Origin:    store.OriginLive,
		StartedAt: startedAt,
		Bytes:     14,
	})
	if err != nil {
		h.t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	return recording
}

// attachBroadcast links a broadcast carrying metadata to a recording.
func (h *harness) attachBroadcast(recordingID int64, title, category string) store.Broadcast {
	h.t.Helper()

	broadcast, err := h.store.UpsertBroadcast(store.Broadcast{
		ChannelID: h.channel.ID,
		RemoteID:  "stream-1",
		StartedAt: startedAt,
		Title:     title,
		Category:  category,
		Source:    store.SourceLive,
	})
	if err != nil {
		h.t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	if err := h.store.SetBroadcast(recordingID, &broadcast.ID); err != nil {
		h.t.Fatalf("SetBroadcast() err = %v, want nil", err)
	}
	return broadcast
}

// withContainer rebuilds the organizer for another final container, which
// is the one setting that decides whether the remux stage runs at all.
func (h *harness) withContainer(container string) {
	h.t.Helper()

	lib, err := library.Open(h.root)
	if err != nil {
		h.t.Fatalf("library.Open() err = %v, want nil", err)
	}
	parsed, err := naming.Parse(defaultTemplate)
	if err != nil {
		h.t.Fatalf("naming.Parse() err = %v, want nil", err)
	}
	h.organizer = New(lib, h.store, parsed, h.processor,
		Options{Container: container, Location: time.UTC})
}

// exists reports whether a library-relative path is present.
func (h *harness) exists(relPath string) bool {
	_, err := os.Stat(filepath.Join(h.root, relPath))
	return err == nil
}

// reload returns the recording's current stored state.
func (h *harness) reload(id int64) store.Recording {
	h.t.Helper()

	recording, err := h.store.Recording(id)
	if err != nil {
		h.t.Fatalf("Recording() err = %v, want nil", err)
	}
	return recording
}

// ///////////////////////////////////////////////
// The happy path
// ///////////////////////////////////////////////

// TestFinalize_RemuxSizesTheRow proves the space budget follows the remux.
// The cap is measured against the stored byte count, so a repack that
// reclaims container overhead frees nothing until the row is re-measured
// against the file that now exists.
func TestFinalize_RemuxSizesTheRow(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	h.processor.remuxOutput = []byte("packed")
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	stored := h.reload(recording.ID)
	if stored.Bytes != int64(len("packed")) {
		t.Errorf("stored Bytes = %d, want %d: the row still describes the capture",
			stored.Bytes, len("packed"))
	}

	total, err := h.store.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes() err = %v, want nil", err)
	}
	if total != int64(len("packed")) {
		t.Errorf("TotalBytes() = %d, want %d: the budget did not follow the remux",
			total, len("packed"))
	}
}

func TestFinalize_RecordsTheMediaLength(t *testing.T) {
	// A capture's length is wall clock taken around the subprocess, and
	// nothing corrects it. streamlink drops the segments an ad replaced, so
	// the file holds less broadcast than the row's span claims, and the
	// difference is invisible to a detector that only reads wall boundaries.
	h := newHarness(t, defaultTemplate)
	h.processor.mediaDuration = 95 * time.Minute
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	outcome, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	stored := h.reload(recording.ID)
	if stored.MediaDuration != 95*time.Minute {
		t.Errorf("MediaDuration = %s, want the length the file actually holds", stored.MediaDuration)
	}

	// Measured where it ended up, because that is the file the library keeps
	// and the only one a later reader can check the row against.
	want := filepath.Join(h.root, filepath.FromSlash(outcome.Path))
	if len(h.processor.measured) != 1 || h.processor.measured[0] != want {
		t.Errorf("measured %v, want exactly %q", h.processor.measured, want)
	}
}

func TestFinalize_SizesARecordingThatNeededNoRemux(t *testing.T) {
	// A download that arrives in the configured container skips the remux,
	// which is where the only sizing call sat. The row then reaches complete
	// carrying the zero it was created with: the library reads smaller than
	// it is forever, and the purge pane tells the operator that deleting the
	// recording frees nothing.
	h := newHarness(t, defaultTemplate)
	h.withContainer("mp4")

	relPath := "incoming/twitch-examplechannel-1772658900.mp4"
	full := filepath.Join(h.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating the incoming directory: %v", err)
	}
	if err := os.WriteFile(full, []byte(strings.Repeat("x", 4096)), 0o644); err != nil {
		t.Fatalf("writing the download: %v", err)
	}

	// No Bytes, the way a backfill fetch registers what it downloaded.
	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: h.channel.ID, Path: relPath,
		State: store.StateAwaitingFinalize, Origin: store.OriginRecovered, StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	if h.processor.remuxCalls != 0 {
		t.Fatalf("remux ran %d times, want 0 for a file already in the container", h.processor.remuxCalls)
	}
	if got := h.reload(recording.ID).Bytes; got != 4096 {
		t.Errorf("Bytes = %d, want 4096: the size of what is on disk", got)
	}
}

func TestFinalize_SizesAgainstTheLibraryPath(t *testing.T) {
	// The size that matters is of the file the row will name for the rest of
	// its life, which is the one in the library rather than the remux output
	// still sitting in the incoming directory.
	h := newHarness(t, defaultTemplate)
	h.processor.remuxOutput = []byte(strings.Repeat("y", 2048))
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	outcome, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	info, err := os.Stat(filepath.Join(h.root, filepath.FromSlash(outcome.Path)))
	if err != nil {
		t.Fatalf("stat of the library file: %v", err)
	}
	if got := h.reload(recording.ID).Bytes; got != info.Size() {
		t.Errorf("Bytes = %d, want %d: the size of the file in the library", got, info.Size())
	}
}

func TestFinalize_ReportsAMediaLengthItCouldNotRead(t *testing.T) {
	// A length nobody could read is not a length of zero. Storing one would
	// make the detector report the whole recording missing, so the failure
	// has to reach the sweep that retries it.
	h := newHarness(t, defaultTemplate)
	h.processor.durationErr = errors.New("ffprobe is not installed")
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err == nil {
		t.Fatal("Finalize() err = nil, want the unreadable length surfaced")
	}

	stored := h.reload(recording.ID)
	if stored.MediaDuration != 0 {
		t.Errorf("MediaDuration = %s, want it left unrecorded", stored.MediaDuration)
	}
	if stored.State == store.StateComplete {
		t.Error("State = complete, want the recording left for the sweep to retry")
	}
}

func TestFinalize_RemuxesAndNames(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	want := "ExampleChannel/2026/ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv"
	if got.Path != want {
		t.Errorf("Outcome.Path = %q, want %q", got.Path, want)
	}
	if !got.Remuxed || !got.Renamed || got.Parked {
		t.Errorf("Outcome = %+v, want remuxed and renamed", got)
	}
	if !h.exists(want) {
		t.Errorf("library file %q missing", want)
	}
	if h.exists(recording.Path) {
		t.Errorf("capture file %q survived, want it consumed by the remux", recording.Path)
	}

	stored := h.reload(recording.ID)
	if stored.State != store.StateComplete {
		t.Errorf("State = %q, want %q", stored.State, store.StateComplete)
	}
	// A row names the same file on every host, so it holds the slash form
	// of the path the organizer built with this host's separator.
	if stored.Path != filepath.ToSlash(want) {
		t.Errorf("stored Path = %q, want %q", stored.Path, filepath.ToSlash(want))
	}
}

func TestFinalize_WritesARebuildableSidecar(t *testing.T) {
	// The database is a cache. A library that loses it must be rebuildable
	// from the files themselves.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.ts")
	broadcast := h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	if err := h.store.ObserveTitle(broadcast.ID, startedAt, "starting soon", "Just Chatting"); err != nil {
		t.Fatalf("ObserveTitle() err = %v, want nil", err)
	}
	if err := h.store.ObserveTitle(broadcast.ID, startedAt.Add(3*time.Minute),
		"Midnight Build Stream", "Just Chatting"); err != nil {
		t.Fatalf("ObserveTitle() err = %v, want nil", err)
	}
	if _, err := h.store.AddGap(recording.ID, 0, 33*time.Minute, "recorder started late"); err != nil {
		t.Fatalf("AddGap() err = %v, want nil", err)
	}

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	sidecarPath := SidecarPath(filepath.Join(h.root, got.Path))
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("reading sidecar: %v", err)
	}

	var sidecar Sidecar
	if err := json.Unmarshal(data, &sidecar); err != nil {
		t.Fatalf("parsing sidecar: %v", err)
	}
	if sidecar.Title != "Midnight Build Stream" {
		t.Errorf("sidecar Title = %q, want the broadcast title", sidecar.Title)
	}
	if sidecar.Channel != "examplechannel" || sidecar.Platform != "twitch" {
		t.Errorf("sidecar identifies %s/%s, want twitch/examplechannel", sidecar.Platform, sidecar.Channel)
	}
	if len(sidecar.TitleHistory) != 2 {
		t.Errorf("sidecar TitleHistory has %d entries, want 2", len(sidecar.TitleHistory))
	}
	if len(sidecar.Gaps) != 1 || sidecar.Gaps[0].Reason != "recorder started late" {
		t.Errorf("sidecar Gaps = %+v, want the recorded gap", sidecar.Gaps)
	}
}

func TestFinalize_SidecarCarriesTheSizeTheRowHolds(t *testing.T) {
	// Size is what the cap, the free-space floor, the purge scoring and the
	// download admission all run on. A sidecar that reports zero rebuilds a
	// library reading as empty, which admits downloads until the disk fills.
	//
	// The file is read rather than the struct, because the whole question is
	// whether the two agree.
	h := newHarness(t, defaultTemplate)
	h.processor.remuxOutput = []byte("packed")
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	data, err := os.ReadFile(SidecarPath(filepath.Join(h.root, got.Path)))
	if err != nil {
		t.Fatalf("reading sidecar: %v", err)
	}
	var sidecar Sidecar
	if err := json.Unmarshal(data, &sidecar); err != nil {
		t.Fatalf("parsing sidecar: %v", err)
	}

	stored := h.reload(recording.ID)
	if stored.Bytes == 0 {
		t.Fatal("the row holds no size, so this proves nothing about the sidecar")
	}
	if sidecar.Bytes != stored.Bytes {
		t.Errorf("sidecar Bytes = %d, want the %d the row holds", sidecar.Bytes, stored.Bytes)
	}
}

func TestFinalize_SidecarCarriesTheMeasuredMediaLength(t *testing.T) {
	// A fetched copy never runs through FinishRecording, so duration_ms stays
	// zero and the measured media length is the only length it has. Leaving
	// that out of the sidecar leaves such a recording with no duration at all
	// for anything rebuilding from the files.
	h := newHarness(t, defaultTemplate)
	h.processor.mediaDuration = 95 * time.Minute
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	data, err := os.ReadFile(SidecarPath(filepath.Join(h.root, got.Path)))
	if err != nil {
		t.Fatalf("reading sidecar: %v", err)
	}
	var sidecar Sidecar
	if err := json.Unmarshal(data, &sidecar); err != nil {
		t.Fatalf("parsing sidecar: %v", err)
	}

	if sidecar.MediaDurationMS != (95 * time.Minute).Milliseconds() {
		t.Errorf("sidecar MediaDurationMS = %d, want %d",
			sidecar.MediaDurationMS, (95 * time.Minute).Milliseconds())
	}
}

func TestFinalize_SidecarCarriesHowMuchThePlatformSilenced(t *testing.T) {
	// Nothing in the media says which stretches came back silent, so a
	// recovered copy that does not carry the figure beside it cannot be told
	// from one the platform left alone. A live capture never asked, and an
	// absent field is what says so.
	tests := []struct {
		name  string
		muted *time.Duration
		want  *int64
	}{
		{name: "a recovered copy the platform silenced", muted: new(3 * time.Minute), want: new(int64(180_000))},
		{name: "a recovered copy it left alone", muted: new(time.Duration(0)), want: new(int64(0))},
		{name: "a live capture nobody asked about", muted: nil, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, defaultTemplate)
			recording := h.capture("incoming/twitch-examplechannel-1.ts")
			h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
			if tt.muted != nil {
				if err := h.store.SetMutedDuration(recording.ID, *tt.muted); err != nil {
					t.Fatalf("SetMutedDuration() err = %v, want nil", err)
				}
			}

			got, err := h.organizer.Finalize(context.Background(), recording.ID)
			if err != nil {
				t.Fatalf("Finalize() err = %v, want nil", err)
			}

			data, err := os.ReadFile(SidecarPath(filepath.Join(h.root, got.Path)))
			if err != nil {
				t.Fatalf("reading sidecar: %v", err)
			}
			var sidecar Sidecar
			if err := json.Unmarshal(data, &sidecar); err != nil {
				t.Fatalf("parsing sidecar: %v", err)
			}

			switch {
			case tt.want == nil && sidecar.MutedMS != nil:
				t.Errorf("sidecar MutedMS = %d, want it absent", *sidecar.MutedMS)
			case tt.want != nil && sidecar.MutedMS == nil:
				t.Errorf("sidecar MutedMS is absent, want %d", *tt.want)
			case tt.want != nil && *sidecar.MutedMS != *tt.want:
				t.Errorf("sidecar MutedMS = %d, want %d", *sidecar.MutedMS, *tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// The blocked-name path
// ///////////////////////////////////////////////

func TestFinalize_ParksWhenMetadataIsMissing(t *testing.T) {
	// A build rendering from absent metadata produced
	// " - 2026-03-04 21-15 - .ts". The recording must survive intact under
	// its capture name rather than be renamed to something with holes in it.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil for a parked recording", err)
	}

	if !got.Parked {
		t.Fatalf("Outcome = %+v, want it parked", got)
	}
	if len(got.Missing) == 0 {
		t.Error("Outcome.Missing is empty, want the blocking placeholders named")
	}
	foundTitle := false
	for _, missing := range got.Missing {
		if missing == "title" {
			foundTitle = true
		}
	}
	if !foundTitle {
		t.Errorf("Outcome.Missing = %v, want it to name title", got.Missing)
	}

	// The remux still ran, because it needs no metadata.
	remuxed := "incoming/twitch-examplechannel-1772658900.mkv"
	if !h.exists(remuxed) {
		t.Errorf("remuxed file %q missing, want the metadata-free stage to have run", remuxed)
	}

	stored := h.reload(recording.ID)
	if stored.State != store.StateAwaitingMetadata {
		t.Errorf("State = %q, want %q", stored.State, store.StateAwaitingMetadata)
	}
	if stored.Path != remuxed {
		t.Errorf("stored Path = %q, want the capture-derived name %q", stored.Path, remuxed)
	}
	if stored.Bytes == 0 {
		t.Error("Bytes = 0, want the captured bytes preserved")
	}
}

func TestFinalize_RetryCompletesAParkedRecording(t *testing.T) {
	// Finalize is repeatable so the daemon can sweep parked recordings on
	// a timer rather than needing a separate retry path.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")

	first, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("first Finalize() err = %v, want nil", err)
	}
	if !first.Parked {
		t.Fatalf("first Finalize() = %+v, want it parked", first)
	}

	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	second, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("second Finalize() err = %v, want nil", err)
	}
	if second.Parked {
		t.Fatalf("second Finalize() = %+v, want it completed", second)
	}
	if second.Remuxed {
		t.Error("second Finalize() remuxed again, want the finished stage skipped")
	}
	if h.processor.remuxCalls != 1 {
		t.Errorf("remux ran %d times, want exactly 1", h.processor.remuxCalls)
	}

	want := filepath.Join("ExampleChannel", "2026", "ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv")
	if !h.exists(want) {
		t.Errorf("library file %q missing after retry", want)
	}
}

// ///////////////////////////////////////////////
// New
// ///////////////////////////////////////////////

func TestNew_Defaults(t *testing.T) {
	// An Organizer built from a zero Options has to be usable, because a
	// caller that has no opinion about container or timezone is the common
	// case and a nil location would panic at the first render.
	tests := []struct {
		name          string
		opts          Options
		wantContainer string
		wantLocation  *time.Location
	}{
		{
			name:          "empty options",
			opts:          Options{},
			wantContainer: "mkv",
			wantLocation:  time.Local,
		},
		{
			name:          "a leading dot is trimmed",
			opts:          Options{Container: ".mp4", Location: time.UTC},
			wantContainer: "mp4",
			wantLocation:  time.UTC,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			organizer := New(nil, nil, nil, nil, tt.opts)

			if organizer.container != tt.wantContainer {
				t.Errorf("container = %q, want %q", organizer.container, tt.wantContainer)
			}
			if organizer.location != tt.wantLocation {
				t.Errorf("location = %v, want %v", organizer.location, tt.wantLocation)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Missing rows
// ///////////////////////////////////////////////

func TestChannelFor_UnknownChannel(t *testing.T) {
	// A foreign key keeps this out of the database, so the branch guards an
	// invariant rather than an input. It still has to report rather than
	// return a zero channel that would name every recording "-".
	h := newHarness(t, defaultTemplate)

	_, err := h.organizer.channelFor(store.Recording{ID: 1, ChannelID: 9999})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("channelFor() err = %v, want it to wrap %v", err, store.ErrNotFound)
	}
}

func TestFieldsFor_BroadcastRowIsGone(t *testing.T) {
	// A foreign key nulls the link when a broadcast is deleted, so a
	// dangling id is an invariant breach. Naming still has to fall back to
	// what the recording itself knows rather than failing outright, which
	// leaves the title empty and parks the rename.
	h := newHarness(t, defaultTemplate)
	missing := int64(9999)

	fields, remoteID, err := h.organizer.fieldsFor(
		store.Recording{ID: 1, ChannelID: h.channel.ID, BroadcastID: &missing, StartedAt: startedAt},
		h.channel)
	if err != nil {
		t.Fatalf("fieldsFor() err = %v, want a dangling link to be survivable", err)
	}
	if fields.Title != "" {
		t.Errorf("Title = %q, want it empty so the rename parks", fields.Title)
	}
	if fields.Channel != h.channel.Name {
		t.Errorf("Channel = %q, want the recording's own channel %q", fields.Channel, h.channel.Name)
	}
	if remoteID != "" {
		t.Errorf("remoteID = %q, want it empty", remoteID)
	}
}

// ///////////////////////////////////////////////
// Parked on an unusable name
// ///////////////////////////////////////////////

func TestFinalize_ParksWhenAValueSanitizesAway(t *testing.T) {
	// A display name of "..." is not missing, so it is not a
	// MissingFieldError, but it sanitizes to an empty path segment. The
	// recording is every bit as intact as one waiting on a title. Returning
	// an error would mark it complete and drop it out of the sweep, with the
	// file still in incoming/.
	h := newHarness(t, defaultTemplate)
	if _, err := h.store.UpsertChannel("twitch", "dots", "..."); err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
	dotted, err := h.store.Channel("twitch", "dots")
	if err != nil {
		t.Fatalf("Channel() err = %v, want nil", err)
	}

	full := filepath.Join(h.root, "incoming", "twitch-dots-1.ts")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating capture directory: %v", err)
	}
	if err := os.WriteFile(full, []byte("captured bytes"), 0o644); err != nil {
		t.Fatalf("writing capture: %v", err)
	}
	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: dotted.ID, Path: "incoming/twitch-dots-1.ts",
		State: store.StateCapturing, Origin: store.OriginLive, StartedAt: startedAt, Bytes: 14,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	h.attachBroadcast(recording.ID, "a normal stream", "Just Chatting")

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want an unusable name to park rather than fail", err)
	}
	if !got.Parked {
		t.Fatalf("Outcome = %+v, want it parked", got)
	}
	if got.Reason == "" {
		t.Error("Outcome.Reason is empty, want it to say why the name could not be rendered")
	}
	if stored := h.reload(recording.ID); stored.State != store.StateAwaitingMetadata {
		t.Errorf("State = %q, want %q so the sweep retries it", stored.State, store.StateAwaitingMetadata)
	}
}

func TestFinalize_RecordsThePathBeforeTheSidecar(t *testing.T) {
	// If the sidecar fails after the move, the stored path must already be
	// the new one. Storing it afterwards leaves the row naming a file that
	// has moved, and every retry then fails on the missing source forever.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	// Stands in for a full disk or a permission failure: the move has
	// already happened by the time the sidecar is written.
	want := filepath.Join("ExampleChannel", "2026", "ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv")
	original := writeSidecarFile
	t.Cleanup(func() { writeSidecarFile = original })
	writeSidecarFile = func(context.Context, string, []byte, os.FileMode) error {
		return errors.New("no space left on device")
	}

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err == nil {
		t.Fatal("Finalize() err = nil, want the sidecar failure reported")
	}

	stored := h.reload(recording.ID)
	if stored.Path != filepath.ToSlash(want) {
		t.Errorf("stored Path = %q, want the file's real location %q", stored.Path, filepath.ToSlash(want))
	}
	if !h.exists(stored.Path) {
		t.Errorf("stored Path %q does not exist, so a retry can never find the recording", stored.Path)
	}
}

// ///////////////////////////////////////////////
// Containment
// ///////////////////////////////////////////////

// namedChannel registers a channel with a chosen display name and a capture
// ready to finalize, since the display name is what a streamer controls.
func (h *harness) namedChannel(t *testing.T, login, display, relPath string) store.Recording {
	t.Helper()

	if _, err := h.store.UpsertChannel("twitch", login, display); err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
	channel, err := h.store.Channel("twitch", login)
	if err != nil {
		t.Fatalf("Channel() err = %v, want nil", err)
	}

	full := filepath.Join(h.root, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating capture directory: %v", err)
	}
	if err := os.WriteFile(full, []byte("captured bytes"), 0o644); err != nil {
		t.Fatalf("writing capture: %v", err)
	}

	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: channel.ID, Path: relPath,
		State: store.StateCapturing, Origin: store.OriginLive, StartedAt: startedAt, Bytes: 14,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	return recording
}

func TestFinalize_KeepsRecordingsOutOfTheLibrarysOwnDirectories(t *testing.T) {
	// A display name arrives from platform metadata, so a streamer picks
	// it. One matching a library directory would file recordings into the
	// state directory. The sidecar beside them lands on the ownership
	// marker, and every later open of the library fails.
	for _, display := range []string{".dvr", "incoming", ".DVR", "Incoming"} {
		t.Run(display, func(t *testing.T) {
			h := newHarness(t, "{author}/{title}.{ext}")
			recording := h.namedChannel(t, "victim", display, "incoming/twitch-victim-1.ts")
			h.attachBroadcast(recording.ID, "library", "Just Chatting")

			marker := filepath.Join(h.root, ".dvr", "library.json")
			before, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("reading the marker: %v", err)
			}

			got, err := h.organizer.Finalize(context.Background(), recording.ID)
			if err != nil {
				t.Fatalf("Finalize() err = %v, want nil", err)
			}

			after, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("the ownership marker is gone: %v", err)
			}
			if string(after) != string(before) {
				t.Errorf("the ownership marker was overwritten\n got: %s\nwant: %s", after, before)
			}
			if _, err := library.Open(h.root); err != nil {
				t.Errorf("library.Open() err = %v, want the library still openable", err)
			}

			first, _, _ := strings.Cut(got.Path, string(filepath.Separator))
			if strings.EqualFold(first, ".dvr") || strings.EqualFold(first, "incoming") {
				t.Errorf("Outcome.Path = %q, want it outside the library's own directories", got.Path)
			}
		})
	}
}

func TestRefuseReserved(t *testing.T) {
	// naming sanitizes what it renders, so this is the backstop for a caller
	// that renders a path some other way. A backstop that only guards the
	// state directory leaves the whole volume outside the root unguarded.
	h := newHarness(t, defaultTemplate)

	tests := []struct {
		name    string
		relPath string
		wantErr bool
	}{
		{name: "an ordinary library path", relPath: filepath.Join("ExampleChannel", "2026", "show.mkv")},
		{name: "a path at the root", relPath: "show.mkv"},
		{name: "the state directory", relPath: filepath.Join(".dvr", "library.json"), wantErr: true},
		{name: "the state directory itself", relPath: ".dvr", wantErr: true},
		{name: "one level above the root", relPath: filepath.Join("..", "escaped.mkv"), wantErr: true},
		{name: "two levels above the root", relPath: filepath.Join("..", "..", "escaped.mkv"), wantErr: true},
		{name: "climbing back down after escaping", relPath: filepath.Join("..", "sibling", "escaped.mkv"), wantErr: true},
		{name: "an absolute path", relPath: filepath.Join(t.TempDir(), "escaped.mkv"), wantErr: true},
		{name: "an empty path", relPath: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.organizer.refuseReserved(tt.relPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("refuseReserved(%q) err = %v, wantErr %v", tt.relPath, err, tt.wantErr)
			}
		})
	}
}

func TestFinalize_ConcurrentRecordingsWithOneNameBothSurvive(t *testing.T) {
	// One organizer serves every channel watcher and the sweep, and a
	// broadcast split by a reconnect renders the same name twice. A rename
	// replaces its destination on both platforms, so a check that the name
	// is free is a race that destroys a recording.
	h := newHarness(t, defaultTemplate)

	const count = 4
	ids := make([]int64, 0, count)
	for i := range count {
		recording := h.capture(fmt.Sprintf("incoming/twitch-examplechannel-%d.ts", i))
		h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
		ids = append(ids, recording.ID)
	}

	var wg sync.WaitGroup
	paths := make([]string, count)
	errs := make([]error, count)
	for i, id := range ids {
		wg.Go(func() {
			outcome, err := h.organizer.Finalize(context.Background(), id)
			paths[i], errs[i] = outcome.Path, err
		})
	}
	wg.Wait()

	seen := map[string]bool{}
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("Finalize(%d) err = %v, want nil", ids[i], errs[i])
		}
		if seen[paths[i]] {
			t.Errorf("two recordings share the path %q, so one was overwritten", paths[i])
		}
		seen[paths[i]] = true
		if !h.exists(paths[i]) {
			t.Errorf("recording %d is missing from %q", ids[i], paths[i])
		}
	}

	media, err := filepath.Glob(filepath.Join(h.root, "ExampleChannel", "2026", "*.mkv"))
	if err != nil {
		t.Fatalf("Glob() err = %v, want nil", err)
	}
	if len(media) != count {
		t.Errorf("library holds %d recordings, want all %d", len(media), count)
	}
}

func TestFinalize_DoesNotOverwriteAnExistingSidecar(t *testing.T) {
	// Deduplication consults the media name, and the sidecar shares its
	// stem. A media slot that is free while its sidecar is taken would
	// destroy that sidecar, which the package doc calls the record and the
	// database only a cache.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	orphan := filepath.Join(h.root, "ExampleChannel", "2026",
		"ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv.json")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatalf("creating the library directory: %v", err)
	}
	if err := os.WriteFile(orphan, []byte(`{"sole":"copy"}`), 0o644); err != nil {
		t.Fatalf("seeding the orphaned sidecar: %v", err)
	}

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	survived, err := os.ReadFile(orphan)
	if err != nil {
		t.Fatalf("the existing sidecar is gone: %v", err)
	}
	if string(survived) != `{"sole":"copy"}` {
		t.Errorf("existing sidecar = %s, want it untouched", survived)
	}
}

func TestSamePath(t *testing.T) {
	// A stored path and a rendered candidate are built by different code and
	// meet here. Reading two spellings of one path as two files makes the
	// self-collision guard rename a finished recording to " (2)" on every
	// sweep. Nothing reports an error while it happens.
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{
			name: "the same slash form",
			a:    "ExampleChannel/2026/show.mkv",
			b:    "ExampleChannel/2026/show.mkv",
			want: true,
		},
		{
			name: "a host-joined path against its slash twin",
			a:    filepath.Join("ExampleChannel", "2026", "show.mkv"),
			b:    "ExampleChannel/2026/show.mkv",
			want: true,
		},
		{
			name: "two different recordings",
			a:    "ExampleChannel/2026/show.mkv",
			b:    "ExampleChannel/2026/show (2).mkv",
			want: false,
		},
		{
			name: "two different directories",
			a:    "ExampleChannel/2026/show.mkv",
			b:    "OtherChannel/2026/show.mkv",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := samePath(tt.a, tt.b); got != tt.want {
				t.Errorf("samePath(%q, %q) = %t, want %t", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// The sidecar and the recording it describes
// ///////////////////////////////////////////////

func TestSidecarPath(t *testing.T) {
	// The suffix is appended, never substituted. Substituting it puts the
	// sidecar on top of any recording whose own name ends in .json, and
	// gives one name to the sidecars of two containers.
	tests := []struct {
		name  string
		media string
		want  string
	}{
		{name: "the ordinary container", media: "Show.mkv", want: "Show.mkv.json"},
		{name: "a title ending in the sidecar suffix", media: "Season Finale.json", want: "Season Finale.json.json"},
		{name: "no extension at all", media: "Show", want: "Show.json"},
		{name: "a second container of the same recording", media: "Show.mp4", want: "Show.mp4.json"},
		{name: "a dotted title", media: "Ep 1. Alpha.mkv", want: "Ep 1. Alpha.mkv.json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SidecarPath(tt.media); got != tt.want {
				t.Errorf("SidecarPath(%q) = %q, want %q", tt.media, got, tt.want)
			}
			if SidecarPath(tt.media) == tt.media {
				t.Errorf("SidecarPath(%q) returned the media path itself", tt.media)
			}
		})
	}
}

func TestRefuseSelfOverwrite(t *testing.T) {
	tests := []struct {
		name    string
		media   string
		sidecar string
		wantErr bool
	}{
		{name: "a distinct sidecar", media: "Show.mkv", sidecar: "Show.mkv.json"},
		{name: "the recording itself", media: "Show.json", sidecar: "Show.json", wantErr: true},
		{
			// A case-insensitive filesystem reaches one file either way.
			name:    "the recording under another casing",
			media:   "Show.JSON",
			sidecar: "Show.json",
			wantErr: true,
		},
		{name: "a sibling recording", media: "Show.mkv", sidecar: "Other.mkv.json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := refuseSelfOverwrite(tt.media, tt.sidecar)
			if (err != nil) != tt.wantErr {
				t.Errorf("refuseSelfOverwrite(%q, %q) err = %v, wantErr %v", tt.media, tt.sidecar, err, tt.wantErr)
			}
		})
	}
}

func TestFinalize_TheSidecarNeverOverwritesTheRecording(t *testing.T) {
	// The streamer picks the title and the operator picks the template, and
	// a template that does not end in .{ext} is one the validator accepts.
	// Between them they can name a recording anything at all, and the
	// sidecar written beside it must never be the recording itself.
	tests := []struct {
		name     string
		template string
		title    string
		want     string
	}{
		{
			name:     "a title ending in the sidecar suffix",
			template: "{author}/{title}",
			title:    "Season Finale.json",
			want:     "ExampleChannel/Season Finale.json",
		},
		{
			name:     "a template with no extension",
			template: "{author}/{title}",
			title:    "Midnight Build Stream",
			want:     "ExampleChannel/Midnight Build Stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.template)
			recording := h.capture("incoming/twitch-examplechannel-1.ts")
			h.attachBroadcast(recording.ID, tt.title, "Just Chatting")

			got, err := h.organizer.Finalize(context.Background(), recording.ID)
			if err != nil {
				t.Fatalf("Finalize() err = %v, want nil", err)
			}
			if got.Path != tt.want {
				t.Fatalf("Outcome.Path = %q, want %q", got.Path, tt.want)
			}

			media, err := os.ReadFile(filepath.Join(h.root, got.Path))
			if err != nil {
				t.Fatalf("reading the filed recording: %v", err)
			}
			if string(media) != "captured bytes" {
				t.Errorf("the library file holds %q, want the recording's own bytes", media)
			}

			var sidecar Sidecar
			data, err := os.ReadFile(SidecarPath(filepath.Join(h.root, got.Path)))
			if err != nil {
				t.Fatalf("reading the sidecar: %v", err)
			}
			if err := json.Unmarshal(data, &sidecar); err != nil {
				t.Fatalf("parsing the sidecar: %v", err)
			}
			if sidecar.Title != tt.title {
				t.Errorf("sidecar Title = %q, want %q", sidecar.Title, tt.title)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Overlapping calls
// ///////////////////////////////////////////////

func TestFinalize_RefusesACallThatOverlapsAnother(t *testing.T) {
	// The capture goroutine finalizes what it just recorded, and a sweep
	// tick inside that window enumerates the same row. Both running means
	// two remuxes writing one output path. A re-render with a changed title
	// also moves a file the other already named.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	// Buffered, so a second call that reaches the remux blocks on the
	// release and not on the signal. The wait below can then report it
	// rather than hang on it.
	h.processor.entered = make(chan struct{}, 2)
	h.processor.release = make(chan struct{})

	first := make(chan error, 1)
	go func() {
		_, err := h.organizer.Finalize(context.Background(), recording.ID)
		first <- err
	}()
	<-h.processor.entered

	second := make(chan error, 1)
	go func() {
		_, err := h.organizer.Finalize(context.Background(), recording.ID)
		second <- err
	}()

	refused := false
	select {
	case err := <-second:
		refused = true
		if !errors.Is(err, ErrBusy) {
			t.Errorf("the overlapping Finalize() err = %v, want %v", err, ErrBusy)
		}
	case <-time.After(2 * time.Second):
		t.Error("the overlapping Finalize() never returned, want it refused rather than run alongside")
	}

	close(h.processor.release)
	if err := <-first; err != nil {
		t.Fatalf("the first Finalize() err = %v, want nil", err)
	}
	if !refused {
		<-second
	}
	if h.processor.remuxCalls != 1 {
		t.Errorf("remux ran %d times, want exactly 1", h.processor.remuxCalls)
	}

	want := filepath.Join("ExampleChannel", "2026", "ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv")
	if !h.exists(want) {
		t.Errorf("library file %q missing", want)
	}
	if stored := h.reload(recording.ID); stored.State != store.StateComplete {
		t.Errorf("State = %q, want %q", stored.State, store.StateComplete)
	}
}

// ///////////////////////////////////////////////
// Adopting a file the database lost track of
// ///////////////////////////////////////////////

func TestFinalize_AdoptsAFileMovedBeforeThePathWasStored(t *testing.T) {
	// The move happens before the path reaches the database, so a failure
	// between the two leaves the row naming a source that is gone. Every
	// retry then dies on the missing source and the file sits in the
	// library with no sidecar, invisible to a rebuild.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.mkv")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	want := "ExampleChannel/2026/ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(h.root, want)), 0o755); err != nil {
		t.Fatalf("creating the library directory: %v", err)
	}
	if err := os.Rename(filepath.Join(h.root, recording.Path), filepath.Join(h.root, want)); err != nil {
		t.Fatalf("staging the moved file: %v", err)
	}

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want the moved file adopted", err)
	}
	if got.Path != want {
		t.Errorf("Outcome.Path = %q, want %q", got.Path, want)
	}

	stored := h.reload(recording.ID)
	if stored.Path != filepath.ToSlash(want) {
		t.Errorf("stored Path = %q, want %q", stored.Path, filepath.ToSlash(want))
	}
	if stored.State != store.StateComplete {
		t.Errorf("State = %q, want %q", stored.State, store.StateComplete)
	}
	if !h.exists(SidecarPath(want)) {
		t.Errorf("sidecar for %q missing, want the adopted recording rebuildable", want)
	}
}

func TestFinalize_DoesNotAdoptAFinishedRecording(t *testing.T) {
	// A sidecar beside the media is what a finished finalize leaves. Taking
	// a name that already has one would point two rows at one file and let
	// the second overwrite the first's sidecar.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.mkv")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	taken := filepath.Join("ExampleChannel", "2026", "ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(h.root, taken)), 0o755); err != nil {
		t.Fatalf("creating the library directory: %v", err)
	}
	for _, seeded := range []string{taken, SidecarPath(taken)} {
		if err := os.WriteFile(filepath.Join(h.root, seeded), []byte("somebody else's"), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", seeded, err)
		}
	}
	if err := os.Remove(filepath.Join(h.root, recording.Path)); err != nil {
		t.Fatalf("removing the source: %v", err)
	}

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err == nil {
		t.Error("Finalize() err = nil, want the missing source reported rather than a stranger adopted")
	}

	survived, err := os.ReadFile(filepath.Join(h.root, taken))
	if err != nil {
		t.Fatalf("the finished recording is gone: %v", err)
	}
	if string(survived) != "somebody else's" {
		t.Errorf("the finished recording = %q, want it untouched", survived)
	}
}

// ///////////////////////////////////////////////
// Parked on a held file
// ///////////////////////////////////////////////

// holdRename makes every rename report the file as held by another program,
// and returns a release function that restores normal behavior.
func holdRename(t *testing.T) func() {
	t.Helper()

	original := renameFile
	renameFile = func(_ context.Context, source, _ string) error {
		return &fsretry.LockedError{
			Op: "rename", Path: source, Attempts: 9,
			Waited: 26 * time.Second, Err: errors.New("used by another process"),
		}
	}

	release := func() { renameFile = original }
	t.Cleanup(release)
	return release
}

func TestFinalize_ParksWhenAnotherProgramHoldsTheFile(t *testing.T) {
	// A backup agent reading a capture that just finished must delay the
	// move, never fail the recording. The bytes are already safe on disk.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
	holdRename(t)

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want a held file to park rather than fail", err)
	}
	if !got.Parked || !got.Locked {
		t.Errorf("Outcome = %+v, want it parked and marked locked", got)
	}

	stored := h.reload(recording.ID)
	if stored.State != store.StateAwaitingFile {
		t.Errorf("State = %q, want %q", stored.State, store.StateAwaitingFile)
	}

	// The remux ran, so the file is under its remuxed capture name and
	// nothing has been moved into the library.
	remuxed := "incoming/twitch-examplechannel-1772658900.mkv"
	if !h.exists(remuxed) {
		t.Errorf("remuxed file %q missing, want the recording intact where it was", remuxed)
	}
	named := filepath.Join("ExampleChannel", "2026", "ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv")
	if h.exists(named) {
		t.Errorf("library file %q exists, want nothing moved while the file was held", named)
	}
}

func TestFinalize_CompletesOnceTheHoldEnds(t *testing.T) {
	// The sweep calls Finalize again, so releasing the file has to be all
	// it takes for the recording to land.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
	release := holdRename(t)

	first, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("first Finalize() err = %v, want nil", err)
	}
	if !first.Locked {
		t.Fatalf("first Finalize() = %+v, want it parked on the lock", first)
	}

	release()

	second, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("second Finalize() err = %v, want nil", err)
	}
	if second.Parked || second.Locked {
		t.Fatalf("second Finalize() = %+v, want it completed", second)
	}
	if h.processor.remuxCalls != 1 {
		t.Errorf("remux ran %d times, want the finished stage skipped on the retry", h.processor.remuxCalls)
	}

	want := filepath.Join("ExampleChannel", "2026", "ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv")
	if !h.exists(want) {
		t.Errorf("library file %q missing after the hold ended", want)
	}
	if stored := h.reload(recording.ID); stored.State != store.StateComplete {
		t.Errorf("State = %q, want %q", stored.State, store.StateComplete)
	}
}

func TestFinalize_AFailedRenameThatIsNotALockStillFails(t *testing.T) {
	// Parking is for a wait that ends on its own. A broken filesystem does
	// not, and hiding it as a park would leave the recording circling the
	// sweep forever with nothing reported.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	original := renameFile
	t.Cleanup(func() { renameFile = original })
	broken := errors.New("the volume is gone")
	renameFile = func(context.Context, string, string) error { return broken }

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if !errors.Is(err, broken) {
		t.Fatalf("Finalize() err = %v, want it to report %v", err, broken)
	}
	if got.Parked {
		t.Errorf("Outcome = %+v, want a real failure rather than a park", got)
	}
	if stored := h.reload(recording.ID); stored.State == store.StateAwaitingFile {
		t.Error("State = awaiting_file, want a broken rename left out of the sweep")
	}
}

func TestFinalize_AuthorFallbackIsReported(t *testing.T) {
	// A missing display name degrades the name rather than blocking it,
	// but a silently degraded name must stay visible.
	h := newHarness(t, defaultTemplate)
	if _, err := h.store.UpsertChannel("twitch", "nameless", ""); err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
	nameless, err := h.store.Channel("twitch", "nameless")
	if err != nil {
		t.Fatalf("Channel() err = %v, want nil", err)
	}

	full := filepath.Join(h.root, "incoming", "twitch-nameless-1.ts")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("creating capture directory: %v", err)
	}
	if err := os.WriteFile(full, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("writing capture: %v", err)
	}
	recording, err := h.store.CreateRecording(store.Recording{
		ChannelID: nameless.ID, Path: "incoming/twitch-nameless-1.ts",
		State: store.StateCapturing, Origin: store.OriginLive, StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}

	broadcast, err := h.store.UpsertBroadcast(store.Broadcast{
		ChannelID: nameless.ID, StartedAt: startedAt, Title: "a title", Source: store.SourceLive,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	if err := h.store.SetBroadcast(recording.ID, &broadcast.ID); err != nil {
		t.Fatalf("SetBroadcast() err = %v, want nil", err)
	}

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}
	if len(got.Fallbacks) != 1 || got.Fallbacks[0] != "author" {
		t.Errorf("Outcome.Fallbacks = %v, want [author]", got.Fallbacks)
	}
	if !strings.Contains(got.Path, "nameless") {
		t.Errorf("Outcome.Path = %q, want the channel login standing in for the display name", got.Path)
	}
}

// ///////////////////////////////////////////////
// Failure handling
// ///////////////////////////////////////////////

func TestFinalize_KeepsTheCaptureWhenRemuxFails(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.ts")
	h.attachBroadcast(recording.ID, "a title", "")
	h.processor.remuxErr = errors.New("ffmpeg died")

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err == nil {
		t.Fatal("Finalize() err = nil, want the remux failure surfaced")
	}
	if !h.exists(recording.Path) {
		t.Error("capture file removed after a failed remux, want it kept")
	}
}

func TestFinalize_KeepsTheCaptureWhenVerificationFails(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.ts")
	h.attachBroadcast(recording.ID, "a title", "")
	h.processor.verifyErr = errors.New("duration differs")

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err == nil {
		t.Fatal("Finalize() err = nil, want the verification failure surfaced")
	}
	if !h.exists(recording.Path) {
		t.Error("capture file removed after failed verification, want it kept")
	}
	if h.exists("incoming/twitch-examplechannel-1.mkv") {
		t.Error("unverified remux output survived, want it removed")
	}
}

func TestFinalize_ResumesAfterTheCaptureCouldNotBeRemoved(t *testing.T) {
	// A backup agent holding the capture past the retry window is the case
	// the retry exists for. Recording the remux afterwards would leave the
	// row naming the capture with the finished file beside it, and every
	// later sweep would refuse to write over that file, forever.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "")
	h.processor.removeErr = errors.New("the file is held by another process")

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err == nil {
		t.Fatal("Finalize() err = nil, want the failed removal surfaced")
	}

	remuxed := "incoming/twitch-examplechannel-1.mkv"
	if got := h.reload(recording.ID).Path; got != remuxed {
		t.Fatalf("stored path = %q, want %q, the file that is actually the recording", got, remuxed)
	}

	// The hold ends and the sweep comes round again.
	h.processor.removeErr = nil
	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want the recording finished on the next sweep", err)
	}
	if h.processor.remuxCalls != 1 {
		t.Errorf("remuxCalls = %d, want the multi-gigabyte remux done once rather than on every sweep",
			h.processor.remuxCalls)
	}

	want := "ExampleChannel/2026/ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv"
	if got.Path != want {
		t.Errorf("Outcome.Path = %q, want %q", got.Path, want)
	}
	if !h.exists(want) {
		t.Errorf("library file %q missing", want)
	}
	if state := h.reload(recording.ID).State; state != store.StateComplete {
		t.Errorf("state = %q, want %q", state, store.StateComplete)
	}
}

func TestFinalize_GivesUpOnARemuxThatFailsTheSameWayEverySweep(t *testing.T) {
	// Every pending recording is retried on a timer, and a remux that will
	// not succeed costs a full pass over a multi-gigabyte file each time it
	// comes round. A terminal state stops the loop and puts the recording
	// somewhere an operator can see it.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.ts")
	h.attachBroadcast(recording.ID, "a title", "")
	h.processor.verifyErr = errors.New("output has 1 audio stream, source has 2")

	for sweep := 1; sweep <= maxRemuxAttempts; sweep++ {
		_, err := h.organizer.Finalize(context.Background(), recording.ID)
		if err == nil {
			t.Fatalf("sweep %d: Finalize() err = nil, want the failure surfaced", sweep)
		}
		// The sentinel is what tells a caller to stop counting, so it belongs
		// to the sweep that changes the state and to no earlier one.
		if gaveUp, last := errors.Is(err, ErrGaveUp), sweep == maxRemuxAttempts; gaveUp != last {
			t.Errorf("sweep %d: errors.Is(err, ErrGaveUp) = %t, want %t", sweep, gaveUp, last)
		}
	}

	if state := h.reload(recording.ID).State; state != store.StateFailed {
		t.Errorf("state = %q after %d identical failures, want %q so the sweep stops retrying it",
			state, maxRemuxAttempts, store.StateFailed)
	}
	// The sweep enumerates exactly this set, so this is the query that
	// decides whether the recording comes round again.
	pending, err := h.store.RecordingsByState(store.PendingStates...)
	if err != nil {
		t.Fatalf("RecordingsByState() err = %v, want nil", err)
	}
	for _, row := range pending {
		if row.ID == recording.ID {
			t.Errorf("recording %d is still swept in state %q, want it out of the sweep",
				row.ID, row.State)
		}
	}

	if !h.exists(recording.Path) {
		t.Error("capture file removed when the remux was given up on, want it kept on disk")
	}
}

func TestFinalize_ARemuxThatSucceedsClearsTheBudget(t *testing.T) {
	// A program holding the file for two sweeps and letting go on the third
	// is an ordinary delay, not a recording to give up on. The count is of
	// failures in a row, so a recording that gets through carries no tally
	// into the next thing that goes wrong.
	h := newHarness(t, defaultTemplate)

	first := h.capture("incoming/twitch-examplechannel-1.ts")
	h.attachBroadcast(first.ID, "a title", "")
	h.processor.verifyErr = errors.New("output has 1 audio stream, source has 2")

	for sweep := 1; sweep < maxRemuxAttempts; sweep++ {
		if _, err := h.organizer.Finalize(context.Background(), first.ID); err == nil {
			t.Fatalf("sweep %d: Finalize() err = nil, want the failure surfaced", sweep)
		}
	}
	if state := h.reload(first.ID).State; state == store.StateFailed {
		t.Fatalf("state = %q after %d failures, want the recording still retried",
			state, maxRemuxAttempts-1)
	}

	h.processor.verifyErr = nil
	if _, err := h.organizer.Finalize(context.Background(), first.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want the recording finished once the failure cleared", err)
	}

	// A budget is per recording, so one recording's failures cannot spend
	// another's.
	h.processor.verifyErr = errors.New("output has 1 audio stream, source has 2")
	second := h.capture("incoming/twitch-examplechannel-2.ts")
	h.attachBroadcast(second.ID, "another title", "")
	if _, err := h.organizer.Finalize(context.Background(), second.ID); err == nil {
		t.Fatal("Finalize() err = nil, want the failure surfaced")
	}
	if state := h.reload(second.ID).State; state == store.StateFailed {
		t.Errorf("state = %q after one failure, want a recording given up on only after %d",
			state, maxRemuxAttempts)
	}
}

func TestFinalize_TheBudgetCountsOnlyRecordingsFailingNow(t *testing.T) {
	// The daemon runs for months. A count kept for every recording that
	// ever failed a remux is a count that grows with the library, and none
	// of those recordings reaches the remux stage again.
	h := newHarness(t, defaultTemplate)

	settled := h.capture("incoming/twitch-examplechannel-1.ts")
	h.attachBroadcast(settled.ID, "a title", "")
	h.processor.verifyErr = errors.New("duration differs")
	if _, err := h.organizer.Finalize(context.Background(), settled.ID); err == nil {
		t.Fatal("Finalize() err = nil, want the failure surfaced")
	}

	h.processor.verifyErr = nil
	if _, err := h.organizer.Finalize(context.Background(), settled.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want the recording finished", err)
	}

	givenUp := h.capture("incoming/twitch-examplechannel-2.ts")
	h.attachBroadcast(givenUp.ID, "another title", "")
	h.processor.verifyErr = errors.New("duration differs")
	for sweep := 1; sweep <= maxRemuxAttempts; sweep++ {
		if _, err := h.organizer.Finalize(context.Background(), givenUp.ID); err == nil {
			t.Fatalf("sweep %d: Finalize() err = nil, want the failure surfaced", sweep)
		}
	}
	if state := h.reload(givenUp.ID).State; state != store.StateFailed {
		t.Fatalf("state = %q, want %q", state, store.StateFailed)
	}

	if got := h.organizer.remuxBudget.tracking(); got != 0 {
		t.Errorf("the budget is counting %d recordings, want none once each one is settled", got)
	}
}

func TestFinalize_MissingCaptureFile(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.ts")
	if err := os.Remove(filepath.Join(h.root, recording.Path)); err != nil {
		t.Fatalf("removing capture: %v", err)
	}

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err == nil {
		t.Error("Finalize() err = nil, want an error when the file is gone")
	}
}

func TestFinalize_UnknownRecording(t *testing.T) {
	h := newHarness(t, defaultTemplate)

	if _, err := h.organizer.Finalize(context.Background(), 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Finalize() err = %v, want it to wrap ErrNotFound", err)
	}
}

// ///////////////////////////////////////////////
// Collisions
// ///////////////////////////////////////////////

func TestFinalize_DeduplicatesACollidingName(t *testing.T) {
	// Two broadcasts can legitimately share a channel, date, and title.
	h := newHarness(t, "{author} - {date} - {title}.{ext}")

	first := h.capture("incoming/twitch-examplechannel-1.ts")
	h.attachBroadcast(first.ID, "same title", "")
	if _, err := h.organizer.Finalize(context.Background(), first.ID); err != nil {
		t.Fatalf("first Finalize() err = %v, want nil", err)
	}

	second := h.capture("incoming/twitch-examplechannel-2.ts")
	broadcast, err := h.store.UpsertBroadcast(store.Broadcast{
		ChannelID: h.channel.ID, RemoteID: "stream-2",
		StartedAt: startedAt.Add(6 * time.Hour), Title: "same title", Source: store.SourceLive,
	})
	if err != nil {
		t.Fatalf("UpsertBroadcast() err = %v, want nil", err)
	}
	if err := h.store.SetBroadcast(second.ID, &broadcast.ID); err != nil {
		t.Fatalf("SetBroadcast() err = %v, want nil", err)
	}

	got, err := h.organizer.Finalize(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("second Finalize() err = %v, want nil", err)
	}

	if !strings.Contains(got.Path, "(2)") {
		t.Errorf("Outcome.Path = %q, want a deduplicated name", got.Path)
	}
	if !h.exists("ExampleChannel - 2026-03-04 - same title.mkv") {
		t.Error("first recording was overwritten, want both kept")
	}
	if !h.exists(got.Path) {
		t.Errorf("second recording %q missing", got.Path)
	}
}

// ///////////////////////////////////////////////
// Purge
// ///////////////////////////////////////////////

// trashed reads a recording's state and path back after a purge.
func (h *harness) trashed(recordingID int64) store.Recording {
	h.t.Helper()
	return h.reload(recordingID)
}

func TestTrash_MovesTheRecordingOutOfTheLibrary(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}
	before := h.reload(recording.ID)

	moved, err := h.organizer.Trash(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Trash() err = %v, want nil", err)
	}

	if h.exists(before.Path) {
		t.Errorf("library file %q survived the purge", before.Path)
	}
	if !h.exists(moved) {
		t.Errorf("trashed file %q is missing", moved)
	}
	got := h.trashed(recording.ID)
	if got.State != store.StateTrashed {
		t.Errorf("State = %q, want %q", got.State, store.StateTrashed)
	}
	if got.Path != moved {
		t.Errorf("stored Path = %q, want the trashed path %q", got.Path, moved)
	}
}

func TestTrash_TakesTheSidecarWithIt(t *testing.T) {
	// A sidecar left in the library names a recording that is not there, and
	// the rebuild that reads sidecars would restore a row for a file the
	// operator deleted.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}
	before := h.reload(recording.ID)
	if !h.exists(before.Path + ".json") {
		t.Fatalf("no sidecar at %q to begin with", before.Path+".json")
	}

	moved, err := h.organizer.Trash(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Trash() err = %v, want nil", err)
	}

	if h.exists(before.Path + ".json") {
		t.Errorf("sidecar %q survived in the library", before.Path+".json")
	}
	if !h.exists(moved + ".json") {
		t.Errorf("sidecar did not follow the recording to %q", moved+".json")
	}
}

// ///////////////////////////////////////////////
// Recompress
// ///////////////////////////////////////////////

// recompressible returns a finished recording with a file behind it.
func recompressible(t *testing.T, h *harness, relPath string) store.Recording {
	t.Helper()

	recording := h.capture(relPath)
	if err := h.store.SetState(recording.ID, store.StateComplete); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}
	return recording
}

// writeSmaller is an encode that produces a believably denser file.
func writeSmaller(_ context.Context, _, output string) error {
	return os.WriteFile(output, []byte("dense"), 0o644)
}

func TestRecompress_PutsTheReEncodeInTheOriginalsPlace(t *testing.T) {
	// The path is what every other part of the library reaches the file
	// by, so a re-encode that lands beside it under a new name is a
	// recording nothing can find.
	h := newHarness(t, defaultTemplate)
	recording := recompressible(t, h, "ExampleChannel/2026/old.mkv")

	if err := h.organizer.Recompress(context.Background(), recording.ID, false, writeSmaller); err != nil {
		t.Fatalf("Recompress() err = %v, want nil", err)
	}

	full := filepath.Join(h.root, recording.Path)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("reading %s: %v", full, err)
	}
	if string(data) != "dense" {
		t.Errorf("the file at the recording's path is %q, want the re-encode", data)
	}

	got, err := h.store.Recording(recording.ID)
	if err != nil {
		t.Fatalf("Recording() err = %v, want nil", err)
	}
	if got.RecompressedAt == nil {
		t.Error("RecompressedAt = nil, want the re-encode recorded")
	}
	if got.Bytes != int64(len("dense")) {
		t.Errorf("Bytes = %d, want the size of the re-encode", got.Bytes)
	}
}

func TestRecompress_LeavesNothingBehindByDefault(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	recording := recompressible(t, h, "ExampleChannel/2026/old.mkv")

	if err := h.organizer.Recompress(context.Background(), recording.ID, false, writeSmaller); err != nil {
		t.Fatalf("Recompress() err = %v, want nil", err)
	}

	full := filepath.Join(h.root, recording.Path)
	for _, leftover := range []string{
		full + recompressSuffix, full + supersededSuffix, full + originalSuffix,
	} {
		if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the re-encode", filepath.Base(leftover))
		}
	}
}

func TestRecompress_KeepsTheOriginalWhenAsked(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	recording := recompressible(t, h, "ExampleChannel/2026/old.mkv")

	if err := h.organizer.Recompress(context.Background(), recording.ID, true, writeSmaller); err != nil {
		t.Fatalf("Recompress() err = %v, want nil", err)
	}

	kept := filepath.Join(h.root, recording.Path) + originalSuffix
	data, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("reading the kept original: %v", err)
	}
	if string(data) != "captured bytes" {
		t.Errorf("the kept original holds %q, want the pre-encode bytes", data)
	}
}

func TestRecompress_KeepsTheRecordingWhenAStepFails(t *testing.T) {
	// This is the whole risk of the rung. Every failure has to leave a
	// playable file at the recording's own path.
	cases := []struct {
		name    string
		arrange func(h *harness)
		encode  func(ctx context.Context, source, output string) error
	}{
		{
			name:   "the encode itself fails",
			encode: func(context.Context, string, string) error { return errors.New("no encoder") },
		},
		{
			name:    "the output does not verify",
			arrange: func(h *harness) { h.processor.verifyErr = errors.New("shorter than the source") },
			encode:  writeSmaller,
		},
		{
			// This is the case that reaches the rollback. The original is
			// already set aside by the time the install fails, so nothing
			// but the restore puts it back.
			name:   "the encoder reports success and writes nothing",
			encode: func(context.Context, string, string) error { return nil },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, defaultTemplate)
			recording := recompressible(t, h, "ExampleChannel/2026/old.mkv")
			if tc.arrange != nil {
				tc.arrange(h)
			}

			if err := h.organizer.Recompress(
				context.Background(), recording.ID, false, tc.encode); err == nil {
				t.Fatal("Recompress() err = nil, want the failure reported")
			}

			full := filepath.Join(h.root, recording.Path)
			data, err := os.ReadFile(full)
			if err != nil {
				t.Fatalf("the recording is gone after a failed re-encode: %v", err)
			}
			if string(data) != "captured bytes" {
				t.Errorf("the recording holds %q, want the original bytes", data)
			}

			got, err := h.store.Recording(recording.ID)
			if err != nil {
				t.Fatalf("Recording() err = %v, want nil", err)
			}
			if got.RecompressedAt != nil {
				t.Error("a failed re-encode was recorded as done")
			}
		})
	}
}

func TestRecompress_RefusesWhatMustNotBeReEncoded(t *testing.T) {
	cases := []struct {
		name  string
		state store.State
		twice bool
	}{
		{name: "a recording still being captured", state: store.StateCapturing},
		{name: "a recording the organizer has not finished", state: store.StateAwaitingMetadata},
		{name: "a recording in the trash", state: store.StateTrashed},
		{name: "a recording already re-encoded", state: store.StateComplete, twice: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, defaultTemplate)
			recording := recompressible(t, h, "ExampleChannel/2026/old.mkv")
			if tc.twice {
				if err := h.organizer.Recompress(
					context.Background(), recording.ID, false, writeSmaller); err != nil {
					t.Fatalf("the first Recompress() err = %v, want nil", err)
				}
			}
			if err := h.store.SetState(recording.ID, tc.state); err != nil {
				t.Fatalf("SetState() err = %v, want nil", err)
			}

			err := h.organizer.Recompress(context.Background(), recording.ID, false, writeSmaller)

			if !errors.Is(err, ErrNotRecompressable) {
				t.Errorf("Recompress() err = %v, want ErrNotRecompressable", err)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Purge refusals and release
// ///////////////////////////////////////////////

func TestTrash_WithNoSidecarStillPurges(t *testing.T) {
	// The sidecar is written after the media moves, so a recording purged
	// between those two steps has none. Refusing would strand exactly the
	// recording an operator is most likely to be clearing out.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	// A failed capture is exactly this shape: bytes on disk, no sidecar,
	// and nothing that will ever write one.
	if err := h.store.SetState(recording.ID, store.StateFailed); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}

	if _, err := h.organizer.Trash(context.Background(), recording.ID); err != nil {
		t.Fatalf("Trash() err = %v, want nil", err)
	}

	if got := h.trashed(recording.ID).State; got != store.StateTrashed {
		t.Errorf("State = %q, want %q", got, store.StateTrashed)
	}
}

func TestTrash_RefusesAStateThatMayNotBePurged(t *testing.T) {
	// The ranking is a snapshot. Between the operator reading it and
	// confirming, the recorder can start writing to that row.
	tests := []struct {
		name  string
		state store.State
	}{
		{name: "capturing", state: store.StateCapturing},
		{name: "awaiting finalize", state: store.StateAwaitingFinalize},
		{name: "already trashed", state: store.StateTrashed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, defaultTemplate)
			recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
			if err := h.store.SetState(recording.ID, tt.state); err != nil {
				t.Fatalf("SetState() err = %v, want nil", err)
			}

			_, err := h.organizer.Trash(context.Background(), recording.ID)

			if !errors.Is(err, ErrNotPurgeable) {
				t.Errorf("Trash() err = %v, want it to wrap ErrNotPurgeable", err)
			}
			if !h.exists(recording.Path) {
				t.Errorf("file %q was moved despite the refusal", recording.Path)
			}
		})
	}
}

func TestTrash_RefusesAPinnedRecording(t *testing.T) {
	// Pinning is the operator's own instruction, and a purge list built
	// before they set it must not be able to act against it.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	if err := h.store.SetPinned(recording.ID, true); err != nil {
		t.Fatalf("SetPinned() err = %v, want nil", err)
	}

	_, err := h.organizer.Trash(context.Background(), recording.ID)

	if !errors.Is(err, ErrNotPurgeable) {
		t.Errorf("Trash() err = %v, want it to wrap ErrNotPurgeable", err)
	}
	if !h.exists(recording.Path) {
		t.Errorf("pinned file %q was moved", recording.Path)
	}
}

func TestTrash_RefusesWhileAFinalizeHoldsTheRecording(t *testing.T) {
	// This is the whole reason the purge lives beside Finalize. The states
	// an operator may purge include two the sweep is still retrying, so a
	// purge and a finalize can reach one row at the same instant.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	h.processor.entered = make(chan struct{})
	h.processor.release = make(chan struct{})
	finalized := make(chan error, 1)
	go func() {
		_, err := h.organizer.Finalize(context.Background(), recording.ID)
		finalized <- err
	}()
	<-h.processor.entered

	_, err := h.organizer.Trash(context.Background(), recording.ID)

	close(h.processor.release)
	if finalizeErr := <-finalized; finalizeErr != nil {
		t.Fatalf("Finalize() err = %v, want nil", finalizeErr)
	}
	if !errors.Is(err, ErrBusy) {
		t.Errorf("Trash() during a finalize err = %v, want it to wrap ErrBusy", err)
	}
}

func TestTrash_NamesTheFileByRecordingID(t *testing.T) {
	// The trash is flat while the library is nested, so two recordings that
	// render the same library name would collide there. The id is what
	// keeps them apart, and the claim is O_EXCL, so a collision is a lost
	// recording rather than a retry.
	h := newHarness(t, defaultTemplate)
	// Two channels, two directories, one file name. Nested in the library
	// they cannot collide, and flattened into the trash they would.
	first := h.capture("ExampleChannel/2026/ExampleChannel - 2026-03-04 21-15 - Stream.mkv")
	second := h.capture("OtherChannel/2026/ExampleChannel - 2026-03-04 21-15 - Stream.mkv")
	for _, recording := range []store.Recording{first, second} {
		if err := h.store.SetState(recording.ID, store.StateFailed); err != nil {
			t.Fatalf("SetState() err = %v, want nil", err)
		}
	}

	firstMoved, err := h.organizer.Trash(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Trash(first) err = %v, want nil", err)
	}
	secondMoved, err := h.organizer.Trash(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("Trash(second) err = %v, want nil", err)
	}

	if firstMoved == secondMoved {
		t.Fatalf("both recordings trashed to %q", firstMoved)
	}
	for _, moved := range []string{firstMoved, secondMoved} {
		if !h.exists(moved) {
			t.Errorf("trashed file %q is missing", moved)
		}
	}
}

func TestRelease_DeletesTheFileAndTheRow(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}
	moved, err := h.organizer.Trash(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Trash() err = %v, want nil", err)
	}

	if err := h.organizer.Release(context.Background(), recording.ID); err != nil {
		t.Fatalf("Release() err = %v, want nil", err)
	}

	if h.exists(moved) {
		t.Errorf("trashed file %q survived the release", moved)
	}
	if h.exists(moved + ".json") {
		t.Errorf("trashed sidecar %q survived the release", moved+".json")
	}
	if _, err := h.store.Recording(recording.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Recording() after release err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestRelease_RefusesAnythingNotInTheTrash(t *testing.T) {
	// This is the only thing in the project that deletes a recording, so
	// it may only ever finish a decision the operator already made. A
	// state check that let a live recording through would turn the
	// automatic release into an automatic delete.
	tests := []struct {
		name  string
		state store.State
	}{
		{name: "complete", state: store.StateComplete},
		{name: "capturing", state: store.StateCapturing},
		{name: "failed", state: store.StateFailed},
		{name: "awaiting finalize", state: store.StateAwaitingFinalize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, defaultTemplate)
			recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
			if err := h.store.SetState(recording.ID, tt.state); err != nil {
				t.Fatalf("SetState() err = %v, want nil", err)
			}

			err := h.organizer.Release(context.Background(), recording.ID)

			if !errors.Is(err, ErrNotTrashed) {
				t.Errorf("Release() err = %v, want it to wrap ErrNotTrashed", err)
			}
			if !h.exists(recording.Path) {
				t.Errorf("file %q was deleted despite the refusal", recording.Path)
			}
			if _, err := h.store.Recording(recording.ID); err != nil {
				t.Errorf("the row was deleted despite the refusal: %v", err)
			}
		})
	}
}

func TestRelease_WithTheFileAlreadyGoneStillClearsTheRow(t *testing.T) {
	// An operator who emptied the trash by hand leaves rows naming files
	// that are not there. Refusing would keep those bytes counted against
	// the budget forever, which is the opposite of what a release is for.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	if err := h.store.SetState(recording.ID, store.StateFailed); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}
	moved, err := h.organizer.Trash(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Trash() err = %v, want nil", err)
	}
	if err := os.Remove(filepath.Join(h.root, filepath.FromSlash(moved))); err != nil {
		t.Fatalf("removing the trashed file: %v", err)
	}

	if err := h.organizer.Release(context.Background(), recording.ID); err != nil {
		t.Fatalf("Release() err = %v, want nil", err)
	}
	if _, err := h.store.Recording(recording.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Recording() after release err = %v, want the row gone", err)
	}
}

func TestTrash_StopsTheBroadcastBeingFetchedAgain(t *testing.T) {
	// Purging is how an operator makes room. Without this the recovery pass
	// reads the broadcast as missed, downloads it again inside one backfill
	// interval, and spends the bandwidth and the space the purge just freed
	// on the platform's muted copy of what was deleted.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	broadcast := h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	if _, err := h.organizer.Trash(context.Background(), recording.ID); err != nil {
		t.Fatalf("Trash() err = %v, want nil", err)
	}

	fetch, err := h.store.FetchFor(broadcast.ID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want the purge to have written a fetch row", err)
	}
	if fetch.State != store.FetchTerminal {
		t.Errorf("FetchState = %q, want %q so no pass fetches it again", fetch.State, store.FetchTerminal)
	}
	if fetch.LastError == "" {
		t.Error("LastError = \"\", want the refusal to say the operator purged it")
	}
}

func TestTrash_LeavesABroadcastThatKeptARecordingRecoverable(t *testing.T) {
	// Purging one of two files says nothing about the other. The rule is
	// that deleting every copy means the operator does not want this
	// broadcast, not that one deleted file condemns it.
	h := newHarness(t, defaultTemplate)
	first := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	broadcast := h.attachBroadcast(first.ID, "Midnight Build Stream", "Just Chatting")
	if _, err := h.organizer.Finalize(context.Background(), first.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}

	second := h.capture("incoming/twitch-examplechannel-1772662500.ts")
	if err := h.store.SetBroadcast(second.ID, &broadcast.ID); err != nil {
		t.Fatalf("SetBroadcast() err = %v, want nil", err)
	}
	if err := h.store.SetState(second.ID, store.StateComplete); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}

	if _, err := h.organizer.Trash(context.Background(), first.ID); err != nil {
		t.Fatalf("Trash() err = %v, want nil", err)
	}

	if _, err := h.store.FetchFor(broadcast.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FetchFor() err = %v, want no fetch row: the broadcast still holds a recording", err)
	}
}

func TestRelease_StopsTheBroadcastBeingFetchedAgain(t *testing.T) {
	// The release is where the last copy actually leaves the disk, and a
	// library whose trash grace has expired has no other signal that the
	// operator meant it.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	broadcast := h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
	if err := h.store.SetState(recording.ID, store.StateFailed); err != nil {
		t.Fatalf("SetState() err = %v, want nil", err)
	}
	if _, err := h.organizer.Trash(context.Background(), recording.ID); err != nil {
		t.Fatalf("Trash() err = %v, want nil", err)
	}

	if err := h.organizer.Release(context.Background(), recording.ID); err != nil {
		t.Fatalf("Release() err = %v, want nil", err)
	}

	fetch, err := h.store.FetchFor(broadcast.ID)
	if err != nil {
		t.Fatalf("FetchFor() err = %v, want the release to have written a fetch row", err)
	}
	if fetch.State != store.FetchTerminal {
		t.Errorf("FetchState = %q, want %q so no pass fetches it again", fetch.State, store.FetchTerminal)
	}
}

func TestRelease_RefusesWhileAFinalizeHoldsTheRecording(t *testing.T) {
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	h.processor.entered = make(chan struct{})
	h.processor.release = make(chan struct{})
	finalized := make(chan error, 1)
	go func() {
		_, err := h.organizer.Finalize(context.Background(), recording.ID)
		finalized <- err
	}()
	<-h.processor.entered

	err := h.organizer.Release(context.Background(), recording.ID)

	close(h.processor.release)
	if finalizeErr := <-finalized; finalizeErr != nil {
		t.Fatalf("Finalize() err = %v, want nil", finalizeErr)
	}
	if !errors.Is(err, ErrBusy) {
		t.Errorf("Release() during a finalize err = %v, want it to wrap ErrBusy", err)
	}
}

// ///////////////////////////////////////////////
// The container
// ///////////////////////////////////////////////

func TestFinalize_RemuxesAFileThatArrivedInAnotherContainer(t *testing.T) {
	// A recovered broadcast arrives as whatever the platform served, which
	// is routinely mp4 rather than the capture engine's transport stream.
	//
	// The name it is about to be given carries the configured container's
	// extension. A file left in its own container would have its bytes
	// renamed onto a path claiming to be something they are not, and every
	// player and every later verification would read that path and be
	// wrong about what it holds.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.mp4")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}
	if !got.Remuxed {
		t.Error("Outcome.Remuxed = false for an mp4, want it remuxed into the container")
	}

	want := "ExampleChannel/2026/ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv"
	if got.Path != want {
		t.Errorf("Outcome.Path = %q, want %q", got.Path, want)
	}
	if h.exists(recording.Path) {
		t.Errorf("source %q survived, want it consumed by the remux", recording.Path)
	}
}

func TestRemuxIfNeeded_DiscardsAnUnverifiedLeftover(t *testing.T) {
	// Power loss, a forced reboot, or the scheduler's Halt during the remux
	// leaves the output file behind. Remux refuses a path something already
	// holds, so every later sweep dies the same way and the recording goes
	// failed, which no command resets. The whole broadcast leaves the
	// library permanently while its capture sits in incoming.
	//
	// The argument that makes discarding it safe is structural: the row
	// still names the capture, and the path is only recorded once the output
	// is verified, so a leftover seen while the row names the capture was
	// never verified by construction.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	leftover := filepath.Join(h.root, "incoming", "twitch-examplechannel-1772658900.mkv")
	if err := os.WriteFile(leftover, []byte("half a remux"), 0o644); err != nil {
		t.Fatalf("writing the leftover: %v", err)
	}

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want the leftover discarded and the remux run", err)
	}
	if !got.Remuxed {
		t.Error("Outcome.Remuxed = false, want the remux to have run over the leftover")
	}

	stored := h.reload(recording.ID)
	if stored.State != store.StateComplete {
		t.Errorf("State = %q, want %q", stored.State, store.StateComplete)
	}
}

func TestRemuxIfNeeded_AMissingToolDoesNotSpendTheBudget(t *testing.T) {
	// The budget exists to stop a file that cannot be remuxed being
	// rewritten on every sweep forever. A tool that is not installed says
	// nothing about the file, and spending the budget on it abandons every
	// pending recording in three sweeps for a reason an operator fixes by
	// installing one program.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.ts")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
	h.processor.remuxErr = fmt.Errorf("locating ffmpeg: %w", deps.ErrNotFound)

	for range maxRemuxAttempts + 1 {
		if _, err := h.organizer.Finalize(context.Background(), recording.ID); err == nil {
			t.Fatal("Finalize() err = nil with no ffmpeg installed, want it reported")
		}
	}

	if got := h.reload(recording.ID).State; got == store.StateFailed {
		t.Errorf("State = %q, want the recording still in the sweep: nothing said the media was bad", got)
	}
	if !h.exists(recording.Path) {
		t.Error("the capture was discarded, want it kept for the sweep that runs once ffmpeg is there")
	}
}

func TestFinalize_LeavesAFileAlreadyInTheContainer(t *testing.T) {
	// Remuxing a file into the container it is already in would spend an
	// ffmpeg pass to produce the same bytes.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1772658900.mkv")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	got, err := h.organizer.Finalize(context.Background(), recording.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want nil", err)
	}
	if got.Remuxed {
		t.Error("Outcome.Remuxed = true for a file already in the container, want false")
	}
}

func TestFinalize_RefusesToAdoptAFileItCannotShowIsTheRecording(t *testing.T) {
	// Every segment of a rendered name comes from the stream, so a title
	// can be chosen to land on a file the operator put in the library.
	// Adopting on position alone would hand that file to a row the purge
	// can act on.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.mkv")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")

	target := "ExampleChannel/2026/ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream.mkv"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(h.root, target)), 0o755); err != nil {
		t.Fatalf("creating the library directory: %v", err)
	}
	// Neither the recorder's size nor a length it could have captured.
	theirs := []byte("a home video the operator put here themselves")
	if err := os.WriteFile(filepath.Join(h.root, target), theirs, 0o644); err != nil {
		t.Fatalf("staging the operator's file: %v", err)
	}
	if err := os.Remove(filepath.Join(h.root, recording.Path)); err != nil {
		t.Fatalf("removing the capture: %v", err)
	}
	h.processor.mediaDuration = 3 * time.Hour

	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err == nil {
		t.Fatal("Finalize() adopted a file it cannot show is the recording, want a refusal")
	}

	if stored := h.reload(recording.ID); stored.Path == target {
		t.Errorf("stored Path = %q, want the row to keep naming its own capture", stored.Path)
	}
	if got, err := os.ReadFile(filepath.Join(h.root, target)); err != nil || string(got) != string(theirs) {
		t.Errorf("the operator's file read %q, %v; want it untouched", got, err)
	}
}

func TestFinalize_AdoptsAMoveThatLandedOnADeduplicatedName(t *testing.T) {
	// A reconnect splits one broadcast into two recordings that render one
	// name, so an interrupted move is as likely to have landed on " (2)"
	// as on the bare name. Searching only the bare name leaves that
	// recording with no row naming its file and nothing able to find it.
	h := newHarness(t, defaultTemplate)
	first := h.capture("incoming/twitch-examplechannel-1.mkv")
	h.attachBroadcast(first.ID, "Midnight Build Stream", "Just Chatting")
	if _, err := h.organizer.Finalize(context.Background(), first.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want the first recording filed", err)
	}

	second := h.capture("incoming/twitch-examplechannel-2.mkv")
	h.attachBroadcast(second.ID, "Midnight Build Stream", "Just Chatting")

	want := "ExampleChannel/2026/ExampleChannel - 2026-03-04 21-15 - Midnight Build Stream (2).mkv"
	if err := os.Rename(filepath.Join(h.root, second.Path), filepath.Join(h.root, want)); err != nil {
		t.Fatalf("staging the moved file: %v", err)
	}

	got, err := h.organizer.Finalize(context.Background(), second.ID)
	if err != nil {
		t.Fatalf("Finalize() err = %v, want the deduplicated name adopted", err)
	}
	if got.Path != want {
		t.Errorf("Outcome.Path = %q, want %q", got.Path, want)
	}
	if stored := h.reload(second.ID); stored.Path != filepath.ToSlash(want) {
		t.Errorf("stored Path = %q, want %q", stored.Path, filepath.ToSlash(want))
	}
}

func TestTrash_LeavesTheFileWhereTheRowSaysWhenTheSidecarCannotMove(t *testing.T) {
	// The state is recorded after the move, so a move that half completes
	// must leave the row and the filesystem agreeing. Media in the trash
	// under a row that still reads complete is media nothing looks at
	// again: releasing walks trashed rows and this one is not one.
	h := newHarness(t, defaultTemplate)
	recording := h.capture("incoming/twitch-examplechannel-1.mkv")
	h.attachBroadcast(recording.ID, "Midnight Build Stream", "Just Chatting")
	if _, err := h.organizer.Finalize(context.Background(), recording.ID); err != nil {
		t.Fatalf("Finalize() err = %v, want the recording filed", err)
	}
	filed := h.reload(recording.ID)

	// Claim the name the sidecar is about to move to, which is what a
	// rename refuses on.
	trashed := fmt.Sprintf("%d-%s", recording.ID, filepath.Base(filepath.FromSlash(filed.Path)))
	blocker := filepath.Join(paths.TrashDir(h.root), trashed+sidecarSuffix)
	if err := os.MkdirAll(filepath.Dir(blocker), 0o755); err != nil {
		t.Fatalf("creating the trash directory: %v", err)
	}
	if err := os.WriteFile(blocker, []byte("taken"), 0o644); err != nil {
		t.Fatalf("claiming the trash sidecar name: %v", err)
	}

	if _, err := h.organizer.Trash(context.Background(), recording.ID); err == nil {
		t.Fatal("Trash() reported success with the sidecar move refused, want an error")
	}

	after := h.reload(recording.ID)
	if _, err := os.Stat(filepath.Join(h.root, filepath.FromSlash(after.Path))); err != nil {
		t.Errorf("the row names %q, which is not there: %v", after.Path, err)
	}
}
