package backfill

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/fetch"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/space"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// fakePatchStore holds one broadcast's recordings and their gaps.
type fakePatchStore struct {
	recordings []store.Recording
	gaps       map[int64][]store.Gap
	filled     []int64
	created    []store.Recording
	createErr  error
	fillErr    error
	nextID     int64
}

// patchDownloader records the requests it was given.
type patchDownloader struct {
	requests []fetch.Request
	err      error
	// onDownload runs as the download starts, so a test can cancel the
	// context the way a shutdown does with one already in flight.
	onDownload func()
}

// fakeMeasurer reports a scripted media length for whatever a patch
// downloaded.
type fakeMeasurer struct {
	duration time.Duration
	err      error
	measured []string
	// silent is what the audio measurement answers, and listened records
	// every file it was asked about, so a test can prove an ordinary patch
	// is never measured for audio.
	silent    bool
	silentErr error
	listened  []string
}

// patchAttemptLimit is the cap the tests give a patcher. Small, so a test
// that exhausts a gap does not have to run the real default's worth of
// passes to reach the end.
const patchAttemptLimit = 3

// patchStart anchors every case in this file.
var patchStart = time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC)

// Duration implements Measurer.
func (f *fakeMeasurer) Duration(_ context.Context, path string) (time.Duration, error) {
	f.measured = append(f.measured, path)
	return f.duration, f.err
}

// SilentBetween implements Measurer.
func (f *fakeMeasurer) SilentBetween(_ context.Context, path string, _, _ time.Duration) (bool, error) {
	f.listened = append(f.listened, path)
	return f.silent, f.silentErr
}

// RecordingsForBroadcast implements PatchStore.
func (f *fakePatchStore) RecordingsForBroadcast(int64) ([]store.Recording, error) {
	return append([]store.Recording(nil), f.recordings...), nil
}

// AddGap implements PatchStore. Detection is exercised in gaps_test.go, so
// this only has to be repeatable the way the real unique span is.
func (f *fakePatchStore) AddGap(recordingID int64, start, end time.Duration, reason string) (store.Gap, error) {
	for _, existing := range f.gaps[recordingID] {
		if existing.Start == start && existing.End == end {
			return existing, nil
		}
	}
	f.nextID++
	gap := store.Gap{
		ID: f.nextID, RecordingID: recordingID,
		Start: start, End: end, Reason: reason,
	}
	if f.gaps == nil {
		f.gaps = map[int64][]store.Gap{}
	}
	f.gaps[recordingID] = append(f.gaps[recordingID], gap)
	return gap, nil
}

// ChargeGap implements PatchStore, recording a failed patch the way the
// store does so a test can see a gap stop being retried.
func (f *fakePatchStore) ChargeGap(id int64, limit int, terminal bool) error {
	for _, gaps := range f.gaps {
		for i := range gaps {
			if gaps[i].ID != id {
				continue
			}
			if terminal {
				gaps[i].Attempts = limit
				return nil
			}
			gaps[i].Attempts++
			return nil
		}
	}
	return fmt.Errorf("no gap %d", id)
}

// Gaps implements PatchStore.
func (f *fakePatchStore) Gaps(recordingID int64) ([]store.Gap, error) {
	return append([]store.Gap(nil), f.gaps[recordingID]...), nil
}

// FillGap implements PatchStore.
func (f *fakePatchStore) FillGap(id int64, at time.Time) error {
	if f.fillErr != nil {
		return f.fillErr
	}
	f.filled = append(f.filled, id)
	for recordingID, gaps := range f.gaps {
		for i := range gaps {
			if gaps[i].ID == id {
				stamp := at
				f.gaps[recordingID][i].FilledAt = &stamp
			}
		}
	}
	return nil
}

// CreateRecording implements PatchStore.
func (f *fakePatchStore) CreateRecording(r store.Recording) (store.Recording, error) {
	if f.createErr != nil {
		return store.Recording{}, f.createErr
	}
	f.nextID++
	r.ID = f.nextID
	f.created = append(f.created, r)
	return r, nil
}

