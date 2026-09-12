//go:build windows

package vmpool

import "errors"

// killPidIgnoringAbsent has no Windows implementation: syscall.Kill, syscall.SIGKILL
// and syscall.ESRCH are unix-only names with no Windows equivalent in Go's standard
// syscall package. Windows is not a deployment target for this project (the worker
// runs on Linux; the "unix" build in cgroup_unix.go additionally covers darwin only
// because that is where this package's own tests run day to day) — this stub exists so
// that fact is a deliberate, documented choice rather than an accidental compile
// failure on a platform nobody was checking. SweepOrphans itself is otherwise
// platform-independent (directory walking, cgroup.procs parsing), so without this file
// GOOS=windows would fail on a single missing symbol deep in an unrelated call chain,
// which is a worse failure mode than a clear error at the one call site that would
// ever reach it.
func killPidIgnoringAbsent(pid int) error {
	return errors.New("vmpool: killing an orphaned VM process is not supported on windows")
}
