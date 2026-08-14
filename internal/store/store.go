// Package store persists everything stream-dvr knows about broadcasts and
// the files it captured from them.
//
// The database is a cache over the library, not the library itself. Every
// recording also carries a JSON sidecar on disk, so a lost or corrupt
// database can be rebuilt by scanning the directory. Nothing here is the
// only copy of anything.
//
// Timestamps are stored as Unix nanoseconds. Callers pass and receive
// time.Time and never see the encoding.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	// modernc.org/sqlite is a pure-Go driver, so builds need no cgo and
	// cross-compile without a C toolchain.
	"modernc.org/sqlite"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Store is an open library database.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

// Channel is a watched source.
type Channel struct {
	ID          int64
	Platform    string
	Name        string
	DisplayName string
	CreatedAt   time.Time
}

// Session is one run of the recording daemon.
type Session struct {
	ID          int64
	StartedAt   time.Time
	HeartbeatAt time.Time
	StoppedAt   *time.Time
}

// scanner reads one row, satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// querier is the subset of *sql.DB and *sql.Tx that a read-then-write pair
// needs, so a lookup and the write that depends on it can share one
// transaction rather than racing between them.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// SchemaMismatchError reports a database whose schema is not the one this
// build understands.
//
// Got above Want is a newer binary's library opened by an older one, and Got
// below Want is a database whose owner has not started yet. Both are named
// because the fix differs: upgrade this binary, or run the recorder once.
type SchemaMismatchError struct {
	Want int
	Got  int
}

