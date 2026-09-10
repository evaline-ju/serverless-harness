package vmpool

import "time"

// Clock is the time seam. Everything in this package that waits goes through it —
// the replenish delay (spec §4.4) and the reclaim ticker (spec §4.2) — so the unit
// tests spec §8 requires ("all with an injected clock and no KVM") contain no
// sleeps and no flakes.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is the subset of *time.Timer this package uses.
type Timer interface {
	// Stop cancels the timer, reporting whether it had not already fired.
	Stop() bool
}

type realClock struct{}

// RealClock is the production Clock.
func RealClock() Clock { return realClock{} }

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
