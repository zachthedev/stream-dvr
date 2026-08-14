package migrate

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening in-memory SQLite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// openFileDB opens its own handle on a shared database file, with the
// pragmas a caller running against a real library uses. Each handle is a
// separate pool, so concurrent callers contend for the file lock exactly as
// separate processes do.
func openFileDB(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite",
		"file:"+url.PathEscape(path)+"?_pragma=busy_timeout(10000)&_txlock=immediate")
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// countingInit returns an Init that creates one table and counts how often
// it ran. Creating the same table twice fails, so a second call is both
// counted and reported.
func countingInit(runs *atomic.Int64) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		runs.Add(1)
		_, err := tx.Exec("CREATE TABLE items (id INTEGER PRIMARY KEY)")
		return err
	}
}

// ///////////////////////////////////////////////
// NewSQL + WithInit
// ///////////////////////////////////////////////

func TestNewSQL_SetsCurrentVersion(t *testing.T) {
	r := NewSQL(3)
	if r.CurrentVersion != 3 {
		t.Errorf("CurrentVersion = %d, want 3", r.CurrentVersion)
	}
}

func TestSQLRegistry_WithInit(t *testing.T) {
	fn := func(*sql.Tx) error { return nil }
	r := NewSQL(1).WithInit(fn)
	if r.Init == nil {
		t.Error("WithInit left Init nil")
	}
}

// ///////////////////////////////////////////////
// SQLRegistry.Register
// ///////////////////////////////////////////////

func TestSQLRegistry_Register_SortsByVersion(t *testing.T) {
	r := NewSQL(3)
	r.Register(SQLMigration{Version: 3, Description: "third"})
	r.Register(SQLMigration{Version: 2, Description: "second"})

	wantOrder := []int{2, 3}
	for i, m := range r.Migrations {
		if m.Version != wantOrder[i] {
			t.Errorf("Migrations[%d].Version = %d, want %d", i, m.Version, wantOrder[i])
		}
	}
}

func TestSQLRegistry_Register_DuplicateVersionPanics(t *testing.T) {
	r := NewSQL(2)
	r.Register(SQLMigration{Version: 2, Description: "first"})
	assertPanics(t, func() {
		r.Register(SQLMigration{Version: 2, Description: "duplicate"})
	})
}

func TestSQLRegistry_Register_Version1Panics(t *testing.T) {
	r := NewSQL(1)
	assertPanics(t, func() {
		r.Register(SQLMigration{Version: 1, Description: "use Init"})
	})
}

// ///////////////////////////////////////////////
// SQLRegistry.NeedsMigration
// ///////////////////////////////////////////////

func TestSQLRegistry_NeedsMigration_FreshDB(t *testing.T) {
	r := NewSQL(2)
	needs, err := r.NeedsMigration(openTestDB(t))
	if err != nil {
		t.Fatalf("NeedsMigration: %v", err)
	}
	if !needs {
		t.Error("NeedsMigration = false on fresh DB, want true")
	}
}

func TestSQLRegistry_NeedsMigration_UpToDate(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatalf("setting user_version: %v", err)
	}
	r := NewSQL(2)
	needs, err := r.NeedsMigration(db)
	if err != nil {
		t.Fatalf("NeedsMigration: %v", err)
	}
	if needs {
		t.Error("NeedsMigration = true when at current version, want false")
	}
}

func TestSQLRegistry_NeedsMigration_ClosedDB(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening DB: %v", err)
	}
	db.Close()

	r := NewSQL(1)
	if _, err := r.NeedsMigration(db); err == nil {
		t.Error("NeedsMigration error = nil, want error on closed DB")
	}
}

// ///////////////////////////////////////////////
// SQLRegistry.Run
// ///////////////////////////////////////////////

func TestSQLRegistry_Run_FreshDBRunsInit(t *testing.T) {
	db := openTestDB(t)
	r := NewSQL(1).WithInit(func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE items (id INTEGER PRIMARY KEY)")
		return err
	})

	if err := r.Run(db); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("user_version = %d, want 1", version)
	}
	if _, err := db.Exec("INSERT INTO items (id) VALUES (1)"); err != nil {
		t.Errorf("insert failed: %v", err)
	}
}

