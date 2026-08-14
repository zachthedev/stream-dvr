package buildenv

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// injected matches a variable reference inside the linker flags.
var injected = regexp.MustCompile(`\$\(([A-Z][A-Z0-9_]*)\)`)

// linkerSymbol matches the package path and symbol a -X flag injects into,
// which is the half of the flag that must be the same everywhere.
var linkerSymbol = regexp.MustCompile(`-X\s+([^\s=]+)=`)

// makeAssignment matches a Makefile ?= assignment, which is where a $(NAME)
// in the linker flags resolves from.
//
// The whitespace either side of the operator is spaces and tabs rather than
// \s, which would swallow the newline after an assignment with no value and
// take the next line as the value.
var makeAssignment = regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)[ \t]*\?=[ \t]*(.*)$`)

// templateEnv matches a goreleaser template reading an environment variable.
var templateEnv = regexp.MustCompile(`\.Env\.([A-Z][A-Z0-9_]*)`)

// workflowEnv matches an environment variable a workflow step sets.
var workflowEnv = regexp.MustCompile(`(?m)^\s*([A-Z][A-Z0-9_]*):`)

// projectFile reads a file at the repository root.
func projectFile(t *testing.T, name string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}

// ldflagsBlock returns the LDFLAGS assignment, continuations included.
func ldflagsBlock(t *testing.T, makefile string) string {
	t.Helper()

	var block []string
	inside := false
	for line := range strings.SplitSeq(makefile, "\n") {
		if strings.HasPrefix(line, "LDFLAGS") || strings.HasPrefix(line, "override LDFLAGS") {
			inside = true
		}
		if !inside {
			continue
		}
		block = append(block, line)
		if !strings.HasSuffix(strings.TrimRight(line, "\r"), "\\") {
			break
		}
	}

	if len(block) == 0 {
		t.Fatal("the Makefile has no LDFLAGS assignment, so nothing is injected into the binary")
	}
	return strings.Join(block, "\n")
}

// yamlBlock returns the lines nested under a key, which is that key's whole
// value however many lines it spans.
//
// The carriage return goes first, so a checkout on Windows measures the same
// indent a checkout on Linux does.
func yamlBlock(t *testing.T, doc, key string) string {
	t.Helper()

	var block []string
	indent := -1
	for line := range strings.SplitSeq(doc, "\n") {
		line = strings.TrimRight(line, "\r")
		if indent < 0 {
			if strings.TrimSpace(line) == key+":" {
				indent = len(line) - len(strings.TrimLeft(line, " "))
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			block = append(block, line)
			continue
		}
		if len(line)-len(strings.TrimLeft(line, " ")) <= indent {
			break
		}
		block = append(block, line)
	}

	if len(block) == 0 {
		t.Fatalf("nothing is nested under %q, so the file's shape changed and this reads none of it", key)
	}
	return strings.Join(block, "\n")
}

// makeAssignments reads a Makefile's ?= assignments as name to value.
func makeAssignments(t *testing.T, makefile string) map[string]string {
	t.Helper()

	assignments := map[string]string{}
	for _, match := range makeAssignment.FindAllStringSubmatch(makefile, -1) {
		assignments[match[1]] = strings.TrimSpace(match[2])
	}
	return assignments
}

// expandMake resolves the $(NAME) references in a Makefile value, leaving a
// name the Makefile does not assign as it stands.
func expandMake(value string, assignments map[string]string) string {
	return injected.ReplaceAllStringFunc(value, func(reference string) string {
		if replacement, ok := assignments[injected.FindStringSubmatch(reference)[1]]; ok {
			return replacement
		}
		return reference
	})
}

// ///////////////////////////////////////////////
// The schema
// ///////////////////////////////////////////////

func TestVariables_DeclaresEveryValueTheBuildInjects(t *testing.T) {
	// The drift this package exists to prevent. A variable the linker
	// stamps into the binary but nothing declares is one the template never
	// mentions, so an operator has no way to learn it exists.
	//
	// Names ending in _PKG are excluded by shape rather than by list: they
	// hold the import path a value is injected into, which is a fact about
	// the code and not a setting anyone chooses.
	declared := map[string]bool{}
	for _, variable := range Variables() {
		declared[variable.Name] = true
	}

	for _, match := range injected.FindAllStringSubmatch(ldflagsBlock(t, projectFile(t, "Makefile")), -1) {
		name := match[1]
		if strings.HasSuffix(name, "_PKG") {
			continue
		}
		if !declared[name] {
			t.Errorf("the Makefile injects %s but nothing declares it, so .env.template omits it", name)
		}
	}
}

func TestVariables_AreReadByTheMakefile(t *testing.T) {
	// The template tells an operator to put values in .env. A Makefile that
	// does not include one would leave every value there inert, and nothing
	// about the build would say so.
	if !strings.Contains(projectFile(t, "Makefile"), "-include .env") {
		t.Error("the Makefile does not include .env, so nothing reads what the template asks for")
	}
}

func TestVariables_NamesAreUnique(t *testing.T) {
	// Two entries under one name render two blocks that contradict each
	// other, and only the last assignment would take effect.
	seen := map[string]bool{}

	for _, variable := range Variables() {
		if seen[variable.Name] {
			t.Errorf("%s is declared twice", variable.Name)
		}
		seen[variable.Name] = true
	}
}

func TestVariables_EachSaysWhatHappensWithoutIt(t *testing.T) {
	// Every variable here is optional, so the only question an operator has
	// is whether to bother. A blank answer makes the template a list of
	// names.
	for _, variable := range Variables() {
		if variable.Name == "" {
			t.Error("a variable has no name")
		}
		if variable.Purpose == "" {
			t.Errorf("%s has no purpose, so the template cannot say what it is for", variable.Name)
		}
		if variable.Absent == "" {
			t.Errorf("%s does not say what a build without it does", variable.Name)
		}
	}
}

// ///////////////////////////////////////////////
// What the release injects
// ///////////////////////////////////////////////

func TestVariables_ReachTheReleasedBinary(t *testing.T) {
	// A check job builds through the Makefile and a release builds through
	// goreleaser. A value one injects and the other does not is a difference
	// between the binary an operator downloads and every binary CI ever
	// built, which no job compares.
	makefile := projectFile(t, "Makefile")
	assignments := makeAssignments(t, makefile)

	built := map[string]bool{}
	for _, match := range linkerSymbol.FindAllStringSubmatch(ldflagsBlock(t, makefile), -1) {
		built[expandMake(match[1], assignments)] = true
	}
	if len(built) == 0 {
		t.Fatal("the Makefile's linker flags name no symbol, so this compares nothing")
	}

	released := map[string]bool{}
	for _, match := range linkerSymbol.FindAllStringSubmatch(yamlBlock(t, projectFile(t, ".goreleaser.yml"), "ldflags"), -1) {
		released[match[1]] = true
	}
	if len(released) == 0 {
		t.Fatal(".goreleaser.yml's linker flags name no symbol, so this compares nothing")
	}

	for _, symbol := range slices.Sorted(maps.Keys(built)) {
		if !released[symbol] {
			t.Errorf("the Makefile injects %s and .goreleaser.yml does not, so a released binary never receives it", symbol)
		}
	}
	for _, symbol := range slices.Sorted(maps.Keys(released)) {
		if !built[symbol] {
			t.Errorf("the release injects %s and the Makefile does not, so no check job ever builds with it", symbol)
		}
	}
}

func TestVariables_AreSuppliedToTheReleaseWorkflow(t *testing.T) {
	// A template reading an environment variable nothing sets renders empty
	// and the release builds anyway, so the binary carries a blank where a
	// value belongs and no job reports it.
	referenced := map[string]bool{}
	for _, match := range templateEnv.FindAllStringSubmatch(yamlBlock(t, projectFile(t, ".goreleaser.yml"), "ldflags"), -1) {
		referenced[match[1]] = true
	}

	supplied := map[string]bool{}
	for _, match := range workflowEnv.FindAllStringSubmatch(yamlBlock(t, projectFile(t, ".github/workflows/deliver.yml"), "env"), -1) {
		supplied[match[1]] = true
	}

	for _, name := range slices.Sorted(maps.Keys(referenced)) {
		if !supplied[name] {
			t.Errorf("%s reaches the release build from .goreleaser.yml, and deliver.yml sets no such environment variable", name)
		}
	}
}

// ///////////////////////////////////////////////
// What must never be committed
// ///////////////////////////////////////////////

func TestVariables_TheTemplateIsCommittedAndDotEnvIsNot(t *testing.T) {
	// A real Twitch application id lands in .env. Committing one ties a
	// public repository to a single developer account, and a rule that
	// ignored the template alongside it would leave contributors with no
	// documentation at all.
	// Compared line by line rather than as a substring. A checkout on
	// Windows carries CRLF, so a search for a rule wrapped in "\n" finds
	// nothing there and the rule reads as missing on one platform only.
	rules := map[string]bool{}
	for line := range strings.SplitSeq(projectFile(t, ".gitignore"), "\n") {
		rules[strings.TrimSpace(line)] = true
	}

	if !rules[".env"] {
		t.Error(".gitignore does not ignore .env, so an operator's own values can be committed")
	}
	if !rules["!.env.template"] {
		t.Error(".gitignore does not re-admit .env.template, so the generated template cannot be committed")
	}

	// An ignore rule is not enforcement: `git add -f` tracks the file anyway
	// and the rule then does nothing. The Makefile includes that file before
	// every `?=`, so a tracked one sets LDFLAGS, CC and BUILD_TARGET
	// outright, and a value in it runs as a shell command the first time
	// anyone types `make build`. Reviewing a branch is enough to be caught
	// by it, and it is not a file a reviewer opens. So the assertion that
	// matters is that git does not track it.
	tracked := exec.Command("git", "ls-files", "--error-unmatch", ".env")
	tracked.Dir = filepath.Join("..", "..")
	if err := tracked.Run(); err == nil {
		t.Error(".env is tracked by git, so it reaches a clone and the Makefile reads it before every build")
	}
}
