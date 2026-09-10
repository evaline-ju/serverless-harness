package vmpool

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Exec is one command as the pool sees it: pb.Exec minus the wire types.
type Exec struct {
	ReqID     uint64
	Command   string
	Stdin     []byte
	TimeoutS  uint32
	Streaming bool
}

// Pool is the package's whole contract (spec §4.1).
//
// Exec is deliberately high-level rather than Acquire/Destroy: the VM handle never
// escapes the package, destroy is a defer, and a caller cannot leak a VM by
// returning early.
type Pool interface {
	// Exec acquires a standby VM bound to key's workspace, runs exactly one
	// command, and destroys the VM before returning. A VM is NEVER reused.
	Exec(ctx context.Context, key string, e Exec, out Sink) (Result, error)
	// Reclaim drops a run's standby VMs AND its workspace directory — the full
	// form. Dropping standbys alone is internal to the sweep (spec §4.4).
	Reclaim(ctx context.Context, key string) error
	Stats() Stats
	// Close stops the sweep and destroys everything. Beyond spec §4.1's listing:
	// §6 requires a shutdown that leaves no orphan VMs.
	Close() error
}

type pool struct {
	cfg Config
	lc  Launcher
	clk Clock

	mu     sync.Mutex
	runs   map[string]*runPool
	closed bool
	seq    uint64

	counters *counters
}

// New validates cfg and returns a Pool. It does NOT start the reclaim ticker —
// Task 6 adds that, and until then the pool has nothing to reclaim on a timer.
func New(cfg Config, lc Launcher, clk Clock) (Pool, error) {
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	if lc == nil {
		return nil, fmt.Errorf("vmpool: a Launcher is required")
	}
	if lc.Kind() != cfg.VMM {
		return nil, fmt.Errorf("vmpool: Launcher is %q but Config.VMM is %q — the A/B is an image "+
			"swap, so a mismatch here would silently measure the wrong arm", lc.Kind(), cfg.VMM)
	}
	if clk == nil {
		clk = RealClock()
	}
	return &pool{cfg: cfg, lc: lc, clk: clk, runs: map[string]*runPool{}, counters: newCounters()}, nil
}

func (p *pool) refuse(r RefusalReason, format string, a ...any) error {
	p.counters.refuse(r)
	return refusal(r, format, a...)
}

