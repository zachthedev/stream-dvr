//go:build linux

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// systemdFixture returns a manager writing units into a temp directory.
func systemdFixture(t *testing.T) systemdManager {
	t.Helper()

	return systemdManager{
		unitDir:    filepath.Join(t.TempDir(), "systemd", "user"),
		searchPath: searchPath("/home/operator"),
	}
}

// registered returns a manager with the unit already on disk, which is the
// state every method other than Install acts on.
func registered(t *testing.T, name string) systemdManager {
	t.Helper()

	manager := systemdFixture(t)
	if err := os.MkdirAll(manager.unitDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", manager.unitDir, err)
	}
	if err := os.WriteFile(manager.unitPath(name), []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatalf("writing the unit: %v", err)
	}
	return manager
}

// ///////////////////////////////////////////////
// Unit file
// ///////////////////////////////////////////////

func TestSystemdManager_UnitFile(t *testing.T) {
	unit := systemdFixture(t).unitFile(definition())

	tests := []struct {
		name string
		want string
		why  string
	}{
		{
			name: "starts at login rather than at boot",
			want: "WantedBy=default.target",
			why:  "a user unit belongs to the user's own target",
		},
		{
			name: "restarts after a crash",
			want: "Restart=always",
			why:  "a recorder that dies must come back without a person noticing",
		},
		{
			name: "waits for the network",
			want: "After=network-online.target",
			why:  "probing a channel before the network is up fails every time",
		},
		{
			name: "runs the command",
			want: definition().Executable,
			why:  "the unit has to name the binary",
		},
		{
			name: "passes the arguments",
			want: "serve",
			why:  "without them the binary prints help and exits",
		},
		{
			name: "gives a graceful shutdown time to finish",
			want: "TimeoutStopSec=",
			why:  "remuxing a finished recording outlives the 90-second default",
		},
		{
			name: "signals the daemon rather than the whole group",
			want: "KillMode=mixed",
			why:  "killing streamlink alongside it leaves the daemon nothing to finalize",
		},
		{
			name: "counts restarts over a window that can be filled",
			want: "StartLimitIntervalSec=",
			why:  "the default window is shorter than one RestartSec, so the burst never trips",
		},
		{
			name: "gives up after a burst",
			want: "StartLimitBurst=",
			why:  "a config error must stop rather than restart forever",
		},
		{
			name: "sets a PATH the tools are actually on",
			want: "Environment=",
			why:  "the unit otherwise inherits systemd's minimal PATH and finds no streamlink",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(unit, tt.want) {
				t.Errorf("unit file missing %q (%s):\n%s", tt.want, tt.why, unit)
			}
		})
	}
}

func TestSystemdManager_UnitFileBoundsARestartLoop(t *testing.T) {
	// systemd counts the burst over StartLimitIntervalSec, which defaults to
	// 10 seconds. A unit that pauses RestartSec between attempts can never
	// fill a window shorter than that, so the limit never trips and a
	// recorder that cannot read its config restarts on the same schedule for
	// as long as the machine is up.
	if startLimitWindow <= restartBurst*restartDelay {
		t.Errorf("startLimitWindow = %d, want more than restartBurst*restartDelay = %d",
			startLimitWindow, restartBurst*restartDelay)
	}

	unit := systemdFixture(t).unitFile(definition())
	for _, want := range []string{
		"StartLimitIntervalSec=600",
		"StartLimitBurst=5",
		"RestartSec=30",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit file missing %q:\n%s", want, unit)
		}
	}
}

func TestSystemdManager_UnitFileCarriesTheToolDirectories(t *testing.T) {
	// The classic works-in-my-terminal-fails-as-a-service trap: doctor
	// resolves streamlink from a login shell and the daemon, running with
	// systemd's own PATH, then reports it missing.
	unit := systemdFixture(t).unitFile(definition())

	line := ""
	for candidate := range strings.SplitSeq(unit, "\n") {
		if strings.HasPrefix(candidate, "Environment=") {
			line = candidate
		}
	}
	if line == "" {
		t.Fatalf("no Environment directive in:\n%s", unit)
	}

	for _, want := range []string{
		"/home/operator/.local/bin",
		"/usr/local/bin",
		"/opt/homebrew/bin",
		"/snap/bin",
		"/usr/bin",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("PATH missing %q: %s", want, line)
		}
	}
}

