package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// newStore opens an in-memory store for one test.
func newStore(t *testing.T) *Store {
	t.Helper()

	store, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory() err = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() err = %v, want nil", err)
		}
	})
	return store
}

// newChannel creates a channel to hang test rows from.
func newChannel(t *testing.T, store *Store) Channel {
	t.Helper()

	channel, err := store.UpsertChannel("twitch", "examplechannel", "ExampleChannel")
	if err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
	return channel
}

// breakRecording makes one recording's row unreadable, the way a database
// edited by something other than this program can be. SQLite keeps a value
// its column's affinity cannot convert, and the scanner then refuses the
// row. The size stays intact so a SQL sum still counts it.
func breakRecording(t *testing.T, store *Store, id int64) {
	t.Helper()

	if _, err := store.db.Exec(
		`UPDATE recordings SET duration_ms = 'not a number' WHERE id = ?`, id); err != nil {
		t.Fatalf("making recording %d unreadable: %v", id, err)
	}
}

// queryPlan returns the plan SQLite chose for a statement, so a test can
// tell an index lookup from a full scan.
func queryPlan(t *testing.T, store *Store, query string, args ...any) string {
	t.Helper()

	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explaining %q: %v", query, err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var (
			id, parent, notUsed int
			detail              string
		)
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("reading query plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading query plan: %v", err)
	}
	return plan.String()
}

// ///////////////////////////////////////////////
// Schema
// ///////////////////////////////////////////////

func TestInitSchema_FreshDatabaseIsAtCurrentVersion(t *testing.T) {
	store := newStore(t)

	got, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion() err = %v, want nil", err)
	}
	if got != schemaVersion {
		t.Errorf("SchemaVersion() = %d, want %d", got, schemaVersion)
	}
}