// Download implements Downloader.
func (d *patchDownloader) Download(_ context.Context, request fetch.Request) (fetch.Result, error) {
	d.requests = append(d.requests, request)
	if d.onDownload != nil {
		d.onDownload()
	}
	if d.err != nil {
		return fetch.Result{}, d.err
	}
	// The tool substitutes the real extension for the template.
	produced := strings.Replace(request.Output, ".%(ext)s", ".mp4", 1)
	// The bytes are written, because what happens to them afterwards is what
	// several cases are about: a rejected patch has to be gone so the next
	// pass fetches again, and an unmeasured one has to survive.
	if err := os.MkdirAll(filepath.Dir(produced), 0o755); err == nil {
		_ = os.WriteFile(produced, []byte("media"), 0o644)
	}
	return fetch.Result{Path: produced}, nil
}

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// gappedBroadcast returns a broadcast whose capture stopped and restarted,
// leaving one hole in the middle.
func gappedBroadcast() (store.Broadcast, *fakePatchStore) {
	firstEnd := patchStart.Add(10 * time.Minute)
	secondStart := patchStart.Add(25 * time.Minute)
	ended := patchStart.Add(time.Hour)

	broadcast := store.Broadcast{
		ID: 1, ChannelID: 1, RemoteID: "v100001",
		URL:          "https://example.com/videos/v100001",
		StartedAt:    patchStart,
		VodStartedAt: &patchStart,
		EndedAt:      &ended,
	}
	captures := &fakePatchStore{
		recordings: []store.Recording{
			{
				ID: 1, State: store.StateComplete, Origin: store.OriginLive,
				StartedAt: patchStart, EndedAt: &firstEnd,
			},
			{
				ID: 2, State: store.StateComplete, Origin: store.OriginLive,
				StartedAt: secondStart, EndedAt: &ended,
			},
		},
	}
	return broadcast, captures
}

// newPatcher builds a patcher over a temporary library.
//
// With no measurer supplied it gets one answering the fixture hole's own
// length, so the length check passes and a case that is not about it does
// not have to say so.
func newPatcher(t *testing.T, downloader Downloader, patches PatchStore,
	measure ...Measurer,
) (*Patcher, string) {
	t.Helper()

	measurer := Measurer(&fakeMeasurer{duration: 15 * time.Minute})
	if len(measure) > 0 {
		measurer = measure[0]
	}

	root := t.TempDir()
	patcher := NewPatcher(downloader, patches, func(context.Context, int64) error {
		return nil
	}, measurer, PatchOptions{LibraryRoot: root, MaxAttempts: patchAttemptLimit}, nil)
	return patcher, root
}

// ///////////////////////////////////////////////
// What a patch produces
// ///////////////////////////////////////////////

func TestPatch_FetchesOnlyTheMissingRange(t *testing.T) {
	// The point of patching rather than refetching. A reconnect costs
	// minutes out of an hours-long broadcast, and downloading the whole
	// thing again to recover them spends the bandwidth and the disk of the
	// entire capture.
	broadcast, patches := gappedBroadcast()
	downloader := &patchDownloader{}
	patcher, _ := newPatcher(t, downloader, patches)

	filled, err := patcher.Patch(context.Background(), broadcast, passChannels("examplechannel")[0],
		patchStart.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if filled != 1 {
		t.Fatalf("Patch filled %d gaps, want 1", filled)
	}

	if len(downloader.requests) != 1 {
		t.Fatalf("Patch made %d downloads, want 1", len(downloader.requests))
	}
	// The hole runs from 10 minutes in to 25 minutes in.
	if got, want := downloader.requests[0].Sections, "*600-1500"; got != want {
		t.Errorf("Sections = %q, want %q", got, want)
	}
}

func TestPatch_RefusesWhenTheLibraryHasNoRoom(t *testing.T) {
	// A patch writes into the same volume a capture does, and its bytes are
	// invisible to the size cap until the file is claimed. Left unbudgeted
	// it fills the library the capture budget is guarding.
	broadcast, patches := gappedBroadcast()
	downloader := &patchDownloader{}
	patcher, _ := newPatcher(t, downloader, patches)
	patcher.admit = func(int64) error {
		return &space.RefusalError{Limit: "library max_size", Need: 1 << 34, Have: 1 << 30}
	}

	_, err := patcher.Patch(context.Background(), broadcast, passChannels("examplechannel")[0],
		patchStart.Add(48*time.Hour))

	if !errors.Is(err, ErrNoRoom) {
		t.Fatalf("Patch() err = %v, want it to wrap %v", err, ErrNoRoom)
	}
	if len(downloader.requests) != 0 {
		t.Error("a download ran despite the refusal")
	}
	for _, gaps := range patches.gaps {
		for _, gap := range gaps {
			if gap.Attempts != 0 {
				t.Errorf("gap %d spent %d attempts on a full library, want 0", gap.ID, gap.Attempts)
			}
		}
	}
}

func TestPatch_DoesNotChargeAShutdown(t *testing.T) {
	// A gap gets five attempts, and the operator's own reboot must not
	// spend them. A download killed by a shutdown answers with an error
	// like any other, so without a recheck five reboots during a patch
	// abandon a hole the platform would have served.
	broadcast, patches := gappedBroadcast()

	ctx, cancel := context.WithCancel(context.Background())
	downloader := &patchDownloader{err: context.Canceled}
	downloader.onDownload = cancel
	patcher, _ := newPatcher(t, downloader, patches)

	if _, err := patcher.Patch(ctx, broadcast, passChannels("examplechannel")[0],
		patchStart.Add(48*time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Patch() err = %v, want the cancellation reported", err)
	}

	for _, gaps := range patches.gaps {
		for _, gap := range gaps {
			if gap.Attempts != 0 {
				t.Errorf("gap %d spent %d attempts on a shutdown, want 0", gap.ID, gap.Attempts)
			}
		}
	}
}

func TestPatch_SendsTheBroadcastAddress(t *testing.T) {
	// The same address failure the whole-broadcast fetch has: a bare video id
	// is not something yt-dlp can be pointed at, so every patch fails and
	// each one spends one of the gap's attempts.
	broadcast, patches := gappedBroadcast()
	downloader := &patchDownloader{}
	patcher, _ := newPatcher(t, downloader, patches)

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}

	if len(downloader.requests) != 1 {
		t.Fatalf("Patch made %d downloads, want 1", len(downloader.requests))
	}
	if got := downloader.requests[0].URL; got != broadcast.URL {
		t.Errorf("Request.URL = %q, want the broadcast's address %q", got, broadcast.URL)
	}
}

func TestPatch_RefusesABroadcastWithNoAddress(t *testing.T) {
	// A hole is still worth filing, so detection runs. Downloading is what
	// cannot happen, and charging the gap for it would spend its attempts on
	// a question the next discovery pass answers.
	broadcast, patches := gappedBroadcast()
	broadcast.URL = ""
	downloader := &patchDownloader{}
	patcher, _ := newPatcher(t, downloader, patches)

	filled, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))

	if !errors.Is(err, ErrNoAddress) {
		t.Errorf("Patch() err = %v, want ErrNoAddress", err)
	}
	if filled != 0 {
		t.Errorf("Patch filled %d gaps, want none with nowhere to fetch from", filled)
	}
	if len(downloader.requests) != 0 {
		t.Error("Patch downloaded from a broadcast with no address")
	}
	for _, gaps := range patches.gaps {
		for _, gap := range gaps {
			if gap.Attempts != 0 {
				t.Errorf("gap %d spent %d attempts, want none", gap.ID, gap.Attempts)
			}
		}
	}
	if len(patches.gaps) == 0 {
		t.Error("Patch filed no gaps, want the hole detected even with nowhere to fetch from")
	}
}

