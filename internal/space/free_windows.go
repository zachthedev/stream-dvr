//go:build windows

package space

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// Free returns the bytes available on the volume holding path.
//
// It reports the space available to the calling user rather than the
// volume's raw free space, so a disk quota is respected rather than
// discovered when a write fails.
func Free(path string) (int64, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("reading free space at %s: %w", path, err)
	}

	var available, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &available, &total, &totalFree); err != nil {
		return 0, fmt.Errorf("reading free space at %s: %w", path, err)
	}
	return int64(available), nil
}
