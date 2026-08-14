package migrate

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	fn()
}

// ///////////////////////////////////////////////
// NewBytes + WithLogger
// ///////////////////////////////////////////////

func TestNewBytes_SetsCurrentVersion(t *testing.T) {
	r := NewBytes(5)
	if r.CurrentVersion != 5 {
		t.Errorf("CurrentVersion = %d, want 5", r.CurrentVersion)
	}
}

func TestBytesRegistry_WithLogger(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	r := NewBytes(1).WithLogger(l)
	if r.Logger != l {
		t.Error("WithLogger did not assign the logger")
	}
}

// ///////////////////////////////////////////////
// BytesRegistry.Register
// ///////////////////////////////////////////////

func TestBytesRegistry_Register_SortsByVersion(t *testing.T) {
	r := NewBytes(3)
	r.Register(BytesMigration{Version: 3, Description: "third"})
	r.Register(BytesMigration{Version: 1, Description: "first"})
	r.Register(BytesMigration{Version: 2, Description: "second"})

	wantOrder := []int{1, 2, 3}
	for i, m := range r.Migrations {
		if m.Version != wantOrder[i] {
			t.Errorf("Migrations[%d].Version = %d, want %d", i, m.Version, wantOrder[i])
		}
	}
}

func TestBytesRegistry_Register_DuplicatePanics(t *testing.T) {
	r := NewBytes(1)
	r.Register(BytesMigration{Version: 1, Description: "first"})
	assertPanics(t, func() {
		r.Register(BytesMigration{Version: 1, Description: "duplicate"})
	})
}

// ///////////////////////////////////////////////
// BytesRegistry.NeedsMigration
// ///////////////////////////////////////////////

func TestBytesRegistry_NeedsMigration(t *testing.T) {
	r := NewBytes(2)
	r.Register(BytesMigration{Version: 2, Upgrade: identityUpgrade})

	tests := []struct {
		name    string
		version int
		force   bool
		want    bool
	}{
		{name: "behind", version: 0, want: true},
		{name: "one behind", version: 1, want: true},
		{name: "current", version: 2, want: false},
		{name: "ahead", version: 3, want: false},
		{name: "current with force", version: 2, force: true, want: true},
		{name: "ahead with force", version: 3, force: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.NeedsMigration(tt.version, tt.force); got != tt.want {
				t.Errorf("NeedsMigration(%d, %v) = %v, want %v", tt.version, tt.force, got, tt.want)
			}
		})
	}
}

func TestBytesRegistry_NeedsMigration_ForceWithNoMigrations(t *testing.T) {
	r := NewBytes(1)
	if r.NeedsMigration(1, true) {
		t.Error("NeedsMigration with force but no migrations = true, want false")
	}
}

// ///////////////////////////////////////////////
// BytesRegistry.Run
// ///////////////////////////////////////////////

