package importer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/naming"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Fakes
// ///////////////////////////////////////////////

// fakeCatalog records what an import wrote, without a database.
type fakeCatalog struct {
	mu sync.Mutex

	channels   []store.Channel
	paths      []string
	recordings []store.Recording
	gaps       []store.Gap
	media      map[int64]time.Duration
	muted      map[int64]time.Duration
	filled     []int64

	nextID     int64
	pathsErr   error
	mediaErr   error
	broadcasts map[string]store.Broadcast
	// titles records every title observation restored onto a broadcast.
	titles []store.TitleObservation
	// upsertErr fails a broadcast restore, so a test can watch an import
	// carry on with the recording unattached.
	upsertErr error
}

// fakeProber answers with a fixed length, so no subprocess runs.
type fakeProber struct {
	length time.Duration
	err    error
}

// theRecording is the one path every case files under, rendered by the
// default template.
const theRecording = "atrioc/2026/atrioc - 2026-08-15 18-34 - movie night.mkv"

func newCatalog(channels ...store.Channel) *fakeCatalog {
	return &fakeCatalog{
		channels: channels,
		media:    map[int64]time.Duration{},
		muted:    map[int64]time.Duration{},
	}
}

func (c *fakeCatalog) RecordingPaths() ([]string, error) {
	if c.pathsErr != nil {
		return nil, c.pathsErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...), nil
}

func (c *fakeCatalog) Channels() ([]store.Channel, error) {
	return append([]store.Channel(nil), c.channels...), nil
}

func (c *fakeCatalog) UpsertChannel(platform, name, displayName string) (store.Channel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, channel := range c.channels {
		if channel.Platform == platform && strings.EqualFold(channel.Name, name) {
			return channel, nil
		}
	}
	c.nextID++
	channel := store.Channel{ID: c.nextID, Platform: platform, Name: name, DisplayName: displayName}
	c.channels = append(c.channels, channel)
	return channel, nil
}

func (c *fakeCatalog) BroadcastByRemoteID(channelID int64, remoteID string) (store.Broadcast, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if found, ok := c.broadcasts[remoteID]; ok && found.ChannelID == channelID {
		return found, nil
	}
	return store.Broadcast{}, store.ErrNotFound
}

func (c *fakeCatalog) UpsertBroadcast(b store.Broadcast) (store.Broadcast, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.upsertErr != nil {
		return store.Broadcast{}, c.upsertErr
	}
	if c.broadcasts == nil {
		c.broadcasts = map[string]store.Broadcast{}
	}
	// Scoped to the channel, as the store is: an archive identifier is
	// unique inside one channel and says nothing across two.
	if found, ok := c.broadcasts[b.RemoteID]; ok && found.ChannelID == b.ChannelID {
		return found, nil
	}
	c.nextID++
	b.ID = c.nextID
	c.broadcasts[b.RemoteID] = b
	return b, nil
}

func (c *fakeCatalog) BroadcastsBetween(channelID int64, from, to time.Time) ([]store.Broadcast, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var near []store.Broadcast
	for _, b := range c.broadcasts {
		if b.ChannelID != channelID {
			continue
		}
		if b.StartedAt.Before(from) || b.StartedAt.After(to) {
			continue
		}
		near = append(near, b)
	}
	return near, nil
}

func (c *fakeCatalog) SetBroadcastRecording(id, broadcastID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.recordings {
		if c.recordings[i].ID == id {
			c.recordings[i].BroadcastID = &broadcastID
			return nil
		}
	}
	return store.ErrNotFound
}

func (c *fakeCatalog) ObserveTitle(broadcastID int64, at time.Time, title, category string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.titles = append(c.titles, store.TitleObservation{
		ObservedAt: at, Title: title, Category: category,
	})
	_ = broadcastID
	return nil
}

func (c *fakeCatalog) CreateRecording(r store.Recording) (store.Recording, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// The real store holds UNIQUE(path), and an import leans on it: two runs
	// over one directory must not make two rows for one file.
	for _, existing := range c.recordings {
		if existing.Path == r.Path {
			return store.Recording{}, store.ErrDuplicatePath
		}
	}
	if !r.State.Valid() || !r.Origin.Valid() {
		return store.Recording{}, errors.New("invalid recording")
	}
	c.nextID++
	r.ID = c.nextID
	c.recordings = append(c.recordings, r)
	c.paths = append(c.paths, r.Path)
	return r, nil
}

func (c *fakeCatalog) SetMediaDuration(id int64, duration time.Duration) error {
	if c.mediaErr != nil {
		return c.mediaErr
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.media[id] = duration
	return nil
}

func (c *fakeCatalog) SetMutedDuration(id int64, muted time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.muted[id] = muted
	return nil
}

func (c *fakeCatalog) AddGap(recordingID int64, start, end time.Duration, reason string) (store.Gap, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	gap := store.Gap{ID: c.nextID, RecordingID: recordingID, Start: start, End: end, Reason: reason}
	c.gaps = append(c.gaps, gap)
	return gap, nil
}

func (c *fakeCatalog) FillGap(id int64, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filled = append(c.filled, id)
	return nil
}

func (c *fakeCatalog) recordingFor(path string) (store.Recording, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, recording := range c.recordings {
		if recording.Path == path {
			return recording, true
		}
	}
	return store.Recording{}, false
}

func (p fakeProber) Duration(context.Context, string) (time.Duration, error) {
	return p.length, p.err
}

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// libraryAt builds a library rooted in a temporary directory.
func libraryAt(t *testing.T) *library.Library {
	t.Helper()

	lib, err := library.Create(t.TempDir(), "test")
	if err != nil {
		t.Fatalf("library.Create() err = %v, want nil", err)
	}
	return lib
}

// writeMedia puts a file of a given size in the library.
func writeMedia(t *testing.T, lib *library.Library, relPath string, size int) {
	t.Helper()

	full := lib.RelPath(relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll() err = %v, want nil", err)
	}
	if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) err = %v, want nil", relPath, err)
	}
}

// writeSidecar puts a record beside a media file.
func writeSidecar(t *testing.T, lib *library.Library, relPath string, sidecar organize.Sidecar) {
	t.Helper()

	body, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatalf("Marshal() err = %v, want nil", err)
	}
	if err := os.WriteFile(lib.RelPath(organize.SidecarPath(relPath)), body, 0o644); err != nil {
		t.Fatalf("WriteFile(sidecar) err = %v, want nil", err)
	}
}

