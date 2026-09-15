//go:build !unix

package vmpool

import osexec "os/exec"

// The non-unix half — a no-op, deliberately not a second implementation. See
// launcher_chv_unix.go's comment: Cloud Hypervisor and virtiofsd require Linux,
// same as Firecracker/jailer, and launcher_chv.go's Restore already refuses before
// getting this far on such a platform via fcPlatformSupported (reused from the
// Firecracker arm). This exists only so launcher_chv.go itself needs no build
// tags of its own.
func chvIsolateAndDropPrivilegesPlatform(*osexec.Cmd, int, int) {}
