package vmpool

import "context"

// BenchmarkHooks exposes the individual lifecycle primitives to vmpoolctl so E10 can
// price them as TERMS rather than as one round trip (spec §7.2: "Its rungs are terms,
// not concurrency"). It is deliberately not part of Pool: nothing in the request path
// may acquire a VM without the destroy defer that Exec provides.
type BenchmarkHooks interface {
	RestoreOne(ctx context.Context, key string) (VM, error)
	FillStandbys(ctx context.Context, key string, n int) error
	DestroyAllStandbys(ctx context.Context) (int, error)
}

var _ BenchmarkHooks = (*pool)(nil)

// RestoreOne restores exactly one VM bound to key and returns it directly to the
// caller — E10 rung 3 (replenishment cost) and rung 4's teardown variants need the
// bare Restore call timed on its own, with none of ExecPhased's acquire bookkeeping
// (warm/cold accounting, standby pool membership) folded in, because that bookkeeping
// is not what a replenishment rung is pricing. The caller owns the returned VM and
// must destroy it; this is a diagnostic entry point, not a path Exec can reach, so it
// carries none of Exec's destroy-defer guarantee.
func (p *pool) RestoreOne(ctx context.Context, key string) (VM, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	dir, err := p.workspaceDir(key)
	if err != nil {
		return nil, err
	}
	if err := ensureWorkspace(dir); err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	id := p.nextIDLocked()
	p.mu.Unlock()
	return p.warm(ctx, key, dir, id)
}

// FillStandbys restores n VMs and parks them as key's standbys, mirroring the
// warming bookkeeping acquire uses (rp.warming / rp.settled) so a concurrent Exec on
// the same key during a fill sees a consistent picture rather than an under-counted
// warming figure. E10 rung 4's teardown-bulk variant uses this to build the D
// standbys it then reclaims in one DestroyAllStandbys call, pricing "the bulk reclaim
// the sweep actually performs" rather than one destroy at a time.
func (p *pool) FillStandbys(ctx context.Context, key string, n int) error {
	if err := checkKey(key); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return ErrClosed
		}
		rp, _, err := p.runLocked(key)
		if err != nil {
			p.mu.Unlock()
			return err
		}
		dir, id := rp.dir, p.nextIDLocked()
		rp.warming++
		p.mu.Unlock()

		vm, warmErr := p.warm(ctx, key, dir, id)

		p.mu.Lock()
		rp.warming--
		rp.signalSettledLocked()
		if warmErr != nil {
			p.mu.Unlock()
			return warmErr
		}
		rp.ready = append(rp.ready, vm)
		rp.lastExec = p.clk.Now()
		p.mu.Unlock()
	}
	return nil
}

// DestroyAllStandbys tears down every paused standby across every run pool in one
// batch — E10 rung 4's teardown-bulk variant, because "the per-VM number does not
// predict" the sweep's bulk reclaim (spec §7.2). It mirrors Close's own
// collect-under-lock-then-destroy-outside-it pattern: the destroys themselves must
// never happen while p.mu is held, since VM.Destroy blocks on real teardown work. A
// destroy failure is counted (mirroring pool.destroy) and the first error is
// returned, but every standby is still attempted.
func (p *pool) DestroyAllStandbys(ctx context.Context) (int, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, ErrClosed
	}
	var victims []VM
	for _, rp := range p.runs {
		victims = append(victims, rp.ready...)
		rp.ready = nil
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
	return len(victims), firstErr
}
