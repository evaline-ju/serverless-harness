//go:build unix

package main

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// rlimitMemlock and rlimitNofile read (never raise) the two rlimits spec §7.5 requires
// recorded per run. This uses golang.org/x/sys/unix, not the standard syscall package:
// RLIMIT_MEMLOCK is not a member of Go's standard syscall package on ANY platform,
// including Linux — internal/vmpool/cgroup_linux.go's RaiseMemlockLimit hit exactly
// this (its own doc comment: "The previous version referenced syscall.RLIMIT_MEMLOCK,
// which does not exist, so `GOOS=linux go build ./...` failed outright") and switched
// to x/sys/unix for the same reason this file does.
//
// x/sys/unix's own build tag ("aix || darwin || dragonfly || freebsd || linux ||
// netbsd || openbsd || solaris") is exactly Go's "unix" build constraint, so one file
// serves every unix-like target this binary builds for — linux, the deployment
// target, and darwin, where this package's own tests run day to day — without a
// further linux/darwin split. limits_windows.go covers the one remaining leg of the
// three-way cross-platform check this project requires.
func rlimitMemlock() string { return formatRlimit(unix.RLIMIT_MEMLOCK) }
func rlimitNofile() string  { return formatRlimit(unix.RLIMIT_NOFILE) }

func formatRlimit(resource int) string {
	var rl unix.Rlimit
	if err := unix.Getrlimit(resource, &rl); err != nil {
		return "unavailable"
	}
	if rl.Cur == unix.RLIM_INFINITY {
		return "unlimited"
	}
	return strconv.FormatUint(uint64(rl.Cur), 10)
}
