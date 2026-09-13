//go:build !windows

package main

import "syscall"

// childCPUUsage returns RUSAGE_CHILDREN's user+system CPU time in microseconds. The
// VMMs vmpoolctl launches (Firecracker's jailer, cloud-hypervisor) are children of
// this process, so their CPU IS replenishment's CPU cost — spec §7.2: "The CPU number
// is what §7.3 divides into host capacity. Wall time alone misleads." A caller takes
// two readings around the measured window and subtracts; see mode "replenish"'s use in
// main.go.
//
// syscall.Getrusage/RUSAGE_CHILDREN is standard-library, unlike RLIMIT_MEMLOCK
// (limits_unix.go) — it is defined identically (by name) on both linux and darwin, so
// "!windows" (rather than the narrower "unix") is the correct, and simplest, tag: it is
// also true on the handful of other unix-likes x/sys/unix covers, and Getrusage/
// RUSAGE_CHILDREN exist there too by the same standard-library convention.
func childCPUUsage() (int64, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &ru); err != nil {
		return 0, err
	}
	// Timeval.Usec's width differs by platform (int32 on darwin, int64 on linux);
	// converting explicitly to int64 rather than relying on an untyped identity keeps
	// this file identical across the two, and Sec is int64 on both already.
	userUs := int64(ru.Utime.Sec)*1e6 + int64(ru.Utime.Usec)
	sysUs := int64(ru.Stime.Sec)*1e6 + int64(ru.Stime.Usec)
	return userUs + sysUs, nil
}