// writeRaw puts arbitrary bytes at a sidecar's path.
func writeRaw(t *testing.T, lib *library.Library, relPath, body string) {
	t.Helper()

	if err := os.WriteFile(lib.RelPath(organize.SidecarPath(relPath)), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(sidecar) err = %v, want nil", err)
	}
}

// importerFor wires an importer over a library with the default template.
func importerFor(t *testing.T, lib *library.Library, catalog Catalog, prober Prober, options Options) *Importer {
	t.Helper()

	template, err := naming.Parse(naming.DefaultTemplate)
	if err != nil {
		t.Fatalf("Parse() err = %v, want nil", err)
	}
	return New(lib, catalog, prober, template, time.UTC, options)
}

// atrioc is a channel this machine knows.
func atrioc() store.Channel {
	return store.Channel{ID: 7, Platform: "twitch", Name: "atrioc", DisplayName: "Atrioc"}
}

// fullSidecar is a record with everything a finished recording carries.
func fullSidecar() organize.Sidecar {
	return organize.Sidecar{
		SchemaVersion:   organize.SidecarVersion,
		Platform:        "twitch",
		Channel:         "atrioc",
		Author:          "Atrioc",
		Title:           "movie night",
		StartedAt:       time.Date(2026, 8, 15, 18, 34, 0, 0, time.UTC),
		DurationMS:      (2 * time.Hour).Milliseconds(),
		MediaDurationMS: (2 * time.Hour).Milliseconds(),
		Bytes:           1024,
		Origin:          string(store.OriginRecovered),
	}
}

// only returns the single file a run considered.
func only(t *testing.T, report Report) File {
	t.Helper()

	if len(report.Files) != 1 {
		t.Fatalf("run considered %d files, want 1: %+v", len(report.Files), report.Files)
	}
	return report.Files[0]
}

// run executes an import and fails the test on an error.
func run(t *testing.T, importer *Importer) Report {
	t.Helper()

	report, err := importer.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}
	return report
}

// ///////////////////////////////////////////////
// Tier 1: the sidecar
// ///////////////////////////////////////////////

func TestRun_RestoresARecordingFromItsSidecar(t *testing.T) {
	// The sidecar exists to be read back. It carries what the recorder
	// observed, so nothing about the row it produces is a guess.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	writeSidecar(t, lib, theRecording, fullSidecar())

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Restored {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Restored, file.Reason)
	}

	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("no recording was created")
	}
	// The sidecar's own origin comes back. A restore that stamped every row
	// imported would lose the distinction between a live capture and an
	// archive copy, which is what tells an operator whether the audio can
	// be muted.
	if recording.Origin != store.OriginRecovered {
		t.Errorf("origin = %q, want %q", recording.Origin, store.OriginRecovered)
	}
	if recording.State != store.StateComplete {
		t.Errorf("state = %q, want %q", recording.State, store.StateComplete)
	}
	if !recording.StartedAt.Equal(fullSidecar().StartedAt) {
		t.Errorf("started = %s, want %s", recording.StartedAt, fullSidecar().StartedAt)
	}
	if len(file.Disagreements) != 0 {
		t.Errorf("disagreements = %v, want none", file.Disagreements)
	}
}

func TestRun_MeasuresTheFileRatherThanBelievingItsSidecar(t *testing.T) {
	// A sidecar in the operator's own library states zero bytes for a
	// recording of ten gigabytes, because an older build stamped the row
	// before measuring the file. A row copied from that claim puts the size
	// cap ten gigabytes out and the library never notices it is full.
	tests := []struct {
		name         string
		claimedBytes int64
		claimedMedia int64
		wantReported bool
		why          string
	}{
		{
			name:         "a sidecar that never measured anything",
			claimedBytes: 0,
			claimedMedia: 0,
			wantReported: false,
			why:          "zero is how this project spells nobody measured it, not a contradiction",
		},
		{
			name:         "a sidecar whose size disagrees",
			claimedBytes: 999,
			claimedMedia: (2 * time.Hour).Milliseconds(),
			wantReported: true,
			why:          "the file is the authority on its own size",
		},
		{
			name:         "a sidecar whose length disagrees",
			claimedBytes: 4096,
			claimedMedia: (9 * time.Hour).Milliseconds(),
			wantReported: true,
			why:          "the media is the authority on its own length",
		},
		{
			name:         "a sidecar a second out",
			claimedBytes: 4096,
			claimedMedia: (2*time.Hour + 900*time.Millisecond).Milliseconds(),
			wantReported: false,
			why:          "a remux moves container timing, and a second is the same recording",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lib := libraryAt(t)
			writeMedia(t, lib, theRecording, 4096)

			sidecar := fullSidecar()
			sidecar.Bytes = tt.claimedBytes
			sidecar.MediaDurationMS = tt.claimedMedia
			writeSidecar(t, lib, theRecording, sidecar)

			catalog := newCatalog(atrioc())
			report := run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

			file := only(t, report)
			if got := len(file.Disagreements) > 0; got != tt.wantReported {
				t.Errorf("disagreements = %v, want reported = %t, because %s",
					file.Disagreements, tt.wantReported, tt.why)
			}

			recording, ok := catalog.recordingFor(theRecording)
			if !ok {
				t.Fatal("no recording was created")
			}
			// Whatever the sidecar claimed, the stored size is the file's.
			if recording.Bytes != 4096 {
				t.Errorf("bytes = %d, want the measured 4096", recording.Bytes)
			}
			if got := catalog.media[recording.ID]; got != 2*time.Hour {
				t.Errorf("media duration = %s, want the measured 2h", got)
			}
		})
	}
}

