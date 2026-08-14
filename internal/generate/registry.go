// Package generate holds the declaration of every file this project
// generates, and writes those files on demand.
//
// A generator is an [OutputEntry] naming the file it produces, the input
// patterns it depends on, and the function that produces its bytes. The
// [Registry] collects those entries and answers three questions for
// cmd/generate and the pre-commit hook:
//
//   - [Registry.Run] writes every registered output, or only the paths
//     named.
//   - [Registry.Outputs] names every registered path, which is what the
//     hook stages after a run.
//   - [Registry.Inputs] names every registered pattern, which is how the
//     hook skips a run when no relevant file changed.
//
// cmd/generate registers the entries. A package that owns a schema imports
// this one for its doc types, so registering from in here would close that
// cycle. Format helpers such as [TOMLConfig] and [JSONSchema] supply an
// entry's Generate function.
package generate

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// OutputEntry declares a generated file: its output path, the input
// patterns it depends on, and the function that produces its bytes.
type OutputEntry struct {
	// Path is the output file path, relative to the project root.
	Path string
	// Inputs are glob patterns this output depends on. The pre-commit
	// hook uses these to skip generation when no staged file matches.
	Inputs []string
	// Template marks an output an operator is meant to copy and edit,
	// such as the default config or the build environment template. A
	// format generator reads it to leave off the "do not edit" banner,
	// which a file somebody is told to change must not open with.
	Template bool
	// Generate produces the file's content bytes. It receives the entry
	// so a format generator can read Template without every format
	// config carrying its own copy of the field.
	Generate func(OutputEntry) ([]byte, error)
}

// Registry holds a set of [OutputEntry] values.
type Registry struct {
	entries []OutputEntry
}

// ///////////////////////////////////////////////
// Registry methods
// ///////////////////////////////////////////////

// Register appends an entry. It panics on an empty path, a nil Generate
// function, or a path some other entry already claims.
func (r *Registry) Register(e OutputEntry) {
	if e.Path == "" {
		panic("generate: OutputEntry.Path must not be empty")
	}
	if e.Generate == nil {
		panic(fmt.Sprintf("generate: OutputEntry.Generate is nil for %q", e.Path))
	}
	for _, existing := range r.entries {
		if existing.Path == e.Path {
			panic(fmt.Sprintf("generate: duplicate OutputEntry path %q", e.Path))
		}
	}
	r.entries = append(r.entries, e)
}

// Entries returns a copy of all registered entries, sorted by path.
func (r *Registry) Entries() []OutputEntry {
	out := make([]OutputEntry, len(r.entries))
	copy(out, r.entries)
	slices.SortFunc(out, func(a, b OutputEntry) int { return cmp.Compare(a.Path, b.Path) })
	return out
}

// Outputs returns every registered output path, sorted.
func (r *Registry) Outputs() []string {
	out := make([]string, len(r.entries))
	for i, e := range r.entries {
		out[i] = filepath.ToSlash(e.Path)
	}
	slices.Sort(out)
	return out
}

// Inputs returns every unique input pattern across all entries, sorted.
func (r *Registry) Inputs() []string {
	seen := map[string]struct{}{}
	var patterns []string
	for _, e := range r.entries {
		for _, p := range e.Inputs {
			norm := filepath.ToSlash(p)
			if _, ok := seen[norm]; ok {
				continue
			}
			seen[norm] = struct{}{}
			patterns = append(patterns, norm)
		}
	}
	slices.Sort(patterns)
	return patterns
}

// Run writes registered outputs to disk. With no arguments, it writes every
// registered entry. With one or more paths, it writes only the entries whose
// Path matches, and an unknown path is an error.
func (r *Registry) Run(paths ...string) error {
	if len(paths) == 0 {
		for _, e := range r.Entries() {
			if err := r.runEntry(e); err != nil {
				return err
			}
		}
		return nil
	}
	byPath := make(map[string]OutputEntry, len(r.entries))
	for _, e := range r.entries {
		byPath[e.Path] = e
	}
	for _, p := range paths {
		e, ok := byPath[p]
		if !ok {
			return fmt.Errorf("generate: no entry registered for path %q", p)
		}
		if err := r.runEntry(e); err != nil {
			return err
		}
	}
	return nil
}

// ///////////////////////////////////////////////
// Internal helpers
// ///////////////////////////////////////////////

// runEntry executes one entry and writes its output to disk.
func (r *Registry) runEntry(e OutputEntry) error {
	data, err := e.Generate(e)
	if err != nil {
		return fmt.Errorf("generate: generating %s: %w", e.Path, err)
	}
	if dir := filepath.Dir(e.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("generate: creating parent dir for %s: %w", e.Path, err)
		}
	}
	if err := os.WriteFile(e.Path, data, 0o644); err != nil { //nolint:gosec // generated files are not secrets
		return fmt.Errorf("generate: writing %s: %w", e.Path, err)
	}
	return nil
}