func TestPatch_NeverTouchesTheRecordingTheHoleIsIn(t *testing.T) {
	// The hard rule of this package, at its sharpest. A splice rewrites a
	// multi-gigabyte live capture to insert a couple of minutes, putting the
	// one irreplaceable file at risk to recover the replaceable part.
	broadcast, patches := gappedBroadcast()
	patcher, _ := newPatcher(t, &patchDownloader{}, patches)

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}

	if len(patches.created) != 1 {
		t.Fatalf("Patch created %d recordings, want 1", len(patches.created))
	}
	created := patches.created[0]
	for _, existing := range patches.recordings {
		if created.Path == existing.Path {
			t.Errorf("the patch wrote over recording %d's file", existing.ID)
		}
	}
	if created.Origin != store.OriginRecovered {
		t.Errorf("Origin = %q, want %q", created.Origin, store.OriginRecovered)
	}
}

func TestPatch_AnchorsTheNewRecordingWhereTheHoleStarts(t *testing.T) {
	// A patch that claimed the broadcast's own start would sort above the
	// capture it belongs after, and the calendar would show it covering a
	// stretch that is already held.
	broadcast, patches := gappedBroadcast()
	patcher, _ := newPatcher(t, &patchDownloader{}, patches)

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}

	want := patchStart.Add(10 * time.Minute)
	if got := patches.created[0].StartedAt; !got.Equal(want) {
		t.Errorf("StartedAt = %v, want %v", got, want)
	}
}

func TestPatch_WritesInsideTheIncomingDirectory(t *testing.T) {
	// The same constraint the whole-broadcast fetch has: a remote title must
	// never reach the filesystem, so the tool is told exactly where to write.
	broadcast, patches := gappedBroadcast()
	downloader := &patchDownloader{}
	patcher, root := newPatcher(t, downloader, patches)

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}

	if got, want := filepath.Dir(downloader.requests[0].Output), paths.IncomingDir(root); got != want {
		t.Errorf("wrote to %q, want %q", got, want)
	}
	if !strings.HasPrefix(patches.created[0].Path, paths.IncomingDirName+"/") {
		t.Errorf("stored path %q, want it under %q", patches.created[0].Path, paths.IncomingDirName)
	}
}

// ///////////////////////////////////////////////
// What a patch refuses to guess at
// ///////////////////////////////////////////////

