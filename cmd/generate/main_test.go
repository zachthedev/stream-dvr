package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zach.tools/go/stream-dvr/internal/generate"
)

// resetRegistry swaps in an empty registry and restores the package Default
// when the test ends.
func resetRegistry(t *testing.T) {
	t.Helper()
	saved := generate.Default
	t.Cleanup(func() { generate.Default = saved })
	generate.Default = &generate.Registry{}
}

// runInDir changes the working directory to dir for the test's lifetime. It
// writes a minimal go.mod there, so ensureProjectRoot treats dir as the root.
func runInDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
}

// ///////////////////////////////////////////////
// dispatch
// ///////////////////////////////////////////////

func TestDispatch_NoArgsRuns(t *testing.T) {
	resetRegistry(t)
	dir := t.TempDir()
	runInDir(t, dir)

	generate.Default.Register(generate.OutputEntry{
		Path:     "out.txt",
		Generate: func(generate.OutputEntry) ([]byte, error) { return []byte("ok"), nil },
	})

	if err := dispatch(nil, os.Stdout); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "ok" {
		t.Errorf("out.txt = %q, want %q", data, "ok")
	}
}

func TestDispatch_Run(t *testing.T) {
	resetRegistry(t)
	dir := t.TempDir()
	runInDir(t, dir)

	generate.Default.Register(generate.OutputEntry{
		Path:     "ran.txt",
		Generate: func(generate.OutputEntry) ([]byte, error) { return []byte("y"), nil },
	})

	if err := dispatch([]string{"run"}, os.Stdout); err != nil {
		t.Fatalf("dispatch run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); err != nil {
		t.Errorf("ran.txt missing: %v", err)
	}
}

func TestDispatch_RunSubsetWritesOnlyListedEntries(t *testing.T) {
	resetRegistry(t)
	dir := t.TempDir()
	runInDir(t, dir)

	generate.Default.Register(generate.OutputEntry{
		Path:     "wanted.txt",
		Generate: func(generate.OutputEntry) ([]byte, error) { return []byte("w"), nil },
	})
	generate.Default.Register(generate.OutputEntry{
		Path:     "skipped.txt",
		Generate: func(generate.OutputEntry) ([]byte, error) { return []byte("s"), nil },
	})

	if err := dispatch([]string{"run", "wanted.txt"}, os.Stdout); err != nil {
		t.Fatalf("dispatch run wanted.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wanted.txt")); err != nil {
		t.Errorf("wanted.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "skipped.txt")); err == nil {
		t.Error("skipped.txt was written, want only wanted.txt")
	}
}

func TestDispatch_RunSubsetMultiplePaths(t *testing.T) {
	resetRegistry(t)
	dir := t.TempDir()
	runInDir(t, dir)

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		generate.Default.Register(generate.OutputEntry{
			Path:     name,
			Generate: func(generate.OutputEntry) ([]byte, error) { return []byte(name), nil },
		})
	}

	if err := dispatch([]string{"run", "a.txt", "c.txt"}, os.Stdout); err != nil {
		t.Fatalf("dispatch run a.txt c.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Errorf("a.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c.txt")); err != nil {
		t.Errorf("c.txt missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err == nil {
		t.Error("b.txt was written, want only a.txt and c.txt")
	}
}

func TestDispatch_RunSubsetUnknownPathErrors(t *testing.T) {
	resetRegistry(t)
	dir := t.TempDir()
	runInDir(t, dir)

	if err := dispatch([]string{"run", "missing.txt"}, os.Stdout); err == nil {
		t.Error("dispatch run missing.txt error = nil, want error for unknown path")
	}
}

func TestDispatch_ListOutputs(t *testing.T) {
	resetRegistry(t)
	generate.Default.Register(generate.OutputEntry{
		Path:     "config.toml",
		Generate: func(generate.OutputEntry) ([]byte, error) { return nil, nil },
	})

	var buf bytes.Buffer
	tempWithWriter(t, &buf, func(f *os.File) {
		if err := dispatch([]string{"list", "outputs"}, f); err != nil {
			t.Fatalf("dispatch list outputs: %v", err)
		}
	})
	got := buf.String()
	if !strings.Contains(got, "config.toml") {
		t.Errorf("output = %q, want to contain %q", got, "config.toml")
	}
}

func TestDispatch_ListInputs(t *testing.T) {
	resetRegistry(t)
	generate.Default.Register(generate.OutputEntry{
		Path:     "out.txt",
		Inputs:   []string{"src/*.go"},
		Generate: func(generate.OutputEntry) ([]byte, error) { return nil, nil },
	})

	var buf bytes.Buffer
	tempWithWriter(t, &buf, func(f *os.File) {
		if err := dispatch([]string{"list", "inputs"}, f); err != nil {
			t.Fatalf("dispatch list inputs: %v", err)
		}
	})
	got := buf.String()
	if !strings.Contains(got, "src/*.go") {
		t.Errorf("output = %q, want to contain %q", got, "src/*.go")
	}
}

func TestDispatch_ListRequiresTarget(t *testing.T) {
	resetRegistry(t)
	if err := dispatch([]string{"list"}, os.Stdout); err == nil {
		t.Error("dispatch list error = nil, want error for missing target")
	}
}

func TestDispatch_ListUnknownTarget(t *testing.T) {
	resetRegistry(t)
	if err := dispatch([]string{"list", "bogus"}, os.Stdout); err == nil {
		t.Error("dispatch list bogus error = nil, want error")
	}
}

func TestDispatch_UnknownSubcommand(t *testing.T) {
	resetRegistry(t)
	err := dispatch([]string{"bogus"}, os.Stdout)
	if err == nil {
		t.Error("dispatch bogus error = nil, want error")
	}
}

func TestDispatch_Help(t *testing.T) {
	resetRegistry(t)
	var buf bytes.Buffer
	tempWithWriter(t, &buf, func(f *os.File) {
		if err := dispatch([]string{"help"}, f); err != nil {
			t.Fatalf("dispatch help: %v", err)
		}
	})
	if !strings.Contains(buf.String(), "Usage:") {
		t.Errorf("help output missing Usage: header:\n%s", buf.String())
	}
}

// tempWithWriter runs fn with an os.File that proxies writes into buf. A test
// reads what a subcommand printed without touching os.Stdout.
func tempWithWriter(t *testing.T, buf *bytes.Buffer, fn func(*os.File)) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()
	fn(w)
	w.Close()
	<-done
	r.Close()
}
