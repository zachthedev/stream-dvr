//go:build windows && schedulertest

package service

import (
	"errors"
	"testing"
)

// This file registers a real task, so it is behind its own build tag and
// never runs as part of the suite:
//
//	go test -tags schedulertest ./internal/service/
//
// It covers the one thing the ordinary suite cannot. A document that
// validates is not proof that registering it works, because validation stops
// before the scheduler writes anything. The document itself is covered
// without elevation by the validate test in the untagged file.

// liveTaskName is the registration these tests create and remove. It is not
// the recorder's name, so a failure here cannot touch a real install.
const liveTaskName = "stream-dvr-schedulertest"

// liveTaskExecutable is what the registered task would run.
//
// The document carries boot and logon triggers, so a registration that
// outlives the test is an autostart entry. Under the system directory it is
// one nothing can fill: writing there needs elevation, and this test does
// not run elevated. A path under the test's own temporary directory would
// be user-writable, which turns an orphan into somewhere to plant a binary.
const liveTaskExecutable = `C:\Windows\System32\stream-dvr-schedulertest-no-such-file.exe`

func TestComScheduler_RegistersAndRemovesARealTask(t *testing.T) {
	// The round trip the ordinary tests cannot reach: the document this
	// package builds, handed to the real RegisterTask, read back, and
	// removed.
	sched := comScheduler{}

	def := Definition{
		Name:        liveTaskName,
		Description: "stream-dvr scheduler round trip, removed by the test that made it",
		Executable:  liveTaskExecutable,
		Args:        []string{"serve"},
	}

	account, err := currentUser()
	if err != nil {
		t.Fatalf("currentUser() err = %v, want nil", err)
	}
	document, err := buildTaskXML(def, account)
	if err != nil {
		t.Fatalf("buildTaskXML() err = %v, want nil", err)
	}

	// t.Cleanup does not run when the test binary dies by panic, which is
	// how a -timeout expiry and a Ctrl-C both end. Sweeping first is what
	// reaps a registration an earlier run could not remove.
	if err := sched.Delete(liveTaskName); err != nil && !errors.Is(err, errTaskNotFound) {
		t.Fatalf("could not clear a registration left by an earlier run: %v", err)
	}

	// Registered before the assertions, so a failure part way through still
	// removes what it made.
	t.Cleanup(func() {
		if err := sched.Delete(liveTaskName); err != nil && !errors.Is(err, errTaskNotFound) {
			t.Errorf("cleanup could not remove %s: %v", liveTaskName, err)
		}
	})

	if err := sched.Register(liveTaskName, document); err != nil {
		if errors.Is(err, errAccessDenied) {
			t.Skipf("registering into the root folder needs an elevated shell: %v", err)
		}
		t.Fatalf("Register() err = %v, want the scheduler to accept the document", err)
	}

	state, err := sched.State(liveTaskName)
	if err != nil {
		t.Fatalf("State() err = %v, want the task this test just registered", err)
	}
	if state != StateInstalled && state != StateRunning {
		t.Errorf("State() = %q, want the registration reported as present", state)
	}

	if err := sched.Delete(liveTaskName); err != nil {
		t.Fatalf("Delete() err = %v, want nil", err)
	}
	if _, err := sched.State(liveTaskName); !errors.Is(err, errTaskNotFound) {
		t.Errorf("State() err = %v after Delete, want errTaskNotFound", err)
	}
}
