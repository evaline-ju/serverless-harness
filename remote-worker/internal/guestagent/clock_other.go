//go:build !linux

package guestagent

import (
	"errors"
	"time"
)

// osSetWallClock is unavailable off Linux. The agent only ever runs in a Linux guest;
// this exists so the package builds and its tests run on a developer's macOS laptop
// (spec §9 puts macOS out of scope for measurement, not for development).
//
// Not called directly from ServeConn — see the setWallClock var in agent.go.
func osSetWallClock(time.Time) error { return errors.New("setting the wall clock is Linux-only") }
