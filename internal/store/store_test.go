package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sessionStale is the window a test claims a library within. It mirrors the
// daemon's own StaleAfter, so no test passes on a window production would
// never use.
const sessionStale = 3 * time.Minute

// ///////////////////////////////////////////////
// Lifecycle
// ///////////////////////////////////////////////

func TestOpen_CreatesTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested-not-created", "library.db")

	// The state directory is created by the library package, so Open must
	// not be expected to build a missing tree.
	if _, err := Open(path); err == nil {
		t.Error("Open() into a missing directory err = nil, want an error")
	}
}

func TestOpen_SurvivesConcurrentFirstRuns(t *testing.T) {
	// The daemon and the TUI both open the store. Starting the service and
	// opening the calendar on a new library runs the first-run path twice at
	// once, and neither is allowed to die on the other's schema.
	path := filepath.Join(t.TempDir(), "library.db")

	const openers = 8
	var (
		start  sync.WaitGroup
		finish sync.WaitGroup
		errs   = make([]error, openers)
	)
	start.Add(1)
	for i := range openers {
		finish.Go(func() {
			start.Wait()
			opened, err := Open(path)
			errs[i] = err
			if err == nil {
				opened.Close()
			}
		})
	}
	start.Done()
	finish.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("opener %d: Open() err = %v, want nil", i, err)
		}
	}
}

func TestOpenMemory_KeepsTwoStoresApart(t *testing.T) {
	// The databases are named and shared so the pool cannot drop the schema
	// under them, which makes the name the only thing keeping one test's
	// rows out of another's.
	first := newStore(t)
	second := newStore(t)

	if _, err := first.UpsertChannel("twitch", "onlyhere", "Only Here"); err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}

	channels, err := second.Channels()
	if err != nil {
		t.Fatalf("Channels() err = %v, want nil", err)
	}
	if len(channels) != 0 {
		t.Errorf("the second store sees %d channels, want none of the first store's", len(channels))
	}
}

func TestOpen_RefusesADatabaseFromANewerBuild(t *testing.T) {
	// A library written by a newer build holds shapes this one cannot read.
	// Opening it and writing to it is how the newer build's work is lost.
	path := filepath.Join(t.TempDir(), "library.db")

	created, err := Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	if _, err := created.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("setting user_version: %v", err)
	}
	created.Close()

	reopened, err := Open(path)
	if err == nil {
		reopened.Close()
		t.Fatal("Open() err = nil, want a refusal for a database at version 99")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("Open() err = %v, want it to name the version it found", err)
	}
}

func TestOpen_RoundTripsToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	defer store.Close()

	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion() err = %v, want nil", err)
	}
	if version != schemaVersion {
		t.Errorf("SchemaVersion() = %d, want %d", version, schemaVersion)
	}
}

func TestOpen_MigratesAnExistingLibrary(t *testing.T) {
	// A fresh database is created at version 1 and then walks the same
	// migrations an older one does, so a column a migration adds has to be
	// there either way. Only the readable views are rebuilt outside that
	// chain, and a query against one is what proves they were.
	path := filepath.Join(t.TempDir(), "library.db")

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	defer store.Close()

	version, err := store.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion() err = %v, want nil", err)
	}
	if version != schemaVersion {
		t.Errorf("SchemaVersion() = %d, want %d", version, schemaVersion)
	}

	var count int
	if err := store.db.QueryRow(
		`SELECT count(recompressed_at_utc) FROM recordings_readable`).Scan(&count); err != nil {
		t.Errorf("the readable view does not render recompressed_at: %v", err)
	}
}