func TestPatch_RefusesWhenTheStoredCopysStartIsUnknown(t *testing.T) {
	// A download range indexes into the stored copy from its own t=0, and
	// nothing else in the row says where that is. Guessing that it coincides
	// with the broadcast's start downloads a stretch the recorder already
	// holds and marks the hole patched, which is worse than leaving it open.
	broadcast, patches := gappedBroadcast()
	broadcast.VodStartedAt = nil
	downloader := &patchDownloader{}
	patcher, _ := newPatcher(t, downloader, patches)

	filled, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))

	if !errors.Is(err, ErrNoAnchor) {
		t.Errorf("Patch() err = %v, want ErrNoAnchor", err)
	}
	if filled != 0 {
		t.Errorf("Patch filled %d gaps, want none", filled)
	}
	if len(downloader.requests) != 0 {
		t.Error("Patch downloaded a range it could not index")
	}
	if len(patches.filled) != 0 {
		t.Errorf("Patch marked %v filled, want none", patches.filled)
	}
	// The attempts stay for when discovery learns the anchor, or the hole is
	// abandoned before it was ever patchable.
	for _, gaps := range patches.gaps {
		for _, gap := range gaps {
			if gap.Attempts != 0 {
				t.Errorf("gap %d spent %d attempts, want none", gap.ID, gap.Attempts)
			}
		}
	}
}

func TestPatch_RecoversAMutedGapWhereTheOriginalSurvives(t *testing.T) {
	// Some copies are stored with the audio as broadcast kept beside the
	// silenced variant. Where it survives, the hole is genuinely fillable
	// and refusing it loses footage the operator could have had.
	const original = "https://cdn.example/vod/chunked/index-dvr.m3u8"

	broadcast, patches := gappedBroadcast()
	broadcast.Muted = []store.MutedSpan{{Offset: 12 * time.Minute, Duration: 90 * time.Second}}
	downloader := &patchDownloader{}
	measurer := &fakeMeasurer{duration: 15 * time.Minute}

	patcher := NewPatcher(downloader, patches, func(context.Context, int64) error { return nil },
		measurer, PatchOptions{
			LibraryRoot: t.TempDir(), MaxAttempts: patchAttemptLimit,
			OriginalAudio: func(context.Context, string, []store.MutedSpan) (string, bool, error) {
				return original, true, nil
			},
		}, nil)

	filled, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if filled != 1 {
		t.Fatalf("Patch filled %d gaps, want 1", filled)
	}
	if len(downloader.requests) != 1 {
		t.Fatalf("downloads = %d, want 1", len(downloader.requests))
	}
	// The whole point: the range is fetched from the copy holding the audio,
	// not from the one playback serves.
	if got := downloader.requests[0].URL; got != original {
		t.Errorf("fetched from %q, want the copy holding the original audio %q", got, original)
	}
	if len(measurer.listened) != 1 {
		t.Errorf("measured the audio of %d files, want the recovered range checked",
			len(measurer.listened))
	}
}

func TestPatch_KeepsARecoveredPatchWhoseMuteSitsPastTheDeliveredEnd(t *testing.T) {
	// A cut lands on a keyframe, so a whole patch is allowed to come back a
	// little short of the hole it was asked for. A mute near the end then
	// maps to a window past the end of the file, which passes no samples and
	// measures as silence. Reading that as a bad patch throws away a good
	// one and retires the hole with it.
	//
	// The fixture hole runs 10 to 25 minutes in. The delivered file is
	// 13m40s, inside the allowance, and the mute sits at 24m50s.
	broadcast, patches := gappedBroadcast()
	broadcast.Muted = []store.MutedSpan{{Offset: 24*time.Minute + 50*time.Second, Duration: 90 * time.Second}}
	downloader := &patchDownloader{}
	measurer := &fakeMeasurer{duration: 13*time.Minute + 40*time.Second, silent: true}

	patcher := NewPatcher(downloader, patches, func(context.Context, int64) error { return nil },
		measurer, PatchOptions{
			LibraryRoot: t.TempDir(), MaxAttempts: patchAttemptLimit,
			OriginalAudio: func(context.Context, string, []store.MutedSpan) (string, bool, error) {
				return "https://cdn.example/vod/chunked/index-dvr.m3u8", true, nil
			},
		}, nil)

	filled, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if filled != 1 {
		t.Errorf("Patch filled %d gaps, want the patch kept", filled)
	}
	// Nothing to measure: the window sits past what the file holds, and
	// measuring it anyway is what produced the false silence.
	if len(measurer.listened) != 0 {
		t.Errorf("measured %v, want no window past the delivered end", measurer.listened)
	}
}

