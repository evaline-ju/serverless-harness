//go:build linux

package vmpool

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// RaiseMemlockLimit raises RLIMIT_MEMLOCK to its own hard limit and returns both values
// so the worker can log them. Spec §7.5: RLIMIT_MEMLOCK, vm.max_map_count, nofile,
// TasksMax and pid_max must be recorded per run, because "these fail at 500 VMs after
// working at 20, indistinguishably from a real ceiling" — a kernel limit mistaken for a
// density ceiling wastes exactly the kind of debugging time §7.5 is trying to avoid.
//
// PinMemoryFile (pin_linux.go) mlocks the snapshot memory file per VM — the density
// mechanism spec §7.3 requires — and that allocation is the one the kernel cannot
// reclaim under pressure. Without raising the limit first, mlock starts failing well
// before the host's actual memory ceiling, in a way that looks identical to genuinely
// running out of RAM until someone checks `ulimit -l`.
//
// Fix round 1 (coordinator review of 36dbcb9): RLIMIT_MEMLOCK is NOT a member of Go's
// standard "syscall" package on linux/amd64 — only golang.org/x/sys/unix exports it,
// which is already a dependency of this module and already used the same way in
// internal/guestagent/listen_linux.go. The previous version referenced
// syscall.RLIMIT_MEMLOCK, which does not exist, so `GOOS=linux go build ./...` failed
// outright while `go build ./...` on darwin (this file excluded by its own build tag)
// stayed silently green. unix.Rlimit's Cur/Max fields are already uint64, so the
// uint64(...) casts the syscall.Rlimit version needed are gone too.
func RaiseMemlockLimit() (soft, hard uint64, err error) {
	var rl unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_MEMLOCK, &rl); err != nil {
		return 0, 0, fmt.Errorf("vmpool: RaiseMemlockLimit: getrlimit: %w", err)
	}
	rl.Cur = rl.Max
	if err := unix.Setrlimit(unix.RLIMIT_MEMLOCK, &rl); err != nil {
		return 0, 0, fmt.Errorf("vmpool: RaiseMemlockLimit: setrlimit(cur=%d,max=%d): %w", rl.Cur, rl.Max, err)
	}
	return rl.Cur, rl.Max, nil
}
