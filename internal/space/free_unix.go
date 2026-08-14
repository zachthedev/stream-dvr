//go:build !windows

package space

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Free returns the bytes available on the filesystem holding path.
//
// It uses the blocks available to an unprivileged user rather than the
// total free blocks, because the reserved portion is not space this tool
// may spend.
func Free(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("reading free space at %s: %w", path, err)
	}
	// Statfs_t.Bsize is int64 on linux and uint32 on darwin, so the
	// conversion is redundant on one platform and load-bearing on the
	// other. Dropping it to satisfy a linter reading only linux breaks the
	// darwin build outright.
	//nolint:unconvert // Bsize is uint32 on darwin, where this conversion is required
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
