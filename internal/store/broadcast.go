package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Source records how a broadcast became known, which decides how much its
// timing can be trusted.
type Source string

// Broadcast is one live session, whether or not it was recorded.
type Broadcast struct {
	ID        int64
	ChannelID int64
	// StreamID names the live session, which is what the recorder sees while
	// the channel is on air.
	StreamID string
	// RemoteID names the stored copy, which is what the archive publishes
	// afterward. It is a different namespace from StreamID and the two never
	// collide.
	RemoteID string
	// URL is where the stored copy is fetched from. It is empty until a
	// listing reports one, which a live-observed broadcast has to wait for.
	URL       string
	StartedAt time.Time
	EndedAt   *time.Time
	// VodStartedAt is where the stored copy's own timeline begins, which is
	// what a download range is indexed from. It is kept apart from StartedAt
	// because that moves as better sources describe the broadcast, while the
	// stored copy's t=0 is fixed by whoever recorded it. Nil means nobody has
	// reported it.
	VodStartedAt *time.Time
	// Muted is the stretches of the stored copy the platform silenced,
	// against that copy's own timeline.
	//
	// Nil means nobody could ask, which is what a machine with no platform
	// session has. A non-nil empty list means the platform answered and
	// silenced nothing. The distinction is the whole value of the field: only
	// the second one licenses filling a hole from this copy without asking
	// anything further.
	Muted        []MutedSpan
	Title        string
	Category     string
	Source       Source
	DiscoveredAt time.Time
}

// MutedSpan is one stretch of a stored copy the platform silenced.
//
// Playback serves silence for these, so a patch taken the ordinary way fills
// a hole with nothing. Whether the audio as broadcast survives beside it
// depends on how the copy was stored, and that is a question for the
// platform's own package. This is what says the question is worth asking.
type MutedSpan struct {
	// Offset is where the stretch begins in the stored copy.
	Offset time.Duration
	// Duration is how long it runs.
	Duration time.Duration
}

// TitleObservation is one reading of a broadcast's title and category.
type TitleObservation struct {
	ID          int64
	BroadcastID int64
	ObservedAt  time.Time
	Title       string
	Category    string
}

// storedMutedSpan is how one muted stretch is written to the column.
//
// Whole milliseconds rather than a duration string, so the column reads the
// same way every other stored interval does and no locale or unit spelling
// can change its meaning.
type storedMutedSpan struct {
	OffsetMS   int64 `json:"offset_ms"`
	DurationMS int64 `json:"duration_ms"`
}

// Source values, in descending order of precision.
const (
	// SourceLive means stream-dvr watched the broadcast happen, so the
	// start time is exact.
	SourceLive Source = "live"
	// SourceAPI means the platform reported it, usually as a VOD. The
	// start time is close but can differ by seconds.
	SourceAPI Source = "api"
	// SourceTracker means a third-party site reported it. It may be the
	// only evidence a broadcast existed, and its timing is approximate.
	SourceTracker Source = "tracker"
)

// overlapWindow bounds how far apart two start times can be and still
// describe the same broadcast. Trackers round to the minute and a VOD's
// recorded start can trail the live one, so exact matching would file one
// broadcast twice and report a phantom gap.
const overlapWindow = 15 * time.Minute

// maxMutedMS bounds an offset or a length in a stored silenced stretch.
//
// A year in milliseconds, which no broadcast approaches and which leaves the
// conversion to nanoseconds four orders of magnitude short of overflowing.
// The bound is for the arithmetic rather than for the platform.
const maxMutedMS = int64(365 * 24 * 60 * 60 * 1000)

// broadcastSelect is the column list every broadcast query shares.
const broadcastSelect = `
	SELECT id, channel_id, stream_id, remote_id, url, started_at, ended_at,
	       vod_started_at, muted_spans, title, category, source, discovered_at
	FROM broadcasts`

// Valid reports whether a source is one of the known values.
func (s Source) Valid() bool {
	switch s {
	case SourceLive, SourceAPI, SourceTracker:
		return true
	default:
		return false
	}
}

