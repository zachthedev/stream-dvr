//go:build !windows && !linux && !darwin

package service

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestNewManager(t *testing.T) {
	// An install that silently registers nothing is worse than one that
	// refuses: the operator walks away believing the recorder is up.
	manager, err := newManager()

	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("newManager() err = %v, want it to wrap ErrUnsupported", err)
	}
	if manager != nil {
		t.Errorf("newManager() = %v, want no manager on %s", manager, runtime.GOOS)
	}
	if err != nil && !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("newManager() err = %q, want it to name the platform", err)
	}
}
