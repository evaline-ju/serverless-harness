package vmpool

import (
	"context"
	"log"
	"time"
)

// Backoff bounds for a failing run pool (spec §6: "exponential backoff per run
// pool. Never a hang"). The cap matters as much as the growth: without it a
// persistently failing run stops retrying in any useful sense, and a recovered host
// would not refill until the next Exec.
const (
	replenishBackoffMin = 250 * time.Millisecond
	replenishBackoffMax = 30 * time.Second
)

func nextBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return replenishBackoffMin
	}
	return min(d*2, replenishBackoffMax)
}

// scheduleReplenish arms a refill of ONE slot for key.
//
// DELAYED, NOT IMMEDIATE, and spec §4.4 is worth restating because the number is
// large: every Exec schedules a replenishment, so an unconditional refill hands D
// fresh standbys to a run at the exact moment it stops issuing work. At 256 MiB
// guests, D=2 and ~100 runs finishing in five minutes that is ~50 GiB of memory
// spent on VMs that will never serve a command. A run's next Exec is separated by a
// model round trip of hundreds of ms, so a 200ms delay still completes well before
// it arrives.
//
// It cannot take the residual to zero. A dispatch's LAST wire event is
// cleanupWorkspace (converge.ts:44-51) — an Exec like any other, indistinguishable
// from work — so the pool pops a standby, refreshes the idle clock and schedules a
// refill for a run that is already over. StandbyIdle ages that remainder out; only
// spec §9's Release removes it, and §7.3's idle-standby-residency metric is what
// measures it.
func (p *pool) scheduleReplenish(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rp := p.runs[key]; rp != nil {
		p.scheduleReplenishLocked(rp)
	}
}

// scheduleReplenishLocked arms the timer. Caller holds p.mu.
func (p *pool) scheduleReplenishLocked(rp *runPool) {
	if p.closed {
		return
	}
	// Refill toward D, counting what is already Ready or on its way.
	if len(rp.ready)+rp.warming >= p.cfg.StandbyDepth {
		return
	}
	key := rp.key
	delay := p.cfg.ReplenishDelay + rp.backoff
	// tm is written after AfterFunc returns but while p.mu is still held, and the
	// callback below takes p.mu before reading it, so the lock orders the two. This
	// is the only place in the package that depends on that.
	var tm Timer
	tm = p.clk.AfterFunc(delay, func() {
		p.mu.Lock()
		if rp2 := p.runs[key]; rp2 != nil {
			rp2.forgetPending(tm)
		}
		p.mu.Unlock()
		p.replenishOne(key)
	})
	rp.pending = append(rp.pending, tm)
}

// replenishOne restores one standby for key, then arms the next slot if D is not
// yet met. One timer per slot rather than a burst, so a finished Exec does not
// create D VMs in the same instant.
func (p *pool) replenishOne(key string) {
	p.mu.Lock()
	rp := p.runs[key]
	if rp == nil || p.closed {
		p.mu.Unlock()
		return
	}
	if len(rp.ready)+rp.warming >= p.cfg.StandbyDepth {
		p.mu.Unlock()
		return
	}
	if err := p.admitLocked(false); err != nil {
		// A refused refill is nobody's error: the next Exec takes a cold acquire and
		// is counted as one. Never a hang (spec §6).
		rp.backoff = nextBackoff(rp.backoff)
		p.scheduleReplenishLocked(rp)
		p.mu.Unlock()
		p.counters.replenishFailed()
		return
	}
	rp.warming++
	dir, id := rp.dir, p.nextIDLocked()
	// Registered under the SAME p.mu hold that verified !p.closed above, so no Add can
	// land after Close's Wait: Close sets closed under p.mu and only then unlocks and
	// waits, and any replenishOne reaching this lock afterwards returns at the check
	// above without adding. The matching Done is deferred rather than placed at each
	// return so it also covers the post-warm `case p.closed` branch below — which is
	// the one that actually destroys this VM, and the one whose completion Close is
	// waiting for. See Close for why waiting matters at all.
	p.inFlightWarms.Add(1)
	defer p.inFlightWarms.Done()
	p.mu.Unlock()

	vm, err := p.warm(context.Background(), key, dir, id)

	p.mu.Lock()
	rp.warming--
	// Wake any cold acquirer waiting on this warming, on BOTH paths. A waiter woken
	// only on success would block until its own deadline whenever a warming failed —
	// or whenever the warming was a synchronous cold acquire whose VM went straight
	// to its caller and never became a standby.
	rp.signalSettledLocked()
	switch {
	case err != nil:
		rp.backoff = nextBackoff(rp.backoff)
		p.scheduleReplenishLocked(rp)
		p.mu.Unlock()
		p.counters.replenishFailed()
		log.Printf("vmpool: replenish %q: %v", key, err)
		return
	case p.closed:
		p.mu.Unlock()
		_ = vm.Destroy()
		return
	}
	rp.backoff = 0
	rp.ready = append(rp.ready, vm)
	p.scheduleReplenishLocked(rp)
	p.mu.Unlock()
	p.counters.replenished()
}
