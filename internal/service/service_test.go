package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Helper process
// ///////////////////////////////////////////////

// execLog records what a manager asked the platform to do.
type execLog struct {
	calls [][]string
}

// TestHelperProcess stands in for the platform's own registration command.
// It is not a test: it runs only when the parent re-invokes this binary.
//
// The modes are named for the answer they give rather than for the command
// that gives it, because the three platforms ask different programs the same
// questions.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		t.Skip("helper process, invoked only by fakeExec")
	}

	switch os.Getenv("FAKE_MODE") {
	case "ok":
		os.Exit(0)
	case "absent":
		os.Stdout.WriteString("ERROR: The system cannot find the file specified.\n")
		os.Exit(1)
	case "denied":
		os.Stdout.WriteString("ERROR: Access is denied.\n")
		os.Exit(1)
	case "running":
		os.Stdout.WriteString("TaskName: stream-dvr\nStatus: Running\n")
		os.Exit(0)
	case "installed":
		os.Stdout.WriteString("TaskName: stream-dvr\nStatus: Ready\n")
		os.Exit(0)
	case "broken":
		os.Stdout.WriteString("ERROR: something else went wrong\n")
		os.Exit(1)

	// systemctl is-active prints one word and exits non-zero for anything
	// that is not active, so the word is the whole answer.
	case "unit-active":
		os.Stdout.WriteString("active\n")
		os.Exit(0)
	case "unit-inactive":
		os.Stdout.WriteString("inactive\n")
		os.Exit(3)
	case "unit-unknown":
		os.Stdout.WriteString("unknown\n")
		os.Exit(4)
	case "unit-unreachable":
		os.Stdout.WriteString("Failed to connect to bus: No medium found\n")
		os.Exit(1)

	// launchctl print writes a block describing the job, and refuses with a
	// message rather than a status when the job is not in the domain.
	case "job-running":
		os.Stdout.WriteString(launchdPrint("running"))
		os.Exit(0)
	case "job-waiting":
		os.Stdout.WriteString(launchdPrint("waiting"))
		os.Exit(0)
	case "job-unloaded":
		os.Stdout.WriteString("Could not find service \"stream-dvr\" in domain for gui\n")
		os.Exit(113)
	}
	os.Exit(0)
}

// launchdPrint renders a launchctl print block reporting the given state.
func launchdPrint(state string) string {
	return "gui/501/stream-dvr = {\n" +
		"\tactive count = 1\n" +
		"\tpath = /Users/operator/Library/LaunchAgents/stream-dvr.plist\n" +
		"\tstate = " + state + "\n" +
		"}\n"
}

// fakeExec redirects platform invocations to the helper process.
func fakeExec(t *testing.T, mode string) {
	t.Helper()

	recordExec(t, mode)
}

// recordExec redirects platform invocations to the helper process and keeps
// every argument vector, so a test can assert on what the manager actually
// asked the platform to do rather than only on what it returned.
func recordExec(t *testing.T, mode string) *execLog {
	t.Helper()

	// A coverage-instrumented binary with nowhere to write warns on stdout
	// as it exits, and the helper's stdout is the platform's answer as far
	// as the manager is concerned. Giving it a directory keeps the answer to
	// what the mode wrote.
	coverDir := t.TempDir()

	log := &execLog{}
	original := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		log.calls = append(log.calls, append([]string{name}, args...))

		helper := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
		cmd := exec.CommandContext(context.Background(), os.Args[0], helper...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"FAKE_MODE="+mode,
			"GOCOVERDIR="+coverDir,
		)
		return cmd
	}
	t.Cleanup(func() { execCommand = original })
	return log
}

// ran reports whether the platform was invoked with exactly this argument
// vector.
func (l *execLog) ran(want ...string) bool {
	for _, call := range l.calls {
		if slices.Equal(call, want) {
			return true
		}
	}
	return false
}

// definition returns a definition for tests.
func definition() Definition {
	return Definition{
		Name:        "stream-dvr",
		Description: "Records live streams.",
		Executable:  absoluteExecutable(),
		Args:        []string{"serve"},
	}
}

// absoluteExecutable returns a path validate accepts on the host.
//
// filepath.IsAbs answers by the host's own rules, so one hard-coded path
// cannot serve every platform: a drive letter is a relative path to a Unix
// build, and validate refuses it before any platform code runs. Both forms
// carry a space, which is what the generated unit and the scheduler's
// argument string have to survive.
func absoluteExecutable() string {
	if runtime.GOOS == "windows" {
		return `C:\Program Files\stream-dvr\stream-dvr.exe`
	}
	return "/opt/stream dvr/stream-dvr"
}

// ///////////////////////////////////////////////
// Validation
// ///////////////////////////////////////////////

