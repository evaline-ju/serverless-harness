//go:build linux

package vmpool

import (
	"fmt"
	"os"
	"syscall"
)

// PinMemoryFile maps a snapshot's memory file MAP_SHARED and mlocks it, which is what
// `vmtouch -dl` does and why spec §7.3 calls it required.
//
// THIS IS THE DENSITY MECHANISM. Each VMM maps the same file MAP_PRIVATE, so every
// unmodified guest page is the SAME host page-cache page across every VM on the host —
// which is why spec §7.3 insists on PSS rather than RSS when reporting: summing RSS
// across 200 VMMs multiplies the shared set by 200 and reports ~50 GiB where the truth
// is ~2 GiB, wrong by more than an order of magnitude in the pessimistic direction.
//
// It is also the one allocation the kernel CANNOT reclaim, which is why swap is off and
// why the memory gate exists rather than trusting pressure to degrade gracefully
// (spec §6). RLIMIT_MEMLOCK must be raised first, and recorded per run (spec §7.5).
func PinMemoryFile(path string) (func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	n := int(fi.Size())
	if n == 0 {
		return nil, fmt.Errorf("pin %s: file is empty", path)
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, n, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	if err := syscall.Mlock(b); err != nil {
		_ = syscall.Munmap(b)
		return nil, fmt.Errorf("mlock %s (%d bytes): %w — raise RLIMIT_MEMLOCK; spec §6 "+
			"lists it among the kernel limits that fail at 500 VMs after working at 20", path, n, err)
	}
	return func() error {
		_ = syscall.Munlock(b)
		return syscall.Munmap(b)
	}, nil
}
