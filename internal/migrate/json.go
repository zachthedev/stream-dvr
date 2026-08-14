package migrate

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// JSONMigration represents a schema migration for a JSON document.
// Upgrade receives the parsed document as a generic map.
type JSONMigration struct {
	// Version is the schema version this migration produces.
	Version int
	// Description is a short human-readable label for log output.
	Description string
	// Upgrade transforms a parsed JSON document.
	Upgrade func(doc map[string]any) (map[string]any, error)
}

// JSONRegistry holds the migrations for a single JSON schema target.
// A top-level key in the document holds the version, "version" by default.
// Chain [JSONRegistry.WithVersionKey] to override it.
type JSONRegistry struct {
	baseRegistry[JSONMigration]
	// VersionKey is the top-level key storing the schema version.
	// Defaults to "version" when empty.
	VersionKey string
}

// ///////////////////////////////////////////////
// JSONMigration methods
// ///////////////////////////////////////////////

func (m JSONMigration) vsn() int     { return m.Version }
func (m JSONMigration) desc() string { return m.Description }

// ///////////////////////////////////////////////
// JSONRegistry constructors
// ///////////////////////////////////////////////

// NewJSON constructs a [JSONRegistry] targeting schema version currentVersion.
// Chain With* setters for optional fields.
func NewJSON(currentVersion int) *JSONRegistry {
	return &JSONRegistry{
		baseRegistry: baseRegistry[JSONMigration]{CurrentVersion: currentVersion},
	}
}

// WithVersionKey overrides the default "version" key used to read/write the
// schema version in the JSON document.
func (r *JSONRegistry) WithVersionKey(key string) *JSONRegistry {
	r.VersionKey = key
	return r
}

// WithLogger sets the logger used for migration progress messages.
func (r *JSONRegistry) WithLogger(l *slog.Logger) *JSONRegistry {
	r.Logger = l
	return r
}

// ///////////////////////////////////////////////
// JSONRegistry methods
// ///////////////////////////////////////////////

// NeedsMigration reports whether the JSON document at data needs upgrading.
// An empty input is treated as version 0.
func (r *JSONRegistry) NeedsMigration(data []byte) (bool, error) {
	version, _, err := r.parse(data)
	if err != nil {
		return false, err
	}
	return r.checkVersion(version, false), nil
}

// Run parses the JSON document, applies pending migrations, writes the new
// version into the document, and returns the re-serialized bytes plus the
// final version reached. A document newer than this build understands is
// refused.
func (r *JSONRegistry) Run(data []byte) ([]byte, int, error) {
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
	doc[r.versionKey()] = version
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, version, fmt.Errorf("encoding JSON: %w", err)
	}
	return out, version, nil
}

// RunDev parses the document, applies each dev transform, and re-serializes.
// No version is advanced.
func (r *JSONRegistry) RunDev(data []byte) ([]byte, error) {
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
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding JSON: %w", err)
	}
	return out, nil
}

// ///////////////////////////////////////////////
// Internal helpers
// ///////////////////////////////////////////////

// versionKey returns the configured version key, defaulting to "version".
func (r *JSONRegistry) versionKey() string {
	if r.VersionKey == "" {
		return "version"
	}
	return r.VersionKey
}

// parse decodes the JSON document and extracts the version. An empty input
// returns version 0 with an empty map, which is what a file nothing has
// written carries. encoding/json decodes numbers into float64.
func (r *JSONRegistry) parse(data []byte) (int, map[string]any, error) {
	doc := map[string]any{}
	if len(data) == 0 {
		return 0, doc, nil
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return 0, nil, fmt.Errorf("parsing JSON: %w", err)
	}
	raw, ok := doc[r.versionKey()]
	if !ok {
		return 0, doc, nil
	}
	n, ok := raw.(float64)
	if !ok {
		return 0, nil, fmt.Errorf("reading %q: expected number, got %T", r.versionKey(), raw)
	}
	if n != float64(int(n)) {
		return 0, nil, fmt.Errorf("reading %q: non-integer value %v", r.versionKey(), n)
	}
	return int(n), doc, nil
}
