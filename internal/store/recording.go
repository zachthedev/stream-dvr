package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// State is where a recording sits in its lifecycle.
type State string

// Origin records where a recording's bytes came from.
type Origin string

// Recording is one file in the library.
type Recording struct {
	ID          int64
	ChannelID   int64
	BroadcastID *int64
	// Path is relative to the library root, always with forward slashes
	// whatever the host separator is. It is the capture name until the
	// organizer renders a validated one.
	Path     string
	State    State
	Origin   Origin
	Bytes    int64
	Duration time.Duration
	// MediaDuration is how much broadcast the file actually holds, measured
	// from the media rather than from a clock around the capture. It falls
	// short of Duration by whatever the recorder never received, which is
	// what an ad break costs on a platform that replaces the segments. Zero
	// means nobody has measured it.
	MediaDuration time.Duration
	// MutedDuration is how much of this file the platform silenced, for a
	// copy fetched from an archive. Nil means nobody asked, which is every
	// live capture and every machine with no platform session, and is not the
	// same as an answer of none.
	MutedDuration *time.Duration
	StartedAt     time.Time
	EndedAt       *time.Time
	WatchedAt     *time.Time
	// RecompressedAt is when this recording was re-encoded to a denser
	// codec. Nil means never, which is what every recording is until the
	// recompress rung is switched on for the machine holding it.
	RecompressedAt *time.Time
	Pinned         bool
	Refetchable    bool
	Note           string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Gap is a hole in a broadcast's coverage, measured from the broadcast's
// start, not the recording's.
//
// The anchor is the broadcast because a late start is a gap that sits
// before the recording begins, and no offset from a recording's own start
// can express one. It attaches to the earliest recording of that
// broadcast, which is the row that survives however many reconnects
// followed.
//
// The same anchor is what makes a gap patchable: a stored copy's timeline
// starts when the broadcast did, so these offsets index straight into it.
type Gap struct {
	ID          int64
	RecordingID int64
	Start       time.Duration
	End         time.Duration
	Reason      string
	FilledAt    *time.Time
	// Attempts counts patches tried against this gap. The patcher holds no
	// claim, so this row is the only place a failure is remembered and the
	// only thing that stops a hole nothing can fill costing one download on
	// every pass for the life of the library.
	Attempts int
}

// State values.
const (
	// StateCapturing means the file is open and growing.
	StateCapturing State = "capturing"
	// StateAwaitingFinalize means capture finished and the recording has
	// not reached the library yet. It is set before finalizing, not after,
	// so a crash, a kill, or a failure part way through leaves a row that
	// says the work is unfinished.
	StateAwaitingFinalize State = "awaiting_finalize"
	// StateAwaitingMetadata means capture finished but a required naming
	// field is still missing, so the file keeps its capture name. This is
	// a holding state, never a failure: the recording is intact and the
	// organizer retries as metadata arrives.
	StateAwaitingMetadata State = "awaiting_metadata"
	// StateAwaitingFile means capture finished and the name is ready, but
	// another program holds the file so it cannot be moved. Like
	// StateAwaitingMetadata this is a holding state, never a failure: the
	// recording is intact and the organizer retries once the hold ends.
	StateAwaitingFile State = "awaiting_file"
	// StateComplete means the file is named, verified, and final.
	StateComplete State = "complete"
	// StateFailed means capture or post-processing gave up. Any bytes
	// written are still on disk.
	StateFailed State = "failed"
	// StateTrashed means the operator purged it and the grace period has
	// not expired.
	StateTrashed State = "trashed"
	// StateMissing means the row names a file that is not on the volume.
	// It is what an operator deleting recordings in a file manager leaves
	// behind, and the state that stops those bytes being counted against
	// the size cap forever.
	StateMissing State = "missing"
)

// Origin values.
const (
	// OriginLive is a capture of a broadcast as it happened. Its audio is
	// never muted, because platforms mute the stored copy afterward.
	OriginLive Origin = "live"
	// OriginRecovered is a download of a past broadcast from an archive.
	OriginRecovered Origin = "recovered"
	// OriginImported is a file already in the library that no row named,
	// read back from its own name because it carried no sidecar.
	//
	// It is separated from the other two because its metadata is a reading
	// rather than a record. A name states a title to the minute, in whatever
	// zone rendered it, after a sanitization that cannot be undone, so every
	// field on the row is a claim about the file rather than something the
	// recorder observed. Anything that treats a recording as evidence has to
	// be able to tell those apart.
	//
	// A file with a sidecar is not this. The sidecar carries the origin the
	// recording actually had, and restoring it is a rebuild rather than a
	// guess.
	OriginImported Origin = "imported"
)

// ///////////////////////////////////////////////
// Recordings
// ///////////////////////////////////////////////

// recordingSelect is the column list every recording query shares.
const recordingSelect = `
	SELECT id, channel_id, broadcast_id, path, state, origin, bytes, duration_ms,
	       media_duration_ms, muted_ms, started_at, ended_at, watched_at, recompressed_at,
	       pinned, refetchable, note, created_at, updated_at
	FROM recordings`

// PendingStates are the states a recording sits in between a finished
// capture and a place in the library. Every one of them is a holding state:
// the file is intact and the organizer has work left to do on it.
//
// The sweep queries exactly this set, so a state added here is retried
// without touching the daemon. A state left out of it is a recording
// nothing ever looks at again.
var PendingStates = []State{StateAwaitingFinalize, StateAwaitingMetadata, StateAwaitingFile}

// Valid reports whether a state is one of the known values.
func (s State) Valid() bool {
	switch s {
	case StateCapturing, StateAwaitingFinalize, StateAwaitingMetadata,
		StateAwaitingFile, StateComplete, StateFailed, StateTrashed, StateMissing:
		return true
	default:
		return false
	}
}

// OccupiesDisk reports whether a recording in this state still consumes
// space on the volume.
//
// It is what the size cap sums over, and it is wider than HoldsBytes on
// purpose: a failed capture's bytes and a trashed recording inside its undo
// window are both on the disk whether or not anything will play them. Only
// a file that is gone occupies nothing.
func OccupiesDisk(state State) bool {
	return state != StateMissing
}

// HoldsBytes reports whether a recording in this state has a file behind it.
//
// A failed or trashed recording holds nothing. It is one rule because two
// readers depend on it agreeing: the gap detector treats a recording that
// holds bytes as a boundary, and a hole where one that holds nothing sits is
// part of the surrounding recording rather than a hole between two.
func HoldsBytes(state State) bool {
	switch state {
	case StateComplete, StateCapturing,
		StateAwaitingFinalize, StateAwaitingMetadata, StateAwaitingFile:
		return true
	default:
		return false
	}
}

// Valid reports whether an origin is one of the known values.
func (o Origin) Valid() bool {
	return o == OriginLive || o == OriginRecovered || o == OriginImported
}

// CreateRecording registers a file that capture is about to write.
//
// It is called before any bytes exist, so a crash still leaves a row naming
// the file that was in flight.
func (s *Store) CreateRecording(r Recording) (Recording, error) {
	if !r.State.Valid() {
		return Recording{}, fmt.Errorf("recording state %q is not valid", r.State)
	}
	if !r.Origin.Valid() {
		return Recording{}, fmt.Errorf("recording origin %q is not valid", r.Origin)
	}
	stored, err := storablePath(r.Path)
	if err != nil {
		return Recording{}, err
	}
	r.Path = stored
	if err := requireMeasured(r.Bytes, r.Duration); err != nil {
		return Recording{}, fmt.Errorf("creating recording %s: %w", r.Path, err)
	}
	if err := requireStorable(r.StartedAt); err != nil {
		return Recording{}, fmt.Errorf("creating recording %s: %w", r.Path, err)
	}
	if err := requireStorablePtr(r.EndedAt, r.WatchedAt); err != nil {
		return Recording{}, fmt.Errorf("creating recording %s: %w", r.Path, err)
	}
	if r.EndedAt != nil && r.EndedAt.Before(r.StartedAt) {
		return Recording{}, fmt.Errorf("creating recording %s: it ends %s, before it started %s",
			r.Path, r.EndedAt.UTC().Format(time.RFC3339), r.StartedAt.UTC().Format(time.RFC3339))
	}

	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt = now, now

	result, err := s.db.Exec(`
		INSERT INTO recordings
			(channel_id, broadcast_id, path, state, origin, bytes, duration_ms,
			 started_at, ended_at, watched_at, pinned, refetchable, note, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ChannelID, r.BroadcastID, r.Path, string(r.State), string(r.Origin),
		r.Bytes, r.Duration.Milliseconds(), encodeTime(r.StartedAt), encodeTimePtr(r.EndedAt),
		encodeTimePtr(r.WatchedAt), r.Pinned, r.Refetchable, r.Note,
		encodeTime(r.CreatedAt), encodeTime(r.UpdatedAt))
	if err != nil {
		if isDuplicate(err) {
			return Recording{}, fmt.Errorf("creating recording %s: %w", r.Path, ErrDuplicatePath)
		}
		return Recording{}, fmt.Errorf("creating recording %s: %w", r.Path, err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Recording{}, fmt.Errorf("creating recording %s: %w", r.Path, err)
	}
	r.ID = id
	r.StartedAt = r.StartedAt.UTC()
	return r, nil
}

// Recording returns one recording by id.
func (s *Store) Recording(id int64) (Recording, error) {
	row := s.db.QueryRow(recordingSelect+` WHERE id = ?`, id)

	recording, err := scanRecording(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Recording{}, fmt.Errorf("recording %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Recording{}, fmt.Errorf("reading recording %d: %w", id, err)
	}
	return recording, nil
}

// FinishRecording records the outcome of a finished capture.
//
// The path is updated here because a recording is renamed only once its
// metadata validates, which happens after the bytes are on disk.
func (s *Store) FinishRecording(id int64, state State, path string, bytes int64, duration time.Duration, endedAt time.Time) error {
	if !state.Valid() {
		return fmt.Errorf("recording state %q is not valid", state)
	}
	path, err := storablePath(path)
	if err != nil {
		return err
	}
	if err := requireMeasured(bytes, duration); err != nil {
		return fmt.Errorf("finishing recording %d: %w", id, err)
	}
	if err := requireStorable(endedAt); err != nil {
		return fmt.Errorf("finishing recording %d: %w", id, err)
	}

	// Reading the start time and writing an end against it is one decision.
	// Run apart, a rename landing between them leaves a recording that
	// finished before it began, which every duration derived from the row
	// then reports as negative.
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("finishing recording %d: %w", id, err) // coverage:partial (Begin never fails on live SQLite)
	}
	defer tx.Rollback() // no-op after Commit

	var startedAt int64
	err = tx.QueryRow(`SELECT started_at FROM recordings WHERE id = ?`, id).Scan(&startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("recording %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("finishing recording %d: %w", id, err)
	}
	if encodeTime(endedAt) < startedAt {
		return fmt.Errorf("finishing recording %d: it ends %s, before it started %s",
			id, endedAt.UTC().Format(time.RFC3339), decodeTime(startedAt).Format(time.RFC3339))
	}

	result, err := tx.Exec(`
		UPDATE recordings
		SET state = ?, path = ?, bytes = ?, duration_ms = ?, ended_at = ?, updated_at = ?
		WHERE id = ?`,
		string(state), path, bytes, duration.Milliseconds(),
		encodeTime(endedAt), encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("finishing recording %d: %w", id, err)
	}
	if err := requireRow(result, id, "recording"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finishing recording %d: %w", id, err) // coverage:partial (Commit never fails on in-memory SQLite)
	}
	return nil
}

// SetState moves a recording to a new state.
func (s *Store) SetState(id int64, state State) error {
	if !state.Valid() {
		return fmt.Errorf("recording state %q is not valid", state)
	}

	result, err := s.db.Exec(`UPDATE recordings SET state = ?, updated_at = ? WHERE id = ?`,
		string(state), encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("setting state of recording %d: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// SetPath renames a recording, which the organizer does once a validated
// name is available.
func (s *Store) SetPath(id int64, path string) error {
	path, err := storablePath(path)
	if err != nil {
		return err
	}

	result, err := s.db.Exec(`UPDATE recordings SET path = ?, updated_at = ? WHERE id = ?`,
		path, encodeTime(time.Now()), id)
	if err != nil {
		// Classified rather than passed through raw. Another row already
		// naming this file is a state a caller can act on, and the organizer
		// treats a lost path as unrecoverable, so it has to be able to tell
		// that case from a broken database.
		if isDuplicate(err) {
			return fmt.Errorf("renaming recording %d to %s: %w", id, path, ErrDuplicatePath)
		}
		return fmt.Errorf("renaming recording %d: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// SetBytes records a recording's size on disk after a stage that changed
// it.
//
// The space budget measures max_size against the sum of this column, so a
// stage that shrinks a file and does not report it buys disk the budget
// never sees. Remux reclaims container overhead and a recompress can halve
// a recording, and neither frees a byte of headroom until the row agrees
// with the file.
func (s *Store) SetBytes(id int64, bytes int64) error {
	if err := requireMeasured(bytes, 0); err != nil {
		return fmt.Errorf("sizing recording %d: %w", id, err)
	}

	result, err := s.db.Exec(`UPDATE recordings SET bytes = ?, updated_at = ? WHERE id = ?`,
		bytes, encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("sizing recording %d: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// SetMediaDuration records how much broadcast a finished file actually
// holds.
//
// It is the only correction to a length that is otherwise wall clock taken
// around a subprocess. The gap detector compares the two, which is the only
// way content missing from inside one recording becomes visible at all, so a
// negative figure is refused rather than stored as a shortfall nobody can
// explain.
func (s *Store) SetMediaDuration(id int64, duration time.Duration) error {
	if duration < 0 {
		return fmt.Errorf("measuring recording %d: a media length of %s is negative", id, duration)
	}

	result, err := s.db.Exec(`UPDATE recordings SET media_duration_ms = ?, updated_at = ? WHERE id = ?`,
		duration.Milliseconds(), encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("measuring recording %d: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// SetBroadcastRecording attaches a recording to the broadcast it is a copy
// of, after the fact.
//
// Attaching later is what lets a caller create the recording first and only
// then reach for a broadcast. Creating the broadcast first leaves one behind
// with no capture when the recording is refused, and a broadcast with no
// capture reads as a broadcast nobody caught.
func (s *Store) SetBroadcastRecording(id, broadcastID int64) error {
	result, err := s.db.Exec(`UPDATE recordings SET broadcast_id = ?, updated_at = ? WHERE id = ?`,
		broadcastID, encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("attaching recording %d to broadcast %d: %w", id, broadcastID, err)
	}
	return requireRow(result, id, "recording")
}

// SetMutedDuration records how much of a recording the platform silenced.
//
// Only a copy fetched from an archive can carry one: a live capture holds
// the audio as it aired. The column stays null until something asks the
// platform, so this is never called to mean "none".
func (s *Store) SetMutedDuration(id int64, muted time.Duration) error {
	if muted < 0 {
		return fmt.Errorf("recording %d: a silenced length of %s is negative", id, muted)
	}

	result, err := s.db.Exec(`UPDATE recordings SET muted_ms = ?, updated_at = ? WHERE id = ?`,
		muted.Milliseconds(), encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("recording how much of recording %d is silenced: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// SetBroadcast links a recording to the broadcast it captured, or clears
// the link when broadcastID is nil.
//
// Capture starts before the broadcast is necessarily known, so the link is
// made afterward rather than at creation.
func (s *Store) SetBroadcast(id int64, broadcastID *int64) error {
	result, err := s.db.Exec(`UPDATE recordings SET broadcast_id = ?, updated_at = ? WHERE id = ?`,
		broadcastID, encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("linking recording %d to a broadcast: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// MarkWatched records that a recording was watched, which raises its purge
// score. Passing nil clears the mark.
func (s *Store) MarkWatched(id int64, at *time.Time) error {
	if err := requireStorablePtr(at); err != nil {
		return fmt.Errorf("marking recording %d watched: %w", id, err)
	}

	result, err := s.db.Exec(`UPDATE recordings SET watched_at = ?, updated_at = ? WHERE id = ?`,
		encodeTimePtr(at), encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("marking recording %d watched: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// SetPinned protects a recording from the purge list, or releases it.
func (s *Store) SetPinned(id int64, pinned bool) error {
	result, err := s.db.Exec(`UPDATE recordings SET pinned = ?, updated_at = ? WHERE id = ?`,
		pinned, encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("pinning recording %d: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// SetRefetchable records whether the broadcast still exists upstream. A
// recording that can be downloaded again is cheaper to delete than a sole
// surviving copy, and the purge score reflects that.
func (s *Store) SetRefetchable(id int64, refetchable bool) error {
	result, err := s.db.Exec(`UPDATE recordings SET refetchable = ?, updated_at = ? WHERE id = ?`,
		refetchable, encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("setting refetchable on recording %d: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// RecordingsByState returns every recording in any of the given states,
// oldest first. Passing no state returns nothing.
func (s *Store) RecordingsByState(states ...State) ([]Recording, error) {
	if len(states) == 0 {
		return nil, nil
	}

	args := make([]any, len(states))
	for i, state := range states {
		// The sweep asks for the states a recording can be stuck in. A typo
		// in that list would return an empty result and read as no work
		// pending, rather than as the mistake it is.
		if !state.Valid() {
			return nil, fmt.Errorf("recording state %q is not valid", state)
		}
		args[i] = string(state)
	}
	list := strings.Repeat(", ?", len(states)-1)
	recordings, _, err := s.queryRecordings(
		recordingSelect+` WHERE state IN (?`+list+`) ORDER BY started_at`, args...)
	return recordings, err
}

// RecompressCandidates returns finished recordings older than the cutoff
// that have never been re-encoded, oldest first.
//
// Only StateComplete qualifies. A recording the organizer is still working
// on has a file it is about to move, and re-encoding underneath that would
// leave the move pointing at bytes nothing verified.
//
// Oldest first is deliberate: the recompress rung exists to buy headroom,
// and the recordings least likely to be watched again pay for it.
func (s *Store) RecompressCandidates(before time.Time) ([]Recording, error) {
	if err := requireStorable(before); err != nil {
		return nil, fmt.Errorf("listing recompress candidates: %w", err)
	}

	// An imported recording is excluded. Its start comes from a filename and
	// is old by construction, so every one of them heads this queue the
	// moment it is adopted. Recompressing removes the original, and an
	// operator who adopts an archive is asking the library to record where
	// their files are, not to re-encode them.
	recordings, _, err := s.queryRecordings(recordingSelect+`
		WHERE state = ? AND origin != ? AND recompressed_at IS NULL AND started_at < ?
		ORDER BY started_at`, string(StateComplete), string(OriginImported), encodeTime(before))
	if err != nil {
		return nil, fmt.Errorf("listing recompress candidates: %w", err)
	}
	return recordings, nil
}

// MarkRecompressed records that a recording was re-encoded, and its new
// size.
//
// The two are written together because they are one fact. A mark without a
// size leaves the budget measuring the pre-encode file. A size without a
// mark offers the recording for re-encoding again next sweep, which spends
// hours to save nothing.
func (s *Store) MarkRecompressed(id int64, at time.Time, bytes int64) error {
	if err := requireStorable(at); err != nil {
		return fmt.Errorf("recording a recompress of %d: %w", id, err)
	}
	if err := requireMeasured(bytes, 0); err != nil {
		return fmt.Errorf("recording a recompress of %d: %w", id, err)
	}

	result, err := s.db.Exec(
		`UPDATE recordings SET recompressed_at = ?, bytes = ?, updated_at = ? WHERE id = ?`,
		encodeTime(at), bytes, encodeTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("recording a recompress of %d: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// TrashedBefore returns recordings purged before the cutoff, longest in
// the trash first.
//
// The purge time is read from updated_at, which is the moment the state
// became trashed: no sweep and no watcher looks at a trashed row, so
// nothing touches it again until it is released. The ordering is by that
// same column rather than by broadcast date, because the question here is
// which undo window has been open longest.
//
// That is an invariant nothing enforces. updated_at is general purpose,
// and any future write to a trashed row moves it, which restarts the
// operator's undo window. The direction is safe, since the window only
// ever extends, but a write added later would change the meaning of this
// query without touching it. A dedicated trashed_at column is what would
// make it impossible.
func (s *Store) TrashedBefore(cutoff time.Time) ([]Recording, error) {
	if err := requireStorable(cutoff); err != nil {
		return nil, fmt.Errorf("listing trashed recordings: %w", err)
	}

	recordings, _, err := s.queryRecordings(
		recordingSelect+` WHERE state = ? AND updated_at < ? ORDER BY updated_at`,
		string(StateTrashed), encodeTime(cutoff))
	if err != nil {
		return nil, fmt.Errorf("listing trashed recordings: %w", err)
	}
	return recordings, nil
}

// DeleteRecording removes a recording's row.
//
// It is the last step of releasing a purge, run once the file itself is
// gone. Called in that order, a failure here leaves a row naming a deleted
// file, which the operator can see and act on. The reverse order loses the
// only record that the file was ever there.
func (s *Store) DeleteRecording(id int64) error {
	result, err := s.db.Exec(`DELETE FROM recordings WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting recording %d: %w", id, err)
	}
	return requireRow(result, id, "recording")
}

// RecordingsForChannel returns a channel's recordings that started in a
// window, oldest first.
func (s *Store) RecordingsForChannel(channelID int64, from, to time.Time) ([]Recording, error) {
	recordings, _, err := s.recordingsForChannel(channelID, from, to)
	return recordings, err
}

// recordingsForChannel is RecordingsForChannel plus the number of rows it
// had to skip, which coverage needs to say a day is not fully accounted for.
func (s *Store) recordingsForChannel(channelID int64, from, to time.Time) ([]Recording, int, error) {
	if err := requireStorable(from, to); err != nil {
		return nil, 0, fmt.Errorf("listing recordings: %w", err)
	}

	return s.queryRecordings(recordingSelect+`
		WHERE channel_id = ? AND started_at >= ? AND started_at < ?
		ORDER BY started_at`, channelID, encodeTime(from), encodeTime(to))
}

// RecordingsForBroadcast returns every recording of one broadcast.
func (s *Store) RecordingsForBroadcast(broadcastID int64) ([]Recording, error) {
	recordings, _, err := s.queryRecordings(
		recordingSelect+` WHERE broadcast_id = ? ORDER BY started_at`, broadcastID)
	return recordings, err
}

// RecordingPaths returns every path a recording row names, in whatever
// state it is in.
//
// It answers the question "does anything own this file". A row in any state
// owns the file it names, including a failed one, whose bytes are still on
// disk and are the operator's to keep or delete.
func (s *Store) RecordingPaths() ([]string, error) {
	rows, err := s.db.Query(`SELECT path FROM recordings`)
	if err != nil {
		return nil, fmt.Errorf("listing recording paths: %w", err)
	}
	defer rows.Close()

	var recorded []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("reading a recording path: %w", err)
		}
		recorded = append(recorded, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing recording paths: %w", err)
	}
	return recorded, nil
}

