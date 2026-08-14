package generate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

func staticEntry(path, body string) OutputEntry {
	return OutputEntry{
		Path:     path,
		Generate: func(OutputEntry) ([]byte, error) { return []byte(body), nil },
	}
}

// ///////////////////////////////////////////////
// Registry.Register
// ///////////////////////////////////////////////

func TestRegistry_Register_AppendsEntry(t *testing.T) {
	r := &Registry{}
	r.Register(staticEntry("a.txt", "a"))
	if len(r.entries) != 1 {
		t.Fatalf("entries count = %d, want 1", len(r.entries))
	}
}

func TestRegistry_Register_EmptyPathPanics(t *testing.T) {
	r := &Registry{}
	assertPanics(t, func() {
		r.Register(OutputEntry{Generate: func(OutputEntry) ([]byte, error) { return nil, nil }})
	})
}

func TestRegistry_Register_NilGeneratePanics(t *testing.T) {
	r := &Registry{}
	assertPanics(t, func() {
		r.Register(OutputEntry{Path: "a.txt"})
	})
}

func TestRegistry_Register_DuplicatePathPanics(t *testing.T) {
	r := &Registry{}
	r.Register(staticEntry("a.txt", "a"))
	assertPanics(t, func() {
		r.Register(staticEntry("a.txt", "b"))
	})
}

// ///////////////////////////////////////////////
// Registry.Entries
// ///////////////////////////////////////////////

func TestRegistry_Entries_ReturnsSortedCopy(t *testing.T) {
	r := &Registry{}
	r.Register(staticEntry("z.txt", "z"))
	r.Register(staticEntry("a.txt", "a"))
	r.Register(staticEntry("m.txt", "m"))

	entries := r.Entries()
	want := []string{"a.txt", "m.txt", "z.txt"}
	for i, e := range entries {
		if e.Path != want[i] {
			t.Errorf("Entries()[%d].Path = %q, want %q", i, e.Path, want[i])
		}
	}

	entries[0].Path = "mutated"
	if r.entries[0].Path == "mutated" {
		t.Error("Entries() returned a reference; caller mutation leaked")
	}
}

// ///////////////////////////////////////////////
// Registry.Outputs
// ///////////////////////////////////////////////

func TestRegistry_Outputs_SortedForwardSlash(t *testing.T) {
	r := &Registry{}
	r.Register(staticEntry(filepath.Join("sub", "b.txt"), ""))
	r.Register(staticEntry("a.txt", ""))

	got := r.Outputs()
	want := []string{"a.txt", "sub/b.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("Outputs() = %v, want %v", got, want)
	}
}

// ///////////////////////////////////////////////
// Registry.Inputs
// ///////////////////////////////////////////////

func TestRegistry_Inputs_DedupsAndSorts(t *testing.T) {
	r := &Registry{}
	r.Register(OutputEntry{
		Path:     "a.txt",
		Inputs:   []string{"src/*.go", "data/*.json"},
		Generate: func(OutputEntry) ([]byte, error) { return nil, nil },
	})
	r.Register(OutputEntry{
		Path:     "b.txt",
		Inputs:   []string{"src/*.go", "docs/*.md"},
		Generate: func(OutputEntry) ([]byte, error) { return nil, nil },
	})

	got := r.Inputs()
	want := []string{"data/*.json", "docs/*.md", "src/*.go"}
	if !slices.Equal(got, want) {
		t.Errorf("Inputs() = %v, want %v", got, want)
	}
}

// ///////////////////////////////////////////////
// Registry.Run
// ///////////////////////////////////////////////

func TestRegistry_Run_WritesEveryOutput(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{}
	r.Register(staticEntry(filepath.Join(dir, "a.txt"), "hello a"))
	r.Register(staticEntry(filepath.Join(dir, "sub", "b.txt"), "hello b"))

	if err := r.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, want := range []struct {
		path, body string
	}{
		{filepath.Join(dir, "a.txt"), "hello a"},
		{filepath.Join(dir, "sub", "b.txt"), "hello b"},
	} {
		got, err := os.ReadFile(want.path)
		if err != nil {
			t.Errorf("reading %s: %v", want.path, err)
			continue
		}
		if string(got) != want.body {
			t.Errorf("%s = %q, want %q", want.path, got, want.body)
		}
	}
}

func TestRegistry_Run_GenerateErrorPropagates(t *testing.T) {
	r := &Registry{}
	r.Register(OutputEntry{
		Path:     "boom.txt",
		Generate: func(OutputEntry) ([]byte, error) { return nil, fmt.Errorf("kaboom") },
	})

	err := r.Run()
	if err == nil {
		t.Fatal("Run error = nil, want error")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("Run error = %v, want to contain %q", err, "kaboom")
	}
}

func TestRegistry_Run_SubsetWritesOnlyMatchingEntries(t *testing.T) {
	dir := t.TempDir()
	r := &Registry{}
	r.Register(staticEntry(filepath.Join(dir, "a.txt"), "a"))
	r.Register(staticEntry(filepath.Join(dir, "b.txt"), "b"))
	r.Register(staticEntry(filepath.Join(dir, "c.txt"), "c"))

	if err := r.Run(filepath.Join(dir, "a.txt"), filepath.Join(dir, "c.txt")); err != nil {
		t.Fatalf("Run subset: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Errorf("a.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); err != nil {
		t.Errorf("c.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err == nil {
		t.Error("b.txt was written but only a.txt and c.txt were requested")
	}
}

func TestRegistry_Run_SubsetUnknownPathErrors(t *testing.T) {
	r := &Registry{}
	err := r.Run("missing.txt")
	if err == nil {
		t.Error("Run error = nil, want error for unknown path")
	}
}
