//go:build unix

package vmpool

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroupForTest puts a test child in its own process group, exactly as BOTH
// real launchers do for the VMM they spawn (fcIsolateProcessGroup in
// launcher_firecracker_unix.go; chvIsolateAndDropPrivilegesPlatform in
// launcher_chv_unix.go). SweepOrphans' third guard refuses any pid in the SWEEPING
// process's own group, so a stand-in for a leaked VMM has to be isolated the same way the
// real one is or the test would be modelling a process shape that cannot occur: a real
// orphan is left behind by a PREVIOUS worker incarnation and is therefore never in the
// current sweeper's process group.
//
// Build-tagged because syscall.SysProcAttr.Setpgid is a unix-only field, and `go vet`
// compiles test files — leaving this in cgroup_test.go would break GOOS=windows vet on an
// "unknown field Setpgid" error, the same trap launcher_firecracker_unix.go's own comment
// records.
func isolateProcessGroupForTest(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