// TotalBytes returns the bytes held by recordings that occupy disk.
//
// Trashed and failed rows are counted, because their files are still on the
// volume. A row whose file is gone is not, because the cap it feeds is a cap
// on the disk, and counting bytes nothing can free stops recording
// permanently for a reason the operator cannot see.
//
// The total walks the same scan every listing does rather than adding the
// column in SQL, so both see the same recordings. SUM counts a row no
// scanner can read, which leaves the space budget and the list the operator
// is shown disagreeing by exactly the rows neither can explain.
func (s *Store) TotalBytes() (int64, error) {
	recordings, _, err := s.queryRecordings(recordingSelect)
	if err != nil {
		return 0, fmt.Errorf("summing recording bytes: %w", err)
	}

	var total int64
	for _, recording := range recordings {
		if OccupiesDisk(recording.State) {
			total += recording.Bytes
		}
	}
	return total, nil
}

// queryRecordings runs a recording query, scanning every row it can and
// counting the rest.
func (s *Store) queryRecordings(query string, args ...any) ([]Recording, int, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("listing recordings: %w", err)
	}
	defer rows.Close()

	var (
		recordings []Recording
		skipped    int
	)
	for rows.Next() {
		recording, err := scanRecording(rows)
		if err != nil {
			// A row nothing can read is one lost recording. Failing the
			// query instead would lose every other recording with it, and
			// the sweep that retries pending work would see an empty list
			// and conclude there was nothing to do. The count travels back
			// so a caller can say its answer is short.
			skipped++
			s.logger().Warn("skipping unreadable recording row", "recording_id", recording.ID, "error", err)
			continue
		}
		recordings = append(recordings, recording)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing recordings: %w", err)
	}
	return recordings, skipped, nil
}