// RecorderHeldError reports a library another recorder still holds, and says
// how long that claim has left.
//
// A recorder killed rather than stopped never closes its row, so the claim
// outlives the process by up to the staleness window. Reporting only that a
// recorder is running is wrong in exactly that case, and leaves the operator
// with no way to know that waiting is the whole fix.
type RecorderHeldError struct {
	// HeartbeatAt is when the holder was last known alive.
	HeartbeatAt time.Time
	// ClearsAt is when the claim goes stale and the library frees itself.
	ClearsAt time.Time
	// At is the moment the claim was refused, so the message can measure
	// against it rather than against a clock the caller cannot control.
	At time.Time
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// sqliteBusy is SQLITE_BUSY. The driver reports raw result codes, and the
// named constant lives in its C-translation package, which is far too heavy
// an import for one integer.
const sqliteBusy = 5

// sqliteConstraintUnique is SQLITE_CONSTRAINT_UNIQUE, an extended result
// code, so it names the violated kind rather than constraints in general.
const sqliteConstraintUnique = 2067

// Retry budget for the one lock a busy timeout does not cover. It matches
// the busy_timeout the DSN sets for every other lock.
const (
	connectAttempts = 50
	connectBackoff  = 100 * time.Millisecond
)

// ///////////////////////////////////////////////
// Time encoding
// ///////////////////////////////////////////////

// Timestamps are Unix nanoseconds in INTEGER columns, so a range bound and
// an ORDER BY compare numbers.
//
// Text cannot do this safely. Any layout wide enough to hold a fraction
// sorts a value that has one against one that does not, and '.' sorts below
// every digit, so a row a fraction of a second past a bound lands on the
// wrong side of it. Integers have no such shape to get wrong, which is why
// the encoding is not merely a wider text layout.
//
// The readable views in initSchema render these back to RFC 3339, so an
// ad-hoc sqlite3 query still shows dates rather than 19-digit integers.

// liveSessionQuery reports the newest heartbeat among the recorders holding
// this library, and NULL when none holds it.
//
// One definition, used by the claim that refuses a second recorder and by
// anything asking whether one is running. Two copies would let a command
// decide the library is free while the claim disagrees.
//
// The heartbeat rather than a count, because a refusal that cannot say when
// the holder was last seen cannot say when the claim clears either. A
// recorder killed rather than stopped leaves a row it never gets to close,
// so that is the case an operator most often meets.
const liveSessionQuery = `
	SELECT MAX(heartbeat_at) FROM daemon_sessions
	WHERE stopped_at IS NULL AND heartbeat_at > ?`

// minStorable and maxStorable bound what Unix nanoseconds can name, roughly
// 1677 to 2262. Outside that range UnixNano wraps, so a year 3000 broadcast
// stores as a 1970s one and sorts before every real recording.
var (
	minStorable = time.Unix(0, math.MinInt64).UTC()
	maxStorable = time.Unix(0, math.MaxInt64).UTC()
)

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

// ErrNotFound reports a row that does not exist.
var ErrNotFound = errors.New("not found")

// ErrRecorderRunning reports a library another recorder already holds.
//
// One library takes one recorder. Two writing the same database race on
// every capture they both notice, and the second would file a broadcast the
// first is still writing.
var ErrRecorderRunning = errors.New("a recorder is already running against it")

// ErrNoDatabase reports a library whose database has not been created yet.
//
// A reader answers this rather than creating the file, because an empty
// database is indistinguishable from a library whose recordings were lost.
var ErrNoDatabase = errors.New("the library has no database yet")

// ErrDuplicatePath reports a recording whose path another row already holds.
//
// A recovered file's name is derived from where its broadcast starts, so a
// caller that meets this is looking at work an earlier pass finished rather
// than at a failure. It is answered here because the driver states the
// violated constraint in a message, and only this package holds that error.
var ErrDuplicatePath = errors.New("a recording already holds that path")

// memoryStores names each in-memory database, so two of them share nothing.
var memoryStores atomic.Int64

// ///////////////////////////////////////////////
// Lifecycle
// ///////////////////////////////////////////////

// Open opens the library database at path, creating and migrating it as
// needed.
//
// WAL mode lets the TUI read while the daemon writes. Foreign keys are
// enabled explicitly because SQLite leaves them off by default, which would
// silently reduce every reference in the schema to a comment.
//
// Transactions take the write lock as they begin rather than on their first
// write. The daemon and the TUI are separate processes, so a read-then-write
// pair that only locks at the write can find the row it matched already
// changed, and upgrading the lock at that point is what returns SQLITE_BUSY.
//
// Pragmas apply in the order they appear, and busy_timeout leads because
// switching a database to WAL needs an exclusive lock. Two processes opening
// one library at the same moment collide there, before any query runs.
// Without the timeout already in force, the loser fails where it would
// otherwise wait.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", libraryDSN(path))
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}

	// A single writer avoids SQLITE_BUSY under concurrent writes; readers
	// are served from WAL without blocking.
	db.SetMaxOpenConns(1)

	if err := connect(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to database %s: %w", path, err)
	}
	if err := registry.Run(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating database %s: %w", path, err)
	}
	if err := ensureViews(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("building the readable views of %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// libraryDSN names the library database at path.
//
// The reader and the writer share it so a pragma set for one is set for the
// other: a reader running without foreign_keys would answer queries the
// writer's schema forbids.
func libraryDSN(path string) string {
	return "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(on)" +
		"&_txlock=immediate"
}

// OpenClient opens an existing library database without owning its schema.
//
// The calendar reads a library the recorder writes, and the two run from
// separate binaries that need not be the same build. Open migrates, so a
// newer calendar opened against a live daemon's library would move the schema
// underneath it. A client stats the file first, because sql.Open would
// otherwise create an empty database and report a library with no recordings
// rather than no library.
//
// A client handle names schema ownership, not access. Marking a recording
// watched goes through this handle.
func OpenClient(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("opening database %s: %w", path, ErrNoDatabase)
		}
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}

	db, err := sql.Open("sqlite", libraryDSN(path))
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	if err := connect(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to database %s: %w", path, err)
	}

	client := &Store{db: db}
	version, err := client.SchemaVersion()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}

	switch version {
	case schemaVersion:
		return client, nil
	case 0:
		// The file exists and no migration ever ran against it, which is
		// what a database created by a stray open looks like.
		db.Close()
		return nil, fmt.Errorf("opening database %s: %w", path, ErrNoDatabase)
	default:
		db.Close()
		return nil, fmt.Errorf("opening database %s: %w", path,
			&SchemaMismatchError{Want: schemaVersion, Got: version})
	}
}

