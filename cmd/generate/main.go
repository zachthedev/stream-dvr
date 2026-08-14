// Command generate runs every registered generator in [generate.Default] and
// writes its outputs. This package's go:generate directive and the pre-commit
// hook both invoke it.
//
// Subcommands:
//
//	generate run                       write every registered output
//	generate run <path> [<path>...]    write only the listed outputs
//	generate list outputs              print every registered output path
//	generate list inputs               print every registered input pattern
//
// With no arguments, generate defaults to "run".
package main

//go:generate go run .

import (
	"fmt"
	"os"
	"path/filepath"

	"zach.tools/go/stream-dvr/internal/buildenv"
	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/generate"
)

// init registers every generated output.
//
// It lives here because config imports generate for its FieldDoc type.
// Registering from inside generate would close that import cycle.
func init() {
	generate.Default.Register(generate.OutputEntry{
		Path:     "config.default.toml",
		Inputs:   []string{"internal/config/*.go"},
		Template: true,
		Generate: func(generate.OutputEntry) ([]byte, error) { return config.Render() },
	})

	generate.Default.Register(generate.OutputEntry{
		Path:   "config.schema.json",
		Inputs: []string{"internal/config/*.go"},
		Generate: generate.JSONSchema{
			Target:      &config.Config{},
			Title:       "stream-dvr configuration",
			Description: "Configuration for the stream-dvr recording daemon.",
		}.Generate,
	})

	generate.Default.Register(generate.OutputEntry{
		Path:     ".env.template",
		Inputs:   []string{"internal/buildenv/*.go"},
		Template: true,
		Generate: generate.DotEnv{
			ProjectName: "stream-dvr",
			Consumer:    "The Makefile",
			Variables:   envVariables(),
		}.Generate,
	})
}

// envVariables copies the build schema into the generator's own shape.
//
// The copy is field by field, so internal/buildenv describes itself without
// importing a generator. That keeps the schema readable as a plain
// declaration.
func envVariables() []generate.EnvVar {
	declared := buildenv.Variables()

	variables := make([]generate.EnvVar, 0, len(declared))
	for _, variable := range declared {
		variables = append(variables, generate.EnvVar{
			Name:    variable.Name,
			Purpose: variable.Purpose,
			Example: variable.Example,
			Absent:  variable.Absent,
		})
	}
	return variables
}

func main() {
	if err := dispatch(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// ensureProjectRoot walks up from the working directory to the one holding
// go.mod, then changes into it.
//
// Every OutputEntry path is relative to the project root. A go:generate
// directive runs in the package directory and every other caller runs in the
// root, so the walk is what makes both resolve the same paths.
func ensureProjectRoot() error {
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if dir == start {
				return nil
			}
			return os.Chdir(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("go.mod not found in any ancestor of %s", start)
		}
		dir = parent
	}
}

// dispatch parses argv and runs the matching subcommand. It sits apart from
// main so a test can drive the whole tree.
func dispatch(args []string, stdout *os.File) error {
	if len(args) == 0 || args[0] == "run" {
		if err := ensureProjectRoot(); err != nil {
			return err
		}
		var paths []string
		if len(args) > 0 {
			paths = args[1:]
		}
		return generate.Default.Run(paths...)
	}
	switch args[0] {
	case "list":
		return dispatchList(args[1:], stdout)
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q; try 'generate help'", args[0])
	}
}

func dispatchList(args []string, stdout *os.File) error {
	if len(args) == 0 {
		return fmt.Errorf("list requires a target: outputs or inputs")
	}
	switch args[0] {
	case "outputs":
		for _, p := range generate.Default.Outputs() {
			fmt.Fprintln(stdout, p)
		}
	case "inputs":
		for _, p := range generate.Default.Inputs() {
			fmt.Fprintln(stdout, p)
		}
	default:
		return fmt.Errorf("unknown list target %q; want outputs or inputs", args[0])
	}
	return nil
}

func printUsage(stdout *os.File) {
	fmt.Fprintln(stdout, `Usage: generate [subcommand]

Subcommands:
  run [path...]    Write registered outputs. With no paths, writes every
                   registered output. With one or more paths, writes only
                   the listed entries (each must be a registered path).
  list outputs     Print every registered output path, one per line.
  list inputs      Print every registered input pattern, one per line.
  help             Show this help.`)
}
