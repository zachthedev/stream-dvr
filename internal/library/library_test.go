package library

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zach.tools/go/stream-dvr/internal/paths"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// otherOwner returns the lineage this binary does not own. BuildOwner is a
// compile-time constant, so a cross-ownership test cannot switch the
// binary. It writes a marker naming the opposite lineage instead, which
// exercises the refusal under either build tag.
func otherOwner() Owner {
	if BuildOwner == OwnerProd {
		return OwnerDev
	}
	return OwnerProd
}

// writeRawMarker plants arbitrary marker content under root, for cases a
// well-behaved Create would never produce.
func writeRawMarker(t *testing.T, root, content string) {
	t.Helper()

	if err := os.MkdirAll(paths.StateDir(root), 0o755); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}
	if err := os.WriteFile(paths.MarkerPath(root), []byte(content), 0o644); err != nil {
		t.Fatalf("writing marker: %v", err)
	}
}

// markerJSON renders a marker with the given owner and schema version.
func markerJSON(t *testing.T, owner Owner, schemaVersion int) string {
	t.Helper()

	data, err := json.Marshal(Marker{
		SchemaVersion: schemaVersion,
		Owner:         owner,
		CreatedAt:     time.Now().UTC(),
		CreatedBy:     "test",
	})
	if err != nil {
		t.Fatalf("encoding marker: %v", err)
	}
	return string(data)
}

// ///////////////////////////////////////////////
// Create
// ///////////////////////////////////////////////

func TestCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "library")

	lib, err := Create(root, "test")
	if err != nil {
		t.Fatalf("Create() err = %v, want nil", err)
	}

	if lib.Root() != root {
		t.Errorf("Root() = %q, want %q", lib.Root(), root)
	}
	if lib.Owner() != BuildOwner {
		t.Errorf("Owner() = %q, want %q", lib.Owner(), BuildOwner)
	}
	if lib.CreatedAt().IsZero() {
		t.Error("CreatedAt() is zero, want the creation time")
	}

	for _, dir := range []string{lib.StateDir(), lib.IncomingDir(), lib.TrashDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("stat %s: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", dir)
		}
	}
}

func TestCreate_RefusesExistingLibrary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "library")
	if _, err := Create(root, "test"); err != nil {
		t.Fatalf("first Create() err = %v, want nil", err)
	}

	_, err := Create(root, "test")
	if !errors.Is(err, ErrAlreadyLibrary) {
		t.Errorf("second Create() err = %v, want it to wrap ErrAlreadyLibrary", err)
	}
}

func TestCreate_LeavesNoTemporaryMarker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "library")
	if _, err := Create(root, "test"); err != nil {
		t.Fatalf("Create() err = %v, want nil", err)
	}

	// The marker is renamed into place so a crash cannot leave a truncated
	// file that makes the library unopenable.
	temp := paths.MarkerPath(root) + ".tmp"
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s = %v, want the temporary marker to be gone", temp, err)
	}
}

// ///////////////////////////////////////////////
// Open
// ///////////////////////////////////////////////

func TestOpen_RoundTripsCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "library")
	if _, err := Create(root, "test"); err != nil {
		t.Fatalf("Create() err = %v, want nil", err)
	}

	lib, err := Open(root)
	if err != nil {
		t.Fatalf("Open() err = %v, want nil", err)
	}
	if lib.Owner() != BuildOwner {
		t.Errorf("Owner() = %q, want %q", lib.Owner(), BuildOwner)
	}
}

func TestOpen_RefusesForeignOwner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "library")
	foreign := otherOwner()
	writeRawMarker(t, root, markerJSON(t, foreign, 1))

	_, err := Open(root)

	var ownership *OwnershipError
	if !errors.As(err, &ownership) {
		t.Fatalf("Open() err = %v, want an *OwnershipError", err)
	}
	if ownership.Want != BuildOwner {
		t.Errorf("OwnershipError.Want = %q, want %q", ownership.Want, BuildOwner)
	}
	if ownership.Got != foreign {
		t.Errorf("OwnershipError.Got = %q, want %q", ownership.Got, foreign)
	}
	if ownership.Root != root {
		t.Errorf("OwnershipError.Root = %q, want %q", ownership.Root, root)
	}

	// The message must name both lineages, or the operator cannot tell
	// which binary to reach for.
	message := ownership.Error()
	for _, want := range []string{root, string(BuildOwner), string(foreign)} {
		if !strings.Contains(message, want) {
			t.Errorf("OwnershipError.Error() = %q, want it to mention %q", message, want)
		}
	}
}

func TestOpen_Rejects(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr error
	}{
		{
			name:    "malformed json",
			content: "{not json",
			wantErr: nil,
		},
		{
			name:    "schema newer than this build",
			content: markerJSONLiteral(OwnerProd, markerVersion+1),
			wantErr: ErrUnknownSchema,
		},
		{
			name:    "unknown owner",
			content: markerJSONLiteral("staging", 1),
			wantErr: nil,
		},
		{
			name:    "empty owner",
			content: markerJSONLiteral("", 1),
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "library")
			writeRawMarker(t, root, tt.content)

			_, err := Open(root)
			if err == nil {
				t.Fatal("Open() err = nil, want a rejection")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Open() err = %v, want it to wrap %v", err, tt.wantErr)
			}
		})
	}
}

