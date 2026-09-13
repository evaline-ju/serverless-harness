package vmpool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envFrom is this file's own copy of the helper cmd/microvm-worker/main_test.go and
// cmd/vmpoolctl/main_test.go each already keep (unexported, in package main there —
// this package cannot import either), letting a test inject LauncherFromEnv's get
// func(string) string from a plain map instead of the real environment.
func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestChvDefaultRunDirIsNotInsideSnapshotDir is fix round 10's instruction (h): the
// derived default RunDir must be a SIBLING of SnapshotDir, never a CHILD of it —
// SnapshotDir is root-owned 0555 and hash-pinned in the build manifest
// (deploy/microvm/build-snapshot.sh's lock_down), so nothing may create or write
// beneath it. This is pure path-string logic with no device syscall involved, so
// unlike the device-sharing assertions below it needs no honesty caveat about this
// darwin dev machine's single filesystem device: mutating chvDefaultRunDir's
// derivation to filepath.Join(snapshotDir, ...) instead of
// filepath.Join(filepath.Dir(snapshotDir), ...) makes this fail immediately and for
// real, on any machine.
func TestChvDefaultRunDirIsNotInsideSnapshotDir(t *testing.T) {
	parent := t.TempDir()
	snapshotDir := filepath.Join(parent, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0o555); err != nil {
		t.Fatalf("MkdirAll snapshotDir: %v", err)
	}

	dir, createdHere, err := chvDefaultRunDir(snapshotDir, "test-not-inside")
	if err != nil {
		t.Fatalf("chvDefaultRunDir: %v", err)
	}
	t.Cleanup(func() {
		if createdHere {
			_ = os.RemoveAll(dir)
		}
	})

	clean := filepath.Clean(snapshotDir)
	if dir == clean || strings.HasPrefix(dir, clean+string(filepath.Separator)) {
		t.Fatalf("chvDefaultRunDir(%s) = %s, which is inside SnapshotDir — it must be a "+
			"sibling, not a child: SnapshotDir is root-owned read-only and hash-pinned",
			snapshotDir, dir)
	}
	if got, want := filepath.Dir(dir), filepath.Dir(snapshotDir); got != want {
		t.Fatalf("chvDefaultRunDir(%s) = %s, whose parent is %s; want parent %s (a sibling of "+
			"SnapshotDir, sharing its parent directory)", snapshotDir, dir, got, want)
	}
}

// TestChvDefaultRunDirSharesDeviceWithSnapshotDir mirrors
// TestSameDeviceSiblingDirSharesDeviceWithTarget (launcher_firecracker_test.go) for
// the CloudHypervisor arm's own default derivation: on a real filesystem, the
// derived RunDir must land on the same device as SnapshotDir, since Restore
// hardlinks the golden snapshot's files into it and hardlink(2) is EXDEV across a
// device boundary. Honesty caveat, same as that precedent's own doc comment: this
// repo's darwin dev machine has exactly one filesystem device
// (TestSameDeviceSiblingDirSharesDeviceWithTarget's history), so this test can only
// confirm the MATCH case for real — it cannot exhibit a genuine cross-device
// mismatch by path choice alone. TestChvDefaultRunDirSelfCheckCatchesDeviceMismatch
// below covers the mismatch branch through the withFakeDevices seam instead.
func TestChvDefaultRunDirSharesDeviceWithTarget(t *testing.T) {
	parent := t.TempDir()
	snapshotDir := filepath.Join(parent, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("MkdirAll snapshotDir: %v", err)
	}

	dir, createdHere, err := chvDefaultRunDir(snapshotDir, "test-shares-device")
	if err != nil {
		t.Fatalf("chvDefaultRunDir: %v", err)
	}
	t.Cleanup(func() {
		if createdHere {
			_ = os.RemoveAll(dir)
		}
	})

	wantDev := deviceOf(t, snapshotDir)
	gotDev := deviceOf(t, dir)
	if gotDev != wantDev {
		t.Fatalf("chvDefaultRunDir(%s) = %s, on device %d; want device %d (same as SnapshotDir) "+
			"— a hardlink from the snapshot dir into this directory would be cross-device and "+
			"therefore always fail EXDEV", snapshotDir, dir, gotDev, wantDev)
	}
}

