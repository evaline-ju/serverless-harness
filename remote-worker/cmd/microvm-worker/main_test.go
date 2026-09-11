package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kagenti/serverless-harness/remote-worker/internal/vmpool"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestPoolConfigFromEnvironment(t *testing.T) {
	cfg, err := poolConfig(envFrom(map[string]string{
		"SH_VMM":               "firecracker",
		"SH_SNAPSHOT_DIR":      "/srv/snapshots",
		"SH_WORKSPACE_ROOT":    "/srv/workspaces",
		"SH_STANDBY_DEPTH":     "3",
		"SH_GUEST_RAM_MB":      "512",
		"SH_MAX_RUNS":          "48",
		"SH_MAX_COMMITTED_MB":  "20480",
		"SH_MEMORY_RESERVE_MB": "2048",
	}))
	if err != nil {
		t.Fatalf("poolConfig: %v", err)
	}
	if cfg.VMM != vmpool.Firecracker || cfg.StandbyDepth != 3 || cfg.GuestRAMBytes != 512<<20 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.MaxRuns != 48 || cfg.MaxCommittedBytes != 20480<<20 || cfg.MemoryReserveBytes != 2048<<20 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// Spec §6: "KVM unavailable at startup ... Fail the unit at start with an explicit
// message; never fall back to running commands on the host." The strongest form of
// that is having no code path that could: --vmm=fake exists in vmpoolctl and must
// not be reachable here.
func TestThereIsNoHostFallbackLauncher(t *testing.T) {
	get := envFrom(map[string]string{})
	if _, err := launcherFor(vmpool.VMMKind("fake"), get, t.TempDir()); err == nil {
		t.Fatal("launcherFor accepted the host-bash fake — §3.5's privilege argument rests on " +
			"nothing agent-influenced ever executing outside a VM")
	}
	for _, k := range []vmpool.VMMKind{"", "qemu", "gvisor"} {
		if _, err := launcherFor(k, get, t.TempDir()); err == nil {
			t.Fatalf("launcherFor(%q) accepted an unknown VMM", k)
		}
	}
}

func TestPoolConfigRefusesAMissingSnapshotDir(t *testing.T) {
	_, err := poolConfig(envFrom(map[string]string{
		"SH_VMM":              "firecracker",
		"SH_WORKSPACE_ROOT":   "/srv/workspaces",
		"SH_MAX_COMMITTED_MB": "1024",
	}))
	if err == nil || !strings.Contains(err.Error(), "SH_SNAPSHOT_DIR") {
		t.Fatalf("err = %v, want it to name SH_SNAPSHOT_DIR", err)
	}
}

func TestPoolConfigRefusesAMissingMemoryBudget(t *testing.T) {
	_, err := poolConfig(envFrom(map[string]string{
		"SH_VMM":            "firecracker",
		"SH_SNAPSHOT_DIR":   "/srv/snapshots",
		"SH_WORKSPACE_ROOT": "/srv/workspaces",
	}))
	// Spec §6 makes the memory gate mandatory: without it pressure goes straight to
	// the OOM killer, whose size-ranked favourites include microvm-worker itself.
	if err == nil || !strings.Contains(err.Error(), "SH_MAX_COMMITTED_MB") {
		t.Fatalf("err = %v, want it to name SH_MAX_COMMITTED_MB", err)
	}
}

// Fix-round item 5: the manifest's InstanceType must be verified against the
// running host, additively alongside the existing verify+pin+probe block.

func fixedHost(t string) func(context.Context) string {
	return func(context.Context) string { return t }
}

func TestVerifyInstanceTypeAcceptsAMatch(t *testing.T) {
	get := envFrom(map[string]string{})
	if err := verifyInstanceType(get, "c6i.large", fixedHost("c6i.large")); err != nil {
		t.Fatalf("matching instance types should not be refused: %v", err)
	}
}

func TestVerifyInstanceTypeRefusesAMismatchNamingBothValues(t *testing.T) {
	get := envFrom(map[string]string{})
	err := verifyInstanceType(get, "c6i.large", fixedHost("m5.xlarge"))
	if err == nil {
		t.Fatal("a mismatched instance type must be refused")
	}
	if !strings.Contains(err.Error(), "c6i.large") || !strings.Contains(err.Error(), "m5.xlarge") {
		t.Fatalf("err = %v, want it to name both the manifest and host instance types", err)
	}
}

func TestVerifyInstanceTypeOverrideEnvBypassesAMismatch(t *testing.T) {
	get := envFrom(map[string]string{"SH_ALLOW_INSTANCE_TYPE_MISMATCH": "true"})
	if err := verifyInstanceType(get, "c6i.large", fixedHost("m5.xlarge")); err != nil {
		t.Fatalf("the override env var should bypass a mismatch: %v", err)
	}
}

func TestVerifyInstanceTypeSkipsWhenManifestHasNoRecordedType(t *testing.T) {
	get := envFrom(map[string]string{})
	// An older manifest with no InstanceType recorded: nothing to compare, so this
	// must fail open, not closed.
	if err := verifyInstanceType(get, "", fixedHost("m5.xlarge")); err != nil {
		t.Fatalf("an empty manifest InstanceType must not be refused: %v", err)
	}
}

func TestVerifyInstanceTypeSkipsWhenHostIsUndetectable(t *testing.T) {
	get := envFrom(map[string]string{})
	// No metadata service and no stable host identity available: fail open rather
	// than block every worker's startup on an environment this check cannot reach.
	if err := verifyInstanceType(get, "c6i.large", fixedHost("")); err != nil {
		t.Fatalf("an undetectable host must not be refused: %v", err)
	}
}

// stableHostIdentity itself is exercised only for "never returns empty" -- the
// metadata-service probes need real network access this test suite must not
// depend on, and are covered by design (fail-open on every non-2xx/timeout/error)
// rather than by hitting real cloud endpoints from a unit test.
func TestStableHostIdentityNeverReturnsEmpty(t *testing.T) {
	if got := stableHostIdentity(); got == "" {
		t.Fatal("stableHostIdentity must always return a non-empty fallback")
	}
}