func TestSQLRegistry_Run_ConcurrentCallersInitialiseOnce(t *testing.T) {
	// Starting the daemon and opening the TUI on a new library runs this
	// path twice at once. Every caller must come away with a usable
	// database, and the schema must be built exactly once.
	path := filepath.Join(t.TempDir(), "library.db")

	const callers = 8
	var (
		runs   atomic.Int64
		start  sync.WaitGroup
		finish sync.WaitGroup
		errs   = make([]error, callers)
	)
	start.Add(1)
	for i := range callers {
		db := openFileDB(t, path)
		r := NewSQL(1).WithInit(countingInit(&runs))
		finish.Go(func() {
			start.Wait()
			errs[i] = r.Run(db)
		})
	}
	start.Done()
	finish.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: Run() err = %v, want nil", i, err)
		}
	}
	if got := runs.Load(); got != 1 {
		t.Errorf("Init ran %d times, want 1", got)
	}

	db := openFileDB(t, path)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("user_version = %d, want 1", version)
	}
}

func TestSQLRegistry_Run_ConcurrentCallersMigrateOnce(t *testing.T) {
	// The same race applies to every later upgrade, where running a
	// migration twice corrupts rather than merely erroring.
	path := filepath.Join(t.TempDir(), "library.db")
	if err := NewSQL(1).WithInit(func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE items (id INTEGER PRIMARY KEY, seen INTEGER NOT NULL DEFAULT 0)")
		return err
	}).Run(openFileDB(t, path)); err != nil {
		t.Fatalf("Run() err = %v, want nil", err)
	}

	const callers = 8
	var (
		upgrades atomic.Int64
		start    sync.WaitGroup
		finish   sync.WaitGroup
		errs     = make([]error, callers)
	)
	start.Add(1)
	for i := range callers {
		db := openFileDB(t, path)
		r := NewSQL(2).WithInit(func(*sql.Tx) error { return nil })
		r.Register(SQLMigration{
			Version:     2,
			Description: "count the upgrade",
			Upgrade: func(tx *sql.Tx) error {
				upgrades.Add(1)
				_, err := tx.Exec("INSERT INTO items (id) VALUES (1)")
				return err
			},
		})
		finish.Go(func() {
			start.Wait()
			errs[i] = r.Run(db)
		})
	}
	start.Done()
	finish.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: Run() err = %v, want nil", i, err)
		}
	}
	if got := upgrades.Load(); got != 1 {
		t.Errorf("migration ran %d times, want 1", got)
	}
}

func TestSQLRegistry_Run_RefusesANewerDatabase(t *testing.T) {
	// A database written by a newer build holds columns and states this one
	// cannot read. Opening it anyway is how a library gets quietly
	// downgraded.
	tests := []struct {
		name      string
		stored    int
		target    int
		migration int
		wantErr   bool
	}{
		{name: "same version", stored: 1, target: 1, wantErr: false},
		{name: "older database", stored: 1, target: 3, migration: 3, wantErr: false},
		{name: "newer database", stored: 99, target: 1, wantErr: true},
		{name: "one past a registered migration", stored: 4, target: 1, migration: 3, wantErr: true},
		{name: "at a registered migration past the target", stored: 3, target: 1, migration: 3, wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", tt.stored)); err != nil {
				t.Fatalf("seeding user_version: %v", err)
			}

			r := NewSQL(tt.target).WithInit(func(*sql.Tx) error { return nil })
			if tt.migration >= 2 {
				r.Register(SQLMigration{
					Version:     tt.migration,
					Description: "no-op",
					Upgrade:     func(*sql.Tx) error { return nil },
				})
			}

			err := r.Run(db)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Run() err = nil, want a refusal for a database at version %d", tt.stored)
				}
				if !strings.Contains(err.Error(), "upgrade the application") {
					t.Errorf("Run() err = %v, want it to say how to fix the mismatch", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Run() err = %v, want nil", err)
			}
		})
	}
}

func TestSQLRegistry_Run_AppliesMigrations(t *testing.T) {
	db := openTestDB(t)
	r := NewSQL(2).WithInit(func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)")
		return err
	})
	r.Register(SQLMigration{
		Version:     2,
		Description: "add column",
		Upgrade: func(tx *sql.Tx) error {
			_, err := tx.Exec("ALTER TABLE items ADD COLUMN value TEXT DEFAULT ''")
			return err
		},
	})

	if err := r.Run(db); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version != 2 {
		t.Errorf("user_version = %d, want 2", version)
	}
	if _, err := db.Exec("INSERT INTO items (id, name, value) VALUES (1, 'test', 'val')"); err != nil {
		t.Errorf("insert failed after migration: %v", err)
	}
}

