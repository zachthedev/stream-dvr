package service

import (
	"errors"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// scheduler is the Task Scheduler as this package needs it.
//
// It exists to put the classification of a failure on one side of a line
// and the meaning of it on the other. Only an implementation can know how
// it decided what a failure meant, and everything above this interface
// works in sentinels instead.
//
// Register takes the task document itself rather than a path to one,
// because that is the shape the API wants and the shape that needs no
// temporary file for anything to swap between writing and reading.
//
// The document is text, not bytes. RegisterTask takes a wide string, so
// the scheduler decodes it before the XML parser ever sees it, and encoded
// bytes handed over as a string arrive as one character per byte.
type scheduler interface {
	// Register creates or replaces a task from its XML document.
	Register(name, document string) error
	// Delete removes a task, reporting errTaskNotFound for one that is
	// not registered.
	Delete(name string) error
	// Run starts a registered task now.
	Run(name string) error
	// Halt ends a running task, leaving its registration.
	Halt(name string) error
	// State reports a task's condition, reporting errTaskNotFound for one
	// that is not registered.
	State(name string) (State, error)
}

// ///////////////////////////////////////////////
// Errors
// ///////////////////////////////////////////////

var (
	// errTaskNotFound reports a name nothing is registered under. It is an
	// answer rather than a failure: Status turns it into StateAbsent and
	// Uninstall treats it as already done.
	errTaskNotFound = errors.New("no task is registered under that name")

	// errAccessDenied reports an operation needing an elevated shell.
	errAccessDenied = errors.New("the scheduler refused access")
)
