//go:build darwin

package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// launchdManager registers a launchd user agent.
//
// An agent in the operator's own LaunchAgents directory rather than a daemon
// in /Library, for the same reason Windows gets a task and Linux a user
// unit: a system daemon runs as root and cannot see the streamlink
// credentials in the operator's home directory, so it would record only what
// is public.
//
// The limit that comes with that choice: an agent in the gui domain is
// loaded when the operator signs in and unloaded when they sign out, so a
// Mac sitting at the login window records nothing. macOS has no equivalent
// of systemd's linger for a GUI agent. Windows and Linux do keep recording
// with nobody signed in, through an S4U task with a boot trigger and
// through loginctl enable-linger, so this is the one platform where that
// does not hold. Trading it away would mean a root daemon that cannot read
// the credentials, which records less rather than more.
type launchdManager struct {
	// agentDir is where per-user agents live. It is a field so tests can
	// point it at a temporary directory.
	agentDir string
	// domain is the launchd domain target the agent is bootstrapped into.
	domain string
	// searchPath is the PATH the recorder runs with.
	searchPath string
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// restartDelay is the pause before launchd starts the recorder again, in
// seconds. Long enough that a channel outage is not hammered, short enough
// that a crash costs little of a broadcast.
//
// launchd has no equivalent of a start-limit burst, so an agent that exits
// immediately every time restarts on this interval for as long as it keeps
// failing. The interval is the only bound available.
const restartDelay = 30

// stopTimeout is how long launchd waits for a graceful shutdown before it
// resorts to SIGKILL, in seconds.
//
// The daemon answers SIGTERM by finalizing what it is capturing, and
// remuxing a multi-gigabyte recording is minutes of work. The default 20
// seconds kills that halfway through and leaves the recording in incoming/.
const stopTimeout = 300

// plistHeader is the declaration and doctype every property list carries.
const plistHeader = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
`

// ///////////////////////////////////////////////
// Constructor
// ///////////////////////////////////////////////

// newManager returns the manager for this platform.
func newManager() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locating the user's home directory: %w", err)
	}
	return launchdManager{
		agentDir:   filepath.Join(home, "Library", "LaunchAgents"),
		domain:     fmt.Sprintf("gui/%d", os.Getuid()),
		searchPath: searchPath(home),
	}, nil
}

// Mechanism implements Manager.
func (launchdManager) Mechanism() string { return "launchd user agent" }

// ///////////////////////////////////////////////
// Manager
// ///////////////////////////////////////////////

// Install implements Manager.
func (m launchdManager) Install(def Definition) error {
	if err := validate(def); err != nil {
		return err
	}
	if err := os.MkdirAll(m.agentDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", m.agentDir, err)
	}

	document, err := m.agentFile(def)
	if err != nil {
		return err
	}

	// The user's own launchd reads the agent, so nothing outside the account
	// needs it. It names the config file and the library root, which is a
	// map of where the recordings are.
	agent := m.agentPath(def.Name)
	if err := os.WriteFile(agent, document, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", agent, err)
	}

	// An agent file left behind by a failed install is worse than no file at
	// all: Status keys off its existence, so it would report the recorder as
	// installed forever while nothing ever starts it.
	installed := false
	defer func() {
		if !installed {
			os.Remove(agent) // the install failure is what matters
		}
	}()

	// A job already in the domain refuses a second bootstrap, so any earlier
	// registration goes first. Its absence is the ordinary case on a first
	// install rather than a failure.
	_, _ = execCommand("launchctl", "bootout", m.target(def.Name)).CombinedOutput()

	if output, err := execCommand("launchctl", "bootstrap", m.domain, agent).CombinedOutput(); err != nil {
		return fmt.Errorf("loading %s: %w: %s", def.Name, err, strings.TrimSpace(string(output)))
	}

	installed = true
	return nil
}

// Uninstall implements Manager.
func (m launchdManager) Uninstall(name string) error {
	if err := validateName(name); err != nil {
		return err
	}

	agent := m.agentPath(name)
	if _, err := os.Stat(agent); os.IsNotExist(err) {
		return nil
	}

	// A failed bootout does not stop removal, since a job that is already
	// unloaded must still lose its file. It does have to be reported: the
	// registration is gone from disk either way, and if the recorder is
	// still up then nothing on disk points at it any more.
	bootoutOutput, bootoutErr := execCommand("launchctl", "bootout", m.target(name)).CombinedOutput()

	if err := os.Remove(agent); err != nil {
		return fmt.Errorf("removing %s: %w", agent, err)
	}
	if bootoutErr != nil && !isNotLoaded(bootoutOutput) {
		return fmt.Errorf("removed %s but could not stop it, it may still be running: %w: %s",
			agent, bootoutErr, strings.TrimSpace(string(bootoutOutput)))
	}
	return nil
}

// Start implements Manager.
func (m launchdManager) Start(name string) error {
	if err := validateName(name); err != nil {
		return err
	}

	// kickstart reaches a job that is in the domain and bootstrap puts one
	// there. Running both in order starts the recorder from either state,
	// and each refuses the state the other handles.
	_, _ = execCommand("launchctl", "bootstrap", m.domain, m.agentPath(name)).CombinedOutput()

	output, err := execCommand("launchctl", "kickstart", m.target(name)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("starting %s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Stop implements Manager.
//
// The agent file stays on disk, so launchd loads the recorder again at the
// next login. Removing it for good is Uninstall's job.
func (m launchdManager) Stop(name string) error {
	if err := validateName(name); err != nil {
		return err
	}

	output, err := execCommand("launchctl", "bootout", m.target(name)).CombinedOutput()
	if err != nil && !isNotLoaded(output) {
		return fmt.Errorf("stopping %s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Status implements Manager.
func (m launchdManager) Status(name string) (Status, error) {
	if err := validateName(name); err != nil {
		return Status{}, err
	}

	if _, err := os.Stat(m.agentPath(name)); os.IsNotExist(err) {
		return Status{State: StateAbsent, Detail: name}, nil
	}

	detail := m.target(name)
	output, err := execCommand("launchctl", "print", detail).CombinedOutput()
	if err != nil {
		// An agent file with no job in the domain is registered and not
		// running, which is what every machine reports between logins. Any
		// other refusal is a failure to find out, and reporting it as
		// absent would tell an operator the recorder is unregistered when
		// it may be recording.
		if isNotLoaded(output) {
			return Status{State: StateInstalled, Detail: detail}, nil
		}
		return Status{}, fmt.Errorf("querying %s: %w: %s", detail, err, strings.TrimSpace(string(output)))
	}

	return Status{State: printState(output), Detail: detail}, nil
}

// ///////////////////////////////////////////////
// Agent file
// ///////////////////////////////////////////////

// agentPath returns the agent file's location.
func (m launchdManager) agentPath(name string) string {
	return filepath.Join(m.agentDir, name+".plist")
}

// target returns the service target launchctl addresses the job by.
func (m launchdManager) target(name string) string {
	return m.domain + "/" + name
}

// agentFile renders the property list.
//
// RunAtLoad starts the recorder as soon as the agent is loaded, and
// KeepAlive starts it again whenever it exits. An agent in the gui domain
// is loaded when the operator signs in, so together these mean the recorder
// runs for as long as the session does and needs nobody to start it. They
// do not reach back before the session: see the type's own doc.
//
// ProcessType is Standard rather than Background because launchd throttles
// the CPU and disk of a background job, and a live capture writing a stream
// to disk cannot absorb that.
func (m launchdManager) agentFile(def Definition) ([]byte, error) {
	arguments := make([]string, 0, len(def.Args)+1)
	arguments = append(arguments, def.Executable)
	arguments = append(arguments, def.Args...)

	var body bytes.Buffer
	body.WriteString(plistHeader)
	body.WriteString("<plist version=\"1.0\">\n<dict>\n")

	if err := plistString(&body, "Label", def.Name); err != nil {
		return nil, err
	}
	if err := plistStringArray(&body, "ProgramArguments", arguments); err != nil {
		return nil, err
	}
	body.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	body.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	fmt.Fprintf(&body, "\t<key>ThrottleInterval</key>\n\t<integer>%d</integer>\n", restartDelay)
	fmt.Fprintf(&body, "\t<key>ExitTimeOut</key>\n\t<integer>%d</integer>\n", stopTimeout)
	if err := plistString(&body, "ProcessType", "Standard"); err != nil {
		return nil, err
	}

	body.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	if err := plistString(&body, "PATH", m.searchPath); err != nil {
		return nil, err
	}
	body.WriteString("\t</dict>\n")

	body.WriteString("</dict>\n</plist>\n")
	return body.Bytes(), nil
}

// plistString writes one key and its string value.
//
// Every value here is a path, a name or a description the operator supplied,
// so each one is escaped rather than interpolated: an ampersand or an angle
// bracket in a library path would otherwise produce a document launchd
// refuses to parse, and the agent would never load.
func plistString(out *bytes.Buffer, key, value string) error {
	if err := plistElement(out, "\t", "key", key); err != nil {
		return err
	}
	return plistElement(out, "\t", "string", value)
}

// plistStringArray writes one key and its array of string values.
func plistStringArray(out *bytes.Buffer, key string, values []string) error {
	if err := plistElement(out, "\t", "key", key); err != nil {
		return err
	}
	out.WriteString("\t<array>\n")
	for _, value := range values {
		if err := plistElement(out, "\t\t", "string", value); err != nil {
			return err
		}
	}
	out.WriteString("\t</array>\n")
	return nil
}

// plistElement writes one element with its text escaped.
func plistElement(out *bytes.Buffer, indent, name, text string) error {
	out.WriteString(indent + "<" + name + ">")
	if err := xml.EscapeText(out, []byte(text)); err != nil {
		return fmt.Errorf("escaping the %s element: %w", name, err)
	}
	out.WriteString("</" + name + ">\n")
	return nil
}

// ///////////////////////////////////////////////
// launchctl output
// ///////////////////////////////////////////////

// printState reads the job's condition out of a launchctl print block.
//
// The block prints the agent path and the whole argument vector beside the
// state, so a word found anywhere in it is not an answer about the job. Only
// the state field is.
func printState(output []byte) State {
	for line := range strings.SplitSeq(string(output), "\n") {
		label, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(label) != "state" {
			continue
		}
		if strings.TrimSpace(value) == "running" {
			return StateRunning
		}
		return StateInstalled
	}
	return StateInstalled
}

// isNotLoaded reports whether launchctl refused because the job is not in
// the domain.
//
// bootout and print both exit non-zero for that, and it is an answer rather
// than a failure: the agent file is on disk and launchd loads it again at
// the next login.
func isNotLoaded(output []byte) bool {
	text := string(output)
	return strings.Contains(text, "Could not find service") ||
		strings.Contains(text, "No such process")
}
