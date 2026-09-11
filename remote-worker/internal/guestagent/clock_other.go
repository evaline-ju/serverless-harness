//go:build !linux

package guestagent

import (
	"errors"
	"time"
)

// setWallClock is unavailable off Linux. The agent only ever runs in a Linux guest;
// this exists so the package builds and its tests run on a developer's macOS laptop
// (spec §9 puts macOS out of scope for measurement, not for development).
func setWallClock(time.Time) error { return errors.New("setting the wall clock is Linux-only") }
