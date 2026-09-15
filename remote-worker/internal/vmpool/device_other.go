//go:build !unix

package vmpool

import (
	"fmt"
	"os"
	"runtime"
)

// deviceNumber has no non-unix implementation: st_dev (and hardlink(2)'s EXDEV,
// the failure this package's device-sharing check exists to catch early — see
// checkPathsShareDevice) have no equivalent this package uses on Windows, which is
// not a deployment target (cgroup_windows.go's doc comment makes the same point
// for a different syscall). This stub exists so that fact is a deliberate,
// documented choice — GOOS=windows go vet ./... still compiles this package
// (Task 17's own reason for cgroup_windows.go) — rather than an accidental
// compile failure on a platform nobody runs this on. checkPathsShareDevice treats
// this error as "cannot verify" and returns it verbatim; it does not silently
// skip the check, because a silent skip would be indistinguishable from "checked
// and fine" to anyone reading the startup log.
func deviceNumber(path string) (uint64, error) {
	return 0, fmt.Errorf("vmpool: device-sharing check is not supported on %s (path %s)", runtime.GOOS, path)
}

// statOwnerMode has no non-unix implementation, for the identical reason
// deviceNumber above has none: st_uid/st_gid have no equivalent this package
// uses on Windows, which is not a deployment target. checkPathTraversableBy
// (traversalcheck.go) treats this error as "cannot verify" and returns it
// verbatim, the same contract checkPathsShareDevice already has with
// deviceNumber's stub.
func statOwnerMode(path string) (uid, gid uint32, mode os.FileMode, err error) {
	return 0, 0, 0, fmt.Errorf("vmpool: traversal check is not supported on %s (path %s)", runtime.GOOS, path)
}
