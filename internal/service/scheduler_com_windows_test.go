package service

import (
	"errors"
	"strings"
	"testing"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// unregisteredName is a name nothing is registered under. Asking about it
// is a read, so the tests below leave the machine's scheduler untouched.
const unregisteredName = "stream-dvr-test-nothing-registers-this"

// ///////////////////////////////////////////////
// classifyCOM
// ///////////////////////////////////////////////

func TestClassifyCOM_TurnsAnHRESULTIntoAnAnswer(t *testing.T) {
	// The whole reason this package goes through COM. schtasks exits 1 for
	// a task that is not registered and 1 for everything else, so telling
	// them apart means matching English stderr. An HRESULT is the same
	// number on every Windows in every language.
	tests := []struct {
		name    string
		hresult uintptr
		want    error
	}{
		{name: "no such task", hresult: notFoundHRESULT, want: errTaskNotFound},
		{name: "refused", hresult: accessDeniedHRESULT, want: errAccessDenied},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCOM(ole.NewError(tt.hresult)); !errors.Is(got, tt.want) {
				t.Errorf("classifyCOM(0x%08X) = %v, want %v", tt.hresult, got, tt.want)
			}
		})
	}
}

func TestClassifyCOM_LeavesAFailureItCannotName(t *testing.T) {
	// A code this build has no meaning for reaches the operator as itself.
	// Guessing would be how a registered task gets reported absent.
	cause := ole.NewError(0x80004005)

	if got := classifyCOM(cause); !errors.Is(got, cause) {
		t.Errorf("classifyCOM() = %v, want the cause unchanged", got)
	}
}

func TestClassifyCOM_PassesNilThrough(t *testing.T) {
	if got := classifyCOM(nil); got != nil {
		t.Errorf("classifyCOM(nil) = %v, want nil", got)
	}
}

func TestClassifyCOM_ReadsTheHRESULTInsideADispatchException(t *testing.T) {
	// The measured shape. Anything raised through IDispatch arrives as
	// DISP_E_EXCEPTION, which says only that the call threw, and the code
	// worth reading is the EXCEPINFO's. Classifying on the outer one would
	// make every failure look identical.
	outer := ole.NewError(dispatchExceptionHRESULT)

	if got := classifyCOM(outer); errors.Is(got, errTaskNotFound) {
		t.Error("classifyCOM read DISP_E_EXCEPTION as a missing task")
	}
}

// ///////////////////////////////////////////////
// stateOf
// ///////////////////////////////////////////////

