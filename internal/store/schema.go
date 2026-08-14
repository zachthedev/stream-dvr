package store

import (
	"database/sql"
	"fmt"
	"strings"

	"zach.tools/go/stream-dvr/internal/migrate"
)

// ///////////////////////////////////////////////
// Schema
// ///////////////////////////////////////////////

// schemaVersion is the schema this build produces.
const schemaVersion = 1

// registry owns the library database's schema. initSchema creates the whole
// shape in one step, so the registry holds no migration.
var registry = newRegistry()

// readableViews names the nanosecond columns each table's view renders, as
// the schema stands now.
//
// A view carries its table's own columns through with *, so a new column
// needs an entry here only when it holds a timestamp. ensureViews rebuilds
// every view from this list after the migrations run, so it describes the
// current schema rather than the one initSchema creates.
var readableViews = []struct {
	table   string
	columns []string
}{
	{"channels", []string{"created_at"}},
	{"broadcasts", []string{"started_at", "ended_at", "vod_started_at", "discovered_at"}},
	{"recordings", []string{
		"started_at", "ended_at", "watched_at", "recompressed_at", "created_at", "updated_at",
	}},
	{"title_history", []string{"observed_at"}},
	{"gaps", []string{"filled_at"}},
	{"daemon_sessions", []string{"started_at", "heartbeat_at", "stopped_at"}},
	{"broadcast_fetches", []string{"next_attempt_at", "claimed_at", "updated_at"}},
}

// newRegistry builds the schema registry.
//
// initSchema creates the whole shape at version 1, which is the migrate
// package's contract: runInit stamps user_version = 1.
func newRegistry() *migrate.SQLRegistry {
	return migrate.NewSQL(schemaVersion).WithInit(initSchema)
}

