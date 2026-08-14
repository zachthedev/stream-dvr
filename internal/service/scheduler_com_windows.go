package service

import (
	"errors"
	"fmt"
	"runtime"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// comScheduler drives the Task Scheduler through its COM API.
//
// The command line cannot say why it failed. schtasks exits 1 for a task
// that is not registered and 1 for every other failure, so the only way to
// tell them apart is matching its English stderr, and on a non-English
// Windows uninstall cannot tell "already gone" from "it failed". COM
// answers with an HRESULT, the same number in every language.
type comScheduler struct{}

// session is one connected Task Scheduler, held on one locked thread.
//
// COM apartments belong to threads, so everything from CoInitializeEx to
// the last Release has to happen on the same one. A session owns that
// thread for as long as it lives.
type session struct {
	service *ole.IDispatch
	root    *ole.IDispatch
	// uninitialize is false when the thread already had an apartment, in
	// which case ending ours would take one this code did not open.
	uninitialize bool
}

// ///////////////////////////////////////////////
// Constants
// ///////////////////////////////////////////////

// HRESULTs the scheduler answers with.
//
// notFoundHRESULT is measured: asking the root folder for a name nothing is
// registered under answers DISP_E_EXCEPTION on the outside, carrying this
// as the EXCEPINFO's SCODE.
//
// accessDeniedHRESULT is HRESULT_FROM_WIN32(ERROR_ACCESS_DENIED), reasoned
// rather than measured, because provoking it means attempting a write
// against a task this project does not own.
const (
	notFoundHRESULT     = 0x80070002
	accessDeniedHRESULT = 0x80070005
)

// dispatchExceptionHRESULT is what a call raised through IDispatch reports
// on the outside, whatever actually went wrong. The code worth reading sits
// in the EXCEPINFO behind it.
const dispatchExceptionHRESULT = 0x80020009

// changedModeHRESULT reports a thread whose apartment was opened before
// this code asked for one. The existing apartment is usable, so the only
// thing it changes is that this code must not close it.
const changedModeHRESULT = 0x80010106

// sFalseHRESULT reports a thread that already held an apartment of the mode
// asked for. It is a success, and it counts a reference, so the apartment is
// this code's to close like any other.
const sFalseHRESULT = 0x00000001

// Task Scheduler enumerations, as its IDL declares them.
const (
	// taskCreateOrUpdate registers a task, replacing one of the same name.
	taskCreateOrUpdate = 6
	// taskValidateOnly parses and checks a document without registering
	// anything, which is how a test can put a real document in front of the
	// real scheduler without changing the machine it runs on.
	taskValidateOnly = 1
	// taskLogonNone leaves the logon method to the registration document,
	// which is what carries S4U.
	taskLogonNone = 0
)

// TASK_STATE values.
const (
	taskStateUnknown  = 0
	taskStateDisabled = 1
	taskStateQueued   = 2
	taskStateReady    = 3
	taskStateRunning  = 4
)

// ///////////////////////////////////////////////
// Sessions
// ///////////////////////////////////////////////

// connect opens a Task Scheduler session on a locked thread.
//
// The caller closes it, and must not hand it to another goroutine: the
// apartment is this thread's and the interfaces are only valid inside it.
func connect() (*session, error) {
	runtime.LockOSThread()

	uninitialize := true
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		// go-ole reports any non-zero HRESULT as an error, and two of them
		// are successes that say how the apartment was reached.
		var oleErr *ole.OleError
		if !errors.As(err, &oleErr) {
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("opening a COM apartment: %w", err)
		}
		switch uint32(oleErr.Code()) {
		case sFalseHRESULT:
			// An apartment of this mode was already open on the thread, and
			// this call counted a reference on it, so it closes like any.
		case changedModeHRESULT:
			// Somebody else's apartment, which serves as well as one of
			// ours, and which counted no reference for this code to drop.
			uninitialize = false
		default:
			runtime.UnlockOSThread()
			return nil, fmt.Errorf("opening a COM apartment: %w", err)
		}
	}

	opened := &session{uninitialize: uninitialize}

	unknown, err := oleutil.CreateObject("Schedule.Service")
	if err != nil {
		opened.Close()
		return nil, fmt.Errorf("creating the Task Scheduler object: %w", err)
	}

	opened.service, err = unknown.QueryInterface(ole.IID_IDispatch)
	// Released here rather than deferred, and before any path that closes
	// the session. Close uninitializes the apartment, and a call through an
	// interface after that reaches a vtable in an unloaded DLL. The session
	// holds its own reference from here on.
	unknown.Release()
	if err != nil {
		opened.Close()
		return nil, fmt.Errorf("reaching the Task Scheduler interface: %w", err)
	}

	if _, err := oleutil.CallMethod(opened.service, "Connect"); err != nil {
		opened.Close()
		return nil, fmt.Errorf("connecting to the Task Scheduler: %w", classifyCOM(err))
	}

	folder, err := oleutil.CallMethod(opened.service, "GetFolder", `\`)
	if err != nil {
		opened.Close()
		return nil, fmt.Errorf("opening the root task folder: %w", classifyCOM(err))
	}
	// Deliberately not cleared. Clearing a VARIANT releases the interface
	// inside it, and this one is the root folder the session goes on to
	// use. The reference transfers to the session, which releases it in
	// Close.
	opened.root = folder.ToIDispatch()
	if opened.root == nil {
		opened.Close()
		return nil, errors.New("the Task Scheduler answered the root folder request with no interface")
	}

	return opened, nil
}

// Close releases the session and the thread it holds.
func (s *session) Close() {
	if s.root != nil {
		s.root.Release()
		s.root = nil
	}
	if s.service != nil {
		s.service.Release()
		s.service = nil
	}
	if s.uninitialize {
		ole.CoUninitialize()
	}
	runtime.UnlockOSThread()
}

// task returns one registered task, or errTaskNotFound.
//
// The caller releases what comes back.
func (s *session) task(name string) (*ole.IDispatch, error) {
	found, err := oleutil.CallMethod(s.root, "GetTask", name)
	if err != nil {
		return nil, classifyCOM(err)
	}
	// Not cleared, for the reason the root folder is not: the reference
	// transfers to the caller, who releases it.
	task := found.ToIDispatch()
	if task == nil {
		// ToIDispatch answers nil for any variant that is not an interface,
		// and every caller releases what comes back, so handing that on
		// would fault on a nil vtable.
		return nil, errTaskNotFound
	}
	return task, nil
}

// ///////////////////////////////////////////////
// scheduler
// ///////////////////////////////////////////////

// Register implements scheduler.
//
// The document is passed as text rather than written to a file first, so
// there is no temporary path for anything to swap between writing it and
// the scheduler reading it.
func (comScheduler) Register(name, document string) error {
	return withSession(func(s *session) error {
		registered, err := oleutil.CallMethod(s.root, "RegisterTask",
			name, document, taskCreateOrUpdate,
			nil, nil, taskLogonNone, nil)
		if err != nil {
			return classifyCOM(err)
		}
		// The registered task comes back with a reference this code owns
		// and has no further use for. Releasing it directly is what
		// balances that; clearing the VARIANT would do it twice.
		if disp := registered.ToIDispatch(); disp != nil {
			disp.Release()
		}
		return nil
	})
}

// Delete implements scheduler.
func (comScheduler) Delete(name string) error {
	return withSession(func(s *session) error {
		if _, err := oleutil.CallMethod(s.root, "DeleteTask", name, 0); err != nil {
			return classifyCOM(err)
		}
		return nil
	})
}

// Run implements scheduler.
func (comScheduler) Run(name string) error {
	return withSession(func(s *session) error {
		task, err := s.task(name)
		if err != nil {
			return err
		}
		defer task.Release()

		started, err := oleutil.CallMethod(task, "Run", nil)
		if err != nil {
			return classifyCOM(err)
		}
		// The running task carries a reference this code owns and does
		// not need, released directly for the reason Register does.
		if disp := started.ToIDispatch(); disp != nil {
			disp.Release()
		}
		return nil
	})
}

// Halt implements scheduler.
func (comScheduler) Halt(name string) error {
	return withSession(func(s *session) error {
		task, err := s.task(name)
		if err != nil {
			return err
		}
		defer task.Release()

		if _, err := oleutil.CallMethod(task, "Stop", 0); err != nil {
			return classifyCOM(err)
		}
		return nil
	})
}

// State implements scheduler.
func (comScheduler) State(name string) (State, error) {
	var state State
	err := withSession(func(s *session) error {
		task, err := s.task(name)
		if err != nil {
			return err
		}
		defer task.Release()

		raw, err := oleutil.GetProperty(task, "State")
		if err != nil {
			return classifyCOM(err)
		}
		// A state is an integer, so the VARIANT holds no interface and
		// there is nothing to release.
		state = stateOf(int(raw.Val))
		return nil
	})
	return state, err
}

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// withSession runs one operation against a connected scheduler.
func withSession(do func(*session) error) error {
	opened, err := connect()
	if err != nil {
		return err
	}
	defer opened.Close()

	return do(opened)
}

// stateOf turns a TASK_STATE into what a manager reports.
//
// A disabled task keeps its own state because it is the one condition an
// operator cannot act on by reading "installed": the registration is there,
// the triggers are there, and nothing will ever start. Queued and ready
// both mean the recorder is waiting for its trigger, which is what
// installed already says.
func stateOf(raw int) State {
	switch raw {
	case taskStateRunning:
		return StateRunning
	case taskStateDisabled:
		return StateDisabled
	default:
		return StateInstalled
	}
}

// classifyCOM turns a COM failure into a sentinel where it can.
//
// The outer code is DISP_E_EXCEPTION for everything raised through
// IDispatch, which says only that the call threw. The number worth reading
// is the EXCEPINFO's SCODE.
func classifyCOM(err error) error {
	if err == nil {
		return nil
	}

	switch hresultOf(err) {
	case notFoundHRESULT:
		return errTaskNotFound
	case accessDeniedHRESULT:
		return errAccessDenied
	default:
		return err
	}
}

// hresultOf digs out the HRESULT a COM failure actually carries, or 0.
func hresultOf(err error) uint32 {
	var oleErr *ole.OleError
	if !errors.As(err, &oleErr) {
		return 0
	}

	// The EXCEPINFO is where a dispatch call puts the real code. Its
	// absence means the outer code is all there is.
	var info ole.EXCEPINFO
	if errors.As(oleErr.SubError(), &info) && info.SCODE() != 0 {
		return info.SCODE()
	}
	return uint32(oleErr.Code())
}
