package service

import (
	"errors"
	"strings"
	"testing"
)

// fakeScheduler answers with canned sentinels, so the meaning a manager
// gives an outcome is tested without inventing English scheduler output.
type fakeScheduler struct {
	registerErr error
	deleteErr   error
	runErr      error
	haltErr     error
	state       State
	stateErr    error
	registered  []string
	// documents holds what was registered, not just under which name. The
	// document is the whole registration, so a test that checks only the
	// name cannot tell a working install from one that hands the scheduler
	// something it will refuse.
	documents []string
}

// schedulerTestName is the registration these tests act on. It is never a
// name anything real uses, because nothing here reaches a real scheduler.
const schedulerTestName = "stream-dvr-scheduler-test"

// validDefinition returns a registration that passes validation, so a test
// reaches the scheduler rather than stopping at the check before it.
func validDefinition() Definition {
	return Definition{
		Name:        schedulerTestName,
		Description: "a test registration",
		Executable:  `C:\tools\stream-dvr.exe`,
		Args:        []string{"serve"},
	}
}

// ///////////////////////////////////////////////
// Test doubles
// ///////////////////////////////////////////////

// Register implements scheduler.
func (f *fakeScheduler) Register(name, document string) error {
	f.registered = append(f.registered, name)
	f.documents = append(f.documents, document)
	return f.registerErr
}

// Delete implements scheduler.
func (f *fakeScheduler) Delete(string) error { return f.deleteErr }

// Run implements scheduler.
func (f *fakeScheduler) Run(string) error { return f.runErr }

// Halt implements scheduler.
func (f *fakeScheduler) Halt(string) error { return f.haltErr }

// State implements scheduler.
func (f *fakeScheduler) State(string) (State, error) { return f.state, f.stateErr }

// ///////////////////////////////////////////////
// What a manager makes of an outcome
// ///////////////////////////////////////////////

func TestTaskManager_ReportsAnAbsentTaskRatherThanFailing(t *testing.T) {
	// A name nothing is registered under is an answer. Reporting it as a
	// failure would leave uninstall unable to say the job is already done.
	manager := taskManager{sched: &fakeScheduler{stateErr: errTaskNotFound}}

	status, err := manager.Status(schedulerTestName)
	if err != nil {
		t.Fatalf("Status() err = %v, want nil", err)
	}
	if status.State != StateAbsent {
		t.Errorf("State = %q, want %q", status.State, StateAbsent)
	}
}

func TestTaskManager_NeverReportsAFailureToLookAsAbsent(t *testing.T) {
	// The defect this seam exists to prevent. Telling an operator the
	// recorder is unregistered when the query merely failed sends them to
	// install it again over one that may be running.
	manager := taskManager{sched: &fakeScheduler{stateErr: errors.New("the scheduler service is not running")}}

	if _, err := manager.Status(schedulerTestName); err == nil {
		t.Error("Status() err = nil for a query that failed, want the failure reported")
	}
}

func TestTaskManager_NamesElevationForEveryOperation(t *testing.T) {
	// An operator told only "access is denied" does not know that running
	// one command from an elevated shell fixes it.
	//
	// The verb is half the message. Every operation here can be refused, and
	// a refusal that named a different one sends the operator after a task
	// they never touched: told about registering, they go looking for a
	// registration when what they typed was uninstall.
	tests := []struct {
		name  string
		verb  string
		call  func(taskManager) error
		sched *fakeScheduler
	}{
		{
			name:  "install",
			verb:  "registering",
			sched: &fakeScheduler{registerErr: errAccessDenied},
			call:  func(m taskManager) error { return m.Install(validDefinition()) },
		},
		{
			name:  "uninstall",
			verb:  "removing",
			sched: &fakeScheduler{state: StateInstalled, deleteErr: errAccessDenied},
			call:  func(m taskManager) error { return m.Uninstall(schedulerTestName) },
		},
		{
			name:  "start",
			verb:  "starting",
			sched: &fakeScheduler{runErr: errAccessDenied},
			call:  func(m taskManager) error { return m.Start(schedulerTestName) },
		},
		{
			name:  "stop",
			verb:  "stopping",
			sched: &fakeScheduler{haltErr: errAccessDenied},
			call:  func(m taskManager) error { return m.Stop(schedulerTestName) },
		},
		{
			name:  "status",
			verb:  "querying",
			sched: &fakeScheduler{stateErr: errAccessDenied},
			call:  func(m taskManager) error { _, err := m.Status(schedulerTestName); return err },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(taskManager{sched: tt.sched})
			if !errors.Is(err, ErrElevationRequired) {
				t.Fatalf("%s err = %v, want ErrElevationRequired", tt.name, err)
			}
			if !strings.Contains(err.Error(), tt.verb) {
				t.Errorf("%s err = %q, want it to name %q", tt.name, err, tt.verb)
			}
			// Naming the right verb is worth nothing if it names the others
			// too, so the message has to pick one.
			for _, other := range tests {
				if other.verb == tt.verb {
					continue
				}
				if strings.Contains(err.Error(), other.verb) {
					t.Errorf("%s err = %q, want no mention of %q", tt.name, err, other.verb)
				}
			}
		})
	}
}