// initSchema creates the tables a fresh library needs.
//
// SQLite has no time type. Timestamps are Unix nanoseconds in INTEGER
// columns so that a date-window query and an ORDER BY compare numbers, which
// no stored value can be shaped wrong enough to break. Each table gets a
// companion view rendering those columns as RFC 3339, so reading the
// database by hand does not mean reading epoch integers.
func initSchema(tx *sql.Tx) error {
	statements := []struct {
		name string
		ddl  string
	}{
		{
			"channels", `
			CREATE TABLE channels (
				id           INTEGER PRIMARY KEY,
				platform     TEXT NOT NULL,
				name         TEXT NOT NULL,
				display_name TEXT NOT NULL DEFAULT '',
				created_at   INTEGER NOT NULL,
				UNIQUE (platform, name)
			)`,
		},

		// A broadcast is one live session, whether stream-dvr recorded it
		// or merely learned it happened. Coverage is the difference
		// between the broadcasts known here and the recordings on disk.
		{
			"broadcasts", `
			CREATE TABLE broadcasts (
				id            INTEGER PRIMARY KEY,
				channel_id    INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
				-- The live session and its stored copy are two identifiers in
				-- two namespaces that never collide: stream_id is what the
				-- recorder watched, remote_id is the video the archive
				-- publishes. One column for both files every live-captured
				-- broadcast twice.
				stream_id     TEXT NOT NULL DEFAULT '',
				remote_id     TEXT NOT NULL DEFAULT '',
				-- Where the stored copy is fetched from. An identifier is not
				-- an address, and the download tool takes an address.
				url           TEXT NOT NULL DEFAULT '',
				started_at    INTEGER NOT NULL,
				ended_at      INTEGER,
				-- Where the stored copy's own timeline begins, which is what a
				-- download range is indexed from. It is not started_at: the
				-- platform starts recording at its own moment, and started_at
				-- moves as better sources describe the broadcast. Null means
				-- nobody has reported it, which is not the same as zero.
				vod_started_at INTEGER,
				-- The stretches of the stored copy the platform silenced,
				-- against that copy's own timeline. Null means nobody could
				-- ask, and an empty list means the platform answered and
				-- silenced nothing. Only the second licenses patching a hole
				-- from this copy, so the two must stay distinguishable.
				muted_spans   TEXT,
				title         TEXT NOT NULL DEFAULT '',
				category      TEXT NOT NULL DEFAULT '',
				source        TEXT NOT NULL,
				discovered_at INTEGER NOT NULL
			)`,
		},

		// Only broadcasts carrying a platform identifier can be deduplicated
		// exactly. Tracker-sourced rows often have none, so the index is
		// partial and overlap matching handles the rest.
		{"broadcasts_remote_idx", `
			CREATE UNIQUE INDEX broadcasts_remote_idx
				ON broadcasts (channel_id, remote_id)
				WHERE remote_id <> ''`},

		{"broadcasts_stream_idx", `
			CREATE UNIQUE INDEX broadcasts_stream_idx
				ON broadcasts (channel_id, stream_id)
				WHERE stream_id <> ''`},

		{"broadcasts_started_idx", `
			CREATE INDEX broadcasts_started_idx ON broadcasts (channel_id, started_at)`},

		// A recording's broadcast may be unknown at capture time, so the
		// reference is nullable and cleared rather than cascading: losing a
		// broadcast row must never delete a file's record.
		//
		// The CHECK backs the guards in Go. A size, a length, or an end
		// before its start is read as a number by the space budget, the
		// purge score, and the calendar, none of which have a reason to
		// doubt it.
		{
			"recordings", `
			CREATE TABLE recordings (
				id           INTEGER PRIMARY KEY,
				channel_id   INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
				broadcast_id INTEGER REFERENCES broadcasts(id) ON DELETE SET NULL,
				path         TEXT NOT NULL UNIQUE,
				state        TEXT NOT NULL,
				origin       TEXT NOT NULL,
				bytes        INTEGER NOT NULL DEFAULT 0,
				duration_ms  INTEGER NOT NULL DEFAULT 0,
				-- How much broadcast the file actually holds, measured from
				-- the media rather than from a clock around the subprocess.
				-- The recorder drops the segments an ad replaced, so this
				-- falls short of duration_ms by however much never arrived.
				-- Zero means nobody has measured it.
				media_duration_ms INTEGER NOT NULL DEFAULT 0,
				-- How much of this file the platform silenced, for a copy
				-- fetched from an archive. Null means nobody asked, which is
				-- every live capture and every machine with no platform
				-- session, and is not the same as an answer of none.
				muted_ms     INTEGER,
				started_at   INTEGER NOT NULL,
				ended_at     INTEGER,
				watched_at   INTEGER,
				pinned       INTEGER NOT NULL DEFAULT 0,
				refetchable  INTEGER NOT NULL DEFAULT 0,
				note         TEXT NOT NULL DEFAULT '',
				-- Null means never re-encoded, which is what the recompress
				-- scheduler reads to decide a recording is still original.
				recompressed_at INTEGER,
				created_at   INTEGER NOT NULL,
				updated_at   INTEGER NOT NULL,
				CHECK (bytes >= 0),
				CHECK (duration_ms >= 0),
				CHECK (media_duration_ms >= 0),
				CHECK (muted_ms IS NULL OR muted_ms >= 0),
				CHECK (ended_at IS NULL OR ended_at >= started_at)
			)`,
		},

		{"recordings_channel_idx", `
			CREATE INDEX recordings_channel_idx ON recordings (channel_id, started_at)`},

		// The sweep reads by state and wants the oldest first, so the sort
		// column belongs in the index rather than in a temporary B-tree
		// rebuilt on every pass.
		{"recordings_state_idx", `
			CREATE INDEX recordings_state_idx ON recordings (state, started_at)`},

		// Recordings are looked up by broadcast, oldest first, and deleting
		// a broadcast scans them all for the back-reference to clear. The
		// index is partial because a recording whose broadcast is unknown
		// is never found this way.
		{"recordings_broadcast_idx", `
			CREATE INDEX recordings_broadcast_idx ON recordings (broadcast_id, started_at)
				WHERE broadcast_id IS NOT NULL`},

		// Titles change mid-broadcast. Keeping every observation lets the
		// organizer pick one deliberately and lets chapters be derived
		// later.
		{
			"title_history", `
			CREATE TABLE title_history (
				id           INTEGER PRIMARY KEY,
				broadcast_id INTEGER NOT NULL REFERENCES broadcasts(id) ON DELETE CASCADE,
				observed_at  INTEGER NOT NULL,
				title        TEXT NOT NULL,
				category     TEXT NOT NULL DEFAULT ''
			)`,
		},

		{"title_history_idx", `
			CREATE INDEX title_history_idx ON title_history (broadcast_id, observed_at)`},

		// A gap is a hole inside a recording, from a reconnect or a late
		// start. Tracking one is what makes patching it from an archive
		// possible, because the patch needs the exact span. Offsets run from
		// the recording's start, so a negative one names no part of the file,
		// and an empty span is not a hole.
		{
			"gaps", `
			CREATE TABLE gaps (
				id           INTEGER PRIMARY KEY,
				recording_id INTEGER NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
				start_ms     INTEGER NOT NULL,
				end_ms       INTEGER NOT NULL,
				reason       TEXT NOT NULL,
				filled_at    INTEGER,
				attempts     INTEGER NOT NULL DEFAULT 0,
				CHECK (start_ms >= 0 AND end_ms > start_ms)
			)`,
		},

		{"gaps_recording_idx", `
			CREATE INDEX gaps_recording_idx ON gaps (recording_id)`},

		// Session rows are how downtime becomes visible. A daemon that dies
		// leaves a session with a stale heartbeat and no stop time, which
		// is exactly the state that otherwise goes unnoticed for days.
		{
			"daemon_sessions", `
			CREATE TABLE daemon_sessions (
				id           INTEGER PRIMARY KEY,
				started_at   INTEGER NOT NULL,
				heartbeat_at INTEGER NOT NULL,
				stopped_at   INTEGER
			)`,
		},

		// A row lands here on every daemon start and nothing prunes them,
		// while coverage reads the span of each one to tell a quiet day
		// apart from an unwatched one. Without this the calendar scans the
		// whole table on every repaint.
		{"daemon_sessions_span_idx", `
			CREATE INDEX daemon_sessions_span_idx ON daemon_sessions (started_at, heartbeat_at)`},

		// The claim asks a different question than coverage does: which
		// sessions are open and beat recently. The span index leads with
		// started_at and cannot serve that, so every claim, and every
		// command asking whether a recorder is running, scans a table
		// nothing prunes. Partial, because an open session is the only
		// kind this asks about.
		{"daemon_sessions_live_idx", `
			CREATE INDEX daemon_sessions_live_idx ON daemon_sessions (heartbeat_at)
			WHERE stopped_at IS NULL`},

		// One row per broadcast, keyed by it, so a claim is a primary key
		// and two fetchers cannot hold the same broadcast.
		{
			"broadcast_fetches", `
			CREATE TABLE broadcast_fetches (
				broadcast_id    INTEGER PRIMARY KEY REFERENCES broadcasts(id) ON DELETE CASCADE,
				state           TEXT    NOT NULL,
				attempts        INTEGER NOT NULL DEFAULT 0,
				next_attempt_at INTEGER,
				last_error      TEXT    NOT NULL DEFAULT '',
				claimed_at      INTEGER,
				claimed_by      INTEGER,
				updated_at      INTEGER NOT NULL
			)`,
		},

		// What makes gap detection repeatable. The detector re-derives every
		// hole on each pass, so without this it accumulates one duplicate
		// row per pass with nothing to stop it.
		{"gaps_span_idx", `
			CREATE UNIQUE INDEX gaps_span_idx ON gaps (recording_id, start_ms, end_ms)`},
	}

	for _, statement := range statements {
		if _, err := tx.Exec(statement.ddl); err != nil {
			return fmt.Errorf("creating %s: %w", statement.name, err)
		}
	}

	return nil
}

