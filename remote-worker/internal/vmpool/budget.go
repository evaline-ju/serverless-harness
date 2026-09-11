package vmpool

// admitLocked decides whether one more VM may be committed. Caller holds p.mu.
//
// TWO CEILINGS, RECORDED DISTINCTLY. Spec §6: "A memory-driven refusal is recorded
// distinctly from a MaxRuns refusal, or the two ceilings get conflated and neither
// is diagnosable." E11 reports ExecErrors by cause for exactly this reason (§7.3).
//
// MaxRuns IS A BACKSTOP, not primary admission control. There is no "busy" frame in
// the wire contract, so a MaxRuns refusal reaches the harness as a failed exec
// rather than as back-pressure — while the harness already has the right mechanism
// one tier up: SandboxPoolSaturatedError at lease time under KAGENTI_SANDBOX_CAP.
// The consistency requirement is therefore
//
//	MaxRuns >= cap x (records this worker advertises)
//
// and violating it refuses work the harness believed it had capacity for, which is
// P6 §3.9's spurious-429s-truncate-the-rungs failure one tier down.
//
// The memory gate exists so spec §7.4's prediction 1 ("replenishment binds on
// process/memory count before CPU") is observable AS BACK-PRESSURE. Without it the
// prediction would be "confirmed" by the host falling over, which is not a
// measurement.
func (p *pool) admitLocked(newRun bool) error {
	if newRun && len(p.runs) >= p.cfg.MaxRuns {
		return refusal(RefuseMaxRuns,
			"MaxRuns=%d reached with %d active runs; the lease cap one tier up is primary "+
				"admission control and this is its backstop (spec §6)", p.cfg.MaxRuns, len(p.runs))
	}
	committed := p.committedLocked()
	budget := p.cfg.MaxCommittedBytes - p.cfg.MemoryReserveBytes
	if next := committed + p.perVMBytes(); next > budget {
		return refusal(RefuseMemoryBudget,
			"committing %d more bytes would reach %d against a budget of %d "+
				"(MaxCommittedBytes %d less MemoryReserveBytes %d)",
			p.perVMBytes(), next, budget, p.cfg.MaxCommittedBytes, p.cfg.MemoryReserveBytes)
	}
	return nil
}

// committedLocked is every VM this host is holding, in bytes. Caller holds p.mu.
//
// Standbys are charged their FULL guest RAM even though they are paused and their
// memory files are CoW-shared, because a standby is one Resume away from consuming
// all of it. Admission control that charged the paused footprint would admit a host
// it cannot then run — the opposite of the PSS-vs-RSS error in spec §7.3, and in the
// dangerous direction rather than the pessimistic one.
func (p *pool) committedLocked() int64 {
	var vms int64
	for _, rp := range p.runs {
		vms += int64(len(rp.ready) + rp.inFlight + rp.warming)
	}
	return vms * p.perVMBytes()
}

// countRefusal records an already-built refusal and returns it unchanged, so the
// construction site keeps the detailed message and the counting stays in one place.
func (p *pool) countRefusal(err error) error {
	if r := ReasonOf(err); r != "" {
		p.counters.refuse(r)
	}
	return err
}