// Error implements error.
func (e *SchemaMismatchError) Error() string {
	return fmt.Sprintf("database schema is version %d, want version %d", e.Got, e.Want)
}

// Error implements error.
func (e *RecorderHeldError) Error() string {
	return fmt.Sprintf("%s, last seen %s ago. If that recorder is gone, the claim clears in %s",
		ErrRecorderRunning, sinceFor(e.At, e.HeartbeatAt), sinceFor(e.ClearsAt, e.At))
}

// Unwrap reports the sentinel, so every errors.Is against ErrRecorderRunning
// keeps answering whatever detail this carries.
func (e *RecorderHeldError) Unwrap() error { return ErrRecorderRunning }

// sinceFor renders how far `to` sits from `from`, never negative.
//
// A clock that moved backwards, or a heartbeat written by another machine
// running ahead, would otherwise print a negative age and read as nonsense.
func sinceFor(to, from time.Time) time.Duration {
	gap := to.Sub(from).Round(time.Second)
	if gap < 0 {
		return 0
	}
	return gap
}

// connect opens the first connection, which is where the DSN's pragmas are
// applied.
//
// Switching a database into WAL mode takes a lock that busy_timeout does not
// cover: SQLite answers SQLITE_BUSY without ever calling the busy handler.
// The daemon and the TUI opening one new library at the same moment collide
// exactly there, so the loser waits for the winner and then finds the pragma
// a no-op against a database already in WAL.
func connect(db *sql.DB) error {
	var err error
	for range connectAttempts {
		if err = db.Ping(); !isBusy(err) {
			return err
		}
		time.Sleep(connectBackoff)
	}
	return err
}

// isBusy reports a lock held elsewhere, which is worth retrying, as opposed
// to a fault in the caller, which is not.
func isBusy(err error) bool {
	busy, ok := errors.AsType[*sqlite.Error](err)
	return ok && busy.Code() == sqliteBusy
}

// isDuplicate reports a value a UNIQUE constraint already holds.
//
// The extended result code is the test rather than the message, so a
// reworded driver error cannot turn a duplicate into an unrecognised
// failure.
func isDuplicate(err error) bool {
	violated, ok := errors.AsType[*sqlite.Error](err)
	return ok && violated.Code() == sqliteConstraintUnique
}

// OpenMemory opens a private in-memory database, for tests.
//
// The database is named and shared rather than anonymous, and the pool keeps
// its connection. An anonymous in-memory database belongs to the one
// connection that opened it, so a connection database/sql decides to discard
// takes the schema and every row with it, and the replacement answers
// queries from an empty database without erroring.
func OpenMemory() (*Store, error) {
	dsn := fmt.Sprintf("file:memory%d?mode=memory&cache=shared&_pragma=foreign_keys(on)&_txlock=immediate",
		memoryStores.Add(1))

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening in-memory database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	if err := registry.Run(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating in-memory database: %w", err)
	}
	if err := ensureViews(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("building the readable views of the in-memory database: %w", err)
	}
	return &Store{db: db}, nil
}

// WithLogger directs the store's own diagnostics somewhere other than
// slog.Default, and returns the store so it can be chained onto Open.
//
// The only thing the store logs is a row it had to skip, and the log names
// that row so the operator can go and look at it. Which day the row belonged
// to reaches the caller in Day.Degraded, because a scheduled task has
// nowhere to write and the TUI holds the terminal.
func (s *Store) WithLogger(l *slog.Logger) *Store {
	s.log = l
	return s
}

// logger returns where diagnostics go. slog.Default is the fallback, and
// under a scheduled task or the TUI's alternate screen it goes nowhere
// useful, which is why a skip is also counted into the result.
func (s *Store) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