func TestRun_RestoresWhatDoesNotFitOnTheRow(t *testing.T) {
	// Muted stretches and holes are part of the record. Dropped, a patched
	// hole reads as open and gets patched again, and a muted copy reads as
	// clean audio.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	muted := (12 * time.Minute).Milliseconds()
	sidecar.MutedMS = &muted
	sidecar.Gaps = []organize.SidecarGap{
		{StartMS: 0, EndMS: 60_000, Reason: "reconnect", Filled: false},
		{StartMS: 120_000, EndMS: 180_000, Reason: "ads", Filled: true},
	}
	writeSidecar(t, lib, theRecording, sidecar)

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("no recording was created")
	}
	if got := catalog.muted[recording.ID]; got != 12*time.Minute {
		t.Errorf("muted = %s, want 12m", got)
	}
	if len(catalog.gaps) != 2 {
		t.Fatalf("restored %d gaps, want 2", len(catalog.gaps))
	}
	if len(catalog.filled) != 1 {
		t.Errorf("marked %d gaps filled, want the 1 the sidecar carried", len(catalog.filled))
	}
	if only(t, report).Disposition != Restored {
		t.Errorf("disposition = %q, want %q", only(t, report).Disposition, Restored)
	}
}

func TestRun_SaysWhatItCouldNotRestore(t *testing.T) {
	// Title history hangs off a broadcast, and an import creates none. Said
	// out loud rather than dropped: an operator who sees a restored
	// recording is entitled to know the record was not restored whole.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	sidecar.TitleHistory = []organize.SidecarTitle{
		{ObservedAt: sidecar.StartedAt, Title: "movie night"},
		{ObservedAt: sidecar.StartedAt.Add(time.Hour), Title: "still going"},
	}
	writeSidecar(t, lib, theRecording, sidecar)

	report := run(t, importerFor(t, lib, newCatalog(atrioc()), fakeProber{length: time.Hour}, Options{}))

	file := only(t, report)
	if !strings.Contains(file.Reason, "title observations") {
		t.Errorf("reason = %q, want it to name the observations it did not restore", file.Reason)
	}
}

func TestRun_RefusesASidecarFromANewerBuild(t *testing.T) {
	// A newer build recorded fields this one cannot see. Falling back to the
	// filename would replace a complete record with a reading of it, and the
	// operator would never learn the record was there.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	sidecar.SchemaVersion = organize.SidecarVersion + 1
	writeSidecar(t, lib, theRecording, sidecar)

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Refused {
		t.Fatalf("disposition = %q, want %q", file.Disposition, Refused)
	}
	if !strings.Contains(file.Reason, "newer build") {
		t.Errorf("reason = %q, want it to name the version", file.Reason)
	}
	if len(catalog.recordings) != 0 {
		t.Error("a row was created for a record this build cannot read")
	}
}

func TestRun_FallsBackToTheNameWhenASidecarWillNotParse(t *testing.T) {
	// Malformed JSON says nothing about the media beside it. The name is
	// still there to read.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	writeRaw(t, lib, theRecording, "{ this is not json")

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Inferred {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Inferred, file.Reason)
	}
	// The operator has to be able to tell this from a file that never had a
	// sidecar, because this one had a record and it is unreadable.
	if !strings.Contains(file.Reason, "sidecar") {
		t.Errorf("reason = %q, want it to say the sidecar was the problem", file.Reason)
	}
}

// ///////////////////////////////////////////////
// Tier 3: the filename
// ///////////////////////////////////////////////

func TestRun_ReadsARecordingBackFromItsName(t *testing.T) {
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 2048)

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: 90 * time.Minute}, Options{}))

	if file := only(t, report); file.Disposition != Inferred {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Inferred, file.Reason)
	}

	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("no recording was created")
	}
	// The origin is the whole point. Everything downstream reads it to tell
	// a record from a reading of a filename, and coverage paints the day on
	// it.
	if recording.Origin != store.OriginImported {
		t.Errorf("origin = %q, want %q", recording.Origin, store.OriginImported)
	}
	if recording.ChannelID != atrioc().ID {
		t.Errorf("channel = %d, want the known channel %d", recording.ChannelID, atrioc().ID)
	}
	if recording.Bytes != 2048 {
		t.Errorf("bytes = %d, want the measured 2048", recording.Bytes)
	}
	want := time.Date(2026, 8, 15, 18, 34, 0, 0, time.UTC)
	if !recording.StartedAt.Equal(want) {
		t.Errorf("started = %s, want %s", recording.StartedAt, want)
	}
}

func TestRun_RefusesANameNamingAChannelNobodyConfigured(t *testing.T) {
	// The default template writes the author, which is a display name and
	// may be no login at all. Creating a channel from it puts a login
	// nobody registered into the calendar and into coverage.
	lib := libraryAt(t)
	writeMedia(t, lib, "stranger/2026/stranger - 2026-08-15 18-34 - movie night.mkv", 2048)

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Refused {
		t.Fatalf("disposition = %q, want %q", file.Disposition, Refused)
	}
	if !strings.Contains(file.Reason, "stranger") {
		t.Errorf("reason = %q, want it to name the channel it could not place", file.Reason)
	}
	if len(catalog.channels) != 1 {
		t.Errorf("channels = %d, want the one that was already known", len(catalog.channels))
	}
}

func TestRun_CountsAConfiguredChannelAsKnown(t *testing.T) {
	// This is the case the whole package exists for: the database is gone
	// and the config is not. Matching only against channel rows would refuse
	// every file in the library precisely when there is nothing else left.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 2048)

	catalog := newCatalog()
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{
		Configured: []config.Channel{{Platform: "twitch", Name: "atrioc", Enabled: true}},
	}))

	file := only(t, report)
	if file.Disposition != Inferred {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Inferred, file.Reason)
	}

	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("no recording was created")
	}
	// The channel had no row, so one was written. It carries the platform
	// the operator configured rather than a guess.
	if len(catalog.channels) != 1 {
		t.Fatalf("channels = %d, want the configured one written", len(catalog.channels))
	}
	if catalog.channels[0].Platform != "twitch" {
		t.Errorf("platform = %q, want %q", catalog.channels[0].Platform, "twitch")
	}
	if recording.ChannelID != catalog.channels[0].ID {
		t.Errorf("recording names channel %d, want %d", recording.ChannelID, catalog.channels[0].ID)
	}
}

func TestRun_ADryRunWritesNoChannelEither(t *testing.T) {
	// A configured channel with no row is written only once a file actually
	// matches it. A run that adopts nothing has to leave the database as it
	// found it.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 2048)

	catalog := newCatalog()
	run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{
		DryRun:     true,
		Configured: []config.Channel{{Platform: "twitch", Name: "atrioc", Enabled: true}},
	}))

	if len(catalog.channels) != 0 {
		t.Errorf("a dry run wrote %d channels, want none", len(catalog.channels))
	}
}

