package vmpool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSnapshot(t *testing.T, dir string, mutate func(*Manifest)) string {
	t.Helper()
	img := filepath.Join(dir, "swebench-py311")
	if err := os.MkdirAll(img, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"vmstate": "state-bytes",
		"memfile": "memory-bytes",
		"kernel":  "vmlinux-bytes",
		"rootfs":  "rootfs-bytes",
		"agent":   "agent-bytes",
	} {
		if err := os.WriteFile(filepath.Join(img, name), []byte(body), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	m := Manifest{Image: "swebench-py311", VMM: string(Firecracker), InstanceType: "c8i.metal-48xl", GuestRAMMB: 256}
	if err := m.Fill(img); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if mutate != nil {
		mutate(&m)
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(img, "manifest.json"), b, 0o444); err != nil {
		t.Fatal(err)
	}
	return img
}

func TestManifestVerifiesAGoodSnapshot(t *testing.T) {
	img := writeSnapshot(t, t.TempDir(), nil)
	m, err := LoadManifest(img)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if err := m.Verify(img); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if m.Hash == "" || !strings.HasPrefix(m.Hash, "sha256:") {
		t.Fatalf("Hash = %q", m.Hash)
	}
}

func TestManifestFailsLoudlyOnAStaleRootfs(t *testing.T) {
	dir := t.TempDir()
	img := writeSnapshot(t, dir, nil)
	// Someone rebuilt the rootfs and forgot the snapshot. Spec §5.5: a stale snapshot
	// must fail LOUDLY rather than silently serving an old toolchain — which would show
	// up as a workload failing for reasons nothing in the run record explains.
	//
	// writeSnapshot left this file 0o444 (read-only, as a real locked-down snapshot
	// is), so it must be made writable again before it can be overwritten in place —
	// os.WriteFile's mode argument only applies when it CREATES the file, not when it
	// truncates an existing one, and a 0o444 file has no write bit for anyone.
	if err := os.Chmod(filepath.Join(img, "rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(img, "rootfs"), []byte("new-toolchain"), 0o444); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(img)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	err = m.Verify(img)
	if err == nil {
		t.Fatal("Verify accepted a snapshot whose rootfs had changed")
	}
	if !strings.Contains(err.Error(), "rootfs") {
		t.Fatalf("err = %v, want it to name which component drifted", err)
	}
}

func TestManifestRejectsATamperedHash(t *testing.T) {
	img := writeSnapshot(t, t.TempDir(), func(m *Manifest) { m.Hash = "sha256:0000" })
	m, err := LoadManifest(img)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	// Only a 64-bit CRC guards the state file and the VMM TRUSTS these files (spec
	// §2.4), so integrity is checked here, outside the VMM.
	if err := m.Verify(img); err == nil {
		t.Fatal("Verify accepted a manifest whose own hash did not match its components")
	}
}

func TestManifestRequiresTheSnapshotFilesToExist(t *testing.T) {
	img := writeSnapshot(t, t.TempDir(), nil)
	if err := os.Remove(filepath.Join(img, "memfile")); err != nil {
		t.Fatal(err)
	}
	m, _ := LoadManifest(img)
	if err := m.Verify(img); err == nil || !strings.Contains(err.Error(), "memfile") {
		t.Fatalf("err = %v, want it to name the missing memfile", err)
	}
}

func TestProbeRestoresOneVMAndDestroysIt(t *testing.T) {
	p, lc, _ := testPool(t)
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if lc.createdCount() != 1 || lc.liveCount() != 0 {
		t.Fatalf("created=%d live=%d, want 1 and 0", lc.createdCount(), lc.liveCount())
	}
	// The probe must leave NO trace in the pool: a reserved key that lingered would
	// count against MaxRuns and show up in every Stats scrape as a phantom active run.
	if s := p.Stats(); s.ActiveRuns != 0 || s.StandbysResident != 0 {
		t.Fatalf("Stats = %+v after Probe, want an empty pool", s)
	}
}

func TestProbeFailsWhenNoVMCanBeRestored(t *testing.T) {
	p, lc, _ := testPool(t)
	lc.setRestoreErr(errNoKVM)
	// Spec §6: fail at start, not on a user's first request.
	if err := p.Probe(context.Background()); err == nil {
		t.Fatal("Probe succeeded with a launcher that cannot restore")
	}
}

var errNoKVM = os.ErrPermission