// scanRecording reads one recording row.
func scanRecording(row scanner) (Recording, error) {
	var (
		r              Recording
		broadcastID    sql.NullInt64
		state          string
		origin         string
		durationMS     int64
		mediaMS        int64
		startedAt      int64
		createdAt      int64
		updatedAt      int64
		endedAt        sql.NullInt64
		watchedAt      sql.NullInt64
		recompressedAt sql.NullInt64
		mutedMS        sql.NullInt64
	)
	if err := row.Scan(&r.ID, &r.ChannelID, &broadcastID, &r.Path, &state, &origin,
		&r.Bytes, &durationMS, &mediaMS, &mutedMS, &startedAt, &endedAt, &watchedAt, &recompressedAt,
		&r.Pinned, &r.Refetchable, &r.Note, &createdAt, &updatedAt); err != nil {
		// The id is the first column and columns are assigned in order, so
		// a row that fails on a later one still names itself. A zero id
		// says the id column was the unreadable one.
		return Recording{ID: r.ID}, err
	}

	if broadcastID.Valid {
		r.BroadcastID = &broadcastID.Int64
	}
	r.State = State(state)
	r.Origin = Origin(origin)
	r.Duration = time.Duration(durationMS) * time.Millisecond
	r.MediaDuration = time.Duration(mediaMS) * time.Millisecond
	if mutedMS.Valid {
		muted := time.Duration(mutedMS.Int64) * time.Millisecond
		r.MutedDuration = &muted
	}
	r.StartedAt = decodeTime(startedAt)
	r.CreatedAt = decodeTime(createdAt)
	r.UpdatedAt = decodeTime(updatedAt)
	r.EndedAt = decodeTimePtr(endedAt)
	r.WatchedAt = decodeTimePtr(watchedAt)
	r.RecompressedAt = decodeTimePtr(recompressedAt)
	return r, nil
}

