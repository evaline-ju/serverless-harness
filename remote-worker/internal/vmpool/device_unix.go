//go:build unix

package vmpool

import (
	"fmt"
	"os"
	"syscall"
)

// deviceNumber returns the identifier of the filesystem device backing path, via
// stat(2)'s st_dev — the exact quantity hardlink(2) refuses to cross (EXDEV:
// "Invalid cross-device link"). Two paths hardlink-compatible with each other is
// exactly "deviceNumber returns the same value for both"; see
// checkPathsShareDevice, this function's only caller, for why that matters here.
func deviceNumber(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Every unix Go build defines os.FileInfo.Sys() as *syscall.Stat_t; this
		// branch exists so a future exotic unix target fails with a clear cause
		// here rather than a panic at the call site's type assertion.
		return 0, fmt.Errorf("vmpool: %s: os.FileInfo.Sys() is %T, not *syscall.Stat_t", path, info.Sys())
	}
	return uint64(st.Dev), nil
}
