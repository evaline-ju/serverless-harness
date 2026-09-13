//go:build !unix

package vmpool

import "os/exec"

// isolateProcessGroupForTest is a no-op off unix. These tests spawn `sleep` and signal
// pids, neither of which works on windows anyway (killPidIgnoringAbsent refuses outright
// there) — this exists purely so the package still COMPILES under GOOS=windows, which
// `go vet` requires of test files. See cgroup_pgroup_unix_test.go for the real one.
func isolateProcessGroupForTest(cmd *exec.Cmd) {}
