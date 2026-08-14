// Package library manages the on-disk recording library and the ownership
// marker that gates access to it.
//
// A library is any directory holding a .dvr/library.json marker. The marker
// names the build that owns it. A development build cannot open a production
// library and a production build cannot open a development sandbox, because
// the owner is fixed at compile time by a build tag rather than by config.
// Configuration can be edited or mistyped. A build tag cannot.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"zach.tools/go/stream-dvr/internal/fsretry"
	"zach.tools/go/stream-dvr/internal/paths"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Owner identifies the build lineage that may open a library.
type Owner string

// Marker is the on-disk ownership record at .dvr/library.json.
type Marker struct {
	SchemaVersion int       `json:"schema_version"`
	Owner         Owner     `json:"owner"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by"`
}

// Library is an opened recording library rooted at a directory.
type Library struct {
	root   string
	marker Marker
}

// OwnershipError reports a build attempting to open a library belonging to
// the other lineage.
type OwnershipError struct {
	Root string
	Want Owner
	Got  Owner
}

// Owner values. BuildOwner selects one of these at compile time.
const (
	// OwnerProd marks a library written by a released binary.
	OwnerProd Owner = "prod"
	// OwnerDev marks a sandbox written by a binary built with -tags dev.
	OwnerDev Owner = "dev"
)

// markerVersion is the marker schema version. Bump it when the marker
// gains a field that older binaries must not silently ignore.
const markerVersion = 1

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

var (
	// ErrNotLibrary reports a directory with no ownership marker. Claim it
	// deliberately with Adopt rather than opening it.
	ErrNotLibrary = errors.New("not a library: no ownership marker")

	// ErrAlreadyLibrary reports an attempt to initialize over an existing
	// marker.
	ErrAlreadyLibrary = errors.New("already a library: ownership marker exists")

	// ErrUnknownSchema reports a marker written by a newer binary.
	ErrUnknownSchema = errors.New("library marker schema is newer than this build")
)

// Error implements error.
func (e *OwnershipError) Error() string {
	return fmt.Sprintf("library %s is owned by the %s build, this is the %s build", e.Root, e.Got, e.Want)
}

// ///////////////////////////////////////////////
// Constructors
// ///////////////////////////////////////////////

// Open reads an existing library and verifies this build owns it.
//
// Returns ErrNotLibrary when no marker exists, and *OwnershipError when the
// marker names the other build lineage.
func Open(root string) (*Library, error) {
	marker, err := readMarker(root)
	if err != nil {
		return nil, err
	}
	if marker.Owner != BuildOwner {
		return nil, &OwnershipError{Root: root, Want: BuildOwner, Got: marker.Owner}
	}
	return &Library{root: root, marker: marker}, nil
}

// Create initializes a new library owned by this build and lays out its
// directories. It fails with ErrAlreadyLibrary if a marker is present, so
// it never reassigns ownership of an existing library.
func Create(root, createdBy string) (*Library, error) {
	if _, err := os.Stat(paths.MarkerPath(root)); err == nil {
		return nil, fmt.Errorf("creating library at %s: %w", root, ErrAlreadyLibrary)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("checking marker at %s: %w", root, err)
	}

	for _, dir := range []string{paths.StateDir(root), paths.IncomingDir(root), paths.TrashDir(root)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	marker := Marker{
		SchemaVersion: markerVersion,
		Owner:         BuildOwner,
		CreatedAt:     time.Now().UTC(),
		CreatedBy:     createdBy,
	}
	if err := writeMarker(root, marker); err != nil {
		return nil, err
	}
	return &Library{root: root, marker: marker}, nil
}

// Adopt claims an unmarked directory of existing recordings for this build.
// It is the only way a populated directory becomes a library, and it exists
// so that claiming one is always a deliberate act.
//
// Returns ErrAlreadyLibrary when a marker is already present.
func Adopt(root, createdBy string) (*Library, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("adopting %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("adopting %s: not a directory", root)
	}
	return Create(root, createdBy)
}

// ///////////////////////////////////////////////
// Accessors
// ///////////////////////////////////////////////

// Root returns the library's root directory.
func (l *Library) Root() string { return l.root }

// Owner returns the build lineage that owns the library.
func (l *Library) Owner() Owner { return l.marker.Owner }

// CreatedAt returns when the library was initialized.
func (l *Library) CreatedAt() time.Time { return l.marker.CreatedAt }

// StateDir returns the library's state directory.
func (l *Library) StateDir() string { return paths.StateDir(l.root) }

// DatabasePath returns the library's SQLite database path.
func (l *Library) DatabasePath() string { return paths.DatabasePath(l.root) }

// IncomingDir returns the directory holding in-progress captures.
func (l *Library) IncomingDir() string { return paths.IncomingDir(l.root) }

// TrashDir returns the directory holding deleted recordings during their
// grace period.
func (l *Library) TrashDir() string { return paths.TrashDir(l.root) }

// Verify re-reads the ownership marker, reporting whether the library is
// still there and still owned by the build that opened it.
//
// An open library is a handle to a directory that can go away underneath
// it: a network share unmounts, an external volume is unplugged, and the
// root then reads as a directory where every recording is missing. Anything
// that concludes something from a file being absent asks this first, so an
// unreachable volume cannot be mistaken for an empty library.
func (l *Library) Verify() error {
	marker, err := readMarker(l.root)
	if err != nil {
		return err
	}
	if marker.Owner != l.marker.Owner {
		return &OwnershipError{Root: l.root, Want: l.marker.Owner, Got: marker.Owner}
	}
	return nil
}

// ///////////////////////////////////////////////
// Marker persistence
// ///////////////////////////////////////////////

// readMarker loads and validates the ownership marker under root.
func readMarker(root string) (Marker, error) {
	data, err := os.ReadFile(paths.MarkerPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return Marker{}, fmt.Errorf("opening library at %s: %w", root, ErrNotLibrary)
	}
	if err != nil {
		return Marker{}, fmt.Errorf("reading marker at %s: %w", root, err)
	}

	var marker Marker
	if err := json.Unmarshal(data, &marker); err != nil {
		return Marker{}, fmt.Errorf("parsing marker at %s: %w", root, err)
	}
	if marker.SchemaVersion > markerVersion {
		return Marker{}, fmt.Errorf("marker at %s is version %d: %w", root, marker.SchemaVersion, ErrUnknownSchema)
	}
	if marker.Owner != OwnerProd && marker.Owner != OwnerDev {
		return Marker{}, fmt.Errorf("marker at %s has unknown owner %q", root, marker.Owner)
	}
	return marker, nil
}

// writeMarker persists the ownership marker under root.
//
// The write is atomic, so a crash or a full disk part way through cannot
// leave a truncated marker. Every open of the library reads this file
// first, and one that will not parse locks the operator out of their own
// recordings.
func writeMarker(root string, marker Marker) error {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding marker: %w", err)
	}
	data = append(data, '\n')

	final := paths.MarkerPath(root)
	// World-readable on purpose. The marker names the build that owns the
	// library and carries no secret, and a library on shared storage has to
	// stay readable by whoever mounts it.
	if err := fsretry.WriteFileAtomic(context.Background(), final, data, 0o644); err != nil {
		return fmt.Errorf("writing marker at %s: %w", final, err)
	}
	return nil
}

// RelPath returns a path inside the library, joined to its root.
func (l *Library) RelPath(parts ...string) string {
	return filepath.Join(append([]string{l.root}, parts...)...)
}