func TestPatch_ASilentRecoveredRangeStaysRetryable(t *testing.T) {
	// Every segment answered before the download, so a silent result is
	// about one assembled file rather than about the copy. A partial
	// response or a stopped decode produces it once, and charging it
	// terminal would retire a hole the platform would have served.
	broadcast, patches := gappedBroadcast()
	broadcast.Muted = []store.MutedSpan{{Offset: 12 * time.Minute, Duration: 90 * time.Second}}

	patcher := NewPatcher(&patchDownloader{}, patches, func(context.Context, int64) error { return nil },
		&fakeMeasurer{duration: 15 * time.Minute, silent: true}, PatchOptions{
			LibraryRoot: t.TempDir(), MaxAttempts: patchAttemptLimit,
			OriginalAudio: func(context.Context, string, []store.MutedSpan) (string, bool, error) {
				return "https://cdn.example/vod/chunked/index-dvr.m3u8", true, nil
			},
		}, nil)

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}

	for _, gaps := range patches.gaps {
		for _, gap := range gaps {
			if gap.Attempts >= patchAttemptLimit {
				t.Errorf("gap %d is at %d attempts of %d after one silent reading, want it retryable",
					gap.ID, gap.Attempts, patchAttemptLimit)
			}
		}
	}
}

func TestPatch_ALookupThatCanNeverSucceedRetiresTheHole(t *testing.T) {
	// A deleted copy answers permanently. Reading that as "ask later" skips
	// the hole on every pass for good and spends a subprocess each time to
	// be told the same thing, which is the loop the attempt cap exists to
	// end.
	broadcast, patches := gappedBroadcast()
	broadcast.Muted = []store.MutedSpan{{Offset: 12 * time.Minute, Duration: 90 * time.Second}}
	downloader := &patchDownloader{}

	patcher := NewPatcher(downloader, patches, func(context.Context, int64) error { return nil },
		&fakeMeasurer{duration: 15 * time.Minute}, PatchOptions{
			LibraryRoot: t.TempDir(), MaxAttempts: patchAttemptLimit,
			OriginalAudio: func(context.Context, string, []store.MutedSpan) (string, bool, error) {
				return "", false, &fetch.ToolError{Failure: fetch.FailurePermanent}
			},
		}, nil)

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if len(downloader.requests) != 0 {
		t.Errorf("downloaded %d times, want none for a copy that is gone", len(downloader.requests))
	}

	retired := false
	for _, gaps := range patches.gaps {
		for _, gap := range gaps {
			if gap.Attempts >= patchAttemptLimit {
				retired = true
			}
		}
	}
	if !retired {
		t.Error("the hole is still retryable after a permanent answer, want it retired")
	}
}

func TestPatch_RemovesARejectedPatchSoTheRetryFetchesAgain(t *testing.T) {
	// The tool passes --no-overwrites, so a rejected file left where it is
	// makes the next pass re-measure the same bytes and reach the same
	// verdict, spending every attempt without ever asking the platform
	// again. The bytes also sit outside the size cap, because nothing claims
	// them.
	broadcast, patches := gappedBroadcast()
	downloader := &patchDownloader{}
	// Short of the hole by more than the allowance, so the range is refused.
	patcher, root := newPatcher(t, downloader, patches, &fakeMeasurer{duration: time.Minute})

	incoming := paths.IncomingDir(root)
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		t.Fatalf("creating the incoming directory: %v", err)
	}

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}

	left, err := os.ReadDir(incoming)
	if err != nil {
		t.Fatalf("reading the incoming directory: %v", err)
	}
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, entry := range left {
			names = append(names, entry.Name())
		}
		t.Errorf("incoming holds %v, want the rejected patch removed", names)
	}
}

func TestPatch_KeepsADownloadWhoseCheckCouldNotRun(t *testing.T) {
	// A probe that could not run says nothing about the bytes. Deleting them
	// over it spends the download again on the next pass and learns no more
	// the second time.
	broadcast, patches := gappedBroadcast()
	measurer := &fakeMeasurer{err: errors.New("ffprobe is not installed")}
	patcher, root := newPatcher(t, &patchDownloader{}, patches, measurer)

	incoming := paths.IncomingDir(root)
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		t.Fatalf("creating the incoming directory: %v", err)
	}

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}

	left, err := os.ReadDir(incoming)
	if err != nil {
		t.Fatalf("reading the incoming directory: %v", err)
	}
	if len(left) != 1 {
		t.Errorf("incoming holds %d files, want the unmeasured download kept", len(left))
	}
}