// storablePath rejects an empty recording path and returns the one form a
// row is stored in.
//
// The path is the only link between a row and the file it describes, so
// clearing it strands the recording: nothing can find the bytes, and the
// sidecar rebuild has no path to match against. A second empty path then
// collides on the UNIQUE constraint, which is how the damage surfaces.
//
// Forward slashes are the one form both platforms read back, so a row
// written on one host names the same file on the other. filepath.ToSlash
// does nothing off Windows, which leaves a Linux name that legitimately
// carries a backslash intact.
func storablePath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("recording path is required")
	}
	// A stored path is what the purge later deletes and what the sidecar
	// rebuild writes beside, both resolved against the library root. One
	// that walks out of the root, or names a volume of its own, reaches a
	// file no part of this library owns. Every component is built from
	// remote metadata, so this is the last gate before that becomes durable.
	if !filepath.IsLocal(filepath.FromSlash(path)) {
		return "", fmt.Errorf("recording path %q resolves outside the library", path)
	}
	// Cleaned as well as checked, so one file has one spelling. IsLocal
	// cleans internally to answer, and returning the caller's spelling
	// instead lets "a/x.mp4", "a/./x.mp4" and "a/b/../x.mp4" become three
	// rows naming one file: UNIQUE(path) separates them, the size cap
	// counts the bytes three times, and asking whether anything owns a
	// file answers three times over.
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if cleaned == "." {
		return "", fmt.Errorf("recording path %q names no file", path)
	}
	return cleaned, nil
}