func TestRun_ADatabaseRowBeatsTheConfiguredChannel(t *testing.T) {
	// Both name one channel. The row carries the identifier every recording
	// already points at, so a second row for the same login would split one
	// channel's history in two.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 2048)

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{
		Configured: []config.Channel{{Platform: "twitch", Name: "atrioc", Enabled: true}},
	}))

	if file := only(t, report); file.Disposition != Inferred {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Inferred, file.Reason)
	}
	recording, _ := catalog.recordingFor(theRecording)
	if recording.ChannelID != atrioc().ID {
		t.Errorf("recording names channel %d, want the existing row %d",
			recording.ChannelID, atrioc().ID)
	}
	if len(catalog.channels) != 1 {
		t.Errorf("channels = %d, want no second row for one login", len(catalog.channels))
	}
}

func TestRun_MatchesAChannelByItsDisplayNameToo(t *testing.T) {
	// The default template renders the author, so the name on disk is
	// usually the display name rather than the login.
	lib := libraryAt(t)
	writeMedia(t, lib, "Atrioc/2026/Atrioc - 2026-08-15 18-34 - movie night.mkv", 2048)

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Inferred {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Inferred, file.Reason)
	}
	if file.RecordingID == 0 {
		t.Error("no row was created")
	}
}

func TestRun_RefusesWhatItCannotAccountFor(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		prober Prober
		want   string
	}{
		{
			name:   "a name from some other tool",
			path:   "atrioc/2026/whatever.mkv",
			prober: fakeProber{length: time.Hour},
			want:   "naming template",
		},
		{
			name:   "a file nothing can measure",
			path:   theRecording,
			prober: fakeProber{err: errors.New("not media")},
			want:   "length",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lib := libraryAt(t)
			writeMedia(t, lib, tt.path, 2048)

			catalog := newCatalog(atrioc())
			report := run(t, importerFor(t, lib, catalog, tt.prober, Options{}))

			file := only(t, report)
			if file.Disposition != Refused {
				t.Fatalf("disposition = %q, want %q", file.Disposition, Refused)
			}
			if !strings.Contains(file.Reason, tt.want) {
				t.Errorf("reason = %q, want it to mention %q", file.Reason, tt.want)
			}
			// A refusal leaves the file exactly where it is, which is what
			// makes running this against somebody's archive safe.
			if _, err := os.Stat(lib.RelPath(tt.path)); err != nil {
				t.Errorf("the refused file is gone: %v", err)
			}
			if len(catalog.recordings) != 0 {
				t.Error("a row was created for a file nothing could account for")
			}
		})
	}
}

func TestRun_NamesWhatAFilenameCouldNotCarryBack(t *testing.T) {
	// A sanitized title and a deduplication suffix are both readings the
	// operator has to be able to question.
	lib := libraryAt(t)
	writeMedia(t, lib, "atrioc/2026/atrioc - 2026-08-15 18-34 - movie_ night (2).mkv", 2048)

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Inferred {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Inferred, file.Reason)
	}
	for _, want := range []string{"title", "deduplication"} {
		if !strings.Contains(file.Reason, want) {
			t.Errorf("reason = %q, want it to mention %q", file.Reason, want)
		}
	}
}

// ///////////////////////////////////////////////
// The scan
// ///////////////////////////////////////////////

func TestRun_SkipsWhatTheLibraryAlreadyNames(t *testing.T) {
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	catalog := newCatalog(atrioc())
	catalog.paths = []string{theRecording}

	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Skipped {
		t.Fatalf("disposition = %q, want %q", file.Disposition, Skipped)
	}
	if len(catalog.recordings) != 0 {
		t.Error("a second row was created for a file already recorded")
	}
}

func TestRun_LeavesTheLibrarysOwnDirectoriesAlone(t *testing.T) {
	// The state directory holds the database and the trash, and incoming
	// holds captures that have not finished. Both have an owner.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	writeMedia(t, lib, ".dvr/trash/atrioc - 2026-08-15 18-34 - deleted.mkv", 1024)
	writeMedia(t, lib, "incoming/atrioc - 2026-08-15 18-34 - capturing.mkv", 1024)

	report := run(t, importerFor(t, lib, newCatalog(atrioc()), fakeProber{length: time.Hour}, Options{}))

	if len(report.Files) != 1 {
		t.Fatalf("considered %d files, want only the one in the library: %+v",
			len(report.Files), report.Files)
	}
	if report.Files[0].Path != theRecording {
		t.Errorf("considered %q, want %q", report.Files[0].Path, theRecording)
	}
}

func TestRun_IgnoresWhatIsNotAContainerThisProjectWrites(t *testing.T) {
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	for _, ignored := range []string{
		"atrioc/2026/notes.txt",
		"atrioc/2026/thumb.png",
		"atrioc/cover.jpg",
	} {
		writeMedia(t, lib, ignored, 16)
	}

	report := run(t, importerFor(t, lib, newCatalog(atrioc()), fakeProber{length: time.Hour}, Options{}))

	if len(report.Files) != 1 {
		t.Errorf("considered %d files, want only the media: %+v", len(report.Files), report.Files)
	}
}

func TestRun_ConsidersEveryContainerTheProjectCanWrite(t *testing.T) {
	// The container is configurable, so a library holds whichever one was
	// set when each recording finished. Walking past the others would leave
	// them invisible forever.
	lib := libraryAt(t)
	for _, ext := range []string{"mkv", "mp4", "ts"} {
		writeMedia(t, lib, "atrioc/2026/atrioc - 2026-08-15 18-34 - movie night."+ext, 1024)
	}

	report := run(t, importerFor(t, lib, newCatalog(atrioc()), fakeProber{length: time.Hour}, Options{}))

	if report.Imported() != 3 {
		t.Errorf("imported %d, want all 3 containers: %+v", report.Imported(), report.Files)
	}
}

// ///////////////////////////////////////////////
// Safety
// ///////////////////////////////////////////////