func TestPatch_LooksUpTheOriginalOncePerBroadcast(t *testing.T) {
	// The expensive half is resolving the copy's address and reading its
	// playlist, and every hole in one broadcast shares both answers. Asking
	// per hole spends a subprocess and a playlist read each time.
	broadcast, patches := gappedBroadcast()
	broadcast.Muted = []store.MutedSpan{
		{Offset: 12 * time.Minute, Duration: 90 * time.Second},
		{Offset: 40 * time.Minute, Duration: 90 * time.Second},
	}
	// A second hole, over the second silenced stretch.
	second := patchStart.Add(38 * time.Minute)
	third := patchStart.Add(45 * time.Minute)
	patches.recordings = append(patches.recordings, store.Recording{
		ID: 3, State: store.StateComplete, Origin: store.OriginLive,
		StartedAt: second, EndedAt: &third,
	})

	lookups := 0
	patcher := NewPatcher(&patchDownloader{}, patches, func(context.Context, int64) error { return nil },
		&fakeMeasurer{duration: 15 * time.Minute}, PatchOptions{
			LibraryRoot: t.TempDir(), MaxAttempts: patchAttemptLimit,
			OriginalAudio: func(context.Context, string, []store.MutedSpan) (string, bool, error) {
				lookups++
				return "https://cdn.example/vod/chunked/index-dvr.m3u8", true, nil
			},
		}, nil)

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if lookups != 1 {
		t.Errorf("looked up the original audio %d times for one broadcast, want 1", lookups)
	}
}

func TestPatch_RefusesARecoveredRangeThatCameBackSilent(t *testing.T) {
	// The trap this guard exists for. A source that refuses the parts
	// carrying the audio still yields a file of the right length, because
	// the demuxer fills from whatever it could reach. The length passes, and
	// FillGap would make it permanent, so the audio is what decides.
	broadcast, patches := gappedBroadcast()
	broadcast.Muted = []store.MutedSpan{{Offset: 12 * time.Minute, Duration: 90 * time.Second}}
	downloader := &patchDownloader{}
	measurer := &fakeMeasurer{duration: 15 * time.Minute, silent: true}

	patcher := NewPatcher(downloader, patches, func(context.Context, int64) error { return nil },
		measurer, PatchOptions{
			LibraryRoot: t.TempDir(), MaxAttempts: patchAttemptLimit,
			OriginalAudio: func(context.Context, string, []store.MutedSpan) (string, bool, error) {
				return "https://cdn.example/vod/chunked/index-dvr.m3u8", true, nil
			},
		}, nil)

	filled, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if filled != 0 {
		t.Errorf("Patch filled %d gaps, want 0 for a range that came back silent", filled)
	}
}

func TestPatch_DoesNotMeasureTheAudioOfAnOrdinaryPatch(t *testing.T) {
	// A broadcast may hold silence legitimately, and an ordinary patch was
	// never fetched from a source that could refuse part of it. Measuring
	// one would refuse a correct patch of a quiet stretch.
	broadcast, patches := gappedBroadcast()
	downloader := &patchDownloader{}
	measurer := &fakeMeasurer{duration: 15 * time.Minute, silent: true}
	patcher, _ := newPatcher(t, downloader, patches, measurer)

	filled, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if filled != 1 {
		t.Errorf("Patch filled %d gaps, want 1", filled)
	}
	if len(measurer.listened) != 0 {
		t.Errorf("measured the audio of %v, want an ordinary patch left alone", measurer.listened)
	}
}

func TestPatch_RefusesAMutedGapWhenTheLookupFails(t *testing.T) {
	// A lookup that cannot answer is not an answer of yes. Treating a
	// failure as permission would patch from the copy playback serves, which
	// is the silence this refuses in the first place.
	broadcast, patches := gappedBroadcast()
	broadcast.Muted = []store.MutedSpan{{Offset: 12 * time.Minute, Duration: 90 * time.Second}}
	downloader := &patchDownloader{}

	patcher := NewPatcher(downloader, patches, func(context.Context, int64) error { return nil },
		&fakeMeasurer{duration: 15 * time.Minute}, PatchOptions{
			LibraryRoot: t.TempDir(), MaxAttempts: patchAttemptLimit,
			OriginalAudio: func(context.Context, string, []store.MutedSpan) (string, bool, error) {
				return "", false, errors.New("the platform did not answer")
			},
		}, nil)

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if len(downloader.requests) != 0 {
		t.Errorf("downloaded %d times, want none when the lookup failed", len(downloader.requests))
	}
	// The damage a swallowed failure does is here rather than in the
	// download. A terminal charge slams attempts to the cap and retires the
	// gap for good, so one bad minute on the platform's side would cost
	// every silenced hole in the pass, permanently.
	for _, gaps := range patches.gaps {
		for _, gap := range gaps {
			if gap.Attempts >= patchAttemptLimit {
				t.Errorf("gap %d is at %d attempts of %d after a lookup failure, want it retryable",
					gap.ID, gap.Attempts, patchAttemptLimit)
			}
		}
	}
}

