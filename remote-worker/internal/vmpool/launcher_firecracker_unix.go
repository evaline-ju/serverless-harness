//go:build unix

package vmpool

import (
	"errors"
	osexec "os/exec"
	"syscall"
)

// The unix half of the Firecracker launcher's process-group handling. Isolated
// here for the same reason as internal/exec/runner_unix.go (see its comment):
// syscall.SysProcAttr's fields and syscall.Kill/syscall.ESRCH are per-platform,
// so referencing them directly from launcher_firecracker.go made every non-unix
// build fail deep inside that file with "unknown field Setpgid" and "undefined:
// syscall.Kill" — errors about a struct literal, saying nothing about the real
// cause. Firecracker and jailer only run on Linux anyway, so the !unix side
// (launcher_firecracker_other.go) is a refusal, not a second implementation.

// fcPlatformSupported is nil on unix; see launcher_firecracker_other.go for why
// the check exists at all.
func fcPlatformSupported() error { return nil }

// fcIsolateProcessGroup puts the jailer child in its own process group, so
// fcKillProcessGroup's -pid kill reaches jailer AND the firecracker process it
// execve's into (same pid) AND any helper thread/process Firecracker's own
// vcpu/seccomp machinery spawns, without also reaching this launcher's own
// process group.
func fcIsolateProcessGroup(cmd *osexec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// fcKillProcessGroup SIGKILLs the group led by pid.
func fcKillProcessGroup(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// fcProcessNotFound reports whether err is the "already gone" outcome of
// fcKillProcessGroup — expected when Destroy races a process that already
// exited on its own, and not itself a failure worth surfacing.
func fcProcessNotFound(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