func TestRun_NeverMovesOrRewritesMedia(t *testing.T) {
	// An import records where a file already is. Adopting and reorganizing
	// in one step is how an archive gets lost.
	lib := libraryAt(t)
	misfiled := "somewhere else/atrioc - 2026-08-15 18-34 - movie night.mkv"
	writeMedia(t, lib, misfiled, 1024)
	writeSidecar(t, lib, misfiled, fullSidecar())

	before, err := os.Stat(lib.RelPath(misfiled))
	if err != nil {
		t.Fatalf("Stat() err = %v, want nil", err)
	}

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	after, err := os.Stat(lib.RelPath(misfiled))
	if err != nil {
		t.Fatalf("the file moved: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Error("the file was rewritten")
	}
	// The row names where the file actually is, not where the template
	// would put it.
	if recording, ok := catalog.recordingFor(misfiled); !ok {
		t.Errorf("no row names %q: %+v", misfiled, report.Files)
	} else if recording.Path != misfiled {
		t.Errorf("path = %q, want %q", recording.Path, misfiled)
	}
}

func TestRun_DryRunWritesNothing(t *testing.T) {
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	writeSidecar(t, lib, theRecording, fullSidecar())

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour},
		Options{DryRun: true}))

	if !report.DryRun {
		t.Error("report does not say it was a dry run")
	}
	// It still has to say what it would do, or a dry run tells the operator
	// nothing they can act on.
	if file := only(t, report); file.Disposition != Restored {
		t.Errorf("disposition = %q, want %q", file.Disposition, Restored)
	}
	if len(catalog.recordings) != 0 {
		t.Errorf("a dry run created %d rows, want none", len(catalog.recordings))
	}
	if len(catalog.channels) != 1 {
		t.Errorf("a dry run left %d channels, want only the one it started with",
			len(catalog.channels))
	}
}

func TestRun_LimitsToOneChannelWhenAsked(t *testing.T) {
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	other := "examplechannel/2026/examplechannel - 2026-08-15 18-34 - movie night.mkv"
	writeMedia(t, lib, other, 1024)

	catalog := newCatalog(atrioc(),
		store.Channel{ID: 8, Platform: "twitch", Name: "examplechannel"})
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour},
		Options{Channel: "atrioc"}))

	if report.Imported() != 1 {
		t.Fatalf("imported %d, want only the channel asked for: %+v", report.Imported(), report.Files)
	}
	if _, ok := catalog.recordingFor(theRecording); !ok {
		t.Error("the channel that was asked for was not imported")
	}
	if _, ok := catalog.recordingFor(other); ok {
		t.Error("a channel nobody asked for was imported")
	}
}

// ///////////////////////////////////////////////
// Two runs at once
// ///////////////////////////////////////////////

func TestRun_ARowThatAppearsMidScanIsASkip(t *testing.T) {
	// UNIQUE(path) is what actually decides this. The scan's own path check
	// cannot: it reads the paths once, and another run can claim the file
	// between that read and the write. Reporting the loser as a failure
	// would have an operator chasing an error that means the work is done.
	//
	// The catalog is desynchronized on purpose, so the scan sees no paths
	// while the write finds one. Racing two goroutines would exercise this
	// only when the scheduler happened to interleave them.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	writeSidecar(t, lib, theRecording, fullSidecar())

	catalog := newCatalog(atrioc())
	catalog.recordings = []store.Recording{{ID: 1, Path: theRecording}}

	report := run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Skipped {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Skipped, file.Reason)
	}
	if !strings.Contains(file.Reason, "while the scan was running") {
		t.Errorf("reason = %q, want it to say the row appeared mid-scan", file.Reason)
	}
	if len(catalog.recordings) != 1 {
		t.Errorf("created %d rows for one file, want the 1 that was already there",
			len(catalog.recordings))
	}
}

func TestRun_TwoRunsOverOneDirectoryMakeOneRow(t *testing.T) {
	// Whatever order they interleave in, one file is one row and neither
	// run fails.
	lib := libraryAt(t)
	for _, name := range []string{"one", "two", "three", "four"} {
		writeMedia(t, lib, "atrioc/2026/atrioc - 2026-08-15 18-34 - "+name+".mkv", 1024)
	}

	catalog := newCatalog(atrioc())
	var (
		wg      sync.WaitGroup
		reports [2]Report
		errs    [2]error
	)
	for i := range reports {
		wg.Go(func() {
			reports[i], errs[i] = importerFor(t, lib, catalog,
				fakeProber{length: time.Hour}, Options{}).Run(context.Background())
		})
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d err = %v, want nil", i, err)
		}
	}
	if len(catalog.recordings) != 4 {
		t.Fatalf("created %d rows for 4 files, want 4", len(catalog.recordings))
	}
	// Between them the two runs account for all eight sightings, and none
	// of them is a refusal.
	var imported, skipped int
	for _, report := range reports {
		imported += report.Imported()
		skipped += report.Count(Skipped)
		if refused := report.Count(Refused); refused != 0 {
			t.Errorf("a run refused %d files: %+v", refused, report.Files)
		}
	}
	if imported != 4 || imported+skipped != 8 {
		t.Errorf("runs reported %d imported and %d skipped, want 4 and 4", imported, skipped)
	}
}

func TestRun_StopsWhenTheContextIsCancelled(t *testing.T) {
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := importerFor(t, lib, newCatalog(atrioc()),
		fakeProber{length: time.Hour}, Options{}).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run() err = %v, want context.Canceled", err)
	}
}

// ///////////////////////////////////////////////
// A sidecar is a file, not an authority on identity
// ///////////////////////////////////////////////

func TestRun_RefusesASidecarNamingAChannelNobodyConfigured(t *testing.T) {
	// A library adopted from elsewhere holds whatever somebody put in it.
	// Without this gate a dropped file writes a channel row with a login
	// nobody configured, and that row appears in the calendar and in
	// coverage as though the operator had asked for it.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	sidecar.Channel = "somebody-else"
	writeSidecar(t, lib, theRecording, sidecar)

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Refused {
		t.Fatalf("disposition = %q, want %q", file.Disposition, Refused)
	}
	if !strings.Contains(file.Reason, "somebody-else") {
		t.Errorf("reason = %q, want it to name the channel", file.Reason)
	}
	if len(catalog.channels) != 1 {
		t.Errorf("channels = %d, want no row invented from a sidecar", len(catalog.channels))
	}
}