func TestStateOf_SeparatesRunningFromMerelyRegistered(t *testing.T) {
	// An operator asking after the recorder needs to know it is running.
	// Collapsing the two would make a stopped recorder look healthy, and
	// reporting a disabled task as absent would send them to install a
	// second one over it.
	//
	// Disabled is its own answer for the opposite reason: the registration
	// is complete and its triggers are set, and it will still never start.
	// Reported as installed it is indistinguishable from a working recorder
	// that is between broadcasts.
	tests := []struct {
		name string
		raw  int
		want State
	}{
		{name: "running", raw: taskStateRunning, want: StateRunning},
		{name: "ready", raw: taskStateReady, want: StateInstalled},
		{name: "queued", raw: taskStateQueued, want: StateInstalled},
		{name: "disabled", raw: taskStateDisabled, want: StateDisabled},
		{name: "unknown", raw: taskStateUnknown, want: StateInstalled},
		{name: "a value this build has no word for", raw: 99, want: StateInstalled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stateOf(tt.raw); got != tt.want {
				t.Errorf("stateOf(%d) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// ///////////////////////////////////////////////
// Against the machine's own scheduler
// ///////////////////////////////////////////////

// Nothing below registers, deletes, starts, or stops a task, because every
// one of those changes the machine running the tests. Reads, and one
// validate, which parses a document and stops.

func TestComScheduler_ReportsAnUnregisteredNameAsAbsent(t *testing.T) {
	// This is what pins the HRESULT constant to reality. notFoundHRESULT
	// was measured rather than remembered, and a Windows that answers
	// differently makes uninstall report a failure where the work is
	// already done.
	_, err := (comScheduler{}).State(unregisteredName)

	if !errors.Is(err, errTaskNotFound) {
		t.Errorf("State(%q) err = %v, want errTaskNotFound", unregisteredName, err)
	}
}

func TestComScheduler_ConnectsToTheSchedulerService(t *testing.T) {
	// Everything else rests on the apartment opening and the service
	// answering. A failure here is worth telling apart from a task that is
	// simply not registered.
	opened, err := connect()
	if err != nil {
		t.Fatalf("connect() err = %v, want nil", err)
	}
	defer opened.Close()

	if opened.root == nil {
		t.Error("connect() returned no root folder")
	}
}

// validateDocument puts a document in front of the real scheduler without
// registering it. TASK_VALIDATE_ONLY parses and checks, then stops, so this
// changes nothing and needs no elevated shell.
//
// It names the task, so the caller can prove nothing was registered rather
// than trusting the flag. One token separates taskValidateOnly from
// taskCreateOrUpdate, and a slip between them would turn every call here
// into a real registration with nothing to say so.
func validateDocument(t *testing.T, document string) error {
	t.Helper()

	err := withSession(func(s *session) error {
		_, callErr := oleutil.CallMethod(s.root, "RegisterTask",
			unregisteredName, document, taskValidateOnly, nil, nil, taskLogonNone, nil)
		return callErr
	})

	if _, stateErr := (comScheduler{}).State(unregisteredName); !errors.Is(stateErr, errTaskNotFound) {
		t.Fatalf("validating registered %s, so this test is not the read it claims to be", unregisteredName)
	}
	return err
}

// refusedTheDocument reports whether an error is the scheduler rejecting
// what it was given, rather than this test failing to reach it at all. A
// machine whose scheduler service is unreachable answers every negative
// case identically to a real refusal.
func refusedTheDocument(err error) bool {
	return err != nil &&
		!errors.Is(err, errAccessDenied) &&
		!errors.Is(err, errTaskNotFound) &&
		!strings.Contains(err.Error(), "connecting to the Task Scheduler")
}

func TestComScheduler_BuildsADocumentTheSchedulerAccepts(t *testing.T) {
	// Every other test on the document stops at Go's XML decoder, which
	// answers a different question: whether it parses, not whether the
	// scheduler will take it. A document Go reads happily and the scheduler
	// refuses passes the whole suite and fails on the one machine that
	// matters, which is exactly what a document encoded to bytes and handed
	// over as a string did.
	account, err := currentUser()
	if err != nil {
		t.Fatalf("currentUser() err = %v, want nil", err)
	}
	document, err := buildTaskXML(definition(), account)
	if err != nil {
		t.Fatalf("buildTaskXML() err = %v, want nil", err)
	}

	if err := validateDocument(t, document); err != nil {
		t.Errorf("the scheduler refused the document this package builds: %v", err)
	}
}

func TestComScheduler_RefusesADocumentThatIsNotATask(t *testing.T) {
	// The other half of the assertion above. A validate that accepted
	// anything would make the test before it prove nothing.
	tests := []struct {
		name     string
		document string
	}{
		{name: "not XML at all", document: "not a task document"},
		{name: "XML that is not a task", document: "<Task/>"},
		{
			// The shape that passed every offline test: UTF-16 bytes handed
			// over as a string arrive one character per byte.
			name:     "the document as encoded bytes",
			document: "\xFF\xFE<\x00T\x00a\x00s\x00k\x00/\x00>\x00",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateDocument(t, tt.document); !refusedTheDocument(err) {
				t.Errorf("validate err = %v, want the scheduler refusing the document", err)
			}
		})
	}
}

func TestComScheduler_SurvivesRepeatedSessions(t *testing.T) {
	// Each operation opens its own session on its own locked thread, so an
	// install followed by a status is two apartments in a row. Getting the
	// reference counting wrong here crashes the process rather than
	// returning an error.
	for range 3 {
		if _, err := (comScheduler{}).State(unregisteredName); !errors.Is(err, errTaskNotFound) {
			t.Fatalf("State() err = %v, want errTaskNotFound", err)
		}
	}
}
