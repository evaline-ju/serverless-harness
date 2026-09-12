//go:build linux

package vmpool

import (
	"fmt"
	"syscall"
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
func RaiseMemlockLimit() (soft, hard uint64, err error) {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_MEMLOCK, &rl); err != nil {
		return 0, 0, fmt.Errorf("vmpool: RaiseMemlockLimit: getrlimit: %w", err)
	}
	rl.Cur = rl.Max
	if err := syscall.Setrlimit(syscall.RLIMIT_MEMLOCK, &rl); err != nil {
		return 0, 0, fmt.Errorf("vmpool: RaiseMemlockLimit: setrlimit(cur=%d,max=%d): %w", rl.Cur, rl.Max, err)
	}
	return uint64(rl.Cur), uint64(rl.Max), nil
}
