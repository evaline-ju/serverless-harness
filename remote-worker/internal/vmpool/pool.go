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
	// ExecPhased is Exec with the hot path decomposed into Phases, for vmpoolctl
	// and E10's driver (spec §3.1). ph may be nil. There is exactly one
	// implementation: Exec delegates to this with a throwaway Phases.
	ExecPhased(ctx context.Context, key string, e Exec, out Sink, ph *Phases) (Result, error)
	// Reclaim drops a run's standby VMs AND its workspace directory — the full
	// form. Dropping standbys alone is internal to the sweep (spec §4.4).
	Reclaim(ctx context.Context, key string) error
	Stats() Stats
	// Probe restores one VM, runs a trivial command in it and destroys it, without
	// otherwise touching the pool's accounting. Spec §6's last row: a pinned hash is
	// not sufficient — a snapshot can be intact and still unrestorable on this host —
	// so the caller (the worker's startup sequence) must fail at START, not on a
	// user's first request.
	Probe(ctx context.Context) error
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

	// ticker is spec §4.2's second sweep trigger: the per-Exec sweep alone cannot
	// reclaim a run whose last Exec was also its last (Task 6).
	ticker      Timer
	reclaimQ    chan reclaimBatch
	reclaimDone sync.WaitGroup
}

// New validates cfg and returns a Pool. It starts the reclaim goroutine and arms
// the reclaim ticker (spec §4.2's second sweep trigger) before returning, so a
// caller's very first Exec is already covered by both.
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
	p := &pool{cfg: cfg, lc: lc, clk: clk, runs: map[string]*runPool{}, counters: newCounters()}
	// Sized so a full host's worth of sweeps queues rather than blocking; a full
	// queue falls back to an inline destroy (see sweep).
	p.reclaimQ = make(chan reclaimBatch, 64)
	p.reclaimDone.Add(1)
	go p.reclaimLoop()
	p.mu.Lock()
	p.armTickerLocked()
	p.mu.Unlock()
	return p, nil
}

func (p *pool) refuse(r RefusalReason, format string, a ...any) error {
	p.counters.refuse(r)
	return refusal(r, format, a...)
}

// classify maps an error from any timeout-bounded step onto the sentinel the wire
// contract needs, or returns nil if the step's error is not a cancellation at all and
// the caller should classify it on its own terms.
//
// Timeout is tested BEFORE abort, and that order is load-bearing: the timer cancels
// runCtx, so checking the abort case first would report every timeout as an abort and
// the worker would emit a terminal signal frame where the contract requires
// ExecError{"timeout:<n>"}. The outer ctx is what distinguishes a genuine abort from
// the timer's own cancellation, which is why it — and not runCtx — is the argument.
func (p *pool) classify(ctx context.Context, timedOut *atomic.Bool, timeoutS uint32) error {
	switch {
	case timedOut.Load():
		return fmt.Errorf("%w:%d", ErrTimeout, timeoutS)
	case ctx.Err() != nil:
		return ErrAborted
	}
	return nil
}

func (p *pool) Exec(ctx context.Context, key string, e Exec, out Sink) (Result, error) {
	return p.ExecPhased(ctx, key, e, out, nil)
}

// Phases is the hot path decomposed, filled by ExecPhased. E10 rung 2 measures
// "vsock -> run -> response -> teardown" as separate terms, and a single round-trip
// number cannot answer the 15ms question — it hides which term is the cost.
type Phases struct {
	Acquire time.Duration // pop a Ready VM, or warm one (a cold acquire)
	Resume  time.Duration
	Run     time.Duration
	Destroy time.Duration
	Cold    ColdCause // "" when the acquire was warm
}

