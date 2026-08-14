package migrate

import (
	"bytes"
	"fmt"
	"log/slog"

	"github.com/BurntSushi/toml"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// TOMLMigration represents a schema migration for a TOML document.
// Upgrade receives the parsed document as a generic map; returning a
// modified map or replacing it wholesale both work.
type TOMLMigration struct {
	// Version is the schema version this migration produces.
	Version int
	// Description is a short human-readable label for log output.
	Description string
	// Upgrade transforms a parsed TOML document.
	Upgrade func(doc map[string]any) (map[string]any, error)
}

// TOMLRegistry holds the migrations for a single TOML schema target.
// A top-level key in the document holds the version, "version" by default.
// Chain [TOMLRegistry.WithVersionKey] to override it.
type TOMLRegistry struct {
	baseRegistry[TOMLMigration]
	// VersionKey is the top-level key storing the schema version.
	// Defaults to "version" when empty.
	VersionKey string
}

// ///////////////////////////////////////////////
// TOMLMigration methods
// ///////////////////////////////////////////////

func (m TOMLMigration) vsn() int     { return m.Version }
func (m TOMLMigration) desc() string { return m.Description }

// ///////////////////////////////////////////////
// TOMLRegistry constructors
// ///////////////////////////////////////////////

// NewTOML constructs a [TOMLRegistry] targeting schema version currentVersion.
// Chain With* setters for optional fields.
func NewTOML(currentVersion int) *TOMLRegistry {
	return &TOMLRegistry{
		baseRegistry: baseRegistry[TOMLMigration]{CurrentVersion: currentVersion},
	}
}

// WithVersionKey overrides the default "version" key used to read/write the
// schema version in the TOML document.
func (r *TOMLRegistry) WithVersionKey(key string) *TOMLRegistry {
	r.VersionKey = key
	return r
}

// WithLogger sets the logger used for migration progress messages.
func (r *TOMLRegistry) WithLogger(l *slog.Logger) *TOMLRegistry {
	r.Logger = l
	return r
}

// ///////////////////////////////////////////////
// TOMLRegistry methods
// ///////////////////////////////////////////////

// NeedsMigration reports whether the TOML document at data needs upgrading.
// An empty input is treated as version 0.
func (r *TOMLRegistry) NeedsMigration(data []byte) (bool, error) {
	version, _, err := r.parse(data)
	if err != nil {
		return false, err
	}
	return r.checkVersion(version, false), nil
}

// Run parses the TOML document, applies pending migrations, writes the new
// version into the document, and returns the re-serialized bytes plus the
// final version reached. A document newer than this build understands is
// refused.
//
// Every comment in the source document is lost: the migration operates on a
// parsed map and the result is serialized from that map alone. A caller
// whose file carries documentation must regenerate it from its own template
// after migrating.
func (r *TOMLRegistry) Run(data []byte) ([]byte, int, error) {
	version, doc, err := r.parse(data)
	if err != nil {
		return nil, version, err
	}
	if err := r.requireKnownVersion(version); err != nil {
		return nil, version, err
	}
	for _, m := range r.Migrations {
		if version >= m.Version {
			continue
		}
		logMigration(r.Logger, m.Version, m.Description)
		doc, err = m.Upgrade(doc)
		if err != nil {
			return nil, version, fmt.Errorf("migration to v%d failed: %w", m.Version, err)
		}
		version = m.Version
	}
	doc[r.versionKey()] = int64(version)
	out, err := r.encode(doc)
	if err != nil {
		return nil, version, err
	}
	return out, version, nil
}

// RunDev parses the document, applies each dev transform, and re-serializes.
// No version is advanced.
func (r *TOMLRegistry) RunDev(data []byte) ([]byte, error) {
	_, doc, err := r.parse(data)
	if err != nil {
		return nil, err
	}
	for _, m := range r.Dev {
		doc, err = m.Upgrade(doc)
		if err != nil {
			return nil, fmt.Errorf("dev transform %q: %w", m.Description, err)
		}
	}
	return r.encode(doc)
}

// ///////////////////////////////////////////////
// Internal helpers
// ///////////////////////////////////////////////

// versionKey returns the configured version key, defaulting to "version".
func (r *TOMLRegistry) versionKey() string {
	if r.VersionKey == "" {
		return "version"
	}
	return r.VersionKey
}

// parse decodes the TOML document and extracts the version. An empty input
// returns version 0 with an empty map, which is what a file nothing has
// written carries. BurntSushi/toml decodes integers into int64.
func (r *TOMLRegistry) parse(data []byte) (int, map[string]any, error) {
	doc := map[string]any{}
	if len(data) == 0 {
		return 0, doc, nil
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return 0, nil, fmt.Errorf("parsing TOML: %w", err)
	}
	raw, ok := doc[r.versionKey()]
	if !ok {
		return 0, doc, nil
	}
	n, ok := raw.(int64)
	if !ok {
		return 0, nil, fmt.Errorf("reading %q: expected integer, got %T", r.versionKey(), raw)
	}
	return int(n), doc, nil
}

// encode serializes the TOML document from the parsed map, which holds
// keys and values and nothing else. See [TOMLRegistry.Run] on comments.
func (r *TOMLRegistry) encode(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return nil, fmt.Errorf("encoding TOML: %w", err)
	}
	return buf.Bytes(), nil
}
