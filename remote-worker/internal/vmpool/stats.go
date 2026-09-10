package vmpool

import "sync"

// Stats is spec §7.1's vocabulary, kept apart on purpose. The dishonest number
// available here is "we ran 5,000 microVMs on one host": standbys are paused and
// CoW-shared, so that count inflates almost arbitrarily. StandbysResident is a
// memory-and-process statement, NOT a throughput claim; only InFlight is the
// resource-consuming quantity a knee applies to.
type Stats struct {
	InFlight             int   // VMs actually running a command
	ActiveRuns           int   // distinct workspace_keys with a live pool
	ParkedRuns           int   // RunParked keys: a workspace with zero standbys
	StandbysResident     int   // total paused VMs
	IdleStandbyResidency int   // standbys of runs idle > StandbyIdle/2 (spec §7.1)
	CommittedBytes       int64 // what the memory gate is holding against MaxCommittedBytes

	WarmAcquires      uint64
	ColdAcquires      map[ColdCause]uint64
	Refusals          map[RefusalReason]uint64
	Replenishments    uint64
	ReplenishFailures uint64
	DestroyFailures   uint64
}

// counters is the mutable half. A mutex over two small maps rather than atomics
// per key: this is read once per scrape and written once per Exec.
type counters struct {
	mu                sync.Mutex
	warm              uint64
	cold              map[ColdCause]uint64
	refusals          map[RefusalReason]uint64
	replenishments    uint64
	replenishFailures uint64
	destroyFailures   uint64
}

func newCounters() *counters {
	return &counters{cold: map[ColdCause]uint64{}, refusals: map[RefusalReason]uint64{}}
}

func (c *counters) warmAcquire() { c.mu.Lock(); c.warm++; c.mu.Unlock() }

func (c *counters) coldAcquire(cause ColdCause) { c.mu.Lock(); c.cold[cause]++; c.mu.Unlock() }

func (c *counters) refuse(r RefusalReason) { c.mu.Lock(); c.refusals[r]++; c.mu.Unlock() }

func (c *counters) replenished() { c.mu.Lock(); c.replenishments++; c.mu.Unlock() }

func (c *counters) replenishFailed() { c.mu.Lock(); c.replenishFailures++; c.mu.Unlock() }

func (c *counters) destroyFailed() { c.mu.Lock(); c.destroyFailures++; c.mu.Unlock() }

func (c *counters) snapshot(into *Stats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	into.WarmAcquires = c.warm
	into.Replenishments = c.replenishments
	into.ReplenishFailures = c.replenishFailures
	into.DestroyFailures = c.destroyFailures
	into.ColdAcquires = make(map[ColdCause]uint64, len(c.cold))
	for k, v := range c.cold {
		into.ColdAcquires[k] = v
	}
	into.Refusals = make(map[RefusalReason]uint64, len(c.refusals))
	for k, v := range c.refusals {
		into.Refusals[k] = v
	}
}