// requireMeasured rejects a negative size or length. No capture produces
// either, and both feed the space budget and the purge score as numbers
// those readers have no reason to doubt.
func requireMeasured(bytes int64, duration time.Duration) error {
	if bytes < 0 {
		return fmt.Errorf("recording bytes %d must not be negative", bytes)
	}
	if duration < 0 {
		return fmt.Errorf("recording duration %s must not be negative", duration)
	}
	return nil
}

// requireRow turns a no-op update into ErrNotFound, so a caller updating a
// deleted row learns about it and does not assume success.
func requireRow(result sql.Result, id int64, kind string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking updated %s %d: %w", kind, id, err)
	}
	if affected == 0 {
		return fmt.Errorf("%s %d: %w", kind, id, ErrNotFound)
	}
	return nil
}

// ///////////////////////////////////////////////
// Gaps
// ///////////////////////////////////////////////

// AddGap records a hole in a broadcast's coverage, measured from the
// broadcast's start. See Gap for why the anchor is the broadcast.
//
// Both bounds are validated and returned in the milliseconds they are stored
// in. Checking the nanosecond arguments instead accepts a gap that rounds
// away to nothing, and hands back a Gap the row does not agree with, which
// the backfill engine would then try to patch.
func (s *Store) AddGap(recordingID int64, start, end time.Duration, reason string) (Gap, error) {
	if start < 0 {
		return Gap{}, fmt.Errorf("gap start %s must not be negative", start)
	}
	startMS, endMS := start.Milliseconds(), end.Milliseconds()
	if endMS <= startMS {
		return Gap{}, fmt.Errorf("gap end %s must be after start %s, and to the millisecond it is not", end, start)
	}

	// Filing the same gap twice is the ordinary case, not the odd one: the
	// detector reads the recordings of a broadcast and re-derives every
	// hole in them each time it runs. The conflict target is the span, so
	// a second pass returns the row the first one wrote and the reason
	// recorded then survives.
	//
	// The no-op update is what makes RETURNING fire. DO NOTHING suppresses
	// the row entirely, so the caller gets no id back and cannot tell a
	// duplicate from a failure.
	var (
		id     int64
		stored string
	)
	err := s.db.QueryRow(`
		INSERT INTO gaps (recording_id, start_ms, end_ms, reason) VALUES (?, ?, ?, ?)
		ON CONFLICT (recording_id, start_ms, end_ms) DO UPDATE SET reason = reason
		RETURNING id, reason`,
		recordingID, startMS, endMS, reason).Scan(&id, &stored)
	if err != nil {
		return Gap{}, fmt.Errorf("recording gap in %d: %w", recordingID, err)
	}

	return Gap{
		ID:          id,
		RecordingID: recordingID,
		Start:       time.Duration(startMS) * time.Millisecond,
		End:         time.Duration(endMS) * time.Millisecond,
		Reason:      stored,
	}, nil
}

