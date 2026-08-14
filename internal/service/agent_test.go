package service

import (
	"errors"
	"runtime"
	"testing"
)

// ///////////////////////////////////////////////
// NewAgentAutostart
// ///////////////////////////////////////////////

func TestNewAgentAutostart_AnswersForThisPlatform(t *testing.T) {
	// Only Windows starts the recorder somewhere it cannot raise a
	// notification from, so only Windows has a helper to register. The
	// caller distinguishes "nothing to do here" from a real failure, and
	// this is what it distinguishes them by.
	autostart, err := NewAgentAutostart()

	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatalf("NewAgentAutostart() err = %v, want nil on Windows", err)
		}
		if autostart == nil {
			t.Fatal("NewAgentAutostart() returned no registration and no error")
		}
		if autostart.Mechanism() == "" {
			t.Error("Mechanism() is empty, want somewhere an operator can look")
		}
		return
	}

	if !errors.Is(err, ErrNoAgent) {
		t.Errorf("NewAgentAutostart() err = %v, want ErrNoAgent on %s", err, runtime.GOOS)
	}
	if autostart != nil {
		t.Error("NewAgentAutostart() returned a registration on a platform that needs none")
	}
}

func TestErrNoAgent_IsNotAnAutostartFailure(t *testing.T) {
	// install reports ErrUnsupported to the operator and passes over
	// ErrNoAgent in silence, so the two must never match each other.
	if errors.Is(ErrNoAgent, ErrUnsupported) {
		t.Error("ErrNoAgent matches ErrUnsupported, want them distinguishable")
	}
	if errors.Is(ErrUnsupported, ErrNoAgent) {
		t.Error("ErrUnsupported matches ErrNoAgent, want them distinguishable")
	}
}
