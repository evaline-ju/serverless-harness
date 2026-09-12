//go:build unix

package vmpool

import "syscall"

// killPidIgnoringAbsent sends SIGKILL to pid. A pid that has already exited (ESRCH) is
// treated as already-swept, not an error — the brief's own framing: "pids that no
// longer exist still count as swept."
//
// The "unix" build constraint (Go 1.19+) covers both linux, the deployment target, and
// darwin, the platform this package's own tests run on day to day — syscall.Kill,
// syscall.SIGKILL and syscall.ESRCH are all defined identically (by name) on both, so
// one implementation serves both without a further linux/darwin split. See
// cgroup_windows.go for why this got its own build-tagged file at all (fix round 1,
// coordinator review of 36dbcb9, item 3): windows is not a target for this project, but
// leaving killPidIgnoringAbsent in cgroup.go with no build tag meant GOOS=windows
// failed to compile the whole package on a missing-symbol error rather than an
// intentional, documented stub.
func killPidIgnoringAbsent(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