// FillGap marks a gap as patched from an archive.
func (s *Store) FillGap(id int64, at time.Time) error {
	if err := requireStorable(at); err != nil {
		return fmt.Errorf("filling gap %d: %w", id, err)
	}

	result, err := s.db.Exec(`UPDATE gaps SET filled_at = ? WHERE id = ?`, encodeTime(at), id)
	if err != nil {
		return fmt.Errorf("filling gap %d: %w", id, err)
	}
	return requireRow(result, id, "gap")
}

// ChargeGap records that a patch of this gap failed.
//
// The patcher takes no claim, so nothing else remembers. Without this a hole
// whose source was deleted from the platform costs one download of its whole
// range on every pass, forever, and the only trace is a warning.
//
// Passing terminal jumps straight to the cap, because a refusal a timer
// cannot change is not worth the remaining attempts.
func (s *Store) ChargeGap(id int64, limit int, terminal bool) error {
	var result sql.Result
	var err error
	if terminal {
		result, err = s.db.Exec(`UPDATE gaps SET attempts = ? WHERE id = ?`, limit, id)
	} else {
		result, err = s.db.Exec(`UPDATE gaps SET attempts = attempts + 1 WHERE id = ?`, id)
	}
	if err != nil {
		return fmt.Errorf("charging gap %d: %w", id, err)
	}
	// A charge against a row that is not there remembers nothing, and the
	// patcher reads a successful charge as the attempt being remembered.
	// Reported so the same whole-range download is not spent again on
	// every pass, which is the cost this row exists to stop.
	return requireRow(result, id, "gap")
}