func TestSystemdManager_UnitPath(t *testing.T) {
	manager := systemdFixture(t)

	got := manager.unitPath("stream-dvr")
	if filepath.Base(got) != "stream-dvr.service" {
		t.Errorf("unitPath() = %q, want it to end in .service", got)
	}
	if !strings.HasPrefix(got, manager.unitDir) {
		t.Errorf("unitPath() = %q, want it under %q", got, manager.unitDir)
	}
}

func TestUnitWord(t *testing.T) {
	// The unit is a file that runs at every boot and that nobody reads
	// again, so a value that escapes its directive picks what runs.
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "plain", value: "serve", want: `"serve"`},
		{name: "a path with a space", value: "/opt/stream dvr/x", want: `"/opt/stream dvr/x"`},
		{name: "a backslash", value: `a\b`, want: `"a\\b"`},
		{name: "a quote", value: `a"b`, want: `"a\"b"`},
		{
			// systemd expands a bare % as a specifier, so %h would become
			// the home directory of whoever the unit runs as.
			name:  "a specifier",
			value: "%h",
			want:  `"%%h"`,
		},
		{
			// A raw newline ends the directive whatever quoting surrounds
			// it, and starts one of the writer's choosing.
			name:  "a line break",
			value: "a\nUser=root",
			want:  `"a\nUser=root"`,
		},
		{name: "a carriage return", value: "a\rb", want: `"a\rb"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unitWord(tt.value)
			if got != tt.want {
				t.Errorf("unitWord(%q) = %s, want %s", tt.value, got, tt.want)
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("unitWord(%q) = %s, want no raw line break", tt.value, got)
			}
		})
	}
}

func TestUnitCommand(t *testing.T) {
	def := definition()
	def.Args = []string{"serve", "--config", "/etc/stream dvr/config.toml"}

	got := unitCommand(def)
	for _, want := range []string{
		`"` + def.Executable + `"`,
		`"serve"`,
		`"--config"`,
		`"/etc/stream dvr/config.toml"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unitCommand() = %s, want it to contain %s", got, want)
		}
	}
}

// ///////////////////////////////////////////////
// Install
// ///////////////////////////////////////////////

func TestSystemdManager_Install(t *testing.T) {
	calls := recordExec(t, "ok")
	manager := systemdFixture(t)
	def := definition()

	if err := manager.Install(def); err != nil {
		t.Fatalf("Install() err = %v, want nil", err)
	}

	t.Run("writes the unit", func(t *testing.T) {
		unit, err := os.ReadFile(manager.unitPath(def.Name))
		if err != nil {
			t.Fatalf("reading the unit: %v", err)
		}
		if !strings.Contains(string(unit), def.Executable) {
			t.Errorf("unit does not name the binary:\n%s", unit)
		}
	})

	t.Run("reloads systemd", func(t *testing.T) {
		// A unit systemd has not re-read is a file on disk and nothing else.
		if !calls.ran("systemctl", "--user", "daemon-reload") {
			t.Errorf("calls = %v, want a daemon-reload", calls.calls)
		}
	})

	t.Run("enables and starts the unit", func(t *testing.T) {
		if !calls.ran("systemctl", "--user", "enable", "--now", "stream-dvr.service") {
			t.Errorf("calls = %v, want enable --now", calls.calls)
		}
	})

	t.Run("enables linger", func(t *testing.T) {
		// Without it the unit stops at logout, which for a recorder means
		// missing every broadcast while nobody is signed in.
		if !calls.ran("loginctl", "enable-linger") {
			t.Errorf("calls = %v, want linger enabled", calls.calls)
		}
	})

	t.Run("keeps the unit to the account", func(t *testing.T) {
		// The unit names the config file and the library root, and only the
		// user's own systemd instance ever reads it.
		info, err := os.Stat(manager.unitPath(def.Name))
		if err != nil {
			t.Fatalf("Stat() err = %v", err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("unit mode = %o, want nothing for group or other", perm)
		}
	})
}

func TestSystemdManager_InstallLeavesNoUnitBehindWhenItFails(t *testing.T) {
	// Status keys off the unit file's existence, so one left behind by a
	// failed install reports the recorder as installed forever while nothing
	// ever starts it.
	fakeExec(t, "broken")
	manager := systemdFixture(t)

	if err := manager.Install(definition()); err == nil {
		t.Fatal("Install() err = nil, want the failure reported")
	}
	if _, err := os.Stat(manager.unitPath(definition().Name)); !os.IsNotExist(err) {
		t.Errorf("Stat() err = %v, want the unit removed", err)
	}
}

func TestSystemdManager_InstallRejectsAnIncompleteDefinition(t *testing.T) {
	if err := systemdFixture(t).Install(Definition{}); err == nil {
		t.Error("Install() err = nil, want a rejection")
	}
}

// ///////////////////////////////////////////////
// Status
// ///////////////////////////////////////////////

func TestSystemdManager_Status(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		want    State
		wantErr bool
	}{
		{name: "active", mode: "unit-active", want: StateRunning},
		{name: "stopped", mode: "unit-inactive", want: StateInstalled},
		{
			// systemd knows nothing of the unit even though its file is
			// there, which is what an unreloaded install looks like.
			name: "unknown to systemd",
			mode: "unit-unknown",
			want: StateAbsent,
		},
		{
			// A query that could not be answered is not an answer. Reporting
			// it as installed would tell an operator the recorder is stopped
			// while it is recording.
			name:    "systemd unreachable",
			mode:    "unit-unreachable",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeExec(t, tt.mode)

			got, err := registered(t, "stream-dvr").Status("stream-dvr")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Status() err = nil, want the failed query reported")
				}
				if got.State != "" {
					t.Errorf("State = %q, want no claim about a query that failed", got.State)
				}
				return
			}
			if err != nil {
				t.Fatalf("Status() err = %v, want nil", err)
			}
			if got.State != tt.want {
				t.Errorf("State = %q, want %q", got.State, tt.want)
			}
		})
	}
}