func TestPatch_RefusesAGapThePlatformMuted(t *testing.T) {
	// The platform replaces the audio of a stretch it judges to hold
	// copyrighted music. Playback then serves silence, so a hole filled from
	// a muted stretch comes back with nothing worth having, and FillGap is
	// permanent, so it would read as patched forever. This is the machine
	// with no route to the original, which is the default.
	//
	// The fixture hole runs from ten to twenty-five minutes in, and the
	// stored copy's timeline agrees with the broadcast's.
	tests := []struct {
		name         string
		muted        []store.MutedSpan
		wantDownload bool
	}{
		{
			name:         "a mute inside the hole",
			muted:        []store.MutedSpan{{Offset: 12 * time.Minute, Duration: 90 * time.Second}},
			wantDownload: false,
		},
		{
			name:         "a mute overlapping the hole's edge",
			muted:        []store.MutedSpan{{Offset: 5 * time.Minute, Duration: 8 * time.Minute}},
			wantDownload: false,
		},
		{
			name:         "a mute elsewhere in the broadcast",
			muted:        []store.MutedSpan{{Offset: 40 * time.Minute, Duration: 2 * time.Minute}},
			wantDownload: true,
		},
		{
			name:         "a mute ending exactly where the hole opens",
			muted:        []store.MutedSpan{{Offset: 8 * time.Minute, Duration: 2 * time.Minute}},
			wantDownload: true,
		},
		{
			name:         "the platform muted nothing",
			muted:        []store.MutedSpan{},
			wantDownload: true,
		},
		{
			name:         "nobody asked the platform",
			muted:        nil,
			wantDownload: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broadcast, patches := gappedBroadcast()
			broadcast.Muted = tt.muted
			downloader := &patchDownloader{}
			patcher, _ := newPatcher(t, downloader, patches)

			filled, err := patcher.Patch(context.Background(), broadcast,
				passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))
			if err != nil {
				t.Fatalf("Patch() err = %v, want nil", err)
			}

			if gotDownload := len(downloader.requests) > 0; gotDownload != tt.wantDownload {
				t.Errorf("downloaded = %t, want %t", gotDownload, tt.wantDownload)
			}
			if tt.wantDownload {
				if filled != 1 {
					t.Errorf("Patch filled %d gaps, want 1", filled)
				}
				return
			}

			if filled != 0 {
				t.Errorf("Patch filled %d gaps, want none", filled)
			}
			if len(patches.filled) != 0 {
				t.Errorf("Patch marked %v filled, want none", patches.filled)
			}
			// Charged terminal: no later pass can make the platform serve the
			// audio it replaced, so retrying spends a whole-range download
			// every pass for the life of the library.
			for _, gaps := range patches.gaps {
				for _, gap := range gaps {
					if gap.Attempts < patchAttemptLimit {
						t.Errorf("gap %d has %d attempts left, want it charged terminal", gap.ID, gap.Attempts)
					}
				}
			}
		})
	}
}

func TestPatch_DoesNotFillAGapTheDownloadCameUpShortOn(t *testing.T) {
	// FillGap is permanent. A range that came back short is a range indexed
	// somewhere other than the hole, and marking the hole filled behind it
	// means nothing ever looks at it again.
	tests := []struct {
		name       string
		measured   time.Duration
		measureErr error
		wantFilled bool
	}{
		{name: "the whole range arrived", measured: 15 * time.Minute, wantFilled: true},
		{name: "keyframe alignment trimmed a little", measured: 14*time.Minute + 30*time.Second, wantFilled: true},
		{name: "the range came back nearly empty", measured: 4 * time.Second, wantFilled: false},
		{name: "half the range arrived", measured: 7 * time.Minute, wantFilled: false},
		{name: "the length could not be read", measureErr: errors.New("ffprobe is not installed"), wantFilled: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broadcast, patches := gappedBroadcast()
			downloader := &patchDownloader{}
			patcher, _ := newPatcher(t, downloader, patches,
				&fakeMeasurer{duration: tt.measured, err: tt.measureErr})

			filled, err := patcher.Patch(context.Background(), broadcast,
				passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))
			if err != nil {
				t.Fatalf("Patch() err = %v, want nil", err)
			}

			if gotFilled := filled > 0; gotFilled != tt.wantFilled {
				t.Errorf("Patch filled %d gaps, want filled: %t", filled, tt.wantFilled)
			}
			if gotMarked := len(patches.filled) > 0; gotMarked != tt.wantFilled {
				t.Errorf("Patch marked %v filled, want marked: %t", patches.filled, tt.wantFilled)
			}
		})
	}
}

// ///////////////////////////////////////////////
// What a patch refuses to do twice
// ///////////////////////////////////////////////

func TestPatch_LeavesAGapThatIsAlreadyFilled(t *testing.T) {
	// A pass runs on a timer. Re-downloading every filled hole on every tick
	// would spend a request and the bandwidth per gap forever.
	broadcast, patches := gappedBroadcast()
	downloader := &patchDownloader{}
	patcher, _ := newPatcher(t, downloader, patches)
	when := patchStart.Add(48 * time.Hour)

	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], when); err != nil {
		t.Fatalf("first Patch() err = %v, want nil", err)
	}
	if _, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], when); err != nil {
		t.Fatalf("second Patch() err = %v, want nil", err)
	}

	if len(downloader.requests) != 1 {
		t.Errorf("a second pass downloaded again: %d requests, want 1", len(downloader.requests))
	}
}