func TestBytesRegistry_Run_AppliesPendingMigrations(t *testing.T) {
	r := NewBytes(2)
	r.Register(BytesMigration{
		Version:     1,
		Description: "init",
		Upgrade: func(data []byte) ([]byte, error) {
			return append(data, []byte(":v1")...), nil
		},
	})
	r.Register(BytesMigration{
		Version:     2,
		Description: "extend",
		Upgrade: func(data []byte) ([]byte, error) {
			return append(data, []byte(":v2")...), nil
		},
	})

	out, version, err := r.Run([]byte("data"), 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(out), "data:v1:v2"; got != want {
		t.Errorf("Run data = %q, want %q", got, want)
	}
	if got, want := version, 2; got != want {
		t.Errorf("Run version = %d, want %d", got, want)
	}
}

func TestBytesRegistry_Run_RefusesANewerVersion(t *testing.T) {
	r := NewBytes(1)

	if _, _, err := r.Run([]byte("data"), 99); err == nil {
		t.Error("Run err = nil, want a refusal for data at version 99")
	}
}

func TestBytesRegistry_Run_SkipsAppliedVersions(t *testing.T) {
	r := NewBytes(2)
	r.Register(BytesMigration{
		Version:     1,
		Description: "should be skipped",
		Upgrade: func(data []byte) ([]byte, error) {
			t.Errorf("v1 upgrade should not run when fromVersion >= 1")
			return data, nil
		},
	})
	r.Register(BytesMigration{
		Version:     2,
		Description: "runs",
		Upgrade: func(data []byte) ([]byte, error) {
			return []byte("done"), nil
		},
	})

	out, version, err := r.Run([]byte("start"), 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := string(out), "done"; got != want {
		t.Errorf("Run data = %q, want %q", got, want)
	}
	if version != 2 {
		t.Errorf("Run version = %d, want 2", version)
	}
}

func TestBytesRegistry_Run_ErrorStopsProgress(t *testing.T) {
	r := NewBytes(3)
	r.Register(BytesMigration{
		Version: 2,
		Upgrade: func(data []byte) ([]byte, error) {
			return nil, fmt.Errorf("v2 exploded")
		},
	})
	r.Register(BytesMigration{
		Version: 3,
		Upgrade: func(data []byte) ([]byte, error) {
			t.Errorf("v3 should not run after v2 failure")
			return data, nil
		},
	})

	_, version, err := r.Run([]byte("data"), 1)
	if err == nil {
		t.Fatal("Run error = nil, want error")
	}
	if !strings.Contains(err.Error(), "v2 exploded") {
		t.Errorf("Run error = %v, want to contain %q", err, "v2 exploded")
	}
	if version != 1 {
		t.Errorf("Run version = %d, want 1 (unchanged after failure)", version)
	}
}

// ///////////////////////////////////////////////
// BytesRegistry.RegisterDev, RunDev, HasDev
// ///////////////////////////////////////////////

func TestBytesRegistry_RegisterDev_DuplicatePanics(t *testing.T) {
	r := NewBytes(1)
	r.RegisterDev(BytesMigration{Description: "fix timestamps"})
	assertPanics(t, func() {
		r.RegisterDev(BytesMigration{Description: "fix timestamps"})
	})
}

func TestBytesRegistry_RunDev_AppliesInRegistrationOrder(t *testing.T) {
	r := NewBytes(1)
	r.RegisterDev(BytesMigration{
		Description: "first",
		Upgrade: func(data []byte) ([]byte, error) {
			return append(data, '1'), nil
		},
	})
	r.RegisterDev(BytesMigration{
		Description: "second",
		Upgrade: func(data []byte) ([]byte, error) {
			return append(data, '2'), nil
		},
	})

	out, err := r.RunDev([]byte("x"))
	if err != nil {
		t.Fatalf("RunDev: %v", err)
	}
	if got, want := string(out), "x12"; got != want {
		t.Errorf("RunDev data = %q, want %q", got, want)
	}
}

func TestBytesRegistry_RunDev_NoTransforms(t *testing.T) {
	r := NewBytes(1)
	out, err := r.RunDev([]byte("unchanged"))
	if err != nil {
		t.Fatalf("RunDev: %v", err)
	}
	if got, want := string(out), "unchanged"; got != want {
		t.Errorf("RunDev data = %q, want %q", got, want)
	}
}

func TestBytesRegistry_RunDev_Error(t *testing.T) {
	r := NewBytes(1)
	r.RegisterDev(BytesMigration{
		Description: "boom",
		Upgrade: func(data []byte) ([]byte, error) {
			return nil, fmt.Errorf("kaboom")
		},
	})
	_, err := r.RunDev([]byte("x"))
	if err == nil {
		t.Fatal("RunDev error = nil, want error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("RunDev error = %v, want to contain %q", err, "boom")
	}
}

func TestBytesRegistry_HasDev(t *testing.T) {
	r := NewBytes(1)
	if r.HasDev() {
		t.Error("HasDev = true on fresh registry, want false")
	}
	r.RegisterDev(BytesMigration{Description: "x", Upgrade: identityUpgrade})
	if !r.HasDev() {
		t.Error("HasDev = false after RegisterDev, want true")
	}
}

// identityUpgrade is a no-op Upgrade used where the function is required
// but the test does not exercise it.
func identityUpgrade(data []byte) ([]byte, error) { return data, nil }