// ExecPhased is Exec with instrumentation. Exec delegates to it with a throwaway
// Phases, so there is exactly one implementation and the measured path IS the
// production path (spec §3.1: "E10 must measure the production code path"). ph may
// be nil — every stamp below guards for it — so callers that do not care about the
// decomposition (i.e. Exec itself) pay nothing for it.
func (p *pool) ExecPhased(ctx context.Context, key string, e Exec, out Sink, ph *Phases) (Result, error) {
	// Trigger 1 of spec §4.2's two: a map walk over at most MaxRuns entries,
	// microseconds, not a timer per run. The destroys it schedules happen on the
	// reclaim goroutine, so this adds no munmap to the hot path.
	p.sweep(p.clk.Now())

	if err := checkKey(key); err != nil {
		return Result{}, p.countRefusal(err)
	}

	// The timeout must bound the WHOLE Exec, including a cold acquire's VM boot —
	// not just vm.Run — or a wedged VMM hangs until the relay stream dies. The
	// container worker times its exec around the entire process spawn for the same
	// reason: the harness cannot tell the two tiers apart, and spec §6 requires a
	// cold-acquire storm to stay "counted, never queued unboundedly."
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

	t0 := p.clk.Now()
	vm, cause, err := p.acquire(runCtx, key)
	if ph != nil {
		ph.Acquire = p.clk.Now().Sub(t0)
		ph.Cold = cause
	}
	if err != nil {
		if cErr := p.classify(ctx, &timedOut, e.TimeoutS); cErr != nil {
			return Result{}, cErr
		}
		return Result{}, err
	}
	// One identical teardown for abort, timeout and success (spec §4.1), and no
	// early return below can leak a VM.
	defer func() {
		t0 := p.clk.Now()
		p.destroy(key, vm)
		if ph != nil {
			ph.Destroy = p.clk.Now().Sub(t0)
		}
	}()

	if vm.Key() != key {
		return Result{}, fmt.Errorf("%w: popped %q for %q", ErrKeyMismatch, vm.Key(), key)
	}
	t0 = p.clk.Now()
	resumeErr := vm.Resume(runCtx)
	if ph != nil {
		ph.Resume = p.clk.Now().Sub(t0)
	}
	if resumeErr != nil {
		if cErr := p.classify(ctx, &timedOut, e.TimeoutS); cErr != nil {
			return Result{}, cErr
		}
		return Result{}, p.refuse(RefuseSpawn, "resume %q: %v", key, resumeErr)
	}

	t0 = p.clk.Now()
	res, runErr := vm.Run(runCtx, Command{
		Command:   e.Command,
		Stdin:     e.Stdin,
		TimeoutS:  e.TimeoutS,
		Streaming: e.Streaming,
		CapBytes:  OutputCapBytes,
	}, out)
	if ph != nil {
		ph.Run = p.clk.Now().Sub(t0)
	}

	if cErr := p.classify(ctx, &timedOut, e.TimeoutS); cErr != nil {
		return res, cErr
	}
	return res, runErr
}

// acquire returns a VM bound to key: warm if one is Ready, cold otherwise. The
// returned ColdCause is the cause of the cold acquire ("" for a warm one), so
// ExecPhased can carry it into Phases.Cold; every error path returns an empty
// cause because there is nothing yet to attribute.
//
// The cold path blocks on the warming already in flight rather than starting a
// second one (spec §4.2), then loops. Looping rather than recursing is what keeps
// the acquire counted exactly once — cold-acquire rate is E11's headline diagnostic,
// and double-counting it would read as replenishment falling behind.
func (p *pool) acquire(ctx context.Context, key string) (VM, ColdCause, error) {
	counted := false
	var cause ColdCause
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, "", ErrClosed
		}
		rp, fresh, err := p.runLocked(key)
		if err != nil {
			p.mu.Unlock()
			return nil, "", p.countRefusal(err)
		}
		rp.lastExec = p.clk.Now()

		if n := len(rp.ready); n > 0 {
			vm := rp.ready[n-1]
			rp.ready = rp.ready[:n-1]
			rp.inFlight++
			p.mu.Unlock()
			if !counted {
				p.counters.warmAcquire()
			}
			return vm, cause, nil
		}

		if !counted {
			cause = ColdExhausted
			switch {
			case fresh:
				cause = ColdFirstExec
			case rp.parked:
				cause = ColdParked
			}
			p.counters.coldAcquire(cause)
			counted = true
		}
		rp.parked = false

		if rp.warming > 0 {
			settled := rp.settled
			p.mu.Unlock()
			select {
			case <-settled:
				continue
			case <-ctx.Done():
				return nil, "", ErrAborted
			}
		}

		if err := p.admitLocked(false); err != nil {
			p.mu.Unlock()
			return nil, "", p.countRefusal(err)
		}
		rp.warming++
		rp.inFlight++
		dir, id := rp.dir, p.nextIDLocked()
		p.mu.Unlock()

		vm, err := p.warm(ctx, key, dir, id)

		p.mu.Lock()
		rp.warming--
		rp.signalSettledLocked()
		if err != nil {
			rp.inFlight--
			rp.backoff = nextBackoff(rp.backoff)
			p.mu.Unlock()
			if ctx.Err() != nil {
				// An abort during a cold warm is not a spawn failure. Counting it as
				// one pollutes the by-cause exec-error accounting, which has to tell a
				// real spawn failure apart from an ordinary cancellation, and it would
				// make the worker emit an exec error where the wire contract wants an
				// abort.
				return nil, "", ErrAborted
			}
			return nil, "", p.refuse(RefuseSpawn, "restore for %q: %v", key, err)
		}
		rp.backoff = 0
		p.mu.Unlock()
		return vm, cause, nil
	}
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
	if rp := p.runs[key]; rp != nil {
		if rp.inFlight > 0 {
			rp.inFlight--
		}
		p.scheduleReplenishLocked(rp)
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
	if err := p.admitLocked(true); err != nil {
		return nil, false, err
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
	}
	s.CommittedBytes = p.committedLocked()
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
	if p.ticker != nil {
		p.ticker.Stop()
	}
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

	// p.closed is now true, and every sweep checks it under p.mu before sending on
	// reclaimQ (see sweep) — so no sweep can still be attempting a send here, and
	// closing the queue cannot race one. This drains the reclaim goroutine
	// deterministically: Close does not return with a destroy still running on it,
	// even though it does not (and never did — see acquire) wait for an in-flight
	// cold warm.
	close(p.reclaimQ)
	p.reclaimDone.Wait()
	return firstErr
}