func TestUnitState(t *testing.T) {
	// The query merges stderr, so anything systemd or dbus has to say about
	// the environment lands on the same stream as the answer.
	tests := []struct {
		name      string
		output    string
		want      State
		wantKnown bool
	}{
		{name: "active", output: "active\n", want: StateRunning, wantKnown: true},
		{name: "activating", output: "activating\n", want: StateRunning, wantKnown: true},
		{name: "reloading", output: "reloading\n", want: StateRunning, wantKnown: true},
		{name: "inactive", output: "inactive\n", want: StateInstalled, wantKnown: true},
		{name: "failed", output: "failed\n", want: StateInstalled, wantKnown: true},
		{name: "deactivating", output: "deactivating\n", want: StateInstalled, wantKnown: true},
		{name: "unknown", output: "unknown\n", want: StateAbsent, wantKnown: true},
		{
			name:      "a warning after the answer",
			output:    "active\nwarning: something on stderr\n",
			want:      StateRunning,
			wantKnown: true,
		},
		{
			name:      "a warning before the answer",
			output:    "warning: something on stderr\nactive\n",
			want:      StateRunning,
			wantKnown: true,
		},
		{
			// A query that could not be answered is not an answer, and
			// calling it installed would report a running recorder stopped.
			name:   "no answer at all",
			output: "Failed to connect to bus: No medium found\n",
		},
		{name: "nothing", output: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, known := unitState([]byte(tt.output))
			if known != tt.wantKnown {
				t.Fatalf("unitState(%q) known = %v, want %v", tt.output, known, tt.wantKnown)
			}
			if got != tt.want {
				t.Errorf("unitState(%q) = %q, want %q", tt.output, got, tt.want)
			}
		})
	}
}

func TestSystemdManager_StatusAbsentWithoutAUnit(t *testing.T) {
	// Reporting absent must not require systemd to be running, because the
	// common case is a machine where nothing has been installed yet.
	refuseExec(t)

	got, err := systemdFixture(t).Status("stream-dvr")
	if err != nil {
		t.Fatalf("Status() err = %v, want nil", err)
	}
	if got.State != StateAbsent {
		t.Errorf("State = %q, want %q", got.State, StateAbsent)
	}
}

// ///////////////////////////////////////////////
// Uninstall
// ///////////////////////////////////////////////

