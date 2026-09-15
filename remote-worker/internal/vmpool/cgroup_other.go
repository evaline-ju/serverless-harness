//go:build !linux

package vmpool

import "errors"

// RaiseMemlockLimit is Linux-only. Spec §9 puts macOS out of scope for measurement, and
// there is no RLIMIT_MEMLOCK-raising or cgroup ceiling to record on a platform that
// never runs the worker in production — matching PinMemoryFile's pin_other.go stub.
func RaiseMemlockLimit() (soft, hard uint64, err error) {
	return 0, 0, errors.New("vmpool: raising RLIMIT_MEMLOCK requires Linux")
}