func TestPatch_MarksTheGapOnlyAfterTheFileIsInTheLibrary(t *testing.T) {
	// A gap marked filled with nothing behind it is never looked at again,
	// so the hole becomes permanent and invisible at once.
	broadcast, patches := gappedBroadcast()
	patches.createErr = errors.New("the database refused the row")
	patcher, _ := newPatcher(t, &patchDownloader{}, patches)

	filled, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if filled != 0 {
		t.Errorf("Patch reported %d filled, want 0", filled)
	}
	if len(patches.filled) != 0 {
		t.Errorf("Patch marked gaps %v filled with no recording written", patches.filled)
	}
}

func TestPatch_KeepsGoingWhenOneHoleWillNotDownload(t *testing.T) {
	// A pass fills what it can. A hole the platform will not serve must not
	// cost the ones after it, and it is retried on the next pass anyway.
	broadcast, patches := gappedBroadcast()
	downloader := &patchDownloader{err: errors.New("yt-dlp exited 1")}
	patcher, _ := newPatcher(t, downloader, patches)

	filled, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))
	if err != nil {
		t.Errorf("Patch() err = %v, want nil for a download that failed", err)
	}
	if filled != 0 {
		t.Errorf("Patch reported %d filled, want 0", filled)
	}
	if len(patches.filled) != 0 {
		t.Errorf("Patch marked %v filled after a failed download", patches.filled)
	}
}

func TestPatch_HasNothingToDoForABroadcastNothingCaptured(t *testing.T) {
	// That is a fetch, not a patch. There is no recording to attach a hole
	// to, and the whole broadcast is what is missing.
	ended := patchStart.Add(time.Hour)
	broadcast := store.Broadcast{
		ID: 1, ChannelID: 1, RemoteID: "v100001",
		URL:          "https://example.com/videos/v100001",
		StartedAt:    patchStart,
		VodStartedAt: &patchStart,
		EndedAt:      &ended,
	}
	downloader := &patchDownloader{}
	patcher, _ := newPatcher(t, downloader, &fakePatchStore{})

	filled, err := patcher.Patch(context.Background(), broadcast,
		passChannels("examplechannel")[0], patchStart.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("Patch() err = %v, want nil", err)
	}
	if filled != 0 || len(downloader.requests) != 0 {
		t.Errorf("Patch filled %d and downloaded %d, want 0 and 0", filled, len(downloader.requests))
	}
}

// TestPatch_StopsRetryingAHoleThatWillNotFill covers the cost of a gap
// nothing can patch.
//
// The patcher takes no claim, so the attempt count on the gap row is the
// only thing that remembers a failure. Without it an unfillable hole costs
// one yt-dlp invocation and one range download on every pass, forever. A
// platform deleting a broadcast is the ordinary way to reach that.
func TestPatch_StopsRetryingAHoleThatWillNotFill(t *testing.T) {
	const extraPasses = 3

	t.Run("an ordinary failure stops at the cap", func(t *testing.T) {
		broadcast, patches := gappedBroadcast()
		downloader := &patchDownloader{err: errors.New("connection reset by peer")}
		patcher, _ := newPatcher(t, downloader, patches)

		for pass := 1; pass <= patchAttemptLimit+extraPasses; pass++ {
			if _, err := patcher.Patch(context.Background(), broadcast,
				passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
				t.Fatalf("pass %d: Patch() err = %v, want nil", pass, err)
			}
		}

		if got := len(downloader.requests); got != patchAttemptLimit {
			t.Errorf("downloads = %d over %d passes, want it to stop at the cap of %d",
				got, patchAttemptLimit+extraPasses, patchAttemptLimit)
		}
	})

	t.Run("a terminal failure spends no further attempt", func(t *testing.T) {
		// A video the platform removed cannot become available on a timer,
		// so the attempts left over buy nothing and cost a range download
		// each.
		broadcast, patches := gappedBroadcast()
		downloader := &patchDownloader{err: &fetch.ToolError{
			Failure: fetch.FailurePermanent,
			Excerpt: "video unavailable",
		}}
		patcher, _ := newPatcher(t, downloader, patches)

		for pass := 1; pass <= patchAttemptLimit+extraPasses; pass++ {
			if _, err := patcher.Patch(context.Background(), broadcast,
				passChannels("examplechannel")[0], patchStart.Add(48*time.Hour)); err != nil {
				t.Fatalf("pass %d: Patch() err = %v, want nil", pass, err)
			}
		}

		if got := len(downloader.requests); got != 1 {
			t.Errorf("downloads = %d, want 1: a permanent failure ends the retries at once", got)
		}
	})
}