// ensureViews rebuilds every readable view against the schema as it now
// stands.
//
// Views are derived rather than migrated. A view carries its table's own
// columns through with *, and SQLite resolves that once at creation, so a
// migration that adds a column leaves the view listing only the columns
// present when it was created. Dropping and recreating all of them after the
// migrations run means no migration can forget its own view, and the set is
// always the one readableViews describes.
//
// It runs in one transaction because the drop and the create are one step.
// Two processes opening a library at the same moment is the ordinary case,
// and split across statements the second finds the view it just dropped
// already recreated by the first.
func ensureViews(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("beginning the view rebuild: %w", err)
	}
	defer tx.Rollback()

	for _, view := range readableViews {
		if _, err := tx.Exec(`DROP VIEW IF EXISTS ` + view.table + `_readable`); err != nil {
			return fmt.Errorf("dropping %s_readable: %w", view.table, err)
		}
		if _, err := tx.Exec(viewDDL(view.table, view.columns)); err != nil {
			return fmt.Errorf("creating %s_readable: %w", view.table, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing the view rebuild: %w", err)
	}
	return nil
}

// viewDDL builds a table's readable view: every column it already has, plus
// each nanosecond column rendered as RFC 3339 under a _utc name.
//
// Milliseconds are enough to read a row and to tell two near-simultaneous
// ones apart. Dividing as a float rather than with integer division keeps
// timestamps before 1970 on the correct side of the epoch.
func viewDDL(table string, columns []string) string {
	rendered := make([]string, 0, len(columns))
	for _, column := range columns {
		rendered = append(rendered, fmt.Sprintf(
			"strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ', %s.%s / 1000000000.0, 'unixepoch') AS %s_utc",
			table, column, column))
	}
	return fmt.Sprintf("CREATE VIEW %s_readable AS SELECT %s.*, %s FROM %s",
		table, table, strings.Join(rendered, ", "), table)
}