// Close releases the database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing database: %w", err)
	}
	return nil
}

// SchemaVersion returns the database's current schema version.
func (s *Store) SchemaVersion() (int, error) {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("reading schema version: %w", err)
	}
	return version, nil
}

// encodeTime renders a timestamp for storage. Every public method that
// accepts a caller's time guards it with requireStorable first, so the value
// reaching here is known to fit.
func encodeTime(t time.Time) int64 {
	return t.UTC().UnixNano()
}

// encodeTimePtr renders an optional timestamp, yielding NULL for nil.
func encodeTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return encodeTime(*t)
}

// decodeTime turns a stored timestamp back into a time.
//
// Every int64 names an instant, so this cannot fail. That is the point of
// the integer encoding: no stored value can be unreadable, so one bad row
// cannot blank the query that found it.
func decodeTime(nanos int64) time.Time {
	return time.Unix(0, nanos).UTC()
}

// decodeTimePtr reads an optional stored timestamp, yielding nil for NULL.
func decodeTimePtr(nanos sql.NullInt64) *time.Time {
	if !nanos.Valid {
		return nil
	}
	decoded := decodeTime(nanos.Int64)
	return &decoded
}

// requireStorable rejects a timestamp Unix nanoseconds cannot name.
//
// The zero time.Time is the case this catches in practice: a caller that
// leaves a required timestamp unset would otherwise store a wrapped value
// and file the recording in 1754.
func requireStorable(times ...time.Time) error {
	for _, t := range times {
		if t.Before(minStorable) || t.After(maxStorable) {
			return fmt.Errorf("timestamp %s is outside the storable range %s to %s",
				t.UTC().Format(time.RFC3339),
				minStorable.Format(time.RFC3339),
				maxStorable.Format(time.RFC3339))
		}
	}
	return nil
}

// requireStorablePtr is requireStorable for optional timestamps, where nil
// means the column stays NULL.
func requireStorablePtr(times ...*time.Time) error {
	for _, t := range times {
		if t == nil {
			continue
		}
		if err := requireStorable(*t); err != nil {
			return err
		}
	}
	return nil
}

// ///////////////////////////////////////////////
// Channels
// ///////////////////////////////////////////////

// UpsertChannel returns the channel for a platform and name, creating it
// when absent and refreshing its display name when supplied.
//
// The daemon calls this for every configured channel at startup, so it must
// be safe to run repeatedly against an existing row.
func (s *Store) UpsertChannel(platform, name, displayName string) (Channel, error) {
	now := time.Now().UTC()

	// The write and the read-back are one decision, like every other
	// read-then-write pair here. Run apart, a channel deleted between them
	// makes a call that just succeeded report ErrNotFound.
	tx, err := s.db.Begin()
	if err != nil {
		return Channel{}, fmt.Errorf("upserting channel %s/%s: %w", platform, name, err) // coverage:partial (Begin never fails on live SQLite)
	}
	defer tx.Rollback() // no-op after Commit

	if _, err := tx.Exec(`
		INSERT INTO channels (platform, name, display_name, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (platform, name) DO UPDATE SET
			display_name = CASE WHEN excluded.display_name <> '' THEN excluded.display_name ELSE channels.display_name END`,
		platform, name, displayName, encodeTime(now)); err != nil {
		return Channel{}, fmt.Errorf("upserting channel %s/%s: %w", platform, name, err)
	}

	channel, err := readChannel(tx, platform, name)
	if err != nil {
		return Channel{}, err
	}
	if err := tx.Commit(); err != nil {
		return Channel{}, fmt.Errorf("upserting channel %s/%s: %w", platform, name, err) // coverage:partial (Commit never fails on in-memory SQLite)
	}
	return channel, nil
}

// Channel returns one channel by platform and name.
func (s *Store) Channel(platform, name string) (Channel, error) {
	return readChannel(s.db, platform, name)
}

