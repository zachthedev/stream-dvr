package service

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// runKeyAutostart starts the notify agent from the per-user Run key.
//
// The Run key is the one way to start a process in the operator's session
// that needs no privilege. A logon-triggered scheduled task is tidier and
// carries no malware association, but registering one needs an elevated
// shell, and a notification helper is not worth asking an operator to
// elevate for.
//
// The root and path are fields so a test can register somewhere that is
// not the key Windows actually reads at logon.
type runKeyAutostart struct {
	root  registry.Key
	path  string
	value string
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// runKeyPath is where Windows looks at every interactive logon. The
// per-user key rather than the machine one: the agent is useful only to
// the operator whose session it runs in, and the machine key needs
// administrator rights.
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// runKeyValue names this project's entry. It is distinct from the
// recorder's scheduled task name, because the two are separate
// registrations and an operator reading either list has to see which is
// which.
const runKeyValue = "stream-dvr-notify"

// ///////////////////////////////////////////////
// Constructor
// ///////////////////////////////////////////////

// newAgentAutostart returns the Run key registration.
func newAgentAutostart() (AgentAutostart, error) {
	return runKeyAutostart{
		root:  registry.CURRENT_USER,
		path:  runKeyPath,
		value: runKeyValue,
	}, nil
}

// ///////////////////////////////////////////////
// AgentAutostart
// ///////////////////////////////////////////////

// Install writes the command Windows runs at logon.
func (r runKeyAutostart) Install(executable string, args []string) error {
	if strings.TrimSpace(executable) == "" {
		return errors.New("registering the notify agent needs the path to this binary")
	}

	key, _, err := registry.CreateKey(r.root, r.path, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("opening the Run key: %w", err)
	}
	defer key.Close()

	if err := key.SetStringValue(r.value, runCommand(executable, args)); err != nil {
		return fmt.Errorf("registering %s to start at logon: %w", r.value, err)
	}
	return nil
}

// Uninstall removes the registration, and reports nothing for one that was
// never there.
func (r runKeyAutostart) Uninstall() error {
	key, err := registry.OpenKey(r.root, r.path, registry.SET_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("opening the Run key: %w", err)
	}
	defer key.Close()

	if err := key.DeleteValue(r.value); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("removing %s from the Run key: %w", r.value, err)
	}
	return nil
}

// Installed reports whether the entry is there.
func (r runKeyAutostart) Installed() (bool, error) {
	key, err := registry.OpenKey(r.root, r.path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("opening the Run key: %w", err)
	}
	defer key.Close()

	switch _, _, err := key.GetStringValue(r.value); {
	case errors.Is(err, registry.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading %s from the Run key: %w", r.value, err)
	}
	return true, nil
}

// Mechanism names the facility.
func (r runKeyAutostart) Mechanism() string {
	return "logon entry in " + r.path
}

// ///////////////////////////////////////////////
// Command
// ///////////////////////////////////////////////

// runCommand builds the command line Windows runs at logon.
//
// The executable is quoted because a program under "Program Files" has a
// space in its path, and Windows reads an unquoted one as a program name
// followed by arguments. Arguments are quoted only when they need it, so
// the entry stays readable to an operator looking at what runs at logon.
func runCommand(executable string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, `"`+executable+`"`)

	for _, arg := range args {
		if strings.ContainsAny(arg, ` "`) {
			// A quote inside an argument is doubled rather than escaped
			// with a backslash, because the path ahead of it may end in one.
			parts = append(parts, `"`+strings.ReplaceAll(arg, `"`, `""`)+`"`)
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}
