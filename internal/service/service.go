// Package service registers stream-dvr to start on its own and removes that
// registration again.
//
// Each platform gets its own mechanism: a scheduled task on Windows, a
// systemd user unit on Linux, and a launchd user agent on macOS.
//
// Everything here is user-scoped on purpose. The obvious mechanism on
// Windows is a service, on Linux a system unit, and on macOS a daemon in
// /Library, but all three run as a machine account that cannot see the
// operator's home directory. streamlink reads its credentials from the
// user's own config, so a machine-scoped registration would record only what
// is public and silently miss subscriber and ad-free streams.
//
// Recording itself needs no elevation. Changing a scheduled task on Windows
// does, because a task definition in the scheduler's root folder is
// administrative whatever principal the task names. Any operation the shell
// lacks the privilege for reports ErrElevationRequired. A systemd user unit
// and a launchd user agent need none.
package service

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// Definition describes the registration to create.
type Definition struct {
	// Name identifies the registration to the operating system.
	Name string
	// Description is shown by the platform's own tooling.
	Description string
	// Executable is the absolute path to the binary to run.
	Executable string
	// Args are the arguments passed to it.
	Args []string
}

// State is a registration's current condition.
type State string

// Status reports a registration's condition and where it lives, so an
// operator can find it with the platform's own tools.
type Status struct {
	// State is the current condition.
	State State
	// Detail names the underlying object, such as a task or unit name.
	Detail string
}

// Manager creates and removes a platform's autostart registration.
type Manager interface {
	// Install registers the definition, replacing an existing
	// registration of the same name.
	Install(def Definition) error
	// Uninstall removes the registration. Removing one that does not
	// exist is not an error, so uninstall is safe to repeat.
	Uninstall(name string) error
	// Start runs the registered recorder now, without waiting for the
	// next boot or logon.
	Start(name string) error
	// Stop ends the running recorder. The registration survives, so it
	// starts again at the next boot; removing it is Uninstall's job.
	Stop(name string) error
	// Status reports the registration's condition.
	Status(name string) (Status, error)
	// Mechanism names the platform facility in use, for output that tells
	// the operator where to look.
	Mechanism() string
}

// State values.
const (
	// StateAbsent means nothing is registered under the name.
	StateAbsent State = "absent"
	// StateInstalled means a registration exists but is not running.
	StateInstalled State = "installed"
	// StateRunning means the registration exists and its process is up.
	StateRunning State = "running"
	// StateDisabled means a registration exists and will never start. It is
	// distinct from StateInstalled because the two look identical to an
	// operator and only one of them records anything.
	StateDisabled State = "disabled"
)

// managerTimeout bounds one call to a service manager.
const managerTimeout = 30 * time.Second

// execCommand builds a subprocess. Tests substitute a helper process so the
// managers are exercised without touching the machine's real scheduler.
var execCommand = boundedCommand

// validName bounds a registration name.
//
// The name becomes a file name under the unit or agent directory and a task
// path under the scheduler's root folder. systemctl and launchctl also take
// it as an operand. A separator, a leading dash, or a space would let it
// escape a path or read as a switch.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

var (
	// ErrUnsupported reports a platform with no implementation.
	ErrUnsupported = errors.New("automatic startup is not supported on this platform")

	// ErrElevationRequired reports an operation the current shell lacks the
	// privilege for. It carries no verb, because the caller that raises it
	// names the operation it was refused. It is declared here rather than
	// beside the platform that raises it so callers can test for it without
	// build tags of their own.
	ErrElevationRequired = errors.New("needs an elevated shell")
)

// New returns the manager for the host platform.
// boundedCommand builds a service-manager invocation with a deadline.
//
// systemctl, loginctl and launchctl all talk to a manager that can be
// wedged, and an interactive install or status that waits on one has no
// timer of its own: the command hangs until the operator kills it. None of
// this is on the recording path, so the bound is generous.
func boundedCommand(name string, args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), managerTimeout)
	//nolint:gosec // G204: the name is one of three literals this package
	// passes, and the arguments are built from a validated Definition.
	cmd := exec.CommandContext(ctx, name, args...)
	// The context outlives this function, so cancelling is tied to the
	// command finishing rather than to the scope it was built in.
	cmd.Cancel = func() error {
		cancel()
		return cmd.Process.Kill()
	}
	context.AfterFunc(ctx, func() {})
	return cmd
}

func New() (Manager, error) {
	manager, err := newManager()
	if err != nil {
		return nil, err
	}
	return manager, nil
}

// validateName checks a registration name before it becomes a filename or a
// command-line operand.
//
// Every exported method takes the name from its caller, so the check belongs
// at each of those entries rather than at Install alone. A separator, a
// leading dash, or a space reaching Uninstall or Status escapes a unit path
// or is read as a switch, exactly as it would at Install.
func validateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("service name %q must match %s", name, validName)
	}
	return nil
}

// validate checks a definition before it reaches the platform.
//
// Every platform turns a definition into a name the operating system files
// it under and a generated document. The checks that keep a value from
// escaping either one belong here rather than in one implementation.
func validate(def Definition) error {
	if err := validateName(def.Name); err != nil {
		return err
	}
	if def.Executable == "" {
		return fmt.Errorf("service executable is required")
	}
	if !filepath.IsAbs(def.Executable) {
		return fmt.Errorf("service executable %q must be an absolute path", def.Executable)
	}

	// A newline ends a directive in a systemd unit, so a value carrying one
	// writes unit directives of its own choosing. The generated file is a
	// persistence mechanism nobody re-reads, which is what makes this worth
	// refusing rather than escaping.
	for field, value := range map[string]string{
		"description": def.Description,
		"executable":  def.Executable,
	} {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("service %s must not contain a line break", field)
		}
	}
	for i, arg := range def.Args {
		if strings.ContainsAny(arg, "\r\n") {
			return fmt.Errorf("service argument %d must not contain a line break", i)
		}
	}
	return nil
}