// readChannel reads one channel from a database or a transaction.
func readChannel(q querier, platform, name string) (Channel, error) {
	row := q.QueryRow(`
		SELECT id, platform, name, display_name, created_at
		FROM channels WHERE platform = ? AND name = ?`, platform, name)

	channel, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, fmt.Errorf("channel %s/%s: %w", platform, name, ErrNotFound)
	}
	if err != nil {
		return Channel{}, fmt.Errorf("reading channel %s/%s: %w", platform, name, err)
	}
	return channel, nil
}

// Channels returns every known channel, ordered by platform then name.
func (s *Store) Channels() ([]Channel, error) {
	rows, err := s.db.Query(`
		SELECT id, platform, name, display_name, created_at
		FROM channels ORDER BY platform, name`)
	if err != nil {
		return nil, fmt.Errorf("listing channels: %w", err)
	}
	defer rows.Close()

	var channels []Channel
	for rows.Next() {
		channel, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("reading channel: %w", err)
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing channels: %w", err)
	}
	return channels, nil
}

// scanChannel reads one channel row.
func scanChannel(row scanner) (Channel, error) {
	var (
		channel   Channel
		createdAt int64
	)
	if err := row.Scan(&channel.ID, &channel.Platform, &channel.Name, &channel.DisplayName, &createdAt); err != nil {
		return Channel{}, err
	}

	channel.CreatedAt = decodeTime(createdAt)
	return channel, nil
}

// ///////////////////////////////////////////////
// Daemon sessions
// ///////////////////////////////////////////////

// RecorderRunning reports whether a recorder holds this library now.
//
// It asks the same question StartSession's claim does, through the same
// query, so a command that checks before acting cannot reach a different
// answer than the claim would.
//
// A session whose heartbeat has gone stale does not count, because a
// daemon killed rather than stopped leaves its row behind and would
// otherwise hold the library until somebody edited the database.
func (s *Store) RecorderRunning(at time.Time, activeWithin time.Duration) (bool, error) {
	var beat sql.NullInt64
	if err := s.db.QueryRow(liveSessionQuery, encodeTime(at.Add(-activeWithin))).Scan(&beat); err != nil {
		return false, fmt.Errorf("checking for a running recorder: %w", err)
	}
	return beat.Valid, nil
}

// StartSession claims the library for one recorder and records that it came
// up, answering ErrRecorderRunning when another already holds it.
//
// The session row is the claim. A live recorder is a row with no stopped_at
// whose heartbeat is within activeWithin, so a crashed daemon releases the
// library by falling silent rather than by cleaning up after itself.
//
// The check and the insert share one transaction, and the DSN opens every
// transaction with the write lock already taken. That is what stops a second
// recorder's check running between the first one's check and its insert.
//
// No lock file and no PID liveness. A PID check needs a platform split and
// answers nothing about a library on a network share opened from another
// machine. The heartbeat answers both.
func (s *Store) StartSession(at time.Time, activeWithin time.Duration) (Session, error) {
	if err := requireStorable(at); err != nil {
		return Session{}, fmt.Errorf("starting session: %w", err)
	}
	stamp := encodeTime(at)

	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, fmt.Errorf("starting session: %w", err)
	}
	defer tx.Rollback()

	var beat sql.NullInt64
	if err := tx.QueryRow(liveSessionQuery, encodeTime(at.Add(-activeWithin))).Scan(&beat); err != nil {
		return Session{}, fmt.Errorf("starting session: %w", err)
	}
	if beat.Valid {
		held := decodeTime(beat.Int64)
		return Session{}, fmt.Errorf("this library: %w", &RecorderHeldError{
			HeartbeatAt: held,
			ClearsAt:    held.Add(activeWithin),
			At:          at,
		})
	}

	result, err := tx.Exec(`
		INSERT INTO daemon_sessions (started_at, heartbeat_at) VALUES (?, ?)`, stamp, stamp)
	if err != nil {
		return Session{}, fmt.Errorf("starting session: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Session{}, fmt.Errorf("starting session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("starting session: %w", err)
	}
	return Session{ID: id, StartedAt: at.UTC(), HeartbeatAt: at.UTC()}, nil
}

