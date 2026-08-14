package main

import (
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// semverRE validates that stdout looks like SemVer 2.0.0:
// MAJOR.MINOR.PATCH with optional -prerelease and/or build metadata.
var semverRE = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?(?:\+[A-Za-z0-9.-]+)?$`)

func TestMain_OutputShape(t *testing.T) {
	t.Helper()
	// coverage:ignore (exec wrapper; relies on go toolchain + git on PATH)
	out, err := exec.Command("go", "run", ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go run .: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if got == "" {
		t.Fatal("version helper produced empty output")
	}
	if !semverRE.MatchString(got) {
		t.Errorf("version helper output %q is not strict SemVer 2.0.0", got)
	}
}

// TestMain_Direct invokes main() in-process so the single fmt.Print call
// is attributed to this package's coverage profile (the go-run subtest
// executes in a subprocess that the parent profile cannot see).
func TestMain_Direct(t *testing.T) {
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	main()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	got := strings.TrimSpace(string(raw))
	if got == "" {
		t.Fatal("main() produced empty output")
	}
	if !semverRE.MatchString(got) {
		t.Errorf("main() wrote %q, want strict SemVer 2.0.0", got)
	}
}