func TestOpen_RebuildsTheViewsEveryTime(t *testing.T) {
	// The views are derived rather than migrated, so a database whose views
	// were dropped by hand gets them back on the next open rather than
	// failing every query that reads one.
	path := filepath.Join(t.TempDir(), "library.db")
	created, err := Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	if _, err := created.db.Exec(`DROP VIEW recordings_readable`); err != nil {
		t.Fatalf("dropping the view: %v", err)
	}
	created.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	defer reopened.Close()

	var count int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM recordings_readable`).Scan(&count); err != nil {
		t.Errorf("the view was not rebuilt: %v", err)
	}
}

// ///////////////////////////////////////////////
// Client lifecycle
// ///////////////////////////////////////////////

func TestOpenClient_ReadsADatabaseTheRecorderOwns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")
	owner, err := Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	owner.Close()

	client, err := OpenClient(path)
	if err != nil {
		t.Fatalf("OpenClient() err = %v, want nil", err)
	}
	defer client.Close()

	version, err := client.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion() err = %v, want nil", err)
	}
	if version != schemaVersion {
		t.Errorf("SchemaVersion() = %d, want %d", version, schemaVersion)
	}
}

func TestOpenClient_RefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, path string)
		check   func(t *testing.T, err error)
	}{
		{
			name:    "no database at all",
			prepare: func(*testing.T, string) {},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoDatabase) {
					t.Errorf("OpenClient() err = %v, want ErrNoDatabase", err)
				}
			},
		},
		{
			name: "a file nothing ever migrated",
			prepare: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatalf("writing an empty database: %v", err)
				}
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNoDatabase) {
					t.Errorf("OpenClient() err = %v, want ErrNoDatabase", err)
				}
			},
		},
		{
			name: "a schema from another build",
			prepare: func(t *testing.T, path string) {
				forceVersion(t, path, 999)
			},
			check: func(t *testing.T, err error) {
				mismatch, ok := errors.AsType[*SchemaMismatchError](err)
				if !ok {
					t.Fatalf("OpenClient() err = %v, want a *SchemaMismatchError", err)
				}
				if mismatch.Got != 999 {
					t.Errorf("Got = %d, want 999", mismatch.Got)
				}
				if mismatch.Want != schemaVersion {
					t.Errorf("Want = %d, want %d", mismatch.Want, schemaVersion)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "library.db")
			tc.prepare(t, path)

			client, err := OpenClient(path)
			if err == nil {
				client.Close()
				t.Fatal("OpenClient() err = nil, want a refusal")
			}
			tc.check(t, err)
		})
	}
}

func TestOpenClient_CreatesNothingWhereThereIsNoLibrary(t *testing.T) {
	// sql.Open creates the file it is given, so a client that opened before
	// it looked would report an empty library rather than a missing one.
	dir := t.TempDir()

	client, err := OpenClient(filepath.Join(dir, "library.db"))
	if err == nil {
		client.Close()
		t.Fatal("OpenClient() err = nil, want a refusal")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Errorf("OpenClient() left %d entries behind, want none", len(entries))
	}
}

func TestOpenClient_NeverMigrates(t *testing.T) {
	// The daemon owns the schema. A calendar that migrated would move it
	// underneath a recorder that is already running against it.
	path := filepath.Join(t.TempDir(), "library.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("writing an empty database: %v", err)
	}

	client, err := OpenClient(path)
	if err == nil {
		client.Close()
		t.Fatal("OpenClient() err = nil, want a refusal")
	}

	version, tables := inspect(t, path)
	if version != 0 {
		t.Errorf("user_version = %d, want 0: OpenClient() migrated the database", version)
	}
	if tables != 0 {
		t.Errorf("the database holds %d tables, want 0: OpenClient() built the schema", tables)
	}
}

func TestOpenClient_Writes(t *testing.T) {
	// A client handle names schema ownership, not access. Marking a
	// recording watched and pinning one both go through this handle.
	path := filepath.Join(t.TempDir(), "library.db")
	owner, err := Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	owner.Close()

	client, err := OpenClient(path)
	if err != nil {
		t.Fatalf("OpenClient() err = %v, want nil", err)
	}
	defer client.Close()

	if _, err := client.UpsertChannel("twitch", "examplechannel", "ExampleChannel"); err != nil {
		t.Fatalf("UpsertChannel() err = %v, want nil", err)
	}
}

func TestSchemaMismatchError_NamesBothVersions(t *testing.T) {
	// The message is the whole diagnosis: which way the versions differ is
	// what says whether to upgrade the binary or start the recorder.
	got := (&SchemaMismatchError{Want: 3, Got: 7}).Error()

	for _, want := range []string{"3", "7"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q, want it to name version %s", got, want)
		}
	}
}

// forceVersion stamps a migrated database with a schema version this build
// cannot produce, which is the only way to make one from inside it.
func forceVersion(t *testing.T, path string, version int) {
	t.Helper()

	created, err := Open(path)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	defer created.Close()

	if _, err := created.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("setting user_version: %v", err)
	}
}

// inspect reads a database's shape without migrating it, which is what tells
// a migration that ran from one that did not.
func inspect(t *testing.T, path string) (version, tables int) {
	t.Helper()

	db, err := sql.Open("sqlite", libraryDSN(path))
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer db.Close()

	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	const count = `SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`
	if err := db.QueryRow(count).Scan(&tables); err != nil {
		t.Fatalf("counting tables: %v", err)
	}
	return version, tables
}

// ///////////////////////////////////////////////
// Channels
// ///////////////////////////////////////////////

func TestUpsertChannel(t *testing.T) {
	store := newStore(t)

	t.Run("creates on first call", func(t *testing.T) {
		channel, err := store.UpsertChannel("twitch", "examplechannel", "ExampleChannel")
		if err != nil {
			t.Fatalf("UpsertChannel() err = %v, want nil", err)
		}
		if channel.ID == 0 {
			t.Error("UpsertChannel() returned a zero id")
		}
		if channel.DisplayName != "ExampleChannel" {
			t.Errorf("DisplayName = %q, want %q", channel.DisplayName, "ExampleChannel")
		}
	})

	t.Run("is safe to repeat", func(t *testing.T) {
		// The daemon upserts every configured channel on each startup.
		first, err := store.UpsertChannel("twitch", "examplechannel", "ExampleChannel")
		if err != nil {
			t.Fatalf("UpsertChannel() err = %v, want nil", err)
		}
		second, err := store.UpsertChannel("twitch", "examplechannel", "ExampleChannel")
		if err != nil {
			t.Fatalf("UpsertChannel() err = %v, want nil", err)
		}
		if first.ID != second.ID {
			t.Errorf("second upsert id = %d, want %d", second.ID, first.ID)
		}
	})

	t.Run("refreshes a changed display name", func(t *testing.T) {
		if _, err := store.UpsertChannel("twitch", "renamer", "Old Name"); err != nil {
			t.Fatalf("UpsertChannel() err = %v, want nil", err)
		}
		got, err := store.UpsertChannel("twitch", "renamer", "New Name")
		if err != nil {
			t.Fatalf("UpsertChannel() err = %v, want nil", err)
		}
		if got.DisplayName != "New Name" {
			t.Errorf("DisplayName = %q, want %q", got.DisplayName, "New Name")
		}
	})

	t.Run("a blank display name does not erase a known one", func(t *testing.T) {
		// A failed metadata call must not downgrade what is already known,
		// or a naming fallback would kick in for no reason.
		if _, err := store.UpsertChannel("twitch", "keeper", "Keeper"); err != nil {
			t.Fatalf("UpsertChannel() err = %v, want nil", err)
		}
		got, err := store.UpsertChannel("twitch", "keeper", "")
		if err != nil {
			t.Fatalf("UpsertChannel() err = %v, want nil", err)
		}
		if got.DisplayName != "Keeper" {
			t.Errorf("DisplayName = %q, want the previously known %q", got.DisplayName, "Keeper")
		}
	})
}

func TestChannel_NotFound(t *testing.T) {
	store := newStore(t)

	if _, err := store.Channel("twitch", "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Channel() err = %v, want it to wrap ErrNotFound", err)
	}
}

func TestChannels_Ordered(t *testing.T) {
	store := newStore(t)

	for _, seed := range []struct{ platform, name string }{
		{"youtube", "zeta"},
		{"twitch", "beta"},
		{"twitch", "alpha"},
	} {
		if _, err := store.UpsertChannel(seed.platform, seed.name, ""); err != nil {
			t.Fatalf("UpsertChannel() err = %v, want nil", err)
		}
	}

	channels, err := store.Channels()
	if err != nil {
		t.Fatalf("Channels() err = %v, want nil", err)
	}

	want := []string{"twitch/alpha", "twitch/beta", "youtube/zeta"}
	if len(channels) != len(want) {
		t.Fatalf("Channels() returned %d, want %d", len(channels), len(want))
	}
	for i, channel := range channels {
		got := channel.Platform + "/" + channel.Name
		if got != want[i] {
			t.Errorf("Channels()[%d] = %q, want %q", i, got, want[i])
		}
	}
}

// ///////////////////////////////////////////////
// Daemon sessions
// ///////////////////////////////////////////////

func TestSession_TracksDowntime(t *testing.T) {
	// A daemon that dies leaves a session with a stale heartbeat and no
	// stop time. Comparing the next start against that heartbeat is how a
	// silent multi-day outage becomes a reportable number.
	store := newStore(t)

	crashStart := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	crashed, err := store.StartSession(crashStart, sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	lastAlive := crashStart.Add(2 * time.Hour)
	if err := store.Heartbeat(crashed.ID, lastAlive); err != nil {
		t.Fatalf("Heartbeat() err = %v, want nil", err)
	}

	restart := crashStart.Add(96 * time.Hour)
	current, err := store.StartSession(restart, sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	previous, err := store.LastSession(current.ID)
	if err != nil {
		t.Fatalf("LastSession() err = %v, want nil", err)
	}
	if previous.ID != crashed.ID {
		t.Errorf("LastSession() id = %d, want %d", previous.ID, crashed.ID)
	}
	if previous.StoppedAt != nil {
		t.Errorf("StoppedAt = %v, want nil for a crashed session", previous.StoppedAt)
	}
	if got := restart.Sub(previous.HeartbeatAt); got != 94*time.Hour {
		t.Errorf("downtime = %s, want %s", got, 94*time.Hour)
	}
}

func TestSession_CleanShutdownIsDistinguishable(t *testing.T) {
	store := newStore(t)

	start := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	session, err := store.StartSession(start, sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	stop := start.Add(time.Hour)
	if err := store.StopSession(session.ID, stop); err != nil {
		t.Fatalf("StopSession() err = %v, want nil", err)
	}

	next, err := store.StartSession(stop.Add(time.Minute), sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	previous, err := store.LastSession(next.ID)
	if err != nil {
		t.Fatalf("LastSession() err = %v, want nil", err)
	}
	if previous.StoppedAt == nil {
		t.Fatal("StoppedAt = nil, want the recorded shutdown time")
	}
	if !previous.StoppedAt.Equal(stop) {
		t.Errorf("StoppedAt = %s, want %s", previous.StoppedAt, stop)
	}
}

func TestStartSession_RefusesALibraryAnotherRecorderHolds(t *testing.T) {
	// Two recorders on one library race on every capture they both notice,
	// and the second files a broadcast the first is still writing.
	store := newStore(t)

	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	if _, err := store.StartSession(at, sessionStale); err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	_, err := store.StartSession(at.Add(time.Second), sessionStale)
	if !errors.Is(err, ErrRecorderRunning) {
		t.Fatalf("the second StartSession() err = %v, want ErrRecorderRunning", err)
	}

	// A refused claim must write nothing. A row left behind reads as a
	// crash to the next start, which would report an outage that never was.
	sessions, err := store.SessionsBetween(at.Add(-time.Hour), at.Add(time.Hour))
	if err != nil {
		t.Fatalf("SessionsBetween() err = %v, want nil", err)
	}
	if len(sessions) != 1 {
		t.Errorf("the library holds %d sessions, want the refused one not recorded", len(sessions))
	}
}

func TestStartSession_SaysWhenAHeldLibraryFreesItself(t *testing.T) {
	// A recorder killed rather than stopped never closes its row, so the
	// claim outlives the process. That is what an operator meets after
	// stopping the scheduled task, and "a recorder is already running"
	// describes a process that is gone. Without the wait being named there
	// is nothing to do but guess.
	store := newStore(t)

	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	if _, err := store.StartSession(at, sessionStale); err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	_, err := store.StartSession(at.Add(30*time.Second), sessionStale)
	if err == nil {
		t.Fatal("StartSession() err = nil against a held library, want a refusal")
	}

	// The sentinel still answers, because callers branch on it.
	if !errors.Is(err, ErrRecorderRunning) {
		t.Errorf("err = %v, want it to wrap ErrRecorderRunning", err)
	}

	held, ok := errors.AsType[*RecorderHeldError](err)
	if !ok {
		t.Fatalf("err = %v, want a *RecorderHeldError carrying the claim", err)
	}
	if !held.HeartbeatAt.Equal(at) {
		t.Errorf("HeartbeatAt = %s, want the holder's last beat %s", held.HeartbeatAt, at)
	}
	if want := at.Add(sessionStale); !held.ClearsAt.Equal(want) {
		t.Errorf("ClearsAt = %s, want %s", held.ClearsAt, want)
	}

	// The message is the whole point, so it is asserted rather than the
	// fields alone.
	for _, want := range []string{"last seen 30s ago", "clears in"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to carry %q", err, want)
		}
	}
}

func TestRecorderHeldError_NeverReportsANegativeWait(t *testing.T) {
	// A clock that moved backwards, or a heartbeat written by a machine
	// running ahead, would otherwise print a negative age and read as
	// nonsense.
	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	held := &RecorderHeldError{
		HeartbeatAt: at.Add(time.Hour),
		ClearsAt:    at.Add(-time.Hour),
		At:          at,
	}

	if strings.Contains(held.Error(), "-") {
		t.Errorf("Error() = %q, want no negative duration", held.Error())
	}
}

func TestStartSession_ClaimsALibraryNoOneIsHolding(t *testing.T) {
	cases := []struct {
		name  string
		claim func(t *testing.T, store *Store, at time.Time)
	}{
		{
			name:  "no recorder ever ran",
			claim: func(*testing.T, *Store, time.Time) {},
		},
		{
			name: "the last recorder shut down cleanly",
			claim: func(t *testing.T, store *Store, at time.Time) {
				session, err := store.StartSession(at, sessionStale)
				if err != nil {
					t.Fatalf("StartSession() err = %v, want nil", err)
				}
				if err := store.StopSession(session.ID, at.Add(time.Minute)); err != nil {
					t.Fatalf("StopSession() err = %v, want nil", err)
				}
			},
		},
		{
			name: "the last recorder crashed and its heartbeat went stale",
			claim: func(t *testing.T, store *Store, at time.Time) {
				if _, err := store.StartSession(at, sessionStale); err != nil {
					t.Fatalf("StartSession() err = %v, want nil", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The clock is the argument, never a sleep. A stale heartbeat is
			// three minutes of silence and no test may wait for it.
			store := newStore(t)
			at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
			tc.claim(t, store, at)

			if _, err := store.StartSession(at.Add(time.Hour), sessionStale); err != nil {
				t.Errorf("StartSession() err = %v, want the library claimed", err)
			}
		})
	}
}

func TestStartSession_HoldsTheLibraryWhileTheHeartbeatIsFresh(t *testing.T) {
	// The heartbeat is the whole liveness signal, so a recorder that keeps
	// beating keeps the library however long it runs.
	store := newStore(t)

	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	session, err := store.StartSession(at, sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	beat := at.Add(6 * time.Hour)
	if err := store.Heartbeat(session.ID, beat); err != nil {
		t.Fatalf("Heartbeat() err = %v, want nil", err)
	}

	_, err = store.StartSession(beat.Add(time.Minute), sessionStale)
	if !errors.Is(err, ErrRecorderRunning) {
		t.Errorf("StartSession() err = %v, want ErrRecorderRunning while the heartbeat is fresh", err)
	}
}

func TestHeartbeat_ReportsASessionThatIsGone(t *testing.T) {
	// Rebuilding the database is the documented recovery path, and it
	// leaves the running daemon holding an id nothing answers to. Silently
	// succeeding there means it beats into nothing for the rest of the run,
	// and every day of that run paints unknown while the daemon was up.
	store := newStore(t)

	if err := store.Heartbeat(4242, broadcastStart); !errors.Is(err, ErrNotFound) {
		t.Errorf("Heartbeat() err = %v, want %v", err, ErrNotFound)
	}
}

func TestStopSession_ReportsASessionThatIsGone(t *testing.T) {
	store := newStore(t)

	if err := store.StopSession(4242, broadcastStart); !errors.Is(err, ErrNotFound) {
		t.Errorf("StopSession() err = %v, want %v", err, ErrNotFound)
	}
}

func TestLastSession_NoneBefore(t *testing.T) {
	store := newStore(t)

	first, err := store.StartSession(time.Now(), sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}
	if _, err := store.LastSession(first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("LastSession() err = %v, want it to wrap ErrNotFound", err)
	}
}

// ///////////////////////////////////////////////
// RecorderRunning
// ///////////////////////////////////////////////

func TestRecorderRunning_AgreesWithTheClaim(t *testing.T) {
	// A command that checks before acting must reach the same answer the
	// claim would. Two definitions of "a recorder is running" would let a
	// backfill decide the library is free while StartSession disagrees.
	store := newStore(t)
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const window = 90 * time.Second

	running, err := store.RecorderRunning(at, window)
	if err != nil {
		t.Fatalf("RecorderRunning() err = %v, want nil", err)
	}
	if running {
		t.Error("RecorderRunning() = true on an unclaimed library, want false")
	}

	if _, err := store.StartSession(at, window); err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	running, err = store.RecorderRunning(at, window)
	if err != nil {
		t.Fatalf("RecorderRunning() err = %v, want nil", err)
	}
	if !running {
		t.Error("RecorderRunning() = false while a session holds the library, want true")
	}

	// The claim itself must refuse at the same moment.
	if _, err := store.StartSession(at, window); !errors.Is(err, ErrRecorderRunning) {
		t.Errorf("StartSession() err = %v, want ErrRecorderRunning while one runs", err)
	}
}

func TestRecorderRunning_IgnoresASessionThatWentStale(t *testing.T) {
	// A daemon killed rather than stopped leaves its row behind. Counting
	// it would hold the library until somebody edited the database.
	store := newStore(t)
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	const window = 90 * time.Second

	if _, err := store.StartSession(at, window); err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	running, err := store.RecorderRunning(at.Add(10*time.Minute), window)
	if err != nil {
		t.Fatalf("RecorderRunning() err = %v, want nil", err)
	}
	if running {
		t.Error("RecorderRunning() = true for a heartbeat long past, want false")
	}
}

func TestReopenSession_ClosesTheOldRowAndOpensTheNewAsOneDecision(t *testing.T) {
	// StartSession puts its check and its insert in one immediate
	// transaction so nothing can slip between them. Stopping first from
	// outside that transaction hands away exactly the window it was built
	// to close: for one round trip the library is unclaimed.
	store := newStore(t)

	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	first, err := store.StartSession(at, sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	beat := at.Add(30 * time.Second)
	resumed := at.Add(5 * time.Minute)
	second, err := store.ReopenSession(first.ID, beat, resumed, sessionStale)
	if err != nil {
		t.Fatalf("ReopenSession() err = %v, want nil", err)
	}
	if second.ID == first.ID {
		t.Error("ReopenSession() returned the same session, want a new row")
	}

	// The library is still claimed afterwards, which is the whole point.
	if _, err := store.StartSession(resumed.Add(time.Second), sessionStale); !errors.Is(err, ErrRecorderRunning) {
		t.Errorf("StartSession() err = %v after a reopen, want the library still held", err)
	}

	// The frozen stretch belongs to neither session.
	sessions, err := store.SessionsBetween(at.Add(-time.Hour), resumed.Add(time.Hour))
	if err != nil {
		t.Fatalf("SessionsBetween() err = %v, want nil", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("SessionsBetween() returned %d sessions, want the old one and the new", len(sessions))
	}
	if sessions[0].StoppedAt == nil || !sessions[0].StoppedAt.Equal(beat.UTC()) {
		t.Errorf("the old session stopped at %v, want the last honest beat %v", sessions[0].StoppedAt, beat.UTC())
	}
}

func TestReopenSession_RefusesWhenAnotherRecorderHoldsTheLibrary(t *testing.T) {
	// The liveness check runs inside the same transaction as the stop, so
	// the row just closed cannot answer it and a live one still can.
	store := newStore(t)

	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	mine, err := store.StartSession(at, sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	// A second recorder that took the library while this one was frozen.
	if _, err := store.db.Exec(
		`INSERT INTO daemon_sessions (started_at, heartbeat_at) VALUES (?, ?)`,
		encodeTime(at.Add(4*time.Minute)), encodeTime(at.Add(4*time.Minute))); err != nil {
		t.Fatalf("staging the other recorder: %v", err)
	}

	_, err = store.ReopenSession(mine.ID, at.Add(30*time.Second), at.Add(5*time.Minute), sessionStale)
	if err == nil {
		t.Error("ReopenSession() took a library another recorder holds, want a refusal")
	}
}

func TestReopenSession_RefusesASessionThatIsNotThere(t *testing.T) {
	store := newStore(t)

	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	_, err := store.ReopenSession(999999, at, at.Add(time.Minute), sessionStale)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ReopenSession() err = %v, want ErrNotFound", err)
	}
}

func TestReopenSession_RefusesAnUnstorableTime(t *testing.T) {
	store := newStore(t)

	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	session, err := store.StartSession(at, sessionStale)
	if err != nil {
		t.Fatalf("StartSession() err = %v, want nil", err)
	}

	if _, err := store.ReopenSession(session.ID, time.Time{}, at, sessionStale); err == nil {
		t.Error("ReopenSession() accepted a zero stop time, want a refusal")
	}
	if _, err := store.ReopenSession(session.ID, at, time.Time{}, sessionStale); err == nil {
		t.Error("ReopenSession() accepted a zero start time, want a refusal")
	}
}

func TestFirstSessionStart_ReportsWhenTheLibraryWasFirstRecordedTo(t *testing.T) {
	// Automatic recovery reaches no further back than this, so a fresh
	// install cannot treat a channel's whole archive as something it
	// missed. The earliest start, not the newest, whatever order the rows
	// arrived in.
	store := newStore(t)
	first := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	for offset := range 3 {
		at := first.Add(time.Duration(offset) * 24 * time.Hour)
		session, err := store.StartSession(at, time.Hour)
		if err != nil {
			t.Fatalf("StartSession() err = %v, want nil", err)
		}
		if err := store.StopSession(session.ID, at.Add(time.Hour)); err != nil {
			t.Fatalf("StopSession() err = %v, want nil", err)
		}
	}

	got, err := store.FirstSessionStart()
	if err != nil {
		t.Fatalf("FirstSessionStart() err = %v, want nil", err)
	}
	if !got.Equal(first) {
		t.Errorf("FirstSessionStart() = %v, want %v", got, first)
	}
}

func TestFirstSessionStart_SaysSoWhenNoRecorderEverHeldTheLibrary(t *testing.T) {
	// The state of every library before its first start. The caller reads
	// it as "nothing could have been missed" rather than as a failure, so
	// the two must not look alike.
	store := newStore(t)

	if _, err := store.FirstSessionStart(); !errors.Is(err, ErrNotFound) {
		t.Errorf("FirstSessionStart() err = %v, want ErrNotFound", err)
	}
}