// Heartbeat records that the daemon is still alive.
//
// A heartbeat against a session that is not there returns ErrNotFound.
// Rebuilding the database is the documented recovery path, and it leaves the
// daemon holding an id nothing answers to. A silent success means the daemon
// beats into nothing for the rest of its run, while every day of that run
// paints unknown.
func (s *Store) Heartbeat(sessionID int64, at time.Time) error {
	if err := requireStorable(at); err != nil {
		return fmt.Errorf("recording heartbeat: %w", err)
	}
	result, err := s.db.Exec(
		`UPDATE daemon_sessions SET heartbeat_at = ? WHERE id = ?`, encodeTime(at), sessionID)
	if err != nil {
		return fmt.Errorf("recording heartbeat: %w", err)
	}
	return requireRow(result, sessionID, "session")
}

// StopSession records a clean shutdown, which is what distinguishes one
// from a crash when the next session looks for downtime. It returns
// ErrNotFound for a session that is not there.
func (s *Store) StopSession(sessionID int64, at time.Time) error {
	if err := requireStorable(at); err != nil {
		return fmt.Errorf("stopping session: %w", err)
	}
	result, err := s.db.Exec(
		`UPDATE daemon_sessions SET stopped_at = ?, heartbeat_at = ? WHERE id = ?`,
		encodeTime(at), encodeTime(at), sessionID)
	if err != nil {
		return fmt.Errorf("stopping session: %w", err)
	}
	return requireRow(result, sessionID, "session")
}

