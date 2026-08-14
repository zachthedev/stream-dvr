package fsretry

import (
	"errors"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// isLocked reports whether an error means another program holds the file.
//
// Windows refuses to rename, delete, or open-for-write a file that another
// process opened without FILE_SHARE_DELETE, reporting a sharing violation.
// A byte-range lock produces the neighbouring lock violation. Neither
// satisfies errors.Is against fs.ErrPermission, so a permission check sees
// nothing and the operation looks like an unrecoverable failure.
func isLocked(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// replaceBlocked reports whether a rename onto path was refused because
// something holds that file open.
//
// Windows will not replace a file any handle is open on, not even a plain
// reader's, and answers with a denied access rather than a sharing
// violation. That is the same answer an ACL the operator has to fix gives.
// Opening the target for writing separates them: a write this process is
// allowed to make means the refusal came from an open handle, and a reader
// closes on its own.
func replaceBlocked(path string, err error) bool {
	if !errors.Is(err, fs.ErrPermission) {
		return false
	}

	file, openErr := os.OpenFile(path, os.O_WRONLY, 0)
	if openErr != nil {
		return isLocked(openErr)
	}
	file.Close() // opened only to ask why the replacement was refused
	return true
}
