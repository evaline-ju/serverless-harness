//go:build linux

package guestagent

import (
	"syscall"
	"time"
)

// setWallClock sets CLOCK_REALTIME. It needs CAP_SYS_TIME, which the agent has because
// it is guest root — inside the VM boundary, which is the boundary this design moved
// (spec §1). Nothing on the host is trusted to the guest by this.
func setWallClock(t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Settimeofday(&tv)
}
