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

// statOwnerMode returns path's owning uid/gid and permission mode, via stat(2)'s
// st_uid/st_gid -- the fields checkPathTraversableBy (traversalcheck.go) needs to
// decide which permission class (owner/group/other) governs traversal into path
// by a given uid:gid. Companion to deviceNumber above, from the same
// *syscall.Stat_t, different fields.
func statOwnerMode(path string) (uid, gid uint32, mode os.FileMode, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// See deviceNumber's identical branch above for why this is a documented
		// "should never happen on a real unix target" rather than a bare panic.
		return 0, 0, 0, fmt.Errorf("vmpool: %s: os.FileInfo.Sys() is %T, not *syscall.Stat_t", path, info.Sys())
	}
	return uint32(st.Uid), uint32(st.Gid), info.Mode(), nil
}
