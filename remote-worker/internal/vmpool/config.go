// Package vmpool owns microVM lifecycle for the per-Exec sandbox tier: a golden
// snapshot restored into a paused standby, acquired for exactly ONE command, and
// destroyed afterwards. A VM is never reused.
//
// See docs/specs/2026-09-09-p4-microvm-sandbox-design.md §4. The load-bearing
// decision is §3.2's: replenishment costs 10-25ms and cannot sit inside a request
// with a 15ms budget, so it happens in the gap the model's own latency provides.
package vmpool

import (
	"fmt"
	"time"
)

// VMMKind selects the hypervisor. It is a seam rather than a build choice because
// spec §7.2 requires E10 to price both arms with the same code above them.
type VMMKind string

const (
	CloudHypervisor VMMKind = "cloud-hypervisor"
	Firecracker     VMMKind = "firecracker"

	// FakeVMM is a sentinel selected only by the vmpoolctl benchmark CLI, which
	// runs commands with host bash rather than in a VM. The production worker
	// binary has no code path that can select it: spec §3.5's privilege argument
	// rests on nothing agent-influenced ever executing outside a VM. FakeVMM is
	// deliberately a DISTINCT value rather than an alias for a real arm, so that
	// a Config naming a real hypervisor can never be served by the fake launcher.
	FakeVMM VMMKind = "fake"
)

// Defaults, from spec §4.1. E11 holds StandbyIdle, WorkspaceIdle, ReplenishDelay
// and ReclaimScanInterval at these values and RECORDS them rather than sweeping
// them (spec §7.3), so changing one changes what a published rung means.
const (
	DefaultStandbyDepth       = 2
	DefaultGuestRAMBytes      = 256 << 20
	DefaultStandbyIdle        = 90 * time.Second
	DefaultWorkspaceIdle      = 30 * time.Minute
	DefaultReplenishDelay     = 200 * time.Millisecond
	DefaultMaxReclaimsPerScan = 8
	// DefaultVMOverheadBytes is the per-VM cost OUTSIDE guest RAM: the VMM's own
	// anonymous memory, plus virtiofsd's where it runs. Charged to the budget so
	// MaxCommittedBytes bounds what the host actually commits — spec §7.3's
	// "VMs x VMM/virtiofsd overhead" term, which its sketch puts at ~2 GiB of 21.
	DefaultVMOverheadBytes = 32 << 20
)

// Config is the pool's whole configuration surface (spec §4.1).
type Config struct {
	VMM           VMMKind // the seam E10 prices
	SnapshotDir   string  // golden snapshots; vmtouch -dl'd at start
	WorkspaceRoot string  // per-run workspaces

	StandbyDepth  int   // D
	GuestRAMBytes int64 // the dominant density term (spec §7.3)
	MaxRuns       int   // concurrent workspace_keys; BACKSTOP only, see below

	// MaxCommittedBytes and MemoryReserveBytes gate admission on a computable
	// budget. MaxRuns alone cannot: CoW growth is workload-dependent (spec §6).
	MaxCommittedBytes  int64
	MemoryReserveBytes int64
	VMOverheadBytes    int64

	StandbyIdle         time.Duration // no Exec for this long -> drop standbys, KEEP workspace
	WorkspaceIdle       time.Duration // no Exec for this long -> delete the workspace too
	ReplenishDelay      time.Duration // grace before refilling a popped slot
	ReclaimScanInterval time.Duration // idle-host sweep tick
	MaxReclaimsPerScan  int           // VMs destroyed per sweep; rate-limits munmap
}

// Normalize fills defaults and then validates, returning the first violation.
//
// It validates rather than clamping because every rule below is a case where a
// wrong value fails silently in production: a zero MaxCommittedBytes turns spec
// §7.4's prediction 1 into "confirmed by the host falling over", which is not a
// measurement, and an inverted idle pair pins RAM while deleting the cheap thing.
func (c *Config) Normalize() error {
	if c.StandbyDepth <= 0 {
		c.StandbyDepth = DefaultStandbyDepth
	}
	if c.GuestRAMBytes <= 0 {
		c.GuestRAMBytes = DefaultGuestRAMBytes
	}
	if c.VMOverheadBytes <= 0 {
		c.VMOverheadBytes = DefaultVMOverheadBytes
	}
	if c.StandbyIdle <= 0 {
		c.StandbyIdle = DefaultStandbyIdle
	}
	if c.WorkspaceIdle <= 0 {
		c.WorkspaceIdle = DefaultWorkspaceIdle
	}
	if c.ReplenishDelay <= 0 {
		c.ReplenishDelay = DefaultReplenishDelay
	}
	if c.MaxReclaimsPerScan <= 0 {
		c.MaxReclaimsPerScan = DefaultMaxReclaimsPerScan
	}
	// Derived, not a constant of its own: spec §4.1 says StandbyIdle/4, and a
	// separate default would silently stop tracking a tuned StandbyIdle.
	if c.ReclaimScanInterval <= 0 {
		c.ReclaimScanInterval = c.StandbyIdle / 4
	}

	switch c.VMM {
	case CloudHypervisor, Firecracker, FakeVMM:
	default:
		return fmt.Errorf("vmpool: VMM %q is not one of %q, %q, %q", c.VMM, CloudHypervisor, Firecracker, FakeVMM)
	}
	if c.SnapshotDir == "" {
		return fmt.Errorf("vmpool: SnapshotDir is required (the golden snapshot has no default location)")
	}
	if c.WorkspaceRoot == "" {
		return fmt.Errorf("vmpool: WorkspaceRoot is required (per-run workspaces have no default location)")
	}
	if c.MaxRuns <= 0 {
		return fmt.Errorf("vmpool: MaxRuns must be > 0 (spec §6: the backstop behind the lease cap)")
	}
	if c.MaxCommittedBytes <= 0 {
		return fmt.Errorf("vmpool: MaxCommittedBytes must be > 0 — without the memory gate, " +
			"pressure goes straight to the OOM killer and spec §7.4's prediction 1 is " +
			"'confirmed' by the host falling over (spec §6)")
	}
	if c.MemoryReserveBytes >= c.MaxCommittedBytes {
		return fmt.Errorf("vmpool: MemoryReserveBytes (%d) >= MaxCommittedBytes (%d): nothing could ever be admitted",
			c.MemoryReserveBytes, c.MaxCommittedBytes)
	}
	if c.StandbyIdle >= c.WorkspaceIdle {
		return fmt.Errorf("vmpool: StandbyIdle (%v) must be < WorkspaceIdle (%v): RAM is urgent and disk is not (spec §4.4)",
			c.StandbyIdle, c.WorkspaceIdle)
	}
	if c.ReclaimScanInterval > c.StandbyIdle {
		return fmt.Errorf("vmpool: ReclaimScanInterval (%v) > StandbyIdle (%v): the sweep could not converge "+
			"within spec §8's StandbyIdle+ReclaimScanInterval leak bound", c.ReclaimScanInterval, c.StandbyIdle)
	}
	return nil
}

// ReclaimScanIntervalForTest exposes the derived interval. Normalize runs inside
// New, so a test that wants to advance exactly one tick cannot compute it from the
// Config it passed in.
func (c Config) ReclaimScanIntervalForTest() time.Duration {
	if c.ReclaimScanInterval > 0 {
		return c.ReclaimScanInterval
	}
	if c.StandbyIdle > 0 {
		return c.StandbyIdle / 4
	}
	return DefaultStandbyIdle / 4
}
