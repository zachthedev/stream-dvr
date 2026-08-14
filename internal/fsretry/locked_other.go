//go:build !windows

package fsretry

import (
	"errors"
	"syscall"
)

// isLocked reports whether an error means another program holds the file.
//
// A Unix filesystem lets a file be renamed or unlinked while it is open, so
// the sharing violation has no local equivalent and an open backup agent
// costs nothing. What remains are three conditions that pass on their own:
//
//   - EBUSY arrives from network filesystems, where the server enforces a
//     share mode the local kernel does not.
//   - ESTALE is an NFS handle the server invalidated, which a retry
//     re-resolves from the path.
//   - ETXTBSY is a file being executed, which ends when that process does.
//     A library on a shared volume can have a player running from it.
func isLocked(err error) bool {
	return errors.Is(err, syscall.EBUSY) ||
		errors.Is(err, syscall.ESTALE) ||
		errors.Is(err, syscall.ETXTBSY)
}

// replaceBlocked reports whether a rename onto path was refused because
// something holds that file open.
//
// Nothing on Unix is: rename(2) replaces its destination whoever has it
// open, so a refusal is the filesystem's own answer and waiting cannot
// change it.
func replaceBlocked(string, error) bool { return false }
