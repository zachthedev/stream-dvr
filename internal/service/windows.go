//go:build windows

package service

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os/user"
	"strings"
	"syscall"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// taskManager registers a Scheduled Task that runs as the invoking user.
//
// A task rather than a Windows service, because a service runs as a machine
// account that cannot read the operator's streamlink credentials and would
// silently record only what is public.
//
// The task uses the S4U logon type with a boot trigger, so it starts after a
// reboot with nobody signed in and needs no stored password. A logon-only
// mechanism, such as the per-user Run key, records nothing while a rebooted
// machine sits at the sign-in screen, which for a recorder means losing
// every broadcast until someone notices.
type taskManager struct {
	// sched is the scheduler this manager drives. It is a field so a test
	// can exercise the classification directly, and so the command-line
	// implementation can be replaced without touching anything here.
	sched scheduler
}

// ///////////////////////////////////////////////
// Task XML
// ///////////////////////////////////////////////

// taskXML is the Task Scheduler 1.2 document RegisterTask accepts.
//
// The document is what carries the S4U logon type, which runs the task as
// the operator with no password stored anywhere.
type taskXML struct {
	XMLName      xml.Name `xml:"Task"`
	Version      string   `xml:"version,attr"`
	Namespace    string   `xml:"xmlns,attr"`
	Registration struct {
		Description string `xml:"Description"`
		Author      string `xml:"Author"`
	} `xml:"RegistrationInfo"`
	Triggers struct {
		Boot struct {
			Enabled bool `xml:"Enabled"`
		} `xml:"BootTrigger"`
		Logon struct {
			Enabled bool   `xml:"Enabled"`
			UserID  string `xml:"UserId"`
		} `xml:"LogonTrigger"`
	} `xml:"Triggers"`
	Principals struct {
		Principal struct {
			ID        string `xml:"id,attr"`
			UserID    string `xml:"UserId"`
			LogonType string `xml:"LogonType"`
			RunLevel  string `xml:"RunLevel"`
		} `xml:"Principal"`
	} `xml:"Principals"`
	Settings taskSettings `xml:"Settings"`
	Actions  struct {
		Context string `xml:"Context,attr"`
		Exec    struct {
			Command   string `xml:"Command"`
			Arguments string `xml:"Arguments,omitempty"`
		} `xml:"Exec"`
	} `xml:"Actions"`
}

// taskSettings holds the scheduler options a long-running recorder needs.
type taskSettings struct {
	MultipleInstancesPolicy    string `xml:"MultipleInstancesPolicy"`
	DisallowStartIfOnBatteries bool   `xml:"DisallowStartIfOnBatteries"`
	StopIfGoingOnBatteries     bool   `xml:"StopIfGoingOnBatteries"`
	AllowHardTerminate         bool   `xml:"AllowHardTerminate"`
	StartWhenAvailable         bool   `xml:"StartWhenAvailable"`
	RunOnlyIfNetworkAvailable  bool   `xml:"RunOnlyIfNetworkAvailable"`
	AllowStartOnDemand         bool   `xml:"AllowStartOnDemand"`
	Enabled                    bool   `xml:"Enabled"`
	Hidden                     bool   `xml:"Hidden"`
	RunOnlyIfIdle              bool   `xml:"RunOnlyIfIdle"`
	WakeToRun                  bool   `xml:"WakeToRun"`
	// PT0S disables the execution time limit. The default is three days,
	// which would kill a recorder that has been running since the last
	// reboot.
	ExecutionTimeLimit string `xml:"ExecutionTimeLimit"`
	Priority           int    `xml:"Priority"`
	IdleSettings       struct {
		StopOnIdleEnd bool `xml:"StopOnIdleEnd"`
		RestartOnIdle bool `xml:"RestartOnIdle"`
	} `xml:"IdleSettings"`
	RestartOnFailure struct {
		Interval string `xml:"Interval"`
		Count    int    `xml:"Count"`
	} `xml:"RestartOnFailure"`
}

// newManager returns the Windows manager.
func newManager() (Manager, error) { return taskManager{sched: comScheduler{}}, nil }

// Mechanism implements Manager.
func (taskManager) Mechanism() string { return "Scheduled Task (runs as you, at startup)" }

