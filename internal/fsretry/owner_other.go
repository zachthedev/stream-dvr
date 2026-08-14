//go:build !windows

package fsretry

import (
	"fmt"
	"os"
)

// ///////////////////////////////////////////////
// Ownership
// ///////////////////////////////////////////////

// RestrictToOwner reduces a file or directory's mode to perm.
//
// A file this is called on is created with perm already, so this matters
// where one is already at the path: an operator's own chmod, or a config
// restored from an archive that carried a wider mode.
//
// It fails on a filesystem with no mode to set, which is the same class of
// mount where the Windows call has no access list to replace, so both
// platforms refuse in the same place.
func RestrictToOwner(path string, perm os.FileMode) error {
	// G703 traces a caller's path into a filesystem call, which is what this
	// package exists to do. Confining a path built from remote text belongs
	// to the callers that build one, and each has its own test for it.
	if err := os.Chmod(path, perm); err != nil { // G703: the path the caller asked to restrict is the argument
		return fmt.Errorf("restricting %s to its owner: %w", path, err)
	}
	return nil
}