func TestInitSchema_CreatesEveryTable(t *testing.T) {
	store := newStore(t)

	want := []string{
		"channels", "broadcasts", "recordings",
		"title_history", "gaps", "daemon_sessions",
	}
	for _, table := range want {
		t.Run(table, func(t *testing.T) {
			var name string
			err := store.db.QueryRow(
				`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
			if err != nil {
				t.Errorf("table %q missing: %v", table, err)
			}
		})
	}
}

func TestInitSchema_EnforcesForeignKeys(t *testing.T) {
	// SQLite leaves foreign keys off by default. Without the pragma every
	// REFERENCES clause in the schema is decoration.
	store := newStore(t)

	_, err := store.db.Exec(`
		INSERT INTO broadcasts (channel_id, started_at, source, discovered_at)
		VALUES (99999, '2026-03-04T00:00:00Z', 'live', '2026-03-04T00:00:00Z')`)
	if err == nil {
		t.Error("inserting a broadcast for a missing channel succeeded, want a foreign key violation")
	}
}

func TestInitSchema_RemoteIDUniquePerChannel(t *testing.T) {
	store := newStore(t)
	channel := newChannel(t, store)

	insert := func(remoteID string) error {
		_, err := store.db.Exec(`
			INSERT INTO broadcasts (channel_id, remote_id, started_at, source, discovered_at)
			VALUES (?, ?, '2026-03-04T00:00:00Z', 'live', '2026-03-04T00:00:00Z')`,
			channel.ID, remoteID)
		return err
	}

	t.Run("duplicate remote id is rejected", func(t *testing.T) {
		if err := insert("stream-1"); err != nil {
			t.Fatalf("first insert err = %v, want nil", err)
		}
		if err := insert("stream-1"); err == nil {
			t.Error("second insert with the same remote id succeeded, want a uniqueness violation")
		}
	})

	t.Run("blank remote ids do not collide", func(t *testing.T) {
		// Tracker-sourced broadcasts have no platform identifier, so the
		// index must be partial or the second one could never be stored.
		if err := insert(""); err != nil {
			t.Fatalf("first blank insert err = %v, want nil", err)
		}
		if err := insert(""); err != nil {
			t.Errorf("second blank insert err = %v, want nil", err)
		}
	})
}

func TestInitSchema_IndexesTheQueriesTheDaemonRepeats(t *testing.T) {
	// Every one of these runs on a timer or on every calendar repaint, so a
	// full table scan is a cost paid over and over. daemon_sessions in
	// particular gains a row per daemon start and nothing prunes it.
	store := newStore(t)

	tests := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name:  "recordings of one broadcast",
			query: recordingSelect + ` WHERE broadcast_id = ? ORDER BY started_at`,
			args:  []any{int64(1)},
		},
		{
			name:  "recordings the sweep retries",
			query: recordingSelect + ` WHERE state IN (?) ORDER BY started_at`,
			args:  []any{string(StateAwaitingFile)},
		},
		{
			name:  "sessions overlapping a window",
			query: `SELECT id FROM daemon_sessions WHERE started_at < ? AND heartbeat_at >= ? ORDER BY started_at`,
			args:  []any{int64(2), int64(1)},
		},
		{
			name:  "clearing the back-reference when a broadcast goes",
			query: `DELETE FROM broadcasts WHERE id = ?`,
			args:  []any{int64(1)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := queryPlan(t, store, tt.query, tt.args...)
			if strings.Contains(plan, "SCAN") {
				t.Errorf("query plan scans a whole table:\n%s", plan)
			}
			if strings.Contains(plan, "TEMP B-TREE") {
				t.Errorf("query plan sorts into a temporary tree:\n%s", plan)
			}
		})
	}
}

func TestInitSchema_RefusesRowsThatDescribeNoRealFile(t *testing.T) {
	// The Go guards are the first line, and these constraints are what
	// stops anything else, including a hand-edit, writing a number the
	// space budget and the calendar would read as meaningful.
	store := newStore(t)
	channel := newChannel(t, store)

	recording, err := store.CreateRecording(Recording{
		ChannelID: channel.ID, Path: "checked.mkv", State: StateCapturing,
		Origin: OriginLive, StartedAt: dayAt(10, 20),
	})
	if err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}
	gap, err := store.AddGap(recording.ID, 0, time.Minute, "reconnect")
	if err != nil {
		t.Fatalf("AddGap() err = %v, want nil", err)
	}

	tests := []struct {
		name   string
		update string
		args   []any
	}{
		{name: "negative bytes", update: `UPDATE recordings SET bytes = -1 WHERE id = ?`, args: []any{recording.ID}},
		{name: "negative duration", update: `UPDATE recordings SET duration_ms = -1 WHERE id = ?`, args: []any{recording.ID}},
		{
			name:   "an end before its start",
			update: `UPDATE recordings SET ended_at = started_at - 1 WHERE id = ?`,
			args:   []any{recording.ID},
		},
		{name: "a gap starting before the file", update: `UPDATE gaps SET start_ms = -1 WHERE id = ?`, args: []any{gap.ID}},
		{name: "a gap of no length", update: `UPDATE gaps SET end_ms = start_ms WHERE id = ?`, args: []any{gap.ID}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.db.Exec(tt.update, tt.args...); err == nil {
				t.Error("the write succeeded, want the schema to refuse it")
			}
		})
	}
}

func TestInitSchema_MigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open() err = %v, want nil", err)
	}
	channel, err := first.UpsertChannel("twitch", "examplechannel", "ExampleChannel")
	if err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() err = %v, want nil", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening err = %v, want nil", err)
	}
	defer second.Close()

	// Re-running init would drop the row along with the table.
	got, err := second.Channel("twitch", "examplechannel")
	if err != nil {
		t.Fatalf("Channel() after reopen err = %v, want nil", err)
	}
	if got.ID != channel.ID {
		t.Errorf("channel id after reopen = %d, want %d", got.ID, channel.ID)
	}
}

// ///////////////////////////////////////////////
// Readable views
// ///////////////////////////////////////////////

func TestReadableViews_RenderStoredNanosecondsAsRFC3339(t *testing.T) {
	// The views are the reason storing epoch integers costs nothing in
	// readability. A view that renders the raw integer, or the wrong
	// instant, gives that back.
	store := newStore(t)
	channel := newChannel(t, store)

	at := time.Date(2026, 4, 1, 0, 0, 0, 123000000, time.UTC)
	if _, err := store.CreateRecording(Recording{
		ChannelID: channel.ID, Path: "one.ts", State: StateCapturing,
		Origin: OriginLive, StartedAt: at,
	}); err != nil {
		t.Fatalf("CreateRecording() err = %v, want nil", err)
	}

	var rendered string
	if err := store.db.QueryRow(
		`SELECT started_at_utc FROM recordings_readable WHERE path = ?`, "one.ts").Scan(&rendered); err != nil {
		t.Fatalf("reading recordings_readable err = %v, want nil", err)
	}
	if want := "2026-04-01T00:00:00.123Z"; rendered != want {
		t.Errorf("started_at_utc = %q, want %q", rendered, want)
	}
}

func TestReadableViews_CoverEveryTimestampColumn(t *testing.T) {
	// A timestamp column added without an entry in readableViews reads back
	// as a 19-digit integer, and only from the one query nobody wrote a
	// test for.
	store := newStore(t)

	declared := make(map[string]map[string]bool, len(readableViews))
	for _, view := range readableViews {
		columns := make(map[string]bool, len(view.columns))
		for _, column := range view.columns {
			columns[column] = true
		}
		declared[view.table] = columns
	}

	rows, err := store.db.Query(`
		SELECT m.name, info.name
		FROM sqlite_master AS m
		JOIN pragma_table_info(m.name) AS info
		WHERE m.type = 'table' AND info.type = 'INTEGER' AND info.name LIKE '%\_at' ESCAPE '\'`)
	if err != nil {
		t.Fatalf("listing timestamp columns err = %v, want nil", err)
	}
	defer rows.Close()

	var found int
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatalf("reading a column err = %v, want nil", err)
		}
		found++
		if !declared[table][column] {
			t.Errorf("%s.%s holds a timestamp with no entry in readableViews", table, column)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listing timestamp columns err = %v, want nil", err)
	}
	if found == 0 {
		t.Fatal("found no timestamp columns, so this test proved nothing")
	}
}

func TestEncodeTime_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		when time.Time
	}{
		{name: "utc", when: time.Date(2026, 3, 4, 21, 15, 0, 0, time.UTC)},
		{name: "with nanoseconds", when: time.Date(2026, 3, 4, 21, 15, 0, 123456789, time.UTC)},
		{name: "non-utc is normalized", when: time.Date(2026, 3, 4, 21, 15, 0, 0, time.FixedZone("x", -5*3600))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeTime(encodeTime(tt.when))
			if !got.Equal(tt.when) {
				t.Errorf("round trip = %s, want %s", got, tt.when)
			}
			if got.Location() != time.UTC {
				t.Errorf("round trip location = %s, want UTC", got.Location())
			}
		})
	}
}

