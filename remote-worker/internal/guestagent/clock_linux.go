//go:build linux

package guestagent

import (
	"syscall"
	"time"
)

// osSetWallClock sets CLOCK_REALTIME. It needs CAP_SYS_TIME, which the agent has
// because it is guest root — inside the VM boundary, which is the boundary this
// design moved (spec §1). Nothing on the host is trusted to the guest by this.
//
// Not called directly from ServeConn — see the setWallClock var in agent.go, which
// tests overwrite to observe (and count) clock-correction attempts without a real
// CAP_SYS_TIME syscall, the same indirection launcher_chv.go uses for chvChown.
func osSetWallClock(t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Settimeofday(&tv)
}
