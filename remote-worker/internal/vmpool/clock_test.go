package vmpool

import (
	"testing"
	"time"
)

func TestFakeClockFiresDueTimersEarliestFirst(t *testing.T) {
	c := newFakeClock()
	var order []string
	var seen []time.Time
	for _, d := range []struct {
		name string
		d    time.Duration
	}{{"third", 30 * time.Millisecond}, {"first", 10 * time.Millisecond}, {"second", 20 * time.Millisecond}} {
		name := d.name
		c.AfterFunc(d.d, func() { order = append(order, name); seen = append(seen, c.Now()) })
	}
	c.Advance(25 * time.Millisecond)
	if got := len(order); got != 2 {
		t.Fatalf("fired %d timers (%v), want 2 — 30ms is not due yet", got, order)
	}
	if order[0] != "first" || order[1] != "second" {
		t.Fatalf("order = %v, want [first second]", order)
	}
	// Now() inside a callback must be the timer's OWN deadline, not the Advance
	// target: the sweep reads Now() to compare against per-run idle clocks, so a
	// clock that jumps straight to the target would age runs by the wrong amount.
	if seen[0].Sub(seen[1]) != -10*time.Millisecond {
		t.Fatalf("callbacks saw %v then %v; want 10ms apart", seen[0], seen[1])
	}
	c.Advance(10 * time.Millisecond)
	if len(order) != 3 || order[2] != "third" {
		t.Fatalf("order = %v, want third to fire after the second Advance", order)
	}
}

func TestFakeClockStoppedTimerDoesNotFire(t *testing.T) {
	c := newFakeClock()
	fired := false
	tm := c.AfterFunc(time.Millisecond, func() { fired = true })
	if !tm.Stop() {
		t.Fatal("Stop() on a live timer returned false")
	}
	if tm.Stop() {
		t.Fatal("Stop() on an already-stopped timer returned true")
	}
	c.Advance(time.Second)
	if fired {
		t.Fatal("a stopped timer fired")
	}
}

// The reclaim ticker re-arms itself from inside its own callback (spec §4.2's
// second trigger). Advance must run those callbacks with its lock released and
// must terminate: a self-rescheduling timer always lands past the target.
func TestFakeClockReArmingTickerTerminates(t *testing.T) {
	c := newFakeClock()
	ticks := 0
	var arm func()
	arm = func() {
		c.AfterFunc(10*time.Millisecond, func() {
			ticks++
			arm()
		})
	}
	arm()
	c.Advance(100 * time.Millisecond)
	if ticks != 10 {
		t.Fatalf("ticks = %d, want 10", ticks)
	}
}
