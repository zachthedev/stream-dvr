// Package migrate applies sequential schema migrations to on-disk data.
// Four registry types cover the common file formats, and each one works on
// its own.
//
//   - [BytesRegistry]: format-agnostic. The caller reads and writes the
//     file, and extracts the version field.
//
//   - [SQLRegistry]: for SQLite databases. Version tracking uses
//     PRAGMA user_version, and every migration runs inside a transaction.
//
//   - [TOMLRegistry]: for TOML documents with a top-level version key.
//     Migrations operate on a parsed map[string]any.
//
//   - [JSONRegistry]: the same as TOMLRegistry, for JSON documents.
//
// An embedded generic base gives all four the same fields: CurrentVersion,
// Migrations, Dev, and Logger. A factory function builds each one
// (NewBytes, NewSQL, NewTOML, NewJSON), and fluent With* setters cover the
// optional fields.
//
// Each schema target gets its own registry, so version numbers and
// migration lists stay independent across targets.
//
// # Dev transforms
//
// Every registry carries an optional Dev slice for development-only
// transforms that run without advancing the schema version. RunDev applies
// them. Gate that call behind a dev-mode flag, and clear the entries out
// before committing a real migration.
//
// # Extensibility
//
// To add a format the built-ins do not cover, such as YAML or protobuf,
// define a migration type with vsn and desc methods plus a registry type
// that embeds baseRegistry. Register, RegisterDev, and HasDev come with the
// embed. Run, RunDev, and NeedsMigration are format-specific, so the new
// type implements those itself.
package migrate

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// versioned is the minimal interface every migration type satisfies, so the
// generic base can read its metadata.
type versioned interface {
	vsn() int
	desc() string
}

// baseRegistry holds the fields and methods shared by every format-specific
// registry. Each concrete registry embeds baseRegistry[M] with its own
// migration type M.
type baseRegistry[M versioned] struct {
	// CurrentVersion is the latest schema version this registry targets.
	CurrentVersion int
	// Migrations is the ordered list of versioned upgrades.
	Migrations []M
	// Dev holds development-only transforms that run without advancing the
	// schema version. RegisterDev populates it.
	Dev []M
	// Logger receives migration progress. If nil, slog.Default() does.
	Logger *slog.Logger
}

// BytesMigration upgrades serialized data from one version to the next.
// Format-agnostic: the caller handles parsing and serializing.
type BytesMigration struct {
	// Version is the schema version this migration produces.
	Version int
	// Description is a short human-readable label for log output.
	Description string
	// Upgrade transforms data from the prior version to [BytesMigration.Version].
	Upgrade func(data []byte) ([]byte, error)
}

// BytesRegistry holds the version and migrations for a single byte-based
// schema target. The caller is responsible for reading/writing the file and
// for extracting the current version from the serialized data.
type BytesRegistry struct {
	baseRegistry[BytesMigration]
}

// ///////////////////////////////////////////////
// baseRegistry methods (shared across all registries)
// ///////////////////////////////////////////////

// Register appends a migration and maintains sorted order by version.
// Panics on duplicate version.
func (b *baseRegistry[M]) Register(m M) {
	for _, existing := range b.Migrations {
		if existing.vsn() == m.vsn() {
			panic(fmt.Sprintf("migrate: duplicate migration version %d (description: %q)", m.vsn(), m.desc()))
		}
	}
	b.Migrations = append(b.Migrations, m)
	slices.SortFunc(b.Migrations, func(a, c M) int {
		return cmp.Compare(a.vsn(), c.vsn())
	})
}

// RegisterDev appends a dev-only transform. Panics on duplicate description.
func (b *baseRegistry[M]) RegisterDev(m M) {
	for _, existing := range b.Dev {
		if existing.desc() == m.desc() {
			panic(fmt.Sprintf("migrate: duplicate dev transform %q", m.desc()))
		}
	}
	b.Dev = append(b.Dev, m)
}

