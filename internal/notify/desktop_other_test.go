//go:build !windows && !linux && !darwin

package notify

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

func TestDesktopAvailable_NamesThePlatformItCannotServe(t *testing.T) {
	// A platform nobody planned for is told, the way the service package
	// refuses one, rather than left waiting for a notification that was
	// never coming.
	err := desktopAvailable()

	if err == nil {
		t.Fatal("desktopAvailable() err = nil on an unsupported platform")
	}
	if !strings.Contains(err.Error(), runtime.GOOS) {
		t.Errorf("desktopAvailable() err = %v, want it to name %s", err, runtime.GOOS)
	}
}

func TestRaise_RefusesRatherThanReportingSuccess(t *testing.T) {
	if err := raise(context.Background(), "a title", "a body"); err == nil {
		t.Error("raise() err = nil, want the same refusal desktopAvailable gives")
	}
}
