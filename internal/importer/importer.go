// Package importer adopts files already sitting in a library that no
// recording row names.
//
// It exists because the sidecar was built to be read. Every finished
// recording gets one, and organize.Sidecar states outright that the database
// is a cache while the files and the media are the record. Nothing walked the
// library to act on that, so a library whose database was lost held its whole
// archive and could not list any of it.
//
// Nothing here moves, renames, or writes a media file. An import records
// where a file already is. Adopting and reorganizing in one step is how an
// archive gets lost, and keeping the two apart is what makes this safe to run
// against a directory somebody spent years filling.
//
// Two tiers, and the difference between them is what is known rather than
// how hard it was to find out. A file with a sidecar is restored: the sidecar
// carries what the recorder observed, down to the origin the recording
// actually had. A file without one is read back from its own name, which
// yields a title to the minute, in whatever zone rendered it, after a
// sanitization nothing can undo. Those rows are marked store.OriginImported
// so that everything downstream can tell a record from a reading.
package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/naming"
	"zach.tools/go/stream-dvr/internal/organize"
	"zach.tools/go/stream-dvr/internal/store"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Disposition is what an import did with one file.
type Disposition string

// Catalog is the part of the store an import reads and writes.
//
// It is declared here rather than taken as *store.Store so a test can drive
// the decisions without a database, and so the surface an import is allowed
// to touch is written down. Nothing here deletes or updates an existing
// recording: an import adds rows and does nothing else.
type Catalog interface {
	RecordingPaths() ([]string, error)
	Channels() ([]store.Channel, error)
	UpsertChannel(platform, name, displayName string) (store.Channel, error)
	BroadcastByRemoteID(channelID int64, remoteID string) (store.Broadcast, error)
	UpsertBroadcast(b store.Broadcast) (store.Broadcast, error)
	BroadcastsBetween(channelID int64, from, to time.Time) ([]store.Broadcast, error)
	SetBroadcastRecording(id, broadcastID int64) error
	ObserveTitle(broadcastID int64, at time.Time, title, category string) error
	CreateRecording(r store.Recording) (store.Recording, error)
	SetMediaDuration(id int64, duration time.Duration) error
	SetMutedDuration(id int64, muted time.Duration) error
	AddGap(recordingID int64, start, end time.Duration, reason string) (store.Gap, error)
	FillGap(id int64, at time.Time) error
}

// Prober measures a media file.
type Prober interface {
	Duration(ctx context.Context, path string) (time.Duration, error)
}

// File is one candidate the scan considered, and what became of it.
type File struct {
	// Path is where the file sits, relative to the library root.
	Path string
	// Disposition is what happened to it.
	Disposition Disposition
	// RecordingID is the row this created, or zero.
	RecordingID int64
	// Reason says why, for anything that was not straightforwardly
	// imported. It is empty for a clean restore.
	Reason string
	// Disagreements name what the file contradicted about itself, sorted.
	// A sidecar states a size and a length, and the file is the authority
	// on both, so a mismatch is reported rather than resolved silently.
	Disagreements []string
}

// Report is what one run amounted to.
type Report struct {
	// Files are every candidate considered, in the order they were found.
	Files []File
	// DryRun reports that nothing was written.
	DryRun bool
}

// Options configure one import.
type Options struct {
	// DryRun scans and decides without writing a row.
	DryRun bool
	// Channel limits the run to one channel login, matched the same way a
	// name is matched against the channels this machine knows. Empty means
	// every channel.
	Channel string
	// Configured are the channels the operator listed in the config.
	//
	// They count as known alongside the ones the database holds. A library
	// whose database was lost still has its config, and refusing every file
	// because no channel row survived would leave the one case this exists
	// for unable to import anything.
	Configured []config.Channel
}

// Importer adopts library files no recording row names.
type Importer struct {
	library  *library.Library
	catalog  Catalog
	prober   Prober
	template *naming.Template
	location *time.Location
	options  Options
}

// channelIndex resolves a name read off a filename to a channel this machine
// already knows.
type channelIndex struct {
	byName map[string]store.Channel
}

// ///////////////////////////////////////////////
// Dispositions
// ///////////////////////////////////////////////

