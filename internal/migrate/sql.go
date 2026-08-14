package migrate

import (
	"database/sql"
	"fmt"
	"log/slog"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// SQLMigration represents a schema migration that upgrades a SQLite database
// from one version to the next.
type SQLMigration struct {
	// Version is the schema version this migration produces. Must be >= 2;
	// version 1 is the initial schema produced by [SQLRegistry.Init].
	Version int
	// Description is a short human-readable label for log output.
	Description string
	// Upgrade transforms the database schema within a transaction.
	Upgrade func(tx *sql.Tx) error
}

// SQLRegistry holds the initial schema and migrations for a single SQLite
// database. Version tracking uses PRAGMA user_version. A fresh database sits
// at user_version = 0 and receives [SQLRegistry.Init]. An existing one
// applies any pending [SQLMigration] in sequence.
type SQLRegistry struct {
	baseRegistry[SQLMigration]
	// Init creates the initial schema (tables, views, indexes) in a fresh
	// database. Runs inside a transaction. Required.
	Init func(tx *sql.Tx) error
}

// versionReader is the subset of *sql.DB and *sql.Tx that reading
// user_version needs, so the pooled read and the re-read inside a
// transaction share one implementation.
type versionReader interface {
	QueryRow(query string, args ...any) *sql.Row
}

// ///////////////////////////////////////////////
// SQLMigration methods
// ///////////////////////////////////////////////

func (m SQLMigration) vsn() int     { return m.Version }
func (m SQLMigration) desc() string { return m.Description }

// ///////////////////////////////////////////////
// SQLRegistry constructors
// ///////////////////////////////////////////////

// NewSQL constructs a [SQLRegistry] targeting schema version currentVersion.
// Chain With* setters for optional fields.
func NewSQL(currentVersion int) *SQLRegistry {
	return &SQLRegistry{
		baseRegistry: baseRegistry[SQLMigration]{CurrentVersion: currentVersion},
	}
}

// WithInit sets the initial-schema builder. Required before calling Run.
func (r *SQLRegistry) WithInit(fn func(tx *sql.Tx) error) *SQLRegistry {
	r.Init = fn
	return r
}

// WithLogger sets the logger used for migration progress messages.
func (r *SQLRegistry) WithLogger(l *slog.Logger) *SQLRegistry {
	r.Logger = l
	return r
}

// ///////////////////////////////////////////////
// SQLRegistry methods
// ///////////////////////////////////////////////

// Register appends a SQL migration. Panics on duplicate version or if
// version < 2 (use Init for the initial schema).
func (r *SQLRegistry) Register(m SQLMigration) {
	if m.Version < 2 {
		panic(fmt.Sprintf("migrate: SQL migration version must be >= 2 (got %d); use Init for initial schema", m.Version))
	}
	r.baseRegistry.Register(m)
}

// NeedsMigration reports whether the database needs any migrations applied.
// If the version cannot be read, on a closed database for instance, it
// returns an error.
func (r *SQLRegistry) NeedsMigration(db *sql.DB) (bool, error) {
	currentVersion, err := readUserVersion(db)
	if err != nil {
		return false, err
	}
	return r.checkVersion(currentVersion, false), nil
}

// Run initializes or upgrades the database schema. For a fresh database
// (user_version = 0), it runs Init and sets user_version to 1. For existing
// databases, it applies any pending migrations sequentially. A database
// newer than this build understands is refused.
//
// Every version decision is taken again inside the transaction that acts on
// it. The read that selects a path runs on a pooled connection, where two
// processes opening the same new database both see 0; only the transaction
// holds a lock, so only a decision taken under it can be trusted.
func (r *SQLRegistry) Run(db *sql.DB) error {
	if r.Init == nil {
		return fmt.Errorf("migrate: Init function is required")
	}

	currentVersion, err := readUserVersion(db)
	if err != nil {
		return err
	}
	if err := r.requireKnownVersion(currentVersion); err != nil {
		return err
	}

	if currentVersion < 0 {
		// user_version is a signed field in the SQLite header, so a corrupt
		// header or a foreign file using it as its own magic number lands
		// here. Treated as zero it would read as a fresh database, skip
		// initialization, and hand back a handle over a file with no tables.
		return fmt.Errorf("schema version is %d, so the database file is not one this tool wrote", currentVersion)
	}

	if currentVersion == 0 {
		if currentVersion, err = r.runInit(db); err != nil {
			return err
		}
		if err := r.requireKnownVersion(currentVersion); err != nil {
			return err
		}
	}

	for _, m := range r.Migrations {
		if currentVersion >= m.Version {
			continue
		}
		if currentVersion, err = r.applyMigration(db, m); err != nil {
			return err
		}
	}
	return nil
}

// RunDev applies each Dev migration inside its own transaction. No version
// advances. Use it to iterate on schema or data during development, before
// committing a real [SQLMigration].
//
// For a non-trivial schema change, prefer create-swap-drop (CREATE the new
// table, INSERT SELECT into it, DROP the old one, RENAME) over ALTER TABLE.
// An ALTER rewrites sqlite_master.sql in place, so accumulated ALTERs leave
// the DDL that .schema prints hard to read. Create-swap-drop leaves
// sqlite_master clean.
func (r *SQLRegistry) RunDev(db *sql.DB) error {
	for _, m := range r.Dev {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for dev transform %q: %w", m.Description, err)
		}
		if err := m.Upgrade(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("dev transform %q: %w", m.Description, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit dev transform %q: %w", m.Description, err) // coverage:partial (Commit never fails on in-memory SQLite)
		}
	}
	return nil
}

// ///////////////////////////////////////////////
// Internal helpers
// ///////////////////////////////////////////////

// readUserVersion reads the schema version from a database or a transaction.
func readUserVersion(from versionReader) (int, error) {
	var version int
	if err := from.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("reading user_version: %w", err)
	}
	return version, nil
}

