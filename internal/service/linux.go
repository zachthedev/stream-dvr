//go:build linux

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// systemdManager registers a systemd user unit.
//
// A user unit rather than a system one, for the same reason Windows gets a
// task rather than a service: a system unit runs as a machine account with
// no access to the operator's streamlink credentials. Linger is enabled so
// the unit survives logout, which a user unit otherwise does not.
type systemdManager struct {
	// unitDir is where user units live. It is a field so tests can point
	// it at a temporary directory.
	unitDir string
	// searchPath is the PATH the recorder runs with.
	searchPath string
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// restartDelay is the pause before systemd starts the recorder again, in
// seconds. Long enough that a channel outage is not hammered, short enough
// that a crash costs little of a broadcast.
const restartDelay = 30

// restartBurst is how many restarts inside startLimitWindow systemd allows
// before it gives up.
const restartBurst = 5

// startLimitWindow is the span the burst is counted over, in seconds.
//
// It has to exceed restartBurst * restartDelay or the burst is never
// reachable and the limit never trips: a unit that exits immediately every
// time would restart on the same schedule forever, which is what an
// unreadable config or a missing library produces.
const startLimitWindow = 600

// stopTimeout is how long systemd waits for a graceful shutdown before it
// resorts to SIGKILL, in seconds.
//
// The daemon answers SIGTERM by finalizing what it is capturing, and
// remuxing a multi-gigabyte recording is minutes of work. The default 90
// seconds kills that halfway through and leaves the recording in incoming/.
const stopTimeout = 300

// ///////////////////////////////////////////////
// Constructor
// ///////////////////////////////////////////////

// newManager returns the manager for this platform.
func newManager() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locating the user's home directory: %w", err)
	}
	return systemdManager{
		unitDir:    filepath.Join(home, ".config", "systemd", "user"),
		searchPath: searchPath(home),
	}, nil
}

// Mechanism implements Manager.
func (systemdManager) Mechanism() string { return "systemd user unit" }

// ///////////////////////////////////////////////
// Manager
// ///////////////////////////////////////////////