const (
	// Restored is a file whose sidecar carried its record. Nothing about
	// the row is a guess.
	Restored Disposition = "restored"
	// Inferred is a file read back from its own name, for want of a
	// sidecar. Its row is marked store.OriginImported.
	Inferred Disposition = "inferred"
	// Skipped is a file some recording already names.
	Skipped Disposition = "skipped"
	// Refused is a file nothing could account for. It is left exactly where
	// it is, which is the same thing that happens to it today.
	Refused Disposition = "refused"
)

// ///////////////////////////////////////////////
// Construction
// ///////////////////////////////////////////////

// New returns an importer over one library.
func New(lib *library.Library, catalog Catalog, prober Prober,
	template *naming.Template, location *time.Location, options Options,
) *Importer {
	if location == nil {
		location = time.UTC
	}
	return &Importer{
		library:  lib,
		catalog:  catalog,
		prober:   prober,
		template: template,
		location: location,
		options:  options,
	}
}

// ///////////////////////////////////////////////
// Running
// ///////////////////////////////////////////////

// Run scans the library and adopts what it can account for.
//
// A file that cannot be accounted for is reported and left alone. Refusing to
// import is never a failure: the file is exactly as it was, and the operator
// gets to see which names their template cannot read.
func (i *Importer) Run(ctx context.Context) (Report, error) {
	recorded, err := i.recordedPaths()
	if err != nil {
		return Report{}, err
	}
	index, err := i.knownChannels()
	if err != nil {
		return Report{}, err
	}

	report := Report{DryRun: i.options.DryRun}
	candidates, err := i.candidates()
	if err != nil {
		return Report{}, err
	}
	for _, relPath := range candidates {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		if recorded[pathKey(relPath)] {
			report.Files = append(report.Files, File{
				Path:        relPath,
				Disposition: Skipped,
				Reason:      "a recording already names this file",
			})
			continue
		}
		report.Files = append(report.Files, i.adopt(ctx, relPath, index))
	}
	return report, nil
}

// recordedPaths indexes every path the library already names, keyed so that
// two spellings of one file collide.
//
// Folded, because the column is not. Windows and macOS both hand back a
// directory that already exists whatever case is asked for, so a display name
// recapitalized between two recordings puts the file under the old spelling
// while the row carries the new one. UNIQUE(path) compares bytes and sees two
// files, and the second row then counts the same bytes again against the size
// cap, which trips the purge early and starts offering real recordings for
// deletion.
//
// Folding costs the case where one library genuinely holds two files differing
// only in case, which is possible on Linux and is a library nobody can keep
// straight anyway. Skipping the second is the safe direction: nothing is
// deleted, and the file stays exactly where it is.
func (i *Importer) recordedPaths() (map[string]bool, error) {
	stored, err := i.catalog.RecordingPaths()
	if err != nil {
		return nil, fmt.Errorf("reading recorded paths: %w", err)
	}
	recorded := make(map[string]bool, len(stored))
	for _, path := range stored {
		recorded[pathKey(path)] = true
	}
	return recorded, nil
}

// knownChannels indexes the channels this machine already knows, by login and
// by display name, both folded.
//
// A name read off a filename is matched against this and nothing else. The
// alternative is creating a channel from whatever the name yielded, and the
// default template writes the author, which is a display name that may not be
// any login at all. A channel invented that way then appears in the calendar
// and in coverage as though the operator had configured it.
func (i *Importer) knownChannels() (channelIndex, error) {
	channels, err := i.catalog.Channels()
	if err != nil {
		return channelIndex{}, fmt.Errorf("reading channels: %w", err)
	}

	index := channelIndex{byName: make(map[string]store.Channel, len(channels)*2)}

	// Logins first, from both sources, because a login is the identity. A
	// configured channel with no row yet is known but carries no identifier,
	// and a database row for the same login replaces it. The row is written
	// only once a file matches, which keeps a dry run from creating one.
	for _, channel := range i.options.Configured {
		index.byName[fold(channel.Name)] = store.Channel{
			Platform: channel.Platform,
			Name:     channel.Name,
		}
	}
	for _, channel := range channels {
		index.byName[fold(channel.Name)] = channel
	}

	// Display names last, and never over a login. A display name is remote,
	// free, and changeable: a streamer who picks one matching another
	// channel's login would otherwise collect that channel's recordings, and
	// case folding widens the target because a name need only fold onto the
	// login rather than equal it.
	for _, channel := range channels {
		if channel.DisplayName == "" {
			continue
		}
		if _, claimed := index.byName[fold(channel.DisplayName)]; claimed {
			continue
		}
		index.byName[fold(channel.DisplayName)] = channel
	}
	return index, nil
}