// ///////////////////////////////////////////////
// Broadcasts
// ///////////////////////////////////////////////

// UpsertBroadcast records a broadcast, merging it with an existing row that
// describes the same session.
//
// A broadcast can be discovered more than once: live while recording, again
// from the platform's VOD list, and again from a tracker. Matching is by
// remote identifier when both rows carry one, and otherwise by a start time
// inside overlapWindow, so the three discoveries collapse into one row
// rather than three and the calendar does not invent gaps. Two identifiers
// that disagree are never merged.
//
// A more precise source upgrades an existing row's timing and metadata. A
// less precise one only fills blanks.
func (s *Store) UpsertBroadcast(b Broadcast) (Broadcast, error) {
	if !b.Source.Valid() {
		return Broadcast{}, fmt.Errorf("broadcast source %q is not one of live, api, tracker", b.Source)
	}
	if b.DiscoveredAt.IsZero() {
		b.DiscoveredAt = time.Now().UTC()
	}
	if err := requireStorable(b.StartedAt, b.DiscoveredAt); err != nil {
		return Broadcast{}, fmt.Errorf("upserting broadcast: %w", err)
	}
	if err := requireStorablePtr(b.EndedAt); err != nil {
		return Broadcast{}, fmt.Errorf("upserting broadcast: %w", err)
	}

	// The match and the write that follows it are one decision. Run apart,
	// two discoveries of the same broadcast can both find nothing and both
	// insert, which is how one session becomes several rows and the
	// calendar grows broadcasts that never happened.
	tx, err := s.db.Begin()
	if err != nil {
		return Broadcast{}, fmt.Errorf("upserting broadcast: %w", err) // coverage:partial (Begin never fails on live SQLite)
	}
	defer tx.Rollback() // no-op after Commit

	existing, matchErr := matchBroadcast(tx, b)
	var result Broadcast
	switch {
	case errors.Is(matchErr, ErrNotFound):
		result, err = insertBroadcast(tx, b)
	case matchErr != nil:
		return Broadcast{}, matchErr
	default:
		result, err = mergeBroadcast(tx, existing, b)
	}
	if err != nil {
		return Broadcast{}, err
	}

	if err := tx.Commit(); err != nil {
		return Broadcast{}, fmt.Errorf("upserting broadcast: %w", err) // coverage:partial (Commit never fails on in-memory SQLite)
	}
	return result, nil
}

// matchBroadcast finds the row describing the same session as b.
//
// The stream id is asked first. It is the identifier a live capture carries
// and the one Twitch repeats on the stored copy, so it is the only thing
// that can reunite a row the recorder opened with the archive listing of the
// same broadcast.
func matchBroadcast(q querier, b Broadcast) (Broadcast, error) {
	if found, err := matchByColumn(q, "stream_id", b.ChannelID, b.StreamID); err == nil {
		return found, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Broadcast{}, err
	}
	if found, err := matchByColumn(q, "remote_id", b.ChannelID, b.RemoteID); err == nil {
		return found, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Broadcast{}, err
	}

	// Nanosecond integers subtract directly, so the nearest candidate is an
	// exact comparison rather than a conversion to Julian days and back.
	//
	// Overlap never crosses two identifiers that disagree, and each one is
	// compared only against its own kind: a stream id and a video id name
	// different things, so a disagreement between them is not evidence of
	// anything. A discovery carrying an identifier may claim a row that has
	// none, which is how a tracker's approximate row gains its VOD id, but
	// two identifiers of one kind name two broadcasts. A channel that drops
	// and restarts inside the window produces exactly that pair, and merging
	// them discards the second title, flips the identity to whichever
	// discovery landed last, and reports one broadcast on a day that held
	// two.
	low, high := overlapBounds(b.StartedAt)
	row := q.QueryRow(broadcastSelect+`
		WHERE channel_id = ? AND started_at BETWEEN ? AND ?
			AND (? = '' OR remote_id = '' OR remote_id = ?)
			AND (? = '' OR stream_id = '' OR stream_id = ?)
		ORDER BY ABS(started_at - ?) LIMIT 1`,
		b.ChannelID, low, high, b.RemoteID, b.RemoteID, b.StreamID, b.StreamID,
		encodeTime(b.StartedAt))

	found, err := scanBroadcast(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Broadcast{}, ErrNotFound
	}
	if err != nil {
		return Broadcast{}, fmt.Errorf("matching broadcast by start time: %w", err)
	}
	return found, nil
}