// openGapRecordings reports which of a channel's recordings in a window
// still hold a stretch nothing has filled.
//
// It answers per recording rather than per gap, because coverage asks only
// whether anything is missing. One query rather than one per recording: a
// month of daily broadcasts would otherwise cost thirty round trips to
// paint one grid.
//
// It returns the rows it could not read alongside the answer, which is what
// lets a day say its tally may be short rather than quietly claim to be
// whole.
func (s *Store) openGapRecordings(channelID int64, from, to time.Time) (map[int64]bool, int, error) {
	if err := requireStorable(from, to); err != nil {
		return nil, 0, fmt.Errorf("listing open gaps: %w", err)
	}

	rows, err := s.db.Query(`
		SELECT DISTINCT gaps.recording_id
		FROM gaps
		JOIN recordings ON recordings.id = gaps.recording_id
		WHERE recordings.channel_id = ?
		  AND recordings.started_at >= ? AND recordings.started_at < ?
		  AND gaps.filled_at IS NULL`,
		channelID, encodeTime(from), encodeTime(to))
	if err != nil {
		return nil, 0, fmt.Errorf("listing open gaps: %w", err)
	}
	defer rows.Close()

	holed := make(map[int64]bool)
	skipped := 0
	for rows.Next() {
		var recordingID int64
		if err := rows.Scan(&recordingID); err != nil {
			s.logger().Warn("skipping unreadable open gap row",
				"channel_id", channelID, "error", err)
			skipped++
			continue
		}
		holed[recordingID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing open gaps: %w", err)
	}
	return holed, skipped, nil
}

// Gaps returns every hole filed against a recording, ordered by where it
// starts in the broadcast.
//
// The order is the reading order: a hole at the start of a broadcast and
// one at the end mean different things, and a list sorted any other way
// makes a reader work out which is which.
func (s *Store) Gaps(recordingID int64) ([]Gap, error) {
	rows, err := s.db.Query(`
		SELECT id, recording_id, start_ms, end_ms, reason, filled_at, attempts
		FROM gaps WHERE recording_id = ? ORDER BY start_ms`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("listing gaps: %w", err)
	}
	defer rows.Close()

	var gaps []Gap
	for rows.Next() {
		var (
			gap      Gap
			startMS  int64
			endMS    int64
			filledAt sql.NullInt64
		)
		if err := rows.Scan(&gap.ID, &gap.RecordingID, &startMS, &endMS, &gap.Reason,
			&filledAt, &gap.Attempts); err != nil {
			s.logger().Warn("skipping unreadable gap row",
				"recording_id", recordingID, "gap_id", gap.ID, "error", err)
			continue
		}
		gap.Start = time.Duration(startMS) * time.Millisecond
		gap.End = time.Duration(endMS) * time.Millisecond
		gap.FilledAt = decodeTimePtr(filledAt)
		gaps = append(gaps, gap)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing gaps: %w", err)
	}
	return gaps, nil
}
