package migrate

import (
	"fmt"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// NewTOML + WithVersionKey
// ///////////////////////////////////////////////

func TestNewTOML_SetsCurrentVersion(t *testing.T) {
	r := NewTOML(3)
	if r.CurrentVersion != 3 {
		t.Errorf("CurrentVersion = %d, want 3", r.CurrentVersion)
	}
}

func TestTOMLRegistry_WithVersionKey(t *testing.T) {
	r := NewTOML(1).WithVersionKey("v")
	if r.VersionKey != "v" {
		t.Errorf("VersionKey = %q, want %q", r.VersionKey, "v")
	}
}

// ///////////////////////////////////////////////
// TOMLRegistry.Register
// ///////////////////////////////////////////////

func TestTOMLRegistry_Register_SortsByVersion(t *testing.T) {
	r := NewTOML(3)
	r.Register(TOMLMigration{Version: 3})
	r.Register(TOMLMigration{Version: 1})
	r.Register(TOMLMigration{Version: 2})

	wantOrder := []int{1, 2, 3}
	for i, m := range r.Migrations {
		if m.Version != wantOrder[i] {
			t.Errorf("Migrations[%d].Version = %d, want %d", i, m.Version, wantOrder[i])
		}
	}
}

func TestTOMLRegistry_Register_DuplicatePanics(t *testing.T) {
	r := NewTOML(1)
	r.Register(TOMLMigration{Version: 1})
	assertPanics(t, func() {
		r.Register(TOMLMigration{Version: 1})
	})
}

// ///////////////////////////////////////////////
// TOMLRegistry.NeedsMigration
// ///////////////////////////////////////////////

func TestTOMLRegistry_NeedsMigration(t *testing.T) {
	r := NewTOML(2)
	r.Register(TOMLMigration{Version: 2, Upgrade: identityTOMLUpgrade})

	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "empty", data: "", want: true},
		{name: "no version key", data: `name = "x"`, want: true},
		{name: "version behind", data: `version = 1`, want: true},
		{name: "at current", data: `version = 2`, want: false},
		{name: "ahead", data: `version = 3`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.NeedsMigration([]byte(tt.data))
			if err != nil {
				t.Fatalf("NeedsMigration: %v", err)
			}
			if got != tt.want {
				t.Errorf("NeedsMigration(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

func TestTOMLRegistry_NeedsMigration_InvalidTOML(t *testing.T) {
	r := NewTOML(1)
	_, err := r.NeedsMigration([]byte(`[unclosed`))
	if err == nil {
		t.Error("NeedsMigration error = nil, want error on invalid TOML")
	}
}

// ///////////////////////////////////////////////
// TOMLRegistry.Run
// ///////////////////////////////////////////////

func TestTOMLRegistry_Run_AppliesAndWritesVersion(t *testing.T) {
	r := NewTOML(2)
	r.Register(TOMLMigration{
		Version:     1,
		Description: "add name",
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			doc["name"] = "initialized"
			return doc, nil
		},
	})
	r.Register(TOMLMigration{
		Version:     2,
		Description: "add count",
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			doc["count"] = int64(5)
			return doc, nil
		},
	})

	out, version, err := r.Run(nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if version != 2 {
		t.Errorf("Run version = %d, want 2", version)
	}
	got := string(out)
	for _, want := range []string{`version = 2`, `name = "initialized"`, `count = 5`} {
		if !strings.Contains(got, want) {
			t.Errorf("Run output = %q, want to contain %q", got, want)
		}
	}
}

func TestTOMLRegistry_Run_DropsTheDocumentsComments(t *testing.T) {
	// A migration operates on a parsed map and the result is serialized
	// from that map alone, so nothing carries a comment through. A caller
	// whose file is mostly documentation has to regenerate it from its own
	// template, and this pins the behaviour so nobody assumes otherwise.
	source := "# how much disk the library may use\nversion = 1\nmax_size = \"500GB\" # the cap\n"

	r := NewTOML(2)
	r.Register(TOMLMigration{
		Version:     2,
		Description: "add a key",
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			doc["min_free"] = "100GB"
			return doc, nil
		},
	})

	out, _, err := r.Run([]byte(source))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(string(out), "#") {
		t.Errorf("Run output = %q, want the documented loss of every comment", out)
	}
	if !strings.Contains(string(out), `max_size = "500GB"`) {
		t.Errorf("Run output = %q, want the values kept", out)
	}
}

func TestTOMLRegistry_Run_RefusesANewerDocument(t *testing.T) {
	// Migration only runs forward, so a document from a newer build has no
	// path back to a shape this one understands. Reading it drops whatever
	// the newer format added, and writing it back makes that permanent.
	r := NewTOML(1)

	_, _, err := r.Run([]byte("version = 99\n"))
	if err == nil {
		t.Fatal("Run err = nil, want a refusal for a document at version 99")
	}
	if !strings.Contains(err.Error(), "upgrade the application") {
		t.Errorf("Run err = %v, want it to say how to fix the mismatch", err)
	}
}

func TestTOMLRegistry_Run_CustomVersionKey(t *testing.T) {
	r := NewTOML(1).WithVersionKey("v")
	r.Register(TOMLMigration{
		Version: 1,
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			doc["key"] = "value"
			return doc, nil
		},
	})

	out, _, err := r.Run(nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `v = 1`) {
		t.Errorf("Run output = %q, want to contain %q", got, `v = 1`)
	}
	// Check that the default "version" key is absent.
	for line := range strings.SplitSeq(got, "\n") {
		if strings.HasPrefix(line, "version =") {
			t.Errorf("Run output contains default version line: %q", line)
		}
	}
}

func TestTOMLRegistry_Run_SkipsAppliedMigrations(t *testing.T) {
	r := NewTOML(2)
	r.Register(TOMLMigration{
		Version: 1,
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			t.Error("v1 upgrade ran when version is already 1")
			return doc, nil
		},
	})
	r.Register(TOMLMigration{
		Version: 2,
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			doc["upgraded"] = true
			return doc, nil
		},
	})

	input := []byte(`version = 1` + "\n" + `existing = "x"`)
	out, version, err := r.Run(input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if version != 2 {
		t.Errorf("Run version = %d, want 2", version)
	}
	got := string(out)
	if !strings.Contains(got, `existing = "x"`) {
		t.Errorf("Run output lost existing key: %q", got)
	}
	if !strings.Contains(got, `upgraded = true`) {
		t.Errorf("Run output missing v2 change: %q", got)
	}
}