// TestChvDefaultRunDirSelfCheckCatchesDeviceMismatch forces chvDefaultRunDir's own
// internal checkPathsShareDevice call to fail, through the withFakeDevices seam
// devicecheck_test.go already defines for exactly this reason (this dev machine
// cannot produce a real cross-device pair of paths). Unlike the darwin single-device
// caveat on the test above, this IS a genuine catch: it exercises the real
// production comparison and error path inside chvDefaultRunDir, with only the
// source of the two device numbers faked — and it additionally confirms the
// createdHere cleanup contract: since this call created the directory, a failing
// self-check must remove it again rather than leaving an empty, unusable directory
// behind.
func TestChvDefaultRunDirSelfCheckCatchesDeviceMismatch(t *testing.T) {
	parent := t.TempDir()
	snapshotDir := filepath.Join(parent, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("MkdirAll snapshotDir: %v", err)
	}
	wantDir := filepath.Join(parent, ".chv-run-test-mismatch")

	withFakeDevices(t, map[string]uint64{wantDir: 1, snapshotDir: 2})

	dir, createdHere, err := chvDefaultRunDir(snapshotDir, "test-mismatch")
	if err == nil {
		t.Fatalf("chvDefaultRunDir with faked mismatched devices: got nil error, dir=%s createdHere=%v", dir, createdHere)
	}
	if !strings.Contains(err.Error(), "must all be on the same filesystem device") {
		t.Fatalf("chvDefaultRunDir error = %q, want it to name the device mismatch (from checkPathsShareDevice)", err)
	}
	if dir != "" || createdHere {
		t.Fatalf("chvDefaultRunDir on failure: dir=%q createdHere=%v, want \"\"/false", dir, createdHere)
	}
	if _, statErr := os.Stat(wantDir); !os.IsNotExist(statErr) {
		t.Fatalf("chvDefaultRunDir left %s behind after its own device self-check failed "+
			"(it created this directory, so it must remove it again on failure): stat err = %v",
			wantDir, statErr)
	}
}

// TestChvDefaultRunDirReusesAnExistingDirectory confirms createdHere is false, and no
// error occurs, on a second call against a directory the first call already created
// — the "left over from this host's last run" case chvDefaultRunDir's doc comment
// describes, and the case LauncherFromEnv relies on to decide it is NOT responsible
// for cleaning up a pre-existing directory it did not create.
func TestChvDefaultRunDirReusesAnExistingDirectory(t *testing.T) {
	parent := t.TempDir()
	snapshotDir := filepath.Join(parent, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("MkdirAll snapshotDir: %v", err)
	}

	dir1, created1, err := chvDefaultRunDir(snapshotDir, "test-reuse")
	if err != nil {
		t.Fatalf("first chvDefaultRunDir: %v", err)
	}
	if !created1 {
		t.Fatalf("first chvDefaultRunDir: createdHere = false, want true (directory did not exist yet)")
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir1) })

	dir2, created2, err := chvDefaultRunDir(snapshotDir, "test-reuse")
	if err != nil {
		t.Fatalf("second chvDefaultRunDir: %v", err)
	}
	if created2 {
		t.Fatalf("second chvDefaultRunDir: createdHere = true, want false (directory already existed)")
	}
	if dir2 != dir1 {
		t.Fatalf("second chvDefaultRunDir = %s, want the same directory as the first call (%s)", dir2, dir1)
	}
}