func TestValidate(t *testing.T) {
	absolute := definition().Executable

	tests := []struct {
		name    string
		def     Definition
		wantErr bool
	}{
		{name: "complete", def: definition()},
		{name: "no name", def: Definition{Executable: absolute}, wantErr: true},
		{name: "no executable", def: Definition{Name: "x"}, wantErr: true},
		{
			// The name becomes a file name on every platform, so a
			// separator in it files the registration outside the directory
			// it was meant for.
			name:    "a name that traverses",
			def:     Definition{Name: `..\..\evil`, Executable: absolute},
			wantErr: true,
		},
		{
			name:    "a name with a forward slash",
			def:     Definition{Name: "a/b", Executable: absolute},
			wantErr: true,
		},
		{
			// systemctl and launchctl take the name as an operand, where a
			// leading slash reads as a path.
			name:    "a name that looks like a switch",
			def:     Definition{Name: "/Delete", Executable: absolute},
			wantErr: true,
		},
		{
			name:    "a name that is only spaces",
			def:     Definition{Name: "   ", Executable: absolute},
			wantErr: true,
		},
		{
			// A relative path resolves against whatever directory the
			// scheduler happens to run in at boot.
			name:    "a relative executable",
			def:     Definition{Name: "stream-dvr", Executable: "stream-dvr.exe"},
			wantErr: true,
		},
		{
			// A newline ends a systemd directive and starts one of the
			// writer's choosing, in a file that runs at every boot.
			name: "a line break in the description",
			def: Definition{
				Name: "stream-dvr", Executable: absolute,
				Description: "Records\nExecStartPre=/bin/sh -c id",
			},
			wantErr: true,
		},
		{
			name: "a line break in an argument",
			def: Definition{
				Name: "stream-dvr", Executable: absolute,
				Args: []string{"serve", "--config", "/tmp/a.toml\nUser=root"},
			},
			wantErr: true,
		},
		{
			name: "a carriage return in an argument",
			def: Definition{
				Name: "stream-dvr", Executable: absolute,
				Args: []string{"serve", "--config", "/tmp/a.toml\rUser=root"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(tt.def)
			if tt.wantErr && err == nil {
				t.Error("validate() err = nil, want a rejection")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("validate() err = %v, want nil", err)
			}
		})
	}
}

func TestManager_EveryEntryRejectsAHostileName(t *testing.T) {
	// The name becomes a file name on every platform and an operand to
	// systemctl and launchctl. Checking it at Install alone leaves every
	// other entry taking a name from its caller and passing it through.
	names := []string{
		"",
		`..\..\evil`,
		"a/b",
		"/Delete",
		"   ",
		"stream dvr",
		"-foreground",
		strings.Repeat("a", 65),
	}

	entries := []struct {
		name   string
		invoke func(Manager, string) error
	}{
		{name: "Uninstall", invoke: Manager.Uninstall},
		{name: "Start", invoke: Manager.Start},
		{name: "Stop", invoke: Manager.Stop},
		{name: "Status", invoke: func(m Manager, name string) error { _, err := m.Status(name); return err }},
	}

	for _, entry := range entries {
		for _, name := range names {
			t.Run(entry.name+"/"+name, func(t *testing.T) {
				refuseExec(t)

				manager, err := New()
				if err != nil {
					t.Fatalf("New() err = %v, want nil", err)
				}
				if err := entry.invoke(manager, name); err == nil {
					t.Errorf("%s(%q) err = nil, want a rejection", entry.name, name)
				}
			})
		}
	}
}

// refuseExec fails the test if anything reaches the platform's scheduler.
//
// A name has to be refused before it becomes a command-line operand or a
// unit path, so reaching the scheduler at all is the failure.
func refuseExec(t *testing.T) {
	t.Helper()

	original := execCommand
	execCommand = func(name string, args ...string) *exec.Cmd {
		t.Errorf("reached the scheduler as %s %q, want the name refused first", name, args)
		return exec.Command(os.Args[0], "-test.run=TestHelperProcess", "--")
	}
	t.Cleanup(func() { execCommand = original })
}

// ///////////////////////////////////////////////
// Manager construction
// ///////////////////////////////////////////////

func TestNew(t *testing.T) {
	manager, err := New()
	if err != nil {
		t.Fatalf("New() err = %v, want nil", err)
	}
	if manager.Mechanism() == "" {
		t.Error("Mechanism() is empty, want it to name the platform facility")
	}
}

func TestNew_SupportsThisPlatform(t *testing.T) {
	// Every platform the gate builds for has a registration mechanism of its
	// own. A manager that construction refuses is the shape of a platform
	// nobody implemented, and finding that here beats finding it when an
	// operator runs install.
	manager, err := New()
	if err != nil {
		t.Fatalf("New() on %s err = %v, want an autostart mechanism", runtime.GOOS, err)
	}
	if errors.Is(err, ErrUnsupported) {
		t.Fatalf("New() on %s refused as unsupported", runtime.GOOS)
	}
	if manager.Mechanism() == "unsupported" {
		t.Errorf("Mechanism() = %q on %s, want the platform's own facility",
			manager.Mechanism(), runtime.GOOS)
	}
}

// ///////////////////////////////////////////////
// State
// ///////////////////////////////////////////////

func TestState_ValuesAreDistinct(t *testing.T) {
	// A state that duplicated another would report a registration as
	// something it is not, and the compiler is happy either way because
	// these are strings. StateDisabled is here because telling it apart
	// from StateInstalled is the whole reason it exists.
	states := []State{StateAbsent, StateInstalled, StateRunning, StateDisabled}

	seen := make(map[State]bool, len(states))
	for _, state := range states {
		if state == "" {
			t.Error("a state value is empty")
		}
		if seen[state] {
			t.Errorf("state %q is duplicated", state)
		}
		seen[state] = true
	}
}