// ReopenSession closes a frozen session and opens its successor as one
// decision, returning the new one.
//
// Two calls cannot do this. StartSession puts its check and its insert in
// one immediate transaction precisely so no second recorder can slip
// between them, and stopping first from outside that transaction hands
// away exactly the window it was built to close: for one round trip the
// library is unclaimed, and a failure to open the successor leaves it
// unclaimed for the rest of the run while this process keeps beating into
// a row it already stopped.
func (s *Store) ReopenSession(sessionID int64, stoppedAt, startedAt time.Time,
	activeWithin time.Duration,
) (Session, error) {
	if err := requireStorable(stoppedAt); err != nil {
		return Session{}, fmt.Errorf("reopening session: %w", err)
	}
	if err := requireStorable(startedAt); err != nil {
		return Session{}, fmt.Errorf("reopening session: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return Session{}, fmt.Errorf("reopening session: %w", err)
	}
	defer tx.Rollback()

	stop := encodeTime(stoppedAt)
	result, err := tx.Exec(
		`UPDATE daemon_sessions SET stopped_at = ?, heartbeat_at = ? WHERE id = ?`,
		stop, stop, sessionID)
	if err != nil {
		return Session{}, fmt.Errorf("reopening session: %w", err)
	}
	if err := requireRow(result, sessionID, "session"); err != nil {
		return Session{}, err
	}

	// Asked inside the same transaction as the stop, so the row just
	// closed cannot answer and no other recorder can have taken the
	// library in between.
	var beat sql.NullInt64
	if err := tx.QueryRow(liveSessionQuery,
		encodeTime(startedAt.Add(-activeWithin))).Scan(&beat); err != nil {
		return Session{}, fmt.Errorf("reopening session: %w", err)
	}
	if beat.Valid {
		held := decodeTime(beat.Int64)
		return Session{}, fmt.Errorf("this library: %w", &RecorderHeldError{
			HeartbeatAt: held,
			ClearsAt:    held.Add(activeWithin),
			At:          startedAt,
		})
	}

	start := encodeTime(startedAt)
	opened, err := tx.Exec(
		`INSERT INTO daemon_sessions (started_at, heartbeat_at) VALUES (?, ?)`, start, start)
	if err != nil {
		return Session{}, fmt.Errorf("reopening session: %w", err)
	}
	id, err := opened.LastInsertId()
	if err != nil {
		return Session{}, fmt.Errorf("reopening session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("reopening session: %w", err)
	}
	return Session{ID: id, StartedAt: startedAt.UTC(), HeartbeatAt: startedAt.UTC()}, nil
}

// SessionsBetween returns every session that overlapped a window, oldest
// first.
//
// A session's span runs from its start to its last heartbeat, because a
// crashed session's stop time is exactly what is missing. Coverage uses
// this to tell a day with no broadcast apart from a day nothing was
// watching.
func (s *Store) SessionsBetween(from, to time.Time) ([]Session, error) {
	sessions, _, err := s.sessionsBetween(from, to)
	return sessions, err
}

// sessionsBetween is SessionsBetween plus the number of rows it had to skip,
// which coverage needs to say a day is not fully accounted for.
func (s *Store) sessionsBetween(from, to time.Time) ([]Session, int, error) {
	if err := requireStorable(from, to); err != nil {
		return nil, 0, fmt.Errorf("listing sessions: %w", err)
	}

	rows, err := s.db.Query(`
		SELECT id, started_at, heartbeat_at, stopped_at
		FROM daemon_sessions
		WHERE started_at < ? AND heartbeat_at >= ?
		ORDER BY started_at`, encodeTime(to), encodeTime(from))
	if err != nil {
		return nil, 0, fmt.Errorf("listing sessions: %w", err)
	}
	defer rows.Close()

	var (
		sessions []Session
		skipped  int
	)
	for rows.Next() {
		var (
			session   Session
			startedAt int64
			heartbeat int64
			stoppedAt sql.NullInt64
		)
		if err := rows.Scan(&session.ID, &startedAt, &heartbeat, &stoppedAt); err != nil {
			skipped++
			s.logger().Warn("skipping unreadable daemon session row", "session_id", session.ID, "error", err)
			continue
		}
		session.StartedAt = decodeTime(startedAt)
		session.HeartbeatAt = decodeTime(heartbeat)
		session.StoppedAt = decodeTimePtr(stoppedAt)
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("listing sessions: %w", err)
	}
	return sessions, skipped, nil
}

// LastSession returns the most recent session before the given one, or
// ErrNotFound when this is the first.
//
// The daemon compares its start against that session's last heartbeat to
// report how long it was down, which is the report that turns a silent
// multi-day outage into a visible one.
func (s *Store) LastSession(before int64) (Session, error) {
	row := s.db.QueryRow(`
		SELECT id, started_at, heartbeat_at, stopped_at
		FROM daemon_sessions WHERE id < ? ORDER BY id DESC LIMIT 1`, before)

	var (
		session   Session
		startedAt int64
		heartbeat int64
		stoppedAt sql.NullInt64
	)
	err := row.Scan(&session.ID, &startedAt, &heartbeat, &stoppedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("no session before %d: %w", before, ErrNotFound)
	}
	if err != nil {
		return Session{}, fmt.Errorf("reading previous session: %w", err)
	}

	session.StartedAt = decodeTime(startedAt)
	session.HeartbeatAt = decodeTime(heartbeat)
	session.StoppedAt = decodeTimePtr(stoppedAt)
	return session, nil
}

// FirstSessionStart returns when a recorder first claimed this library.
//
// It bounds how far back automatic recovery may reach. A recorder cannot
// have missed a broadcast that aired before it existed, so a library with
// one day of history has one day to recover, however wide a window is asked
// for. Without it a fresh install downloads a channel's entire archive on
// its first routine round.
//
// ErrNotFound means no recorder has ever claimed the library, which is the
// state of every library before the first start.
func (s *Store) FirstSessionStart() (time.Time, error) {
	var startedAt int64
	err := s.db.QueryRow(`SELECT started_at FROM daemon_sessions ORDER BY started_at ASC LIMIT 1`).
		Scan(&startedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("no recorder has claimed this library: %w", ErrNotFound)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("reading the first session: %w", err)
	}
	return decodeTime(startedAt), nil
}
