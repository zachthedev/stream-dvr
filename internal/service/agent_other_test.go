//go:build !windows

package service

import (
	"errors"
	"testing"
)

func TestNewAgentAutostart_ReportsNoHelperIsNeeded(t *testing.T) {
	// The recorder here runs inside the operator's session already, so a
	// helper would duplicate what it does rather than enable anything.
	autostart, err := newAgentAutostart()

	if !errors.Is(err, ErrNoAgent) {
		t.Errorf("newAgentAutostart() err = %v, want ErrNoAgent", err)
	}
	if autostart != nil {
		t.Error("newAgentAutostart() returned a registration, want none")
	}
}
