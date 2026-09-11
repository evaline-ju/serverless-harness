package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

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
	if rec.VMM != "fake" {
		t.Errorf("VMM = %q, want fake — spec §6 requires the substrate recorded in every run record", rec.VMM)
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
