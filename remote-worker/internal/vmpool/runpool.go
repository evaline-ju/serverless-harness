package vmpool

import (
	"sync"
	"time"
)

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

	// execGate is the ONE field here that is NOT guarded by pool.mu and takes a
	// lock of its own — deliberately, because what it protects (a VM's Resume and
	// Run, which dial vsock and block on a guest) must never happen while pool.mu
	// is held. ExecPhased holds it from a successful acquire until this VM is
	// destroyed, but only when the Launcher reports SerializesExecsPerRun():
	// Firecracker's ext4 workspace is not a shared-disk filesystem, so a second
	// guest mounting it rw while the first still holds it would corrupt it
	// (spec §4.3). Zero value is an unlocked Mutex, so a run's first Exec needs no
	// separate initialization.
	execGate sync.Mutex
}

func (rp *runPool) signalSettledLocked() {
	close(rp.settled)
	rp.settled = make(chan struct{})
}

func (rp *runPool) idleFor(now time.Time) time.Duration { return now.Sub(rp.lastExec) }

// busy reports whether anything about this run is in motion. A run with work in
// flight or a warming pending is never swept, whatever its idle clock says.
func (rp *runPool) busy() bool { return rp.inFlight > 0 || rp.warming > 0 }

// forgetPending drops a fired timer so pending does not grow across a long run.
// Caller holds pool.mu.
func (rp *runPool) forgetPending(tm Timer) {
	for i, t := range rp.pending {
		if t == tm {
			rp.pending = append(rp.pending[:i], rp.pending[i+1:]...)
			return
		}
	}
}