// runInit applies the initial schema in a transaction, sets user_version to
// 1, and returns the version in force afterward.
//
// A caller that lost the race to build the schema finds a non-zero version
// here and reports it unchanged. The loser adopts the winner's database and
// never runs the initial schema over tables that already exist.
func (r *SQLRegistry) runInit(db *sql.DB) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx for init: %w", err) // coverage:partial (Begin never fails on live SQLite)
	}
	defer tx.Rollback() // no-op after Commit

	version, err := readUserVersion(tx)
	if err != nil {
		return 0, err
	}
	if version != 0 {
		return version, nil
	}

	if err := r.Init(tx); err != nil {
		return 0, fmt.Errorf("schema init failed: %w", err)
	}
	if _, err := tx.Exec("PRAGMA user_version = 1"); err != nil {
		return 0, fmt.Errorf("setting user_version to 1: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit init: %w", err) // coverage:partial (Commit never fails on in-memory SQLite)
	}
	return 1, nil
}

// applyMigration runs one migration inside a transaction and returns the
// version in force afterward. The deferred rollback scope is per-call so it
// runs immediately on error.
//
// A caller that finds the migration already applied reports the version it
// found, which is how two processes upgrading the same database at once
// leave one upgrade rather than two.
func (r *SQLRegistry) applyMigration(db *sql.DB, m SQLMigration) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx for migration v%d: %w", m.Version, err)
	}
	defer tx.Rollback() // no-op after Commit

	version, err := readUserVersion(tx)
	if err != nil {
		return 0, err
	}
	if version >= m.Version {
		return version, nil
	}

	logMigration(r.Logger, m.Version, m.Description)
	if err := m.Upgrade(tx); err != nil {
		return 0, fmt.Errorf("migration to v%d failed: %w", m.Version, err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
		return 0, fmt.Errorf("setting user_version to %d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit migration v%d: %w", m.Version, err) // coverage:partial (Commit never fails on in-memory SQLite)
	}
	return m.Version, nil
}
