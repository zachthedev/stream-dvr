package migrate

import (
	"encoding/json"
	"fmt"
	"testing"
)

// ///////////////////////////////////////////////
// NewJSON + WithVersionKey
// ///////////////////////////////////////////////

func TestNewJSON_SetsCurrentVersion(t *testing.T) {
	r := NewJSON(3)
	if r.CurrentVersion != 3 {
		t.Errorf("CurrentVersion = %d, want 3", r.CurrentVersion)
	}
}

func TestJSONRegistry_WithVersionKey(t *testing.T) {
	r := NewJSON(1).WithVersionKey("v")
	if r.VersionKey != "v" {
		t.Errorf("VersionKey = %q, want %q", r.VersionKey, "v")
	}
}

// ///////////////////////////////////////////////
// JSONRegistry.Register
// ///////////////////////////////////////////////

func TestJSONRegistry_Register_SortsByVersion(t *testing.T) {
	r := NewJSON(3)
	r.Register(JSONMigration{Version: 3})
	r.Register(JSONMigration{Version: 1})
	r.Register(JSONMigration{Version: 2})

	wantOrder := []int{1, 2, 3}
	for i, m := range r.Migrations {
		if m.Version != wantOrder[i] {
			t.Errorf("Migrations[%d].Version = %d, want %d", i, m.Version, wantOrder[i])
		}
	}
}

func TestJSONRegistry_Register_DuplicatePanics(t *testing.T) {
	r := NewJSON(1)
	r.Register(JSONMigration{Version: 1})
	assertPanics(t, func() {
		r.Register(JSONMigration{Version: 1})
	})
}

// ///////////////////////////////////////////////
// JSONRegistry.NeedsMigration
// ///////////////////////////////////////////////

func TestJSONRegistry_NeedsMigration(t *testing.T) {
	r := NewJSON(2)
	r.Register(JSONMigration{Version: 2, Upgrade: identityJSONUpgrade})

	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "empty", data: "", want: true},
		{name: "no version key", data: `{"name": "x"}`, want: true},
		{name: "version behind", data: `{"version": 1}`, want: true},
		{name: "at current", data: `{"version": 2}`, want: false},
		{name: "ahead", data: `{"version": 3}`, want: false},
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

func TestJSONRegistry_NeedsMigration_InvalidJSON(t *testing.T) {
	r := NewJSON(1)
	_, err := r.NeedsMigration([]byte(`{broken`))
	if err == nil {
		t.Error("NeedsMigration error = nil, want error on invalid JSON")
	}
}

// ///////////////////////////////////////////////
// JSONRegistry.Run
// ///////////////////////////////////////////////

func TestJSONRegistry_Run_AppliesAndWritesVersion(t *testing.T) {
	r := NewJSON(2)
	r.Register(JSONMigration{
		Version: 1,
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			doc["name"] = "initialized"
			return doc, nil
		},
	})
	r.Register(JSONMigration{
		Version: 2,
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			doc["count"] = 5
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

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	gotVersion, ok := parsed["version"].(float64)
	if !ok {
		t.Fatalf("version has unexpected type %T", parsed["version"])
	}
	if int(gotVersion) != 2 {
		t.Errorf("output version = %v, want 2", gotVersion)
	}
	if got, want := parsed["name"], "initialized"; got != want {
		t.Errorf("output name = %v, want %v", got, want)
	}
}

func TestJSONRegistry_Run_RefusesANewerDocument(t *testing.T) {
	r := NewJSON(1)

	if _, _, err := r.Run([]byte(`{"version": 99}`)); err == nil {
		t.Error("Run err = nil, want a refusal for a document at version 99")
	}
}

func TestJSONRegistry_Run_CustomVersionKey(t *testing.T) {
	r := NewJSON(1).WithVersionKey("v")
	r.Register(JSONMigration{
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
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if _, ok := parsed["v"]; !ok {
		t.Errorf("output missing custom version key %q: %q", "v", string(out))
	}
	if _, ok := parsed["version"]; ok {
		t.Errorf("output should not contain default version key: %q", string(out))
	}
}

func TestJSONRegistry_Run_SkipsAppliedMigrations(t *testing.T) {
	r := NewJSON(2)
	r.Register(JSONMigration{
		Version: 1,
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			t.Error("v1 upgrade ran when version is already 1")
			return doc, nil
		},
	})
	r.Register(JSONMigration{
		Version: 2,
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			doc["upgraded"] = true
			return doc, nil
		},
	})

	input := []byte(`{"version": 1, "existing": "x"}`)
	out, version, err := r.Run(input)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if version != 2 {
		t.Errorf("Run version = %d, want 2", version)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if got, want := parsed["existing"], "x"; got != want {
		t.Errorf("output lost existing key: got %v want %v", got, want)
	}
	if got, want := parsed["upgraded"], true; got != want {
		t.Errorf("output missing v2 change: got %v want %v", got, want)
	}
}

func TestJSONRegistry_Run_UpgradeError(t *testing.T) {
	r := NewJSON(1)
	r.Register(JSONMigration{
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

func TestJSONRegistry_Run_InvalidVersionType(t *testing.T) {
	r := NewJSON(1)
	_, _, err := r.Run([]byte(`{"version": "not a number"}`))
	if err == nil {
		t.Error("Run error = nil, want error for non-integer version")
	}
}

// ///////////////////////////////////////////////
// JSONRegistry.RunDev
// ///////////////////////////////////////////////

func TestJSONRegistry_RunDev_AppliesTransforms(t *testing.T) {
	r := NewJSON(1)
	r.RegisterDev(JSONMigration{
		Description: "rename key",
		Upgrade: func(doc map[string]any) (map[string]any, error) {
			if v, ok := doc["old"]; ok {
				doc["new"] = v
				delete(doc, "old")
			}
			return doc, nil
		},
	})

	out, err := r.RunDev([]byte(`{"old": "value"}`))
	if err != nil {
		t.Fatalf("RunDev: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parsing output: %v", err)
	}
	if got, want := parsed["new"], "value"; got != want {
		t.Errorf("RunDev output new = %v, want %v", got, want)
	}
	if _, ok := parsed["old"]; ok {
		t.Errorf("RunDev output still contains old key: %q", string(out))
	}
}

// identityJSONUpgrade is a no-op Upgrade used where the function is required
// but the test does not exercise it.
func identityJSONUpgrade(doc map[string]any) (map[string]any, error) { return doc, nil }
