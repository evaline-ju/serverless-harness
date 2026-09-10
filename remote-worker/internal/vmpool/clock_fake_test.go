package vmpool

import (
	"sync"
	"time"
)

// fakeClock is virtual time for the whole package's tests. Advance fires due
// timers with the lock RELEASED, so a callback may arm another timer — which the
// reclaim ticker does on every tick.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c       *fakeClock
	at      time.Time
	f       func()
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), f: f}
	// Compact spent entries so a long soak test does not grow this slice without
	// bound; a re-arming ticker adds one per tick.
	if len(c.timers) > 1024 {
		live := c.timers[:0]
		for _, old := range c.timers {
			if !old.stopped {
				live = append(live, old)
			}
		}
		c.timers = live
	}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

// Advance moves virtual time to now+d, firing every timer due at or before that
// moment, earliest first, with Now() set to each timer's own deadline while its
// callback runs.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.timers {
			if t.stopped || t.at.After(target) {
				continue
			}
			if next == nil || t.at.Before(next.at) {
				next = t
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		next.stopped = true // one-shot: AfterFunc, not a ticker
		c.now = next.at
		f := next.f
		c.mu.Unlock()
		f()
	}
}