func TestSQLRegistry_Run_SkipsAppliedMigrations(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("setting user_version: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE items (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating table: %v", err)
	}

	initCalled := false
	r := NewSQL(1).WithInit(func(tx *sql.Tx) error {
		initCalled = true
		return nil
	})
	if err := r.Run(db); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if initCalled {
		t.Error("Init ran on an existing DB, should be skipped")
	}
}

func TestSQLRegistry_Run_RollsBackOnFailure(t *testing.T) {
	db := openTestDB(t)
	r := NewSQL(2).WithInit(func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE items (id INTEGER PRIMARY KEY)")
		return err
	})
	r.Register(SQLMigration{
		Version:     2,
		Description: "fails",
		Upgrade: func(tx *sql.Tx) error {
			return fmt.Errorf("deliberate failure")
		},
	})

	err := r.Run(db)
	if err == nil {
		t.Fatal("Run error = nil, want error")
	}
	if !strings.Contains(err.Error(), "deliberate failure") {
		t.Errorf("Run error = %v, want to contain %q", err, "deliberate failure")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("user_version = %d, want 1 (v2 rolled back)", version)
	}
}

func TestSQLRegistry_Run_NilInitErrors(t *testing.T) {
	db := openTestDB(t)
	r := NewSQL(1)
	if err := r.Run(db); err == nil {
		t.Error("Run error = nil, want error when Init is nil")
	}
}

func TestSQLRegistry_Run_InitErrorLeavesVersionZero(t *testing.T) {
	db := openTestDB(t)
	r := NewSQL(1).WithInit(func(tx *sql.Tx) error {
		return fmt.Errorf("init failed")
	})
	if err := r.Run(db); err == nil {
		t.Fatal("Run error = nil, want error from Init")
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version != 0 {
		t.Errorf("user_version = %d, want 0 (init rolled back)", version)
	}
}

func TestSQLRegistry_Run_ClosedDBErrors(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("opening DB: %v", err)
	}
	db.Close()

	r := NewSQL(1).WithInit(func(*sql.Tx) error { return nil })
	if err := r.Run(db); err == nil {
		t.Error("Run error = nil, want error on closed DB")
	}
}

// ///////////////////////////////////////////////
// SQLRegistry.RunDev + Dev
// ///////////////////////////////////////////////

func TestSQLRegistry_RunDev_AppliesInRegistrationOrder(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec("CREATE TABLE counter (n INTEGER DEFAULT 0)"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := db.Exec("INSERT INTO counter (n) VALUES (0)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := NewSQL(1)
	r.RegisterDev(SQLMigration{
		Description: "add 1",
		Upgrade: func(tx *sql.Tx) error {
			_, err := tx.Exec("UPDATE counter SET n = n + 1")
			return err
		},
	})
	r.RegisterDev(SQLMigration{
		Description: "multiply 10",
		Upgrade: func(tx *sql.Tx) error {
			_, err := tx.Exec("UPDATE counter SET n = n * 10")
			return err
		},
	})

	if err := r.RunDev(db); err != nil {
		t.Fatalf("RunDev: %v", err)
	}
	var n int
	if err := db.QueryRow("SELECT n FROM counter").Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 10 {
		t.Errorf("counter = %d, want 10 (add1 then *10)", n)
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version != 0 {
		t.Errorf("user_version = %d, want 0 (RunDev should not bump)", version)
	}
}

func TestSQLRegistry_RunDev_RollsBackOnError(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec("CREATE TABLE counter (n INTEGER DEFAULT 0)"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := db.Exec("INSERT INTO counter (n) VALUES (5)"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r := NewSQL(1)
	r.RegisterDev(SQLMigration{
		Description: "boom",
		Upgrade: func(tx *sql.Tx) error {
			if _, err := tx.Exec("UPDATE counter SET n = 99"); err != nil {
				return err
			}
			return fmt.Errorf("deliberate failure")
		},
	})

	if err := r.RunDev(db); err == nil {
		t.Fatal("RunDev error = nil, want error")
	}
	var n int
	if err := db.QueryRow("SELECT n FROM counter").Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 5 {
		t.Errorf("counter = %d, want 5 (update should have rolled back)", n)
	}
}