func TestRun_RefusesASidecarNamingAPlatformThisBuildHasNever(t *testing.T) {
	// The platform reaches UpsertChannel, and from there the calendar and
	// every provider lookup. A sidecar naming one nothing implements makes a
	// channel no part of this program can act on.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	sidecar.Platform = "../../etc"
	writeSidecar(t, lib, theRecording, sidecar)

	report := run(t, importerFor(t, lib, newCatalog(atrioc()),
		fakeProber{length: 2 * time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Refused {
		t.Fatalf("disposition = %q, want %q", file.Disposition, Refused)
	}
	if !strings.Contains(file.Reason, "platform") {
		t.Errorf("reason = %q, want it to name the platform", file.Reason)
	}
}

func TestRun_RefusesASidecarTooLargeToBeARecord(t *testing.T) {
	// The scan reads every file named like a sidecar, and os.ReadFile sizes
	// its buffer from the stat, so an oversized one is allocated whole
	// before anything looks at it.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	writeRaw(t, lib, theRecording,
		`{"schema_version":1,"title":"`+strings.Repeat("a", maxSidecarBytes)+`"}`)

	report := run(t, importerFor(t, lib, newCatalog(atrioc()),
		fakeProber{length: time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Refused {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Refused, file.Reason)
	}
	if !strings.Contains(file.Reason, "larger than") {
		t.Errorf("reason = %q, want it to say the sidecar was too large", file.Reason)
	}
}

func TestRun_RefusesASidecarCarryingMoreGapsThanAnyRecordingHas(t *testing.T) {
	// Each gap is its own insert and a filled one is an insert and an
	// update, so an unbounded list turns one file into hundreds of thousands
	// of writes.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	for i := range maxSidecarGaps + 1 {
		sidecar.Gaps = append(sidecar.Gaps, organize.SidecarGap{
			StartMS: int64(i) * 10, EndMS: int64(i)*10 + 5, Reason: "reconnect",
		})
	}
	writeSidecar(t, lib, theRecording, sidecar)

	catalog := newCatalog(atrioc())
	report := run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	if file := only(t, report); file.Disposition != Refused {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Refused, file.Reason)
	}
	if len(catalog.gaps) != 0 {
		t.Errorf("wrote %d gaps for a sidecar it refused", len(catalog.gaps))
	}
}

// ///////////////////////////////////////////////
// Channel identity
// ///////////////////////////////////////////////

func TestRun_ALoginIsNeverShadowedByAnotherChannelsDisplayName(t *testing.T) {
	// A display name is remote, free, and changeable. A streamer who picks
	// one matching another channel's login would otherwise collect that
	// channel's recordings, and case folding widens the target because a
	// name need only fold onto the login rather than equal it.
	tests := []struct {
		name    string
		login   string
		display string
		path    string
	}{
		{
			name: "an exact collision", login: "atrioc", display: "atrioc",
			path: theRecording,
		},
		{
			name: "a collision by case", login: "atrioc", display: "ATRIOC",
			path: theRecording,
		},
		{
			// U+212A KELVIN SIGN lowercases to an ordinary k, so a display
			// name nobody would read as the login folds onto it.
			name: "a collision by folding", login: "kotaku", display: "Kotaku",
			path: "kotaku/2026/kotaku - 2026-08-15 18-34 - movie night.mkv",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lib := libraryAt(t)
			writeMedia(t, lib, tt.path, 2048)

			// eve holds the display name. The login belongs to nobody yet
			// and arrives from the config, which is the weaker of the two
			// and so the one worth protecting.
			eve := store.Channel{ID: 99, Platform: "twitch", Name: "eve", DisplayName: tt.display}
			catalog := newCatalog(eve)
			run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{
				Configured: []config.Channel{{Platform: "twitch", Name: tt.login, Enabled: true}},
			}))

			recording, ok := catalog.recordingFor(tt.path)
			if !ok {
				t.Fatal("nothing was imported, so this proved nothing")
			}
			if recording.ChannelID == eve.ID {
				t.Errorf("the recording landed on %q, whose display name is %q",
					eve.Name, tt.display)
			}
		})
	}
}

// ///////////////////////////////////////////////
// One file is one row
// ///////////////////////////////////////////////

func TestRun_SkipsAFileAlreadyRecordedUnderAnotherSpelling(t *testing.T) {
	// Windows and macOS hand back a directory that already exists whatever
	// case is asked for, so a display name recapitalized between two
	// recordings puts the file under the old spelling while the row carries
	// the new one. UNIQUE(path) compares bytes and sees two files, and the
	// second row counts the same bytes again against the size cap.
	lib := libraryAt(t)
	writeMedia(t, lib, "Atrioc/2026/Atrioc - 2026-08-15 18-34 - movie night.mkv", 2048)

	catalog := newCatalog(atrioc())
	catalog.paths = []string{"atrioc/2026/atrioc - 2026-08-15 18-34 - movie night.mkv"}

	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{}))

	if file := only(t, report); file.Disposition != Skipped {
		t.Errorf("disposition = %q, want %q (%s)", file.Disposition, Skipped, file.Reason)
	}
	if len(catalog.recordings) != 0 {
		t.Errorf("created %d rows for a file already recorded", len(catalog.recordings))
	}
}

func TestRun_AWriteFailureAfterTheRowStillReportsTheRow(t *testing.T) {
	// The row exists from CreateRecording onward. Reporting "not imported"
	// about a file that now has a row makes the summary undercount, and the
	// next run reports the same file as skipped, so two runs disagree.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 2048)

	catalog := newCatalog(atrioc())
	catalog.mediaErr = errors.New("the disk went away")

	report := run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{}))

	file := only(t, report)
	if file.Disposition != Inferred {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Inferred, file.Reason)
	}
	if file.RecordingID == 0 {
		t.Error("the report drops the id of a row that was created")
	}
	if !strings.Contains(file.Reason, "disk went away") {
		t.Errorf("reason = %q, want the failure carried", file.Reason)
	}
	if report.Imported() != 1 {
		t.Errorf("Imported() = %d, want the row counted", report.Imported())
	}
}

// ///////////////////////////////////////////////
// Attaching to a broadcast
// ///////////////////////////////////////////////