func TestSystemdManager_Uninstall(t *testing.T) {
	calls := recordExec(t, "ok")
	manager := registered(t, "stream-dvr")

	if err := manager.Uninstall("stream-dvr"); err != nil {
		t.Fatalf("Uninstall() err = %v, want nil", err)
	}

	if !calls.ran("systemctl", "--user", "disable", "--now", "stream-dvr.service") {
		t.Errorf("calls = %v, want disable --now", calls.calls)
	}
	if !calls.ran("systemctl", "--user", "daemon-reload") {
		t.Errorf("calls = %v, want a daemon-reload", calls.calls)
	}
	if _, err := os.Stat(manager.unitPath("stream-dvr")); !os.IsNotExist(err) {
		t.Errorf("Stat() err = %v, want the unit removed", err)
	}
}

func TestSystemdManager_UninstallReportsARecorderItCouldNotStop(t *testing.T) {
	// The registration is gone from disk either way. If the recorder is
	// still up then nothing on disk points at it any more, which is the one
	// state an operator has to be told about.
	fakeExec(t, "broken")
	manager := registered(t, "stream-dvr")

	err := manager.Uninstall("stream-dvr")
	if err == nil {
		t.Fatal("Uninstall() err = nil, want the failure reported")
	}
	if _, statErr := os.Stat(manager.unitPath("stream-dvr")); !os.IsNotExist(statErr) {
		t.Errorf("Stat() err = %v, want the unit removed anyway", statErr)
	}
}

func TestSystemdManager_UninstallIsSafeToRepeat(t *testing.T) {
	// Removing a registration that is not there is the desired end state.
	refuseExec(t)

	if err := systemdFixture(t).Uninstall("stream-dvr"); err != nil {
		t.Errorf("Uninstall() err = %v, want nil for an absent unit", err)
	}
}

// ///////////////////////////////////////////////
// Start and Stop
// ///////////////////////////////////////////////

func TestSystemdManager_StartAndStop(t *testing.T) {
	// The interface drives these, so a broken control path shows up as a
	// button that silently does nothing.
	tests := []struct {
		name   string
		invoke func(systemdManager, string) error
		verb   string
	}{
		{name: "start", invoke: systemdManager.Start, verb: "start"},
		{name: "stop", invoke: systemdManager.Stop, verb: "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.name+" reaches systemctl", func(t *testing.T) {
			calls := recordExec(t, "ok")

			if err := tt.invoke(systemdFixture(t), "stream-dvr"); err != nil {
				t.Fatalf("%s() err = %v, want nil", tt.name, err)
			}
			if !calls.ran("systemctl", "--user", tt.verb, "stream-dvr.service") {
				t.Errorf("calls = %v, want %s of the unit", calls.calls, tt.verb)
			}
		})

		t.Run(tt.name+" carries systemd's own message", func(t *testing.T) {
			fakeExec(t, "broken")

			err := tt.invoke(systemdFixture(t), "stream-dvr")
			if err == nil {
				t.Fatalf("%s() err = nil, want the failure reported", tt.name)
			}
			if !strings.Contains(err.Error(), "something else went wrong") {
				t.Errorf("%s() err = %q, want it to quote systemd", tt.name, err)
			}
		})

		t.Run(tt.name+" rejects an empty name", func(t *testing.T) {
			refuseExec(t)

			if err := tt.invoke(systemdFixture(t), ""); err == nil {
				t.Errorf("%s() err = nil, want a rejection", tt.name)
			}
		})
	}
}

func TestSystemdManager_RejectsAnEmptyName(t *testing.T) {
	refuseExec(t)
	manager := systemdFixture(t)

	if err := manager.Uninstall(""); err == nil {
		t.Error("Uninstall() err = nil, want a rejection")
	}
	if _, err := manager.Status(""); err == nil {
		t.Error("Status() err = nil, want a rejection")
	}
}

func TestSystemdManager_Mechanism(t *testing.T) {
	if systemdFixture(t).Mechanism() == "" {
		t.Error("Mechanism() is empty, want it to name the platform facility")
	}
}

// ///////////////////////////////////////////////
// Platform selection
// ///////////////////////////////////////////////

func TestNewManager(t *testing.T) {
	manager, err := newManager()
	if err != nil {
		t.Fatalf("newManager() err = %v, want nil", err)
	}

	systemd, ok := manager.(systemdManager)
	if !ok {
		t.Fatalf("newManager() = %T, want a systemd manager", manager)
	}
	if systemd.unitDir == "" {
		t.Error("unitDir is empty, want the user unit directory")
	}
	if systemd.searchPath == "" {
		t.Error("searchPath is empty, want the tool directories")
	}
}