// TestEncodeTime_SortsTheWayTimeDoes is the property the whole encoding
// exists for. A stored order that disagrees with chronological order puts
// rows on the wrong side of every range bound in the package.
func TestEncodeTime_SortsTheWayTimeDoes(t *testing.T) {
	bound := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		earlier time.Time
		later   time.Time
	}{
		{
			// Rendered as RFC 3339 with trailing zeros trimmed, the later
			// value is the earlier one plus more characters, and '.' sorts
			// below 'Z'. Text sorts this pair backwards.
			name:    "a fraction of a second past a whole one",
			earlier: bound,
			later:   bound.Add(123456700 * time.Nanosecond),
		},
		{name: "one nanosecond apart", earlier: bound, later: bound.Add(time.Nanosecond)},
		{name: "across a year", earlier: bound, later: bound.AddDate(1, 0, 0)},
		{name: "either side of the epoch", earlier: time.Unix(-1, 0), later: time.Unix(1, 0)},
		{name: "the whole storable range", earlier: minStorable, later: maxStorable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.earlier.Before(tt.later) {
				t.Fatalf("case is malformed: %s is not before %s", tt.earlier, tt.later)
			}
			if got, want := encodeTime(tt.earlier), encodeTime(tt.later); got >= want {
				t.Errorf("encodeTime(%s) = %d, want less than encodeTime(%s) = %d",
					tt.earlier, got, tt.later, want)
			}
		})
	}
}

func TestRequireStorable(t *testing.T) {
	tests := []struct {
		name    string
		when    time.Time
		wantErr bool
	}{
		{name: "an ordinary timestamp", when: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		{name: "the earliest storable", when: minStorable},
		{name: "the latest storable", when: maxStorable},
		{
			// The realistic failure: a caller that never set the field.
			// Stored unchecked it wraps, and the recording files in 1754.
			name:    "the zero time",
			when:    time.Time{},
			wantErr: true,
		},
		{name: "a nanosecond before the range", when: minStorable.Add(-time.Nanosecond), wantErr: true},
		{name: "a nanosecond after the range", when: maxStorable.Add(time.Nanosecond), wantErr: true},
		{name: "far past the range", when: time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireStorable(tt.when)
			if tt.wantErr && err == nil {
				t.Fatalf("requireStorable(%s) err = nil, want an error", tt.when)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("requireStorable(%s) err = %v, want nil", tt.when, err)
			}
		})
	}
}

func TestRequireStorablePtr_AllowsNil(t *testing.T) {
	if err := requireStorablePtr(nil); err != nil {
		t.Errorf("requireStorablePtr(nil) err = %v, want nil", err)
	}

	zero := time.Time{}
	if err := requireStorablePtr(&zero); err == nil {
		t.Error("requireStorablePtr(&zero) err = nil, want an error")
	}
}