func TestRun_AttachesARestoredRecordingToABroadcastAlreadyHere(t *testing.T) {
	// A recovery pass asks which recordings a broadcast has, and an
	// unattached one answers none, so it fetches a copy of a file already on
	// the disk. The sidecar carries the archive identifier that settles it.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	sidecar.RemoteID = "2847353784"
	writeSidecar(t, lib, theRecording, sidecar)

	catalog := newCatalog(atrioc())
	catalog.broadcasts = map[string]store.Broadcast{
		"2847353784": {ID: 42, ChannelID: atrioc().ID, RemoteID: "2847353784"},
	}

	report := run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	if file := only(t, report); file.Disposition != Restored {
		t.Fatalf("disposition = %q, want %q (%s)", file.Disposition, Restored, file.Reason)
	}
	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("no recording was created")
	}
	if recording.BroadcastID == nil {
		t.Fatal("the recording names no broadcast, so a recovery pass would fetch it again")
	}
	if *recording.BroadcastID != 42 {
		t.Errorf("broadcast = %d, want the one already here, 42", *recording.BroadcastID)
	}
}

func TestRun_RestoresTheBroadcastASidecarNames(t *testing.T) {
	// A remote id is an observation the recorder made and wrote down, so a
	// sidecar carrying one is evidence the broadcast happened. Restoring it
	// is what gives the recording a title, and what stops a later recovery
	// pass fetching a copy of a file already on the disk: the pass asks
	// which recordings a broadcast has, and an unattached one answers none.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	sidecar.RemoteID = "2847353784"
	sidecar.Title = "GET SMARTER SATURDAYS"
	writeSidecar(t, lib, theRecording, sidecar)

	catalog := newCatalog(atrioc())
	run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("no recording was created")
	}
	if recording.BroadcastID == nil {
		t.Fatal("the recording names no broadcast, so it carries no title and reads as uncovered")
	}

	restored, ok := catalog.broadcasts["2847353784"]
	if !ok {
		t.Fatal("no broadcast was restored under the sidecar's identifier")
	}
	if restored.Title != "GET SMARTER SATURDAYS" {
		t.Errorf("restored title = %q, want the sidecar's", restored.Title)
	}
	if restored.ChannelID != recording.ChannelID {
		t.Errorf("restored broadcast is on channel %d, recording on %d",
			restored.ChannelID, recording.ChannelID)
	}
	// The platform reported it as a VOD. Claiming live would say this
	// recorder watched it happen.
	if restored.Source != store.SourceAPI {
		t.Errorf("restored source = %q, want %q", restored.Source, store.SourceAPI)
	}
}

func TestRun_NeverInventsABroadcastWithoutAnIdentifier(t *testing.T) {
	// A file read back from a filename has no identifier to match on, and
	// guessing from the date would attach a broadcast on the strength of a
	// name. Nothing is invented where nothing was observed.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	sidecar.RemoteID = ""
	writeSidecar(t, lib, theRecording, sidecar)

	catalog := newCatalog(atrioc())
	run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("no recording was created")
	}
	if recording.BroadcastID != nil {
		t.Errorf("the recording names broadcast %d, but there is no identifier to match on",
			*recording.BroadcastID)
	}
	if len(catalog.broadcasts) != 0 {
		t.Errorf("%d broadcasts were invented for a sidecar naming none", len(catalog.broadcasts))
	}
}

func TestRun_NeverAttachesABroadcastFromAnotherChannel(t *testing.T) {
	// An archive identifier is unique inside its own channel and says
	// nothing across two. Attaching this recording to the other channel's
	// row would credit one channel's broadcast with another's file.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	sidecar.RemoteID = "2847353784"
	writeSidecar(t, lib, theRecording, sidecar)

	catalog := newCatalog(atrioc())
	catalog.broadcasts = map[string]store.Broadcast{
		"2847353784": {ID: 42, ChannelID: 999, RemoteID: "2847353784", Title: "someone else"},
	}

	run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("no recording was created")
	}
	if recording.BroadcastID != nil && *recording.BroadcastID == 42 {
		t.Error("the recording was attached to another channel's broadcast")
	}
	if recording.ChannelID == 999 {
		t.Error("the recording landed on the other channel entirely")
	}
}

func TestRun_CarriesOnWhenABroadcastCannotBeRestored(t *testing.T) {
	// The file is the thing being imported. A broadcast that will not store
	// costs the title and the coverage, and losing the recording over it
	// would cost the import.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)

	sidecar := fullSidecar()
	sidecar.RemoteID = "2847353784"
	writeSidecar(t, lib, theRecording, sidecar)

	catalog := newCatalog(atrioc())
	catalog.upsertErr = errors.New("the broadcast table is unavailable")

	run(t, importerFor(t, lib, catalog, fakeProber{length: 2 * time.Hour}, Options{}))

	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("the recording was lost because its broadcast could not be restored")
	}
	if recording.BroadcastID != nil {
		t.Errorf("the recording names broadcast %d, but none was stored", *recording.BroadcastID)
	}
}

func TestRun_ARecordingReadFromItsNameNamesNoBroadcast(t *testing.T) {
	// Its date is a wall clock to the minute and its title went through a
	// rendering nothing can undo, so no identifier survives to match on.
	// Guessing from the date would attach a broadcast on the strength of a
	// filename.
	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 2048)

	catalog := newCatalog(atrioc())
	catalog.broadcasts = map[string]store.Broadcast{
		"2847353784": {ID: 42, ChannelID: atrioc().ID, RemoteID: "2847353784"},
	}

	run(t, importerFor(t, lib, catalog, fakeProber{length: time.Hour}, Options{}))

	recording, ok := catalog.recordingFor(theRecording)
	if !ok {
		t.Fatal("no recording was created")
	}
	if recording.BroadcastID != nil {
		t.Errorf("a recording read from its name names broadcast %d", *recording.BroadcastID)
	}
}

// ///////////////////////////////////////////////
// Restoring against the real store
// ///////////////////////////////////////////////

// realCatalog opens an in-memory store, which satisfies Catalog. The fake
// answers what it was told to; these tests are about what the store does.
func realCatalog(t *testing.T) *store.Store {
	t.Helper()

	db, err := store.OpenMemory()
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var _ Catalog = db
	return db
}

