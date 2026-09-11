//go:build !unix

package vmpool

import (
	"fmt"
	osexec "os/exec"
	"runtime"
)

// The non-unix half: a refusal, deliberately not a second implementation — see
// launcher_firecracker_unix.go's comment. Firecracker and jailer require Linux
// (KVM, cgroups, chroot); there is no non-unix equivalent worth pretending to
// support, so this build refuses clearly rather than failing opaquely inside
// os/exec or leaving a zombie process nothing can reap.

// fcPlatformSupported refuses before any jailer process is spawned.
func fcPlatformSupported() error {
	return fmt.Errorf("firecracker: %s is not a supported host: jailer and Firecracker require Linux/KVM", runtime.GOOS)
}

// fcIsolateProcessGroup is unreachable — Restore returns on fcPlatformSupported
// first. It exists so launcher_firecracker.go needs no build tags of its own.
func fcIsolateProcessGroup(*osexec.Cmd) {}

// fcKillProcessGroup is likewise unreachable.
func fcKillProcessGroup(int) error { return fcPlatformSupported() }

// fcProcessNotFound is likewise unreachable.
func fcProcessNotFound(error) bool { return false }
