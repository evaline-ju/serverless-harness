//go:build unix

package vmpool

import (
	osexec "os/exec"
	"syscall"
)

// The unix half of the Cloud Hypervisor launcher's process handling, isolated for
// the same reason as launcher_firecracker_unix.go: syscall.SysProcAttr's fields
// are per-platform, so referencing Credential directly from launcher_chv.go would
// make every non-unix build fail deep inside a struct literal instead of at this
// file's clean, named boundary. Cloud Hypervisor and virtiofsd only run on Linux
// anyway (launcher_chv_other.go's refusal covers everything else), so this is not
// a second implementation, just where the one real implementation lives.
//
// This reuses launcher_firecracker.go's process-group helpers directly
// (fcPlatformSupported, fcIsolateProcessGroup, fcKillProcessGroup,
// fcProcessNotFound): their logic — Setpgid isolation, -pid SIGKILL, ESRCH
// detection — is generic across both VMM arms, not Firecracker-specific, so this
// file adds only the one genuinely new piece Cloud Hypervisor's launcher needs:
// dropping virtiofsd's privileges via SysProcAttr.Credential.

// chvIsolateAndDropPrivilegesPlatform isolates cmd into its own process group
// (same purpose as fcIsolateProcessGroup: so fcKillProcessGroup's -pid kill on
// Destroy reaches this process and anything it spawns, without also reaching this
// launcher's own process group) and, when uid and gid are both nonzero, sets
// SysProcAttr.Credential so the child drops to that uid/gid before exec — the
// mechanism CHVOptions.VirtiofsdUID's doc comment and hardware-corrections C2
// require for virtiofsd. uid==0 (the sentinel Restore uses for the
// cloud-hypervisor process itself, which this arm does not run under a dedicated
// unprivileged account) isolates only and leaves Credential nil, matching the
// caller's contract in launcher_chv.go's chvIsolateAndDropPrivileges doc comment.
func chvIsolateAndDropPrivilegesPlatform(cmd *osexec.Cmd, uid, gid int) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if uid != 0 && gid != 0 {
		attr.Credential = &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		}
	}
	cmd.SysProcAttr = attr
}