// HasDev reports whether any dev transforms are registered.
func (b *baseRegistry[M]) HasDev() bool {
	return len(b.Dev) > 0
}

// requireKnownVersion rejects data written by a newer build than this one.
//
// Migration only ever runs forward, so a document or database from the
// future has no path back to a shape this build understands. Reading it
// anyway drops whatever the newer format added, and writing it back makes
// that loss permanent.
func (b *baseRegistry[M]) requireKnownVersion(fileVersion int) error {
	highest := b.CurrentVersion
	for _, m := range b.Migrations {
		if m.vsn() > highest {
			highest = m.vsn()
		}
	}
	if fileVersion <= highest {
		return nil
	}
	return fmt.Errorf("schema version is %d but this build understands at most %d; upgrade the application",
		fileVersion, highest)
}

// checkVersion reports whether fileVersion is behind CurrentVersion or any
// registered migration. force=true reports true whenever migrations exist.
// Concrete registries call this from their format-specific NeedsMigration.
func (b *baseRegistry[M]) checkVersion(fileVersion int, force bool) bool {
	if fileVersion < b.CurrentVersion {
		return true
	}
	if force && len(b.Migrations) > 0 {
		return true
	}
	for _, m := range b.Migrations {
		if fileVersion < m.vsn() {
			return true
		}
	}
	return false
}

// ///////////////////////////////////////////////
// BytesMigration methods
// ///////////////////////////////////////////////

func (m BytesMigration) vsn() int     { return m.Version }
func (m BytesMigration) desc() string { return m.Description }

// ///////////////////////////////////////////////
// BytesRegistry constructors
// ///////////////////////////////////////////////

// NewBytes constructs a [BytesRegistry] targeting schema version currentVersion.
// Chain With* setters for optional fields.
func NewBytes(currentVersion int) *BytesRegistry {
	return &BytesRegistry{
		baseRegistry: baseRegistry[BytesMigration]{CurrentVersion: currentVersion},
	}
}

// WithLogger sets the logger used for migration progress messages.
func (r *BytesRegistry) WithLogger(l *slog.Logger) *BytesRegistry {
	r.Logger = l
	return r
}

// ///////////////////////////////////////////////
// BytesRegistry methods
// ///////////////////////////////////////////////

// NeedsMigration reports whether a file at fileVersion needs upgrading.
// Pass force = true to report true whenever any migration is registered,
// whatever the version.
func (r *BytesRegistry) NeedsMigration(fileVersion int, force bool) bool {
	return r.checkVersion(fileVersion, force)
}

// Run applies registered migrations sequentially where fromVersion < m.Version.
// Returns the transformed data and the final version reached. Data newer
// than this build understands is refused.
func (r *BytesRegistry) Run(data []byte, fromVersion int) ([]byte, int, error) {
	if err := r.requireKnownVersion(fromVersion); err != nil {
		return nil, fromVersion, err
	}

	version := fromVersion
	for _, m := range r.Migrations {
		if version < m.Version {
			logMigration(r.Logger, m.Version, m.Description)
			var err error
			data, err = m.Upgrade(data)
			if err != nil {
				return nil, version, fmt.Errorf("migration to v%d failed: %w", m.Version, err)
			}
			version = m.Version
		}
	}
	return data, version, nil
}

// RunDev applies dev transforms in the order they were registered. No
// version advances. Callers use this for local data fixes during
// development.
func (r *BytesRegistry) RunDev(data []byte) ([]byte, error) {
	for _, m := range r.Dev {
		var err error
		data, err = m.Upgrade(data)
		if err != nil {
			return nil, fmt.Errorf("dev transform %q: %w", m.Description, err)
		}
	}
	return data, nil
}

// ///////////////////////////////////////////////
// Shared helpers
// ///////////////////////////////////////////////

// logMigration logs a migration step. Every registry type calls it, so the
// log format stays consistent. If logger is nil, it logs to slog.Default().
func logMigration(logger *slog.Logger, version int, description string) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("applying migration", slog.Int("version", version), slog.String("description", description))
}