func TestTaskManager_StartAndStopReachDifferentVerbs(t *testing.T) {
	// One helper serves both, picked apart by a verb string. A seam that
	// routed both to the same operation would make stop start the task.
	sched := &fakeScheduler{runErr: errors.New("run was called"), haltErr: errors.New("halt was called")}
	manager := taskManager{sched: sched}

	if err := manager.Start(schedulerTestName); err == nil || !strings.Contains(err.Error(), "run was called") {
		t.Errorf("Start() err = %v, want it to reach Run", err)
	}
	if err := manager.Stop(schedulerTestName); err == nil || !strings.Contains(err.Error(), "halt was called") {
		t.Errorf("Stop() err = %v, want it to reach Halt", err)
	}
}

// ///////////////////////////////////////////////
// What a manager does before it reaches a scheduler
// ///////////////////////////////////////////////

func TestTaskManager_RejectsAnIncompleteDefinition(t *testing.T) {
	// Validation runs first, so a definition missing its executable never
	// reaches the scheduler and cannot half-register anything.
	sched := &fakeScheduler{}

	if err := (taskManager{sched: sched}).Install(Definition{}); err == nil {
		t.Error("Install() err = nil for an empty definition, want a rejection")
	}
	if len(sched.registered) != 0 {
		t.Errorf("Install reached the scheduler with %v, want it stopped at validation", sched.registered)
	}
}

func TestTaskManager_RejectsAnEmptyName(t *testing.T) {
	// A blank name would reach the scheduler as a wildcard or an error in
	// its own words, neither of which is this package's answer.
	tests := []struct {
		name   string
		invoke func(taskManager) error
	}{
		{name: "uninstall", invoke: func(m taskManager) error { return m.Uninstall("") }},
		{name: "start", invoke: func(m taskManager) error { return m.Start("") }},
		{name: "stop", invoke: func(m taskManager) error { return m.Stop("") }},
		{name: "status", invoke: func(m taskManager) error { _, err := m.Status(""); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.invoke(taskManager{sched: &fakeScheduler{}}); err == nil {
				t.Errorf("%s() err = nil for an empty name, want a rejection", tt.name)
			}
		})
	}
}

func TestTaskManager_RegistersTheDocumentItRendered(t *testing.T) {
	// The document is what carries S4U, and S4U is what lets the recorder
	// start with nobody signed in. A manager that registered a name without
	// it would produce a task that records nothing until someone logs on.
	sched := &fakeScheduler{}

	if err := (taskManager{sched: sched}).Install(validDefinition()); err != nil {
		t.Fatalf("Install() err = %v, want nil", err)
	}
	if len(sched.registered) != 1 || sched.registered[0] != schedulerTestName {
		t.Errorf("registered %v, want [%s]", sched.registered, schedulerTestName)
	}
	if len(sched.documents) != 1 {
		t.Fatalf("registered %d documents, want 1", len(sched.documents))
	}
	if got := unmarshalTask(t, sched.documents[0]).Principals.Principal.LogonType; got != "S4U" {
		t.Errorf("LogonType = %q, want S4U", got)
	}
}

func TestTaskManager_TreatsRemovingAnAbsentTaskAsDone(t *testing.T) {
	// Uninstall is run twice by anyone who is unsure, and by every script
	// that cleans up before installing. The second run is not a failure.
	manager := taskManager{sched: &fakeScheduler{stateErr: errTaskNotFound}}

	if err := manager.Uninstall(schedulerTestName); err != nil {
		t.Errorf("Uninstall() err = %v, want nil for a task that is not there", err)
	}
}

func TestTaskManager_ReportsEveryStateTheSchedulerCanBeIn(t *testing.T) {
	// The three the operator acts on. Collapsing installed into running
	// would make a stopped recorder look healthy.
	tests := []struct {
		name  string
		state State
		want  State
	}{
		{name: "registered and idle", state: StateInstalled, want: StateInstalled},
		{name: "registered and running", state: StateRunning, want: StateRunning},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := taskManager{sched: &fakeScheduler{state: tt.state}}

			got, err := manager.Status(schedulerTestName)
			if err != nil {
				t.Fatalf("Status() err = %v, want nil", err)
			}
			if got.State != tt.want {
				t.Errorf("State = %q, want %q", got.State, tt.want)
			}
		})
	}
}