func TestTOMLRegistry_Run_UpgradeError(t *testing.T) {
	r := NewTOML(1)
	r.Register(TOMLMigration{
		Version: 1,
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			return nil, fmt.Errorf("upgrade failed")
		},
	})
	_, version, err := r.Run(nil)
	if err == nil {
		t.Fatal("Run error = nil, want error")
	}
	if version != 0 {
		t.Errorf("Run version = %d, want 0 (unchanged after failure)", version)
	}
}

func TestTOMLRegistry_Run_InvalidVersionType(t *testing.T) {
	r := NewTOML(1)
	_, _, err := r.Run([]byte(`version = "not a number"`))
	if err == nil {
		t.Error("Run error = nil, want error for non-integer version")
	}
}

// ///////////////////////////////////////////////
// TOMLRegistry.RunDev
// ///////////////////////////////////////////////

func TestTOMLRegistry_RunDev_AppliesTransforms(t *testing.T) {
	r := NewTOML(1)
	r.RegisterDev(TOMLMigration{
		Description: "rename key",
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			if v, ok := doc["old"]; ok {
				doc["new"] = v
				delete(doc, "old")
			}
			return doc, nil
		},
	})

	out, err := r.RunDev([]byte(`old = "value"`))
	if err != nil {
		t.Fatalf("RunDev: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `new = "value"`) {
		t.Errorf("RunDev output = %q, want to contain %q", got, `new = "value"`)
	}
	if strings.Contains(got, `old = `) {
		t.Errorf("RunDev output still contains old key: %q", got)
	}
}

// identityTOMLUpgrade is a no-op Upgrade used where the function is required
// but the test does not exercise it.
func identityTOMLUpgrade(doc map[string]any) (map[string]any, error) { return doc, nil }
