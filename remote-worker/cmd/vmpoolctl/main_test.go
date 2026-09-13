package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagenti/serverless-harness/remote-worker/internal/vmpool"
)

// testPerVMBytes stands in for the real vmpool.PerVMBytes(cfg) figure realMain
// computes — any positive value works here, since this test does not assert the
// SPECIFIC memory bound reaches the launcher (that agreement is asserted at the
// vmpool package level). What matters is only that it is > 0, so
// CHVOptions.validate() does not refuse the ParentCgroup default
// vmpool.LauncherFromEnv always sets (hardware-corrections D1). Mirrors
// cmd/microvm-worker/main_test.go's testPerVMBytes exactly.
const testPerVMBytes = int64(256 << 20)

// Fix round 9: launcher's CloudHypervisor case must actually route to
// vmpool.LauncherFromEnv / NewCloudHypervisorLauncher. Before this, the case was a
// hardcoded "not wired yet (Phase D)" error — the second occurrence of the exact
// defect round 3 fixed in cmd/microvm-worker/main.go's launcherFor, because that
// fix never propagated into this file's own copy of the switch. Mirrors
// TestLauncherForWiresCloudHypervisor's shape exactly.
func TestLauncherWiresCloudHypervisor(t *testing.T) {
	lc, err := launcher(string(vmpool.CloudHypervisor), t.TempDir(), testPerVMBytes)
	if err != nil {
		t.Fatalf("launcher(cloud-hypervisor): %v", err)
	}
	if lc.Kind() != vmpool.CloudHypervisor {
		t.Fatalf("Kind() = %v, want %v", lc.Kind(), vmpool.CloudHypervisor)
	}
	if lc.SerializesExecsPerRun() {
		t.Fatal("the Cloud Hypervisor arm must not serialize execs per run (spec §4.3): " +
			"virtio-fs makes the host filesystem, not a guest-owned block device, the " +
			"concurrency authority")
	}
}

// run invokes the CLI's real entry point in-process, so the test exercises flag
// parsing and the JSON contract E10's shell driver depends on — not a re-implementation
// of them.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := realMain(args, &out)
	return out.String(), err
}

func TestRunsOneExecInAVMAndReportsItAsJSON(t *testing.T) {
	dir := t.TempDir()
	out, err := run(t,
		"--vmm=fake", "--snapshot-dir="+dir, "--workspace-root="+dir,
		"--key=run-a", "--iterations=3", "--json", "--", "echo hello")
	if err != nil {
		t.Fatalf("realMain: %v (out=%s)", err, out)
	}
	var rec runResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
		t.Fatalf("output is not one JSON record: %v\n%s", err, out)
	}
	if rec.Iterations != 3 || rec.Failures != 0 {
		t.Fatalf("record = %+v, want 3 iterations and 0 failures", rec)
	}
	// The fields E10 rung 2 and 3 are built on. Missing any of them makes a rung
	// unreportable, so they are asserted rather than assumed.
	if rec.WarmAcquires+sumUint(rec.ColdAcquires) != 3 {
		t.Errorf("acquires = %d warm + %v cold, want 3 total", rec.WarmAcquires, rec.ColdAcquires)
	}
	if rec.P50AcquireUs == 0 || rec.P50RunUs == 0 || rec.P50DestroyUs == 0 {
		t.Errorf("record = %+v, want the hot path decomposed into acquire/run/destroy", rec)
	}
	// Resume is a term too — on the Firecracker arm it performs the workspace
	// mount, exactly a cost this benchmark exists to expose. The fake's Resume is
	// a no-op so it may legitimately measure ~0us; what must hold is that the
	// field is present in the contract, not that it's nonzero.
	if !strings.Contains(out, `"p50_resume_us"`) || !strings.Contains(out, `"p95_resume_us"`) {
		t.Errorf("record missing resume phase fields, out=%s", out)
	}
	if rec.VMM != "fake" {
		t.Errorf("VMM = %q, want fake — spec §6 requires the substrate recorded in every run record", rec.VMM)
	}
}

// TestAnUnquotedMultiWordCommandIsNotSilentlyTruncated guards against the worst
// defect this CLI could have: silently measuring a different, possibly no-op
// command while reporting a clean result. "-- echo hello world > out.txt" must run
// in full, not just "echo" — checked here by an observable side effect (a file
// written into the workspace), not just the exit status, because the broken
// version of this code also exits 0 with plausible timings.
func TestAnUnquotedMultiWordCommandIsNotSilentlyTruncated(t *testing.T) {
	dir := t.TempDir()
	out, err := run(t,
		"--vmm=fake", "--snapshot-dir="+dir, "--workspace-root="+dir,
		"--key=run-b", "--iterations=1", "--", "echo", "hello", "world", ">", "out.txt")
	if err != nil {
		t.Fatalf("realMain: %v (out=%s)", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "run-b", "out.txt"))
	if err != nil {
		t.Fatalf("workspace file missing — the command was truncated to its first word: %v", err)
	}
	if want := "hello world"; strings.TrimSpace(string(got)) != want {
		t.Fatalf("out.txt = %q, want %q", got, want)
	}
}

func TestRefusesAnEmptyKey(t *testing.T) {
	dir := t.TempDir()
	_, err := run(t, "--vmm=fake", "--snapshot-dir="+dir, "--workspace-root="+dir, "--key=", "--", "true")
	if err == nil {
		t.Fatal("realMain accepted an empty --key")
	}
	if !strings.Contains(err.Error(), "empty-workspace-key") {
		t.Fatalf("err = %v, want the empty-workspace-key refusal (spec §3.4)", err)
	}
}

func TestRefusesAnUnknownVMM(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, "--vmm=qemu", "--snapshot-dir="+dir, "--workspace-root="+dir, "--key=k", "--", "true"); err == nil {
		t.Fatal("realMain accepted --vmm=qemu")
	}
}

func sumUint(m map[string]uint64) uint64 {
	var n uint64
	for _, v := range m {
		n += v
	}
	return n
}