// Install implements Manager.
func (m systemdManager) Install(def Definition) error {
	if err := validate(def); err != nil {
		return err
	}
	if err := os.MkdirAll(m.unitDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", m.unitDir, err)
	}

	// The user's own systemd instance reads the unit, so nothing outside the
	// account needs it. It names the config file and the library root, which
	// is a map of where the recordings are.
	unit := m.unitPath(def.Name)
	if err := os.WriteFile(unit, []byte(m.unitFile(def)), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", unit, err)
	}

	// A unit file left behind by a failed install is worse than no file at
	// all: Status keys off its existence, so it would report the recorder
	// as installed forever while nothing ever starts it.
	installed := false
	defer func() {
		if !installed {
			os.Remove(unit) // the install failure is what matters
		}
	}()

	if output, err := execCommand("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("reloading systemd: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := execCommand("systemctl", "--user", "enable", "--now",
		def.Name+".service").CombinedOutput(); err != nil {
		return fmt.Errorf("enabling %s: %w: %s", def.Name, err, strings.TrimSpace(string(output)))
	}

	// Without linger a user unit stops at logout, which for a recorder
	// means missing every broadcast while nobody is signed in.
	if output, err := execCommand("loginctl", "enable-linger").CombinedOutput(); err != nil {
		return fmt.Errorf("enabling linger: %w: %s", err, strings.TrimSpace(string(output)))
	}

	installed = true
	return nil
}

// Uninstall implements Manager.
func (m systemdManager) Uninstall(name string) error {
	if err := validateName(name); err != nil {
		return err
	}

	unit := m.unitPath(name)
	if _, err := os.Stat(unit); os.IsNotExist(err) {
		return nil
	}

	// A failed disable does not stop removal, since a unit that is already
	// stopped must still lose its file. It does have to be reported: the
	// registration is gone from disk either way, and if the recorder is
	// still up then nothing on disk points at it any more.
	disableOutput, disableErr := execCommand("systemctl", "--user",
		"disable", "--now", name+".service").CombinedOutput()

	if err := os.Remove(unit); err != nil {
		return fmt.Errorf("removing %s: %w", unit, err)
	}
	if output, err := execCommand("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("reloading systemd: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if disableErr != nil {
		return fmt.Errorf("removed %s but could not stop it, it may still be running: %w: %s",
			unit, disableErr, strings.TrimSpace(string(disableOutput)))
	}
	return nil
}

// Start implements Manager.
func (m systemdManager) Start(name string) error {
	return m.control(name, "start", "starting")
}

// Stop implements Manager.
//
// The unit stays enabled, so the recorder starts again at the next login.
// Removing it for good is Uninstall's job.
func (m systemdManager) Stop(name string) error {
	return m.control(name, "stop", "stopping")
}

// control runs a systemctl verb against the unit.
func (m systemdManager) control(name, verb, action string) error {
	if err := validateName(name); err != nil {
		return err
	}

	output, err := execCommand("systemctl", "--user", verb, name+".service").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", action, name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Status implements Manager.
func (m systemdManager) Status(name string) (Status, error) {
	if err := validateName(name); err != nil {
		return Status{}, err
	}

	if _, err := os.Stat(m.unitPath(name)); os.IsNotExist(err) {
		return Status{State: StateAbsent, Detail: name + ".service"}, nil
	}

	// is-active exits non-zero for a unit that is merely stopped, so the
	// exit status alone cannot separate that from systemd being
	// unreachable. The word it prints can.
	detail := name + ".service"
	output, _ := execCommand("systemctl", "--user", "is-active", detail).CombinedOutput()

	state, known := unitState(output)
	if !known {
		return Status{}, fmt.Errorf("querying %s: %s", detail, strings.TrimSpace(string(output)))
	}
	return Status{State: state, Detail: detail}, nil
}

// unitState reads the unit's condition out of an is-active answer.
//
// One unit is asked about and one word comes back, but the call merges
// stderr, so a warning from dbus or from the locale arrives on the same
// stream. Matching the word on its own line rather than the whole output
// keeps a running recorder from reading as a query that failed.
//
// An answer carrying no state word at all is not an answer, and the second
// return says so. Reporting that as installed would tell an operator the
// recorder is stopped while it is recording.
func unitState(output []byte) (State, bool) {
	for line := range strings.SplitSeq(string(output), "\n") {
		switch strings.TrimSpace(line) {
		case "active", "activating", "reloading":
			return StateRunning, true
		case "inactive", "failed", "deactivating":
			return StateInstalled, true
		case "unknown":
			return StateAbsent, true
		}
	}
	return "", false
}

// ///////////////////////////////////////////////
// Unit file
// ///////////////////////////////////////////////

// unitPath returns the unit file's location.
func (m systemdManager) unitPath(name string) string {
	return filepath.Join(m.unitDir, name+".service")
}

// unitWord renders one value the way systemd parses it back.
//
// systemd splits a directive on whitespace, so a library root under a path
// with a space would reach the recorder as two operands. Quoting keeps the
// value whole, a '%' has to be doubled or systemd expands it as a specifier,
// and a line break has to become its escape or it ends the directive and
// starts one of the writer's choosing.
func unitWord(value string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"%", "%%",
		"\n", `\n`,
		"\r", `\r`,
	).Replace(value)
	return `"` + escaped + `"`
}

// unitCommand renders ExecStart as the argument vector the caller asked for.
func unitCommand(def Definition) string {
	words := make([]string, 0, len(def.Args)+1)
	for _, word := range append([]string{def.Executable}, def.Args...) {
		words = append(words, unitWord(word))
	}
	return strings.Join(words, " ")
}

// unitFile renders the unit.
//
// Every interpolated value is refused a line break by validate, because a
// newline here ends a directive and starts one of the writer's choosing in a
// file that runs at every boot and that nobody reads again.
//
// KillMode=mixed sends SIGTERM to the daemon alone and leaves the rest of
// the control group to it. The default signals every process at once, which
// takes streamlink and ffmpeg down at the same instant as the daemon and
// leaves it nothing to finalize.
func (m systemdManager) unitFile(def Definition) string {
	return fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target
StartLimitIntervalSec=%d
StartLimitBurst=%d

[Service]
Type=simple
ExecStart=%s
Environment=%s
Restart=always
RestartSec=%d
TimeoutStopSec=%d
KillMode=mixed

[Install]
WantedBy=default.target
`,
		unitWord(def.Description),
		startLimitWindow,
		restartBurst,
		unitCommand(def),
		unitWord("PATH="+m.searchPath),
		restartDelay,
		stopTimeout,
	)
}