// matchByColumn finds a broadcast by one of its identifier columns.
//
// An empty value matches nothing rather than every row that has none, which
// is why it answers ErrNotFound before asking. The column name is a literal
// from this file's two call sites and never comes from a caller.
func matchByColumn(q querier, column string, channelID int64, value string) (Broadcast, error) {
	if value == "" {
		return Broadcast{}, ErrNotFound
	}

	row := q.QueryRow(broadcastSelect+`
		WHERE channel_id = ? AND `+column+` = ?`, channelID, value)
	found, err := scanBroadcast(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Broadcast{}, ErrNotFound
	}
	if err != nil {
		return Broadcast{}, fmt.Errorf("matching broadcast by %s: %w", column, err)
	}
	return found, nil
}

// overlapBounds returns the window around a start time that can hold the
// same broadcast, clamped to what Unix nanoseconds can name.
//
// A timestamp within overlapWindow of either bound wraps when the window is
// added, leaving a low bound above the high one. BETWEEN then matches
// nothing and deduplication is silently off.
func overlapBounds(t time.Time) (int64, int64) {
	center := encodeTime(t)

	low := center - int64(overlapWindow)
	if low > center {
		low = math.MinInt64
	}
	high := center + int64(overlapWindow)
	if high < center {
		high = math.MaxInt64
	}
	return low, high
}