func (p *pool) Exec(ctx context.Context, key string, e Exec, out Sink) (Result, error) {
	if err := checkKey(key); err != nil {
		if r := ReasonOf(err); r != "" {
			p.counters.refuse(r)
		}
		return Result{}, err
	}

	vm, err := p.acquire(ctx, key)
	if err != nil {
		return Result{}, err
	}
	// One identical teardown for abort, timeout and success (spec §4.1), and no
	// early return below can leak a VM.
	defer p.destroy(key, vm)

	if vm.Key() != key {
		return Result{}, fmt.Errorf("%w: popped %q for %q", ErrKeyMismatch, vm.Key(), key)
	}
	if err := vm.Resume(ctx); err != nil {
		return Result{}, p.refuse(RefuseSpawn, "resume %q: %v", key, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var timedOut atomic.Bool
	if e.TimeoutS > 0 {
		tm := p.clk.AfterFunc(time.Duration(e.TimeoutS)*time.Second, func() {
			timedOut.Store(true)
			cancel()
		})
		defer tm.Stop()
	}

	res, runErr := vm.Run(runCtx, Command{
		Command:   e.Command,
		Stdin:     e.Stdin,
		TimeoutS:  e.TimeoutS,
		Streaming: e.Streaming,
		CapBytes:  OutputCapBytes,
	}, out)

	// Order matters: a timeout also cancels runCtx, so timeout is checked first or
	// every timeout would report as an abort and the worker would emit End{-1}
	// instead of ExecError{"timeout:<n>"}.
	switch {
	case timedOut.Load():
		return res, fmt.Errorf("%w:%d", ErrTimeout, e.TimeoutS)
	case ctx.Err() != nil:
		return res, ErrAborted
	}
	return res, runErr
}

// acquire returns a VM bound to key, warm if one is Ready and cold otherwise.
// Task 5 replaces the cold branch with "block on the warming already in flight
// rather than starting a second one"; here there are no standbys yet, so a cold
// acquire warms one synchronously.
func (p *pool) acquire(ctx context.Context, key string) (VM, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	rp, fresh, err := p.runLocked(key)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	rp.lastExec = p.clk.Now()

	if n := len(rp.ready); n > 0 {
		vm := rp.ready[n-1]
		rp.ready = rp.ready[:n-1]
		rp.inFlight++
		p.mu.Unlock()
		p.counters.warmAcquire()
		return vm, nil
	}

	cause := ColdExhausted
	switch {
	case fresh:
		cause = ColdFirstExec
	case rp.parked:
		cause = ColdParked
	}
	rp.parked = false
	rp.inFlight++
	dir, id := rp.dir, p.nextIDLocked()
	p.mu.Unlock()

	p.counters.coldAcquire(cause)
	vm, err := p.warm(ctx, key, dir, id)
	if err != nil {
		p.mu.Lock()
		rp.inFlight--
		p.mu.Unlock()
		return nil, p.refuse(RefuseSpawn, "restore for %q: %v", key, err)
	}
	return vm, nil
}

// warm creates the run's workspace if it does not exist and restores one VM into
// it. The mkdir is off the lock: a slow filesystem must not stall every other run's
// Exec, and MkdirAll is idempotent so concurrent warms for one key race harmlessly.
func (p *pool) warm(ctx context.Context, key, dir, id string) (VM, error) {
	if err := ensureWorkspace(dir); err != nil {
		return nil, fmt.Errorf("workspace %s: %w", dir, err)
	}
	return p.lc.Restore(ctx, RestoreRequest{
		ID:            id,
		Key:           key,
		WorkspaceDir:  dir,
		GuestRAMBytes: p.cfg.GuestRAMBytes,
	})
}

// destroy is the single teardown. A destroy failure is counted and logged, never
// returned: it does not change what the command did, and spec §6 makes leaked VMs
// the #1 practical failure — so it must be visible in Stats rather than folded into
// one exec's error.
func (p *pool) destroy(key string, vm VM) {
	p.mu.Lock()
	if rp := p.runs[key]; rp != nil && rp.inFlight > 0 {
		rp.inFlight--
	}
	p.mu.Unlock()
	if err := vm.Destroy(); err != nil {
		p.counters.destroyFailed()
		log.Printf("vmpool: destroy VM for %q: %v", key, err)
	}
}

// runLocked returns key's runPool, creating it if unseen. Caller holds p.mu.
func (p *pool) runLocked(key string) (*runPool, bool, error) {
	if rp := p.runs[key]; rp != nil {
		return rp, false, nil
	}
	dir, err := p.workspaceDir(key)
	if err != nil {
		return nil, false, err
	}
	rp := &runPool{key: key, dir: dir, settled: make(chan struct{}), lastExec: p.clk.Now()}
	p.runs[key] = rp
	return rp, true, nil
}

func (p *pool) nextIDLocked() string {
	p.seq++
	return "vm-" + strconv.FormatUint(p.seq, 10)
}

// Reclaim drops a run's standbys AND its workspace directory. Safe to fire early:
// spec §4.4 establishes the workspace is a per-dispatch derivation — a detached
// worktree at a pinned commit, with continuity living in the Redis session log — so
// reclaiming costs a re-converge, never data.
func (p *pool) Reclaim(ctx context.Context, key string) error {
	p.mu.Lock()
	rp := p.runs[key]
	if rp == nil {
		p.mu.Unlock()
		return nil
	}
	victims := rp.ready
	rp.ready = nil
	for _, tm := range rp.pending {
		tm.Stop()
	}
	rp.pending = nil
	dir := rp.dir
	// Keep the entry while work is in flight: deleting it would let the next Exec
	// re-create the directory under a run that is still using the old one.
	if !rp.busy() {
		delete(p.runs, key)
	}
	p.mu.Unlock()

	for _, vm := range victims {
		if err := vm.Destroy(); err != nil {
			p.counters.destroyFailed()
			log.Printf("vmpool: reclaim %q: destroy: %v", key, err)
		}
	}
	return removeWorkspace(dir)
}

func (p *pool) Stats() Stats {
	var s Stats
	p.mu.Lock()
	now := p.clk.Now()
	half := p.cfg.StandbyIdle / 2
	for _, rp := range p.runs {
		s.ActiveRuns++
		s.InFlight += rp.inFlight
		s.StandbysResident += len(rp.ready)
		if rp.parked {
			s.ParkedRuns++
		}
		if rp.idleFor(now) > half {
			s.IdleStandbyResidency += len(rp.ready)
		}
		s.CommittedBytes += int64(len(rp.ready)+rp.inFlight+rp.warming) * p.perVMBytes()
	}
	p.mu.Unlock()
	p.counters.snapshot(&s)
	return s
}

// perVMBytes is what one VM costs the budget. A standby is charged its FULL guest
// RAM even though it is paused and CoW-shared, because it is one Resume away from
// consuming all of it — admission control that charged the paused footprint would
// admit a host it cannot then run (spec §6, §7.3).
func (p *pool) perVMBytes() int64 { return p.cfg.GuestRAMBytes + p.cfg.VMOverheadBytes }

func (p *pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var victims []VM
	for _, rp := range p.runs {
		victims = append(victims, rp.ready...)
		rp.ready = nil
		for _, tm := range rp.pending {
			tm.Stop()
		}
		rp.pending = nil
	}
	p.mu.Unlock()

	var firstErr error
	for _, vm := range victims {
		if err := vm.Destroy(); err != nil {
			p.counters.destroyFailed()
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