// buildTaskXML renders the task body that RegisterTask is handed.
//
// It carries no XML declaration. The scheduler takes the document as a wide
// string and so already knows the encoding, and a declaration naming any
// encoding at all contradicts the string it arrives in, which the parser
// rejects with "unable to switch the encoding".
func buildTaskXML(def Definition, user string) (string, error) {
	var task taskXML
	task.Version = "1.2"
	task.Namespace = "http://schemas.microsoft.com/windows/2004/02/mit/task"
	task.Registration.Description = def.Description
	task.Registration.Author = user

	task.Triggers.Boot.Enabled = true
	task.Triggers.Logon.Enabled = true
	task.Triggers.Logon.UserID = user

	task.Principals.Principal.ID = "Author"
	task.Principals.Principal.UserID = user
	// S4U runs as the user without a stored password, which is what lets
	// the task start before anyone signs in.
	task.Principals.Principal.LogonType = "S4U"
	task.Principals.Principal.RunLevel = "LeastPrivilege"

	task.Settings = taskSettings{
		MultipleInstancesPolicy:    "IgnoreNew",
		DisallowStartIfOnBatteries: false,
		StopIfGoingOnBatteries:     false,
		AllowHardTerminate:         true,
		StartWhenAvailable:         true,
		RunOnlyIfNetworkAvailable:  false,
		AllowStartOnDemand:         true,
		Enabled:                    true,
		Hidden:                     false,
		RunOnlyIfIdle:              false,
		WakeToRun:                  false,
		ExecutionTimeLimit:         "PT0S",
		Priority:                   7,
	}
	task.Settings.RestartOnFailure.Interval = "PT1M"
	task.Settings.RestartOnFailure.Count = 3

	task.Actions.Context = "Author"
	task.Actions.Exec.Command = def.Executable
	task.Actions.Exec.Arguments = commandLine(def.Args)

	body, err := xml.MarshalIndent(task, "", "  ")
	if err != nil {
		return "", fmt.Errorf("rendering task XML: %w", err)
	}
	return string(body), nil
}

// ///////////////////////////////////////////////
// Manager
// ///////////////////////////////////////////////

// Install implements Manager.
func (m taskManager) Install(def Definition) error {
	if err := validate(def); err != nil {
		return err
	}

	account, err := currentUser()
	if err != nil {
		return err
	}
	document, err := buildTaskXML(def, account)
	if err != nil {
		return err
	}

	if err := m.sched.Register(def.Name, document); err != nil {
		return elevationOr(def.Name, "registering", err)
	}
	return nil
}

// elevationOr names a refusal an operator can act on, or wraps what came
// back with the operation that produced it.
//
// The action leads either way, so a refusal names the operation the operator
// asked for. Registering, removing, querying and controlling a task can each
// be refused, and the action word is what tells them apart.
func elevationOr(name, action string, cause error) error {
	if errors.Is(cause, errAccessDenied) {
		cause = ErrElevationRequired
	}
	return fmt.Errorf("%s scheduled task %s: %w", action, name, cause)
}

// Uninstall implements Manager.
func (m taskManager) Uninstall(name string) error {
	if err := validateName(name); err != nil {
		return err
	}

	status, err := m.Status(name)
	if err != nil {
		return err
	}
	if status.State == StateAbsent {
		return nil
	}

	if err := m.sched.Delete(name); err != nil {
		// A task that vanished between the query and here is already
		// removed, which is what uninstall was asked for.
		if errors.Is(err, errTaskNotFound) {
			return nil
		}
		return elevationOr(name, "removing", err)
	}
	return nil
}

// Start implements Manager.
func (m taskManager) Start(name string) error {
	return m.control(name, "/Run", "starting")
}

// Stop implements Manager.
//
// The registration survives, so the recorder starts again at the next boot.
// Removing it for good is Uninstall's job.
func (m taskManager) Stop(name string) error {
	return m.control(name, "/End", "stopping")
}

// control starts or ends a registered task.
func (m taskManager) control(name, verb, action string) error {
	if err := validateName(name); err != nil {
		return err
	}

	run := m.sched.Run
	if verb == "/End" {
		run = m.sched.Halt
	}
	if err := run(name); err != nil {
		return elevationOr(name, action, err)
	}
	return nil
}

// Status implements Manager.
func (m taskManager) Status(name string) (Status, error) {
	if err := validateName(name); err != nil {
		return Status{}, err
	}

	state, err := m.sched.State(name)
	if err != nil {
		// A task that is not registered is an answer rather than a
		// failure. Every other failure is a failure to find out, and
		// reporting one as absent would tell an operator the recorder is
		// unregistered when it may be running.
		if errors.Is(err, errTaskNotFound) {
			return Status{State: StateAbsent, Detail: name}, nil
		}
		return Status{}, elevationOr(name, "querying", err)
	}
	return Status{State: state, Detail: name}, nil
}

// commandLine renders arguments the way Windows parses them back.
//
// The scheduler stores one Arguments string and CommandLineToArgvW splits it
// again at launch, so joining raw loses every argument containing a space.
// A library under "C:\Program Files" would reach the recorder as two
// operands, and a value could introduce a flag of its own.
func commandLine(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, syscall.EscapeArg(arg))
	}
	return strings.Join(quoted, " ")
}

// currentUser returns the account the task runs as.
//
// It comes from the process token rather than USERNAME and USERDOMAIN.
// Those live in HKCU\Environment, which the user can write at medium
// integrity and which propagates into a shell elevated from that account,
// so an unprivileged process could otherwise choose the principal an
// elevated install writes into the task.
func currentUser() (string, error) {
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolving the current account: %w", err)
	}
	if account.Username == "" {
		return "", fmt.Errorf("the current account has no name")
	}
	return account.Username, nil
}