// TestLauncherFromEnvDerivesRunDirBesideSnapshotDir is LauncherFromEnv's own
// end-to-end coverage of round 10's fix: with SH_CHV_RUN_DIR unset, the launcher it
// builds must carry a RunDir that is a sibling of, and shares a device with,
// snapshotDir — the same two properties chvDefaultRunDir's own tests pin, checked
// here through the public entry point both cmd/microvm-worker/main.go's launcherFor
// and cmd/vmpoolctl/main.go's launcher actually call, so a future change to either
// call site's wiring (not just to chvDefaultRunDir itself) would also be caught.
func TestLauncherFromEnvDerivesRunDirBesideSnapshotDir(t *testing.T) {
	parent := t.TempDir()
	snapshotDir := filepath.Join(parent, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("MkdirAll snapshotDir: %v", err)
	}

	lc, err := LauncherFromEnv(CloudHypervisor, envFrom(nil), snapshotDir, 256<<20, "test-e2e")
	if err != nil {
		t.Fatalf("LauncherFromEnv: %v", err)
	}
	chv, ok := lc.(*chvLauncher)
	if !ok {
		t.Fatalf("LauncherFromEnv(CloudHypervisor) returned %T, want *chvLauncher", lc)
	}
	t.Cleanup(func() { _ = os.RemoveAll(chv.opts.RunDir) })

	clean := filepath.Clean(snapshotDir)
	if chv.opts.RunDir == clean || strings.HasPrefix(chv.opts.RunDir, clean+string(filepath.Separator)) {
		t.Fatalf("LauncherFromEnv derived RunDir %s, which is inside SnapshotDir %s", chv.opts.RunDir, snapshotDir)
	}
	if err := checkPathsShareDevice("test", namedPath{"RunDir", chv.opts.RunDir}, namedPath{"SnapshotDir", snapshotDir}); err != nil {
		t.Fatalf("derived RunDir does not share a device with SnapshotDir: %v", err)
	}
}

// TestLauncherFromEnvRemovesDerivedRunDirOnLaterValidationFailure is round 10's
// instruction (f): since the derived RunDir now lives on persistent storage (a
// sibling of SnapshotDir, not tmpfs), nothing reclaims it on reboot — whatever
// creates it must remove it again on any failure path, including one that happens
// AFTER the directory is created but before LauncherFromEnv returns. perVMBytes=0
// with ParentCgroup's default ("microvm-vms.slice", non-empty) trips
// CHVOptions.validate()'s "ParentCgroup set but CgroupMemoryMaxBytes <= 0" case
// inside NewCloudHypervisorLauncher — a failure downstream of chvDefaultRunDir's own
// (successful) directory creation, exactly the case cleanupRunDir exists for.
func TestLauncherFromEnvRemovesDerivedRunDirOnLaterValidationFailure(t *testing.T) {
	parent := t.TempDir()
	snapshotDir := filepath.Join(parent, "snapshot")
	if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
		t.Fatalf("MkdirAll snapshotDir: %v", err)
	}
	wantDir := filepath.Join(parent, ".chv-run-test-cleanup")

	_, err := LauncherFromEnv(CloudHypervisor, envFrom(nil), snapshotDir, 0, "test-cleanup")
	if err == nil {
		t.Fatalf("LauncherFromEnv with perVMBytes=0: got nil error, want CHVOptions.validate's " +
			"ParentCgroup/CgroupMemoryMaxBytes error")
	}
	if !strings.Contains(err.Error(), "CgroupMemoryMaxBytes") {
		t.Fatalf("LauncherFromEnv error = %q, want the ParentCgroup/CgroupMemoryMaxBytes validation error", err)
	}
	if _, statErr := os.Stat(wantDir); !os.IsNotExist(statErr) {
		t.Fatalf("LauncherFromEnv left %s behind after NewCloudHypervisorLauncher failed validation "+
			"(chvDefaultRunDir created it; LauncherFromEnv's cleanupRunDir must remove it on any "+
			"later failure): stat err = %v", wantDir, statErr)
	}
}

// TestLauncherFromEnvHonoursSHCHVRunDirOverride is instruction (c): SH_CHV_RUN_DIR
// must still fully override the derived default, so a bad operator-supplied value is
// caught by checkDeviceSharing at pool.New (not by this function silently ignoring
// it and substituting its own derivation instead).
func TestLauncherFromEnvHonoursSHCHVRunDirOverride(t *testing.T) {
	snapshotDir := t.TempDir()
	override := t.TempDir()

	lc, err := LauncherFromEnv(CloudHypervisor, envFrom(map[string]string{"SH_CHV_RUN_DIR": override}), snapshotDir, 256<<20, "test-override")
	if err != nil {
		t.Fatalf("LauncherFromEnv: %v", err)
	}
	chv, ok := lc.(*chvLauncher)
	if !ok {
		t.Fatalf("LauncherFromEnv(CloudHypervisor) returned %T, want *chvLauncher", lc)
	}
	if chv.opts.RunDir != override {
		t.Fatalf("LauncherFromEnv RunDir = %s, want the SH_CHV_RUN_DIR override %s unchanged", chv.opts.RunDir, override)
	}
}
