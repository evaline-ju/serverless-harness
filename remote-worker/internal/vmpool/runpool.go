package vmpool

import "time"

// runPool is one workspace_key's state — spec §4.2's second state machine:
//
//	RunAbsent --first Exec--> RunWarming --> RunActive --idle > StandbyIdle--> RunParked
//	RunParked --Exec (cold acquire)--> RunWarming
//	RunParked --idle > WorkspaceIdle--> RunReclaiming --> RunAbsent
//
// RunParked is the state a run on a human gate or awaiting user input sits in:
// workspace on disk, ZERO standby VMs, costing disk rather than RAM (spec §4.4).
//
// Every field is guarded by pool.mu. Nothing here takes a lock of its own — one
// lock, held briefly, is what keeps the per-Exec sweep a microsecond map walk.
type runPool struct {
	key string
	dir string

	ready    []VM // paused standbys, ready to acquire
	inFlight int  // VMs currently running a command for this key
	warming  int  // replenishments in flight

	// settled is closed whenever a warming completes, then replaced. A cold
	// acquire waits on it rather than starting a second warming (spec §4.2).
	settled chan struct{}

	lastExec time.Time
	parked   bool

	// backoff is the per-run replenishment backoff after a spawn failure
	// (spec §6: "exponential backoff per run pool. Never a hang").
	backoff time.Duration

	// pending holds scheduled replenish timers so Close and Reclaim can cancel
	// them; a timer that fires into a reclaimed run would resurrect it.
	pending []Timer
}

func (rp *runPool) signalSettledLocked() {
	close(rp.settled)
	rp.settled = make(chan struct{})
}

func (rp *runPool) idleFor(now time.Time) time.Duration { return now.Sub(rp.lastExec) }

// busy reports whether anything about this run is in motion. A run with work in
// flight or a warming pending is never swept, whatever its idle clock says.
func (rp *runPool) busy() bool { return rp.inFlight > 0 || rp.warming > 0 }
