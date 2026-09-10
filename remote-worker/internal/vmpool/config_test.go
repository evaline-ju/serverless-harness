package vmpool

import (
	"strings"
	"testing"
	"time"
)

// good returns the minimum viable Config: the four fields that have no sane
// default because guessing them wrong is a security or capacity bug.
func good() Config {
	return Config{
		VMM:               Firecracker,
		SnapshotDir:       "/srv/snapshots",
		WorkspaceRoot:     "/srv/workspaces",
		MaxRuns:           64,
		MaxCommittedBytes: 32 << 30,
	}
}

func TestNormalizeAppliesSpecDefaults(t *testing.T) {
	c := good()
	if err := c.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if c.StandbyDepth != DefaultStandbyDepth {
		t.Errorf("StandbyDepth = %d, want %d", c.StandbyDepth, DefaultStandbyDepth)
	}
	if c.GuestRAMBytes != DefaultGuestRAMBytes {
		t.Errorf("GuestRAMBytes = %d, want %d", c.GuestRAMBytes, DefaultGuestRAMBytes)
	}
	if c.StandbyIdle != DefaultStandbyIdle {
		t.Errorf("StandbyIdle = %v, want %v", c.StandbyIdle, DefaultStandbyIdle)
	}
	if c.WorkspaceIdle != DefaultWorkspaceIdle {
		t.Errorf("WorkspaceIdle = %v, want %v", c.WorkspaceIdle, DefaultWorkspaceIdle)
	}
	if c.ReplenishDelay != DefaultReplenishDelay {
		t.Errorf("ReplenishDelay = %v, want %v", c.ReplenishDelay, DefaultReplenishDelay)
	}
	if c.MaxReclaimsPerScan != DefaultMaxReclaimsPerScan {
		t.Errorf("MaxReclaimsPerScan = %d, want %d", c.MaxReclaimsPerScan, DefaultMaxReclaimsPerScan)
	}
	// Spec §4.1: "default StandbyIdle/4" — derived, not a separate constant, so the
	// two cannot drift when StandbyIdle is tuned.
	if want := c.StandbyIdle / 4; c.ReclaimScanInterval != want {
		t.Errorf("ReclaimScanInterval = %v, want StandbyIdle/4 = %v", c.ReclaimScanInterval, want)
	}
}

func TestNormalizeAcceptsFakeVMM(t *testing.T) {
	// FakeVMM is a distinct sentinel, not an alias for a real arm — vmpoolctl is
	// the only caller allowed to select it, but Normalize itself must accept it
	// so that caller can build a valid Config.
	c := good()
	c.VMM = FakeVMM
	if err := c.Normalize(); err != nil {
		t.Fatalf("Normalize rejected FakeVMM: %v", err)
	}
	if c.VMM != FakeVMM {
		t.Fatalf("VMM = %q, want %q", c.VMM, FakeVMM)
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"no snapshot dir", func(c *Config) { c.SnapshotDir = "" }, "SnapshotDir"},
		{"no workspace root", func(c *Config) { c.WorkspaceRoot = "" }, "WorkspaceRoot"},
		{"unknown vmm", func(c *Config) { c.VMM = "qemu" }, "VMM"},
		{"no memory budget", func(c *Config) { c.MaxCommittedBytes = 0 }, "MaxCommittedBytes"},
		{"reserve swallows budget", func(c *Config) { c.MemoryReserveBytes = c.MaxCommittedBytes }, "MemoryReserveBytes"},
		{"no run cap", func(c *Config) { c.MaxRuns = 0 }, "MaxRuns"},
		{"RAM outlives disk", func(c *Config) { c.StandbyIdle = time.Hour; c.WorkspaceIdle = time.Minute }, "StandbyIdle"},
		{"scan cannot converge", func(c *Config) { c.ReclaimScanInterval = 10 * time.Minute }, "ReclaimScanInterval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			tc.mut(&c)
			err := c.Normalize()
			if err == nil {
				t.Fatalf("Normalize accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}
