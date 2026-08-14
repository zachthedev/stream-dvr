//go:build !windows && !linux && !darwin

package service

import (
	"fmt"
	"runtime"
)

// newManager refuses on a platform with no autostart implementation.
//
// The refusal is the construction rather than a manager whose every method
// answers the same way, so a caller finds out at the one call it can act on.
// An install that silently registers nothing is worse than one that refuses:
// the operator walks away believing the recorder is up.
func newManager() (Manager, error) {
	return nil, fmt.Errorf("%s: %w", runtime.GOOS, ErrUnsupported)
}