func TestOpen_MissingMarker(t *testing.T) {
	root := t.TempDir()

	_, err := Open(root)
	if !errors.Is(err, ErrNotLibrary) {
		t.Errorf("Open() err = %v, want it to wrap ErrNotLibrary", err)
	}
}

// markerJSONLiteral renders marker content without needing a *testing.T,
// so it can sit in a table literal.
func markerJSONLiteral(owner Owner, schemaVersion int) string {
	data, err := json.Marshal(Marker{
		SchemaVersion: schemaVersion,
		Owner:         owner,
		CreatedAt:     time.Unix(0, 0).UTC(),
		CreatedBy:     "test",
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}

// ///////////////////////////////////////////////
// Adopt
// ///////////////////////////////////////////////

func TestAdopt_PreservesExistingRecordings(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "ExampleChannel - 2026-03-04 21-15 - title.ts")
	if err := os.WriteFile(existing, []byte("recording"), 0o644); err != nil {
		t.Fatalf("seeding recording: %v", err)
	}

	lib, err := Adopt(root, "test")
	if err != nil {
		t.Fatalf("Adopt() err = %v, want nil", err)
	}
	if lib.Owner() != BuildOwner {
		t.Errorf("Owner() = %q, want %q", lib.Owner(), BuildOwner)
	}

	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("reading adopted recording: %v", err)
	}
	if string(content) != "recording" {
		t.Errorf("adopted recording = %q, want it untouched", content)
	}
}

func TestAdopt_Rejects(t *testing.T) {
	t.Run("missing path", func(t *testing.T) {
		_, err := Adopt(filepath.Join(t.TempDir(), "absent"), "test")
		if err == nil {
			t.Fatal("Adopt() err = nil, want a rejection")
		}
	})

	t.Run("path is a file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatalf("seeding file: %v", err)
		}

		_, err := Adopt(file, "test")
		if err == nil {
			t.Fatal("Adopt() err = nil, want a rejection")
		}
	})

	t.Run("already a library", func(t *testing.T) {
		root := t.TempDir()
		if _, err := Adopt(root, "test"); err != nil {
			t.Fatalf("first Adopt() err = %v, want nil", err)
		}

		_, err := Adopt(root, "test")
		if !errors.Is(err, ErrAlreadyLibrary) {
			t.Errorf("second Adopt() err = %v, want it to wrap ErrAlreadyLibrary", err)
		}
	})
}

// ///////////////////////////////////////////////
// Accessors
// ///////////////////////////////////////////////

func TestLibrary_Accessors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "library")
	lib, err := Create(root, "test")
	if err != nil {
		t.Fatalf("Create() err = %v, want nil", err)
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "StateDir", got: lib.StateDir(), want: paths.StateDir(root)},
		{name: "DatabasePath", got: lib.DatabasePath(), want: paths.DatabasePath(root)},
		{name: "IncomingDir", got: lib.IncomingDir(), want: paths.IncomingDir(root)},
		{name: "TrashDir", got: lib.TrashDir(), want: paths.TrashDir(root)},
		{name: "RelPath", got: lib.RelPath("ExampleChannel", "2026"), want: filepath.Join(root, "ExampleChannel", "2026")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
			if !strings.HasPrefix(tt.got, root) {
				t.Errorf("%s = %q, want it under the library root %q", tt.name, tt.got, root)
			}
		})
	}
}

// TestVerify covers the check every conclusion drawn from an absent file
// rests on.
//
// A network share unmounts and an external volume is unplugged without
// telling anybody, and the root then reads as a directory in which every
// recording is missing. Anything that would act on that, a purge or a
// coverage figure, asks this first, so an unreachable volume must not be
// mistaken for an empty library.
func TestVerify(t *testing.T) {
	t.Run("a library still where it was left", func(t *testing.T) {
		root := t.TempDir()
		lib, err := Create(root, "test")
		if err != nil {
			t.Fatalf("Create() err = %v, want nil", err)
		}

		if err := lib.Verify(); err != nil {
			t.Errorf("Verify() err = %v, want nil for a library that is still there", err)
		}
	})

	t.Run("a root whose marker is gone", func(t *testing.T) {
		// What an unmounted share looks like: the directory resolves and
		// holds nothing, which is indistinguishable from an empty library
		// until the marker is asked for.
		root := t.TempDir()
		lib, err := Create(root, "test")
		if err != nil {
			t.Fatalf("Create() err = %v, want nil", err)
		}
		if err := os.RemoveAll(paths.StateDir(root)); err != nil {
			t.Fatalf("removing the state directory: %v", err)
		}

		if err := lib.Verify(); err == nil {
			t.Error("Verify() err = nil, want a refusal once the marker is gone")
		}
	})

	t.Run("a root another build owns", func(t *testing.T) {
		// Two builds sharing a root would each purge against the other's
		// recordings, so the mismatch has to be named rather than ignored.
		root := t.TempDir()
		lib, err := Create(root, "test")
		if err != nil {
			t.Fatalf("Create() err = %v, want nil", err)
		}
		lib.marker.Owner = Owner("someone else")

		var mismatch *OwnershipError
		if err := lib.Verify(); !errors.As(err, &mismatch) {
			t.Errorf("Verify() err = %v, want an *OwnershipError naming both owners", err)
		}
	})
}