// insertBroadcast writes a new broadcast row.
func insertBroadcast(q querier, b Broadcast) (Broadcast, error) {
	muted, err := encodeMutedSpans(b.Muted)
	if err != nil {
		return Broadcast{}, err
	}

	result, err := q.Exec(`
		INSERT INTO broadcasts
			(channel_id, stream_id, remote_id, url, started_at, ended_at,
			 vod_started_at, muted_spans, title, category, source, discovered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ChannelID, b.StreamID, b.RemoteID, b.URL,
		encodeTime(b.StartedAt), encodeTimePtr(b.EndedAt), encodeTimePtr(b.VodStartedAt),
		muted, b.Title, b.Category, string(b.Source), encodeTime(b.DiscoveredAt))
	if err != nil {
		return Broadcast{}, fmt.Errorf("inserting broadcast: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return Broadcast{}, fmt.Errorf("inserting broadcast: %w", err)
	}
	b.ID = id
	b.StartedAt = b.StartedAt.UTC()
	b.DiscoveredAt = b.DiscoveredAt.UTC()
	return b, nil
}

// mergeTiming folds an incoming discovery's timing into merged.
//
// upgrade says the incoming source outranks the one already stored, which is
// what lets it replace a reading rather than only fill a blank.
func mergeTiming(merged *Broadcast, incoming Broadcast, upgrade bool) {
	if upgrade {
		merged.Source = incoming.Source
		merged.StartedAt = incoming.StartedAt.UTC()
	}
	// THE RULE IS DIRECTIONAL, AND THAT IS WHAT MAKES IT SAFE. A recorder
	// cannot observe a broadcast before it begins, so a live-observed start
	// is always at or after the true one. An earlier start from a source that
	// reports a real timestamp is therefore better information about the same
	// broadcast, and it is the only route to the true start of a broadcast the
	// daemon joined part way through.
	//
	// Later never moves earlier, because a later reading proves nothing about
	// when the broadcast began. SourceTracker is excluded outright: it rounds
	// to the minute, so its "earlier" is an artefact of the rounding rather
	// than a measurement.
	if incoming.Source == SourceAPI && incoming.StartedAt.Before(merged.StartedAt) {
		merged.StartedAt = incoming.StartedAt.UTC()
	}
	if incoming.EndedAt != nil && (upgrade || merged.EndedAt == nil) {
		merged.EndedAt = incoming.EndedAt
	}
	// Only a source describing the stored copy reports where its timeline
	// begins, and the newest such reading describes the copy that exists now,
	// so it takes precedence over an older one whatever the sources rank.
	if incoming.VodStartedAt != nil {
		merged.VodStartedAt = incoming.VodStartedAt
	}
}

// mergeDescription folds an incoming discovery's identifiers and text into
// merged.
func mergeDescription(merged *Broadcast, incoming Broadcast, upgrade bool) {
	if incoming.StreamID != "" {
		merged.StreamID = incoming.StreamID
	}
	if incoming.RemoteID != "" {
		merged.RemoteID = incoming.RemoteID
	}
	// A live-observed broadcast carries no address at all, so filling a blank
	// whatever the source is what makes it fetchable once a listing reports
	// one.
	if incoming.URL != "" && (upgrade || merged.URL == "") {
		merged.URL = incoming.URL
	}
	if incoming.Title != "" && (upgrade || merged.Title == "") {
		merged.Title = incoming.Title
	}
	if incoming.Category != "" && (upgrade || merged.Category == "") {
		merged.Category = incoming.Category
	}
	// A discovery that could not ask says nothing about what is muted, so it
	// must not erase an answer another source gave.
	if incoming.Muted != nil {
		merged.Muted = incoming.Muted
	}
}

// mergeBroadcast folds a new discovery into an existing row.
func mergeBroadcast(q querier, existing, incoming Broadcast) (Broadcast, error) {
	upgrade := precedence(incoming.Source) > precedence(existing.Source)

	merged := existing
	mergeTiming(&merged, incoming, upgrade)
	mergeDescription(&merged, incoming, upgrade)

	muted, err := encodeMutedSpans(merged.Muted)
	if err != nil {
		return Broadcast{}, err
	}

	if _, err := q.Exec(`
		UPDATE broadcasts
		SET stream_id = ?, remote_id = ?, url = ?, started_at = ?, ended_at = ?,
			vod_started_at = ?, muted_spans = ?, title = ?, category = ?, source = ?
		WHERE id = ?`,
		merged.StreamID, merged.RemoteID, merged.URL,
		encodeTime(merged.StartedAt), encodeTimePtr(merged.EndedAt),
		encodeTimePtr(merged.VodStartedAt), muted,
		merged.Title, merged.Category, string(merged.Source), merged.ID); err != nil {
		return Broadcast{}, fmt.Errorf("merging broadcast %d: %w", merged.ID, err)
	}
	return merged, nil
}

// precedence ranks how much a source's timing can be trusted.
func precedence(s Source) int {
	switch s {
	case SourceLive:
		return 3
	case SourceAPI:
		return 2
	case SourceTracker:
		return 1
	default:
		return 0
	}
}

// SetBroadcastMetadata records a broadcast's current title and category.
//
// This is what the live poller calls, and it deliberately overwrites rather
// than merging. UpsertBroadcast's precedence rules exist to stop a weaker
// source displacing a stronger one, but here the source is the same live
// observation that set the value in the first place, and the newer reading
// is simply more current. A blank reading is ignored, so a failed poll
// cannot erase a title that is already known.
func (s *Store) SetBroadcastMetadata(id int64, title, category string) error {
	if title == "" && category == "" {
		return nil
	}

	result, err := s.db.Exec(`
		UPDATE broadcasts SET
			title = CASE WHEN ? <> '' THEN ? ELSE title END,
			category = CASE WHEN ? <> '' THEN ? ELSE category END
		WHERE id = ?`, title, title, category, category, id)
	if err != nil {
		return fmt.Errorf("updating metadata for broadcast %d: %w", id, err)
	}
	return requireRow(result, id, "broadcast")
}

// SetBroadcastEnd records when a broadcast stopped.
//
// The end is what every rule that waits for a finished broadcast reads: the
// settle window, gap detection, and the coverage a day is painted from. An
// end before the row's start is refused rather than stored, the way
// FinishRecording refuses one, because each of those reads the difference as
// a length and none of them has a reason to doubt it.
func (s *Store) SetBroadcastEnd(id int64, endedAt time.Time) error {
	if err := requireStorable(endedAt); err != nil {
		return fmt.Errorf("ending broadcast %d: %w", id, err)
	}

	// Reading the start and writing an end against it is one decision. Run
	// apart, a merge that moves the start lands between them and leaves a
	// broadcast that ended before it began.
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("ending broadcast %d: %w", id, err) // coverage:partial (Begin never fails on live SQLite)
	}
	defer tx.Rollback() // no-op after Commit

	var startedAt int64
	err = tx.QueryRow(`SELECT started_at FROM broadcasts WHERE id = ?`, id).Scan(&startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("broadcast %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("ending broadcast %d: %w", id, err)
	}
	if encodeTime(endedAt) < startedAt {
		return fmt.Errorf("ending broadcast %d: it ends %s, before it started %s",
			id, endedAt.UTC().Format(time.RFC3339), decodeTime(startedAt).Format(time.RFC3339))
	}

	result, err := tx.Exec(`UPDATE broadcasts SET ended_at = ? WHERE id = ?`,
		encodeTime(endedAt), id)
	if err != nil {
		return fmt.Errorf("ending broadcast %d: %w", id, err)
	}
	if err := requireRow(result, id, "broadcast"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ending broadcast %d: %w", id, err) // coverage:partial (Commit never fails on in-memory SQLite)
	}
	return nil
}

// BroadcastByRemoteID returns the broadcast a channel already has under one
// archive identifier, or ErrNotFound.
//
// It finds, and never creates. An import uses it to attach a restored
// recording to a broadcast this library already discovered, which is what
// stops a recovery pass fetching a copy of a file already on the disk.
// Creating the row instead would have the calendar expect a capture of a
// broadcast nothing here observed, on the word of a file somebody put in a
// directory.
func (s *Store) BroadcastByRemoteID(channelID int64, remoteID string) (Broadcast, error) {
	found, err := matchByColumn(s.db, "remote_id", channelID, remoteID)
	if err != nil {
		return Broadcast{}, err
	}
	return found, nil
}

// Broadcast returns one broadcast by id.
func (s *Store) Broadcast(id int64) (Broadcast, error) {
	row := s.db.QueryRow(broadcastSelect+` WHERE id = ?`, id)

	broadcast, err := scanBroadcast(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Broadcast{}, fmt.Errorf("broadcast %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Broadcast{}, fmt.Errorf("reading broadcast %d: %w", id, err)
	}
	return broadcast, nil
}

// BroadcastsBetween returns a channel's broadcasts that started in a window,
// oldest first.
func (s *Store) BroadcastsBetween(channelID int64, from, to time.Time) ([]Broadcast, error) {
	broadcasts, _, err := s.broadcastsBetween(channelID, from, to)
	return broadcasts, err
}

// broadcastsBetween is BroadcastsBetween plus the number of rows it had to
// skip, which coverage needs to say a day is not fully accounted for.
func (s *Store) broadcastsBetween(channelID int64, from, to time.Time) ([]Broadcast, int, error) {
	if err := requireStorable(from, to); err != nil {
		return nil, 0, fmt.Errorf("listing broadcasts: %w", err)
	}

	rows, err := s.db.Query(broadcastSelect+`
		WHERE channel_id = ? AND started_at >= ? AND started_at < ?
		ORDER BY started_at`, channelID, encodeTime(from), encodeTime(to))
	if err != nil {
		return nil, 0, fmt.Errorf("listing broadcasts: %w", err)
	}
	defer rows.Close()

	var (
		broadcasts []Broadcast
		skipped    int
	)
	for rows.Next() {
		broadcast, err := scanBroadcast(rows)
		if err != nil {
			// One unreadable row must not blank the whole window. A
			// calendar missing a broadcast it cannot read is wrong about
			// that day; a calendar missing every broadcast is wrong about
			// the month, and looks like a quiet one.
			skipped++
			s.logger().Warn("skipping unreadable broadcast row",
				"channel_id", channelID, "broadcast_id", broadcast.ID, "error", err)
			continue
		}
		broadcasts = append(broadcasts, broadcast)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing broadcasts: %w", err)
	}
	return broadcasts, skipped, nil
}

// scanBroadcast reads one broadcast row.
func scanBroadcast(row scanner) (Broadcast, error) {
	var (
		b            Broadcast
		source       string
		startedAt    int64
		discoveredAt int64
		endedAt      sql.NullInt64
		vodStartedAt sql.NullInt64
		muted        sql.NullString
	)
	if err := row.Scan(&b.ID, &b.ChannelID, &b.StreamID, &b.RemoteID, &b.URL,
		&startedAt, &endedAt, &vodStartedAt, &muted,
		&b.Title, &b.Category, &source, &discoveredAt); err != nil {
		// The id is the first column and columns are assigned in order, so
		// a row that fails on a later one still names itself.
		return Broadcast{ID: b.ID}, err
	}

	spans, err := decodeMutedSpans(muted)
	if err != nil {
		return Broadcast{ID: b.ID}, err
	}

	b.StartedAt = decodeTime(startedAt)
	b.DiscoveredAt = decodeTime(discoveredAt)
	b.EndedAt = decodeTimePtr(endedAt)
	b.VodStartedAt = decodeTimePtr(vodStartedAt)
	b.Muted = spans
	b.Source = Source(source)
	return b, nil
}

// encodeMutedSpans renders the platform's answer for storage.
//
// Nil answers a NULL column, which is what "nobody could ask" means. An
// empty list is stored as an empty list, because a platform that answered
// and silenced nothing is telling the patcher something a missing answer
// cannot.
func encodeMutedSpans(spans []MutedSpan) (*string, error) {
	if spans == nil {
		return nil, nil
	}

	// Bounded going in as well as coming out. These arrive from a
	// platform's listing, and a span the reader will refuse is a row that
	// cannot be read back at all: the write is where there is still a
	// caller to tell.
	stored := make([]storedMutedSpan, 0, len(spans))
	for _, span := range spans {
		offset := span.Offset.Milliseconds()
		duration := span.Duration.Milliseconds()
		if offset < 0 || duration <= 0 || offset > maxMutedMS || duration > maxMutedMS {
			return nil, fmt.Errorf("muted span at %s for %s is outside what a span may hold",
				span.Offset, span.Duration)
		}
		stored = append(stored, storedMutedSpan{
			OffsetMS:   offset,
			DurationMS: duration,
		})
	}

	encoded, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encoding the muted spans: %w", err) // coverage:partial (a slice of integers always encodes)
	}
	text := string(encoded)
	return &text, nil
}

// decodeMutedSpans reads the platform's answer back, keeping the difference
// between an absent answer and an empty one.
func decodeMutedSpans(column sql.NullString) ([]MutedSpan, error) {
	if !column.Valid {
		return nil, nil
	}

	var stored []storedMutedSpan
	if err := json.Unmarshal([]byte(column.String), &stored); err != nil {
		return nil, fmt.Errorf("reading the muted spans: %w", err)
	}

	spans := make([]MutedSpan, 0, len(stored))
	for _, span := range stored {
		// Bounded here because this is where a reader gets its spans, and a
		// row written by any earlier build reaches it unchecked. Milliseconds
		// multiplied into nanoseconds overflows well inside what the column
		// can hold, and a span whose end wraps negative reads as covering
		// nothing: every guard against patching silenced audio then passes,
		// and the hole is filled from the copy that serves silence and marked
		// done for good.
		//
		// Refused rather than skipped, because the empty list is not the
		// absence of an answer here: null means nobody could ask and an
		// empty list means the platform answered and silenced nothing,
		// and only the second licenses patching a hole from that copy.
		// Thinning a list down to nothing would turn a row this build
		// cannot read into a licence to fill.
		if span.OffsetMS < 0 || span.DurationMS <= 0 ||
			span.OffsetMS > maxMutedMS || span.DurationMS > maxMutedMS {
			return nil, fmt.Errorf("muted span at %dms for %dms is outside what a span may hold",
				span.OffsetMS, span.DurationMS)
		}
		spans = append(spans, MutedSpan{
			Offset:   time.Duration(span.OffsetMS) * time.Millisecond,
			Duration: time.Duration(span.DurationMS) * time.Millisecond,
		})
	}
	return spans, nil
}

// ///////////////////////////////////////////////
// Title history
// ///////////////////////////////////////////////

// ObserveTitle appends a title reading for a broadcast, skipping a value
// identical to the most recent one.
//
// The poller runs on a fixed interval, so most readings repeat. Storing only
// changes keeps the history a record of what the broadcaster did rather than
// a log of how often the poller asked.
func (s *Store) ObserveTitle(broadcastID int64, at time.Time, title, category string) error {
	if err := requireStorable(at); err != nil {
		return fmt.Errorf("recording title for broadcast %d: %w", broadcastID, err)
	}

	// Reading the last observation and appending to it is one decision.
	// Run apart, two pollers reporting the same unchanged title both see no
	// match and both append.
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("recording title for broadcast %d: %w", broadcastID, err) // coverage:partial (Begin never fails on live SQLite)
	}
	defer tx.Rollback() // no-op after Commit

	// The comparison is against the observation this one follows, not the
	// newest one on record. Backfill feeds history out of order, and
	// comparing against a later reading stores a repeat next to its own
	// twin: A at 10:00, B at 11:00, then A at 10:30 leaves A recorded twice
	// in a row.
	var (
		lastTitle    string
		lastCategory string
	)
	err = tx.QueryRow(`
		SELECT title, category FROM title_history
		WHERE broadcast_id = ? AND observed_at <= ?
		ORDER BY observed_at DESC, id DESC LIMIT 1`,
		broadcastID, encodeTime(at)).Scan(&lastTitle, &lastCategory)

	switch {
	case err == nil:
		if lastTitle == title && lastCategory == category {
			return nil
		}
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("reading last title for broadcast %d: %w", broadcastID, err)
	}

	if _, err := tx.Exec(`
		INSERT INTO title_history (broadcast_id, observed_at, title, category)
		VALUES (?, ?, ?, ?)`, broadcastID, encodeTime(at), title, category); err != nil {
		return fmt.Errorf("recording title for broadcast %d: %w", broadcastID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording title for broadcast %d: %w", broadcastID, err) // coverage:partial (Commit never fails on in-memory SQLite)
	}
	return nil
}

// TitleHistory returns every title reading for a broadcast, oldest first.
func (s *Store) TitleHistory(broadcastID int64) ([]TitleObservation, error) {
	rows, err := s.db.Query(`
		SELECT id, broadcast_id, observed_at, title, category
		FROM title_history WHERE broadcast_id = ? ORDER BY observed_at, id`, broadcastID)
	if err != nil {
		return nil, fmt.Errorf("listing title history: %w", err)
	}
	defer rows.Close()

	var observations []TitleObservation
	for rows.Next() {
		var (
			observation TitleObservation
			observedAt  int64
		)
		if err := rows.Scan(&observation.ID, &observation.BroadcastID, &observedAt,
			&observation.Title, &observation.Category); err != nil {
			s.logger().Warn("skipping unreadable title observation",
				"broadcast_id", broadcastID, "observation_id", observation.ID, "error", err)
			continue
		}
		observation.ObservedAt = decodeTime(observedAt)
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing title history: %w", err)
	}
	return observations, nil
}