func TestRun_NeverTakesOverABroadcastTheRecorderWatchedLive(t *testing.T) {
	// The store's upsert matches any nearby row whose archive id is still
	// blank, which is exactly a broadcast the recorder saw live and has not
	// yet seen the listing for. Restoring through it would stamp this
	// file's identifier and start time onto that row: the recorder's own
	// observation replaced by what a file on disk claims, and the real
	// listing then matching nothing and inserting a second row for one
	// broadcast.
	db := realCatalog(t)

	channel, err := db.UpsertChannel("twitch", "atrioc", "")
	if err != nil {
		t.Fatalf("UpsertChannel: %v", err)
	}
	liveStart := time.Date(2026, 8, 15, 18, 34, 0, 0, time.UTC)
	live, err := db.UpsertBroadcast(store.Broadcast{
		ChannelID: channel.ID,
		StreamID:  "LIVE-1",
		Title:     "the title the recorder saw",
		StartedAt: liveStart,
		Source:    store.SourceLive,
	})
	if err != nil {
		t.Fatalf("seeding the live broadcast: %v", err)
	}

	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	sidecar := fullSidecar()
	sidecar.Channel = "atrioc"
	sidecar.RemoteID = "2847353784"
	sidecar.Title = "what the file claims"
	// Inside the store's merge window, which is what makes this reachable.
	sidecar.StartedAt = liveStart.Add(-12 * time.Minute)
	writeSidecar(t, lib, theRecording, sidecar)

	run(t, importerFor(t, lib, db, fakeProber{length: 2 * time.Hour}, Options{}))

	after, err := db.Broadcast(live.ID)
	if err != nil {
		t.Fatalf("reading the live broadcast back: %v", err)
	}
	if after.RemoteID != "" {
		t.Errorf("the live row took the file's archive id %q", after.RemoteID)
	}
	if !after.StartedAt.Equal(liveStart) {
		t.Errorf("the live row's start moved to %s, was %s", after.StartedAt, liveStart)
	}
	if after.Title != "the title the recorder saw" {
		t.Errorf("the live row's title became %q", after.Title)
	}

	// One broadcast, not two. A second row here is the redundant fetch this
	// whole path exists to avoid.
	all, err := db.BroadcastsBetween(channel.ID, liveStart.Add(-time.Hour), liveStart.Add(time.Hour))
	if err != nil {
		t.Fatalf("BroadcastsBetween: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("%d broadcasts for one broadcast", len(all))
	}
}

func TestRun_AttachesToTheBroadcastAlreadyStandingAtThatHour(t *testing.T) {
	// Joining the existing row is what stops a recovery pass fetching a
	// copy of a file already on the disk: the pass asks which recordings a
	// broadcast has, and an unattached one answers none.
	db := realCatalog(t)

	channel, err := db.UpsertChannel("twitch", "atrioc", "")
	if err != nil {
		t.Fatalf("UpsertChannel: %v", err)
	}
	liveStart := time.Date(2026, 8, 15, 18, 34, 0, 0, time.UTC)
	live, err := db.UpsertBroadcast(store.Broadcast{
		ChannelID: channel.ID, StreamID: "LIVE-1",
		StartedAt: liveStart, Source: store.SourceLive,
	})
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	sidecar := fullSidecar()
	sidecar.Channel = "atrioc"
	sidecar.RemoteID = "2847353784"
	sidecar.StartedAt = liveStart.Add(-12 * time.Minute)
	writeSidecar(t, lib, theRecording, sidecar)

	run(t, importerFor(t, lib, db, fakeProber{length: 2 * time.Hour}, Options{}))

	attached, err := db.RecordingsForBroadcast(live.ID)
	if err != nil {
		t.Fatalf("RecordingsForBroadcast: %v", err)
	}
	if len(attached) != 1 {
		t.Fatalf("%d recordings attached to the live broadcast, want 1", len(attached))
	}
}

func TestRun_LeavesNoBroadcastBehindWhenTheRecordingIsRefused(t *testing.T) {
	// A broadcast with no capture reads as one nobody caught, and invites a
	// recovery pass to fetch a copy of a file that was never imported.
	db := realCatalog(t)

	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	sidecar := fullSidecar()
	sidecar.Channel = "atrioc"
	sidecar.RemoteID = "2847353784"
	// Refused by CreateRecording, which checks the ordering.
	ended := sidecar.StartedAt.Add(-time.Hour)
	sidecar.EndedAt = &ended
	writeSidecar(t, lib, theRecording, sidecar)

	report := run(t, importerFor(t, lib, db, fakeProber{length: 2 * time.Hour}, Options{}))

	if len(report.Files) != 1 || report.Files[0].Disposition != Refused {
		t.Fatalf("files = %+v, want one refused", report.Files)
	}

	channels, err := db.Channels()
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	for _, channel := range channels {
		all, err := db.BroadcastsBetween(channel.ID,
			sidecar.StartedAt.Add(-24*time.Hour), sidecar.StartedAt.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("BroadcastsBetween: %v", err)
		}
		if len(all) != 0 {
			t.Errorf("a refused recording left %d broadcasts behind", len(all))
		}
	}
}

func TestRun_KeepsTwoChannelsArchiveIdentifiersApart(t *testing.T) {
	// An archive identifier is unique inside its own channel and says
	// nothing across two. Asserted against the store, because the guard
	// that matters is the channel_id in its own query.
	db := realCatalog(t)

	other, err := db.UpsertChannel("twitch", "someoneelse", "")
	if err != nil {
		t.Fatalf("UpsertChannel: %v", err)
	}
	theirs, err := db.UpsertBroadcast(store.Broadcast{
		ChannelID: other.ID, RemoteID: "2847353784",
		StartedAt: time.Date(2026, 8, 15, 18, 34, 0, 0, time.UTC),
		Source:    store.SourceAPI,
	})
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	lib := libraryAt(t)
	writeMedia(t, lib, theRecording, 1024)
	sidecar := fullSidecar()
	sidecar.Channel = "atrioc"
	sidecar.RemoteID = "2847353784"
	writeSidecar(t, lib, theRecording, sidecar)

	run(t, importerFor(t, lib, db, fakeProber{length: 2 * time.Hour}, Options{}))

	attached, err := db.RecordingsForBroadcast(theirs.ID)
	if err != nil {
		t.Fatalf("RecordingsForBroadcast: %v", err)
	}
	if len(attached) != 0 {
		t.Errorf("%d recordings were credited to another channel's broadcast", len(attached))
	}
}