// candidates lists the media files under the library root, relative to it and
// in a stable order.
//
// The library's own directories are skipped whole. The state directory holds
// the database and the trash, and the incoming directory holds captures that
// have not finished, both of which have an owner already.
//
// Only regular files are considered, and that check is what carries the
// weight rather than the symlink test beside it. WalkDir reports a symlink
// instead of following it, but Windows junctions arrive as irregular files
// and would otherwise be walked into, so both are refused by asking what the
// entry is rather than by naming the forms it must not be.
//
// A hardlink is a regular file and is adopted. The row then names bytes that
// also live under another name outside the library, which costs nothing: a
// purge removes this library's directory entry and the other name survives.
func (i *Importer) candidates() ([]string, error) {
	root := i.library.Root()
	var found []string

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relPath = filepath.ToSlash(relPath)

		if entry.IsDir() {
			if reserved(relPath) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		if isMedia(relPath) {
			found = append(found, relPath)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning %s: %w", root, err)
	}

	slices.Sort(found)
	return found, nil
}

// adopt decides what one file is and records it.
func (i *Importer) adopt(ctx context.Context, relPath string, index channelIndex) File {
	sidecar, err := i.readSidecar(relPath)
	switch {
	case err != nil && !errors.Is(err, os.ErrNotExist):
		// A sidecar too new to read is refused outright rather than falling
		// back to the filename. The newer build recorded fields this one
		// cannot see, and importing from the name would replace a complete
		// record with a reading of it.
		if errors.Is(err, errSidecarTooNew) || errors.Is(err, errSidecarUnusable) {
			return File{Path: relPath, Disposition: Refused, Reason: err.Error()}
		}
		// Anything else is a sidecar this build cannot parse, which says
		// nothing about the media beside it.
		return i.fromName(ctx, relPath, index, "its sidecar does not parse: "+err.Error())
	case err != nil:
		return i.fromName(ctx, relPath, index, "")
	}
	return i.fromSidecar(ctx, relPath, sidecar, index)
}

// fromSidecar restores a recording from the record written beside it.
func (i *Importer) fromSidecar(ctx context.Context, relPath string, sidecar organize.Sidecar,
	index channelIndex,
) File {
	if !i.wanted(sidecar.Channel, sidecar.Author) {
		return File{Path: relPath, Disposition: Skipped, Reason: "another channel was asked for"}
	}
	if sidecar.Channel == "" || sidecar.Platform == "" {
		return i.fromName(ctx, relPath, index, "its sidecar names no channel")
	}

	// A sidecar is a file, and a library adopted from elsewhere holds
	// whatever somebody put in it. It is trusted for what it says about a
	// recording, not for who that recording belongs to: without this gate a
	// dropped file writes a channel row with a platform and a login nobody
	// configured, and that row then appears in the calendar and in coverage
	// as though the operator had asked for it.
	if _, known := index.byName[fold(sidecar.Channel)]; !known {
		return File{
			Path:        relPath,
			Disposition: Refused,
			Reason: fmt.Sprintf("its sidecar names channel %q, which this machine does not know",
				sidecar.Channel),
		}
	}
	if !slices.Contains(config.SupportedPlatforms, sidecar.Platform) {
		return File{
			Path:        relPath,
			Disposition: Refused,
			Reason: fmt.Sprintf("its sidecar names platform %q, which this build does not support",
				sidecar.Platform),
		}
	}

	measured, err := i.measure(ctx, relPath)
	if err != nil {
		return File{Path: relPath, Disposition: Refused, Reason: err.Error()}
	}

	origin := store.Origin(sidecar.Origin)
	if !origin.Valid() {
		return File{
			Path:        relPath,
			Disposition: Refused,
			Reason: fmt.Sprintf("its sidecar names origin %q, which this build does not know",
				sidecar.Origin),
		}
	}

	if i.options.DryRun {
		return File{
			Path: relPath, Disposition: Restored,
			Disagreements: disagreements(sidecar, measured),
		}
	}

	channel, err := i.catalog.UpsertChannel(sidecar.Platform, sidecar.Channel, sidecar.Author)
	if err != nil {
		return File{Path: relPath, Disposition: Refused, Reason: err.Error()}
	}

	// The recording is written first, and the broadcast only once it
	// exists. The other order leaves a broadcast with no capture behind
	// whenever the recording is refused, and a broadcast with no capture
	// reads as one nobody caught and invites a recovery pass to fetch a
	// copy of a file that was never imported.
	stored, err := i.catalog.CreateRecording(store.Recording{
		ChannelID: channel.ID,
		Path:      relPath,
		State:     store.StateComplete,
		Origin:    origin,
		Bytes:     measured.bytes,
		Duration:  claimedOrMeasured(sidecar.DurationMS, measured.media),
		StartedAt: sidecar.StartedAt,
		EndedAt:   sidecar.EndedAt,
	})
	if err != nil {
		return dispositionFor(relPath, err)
	}

	file := File{
		Path: relPath, Disposition: Restored, RecordingID: stored.ID,
		Disagreements: disagreements(sidecar, measured),
	}

	var notes []string
	broadcastID, err := i.broadcastFor(channel.ID, sidecar)
	if err != nil {
		notes = append(notes, err.Error())
	}
	if broadcastID != nil {
		if err := i.catalog.SetBroadcastRecording(stored.ID, *broadcastID); err != nil {
			notes = append(notes, "it could not be attached to its broadcast: "+err.Error())
			broadcastID = nil
		}
	}
	if note := i.restoreDetail(stored.ID, broadcastID, sidecar, measured); note != "" {
		notes = append(notes, note)
	}
	file.Reason = strings.Join(notes, "; ")
	return file
}

// fromName reads a recording back from its own filename.
//
// why carries whatever made the sidecar unusable, so a file that had one and
// could not use it does not read as a file that never had one.
func (i *Importer) fromName(ctx context.Context, relPath string, index channelIndex, why string) File {
	refuse := func(reason string) File {
		return File{Path: relPath, Disposition: Refused, Reason: join(why, reason)}
	}

	match, err := i.template.Match(relPath, i.location)
	if err != nil {
		return refuse(err.Error())
	}
	if match.Fields.StartedAt.IsZero() {
		return refuse("its name carries no date, so nothing places the recording in time")
	}

	name := match.Fields.Channel
	if name == "" {
		name = match.Fields.Author
	}
	if !i.wanted(name, match.Fields.Author) {
		return File{Path: relPath, Disposition: Skipped, Reason: "another channel was asked for"}
	}
	channel, known := index.byName[fold(name)]
	if !known {
		return refuse(fmt.Sprintf("its name reads as channel %q, which this machine does not know",
			name))
	}

	measured, err := i.measure(ctx, relPath)
	if err != nil {
		return refuse(err.Error())
	}
	if measured.media <= 0 {
		return refuse("nothing could measure its length, so it may not be a recording at all")
	}

	if i.options.DryRun {
		return File{Path: relPath, Disposition: Inferred, Reason: join(why, lossyNote(match))}
	}

	// A channel the operator configured but no recording ever reached has no
	// row yet. Writing it here rather than up front means a run that adopts
	// nothing leaves the database exactly as it found it.
	if channel.ID == 0 {
		channel, err = i.catalog.UpsertChannel(channel.Platform, channel.Name, "")
		if err != nil {
			return refuse(err.Error())
		}
	}

	stored, err := i.catalog.CreateRecording(store.Recording{
		ChannelID: channel.ID,
		Path:      relPath,
		State:     store.StateComplete,
		Origin:    store.OriginImported,
		Bytes:     measured.bytes,
		Duration:  measured.media,
		StartedAt: match.Fields.StartedAt,
	})
	if err != nil {
		return dispositionFor(relPath, err)
	}

	note := join(why, lossyNote(match))
	// The row exists from here on. A failure after it is carried as a note
	// rather than a refusal: reporting "not imported" about a file that now
	// has a row makes the summary undercount, and the next run reports the
	// same file as skipped, so two runs disagree about one file.
	if measured.media > 0 {
		if err := i.catalog.SetMediaDuration(stored.ID, measured.media); err != nil {
			note = join(note, "its measured length could not be stored: "+err.Error())
		}
	}
	return File{
		Path: relPath, Disposition: Inferred, RecordingID: stored.ID,
		Reason: note,
	}
}

// restoreDetail writes the parts of a sidecar that do not fit on the
// recording row, and reports what could not be restored.
func (i *Importer) restoreDetail(recordingID int64, broadcastID *int64,
	sidecar organize.Sidecar, measured measurement,
) string {
	var notes []string

	if measured.media > 0 {
		if err := i.catalog.SetMediaDuration(recordingID, measured.media); err != nil {
			notes = append(notes, "its measured length could not be stored: "+err.Error())
		}
	}
	if sidecar.MutedMS != nil {
		muted := time.Duration(*sidecar.MutedMS) * time.Millisecond
		if err := i.catalog.SetMutedDuration(recordingID, muted); err != nil {
			notes = append(notes, "its muted stretch could not be stored: "+err.Error())
		}
	}
	for _, gap := range sidecar.Gaps {
		if err := i.restoreGap(recordingID, gap); err != nil {
			notes = append(notes, err.Error())
		}
	}
	// Title history hangs off a broadcast, so it goes back only where the
	// sidecar named one. A file with no archive identifier has no broadcast
	// to hang them on, and saying so beats dropping them in silence.
	switch {
	case len(sidecar.TitleHistory) == 0:
	case broadcastID == nil && sidecar.RemoteID == "":
		notes = append(notes, fmt.Sprintf("%d title observations were not restored, "+
			"because the sidecar names no broadcast to attach them to", len(sidecar.TitleHistory)))
	case broadcastID == nil:
		notes = append(notes, fmt.Sprintf("%d title observations were not restored, "+
			"because its broadcast could not be stored", len(sidecar.TitleHistory)))
	default:
		for _, observed := range sidecar.TitleHistory {
			if err := i.catalog.ObserveTitle(*broadcastID,
				observed.ObservedAt, observed.Title, observed.Category); err != nil {
				notes = append(notes, "a title observation could not be stored: "+err.Error())
			}
		}
	}
	return strings.Join(notes, "; ")
}

// restoreGap writes one hole back, with the filled state it carried.
func (i *Importer) restoreGap(recordingID int64, gap organize.SidecarGap) error {
	start := time.Duration(gap.StartMS) * time.Millisecond
	end := time.Duration(gap.EndMS) * time.Millisecond

	stored, err := i.catalog.AddGap(recordingID, start, end, gap.Reason)
	if err != nil {
		return fmt.Errorf("a gap could not be stored: %w", err)
	}
	if !gap.Filled {
		return nil
	}
	// The sidecar records that a gap was filled but not when, and a filled
	// gap that reads as open would be patched a second time.
	if err := i.catalog.FillGap(stored.ID, time.Now().UTC()); err != nil {
		return fmt.Errorf("a filled gap could not be marked filled: %w", err)
	}
	return nil
}

// broadcastFor returns the broadcast a sidecar's recording belongs to,
// restoring it from the sidecar when this library holds none, or nil when
// the sidecar names no archive identifier.
//
// Attaching the recording is what stops a later recovery pass fetching a
// copy of a file already on the disk: the pass asks which recordings a
// broadcast has, and an unattached one answers none.
//
// Restored rather than found only. A remote id is an observation the
// recorder made and wrote down, so a sidecar carrying one is evidence the
// broadcast happened, not a guess about it. The row it produces always has
// this recording attached, so it reads as covered rather than as a
// broadcast nobody caught.
//
// A recording read back from a filename gets none of this. Its date is a
// wall clock to the minute and its title went through a rendering nothing can
// undo, so no identifier survives to match on, and guessing from the date
// would attach a broadcast to a file on the strength of a name.
func (i *Importer) broadcastFor(channelID int64, sidecar organize.Sidecar) (*int64, error) {
	if sidecar.RemoteID == "" {
		return nil, nil
	}
	if broadcast, err := i.catalog.BroadcastByRemoteID(channelID, sidecar.RemoteID); err == nil {
		return &broadcast.ID, nil
	}

	// A broadcast already standing at this hour is joined rather than
	// written over. The store's own upsert would take it: its fallback
	// matches any row nearby whose archive id is still blank, which is
	// exactly a broadcast the recorder watched live and has not yet seen
	// the listing for. Handing that row this file's identifier and start
	// time replaces what the recorder observed with what a file on disk
	// claims, and the real listing then matches nothing and inserts a
	// second row for one broadcast.
	if found, err := i.overlapping(channelID, sidecar.StartedAt); err == nil && found != nil {
		return found, nil
	}

	restored, err := i.catalog.UpsertBroadcast(store.Broadcast{
		ChannelID: channelID,
		RemoteID:  sidecar.RemoteID,
		Title:     sidecar.Title,
		Category:  sidecar.Category,
		StartedAt: sidecar.StartedAt,
		EndedAt:   sidecar.EndedAt,
		// The platform reported this one, as a VOD, which is where a
		// sidecar's archive identifier and start time both came from.
		// Saying live would claim this recorder watched it happen.
		Source: store.SourceAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("its broadcast could not be restored: %w", err)
	}
	return &restored.ID, nil
}

// overlapping reports a broadcast this library already holds around a start
// time, or nil.
//
// The window matches the one the store merges discoveries within, so a
// restore joins whatever an ordinary discovery of the same broadcast would
// have joined.
func (i *Importer) overlapping(channelID int64, startedAt time.Time) (*int64, error) {
	if startedAt.IsZero() {
		return nil, nil
	}
	near, err := i.catalog.BroadcastsBetween(channelID,
		startedAt.Add(-broadcastOverlap), startedAt.Add(broadcastOverlap))
	if err != nil || len(near) == 0 {
		return nil, err
	}
	return &near[0].ID, nil
}

// wanted reports whether a file belongs to the channel this run was limited
// to.
func (i *Importer) wanted(name, author string) bool {
	if i.options.Channel == "" {
		return true
	}
	asked := fold(i.options.Channel)
	return fold(name) == asked || fold(author) == asked
}

// readSidecar loads the record written beside a media file.
func (i *Importer) readSidecar(relPath string) (organize.Sidecar, error) {
	file, err := os.Open(i.library.RelPath(organize.SidecarPath(relPath)))
	if err != nil {
		return organize.Sidecar{}, err
	}
	defer file.Close()

	// Read through a limit rather than whole. os.ReadFile sizes its buffer
	// from the stat, so a sidecar of any size is allocated before anything
	// looks at it, and a file named like a sidecar is not one.
	body, err := io.ReadAll(io.LimitReader(file, maxSidecarBytes+1))
	if err != nil {
		return organize.Sidecar{}, fmt.Errorf("reading the sidecar: %w", err)
	}
	if int64(len(body)) > maxSidecarBytes {
		return organize.Sidecar{}, fmt.Errorf("its sidecar is larger than %s, which no record is: %w",
			config.Size(maxSidecarBytes), errSidecarUnusable)
	}

	var sidecar organize.Sidecar
	if err := json.Unmarshal(body, &sidecar); err != nil {
		return organize.Sidecar{}, fmt.Errorf("parsing the sidecar: %w", err)
	}
	if len(sidecar.Gaps) > maxSidecarGaps {
		return organize.Sidecar{}, fmt.Errorf("its sidecar carries %d gaps and no recording has more than %d: %w",
			len(sidecar.Gaps), maxSidecarGaps, errSidecarUnusable)
	}
	if len(sidecar.TitleHistory) > maxSidecarTitles {
		return organize.Sidecar{}, fmt.Errorf(
			"its sidecar carries %d title observations and no broadcast has more than %d: %w",
			len(sidecar.TitleHistory), maxSidecarTitles, errSidecarUnusable)
	}
	if sidecar.SchemaVersion > organize.SidecarVersion {
		return organize.Sidecar{}, fmt.Errorf("its sidecar is version %d and this build reads %d: %w",
			sidecar.SchemaVersion, organize.SidecarVersion, errSidecarTooNew)
	}
	return sidecar, nil
}

// measure reads the file's own account of itself.
//
// Both numbers come from the file rather than from anything written about it.
// A sidecar on disk in this library states zero bytes for a recording of ten
// gigabytes, because it was written by a build that stamped the row before
// measuring it, and a row copied from that claim would put the library's size
// cap ten gigabytes out.
func (i *Importer) measure(ctx context.Context, relPath string) (measurement, error) {
	full := i.library.RelPath(relPath)

	info, err := os.Stat(full)
	if err != nil {
		return measurement{}, fmt.Errorf("measuring %s: %w", relPath, err)
	}

	// A length that cannot be read is left at zero, which the store already
	// means as nobody has measured it. Refusing the whole import over a
	// probe would discard a record for want of a number.
	media, err := i.prober.Duration(ctx, full)
	if err != nil || media < 0 {
		media = 0
	}
	return measurement{bytes: info.Size(), media: media}, nil
}
